# Example usage

Correct usage patterns for each component, at both levels the harness is
consumed: the CLI (`cmd/harness`) and the Go library (`internal/...`). Every
example below matches a verified implementation surface; nothing here is
aspirational.

---

## 1. The CLI

### Run a coding task

```sh
harness --provider ollama --model qwen3 \
        --workspace ~/projects/myrepo \
        "fix the failing test in pkg/foo and verify with go test ./pkg/foo"
```

- Model text streams to **stdout**; tool activity, permission events, span
  records, and the final session line go to **stderr**.
- Coder sessions default to **ask**: every tool call stops for human review.
- Exit 0 on final answer; see the README exit-code table for the rest.

### Scripted / CI execution

```sh
# Approve everything, transient database, bounded turn budget:
harness --provider openai --endpoint http://127.0.0.1:8080/v1 \
        --model llama3.2 --mode coder --allow-all \
        --store :memory: --max-turns 25 \
        --workspace /tmp/job \
        "apply the patch in changes.patch"
```

### Reviewer posture

```sh
harness --provider ollama --model qwen3 --mode reviewer \
        --workspace ~/projects/myrepo \
        "audit pkg/auth for token-handling weaknesses"
```

The workspace mounts read-only and only `fs_read`, `fs_search`, `git_ops`
are exposed. Reviewer sessions default to **allow** (no prompts), since
nothing can mutate state.

### Workflow automation

```sh
cat > gate.json <<'EOF'
{
  "name": "release-gate",
  "steps": [
    {"id": "build", "command": "make build"},
    {"id": "unit",  "command": "make test", "needs": ["build"]},
    {"id": "risk",  "task":    "Summarize residual risks from the build output", "needs": ["build"]}
  ]
}
EOF
harness --provider ollama --model qwen3 --mode automator \
        --workflow gate.json --workspace ~/projects/myrepo
```

`command` steps execute sandboxed; `task` steps run nested agent tasks
(each with its own session). `{stepID}` substitutes earlier outputs into
later steps. A failing step aborts the remainder and exits 2 with the step
named.

### Session resume and rewind

```sh
# Continue a previous session (its persisted context is replayed first):
harness --provider ollama --model qwen3 --resume <session-id> \
        "now also update the docs to match"

# Rewind two turns, then continue from there:
harness --provider ollama --model qwen3 --resume <session-id> --rewind 4 \
        "take the other approach"
```

`--rewind` truncates the stored conversation to turns with sequence ≤ the
value (audit history is untouched), then the resumed context folds the
surviving turns. Resume applies to task runs, not workflows.

### Human-review interaction

The default coder posture prompts per tool call:

```
--> shell_exec {"command":"make test"}

[permission] shell_exec {"command":"make test"}
           reason: default-ask
approve? (y / y: memo / n)
```

- `y` — allow.
- `y: run it twice and compare` — allow, with the memo surfaced to the model
  as a `Human reviewer note` on the resumed turn.
- `n` (or anything) — deny; the model receives a denial observation and is
  expected to adapt.

The identical grammar answers **doom-loop escalations**, whose reason names
the repeating pattern (e.g. `repeated identical/oscillating tool failures
(shell_exec)`). Denying an escalation stops the session with exit 3.

---

## 2. Model providers (`internal/llm`)

```go
package main

import (
	"context"
	"time"

	"harness/internal/llm"
)

func main() {
	// Any of the three dialects:
	oa, _ := llm.NewOpenAI("http://127.0.0.1:8080/v1", os.Getenv("OPENAI_API_KEY"), "", llm.DefaultRetry)
	an, _ := llm.NewAnthropic("", os.Getenv("ANTHROPIC_API_KEY"), "", llm.DefaultRetry)
	ol, _ := llm.NewOllama("http://127.0.0.1:11434", "", llm.DefaultRetry)

	var p llm.Provider = ol // pick one

	// Long streams: lifetime is governed exclusively by context cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	evs, err := p.Stream(ctx, llm.Request{
		Model: "qwen3",
		System: "You are a terse engineering assistant.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hello"}},
	})
	if err != nil { // pre-stream failure after retries exhausted
		log.Fatal(err)
	}
	for ev := range evs {
		switch ev.Type {
		case llm.EventTextDelta:
			io.WriteString(os.Stdout, ev.Text)
		case llm.EventUsage:
			log.Printf("usage: %d total tokens", ev.Usage.TotalTokens)
		case llm.EventError:
			log.Printf("stream fault: %v", ev.Err)
		}
	}
}
```

Construction notes:

- Empty base URLs take per-provider defaults (hosted OpenAI, hosted
  Anthropic, local Ollama). Empty API keys are tolerated — local
  OpenAI-compatible servers commonly need none.
- Retries apply to **pre-stream** failures only (connection setup, headers,
  rate limits) with exponential backoff plus full jitter, honoring
  `Retry-After`. Tune with `llm.RetryConfig{Attempts, BaseDelay, MaxDelay}`.
  Once a stream has emitted, faults surface as `EventError` rather than
  re-driving a half-consumed completion.
- Non-streaming completion (used internally by compaction) is
  `llm.Complete(ctx, provider, req)` → response with `.Text`.
- Token accounting for context budgets: `llm.EstimateConversationTokens` /
  `llm.EstimateTextTokens`.

---

## 3. The agent runtime (`internal/agent`)

```go
ag := &agent.Agent{
	Provider: ol,
	Model:    "qwen3",
	Mode:     agent.ModeCoder,
	Env: tools.Env{
		Workspace:  ws,
		AllowWrite: true,
		AllowNet:   false,
		Sandbox:    sandbox.New(),
	},
	Store:  st,          // optional; nil-safe
	Sink:   mySink,      // optional; nil-safe
	Approver: func(req agent.ApprovalRequest) (agent.Decision, error) {
		// req.Tool, req.Args, req.Reason identify the prompt.
		return agent.Decision{Allow: true, Memo: "prefer the narrower edit"}, nil
	},
	MaxTurns: 25,
	Window:   128000,    // compaction engages at ~80%
}

rep, err := ag.Run(ctx, "make the failing test pass")
// rep.StoppedBy: final | max_turns | escalation | canceled
// rep.Escalated / rep.Escalation name the doom-loop handoff
// rep.SessionID identifies the persisted session for later resume
```

Library-level resume/rewind mirrors the CLI:

```go
turns, _ := st.LoadTurns(ctx, sessID)
seed := agent.FoldTurns(turns)   // re-pairs persisted tool interactions
ag.Seed = seed
ag.ResumeSession = sessID
rep2, _ := ag.Run(ctx, "continue from where it stopped")
```

`FoldTurns` replays text turns verbatim, re-pairs tool calls into their
assistant messages and tool results into user messages (original call IDs
preserved), and drops a dangling call tail whose results never persisted.

---

## 4. Permission policies (`internal/agent`)

```go
policy := agent.NewPolicy(agent.EffectAsk)      // default effect
policy.Allow("fs_read", "")                     // read freely
policy.Allow("fs_search", "")
policy.Deny("shell_exec", "")                   // never execute
policy.Ask("fs_edit", "*.go")                   // glob-scoped to Go sources
// Most specific rule wins: exact tool +2, path-scoped +1; ties keep the
// earlier-registered rule. Unmatched calls fall through to the default.
ag.Policy = policy
```

Path patterns glob-match against the call's `path`/`file`/`dir` arguments.
Every denial and grant is written to the append-only audit table with the
matching rule named.

---

## 5. Tool invocations (the model's surface)

These are the JSON shapes the runtime executes, sandboxed and confined:

```jsonc
// fs_read — windowed, line-numbered read
{"path": "pkg/auth/session.go", "offset": 1, "limit": 200}

// fs_edit — exact, uniqueness-enforced replacement
{"path": "pkg/auth/session.go", "old_string": "TODO(rotate)", "new_string": "rotateKeys()"}

// fs_search — gitignore-aware regexp over lines
{"pattern": "TODO\\(rotate\\)", "glob": "*.go", "max_results": 50}

// shell_exec — ALWAYS inside the sandbox; exit codes are data, not faults
{"command": "go test ./pkg/auth", "timeout_ms": 60000}

// git_ops — restricted inspection of the workspace repo
{"op": "diff", "ref": "HEAD~1"}
```

Operational outcomes (not found, exit ≠ 0, timeout) come back as error-
marked **results** the model reasons over; only faults outside the tool's
control surface as Go errors.

---

## 6. The sandbox directly (`internal/sandbox`)

```go
sb := sandbox.New()

info := sb.Inspect() // Backend, live Capabilities, Notes — print this once
log.Printf("sandbox: %s (%v)", info.Backend, info.Capabilities)

res, err := sb.Run(ctx, sandbox.Config{
	Workspace:  ws,
	AllowWrite: true,     // false mounts the workspace read-only
	AllowNet:   false,    // true skips network isolation
	Timeout:    30 * time.Second,
	MaxOutput:  1 << 20,
	ExtraEnv:   []string{"GOFLAGS=-mod=readonly"},
}, []string{"/bin/sh", "-c", "make test"})
if err != nil {
	log.Fatalf("sandbox: %v", err)
}
log.Printf("exit=%d timedout=%v", res.ExitCode, res.TimedOut)
io.Copy(os.Stdout, bytes.NewReader(res.Stdout))
for _, w := range res.Warnings {
	log.Printf("sandbox warning: %s", w) // degraded-isolation notices
}
```

`Result` also carries `Duration`, truncation flags, the executed
`Command`, and the `Backend`. Non-zero exits are ordinary results — the
agent loop correlates their digests for doom-loop detection rather than
aborting.

---

## 7. Workflows as a library (`internal/agent`)

```go
wf := agent.Workflow{
	Name: "gate",
	Steps: []agent.Step{
		{ID: "build", Command: "make build"},
		{ID: "unit",  Command: "make test", Needs: []string{"build"}},
		{ID: "risk",  Task: "Assess residual risk from {build}", Needs: []string{"build"}},
	},
}
if err := wf.Validate(); err != nil { // duplicate IDs, unknown deps, cycles
	return err
}
rep, err := ag.RunWorkflow(ctx, wf)
for _, s := range rep.Steps {
	log.Printf("step %s: %s", s.ID, s.Status) // ok | failed
}
if rep.Failed != "" {
	log.Printf("aborted at %s", rep.Failed)   // reported, not raised
}
```

`substitute` replaces `{stepID}` with completed outputs (trailing newline
trimmed) in sorted-key order, so later steps compose cleanly over earlier
output.

---

## 8. Persistence (`internal/store`)

```go
st, err := store.Open("/var/lib/harness/sessions.db") // ":memory:" supported
defer st.Close()

sess, _ := st.CreateSession(ctx, "coder", "ollama", "qwen3", ws, nil)

seq, _ := st.AppendTurn(ctx, sess.ID, store.TurnMessage, "user", "the task", nil)
turns, _ := st.LoadTurns(ctx, sess.ID)        // ordered replay
n, _ := st.Rewind(ctx, sess.ID, seq)          // delete turns with seq > seq
log, _ := st.AuditLog(ctx, sess.ID, 100)      // append-only, newest first
```

Turn kinds are first-class rows (`store.TurnMessage`, `TurnToolCall`,
`TurnToolResult`, `TurnEvent`) so rewind and audit see tool interactions
exactly like messages. Writes take immediate transaction locks; WAL
tolerates concurrent readers; `ErrNotFound` marks missing sessions.

---

## 9. Telemetry (`internal/telemetry`)

```go
logger := telemetry.NewLogger(telemetry.LoggerOptions{Writer: os.Stderr})

exporter := func(rec telemetry.Record) {
	// rec: Name, TraceID/SpanID/ParentID (hex), Start/End, Attributes, Events
	wireToBackend(rec)
}
tracer := telemetry.NewTracer(logger, exporter) // exporters optional, sync

ctx = telemetry.ContextWithTracer(ctx, tracer)
ctx, span := telemetry.StartSpan(ctx, "my.operation")
span.SetAttribute("op.target", target)
span.Event("op.checkpoint", map[string]any{"stage": "half"})
span.End() // completed spans emit as structured slog records + exporters
```

Completed spans surface on the logger as `msg="span completed"` records
carrying `span.name`, `span.trace_id`, `span.span_id`, `span.parent_id`,
and sorted `span.attr.*` fields. Spans started before tracer installation
flow to an immutable nop tracer — nothing panics, nothing leaks state.

---

## 10. Sink and approver surfaces

```go
type displaySink struct{ mu sync.Mutex }

func (d *displaySink) OnTextDelta(delta string)            { /* stdout */ }
func (d *displaySink) OnToolCall(name string, args json.RawMessage) { /* stderr */ }
func (d *displaySink) OnToolResult(name, result string, isError bool)  { /* stderr */ }
func (d *displaySink) OnEvent(event string, detail map[string]any)     { /* stderr */ }
// Events include: permission.denied, permission.granted, doom.reflection,
// doom.escalation, compaction.
```

`Approver` is `func(agent.ApprovalRequest) (agent.Decision, error)`. A nil
approver panics when the first gated call reaches an `ask` decision — always
wire one for interactive use, or choose a non-asking policy (`allow`/`deny`)
for unattended runs. A wired approver that returns an error yields a
`permission prompt failed` observation and continues.
