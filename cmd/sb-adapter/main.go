// Command sb-adapter hosts the v1 adapters as separate processes speaking the
// adapter stdio protocol: `sb-adapter echo|claude|codex|opencode`.
package main

import (
	"fmt"
	"os"

	"github.com/TamTunnel/switchboard/internal/adapter"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sb-adapter <echo|claude|codex|opencode>")
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "echo":
		code = adapter.Serve("echo", []string{"echo"}, adapter.EchoRun)
	case "claude":
		code = adapter.Serve("claude", []string{"code", "review"}, adapter.ClaudeRun)
	case "codex":
		code = adapter.Serve("codex", []string{"code", "review"}, adapter.CodexRun)
	case "opencode":
		code = adapter.Serve("opencode", []string{"code", "review"}, adapter.OpencodeRun)
	default:
		fmt.Fprintf(os.Stderr, "sb-adapter: unknown adapter %q\n", os.Args[1])
		os.Exit(2)
	}
	os.Exit(code)
}
