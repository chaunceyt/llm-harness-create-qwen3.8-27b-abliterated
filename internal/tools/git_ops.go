package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"harness/internal/sandbox"
)

// gitRefRe admits commit-ish spellings without shell metacharacters; argv is
// built from fixed subcommands, so this lexical check is the injection guard.
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

type gitOpsTool struct{ env Env }

func newGitOps(env Env) Tool { return &gitOpsTool{env} }

func (t *gitOpsTool) Name() string { return "git_ops" }

func (t *gitOpsTool) Description() string {
	return "Safe read-only git operations executed inside the sandbox with a read-only workspace: status, log, diff, show."
}

func (t *gitOpsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op":    {"type": "string", "enum": ["status", "log", "diff", "show"]},
			"ref":   {"type": "string", "description": "Commit-ish for log/show (default HEAD)"},
			"count": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Log entries (default 20)"},
			"path":  {"type": "string", "description": "Workspace-relative path restriction"}
		},
		"required": ["op"]
	}`)
}

type gitOpsParams struct {
	Op    string `json:"op"`
	Ref   string `json:"ref"`
	Count *int   `json:"count"`
	Path  string `json:"path"`
}

func (t *gitOpsTool) Call(ctx context.Context, _ Env, params json.RawMessage) (*Result, error) {
	p, err := decode[gitOpsParams](params)
	if err != nil {
		return soft("%s", err), nil
	}
	var pathRel string
	if p.Path != "" {
		if pathRel, err = t.env.confineRel(p.Path); err != nil {
			return soft("%s", err), nil
		}
	}
	var argv []string
	switch p.Op {
	case "status":
		argv = []string{"git", "status"}
	case "log":
		n := 20
		if p.Count != nil {
			if *p.Count < 1 {
				return soft("git_ops: count must be >= 1"), nil
			}
			if *p.Count > 200 {
				n = 200
			} else {
				n = *p.Count
			}
		}
		ref := p.Ref
		if ref == "" {
			ref = "HEAD"
		}
		if !gitRefRe.MatchString(ref) {
			return soft("git_ops: bad ref %q", p.Ref), nil
		}
		argv = []string{"git", "log", "--oneline", fmt.Sprintf("-n%d", n), ref}
		if pathRel != "" {
			argv = append(argv, "--", pathRel)
		}
	case "diff":
		argv = []string{"git", "diff"}
		if p.Ref != "" {
			if !gitRefRe.MatchString(p.Ref) {
				return soft("git_ops: bad ref %q", p.Ref), nil
			}
			argv = append(argv, p.Ref)
		}
		if pathRel != "" {
			argv = append(argv, "--", pathRel)
		}
	case "show":
		ref := p.Ref
		if ref == "" {
			ref = "HEAD"
		}
		if !gitRefRe.MatchString(ref) {
			return soft("git_ops: bad ref %q", p.Ref), nil
		}
		argv = []string{"git", "show", ref}
		if pathRel != "" {
			argv = append(argv, "--", pathRel)
		}
	default:
		return soft("git_ops: unknown op %q", p.Op), nil
	}

	cfg := sandbox.Config{
		Workspace:  t.env.Workspace,
		AllowWrite: false, // read-only even inside coder sessions
		AllowNet:   false,
	}
	res, err := t.env.Sandbox.Run(ctx, cfg, argv)
	if err != nil {
		return soft("git_ops: %v", err), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "git %s\nexit: %d\n", strings.Join(argv[1:], " "), res.ExitCode)
	if len(res.Stdout) > 0 {
		sb.Write(res.Stdout)
		if !strings.HasSuffix(string(res.Stdout), "\n") {
			sb.WriteString("\n")
		}
	}
	if len(res.Stderr) > 0 {
		sb.WriteString("\nstderr:\n")
		sb.Write(res.Stderr)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		sb.WriteString("\n[output truncated]\n")
	}
	return &Result{Content: sb.String()}, nil
}
