package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// fakeCLI writes an executable script that prints body to stdout and exits 0.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func cliEnv() *envelope.Envelope {
	env := envelope.New("alice", "bob", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Fix bug", Instructions: "Fix it."}
	return env
}

func TestClaudeRunStreamJSON(t *testing.T) {
	// Emulates `claude -p --output-format stream-json` output.
	cli := fakeCLI(t, `
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it"}]}}'
echo '{"type":"result","subtype":"success","result":"Fixed the bug in foo.go"}'
`)
	t.Setenv("SB_CLAUDE_BIN", cli)
	var progress []string
	res, err := ClaudeRun(cliEnv(), func(s string) { progress = append(progress, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "Fixed the bug in foo.go" {
		t.Fatalf("unexpected summary: %q", res.Summary)
	}
	if len(progress) != 1 || progress[0] != "Working on it" {
		t.Fatalf("progress mismatch: %v", progress)
	}
}

func TestClaudeRunDegradesToRawText(t *testing.T) {
	cli := fakeCLI(t, `echo 'plain text output, no JSON at all'`)
	t.Setenv("SB_CLAUDE_BIN", cli)
	res, err := ClaudeRun(cliEnv(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || !strings.Contains(res.Summary, "plain text") {
		t.Fatalf("expected degraded raw-text result, got %+v", res)
	}
}

func TestClaudeRunNonZeroExit(t *testing.T) {
	cli := fakeCLI(t, `echo 'something broke' >&2; exit 2`)
	t.Setenv("SB_CLAUDE_BIN", cli)
	if _, err := ClaudeRun(cliEnv(), func(string) {}); err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestCodexRunJSON(t *testing.T) {
	cli := fakeCLI(t, `
echo '{"type":"progress","text":"thinking"}'
echo '{"type":"result","text":"Codex did the thing"}'
`)
	t.Setenv("SB_CODEX_BIN", cli)
	res, err := CodexRun(cliEnv(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "Codex did the thing" {
		t.Fatalf("unexpected summary: %q", res.Summary)
	}
}

func TestOpencodeRunPlainText(t *testing.T) {
	cli := fakeCLI(t, `echo 'opencode output here'`)
	t.Setenv("SB_OPENCODE_BIN", cli)
	res, err := OpencodeRun(cliEnv(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "opencode output") {
		t.Fatalf("unexpected summary: %q", res.Summary)
	}
}

func TestEchoRun(t *testing.T) {
	env := cliEnv()
	env.Context.Files = []string{"x.go"}
	res, err := EchoRun(env, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "Fix bug") || len(res.Artifacts) != 1 {
		t.Fatalf("unexpected echo result: %+v", res)
	}
}

func TestEchoRunFailHook(t *testing.T) {
	t.Setenv("SB_ECHO_FAIL", "1")
	if _, err := EchoRun(cliEnv(), func(string) {}); err == nil {
		t.Fatal("expected SB_ECHO_FAIL to produce an error")
	}
}

func TestParseAgentLine(t *testing.T) {
	p, isRes, res := parseAgentLine([]byte(`{"type":"result","result":"done!"}`))
	if !isRes || res != "done!" || p != "" {
		t.Fatalf("result parse failed: %q %v %q", p, isRes, res)
	}
	p, isRes, _ = parseAgentLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`))
	if isRes || p != "hi" {
		t.Fatalf("assistant parse failed: %q %v", p, isRes)
	}
	p, isRes, _ = parseAgentLine([]byte(`not json`))
	if p != "" || isRes {
		t.Fatalf("non-JSON should yield nothing: %q %v", p, isRes)
	}
}
