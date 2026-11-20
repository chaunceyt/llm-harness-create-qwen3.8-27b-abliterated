package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"harness/internal/sandbox"
)

const (
	defaultMaxResults    = 100
	maxNativeFileBytes   = 16 << 20
	maxSearchResultBytes = 256 << 10
)

type fsSearchTool struct{ env Env }

func newFSSearch(env Env) Tool { return &fsSearchTool{env} }

func (t *fsSearchTool) Name() string { return "fs_search" }

func (t *fsSearchTool) Description() string {
	return "Search file contents under the workspace for a regular expression, gitignore-aware. Returns path:line:text matches, capped."
}

func (t *fsSearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern":     {"type": "string", "description": "Regular expression matched against each line"},
			"path":        {"type": "string", "description": "Workspace-relative subdirectory to search (default: workspace root)"},
			"glob":        {"type": "string", "description": "Glob filter applied to file paths (e.g. '*.go')"},
			"max_results": {"type": "integer", "minimum": 1, "description": "Maximum matches to return (default 100)"}
		},
		"required": ["pattern"]
	}`)
}

type fsSearchParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxResults *int   `json:"max_results"`
}

func (t *fsSearchTool) Call(ctx context.Context, _ Env, params json.RawMessage) (*Result, error) {
	p, err := decode[fsSearchParams](params)
	if err != nil {
		return soft("%s", err), nil
	}
	if p.Pattern == "" {
		return soft("fs_search: pattern is required"), nil
	}
	maxResults := defaultMaxResults
	if p.MaxResults != nil {
		if *p.MaxResults < 1 {
			return soft("fs_search: max_results must be >= 1"), nil
		}
		maxResults = *p.MaxResults
	}
	sub := p.Path
	if sub == "" {
		sub = "."
	}
	subAbs, err := t.env.confine(sub)
	if err != nil {
		return soft("%s", err), nil
	}
	subRel, err := filepath.Rel(t.env.Workspace, subAbs)
	if err != nil {
		return soft("fs_search: relativize %s: %v", sub, err), nil
	}

	if t.env.Ripgrep != "" {
		return t.rgSearch(ctx, p, subRel, maxResults)
	}
	return t.nativeSearch(ctx, p, subAbs, maxResults)
}

// rgSearch executes ripgrep inside the sandbox (read-only, net-isolated) and
// parses its line-numbered output.
func (t *fsSearchTool) rgSearch(ctx context.Context, p fsSearchParams, subRel string, maxResults int) (*Result, error) {
	argv := []string{
		t.env.Ripgrep,
		"--line-number", "--color=never", "--no-heading",
		"-e", p.Pattern,
	}
	if p.Glob != "" {
		argv = append(argv, "--glob", p.Glob)
	}
	argv = append(argv, subRel)
	cfg := sandbox.Config{
		Workspace:  t.env.Workspace,
		AllowWrite: false,
		AllowNet:   false,
	}
	res, err := t.env.Sandbox.Run(ctx, cfg, argv)
	if err != nil {
		return soft("fs_search: %v", err), nil
	}
	if res.ExitCode != 0 && res.ExitCode != 1 {
		return soft("fs_search: ripgrep exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr))), nil
	}
	var sb strings.Builder
	count := 0
	truncated := false
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		// rg output: path:line:text — paths with colons are rare; split on
		// the first two separators.
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		j := strings.IndexByte(line[i+1:], ':')
		if j < 0 {
			continue
		}
		path, lineno, text := line[:i], line[i+1:i+1+j], line[i+1+j+1:]
		if count < maxResults {
			sb.WriteString(path + ":" + lineno + ":" + text + "\n")
		}
		count++
		if count == maxResults {
			truncated = true
		}
		if sb.Len() > maxSearchResultBytes {
			truncated = true
			break
		}
	}
	if count == 0 {
		return &Result{Content: "(no matches)"}, nil
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n(results truncated at %d matches / %d bytes)\n", maxResults, maxSearchResultBytes))
	}
	return &Result{Content: sb.String()}, nil
}

// nativeSearch walks the confined subtree with the gitignore-aware matcher.
// Single-goroutine: the ignore cache and counters need no locking.
func (t *fsSearchTool) nativeSearch(_ context.Context, p fsSearchParams, subAbs string, maxResults int) (*Result, error) {
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return soft("fs_search: compile pattern: %v", err), nil
	}
	var globMatch func(name string) bool
	if p.Glob != "" {
		if _, err := filepath.Match(p.Glob, ""); err != nil {
			return soft("fs_search: bad glob: %v", err), nil
		}
		glob := p.Glob
		globMatch = func(name string) bool {
			m, _ := filepath.Match(glob, name)
			return m
		}
	}

	ignoreCache := make(map[string]*gitignoreFile)
	ignoreAt := func(dirAbs string) *gitignoreFile {
		if gi, ok := ignoreCache[dirAbs]; ok {
			return gi
		}
		gi := loadGitignore(dirAbs)
		ignoreCache[dirAbs] = gi
		return gi
	}
	// isIgnored evaluates last-match-wins over the candidate's ancestor
	// gitignores (shallow to deep), each pattern tested against the segments
	// relative to that file's own directory. A candidate's own gitignore does
	// not apply to the candidate itself.
	isIgnored := func(candSegs []string, isDir bool) bool {
		ignored, matched := false, false
		for d := 0; d < len(candSegs); d++ {
			dirAbs := filepath.Join(subAbs, strings.Join(candSegs[:d], "/"))
			gi := ignoreAt(dirAbs)
			if gi == nil {
				continue
			}
			for _, pat := range gi.patterns {
				if pat.matches(candSegs[d:], isDir) {
					matched = true
					ignored = !pat.negated
				}
			}
		}
		return matched && ignored
	}

	var sb strings.Builder
	count := 0
	truncated := false

	err = filepath.WalkDir(subAbs, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // unreadable entry: skip
		}
		rel, err := filepath.Rel(t.env.Workspace, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		candSegs := splitSegs(rel)
		if d.IsDir() {
			if isIgnored(candSegs, true) {
				return fs.SkipDir
			}
			return nil
		}
		if globMatch != nil && !globMatch(d.Name()) {
			return nil
		}
		if isIgnored(candSegs, false) {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		if st.Size() > maxNativeFileBytes {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(b, []byte{0}) {
			return nil // binary
		}
		for i, line := range bytes.Split(b, []byte("\n")) {
			if !re.Match(line) {
				continue
			}
			if count < maxResults {
				lineStr := string(line)
				if len(lineStr) > 2000 {
					lineStr = lineStr[:2000] + "..."
				}
				sb.WriteString(rel + ":" + fmt.Sprint(i+1) + ":" + lineStr + "\n")
			}
			count++
			if count == maxResults || sb.Len() > maxSearchResultBytes {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return soft("fs_search: walk %s: %v", subAbs, err), nil
	}
	if count == 0 {
		return &Result{Content: "(no matches)"}, nil
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n(results truncated at %d matches / %d bytes)\n", maxResults, maxSearchResultBytes))
	}
	return &Result{Content: sb.String()}, nil
}
