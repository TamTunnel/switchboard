package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TamTunnel/switchboard/internal/client"
	"github.com/TamTunnel/switchboard/internal/config"
	"github.com/TamTunnel/switchboard/internal/envelope"
	"github.com/TamTunnel/switchboard/internal/store"
)

var testBinDir string

// TestMain builds the real sb-adapter (and sb) binaries once for integration tests.
func TestMain(m *testing.M) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "cannot locate test file")
		os.Exit(1)
	}
	root := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			fmt.Fprintln(os.Stderr, "go.mod not found")
			os.Exit(1)
		}
		root = parent
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			fmt.Fprintln(os.Stderr, "go toolchain not found")
			os.Exit(1)
		}
		goBin = filepath.Join(home, "go", "bin", "go")
	}
	dir, err := os.MkdirTemp("", "sb-testbin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	for _, bin := range []string{"sb-adapter", "sb"} {
		cmd := exec.Command(goBin, "build", "-o", filepath.Join(dir, bin), "./cmd/"+bin)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", bin, err, out)
			os.Exit(1)
		}
	}
	testBinDir = dir
	os.Exit(m.Run())
}

func echoAgent() config.Agent {
	return config.Agent{Command: []string{filepath.Join(testBinDir, "sb-adapter"), "echo"}, Capabilities: []string{"echo", "code", "review"}}
}

// startDaemon runs an in-process daemon rooted at a temp home and returns a client.
func startDaemon(t *testing.T, agents map[string]config.Agent) (*client.Client, *store.Store, context.CancelFunc) {
	return startDaemonHome(t, t.TempDir(), agents, 50)
}

// startDaemonHome runs an in-process daemon rooted at home with the given
// poll interval, and returns a client.
func startDaemonHome(t *testing.T, home string, agents map[string]config.Agent, pollMs int) (*client.Client, *store.Store, context.CancelFunc) {
	t.Helper()
	cfg := config.Config{Agents: agents, PollIntervalMs: pollMs, DefaultTimeoutS: 600}
	d, err := New(home, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()
	// Wait for the socket.
	sock := SocketPath(home)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	return client.New(sock), st, cancel
}

func errorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeAdapterScript writes an executable shell script speaking (or breaking)
// the adapter protocol, and returns its path.
func fakeAdapterScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-adapter")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// countCopies counts how many status directories hold a file for id.
func countCopies(t *testing.T, st *store.Store, id string) int {
	t.Helper()
	n := 0
	for _, dir := range []string{store.DirPending, store.DirClaimed, store.DirDone, store.DirDead} {
		if _, err := st.Read(dir, id); err == nil {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sendTask(t *testing.T, c *client.Client, from, to, shape string, next []string, timeoutS int) string {
	t.Helper()
	env := envelope.New(from, to, shape)
	env.Task = envelope.Task{Title: "Test task", Instructions: "Do the test thing."}
	env.Task.Constraints.TimeoutSeconds = timeoutS
	env.Next = next
	if err := env.Validate(); err != nil {
		t.Fatal(err)
	}
	id, err := c.Send(env)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return id
}

func TestDelegateEchoEndToEnd(t *testing.T) {
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "original done", func() bool {
		env, _, err := st.ReadAny(id)
		return err == nil && env.Status == envelope.StatusDone && env.Result != nil
	})
	waitFor(t, 10*time.Second, "result in alice inbox", func() bool {
		inbox, err := c.Inbox("alice")
		if err != nil {
			return false
		}
		for _, e := range inbox {
			if e.Kind == envelope.KindResult && e.ReplyTo == id {
				return strings.Contains(e.Result.Summary, "echo:")
			}
		}
		return false
	})
}

func TestPipelineTwoStages(t *testing.T) {
	agents := map[string]config.Agent{"echo": echoAgent(), "echo2": echoAgent()}
	c, st, cancel := startDaemon(t, agents)
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapePipeline, []string{"echo2"}, 0)
	waitFor(t, 15*time.Second, "final result in alice inbox", func() bool {
		inbox, err := c.Inbox("alice")
		if err != nil {
			return false
		}
		for _, e := range inbox {
			if e.Kind == envelope.KindResult && e.To == "alice" {
				return true
			}
		}
		return false
	})
	// The intermediate stage ran: some done envelope addressed to echo2 exists.
	done, _ := st.List(store.DirDone)
	found := false
	for _, e := range done {
		if e.To == "echo2" && e.Kind == envelope.KindTask {
			found = true
		}
	}
	if !found {
		t.Fatal("expected intermediate pipeline stage to echo2 in done/")
	}
	orig, _, err := st.ReadAny(id)
	if err != nil || orig.Status != envelope.StatusDone {
		t.Fatalf("original should be done, got %+v err %v", orig, err)
	}
}

func TestReviewShape(t *testing.T) {
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeReview)
	env.Task = envelope.Task{Title: "Review this", Instructions: "Review please."}
	env.Context.Diff = "--- a\n+++ b\n"
	id, err := c.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "review result", func() bool {
		inbox, err := c.Inbox("alice")
		if err != nil {
			return false
		}
		for _, e := range inbox {
			if e.Kind == envelope.KindResult && e.ReplyTo == id {
				return true
			}
		}
		return false
	})
}

func TestFailingAdapterRetriesOnceThenDead(t *testing.T) {
	t.Setenv("SB_ECHO_FAIL", "1")
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 15*time.Second, "dead-letter", func() bool {
		env, _, err := st.ReadAny(id)
		return err == nil && env.Status == envelope.StatusDead
	})
	env, _, _ := st.ReadAny(id)
	if env.Attempts != 2 {
		t.Fatalf("expected exactly 2 attempts (one retry), got %d", env.Attempts)
	}
	if env.Error == "" {
		t.Fatal("dead letter should carry the error")
	}
}

func TestUnknownAgentDeadImmediately(t *testing.T) {
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "nosuchagent", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "dead-letter", func() bool {
		env, _, err := st.ReadAny(id)
		return err == nil && env.Status == envelope.StatusDead
	})
	env, _, _ := st.ReadAny(id)
	if env.Attempts != 1 {
		t.Fatalf("unroutable should not retry, attempts = %d", env.Attempts)
	}
}

func TestCapabilityRouting(t *testing.T) {
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "cap:echo", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "done via cap routing", func() bool {
		env, _, err := st.ReadAny(id)
		return err == nil && env.Status == envelope.StatusDone
	})
}

func TestSendValidationRejected(t *testing.T) {
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "", Instructions: ""} // invalid
	if _, err := c.Send(env); err == nil {
		t.Fatal("expected daemon to reject invalid envelope")
	}
}

func TestHangAdapterTimesOutThenDead(t *testing.T) {
	t.Setenv("SB_ECHO_HANG", "1")
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapeDelegate, nil, 1) // 1s timeout
	waitFor(t, 20*time.Second, "dead-letter after timeouts", func() bool {
		env, _, err := st.ReadAny(id)
		return err == nil && env.Status == envelope.StatusDead
	})
}

func TestCLIEndToEnd(t *testing.T) {
	// Exercise the real `sb` binary against an in-process daemon socket.
	home := t.TempDir()
	cfg := config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}
	d, err := New(home, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(SocketPath(home)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sb := filepath.Join(testBinDir, "sb")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(sb, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sb %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	id := strings.TrimSpace(run("--home", home, "send", "--to", "echo", "--from", "alice",
		"--title", "CLI task", "--instructions", "Via the CLI."))
	if id == "" {
		t.Fatal("empty id from sb send")
	}
	waitFor(t, 10*time.Second, "CLI inbox result", func() bool {
		out := run("--home", home, "inbox", "--agent", "alice")
		return strings.Contains(out, "result")
	})
	status := run("--home", home, "status", id)
	if !strings.Contains(status, `"status": "done"`) {
		t.Fatalf("expected done status, got:\n%s", status)
	}
	logs := run("--home", home, "logs", id)
	if !strings.Contains(logs, "echo:") {
		t.Fatalf("expected echo progress in logs, got:\n%s", logs)
	}
}

func TestFailRetryLeavesNoStaleClaimedCopy(t *testing.T) {
	// M1 regression, store level: failing an envelope must move it out of
	// claimed/ — not leave a stale claimed/ copy beside the pending retry.
	home := t.TempDir()
	cfg := config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}
	d, err := New(home, cfg, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Status = envelope.StatusRunning
	env.Attempts = 1
	if err := st.Write(env); err != nil { // lands in claimed/
		t.Fatal(err)
	}
	d.fail(env, "boom", true)
	if _, err := st.Read(store.DirClaimed, env.ID); err == nil {
		t.Fatal("stale claimed/ copy remains after retry requeue")
	}
	got, err := st.Read(store.DirPending, env.ID)
	if err != nil {
		t.Fatalf("pending retry missing: %v", err)
	}
	if got.Attempts != 1 || got.Error != "boom" {
		t.Fatalf("retry lost state: %+v", got)
	}
	if n := countCopies(t, st, env.ID); n != 1 {
		t.Fatalf("envelope exists in %d directories, want exactly 1", n)
	}
}

func TestRestartDispatchesRetriedEnvelope(t *testing.T) {
	// M1 regression, restart level: fail once (real fail path), restart
	// the daemon, and the retry must still be dispatched — not dropped
	// because a stale claimed/ copy won over it at startup.
	t.Setenv("SB_ECHO_FAIL", "1")
	home := t.TempDir()
	cfg := config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}
	d, err := New(home, cfg, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Status = envelope.StatusRunning
	env.Attempts = 1
	if err := st.Write(env); err != nil {
		t.Fatal(err)
	}
	d.fail(env, "boom", true) // first failure -> retry in pending/
	if _, err := st.Read(store.DirClaimed, env.ID); err == nil {
		t.Fatal("stale claimed/ copy before restart")
	}
	// "Restart": a fresh daemon on the same home.
	_, st2, cancel := startDaemonHome(t, home, map[string]config.Agent{"echo": echoAgent()}, 50)
	defer cancel()
	waitFor(t, 15*time.Second, "retry dispatched to dead-letter", func() bool {
		e, _, err := st2.ReadAny(env.ID)
		return err == nil && e.Status == envelope.StatusDead
	})
	e, _, _ := st2.ReadAny(env.ID)
	if e.Attempts != 2 {
		t.Fatalf("expected attempts 2 after restart dispatch, got %d", e.Attempts)
	}
	if n := countCopies(t, st2, env.ID); n != 1 {
		t.Fatalf("envelope exists in %d directories, want exactly 1", n)
	}
}

func TestRestartRequeuesClaimed(t *testing.T) {
	// M2: an envelope stranded in claimed/ (previous daemon died
	// mid-dispatch) is requeued on startup with attempts preserved, then
	// dispatched — not left stranded forever.
	home := t.TempDir()
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Status = envelope.StatusRunning
	env.Attempts = 1
	if err := st.Write(env); err != nil { // lands in claimed/
		t.Fatal(err)
	}
	_, st2, cancel := startDaemonHome(t, home, map[string]config.Agent{"echo": echoAgent()}, 50)
	defer cancel()
	waitFor(t, 10*time.Second, "stranded envelope dispatched to done", func() bool {
		e, _, err := st2.ReadAny(env.ID)
		return err == nil && e.Status == envelope.StatusDone
	})
	e, _, _ := st2.ReadAny(env.ID)
	if e.Attempts != 2 {
		t.Fatalf("expected attempts preserved (1) then incremented to 2, got %d", e.Attempts)
	}
}

func TestShutdownWaitsForInflightAndRequeues(t *testing.T) {
	// M3: cancelling the daemon while a dispatch runs must wait for the
	// dispatch (bounded), kill the adapter process group, and leave the
	// envelope requeued — not orphaned, not dropped.
	pidFile := filepath.Join(t.TempDir(), "adapter.pid")
	script := fakeAdapterScript(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"slow"}'
read line
echo $$ > "`+pidFile+`"
sleep 30
`)
	home := t.TempDir()
	cfg := config.Config{
		Agents:          map[string]config.Agent{"slow": {Command: []string{script}, Capabilities: []string{"code"}}},
		PollIntervalMs:  50,
		DefaultTimeoutS: 600,
	}
	d, err := New(home, cfg, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	sock := SocketPath(home)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c := client.New(sock)
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	id := sendTask(t, c, "alice", "slow", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "dispatch started", func() bool {
		e, _, err := st.ReadAny(id)
		if err != nil || e.Status != envelope.StatusRunning {
			return false
		}
		_, err = os.Stat(pidFile)
		return err == nil
	})
	cancel()
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("daemon Run did not return after shutdown (in-flight dispatch orphaned?)")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("adapter pid file missing: %v", err)
	}
	var adapterPid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &adapterPid); err != nil {
		t.Fatalf("bad adapter pid %q: %v", data, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(adapterPid, 0); err != nil {
			break // ESRCH: adapter is dead
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(adapterPid, 0); err == nil {
		t.Fatalf("adapter pid %d still alive after daemon shutdown", adapterPid)
	}
	e, _, err := st.ReadAny(id)
	if err != nil {
		t.Fatalf("envelope missing after shutdown: %v", err)
	}
	if e.Status != envelope.StatusPending || e.Attempts != 1 {
		t.Fatalf("expected envelope requeued in pending with attempts 1, got status %q attempts %d", e.Status, e.Attempts)
	}
}

func TestProtocolMismatchDeadImmediately(t *testing.T) {
	// m2: a protocol mismatch is deterministic — it must dead-letter
	// immediately, not burn the one retry on a rerun that cannot succeed.
	script := fakeAdapterScript(t, `
echo '{"type":"hello","protocol":"sb/999","agent":"fake"}'
`)
	agents := map[string]config.Agent{"fake": {Command: []string{script}, Capabilities: []string{"code"}}}
	c, _, cancel := startDaemon(t, agents)
	defer cancel()
	id := sendTask(t, c, "alice", "fake", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "dead-letter without retry", func() bool {
		env, err := c.Status(id)
		return err == nil && env.Status == envelope.StatusDead
	})
	env, err := c.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if env.Attempts != 1 {
		t.Fatalf("protocol mismatch should not retry, attempts = %d", env.Attempts)
	}
}

func TestSendResultKindRejected(t *testing.T) {
	// m6: a client-sent result envelope must be rejected, not stranded in
	// pending/ where poll() would skip it forever.
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Kind = envelope.KindResult
	if _, err := c.Send(env); err == nil {
		t.Fatal("expected daemon to reject a client-sent result envelope")
	}
	if _, _, err := st.ReadAny(env.ID); err == nil {
		t.Fatal("rejected result envelope must not be stored")
	}
}

func TestSendOversizedEnvelopeRejected(t *testing.T) {
	// m3: /v1/send is bounded at 10MB; an 11MB envelope must fail.
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Big", Instructions: strings.Repeat("x", 11<<20)}
	if _, err := c.Send(env); err == nil {
		t.Fatal("expected oversized envelope to be rejected")
	}
}

func TestCLIFlagShapeValidation(t *testing.T) {
	// m5: --next on non-pipeline shapes and --diff-file on non-review
	// shapes are rejected by the CLI instead of silently accepted.
	sb := filepath.Join(testBinDir, "sb")
	home := t.TempDir()
	runExpectErr := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(sb, args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("sb %v: expected error, got:\n%s", args, out)
		}
		return string(out)
	}
	base := []string{"--home", home, "send", "--to", "echo", "--title", "T", "--instructions", "I"}
	out := runExpectErr(append(base, "--shape", "delegate", "--next", "echo")...)
	if !strings.Contains(out, "--next only applies") {
		t.Fatalf("expected --next rejection, got:\n%s", out)
	}
	diffFile := filepath.Join(t.TempDir(), "changes.diff")
	if err := os.WriteFile(diffFile, []byte("--- a\n+++ b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = runExpectErr(append(base, "--shape", "delegate", "--diff-file", diffFile)...)
	if !strings.Contains(out, "--diff-file only applies") {
		t.Fatalf("expected --diff-file rejection, got:\n%s", out)
	}
	// Positive control: pipeline + --next passes validation and fails
	// only at the daemon connection (no daemon is running on this home).
	out = runExpectErr(append(base, "--shape", "pipeline", "--next", "echo")...)
	if !strings.Contains(out, "daemon not reachable") {
		t.Fatalf("pipeline+--next should pass validation, got:\n%s", out)
	}
}

func TestCLIDefaultAgentFromConfig(t *testing.T) {
	// m7: config.yaml's default_agent is the sender when --from is not
	// passed and $SWITCHBOARD_AGENT is unset.
	t.Setenv("SWITCHBOARD_AGENT", "")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("default_agent: bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}
	d, err := New(home, cfg, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()
	sock := SocketPath(home)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sb := filepath.Join(testBinDir, "sb")
	cmd := exec.Command(sb, "--home", home, "send", "--to", "echo", "--title", "T", "--instructions", "I")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sb send: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	c := client.New(sock)
	env, err := c.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if env.From != "bot" {
		t.Fatalf("expected sender from config default_agent 'bot', got %q", env.From)
	}
}
