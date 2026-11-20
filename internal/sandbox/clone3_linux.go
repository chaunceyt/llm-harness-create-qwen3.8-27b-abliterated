//go:build linux && (amd64 || arm64)

package sandbox

// clone3(2) syscall number, verified against the Go toolchain's own
// generated tables (_SYS_clone3 = 435 on both architectures).
const llSysClone3 = 435
