// Package llm defines a streaming-first abstraction over LLM chat APIs with
// native tool calling, structured-output hints, and token-usage accounting.
//
// The normalized model (Message, ToolCall, ToolResult, Event) is
// provider-agnostic. Concrete providers translate between the normalized
// shapes and their wire dialects (OpenAI-compatible chat completions,
// Anthropic Messages content blocks, Ollama /api/chat). All providers are
// streaming-first: Stream emits incremental events and terminates with
// exactly one EventDone or EventError before closing the channel.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ToolCall is a provider-neutral model-issued function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is the observation returned to the model for one ToolCall.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// Message is a provider-neutral chat message. Meaningful fields per role:
//
//	RoleSystem/RoleUser/RoleAssistant -> Text
//	RoleAssistant                     -> Text and/or ToolCalls
//	RoleUser                          -> ToolResults (optionally with Text)
//
// Metadata carries harness-side annotations (pinning, compaction markers)
// and is never serialized to any provider.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
	Metadata    map[string]any
}

// ToolSpec declares one callable tool exposed to the model. Parameters is a
// JSON Schema object describing the argument structure.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Usage reports token accounting for one exchange, in whichever granularity
// the provider reports (cache fields are zero when unavailable).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
}

// FinishReason normalizes why a completion stopped.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishOther     FinishReason = "other"
)

// Request is a provider-neutral completion request. System is separated from
// Messages so providers can apply prefix caching to stable instructions.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	// ResponseSchema, when set, asks the provider to constrain the final
	// message to a JSON object matching this JSON Schema. Providers apply it
	// via their native mechanism where one exists (OpenAI response_format,
	// Ollama format) and via instruction shaping otherwise (Anthropic).
	ResponseSchema json.RawMessage
	// ExtraBody carries provider-specific passthrough fields merged verbatim
	// into the wire request (advanced knobs, e.g. server-side slot pinning).
	ExtraBody map[string]any
}

// EventType discriminates Event payloads.
type EventType string

const (
	EventTextDelta EventType = "text_delta"
	EventToolDelta EventType = "tool_delta"
	EventUsage     EventType = "usage"
	EventDone      EventType = "done"
	EventError     EventType = "error"
)

// ToolDelta carries one fragment of an in-flight tool call. Fragments are
// accumulated per positional index by the aggregator (Complete) into whole
// ToolCalls; ID and Name typically arrive intact with the first fragment.
type ToolDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// Event is one incremental unit of a streamed completion.
type Event struct {
	Type         EventType
	Text         string // EventTextDelta
	Tool         *ToolDelta
	Usage        *Usage
	FinishReason FinishReason
	Err          error
}

// Provider streams completions. Stream must return a channel that emits any
// number of deltas and exactly one terminal EventDone or EventError before
// closing, honoring ctx cancellation at all times.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// Response is a fully assembled completion.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     *Usage
	Finish    FinishReason
}

// Complete runs Stream to exhaustion and aggregates the result. It is the
// convenience path for non-interactive calls (e.g. compaction summaries);
// interactive callers consume Stream directly.
func Complete(ctx context.Context, p Provider, req Request) (*Response, error) {
	events, err := p.Stream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: stream: %w", err)
	}
	var (
		text      strings.Builder
		usage     *Usage
		finish    FinishReason
		streamErr error
	)
	type slot struct {
		id   string
		name string
		args strings.Builder
	}
	slots := make(map[int]*slot)
	var order []int
	for ev := range events {
		switch ev.Type {
		case EventTextDelta:
			text.WriteString(ev.Text)
		case EventToolDelta:
			d := ev.Tool
			if d == nil {
				continue
			}
			s := slots[d.Index]
			if s == nil {
				s = &slot{}
				slots[d.Index] = s
				order = append(order, d.Index)
			}
			if d.ID != "" {
				s.id = d.ID
			}
			if d.Name != "" {
				s.name = d.Name
			}
			s.args.WriteString(d.Args)
		case EventUsage:
			usage = mergeUsage(usage, ev.Usage)
		case EventDone:
			finish = ev.FinishReason
		case EventError:
			streamErr = ev.Err
		}
	}
	if streamErr != nil {
		return nil, fmt.Errorf("llm: stream: %w", streamErr)
	}
	if finish == "" {
		finish = FinishStop
	}
	out := &Response{Text: text.String(), Usage: usage, Finish: finish}
	for _, idx := range order {
		s := slots[idx]
		args := json.RawMessage(strings.TrimSpace(s.args.String()))
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: s.id, Name: s.name, Arguments: args})
	}
	return out, nil
}

func mergeUsage(a, b *Usage) *Usage {
	if b == nil {
		return a
	}
	if a == nil {
		cp := *b
		return &cp
	}
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	return a
}

// EstimateTextTokens is a coarse upper-bound token estimate used between
// turns when no provider-reported usage is available. Providers' reported
// usage always takes precedence; this heuristic exists only to keep context
// budgets responsive mid-turn.
func EstimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len([]rune(s))
	// ~4 chars/token for prose; add slack for code (denser syntax).
	return n/4 + n%4 + len(s)/32
}

// EstimateMessageTokens estimates one normalized message.
func EstimateMessageTokens(m Message) int {
	n := EstimateTextTokens(m.Text)
	for _, tc := range m.ToolCalls {
		n += 5 + EstimateTextTokens(tc.Name) + EstimateTextTokens(string(tc.Arguments))
	}
	for _, tr := range m.ToolResults {
		n += 5 + EstimateTextTokens(tr.Content)
	}
	return n
}

// EstimateConversationTokens estimates the normalized conversation plus system.
func EstimateConversationTokens(system string, msgs []Message) int {
	n := EstimateTextTokens(system)
	for _, m := range msgs {
		n += EstimateMessageTokens(m)
	}
	return n
}
