//go:build !switchboardtesthooks

package adapter

import "testing"

func TestEchoRunHooksDisabledByDefault(t *testing.T) {
	// F22: in a default build the echo test hooks are inert — a stray
	// SB_ECHO_FAIL=1 in the daemon's environment must not fail runs.
	t.Setenv("SB_ECHO_FAIL", "1")
	if _, err := EchoRun(cliEnv(), func(string) {}); err != nil {
		t.Fatalf("hooks must be disabled without the switchboardtesthooks tag, got error: %v", err)
	}
}
