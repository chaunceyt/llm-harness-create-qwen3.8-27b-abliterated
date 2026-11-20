package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Effect is one permission outcome.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectAsk   Effect = "ask"
	EffectDeny  Effect = "deny"
)

// Rule is one permission rule. Tool matches exactly or with "*";
// PathPattern is a glob tested against path-like arguments ("" = tool-level).
type Rule struct {
	Tool        string
	PathPattern string
	Effect      Effect
}

// Policy evaluates permission rules per tool invocation. Matching is
// most-specific-wins (exact tool > wildcard; path-scoped > tool-level); ties
// keep the earlier-registered rule. Unmatched invocations take Default.
type Policy struct {
	Default Effect
	rules   []Rule
}

// NewPolicy builds a policy with the given default effect ("" = ask).
func NewPolicy(def Effect) *Policy {
	if def == "" {
		def = EffectAsk
	}
	return &Policy{Default: def}
}

// Add registers a rule ("" effect defaults to deny).
func (p *Policy) Add(rule Rule) *Policy {
	if rule.Effect == "" {
		rule.Effect = EffectDeny
	}
	p.rules = append(p.rules, rule)
	return p
}

// Allow registers an allow rule.
func (p *Policy) Allow(tool, pathPattern string) *Policy {
	return p.Add(Rule{Tool: tool, PathPattern: pathPattern, Effect: EffectAllow})
}

// Ask registers an ask rule.
func (p *Policy) Ask(tool, pathPattern string) *Policy {
	return p.Add(Rule{Tool: tool, PathPattern: pathPattern, Effect: EffectAsk})
}

// Deny registers a deny rule.
func (p *Policy) Deny(tool, pathPattern string) *Policy {
	return p.Add(Rule{Tool: tool, PathPattern: pathPattern, Effect: EffectDeny})
}

// pathKeys are the argument names carrying workspace-relative paths.
var pathKeys = []string{"path", "file", "dir"}

func pathArg(args json.RawMessage) (string, bool) {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return "", false
	}
	for _, k := range pathKeys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func (r Rule) matches(tool string, args json.RawMessage) bool {
	if r.Tool != "*" && r.Tool != tool {
		return false
	}
	if r.PathPattern == "" {
		return true
	}
	p, ok := pathArg(args)
	if !ok {
		return false
	}
	if filepath.IsAbs(p) {
		p = filepath.Clean(p)
	}
	m, _ := filepath.Match(r.PathPattern, p)
	return m
}

func specificity(r Rule) int {
	n := 0
	if r.Tool != "*" {
		n += 2
	}
	if r.PathPattern != "" {
		n += 1
	}
	return n
}

func describeRule(r Rule) string {
	if r.PathPattern != "" {
		return fmt.Sprintf("tool=%s path=%s", r.Tool, r.PathPattern)
	}
	return fmt.Sprintf("tool=%s", r.Tool)
}

// decide returns the effect for one invocation plus the matched rule's
// description (or "default" when no rule matched).
func (p *Policy) decide(tool string, args json.RawMessage) (Effect, string) {
	best := -1
	var bestRule Rule
	for _, r := range p.rules {
		if !r.matches(tool, args) {
			continue
		}
		if s := specificity(r); s > best {
			best = s
			bestRule = r
		}
	}
	if best < 0 {
		return p.Default, "default"
	}
	return bestRule.Effect, describeRule(bestRule)
}
