package agent

import (
	"context"
	"fmt"
	"strings"

	"harness/internal/llm"
)

// maybeCompact compresses the conversation when the estimated context
// reaches the configured fraction of the window. Preserved verbatim: pinned
// messages (Metadata["pin"] = true, e.g. pinned file contents) and the most
// recent recentKeep messages. Everything older folds into one structured
// summary produced by a non-streaming provider call.
func (a *Agent) maybeCompact(ctx context.Context, msgs *[]llm.Message, lastUsage *llm.Usage, rec func(string, string, string, any)) error {
	estimate := llm.EstimateConversationTokens(a.systemText(), *msgs)
	if lastUsage != nil && lastUsage.TotalTokens > 0 {
		estimate = lastUsage.TotalTokens
	}
	if estimate < int(float64(a.Window)*a.CompactAt) {
		return nil
	}
	n := len(*msgs)
	if n <= recentKeep+2 {
		return nil // nothing yet worth compressing
	}
	recentFrom := n - recentKeep
	var pinned, middle []llm.Message
	for i, m := range *msgs {
		if i >= recentFrom {
			continue
		}
		if pinnedMsg(m) {
			pinned = append(pinned, m)
			continue
		}
		middle = append(middle, m)
	}
	if len(middle) < 2 {
		return nil
	}
	summary, err := a.summarize(ctx, middle)
	if err != nil {
		return fmt.Errorf("agent: compaction summarize: %w", err)
	}
	out := make([]llm.Message, 0, len(pinned)+1+recentKeep)
	out = append(out, pinned...)
	out = append(out, llm.Message{
		Role:     llm.RoleUser,
		Text:     "Session context compressed by the runtime. Prior context:\n\n" + summary,
		Metadata: map[string]any{"compaction": true},
	})
	for i := recentFrom; i < n; i++ {
		out = append(out, (*msgs)[i])
	}
	*msgs = out
	rec("event", "", "", map[string]any{
		"event":    "compaction",
		"folded":   len(middle),
		"estimate": estimate,
	})
	a.Sink.OnEvent("compaction", map[string]any{"folded": len(middle)})
	return nil
}

func pinnedMsg(m llm.Message) bool {
	v, _ := m.Metadata["pin"].(bool)
	return v
}

const compactPrompt = `Summarize the conversation segment below for continued operation. Use exactly these sections:

## State
Concrete facts about the current state of the workspace and task.

## Completed
Actions already executed and their verified outcomes.

## Open threads
Intended-but-unfinished work, including exact commands or edits that were in flight.

Be terse and concrete; prefer exact identifiers (paths, symbols, commands) over paraphrase.

Segment:
%s`

func (a *Agent) summarize(ctx context.Context, middle []llm.Message) (string, error) {
	var sb strings.Builder
	for i, m := range middle {
		fmt.Fprintf(&sb, "[%d] %s: %s", i+1, m.Role, m.Text)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "\n    tool_call %s %s", tc.Name, tc.Arguments)
		}
		for _, tr := range m.ToolResults {
			excerpt := tr.Content
			if len(excerpt) > 400 {
				excerpt = excerpt[:400] + "..."
			}
			fmt.Fprintf(&sb, "\n    tool_result(error=%v) %s", tr.IsError, excerpt)
		}
		sb.WriteString("\n")
	}
	req := llm.Request{
		Model:  a.Model,
		System: "You compress agent conversation context. Output only the summary.",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Text: fmt.Sprintf(compactPrompt, sb.String()),
		}},
	}
	resp, err := llm.Complete(ctx, a.Provider, req)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}
