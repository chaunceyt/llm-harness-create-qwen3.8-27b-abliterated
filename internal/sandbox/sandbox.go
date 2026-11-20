// Package sandbox executes commands inside isolated local process jails.
//
// The Linux primary backend creates fresh user, mount, PID, UTS, IPC and
// (by default) network namespaces via clone(2), blanks the mount tree with a
// tmpfs over "/", rebinds only the allowlisted system prefixes read-only,
// binds the workspace read-write (or read-only), hides the host $HOME tree
// behind an empty tmpfs, and confines writes further with landlock. macOS
// uses sandbox-exec (Seatbelt) with dynamically generated profiles. Every
// execution enforces timeouts, stdout/stderr byte truncation, and process
// tree cleanup.
//
// If namespace creation is unavailable (e.g. restrictive seccomp), the Linux
// backend degrades to a plainly-labeled unnamespaced execution that still
// applies env hygiene, timeouts, truncation, and process-group termination.
// Degradation is surfaced in Result.Warnings; it is never silent.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Defaults for Config fields left zero.
const (
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxOutput is the retained prefix length per stream. Beyond this
	// the remainder is drained and discarded (preventing both OOM from
	// `cat`-ing huge files and pipe-full deadlock).
	DefaultMaxOutput = 256 * 1024
)

// Config parameterizes one sandboxed execution.
type Config struct {
	// Workspace is the project directory. It is mounted read-write when
	// AllowWrite is true and read-only otherwise (reviewer posture).
	Workspace  string
	AllowWrite bool
	// AllowNet permits outbound networking by skipping the network-namespace
	// isolation and binding /etc/resolv.conf. Default: isolated.
	AllowNet bool
	// Timeout bounds the execution. Zero uses DefaultTimeout.
	Timeout time.Duration
	// MaxOutput bounds retained stdout/stderr bytes per stream. Zero uses
	// DefaultMaxOutput.
	MaxOutput int64
	// ExtraEnv appends variable assignments (KEY=VAL) to the hygienic env.
	ExtraEnv []string
	// ExtraProfileLines appends raw lines to generated Seatbelt profiles
	// (macOS tuning escape hatch).
	ExtraProfileLines []string
}

func (c Config) normalized() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}
	ws, err := filepath.Abs(c.Workspace)
	if err != nil {
		ws = c.Workspace
	}
	c.Workspace = ws
	return c
}

// validateWorkspace enforces the structural preconditions every backend
// relies on: an absolute project path that is not "/" and not rooted inside a
// virtual filesystem we remount.
func validateWorkspace(ws string) error {
	if ws == "" {
		return fmt.Errorf("sandbox: empty workspace")
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return fmt.Errorf("sandbox: resolve workspace: %w", err)
	}
	if abs == "/" {
		return fmt.Errorf("sandbox: workspace must not be /")
	}
	for _, v := range []string{"/proc", "/sys", "/dev"} {
		if abs == v || strings.HasPrefix(abs, v+string(filepath.Separator)) {
			return fmt.Errorf("sandbox: workspace %s is inside virtual filesystem %s", abs, v)
		}
	}
	if st, err := os.Stat(abs); err != nil {
		return fmt.Errorf("sandbox: workspace %s: %w", abs, err)
	} else if !st.IsDir() {
		return fmt.Errorf("sandbox: workspace %s is not a directory", abs)
	}
	return nil
}

// Result reports one sandboxed execution.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
	TimedOut bool

	StdoutTruncated bool
	StderrTruncated bool

	Command []string
	Backend string
	// Warnings carries degraded-isolation notices and skipped optional
	// stages. Non-empty Warnings means the execution was less isolated than
	// the primary posture.
	Warnings []string
}

// Info describes a backend instance's capabilities.
type Info struct {
	Backend      string
	Capabilities []string
	Notes        []string
}

// Sandbox is the execution-jail abstraction. Implementations are platform
// specific; New returns the platform-selected instance.
type Sandbox interface {
	// Run executes argv inside a fresh jail honoring cfg. Stdin receives EOF.
	Run(ctx context.Context, cfg Config, argv []string) (*Result, error)
	// Inspect reports backend identity and live capability probes.
	Inspect() Info
}

// New returns the platform-selected Sandbox instance.
func New() Sandbox { return newSandbox() }

// cappedWriter retains the first keep bytes written to it and discards the
// remainder, counting totals so callers can report truncation.
type cappedWriter struct {
	keep  int64
	buf   []byte
	total int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	if int64(len(w.buf)) < w.keep {
		room := w.keep - int64(len(w.buf))
		if room < int64(len(p)) {
			w.buf = append(w.buf, p[:room]...)
		} else {
			w.buf = append(w.buf, p...)
		}
	}
	return len(p), nil
}

func (w *cappedWriter) bytes() []byte { return w.buf }

func (w *cappedWriter) truncated() bool { return w.total > w.keep }

// drainCapped reads r to EOF retaining the first keep bytes. It always runs
// to EOF (or reader closure) so writers never block on a full pipe.
func drainCapped(r io.Reader, keep int64) (cappedWriter, error) {
	w := &cappedWriter{keep: keep}
	_, err := io.Copy(w, r)
	return *w, err
}

// homePath returns the host HOME absolute path, or "" when unset.
func homePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return home
}

// envAllowlist is the minimal environment executed commands receive.
var envAllowlist = []string{
	"PATH", "HOME", "TMPDIR", "USER", "LOGNAME",
	"LANG", "LC_ALL", "TZ", "TERM",
}

// buildEnv constructs the hygienic environment: an allowlist drawn from
// parentEnv, forced sandboxed HOME/TMPDIR, deterministic ordering, plus
// explicit overrides. No other host variable survives.
func buildEnv(parentEnv []string, sandboxedHome string, extra []string) []string {
	picked := make(map[string]string)
	for _, kv := range parentEnv {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		for _, allow := range envAllowlist {
			if k == allow && picked[allow] == "" {
				picked[allow] = v
				break
			}
		}
	}
	if sandboxedHome != "" {
		picked["HOME"] = sandboxedHome
	}
	picked["TMPDIR"] = "/tmp"
	if picked["TERM"] == "" {
		picked["TERM"] = "xterm-256color"
	}
	var out []string
	for _, k := range envAllowlist {
		if v, ok := picked[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	extraSeen := make(map[string]bool, len(out))
	for _, kv := range out {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			extraSeen[kv[:eq]] = true
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		return extra[i] < extra[j]
	})
	for _, kv := range extra {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if !extraSeen[kv[:eq]] {
			out = append(out, kv)
		}
	}
	return out
}
