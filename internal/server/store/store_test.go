package store

import (
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func event(t *testing.T, runID string, seq uint64) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvMessageDelta, runID, seq, map[string]string{"text": "x"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	env.Seq = seq
	return env
}

func seedRun(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.UpsertRun(Run{ID: id, State: StateRunning, Kind: "structured"}); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
}

func TestApplyEventAdvancesAckOnContiguousSequences(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	for seq := uint64(1); seq <= 3; seq++ {
		accepted, ack, err := s.ApplyEvent(event(t, "run-1", seq))
		if err != nil {
			t.Fatalf("ApplyEvent(%d): %v", seq, err)
		}
		if !accepted {
			t.Fatalf("seq %d not accepted", seq)
		}
		if ack != seq {
			t.Fatalf("ack = %d, want %d", ack, seq)
		}
	}
}

func TestApplyEventIsIdempotent(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	accepted, ack, err := s.ApplyEvent(event(t, "run-1", 1))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if accepted {
		t.Error("replayed event reported as accepted; want false")
	}
	if ack != 1 {
		t.Errorf("ack = %d, want 1", ack)
	}

	got, err := s.Events("run-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d events, want 1 (duplicate must not be stored twice)", len(got))
	}
}

func TestApplyEventHoldsAckOnGapThenAdvances(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	// seq 2를 건너뛰고 3이 먼저 도착하면 ack은 1에 머문다.
	_, ack, err := s.ApplyEvent(event(t, "run-1", 3))
	if err != nil {
		t.Fatal(err)
	}
	if ack != 1 {
		t.Fatalf("ack = %d, want 1 while seq 2 is missing", ack)
	}

	// 빠진 2가 도착하면 3까지 한 번에 이어진다.
	_, ack, err = s.ApplyEvent(event(t, "run-1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if ack != 3 {
		t.Fatalf("ack = %d, want 3 after the gap is filled", ack)
	}
}

func TestResumePointsReportsLastAckedPerRun(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-a")
	seedRun(t, s, "run-b")

	if _, _, err := s.ApplyEvent(event(t, "run-a", 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(event(t, "run-a", 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(event(t, "run-b", 1)); err != nil {
		t.Fatal(err)
	}

	points, err := s.ResumePoints()
	if err != nil {
		t.Fatal(err)
	}
	if points["run-a"] != 2 {
		t.Errorf("run-a = %d, want 2", points["run-a"])
	}
	if points["run-b"] != 1 {
		t.Errorf("run-b = %d, want 1", points["run-b"])
	}
}

func TestUpsertRunPreservesLastAckedSeq(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")
	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	// 상태만 바꾸는 갱신이 ack 커서를 되돌리면 재전송이 무한 반복된다.
	if err := s.UpsertRun(Run{ID: "run-1", State: StateSucceeded, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAckedSeq != 1 {
		t.Fatalf("LastAckedSeq = %d, want 1", got.LastAckedSeq)
	}
	if got.State != StateSucceeded {
		t.Fatalf("State = %q, want %q", got.State, StateSucceeded)
	}
}
