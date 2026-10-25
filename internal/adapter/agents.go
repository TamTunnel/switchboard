package adapter

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// agentRun executes an agent CLI with the given argv, streaming its output
// through parseAgentLine. Structured parsing failures degrade to raw text:
// the returned summary is the terminal result when present, otherwise the
// accumulated output. A non-zero exit with no usable output is an error.
// A parsed terminal error (e.g. codex turn.failed) is an error even when
// the CLI exits 0 — a failed turn must never look like a degraded success.
func agentRun(argv []string, workdir string, progress func(string)) (*Result, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run %s: %w (is the agent CLI installed?)", argv[0], err)
	}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			progress("stderr: " + sc.Text())
		}
		if err := sc.Err(); err != nil {
			progress("stderr: scanner error: " + err.Error())
		}
	}()
	var raw bytes.Buffer
	rawTruncated := false
	var resultText string
	var haveResult bool
	var errText string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if raw.Len() < maxAccumulatedBytes {
			raw.Write(line)
			raw.WriteByte('\n')
			if raw.Len() >= maxAccumulatedBytes && !rawTruncated {
				raw.WriteString("…[truncated: output exceeded 1MB]\n")
				rawTruncated = true
			}
		}
		parsed := parseAgentLine(line)
		switch {
		case parsed.isError:
			errText = parsed.errorText
		case parsed.isResult:
			haveResult = true
			resultText = parsed.resultText
		case parsed.progress != "":
			progress(parsed.progress)
		}
	}
	// A scanner error (e.g. a line over the 1MB token limit) means the
	// stream is incomplete: fail loudly instead of trusting a partial read.
	if err := sc.Err(); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("%s: reading output: %w", argv[0], err)
	}
	waitErr := cmd.Wait()
	if errText != "" {
		return nil, fmt.Errorf("%s failed: %s", argv[0], errText)
	}
	if haveResult {
		if resultText == "" {
			resultText = strings.TrimSpace(raw.String())
		}
		return &Result{Summary: resultText}, nil
	}
	if waitErr != nil {
		if t := strings.TrimSpace(raw.String()); t != "" {
			return nil, fmt.Errorf("%s exited with error: %v (output: %s)", argv[0], waitErr, truncate(t, 500))
		}
		return nil, fmt.Errorf("%s exited with error: %v", argv[0], waitErr)
	}
	// Clean exit, no structured result: degrade to raw text.
	if t := strings.TrimSpace(raw.String()); t != "" {
		return &Result{Summary: t, Degraded: true}, nil
	}
	return nil, fmt.Errorf("%s produced no output", argv[0])
}

// ClaudeRun runs Claude Code headless: `claude -p --output-format stream-json`.
// The prompt is passed as an argument (Claude Code's documented -p usage).
// When the envelope carries a resume session id, --resume is appended.
func ClaudeRun(env *envelope.Envelope, progress func(string)) (*Result, error) {
	prompt := BuildPrompt(env)
	argv := []string{agentBin("SB_CLAUDE_BIN", "claude"), "-p", "--output-format", "stream-json"}
	if env.Context.ResumeSession != "" {
		argv = append(argv, "--resume", env.Context.ResumeSession)
	}
	argv = append(argv, prompt)
	return agentRun(argv, env.Context.Workspace, progress)
}

// CodexRun runs Codex headless: `codex exec --json`. The prompt is passed as
// an argument. Two flags are load-bearing for handoffs, not optional:
//   - --skip-git-repo-check: codex refuses to run outside a git repo by
//     default, so non-repo workspaces would fail outright.
//   - --sandbox workspace-write: the default sandbox is read-only, so an
//     implement-task handoff could never write files. Workspace-write keeps
//     writes inside the workspace; a denial outside it surfaces as a
//     turn.failed error (see parseAgentLine), not a silent success.
//
// Codex's JSON line schema is parsed structurally (thread/item/turn events);
// unparseable output degrades to raw text.
func CodexRun(env *envelope.Envelope, progress func(string)) (*Result, error) {
	prompt := BuildPrompt(env)
	argv := []string{agentBin("SB_CODEX_BIN", "codex"), "exec", "--json",
		"--skip-git-repo-check", "--sandbox", "workspace-write", prompt}
	return agentRun(argv, env.Context.Workspace, progress)
}

// OpencodeRun runs OpenCode: `opencode run`. OpenCode has no pinned stable
// JSON streaming mode in this adapter, so output is treated as plain text
// (provisional — see docs/adapter-protocol.md version matrix).
func OpencodeRun(env *envelope.Envelope, progress func(string)) (*Result, error) {
	prompt := BuildPrompt(env)
	argv := []string{agentBin("SB_OPENCODE_BIN", "opencode"), "run", prompt}
	return agentRun(argv, env.Context.Workspace, progress)
}

// EchoRun is the reference adapter: it performs no agent work, it just
// acknowledges the envelope. It claims no artifacts: echo produces nothing,
// so reporting the envelope's file refs as artifacts would be false.
// Integration tests run the daemon against it.
//
// Test hooks (env vars, never set in production):
//
//	SB_ECHO_FAIL=1  -> report an error instead of a result.
//	SB_ECHO_HANG=1  -> sleep forever (daemon timeout tests).
func EchoRun(env *envelope.Envelope, progress func(string)) (*Result, error) {
	if os.Getenv("SB_ECHO_HANG") == "1" {
		progress("echo: hanging (test hook)")
		time.Sleep(24 * time.Hour)
	}
	if os.Getenv("SB_ECHO_FAIL") == "1" {
		return nil, fmt.Errorf("echo: intentional failure (test hook)")
	}
	progress(fmt.Sprintf("echo: received task %q", env.Task.Title))
	return &Result{
		Summary: fmt.Sprintf("echo: %s — instructions received (%d chars)", env.Task.Title, len(env.Task.Instructions)),
	}, nil
}
