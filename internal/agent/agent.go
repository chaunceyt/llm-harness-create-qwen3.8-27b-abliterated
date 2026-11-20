// Package agent implements the harness's deterministic ReAct loop: streaming
// model turns, sandboxed tool execution, permission gating, doom-loop
// detection, context compaction, and mode personas (coder, reviewer,
// automator). All state is per-invocation; the Agent value is immutable after
// construction. Persistence (store) and tracing (telemetry) are optional and
// nil-safe.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"harness/internal/llm"
	"harness/internal/store"
	"harness/internal/telemetry"
	"harness/internal/tools"
)

// Mode selects the agent persona.
type Mode string

const (
	// ModeCoder executes tasks with the full toolset and a writable workspace.
	ModeCoder Mode = "coder"
	// ModeReviewer analyzes with read-only tools and a read-only workspace.
	ModeReviewer Mode = "reviewer"
	// ModeAutomator executes DAG workflows with the full toolset.
	ModeAutomator Mode = "automator"
)

const (
	defaultMaxTurns = 50
	defaultWindow   = 128000
	defaultCompact  = 0.8
	// recentKeep bounds the recent-turn window preserved verbatim by
	// compaction.
	recentKeep = 6
	// doomLimit trips the ladder after this many consecutive identical
	// call/result signatures.
	doomLimit = 3
)

// Sink receives observable agent activity for display and telemetry.
type Sink interface {
	OnTextDelta(delta string)
	OnToolCall(name string, args json.RawMessage)
	OnToolResult(name string, result string, isError bool)
	OnEvent(event string, detail map[string]any)
}

type nopSink struct{}

func (nopSink) OnTextDelta(string)                 {}
func (nopSink) OnToolCall(string, json.RawMessage) {}
func (nopSink) OnToolResult(string, string, bool)  {}
func (nopSink) OnEvent(string, map[string]any)     {}

// Decision is one HITL outcome.
type Decision struct {
	Allow bool
	// Memo is surfaced to the model when Allow is true.
	Memo string
}

// ApprovalRequest is one HITL prompt to a human.
type ApprovalRequest struct {
	Tool   string
	Args   json.RawMessage
	Reason string
}

// Approver decides HITL prompts (interactive in the CLI).
type Approver func(req ApprovalRequest) (Decision, error)

// Report summarizes one Run invocation.
type Report struct {
	SessionID  string
	Answer     string
	Turns      int
	Usage      llm.Usage
	StoppedBy  string // final|max_turns|escalation|canceled
	Escalated  bool
	Escalation string
}

// Agent is one configured autonomous runtime. Fields are immutable after
// construction; zero fields take safe defaults via normalize.
type Agent struct {
	Provider  llm.Provider
	Model     string
	Mode      Mode
	Env       tools.Env
	Tools     *tools.Registry
	Store     *store.Store
	Tracer    *telemetry.Tracer
	Logger    *slog.Logger
	Sink      Sink
	Approver  Approver
	Policy    *Policy
	MaxTurns  int
	Window    int
	CompactAt float64
	// Seed replays conversation context from a resumed session ahead of
	// the task input. Read-only; safe to share across concurrent Runs.
	Seed []llm.Message
	// ResumeSession continues an existing persisted session: new turns
	// append under its ID. Empty starts a fresh session.
	ResumeSession string
}

func modeToolNames(m Mode) []string {
	if m == ModeReviewer {
		return []string{"fs_read", "fs_search", "git_ops"}
	}
	return nil // every built-in
}

func (a *Agent) normalize() error {
	if a.Provider == nil {
		return fmt.Errorf("agent: provider is required")
	}
	if a.Mode == "" {
		a.Mode = ModeCoder
	}
	if a.MaxTurns <= 0 {
		a.MaxTurns = defaultMaxTurns
	}
	if a.Window <= 0 {
		a.Window = defaultWindow
	}
	if a.CompactAt <= 0 || a.CompactAt >= 1 {
		a.CompactAt = defaultCompact
	}
	if a.Sink == nil {
		a.Sink = nopSink{}
	}
	if a.Logger == nil {
		a.Logger = telemetry.NewLogger(telemetry.LoggerOptions{Writer: os.Stderr})
	}
	if a.Tools == nil {
		reg, err := tools.New(a.Env, modeToolNames(a.Mode)...)
		if err != nil {
			return fmt.Errorf("agent: build tools: %w", err)
		}
		a.Tools = reg
	}
	if a.Policy == nil {
		a.Policy = NewPolicy(defaultEffectFor(a.Mode))
	}
	return nil
}

func defaultEffectFor(m Mode) Effect {
	if m == ModeReviewer {
		return EffectAllow
	}
	return EffectAsk
}

func (a *Agent) systemText() string {
	switch a.Mode {
	case ModeReviewer:
		return "You are a code reviewer. Analyze the workspace with read-only tools and report findings. You cannot modify files; do not attempt to."
	case ModeAutomator:
		return "You are a workflow automator. Execute each workflow step precisely with the available tools and report concrete results."
	default:
		return "You are a coding agent. Complete the task using the provided tools. Prefer targeted file operations over wholesale rewrites; verify effects of commands before proceeding."
	}
}

func (a *Agent) toolSpecs() []llm.ToolSpec {
	var out []llm.ToolSpec
	for _, name := range a.Tools.Names() {
		t, _ := a.Tools.Get(name)
		out = append(out, llm.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}
	return out
}

// Run executes one autonomous session over input and returns the report.
// Default completion operates on a per-invocation copy so concurrent Runs
// never share mutable configuration state.
func (a *Agent) Run(ctx context.Context, input string) (Report, error) {
	ag := *a
	return ag.run(ctx, input)
}

func (a *Agent) run(ctx context.Context, input string) (Report, error) {
	if err := a.normalize(); err != nil {
		return Report{}, err
	}
	var sessionID string
	if a.Store != nil {
		if a.ResumeSession != "" {
			if _, err := a.Store.GetSession(ctx, a.ResumeSession); err != nil {
				return Report{}, fmt.Errorf("agent: resume session %s: %w", a.ResumeSession, err)
			}
			sessionID = a.ResumeSession
		} else {
			sess, err := a.Store.CreateSession(ctx, string(a.Mode), a.Provider.Name(), a.Model, a.Env.Workspace, nil)
			if err != nil {
				return Report{}, fmt.Errorf("agent: create session: %w", err)
			}
			sessionID = sess.ID
		}
	}
	if a.Tracer != nil {
		ctx = telemetry.ContextWithTracer(ctx, a.Tracer)
	}
	ctx, span := telemetry.StartSpan(ctx, "agent.session")
	span.SetAttribute("agent.mode", string(a.Mode))
	span.SetAttribute("agent.model", a.Model)
	span.SetAttribute("agent.session", sessionID)

	rec := func(kind, role, content string, data any) {
		if a.Store == nil || sessionID == "" {
			return
		}
		if kind == store.TurnEvent {
			ev, _ := data.(map[string]any)
			label, _ := ev["event"].(string)
			if label == "" {
				label = "event"
			}
			detail, _ := json.Marshal(data)
			_ = a.Store.Audit(ctx, sessionID, label, string(detail))
			return
		}
		_, _ = a.Store.AppendTurn(ctx, sessionID, kind, role, content, data)
	}
	msgs := make([]llm.Message, 0, len(a.Seed)+1)
	msgs = append(msgs, a.Seed...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: input})
	rec(store.TurnMessage, "user", input, nil)

	report := Report{SessionID: sessionID}
	var lastUsage *llm.Usage
	doom := newDoomState()

	for turn := 0; turn < a.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			report.StoppedBy = "canceled"
			span.End()
			return report, nil
		}
		if err := a.maybeCompact(ctx, &msgs, lastUsage, rec); err != nil {
			a.Logger.Warn("compaction failed; continuing", "err", err)
		}
		req := llm.Request{
			Model:    a.Model,
			System:   a.systemText(),
			Messages: msgs,
			Tools:    a.toolSpecs(),
		}
		out, err := a.consume(ctx, req)
		if err != nil {
			span.End()
			return report, fmt.Errorf("agent: turn %d: %w", turn+1, err)
		}
		report.Turns++
		if out.usage != nil {
			report.Usage = merge(&report.Usage, out.usage)
			lastUsage = out.usage
		}

		if len(out.calls) == 0 {
			report.Answer = out.text
			report.StoppedBy = "final"
			rec(store.TurnMessage, "assistant", out.text, nil)
			span.End()
			return report, nil
		}

		msgs = append(msgs, llm.Message{
			Role:      llm.RoleAssistant,
			Text:      out.text,
			ToolCalls: out.calls,
		})
		rec(store.TurnMessage, "assistant", out.text, map[string]any{"tool_calls": len(out.calls)})

		var results []llm.ToolResult
		aborted := false
		for _, call := range out.calls {
			res, stopped := a.executeCall(ctx, &msgs, rec, doom, call)
			results = append(results, llm.ToolResult{
				ToolCallID: call.ID,
				Content:    res.Content,
				IsError:    res.IsError,
			})
			if stopped {
				aborted = true
			}
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, ToolResults: results})
		rec(store.TurnToolResult, "", "", map[string]any{
			"results": resultsJSON(results),
		})
		if aborted {
			report.Escalated = true
			report.StoppedBy = "escalation"
			report.Escalation = doom.lastReason
			span.End()
			return report, nil
		}
	}

	report.StoppedBy = "max_turns"
	span.End()
	return report, nil
}

// consume runs one streamed turn, forwarding observable activity to the sink
// and aggregating deltas into a whole response.
func (a *Agent) consume(ctx context.Context, req llm.Request) (streamOut, error) {
	events, err := a.Provider.Stream(ctx, req)
	if err != nil {
		return streamOut{}, fmt.Errorf("agent: stream: %w", err)
	}
	var (
		text      strings.Builder
		usage     *llm.Usage
		finish    llm.FinishReason
		streamErr error
	)
	type slot struct {
		id, name string
		args     strings.Builder
	}
	slots := make(map[int]*slot)
	var order []int
	for ev := range events {
		switch ev.Type {
		case llm.EventTextDelta:
			a.Sink.OnTextDelta(ev.Text)
			text.WriteString(ev.Text)
		case llm.EventToolDelta:
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
		case llm.EventUsage:
			u := merge(usage, ev.Usage)
			usage = &u
		case llm.EventDone:
			finish = ev.FinishReason
		case llm.EventError:
			streamErr = ev.Err
		}
	}
	if streamErr != nil {
		return streamOut{}, fmt.Errorf("agent: stream: %w", streamErr)
	}
	out := streamOut{text: text.String(), finish: finish, usage: usage}
	for _, idx := range order {
		s := slots[idx]
		args := json.RawMessage(strings.TrimSpace(s.args.String()))
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		out.calls = append(out.calls, llm.ToolCall{ID: s.id, Name: s.name, Arguments: args})
		a.Sink.OnToolCall(s.name, args)
	}
	return out, nil
}

// streamOut is one aggregated streamed turn.
type streamOut struct {
	text   string
	calls  []llm.ToolCall
	usage  *llm.Usage
	finish llm.FinishReason
}

// executeCall gates, audits, executes, and doom-tracks one tool call. It
// returns (result, escalated) where escalated stops the session.
func (a *Agent) executeCall(ctx context.Context, msgs *[]llm.Message, rec func(string, string, string, any), doom *doomState, call llm.ToolCall) (tools.Result, bool) {
	effect, rule := a.Policy.decide(call.Name, call.Arguments)
	switch effect {
	case EffectDeny:
		rec("event", "", "", map[string]any{"event": "permission.denied", "tool": call.Name, "rule": rule})
		return tools.Result{IsError: true, Content: fmt.Sprintf("denied by permission policy (%s)", rule)}, false
	case EffectAsk:
		dec, err := a.Approver(ApprovalRequest{Tool: call.Name, Args: call.Arguments, Reason: rule})
		if err != nil {
			return tools.Result{IsError: true, Content: fmt.Sprintf("permission prompt failed: %v", err)}, false
		}
		if !dec.Allow {
			rec("event", "", "", map[string]any{"event": "permission.denied", "tool": call.Name, "by": "human"})
			return tools.Result{IsError: true, Content: fmt.Sprintf("denied by human reviewer%s", memoSuffix(dec.Memo))}, false
		}
		rec("event", "", "", map[string]any{"event": "permission.granted", "tool": call.Name, "memo": dec.Memo})
	}

	tool, ok := a.Tools.Get(call.Name)
	if !ok {
		return tools.Result{IsError: true, Content: fmt.Sprintf("unknown tool %q", call.Name)}, false
	}
	ctx, span := telemetry.StartSpan(ctx, "agent.tool."+call.Name)
	span.SetAttribute("tool.name", call.Name)
	res, err := tool.Call(ctx, a.Env, call.Arguments)
	if err != nil {
		span.End()
		return tools.Result{IsError: true, Content: fmt.Sprintf("tool fault: %v", err)}, false
	}
	span.End()
	a.Sink.OnToolResult(call.Name, res.Content, res.IsError)
	rec(store.TurnToolCall, "", call.Name, map[string]any{"id": call.ID, "name": call.Name, "params": call.Arguments})

	doom.observe(call, *res)
	if !doom.tripped() {
		return *res, false
	}
	if !doom.reflected {
		// First trip: ask the model to reconsider before anything else.
		doom.reflected = true
		rec("event", "", "", map[string]any{"event": "doom.reflection", "tool": call.Name})
		a.Sink.OnEvent("doom.reflection", map[string]any{"tool": call.Name})
		*msgs = append(*msgs, llm.Message{
			Role: llm.RoleUser,
			Text: fmt.Sprintf(
				"System note: you have now issued the identical tool call (%s) %d times with identical outcomes. Do not repeat it. State what you would change about your approach, then take a different action.",
				call.Name, doomLimit),
			Metadata: map[string]any{"doom": "reflection"},
		})
		return *res, false
	}

	// Second trip: escalate to the human.
	doom.lastReason = fmt.Sprintf("repeated identical/oscillating tool failures (%s)", call.Name)
	rec("event", "", "", map[string]any{"event": "doom.escalation", "tool": call.Name})
	a.Sink.OnEvent("doom.escalation", map[string]any{"tool": call.Name})
	dec, err := a.Approver(ApprovalRequest{
		Tool:   call.Name,
		Args:   call.Arguments,
		Reason: doom.lastReason,
	})
	if err != nil || !dec.Allow {
		return *res, true
	}
	// Human authorized continuation.
	doom.reflected = false
	doom.run = 0
	doom.ring = nil
	if dec.Memo != "" {
		*msgs = append(*msgs, llm.Message{
			Role:     llm.RoleUser,
			Text:     "Human reviewer note: " + dec.Memo,
			Metadata: map[string]any{"human": "memo"},
		})
	}
	return *res, false
}

func memoSuffix(m string) string {
	if m == "" {
		return ""
	}
	return ": " + m
}

func resultsJSON(rs []llm.ToolResult) string {
	b, err := json.Marshal(rs)
	if err != nil {
		return fmt.Sprintf("%d results", len(rs))
	}
	return string(b)
}

func merge(a, b *llm.Usage) llm.Usage {
	if a == nil {
		if b == nil {
			return llm.Usage{}
		}
		return *b
	}
	if b == nil {
		return *a
	}
	out := *a
	out.PromptTokens += b.PromptTokens
	out.CompletionTokens += b.CompletionTokens
	out.TotalTokens += b.TotalTokens
	out.CacheReadTokens += b.CacheReadTokens
	out.CacheWriteTokens += b.CacheWriteTokens
	return out
}

// callDigest fingerprints one executed call for doom-loop correlation. The
// result digest makes identical failures share signatures while distinct
// outputs break the streak.
func callDigest(name string, args json.RawMessage, res tools.Result) string {
	var canon []byte
	var m map[string]any
	if json.Unmarshal(args, &m) == nil {
		canon, _ = json.Marshal(m) // map keys marshal sorted
	} else {
		canon = args
	}
	h1 := sha256.Sum256(append([]byte(name), append([]byte{'\x00'}, canon...)...))
	h2 := sha256.Sum256([]byte(stableExcerpt(res.Content)))
	return hex.EncodeToString(h1[:6]) + ":" + boolMark(res.IsError) + ":" + hex.EncodeToString(h2[:6])
}

// stableExcerpt strips volatile per-execution fields (timing lines) so
// identical operations correlate despite differing durations, and bounds the
// excerpt.
func stableExcerpt(content string) string {
	var kept []string
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "duration:") {
			continue
		}
		kept = append(kept, l)
	}
	s := strings.Join(kept, "\n")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

func boolMark(b bool) string {
	if b {
		return "e"
	}
	return "ok"
}
