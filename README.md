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
```

The daemon stores everything under `~/.switchboard/` (override with
`--home` or `$SWITCHBOARD_HOME`). See `~/.switchboard/config.yaml` for
agent configuration — the built-in defaults cover `echo`, `claude`, `codex`,
and `opencode`.

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
- **Adapters** — separate processes speaking versioned NDJSON over stdio
  (see `docs/adapter-protocol.md` — that document IS the open standard).
  Each adapter translates an envelope into its agent CLI's invocation, runs
  it, and translates output back into a result envelope. A crashed or hung
  adapter never takes down the daemon.
- **CLI (`sb`)** — `send`, `inbox`, `status`, `logs`. Any agent with shell
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
error attached. Nothing is silently dropped. If the daemon itself restarts
mid-task, in-flight envelopes are requeued with their attempt count
preserved, so a retry survives a restart. On shutdown the daemon waits
(bounded) for in-flight dispatches, so their adapter processes are killed
with the run instead of orphaned.

**Is my data sent anywhere?**
No. Everything is local: Unix socket, local files, local agent CLIs. There
is no cloud relay, no account, no telemetry.

**Can agents on different machines talk?**
Not in v1. Same machine, same workspace only. Network/remote agents are
explicitly out of scope for now.

**Can an agent inject instructions into another via a handoff?**
That's the known risk, and it's documented, not hidden. Every envelope
carries provenance (who sent it), and the prompt builder tells the receiving
agent to treat handoff instructions as **data from an untrusted peer, not
as trusted system instructions**. See `docs/adapter-protocol.md` § Security.

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
- **No personal identifiers** in code, docs, or examples — fake examples
  only (e.g. workspace `/tmp/example-project`, agent names like `alice`).
- **License:** Apache-2.0. See `LICENSE`.
- **Contributing:** adapters are the contribution surface. Implement
  `docs/adapter-protocol.md` in any language, add it to the version matrix
  with an honest testing note, and open a PR.
