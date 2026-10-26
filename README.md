# Switchboard

## The problem

You run more than one coding agent — Claude Code in one terminal, Codex in
another, OpenCode somewhere else. Sooner or later you want one of them to hand
work to another: "you've explored the codebase, now have the other agent
implement the fix," or "review this diff before I merge."

Today that handoff is you. You copy context out of one terminal, paste it into
another, re-explain the task, and hope nothing got lost. The agents can't talk
to each other, so you become the bus — a slow, lossy, human-shaped one.

## The solution, in plain terms

Switchboard is a **local task bus for coding agents**. Think of it like email
for agents: any agent with shell access can send a task to any other agent
with one command, and the result comes back to the sender's inbox. No
central server, no account, no cloud — everything lives on your machine, in
plain files you can read.

```sh
# From any agent's terminal: hand a task to another agent
sb send --to codex --title "Implement rate limiting" \
  --instructions "Add per-key rate limiting to the API. See NOTES.md for the design."

# Later, check what came back
sb inbox
```

That's the whole idea. The "email, not Slack" play: an open protocol anyone
can implement, instead of another walled workspace that owns your handoffs.

## Quick start

### Build

```sh
git clone https://github.com/TamTunnel/switchboard
cd switchboard
CGO_ENABLED=0 go build -o /usr/local/bin/switchboard ./cmd/switchboard
CGO_ENABLED=0 go build -o /usr/local/bin/sb ./cmd/sb
CGO_ENABLED=0 go build -o /usr/local/bin/sb-adapter ./cmd/sb-adapter
```

Go 1.23+ required. The three binaries are static — no runtime to install.
The commands pin CGO_ENABLED=0 because the daemon and CLI import `net`,
whose cgo resolver would otherwise dynamically link a default build; `sb`
also deliberately avoids os/user so no first-party code needs cgo at all.

### Run

```sh
# Terminal 1: start the daemon (it owns the queue)
switchboard daemon

# Terminal 2: send a task, then check results
sb send --to echo --title "Hello" --instructions "Say hello back."
sb inbox          # results land here
sb status <id>    # full envelope as JSON
sb logs <id>      # adapter run log
sb dead           # dead-lettered envelopes and why they failed
```

The daemon stores everything under `~/.switchboard/` (override with
`--home` or `$SWITCHBOARD_HOME`). See `~/.switchboard/config.yaml` for
agent configuration — the built-in defaults cover `echo`, `claude`, `codex`,
and `opencode`, and file agents merge over the defaults (a config with one
custom agent keeps the built-ins; overriding a built-in name replaces just
that agent). Agent names must not start with `cap:` — that prefix is
reserved for capability routing, and the daemon logs a warning if one does.
At most 4 adapters run concurrently by default (`max_in_flight` in the
config); excess envelopes stay pending.

### Handoff shapes

```sh
# 1. Fire-and-forget delegation: A → B, result returns to A's inbox
sb send --to codex --title "Fix the flaky test" --instructions "..."

# 2. Sequential pipeline: A → B → C, final result returns to A
sb send --to claude --shape pipeline --next codex --next opencode \
  --title "Explore, implement, then review" --instructions "..."

# 3. Review request: B reviews A's diff, comments return to A
sb send --to claude --shape review --diff-file changes.diff \
  --title "Review this diff" --instructions "Focus on security issues."
```

Routing is explicit: `--to` takes an agent name, or a capability tag like
`--to cap:review` (first agent declaring that capability wins). No model
picks the agent for you.

### Keep it running

- **systemd (Linux):** copy `deploy/switchboard.service` to
  `~/.config/systemd/user/`, adjust the binary path, `systemctl --user
  enable --now switchboard`.
- **launchd (macOS):** copy `deploy/dev.switchboard.daemon.plist` to
  `~/Library/LaunchAgents/`, adjust the binary path, `launchctl load` it.

## Architecture

```
┌──────────┐   sb send    ┌──────────────┐  stdio NDJSON   ┌─────────────┐
│  Agent A │ ────────────▶│   Daemon     │ ───────────────▶│  Adapter    │
│ (any CLI)│   unix sock  │   (queue,    │  (separate     │  (sb-adapter│
└──────────┘              │    routing,  │   process)     │   echo/     │
       ▲                  │    retries)  │                │   claude/   │
       │  result inbox    └──────┬───────┘                │   codex/    │
       └─────────────────────────┘                        │   opencode) │
                                                          └──────┬──────┘
                                                                   │ agent CLI
                                                                   ▼
                                                            ┌──────────┐
                                                            │ Agent B  │
                                                            └──────────┘
```

**Components**

- **Envelope (`sb/1`)** — the unit of work: id (ULID), from/to, task
  (title, markdown instructions, done criteria, constraints), context refs
  (workspace path, git ref, file list, transcript excerpt), artifacts out,
  and a status lifecycle (pending → claimed → running → done/failed, with a
  dead-letter queue after one retry). Validated against a JSON Schema at the
  daemon boundary.
- **Daemon (`switchboard daemon`)** — owns the queue, routing, adapter
  lifecycle, timeouts, one retry, and dead-lettering. Serves a local API on
  a **Unix socket only** — no network listener in v1, so there is no auth
  surface beyond filesystem permissions on `~/.switchboard`.
- **Store** — maildir-style: envelopes are JSON files in
  `~/.switchboard/{pending,claimed,done,dead}/`. The daemon is the sole
  writer; the in-memory index is rebuilt on start. No database dependency.
  Scaling, stated honestly: the daemon polls by re-scanning `pending/` each
  tick, with an adaptive interval that backs off to 5s when idle — so an
  idle daemon costs almost nothing, but a very large queue still costs a
  full scan per active tick. Keep queues modest; incremental indexing or
  fsnotify is future work, not v1.
- **Adapters** — separate processes speaking versioned NDJSON over stdio
  (see `docs/adapter-protocol.md` — that document IS the open standard).
  Each adapter translates an envelope into its agent CLI's invocation, runs
  it, and translates output back into a result envelope. A crashed or hung
  adapter never takes down the daemon.
- **CLI (`sb`)** — `send`, `inbox`, `status`, `logs`, `dead`. Any agent with shell
  access can hand off; no plugin or MCP integration is needed to start.

**Translation, honestly stated.** v1 translates at the **task level** only:
structured task spec + file refs + markdown summary + transcript excerpt,
formatted into the target agent's prompt. Same machine means file refs are
exact and free. **Session-level translation** (full-fidelity portability of
an agent's session state between different agents' formats) is post-v1 and
is not claimed anywhere in this project.

## FAQs

**Which agents are supported?**
Claude Code, Codex, and OpenCode via their CLIs, plus a built-in `echo`
reference adapter. Any program speaking the adapter protocol can be added —
see `docs/adapter-protocol.md`.

**Do I need the daemon running to send?**
Yes. `sb send` talks to the daemon's Unix socket. If the daemon isn't up,
you get a clear error telling you to start it.

**What happens when an agent fails or hangs?**
Timeout (default 10 minutes, per-task override via `--timeout`), then one
retry, then the envelope goes to the dead-letter queue (`dead/`) with the
error attached — and the origin agent gets a failure notification in its
inbox, so nothing is silently dropped. `sb dead` lists dead letters from
the CLI. Only one daemon may run per home directory (a lock file enforces
it); a second daemon fails fast instead of stealing the socket and
requeueing the first daemon's in-flight task. If the daemon itself restarts
mid-task, in-flight envelopes are requeued with their attempt count
preserved, so a retry survives a restart — and on restart the daemon kills
the previous run's adapter process group (recorded in the claimed envelope)
before requeueing, so the retry never overlaps an orphaned adapter.
Residual risk, stated honestly: an adapter that escaped its process group
(e.g. via setsid/daemonization) cannot be killed this way and may still be
running when the retry starts, and a pid reused after power loss could in
theory point at an unrelated process. On shutdown the daemon waits
(bounded) for in-flight dispatches, so the adapter's process group is
killed with the run instead of orphaned — with the same setsid escape
caveat. Cgroup supervision that would close the setsid hole is out of v1
scope.

**Is my data sent anywhere?**
No. Everything is local: Unix socket, local files, local agent CLIs. There
is no cloud relay, no account, no telemetry.

**What are the size limits?**
`/v1/send` accepts at most 1MB per envelope, and the rendered prompt
handed to an agent CLI must fit in 128KB — agent CLIs receive the prompt
as a single argv element, and Linux refuses any single argument over
128KB. Oversized sends are rejected immediately with a clear error,
instead of dead-lettering after a futile retry. The daemon↔adapter wire
allows lines up to 8MB (a 1MB envelope re-encoded as JSON can grow ~6x
when every byte needs escaping). Pipelines are capped at 64 stages, and
the transcript excerpt carried into each next stage is capped at ~8000
characters. Run logs are capped at 10MB per envelope.

**Is Codex support live-tested?**
No — and the same qualifier applies to Claude Code and OpenCode here.
All three adapters are implemented against their CLIs' documented flags
and unit-tested against scripted stand-ins emitting the documented
formats; none has been live-tested against the real CLI in this project's
build environment. Known Codex failure mode, named plainly: an approval
prompt can hang a non-interactive `codex exec` until the daemon timeout
fires, then one retry, then dead-letter.

**Can agents on different machines talk?**
Not in v1. Same machine, same workspace only. Network/remote agents are
explicitly out of scope for now.

**Can an agent inject instructions into another via a handoff?**
That's the known risk, and it's documented, not hidden. Every envelope
carries provenance (who sent it), and the prompt builder tells the receiving
agent to treat handoff instructions as **data from an untrusted peer, not
as trusted system instructions**. Two qualifiers that matter: `From` is
sender-asserted, not daemon-verified — any local process as the same user
can claim any sender name, and the daemon records it verbatim. The
same-user Unix-socket boundary is the only identity control in v1; there
is no multi-user auth because there is no network surface to authenticate.
Provenance is a display string, not a trust signal. See
`docs/adapter-protocol.md` § Security.

**Why Go?**
A single static binary (~14MB class) with no runtime for the user to
install, stdlib-first for the runtime paths. The only third-party
dependencies are cobra (CLI), yaml.v3 (config), and jsonschema (envelope
schema validation). The daemon is I/O-bound supervision work, not a hot
loop — nothing here justifies a heavier stack.

## Repository best practices

- **Tests:** `go test ./...` — full suite including echo-adapter
  integration tests (daemon + real adapter binary, end to end). Tests must
  be green before any release.
- **Binaries are never committed.** Build outputs (`switchboard`, `sb`,
  `sb-adapter`) are gitignored; releases ship built artifacts, not repo
  binaries.
- **Echo test hooks are tag-gated.** `SB_ECHO_FAIL`/`SB_ECHO_HANG` only
  work in builds with the `switchboardtesthooks` build tag (test binaries);
  shipped binaries ignore them.
- **No personal identifiers** in code, docs, or examples — fake examples
  only (e.g. workspace `/tmp/example-project`, agent names like `alice`).
- **License:** Apache-2.0. See `LICENSE`.
- **Contributing:** adapters are the contribution surface. Implement
  `docs/adapter-protocol.md` in any language, add it to the version matrix
  with an honest testing note, and open a PR.
