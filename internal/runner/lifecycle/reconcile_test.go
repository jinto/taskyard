package lifecycle_test

import (
	"context"
	"os"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/spool"
)

func TestClassifyLostWhenNoSessionAndNoProcess(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 0, SessionID: ""})
	if got != lifecycle.VerdictLost {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictLost)
	}
}

func TestClassifyResumableWhenSessionSurvives(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	// 프로세스는 죽었지만 Provider 세션 ID가 남아 있으면 --resume이 가능하다.
	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 999999, SessionID: "sess-1"})
	if got != lifecycle.VerdictResumable {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictResumable)
	}
}

func TestClassifyAliveWhenProcessStillRunning(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	// 이 테스트 프로세스 자신은 확실히 살아 있다.
	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: os.Getpid(), SessionID: "sess-1"})
	if got != lifecycle.VerdictAlive {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictAlive)
	}
}

func TestReconcileReportsVerdictPerNonTerminalRun(t *testing.T) {
	col := &collector{}
	m, sp, _ := newManager(t, col)

	if err := sp.SaveRun(spool.RunRecord{RunID: "run-lost", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SaveRun(spool.RunRecord{RunID: "run-done", State: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 종료되지 않은 Run 하나에 대해서만 상태 이벤트가 나가야 한다.
	var reconciled int
	for _, e := range col.events {
		if e.Type == protocol.EvRunStateChanged {
			reconciled++
		}
	}
	if reconciled != 1 {
		t.Fatalf("emitted %d state events, want 1 (only the non-terminal run)", reconciled)
	}
}

func TestReconcileSalvagesLostRun(t *testing.T) {
	col := &collector{}
	m, sp, git := newManager(t, col)
	ctx := context.Background()

	ws, err := git.Ensure(ctx, "run-lost", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.Path+"/wip.txt", []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sp.SaveRun(spool.RunRecord{
		RunID: "run-lost", State: "running", Branch: ws.Branch, WorktreePath: ws.Path,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := git.Status(ctx, "run-lost")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatal("lost run's uncommitted work was not salvaged")
	}
}
