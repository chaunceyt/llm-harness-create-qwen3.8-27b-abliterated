//go:build linux && !(amd64 || arm64)

package sandbox

// Landlock syscall numbers are pinned only for amd64 and arm64; elsewhere the
// write-confinement stage is skipped (mount read-only still applies).
func applyLandlock(cfg Config) {}

func probeLandlock() bool { return false }
