package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/spool"
)

func TestClassifyLostWhenNoSessionAndNoProcess(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	got := h.m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 0, SessionID: ""})
	if got != lifecycle.VerdictLost {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictLost)
	}
}

func TestClassifyResumableWhenSessionSurvives(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	// 프로세스는 죽었지만 Provider 세션 ID가 남아 있으면 --resume이 가능하다.
	got := h.m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 999999, SessionID: "sess-1"})
	if got != lifecycle.VerdictResumable {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictResumable)
	}
}

func TestClassifyAliveWhenProcessStillRunning(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	// 이 테스트 프로세스 자신은 확실히 살아 있다.
	got := h.m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: os.Getpid(), SessionID: "sess-1"})
	if got != lifecycle.VerdictAlive {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictAlive)
	}
}

func TestReconcileReportsVerdictPerNonTerminalRun(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	if err := h.sp.SaveRun(spool.RunRecord{RunID: "run-lost", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := h.sp.SaveRun(spool.RunRecord{RunID: "run-done", State: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 종료되지 않은 Run 하나에 대해서만 상태 이벤트가 나가야 한다.
	if got := col.count(protocol.EvRunStateChanged); got != 1 {
		t.Fatalf("emitted %d state events, want 1 (only the non-terminal run)", got)
	}
}

func TestReconcileSalvagesLostRun(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)
	ctx := context.Background()

	ws, err := h.git.Ensure(ctx, "run-lost", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "wip.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sp.SaveRun(spool.RunRecord{
		RunID: "run-lost", State: "running", Branch: ws.Branch, WorktreePath: ws.Path, RepoPath: canonical(t, h.repo),
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := h.git.Status(ctx, "run-lost")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatal("lost run's uncommitted work was not salvaged")
	}
}

// TestReconcileSalvagesUsingRecordedRepo: 기록의 RepoPath가 두 번째 저장소를
// 가리키면 salvage도 그곳에서 일어난다. 첫 저장소로 가면 worktree가 없어
// 아무것도 보존되지 않는다.
func TestReconcileSalvagesUsingRecordedRepo(t *testing.T) {
	col := &collector{}
	second, _ := newRepo(t)
	h := newHarness(t, col, withRepos(second))
	ctx := context.Background()

	secondGit, err := h.repos.Manager(second)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := secondGit.Ensure(ctx, "run-lost", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "wip.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sp.SaveRun(spool.RunRecord{
		RunID: "run-lost", State: "running", Branch: ws.Branch, WorktreePath: ws.Path, RepoPath: canonical(t, second),
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := secondGit.Status(ctx, "run-lost")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatal("lost run in the second repo was not salvaged")
	}
	if col.count(protocol.EvFileChanged) != 1 {
		t.Fatalf("salvage event count = %d, want 1", col.count(protocol.EvFileChanged))
	}
}

// TestReconcileUsesFirstRepoForLegacyRecord: Phase 0 원장에는 RepoPath가 없다.
// 그 기록은 허용 목록의 첫 저장소로 해석한다.
func TestReconcileUsesFirstRepoForLegacyRecord(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)
	ctx := context.Background()

	ws, err := h.git.Ensure(ctx, "run-legacy", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "wip.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sp.SaveRun(spool.RunRecord{
		RunID: "run-legacy", State: "running", Branch: ws.Branch, WorktreePath: ws.Path, // RepoPath 없음
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := h.git.Status(ctx, "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatal("legacy record (no RepoPath) was not salvaged via the first allowed repo")
	}
}
