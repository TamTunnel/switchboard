// Package daemon owns the queue, routing, adapter lifecycle, timeouts, one
// retry, and dead-lettering. It serves a local API on a UNIX SOCKET ONLY —
// there is no network listener in v1, so the only access control is
// filesystem permissions on the socket (0600, inside the user's home).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	wg     sync.WaitGroup  // in-flight dispatches; Run waits for these on shutdown
	sem    chan struct{}   // worker pool: caps concurrent dispatches
}

// shutdownWait bounds how long Run waits for in-flight dispatches after
// shutdown is requested. Dispatches are killed via context cancellation, so
// they finish quickly; the bound is a backstop against a stuck store write.
const shutdownWait = 30 * time.Second

// New creates a daemon rooted at home (e.g. ~/.switchboard).
func New(home string, cfg config.Config, log *slog.Logger) (*Daemon, error) {
	st, err := store.New(home)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	maxFlight := cfg.MaxInFlight
	if maxFlight <= 0 {
		maxFlight = config.DefaultMaxInFlight
	}
	d := &Daemon{home: home, store: st, cfg: cfg, log: log,
		active: map[string]bool{}, sem: make(chan struct{}, maxFlight)}
	// "cap:" is the capability-routing prefix: an agent literally named
	// cap:review would shadow the tag (exact names win in resolve), so
	// warn instead of silently letting the config create the collision.
	for name := range cfg.Agents {
		if strings.HasPrefix(name, "cap:") {
			log.Warn("agent name starts with reserved prefix cap:, capability routing for that tag is shadowed", "agent", name)
		}
	}
	return d, nil
}

// SocketPath returns the Unix socket path for a home directory.
func SocketPath(home string) string {
	return filepath.Join(home, "switchboard.sock")
}

// Run serves the socket API and polls the pending queue until ctx ends.
// It fails fast if another daemon already holds this home's lock: a second
// daemon used to unlink the first daemon's socket, bind its own, and
// requeue the first daemon's in-flight task — a reliable double-run.
func (d *Daemon) Run(ctx context.Context) error {
	// Single-instance lock: flock is released by the OS when the holder
	// dies (including kill -9), so a restart after a crash still works —
	// but two live daemons on one home cannot.
	lockPath := filepath.Join(d.home, "switchboard.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("daemon: lock file: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return fmt.Errorf("daemon: another switchboard daemon is already running on %s (lock held)", d.home)
	}
	defer func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}()

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
	mux.HandleFunc("/v1/dead", d.handleDead)
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

	pollInterval := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	baseInterval := pollInterval
	timer := time.NewTimer(0) // dispatch immediately; don't wait a full interval after start
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			d.waitForInflight()
			return nil
		case <-timer.C:
			started, saturated := d.poll(ctx)
			// Adaptive backoff: idle ticks double the interval up to
			// 5s, so an idle daemon stops re-scanning pending/ twice a
			// second; any dispatch (or a saturated pool) resets it.
			pollInterval = nextPollInterval(baseInterval, pollInterval, started > 0 || saturated)
			timer.Reset(pollInterval)
		}
	}
}

// maxPollInterval caps the adaptive poll backoff (see nextPollInterval).
const maxPollInterval = 5 * time.Second

// nextPollInterval computes the next poll interval: base after any work,
// doubling (capped) after idle ticks. Pure function, unit-tested.
func nextPollInterval(base, current time.Duration, didWork bool) time.Duration {
	if didWork || current <= 0 {
		return base
	}
	next := current * 2
	if next > maxPollInterval {
		return maxPollInterval
	}
	return next
}

// waitForInflight waits for in-flight dispatches to finish (their adapter
// processes are killed by context cancellation, so they fail into a retry
// or dead-letter rather than being orphaned), bounded by shutdownWait.
func (d *Daemon) waitForInflight() {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.log.Info("shutdown: in-flight dispatches finished")
	case <-time.After(shutdownWait):
		d.log.Warn("shutdown: timed out waiting for in-flight dispatches, exiting anyway")
	}
}

// rebuildIndex scans the store at startup. Duplicate ids (possible if the
// daemon crashed mid-move) resolve by scan order: dead > done > claimed >
// pending — the furthest-along copy wins, and the others are removed.
// Envelopes stranded in claimed/ (a previous daemon died mid-dispatch) are
// requeued to pending/ with their attempt count preserved: a retry must
// survive a restart, never silently vanish.
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
				// Defense in depth: List already filters non-ULID ids,
				// but never build a removal path from an unvalidated id.
				if envelope.ValidID(env.ID) {
					_ = os.Remove(filepath.Join(d.home, dir, env.ID+".json"))
				}
				continue
			}
			seen[env.ID] = dir
			if err := env.Validate(); err != nil {
				d.log.Warn("envelope fails validation at startup, leaving in place", "id", env.ID, "dir", dir, "err", err)
			}
		}
	}
	if stranded, err := d.store.List(store.DirClaimed); err == nil {
		for _, env := range stranded {
			// Kill the previous run's adapter group BEFORE requeueing:
			// without this, the orphan keeps running while the retry
			// spawns a second adapter for the same envelope.
			d.killRecordedAdapter(env)
			// The winner of dedupe above is the only copy left; anything
			// still in claimed/ was in flight when the old daemon died.
			d.log.Info("requeueing stranded envelope", "id", env.ID, "attempts", env.Attempts)
			if err := d.store.Move(env, envelope.StatusPending); err != nil {
				d.log.Error("requeue stranded envelope failed", "id", env.ID, "err", err)
			}
		}
	}
	d.log.Info("index rebuilt", "envelopes", len(seen))
}

// killRecordedAdapter SIGKILLs the adapter process group recorded in a
// stranded claimed envelope, then briefly waits for the group to die so the
// requeue cannot overlap the orphan. Best-effort: no recorded pgid (killed
// before the adapter started, or an old envelope) means nothing to kill,
// and a group that escaped via setsid or a reused pgid is out of reach —
// both are documented residual risks, not silent guarantees.
func (d *Daemon) killRecordedAdapter(env *envelope.Envelope) {
	pgid, pid := env.AdapterPgid, env.AdapterPid
	if pgid <= 0 && pid <= 0 {
		return
	}
	kill := func(sig syscall.Signal) error {
		if pgid > 0 {
			return syscall.Kill(-pgid, sig)
		}
		return syscall.Kill(pid, sig)
	}
	// A missing group is the common case (adapter already dead); only log
	// when there is actually something to kill.
	if err := kill(0); err != nil {
		return
	}
	d.log.Info("killing orphaned adapter process group from previous run", "id", env.ID, "pgid", pgid, "pid", pid)
	_ = kill(syscall.SIGKILL)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if err := kill(0); err != nil {
			return // group gone (or a zombie's group reaped)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.log.Warn("orphaned adapter group still present after kill grace period, requeueing anyway", "id", env.ID, "pgid", pgid)
}

// poll dispatches pending envelopes up to the worker-pool cap. It returns
// how many dispatches started and whether the pool saturated (pending work
// remained that the pool could not take) — the Run loop uses both for its
// adaptive backoff: saturated counts as work, never as idle.
func (d *Daemon) poll(ctx context.Context) (started int, saturated bool) {
	pending, err := d.store.List(store.DirPending)
	if err != nil {
		d.log.Error("poll: list pending", "err", err)
		return 0, false
	}
	for _, env := range pending {
		if env.Kind == envelope.KindResult {
			continue // result notifications are never dispatched
		}
		// Worker pool: excess envelopes stay pending for a later tick
		// instead of spawning an unbounded adapter fleet.
		select {
		case d.sem <- struct{}{}:
		default:
			saturated = true
			continue
		}
		d.mu.Lock()
		if d.active[env.ID] {
			d.mu.Unlock()
			<-d.sem
			continue
		}
		d.active[env.ID] = true
		d.mu.Unlock()
		d.wg.Add(1)
		go d.dispatch(ctx, env)
		started++
	}
	return started, saturated
}

func (d *Daemon) dispatch(ctx context.Context, env *envelope.Envelope) {
	defer d.wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			d.log.Error("dispatch panicked, dead-lettering", "id", env.ID, "panic", rec)
			d.fail(env, fmt.Sprintf("daemon: internal panic: %v", rec), false, "")
		}
		d.mu.Lock()
		delete(d.active, env.ID)
		d.mu.Unlock()
		<-d.sem
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

	// Defense in depth: a store-planted envelope bypassed /v1/send
	// validation. Invalid envelopes dead-letter without a retry (and with
	// an origin notification via fail), never run against an adapter.
	if err := env.Validate(); err != nil {
		d.fail(env, "envelope failed validation at dispatch: "+err.Error(), false, "")
		return
	}

	agentName, cmd, err := d.resolve(env.To)
	if err != nil {
		d.fail(env, err.Error(), false, "") // unroutable: deterministic, no retry
		return
	}
	runner := &adapter.Runner{
		Command: cmd,
		Timeout: effectiveTimeout(d.cfg, env),
		OnProgress: func(text string) {
			_ = d.store.AppendLog(env.ID, text)
		},
		OnStart: func(pid, pgid int) {
			// Record the adapter group in the claimed envelope so a
			// later restart can kill it before requeueing (F1).
			env.AdapterPid, env.AdapterPgid = pid, pgid
			_ = d.store.Write(env)
		},
	}
	res, err := runner.Run(ctx, env)
	// The run is over (or never started): the recorded group is dead or
	// reaped, so clear it before the envelope moves on — a stale pid in
	// a done/dead envelope must never be killed by a later restart.
	env.AdapterPid, env.AdapterPgid = 0, 0
	if err != nil {
		// A protocol mismatch is deterministic: retrying against the same
		// adapter cannot succeed, so it must not burn the one retry.
		retryable := !errors.Is(err, adapter.ErrProtocolMismatch)
		d.fail(env, err.Error(), retryable, agentName)
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

// maxTimeoutSeconds caps one adapter run at 24h. Without a cap, a huge
// timeout_seconds (e.g. 10^10) overflows time.Duration multiplication to
// a negative value, and the runner's Timeout > 0 check then disables the
// timeout entirely — the run hangs forever instead of timing out.
const maxTimeoutSeconds = 24 * 3600

// effectiveTimeout resolves the timeout for one dispatch: the envelope's
// override, else the daemon default, clamped to maxTimeoutSeconds.
func effectiveTimeout(cfg config.Config, env *envelope.Envelope) time.Duration {
	secs := cfg.DefaultTimeoutS
	if env.Task.Constraints.TimeoutSeconds > 0 {
		secs = env.Task.Constraints.TimeoutSeconds
	}
	if secs > maxTimeoutSeconds {
		secs = maxTimeoutSeconds
	}
	if secs <= 0 {
		secs = 600
	}
	return time.Duration(secs) * time.Second
}

// fail records a failure: one retry (back to pending) or dead-letter.
// It must NOT set env.Status before Move: Move computes the old directory
// from the current status, and a pre-mutated status leaves a stale copy of
// the envelope behind in claimed/.
func (d *Daemon) fail(env *envelope.Envelope, msg string, retryable bool, worker string) {
	env.Error = msg
	_ = d.store.AppendLog(env.ID, "failed: "+msg)
	if retryable && env.Attempts < 2 {
		d.log.Info("envelope failed, retrying once", "id", env.ID, "attempts", env.Attempts)
		if err := d.store.Move(env, envelope.StatusPending); err != nil {
			d.log.Error("fail: requeue failed", "id", env.ID, "err", err)
		}
		return
	}
	d.log.Info("envelope dead-lettered", "id", env.ID, "attempts", env.Attempts)
	d.deadLetter(env, worker)
}

// deadLetter moves an envelope to dead/ and notifies its origin. env.Error
// must already be set by the caller.
func (d *Daemon) deadLetter(env *envelope.Envelope, worker string) {
	if err := d.store.Move(env, envelope.StatusDead); err != nil {
		d.log.Error("dead-letter failed", "id", env.ID, "err", err)
		return
	}
	d.notifyFailure(env, worker)
}

// notifyFailure delivers a failure notification to the origin agent's inbox
// (a result-kind envelope in done/). Without this, a dead-lettered task
// was invisible: `sb inbox` showed nothing, and only a manual look at
// dead/ revealed it.
func (d *Daemon) notifyFailure(env *envelope.Envelope, worker string) {
	origin := env.Origin
	if origin == "" {
		origin = env.From
	}
	from := worker
	if from == "" {
		from = "switchboard"
	}
	notif := envelope.New(from, origin, env.Shape)
	notif.Kind = envelope.KindResult
	notif.Task = env.Task
	notif.ReplyTo = env.ID
	notif.Error = env.Error
	notif.Status = envelope.StatusDone
	if err := d.store.Write(notif); err != nil {
		d.log.Error("notify failure failed", "id", env.ID, "err", err)
	}
}

// maxTranscriptExcerptChars caps the transcript excerpt carried into the
// next pipeline stage. Without it, each stage prepends its summary and the
// excerpt grows linearly with the pipeline — unbounded context for the
// last agent, and an ever-larger envelope on disk.
const maxTranscriptExcerptChars = 8000

// truncateRunes cuts s to at most n runes (rune-safe, unlike byte slicing).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…[truncated]\n"
}

// route handles post-completion delivery: pipeline advance or result inbox.
func (d *Daemon) route(env *envelope.Envelope, worker string, res *adapter.Result) {
	if env.Shape == envelope.ShapePipeline && len(env.Next) > 0 {
		next := envelope.New(worker, env.Next[0], envelope.ShapePipeline)
		next.Task = env.Task
		next.Context = env.Context
		if res.Summary != "" {
			prev := fmt.Sprintf("Previous stage (%s) result:\n%s\n\n", worker, res.Summary)
			next.Context.TranscriptExcerpt = truncateRunes(prev+env.Context.TranscriptExcerpt, maxTranscriptExcerptChars)
		}
		next.Next = append([]string{}, env.Next[1:]...)
		next.Origin = env.Origin
		if next.Origin == "" {
			next.Origin = env.From
		}
		// A failed advance must NOT be a log line and nothing else: the
		// original stage would sit in done/ looking complete while no
		// next stage ran and the origin was never told. Dead-letter the
		// original (with an origin notification) instead.
		if err := next.Validate(); err != nil {
			d.log.Error("route: next-stage envelope invalid, dead-lettering original", "id", env.ID, "err", err)
			env.Error = "pipeline advance failed: " + err.Error()
			_ = d.store.AppendLog(env.ID, "routing failed: "+err.Error())
			d.deadLetter(env, worker)
			return
		}
		if err := d.store.Write(next); err != nil {
			d.log.Error("route: write next stage failed, dead-lettering original", "id", env.ID, "err", err)
			env.Error = "pipeline advance failed: " + err.Error()
			_ = d.store.AppendLog(env.ID, "routing failed: "+err.Error())
			d.deadLetter(env, worker)
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
		// Same visibility rule as a failed pipeline advance: the stage
		// completed, but the origin was never told. Dead-letter the
		// original so it surfaces in `sb dead` instead of sitting in
		// done/ looking delivered.
		d.log.Error("route: write result notification failed, dead-lettering original", "id", env.ID, "err", err)
		env.Error = "result delivery failed: " + err.Error()
		_ = d.store.AppendLog(env.ID, "routing failed: "+err.Error())
		d.deadLetter(env, worker)
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

// maxSendBytes caps a /v1/send request body. Envelopes are task text, and
// every downstream stage has a smaller ceiling: the daemon↔adapter wire
// (8MB escaped line) and, decisively, agent CLIs taking the prompt as one
// argv element (Linux caps a single argument at 128KB). Accepting 10MB
// envelopes the adapters could never execute just produced dead letters
// after a futile retry.
const maxSendBytes = 1 << 20

// maxPromptBytes caps the rendered prompt (BuildPrompt output) at send
// time. Agent CLIs receive the prompt as a single argv element, and Linux
// refuses any single argument over 128KB (MAX_ARG_STRLEN) with "argument
// list too long" — so a bigger prompt can never run. Rejecting at send
// with a clear error beats dead-lettering after a retry.
const maxPromptBytes = 128 << 10

func (d *Daemon) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Bound the request body: an envelope is small text, never megabytes.
	r.Body = http.MaxBytesReader(w, r.Body, maxSendBytes)
	var env envelope.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "bad envelope: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := env.Validate(); err != nil {
		http.Error(w, "envelope failed validation: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Result notifications are created by the daemon itself (route writes
	// them straight to done/). A client-sent result would land in pending/,
	// where poll() skips it forever — reject it instead of stranding it.
	if env.Kind == envelope.KindResult {
		http.Error(w, "result envelopes are created by the daemon, not sent", http.StatusBadRequest)
		return
	}
	if len(adapter.BuildPrompt(&env)) > maxPromptBytes {
		http.Error(w, "envelope prompt exceeds the 128KB agent-CLI argument limit; shorten instructions or split the task", http.StatusBadRequest)
		return
	}
	// Envelope ids are unique: a resend of an id that already exists in
	// any store directory is a replay, not a new task — it used to
	// re-run the task (twice, if the first run was still in flight).
	// Check and write under one lock so two concurrent sends of the
	// same id cannot both slip through.
	d.mu.Lock()
	if _, _, err := d.store.ReadAny(env.ID); err == nil {
		d.mu.Unlock()
		http.Error(w, "envelope id already exists", http.StatusConflict)
		return
	}
	env.Status = envelope.StatusPending
	env.Attempts = 0
	if env.Kind == "" {
		env.Kind = envelope.KindTask
	}
	err := d.store.Write(&env)
	d.mu.Unlock()
	if err != nil {
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

func (d *Daemon) handleDead(w http.ResponseWriter, r *http.Request) {
	envs, _ := d.store.List(store.DirDead)
	if envs == nil {
		envs = []*envelope.Envelope{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envs)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing ?id=", http.StatusBadRequest)
		return
	}
	if !envelope.ValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
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
	if !envelope.ValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
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
