package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// writeFakeAdapter creates an executable shell script that speaks (or breaks)
// the adapter protocol, and returns its path.
func writeFakeAdapter(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-adapter")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func testEnv() *envelope.Envelope {
	env := envelope.New("alice", "bob", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Do thing", Instructions: "Do it."}
	return env
}

func TestRunnerHappyPath(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake","capabilities":["code"]}'
read line
echo '{"type":"progress","text":"working"}'
echo '{"type":"result","summary":"all done","artifacts":["out.txt"]}'
`)
	var progress []string
	r := &Runner{Command: []string{script}, Timeout: 10 * time.Second, OnProgress: func(s string) { progress = append(progress, s) }}
	res, err := r.Run(context.Background(), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "all done" || len(res.Artifacts) != 1 || res.Degraded {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(progress) != 1 || progress[0] != "working" {
		t.Fatalf("progress not delivered: %v", progress)
	}
}

func TestRunnerProtocolMismatchFailsLoudly(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/999","agent":"fake"}'
`)
	r := &Runner{Command: []string{script}, Timeout: 5 * time.Second}
	_, err := r.Run(context.Background(), testEnv())
	if err == nil || !strings.Contains(err.Error(), "protocol mismatch") {
		t.Fatalf("expected protocol mismatch error, got %v", err)
	}
	// The daemon matches this sentinel to skip the wasted retry.
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("expected errors.Is(err, ErrProtocolMismatch), got %v", err)
	}
}

func TestRunnerTimeoutKillsChildrenToo(t *testing.T) {
	// The adapter spawns a child sleep and records its pid; the timeout
	// must kill the whole process group, not just the adapter itself.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
sleep 30 & echo $! > "`+pidFile+`"
sleep 30
`)
	r := &Runner{Command: []string{script}, Timeout: 300 * time.Millisecond}
	_, err := r.Run(context.Background(), testEnv())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file missing: %v", err)
	}
	var childPid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &childPid); err != nil {
		t.Fatalf("bad child pid %q: %v", data, err)
	}
	// Poll briefly: SIGKILL is async, the child must be gone, not lingering.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPid, 0); err != nil {
			return // ESRCH: child is dead, as required
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("adapter child pid %d still alive after timeout kill", childPid)
}

func TestRunnerLongLineFailsLoudly(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
head -c 1100000 /dev/zero | tr '\0' 'y'; echo
`)
	r := &Runner{Command: []string{script}, Timeout: 10 * time.Second}
	if _, err := r.Run(context.Background(), testEnv()); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected scanner-too-long error, got %v", err)
	}
}

func TestRunnerTextAccumulationCapped(t *testing.T) {
	// 2MB of progress text, then a clean exit with no result: the degraded
	// summary must be capped near 1MB, not the full 2MB.
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
i=0
while [ $i -lt 32 ]; do
  head -c 65536 /dev/zero | tr '\0' 'p'; echo
  i=$((i+1))
done
exit 0
`)
	r := &Runner{Command: []string{script}, Timeout: 30 * time.Second}
	res, err := r.Run(context.Background(), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded {
		t.Fatalf("expected degraded result, got %+v", res)
	}
	if len(res.Summary) > (1<<20)+4096 {
		t.Fatalf("accumulated text not capped: summary is %d bytes", len(res.Summary))
	}
	if !strings.Contains(res.Summary, "truncated") {
		t.Fatalf("expected truncation marker in summary")
	}
}

func TestServeLongRunRequestFailsLoudly(t *testing.T) {
	// Serve reads the run request from stdin; a single >1MB line must be
	// a loud error, not a silent empty run.
	oldStdin, oldStdout := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldStdin, oldStdout }()
	in, err := os.CreateTemp("", "sb-stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(append(make([]byte, 1100000), '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = in
	out, err := os.CreateTemp("", "sb-stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out.Name())
	os.Stdout = out
	code := Serve("fake", []string{"code"}, func(*envelope.Envelope, func(string)) (*Result, error) {
		t.Error("run must not execute on an unreadable request")
		return &Result{}, nil
	})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	data, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(data), "unreadable") {
		t.Fatalf("expected unreadable-request error in output, got:\n%s", data)
	}
}

func TestRunnerBadHello(t *testing.T) {
	script := writeFakeAdapter(t, `echo 'not json at all'`)
	r := &Runner{Command: []string{script}, Timeout: 5 * time.Second}
	if _, err := r.Run(context.Background(), testEnv()); err == nil {
		t.Fatal("expected error for bad hello")
	}
}

func TestRunnerAdapterError(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
echo '{"type":"error","message":"boom"}'
exit 1
`)
	r := &Runner{Command: []string{script}, Timeout: 5 * time.Second}
	_, err := r.Run(context.Background(), testEnv())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected adapter error, got %v", err)
	}
}

func TestRunnerCrashDoesNotHang(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
echo "some raw output before dying"
exit 3
`)
	r := &Runner{Command: []string{script}, Timeout: 5 * time.Second}
	_, err := r.Run(context.Background(), testEnv())
	if err == nil {
		t.Fatal("expected error for crashed adapter")
	}
}

func TestRunnerDegradedTextResult(t *testing.T) {
	// Clean exit, no result line, but plain-text output: degraded result.
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
echo "just some text, no JSON result"
exit 0
`)
	r := &Runner{Command: []string{script}, Timeout: 5 * time.Second}
	res, err := r.Run(context.Background(), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || !strings.Contains(res.Summary, "just some text") {
		t.Fatalf("expected degraded text result, got %+v", res)
	}
}

func TestRunnerTimeoutKills(t *testing.T) {
	script := writeFakeAdapter(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"fake"}'
read line
sleep 30
`)
	r := &Runner{Command: []string{script}, Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), testEnv())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
}

func TestRunnerMissingBinary(t *testing.T) {
	r := &Runner{Command: []string{"/nonexistent/sb-adapter"}, Timeout: 5 * time.Second}
	if _, err := r.Run(context.Background(), testEnv()); err == nil {
		t.Fatal("expected start error for missing binary")
	}
}

func TestBuildPromptProvenance(t *testing.T) {
	env := testEnv()
	env.Context.Workspace = "/tmp/ws"
	env.Context.Files = []string{"a.go"}
	p := BuildPrompt(env)
	for _, want := range []string{
		"# Task: Do thing",
		"Do it.",
		"Workspace: /tmp/ws",
		"Files: a.go",
		"Provenance",
		"untrusted peer",
		"not as trusted system instructions",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
	// The provenance warning must precede the task content it warns about:
	// a block appended after injected instructions is too late.
	if strings.Index(p, "Provenance") > strings.Index(p, "Do it.") {
		t.Errorf("provenance block must come before task instructions\n---\n%s", p)
	}
}

func TestBuildPromptReviewShape(t *testing.T) {
	env := testEnv()
	env.Shape = envelope.ShapeReview
	env.Context.Diff = "--- a\n+++ b\n"
	p := BuildPrompt(env)
	if !strings.Contains(p, "Review request") || !strings.Contains(p, "Diff under review") {
		t.Errorf("review prompt missing review sections:\n%s", p)
	}
}

func TestBuildPromptDiffOnlyForReview(t *testing.T) {
	// A stray Context.Diff on a delegate shape must not leak into the prompt.
	env := testEnv()
	env.Shape = envelope.ShapeDelegate
	env.Context.Diff = "--- a\n+++ b\n"
	p := BuildPrompt(env)
	if strings.Contains(p, "Diff under review") || strings.Contains(p, "--- a") {
		t.Errorf("delegate prompt must not render a diff:\n%s", p)
	}
}
