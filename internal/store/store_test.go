package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func testEnv() *envelope.Envelope {
	env := envelope.New("alice", "bob", envelope.ShapeDelegate)
	env.Task = envelope.Task{Title: "T", Instructions: "I"}
	return env
}

func TestWriteReadMoveList(t *testing.T) {
	st := testStore(t)
	env := testEnv()
	if err := st.Write(env); err != nil {
		t.Fatal(err)
	}
	got, err := st.Read(DirPending, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Title != "T" {
		t.Fatalf("wrong title: %q", got.Task.Title)
	}
	// Move pending -> claimed: old file gone, new file present.
	if err := st.Move(env, envelope.StatusClaimed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), DirPending, env.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("old pending file should be gone after move")
	}
	if _, _, err := st.ReadAny(env.ID); err != nil {
		t.Fatal(err)
	}
	list, err := st.List(DirClaimed)
	if err != nil || len(list) != 1 {
		t.Fatalf("list claimed = %d, err %v", len(list), err)
	}
	// Same-dir status update (claimed -> running) overwrites in place.
	env.Status = envelope.StatusRunning
	if err := st.Write(env); err != nil {
		t.Fatal(err)
	}
	list, _ = st.List(DirClaimed)
	if len(list) != 1 || list[0].Status != envelope.StatusRunning {
		t.Fatalf("running write failed: %+v", list)
	}
}

func TestReadAnyNotFound(t *testing.T) {
	st := testStore(t)
	if _, _, err := st.ReadAny("01AAAAAAAAAAAAAAAAAAAAAAAA"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestLogs(t *testing.T) {
	st := testStore(t)
	id := "01AAAAAAAAAAAAAAAAAAAAAAAA"
	if got, err := st.ReadLog(id); err != nil || got != "" {
		t.Fatalf("empty log expected, got %q err %v", got, err)
	}
	if err := st.AppendLog(id, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLog(id, "world"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadLog(id)
	if err != nil || got != "hello\nworld\n" {
		t.Fatalf("log mismatch: %q err %v", got, err)
	}
}

func TestListSkipsCorrupt(t *testing.T) {
	st := testStore(t)
	_ = os.WriteFile(filepath.Join(st.Root(), DirPending, "bad.json"), []byte("{nope"), 0o600)
	_ = st.Write(testEnv())
	list, err := st.List(DirPending)
	if err != nil || len(list) != 1 {
		t.Fatalf("corrupt file should be skipped, got %d err %v", len(list), err)
	}
}
