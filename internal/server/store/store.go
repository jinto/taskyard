// Package store는 Server의 Run 원장과 이벤트 저장소다.
//
// Runner는 at-least-once로 이벤트를 보내므로 여기서 멱등하게 적용한다.
// ack 커서는 "빠짐없이 연속으로 받은 마지막 seq"이며, 중간이 비면
// 앞으로 나아가지 않는다. PRD §11.5와 §15.3을 따른다.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/jinto/taskyard/internal/protocol"
)

// Run 상태. PRD §9.3의 부분집합으로, 스파이크에 필요한 것만 둔다.
const (
	StateQueued          = "queued"
	StateRunning         = "running"
	StateWaitingApproval = "waiting_approval"
	StateOrphaned        = "orphaned"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateCancelled       = "cancelled"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id                  TEXT    PRIMARY KEY,
  state               TEXT    NOT NULL,
  kind                TEXT    NOT NULL,
  provider_session_id TEXT    NOT NULL DEFAULT '',
  branch              TEXT    NOT NULL DEFAULT '',
  worktree_path       TEXT    NOT NULL DEFAULT '',
  last_acked_seq      INTEGER NOT NULL DEFAULT 0,
  reconcile_state     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS run_events (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  type    TEXT    NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (run_id, seq)
);
`

// ErrRunNotFound는 알 수 없는 Run을 조회했을 때 반환한다.
var ErrRunNotFound = errors.New("run not found")

// Run은 Server가 보는 실행 한 건이다.
type Run struct {
	ID                string
	State             string
	Kind              string
	ProviderSessionID string
	Branch            string
	WorktreePath      string
	LastAckedSeq      uint64
	ReconcileState    string
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertRun은 Run을 만들거나 갱신한다. last_acked_seq는 이벤트 적용만이
// 움직이므로 여기서 절대 덮어쓰지 않는다.
func (s *Store) UpsertRun(r Run) error {
	_, err := s.db.Exec(
		`INSERT INTO runs (id, state, kind, provider_session_id, branch, worktree_path, reconcile_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   state               = excluded.state,
		   kind                = excluded.kind,
		   provider_session_id = excluded.provider_session_id,
		   branch              = excluded.branch,
		   worktree_path       = excluded.worktree_path,
		   reconcile_state     = excluded.reconcile_state`,
		r.ID, r.State, r.Kind, r.ProviderSessionID, r.Branch, r.WorktreePath, r.ReconcileState,
	)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(id string) (Run, error) {
	var r Run
	err := s.db.QueryRow(
		`SELECT id, state, kind, provider_session_id, branch, worktree_path, last_acked_seq, reconcile_state
		 FROM runs WHERE id = ?`, id,
	).Scan(&r.ID, &r.State, &r.Kind, &r.ProviderSessionID, &r.Branch, &r.WorktreePath, &r.LastAckedSeq, &r.ReconcileState)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// ApplyEvent는 이벤트를 저장하고 ack 커서를 가능한 만큼 전진시킨다.
// accepted는 이번 호출로 새로 저장됐는지를 뜻한다. 이미 있던 seq면 false다.
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

	var acked uint64
	err = tx.QueryRow(`SELECT last_acked_seq FROM runs WHERE id = ?`, env.RunID).Scan(&acked)
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
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}
	return affected > 0, acked, nil
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
