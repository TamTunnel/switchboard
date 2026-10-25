//go:build switchboardtesthooks

package adapter

import "testing"

func TestEchoRunFailHook(t *testing.T) {
	t.Setenv("SB_ECHO_FAIL", "1")
	if _, err := EchoRun(cliEnv(), func(string) {}); err == nil {
		t.Fatal("expected SB_ECHO_FAIL to produce an error")
	}
}
