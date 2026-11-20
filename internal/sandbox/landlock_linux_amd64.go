//go:build linux && amd64

package sandbox

// x86-64 landlock syscall numbers (asm/unistd_64.h).
const (
	llSysCreateRuleset = 444 // SYS_landlock_create_ruleset
	llSysAddRule       = 445 // SYS_landlock_add_rule
	llSysRestrictSelf  = 446 // SYS_landlock_restrict_self
)
