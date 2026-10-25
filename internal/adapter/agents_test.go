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

func TestCodexRunRealExecJSONShapes(t *testing.T) {
	// Real `codex exec --json` event shapes (documented stream):
	// thread.started, turn.started, item.completed/agent_message, turn.completed.
	cli := fakeCLI(t, `
echo '{"type":"thread.started","thread_id":"thread_123"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Implemented rate limiting"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":50}}'
`)
	t.Setenv("SB_CODEX_BIN", cli)
	res, err := CodexRun(cliEnv(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "Implemented rate limiting" || res.Degraded {
		t.Fatalf("expected structured codex result, got %+v", res)
	}
}

func TestCodexRunTurnFailedIsError(t *testing.T) {
	// A turn.failed run must be an error even when the CLI exits 0:
	// previously it degraded to a "successful" raw-text result.
	cli := fakeCLI(t, `
echo '{"type":"thread.started","thread_id":"thread_123"}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"partial work"}}'
echo '{"type":"turn.failed","error":{"message":"sandbox denied write outside workspace"}}'
`)
	t.Setenv("SB_CODEX_BIN", cli)
	if _, err := CodexRun(cliEnv(), func(string) {}); err == nil || !strings.Contains(err.Error(), "sandbox denied") {
		t.Fatalf("expected turn.failed error, got %v", err)
	}
}

func TestCodexRunPassesSandboxFlags(t *testing.T) {
	// The fake CLI echoes its argv; the degraded summary must show the
	// sandbox/git flags that make implement-task handoffs actually work.
	cli := fakeCLI(t, `echo "ARGV:$*"`)
	t.Setenv("SB_CODEX_BIN", cli)
	res, err := CodexRun(cliEnv(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--skip-git-repo-check", "--sandbox", "workspace-write"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("codex argv missing %q, summary:\n%s", want, res.Summary)
		}
	}
}

func TestCodexRunLongLineFailsLoudly(t *testing.T) {
	// A single output line over the 1MB scanner token limit must fail,
	// not silently truncate the stream into a partial result.
	cli := fakeCLI(t, `head -c 1100000 /dev/zero | tr '\0' 'y'; echo`)
	t.Setenv("SB_CODEX_BIN", cli)
	if _, err := CodexRun(cliEnv(), func(string) {}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected scanner-too-long error, got %v", err)
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
	if !strings.Contains(res.Summary, "Fix bug") {
		t.Fatalf("unexpected echo result: %+v", res)
	}
	// Echo performs no work, so it must not claim file refs as artifacts.
	if len(res.Artifacts) != 0 {
		t.Fatalf("echo must not claim artifacts, got %v", res.Artifacts)
	}
}

func TestParseAgentLine(t *testing.T) {
	p := parseAgentLine([]byte(`{"type":"result","result":"done!"}`))
	if !p.isResult || p.resultText != "done!" || p.progress != "" {
		t.Fatalf("result parse failed: %+v", p)
	}
	p = parseAgentLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`))
	if p.isResult || p.progress != "hi" {
		t.Fatalf("assistant parse failed: %+v", p)
	}
	p = parseAgentLine([]byte(`not json`))
	if p.progress != "" || p.isResult || p.isError {
		t.Fatalf("non-JSON should yield nothing: %+v", p)
	}
	p = parseAgentLine([]byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"codex answer"}}`))
	if !p.isResult || p.resultText != "codex answer" {
		t.Fatalf("codex agent_message parse failed: %+v", p)
	}
	p = parseAgentLine([]byte(`{"type":"turn.failed","error":{"message":"sandbox denied"}}`))
	if !p.isError || p.errorText != "sandbox denied" {
		t.Fatalf("codex turn.failed parse failed: %+v", p)
	}
	p = parseAgentLine([]byte(`{"type":"thread.started","thread_id":"t1"}`))
	if p.progress != "" || p.isResult || p.isError {
		t.Fatalf("thread.started should be ignored: %+v", p)
	}
}

func TestAgentRunNULByteErrorDoesNotBlameMissingBinary(t *testing.T) {
	// F16: a NUL byte in the prompt makes exec fail, but the old message
	// blamed a missing CLI ("is the agent CLI installed?") — a
	// misdiagnosis. Only a genuinely missing binary earns that hint.
	cli := fakeCLI(t, `echo hi`)
	t.Setenv("SB_CLAUDE_BIN", cli)
	env := cliEnv()
	env.Task.Instructions = "bad\x00instructions"
	_, err := ClaudeRun(env, func(string) {})
	if err == nil {
		t.Fatal("expected error for NUL byte in prompt")
	}
	if strings.Contains(err.Error(), "is the agent CLI installed?") {
		t.Fatalf("NUL-byte error must not blame a missing CLI: %v", err)
	}
}

func TestAgentRunMissingBinaryKeepsInstallHint(t *testing.T) {
	// Positive control for F16: a genuinely missing binary still gets
	// the install hint.
	t.Setenv("SB_CLAUDE_BIN", "/nonexistent/agent-cli-binary")
	_, err := ClaudeRun(cliEnv(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "is the agent CLI installed?") {
		t.Fatalf("missing binary should keep install hint, got: %v", err)
	}
}
