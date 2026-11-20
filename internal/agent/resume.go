package agent

import (
	"encoding/json"

	"harness/internal/llm"
	"harness/internal/store"
)

// FoldTurns replays persisted turns as conversation context for a resumed
// session. Text turns replay verbatim; tool interactions re-pair into
// assistant call messages and user result messages; a dangling call tail
// whose results never persisted is dropped so the resumed conversation ends
// on a well-formed alternation.
func FoldTurns(turns []store.Turn) []llm.Message {
	var out []llm.Message
	for _, tn := range turns {
		switch tn.Kind {
		case store.TurnMessage:
			out = append(out, llm.Message{Role: llm.Role(tn.Role), Text: tn.Content})
		case store.TurnToolCall:
			var d struct {
				ID     string          `json:"id"`
				Name   string          `json:"name"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(tn.Data, &d) != nil || d.ID == "" || d.Name == "" {
				continue
			}
			call := llm.ToolCall{ID: d.ID, Name: d.Name, Arguments: d.Params}
			if n := len(out); n > 0 && out[n-1].Role == llm.RoleAssistant {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, call)
			} else {
				out = append(out, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}})
			}
		case store.TurnToolResult:
			var d struct {
				Results []llm.ToolResult `json:"results"`
			}
			if json.Unmarshal(tn.Data, &d) == nil && len(d.Results) > 0 {
				out = append(out, llm.Message{Role: llm.RoleUser, ToolResults: d.Results})
			}
		}
	}
	if n := len(out); n > 0 && out[n-1].Role == llm.RoleAssistant && len(out[n-1].ToolCalls) > 0 {
		out = out[:n-1]
	}
	return out
}
