//go:build linux && !amd64 && !arm64

package sandbox

// Landlock syscall numbers are pinned only for architectures whose numbers
// are stable and verified (amd64, arm64). Elsewhere landlock is skipped.
const (
	llSysCreateRuleset = 0
	llSysAddRule       = 0
	llSysRestrictSelf  = 0
)
