package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type fsEditTool struct{ env Env }

func newFSEdit(env Env) Tool { return &fsEditTool{env} }

func (t *fsEditTool) Name() string { return "fs_edit" }

func (t *fsEditTool) Description() string {
	return "Edit a file inside the workspace by replacing exact text. old_string must match exactly once unless replace_all is set; files are never rewritten wholesale."
}

func (t *fsEditTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":        {"type": "string", "description": "Workspace-relative or workspace-absolute file path"},
			"old_string":  {"type": "string", "description": "Exact text to replace (must be unique unless replace_all)"},
			"new_string":  {"type": "string", "description": "Replacement text"},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring uniqueness"}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

type fsEditParams struct {
	Path       string `json:"path"`
	Old        string `json:"old_string"`
	New        string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *fsEditTool) Call(_ context.Context, _ Env, params json.RawMessage) (*Result, error) {
	p, err := decode[fsEditParams](params)
	if err != nil {
		return soft("%s", err), nil
	}
	if p.Path == "" || p.Old == "" {
		return soft("fs_edit: path and old_string are required"), nil
	}
	if p.Old == p.New {
		return soft("fs_edit: old_string and new_string are identical"), nil
	}
	if err := t.env.requireWritable("fs_edit"); err != nil {
		return soft("%s", err), nil
	}
	content, err := t.env.readFileBounded("fs_edit", p.Path)
	if err != nil {
		return soft("%s", err), nil
	}
	text := string(content)
	n := strings.Count(text, p.Old)
	if n == 0 {
		return soft("fs_edit: old_string not found in %s", p.Path), nil
	}
	if !p.ReplaceAll && n > 1 {
		return soft("fs_edit: old_string matches %d times in %s; expand the match context or set replace_all", n, p.Path), nil
	}
	var updated string
	if p.ReplaceAll {
		updated = strings.ReplaceAll(text, p.Old, p.New)
	} else {
		i := strings.Index(text, p.Old)
		updated = text[:i] + p.New + text[i+len(p.Old):]
	}
	if err := t.env.writeAtomic("fs_edit", p.Path, []byte(updated)); err != nil {
		return soft("%s", err), nil
	}
	if p.ReplaceAll {
		return &Result{Content: fmt.Sprintf("replaced %d occurrence(s) in %s", n, p.Path)}, nil
	}
	return &Result{Content: fmt.Sprintf("replaced 1 occurrence in %s", p.Path)}, nil
}
