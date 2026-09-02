package spool

import (
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
)

func openTemp(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustEvent(t *testing.T, runID string) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvMessageDelta, runID, 0, map[string]string{"text": "x"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return env
}

func TestAppendIssuesMonotonicSequences(t *testing.T) {
	s := openTemp(t)
	for want := uint64(1); want <= 3; want++ {
		got, err := s.Append("run-1", mustEvent(t, "run-1"))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got != want {
			t.Fatalf("seq = %d, want %d", got, want)
		}
	}
}

func TestSequencesAreIndependentPerRun(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append("run-a", mustEvent(t, "run-a")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Append("run-b", mustEvent(t, "run-b"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("run-b first seq = %d, want 1", got)
	}
}

func TestSinceReturnsEventsAfterSequenceInOrder(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Append("run-1", mustEvent(t, "run-1")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Since("run-1", 2, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, env := range got {
		if want := uint64(i + 3); env.Seq != want {
			t.Errorf("got[%d].Seq = %d, want %d", i, env.Seq, want)
		}
	}
}

func TestAckDropsAcknowledgedEventsButKeepsCounter(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Append("run-1", mustEvent(t, "run-1")); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Ack("run-1", 2); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pending, err := s.Pending("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("Pending = %d, want 1", pending)
	}

	// Ack이 seq 발급 카운터를 되돌리면 안 된다.
	next, err := s.Append("run-1", mustEvent(t, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if next != 4 {
		t.Fatalf("next seq = %d, want 4 (counter must survive Ack)", next)
	}
}

func TestSequenceSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Append("run-1", mustEvent(t, "run-1")); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.Append("run-1", mustEvent(t, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("seq after reopen = %d, want 2", got)
	}
}

func TestRememberCommandIsIdempotent(t *testing.T) {
	s := openTemp(t)

	stored, first, err := s.RememberCommand("cmd-1", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("firstTime = false on first call")
	}
	if string(stored) != `{"ok":true}` {
		t.Fatalf("stored = %s", stored)
	}

	stored, first, err = s.RememberCommand("cmd-1", []byte(`{"ok":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("firstTime = true on repeat call")
	}
	if string(stored) != `{"ok":true}` {
		t.Fatalf("repeat returned %s, want the original result", stored)
	}
}

func TestActiveRunsListsRunsWithPendingEvents(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append("run-a", mustEvent(t, "run-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("run-b", mustEvent(t, "run-b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack("run-a", 1); err != nil {
		t.Fatal(err)
	}

	runs, err := s.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0] != "run-b" {
		t.Fatalf("ActiveRuns = %v, want [run-b]", runs)
	}
}

func TestSaveAndLoadRuns(t *testing.T) {
	s := openTemp(t)

	want := RunRecord{
		RunID:         "run-1",
		State:         "running",
		SessionID:     "sess-1",
		Branch:        "taskyard/run/run-1",
		WorktreePath:  "/tmp/wt/run-1",
		PID:           4242,
		StartedAtUnix: 1700000000,
	}
	if err := s.SaveRun(want); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got, err := s.LoadRuns()
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadRuns returned %d records, want 1", len(got))
	}
	if got[0] != want {
		t.Fatalf("record = %+v, want %+v", got[0], want)
	}
}

func TestSaveRunOverwritesByRunID(t *testing.T) {
	s := openTemp(t)

	if err := s.SaveRun(RunRecord{RunID: "run-1", State: "running", SessionID: ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(RunRecord{RunID: "run-1", State: "succeeded", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].State != "succeeded" || got[0].SessionID != "sess-1" {
		t.Fatalf("record = %+v, want the later values", got[0])
	}
}
