package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"harness/internal/llm"
	"harness/internal/sandbox"
	"harness/internal/store"
	"harness/internal/tools"
)

// fakeProvider replays scripted event turns; calls beyond the scripts yield a
// final text turn. It records each request for assertions.
type fakeProvider struct {
	turns   [][]llm.Event
	calls   int
	lastReq llm.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	f.lastReq = req
	i := f.calls
	f.calls++
	var evs []llm.Event
	if i < len(f.turns) {
		evs = f.turns[i]
	} else {
		evs = []llm.Event{
			{Type: llm.EventTextDelta, Text: "fallback"},
			{Type: llm.EventDone, FinishReason: llm.FinishStop},
		}
	}
	ch := make(chan llm.Event, len(evs))
	go func() {
		for _, e := range evs {
			select {
			case ch <- e:
			case <-ctx.Done():
				ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
				return
			}
		}
		close(ch)
	}()
	return ch, nil
}

func textTurn(text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventTextDelta, Text: text},
		{Type: llm.EventDone, FinishReason: llm.FinishStop},
	}
}

func toolTurn(id, name, args string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolDelta, Tool: &llm.ToolDelta{Index: 0, ID: id, Name: name, Args: args}},
		{Type: llm.EventDone, FinishReason: llm.FinishToolCalls},
	}
}

// captureSink records observable activity.
type captureSink struct {
	mu      sync.Mutex
	deltas  []string
	calls   []string
	results []string
	events  []string
}

func (c *captureSink) OnTextDelta(d string) {
	c.mu.Lock()
	c.deltas = append(c.deltas, d)
	c.mu.Unlock()
}

func (c *captureSink) OnToolCall(name string, _ json.RawMessage) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
}

func (c *captureSink) OnToolResult(name, result string, _ bool) {
	c.mu.Lock()
	c.results = append(c.results, name+"\x00"+result)
	c.mu.Unlock()
}

func (c *captureSink) OnEvent(event string, _ map[string]any) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *captureSink) joined(which int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch which {
	case 0:
		return strings.Join(c.deltas, "\n")
	case 1:
		return strings.Join(c.calls, "\n")
	case 2:
		return strings.Join(c.results, "\n")
	}
	return strings.Join(c.events, "\n")
}

// scriptedAgent builds an agent over a temp workspace with a scripted
// provider and capturing sink.
func scriptedAgent(t *testing.T, mode Mode, allowWrite bool, scripts ...[]llm.Event) (*Agent, *fakeProvider, *captureSink, string) {
	t.Helper()
	ws, err := os.MkdirTemp("", "agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	env := tools.Env{
		Workspace:  ws,
		AllowWrite: allowWrite,
		Sandbox:    sandbox.New(),
	}
	sink := &captureSink{}
	fake := &fakeProvider{turns: scripts}
	ag := &Agent{
		Provider: fake,
		Model:    "fake-model",
		Mode:     mode,
		Env:      env,
		Sink:     sink,
		Policy:   NewPolicy(EffectAllow),
	}
	return ag, fake, sink, ws
}

func writeFixture(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHappyPath(t *testing.T) {
	ag, fake, sink, ws := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "fs_read", `{"path":"lines.txt"}`),
		textTurn("all done"),
	)
	writeFixture(t, ws, "lines.txt", "alpha\nbeta\n")

	rep, err := ag.Run(context.Background(), "read the file")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" || rep.Answer != "all done" || rep.Turns != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(sink.joined(1), "fs_read") {
		t.Errorf("sink saw no tool call: %q", sink.joined(1))
	}
	res := sink.joined(2)
	if !strings.Contains(res, "fs_read") || !strings.Contains(res, "alpha") {
		t.Errorf("sink tool result missing content:\n%s", res)
	}
	if !strings.Contains(sink.joined(0), "all done") {
		t.Error("sink missed the final text deltas")
	}
	// The tool's args reached the model-facing call with a schema'd spec set.
	if len(fake.lastReq.Tools) != 5 {
		t.Errorf("coder exposed %d tool specs, want 5", len(fake.lastReq.Tools))
	}
	if fake.lastReq.System == "" {
		t.Error("no system prompt sent")
	}
}

func TestAgentReviewerPosture(t *testing.T) {
	ag, fake, _, ws := scriptedAgent(t, ModeReviewer, false,
		toolTurn("c1", "fs_edit", `{"path":"x.txt","old_string":"a","new_string":"b"}`),
		textTurn("review complete"),
	)
	writeFixture(t, ws, "x.txt", "a\n")

	rep, err := ag.Run(context.Background(), "review this")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" {
		t.Fatalf("report = %+v", rep)
	}
	// Mutating tools are absent from the reviewer's spec set...
	var names []string
	for _, ts := range fake.lastReq.Tools {
		names = append(names, ts.Name)
	}
	if strings.Join(names, ",") != "fs_read,fs_search,git_ops" {
		t.Errorf("reviewer tool set = %v", names)
	}
	// ...and a call to one is surfaced as an unknown-tool observation.
	trs := fake.lastReq.Messages[len(fake.lastReq.Messages)-1].ToolResults
	if len(trs) != 1 || !trs[0].IsError || !strings.Contains(trs[0].Content, "unknown tool") {
		t.Fatalf("reviewer observation = %+v", trs)
	}
}

func TestAgentPermissionDeny(t *testing.T) {
	ag, fake, _, ws := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "shell_exec", `{"command":"echo hi"}`),
		textTurn("done"),
	)
	_ = ws
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag.Store = st
	ag.Policy = NewPolicy(EffectDeny)

	rep, err := ag.Run(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" {
		t.Fatalf("report = %+v", rep)
	}
	trs := fake.lastReq.Messages[len(fake.lastReq.Messages)-1].ToolResults
	if len(trs) != 1 || !strings.Contains(trs[0].Content, "denied by permission policy") {
		t.Fatalf("denial observation = %+v", trs)
	}
	log, err := st.AuditLog(context.Background(), rep.SessionID, 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range log {
		kinds[e.Kind] = true
	}
	if !kinds["permission.denied"] {
		t.Errorf("audit missing permission.denied: %+v", log)
	}
}

func TestAgentPermissionAsk(t *testing.T) {
	ctx := context.Background()

	// Allow with memo.
	ag, fake, _, ws := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "shell_exec", `{"command":"echo hi"}`),
		textTurn("done"),
	)
	_ = ws
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag.Store = st
	ag.Policy = NewPolicy(EffectAsk)
	var prompted []ApprovalRequest
	ag.Approver = func(req ApprovalRequest) (Decision, error) {
		prompted = append(prompted, req)
		return Decision{Allow: true, Memo: "proceed carefully"}, nil
	}
	rep, err := ag.Run(ctx, "run it")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" || rep.Answer != "done" {
		t.Fatalf("report = %+v", rep)
	}
	if len(prompted) != 1 || prompted[0].Tool != "shell_exec" {
		t.Fatalf("prompts = %+v", prompted)
	}
	res := ""
	for _, r := range fake.lastReq.Messages {
		for _, tr := range r.ToolResults {
			res = tr.Content
		}
	}
	if !strings.Contains(res, "hi") {
		t.Errorf("approved command did not execute:\n%s", res)
	}
	log, _ := st.AuditLog(ctx, rep.SessionID, 100)
	var granted bool
	for _, e := range log {
		if e.Kind == "permission.granted" {
			granted = true
		}
	}
	if !granted {
		t.Error("audit missing permission.granted")
	}

	// Deny by the human.
	ag2, fake2, _, _ := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "shell_exec", `{"command":"echo hi"}`),
		textTurn("done"),
	)
	ag2.Policy = NewPolicy(EffectAsk)
	ag2.Approver = func(ApprovalRequest) (Decision, error) {
		return Decision{Allow: false, Memo: "not now"}, nil
	}
	if _, err := ag2.Run(ctx, "run it"); err != nil {
		t.Fatal(err)
	}
	trs := fake2.lastReq.Messages[len(fake2.lastReq.Messages)-1].ToolResults
	if len(trs) != 1 || !strings.Contains(trs[0].Content, "denied by human reviewer: not now") {
		t.Fatalf("human denial observation = %+v", trs)
	}
}

func TestAgentDoomLoop(t *testing.T) {
	ctx := context.Background()
	failing := []string{}
	for i := 0; i < 4; i++ {
		failing = append(failing, "")
	}
	_ = failing

	scripts := [][]llm.Event{
		toolTurn("c1", "shell_exec", `{"command":"exit 7"}`),
		toolTurn("c2", "shell_exec", `{"command":"exit 7"}`),
		toolTurn("c3", "shell_exec", `{"command":"exit 7"}`),
		toolTurn("c4", "shell_exec", `{"command":"exit 7"}`),
	}

	// Second trip with human denial stops the session.
	ag, _, sink, _ := scriptedAgent(t, ModeCoder, true, scripts...)
	ag.Policy = NewPolicy(EffectAllow)
	ag.Approver = func(req ApprovalRequest) (Decision, error) {
		if !strings.Contains(req.Reason, "tool failures (shell_exec)") {
			t.Errorf("escalation reason = %q", req.Reason)
		}
		return Decision{Allow: false}, nil
	}
	rep, err := ag.Run(ctx, "fix the build")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Escalated || rep.StoppedBy != "escalation" || rep.Turns != 4 {
		t.Fatalf("report = %+v", rep)
	}
	ev := sink.joined(3)
	if !strings.Contains(ev, "doom.reflection") || !strings.Contains(ev, "doom.escalation") {
		t.Errorf("sink events = %q", ev)
	}

	// Second trip with human authorization continues.
	ag2, fake2, sink2, _ := scriptedAgent(t, ModeCoder, true,
		append(scripts, textTurn("recovered"))...)
	ag2.Policy = NewPolicy(EffectAllow)
	ag2.Approver = func(ApprovalRequest) (Decision, error) {
		return Decision{Allow: true, Memo: "try the other branch"}, nil
	}
	rep2, err := ag2.Run(ctx, "fix the build")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.StoppedBy != "final" || rep2.Turns != 5 || rep2.Answer != "recovered" {
		t.Fatalf("report2 = %+v", rep2)
	}
	if !strings.Contains(sink2.joined(3), "doom.reflection") {
		t.Errorf("allow-continue events = %q", sink2.joined(3))
	}
	// The memo reached the model on the resumed turn.
	var memo bool
	for _, m := range fake2.lastReq.Messages {
		if m.Role == llm.RoleUser && strings.HasPrefix(m.Text, "Human reviewer note: try the other branch") {
			memo = true
		}
	}
	if !memo {
		t.Error("memo did not reach the resumed turn")
	}
}

func TestAgentOscillation(t *testing.T) {
	ctx := context.Background()
	e1 := toolTurn("c1", "fs_edit", `{"path":"empty.txt","old_string":"absent1","new_string":"z"}`)
	e2 := toolTurn("c2", "fs_edit", `{"path":"empty.txt","old_string":"absent2","new_string":"z"}`)
	ag, _, sink, ws := scriptedAgent(t, ModeCoder, true, e1, e2, e1, e2, e1)
	writeFixture(t, ws, "empty.txt", "")
	ag.Policy = NewPolicy(EffectAllow)
	var prompted []ApprovalRequest
	ag.Approver = func(req ApprovalRequest) (Decision, error) {
		prompted = append(prompted, req)
		if !strings.Contains(req.Reason, "fs_edit") {
			t.Errorf("escalation reason = %q", req.Reason)
		}
		return Decision{Allow: false}, nil
	}
	rep, err := ag.Run(ctx, "edit things")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Escalated || rep.StoppedBy != "escalation" || rep.Turns != 5 {
		t.Fatalf("report = %+v", rep)
	}
	if len(prompted) != 1 {
		t.Fatalf("approver consulted %d times, want 1", len(prompted))
	}
	ev := sink.joined(3)
	if !strings.Contains(ev, "doom.reflection") || !strings.Contains(ev, "doom.escalation") {
		t.Errorf("sink events = %q", ev)
	}
}

func TestWorkflowValidation(t *testing.T) {
	cases := []struct {
		name string
		wf   Workflow
		want string
	}{
		{"cycle", Workflow{Steps: []Step{
			{ID: "a", Command: "x", Needs: []string{"b"}},
			{ID: "b", Command: "y", Needs: []string{"a"}},
		}}, "cycle"},
		{"unknown dep", Workflow{Steps: []Step{
			{ID: "a", Command: "x", Needs: []string{"zzz"}},
		}}, "unknown step"},
		{"duplicate", Workflow{Steps: []Step{
			{ID: "a", Command: "x"},
			{ID: "a", Command: "y"},
		}}, "duplicate"},
		{"no executor", Workflow{Steps: []Step{
			{ID: "a"},
		}}, "exactly one"},
	}
	for _, tc := range cases {
		err := tc.wf.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
	sound := Workflow{Steps: []Step{{ID: "a", Command: "x"}}}
	if err := sound.Validate(); err != nil {
		t.Errorf("sound workflow rejected: %v", err)
	}
}

func TestAgentCompaction(t *testing.T) {
	ctx := context.Background()
	var scripts [][]llm.Event
	for i := 1; i <= 4; i++ {
		scripts = append(scripts, toolTurn(
			fmt.Sprintf("c%d", i), "fs_read", fmt.Sprintf(`{"path":"big%d.txt"}`, i)))
	}
	scripts = append(scripts,
		textTurn("prior context folded"),
		textTurn("final answer"),
	)
	ag, fake, sink, ws := scriptedAgent(t, ModeCoder, true, scripts...)
	for i := 1; i <= 4; i++ {
		var sb strings.Builder
		for l := 0; l < 200; l++ {
			fmt.Fprintf(&sb, "line %d of big%d.txt with padding padding padding\n", l, i)
		}
		writeFixture(t, ws, fmt.Sprintf("big%d.txt", i), sb.String())
	}
	ag.Window = 1000
	ag.CompactAt = 0.5

	rep, err := ag.Run(ctx, "read everything")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" || rep.Turns != 5 || rep.Answer != "final answer" {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(sink.joined(3), "compaction") {
		t.Errorf("sink events = %q", sink.joined(3))
	}
	first := fake.lastReq.Messages[0]
	if v, _ := first.Metadata["compaction"].(bool); !v {
		t.Fatalf("first message not the compaction summary: %+v", first)
	}
	if !strings.HasPrefix(first.Text, "Session context compressed by the runtime.") {
		t.Errorf("summary text = %q", first.Text)
	}
	if len(fake.lastReq.Messages) != 7 {
		t.Errorf("post-compaction context = %d messages, want 7", len(fake.lastReq.Messages))
	}
}

func TestAgentWorkflowExec(t *testing.T) {
	ctx := context.Background()
	ag, _, _, _ := scriptedAgent(t, ModeAutomator, true, textTurn("BETA"))
	wf := Workflow{
		Name: "wf",
		Steps: []Step{
			{ID: "A", Command: "printf alpha"},
			{ID: "B", Task: "produce the marker"},
			{ID: "C", Command: "printf '%s|%s' {A} {B}", Needs: []string{"A", "B"}},
		},
	}
	rep, err := ag.RunWorkflow(ctx, wf)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != "" {
		t.Fatalf("workflow failed at %q: %+v", rep.Failed, rep.Steps)
	}
	if len(rep.Steps) != 3 || rep.Steps[0].ID != "A" || rep.Steps[1].ID != "B" || rep.Steps[2].ID != "C" {
		t.Fatalf("step order = %+v", rep.Steps)
	}
	if rep.Steps[2].Output != "alpha|BETA" {
		t.Errorf("substituted output = %q", rep.Steps[2].Output)
	}

	failing := Workflow{
		Name: "bad",
		Steps: []Step{
			{ID: "X", Command: "exit 3"},
			{ID: "Y", Command: "echo after", Needs: []string{"X"}},
		},
	}
	rep2, err := ag.RunWorkflow(ctx, failing)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Failed != "X" || len(rep2.Steps) != 1 || rep2.Steps[0].Status != "failed" {
		t.Fatalf("failing workflow = %+v", rep2)
	}
}

func TestAgentResume(t *testing.T) {
	ctx := context.Background()
	ag, fake, _, ws := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "fs_read", `{"path":"note.txt"}`),
		textTurn("first answer"),
		textTurn("second answer"),
	)
	writeFixture(t, ws, "note.txt", "hello\n")
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag.Store = st

	rep, err := ag.Run(ctx, "read the note")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" || rep.SessionID == "" {
		t.Fatalf("report = %+v", rep)
	}
	turns, err := st.LoadTurns(ctx, rep.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	seed := FoldTurns(turns)
	if len(seed) == 0 {
		t.Fatal("no folded context")
	}

	ag.Seed = seed
	ag.ResumeSession = rep.SessionID
	rep2, err := ag.Run(ctx, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.StoppedBy != "final" || rep2.Answer != "second answer" {
		t.Fatalf("report2 = %+v", rep2)
	}
	if rep2.SessionID != rep.SessionID {
		t.Fatalf("resume started a new session: %q vs %q", rep2.SessionID, rep.SessionID)
	}
	if got := fake.lastReq.Messages[0].Text; got != "read the note" {
		t.Errorf("resumed context missing prior task: %q", got)
	}
	turns2, err := st.LoadTurns(ctx, rep2.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns2) != len(turns)+2 {
		t.Fatalf("resumed turns = %d, want %d", len(turns2), len(turns)+2)
	}
	if turns2[len(turns2)-1].Seq != int64(len(turns2)) {
		t.Errorf("sequence discontinuity: last seq %d", turns2[len(turns2)-1].Seq)
	}
}

func TestMergeUsage(t *testing.T) {
	if u := merge(nil, &llm.Usage{PromptTokens: 10, TotalTokens: 15}); u.PromptTokens != 10 || u.TotalTokens != 15 {
		t.Errorf("merge(nil, b) = %+v", u)
	}
	a := llm.Usage{PromptTokens: 3}
	if u := merge(&a, nil); u.PromptTokens != 3 {
		t.Errorf("merge(a, nil) = %+v", u)
	}
	if u := merge(&a, &llm.Usage{CompletionTokens: 4, TotalTokens: 9}); u.PromptTokens != 3 || u.CompletionTokens != 4 || u.TotalTokens != 9 {
		t.Errorf("merge(a, b) = %+v", u)
	}
}

func usageTextTurn(text string, total int) []llm.Event {
	return []llm.Event{
		{Type: llm.EventUsage, Usage: &llm.Usage{
			PromptTokens:     total - total/2,
			CompletionTokens: total / 2,
			TotalTokens:      total,
		}},
		{Type: llm.EventTextDelta, Text: text},
		{Type: llm.EventDone, FinishReason: llm.FinishStop},
	}
}

func usageToolTurn(id, name, args string, total int) []llm.Event {
	return []llm.Event{
		{Type: llm.EventUsage, Usage: &llm.Usage{
			PromptTokens:     total - total/2,
			CompletionTokens: total / 2,
			TotalTokens:      total,
		}},
		{Type: llm.EventToolDelta, Tool: &llm.ToolDelta{Index: 0, ID: id, Name: name, Args: args}},
		{Type: llm.EventDone, FinishReason: llm.FinishToolCalls},
	}
}

func TestAgentUsageAccumulation(t *testing.T) {
	ctx := context.Background()
	ag, _, _, ws := scriptedAgent(t, ModeCoder, true,
		usageToolTurn("c1", "fs_read", `{"path":"note.txt"}`, 15),
		usageTextTurn("two", 30),
	)
	writeFixture(t, ws, "note.txt", "persisted\n")
	rep, err := ag.Run(ctx, "count")
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "final" || rep.Turns != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Usage.TotalTokens != 45 {
		t.Errorf("accumulated usage = %+v, want TotalTokens=45", rep.Usage)
	}
}

func TestAgentPersistence(t *testing.T) {
	ctx := context.Background()
	ag, _, _, ws := scriptedAgent(t, ModeCoder, true,
		toolTurn("c1", "fs_read", `{"path":"note.txt"}`),
		textTurn("done"),
	)
	writeFixture(t, ws, "note.txt", "persisted\n")
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag.Store = st

	rep, err := ag.Run(ctx, "read the note")
	if err != nil {
		t.Fatal(err)
	}
	if rep.SessionID == "" {
		t.Fatal("no session id in report")
	}
	sess, err := st.GetSession(ctx, rep.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Mode != "coder" {
		t.Errorf("session mode = %q, want coder", sess.Mode)
	}
	turns, err := st.LoadTurns(ctx, rep.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, tn := range turns {
		kinds[tn.Kind] = true
	}
	for _, want := range []string{"message", "tool_call", "tool_result"} {
		if !kinds[want] {
			t.Errorf("turns missing kind %q: %v", want, kinds)
		}
	}
}
