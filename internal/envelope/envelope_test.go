package envelope

import (
	"regexp"
	"strings"
	"testing"
)

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestULIDFormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewULID()
		if !ulidRe.MatchString(id) {
			t.Fatalf("ULID %q does not match Crockford pattern", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ULID %q", id)
		}
		seen[id] = true
	}
	// Timestamp prefix: two ULIDs generated now share the first 10 chars
	// (same millisecond is not guaranteed, so only check length/prefix shape).
	if len(NewULID()) != 26 {
		t.Fatal("ULID must be 26 chars")
	}
}

func TestNewAndValidate(t *testing.T) {
	env := New("alice", "bob", ShapeDelegate)
	env.Task = Task{Title: "Do a thing", Instructions: "Do it well."}
	if err := env.Validate(); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	if env.Status != StatusPending || env.Kind != KindTask || env.Origin != "alice" {
		t.Fatalf("bad defaults: %+v", env)
	}
	if !strings.Contains(env.Provenance, "alice") {
		t.Fatalf("provenance must name the sender: %q", env.Provenance)
	}
}

func TestValidateRejects(t *testing.T) {
	base := func() *Envelope {
		env := New("alice", "bob", ShapeDelegate)
		env.Task = Task{Title: "T", Instructions: "I"}
		return env
	}
	cases := map[string]func(*Envelope){
		"bad version":  func(e *Envelope) { e.Version = "sb/2" },
		"bad id":       func(e *Envelope) { e.ID = "not-a-ulid" },
		"empty from":   func(e *Envelope) { e.From = "" },
		"bad shape":    func(e *Envelope) { e.Shape = "teleport" },
		"bad status":   func(e *Envelope) { e.Status = "limbo" },
		"empty title":  func(e *Envelope) { e.Task.Title = "" },
		"empty instr":  func(e *Envelope) { e.Task.Instructions = "" },
		"bad timeout":  func(e *Envelope) { e.Task.Constraints.TimeoutSeconds = -5 },
		"neg attempts": func(e *Envelope) { e.Attempts = -1 },
	}
	for name, mut := range cases {
		env := base()
		mut(env)
		if err := env.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got none", name)
		}
	}
}

func TestValidateBytes(t *testing.T) {
	if err := ValidateBytes([]byte(`{"version":"sb/1","id":"` + NewULID() + `","from":"a","to":"b","shape":"delegate","task":{"title":"T","instructions":"I"},"status":"pending"}`)); err != nil {
		t.Fatalf("valid bytes rejected: %v", err)
	}
	if err := ValidateBytes([]byte(`{invalid json`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
