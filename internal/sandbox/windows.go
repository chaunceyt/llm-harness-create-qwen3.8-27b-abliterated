//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const backendWindows = "windows-degraded"

func newSandbox() Sandbox { return &windowsSandbox{} }

type windowsSandbox struct{}

func (s *windowsSandbox) Inspect() Info {
	return Info{
		Backend:      backendWindows,
		Capabilities: []string{"env-hygiene", "timeouts", "output-truncation"},
		Notes:        []string{"no native process jail on this platform; limits-only execution"},
	}
}

// Run executes with limits only. Windows lacks a portable equivalent of
// namespaces/Seatbelt in the standard library; the result is labeled loudly.
func (s *windowsSandbox) Run(ctx context.Context, cfg Config, argv []string) (*Result, error) {
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

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cfg.Workspace
	cmd.Env = env
	var outW, errW cappedWriter
	outW.keep = cfg.MaxOutput
	errW.keep = cfg.MaxOutput
	cmd.Stdout = &outW
	cmd.Stderr = &errW

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := &Result{
		Stdout:          outW.bytes(),
		Stderr:          errW.bytes(),
		Duration:        dur,
		Command:         argv,
		Backend:         backendWindows,
		StdoutTruncated: outW.truncated(),
		StderrTruncated: errW.truncated(),
		Warnings:        []string{"no process jail available on windows; limits-only execution"},
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if ctx.Err() == context.Canceled {
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
