# harness

A production-grade LLM agent runtime in Go. It runs autonomous sessions for
three personas — **coder** (task execution with the full toolset), **reviewer**
(read-only analysis), and **automator** (DAG workflow execution) — with every
shell command jailed in an OS-level sandbox, human-in-the-loop permission
gating, doom-loop detection, context-window compaction, structured tracing,
and SQLite-backed session persistence with resume and rewind.

This is an autonomous runtime, not a chat wrapper: the model proposes tool
calls, the runtime gates, executes, audits, and feeds back the results in a
deterministic ReAct loop until the task completes, the budget exhausts, a
human intervenes, or the operator cancels.

## Architecture

| Path | Role |
|---|---|
| `cmd/harness` | CLI entrypoint: provider wiring, sandboxed execution, interactive human review, workflow automation, session resume/rewind |
| `internal/llm` | Streaming-first provider abstraction: OpenAI-compatible `/v1/chat/completions`, Anthropic `/v1/messages`, Ollama `/api/chat`. Tool calling, structured outputs, token-usage accounting, pre-stream exponential-backoff retries, context cancellation |
| `internal/agent` | The ReAct loop: turn consumption, permission gating, doom-loop detection (repetition + oscillation), context compaction, mode personas, HITL escalation, DAG workflow execution, resume seams |
| `internal/sandbox` | Process jailing: Linux namespaces + LandLock (primary), macOS Seatbelt, with an empirically-validated degraded lane for seccomp-restricted hosts. Timeouts, capped output draining, process-tree reaping |
| `internal/tools` | JSON-schema-validated tool registry: `fs_read`, `fs_edit`, `fs_search` (gitignore-aware), `shell_exec`, `git_ops`. Workspace confinement is lexical and symlink-verified |
| `internal/store` | Session persistence on SQLite via `modernc.org/sqlite` (pure Go, no CGO): turns (resume/rewind), append-only audit |
| `internal/telemetry` | Structured tracing: OTel-compatible span hooks (trace/span IDs, parent linkage, attributes, exporters) emitted as slog records; component logger |

All state is per-invocation. Agent values are immutable after construction;
concurrent runs share no mutable configuration. Every fault wraps its cause
explicitly.

## Requirements

- **Go ≥ 1.26.0** (module directive). Dependencies are pure Go — including
  the SQLite driver — so there is no CGO requirement and cross-compilation
  is a plain environment-variable build.
- **A model endpoint**, one of:
  - hosted OpenAI-compatible API (`OPENAI_API_KEY`), including local servers
    that speak the same dialect (e.g. llama.cpp's `llama-server`);
  - Anthropic Messages API (`ANTHROPIC_API_KEY`);
  - a local [Ollama](https://ollama.com) instance (no key required).
- **A workspace directory** — any writable path; commands execute with the
  workspace as their root. Reviewer sessions mount it read-only.
- **Sandbox posture** (Linux): the primary jail uses user/mount namespaces
  plus LandLock. Hosts whose seccomp filters deny `clone(CLONE_NEWUSER)`
  (common in containers and sandboxes) automatically run the degraded
  lane — LandLock-only confinement with timeouts and output caps still in
  force — and say so explicitly: every command result carries
  `warning: namespace isolation unavailable ...; running degraded with
  limits only`. Treat that warning as expected host behavior, not a
  regression. macOS uses Seatbelt profiles. There is no silent fallback:
  isolation gaps surface as warnings in results and telemetry.
- Persistence (optional but default-on in the CLI) writes one SQLite file.
  Default location `~/.harness/sessions.db`; override with `--store`, or pass
  `--store :memory:` for a transient database.

## Setup

```sh
go build -o harness ./cmd/harness
```

Verify the tree (formatting, vetting, full unit suite under the race
detector):

```sh
gofmt -l cmd/ internal/
go vet ./...
go test ./... -count=1 -race
```

Cross-compilation requires nothing beyond Go itself:

```sh
GOOS=darwin GOARCH=arm64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
```

## Quickstart

Point at a local Ollama model and run a task:

```sh
harness --provider ollama --model qwen3 \
        --workspace ~/projects/myrepo \
        "summarize what changed in the last commit and list risky lines"
```

Model text streams to **stdout**; every tool invocation, observation,
permission event, and span record goes to **stderr**. Expected first-run
stderr shape on a restricted host:

```
--> git_ops {"op":"log","count":5}
<-- git_ops:
...
warning: namespace isolation unavailable (operation not permitted); running degraded with limits only
[session] id=<12-hex…> stopped=final turns=N tokens=M
```

With hosted providers, export the matching key first:

```sh
export ANTHROPIC_API_KEY=...    # --provider anthropic
export OPENAI_API_KEY=...       # --provider openai
```

Local OpenAI-compatible servers take `--endpoint http://127.0.0.1:8080/v1`.

## Configuration reference

### CLI flags

| Flag | Meaning |
|---|---|
| `--provider` | `openai`, `anthropic`, or `ollama` (required) |
| `--model` | Model name/identifier (required) |
| `--endpoint` | Provider base-URL override (defaults per provider) |
| `--mode` | `coder` (default), `reviewer`, or `automator` |
| `--allow-net` | Permit outbound network from sandboxed commands (default: isolated) |
| `--workspace` | Workspace root (default: current directory) |
| `--store` | Session database path; `:memory:` transient, empty disables persistence (default `~/.harness/sessions.db`) |
| `--resume` | Session ID to resume (task runs) |
| `--rewind` | With `--resume`: truncate the session to this turn sequence first |
| `--max-turns` | Turn budget per task (default 50) |
| `--window` | Context window in estimated tokens (default 128000); compaction triggers at ~80% |
| `--workflow` | Workflow JSON file (requires `--mode automator`) |
| `--allow-all` | Answer human-review prompts with allow (scripted/CI use) |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Task completed with a final answer, or workflow fully succeeded |
| 1 | Runtime failure (provider, sandbox, persistence fault) |
| 2 | Configuration error, workflow step failure, or turn-budget exhaustion |
| 3 | Session escalated to a human (doom-loop second trip) |
| 130 | Canceled by signal (SIGINT/SIGTERM) |

### Human review

Coder and automator sessions default to **ask**: every tool call prompts a
human. Answers:

```
[permission] shell_exec {"command":"make test"}
           reason: default-ask
approve? (y / y: memo / n)
```

- `y` — allow;
- `y: memo` — allow and surface the memo to the model on the resumed turn;
- anything else — deny (the model receives a denial observation and adapts).

Non-interactive stdin denies by default unless `--allow-all` is set. The
same grammar answers **doom-loop escalations**, where the reason names the
repeating/oscillating pattern.

### Workflow JSON (automator)

```json
{
  "name": "release-gate",
  "steps": [
    {"id": "build", "command": "make build"},
    {"id": "tests", "command": "make test", "needs": ["build"]},
    {"id": "review", "task": "Review the diff produced by the build and list risks", "needs": ["build"]}
  ]
}
```

Steps form a DAG: exactly one of `command` (executed sandboxed) or `task`
(a nested agent task) per step; `needs` edges are validated (unknown
dependencies, duplicates, cycles) before anything executes. Earlier steps'
outputs substitute into later steps as `{stepID}`. A failing step aborts the
remainder and is **reported** (exit 2), not raised as a crash.

### Tool surfaces (what the model may issue)

| Tool | Parameters |
|---|---|
| `fs_read` | `path` (required), `offset` (1-based, default 1), `limit` (default 2000) |
| `fs_edit` | `path`, `old_string`, `new_string` (required); `replace_all` (uniqueness otherwise enforced) |
| `fs_search` | `pattern` (required, regexp per line), `path`, `glob`, `max_results` (default 100); gitignore-aware |
| `shell_exec` | `command` (required, via `/bin/sh -c`), `timeout_ms`; **always sandboxed** |
| `git_ops` | `op` (`status`/`log`/`diff`/`show`), `ref`, `count` (≤200), `path` |

Reviewer sessions expose only `fs_read`, `fs_search`, `git_ops` and mount the
workspace read-only; a mutating call from a reviewer surfaces as an
`unknown tool` observation rather than an execution.

## Security posture

- Every shell command executes inside the sandbox; there is no out-of-jail
  execution path. Timeouts bound each execution; output is capped and
  truncated markers are preserved; the process tree is reaped.
- Workspace confinement is enforced lexically (relative/absolute/symlink
  escapes rejected) and verified against the real filesystem, so paths
  cannot wander outside the workspace.
- Permission rules (allow/ask/deny) scope by tool name and glob path
  patterns; the most specific rule wins. Audits record every denial and
  grant, append-only.
- Doom-loop detection correlates executed calls by canonical argument +
  outcome digests. Three identical trips inject a reflection prompt; a
  persisted trip (or an oscillating ABAB error pattern) escalates to a human
  with the pattern named. Context compaction folds older turns into a
  structured summary while preserving pinned content and the recent window.

## Documentation

- [`prompt.md`](docs/prompt.md)
- [`example-usage.md`](docs/example-usage.mdexample-usage.md) — correct usage per component, CLI
  and library level.
- [`tutorial.md`](docs/tutorial.mdtutorial.md) — an end-to-end walk-through exercising every
  feature against a local model.

