# Role & Objective
Act as a Principal Systems Engineer and AI Infrastructure Architect. Your task is to design and implement a modular, production-grade **LLM Agent Harness in Go** (Go 1.23+). 

The harness must act as an autonomous runtime capable of **software engineering (coding)**, **automated code reviews**, and **general workflow automation**, utilizing **OS-level local process jailing** for safe tool execution.

---

# Core Architectural Requirements

## 1. Unified LLM Provider Interface (`internal/llm`)
Implement a clean, streaming-first provider abstraction (`Provider` interface) supporting tool/function calling, structured outputs (JSON Schema), and token usage tracking:
- **OpenAI-Compatible API:** Supports standard OpenAI `/v1/chat/completions`, plus local `llama.cpp` server (`/v1/chat/completions` + native `/completion` slot/cache awareness if applicable) via configurable `BaseURL` and optional `APIKey`.
- **Anthropic Messages API:** Native support for `/v1/messages`, including `tool_use`/`tool_result` content blocks and prompt caching headers (`anthropic-beta: prompt-caching-2024-07-31`).
- **Ollama Native API:** Support `/api/chat` with tool calling, `keep_alive`, and model options (`num_ctx`, `temperature`).
- **Resilience:** Include exponential backoff retries, rate-limit (`429`) handling, and `context.Context` cancellation across all HTTP streams.

## 2. Agentic Loop & State Machine (`internal/agent`)
Implement a deterministic **ReAct / Plan-and-Execute loop** with the following modern harness best practices:
- **Context Window & Compaction Management:** Track token counts per turn. When context approaches a configurable threshold (e.g., 80% of `max_tokens`), automatically summarize/compact older tool outputs while preserving system instructions, pinned files, and recent turns.
- **Loop & Doom-Loop Detection:** Detect repetitive identical tool calls or oscillating error states (e.g., failing the same `go test` 3 times identically) and inject a reflection/course-correction prompt or escalate to human-in-the-loop (HITL).
- **Mode Specialization (Personas/Workflows):**
  - `coder`: Focuses on minimal diffs, test-driven verification, and codebase exploration before editing.
  - `reviewer`: Read-only by default; analyzes `git diff`, checks cyclomatic complexity, security anti-patterns, and edge cases, outputting structured review comments.
  - `automator`: Executes multi-step DAG/shell workflows with structured state passing between steps.
- **Human-in-the-Loop (HITL) Interception:** Configurable permission policies (`allow`, `ask`, `deny`) per tool and path pattern before executing destructive actions.

## 3. Local Process Jailing & Sandboxing (`internal/sandbox`)
Every shell command or script executed by the agent MUST run inside an isolated local sandbox. Design a cross-platform `Sandbox` interface with concrete implementations:
- **Linux Primary (`bubblewrap` / `bwrap` or native Linux namespaces + `landlock`):**
  - Mount the target workspace directory as read-write (or read-only in `reviewer` mode).
  - Bind-mount essential system binaries (`/usr`, `/bin`, `/lib`, `/lib64`, `/etc/ssl`) as **read-only**.
  - Isolate PID, IPC, UTS, and Network namespaces (`--unshare-pid`, `--unshare-net` by default, with an explicit toggle to allow network access for package managers).
  - Mount an ephemeral `tmpfs` for `/tmp` and hide host `$HOME` / sensitive dotfiles (`~/.ssh`, `~/.aws`, `~/.env`).
- **macOS Secondary (`sandbox-exec` / Seatbelt):** Generate dynamic `.sb` Scheme profiles restricting file writes strictly to the workspace and `/tmp`, denying access to user credentials and outbound network unless explicitly permitted.
- **Resource Limits:** Enforce execution timeouts (`context.WithTimeout`), stdout/stderr byte truncation (preventing OOM from `cat` on huge files), and process group cleanup (`syscall.Kill(-pgid, syscall.SIGKILL)`).

## 4. Built-in Tool Suite (`internal/tools`)
Provide a clean JSON-schema-driven tool registry with strongly typed Go structs:
- `fs_read`: Read file contents with line numbers and optional line-range slicing (`start_line`, `end_line`).
- `fs_edit`: Deterministic search-and-replace or unified diff patching (avoid full-file rewrites on large files; verify unique match before applying).
- `fs_search`: Fast regex search (wrapping `ripgrep` / `rg` or native Go fallback) respecting `.gitignore`.
- `shell_exec`: Execute commands inside the `Sandbox` instance, returning exit code, truncated stdout/stderr, and execution duration.
- `git_ops`: Safe wrappers for `status`, `diff`, `log`, and branch inspection.

## 5. Observability & Persistence (`internal/store` & `internal/telemetry`)
- **Structured Tracing:** Use Go's standard `log/slog` and OpenTelemetry-compatible span hooks to trace every LLM request, token count, latency, and sandboxed tool invocation.
- **Session Checkpointing:** Persist conversation history, tool states, and diffs to SQLite (`modernc.org/sqlite`, pure Go, no CGO) so sessions can be resumed, rewound, or audited.

---

# Deliverables & Output Instructions

Please build this systematically in the following order. Do not skip implementations or leave `// TODO` placeholders for critical logic:

1. **Directory Structure & `go.mod`:** Define an idiomatic Go project layout (`cmd/harness`, `internal/...`) using minimal, high-reliability dependencies.
2. **Core Interfaces & Types:** Provide the complete Go code for `internal/llm/provider.go`, `internal/tools/registry.go`, and `internal/sandbox/sandbox.go`.
3. **Provider Implementations:** Provide full, working implementations for OpenAI-compatible (covering `llama.cpp`), Anthropic, and Ollama clients.
4. **Sandbox Implementation:** Provide the complete Linux (`bwrap`/namespaces) and macOS (`sandbox-exec`) jailing implementations, including process group termination and output truncation.
5. **Agent Execution Loop:** Provide the core state machine (`internal/agent/loop.go`) demonstrating streaming tool calls, context compaction, doom-loop detection, and HITL approval hooks.
6. **CLI Entrypoint (`cmd/harness/main.go`):** A clean CLI supporting flags for `--provider`, `--model`, `--endpoint`, `--mode (coder|reviewer|automator)`, `--allow-net`, and `--workspace`.

Write clean, idiomatic Go with explicit error wrapping (`fmt.Errorf("...: %w", err)`), strict concurrency safety, and zero global mutable state.

