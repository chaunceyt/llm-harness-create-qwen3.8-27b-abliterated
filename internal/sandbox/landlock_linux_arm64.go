//go:build linux && arm64

package sandbox

// AArch64 landlock syscall numbers (asm/unistd.h).
const (
	llSysCreateRuleset = 450 // SYS_landlock_create_ruleset
	llSysAddRule       = 451 // SYS_landlock_add_rule
	llSysRestrictSelf  = 452 // SYS_landlock_restrict_self
)
