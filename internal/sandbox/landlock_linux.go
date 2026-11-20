//go:build linux

package sandbox

// Landlock ABI structures (kernel >= 5.13). Sizes are fixed by the kernel:
// ruleset attr is a single u64; the path beneater rule is s32 + pad4 + two
// u64s.

// llRulesetAttr is struct landlock_ruleset_attr.
type llRulesetAttr struct {
	HandledAccessFS uint64
}

// llPathBeneaterAttr is struct landlock_path_beneater_attr.
type llPathBeneaterAttr struct {
	RequestorFD   int32
	_pad          [4]byte
	ResolvedFD    uint64
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
