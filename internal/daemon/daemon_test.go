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
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Agents: agents, PollIntervalMs: 50, DefaultTimeoutS: 600}
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
