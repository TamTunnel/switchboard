// Package daemon owns the queue, routing, adapter lifecycle, timeouts, one
// retry, and dead-lettering. It serves a local API on a UNIX SOCKET ONLY —
// there is no network listener in v1, so the only access control is
// filesystem permissions on the socket (0600, inside the user's home).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/TamTunnel/switchboard/internal/adapter"
	"github.com/TamTunnel/switchboard/internal/config"
	"github.com/TamTunnel/switchboard/internal/envelope"
	"github.com/TamTunnel/switchboard/internal/store"
)

// Daemon is the switchboard daemon.
type Daemon struct {
	home   string
	store  *store.Store
	cfg    config.Config
	log    *slog.Logger
	mu     sync.Mutex
	active map[string]bool // envelope ids currently being processed
}

// New creates a daemon rooted at home (e.g. ~/.switchboard).
func New(home string, cfg config.Config, log *slog.Logger) (*Daemon, error) {
	st, err := store.New(home)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Daemon{home: home, store: st, cfg: cfg, log: log, active: map[string]bool{}}, nil
}

// SocketPath returns the Unix socket path for a home directory.
func SocketPath(home string) string {
	return filepath.Join(home, "switchboard.sock")
}

// Run serves the socket API and polls the pending queue until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	d.rebuildIndex()
	sockPath := SocketPath(d.home)
	_ = os.Remove(sockPath) // stale socket from an unclean shutdown
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	defer func() {
		ln.Close()
		os.Remove(sockPath)
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/send", d.handleSend)
	mux.HandleFunc("/v1/inbox", d.handleInbox)
	mux.HandleFunc("/v1/status", d.handleStatus)
	mux.HandleFunc("/v1/logs", d.handleLogs)
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		_ = srv.Serve(ln)
	}()
	d.log.Info("daemon listening", "socket", sockPath, "home", d.home)

	poll := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			d.poll()
		}
	}
}

// rebuildIndex scans the store at startup. Duplicate ids (possible if the
// daemon crashed mid-move) resolve by scan order: dead > done > claimed >
// pending — the furthest-along copy wins, and the others are removed.
func (d *Daemon) rebuildIndex() {
	seen := map[string]string{}
	for _, dir := range []string{store.DirDead, store.DirDone, store.DirClaimed, store.DirPending} {
		envs, err := d.store.List(dir)
		if err != nil {
			continue
		}
		for _, env := range envs {
			if prev, dup := seen[env.ID]; dup {
				d.log.Warn("duplicate envelope id, dropping stale copy", "id", env.ID, "keep", prev, "drop", dir)
				_ = os.Remove(filepath.Join(d.home, dir, env.ID+".json"))
				continue
			}
			seen[env.ID] = dir
			if err := env.Validate(); err != nil {
				d.log.Warn("envelope fails validation at startup, leaving in place", "id", env.ID, "dir", dir, "err", err)
			}
		}
	}
	d.log.Info("index rebuilt", "envelopes", len(seen))
}

func (d *Daemon) poll() {
	pending, err := d.store.List(store.DirPending)
	if err != nil {
		d.log.Error("poll: list pending", "err", err)
		return
	}
	for _, env := range pending {
		if env.Kind == envelope.KindResult {
			continue // result notifications are never dispatched
		}
		d.mu.Lock()
		if d.active[env.ID] {
			d.mu.Unlock()
			continue
		}
		d.active[env.ID] = true
		d.mu.Unlock()
		go d.dispatch(env)
	}
}

func (d *Daemon) dispatch(env *envelope.Envelope) {
	defer func() {
		if rec := recover(); rec != nil {
			d.log.Error("dispatch panicked, dead-lettering", "id", env.ID, "panic", rec)
			env.Status = envelope.StatusDead
			env.Error = fmt.Sprintf("daemon: internal panic: %v", rec)
			_ = d.store.Move(env, envelope.StatusDead)
		}
		d.mu.Lock()
		delete(d.active, env.ID)
		d.mu.Unlock()
	}()

	// Claim: pending -> claimed, attempts++.
	env.Attempts++
	if err := d.store.Move(env, envelope.StatusClaimed); err != nil {
		d.log.Error("dispatch: claim failed", "id", env.ID, "err", err)
		return
	}
	env.Status = envelope.StatusRunning
	if err := d.store.Write(env); err != nil {
		d.log.Error("dispatch: mark running failed", "id", env.ID, "err", err)
		return
	}

	agentName, cmd, err := d.resolve(env.To)
	if err != nil {
		d.fail(env, err.Error(), false) // unroutable: deterministic, no retry
		return
	}
	timeout := d.cfg.DefaultTimeoutS
	if env.Task.Constraints.TimeoutSeconds > 0 {
		timeout = env.Task.Constraints.TimeoutSeconds
	}
	runner := &adapter.Runner{
		Command: cmd,
		Timeout: time.Duration(timeout) * time.Second,
		OnProgress: func(text string) {
			_ = d.store.AppendLog(env.ID, text)
		},
	}
	ctx := context.Background()
	res, err := runner.Run(ctx, env)
	if err != nil {
		d.fail(env, err.Error(), true)
		return
	}
	if res.Degraded {
		_ = d.store.AppendLog(env.ID, "note: result degraded to raw text (no structured result from adapter)")
	}
	env.Result = &envelope.Result{Summary: res.Summary, Artifacts: res.Artifacts, Diff: res.Diff}
	if err := d.store.Move(env, envelope.StatusDone); err != nil {
		d.log.Error("dispatch: mark done failed", "id", env.ID, "err", err)
		return
	}
	d.route(env, agentName, res)
}

// fail records a failure: one retry (back to pending) or dead-letter.
func (d *Daemon) fail(env *envelope.Envelope, msg string, retryable bool) {
	env.Error = msg
	_ = d.store.AppendLog(env.ID, "failed: "+msg)
	if retryable && env.Attempts < 2 {
		d.log.Info("envelope failed, retrying once", "id", env.ID, "attempts", env.Attempts)
		env.Status = envelope.StatusPending
		if err := d.store.Move(env, envelope.StatusPending); err != nil {
			d.log.Error("fail: requeue failed", "id", env.ID, "err", err)
		}
		return
	}
	d.log.Info("envelope dead-lettered", "id", env.ID, "attempts", env.Attempts)
	if err := d.store.Move(env, envelope.StatusDead); err != nil {
		d.log.Error("fail: dead-letter failed", "id", env.ID, "err", err)
	}
}

// route handles post-completion delivery: pipeline advance or result inbox.
func (d *Daemon) route(env *envelope.Envelope, worker string, res *adapter.Result) {
	if env.Shape == envelope.ShapePipeline && len(env.Next) > 0 {
		next := envelope.New(worker, env.Next[0], envelope.ShapePipeline)
		next.Task = env.Task
		next.Context = env.Context
		if res.Summary != "" {
			prev := fmt.Sprintf("Previous stage (%s) result:\n%s\n\n", worker, res.Summary)
			next.Context.TranscriptExcerpt = prev + env.Context.TranscriptExcerpt
		}
		next.Next = append([]string{}, env.Next[1:]...)
		next.Origin = env.Origin
		if next.Origin == "" {
			next.Origin = env.From
		}
		if err := next.Validate(); err != nil {
			d.log.Error("route: next-stage envelope invalid", "id", env.ID, "err", err)
			return
		}
		if err := d.store.Write(next); err != nil {
			d.log.Error("route: write next stage failed", "id", env.ID, "err", err)
			return
		}
		d.log.Info("pipeline advanced", "from", env.ID, "to", next.ID, "agent", next.To)
		return
	}
	// Final delivery: result notification to the origin agent's inbox.
	origin := env.Origin
	if origin == "" {
		origin = env.From
	}
	notif := envelope.New(worker, origin, env.Shape)
	notif.Kind = envelope.KindResult
	notif.Task = env.Task
	notif.ReplyTo = env.ID
	notif.Result = env.Result
	notif.Status = envelope.StatusDone
	if err := d.store.Write(notif); err != nil {
		d.log.Error("route: write result notification failed", "id", env.ID, "err", err)
		return
	}
	d.log.Info("result delivered", "task", env.ID, "to", origin)
}

// resolve maps a `to` value to an agent name and adapter command. Exact agent
// names win; "cap:<tag>" selects the alphabetically first agent declaring
// that capability (deterministic). Anything else is an error.
func (d *Daemon) resolve(to string) (string, []string, error) {
	if a, ok := d.cfg.Agents[to]; ok {
		return to, a.Command, nil
	}
	if tag, ok := stringsCutPrefix(to, "cap:"); ok {
		names := make([]string, 0, len(d.cfg.Agents))
		for name := range d.cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, cap := range d.cfg.Agents[name].Capabilities {
				if cap == tag {
					return name, d.cfg.Agents[name].Command, nil
				}
			}
		}
		return "", nil, fmt.Errorf("no agent with capability %q", tag)
	}
	return "", nil, fmt.Errorf("unknown agent %q (no such agent, and not a cap: tag)", to)
}

func stringsCutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// --- Socket API handlers ---

func (d *Daemon) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var env envelope.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "bad envelope: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := env.Validate(); err != nil {
		http.Error(w, "envelope failed validation: "+err.Error(), http.StatusBadRequest)
		return
	}
	env.Status = envelope.StatusPending
	env.Attempts = 0
	if env.Kind == "" {
		env.Kind = envelope.KindTask
	}
	if err := d.store.Write(&env); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": env.ID})
}

func (d *Daemon) handleInbox(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		http.Error(w, "missing ?agent=", http.StatusBadRequest)
		return
	}
	var out []*envelope.Envelope
	for _, dir := range []string{store.DirPending, store.DirClaimed} {
		envs, _ := d.store.List(dir)
		for _, e := range envs {
			if e.To == agent && e.Kind == envelope.KindTask {
				out = append(out, e)
			}
		}
	}
	if done, _ := d.store.List(store.DirDone); done != nil {
		for _, e := range done {
			if e.To == agent && e.Kind == envelope.KindResult {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing ?id=", http.StatusBadRequest)
		return
	}
	env, _, err := d.store.ReadAny(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(env)
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing ?id=", http.StatusBadRequest)
		return
	}
	data, err := d.store.ReadLog(id)
	if err != nil {
		http.Error(w, "logs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(data))
}
