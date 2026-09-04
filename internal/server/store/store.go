// Package store는 Server의 Run 원장과 이벤트 저장소다.
//
// Runner는 at-least-once로 이벤트를 보내므로 여기서 멱등하게 적용한다.
// ack 커서는 "빠짐없이 연속으로 받은 마지막 seq"이며, 중간이 비면
// 앞으로 나아가지 않는다. PRD §11.5와 §15.3을 따른다.
//
// Phase 1부터는 프로젝트와 이슈(Task)도 여기 산다. Run은 이슈의 한 단계를
// 실행한 것이고(PRD §6.2), run.state_changed 이벤트가 runs.state와
// tasks.status를 움직인다.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/jinto/taskyard/internal/server/pipeline"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/sqlitex"
)

// Run 상태. PRD §9.3의 부분집합으로, 지금 필요한 것만 둔다.
const (
	StateQueued          = "queued"
	StateRunning         = "running"
	StateWaitingApproval = "waiting_approval"
	StateOrphaned        = "orphaned"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateCancelled       = "cancelled"
	// StateNeedsAttention은 에이전트가 스스로 멈추고 이유를 남긴 상태다(PRD §7.5).
	// 종결은 아니지만 정착 상태다 — 사람이 취소·재시도로만 벗어난다.
	StateNeedsAttention = "needs_attention"
)

// Task(이슈) 상태. 척추에 필요한 셋만. PRD §9.1의 부분집합.
const (
	TaskBacklog    = "backlog"
	TaskInProgress = "in_progress"
	TaskReview     = "review"
	// TaskDone은 PR merge가 확인된 이슈다(PRD §7.6). 나가는 길은 [실행]·재시도뿐.
	TaskDone = "done"
)

// Run의 단계(PRD §7.2). 1단계는 보고서만 남기고, 2단계가 구현한다.
const (
	StageAnalyze = "analyze"
	StageExecute = "execute"
)

// terminalStates는 끝난 Run 상태다. settledStates는 거기에 needs_attention을
// 더한 "되돌리지 않는" 상태다. 정착 → 비정착 전이는 무시한다: Runner 재시작
// 후 Reconcile이 새 seq로 running을 보낼 수 있는데, 그것이 이미 저장된
// succeeded나 needs_attention을 덮어쓰면 안 된다.
var terminalStates = map[string]bool{
	StateSucceeded: true,
	StateFailed:    true,
	StateCancelled: true,
}

var settledStates = map[string]bool{
	StateSucceeded: true, StateFailed: true, StateCancelled: true, StateNeedsAttention: true,
}

// knownStates는 이벤트가 runs.state에 쓸 수 있는 값의 전부다. 그 밖의
// 문자열은 저장은 되되 상태에는 닿지 않는다.
var knownStates = map[string]bool{
	StateQueued: true, StateRunning: true, StateWaitingApproval: true, StateOrphaned: true,
	StateSucceeded: true, StateFailed: true, StateCancelled: true, StateNeedsAttention: true,
}

// IsTerminal은 Run이 끝났는지를 말한다.
func IsTerminal(state string) bool { return terminalStates[state] }

// IsSettled는 Run이 사람의 행동 없이는 더 움직이지 않는지를 말한다. 웹의
// "이미 실행 중" 판정과 재시도 가능 판정이 쓴다.
func IsSettled(state string) bool { return settledStates[state] }

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id                  TEXT    PRIMARY KEY,
  state               TEXT    NOT NULL,
  kind                TEXT    NOT NULL,
  provider_session_id TEXT    NOT NULL DEFAULT '',
  branch              TEXT    NOT NULL DEFAULT '',
  worktree_path       TEXT    NOT NULL DEFAULT '',
  last_acked_seq      INTEGER NOT NULL DEFAULT 0,
  reconcile_state     TEXT    NOT NULL DEFAULT '',
  task_id             TEXT    NOT NULL DEFAULT '',
  stage               TEXT    NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL DEFAULT 0,
  detail              TEXT    NOT NULL DEFAULT '',
  previous_run_id     TEXT    NOT NULL DEFAULT '',
  workspace_run_id    TEXT    NOT NULL DEFAULT '',
  feedback            TEXT    NOT NULL DEFAULT '',
  summary             TEXT    NOT NULL DEFAULT '',
  pr_url              TEXT    NOT NULL DEFAULT '',
  pr_number           INTEGER NOT NULL DEFAULT 0,
  pr_state            TEXT    NOT NULL DEFAULT '',
  pr_checks           TEXT    NOT NULL DEFAULT '',
  pr_review           TEXT    NOT NULL DEFAULT '',
  report_run_id       TEXT    NOT NULL DEFAULT '',
  pending_request_id  TEXT    NOT NULL DEFAULT '',
  pending_tool_use_id TEXT    NOT NULL DEFAULT '',
  pending_approval    TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS artifacts (
  run_id     TEXT    NOT NULL,
  name       TEXT    NOT NULL,
  content    TEXT    NOT NULL,
  truncated  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (run_id, name)
);

CREATE TABLE IF NOT EXISTS run_events (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  type    TEXT    NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS projects (
  id               TEXT    PRIMARY KEY,
  key              TEXT    NOT NULL UNIQUE,
  name             TEXT    NOT NULL,
  repo_path        TEXT    NOT NULL,
  default_branch   TEXT    NOT NULL,
  execute_template TEXT    NOT NULL,
  created_at       INTEGER NOT NULL,
  allowed_tools    TEXT    NOT NULL DEFAULT '',
  create_pr        INTEGER NOT NULL DEFAULT 1,
  cleanup_merged   INTEGER NOT NULL DEFAULT 1,
  analyze_template TEXT    NOT NULL DEFAULT '',
  analyze_enabled  INTEGER NOT NULL DEFAULT 1,
  analyze_skip_below INTEGER NOT NULL DEFAULT 200
);

CREATE TABLE IF NOT EXISTS tasks (
  id         TEXT    PRIMARY KEY,
  project_id TEXT    NOT NULL REFERENCES projects(id),
  number     INTEGER NOT NULL,
  title      TEXT    NOT NULL,
  body       TEXT    NOT NULL DEFAULT '',
  status     TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (project_id, number)
);
`

// runsMigrations는 Phase 0 DB의 runs 테이블에 뒤늦게 추가된 컬럼이다.
var runsMigrations = []sqlitex.Column{
	{Name: "task_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "stage", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "created_at", DDL: "INTEGER NOT NULL DEFAULT 0"},
	{Name: "detail", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "previous_run_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "workspace_run_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "feedback", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "summary", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pr_url", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pr_number", DDL: "INTEGER NOT NULL DEFAULT 0"},
	{Name: "pr_state", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pr_checks", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pr_review", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "report_run_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pending_request_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pending_tool_use_id", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "pending_approval", DDL: "TEXT NOT NULL DEFAULT ''"},
}

// projectsMigrations는 PR #3 이후 projects 테이블에 추가된 컬럼이다.
var projectsMigrations = []sqlitex.Column{
	{Name: "allowed_tools", DDL: "TEXT NOT NULL DEFAULT ''"},
	// 옛 행의 기본값은 PRD 기본 — PR 만들기 켬(§7.2), merge 후 삭제(§8.7.1).
	{Name: "create_pr", DDL: "INTEGER NOT NULL DEFAULT 1"},
	{Name: "cleanup_merged", DDL: "INTEGER NOT NULL DEFAULT 1"},
	// 1단계: 옛 행은 켬, 200자 미만 생략, 템플릿은 비어 있으면 읽을 때 기본값.
	{Name: "analyze_template", DDL: "TEXT NOT NULL DEFAULT ''"},
	{Name: "analyze_enabled", DDL: "INTEGER NOT NULL DEFAULT 1"},
	{Name: "analyze_skip_below", DDL: "INTEGER NOT NULL DEFAULT 200"},
}

var (
	// ErrRunNotFound는 알 수 없는 Run을 조회했을 때 반환한다.
	ErrRunNotFound = errors.New("run not found")
	// ErrArtifactNotFound는 Run에 그 이름의 산출물이 없을 때.
	ErrArtifactNotFound = errors.New("artifact not found")
	// ErrRunActive는 이슈에 정착하지 않은 Run이 있어 새 Run을 만들 수 없을 때.
	ErrRunActive = errors.New("a run is still active for this task")
	// ErrNotCancellable은 서버가 직접 취소할 수 없는 상태의 Run이다 — 활성이면
	// 러너에게 물어야 하고, 종결이면 취소할 것이 없다.
	ErrNotCancellable = errors.New("run is not cancellable from the server")
	// ErrProjectNotFound는 알 수 없는 프로젝트 key를 조회했을 때 반환한다.
	ErrProjectNotFound = errors.New("project not found")
	// ErrTaskNotFound는 프로젝트 안에 그 번호의 이슈가 없을 때 반환한다.
	ErrTaskNotFound = errors.New("task not found")
	// ErrDuplicateKey는 이미 쓰이는 프로젝트 key로 만들려 할 때 반환한다.
	ErrDuplicateKey = errors.New("project key already exists")
)

// Run은 Server가 보는 실행 한 건이다. TaskID가 비어 있으면 이슈 없이 만들어진
// Phase 0 시절의 Run이다.
type Run struct {
	ID                string
	State             string
	Kind              string
	ProviderSessionID string
	Branch            string
	WorktreePath      string
	LastAckedSeq      uint64
	ReconcileState    string
	TaskID            string
	Stage             string
	CreatedAt         time.Time
	// Detail은 마지막 상태 이벤트의 설명이다 — 실패 이유 또는 멈춤 보고의 내용.
	Detail string
	// PreviousRunID·Feedback은 재시도로 만들어진 Run에만 있다(PRD §7.6).
	PreviousRunID string
	Feedback      string
	// WorkspaceRunID는 이 Run이 쓰는 worktree·브랜치의 주인 Run이다. 이어서
	// 재시도는 이전 Run의 것을 쓴다. 비어 있으면 자기 자신.
	WorkspaceRunID string
	// Summary는 에이전트가 남긴 변경 설명(.taskyard/summary.md)이다. 종결
	// 이벤트에 실려 온다. PR 필드는 pr.updated로만 채워진다 — UpsertRun은
	// 건드리지 않는다.
	Summary  string
	PRURL    string
	PRNumber int
	PRState  string
	PRChecks string
	PRReview string
	// ReportRunID는 2단계 Run이 {{stage1_report}}로 쓴 1단계 Run이다. 비어 있으면
	// 보고서 없이 시작했다.
	ReportRunID string
}

// Artifact는 에이전트가 .taskyard/artifacts/ 에 남긴 파일 하나다(ST-06).
type Artifact struct {
	RunID     string
	Name      string
	Content   string
	Truncated bool
	CreatedAt time.Time
}

// Project는 저장소 하나와 이슈 보드, 실행 템플릿을 가진 단위다(PRD §6.1).
// RepoPath는 Runner 머신의 절대 경로이며 Server는 그 존재를 검증하지 않는다 —
// Runner가 허용 목록으로 검증한다(RN-03).
type Project struct {
	ID              string
	Key             string
	Name            string
	RepoPath        string
	DefaultBranch   string
	ExecuteTemplate string
	CreatedAt       time.Time
	// AllowedTools는 승인 없이 통과시킬 도구 패턴이다(PRD §11.6.3). 줄 단위로
	// 저장한다.
	AllowedTools []string
	// CreatePR: 성공한 Run의 브랜치를 push하고 PR을 만든다(GH-05). 원격이 없는
	// 저장소는 끈다. CleanupMerged: merge 확인 후 worktree를 지운다(GH-10).
	CreatePR      bool
	CleanupMerged bool
	// 1단계(분석·설계, PRD §7.2). AnalyzeTemplate은 저장이 비어 있으면 읽을 때
	// 기본 템플릿으로 채워진다 — 옛 프로젝트도 1단계를 얻는다. AnalyzeSkipBelow는
	// 이슈 본문의 rune 수가 이보다 작으면 1단계를 건너뛰는 기준(0이면 안 건너뜀).
	AnalyzeTemplate  string
	AnalyzeEnabled   bool
	AnalyzeSkipBelow int
}

// ProjectSettings는 설정 폼이 한 번에 저장하는 필드들이다.
type ProjectSettings struct {
	RepoPath         string
	DefaultBranch    string
	ExecuteTemplate  string
	AllowedTools     []string
	CreatePR         bool
	CleanupMerged    bool
	AnalyzeTemplate  string
	AnalyzeEnabled   bool
	AnalyzeSkipBelow int
}

func joinTools(tools []string) string { return strings.Join(tools, "\n") }

func splitTools(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// Task는 이슈다. Number는 프로젝트 안에서 1부터 증가한다.
type Task struct {
	ID        string
	ProjectID string
	Number    int
	Title     string
	Body      string
	Status    string
	CreatedAt time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open server db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create server schema: %w", err)
	}
	if err := sqlitex.AddMissingColumns(db, "runs", runsMigrations); err != nil {
		db.Close()
		return nil, err
	}
	if err := sqlitex.AddMissingColumns(db, "projects", projectsMigrations); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---- Run ----

// UpsertRun은 Run을 만들거나 갱신한다. last_acked_seq는 이벤트 적용만이
// 움직이고 created_at은 처음 만들 때만 정해지므로, 둘 다 여기서 절대
// 덮어쓰지 않는다.
func (s *Store) UpsertRun(r Run) error {
	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.Exec(
		`INSERT INTO runs (id, state, kind, provider_session_id, branch, worktree_path, reconcile_state, task_id, stage, created_at,
		                   detail, previous_run_id, workspace_run_id, feedback, report_run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   state               = excluded.state,
		   kind                = excluded.kind,
		   provider_session_id = excluded.provider_session_id,
		   branch              = excluded.branch,
		   worktree_path       = excluded.worktree_path,
		   reconcile_state     = excluded.reconcile_state,
		   task_id             = excluded.task_id,
		   stage               = excluded.stage,
		   detail              = excluded.detail,
		   previous_run_id     = excluded.previous_run_id,
		   workspace_run_id    = excluded.workspace_run_id,
		   feedback            = excluded.feedback,
		   report_run_id       = excluded.report_run_id`,
		r.ID, r.State, r.Kind, r.ProviderSessionID, r.Branch, r.WorktreePath, r.ReconcileState,
		r.TaskID, r.Stage, created.UnixNano(),
		r.Detail, r.PreviousRunID, r.WorkspaceRunID, r.Feedback, r.ReportRunID,
	)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}

// CreateRunIfIdle은 이슈에 정착하지 않은 Run이 없을 때만 새 Run을 만든다 —
// 검사와 삽입이 한 트랜잭션이라(연결이 하나라 직렬) 이어 실행 goroutine과
// 사람의 [실행]이 겹쳐도 하나만 생긴다. 있으면 ErrRunActive.
func (s *Store) CreateRunIfIdle(r Run) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT state FROM runs WHERE task_id = ?`, r.TaskID)
	if err != nil {
		return fmt.Errorf("query task runs: %w", err)
	}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			rows.Close()
			return fmt.Errorf("scan run state: %w", err)
		}
		if !IsSettled(state) {
			rows.Close()
			return ErrRunActive
		}
	}
	rows.Close()

	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := tx.Exec(
		`INSERT INTO runs (id, state, kind, task_id, stage, created_at, previous_run_id, workspace_run_id, feedback, report_run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.State, r.Kind, r.TaskID, r.Stage, created.UnixNano(), r.PreviousRunID, r.WorkspaceRunID, r.Feedback, r.ReportRunID,
	); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return tx.Commit()
}

const runColumns = `id, state, kind, provider_session_id, branch, worktree_path, last_acked_seq, reconcile_state, task_id, stage, created_at,
                    detail, previous_run_id, workspace_run_id, feedback, summary, pr_url, pr_number, pr_state, pr_checks, pr_review, report_run_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanRun(sc rowScanner) (Run, error) {
	var (
		r       Run
		created int64
	)
	err := sc.Scan(&r.ID, &r.State, &r.Kind, &r.ProviderSessionID, &r.Branch, &r.WorktreePath,
		&r.LastAckedSeq, &r.ReconcileState, &r.TaskID, &r.Stage, &created,
		&r.Detail, &r.PreviousRunID, &r.WorkspaceRunID, &r.Feedback,
		&r.Summary, &r.PRURL, &r.PRNumber, &r.PRState, &r.PRChecks, &r.PRReview, &r.ReportRunID)
	if err != nil {
		return Run{}, err
	}
	if created != 0 {
		r.CreatedAt = time.Unix(0, created)
	}
	return r, nil
}

func (s *Store) GetRun(id string) (Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// RunsForTask는 이슈의 Run을 최신순으로 돌려준다. 재시도가 Run을 하나 더
// 다는 모델(PRD §6.2)이라 목록의 첫 줄이 "지금 상태"다.
func (s *Store) RunsForTask(taskID string) ([]Run, error) {
	rows, err := s.db.Query(`SELECT `+runColumns+` FROM runs WHERE task_id = ? ORDER BY created_at DESC, rowid DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query runs for task: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyEvent는 이벤트를 저장하고 ack 커서를 가능한 만큼 전진시킨다.
// accepted는 이번 호출로 새로 저장됐는지를 뜻한다. 이미 있던 seq면 false다.
//
// run.state_changed를 새로 저장했다면 같은 트랜잭션에서 runs.state를 갱신하고,
// succeeded면 이슈를 review로 옮긴다. 종결 상태는 비종결로 되돌리지 않는다.
func (s *Store) ApplyEvent(env protocol.Envelope) (bool, uint64, error) {
	if env.RunID == "" || env.Seq == 0 {
		return false, 0, fmt.Errorf("event needs run_id and seq, got run_id=%q seq=%d", env.RunID, env.Seq)
	}

	payload, err := json.Marshal(env)
	if err != nil {
		return false, 0, fmt.Errorf("marshal envelope: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO run_events (run_id, seq, type, payload) VALUES (?, ?, ?, ?)`,
		env.RunID, env.Seq, env.Type, payload,
	)
	if err != nil {
		return false, 0, fmt.Errorf("insert event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("rows affected: %w", err)
	}

	var (
		acked   uint64
		current string
		taskID  string
		stage   string
	)
	err = tx.QueryRow(`SELECT last_acked_seq, state, task_id, stage FROM runs WHERE id = ?`, env.RunID).Scan(&acked, &current, &taskID, &stage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, ErrRunNotFound
	}
	if err != nil {
		return false, 0, fmt.Errorf("read ack cursor: %w", err)
	}

	// 빈틈이 없는 동안만 커서를 전진시킨다.
	for {
		var exists int
		err := tx.QueryRow(
			`SELECT 1 FROM run_events WHERE run_id = ? AND seq = ?`, env.RunID, acked+1,
		).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return false, 0, fmt.Errorf("probe next seq: %w", err)
		}
		acked++
	}

	if _, err := tx.Exec(`UPDATE runs SET last_acked_seq = ? WHERE id = ?`, acked, env.RunID); err != nil {
		return false, 0, fmt.Errorf("update ack cursor: %w", err)
	}

	if affected > 0 {
		switch env.Type {
		case protocol.EvRunStateChanged:
			if err := applyStateChange(tx, env, current, stage, taskID); err != nil {
				return false, 0, err
			}
		case protocol.EvPRUpdated:
			if err := applyPRUpdate(tx, env, taskID); err != nil {
				return false, 0, err
			}
		case protocol.EvArtifactAdded:
			if err := applyArtifact(tx, env); err != nil {
				return false, 0, err
			}
		case protocol.EvApprovalRequested:
			if err := applyApprovalRequested(tx, env); err != nil {
				return false, 0, err
			}
		case protocol.EvToolFinished:
			if err := clearPendingForTool(tx, env); err != nil {
				return false, 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}
	return affected > 0, acked, nil
}

// applyStateChange는 run.state_changed의 상태를 runs와 tasks에 반영한다.
// 이벤트 body는 lifecycle.eventBody 모양({"body":{"state":…,"detail":…,"session_id":…}})이다.
//
// 이슈 상태 전이(계획의 행렬): succeeded → review, cancelled → backlog,
// failed·needs_attention → 그대로.
func applyStateChange(tx *sql.Tx, env protocol.Envelope, current, stage, taskID string) error {
	var outer struct {
		Body struct {
			State     string `json:"state"`
			Detail    string `json:"detail"`
			SessionID string `json:"session_id"`
			Summary   string `json:"summary"`
			// BeforeStart는 에이전트가 시작조차 못 한 실패다(허용되지 않은
			// 저장소, 러너 바쁨 등). 작업대에 아무것도 없으므로 이슈는
			// backlog로 돌아간다 — 재시도가 아니라 다시 시작이다.
			BeforeStart bool `json:"before_start"`
		} `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body.State == "" {
		// 모양이 다른 이벤트는 저장만 하고 상태는 건드리지 않는다.
		return nil
	}
	next := outer.Body.State

	if !knownStates[next] || (settledStates[current] && !settledStates[next]) {
		return nil
	}
	if _, err := tx.Exec(`UPDATE runs SET state = ?, detail = ? WHERE id = ?`, next, outer.Body.Detail, env.RunID); err != nil {
		return fmt.Errorf("update run state: %w", err)
	}
	if outer.Body.Summary != "" {
		if _, err := tx.Exec(`UPDATE runs SET summary = ? WHERE id = ?`, outer.Body.Summary, env.RunID); err != nil {
			return fmt.Errorf("update run summary: %w", err)
		}
	}
	// 알던 세션을 빈 값으로 지우지 않는다 — 이어서 재시도가 이 값에 기댄다.
	if outer.Body.SessionID != "" {
		if _, err := tx.Exec(`UPDATE runs SET provider_session_id = ? WHERE id = ?`, outer.Body.SessionID, env.RunID); err != nil {
			return fmt.Errorf("update run session: %w", err)
		}
	}
	if IsSettled(next) {
		if err := clearPending(tx, env.RunID); err != nil {
			return err
		}
	}
	if outer.Body.BeforeStart && next == StateFailed {
		return moveTaskFor(tx, StateCancelled, stage, taskID) // cancelled와 같은 자리 — backlog
	}
	return moveTaskFor(tx, next, stage, taskID)
}

// applyApprovalRequested는 대기 중인 승인을 runs에 표시한다. 첫 화면이 이걸
// 보고 "어디서 무엇이 사람을 기다리는지"를 알린다 — 사람이 Run 화면을 열어 두고
// 있지 않으면 승인은 약 5~6분 뒤 실패한다(관측 11번).
func applyApprovalRequested(tx *sql.Tx, env protocol.Envelope) error {
	var outer struct {
		Body struct {
			RequestID string         `json:"request_id"`
			ToolUseID string         `json:"tool_use_id"`
			ToolName  string         `json:"tool_name"`
			Input     map[string]any `json:"input"`
		} `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body.RequestID == "" {
		return nil
	}
	summary, _ := outer.Body.Input["command"].(string)
	if summary == "" {
		summary = outer.Body.ToolName
	}
	if _, err := tx.Exec(
		`UPDATE runs SET pending_request_id = ?, pending_tool_use_id = ?, pending_approval = ? WHERE id = ?`,
		outer.Body.RequestID, outer.Body.ToolUseID, summary, env.RunID,
	); err != nil {
		return fmt.Errorf("mark pending approval: %w", err)
	}
	return nil
}

// clearPendingForTool은 그 도구 호출이 끝나면 대기 표시를 지운다. 승인·거절만이
// 아니라 타임아웃도 여기로 온다 — 어느 쪽이든 사람이 더 할 일은 없다.
func clearPendingForTool(tx *sql.Tx, env protocol.Envelope) error {
	var outer struct {
		Body struct {
			ToolUseID string `json:"tool_use_id"`
		} `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body.ToolUseID == "" {
		return nil
	}
	if _, err := tx.Exec(
		`UPDATE runs SET pending_request_id = '', pending_tool_use_id = '', pending_approval = ''
		 WHERE id = ? AND pending_tool_use_id = ?`, env.RunID, outer.Body.ToolUseID,
	); err != nil {
		return fmt.Errorf("clear pending approval: %w", err)
	}
	return nil
}

func clearPending(tx *sql.Tx, runID string) error {
	if _, err := tx.Exec(
		`UPDATE runs SET pending_request_id = '', pending_tool_use_id = '', pending_approval = '' WHERE id = ?`, runID,
	); err != nil {
		return fmt.Errorf("clear pending approval: %w", err)
	}
	return nil
}

// applyArtifact는 artifact.added를 artifacts에 저장한다. 같은 (run, name)의
// 재전송은 무시한다 — 첫 내용이 정본이다.
func applyArtifact(tx *sql.Tx, env protocol.Envelope) error {
	var outer struct {
		Body protocol.ArtifactBody `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body.Name == "" {
		return nil
	}
	a := outer.Body
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO artifacts (run_id, name, content, truncated, created_at) VALUES (?, ?, ?, ?, ?)`,
		env.RunID, a.Name, a.Content, a.Truncated, time.Now().UnixNano(),
	); err != nil {
		return fmt.Errorf("insert artifact: %w", err)
	}
	return nil
}

// applyPRUpdate는 pr.updated를 runs의 PR 필드에 반영하고, MERGED이면 이슈를
// done으로 옮긴다 — 단, 이 Run이 이슈의 최신 Run일 때만. 재시도가 진행 중인
// 이슈를 옛 PR이 끝내면 안 된다(계획 "서버 반영").
func applyPRUpdate(tx *sql.Tx, env protocol.Envelope, taskID string) error {
	var outer struct {
		Body protocol.PRUpdatedBody `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body.Number == 0 {
		return nil
	}
	pr := outer.Body
	if _, err := tx.Exec(
		`UPDATE runs SET pr_url = ?, pr_number = ?, pr_state = ?, pr_checks = ?, pr_review = ? WHERE id = ?`,
		pr.URL, pr.Number, pr.State, pr.Checks, pr.Review, env.RunID,
	); err != nil {
		return fmt.Errorf("update run pr: %w", err)
	}
	if pr.State != "MERGED" || taskID == "" {
		return nil
	}
	var latest string
	if err := tx.QueryRow(
		`SELECT id FROM runs WHERE task_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, taskID,
	).Scan(&latest); err != nil {
		return fmt.Errorf("find latest run: %w", err)
	}
	if latest != env.RunID {
		return nil
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, TaskDone, taskID); err != nil {
		return fmt.Errorf("move task to done: %w", err)
	}
	return nil
}

// moveTaskFor는 Run 상태에 따른 이슈 상태 전이다. done은 건드리지 않는다 —
// 옛 Run의 늦은 succeeded가 merge된 이슈를 review로 되돌리면 안 된다.
// 1단계의 succeeded는 review가 아니다 — 2단계가 이어진다(in_progress 유지).
func moveTaskFor(tx *sql.Tx, runState, stage, taskID string) error {
	if taskID == "" {
		return nil
	}
	var status string
	switch runState {
	case StateSucceeded:
		if stage == StageAnalyze {
			return nil
		}
		status = TaskReview
	case StateCancelled:
		status = TaskBacklog
	default:
		return nil
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = ? WHERE id = ? AND status != ?`, status, taskID, TaskDone); err != nil {
		return fmt.Errorf("move task to %s: %w", status, err)
	}
	return nil
}

// CancelSettledRun은 프로세스가 없는 needs_attention Run을 서버가 직접
// cancelled로 바꾸고 이슈를 backlog로 옮긴다. 활성 Run은 러너에게
// run.cancel을 보내야 하고, 종결 Run은 취소할 것이 없다 — 둘 다
// ErrNotCancellable.
func (s *Store) CancelSettledRun(runID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var state, taskID string
	err = tx.QueryRow(`SELECT state, task_id FROM runs WHERE id = ?`, runID).Scan(&state, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRunNotFound
	}
	if err != nil {
		return fmt.Errorf("read run: %w", err)
	}
	if state != StateNeedsAttention {
		return fmt.Errorf("%w: state is %s", ErrNotCancellable, state)
	}
	if _, err := tx.Exec(`UPDATE runs SET state = ? WHERE id = ?`, StateCancelled, runID); err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}
	if err := moveTaskFor(tx, StateCancelled, "", taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResumePoints는 Runner의 재연결 시 Welcome에 실어 보낼 Run별 ack 지점이다.
func (s *Store) ResumePoints() (map[string]uint64, error) {
	rows, err := s.db.Query(`SELECT id, last_acked_seq FROM runs`)
	if err != nil {
		return nil, fmt.Errorf("query resume points: %w", err)
	}
	defer rows.Close()

	out := map[string]uint64{}
	for rows.Next() {
		var id string
		var seq uint64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, fmt.Errorf("scan resume point: %w", err)
		}
		out[id] = seq
	}
	return out, rows.Err()
}

// Events는 저장된 이벤트를 seq 오름차순으로 돌려준다. 웹 UI의 재생에 쓴다.
func (s *Store) Events(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error) {
	rows, err := s.db.Query(
		`SELECT payload FROM run_events WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []protocol.Envelope
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, fmt.Errorf("unmarshal event: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// ---- Project ----

const projectColumns = `id, key, name, repo_path, default_branch, execute_template, created_at, allowed_tools, create_pr, cleanup_merged,
                        analyze_template, analyze_enabled, analyze_skip_below`

func scanProject(sc rowScanner) (Project, error) {
	var (
		p       Project
		created int64
		tools   string
	)
	if err := sc.Scan(&p.ID, &p.Key, &p.Name, &p.RepoPath, &p.DefaultBranch, &p.ExecuteTemplate, &created, &tools, &p.CreatePR, &p.CleanupMerged,
		&p.AnalyzeTemplate, &p.AnalyzeEnabled, &p.AnalyzeSkipBelow); err != nil {
		return Project{}, err
	}
	// 비어 있으면 기본 템플릿 — 옛 프로젝트도 1단계를 얻고, 기본이 바뀌면 따라간다.
	if p.AnalyzeTemplate == "" {
		p.AnalyzeTemplate = pipeline.DefaultAnalyzeTemplate
	}
	p.CreatedAt = time.Unix(0, created)
	p.AllowedTools = splitTools(tools)
	return p, nil
}

// CreateProject는 프로젝트를 만든다. ID가 비어 있으면 발급한다. key가 이미
// 쓰이면 ErrDuplicateKey. 연결이 하나(SetMaxOpenConns(1))라 존재 확인과 삽입
// 사이에 끼어들 트랜잭션이 없다.
func (s *Store) CreateProject(p Project) (Project, error) {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRow(`SELECT 1 FROM projects WHERE key = ?`, p.Key).Scan(&exists)
	if err == nil {
		return Project{}, ErrDuplicateKey
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("probe project key: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO projects (`+projectColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Key, p.Name, p.RepoPath, p.DefaultBranch, p.ExecuteTemplate, p.CreatedAt.UnixNano(), joinTools(p.AllowedTools),
		p.CreatePR, p.CleanupMerged, p.AnalyzeTemplate, p.AnalyzeEnabled, p.AnalyzeSkipBelow,
	); err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return p, nil
}

func (s *Store) GetProject(key string) (Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrProjectNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// GetProjectByID는 Task·Run에서 프로젝트로 거슬러 올라갈 때 쓴다.
func (s *Store) GetProjectByID(id string) (Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrProjectNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project by id: %w", err)
	}
	return p, nil
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectColumns + ` FROM projects ORDER BY created_at ASC, rowid ASC`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProjectSettings는 설정 폼의 필드를 한 번에 바꾼다 — 일부만 저장되는
// 창이 없도록 UPDATE 하나로. AllowedTools가 nil이면 비운다. 항목 문법 검사는
// 호출자(웹 폼)의 몫이다.
func (s *Store) UpdateProjectSettings(key string, st ProjectSettings) error {
	res, err := s.db.Exec(
		`UPDATE projects SET repo_path = ?, default_branch = ?, execute_template = ?, allowed_tools = ?,
		                     create_pr = ?, cleanup_merged = ?,
		                     analyze_template = ?, analyze_enabled = ?, analyze_skip_below = ? WHERE key = ?`,
		st.RepoPath, st.DefaultBranch, st.ExecuteTemplate, joinTools(st.AllowedTools), st.CreatePR, st.CleanupMerged,
		st.AnalyzeTemplate, st.AnalyzeEnabled, st.AnalyzeSkipBelow, key)
	if err != nil {
		return fmt.Errorf("update project settings: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// ---- Task ----

const taskColumns = `id, project_id, number, title, body, status, created_at`

func scanTask(sc rowScanner) (Task, error) {
	var (
		t       Task
		created int64
	)
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Number, &t.Title, &t.Body, &t.Status, &created); err != nil {
		return Task{}, err
	}
	t.CreatedAt = time.Unix(0, created)
	return t, nil
}

// CreateTask는 이슈를 만들고 프로젝트 안의 다음 번호를 같은 트랜잭션에서
// 발급한다. Status가 비어 있으면 backlog.
func (s *Store) CreateTask(t Task) (Task, error) {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.Status == "" {
		t.Status = TaskBacklog
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if err := tx.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM tasks WHERE project_id = ?`, t.ProjectID).Scan(&t.Number); err != nil {
		return Task{}, fmt.Errorf("next task number: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Number, t.Title, t.Body, t.Status, t.CreatedAt.UnixNano(),
	); err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("commit: %w", err)
	}
	return t, nil
}

func (s *Store) GetTask(projectID string, number int) (Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE project_id = ? AND number = ?`, projectID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// GetTaskByID는 Run.TaskID에서 이슈로 거슬러 올라갈 때 쓴다.
func (s *Store) GetTaskByID(id string) (Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task by id: %w", err)
	}
	return t, nil
}

// ListTasks는 프로젝트의 이슈를 번호 역순(최신 먼저)으로 돌려준다.
func (s *Store) ListTasks(projectID string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskColumns+` FROM tasks WHERE project_id = ? ORDER BY number DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTaskStatus(taskID, status string) error {
	res, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ---- 산출물과 단계 ----

const artifactColumns = `run_id, name, content, truncated, created_at`

func scanArtifact(sc rowScanner) (Artifact, error) {
	var (
		a       Artifact
		created int64
	)
	if err := sc.Scan(&a.RunID, &a.Name, &a.Content, &a.Truncated, &created); err != nil {
		return Artifact{}, err
	}
	a.CreatedAt = time.Unix(0, created)
	return a, nil
}

// Artifacts는 Run의 산출물을 이름순으로 돌려준다.
func (s *Store) Artifacts(runID string) ([]Artifact, error) {
	rows, err := s.db.Query(`SELECT `+artifactColumns+` FROM artifacts WHERE run_id = ? ORDER BY name`, runID)
	if err != nil {
		return nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Artifact(runID, name string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(`SELECT `+artifactColumns+` FROM artifacts WHERE run_id = ? AND name = ?`, runID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("get artifact: %w", err)
	}
	return a, nil
}

// LatestSucceededRun은 이슈의 가장 최근 succeeded Run을 단계별로 찾는다.
func (s *Store) LatestSucceededRun(taskID, stage string) (Run, bool, error) {
	r, err := scanRun(s.db.QueryRow(
		`SELECT `+runColumns+` FROM runs WHERE task_id = ? AND stage = ? AND state = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		taskID, stage, StateSucceeded))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("latest succeeded run: %w", err)
	}
	return r, true, nil
}

// AwaitingExecute는 1단계가 성공했고 그 뒤에 아무 Run도 없는 이슈다.
type AwaitingExecute struct {
	Task       Task
	AnalyzeRun Run
}

// TasksAwaitingExecute는 이어 실행의 대상이다: 이슈의 가장 최근 Run이
// stage=analyze·succeeded인 것. 언제 몇 번 불러도 같은 답이다 — 2단계 Run이
// 생기면 "가장 최근"이 바뀌어 빠진다.
func (s *Store) TasksAwaitingExecute() ([]AwaitingExecute, error) {
	rows, err := s.db.Query(`
		SELECT ` + runColumns + ` FROM runs r
		WHERE r.stage = '` + StageAnalyze + `' AND r.state = '` + StateSucceeded + `'
		  AND r.id = (SELECT id FROM runs WHERE task_id = r.task_id ORDER BY created_at DESC, rowid DESC LIMIT 1)`)
	if err != nil {
		return nil, fmt.Errorf("query awaiting execute: %w", err)
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []AwaitingExecute
	for _, r := range runs {
		task, err := s.GetTaskByID(r.TaskID)
		if err != nil {
			continue
		}
		out = append(out, AwaitingExecute{Task: task, AnalyzeRun: r})
	}
	return out, nil
}

// UpdateTaskStatusIf는 현재 상태가 from일 때만 to로 바꾼다. 실행 시작의 보상
// (in_progress 로 올린 것을 되돌리기)이 그 사이 들어온 다른 전이를 덮지 않게.
func (s *Store) UpdateTaskStatusIf(taskID, from, to string) error {
	if _, err := s.db.Exec(`UPDATE tasks SET status = ? WHERE id = ? AND status = ?`, to, taskID, from); err != nil {
		return fmt.Errorf("update task status if: %w", err)
	}
	return nil
}

// PendingApproval은 사람을 기다리는 승인 하나다. 첫 화면이 보여 준다.
type PendingApproval struct {
	RunID      string
	Command    string
	ProjectKey string
	TaskNumber int
	TaskTitle  string
}

// PendingApprovals는 지금 사람을 기다리는 승인 전부다. Run이 끝나거나 그 도구
// 호출이 끝나면 목록에서 빠진다.
func (s *Store) PendingApprovals() ([]PendingApproval, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.pending_approval, p.key, t.number, t.title
		FROM runs r
		JOIN tasks t ON t.id = r.task_id
		JOIN projects p ON p.id = t.project_id
		WHERE r.pending_request_id != ''
		ORDER BY r.created_at`)
	if err != nil {
		return nil, fmt.Errorf("query pending approvals: %w", err)
	}
	defer rows.Close()
	var out []PendingApproval
	for rows.Next() {
		var a PendingApproval
		if err := rows.Scan(&a.RunID, &a.Command, &a.ProjectKey, &a.TaskNumber, &a.TaskTitle); err != nil {
			return nil, fmt.Errorf("scan pending approval: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClearPendingApproval은 서버가 결정을 보냈을 때 부른다 — 러너의 tool_finished를
// 기다리지 않고 화면에서 즉시 사라지게.
func (s *Store) ClearPendingApproval(runID, requestID string) error {
	if _, err := s.db.Exec(
		`UPDATE runs SET pending_request_id = '', pending_tool_use_id = '', pending_approval = ''
		 WHERE id = ? AND pending_request_id = ?`, runID, requestID,
	); err != nil {
		return fmt.Errorf("clear pending approval: %w", err)
	}
	return nil
}
