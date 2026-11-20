package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	defaultReadLimit = 2000
	maxLineBytes     = 256 << 10
)

type fsReadTool struct{ env Env }

func newFSRead(env Env) Tool { return &fsReadTool{env} }

func (t *fsReadTool) Name() string { return "fs_read" }

func (t *fsReadTool) Description() string {
	return "Read a file inside the workspace with line numbers. Optional offset (1-based first line) and limit select a window; defaults to the first 2000 lines."
}

func (t *fsReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":   {"type": "string", "description": "Workspace-relative or workspace-absolute file path"},
			"offset": {"type": "integer", "minimum": 1, "description": "First line to return, 1-based (default 1)"},
			"limit":  {"type": "integer", "minimum": 1, "description": "Maximum lines to return (default 2000)"}
		},
		"required": ["path"]
	}`)
}

type fsReadParams struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

func (t *fsReadTool) Call(_ context.Context, _ Env, params json.RawMessage) (*Result, error) {
	p, err := decode[fsReadParams](params)
	if err != nil {
		return soft("%s", err), nil
	}
	if p.Path == "" {
		return soft("fs_read: path is required"), nil
	}
	offset, limit := 1, defaultReadLimit
	if p.Offset != nil {
		if *p.Offset < 1 {
			return soft("fs_read: offset must be >= 1"), nil
		}
		offset = *p.Offset
	}
	if p.Limit != nil {
		if *p.Limit < 1 {
			return soft("fs_read: limit must be >= 1"), nil
		}
		limit = *p.Limit
	}
	text, total, err := t.env.readWindow(p.Path, offset, limit)
	if err != nil {
		return soft("%s", err), nil
	}
	if text == "" {
		return &Result{Content: fmt.Sprintf("(no lines in window; file has %d lines)", total)}, nil
	}
	return &Result{Content: text}, nil
}

// readWindow streams a confined file and retains only the requested line
// window, so retained memory scales with the window, not the file. Lines are
// rendered with right-aligned line numbers, Claude-Code style.
func (e Env) readWindow(path string, offset, limit int) (string, int, error) {
	abs, err := e.confine(path)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, fmt.Errorf("fs_read: open %s: %w", path, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode()&os.ModeType != 0 {
		return "", 0, fmt.Errorf("fs_read: %s is not a regular file", path)
	}

	br := bufio.NewReaderSize(f, 256<<10)
	var sb strings.Builder
	lineNo := 0
	window := 0
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo >= offset && window < limit {
				body := strings.TrimSuffix(line, "\n")
				if len(body) > maxLineBytes {
					body = body[:maxLineBytes] + "\n[line truncated]"
				}
				sb.WriteString(fmt.Sprintf("%6d\t%s\n", lineNo, body))
				window++
			}
		}
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		return "", lineNo, fmt.Errorf("fs_read: %s: %w", path, rerr)
	}
	return sb.String(), lineNo, nil
}
