// Package spool은 Runner의 로컬 이벤트 대기열과 명령 멱등성 원장이다.
//
// Runner는 Server로 보내기 전에 항상 여기 먼저 적는다. 연결이 끊겨도
// 이벤트가 남고, 재연결 시 Server가 알려준 지점부터 다시 보낸다.
// PRD §11.5의 at-least-once 전송과 §11.7의 seq 영속성을 담당한다.
package spool

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/jinto/taskyard/internal/protocol"
)

const schema = `
CREATE TABLE IF NOT EXISTS spool (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (run_id, seq)
);

-- last_issued는 Ack으로 줄어들지 않는다. 재시작 후에도 seq가
-- 이어지도록 spool 행과 분리해 보관한다.
CREATE TABLE IF NOT EXISTS seq_cursor (
  run_id      TEXT    PRIMARY KEY,
  last_issued INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS command_log (
  command_id TEXT PRIMARY KEY,
  result     BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  run_id        TEXT    PRIMARY KEY,
  state         TEXT    NOT NULL,
  session_id    TEXT    NOT NULL DEFAULT '',
  branch        TEXT    NOT NULL DEFAULT '',
  worktree_path TEXT    NOT NULL DEFAULT '',
  pid           INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL DEFAULT 0
);
`

// Spool은 SQLite로 뒷받침되는 이벤트 대기열이다.
type Spool struct {
	db *sql.DB
}

// Open은 경로의 SQLite 파일을 열고 스키마를 보장한다.
func Open(path string) (*Spool, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open spool db: %w", err)
	}
	// 쓰기 직렬화를 단순하게 유지한다. spool은 처리량보다 정확성이 중요하다.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create spool schema: %w", err)
	}
	return &Spool{db: db}, nil
}

func (s *Spool) Close() error { return s.db.Close() }

// Append는 다음 seq를 발급해 봉투에 채우고 저장한다.
func (s *Spool) Append(runID string, env protocol.Envelope) (uint64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var last uint64
	err = tx.QueryRow(`SELECT last_issued FROM seq_cursor WHERE run_id = ?`, runID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read cursor: %w", err)
	}

	next := last + 1
	if _, err := tx.Exec(
		`INSERT INTO seq_cursor (run_id, last_issued) VALUES (?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET last_issued = excluded.last_issued`,
		runID, next,
	); err != nil {
		return 0, fmt.Errorf("bump cursor: %w", err)
	}

	env.Seq = next
	payload, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("marshal envelope: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO spool (run_id, seq, payload) VALUES (?, ?, ?)`,
		runID, next, payload,
	); err != nil {
		return 0, fmt.Errorf("insert spool row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return next, nil
}

// Since는 afterSeq보다 큰 이벤트를 seq 오름차순으로 최대 limit개 돌려준다.
func (s *Spool) Since(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error) {
	rows, err := s.db.Query(
		`SELECT payload FROM spool WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query spool: %w", err)
	}
	defer rows.Close()

	var out []protocol.Envelope
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan payload: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, fmt.Errorf("unmarshal envelope: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// Ack은 Server가 확인한 지점까지의 이벤트를 지운다. seq 카운터는 건드리지 않는다.
func (s *Spool) Ack(runID string, throughSeq uint64) error {
	_, err := s.db.Exec(`DELETE FROM spool WHERE run_id = ? AND seq <= ?`, runID, throughSeq)
	if err != nil {
		return fmt.Errorf("ack spool: %w", err)
	}
	return nil
}

// Pending은 아직 확인되지 않은 이벤트 수를 돌려준다.
func (s *Spool) Pending(runID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM spool WHERE run_id = ?`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// ActiveRuns는 미확인 이벤트가 남은 Run 목록을 돌려준다.
func (s *Spool) ActiveRuns() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT run_id FROM spool ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("query active runs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RememberCommand는 명령을 한 번만 적용하기 위한 원장이다. 처음 보는
// command_id면 result를 저장하고 firstTime=true를 돌려준다. 이미 본
// 명령이면 저장돼 있던 결과와 firstTime=false를 돌려준다.
func (s *Spool) RememberCommand(commandID string, result []byte) ([]byte, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var existing []byte
	err = tx.QueryRow(`SELECT result FROM command_log WHERE command_id = ?`, commandID).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, fmt.Errorf("read command log: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO command_log (command_id, result) VALUES (?, ?)`,
		commandID, result,
	); err != nil {
		return nil, false, fmt.Errorf("insert command log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return result, true, nil
}

// RunRecord는 Runner가 로컬에 남기는 실행 기록이다. 재시작 후 조정
// (PRD §11.7)이 이 원장에서 시작한다.
type RunRecord struct {
	RunID         string
	State         string
	SessionID     string
	Branch        string
	WorktreePath  string
	PID           int
	StartedAtUnix int64
}

// SaveRun은 실행 기록을 만들거나 덮어쓴다.
func (s *Spool) SaveRun(r RunRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO runs (run_id, state, session_id, branch, worktree_path, pid, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET
		   state         = excluded.state,
		   session_id    = excluded.session_id,
		   branch        = excluded.branch,
		   worktree_path = excluded.worktree_path,
		   pid           = excluded.pid,
		   started_at    = excluded.started_at`,
		r.RunID, r.State, r.SessionID, r.Branch, r.WorktreePath, r.PID, r.StartedAtUnix,
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// LoadRuns는 모든 실행 기록을 돌려준다.
func (s *Spool) LoadRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT run_id, state, session_id, branch, worktree_path, pid, started_at FROM runs ORDER BY run_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		if err := rows.Scan(&r.RunID, &r.State, &r.SessionID, &r.Branch, &r.WorktreePath, &r.PID, &r.StartedAtUnix); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
