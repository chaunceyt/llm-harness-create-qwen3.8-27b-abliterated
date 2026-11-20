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

// DefaultAnthropicBeta is the beta header value enabling prompt caching.
const DefaultAnthropicBeta = "prompt-caching-2024-07-31"

// AnthropicClient speaks the native Anthropic Messages API (/v1/messages)
// with tool_use/tool_result content blocks and prompt-caching headers.
type AnthropicClient struct {
	BaseURL       string
	APIKey        string
	UserAgent     string
	BetaHeader    string // anthropic-beta value; empty disables caching beta
	APIVersion    string // anthropic-version value
	EnableCaching bool
	Retry         RetryConfig
	HTTPClient    *http.Client
	ExtraHeaders  http.Header
}

// NewAnthropic validates and fills defaults for an AnthropicClient.
func NewAnthropic(baseURL, apiKey, userAgent string, retry RetryConfig) (*AnthropicClient, error) {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "://") {
		return nil, fmt.Errorf("llm: anthropic base URL %q missing scheme", baseURL)
	}
	if userAgent == "" {
		userAgent = "harness/1.0"
	}
	hc := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
	return &AnthropicClient{
		BaseURL:       baseURL,
		APIKey:        apiKey,
		UserAgent:     userAgent,
		BetaHeader:    DefaultAnthropicBeta,
		APIVersion:    "2023-06-01",
		EnableCaching: true,
		Retry:         retry.normalized(),
		HTTPClient:    hc,
	}, nil
}

func (c *AnthropicClient) Name() string { return "anthropic" }

// --- wire shapes -----------------------------------------------------------

type anBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anMessage struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anBody struct {
	Model       string      `json:"model"`
	MaxTokens   int         `json:"max_tokens"`
	System      []anBlock   `json:"system,omitempty"`
	Messages    []anMessage `json:"messages"`
	Tools       []anTool    `json:"tools,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
}

type anEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Index        *int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason *string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- normalization ---------------------------------------------------------

// schemaInstruction shapes structured output for providers without a native
// schema mechanism. It is appended to the system prefix, which keeps it
// outside the cacheable conversation body.
func schemaInstruction(schema json.RawMessage) string {
	s := string(schema)
	if len(s) > 4000 {
		s = s[:4000] + "…"
	}
	return "\n\n<structured_output>\nRespond with ONLY a single JSON object — no prose, no markdown fences — whose structure matches this JSON Schema exactly:\n" + s + "\n</structured_output>"
}

func (c *AnthropicClient) marshal(req Request) ([]byte, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("llm: anthropic: empty model")
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}
	body := anBody{
		Model:       req.Model,
		MaxTokens:   maxTok,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	var systemParts []string
	if req.System != "" {
		systemParts = append(systemParts, req.System)
	}
	if len(req.ResponseSchema) > 0 {
		systemParts = append(systemParts, schemaInstruction(req.ResponseSchema))
	}
	if len(systemParts) > 0 {
		sb := anBlock{Type: "text", Text: strings.Join(systemParts, "\n\n")}
		if c.EnableCaching && c.BetaHeader != "" {
			sb.CacheControl = json.RawMessage(`{"type":"ephemeral"}`)
		}
		body.System = append(body.System, sb)
	}

	// Locate the last content block across the conversation so the second
	// caching breakpoint sits at the moving frontier (stable prefix before
	// it stays cacheable).
	lastMsgIdx, lastBlockIdx := -1, -1
	plan := make([][]anBlock, len(req.Messages))
	for i, m := range req.Messages {
		var blocks []anBlock
		switch m.Role {
		case RoleSystem:
			if m.Text != "" {
				blocks = append(blocks, anBlock{Type: "text", Text: m.Text})
			}
		case RoleAssistant:
			if m.Text != "" {
				blocks = append(blocks, anBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anBlock{Type: "text", Text: ""})
			}
		case RoleUser:
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anBlock{
					Type:      "tool_result",
					ToolUseID: tr.ToolCallID,
					Content:   tr.Content,
					IsError:   tr.IsError,
				})
			}
			if m.Text != "" {
				blocks = append(blocks, anBlock{Type: "text", Text: m.Text})
			}
			if len(blocks) == 0 {
				continue
			}
		default:
			return nil, fmt.Errorf("llm: anthropic: unsupported role %q", m.Role)
		}
		plan[i] = blocks
		lastMsgIdx, lastBlockIdx = i, len(blocks)-1
	}
	for i, blocks := range plan {
		if len(blocks) == 0 {
			continue
		}
		if i == lastMsgIdx && lastBlockIdx >= 0 && c.EnableCaching && c.BetaHeader != "" {
			blocks[lastBlockIdx].CacheControl = json.RawMessage(`{"type":"ephemeral"}`)
		}
		body.Messages = append(body.Messages, anMessage{
			Role:    string(req.Messages[i].Role),
			Content: blocks,
		})
	}
	if len(body.Messages) == 0 {
		return nil, fmt.Errorf("llm: anthropic: empty conversation")
	}

	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, anTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: anthropic: marshal request: %w", err)
	}
	return raw, nil
}

// --- streaming -------------------------------------------------------------

// Stream opens a streaming Messages completion. Pre-stream transport and
// rate-limit failures are retried under the client's Retry policy.
func (c *AnthropicClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
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
			return nil, fmt.Errorf("llm: anthropic: %w", err)
		}
	}
	return nil, fmt.Errorf("llm: anthropic: failed after %d attempts: %w", c.Retry.Attempts, lastErr)
}

func (c *AnthropicClient) streamOnce(ctx context.Context, req Request) (<-chan Event, error) {
	raw, err := c.marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("llm: anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.UserAgent)
	httpReq.Header.Set("anthropic-version", c.APIVersion)
	if c.APIKey != "" {
		httpReq.Header.Set("x-api-key", c.APIKey)
	}
	if c.EnableCaching && c.BetaHeader != "" {
		httpReq.Header.Set("anthropic-beta", c.BetaHeader)
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}

	var resp *http.Response
	err = withRetries(ctx, "anthropic request", c.Retry, func() error {
		var derr error
		resp, derr = c.HTTPClient.Do(httpReq)
		if derr != nil {
			return fmt.Errorf("llm: anthropic: transport: %w", derr)
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
		pumpErr := pumpSSE(ctx, resp.Body, func(data string) error {
			var ev anEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fmt.Errorf("llm: anthropic: decode event: %w", err)
			}
			if ev.Error != nil {
				return fmt.Errorf("llm: anthropic: stream error (%s): %s", ev.Error.Type, ev.Error.Message)
			}
			switch ev.Type {
			case "message_start":
				if ev.Message != nil && ev.Message.Usage != nil {
					events <- Event{Type: EventUsage, Usage: &Usage{
						PromptTokens:     ev.Message.Usage.InputTokens,
						CacheReadTokens:  ev.Message.Usage.CacheReadInputTokens,
						CacheWriteTokens: ev.Message.Usage.CacheCreationInputTokens,
					}}
				}
			case "content_block_start":
				if ev.Index != nil && ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
					events <- Event{Type: EventToolDelta, Tool: &ToolDelta{
						Index: *ev.Index,
						ID:    ev.ContentBlock.ID,
						Name:  ev.ContentBlock.Name,
					}}
				}
			case "content_block_delta":
				if ev.Delta == nil {
					return nil
				}
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						events <- Event{Type: EventTextDelta, Text: ev.Delta.Text}
					}
				case "input_json_delta":
					if ev.Index != nil {
						events <- Event{
							Type: EventToolDelta,
							Tool: &ToolDelta{Index: *ev.Index, Args: ev.Delta.PartialJSON},
						}
					}
				}
			case "message_delta":
				if ev.Usage != nil {
					events <- Event{Type: EventUsage, Usage: &Usage{
						CompletionTokens: ev.Usage.OutputTokens,
					}}
				}
				if ev.StopReason != nil {
					switch *ev.StopReason {
					case "tool_use":
						finish = FinishToolCalls
					case "max_tokens":
						finish = FinishLength
					case "end_turn", "stop_sequence":
						finish = FinishStop
					default:
						finish = FinishOther
					}
				}
			case "error":
				// Handled above via ev.Error; belt and braces.
			}
			return nil
		})
		switch {
		case pumpErr == nil || pumpErr == errSSEDone:
			if finish == "" {
				finish = FinishStop
			}
			events <- Event{Type: EventDone, FinishReason: finish}
		default:
			if ctx.Err() != nil {
				events <- Event{Type: EventError, Err: fmt.Errorf("llm: anthropic: stream cancelled: %w", ctx.Err())}
			} else {
				events <- Event{Type: EventError, Err: fmt.Errorf("llm: anthropic: %w", pumpErr)}
			}
		}
	}()
	return events, nil
}
