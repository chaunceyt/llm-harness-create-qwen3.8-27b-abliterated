//go:build linux && !(amd64 || arm64)

package sandbox

// No verified clone3 number pinned for this architecture; Run takes the
// degraded lane.
const llSysClone3 = 0
