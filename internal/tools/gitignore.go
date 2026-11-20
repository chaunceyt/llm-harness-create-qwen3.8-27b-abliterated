package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// gitignore.go implements the subset of gitignore pattern semantics the
// native search walker needs: negation (!), anchoring (leading /),
// directory-only (trailing /), single-directory wildcards (*, ?), spanning
// (**), and last-match-wins precedence. Patterns anchor to the directory of
// the file that declares them; nested .gitignore files apply below their
// directory, deepest declarations evaluated last.

// igSeg is one compiled pattern segment. Spanning segments consume zero or
// more path segments; others match exactly one.
type igSeg struct {
	re   *regexp.Regexp
	span bool
}

// igPattern is one compiled .gitignore line.
type igPattern struct {
	negated  bool
	dirOnly  bool
	anchored bool
	segs     []igSeg
}

// compileGitignoreLine parses one .gitignore line. Blank lines and comments
// report ok=false.
func compileGitignoreLine(line string) (igPattern, bool, error) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || strings.HasPrefix(line, "#") {
		return igPattern{}, false, nil
	}
	var p igPattern
	if strings.HasPrefix(line, "!") {
		p.negated = true
		line = line[1:]
	}
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return igPattern{}, false, nil
	}
	for _, seg := range strings.Split(line, "/") {
		if seg == "**" {
			p.segs = append(p.segs, igSeg{span: true})
			continue
		}
		var rb strings.Builder
		for _, r := range []rune(seg) {
			switch r {
			case '*':
				rb.WriteString(".*")
			case '?':
				rb.WriteString(".")
			default:
				rb.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		re, err := regexp.Compile("^" + rb.String() + "$")
		if err != nil {
			return igPattern{}, false, fmt.Errorf("compile pattern %q: %w", line, err)
		}
		p.segs = append(p.segs, igSeg{re: re})
	}
	return p, true, nil
}

// gitignoreFile is one loaded .gitignore.
type gitignoreFile struct {
	patterns []igPattern
}

// loadGitignore reads and compiles dir/.gitignore; missing or empty files
// yield nil.
func loadGitignore(dir string) *gitignoreFile {
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	var gi gitignoreFile
	for _, raw := range strings.Split(string(b), "\n") {
		p, ok, err := compileGitignoreLine(raw)
		if err != nil {
			continue // malformed line: git warns and continues; we skip
		}
		if ok {
			gi.patterns = append(gi.patterns, p)
		}
	}
	if len(gi.patterns) == 0 {
		return nil
	}
	return &gi
}

// matchSegments reports whether segs match pathSegs exactly. Spanning
// segments backtrack over zero or many consumed segments; patterns are short
// so the search stays bounded.
func matchSegments(segs []igSeg, pathSegs []string) bool {
	var rec func(si, pi int) bool
	rec = func(si, pi int) bool {
		if si == len(segs) {
			return pi == len(pathSegs)
		}
		s := segs[si]
		if s.span {
			for k := pi; k <= len(pathSegs); k++ {
				if rec(si+1, k) {
					return true
				}
			}
			return false
		}
		if pi == len(pathSegs) {
			return false
		}
		return s.re.MatchString(pathSegs[pi]) && rec(si+1, pi+1)
	}
	return rec(0, 0)
}

// matches reports whether one pattern matches a candidate path given as
// segments relative to the pattern file's own directory. Unanchored
// patterns match at any depth below that directory.
func (p igPattern) matches(relSegs []string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if p.anchored {
		return matchSegments(p.segs, relSegs)
	}
	for i := 0; i <= len(relSegs); i++ {
		if matchSegments(p.segs, relSegs[i:]) {
			return true
		}
	}
	return false
}

// splitSegs breaks a workspace-relative path into segments.
func splitSegs(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
}
