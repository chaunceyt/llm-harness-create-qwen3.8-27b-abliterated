//go:build linux && (amd64 || arm64)

package sandbox

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock ABI structures (kernel >= 5.13). Sizes are fixed by the kernel:
// the ruleset attribute is a single u64; the path-beneater attribute is
// two s32s followed by a u64 (size 16).

// llRulesetAttr is struct landlock_ruleset_attr.
type llRulesetAttr struct {
	HandledAccessFS uint64
}

// llPathBeneaterAttr is struct landlock_path_beneater_attr.
type llPathBeneaterAttr struct {
	RequestorFD   int32
	ResolvedFD    int32
	AllowedAccess uint64
}

// Landlock file-access bits (LANDLOCK_ACCESS_FS_*).
const (
	llAccessExecute     uint64 = 1 << 0
	llAccessOpenWrite   uint64 = 1 << 1
	llAccessAppend      uint64 = 1 << 2
	llAccessIoUringOp   uint64 = 1 << 3
	llAccessLock        uint64 = 1 << 4
	llAccessFile        uint64 = 1 << 5
	llAccessReadContent uint64 = 1 << 6
	llAccessReadMeta    uint64 = 1 << 7
	llAccessRefer       uint64 = 1 << 8
	llAccessTruncate    uint64 = 1 << 9
	llAccessIoctlDir    uint64 = 1 << 10
)

const (
	// llAccessFull grants every known bit; used for paths that are meant to
	// be fully usable (workspace, /tmp, /dev, the emptied home tmpfs).
	llAccessFull = llAccessExecute |
		llAccessOpenWrite |
		llAccessAppend |
		llAccessIoUringOp |
		llAccessLock |
		llAccessFile |
		llAccessReadContent |
		llAccessReadMeta |
		llAccessRefer |
		llAccessTruncate |
		llAccessIoctlDir

	// llAccessRead permits inspecting and executing under system prefixes.
	llAccessRead = llAccessExecute |
		llAccessReadContent |
		llAccessReadMeta |
		llAccessIoctlDir
)

// landlockRuleTypePathBeneater selects path-beneater rules.
const landlockRuleTypePathBeneater = 1

// applyLandlock confines post-exec file access: full access inside the
// workspace, /tmp, /dev and the (emptied) home; read/execute on system
// prefixes; everything else denied. It degrades with a surfaced warning when
// the kernel lacks landlock or rejects the ruleset.
func applyLandlock(cfg Config) {
	rules := []struct {
		path   string
		access uint64
	}{
		{cfg.Workspace, llAccessFull},
		{"/tmp", llAccessFull},
		{"/dev", llAccessFull},
		{"/usr", llAccessRead},
		{"/bin", llAccessRead},
		{"/lib", llAccessRead},
		{"/lib64", llAccessRead},
		{"/etc/ssl", llAccessRead},
		{"/proc", llAccessRead},
		{"/sys", llAccessRead},
	}
	if home := homePath(); home != "" {
		// The home tmpfs is empty, so full access there leaks nothing while
		// keeping tool caches functional.
		rules = append(rules, struct {
			path   string
			access uint64
		}{home, llAccessFull})
	}

	var attr llRulesetAttr
	attr.HandledAccessFS = llAccessFull
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Sizeof(attr)), 0)
	if errno != 0 {
		childWarnf("landlock create_ruleset: %v; continuing without landlock", errno)
		return
	}
	rsFD := int(r1)
	defer unix.Close(rsFD)
	for _, ru := range rules {
		f, err := unix.Open(ru.path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		var rule llPathBeneaterAttr
		rule.RequestorFD = int32(unix.Getpid())
		rule.ResolvedFD = int32(f)
		rule.AllowedAccess = ru.access
		_, _, addErr := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
			uintptr(rsFD), landlockRuleTypePathBeneater,
			uintptr(unsafe.Pointer(&rule)), uintptr(unsafe.Sizeof(rule)), 0, 0)
		unix.Close(f)
		if addErr != 0 {
			childWarnf("landlock add_rule %s: %v; continuing", ru.path, addErr)
		}
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rsFD), 0, 0)
	if errno != 0 {
		childWarnf("landlock restrict_self: %v; continuing without landlock", errno)
	}
}

// probeLandlock attempts a minimal ruleset creation to test availability.
func probeLandlock() bool {
	var attr llRulesetAttr
	attr.HandledAccessFS = llAccessRead
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Sizeof(attr)), 0)
	if errno != 0 {
		return false
	}
	unix.Close(int(r1))
	return true
}
