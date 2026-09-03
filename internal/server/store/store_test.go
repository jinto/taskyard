package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// ---- Phase 1 척추: 프로젝트·이슈·상태 반영 ----

func stateEvent(t *testing.T, runID string, seq uint64, state string) protocol.Envelope {
	t.Helper()
	// lifecycle.eventBody와 같은 껍데기: {"body": {...}, "raw": ...}
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, seq, map[string]any{
		"body": map[string]any{"state": state, "detail": ""},
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	env.Seq = seq
	return env
}

func seedProject(t *testing.T, s *Store, key string) Project {
	t.Helper()
	p, err := s.CreateProject(Project{Key: key, Name: key, RepoPath: "/repo/" + key, DefaultBranch: "main", ExecuteTemplate: "{{issue}}"})
	if err != nil {
		t.Fatalf("CreateProject(%s): %v", key, err)
	}
	return p
}

func seedTask(t *testing.T, s *Store, p Project, title string) Task {
	t.Helper()
	task, err := s.CreateTask(Task{ProjectID: p.ID, Title: title})
	if err != nil {
		t.Fatalf("CreateTask(%s): %v", title, err)
	}
	return task
}

func TestCreateProjectAssignsIDAndRejectsDuplicateKey(t *testing.T) {
	s := openTemp(t)

	p := seedProject(t, s, "shop")
	if p.ID == "" {
		t.Fatal("CreateProject returned empty ID")
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("CreateProject returned zero CreatedAt")
	}

	got, err := s.GetProject("shop")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ID != p.ID || got.RepoPath != "/repo/shop" || got.ExecuteTemplate != "{{issue}}" {
		t.Fatalf("GetProject = %+v, want %+v", got, p)
	}

	if _, err := s.CreateProject(Project{Key: "shop", Name: "again", RepoPath: "/x", DefaultBranch: "main"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key err = %v, want ErrDuplicateKey", err)
	}
	if _, err := s.GetProject("nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("missing project err = %v, want ErrProjectNotFound", err)
	}

	list, err := s.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Key != "shop" {
		t.Fatalf("ListProjects = %+v, want [shop]", list)
	}
}

func TestProjectAllowedToolsRoundTrip(t *testing.T) {
	s := openTemp(t)
	p, err := s.CreateProject(Project{Key: "shop", Name: "shop", RepoPath: "/r", DefaultBranch: "main", AllowedTools: []string{"Edit", "Bash(go test:*)"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject("shop")
	if len(got.AllowedTools) != 2 || got.AllowedTools[0] != "Edit" || got.AllowedTools[1] != "Bash(go test:*)" {
		t.Fatalf("AllowedTools = %q", got.AllowedTools)
	}
	if err := s.UpdateProjectSettings(p.Key, ProjectSettings{ExecuteTemplate: p.ExecuteTemplate, AllowedTools: []string{"Read"}, CreatePR: true, CleanupMerged: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProject("shop")
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != "Read" {
		t.Fatalf("after update AllowedTools = %q", got.AllowedTools)
	}
	if err := s.UpdateProjectSettings(p.Key, ProjectSettings{ExecuteTemplate: p.ExecuteTemplate}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProject("shop")
	if len(got.AllowedTools) != 0 {
		t.Fatalf("cleared AllowedTools = %q, want none", got.AllowedTools)
	}
	if err := s.UpdateProjectSettings("nope", ProjectSettings{ExecuteTemplate: "x"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestOpenMigratesProjectsAllowedTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// PR #3 시점의 projects 스키마(allowed_tools 없음).
	if _, err := db.Exec(`CREATE TABLE projects (
	  id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, name TEXT NOT NULL, repo_path TEXT NOT NULL,
	  default_branch TEXT NOT NULL, execute_template TEXT NOT NULL, created_at INTEGER NOT NULL);
	  INSERT INTO projects VALUES ('p1', 'shop', 'shop', '/r', 'main', 't', 1);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old projects schema: %v", err)
	}
	defer s.Close()
	p, err := s.GetProject("shop")
	if err != nil || len(p.AllowedTools) != 0 {
		t.Fatalf("GetProject = %+v, err = %v", p, err)
	}
	if err := s.UpdateProjectSettings("shop", ProjectSettings{ExecuteTemplate: "t", AllowedTools: []string{"Edit"}}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateProjectSettings(t *testing.T) {
	s := openTemp(t)
	seedProject(t, s, "shop")

	if err := s.UpdateProjectSettings("shop", ProjectSettings{ExecuteTemplate: "new {{issue}}", AllowedTools: []string{"Edit"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject("shop")
	if got.ExecuteTemplate != "new {{issue}}" || len(got.AllowedTools) != 1 || got.AllowedTools[0] != "Edit" {
		t.Fatalf("project = %+v", got)
	}
	if err := s.UpdateProjectSettings("nope", ProjectSettings{ExecuteTemplate: "x"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestCreateTaskNumbersPerProject(t *testing.T) {
	s := openTemp(t)
	shop := seedProject(t, s, "shop")
	blog := seedProject(t, s, "blog")

	var shopNums, blogNums []int
	for i := 0; i < 3; i++ {
		shopNums = append(shopNums, seedTask(t, s, shop, "s").Number)
		blogNums = append(blogNums, seedTask(t, s, blog, "b").Number)
	}
	for i, want := range []int{1, 2, 3} {
		if shopNums[i] != want || blogNums[i] != want {
			t.Fatalf("numbers shop=%v blog=%v, want 1,2,3 each", shopNums, blogNums)
		}
	}

	task, err := s.GetTask(shop.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskBacklog || task.Number != 2 || task.ProjectID != shop.ID {
		t.Fatalf("GetTask = %+v", task)
	}
	if _, err := s.GetTask(shop.ID, 99); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}

	list, err := s.ListTasks(shop.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Number != 3 || list[2].Number != 1 {
		t.Fatalf("ListTasks should be newest first, got %+v", list)
	}
}

func TestRunsForTaskOrdersNewestFirst(t *testing.T) {
	s := openTemp(t)
	p := seedProject(t, s, "shop")
	task := seedTask(t, s, p, "t")

	for _, id := range []string{"run-a", "run-b", "run-c"} {
		if err := s.UpsertRun(Run{ID: id, State: StateQueued, Kind: "structured", TaskID: task.ID, Stage: "execute"}); err != nil {
			t.Fatal(err)
		}
	}
	// 다른 이슈의 Run은 섞이지 않는다.
	if err := s.UpsertRun(Run{ID: "run-other", State: StateQueued, Kind: "structured", TaskID: "someone-else"}); err != nil {
		t.Fatal(err)
	}

	runs, err := s.RunsForTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != "run-c" || runs[2].ID != "run-a" {
		t.Fatalf("RunsForTask = %+v, want c,b,a", runs)
	}
	if runs[0].Stage != "execute" || runs[0].TaskID != task.ID || runs[0].CreatedAt.IsZero() {
		t.Fatalf("run fields not round-tripped: %+v", runs[0])
	}

	// 갱신은 created_at을 덮어쓰지 않는다.
	first := runs[2].CreatedAt
	if err := s.UpsertRun(Run{ID: "run-a", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun("run-a")
	if !got.CreatedAt.Equal(first) {
		t.Fatalf("UpsertRun overwrote created_at: %v -> %v", first, got.CreatedAt)
	}
}

func TestStateChangedEventUpdatesRunState(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateWaitingApproval)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun("run-1")
	if got.State != StateWaitingApproval {
		t.Fatalf("state = %q, want waiting_approval", got.State)
	}
}

func TestSucceededEventMovesTaskToReview(t *testing.T) {
	s := openTemp(t)
	p := seedProject(t, s, "shop")
	task := seedTask(t, s, p, "t")
	if err := s.UpdateTaskStatus(task.ID, TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRun(Run{ID: "run-1", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(p.ID, task.Number)
	if got.Status != TaskReview {
		t.Fatalf("task status = %q, want review", got.Status)
	}
}

func TestFailedEventLeavesTaskStatusAlone(t *testing.T) {
	s := openTemp(t)
	p := seedProject(t, s, "shop")
	task := seedTask(t, s, p, "t")
	_ = s.UpdateTaskStatus(task.ID, TaskInProgress)
	_ = s.UpsertRun(Run{ID: "run-1", State: StateRunning, Kind: "structured", TaskID: task.ID})

	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateFailed)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(p.ID, task.Number)
	if got.Status != TaskInProgress {
		t.Fatalf("task status = %q, want in_progress (failure does not move the issue)", got.Status)
	}
}

func TestSameSeqResendDoesNotChangeState(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 1, StateRunning))
	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 2, StateSucceeded))

	// seq 1(running)의 재전송. INSERT OR IGNORE로 걸러지고 상태는 그대로다.
	accepted, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateRunning))
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("resend reported accepted")
	}
	got, _ := s.GetRun("run-1")
	if got.State != StateSucceeded {
		t.Fatalf("state = %q, want succeeded", got.State)
	}
}

func TestNewerRunningEventDoesNotRegressSucceededState(t *testing.T) {
	s := openTemp(t)
	p := seedProject(t, s, "shop")
	task := seedTask(t, s, p, "t")
	_ = s.UpdateTaskStatus(task.ID, TaskInProgress)
	_ = s.UpsertRun(Run{ID: "run-1", State: StateRunning, Kind: "structured", TaskID: task.ID})

	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded))

	// Runner 재시작 후 Reconcile이 새 seq로 running을 보낸 경우. 이벤트는
	// 저장되지만(원장은 사실을 기록한다) 종결 상태는 되돌리지 않는다.
	accepted, ack, err := s.ApplyEvent(stateEvent(t, "run-1", 2, StateRunning))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || ack != 2 {
		t.Fatalf("newer event should be stored and acked: accepted=%v ack=%d", accepted, ack)
	}
	run, _ := s.GetRun("run-1")
	if run.State != StateSucceeded {
		t.Fatalf("run state regressed to %q", run.State)
	}
	tk, _ := s.GetTask(p.ID, task.Number)
	if tk.Status != TaskReview {
		t.Fatalf("task status regressed to %q", tk.Status)
	}
	events, _ := s.Events("run-1", 0, 10)
	if len(events) != 2 {
		t.Fatalf("stored %d events, want 2 (the running event must still be recorded)", len(events))
	}

	// 종결 → 종결은 허용한다. 마지막 사실이 이긴다.
	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 3, StateCancelled))
	run, _ = s.GetRun("run-1")
	if run.State != StateCancelled {
		t.Fatalf("terminal->terminal should apply, got %q", run.State)
	}
}

func TestUnknownStateInEventIsStoredButNotApplied(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	accepted, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, "bogus"))
	if err != nil || !accepted {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	got, _ := s.GetRun("run-1")
	if got.State != StateRunning {
		t.Fatalf("state = %q; an unknown state must not reach runs.state", got.State)
	}
}

func TestOpenMigratesExistingRunsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Phase 0 스키마 그대로 만든 DB.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE runs (
	  id TEXT PRIMARY KEY, state TEXT NOT NULL, kind TEXT NOT NULL,
	  provider_session_id TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
	  worktree_path TEXT NOT NULL DEFAULT '', last_acked_seq INTEGER NOT NULL DEFAULT 0,
	  reconcile_state TEXT NOT NULL DEFAULT '');
	  INSERT INTO runs (id, state, kind) VALUES ('run-old', 'succeeded', 'structured');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on old schema: %v", err)
	}
	defer s.Close()

	old, err := s.GetRun("run-old")
	if err != nil {
		t.Fatalf("GetRun after migration: %v", err)
	}
	if old.State != StateSucceeded || old.TaskID != "" || old.Stage != "" {
		t.Fatalf("old row after migration = %+v", old)
	}
	if err := s.UpsertRun(Run{ID: "run-new", State: StateQueued, Kind: "structured", TaskID: "t1", Stage: "execute"}); err != nil {
		t.Fatalf("UpsertRun with new columns: %v", err)
	}
}

// ---- 종료 방식: 정착 상태, 취소, 세션·detail ----

// stateEventWith는 stateEvent에 detail·session_id를 더한 종결 이벤트다.
func stateEventWith(t *testing.T, runID string, seq uint64, state, detail, sessionID string) protocol.Envelope {
	t.Helper()
	body := map[string]any{"state": state, "detail": detail}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, seq, map[string]any{"body": body})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	return env
}

func seedRunningTask(t *testing.T, s *Store) (Project, Task) {
	t.Helper()
	p := seedProject(t, s, "shop")
	task := seedTask(t, s, p, "t")
	if err := s.UpdateTaskStatus(task.ID, TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRun(Run{ID: "run-1", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute"}); err != nil {
		t.Fatal(err)
	}
	return p, task
}

func TestCancelledEventMovesTaskToBacklog(t *testing.T) {
	s := openTemp(t)
	p, task := seedRunningTask(t, s)

	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateCancelled)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(p.ID, task.Number)
	if got.Status != TaskBacklog {
		t.Fatalf("task status = %q, want backlog", got.Status)
	}
}

func TestNeedsAttentionIsSettled(t *testing.T) {
	s := openTemp(t)
	p, task := seedRunningTask(t, s)

	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 1, StateNeedsAttention, "CI가 반복 실패", ""))
	// Reconcile이 보낸 더 새로운 running은 정착 상태를 되돌리지 못한다.
	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 2, StateRunning))

	run, _ := s.GetRun("run-1")
	if run.State != StateNeedsAttention || run.Detail != "CI가 반복 실패" {
		t.Fatalf("run = %+v, want needs_attention with detail", run)
	}
	tk, _ := s.GetTask(p.ID, task.Number)
	if tk.Status != TaskInProgress {
		t.Fatalf("task status = %q, want in_progress (attention does not move the issue)", tk.Status)
	}
	if !IsSettled(StateNeedsAttention) || IsTerminal(StateNeedsAttention) {
		t.Fatal("needs_attention must be settled but not terminal")
	}
}

func TestStateEventRecordsDetailAndSessionID(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 1, StateSucceeded, "", "sess-1"))
	run, _ := s.GetRun("run-1")
	if run.ProviderSessionID != "sess-1" {
		t.Fatalf("provider_session_id = %q, want sess-1", run.ProviderSessionID)
	}

	// 종결 → 종결: detail은 최신 사실로 바뀐다.
	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 2, StateFailed, "나중에 실패", "sess-1"))
	run, _ = s.GetRun("run-1")
	if run.State != StateFailed || run.Detail != "나중에 실패" {
		t.Fatalf("run = %+v", run)
	}
}

func TestEmptySessionIDDoesNotClearKnownOne(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 1, StateRunning, "", "sess-1"))
	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 2, StateFailed, "boom", ""))
	run, _ := s.GetRun("run-1")
	if run.ProviderSessionID != "sess-1" {
		t.Fatalf("known session was cleared by an event without session_id: %+v", run)
	}
}

func TestSettledToSettledApplies(t *testing.T) {
	s := openTemp(t)
	p, task := seedRunningTask(t, s)

	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 1, StateNeedsAttention, "why", ""))
	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 2, StateCancelled))

	run, _ := s.GetRun("run-1")
	if run.State != StateCancelled {
		t.Fatalf("state = %q, want cancelled", run.State)
	}
	tk, _ := s.GetTask(p.ID, task.Number)
	if tk.Status != TaskBacklog {
		t.Fatalf("task status = %q, want backlog", tk.Status)
	}
}

func TestCancelSettledRunMovesNeedsAttentionToCancelled(t *testing.T) {
	s := openTemp(t)
	p, task := seedRunningTask(t, s)

	// running: 러너에게 물어야 한다. 서버가 직접 바꾸지 않는다.
	if err := s.CancelSettledRun("run-1"); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel running via store: err = %v, want ErrNotCancellable", err)
	}

	_, _, _ = s.ApplyEvent(stateEventWith(t, "run-1", 1, StateNeedsAttention, "why", "sess-1"))
	if err := s.CancelSettledRun("run-1"); err != nil {
		t.Fatalf("CancelSettledRun: %v", err)
	}
	run, _ := s.GetRun("run-1")
	if run.State != StateCancelled || run.ProviderSessionID != "sess-1" {
		t.Fatalf("run = %+v, want cancelled with session kept", run)
	}
	tk, _ := s.GetTask(p.ID, task.Number)
	if tk.Status != TaskBacklog {
		t.Fatalf("task status = %q, want backlog", tk.Status)
	}

	// 이미 종결된 Run은 취소할 것이 없다.
	if err := s.CancelSettledRun("run-1"); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel cancelled: err = %v, want ErrNotCancellable", err)
	}
	if err := s.CancelSettledRun("nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cancel unknown: err = %v, want ErrRunNotFound", err)
	}
}

func TestRunNewFieldsRoundTrip(t *testing.T) {
	s := openTemp(t)
	in := Run{
		ID: "run-2", State: StateQueued, Kind: "structured", TaskID: "t", Stage: "execute",
		Detail: "d", PreviousRunID: "run-1", WorkspaceRunID: "run-1", Feedback: "다시 해봐",
	}
	if err := s.UpsertRun(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun("run-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Detail != "d" || got.PreviousRunID != "run-1" || got.WorkspaceRunID != "run-1" || got.Feedback != "다시 해봐" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	runs, _ := s.RunsForTask("t")
	if len(runs) != 1 || runs[0].Feedback != "다시 해봐" {
		t.Fatalf("RunsForTask lost fields: %+v", runs)
	}
}

func TestOpenMigratesNewRunColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// PR #3 시점의 runs 스키마.
	if _, err := db.Exec(`CREATE TABLE runs (
	  id TEXT PRIMARY KEY, state TEXT NOT NULL, kind TEXT NOT NULL,
	  provider_session_id TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
	  worktree_path TEXT NOT NULL DEFAULT '', last_acked_seq INTEGER NOT NULL DEFAULT 0,
	  reconcile_state TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '',
	  stage TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0);
	  INSERT INTO runs (id, state, kind) VALUES ('run-old', 'succeeded', 'structured');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on PR #3 schema: %v", err)
	}
	defer s.Close()
	if _, err := s.GetRun("run-old"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRun(Run{ID: "run-new", State: StateQueued, Kind: "structured", PreviousRunID: "run-old", Feedback: "f"}); err != nil {
		t.Fatalf("UpsertRun with new columns: %v", err)
	}
}

func TestOpenIsIdempotentOnCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cur.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	_ = s2.Close()
}

// ---- PR 생성·추적 (계획 2026-09-03-phase1-pr) ----

func prEvent(t *testing.T, runID string, seq uint64, pr protocol.PRUpdatedBody) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvPRUpdated, runID, seq, map[string]any{"body": pr})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	return env
}

func TestProjectPolicyRoundTrip(t *testing.T) {
	s := openTemp(t)
	p, err := s.CreateProject(Project{Key: "shop", Name: "shop", RepoPath: "/r", DefaultBranch: "main", CreatePR: true, CleanupMerged: false})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject("shop")
	if !got.CreatePR || got.CleanupMerged {
		t.Fatalf("CreateProject lost policies: %+v", got)
	}
	if err := s.UpdateProjectSettings(p.Key, ProjectSettings{ExecuteTemplate: "t", CreatePR: false, CleanupMerged: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProject("shop")
	if got.CreatePR || !got.CleanupMerged || got.ExecuteTemplate != "t" {
		t.Fatalf("UpdateProjectSettings lost policies: %+v", got)
	}
}

func TestOpenMigratesProjectPolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr6.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// PR #6 시점의 projects 스키마(allowed_tools까지). 옛 행은 PRD 기본값 — 둘 다 켬.
	if _, err := db.Exec(`CREATE TABLE projects (
	  id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, name TEXT NOT NULL, repo_path TEXT NOT NULL,
	  default_branch TEXT NOT NULL, execute_template TEXT NOT NULL, created_at INTEGER NOT NULL,
	  allowed_tools TEXT NOT NULL DEFAULT '');
	  INSERT INTO projects VALUES ('p1', 'shop', 'shop', '/r', 'main', 't', 1, '');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on PR #6 schema: %v", err)
	}
	defer s.Close()
	p, err := s.GetProject("shop")
	if err != nil || !p.CreatePR || !p.CleanupMerged {
		t.Fatalf("migrated project = %+v, err = %v; want both policies on", p, err)
	}
}

func TestApplyPRUpdatedStoresAndMovesTaskToDone(t *testing.T) {
	s := openTemp(t)
	_, task := seedRunningTask(t, s)
	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded)); err != nil {
		t.Fatal(err)
	}

	open := protocol.PRUpdatedBody{URL: "https://github.com/o/r/pull/7", Number: 7, State: "OPEN", Checks: "pending"}
	if _, _, err := s.ApplyEvent(prEvent(t, "run-1", 2, open)); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun("run-1")
	if run.PRURL != open.URL || run.PRNumber != 7 || run.PRState != "OPEN" || run.PRChecks != "pending" {
		t.Fatalf("PR fields not stored: %+v", run)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskReview {
		t.Fatalf("task after OPEN = %s, want review", got.Status)
	}

	merged := open
	merged.State, merged.Checks, merged.Review, merged.WorktreeRemoved = "MERGED", "success", "APPROVED", true
	if _, _, err := s.ApplyEvent(prEvent(t, "run-1", 3, merged)); err != nil {
		t.Fatal(err)
	}
	run, _ = s.GetRun("run-1")
	if run.PRState != "MERGED" || run.PRReview != "APPROVED" || run.PRChecks != "success" {
		t.Fatalf("MERGED not stored: %+v", run)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskDone {
		t.Fatalf("task after MERGED = %s, want done", got.Status)
	}
}

func TestDoneDoesNotRegress(t *testing.T) {
	s := openTemp(t)
	_, task := seedRunningTask(t, s)
	_, _, _ = s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded))
	_, _, _ = s.ApplyEvent(prEvent(t, "run-1", 2, protocol.PRUpdatedBody{URL: "u", Number: 7, State: "MERGED"}))
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskDone {
		t.Fatalf("precondition: task = %s, want done", got.Status)
	}

	// 옛 Run의 늦은 succeeded는 done을 review로 되돌리지 못한다.
	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 3, StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskDone {
		t.Fatalf("late succeeded moved task to %s, want done kept", got.Status)
	}

	// 재시도로 새 Run이 최신이 되면, 옛 Run의 MERGED는 PR 필드만 저장하고 이슈는 건드리지 않는다.
	if err := s.UpdateTaskStatus(task.ID, TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRun(Run{ID: "run-2", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute", PreviousRunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRun(Run{ID: "run-old", State: StateSucceeded, Kind: "structured", TaskID: task.ID, Stage: "execute"}); err != nil {
		t.Fatal(err)
	}
	// run-old는 run-2보다 뒤에 INSERT됐지만 created_at을 과거로 둔다 — 최신은 여전히 run-2.
	if _, err := s.db.Exec(`UPDATE runs SET created_at = 1 WHERE id = 'run-old'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(prEvent(t, "run-old", 1, protocol.PRUpdatedBody{URL: "u2", Number: 8, State: "MERGED"})); err != nil {
		t.Fatal(err)
	}
	if old, _ := s.GetRun("run-old"); old.PRState != "MERGED" {
		t.Fatalf("PR fields not stored on non-latest run: %+v", old)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskInProgress {
		t.Fatalf("MERGED on non-latest run moved task to %s, want in_progress kept", got.Status)
	}
}

func TestApplyTerminalStoresSummary(t *testing.T) {
	s := openTemp(t)
	seedRunningTask(t, s)
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, "run-1", 1, map[string]any{
		"body": map[string]any{"state": StateSucceeded, "detail": "", "summary": "Shout를 추가했다"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = 1
	if _, _, err := s.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun("run-1")
	if run.Summary != "Shout를 추가했다" {
		t.Fatalf("summary = %q", run.Summary)
	}
}

// ---- 산출물과 1단계 (계획 2026-09-04-phase1-stages) ----

func artifactEvent(t *testing.T, runID string, seq uint64, name, content string, truncated bool) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvArtifactAdded, runID, seq, map[string]any{
		"body": protocol.ArtifactBody{Name: name, Content: content, Truncated: truncated},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	return env
}

func TestArtifactEventIsStoredAndListed(t *testing.T) {
	s := openTemp(t)
	seedRunningTask(t, s)
	for _, e := range []protocol.Envelope{
		artifactEvent(t, "run-1", 1, "notes.txt", "n", false),
		artifactEvent(t, "run-1", 2, "analysis.md", "# 분석", true),
		artifactEvent(t, "run-1", 3, "analysis.md", "다른 내용", false), // 재전송: 첫 내용 유지
	} {
		if _, _, err := s.ApplyEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Artifacts("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "analysis.md" || list[1].Name != "notes.txt" {
		t.Fatalf("Artifacts = %+v; want two, name order", list)
	}
	a, err := s.Artifact("run-1", "analysis.md")
	if err != nil || a.Content != "# 분석" || !a.Truncated {
		t.Fatalf("Artifact = %+v, err = %v", a, err)
	}
	if _, err := s.Artifact("run-1", "nope"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("err = %v, want ErrArtifactNotFound", err)
	}
}

func TestAnalyzeSucceededKeepsTaskInProgress(t *testing.T) {
	s := openTemp(t)
	_, task := seedRunningTask(t, s)
	if err := s.UpsertRun(Run{ID: "run-1", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: StageAnalyze}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskInProgress {
		t.Fatalf("analyze succeeded moved task to %s, want in_progress", got.Status)
	}
	// 2단계는 기존대로 review.
	if err := s.UpsertRun(Run{ID: "run-2", State: StateRunning, Kind: "structured", TaskID: task.ID, Stage: StageExecute}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(stateEvent(t, "run-2", 1, StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTaskByID(task.ID); got.Status != TaskReview {
		t.Fatalf("execute succeeded moved task to %s, want review", got.Status)
	}
}

func TestLatestSucceededRunByStage(t *testing.T) {
	s := openTemp(t)
	_, task := seedRunningTask(t, s)
	if _, ok, _ := s.LatestSucceededRun(task.ID, StageAnalyze); ok {
		t.Fatal("no analyze run yet, got ok")
	}
	for i, r := range []Run{
		{ID: "a1", State: StateSucceeded, Stage: StageAnalyze},
		{ID: "e1", State: StateFailed, Stage: StageExecute},
		{ID: "a2", State: StateSucceeded, Stage: StageAnalyze},
		{ID: "a3", State: StateNeedsAttention, Stage: StageAnalyze},
	} {
		r.Kind, r.TaskID, r.CreatedAt = "structured", task.ID, time.Unix(int64(100+i), 0)
		if err := s.UpsertRun(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.LatestSucceededRun(task.ID, StageAnalyze)
	if err != nil || !ok || got.ID != "a2" {
		t.Fatalf("LatestSucceededRun = %+v ok=%v err=%v; want a2", got, ok, err)
	}
}

func TestProjectAnalyzeSettingsRoundTripAndDefaults(t *testing.T) {
	s := openTemp(t)
	p, err := s.CreateProject(Project{Key: "shop", Name: "shop", RepoPath: "/r", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject("shop")
	if got.AnalyzeTemplate == "" || !strings.Contains(got.AnalyzeTemplate, "analysis.md") {
		t.Fatalf("empty analyze template should read as the default, got %q", got.AnalyzeTemplate)
	}
	if err := s.UpdateProjectSettings(p.Key, ProjectSettings{ExecuteTemplate: "t", AnalyzeTemplate: "분석 {{issue}}", AnalyzeEnabled: false, AnalyzeSkipBelow: 50}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProject("shop")
	if got.AnalyzeTemplate != "분석 {{issue}}" || got.AnalyzeEnabled || got.AnalyzeSkipBelow != 50 {
		t.Fatalf("settings lost: %+v", got)
	}
}

func TestOpenMigratesProjectAnalyzeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr7.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// PR #7 시점의 projects 스키마. 옛 행: 1단계 켬, 200자 미만 생략, 기본 템플릿.
	if _, err := db.Exec(`CREATE TABLE projects (
	  id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, name TEXT NOT NULL, repo_path TEXT NOT NULL,
	  default_branch TEXT NOT NULL, execute_template TEXT NOT NULL, created_at INTEGER NOT NULL,
	  allowed_tools TEXT NOT NULL DEFAULT '', create_pr INTEGER NOT NULL DEFAULT 1, cleanup_merged INTEGER NOT NULL DEFAULT 1);
	  INSERT INTO projects VALUES ('p1', 'shop', 'shop', '/r', 'main', 't', 1, '', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on PR #7 schema: %v", err)
	}
	defer s.Close()
	p, err := s.GetProject("shop")
	if err != nil || !p.AnalyzeEnabled || p.AnalyzeSkipBelow != 200 || !strings.Contains(p.AnalyzeTemplate, "analysis.md") {
		t.Fatalf("migrated project = %+v, err = %v", p, err)
	}
}

func TestCreateRunIfIdle(t *testing.T) {
	s := openTemp(t)
	_, task := seedRunningTask(t, s) // run-1 running
	err := s.CreateRunIfIdle(Run{ID: "run-2", State: StateQueued, Kind: "structured", TaskID: task.ID, Stage: StageExecute})
	if !errors.Is(err, ErrRunActive) {
		t.Fatalf("err = %v, want ErrRunActive", err)
	}
	if _, err := s.GetRun("run-2"); !errors.Is(err, ErrRunNotFound) {
		t.Fatal("run-2 must not exist after a refused create")
	}
	if _, _, err := s.ApplyEvent(stateEvent(t, "run-1", 1, StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRunIfIdle(Run{ID: "run-2", State: StateQueued, Kind: "structured", TaskID: task.ID, Stage: StageExecute, ReportRunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun("run-2"); got.ReportRunID != "run-1" {
		t.Fatalf("ReportRunID lost: %+v", got)
	}
}

func TestTasksAwaitingExecute(t *testing.T) {
	s := openTemp(t)
	p := seedProject(t, s, "shop")
	t1 := seedTask(t, s, p, "one")
	t2 := seedTask(t, s, p, "two")
	t3 := seedTask(t, s, p, "three")
	for i, r := range []Run{
		{ID: "t1-a", TaskID: t1.ID, State: StateSucceeded, Stage: StageAnalyze},
		{ID: "t2-a", TaskID: t2.ID, State: StateSucceeded, Stage: StageAnalyze},
		{ID: "t2-e", TaskID: t2.ID, State: StateRunning, Stage: StageExecute},
		{ID: "t3-a", TaskID: t3.ID, State: StateNeedsAttention, Stage: StageAnalyze},
	} {
		r.Kind, r.CreatedAt = "structured", time.Unix(int64(100+i), 0)
		if err := s.UpsertRun(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TasksAwaitingExecute()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Task.ID != t1.ID || got[0].AnalyzeRun.ID != "t1-a" {
		t.Fatalf("TasksAwaitingExecute = %+v; want only task one with t1-a", got)
	}
}
