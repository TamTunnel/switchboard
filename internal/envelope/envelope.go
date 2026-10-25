// Package envelope defines the Switchboard task envelope (protocol "sb/1"),
// its JSON Schema validation, and ULID generation. It has no I/O dependencies.
package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ProtocolVersion is the only envelope protocol version v1 understands.
const ProtocolVersion = "sb/1"

// Statuses in the envelope lifecycle.
const (
	StatusPending = "pending"
	StatusClaimed = "claimed"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusDead    = "dead"
)

// Handoff shapes.
const (
	ShapeDelegate = "delegate"
	ShapePipeline = "pipeline"
	ShapeReview   = "review"
)

// Envelope kinds: a task to be executed, or a result notification for an inbox.
const (
	KindTask   = "task"
	KindResult = "result"
)

// Task is the structured work description. v1 is task-level only: a title,
// markdown instructions, done criteria, and constraints. There is deliberately
// no session-state field — full-fidelity session portability is post-v1.
type Task struct {
	Title        string      `json:"title"`
	Instructions string      `json:"instructions"`
	DoneCriteria []string    `json:"done_criteria,omitempty"`
	Constraints  Constraints `json:"constraints,omitempty"`
}

// Constraints are hard limits the daemon enforces (not suggestions).
type Constraints struct {
	// TimeoutSeconds caps one adapter run. Zero means the daemon default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Context carries references the receiving agent needs. Same-machine v1 means
// file refs are exact paths; nothing here is fetched over a network.
type Context struct {
	Workspace         string   `json:"workspace,omitempty"`
	GitRef            string   `json:"git_ref,omitempty"`
	Files             []string `json:"files,omitempty"`
	TranscriptExcerpt string   `json:"transcript_excerpt,omitempty"`
	// Diff is used by the review shape: the diff under review.
	Diff string `json:"diff,omitempty"`
	// ResumeSession, when set, is passed to adapters that support resuming
	// an agent session (e.g. Claude Code --resume). Opaque to the daemon.
	ResumeSession string `json:"resume_session,omitempty"`
}

// Result is what an adapter reports back.
type Result struct {
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Diff      string   `json:"diff,omitempty"`
}

// Envelope is the unit of work on the bus.
type Envelope struct {
	Version  string  `json:"version"`
	ID       string  `json:"id"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Shape    string  `json:"shape"`
	Kind     string  `json:"kind,omitempty"`
	Task     Task    `json:"task"`
	Context  Context `json:"context,omitempty"`
	Status   string  `json:"status"`
	Attempts int     `json:"attempts"`
	// Next holds remaining pipeline stages (agent names or cap: tags).
	// Empty for delegate/review shapes.
	Next []string `json:"next,omitempty"`
	// Origin is the agent the final result is delivered to. Defaults to From.
	Origin string `json:"origin,omitempty"`
	// ReplyTo links a result envelope to the task it answers.
	ReplyTo string `json:"reply_to,omitempty"`
	// Provenance is a human-readable statement of who sent this envelope.
	// Receivers display it; it is not a trust signal.
	Provenance string `json:"provenance,omitempty"`
	// AdapterPid/AdapterPgid record the adapter process (group leader) the
	// daemon started for this envelope's current run. On restart, the
	// daemon SIGKILLs the recorded group before requeueing a stranded
	// claimed envelope, so the retry never overlaps an orphaned adapter.
	// Best-effort only: cleared when the run ends, and useless if the
	// adapter escaped its process group (setsid) or the pid was reused.
	AdapterPid  int     `json:"adapter_pid,omitempty"`
	AdapterPgid int     `json:"adapter_pgid,omitempty"`
	Result      *Result `json:"result,omitempty"`
	Error       string  `json:"error,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
	UpdatedAt   string  `json:"updated_at,omitempty"`
}

// ulidPattern mirrors the "id" pattern in the sb/1 JSON Schema. Store paths
// are built from ids, so ValidID is the traversal guard: anything that is
// not a ULID must never reach a filesystem path.
var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidID reports whether id is a well-formed ULID.
func ValidID(id string) bool { return ulidPattern.MatchString(id) }

// New creates a task envelope with a fresh ULID id. The caller fills To/Task.
func New(from, to, shape string) *Envelope {
	now := time.Now().UTC().Format(time.RFC3339)
	return &Envelope{
		Version:    ProtocolVersion,
		ID:         NewULID(),
		From:       from,
		To:         to,
		Shape:      shape,
		Kind:       KindTask,
		Status:     StatusPending,
		Origin:     from,
		Provenance: "sent by agent " + from,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// Touch updates the updated_at timestamp.
func (e *Envelope) Touch() {
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

// Validate checks the envelope against the sb/1 JSON Schema. This runs at the
// daemon boundary (on send and before dispatch), never trusting CLI input.
func (e *Envelope) Validate() error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return err
	}
	return compiledSchema.Validate(v)
}

// ValidateBytes validates raw envelope JSON without unmarshalling into a struct.
func ValidateBytes(raw []byte) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return err
	}
	return compiledSchema.Validate(v)
}

var compiledSchema *jsonschema.Schema

func init() {
	c := jsonschema.NewCompiler()
	var doc any
	if err := json.Unmarshal([]byte(envelopeSchema), &doc); err != nil {
		panic(err)
	}
	if err := c.AddResource("sb1.json", doc); err != nil {
		panic(err)
	}
	s, err := c.Compile("sb1.json")
	if err != nil {
		panic(err)
	}
	compiledSchema = s
}

const envelopeSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["version", "id", "from", "to", "shape", "task", "status"],
  "properties": {
    "version": {"const": "sb/1"},
    "id": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$"},
    "from": {"type": "string", "minLength": 1, "pattern": "^[^[:cntrl:]]+$"},
    "to": {"type": "string", "minLength": 1},
    "shape": {"enum": ["delegate", "pipeline", "review"]},
    "kind": {"enum": ["task", "result"]},
    "task": {
      "type": "object",
      "required": ["title", "instructions"],
      "properties": {
        "title": {"type": "string", "minLength": 1},
        "instructions": {"type": "string", "minLength": 1},
        "done_criteria": {"type": "array", "items": {"type": "string"}},
        "constraints": {"type": "object", "properties": {
          "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 86400}
        }}
      }
    },
    "context": {"type": "object", "properties": {
      "workspace": {"type": "string"},
      "git_ref": {"type": "string"},
      "files": {"type": "array", "items": {"type": "string"}},
      "transcript_excerpt": {"type": "string"},
      "diff": {"type": "string"},
      "resume_session": {"type": "string"}
    }},
    "status": {"enum": ["pending", "claimed", "running", "done", "failed", "dead"]},
    "attempts": {"type": "integer", "minimum": 0},
    "next": {"type": "array", "items": {"type": "string", "minLength": 1}, "maxItems": 64},
    "origin": {"type": "string"},
    "reply_to": {"type": "string"},
    "provenance": {"type": "string"},
    "result": {"type": "object", "properties": {
      "summary": {"type": "string"},
      "artifacts": {"type": "array", "items": {"type": "string"}},
      "diff": {"type": "string"}
    }},
    "error": {"type": "string"}
  }
}`

// --- ULID (Crockford base32, 26 chars: 48-bit ms timestamp + 80-bit random) ---

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a fresh ULID string.
func NewULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("switchboard: crypto/rand failed: %v", err))
	}
	out := make([]byte, 26)
	// Timestamp: 48 bits -> 10 chars (first char holds only 3 bits, top bits zero).
	out[0] = crockford[(b[0]>>5)&0x07]
	out[1] = crockford[b[0]&0x1f]
	out[2] = crockford[(b[1]>>3)&0x1f]
	out[3] = crockford[((b[1]&0x07)<<2)|(b[2]>>6)]
	out[4] = crockford[(b[2]>>1)&0x1f]
	out[5] = crockford[((b[2]&0x01)<<4)|(b[3]>>4)]
	out[6] = crockford[((b[3]&0x0f)<<1)|(b[4]>>7)]
	out[7] = crockford[(b[4]>>2)&0x1f]
	out[8] = crockford[((b[4]&0x03)<<3)|(b[5]>>5)]
	out[9] = crockford[b[5]&0x1f]
	// Randomness: 80 bits -> 16 chars, encoded as a 5-bit stream.
	bits := 0
	acc := 0
	pos := 10
	for k := 6; k < 16; k++ {
		acc = (acc << 8) | int(b[k])
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[pos] = crockford[(acc>>bits)&0x1f]
			pos++
		}
	}
	if bits > 0 {
		out[pos] = crockford[(acc<<(5-bits))&0x1f]
		pos++
	}
	if pos != 26 {
		panic(fmt.Sprintf("switchboard: ULID encoding bug, pos=%d", pos))
	}
	return string(out)
}
