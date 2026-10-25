//go:build !switchboardtesthooks

package adapter

// echoHooksEnabled reports whether the echo test hooks (SB_ECHO_FAIL,
// SB_ECHO_HANG) are honored. Default builds disable them: shipped binaries
// must never let an environment variable fail or hang echo runs.
func echoHooksEnabled() bool { return false }
