// Package tools implements the harness's built-in agent tools: JSON-schema
// driven, workspace-confined, sandbox-backed. Every filesystem operation is
// lexically and symlink-verified confined to the session workspace; every
// command execution goes through the sandbox jail.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/sandbox"
)

// Result is one tool invocation's outcome. Content is what enters the
// conversation; IsError marks a soft failure surfaced to the model rather
// than a registry-level fault.
type Result struct {
	Content string
	IsError bool
}

// Tool is one built-in capability.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON schema of the tool's parameters.
	Schema() json.RawMessage
	// Call executes the tool. It returns (nil, err) only for faults outside
	// the tool's control; operational outcomes are Results.
	Call(ctx context.Context, env Env, params json.RawMessage) (*Result, error)
}

// Env carries per-session posture into every tool. It is immutable after
// session construction; tools hold copies and never mutate it.
type Env struct {
	Workspace  string // absolute project directory; confines every fs operation
	AllowWrite bool   // false = reviewer posture (read-only workspace)
	AllowNet   bool
	// Ripgrep names the rg binary for fs_search; empty selects the native
	// gitignore-aware walker.
	Ripgrep string
	// Sandbox is the execution jail for shell_exec and git_ops.
	Sandbox sandbox.Sandbox
}

// Registry is an immutable, concurrency-safe tool selection built once per
// session. Post-construction use is read-only.
type Registry struct {
	tools map[string]Tool
	order []string
}

// builtinOrder fixes enumeration order for stable model-facing listings.
var builtinOrder = []string{"fs_read", "fs_edit", "fs_search", "shell_exec", "git_ops"}

var builtinConstructors = map[string]func(Env) Tool{
	"fs_read":    newFSRead,
	"fs_edit":    newFSEdit,
	"fs_search":  newFSSearch,
	"shell_exec": newShellExec,
	"git_ops":    newGitOps,
}

// New builds a Registry. Empty names selects every built-in; explicit names
// select a subset (the mode layer decides posture).
func New(env Env, names ...string) (*Registry, error) {
	if env.Workspace == "" {
		return nil, fmt.Errorf("tools: empty workspace")
	}
	if abs, err := filepath.Abs(env.Workspace); err == nil {
		env.Workspace = abs
	}
	r := &Registry{tools: make(map[string]Tool, len(builtinOrder))}
	selected := builtinOrder
	if len(names) > 0 {
		selected = nil
		for _, n := range names {
			if _, ok := builtinConstructors[n]; !ok {
				return nil, fmt.Errorf("tools: unknown tool %q", n)
			}
			if _, dup := r.tools[n]; dup {
				return nil, fmt.Errorf("tools: duplicate tool %q", n)
			}
			r.tools[n] = builtinConstructors[n](env)
			selected = append(selected, n)
		}
	}
	for _, n := range selected {
		if _, ok := r.tools[n]; !ok {
			r.tools[n] = builtinConstructors[n](env)
		}
		r.order = append(r.order, n)
	}
	return r, nil
}

// Get returns one tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names lists selected tools in enumeration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// decode unmarshals tool params; empty params decode to zero values.
func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) > 0 {
		if err := json.Unmarshal(params, &v); err != nil {
			return v, fmt.Errorf("tools: decode params: %w", err)
		}
	}
	return v, nil
}

// soft renders a soft failure surfaced to the model.
func soft(format string, a ...any) *Result {
	return &Result{IsError: true, Content: fmt.Sprintf(format, a...)}
}

// confine resolves path under the workspace and verifies containment both
// lexically and through symlink resolution. Workspace-relative paths are
// joined; absolute paths must already lie inside.
func (e Env) confine(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("tools: empty path")
	}
	base := e.Workspace
	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Join(base, filepath.Clean(path))
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tools: path %s escapes workspace", path)
	}
	// Symlink-verified containment when both ends resolve.
	if wsReal, werr := filepath.EvalSymlinks(base); werr == nil {
		if tgtReal, terr := filepath.EvalSymlinks(abs); terr == nil {
			if r, err := filepath.Rel(wsReal, tgtReal); err == nil && (r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator))) {
				return "", fmt.Errorf("tools: path %s escapes workspace via symlink", path)
			}
		}
	}
	return abs, nil
}

// confineRel is confine plus workspace relativization.
func (e Env) confineRel(path string) (string, error) {
	abs, err := e.confine(path)
	if err != nil {
		return "", err
	}
	return filepath.Rel(e.Workspace, abs)
}

// requireWritable enforces reviewer posture for mutating tools.
func (e Env) requireWritable(tag string) error {
	if !e.AllowWrite {
		return fmt.Errorf("%s: workspace is read-only (reviewer posture)", tag)
	}
	return nil
}

// maxFileBytes bounds whole-file reads (fs_edit). Larger files require
// targeted operations.
const maxFileBytes = 10 << 20

// readFileBounded reads a whole confined file within the size bound.
func (e Env) readFileBounded(tag, path string) ([]byte, error) {
	abs, err := e.confine(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("%s: stat %s: %w", tag, path, err)
	}
	if st.Mode()&os.ModeType != 0 {
		return nil, fmt.Errorf("%s: %s is not a regular file", tag, path)
	}
	if st.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s: %s is %d bytes, over the %d-byte bound; use fs_read/fs_search", tag, path, st.Size(), maxFileBytes)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("%s: read %s: %w", tag, path, err)
	}
	return b, nil
}

// writeAtomic replaces a confined file via temp-file-in-same-dir + rename.
func (e Env) writeAtomic(tag, path string, content []byte) error {
	abs, err := e.confine(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".tmp-"+tag+"-")
	if err != nil {
		return fmt.Errorf("%s: temp create: %w", tag, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("%s: temp write: %w", tag, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s: temp close: %w", tag, err)
	}
	if err := os.Rename(name, abs); err != nil {
		return fmt.Errorf("%s: rename: %w", tag, err)
	}
	return nil
}
