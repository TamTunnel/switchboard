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

	"github.com/TamTunnel/switchboard/internal/adapter"
	"github.com/TamTunnel/switchboard/internal/client"
	"github.com/TamTunnel/switchboard/internal/config"
	"github.com/TamTunnel/switchboard/internal/envelope"
	"github.com/TamTunnel/switchboard/internal/store"
)

var testBinDir string

// TestMain builds the real sb-adapter (with test hooks enabled), sb, and
// switchboard binaries once for integration tests.
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
	for _, bin := range []string{"sb-adapter", "sb", "switchboard"} {
		args := []string{"build", "-o", filepath.Join(dir, bin)}
		if bin == "sb-adapter" {
			// Echo test hooks (SB_ECHO_FAIL/HANG) are tag-gated: test
			// binaries enable them, shipped binaries ignore them.
			args = append(args, "-tags", "switchboardtesthooks")
		}
		args = append(args, "./cmd/"+bin)
		cmd := exec.Command(goBin, args...)
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
	d.fail(env, "boom", true, "echo")
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
	d.fail(env, "boom", true, "echo") // first failure -> retry in pending/
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
	// m3/F5: /v1/send is bounded at 1MB (the adapters cannot execute
	// anything bigger: agent CLIs cap one argv element at 128KB). A 2MB
	// envelope must fail at send, not dead-letter after a futile retry.
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Big", Instructions: strings.Repeat("x", 2<<20)}
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

// --- Adversarial-judge fix regressions (F1–F22) ---

func TestSecondDaemonFailsFast(t *testing.T) {
	// F1/F11: a second daemon on the same home must fail fast instead of
	// unlinking the first daemon's socket and requeueing its in-flight task.
	home := t.TempDir()
	c, _, cancel := startDaemonHome(t, home, map[string]config.Agent{"echo": echoAgent()}, 50)
	defer cancel()
	d2, err := New(home, config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	errCh := make(chan error, 1)
	go func() { errCh <- d2.Run(ctx2) }()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("second daemon should fail fast with already-running, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second daemon did not fail fast")
	}
	// The first daemon still owns its socket.
	if _, err := os.Stat(SocketPath(home)); err != nil {
		t.Fatalf("first daemon socket missing after second daemon attempt: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("first daemon not responsive after second daemon attempt: %v", err)
	}
}

func TestRestartKillsOrphanedAdapter(t *testing.T) {
	// F1: a claimed envelope carrying a recorded adapter pgid must have
	// that group killed on restart before it is requeued — no orphan
	// surviving alongside the retry.
	home := t.TempDir()
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Status = envelope.StatusRunning
	env.Attempts = 1
	env.AdapterPid, env.AdapterPgid = pgid, pgid
	if err := st.Write(env); err != nil { // lands in claimed/
		t.Fatal(err)
	}
	_, st2, cancel := startDaemonHome(t, home, map[string]config.Agent{"echo": echoAgent()}, 50)
	defer cancel()
	select {
	case <-waitCh:
		// Orphan reaped: the restart killed it.
	case <-time.After(10 * time.Second):
		t.Fatal("orphaned adapter group still alive after daemon restart")
	}
	waitFor(t, 10*time.Second, "stranded envelope dispatched to done", func() bool {
		e, _, err := st2.ReadAny(env.ID)
		return err == nil && e.Status == envelope.StatusDone
	})
}

func TestRestartDoesNotDoubleExecute(t *testing.T) {
	// F1 live repro (judge scenario): slow adapter + kill -9 on the real
	// daemon binary mid-dispatch, then restart. The orphan must be dead
	// before the retry runs, and exactly one result notification exists.
	home := t.TempDir()
	runFile := filepath.Join(home, "runs.log")
	blockFile := filepath.Join(home, "block")
	if err := os.WriteFile(blockFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fakeAdapterScript(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"slow"}'
read line
echo "RUN $$" >> "`+runFile+`"
if [ -f "`+blockFile+`" ]; then sleep 30; fi
echo '{"type":"result","summary":"slow done"}'
`)
	cfgYAML := "agents:\n  slow:\n    command: [\"" + script + "\"]\n    capabilities: [\"code\"]\npoll_interval_ms: 50\ndefault_timeout_seconds: 600\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(testBinDir, "switchboard")
	startBinary := func() *exec.Cmd {
		cmd := exec.Command(bin, "daemon", "--home", home)
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	waitSocket := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(SocketPath(home)); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon socket did not appear")
	}
	daemonCmd := startBinary()
	waitSocket()
	c := client.New(SocketPath(home))
	env := envelope.New("alice", "slow", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Slow", Instructions: "Take a while."}
	id, err := c.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the adapter to start (RUN line), i.e. dispatch in flight.
	var orphanPid int
	waitFor(t, 10*time.Second, "adapter RUN", func() bool {
		data, err := os.ReadFile(runFile)
		if err != nil {
			return false
		}
		_, err = fmt.Sscanf(strings.TrimSpace(string(data)), "RUN %d", &orphanPid)
		return err == nil && orphanPid > 0
	})
	if err := daemonCmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = daemonCmd.Wait()
	// The orphan survives the daemon's death (that is the bug's setup).
	if err := syscall.Kill(-orphanPid, 0); err != nil {
		t.Fatalf("orphan adapter group already dead before restart (setup broken): %v", err)
	}
	// Restart: the block is gone, so the retry completes immediately.
	_ = os.Remove(blockFile)
	daemonCmd2 := startBinary()
	defer func() {
		_ = daemonCmd2.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd2.Wait()
	}()
	waitSocket()
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "retry completes", func() bool {
		e, _, err := st.ReadAny(id)
		return err == nil && e.Status == envelope.StatusDone
	})
	// The orphan group must be dead, not lingering beside the retry.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-orphanPid, 0); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-orphanPid, 0); err == nil {
		t.Fatalf("orphan adapter group %d still alive after restart", orphanPid)
	}
	// Exactly one result notification: the task completed once.
	done, _ := st.List(store.DirDone)
	results := 0
	for _, e := range done {
		if e.Kind == envelope.KindResult && e.ReplyTo == id {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("expected exactly 1 result notification, got %d", results)
	}
	data, _ := os.ReadFile(runFile)
	runs := strings.Count(string(data), "RUN ")
	if runs != 2 {
		t.Fatalf("expected 2 RUN lines (orphan + one retry), got %d", runs)
	}
	e, _, _ := st.ReadAny(id)
	if e.Attempts != 2 {
		t.Fatalf("expected attempts 2 (orphan claim + retry), got %d", e.Attempts)
	}
}

func TestEffectiveTimeoutClamp(t *testing.T) {
	// F4: a huge timeout must clamp to 24h, never overflow to negative.
	cases := []struct {
		def, env, want int
	}{
		{600, 0, 600},
		{600, 30, 30},
		{3, 10000000000, 86400},
		{0, 0, 600},
		{600, 86400, 86400},
	}
	for _, tc := range cases {
		env := envelope.New("a", "b", envelope.ShapeDelegate)
		env.Task.Constraints.TimeoutSeconds = tc.env
		got := effectiveTimeout(config.Config{DefaultTimeoutS: tc.def}, env)
		if got != time.Duration(tc.want)*time.Second {
			t.Errorf("default %d env %d: got %v, want %v", tc.def, tc.env, got, time.Duration(tc.want)*time.Second)
		}
	}
}

func TestSendHugeTimeoutRejected(t *testing.T) {
	// F4 at the API: 10^10s is over the 24h schema maximum.
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	env.Task.Constraints.TimeoutSeconds = 10000000000
	if _, err := c.Send(env); err == nil {
		t.Fatal("expected huge timeout to be rejected at send")
	}
}

func TestSendPromptOver128KRejected(t *testing.T) {
	// F5: a 200KB prompt can never run — agent CLIs take the prompt as
	// one argv element capped at 128KB — so send must reject it.
	c, _, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Big", Instructions: strings.Repeat("x", 200<<10)}
	_, err := c.Send(env)
	if err == nil || !strings.Contains(err.Error(), "128KB") {
		t.Fatalf("expected 128KB prompt-limit rejection, got: %v", err)
	}
}

func TestSend100KBPromptCompletes(t *testing.T) {
	// F5 positive control: a 100KB prompt is under the argv ceiling and
	// must run end to end through the echo adapter.
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Big-ish", Instructions: strings.Repeat("x", 100<<10)}
	id, err := c.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "100KB prompt done", func() bool {
		e, _, err := st.ReadAny(id)
		return err == nil && e.Status == envelope.StatusDone
	})
}

func TestSendDuplicateIDRejectedAfterDone(t *testing.T) {
	// F6 replay case 1: resending an id after the task completed must
	// not re-run it.
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 10*time.Second, "original done", func() bool {
		e, _, err := st.ReadAny(id)
		return err == nil && e.Status == envelope.StatusDone
	})
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Replay", Instructions: "Again."}
	env.ID = id
	if _, err := c.Send(env); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists rejection on replay, got: %v", err)
	}
}

func TestSendDuplicateIDRejectedWhileInflight(t *testing.T) {
	// F6 replay case 2: resending an id while a slow adapter runs it
	// must not dispatch a second copy.
	script := fakeAdapterScript(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"slow"}'
read line
sleep 30
`)
	agents := map[string]config.Agent{"slow": {Command: []string{script}, Capabilities: []string{"code"}}}
	c, st, cancel := startDaemon(t, agents)
	defer cancel()
	env := envelope.New("alice", "slow", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "Slow", Instructions: "Take a while."}
	id, err := c.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "dispatch running", func() bool {
		e, _, err := st.ReadAny(id)
		return err == nil && e.Status == envelope.StatusRunning
	})
	env2 := envelope.New("alice", "slow", envelope.ShapeDelegate)
	env2.Task = envelope.Task{Title: "Dupe", Instructions: "Again."}
	env2.ID = id
	if _, err := c.Send(env2); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists rejection while in flight, got: %v", err)
	}
}

func TestRouteFailureDeadLettersAndNotifies(t *testing.T) {
	// F7: a pipeline advance that cannot validate must dead-letter the
	// original stage and notify the origin — not log and vanish.
	home := t.TempDir()
	d, err := New(home, config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapePipeline)
	env.Task = envelope.Task{Title: "Pipe", Instructions: "Do it."}
	env.Next = []string{""} // invalid hop: route's next stage cannot validate
	env.Status = envelope.StatusDone
	if err := st.Write(env); err != nil {
		t.Fatal(err)
	}
	d.route(env, "echo", &adapter.Result{Summary: "stage done"})
	got, _, err := st.ReadAny(env.ID)
	if err != nil || got.Status != envelope.StatusDead {
		t.Fatalf("original should be dead-lettered, got %+v err %v", got, err)
	}
	if !strings.Contains(got.Error, "pipeline advance failed") {
		t.Fatalf("dead letter should explain the routing failure, got %q", got.Error)
	}
	done, _ := st.List(store.DirDone)
	found := false
	for _, e := range done {
		if e.Kind == envelope.KindResult && e.ReplyTo == env.ID && e.Error != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("origin should receive a failure notification for the routing failure")
	}
}

func TestRouteCapsTranscriptExcerpt(t *testing.T) {
	// F13: the excerpt carried into the next stage is capped, not grown
	// linearly with every stage's summary.
	home := t.TempDir()
	d, err := New(home, config.Config{Agents: map[string]config.Agent{"echo": echoAgent()}, PollIntervalMs: 50, DefaultTimeoutS: 600}, errorLogger())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapePipeline)
	env.Task = envelope.Task{Title: "Pipe", Instructions: "Do it."}
	env.Next = []string{"echo2"}
	env.Context.TranscriptExcerpt = strings.Repeat("x", 20000)
	env.Status = envelope.StatusDone
	if err := st.Write(env); err != nil {
		t.Fatal(err)
	}
	d.route(env, "echo", &adapter.Result{Summary: "stage done"})
	pending, _ := st.List(store.DirPending)
	var next *envelope.Envelope
	for _, e := range pending {
		if e.To == "echo2" {
			next = e
		}
	}
	if next == nil {
		t.Fatal("next stage missing from pending/")
	}
	if n := len([]rune(next.Context.TranscriptExcerpt)); n > 8100 {
		t.Fatalf("transcript excerpt not capped: %d runes", n)
	}
	if !strings.Contains(next.Context.TranscriptExcerpt, "truncated") {
		t.Fatal("capped excerpt should carry a truncation marker")
	}
}

func TestDeadLetterNotifiesOriginAndDeadCommand(t *testing.T) {
	// F8: a dead-lettered task must notify the origin inbox AND appear
	// in `sb dead` — failures were previously invisible from the CLI.
	t.Setenv("SB_ECHO_FAIL", "1")
	c, st, cancel := startDaemon(t, map[string]config.Agent{"echo": echoAgent()})
	defer cancel()
	id := sendTask(t, c, "alice", "echo", envelope.ShapeDelegate, nil, 0)
	waitFor(t, 15*time.Second, "dead-letter", func() bool {
		e, _, err := st.ReadAny(id)
		return err == nil && e.Status == envelope.StatusDead
	})
	waitFor(t, 10*time.Second, "failure notification in origin inbox", func() bool {
		inbox, err := c.Inbox("alice")
		if err != nil {
			return false
		}
		for _, e := range inbox {
			if e.Kind == envelope.KindResult && e.ReplyTo == id && e.Error != "" {
				return true
			}
		}
		return false
	})
	dead, err := c.Dead()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range dead {
		if e.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("dead-letter missing from /v1/dead")
	}
	// CLI surface: `sb dead` lists it.
	home := st.Root()
	sb := filepath.Join(testBinDir, "sb")
	cmd := exec.Command(sb, "--home", home, "dead")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sb dead: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), id) {
		t.Fatalf("sb dead output missing id:\n%s", out)
	}
}

func TestWorkerPoolCapsConcurrency(t *testing.T) {
	// F9: with max_in_flight=2, 8 tasks to a 30s adapter must run at most
	// 2 adapters concurrently; the other 6 stay pending.
	pidFile := filepath.Join(t.TempDir(), "started.pids")
	script := fakeAdapterScript(t, `
echo '{"type":"hello","protocol":"sb/1","agent":"slow"}'
read line
echo $$ >> "`+pidFile+`"
sleep 30
`)
	home := t.TempDir()
	cfg := config.Config{
		Agents:          map[string]config.Agent{"slow": {Command: []string{script}, Capabilities: []string{"code"}}},
		PollIntervalMs:  50,
		DefaultTimeoutS: 600,
		MaxInFlight:     2,
	}
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
	c := client.New(sock)
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		env := envelope.New("alice", "slow", envelope.ShapeDelegate)
		env.Task = envelope.Task{Title: "Slow", Instructions: "Take a while."}
		if _, err := c.Send(env); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 10*time.Second, "2 adapters started", func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		return len(strings.Fields(string(data))) == 2
	})
	// Let the pool settle, then assert the cap held: 2 claimed, 6 pending.
	time.Sleep(500 * time.Millisecond)
	claimed, _ := st.List(store.DirClaimed)
	pending, _ := st.List(store.DirPending)
	if len(claimed) != 2 || len(pending) != 6 {
		t.Fatalf("pool cap violated: claimed=%d pending=%d, want 2/6", len(claimed), len(pending))
	}
	data, _ := os.ReadFile(pidFile)
	if n := len(strings.Fields(string(data))); n != 2 {
		t.Fatalf("adapter processes started = %d, want exactly 2", n)
	}
}

func TestNextPollInterval(t *testing.T) {
	// F12: idle ticks double up to the 5s cap; any work resets to base.
	base := 500 * time.Millisecond
	cases := []struct {
		current time.Duration
		work    bool
		want    time.Duration
	}{
		{500 * time.Millisecond, false, 1000 * time.Millisecond},
		{1000 * time.Millisecond, false, 2000 * time.Millisecond},
		{4000 * time.Millisecond, false, 5000 * time.Millisecond},
		{5000 * time.Millisecond, false, 5000 * time.Millisecond},
		{5000 * time.Millisecond, true, 500 * time.Millisecond},
		{0, false, 500 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := nextPollInterval(base, tc.current, tc.work); got != tc.want {
			t.Errorf("nextPollInterval(%v, %v) = %v, want %v", tc.current, tc.work, got, tc.want)
		}
	}
}

func TestCapPrefixAgentWarns(t *testing.T) {
	// F17: an agent literally named cap:review shadows the tag — the
	// daemon must warn at startup instead of silently letting it.
	home := t.TempDir()
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	_, err := New(home, config.Config{
		Agents: map[string]config.Agent{"cap:review": echoAgent()},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "cap:review") {
		t.Fatalf("expected cap:-prefix warning in logs, got:\n%s", buf.String())
	}
}

func TestDispatchValidatesPlantedEnvelope(t *testing.T) {
	// F20: a store-planted envelope that fails schema validation must
	// dead-letter (with an origin notification), never run.
	home := t.TempDir()
	st, err := store.New(home)
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.New("alice", "echo", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "", Instructions: ""} // invalid: empty title
	if err := st.Write(env); err != nil {                 // lands in pending/
		t.Fatal(err)
	}
	_, st2, cancel := startDaemonHome(t, home, map[string]config.Agent{"echo": echoAgent()}, 50)
	defer cancel()
	waitFor(t, 10*time.Second, "planted invalid envelope dead-lettered", func() bool {
		e, _, err := st2.ReadAny(env.ID)
		return err == nil && e.Status == envelope.StatusDead
	})
	e, _, _ := st2.ReadAny(env.ID)
	if e.Attempts != 1 {
		t.Fatalf("invalid envelope must not retry, attempts = %d", e.Attempts)
	}
	done, _ := st2.List(store.DirDone)
	found := false
	for _, n := range done {
		if n.Kind == envelope.KindResult && n.ReplyTo == env.ID && n.Error != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("origin should be notified of the invalid-envelope dead letter")
	}
}

func TestCLISendStdinCapped(t *testing.T) {
	// F19: piping >1MB of instructions into `sb send` must fail at the
	// CLI, not sail to the daemon.
	sb := filepath.Join(testBinDir, "sb")
	cmd := exec.Command(sb, "--home", t.TempDir(), "send", "--to", "echo", "--title", "T")
	cmd.Stdin = strings.NewReader(strings.Repeat("x", 2<<20))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected stdin-cap error, got:\n%s", out)
	}
	if !strings.Contains(string(out), "exceed 1MB") {
		t.Fatalf("expected 1MB stdin-cap message, got:\n%s", out)
	}
}

func TestCLISendRejectsControlFrom(t *testing.T) {
	// F3 at the CLI: a --from containing a newline is rejected before
	// any envelope is built.
	sb := filepath.Join(testBinDir, "sb")
	cmd := exec.Command(sb, "--home", t.TempDir(), "send", "--to", "echo", "--title", "T",
		"--instructions", "I", "--from", "eve\n## Trusted")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --from control-char rejection, got:\n%s", out)
	}
	if !strings.Contains(string(out), "control characters") {
		t.Fatalf("expected control-char message, got:\n%s", out)
	}
}

func TestCLISendRejectsEmptyNext(t *testing.T) {
	// F7 at the CLI: an empty pipeline hop is rejected before sending.
	sb := filepath.Join(testBinDir, "sb")
	cmd := exec.Command(sb, "--home", t.TempDir(), "send", "--to", "echo", "--title", "T",
		"--instructions", "I", "--shape", "pipeline", "--next", "echo,")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected empty --next rejection, got:\n%s", out)
	}
	if !strings.Contains(string(out), "--next must not be empty") {
		t.Fatalf("expected empty-next message, got:\n%s", out)
	}
}

func TestCLISendRejectsTooManyNext(t *testing.T) {
	// F13 at the CLI: more than 64 pipeline stages are rejected.
	sb := filepath.Join(testBinDir, "sb")
	next := strings.Repeat("echo,", 64) + "echo" // 65 hops
	cmd := exec.Command(sb, "--home", t.TempDir(), "send", "--to", "echo", "--title", "T",
		"--instructions", "I", "--shape", "pipeline", "--next", next)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected too-many-next rejection, got:\n%s", out)
	}
	if !strings.Contains(string(out), "capped at 64") {
		t.Fatalf("expected 64-cap message, got:\n%s", out)
	}
}
