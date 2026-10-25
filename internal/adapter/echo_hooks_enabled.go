//go:build switchboardtesthooks

package adapter

// echoHooksEnabled reports whether the echo test hooks (SB_ECHO_FAIL,
// SB_ECHO_HANG) are honored. Only builds with the switchboardtesthooks tag
// enable them — test binaries built by daemon TestMain, never releases.
func echoHooksEnabled() bool { return true }
