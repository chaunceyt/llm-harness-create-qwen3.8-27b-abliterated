# Tutorial: the harness end-to-end

A walk-through that exercises every major feature against a local model:
sandboxed execution, human review, reviewer posture, workflow automation,
doom-loop handling, compaction, persistence with resume/rewind, and
tracing. Every command drives an **OpenAI-compatible endpoint** via the
`--endpoint` base-URL override; set `EP` to your local server and replace
`<model>` with a model it serves. Each stage states what you should see and
why.

```sh
# One-time setup
go build -o harness ./cmd/harness
export WORKSPACE=/tmp/tutorial-ws && mkdir -p $WORKSPACE
```

---

## Stage 0 — Preconditions and posture check

```sh
go version                 # ≥ go1.26 required
./harness --help           # the full flag surface; exit 0
```

### Pointing the harness at a local LLM

The tutorial drives an OpenAI-compatible endpoint — llama.cpp's
`llama-server`, vLLM, LM Studio, or any server speaking the
`/v1/chat/completions` dialect. Start one, then name it:

```sh
llama-server -m <your-model>.gguf --port 8080    # OpenAI-compatible surface on /v1
EP=http://127.0.0.1:8080/v1                      # remember this for every command below
curl -s "$EP/models"                             # sanity: answers and lists models
```

`--endpoint` overrides the provider base URL. The OpenAI dialect defaults to
the hosted API (`https://api.openai.com/v1`), so local use **requires** the
flag; the runtime POSTs to `<endpoint>/chat/completions`. Local servers
commonly need no auth — with no `OPENAI_API_KEY` set, the harness prints a
one-line warning and proceeds. If your server validates headers, export a
dummy key (`export OPENAI_API_KEY=local`).

Know your host's sandbox posture before interpreting anything later:

```sh
./harness --provider openai --endpoint $EP --model <model> --workspace $WORKSPACE --store :memory: \
    "run: uname -s"
```

On seccomp-restricted hosts (containers, sandboxes) each command result
carries:

```
warning: namespace isolation unavailable (operation not permitted); running degraded with limits only
```

That is the **degraded lane**: LandLock-only confinement with timeouts and
output caps still enforced. It is expected host behavior here — timeouts,
output bounds, and process-tree cleanup apply on every lane.

---

## Stage 1 — A sandboxed coding task

```sh
printf 'package main\n\nfunc Add(a, b int) int { return a + b }\n' > $WORKSPACE/add.go
printf 'go 1.26\n\nmodule adder\n' > $WORKSPACE/go.mod

./harness --provider openai --endpoint $EP --model <model> --allow-all --store :memory: \
    --workspace $WORKSPACE \
    "add a unit test for Add in add_test.go, then run go test and report"
```

What you should see:

- **stdout**: the model's streamed text (its final report).
- **stderr**, interleaved:
  ```
  --> fs_read {"path":"add.go"}
  <-- fs_read:
       1\tpackage main
  --> fs_edit {"path":"add_test.go", ...}
  --> shell_exec {"command":"go test ./..."}
  <-- shell_exec:
       ok  	adder	0.3s
  [session] id=<12-hex…> stopped=final turns=N tokens=M
  ```
- Exit code **0**.

Every `shell_exec` ran inside the sandbox — including `go test`. There is no
out-of-jail execution path anywhere in the runtime.

---

## Stage 2 — Human review (HITL)

Drop `--allow-all` and coder's default posture (**ask**) takes over: every
tool call stops for you.

```sh
./harness --provider openai --endpoint $EP --model <model> --store :memory: \
    --workspace $WORKSPACE \
    "refactor Add into a Sum(a, b, c int) variant"
```

You will be prompted per call:

```
[permission] fs_edit {"path":"add.go","old_string":"...","new_string":"..."}
           reason: default-ask
approve? (y / y: memo / n)
```

Exercise all three answers across the prompts:

- `n` — the model receives `denied by human reviewer` and must adapt.
- `y: keep the original Add as a thin wrapper` — the memo is surfaced to
  the model as a `Human reviewer note` on the resumed turn and steers the
  next action.
- `y` — plain continuation.

The store (not `:memory:` this time, so the audit survives) now holds an
append-only trail: every grant and denial, with the matched rule. Inspect
with any SQLite client:

```sh
sqlite3 ~/.harness/sessions.db \
  'SELECT kind, detail FROM audit WHERE session_id = "<id>" ORDER BY id;'
# kinds include permission.granted / permission.denied
```

(If you'd rather not install sqlite3, the library exposes the same view:
`st.AuditLog(ctx, sessionID, limit)`.)

---

## Stage 3 — Reviewer posture

```sh
git -C $WORKSPACE init -q 2>/dev/null
git -C $WORKSPACE add -A && git -C $WORKSPACE -c user.email=t@t -c user.name=t commit -qm seed

./harness --provider openai --endpoint $EP --model <model> --allow-all --store :memory: \
    --mode reviewer --workspace $WORKSPACE \
    "audit the repo: list every exported symbol and any hardcoded secrets"
```

Properties of this run:

- The workspace mounts **read-only**; mutating tools are simply **absent**
  from the reviewer's spec set (`fs_read`, `fs_search`, `git_ops` only).
- Reviewer defaults to **allow** — nothing can mutate, so no prompts.
- If the model nonetheless attempts a mutating call, it receives an
  `unknown tool` observation and adapts; no execution occurs.

---

## Stage 4 — Sandbox semantics up close

```sh
# Exit codes are data: a failing build is a result the model reasons over.
./harness ... "run: sh -c 'exit 7' and tell me the exit code"

# Timeouts: budget-bounded execution with a timed-out marker.
./harness ... "run: sh -c 'sleep 5' with timeout_ms=1000"

# Output caps: large streams are truncated with explicit markers.
./harness ... "run: yes harness | head -c 300000"
```

Expect `(timed out)` plus a parsed `duration:` line for the timeout case,
`[stdout truncated]` markers for capped streams, and `exit: 7` surfaced as
result content (not a crash) for the failing command. `--allow-net` lifts
the network isolation for sandboxed commands; by default each execution is
network-isolated.

---

## Stage 5 — The doom loop

The runtime fingerprints every executed call (canonical arguments +
outcome digest, with volatile timing lines stripped). Two patterns trip the
ladder:

1. **Repetition** — the same call with the same outcome three times in a row.
   First trip: the model receives a reflection prompt
   (`doom.reflection` event) and must state what it would change.
2. **Oscillation** — an alternating pair of distinct error outcomes
   (ABAB over the last four executions).

A second trip while the pattern persists escalates to **you**:

```
[permission] shell_exec {"command":"go build ./..."}
           reason: repeated identical/oscillating tool failures (shell_exec)
approve? (y / y: memo / n)
```

- `n` stops the session: exit code **3**, `[session] ... stopped=escalation`.
- `y: try building with GOFLAGS=-mod=mod` continues under your memo.

You'll see `[event] doom.reflection` and `[event] doom.escalation` records
on stderr and matching audit rows. You don't need a broken build to observe
the machinery — a task that nudges the model into repeating one command will
show the ladder within a few turns.

---

## Stage 6 — Workflow automation

```sh
cat > $WORKSPACE/gate.json <<'EOF'
{
  "name": "gate",
  "steps": [
    {"id": "build", "command": "go build ./..."},
    {"id": "tests", "command": "go test ./...", "needs": ["build"]},
    {"id": "risk",  "task": "Given the build summary {build}, list the two most dangerous areas", "needs": ["build"]}
  ]
}
EOF

./harness --provider openai --endpoint $EP --model <model> --allow-all \
    --mode automator --workflow $WORKSPACE/gate.json --workspace $WORKSPACE
```

What you should see:

```
[step] build ok
[step] tests ok
[step] risk ok
[workflow] "gate" completed (3 steps)
```

- Steps execute in dependency order (declaration-stable among ready nodes);
  `Validate()` rejects duplicate IDs, unknown dependencies, cycles, and steps
  with zero or two executors **before** anything runs.
- `task` steps are nested agent tasks, each with its own session.
- `{stepID}` substitutes earlier outputs (trimmed) into later steps.
- A failing step prints `[step] X failed: ...`, names the abort in
  `[workflow] "gate" aborted at step X`, and exits **2** — reported, not
  raised.

Try the failure path deliberately:

```sh
cat > bad.json <<'EOF'
{"name":"bad","steps":[
  {"id":"x","command":"sh -c 'exit 3'"},
  {"id":"y","command":"echo never runs","needs":["x"]}]}
EOF
./harness ... --mode automator --workflow bad.json --workspace $WORKSPACE
# exit 2; step y never executes
```

---

## Stage 7 — Context compaction

Long sessions compress automatically when estimated context reaches ~80% of
the window (`--window`, default 128000). Force it early to watch:

```sh
./harness --provider openai --endpoint $EP --model <model> --allow-all \
    --store :memory: --workspace $WORKSPACE --window 2000 \
    "walk pkg/ file by file: read each source, summarize its responsibilities"
```

When the threshold trips you'll see on stderr:

```
[event] compaction {"folded":N,...}
```

Compaction folds older turns through a summarization call into one marked
context message, **preserving** pinned content (e.g. pinned file contents)
and the most recent turns verbatim. The system persona is never folded. An
audit row of kind `compaction` records each compression with its folded
count.

---

## Stage 8 — Persistence: resume and rewind

Run something substantial with the default persistent store, note the session
id from the final `[session]` line, then:

```sh
# Resume: the persisted context replays, then your continuation proceeds.
./harness --provider openai --endpoint $EP --model <model> --resume <session-id> \
    "now write the summary of that analysis to AUDIT.md"

# Rewind two turns and continue down a different branch:
./harness --provider openai --endpoint $EP --model <model> --resume <session-id> --rewind <seq-2> \
    "instead, address the second finding"
```

Semantics:

- Resume folds the persisted turns back into conversation context — tool
  calls re-pair with their results (original IDs preserved); a dangling call
  tail whose results never persisted is dropped, so the replayed context
  ends on a well-formed alternation.
- New turns append under the **same** session id with contiguous sequences.
- `--rewind` truncates to turns with sequence ≤ the value; the append-only
  audit table is untouched either way.

---

## Stage 9 — Observability

Every session and every tool execution emits span-completion records to the
component logger (stderr), in OTel-compatible shape:

```
level=INFO msg="span completed" span.name=agent.session \
  span.trace_id=<32-hex> span.span_id=<16-hex> span.parent_id=<16-hex> \
  span.attr.agent.mode=coder span.attr.agent.model=<model> \
  span.attr.agent.session=<id> span.duration=...
```

Instrument deeper by attaching synchronous exporters at tracer construction
(`telemetry.NewTracer(logger, func(rec telemetry.Record) {...})`) — records
carry trace/span/parent linkage, start/end times, attributes, and events,
suitable for wire formats downstream.

---

## Troubleshooting

| Symptom | Reading |
|---|---|
| `warning: namespace isolation unavailable ...` | Degraded lane (seccomp-restricted host). Expected here; confinement, timeouts, and output caps remain in force. |
| Provider error after several seconds | Pre-stream retries with exponential backoff ran (rate limits/5xx). Non-retryable statuses (e.g. 4xx) fail fast on the first attempt. |
| Connection refused to the local server | Transport faults are retryable: expect several backoff attempts, then exit 1. Confirm `EP` and that the server is actually listening. |
| `HTTP 404`/`405` from the endpoint | Wrong base-URL path — include `/v1` (the runtime POSTs `<endpoint>/chat/completions`). These statuses fail fast without retry. |
| `--provider openai/anthropic` with no key | The runtime warns and proceeds — valid for local servers without auth; hosted APIs will 401. |
| Exit 3 mid-task | Doom-loop escalation was denied. Resume with a memo (`y: <different approach>`) at the prompt next time, or resume the session after changing the task framing. |
| Exit 2 from a workflow | A step failed; the report names it. Fix the step, re-run — earlier steps' outputs are not cached between runs. |
| Empty stdin, no task | The CLI prints usage and exits 2. Pipe a task (`echo "..." \| harness ...`) or pass it positionally. |

## What this tour covered

Sandboxed execution (Stages 1–4) · human review with memos (2) · reviewer
read-only posture (3) · timeout/truncation/exit-code semantics (4) ·
doom-loop reflection and escalation (5) · DAG workflows with substitution
and abort reporting (6) · compaction with pinned/recent preservation (7) ·
resume/rewind over persisted sessions (8) · span-based observability (9).
