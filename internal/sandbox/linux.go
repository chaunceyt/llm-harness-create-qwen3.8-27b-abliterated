//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const backendLinux = "linux-namespaces"

func newSandbox() Sandbox { return &linuxSandbox{} }

type linuxSandbox struct{}

// Inspect probes live capabilities of this kernel/container pairing.
func (s *linuxSandbox) Inspect() Info {
	info := Info{
		Backend:      backendLinux,
		Capabilities: []string{"user-namespace", "mount-namespace", "pid-namespace", "uts-namespace", "ipc-namespace", "network-namespace", "landlock"},
	}
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err == nil {
		var rel strings.Builder
		for _, b := range uts.Release[:] {
			if b == 0 {
				break
			}
			rel.WriteByte(byte(b))
		}
		info.Notes = append(info.Notes, "kernel "+rel.String())
	}
	if v, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		info.Notes = append(info.Notes, "unprivileged_userns_clone="+strings.TrimSpace(string(v)))
	}
	if probeLandlock() {
		info.Capabilities[len(info.Capabilities)-1] = "landlock(active)"
	} else {
		info.Notes = append(info.Notes, "landlock probe failed; write confinement degrades to mount read-only")
	}
	if err := probeNamespaces(); err == nil {
		info.Notes = append(info.Notes, "namespace creation available")
	} else {
		info.Notes = append(info.Notes, fmt.Sprintf("namespace creation denied (%v); degraded lane active", err))
	}
	return info
}

// probeNamespaces attempts a minimal full-flag clone(2) to test whether this
// kernel/container pairing permits namespace creation at all. The probe child
// exits immediately (a syscall-only child path in the runtime's own
// fork-exec style), so the probe is cheap and race-free.
func probeNamespaces() error {
	flags := uintptr(syscall.CLONE_NEWUSER |
		syscall.CLONE_NEWNS |
		syscall.CLONE_NEWPID |
		syscall.CLONE_NEWUTS |
		syscall.CLONE_NEWIPC |
		syscall.CLONE_NEWNET |
		syscall.SIGCHLD)
	r1, _, errno := syscall.RawSyscall(syscall.SYS_CLONE, uintptr(flags), 0, 0)
	if errno != 0 {
		return errno
	}
	if r1 == 0 {
		syscall.Exit(0)
	}
	_, _ = syscall.Wait4(int(r1), nil, 0, nil)
	return nil
}

// Run executes argv inside a fresh set of namespaces created via clone(2).
func (s *linuxSandbox) Run(ctx context.Context, cfg Config, argv []string) (*Result, error) {
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

	// Stdio plumbing exists before clone so the child can capture the fd
	// numbers at exec time.
	inPipe := makePipe()
	outPipe := makePipe()
	errPipe := makePipe()
	inR, outW, errW := int(inPipe.r.Fd()), int(outPipe.w.Fd()), int(errPipe.w.Fd())

	// clone(2) with NULL parent/child tids: fork(2)-like returns (parent
	// receives the child pid, child receives 0) plus fresh namespaces.
	// SYS_CLONE is taken from the syscall package's per-architecture tables.
	flags := uintptr(syscall.CLONE_NEWUSER |
		syscall.CLONE_NEWNS |
		syscall.CLONE_NEWPID |
		syscall.CLONE_NEWUTS |
		syscall.CLONE_NEWIPC |
		syscall.SIGCHLD)
	if !cfg.AllowNet {
		flags |= syscall.CLONE_NEWNET
	}
	start := time.Now()
	r1, _, errno := syscall.RawSyscall(syscall.SYS_CLONE, uintptr(flags), 0, 0)
	if errno != 0 {
		closePipes(inPipe, outPipe, errPipe)
		if errors.Is(errno, syscall.EPERM) || errors.Is(errno, syscall.EACCES) {
			// Namespace creation blocked (seccomp/container policy). Take the
			// degraded lane with a loud, surfaced warning.
			return degradedRun(ctx, cfg, argv, env, []string{
				fmt.Sprintf("namespace isolation unavailable (%v); running degraded with limits only", errno),
			})
		}
		return nil, fmt.Errorf("sandbox: clone: %w", errno)
	}
	if r1 == 0 {
		// Child side: exec the command or exit(125); never returns.
		linuxChildRun(cfg, argv, env, inR, outW, errW)
		return nil, fmt.Errorf("sandbox: child returned unexpectedly")
	}
	pid := int(r1)

	// Write the child's identity maps, then surrender every parent-held pipe
	// end except the two read ends. The child blocks on map-poll before any
	// privileged step, which makes write ordering a liveness condition
	// rather than a race.
	if err := writeIDMap(pid, os.Getuid(), os.Getgid()); err != nil {
		reapTree(pid)
		_, _ = syscall.Wait4(pid, nil, 0, nil)
		closePipes(inPipe, outPipe, errPipe)
		return nil, fmt.Errorf("sandbox: write id maps: %w", err)
	}
	_ = inPipe.closeBoth()
	_ = outPipe.closeWriteEnd()
	_ = errPipe.closeWriteEnd()

	var (
		stdoutW, stderrW cappedWriter
		wg               sync.WaitGroup
	)
	// Concurrent drains: draining two pipes sequentially deadlocks when the
	// command fills the second pipe before the first reaches EOF.
	wg.Add(2)
	go func() {
		defer wg.Done()
		var derr error
		stdoutW, derr = drainCapped(outPipe.readEnd(), cfg.MaxOutput)
		_ = derr
	}()
	go func() {
		defer wg.Done()
		var derr error
		stderrW, derr = drainCapped(errPipe.readEnd(), cfg.MaxOutput)
		_ = derr
	}()
	drainDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(drainDone)
	}()

	killed := false
	select {
	case <-ctx.Done():
		killed = true
		reapTree(pid)
	case <-drainDone:
	}
	_ = outPipe.closeReadEnd()
	_ = errPipe.closeReadEnd()
	wg.Wait()

	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, 0, nil)

	res := &Result{
		Stdout:          stdoutW.bytes(),
		Stderr:          stderrW.bytes(),
		Duration:        time.Since(start),
		Command:         argv,
		Backend:         backendLinux,
		StdoutTruncated: stdoutW.truncated(),
		StderrTruncated: stderrW.truncated(),
	}
	if killed && ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("sandbox: cancelled: %w", ctx.Err())
	}
	if killed && ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Warnings = append(res.Warnings, "execution timed out; process tree killed")
		return res, nil
	}
	switch {
	case ws.Exited():
		res.ExitCode = ws.ExitStatus()
	case ws.Signaled():
		res.ExitCode = 128 + int(ws.Signal())
		res.Warnings = append(res.Warnings, fmt.Sprintf("terminated by signal %s", ws.Signal()))
	default:
		res.ExitCode = -1
		res.Warnings = append(res.Warnings, "unknown exit state")
	}
	return res, nil
}

// writeIDMap writes the child's uid/gid maps mapping its single in-namespace
// identity (0) to the parent's own host credentials. Mapping only the writer's
// own credentials is the documented unprivileged user-namespace bootstrap and
// requires no setgroups manipulation.
func writeIDMap(pid, uid, gid int) error {
	base := "/proc/" + strconv.Itoa(pid)
	if err := os.WriteFile(base+"/uid_map", []byte(fmt.Sprintf("0 %d 1\n", uid)), 0o644); err != nil {
		return fmt.Errorf("uid_map: %w", err)
	}
	if err := os.WriteFile(base+"/gid_map", []byte(fmt.Sprintf("0 %d 1\n", gid)), 0o644); err != nil {
		return fmt.Errorf("gid_map: %w", err)
	}
	return nil
}

// pipe is one parent<->child byte pipe with explicit end ownership.
type pipe struct {
	r, w *os.File
}

func makePipe() *pipe {
	r, w, _ := os.Pipe()
	return &pipe{r: r, w: w}
}

func (p *pipe) readEnd() *os.File { return p.r }

func (p *pipe) closeBoth() error {
	var e error
	if err := p.r.Close(); err != nil {
		e = err
	}
	if err := p.w.Close(); err != nil && e == nil {
		e = err
	}
	return e
}

func (p *pipe) closeWriteEnd() error { return p.w.Close() }

func (p *pipe) closeReadEnd() error { return p.r.Close() }

func closePipes(ps ...*pipe) {
	for _, p := range ps {
		p.closeBoth()
	}
}

// --- child-side setup ------------------------------------------------------

// linuxChildRun runs inside the cloned child after namespace creation. It
// waits for the parent-written identity maps, swaps stdio onto the parent's
// pipes, rebuilds the mount tree, applies landlock, and execs the command.
// It never returns: exec replaces the image; failures exit(125).
func linuxChildRun(cfg Config, argv []string, env []string, inR, outW, errW int) {
	if !idMapsReady(5 * time.Second) {
		childFatalf("identity maps never written by parent")
	}
	if err := swapStdio(inR, outW, errW); err != nil {
		childFatalf("stdio swap: %v", err)
	}
	if err := setupMounts(cfg); err != nil {
		childFatalf("mount setup: %v", err)
	}
	_ = unix.Sethostname([]byte("harness-sandbox"))
	applyLandlock(cfg)
	if err := os.Chdir(cfg.Workspace); err != nil {
		childFatalf("chdir %s: %v", cfg.Workspace, err)
	}
	if err := syscall.Exec(argv[0], argv, env); err != nil {
		childFatalf("exec %s: %v", argv[0], err)
	}
}

// swapStdio redirects fds 0/1/2 onto the parent pipes and closes the
// inherited duplicates so the parent sees EOF exactly when the command
// exits.
func swapStdio(inR, outW, errW int) error {
	for _, pair := range [][2]int{{inR, 0}, {outW, 1}, {errW, 2}} {
		if err := unix.Dup2(pair[0], pair[1]); err != nil {
			return fmt.Errorf("dup2(%d,%d): %w", pair[0], pair[1], err)
		}
	}
	for _, fd := range []int{inR, outW, errW} {
		_ = unix.Close(fd)
	}
	return nil
}

// idMapsReady polls until the parent has written this child's uid/gid maps.
// The child stops itself before resuming only after SIGCONT; the poll makes
// map-write ordering a liveness condition, not a race.
func idMapsReady(deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		b, err := os.ReadFile("/proc/self/uid_map")
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func childFatalf(format string, a ...any) {
	msg := fmt.Sprintf("sandbox: "+format+"\n", a...)
	_, _ = syscall.Write(2, []byte(msg))
	syscall.Exit(125)
}

func childWarnf(format string, a ...any) {
	_, _ = syscall.Write(2, []byte(fmt.Sprintf("sandbox: warn: "+format+"\n", a...)))
}

// --- mount tree ------------------------------------------------------------

// setupMounts rebuilds the filesystem view inside the fresh mount namespace:
// tmpfs over "/", read-only system prefix binds, empty tmpfs $HOME, workspace
// bind at its true path, fresh procfs/sysfs, /dev, and an ephemeral /tmp.
func setupMounts(cfg Config) error {
	ro := func(source, target string) {
		if _, err := os.Lstat(source); err != nil {
			childWarnf("skip %s: %v", source, err)
			return
		}
		_ = os.MkdirAll(target, 0o755)
		if err := unix.Mount(source, target, "none", unix.MS_BIND|unix.MS_REC|unix.MS_RDONLY, ""); err != nil {
			childWarnf("ro-bind %s -> %s: %v", source, target, err)
		}
	}
	roFile := func(source, target string) {
		if _, err := os.Lstat(source); err != nil {
			return
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		if err := unix.Mount(source, target, "none", unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
			childWarnf("ro-file-bind %s -> %s: %v", source, target, err)
		}
	}

	// Blank slate: tmpfs over the root. Everything previously mounted under
	// / (including container overlay layers) is hidden behind it.
	if err := unix.Mount("none", "/", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("tmpfs over /: %w", err)
	}
	// Optional system prefixes, read-only.
	for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc/ssl"} {
		ro(p, p)
	}
	for _, f := range []string{"/etc/passwd", "/etc/group", "/etc/nsswitch.conf"} {
		roFile(f, f)
	}
	if cfg.AllowNet {
		roFile("/etc/resolv.conf", "/etc/resolv.conf")
	}
	// Ephemeral scratch.
	if err := os.MkdirAll("/tmp", 0o1777); err == nil {
		if err := unix.Mount("none", "/tmp", "tmpfs", 0, ""); err != nil {
			childWarnf("tmpfs over /tmp: %v", err)
		}
	}
	// Empty the host $HOME tree (hides ~/.ssh, ~/.aws, ~/.env and friends).
	if home := homePath(); home != "" && home != "/" {
		if err := os.MkdirAll(home, 0o700); err == nil {
			if err := unix.Mount("none", home, "tmpfs", 0, ""); err != nil {
				childWarnf("tmpfs over %s: %v", home, err)
			}
		}
	}
	// Workspace at its true path: read-write or read-only.
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REC)
	if !cfg.AllowWrite {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(cfg.Workspace, cfg.Workspace, "none", flags, ""); err != nil {
		return fmt.Errorf("bind workspace %s: %w", cfg.Workspace, err)
	}
	// Fresh virtual filesystems of the child's own namespaces.
	if err := os.MkdirAll("/proc", 0o555); err == nil {
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			childWarnf("procfs: %v", err)
		}
	}
	if err := os.MkdirAll("/sys", 0o555); err == nil {
		if err := unix.Mount("sysfs", "/sys", "sysfs", unix.MS_RDONLY, ""); err != nil {
			childWarnf("sysfs: %v", err)
		}
	}
	if err := os.MkdirAll("/dev", 0o755); err == nil {
		if _, lerr := os.Lstat("/dev"); lerr == nil {
			if err := unix.Mount("/dev", "/dev", "none", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				childWarnf("bind /dev: %v", err)
			}
		}
	}
	return nil
}

// --- process-tree cleanup ---------------------------------------------------

// reapTree SIGKILLs pid and every descendant discoverable through the /proc
// ppid links. Group signaling is denied by many container runtimes (kill(-pgid)
// returns EPERM) and /proc/<pid>/children is absent without CONFIG_PROC_CHILDREN,
// so a ppid-walk with direct per-process kills is the universal cleanup
// primitive — host-visible across PID namespaces as well.
func reapTree(pid int) {
	kids := procChildrenMap()
	queue := []int{pid}
	seen := make(map[int]bool)
	for len(queue) > 0 && len(seen) < 4096 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		_ = syscall.Kill(p, syscall.SIGKILL)
		queue = append(queue, kids[p]...)
	}
}

// procChildrenMap snapshots parent->child pid links from /proc/*/stat. stat's
// comm field may contain spaces and parentheses, so fields after the last ')'
// are parsed: state first, then ppid.
func procChildrenMap() map[int][]int {
	out := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 >= len(s) {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out[ppid] = append(out[ppid], pid)
	}
	return out
}

// --- degraded lane ------------------------------------------------------------

// degradedRun executes without namespace isolation (limits only) and labels
// the result loudly. Used only when clone(2) namespace flags are denied. The
// ctx already carries cfg.Timeout's deadline (set by Run); deadline handling
// is explicit below — process-group kill on expiry — not delegated to
// exec.CommandContext's cancel semantics.
func degradedRun(ctx context.Context, cfg Config, argv []string, env []string, warns []string) (*Result, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.Workspace
	cmd.Env = env
	var outW, errW cappedWriter
	outW.keep = cfg.MaxOutput
	errW.keep = cfg.MaxOutput
	cmd.Stdout = &outW
	cmd.Stderr = &errW
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sandbox: degraded start: %w", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	killed := false
	var runErr error
	select {
	case runErr = <-waitDone:
	case <-ctx.Done():
		// Group signaling is unreliable under container runtimes, so cleanup
		// walks the /proc ppid links and kills each discovered member
		// directly; the wait completes promptly once the tree is dead.
		killed = true
		reapTree(cmd.Process.Pid)
		runErr = <-waitDone
	}
	dur := time.Since(start)
	res := &Result{
		Stdout:          outW.bytes(),
		Stderr:          errW.bytes(),
		Duration:        dur,
		Command:         argv,
		Backend:         "linux-degraded",
		StdoutTruncated: outW.truncated(),
		StderrTruncated: errW.truncated(),
		Warnings:        warns,
	}
	if killed && ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("sandbox: cancelled: %w", ctx.Err())
	}
	if killed {
		res.TimedOut = true
		res.ExitCode = -1
		res.Warnings = append(res.Warnings, "execution timed out; process tree killed")
		return res, nil
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			return nil, fmt.Errorf("sandbox: degraded exec: %w", runErr)
		}
	}
	return res, nil
}
