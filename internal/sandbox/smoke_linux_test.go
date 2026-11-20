//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSmoke exercises the live jail end-to-end under whichever posture this
// kernel/container pairing actually permits: the namespaced lane (mount
// blank-slate, HOME hiding, landlock write confinement, network isolation)
// or the loudly-labeled degraded lane (limits only). Every Run re-exercises
// the concurrent parent/child clone path, so the iteration loop is a
// shakeout for intermittent stack corruption.
func TestSmoke(t *testing.T) {
	const iterations = 10
	for it := 0; it < iterations; it++ {
		smokeOnce(t, it)
	}
}

func smokeOnce(t *testing.T, it int) {
	t.Helper()
	ws := t.TempDir()
	sb := New()
	ctx := context.Background()

	info := sb.Inspect()
	if info.Backend != backendLinux {
		t.Fatalf("backend = %q, want %q", info.Backend, backendLinux)
	}
	nsOK := probeNamespaces() == nil
	landlockLive := hasCapability(info.Capabilities, "landlock(active)")
	if it == 0 {
		t.Logf("posture: namespaced=%v landlock=%v notes=%v", nsOK, landlockLive, info.Notes)
	}

	must := func(tag string, cfg Config, script string) *Result {
		t.Helper()
		res, err := sb.Run(ctx, cfg, []string{"/bin/sh", "-c", script})
		if err != nil {
			t.Fatalf("%s: transport: %v", tag, err)
		}
		if res == nil {
			t.Fatalf("%s: nil result", tag)
		}
		return res
	}

	// Tool availability probes (pure-shell checks; adapt heavy-output checks).
	haveYes := must("probe", Config{Workspace: ws}, "command -v yes && command -v head").ExitCode == 0
	haveDD := must("probe-dd", Config{Workspace: ws}, "command -v dd").ExitCode == 0

	// s1: execution lands in the right lane, labeled loudly.
	res := must("s1", Config{Workspace: ws}, "echo hello-smoke")
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hello-smoke") {
		t.Fatalf("s1: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if nsOK {
		if res.Backend != backendLinux || len(res.Warnings) != 0 {
			t.Fatalf("s1: backend=%q warnings=%v", res.Backend, res.Warnings)
		}
	} else {
		if res.Backend != "linux-degraded" || len(res.Warnings) == 0 {
			t.Fatalf("s1: degraded lane not loudly labeled: backend=%q warnings=%v", res.Backend, res.Warnings)
		}
	}

	if nsOK {
		// s2a: blank-slate root is tmpfs.
		res = must("s2a", Config{Workspace: ws}, "cat /proc/mounts")
		if !strings.Contains(string(res.Stdout), " / tmpfs ") {
			t.Fatalf("s2a: no tmpfs over / in: %q", res.Stdout)
		}

		// s2b: host HOME contents are hidden behind the empty home tmpfs.
		res = must("s2b", Config{Workspace: ws}, `test -e "$HOME/.ssh" && echo HOME-LEAKED || echo HOME-HIDDEN`)
		if !strings.Contains(string(res.Stdout), "HOME-HIDDEN") || strings.Contains(string(res.Stdout), "HOME-LEAKED") {
			t.Fatalf("s2b: stdout=%q stderr=%q", res.Stdout, res.Stderr)
		}
		if home := homePath(); home != "" {
			probeFile := filepath.Join(home, "sbxsmoke-probe")
			res = must("s2b-w", Config{Workspace: ws}, `echo x > "$HOME/sbxsmoke-probe"`)
			if res.ExitCode != 0 {
				t.Fatalf("s2b-w: write into emptied home failed: exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			if _, serr := os.Stat(probeFile); !os.IsNotExist(serr) {
				t.Fatalf("s2b-w: host home sees sandbox write %s", probeFile)
			}
		}

		// s2c: writes outside the workspace are confined. With landlock live
		// the writable root tmpfs must still deny them; without it they land
		// on the child-private tmpfs (invisible to the host either way).
		script := fmt.Sprintf("echo x > /sbxsmoke-probe-%d && echo LL-BYPASSED", it)
		res = must("s2c", Config{Workspace: ws}, script)
		if landlockLive {
			if res.ExitCode == 0 || strings.Contains(string(res.Stdout), "LL-BYPASSED") {
				t.Fatalf("s2c: landlock live but write outside workspace succeeded: exit=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
			}
		} else {
			if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "LL-BYPASSED") {
				t.Fatalf("s2c: degraded landlock should permit root-tmpfs write: exit=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
			}
		}
		if _, serr := os.Stat(fmt.Sprintf("/sbxsmoke-probe-%d", it)); !os.IsNotExist(serr) {
			t.Fatalf("s2c: host sees child root-tmpfs write")
		}

		// s3: network namespace exposes only loopback.
		res = must("s3", Config{Workspace: ws}, `for d in /sys/class/net/*; do echo "$d"; done`)
		entries := strings.Fields(string(res.Stdout))
		if len(entries) != 1 || !strings.HasSuffix(entries[0], "/lo") {
			t.Fatalf("s3: interfaces=%v", entries)
		}
	} else {
		t.Logf("s2/s3/s8b: skipped (namespaces unavailable; confinement requires the namespaced lane)")
	}

	// s4: timeout kills the process tree, including grandchildren.
	start := time.Now()
	res = must("s4", Config{Workspace: ws, Timeout: 700 * time.Millisecond}, `sh -c "sleep 25"`)
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("s4: timedOut=%v exit=%d", res.TimedOut, res.ExitCode)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("s4: took %v", d)
	}
	noLinger(t, "sleep 25")

	// s5: stdout truncation retains the first MaxOutput bytes.
	if haveYes {
		res = must("s5", Config{Workspace: ws, MaxOutput: 100000}, "yes A | head -c 1000000")
	} else if haveDD {
		res = must("s5", Config{Workspace: ws, MaxOutput: 100000}, "dd if=/dev/zero bs=1000 count=1000 2>/dev/null")
	} else {
		t.Fatalf("s5: unavailable (no yes or dd)")
	}
	if !res.StdoutTruncated || len(res.Stdout) != 100000 {
		t.Fatalf("s5: truncated=%v len=%d", res.StdoutTruncated, len(res.Stdout))
	}

	// s6: concurrent-drain regression. A sequential stderr drain deadlocks
	// here (stderr fills its pipe before stdout reaches EOF) and hangs until
	// timeout; concurrent drains complete.
	var script6 string
	if haveYes {
		script6 = "yes B | head -c 300000 >&2; echo done-stdout"
	} else if haveDD {
		script6 = "dd if=/dev/zero bs=1000 count=300 2>/dev/null >&2; echo done-stdout"
	} else {
		t.Fatalf("s6: unavailable (no yes or dd)")
	}
	res = must("s6", Config{Workspace: ws, MaxOutput: 1 << 20, Timeout: 20 * time.Second}, script6)
	if res.TimedOut {
		t.Fatalf("s6: drain deadlock regression fired: stderr=%q", res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "done-stdout") || len(res.Stderr) != 300000 {
		t.Fatalf("s6: stdout=%q stderrLen=%d", res.Stdout, len(res.Stderr))
	}

	// s8a: workspace writable under coder posture, visible to the host.
	res = must("s8a", Config{Workspace: ws, AllowWrite: true}, "echo ok > wsfile-smoke && cat wsfile-smoke")
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "ok") {
		t.Fatalf("s8a: exit=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if _, serr := os.Stat(filepath.Join(ws, "wsfile-smoke")); serr != nil {
		t.Fatalf("s8a: host does not see workspace write: %v", serr)
	}

	if nsOK {
		// s8b: reviewer posture makes the workspace read-only.
		res = must("s8b", Config{Workspace: ws, AllowWrite: false}, "echo nope > wsfile-ro")
		if res.ExitCode == 0 {
			t.Fatalf("s8b: read-only workspace write succeeded: out=%q", res.Stdout)
		}
		if _, serr := os.Stat(filepath.Join(ws, "wsfile-ro")); !os.IsNotExist(serr) {
			t.Fatalf("s8b: workspace write landed despite read-only posture")
		}
	}

	// s9: env hygiene drops non-allowlisted variables.
	os.Setenv("SMOKE_LEAK_MARKER", "leak")
	os.Setenv("AWS_SECRET_ACCESS_KEY", "leak")
	defer os.Unsetenv("SMOKE_LEAK_MARKER")
	defer os.Unsetenv("AWS_SECRET_ACCESS_KEY")
	res = must("s9", Config{Workspace: ws}, `echo "[${SMOKE_LEAK_MARKER}][${AWS_SECRET_ACCESS_KEY}]"; echo "PATH=${PATH}"`)
	lines := strings.Split(string(res.Stdout), "\n")
	if len(lines) < 2 || lines[0] != "[][]" || !strings.HasPrefix(lines[1], "PATH=") || len(lines[1]) <= len("PATH=") {
		t.Fatalf("s9: stdout=%q", res.Stdout)
	}
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// noLinger fails the test if any host-visible process still carries marker
// in its cmdline. Zombies carry empty cmdlines, so this is race-safe.
func noLinger(t *testing.T, marker string) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("noLinger: %v", err)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(b), marker) {
			t.Fatalf("lingering process %s: %q", e.Name(), b)
		}
	}
}
