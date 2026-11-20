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

// OpenAIClient speaks the OpenAI-compatible /v1/chat/completions dialect. It
// covers hosted OpenAI plus local servers that implement the same dialect,
// notably the llama.cpp server (llama-server), via BaseURL. Server-side
// knobs that differ between implementations (slot pinning, cache affinity,
// sampling overrides) are reachable through ExtraBody passthrough fields.
type OpenAIClient struct {
	BaseURL    string
	APIKey     string
	UserAgent  string
	Retry      RetryConfig
	HTTPClient *http.Client
	ExtraBody  map[string]any
	// ExtraHeaders are applied verbatim to every request.
	ExtraHeaders http.Header
}

// NewOpenAI validates and fills defaults for an OpenAIClient.
func NewOpenAI(baseURL, apiKey, userAgent string, retry RetryConfig) (*OpenAIClient, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "://") {
		return nil, fmt.Errorf("llm: openai base URL %q missing scheme", baseURL)
	}
	if userAgent == "" {
		userAgent = "harness/1.0"
	}
	hc := &http.Client{
		// No overall Timeout: streams are long-lived by design; lifetime is
		// governed exclusively by context.Context cancellation.
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
	return &OpenAIClient{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		UserAgent:  userAgent,
		Retry:      retry.normalized(),
		HTTPClient: hc,
	}, nil
}

func (c *OpenAIClient) Name() string { return "openai" }

// --- wire shapes -----------------------------------------------------------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	Type     string     `json:"type"`
	ID       string     `json:"id"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type oaToolSpec struct {
	Type     string   `json:"type"`
	Function oaFnSpec `json:"function"`
}

type oaFnSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaBody struct {
	Model          string         `json:"model"`
	Messages       []oaMessage    `json:"messages"`
	Tools          []oaToolSpec   `json:"tools,omitempty"`
	Stream         bool           `json:"stream"`
	StreamOptions  map[string]any `json:"stream_options,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	TopP           *float64       `json:"top_p,omitempty"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   *string `json:"content"`
			Role      string  `json:"role"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// --- normalization ---------------------------------------------------------

func (c *OpenAIClient) marshal(req Request) ([]byte, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("llm: openai: empty model")
	}
	body := oaBody{
		Model:       req.Model,
		Stream:      true,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if len(req.System) > 0 {
		body.Messages = append(body.Messages, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			body.Messages = append(body.Messages, oaMessage{Role: "system", Content: m.Text})
		case RoleAssistant:
			out := oaMessage{Role: "assistant"}
			if m.Text != "" {
				out.Content = m.Text
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				out.ToolCalls = append(out.ToolCalls, oaToolCall{
					Type:     "function",
					ID:       tc.ID,
					Function: oaFunction{Name: tc.Name, Arguments: args},
				})
			}
			body.Messages = append(body.Messages, out)
		case RoleUser:
			if m.Text != "" {
				body.Messages = append(body.Messages, oaMessage{Role: "user", Content: m.Text})
			}
			for _, tr := range m.ToolResults {
				body.Messages = append(body.Messages, oaMessage{
					Role:       "tool",
					ToolCallID: tr.ToolCallID,
					Content:    tr.Content,
				})
			}
		default:
			return nil, fmt.Errorf("llm: openai: unsupported role %q", m.Role)
		}
	}
	if len(body.Messages) == 0 {
		return nil, fmt.Errorf("llm: openai: empty conversation")
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
	if len(req.ResponseSchema) > 0 {
		body.ResponseFormat = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "structured_output",
				"schema": json.RawMessage(req.ResponseSchema),
				"strict": false,
			},
		}
	}
	// Streaming usage accounting; unknown-field tolerant on llama.cpp.
	body.StreamOptions = map[string]any{"include_usage": true}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: openai: marshal request: %w", err)
	}
	if len(c.ExtraBody) > 0 || len(req.ExtraBody) > 0 {
		merged := map[string]any{}
		if err := json.Unmarshal(raw, &merged); err != nil {
			return nil, fmt.Errorf("llm: openai: re-marshal: %w", err)
		}
		for k, v := range c.ExtraBody {
			merged[k] = v
		}
		for k, v := range req.ExtraBody {
			merged[k] = v
		}
		if raw, err = json.Marshal(merged); err != nil {
			return nil, fmt.Errorf("llm: openai: re-marshal with extras: %w", err)
		}
	}
	return raw, nil
}

// --- streaming -------------------------------------------------------------

// Stream opens a streaming completion. Transport- and rate-limit failures
// before stream start are retried under the client's Retry policy; failures
// after emission surface as EventError.
func (c *OpenAIClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
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
			return nil, fmt.Errorf("llm: openai: %w", err)
		}
	}
	return nil, fmt.Errorf("llm: openai: failed after %d attempts: %w", c.Retry.Attempts, lastErr)
}

func (c *OpenAIClient) streamOnce(ctx context.Context, req Request) (<-chan Event, error) {
	raw, err := c.marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("llm: openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.UserAgent)
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	var resp *http.Response
	err = withRetries(ctx, "openai request", c.Retry, func() error {
		var derr error
		resp, derr = c.HTTPClient.Do(httpReq)
		if derr != nil {
			return fmt.Errorf("llm: openai: transport: %w", derr)
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
		var (
			finish     FinishReason
			pendingErr error
		)
		type oaSlot struct {
			id   string
			name string
		}
		slots := make(map[int]*oaSlot)
		pumpErr := pumpSSE(ctx, resp.Body, func(data string) error {
			var chunk oaChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return fmt.Errorf("llm: openai: decode chunk: %w", err)
			}
			if chunk.Error != nil {
				return fmt.Errorf("llm: openai: stream error (%s): %s", chunk.Error.Type, chunk.Error.Message)
			}
			if chunk.Usage != nil {
				u := &Usage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				}
				if chunk.Usage.PromptTokensDetails != nil {
					u.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				events <- Event{Type: EventUsage, Usage: u}
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.Content != nil && *ch.Delta.Content != "" {
					events <- Event{Type: EventTextDelta, Text: *ch.Delta.Content}
				}
				for _, tc := range ch.Delta.ToolCalls {
					s := slots[tc.Index]
					if s == nil {
						s = &oaSlot{}
						slots[tc.Index] = s
					}
					if tc.ID != "" {
						s.id = tc.ID
					}
					if tc.Function.Name != "" {
						s.name = tc.Function.Name
					}
					events <- Event{
						Type: EventToolDelta,
						Tool: &ToolDelta{
							Index: tc.Index,
							ID:    tc.ID,
							Name:  tc.Function.Name,
							Args:  tc.Function.Arguments,
						},
					}
				}
				if ch.FinishReason != nil {
					switch *ch.FinishReason {
					case "tool_calls":
						finish = FinishToolCalls
					case "length":
						finish = FinishLength
					case "stop":
						finish = FinishStop
					default:
						finish = FinishOther
					}
				}
			}
			return nil
		})
		switch {
		case pumpErr == nil:
			if finish == "" {
				finish = FinishStop
			}
			events <- Event{Type: EventDone, FinishReason: finish}
		case pumpErr == errSSEDone:
			if finish == "" {
				finish = FinishStop
			}
			events <- Event{Type: EventDone, FinishReason: finish}
		default:
			if ctx.Err() != nil {
				pendingErr = fmt.Errorf("llm: openai: stream cancelled: %w", ctx.Err())
			} else {
				pendingErr = fmt.Errorf("llm: openai: %w", pumpErr)
			}
			events <- Event{Type: EventError, Err: pendingErr}
		}
	}()
	return events, nil
}
