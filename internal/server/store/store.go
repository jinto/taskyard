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
  feedback            TEXT    NOT NULL DEFAULT ''
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
  allowed_tools    TEXT    NOT NULL DEFAULT ''
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
}

// projectsMigrations는 PR #3 이후 projects 테이블에 추가된 컬럼이다.
var projectsMigrations = []sqlitex.Column{
	{Name: "allowed_tools", DDL: "TEXT NOT NULL DEFAULT ''"},
}

var (
	// ErrRunNotFound는 알 수 없는 Run을 조회했을 때 반환한다.
	ErrRunNotFound = errors.New("run not found")
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
		                   detail, previous_run_id, workspace_run_id, feedback)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		   feedback            = excluded.feedback`,
		r.ID, r.State, r.Kind, r.ProviderSessionID, r.Branch, r.WorktreePath, r.ReconcileState,
		r.TaskID, r.Stage, created.UnixNano(),
		r.Detail, r.PreviousRunID, r.WorkspaceRunID, r.Feedback,
	)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}

const runColumns = `id, state, kind, provider_session_id, branch, worktree_path, last_acked_seq, reconcile_state, task_id, stage, created_at,
                    detail, previous_run_id, workspace_run_id, feedback`

type rowScanner interface{ Scan(dest ...any) error }

func scanRun(sc rowScanner) (Run, error) {
	var (
		r       Run
		created int64
	)
	err := sc.Scan(&r.ID, &r.State, &r.Kind, &r.ProviderSessionID, &r.Branch, &r.WorktreePath,
		&r.LastAckedSeq, &r.ReconcileState, &r.TaskID, &r.Stage, &created,
		&r.Detail, &r.PreviousRunID, &r.WorkspaceRunID, &r.Feedback)
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
	)
	err = tx.QueryRow(`SELECT last_acked_seq, state, task_id FROM runs WHERE id = ?`, env.RunID).Scan(&acked, &current, &taskID)
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

	if affected > 0 && env.Type == protocol.EvRunStateChanged {
		if err := applyStateChange(tx, env, current, taskID); err != nil {
			return false, 0, err
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
func applyStateChange(tx *sql.Tx, env protocol.Envelope, current, taskID string) error {
	var outer struct {
		Body struct {
			State     string `json:"state"`
			Detail    string `json:"detail"`
			SessionID string `json:"session_id"`
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
	// 알던 세션을 빈 값으로 지우지 않는다 — 이어서 재시도가 이 값에 기댄다.
	if outer.Body.SessionID != "" {
		if _, err := tx.Exec(`UPDATE runs SET provider_session_id = ? WHERE id = ?`, outer.Body.SessionID, env.RunID); err != nil {
			return fmt.Errorf("update run session: %w", err)
		}
	}
	return moveTaskFor(tx, next, taskID)
}

// moveTaskFor는 Run 상태에 따른 이슈 상태 전이다.
func moveTaskFor(tx *sql.Tx, runState, taskID string) error {
	if taskID == "" {
		return nil
	}
	var status string
	switch runState {
	case StateSucceeded:
		status = TaskReview
	case StateCancelled:
		status = TaskBacklog
	default:
		return nil
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID); err != nil {
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
	if err := moveTaskFor(tx, StateCancelled, taskID); err != nil {
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

const projectColumns = `id, key, name, repo_path, default_branch, execute_template, created_at, allowed_tools`

func scanProject(sc rowScanner) (Project, error) {
	var (
		p       Project
		created int64
		tools   string
	)
	if err := sc.Scan(&p.ID, &p.Key, &p.Name, &p.RepoPath, &p.DefaultBranch, &p.ExecuteTemplate, &created, &tools); err != nil {
		return Project{}, err
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
		`INSERT INTO projects (`+projectColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Key, p.Name, p.RepoPath, p.DefaultBranch, p.ExecuteTemplate, p.CreatedAt.UnixNano(), joinTools(p.AllowedTools),
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

func (s *Store) UpdateProjectTemplate(key, executeTemplate string) error {
	res, err := s.db.Exec(`UPDATE projects SET execute_template = ? WHERE key = ?`, executeTemplate, key)
	if err != nil {
		return fmt.Errorf("update project template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// UpdateProjectAllowedTools는 허용 도구 목록을 통째로 바꾼다. nil이면 비운다.
// 항목 문법 검사는 호출자(웹 폼)의 몫이다.
func (s *Store) UpdateProjectAllowedTools(key string, tools []string) error {
	res, err := s.db.Exec(`UPDATE projects SET allowed_tools = ? WHERE key = ?`, joinTools(tools), key)
	if err != nil {
		return fmt.Errorf("update project allowed tools: %w", err)
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
