package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/jinto/taskyard/internal/runner/spool"
)

// Verdict는 재시작 후 Run의 실제 상태 판정이다(PRD §11.7).
type Verdict string

const (
	// VerdictAlive: Agent 프로세스가 아직 살아 있다.
	VerdictAlive Verdict = "alive"
	// VerdictResumable: 프로세스는 죽었지만 Provider 세션으로 재개할 수 있다.
	VerdictResumable Verdict = "resumable"
	// VerdictLost: 재개할 수 없다. 변경사항만 보존한다.
	VerdictLost Verdict = "lost"
)

// terminalStates는 조정 대상이 아닌 상태다. needs_attention은 프로세스 없이
// 사람을 기다리는 정착 상태라 lost로 오판하면 안 된다.
var terminalStates = map[string]bool{
	"succeeded":       true,
	"failed":          true,
	"cancelled":       true,
	"needs_attention": true,
}

// Classify는 기록 하나의 실제 상태를 판정한다.
func (m *Manager) Classify(rec spool.RunRecord) Verdict {
	if rec.PID > 0 && processAlive(rec.PID) {
		return VerdictAlive
	}
	if rec.SessionID != "" {
		return VerdictResumable
	}
	return VerdictLost
}

// processAlive는 PID가 살아 있는지 본다.
//
// PID 재사용은 남은 위험이다. 정확히 하려면 프로세스 시작시각까지 대조해야
// 하고, 그것은 Phase 1 과제다. 스파이크에서는 오판의 결과가
// "재개 가능한 Run을 alive로 착각"뿐이라 감수한다.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0은 프로세스를 건드리지 않고 존재만 확인한다.
	return p.Signal(syscall.Signal(0)) == nil
}

// Reconcile은 재시작 직후 로컬 원장을 훑어 각 Run의 상태를 판정하고
// 결과를 Server에 올린다. lost로 판정된 Run은 변경사항을 보존한다.
func (m *Manager) Reconcile(ctx context.Context) error {
	records, err := m.cfg.Spool.LoadRuns()
	if err != nil {
		return fmt.Errorf("load runs: %w", err)
	}

	for _, rec := range records {
		if terminalStates[rec.State] {
			continue
		}

		verdict := m.Classify(rec)
		slog.Info("reconciled run", "run_id", rec.RunID, "verdict", verdict)

		switch verdict {
		case VerdictAlive:
			m.emitState(rec.RunID, "running", "reconciled: process still alive")

		case VerdictResumable:
			m.emitState(rec.RunID, "running", "reconciled: session resumable")

		case VerdictLost:
			// 반드시 보존이 먼저다. 사용자 작업을 잃지 않는 것이 최우선이다.
			// 어느 저장소인지는 기록의 RepoPath가 말한다(없으면 첫 허용 저장소).
			// 허용 목록에서 빠진 저장소면 보존은 건너뛰되 판정은 그대로 한다.
			// worktree는 기록의 WorkspaceRunID가 주인이다(이어서 재시도는 이전
			// Run의 것을 쓴다). 비어 있으면 자기 자신.
			wsID := rec.WorkspaceRunID
			if wsID == "" {
				wsID = rec.RunID
			}
			if git, _, err := m.cfg.Repos.resolve(rec.RepoPath); err != nil {
				slog.Warn("cannot salvage: repository no longer allowed", "run_id", rec.RunID, "repo", rec.RepoPath, "err", err)
			} else {
				m.salvage(rec.RunID, wsID, git)
			}
			m.emitTerminal(rec.RunID, "failed", "reconciled: session lost, work salvaged", rec.SessionID)

			rec.State = "failed"
			if err := m.cfg.Spool.SaveRun(rec); err != nil {
				return fmt.Errorf("mark run failed: %w", err)
			}
		}
	}
	return nil
}
