// Package adapter defines the adapter stdio protocol and the daemon-side
// runner that supervises adapter processes.
//
// Protocol (versioned NDJSON over stdio):
//  1. Adapter starts and writes one hello line:
//     {"type":"hello","protocol":"sb/1","agent":"claude","capabilities":["code"]}
//  2. Daemon sends one run line: {"type":"run","envelope":{...}}
//  3. Adapter streams progress lines: {"type":"progress","text":"..."}
//  4. Adapter finishes with exactly one of:
//     {"type":"result","summary":"...","artifacts":[...],"diff":"..."}
//     {"type":"error","message":"..."}
//  5. Adapter exits. A non-zero exit without a result line is a failure.
//
// The protocol version is checked on hello; a mismatch fails loudly and the
// envelope is dead-lettered without a retry, never silently run against the
// wrong contract.
// Unparseable stdout lines are treated as progress text (graceful degradation
// to plain text), and an adapter that exits 0 with text but no result line
// yields a degraded result whose summary is that text.
package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// maxAccumulatedBytes caps text the runner accumulates from one adapter run
// (progress text and unparseable lines). Past the cap, further text is
// dropped with a truncation marker instead of growing memory without bound.
const maxAccumulatedBytes = 1 << 20

// ErrProtocolMismatch marks a hello whose protocol version is not sb/1.
// The daemon matches it with errors.Is to skip the retry: a version mismatch
// is deterministic, retrying against the same adapter cannot succeed.
var ErrProtocolMismatch = errors.New("adapter: protocol mismatch")

// appendCapped appends s to b until maxAccumulatedBytes, then appends a
// truncation marker once and drops the rest.
func appendCapped(b *strings.Builder, truncated *bool, s string) {
	if b.Len() >= maxAccumulatedBytes {
		return
	}
	b.WriteString(s)
	if b.Len() >= maxAccumulatedBytes && !*truncated {
		b.WriteString("\n…[truncated: output exceeded 1MB]\n")
		*truncated = true
	}
}

// ProtocolVersion is the adapter protocol version. It matches the envelope
// version in v1 by design; a future protocol bump is a loud break, not a
// silent drift.
const ProtocolVersion = "sb/1"

// Message types on the adapter wire.
const (
	MsgHello    = "hello"
	MsgRun      = "run"
	MsgProgress = "progress"
	MsgResult   = "result"
	MsgError    = "error"
)

// Hello is the adapter's first stdout line.
type Hello struct {
	Type         string   `json:"type"`
	Protocol     string   `json:"protocol"`
	Agent        string   `json:"agent"`
	Capabilities []string `json:"capabilities"`
}

// RunRequest is the daemon's single instruction to the adapter.
type RunRequest struct {
	Type     string             `json:"type"`
	Envelope *envelope.Envelope `json:"envelope"`
}

// Progress is a streaming status line from the adapter.
type Progress struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResultMsg is the adapter's successful completion.
type ResultMsg struct {
	Type      string   `json:"type"`
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts,omitempty"`
	Diff      string   `json:"diff,omitempty"`
}

// ErrorMsg is the adapter's explicit failure.
type ErrorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Result is what Runner.Run returns.
type Result struct {
	Summary   string
	Artifacts []string
	Diff      string
	// Degraded is true when the adapter produced no structured result and the
	// summary fell back to raw text output.
	Degraded bool
}

// Runner supervises one adapter process for one envelope.
type Runner struct {
	// Command is the adapter argv (argv[0] resolved via PATH).
	Command []string
	// Timeout caps the whole run; zero means no timeout (daemon sets one).
	Timeout time.Duration
	// OnProgress receives progress text lines (for logging).
	OnProgress func(text string)
}

// Run executes the adapter for env and returns its result. The adapter process
// is always reaped; a crash, hang, or protocol violation returns an error and
// never takes down the caller — the daemon converts that into a retry or a
// dead letter.
func (r *Runner) Run(ctx context.Context, env *envelope.Envelope) (*Result, error) {
	if len(r.Command) == 0 {
		return nil, fmt.Errorf("adapter: empty command")
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, r.Command[0], r.Command[1:]...)
	// Own process group so a timeout kills the adapter's children too
	// (agent CLIs spawn subprocesses; killing only the parent would orphan them).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("adapter: start %v: %w", r.Command, err)
	}
	// Kill the whole process group on context expiry (CommandContext only
	// kills the direct child). The killDone guard stops the goroutine from
	// firing after a normal completion, when the pid may already be reaped.
	killDone := make(chan struct{})
	defer close(killDone)
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-killDone:
		}
	}()
	// Drain stderr to progress so agent CLI diagnostics land in the run log.
	// A scanner error (e.g. a >1MB stderr line) is reported, not swallowed.
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			r.progress("stderr: " + sc.Text())
		}
		if err := sc.Err(); err != nil {
			r.progress("stderr: scanner error: " + err.Error())
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	// 1. Expect hello.
	if !sc.Scan() {
		_ = cmd.Wait()
		return nil, fmt.Errorf("adapter: no hello line (exited before handshake)")
	}
	var hello Hello
	if err := json.Unmarshal(sc.Bytes(), &hello); err != nil || hello.Type != MsgHello {
		_ = cmd.Wait()
		return nil, fmt.Errorf("adapter: bad hello line: %s", strings.TrimSpace(sc.Text()))
	}
	// Capabilities on hello are informational in v1: routing uses the
	// daemon's config.yaml, not what the adapter declares here.
	if hello.Protocol != ProtocolVersion {
		_ = cmd.Wait()
		return nil, fmt.Errorf("%w: adapter speaks %q, daemon expects %q (failing loudly, not running)",
			ErrProtocolMismatch, hello.Protocol, ProtocolVersion)
	}

	// 2. Send the run request, then close stdin (adapters read one line).
	req, _ := json.Marshal(RunRequest{Type: MsgRun, Envelope: env})
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("adapter: write run request: %w", err)
	}
	stdin.Close()

	// 3. Consume the stream until result, error, or EOF.
	var result *ResultMsg
	var textBuf strings.Builder
	textTruncated := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var wire struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &wire); err != nil {
			// Graceful degradation: unparseable line becomes progress text.
			appendCapped(&textBuf, &textTruncated, string(line)+"\n")
			r.progress(string(line))
			continue
		}
		switch wire.Type {
		case MsgProgress:
			var p Progress
			if err := json.Unmarshal(line, &p); err == nil {
				r.progress(p.Text)
				appendCapped(&textBuf, &textTruncated, p.Text+"\n")
			}
		case MsgResult:
			var res ResultMsg
			if err := json.Unmarshal(line, &res); err != nil {
				_ = cmd.Wait()
				return nil, fmt.Errorf("adapter: malformed result line: %w", err)
			}
			result = &res
		case MsgError:
			var em ErrorMsg
			if err := json.Unmarshal(line, &em); err == nil {
				_ = cmd.Wait()
				return nil, fmt.Errorf("adapter: %s", em.Message)
			}
			_ = cmd.Wait()
			return nil, fmt.Errorf("adapter: error (unparseable)")
		default:
			// Unknown message type: log as text, keep going.
			r.progress(string(line))
			appendCapped(&textBuf, &textTruncated, string(line)+"\n")
		}
	}
	// A scanner error (e.g. a line over the 1MB token limit) means the
	// stream is incomplete: fail loudly instead of returning a partial
	// result as if the run had succeeded.
	if err := sc.Err(); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("adapter: reading output: %w", err)
	}
	waitErr := cmd.Wait()
	if result != nil {
		return &Result{Summary: result.Summary, Artifacts: result.Artifacts, Diff: result.Diff}, nil
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("adapter: timed out after %s", r.Timeout)
	}
	if waitErr != nil {
		// Non-zero exit without a result: failure, but surface any text.
		if t := strings.TrimSpace(textBuf.String()); t != "" {
			return nil, fmt.Errorf("adapter: exited with error: %v (output: %s)", waitErr, truncate(t, 500))
		}
		return nil, fmt.Errorf("adapter: exited with error: %v", waitErr)
	}
	// Clean exit, no result line: degraded result from accumulated text.
	if t := strings.TrimSpace(textBuf.String()); t != "" {
		return &Result{Summary: t, Degraded: true}, nil
	}
	return nil, fmt.Errorf("adapter: exited cleanly without a result")
}

func (r *Runner) progress(text string) {
	if r.OnProgress != nil {
		r.OnProgress(text)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
