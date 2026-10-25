package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
