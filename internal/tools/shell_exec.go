package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"harness/internal/sandbox"
)

type shellExecTool struct{ env Env }

func newShellExec(env Env) Tool { return &shellExecTool{env} }

func (t *shellExecTool) Name() string { return "shell_exec" }

func (t *shellExecTool) Description() string {
	return "Execute a shell command inside the sandbox jail (workspace-confined). Reports exit code, duration, and stdout/stderr with truncation flags."
}

func (t *shellExecTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command":    {"type": "string", "description": "Command line executed via /bin/sh -c"},
			"timeout_ms": {"type": "integer", "minimum": 1, "description": "Execution budget in milliseconds (default: sandbox default)"}
		},
		"required": ["command"]
	}`)
}

type shellExecParams struct {
	Command   string `json:"command"`
	TimeoutMS *int64 `json:"timeout_ms"`
}

func (t *shellExecTool) Call(ctx context.Context, _ Env, params json.RawMessage) (*Result, error) {
	p, err := decode[shellExecParams](params)
	if err != nil {
		return soft("%s", err), nil
	}
	if p.Command == "" {
		return soft("shell_exec: command is required"), nil
	}
	cfg := sandbox.Config{
		Workspace:  t.env.Workspace,
		AllowWrite: t.env.AllowWrite,
		AllowNet:   t.env.AllowNet,
	}
	if p.TimeoutMS != nil && *p.TimeoutMS > 0 {
		cfg.Timeout = time.Duration(*p.TimeoutMS) * time.Millisecond
	}
	res, err := t.env.Sandbox.Run(ctx, cfg, []string{"/bin/sh", "-c", p.Command})
	if err != nil {
		return soft("shell_exec: %v", err), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "exit: %d", res.ExitCode)
	if res.TimedOut {
		sb.WriteString(" (timed out)")
	}
	fmt.Fprintf(&sb, "\nduration: %v\n", res.Duration.Round(time.Millisecond))
	for _, w := range res.Warnings {
		sb.WriteString("warning: " + w + "\n")
	}
	if len(res.Stdout) > 0 {
		sb.WriteString("\nstdout:\n")
		sb.Write(res.Stdout)
		if res.StdoutTruncated {
			sb.WriteString("\n[stdout truncated]\n")
		}
	}
	if len(res.Stderr) > 0 {
		sb.WriteString("\nstderr:\n")
		sb.Write(res.Stderr)
		if res.StderrTruncated {
			sb.WriteString("\n[stderr truncated]\n")
		}
	}
	// Nonzero exits are data for the model to reason over, not transport
	// faults; the exit code is always surfaced above.
	return &Result{Content: sb.String()}, nil
}
