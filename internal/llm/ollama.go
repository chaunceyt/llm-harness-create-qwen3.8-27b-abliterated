package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OllamaClient speaks the Ollama native /api/chat dialect, including its
// tool-calling protocol, keep_alive session retention, and model options
// (num_ctx, temperature, …).
type OllamaClient struct {
	BaseURL    string
	UserAgent  string
	KeepAlive  string         // default "30m"; pins the loaded model between turns
	Options    map[string]any // persistent model options (e.g. num_ctx)
	Retry      RetryConfig
	HTTPClient *http.Client
	ExtraBody  map[string]any
}

// NewOllama validates and fills defaults for an OllamaClient.
func NewOllama(baseURL, userAgent string, retry RetryConfig) (*OllamaClient, error) {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "://") {
		return nil, fmt.Errorf("llm: ollama base URL %q missing scheme", baseURL)
	}
	if userAgent == "" {
		userAgent = "harness/1.0"
	}
	hc := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
	return &OllamaClient{
		BaseURL:    baseURL,
		UserAgent:  userAgent,
		KeepAlive:  "30m",
		Retry:      retry.normalized(),
		HTTPClient: hc,
	}, nil
}

func (c *OllamaClient) Name() string { return "ollama" }

// --- wire shapes -----------------------------------------------------------

type olToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type olMsg struct {
	Role      string       `json:"role"`
	Content   any          `json:"content,omitempty"`
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
}

type olBody struct {
	Model     string         `json:"model"`
	Messages  []olMsg        `json:"messages"`
	Stream    bool           `json:"stream"`
	Tools     []oaToolSpec   `json:"tools,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
	Format    any            `json:"format,omitempty"`
}

type olChunk struct {
	Model   string `json:"model"`
	Message *struct {
		Role      string       `json:"role"`
		Content   string       `json:"content"`
		ToolCalls []olToolCall `json:"tool_calls"`
	} `json:"message"`
	Done          bool   `json:"done"`
	DoneReason    string `json:"done_reason"`
	PromptEvalCnt int    `json:"prompt_eval_count"`
	EvalCnt       int    `json:"eval_count"`
	Error         string `json:"error"`
}

// --- normalization ---------------------------------------------------------

// toolNameIndex resolves ToolCallID -> tool name across the conversation so
// Ollama's structured tool-result messages can name the originating call.
func toolNameIndex(msgs []Message) map[string]string {
	idx := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				idx[tc.ID] = tc.Name
			}
		}
	}
	return idx
}

func (c *OllamaClient) marshal(req Request) ([]byte, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("llm: ollama: empty model")
	}
	names := toolNameIndex(req.Messages)
	body := olBody{
		Model:     req.Model,
		Stream:    true,
		KeepAlive: c.KeepAlive,
	}
	if req.System != "" {
		body.Messages = append(body.Messages, olMsg{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if m.Text != "" {
				body.Messages = append(body.Messages, olMsg{Role: "system", Content: m.Text})
			}
		case RoleAssistant:
			if len(m.ToolCalls) > 0 {
				out := olMsg{Role: "assistant", Content: ""}
				for _, tc := range m.ToolCalls {
					args := tc.Arguments
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					var call olToolCall
					if err := json.Unmarshal(args, &call.Function.Arguments); err != nil {
						call.Function.Arguments = args
					}
					call.Function.Name = tc.Name
					out.ToolCalls = append(out.ToolCalls, call)
				}
				if m.Text != "" {
					out.Content = m.Text
				}
				body.Messages = append(body.Messages, out)
			} else if m.Text != "" {
				body.Messages = append(body.Messages, olMsg{Role: "assistant", Content: m.Text})
			}
		case RoleUser:
			for _, tr := range m.ToolResults {
				// Ollama's documented protocol: the observation returns as a
				// user message carrying a JSON-encoded result object.
				result := map[string]any{
					"name":    names[tr.ToolCallID],
					"content": tr.Content,
				}
				if tr.IsError {
					result["is_error"] = true
				}
				enc, err := json.Marshal(result)
				if err != nil {
					return nil, fmt.Errorf("llm: ollama: encode tool result: %w", err)
				}
				body.Messages = append(body.Messages, olMsg{Role: "user", Content: string(enc)})
			}
			if m.Text != "" {
				body.Messages = append(body.Messages, olMsg{Role: "user", Content: m.Text})
			}
		default:
			return nil, fmt.Errorf("llm: ollama: unsupported role %q", m.Role)
		}
	}
	if len(body.Messages) == 0 {
		return nil, fmt.Errorf("llm: ollama: empty conversation")
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, oaToolSpec{
			Type:     "function",
			Function: oaFnSpec{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	opts := make(map[string]any, len(c.Options)+2)
	for k, v := range c.Options {
		opts[k] = v
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if len(opts) > 0 {
		body.Options = opts
	}
	if len(req.ResponseSchema) > 0 {
		// Recent Ollama accepts a JSON Schema directly as `format`; older
		// releases fall back to plain JSON mode, which still constrains shape.
		body.Format = json.RawMessage(req.ResponseSchema)
	}
	for k, v := range c.ExtraBody {
		if err := mergeExtra(&body, k, v); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: ollama: marshal request: %w", err)
	}
	return raw, nil
}

func mergeExtra(body *olBody, key string, val any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m[key] = val
	merged, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(merged, body)
}

// --- streaming -------------------------------------------------------------

// Stream opens a streaming /api/chat completion (NDJSON chunks). Pre-stream
// transport failures are retried under the client's Retry policy.
func (c *OllamaClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	var lastErr error
	for attempt := 0; attempt < c.Retry.Attempts; attempt++ {
		evs, err := c.streamOnce(ctx, req)
		if err == nil {
			return evs, nil
		}
		lastErr = err
		if !retryableErr(err) {
			return nil, err
		}
		if attempt == c.Retry.Attempts-1 {
			break
		}
		delay := backoffDelay(c.Retry, attempt)
		var se *statusError
		if errors.As(lastErr, &se) && se.RetryAfter > delay {
			delay = se.RetryAfter
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, fmt.Errorf("llm: ollama: %w", err)
		}
	}
	return nil, fmt.Errorf("llm: ollama: failed after %d attempts: %w", c.Retry.Attempts, lastErr)
}

func (c *OllamaClient) streamOnce(ctx context.Context, req Request) (<-chan Event, error) {
	raw, err := c.marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("llm: ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.UserAgent)

	var resp *http.Response
	err = withRetries(ctx, "ollama request", c.Retry, func() error {
		var derr error
		resp, derr = c.HTTPClient.Do(httpReq)
		if derr != nil {
			return fmt.Errorf("llm: ollama: transport: %w", derr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return classifyResponse(resp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	events := make(chan Event, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		var finish FinishReason
		var argsAcc []string
		var sawToolCall bool
		pumpErr := pumpLines(ctx, resp.Body, func(line string) error {
			line = strings.TrimSpace(line)
			if line == "" {
				return nil
			}
			var chunk olChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				return fmt.Errorf("llm: ollama: decode chunk: %w", err)
			}
			if chunk.Error != "" {
				return fmt.Errorf("llm: ollama: stream error: %s", chunk.Error)
			}
			if chunk.Message != nil {
				if chunk.Message.Content != "" {
					events <- Event{Type: EventTextDelta, Text: chunk.Message.Content}
				}
				for i, tc := range chunk.Message.ToolCalls {
					sawToolCall = true
					for len(argsAcc) <= i {
						argsAcc = append(argsAcc, "")
					}
					// Fragments stream incrementally; some releases deliver the
					// complete argument document only in the final chunk. Append
					// semantics is correct for both: an empty accumulator takes
					// the first fragment verbatim.
					argsAcc[i] += string(tc.Function.Arguments)
					events <- Event{
						Type: EventToolDelta,
						Tool: &ToolDelta{
							Index: i,
							Name:  tc.Function.Name,
							Args:  string(tc.Function.Arguments),
						},
					}
				}
			}
			if chunk.Done {
				finish = mapDoneReason(chunk.DoneReason, sawToolCall)
				events <- Event{Type: EventUsage, Usage: &Usage{
					PromptTokens:     chunk.PromptEvalCnt,
					CompletionTokens: chunk.EvalCnt,
					TotalTokens:      chunk.PromptEvalCnt + chunk.EvalCnt,
				}}
			}
			return nil
		})
		switch {
		case pumpErr == nil:
			if finish == "" {
				finish = FinishStop
			}
			events <- Event{Type: EventDone, FinishReason: finish}
		default:
			if ctx.Err() != nil {
				events <- Event{Type: EventError, Err: fmt.Errorf("llm: ollama: stream cancelled: %w", ctx.Err())}
			} else {
				events <- Event{Type: EventError, Err: fmt.Errorf("llm: ollama: %w", pumpErr)}
			}
		}
	}()
	return events, nil
}

func mapDoneReason(reason string, sawToolCall bool) FinishReason {
	if sawToolCall {
		return FinishToolCalls
	}
	switch reason {
	case "":
		return FinishStop
	case "length", "model_context_length_exceeded":
		return FinishLength
	default:
		return FinishOther
	}
}
