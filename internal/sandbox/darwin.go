//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const backendDarwin = "darwin-sandbox-exec"

func newSandbox() Sandbox { return &darwinSandbox{} }

type darwinSandbox struct{}

func (s *darwinSandbox) Inspect() Info {
	info := Info{
		Backend:      backendDarwin,
		Capabilities: []string{"seatbelt-profile", "file-write-confinement", "network-outbound-gate", "process-group-cleanup"},
	}
	if p, err := exec.LookPath("sandbox-exec"); err == nil {
		info.Notes = append(info.Notes, "sandbox-exec at "+p)
	} else {
		info.Notes = append(info.Notes, "sandbox-exec not found")
	}
	return info
}

// systemReadPaths are the prefixes Seatbelt profiles allow reading (and, via
// process-exec, executing) outside the workspace.
var systemReadPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/System", "/etc/ssl",
	"/etc/passwd", "/etc/group", "/etc/resolv.conf", "/etc/hosts",
	"/private/etc/resolv.conf", "/private/etc/hosts",
	"/dev",
}

// seatbeltProfile generates a fail-closed policy: default deny; execution of
// system binaries and reads of system prefixes permitted; writes strictly to
// the workspace (when writable) and /tmp; outbound network only when
// explicitly permitted; user-credential paths never readable.
func seatbeltProfile(cfg Config) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("(version 1)")
	w("(deny default)")
	w("(allow process-fork)")
	w("(allow process-exec)")
	w("(allow signal)")
	w("(allow sysctl-read)")
	w("(allow mach-lookup)")
	w("(allow user-lookup*)")

	readPaths := append(append([]string{}, systemReadPaths...), "/tmp", "/private/tmp")
	if cfg.AllowWrite {
		readPaths = append(readPaths, cfg.Workspace)
	}
	for _, p := range readPaths {
		w("(allow file-read* (subpath %q))", p)
	}
	if !cfg.AllowWrite {
		w("(allow file-read* (subpath %q))", cfg.Workspace)
	}

	var writePaths []string
	if cfg.AllowWrite {
		writePaths = append(writePaths, cfg.Workspace)
	}
	writePaths = append(writePaths, "/tmp", "/private/tmp")
	for _, p := range writePaths {
		w("(allow file-write* (subpath %q))", p)
	}
	for _, p := range append(writePaths, "/dev") {
		w("(allow file-ioctl* (subpath %q))", p)
	}
	if cfg.AllowNet {
		w("(allow network-outbound)")
	}
	for _, extra := range cfg.ExtraProfileLines {
		w("%s", extra)
	}
	return b.String()
}

// Run executes argv under a dynamically generated Seatbelt policy.
func (s *darwinSandbox) Run(ctx context.Context, cfg Config, argv []string) (*Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	cfg = cfg.normalized()
	if err := validateWorkspace(cfg.Workspace); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	env := buildEnv(os.Environ(), homePath(), cfg.ExtraEnv)

	pf, err := os.CreateTemp("", "harness-sandbox-*.sb")
	if err != nil {
		return nil, fmt.Errorf("sandbox: allocate policy file: %w", err)
	}
	policyPath := pf.Name()
	defer os.Remove(policyPath)
	if _, err := pf.WriteString(seatbeltProfile(cfg)); err != nil {
		_ = pf.Close()
		return nil, fmt.Errorf("sandbox: write policy: %w", err)
	}
	if err := pf.Close(); err != nil {
		return nil, fmt.Errorf("sandbox: close policy: %w", err)
	}

	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{policyPath}, argv...)...)
	cmd.Dir = cfg.Workspace
	cmd.Env = env
	var outW, errW cappedWriter
	outW.keep = cfg.MaxOutput
	errW.keep = cfg.MaxOutput
	cmd.Stdout = &outW
	cmd.Stderr = &errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	// Process-group cleanup: the child leads its own group (Setpgid), so a
	// negative-pid kill reaps the entire spawned tree on timeout.
	if ctx.Err() == context.DeadlineExceeded && cmd.ProcessState != nil {
		_ = syscall.Kill(-cmd.ProcessState.Pid(), syscall.SIGKILL)
	}

	res := &Result{
		Stdout:          outW.bytes(),
		Stderr:          errW.bytes(),
		Duration:        dur,
		Command:         argv,
		Backend:         backendDarwin,
		StdoutTruncated: outW.truncated(),
		StderrTruncated: errW.truncated(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Warnings = append(res.Warnings, "execution timed out; process group killed")
		return res, nil
	}
	if ctx.Err() == context.Canceled {
		if cmd.ProcessState != nil {
			_ = syscall.Kill(-cmd.ProcessState.Pid(), syscall.SIGKILL)
		}
		return nil, fmt.Errorf("sandbox: cancelled: %w", ctx.Err())
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			return nil, fmt.Errorf("sandbox: exec: %w", runErr)
		}
	}
	return res, nil
}
