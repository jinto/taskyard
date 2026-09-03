package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/github"
	"github.com/jinto/taskyard/internal/runner/spool"
)

// PR 생성(GH-05)·추적(GH-06)·merge 후 정리(GH-10). PR은 에이전트가 아니라
// 러너가 만든다 — 결정론적이고, 승인·토큰이 안 들며, 원격 없는 저장소는
// run.start의 pr을 비워 끈다. merge는 사람이 GitHub에서 한다; 여기서는
// 감지만 한다(PRD §7.6 "merge는 사람이 직접").

const (
	maxSummaryBytes = 16 * 1024
	summaryFile     = ".taskyard/summary.md"
	defaultPRPoll   = time.Minute
	prOpTimeout     = 2 * time.Minute
)

// takeSummary는 에이전트가 남긴 변경 설명을 읽고 파일을 지운다. attention과
// 같은 패턴 — 남기면 salvage가 커밋하고 다음 Run이 다시 읽는다. 없으면 빈
// 문자열.
func takeSummary(worktree string) string {
	summary, truncated, ok := readWorktreeFile(worktree, summaryFile, maxSummaryBytes)
	if !ok {
		return ""
	}
	// 비어 있어도 지운다 — 남기면 salvage가 커밋한다.
	if err := os.Remove(filepath.Join(worktree, summaryFile)); err != nil {
		slog.Warn("could not remove summary file", "err", err)
	}
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	if truncated {
		summary += "\n…(summary.md truncated at 16KiB)"
	}
	return summary
}

func (m *Manager) gh() github.Client {
	return github.Client{Binary: m.cfg.GHBinary, Timeout: prOpTimeout}
}

// publishPR은 성공한 Run의 브랜치를 push하고 PR을 만들거나 기존 PR에 붙인 뒤
// pr.updated를 발행한다. 돌려주는 값은 종결 상태와 detail — push·gh 실패는
// needs_attention이다(작업은 브랜치에 남아 있고, 사람이 고친 뒤 이어서
// 재시도하면 여기가 다시 돈다). rec에 PR 필드를 채운다.
//
// 순서: SaveRun(PR 필드, 아직 running) → pr.updated. 그 사이에 죽어도 PR 필드는
// 저장돼 있어 추적되고, view-before-create라 재시도가 같은 PR에 붙는다.
func (m *Manager) publishPR(spec runSpec, rec *spool.RunRecord, summary string) (state, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), prOpTimeout)
	defer cancel()

	wsID := spec.workspaceRunID
	ahead, err := spec.git.AheadOf(ctx, wsID, spec.baseBranch)
	if err != nil {
		return "needs_attention", "PR 생성 실패: " + err.Error()
	}
	if ahead == 0 {
		return "succeeded", "변경 커밋 없음 — PR 없음"
	}
	if err := spec.git.Push(ctx, wsID); err != nil {
		return "needs_attention", "PR 생성 실패: " + err.Error()
	}
	pr, found, err := m.gh().View(ctx, spec.ws.Path, spec.ws.Branch)
	if err == nil && !found {
		body := spec.pr.Body
		if summary != "" {
			body = summary + "\n\n" + spec.pr.Body
		}
		pr, err = m.gh().Create(ctx, spec.ws.Path, spec.baseBranch, spec.ws.Branch, spec.pr.Title, body)
	}
	if err != nil {
		return "needs_attention", "PR 생성 실패: " + err.Error()
	}

	rec.PRURL, rec.PRNumber, rec.PRState, rec.PRChecks, rec.PRReview = pr.URL, pr.Number, pr.State, pr.Checks, pr.Review
	m.supersede(spec.ws.Branch, spec.runID)
	// 아직 running으로 저장한다. 종결 상태로 저장했다가 emitTerminal 전에 죽으면
	// Reconcile이 종결 기록으로 보고 건너뛰어 서버가 종결을 영영 못 받는다.
	interim := *rec
	interim.State = "running"
	if err := m.cfg.Spool.SaveRun(interim); err != nil {
		slog.Error("save run pr fields failed", "run_id", spec.runID, "err", err)
	}
	m.emitPR(spec.runID, protocol.PRUpdatedBody{URL: pr.URL, Number: pr.Number, State: pr.State, Checks: pr.Checks, Review: pr.Review})
	return "succeeded", ""
}

// supersede는 같은 브랜치의 다른 OPEN 기록을 추적에서 뺀다. 이어서 재시도는
// 같은 브랜치·같은 PR을 쓰므로 주인은 가장 최근 Run 하나여야 한다.
func (m *Manager) supersede(branch, ownerRunID string) {
	records, err := m.cfg.Spool.LoadRuns()
	if err != nil {
		return
	}
	for _, r := range records {
		if r.Branch == branch && r.RunID != ownerRunID && r.PRState == "OPEN" {
			_, _ = m.cfg.Spool.UpdatePR(r.RunID, "OPEN", spool.PRUpdate{State: "superseded", Checks: r.PRChecks, Review: r.PRReview, WorktreeRemoved: r.WorktreeRemoved})
		}
	}
}

func (m *Manager) emitPR(runID string, body protocol.PRUpdatedBody) {
	env, err := protocol.NewEvent(protocol.EvPRUpdated, runID, 0, eventBody{Body: map[string]any{
		"url": body.URL, "number": body.Number, "state": body.State,
		"checks": body.Checks, "review": body.Review, "worktree_removed": body.WorktreeRemoved,
	}})
	if err != nil {
		slog.Error("build pr event failed", "err", err)
		return
	}
	if err := m.cfg.Publish(runID, env); err != nil {
		slog.Error("publish pr event failed", "run_id", runID, "err", err)
	}
}

// TrackPRs는 OPEN PR을 주기적으로 보고 바뀐 것만 알린다. main이 Start와
// 나란히 띄운다 — Start의 select에 넣지 않는 이유는 그 루프가 승인 요청을
// 나르기 때문이다. 추적 대상은 spool 기록(Run 상태와 무관)이라 재시작해도
// 이어진다.
func (m *Manager) TrackPRs(ctx context.Context) {
	interval := m.cfg.PRPollInterval
	if interval == 0 {
		interval = defaultPRPoll
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollPRs(ctx)
		}
	}
}

// trackedPRs는 브랜치마다 가장 최근의 OPEN 기록 하나다.
func trackedPRs(records []spool.RunRecord) []spool.RunRecord {
	byBranch := map[string]spool.RunRecord{}
	for _, r := range records {
		if r.PRNumber == 0 || r.PRState != "OPEN" {
			continue
		}
		if cur, ok := byBranch[r.Branch]; !ok || r.StartedAtUnix > cur.StartedAtUnix {
			byBranch[r.Branch] = r
		}
	}
	out := make([]spool.RunRecord, 0, len(byBranch))
	for _, r := range byBranch {
		out = append(out, r)
	}
	return out
}

func (m *Manager) pollPRs(ctx context.Context) {
	records, err := m.cfg.Spool.LoadRuns()
	if err != nil {
		slog.Error("load runs for pr tracking failed", "err", err)
		return
	}
	for _, rec := range trackedPRs(records) {
		// finish가 아직 PR 필드를 쓰는 중인 Run은 건너뛴다 — 두 goroutine이
		// 같은 기록을 번갈아 덮어쓰지 않게.
		if m.isActive(rec.RunID) {
			continue
		}
		git, _, err := m.cfg.Repos.resolve(rec.RepoPath)
		if err != nil {
			slog.Warn("cannot track pr: repository no longer allowed", "run_id", rec.RunID, "err", err)
			continue
		}
		// gh는 저장소 안에서 돌면 된다 — worktree가 사라졌어도 본 저장소로.
		pr, found, err := m.gh().View(ctx, rec.RepoPath, rec.Branch)
		if err != nil || !found {
			slog.Warn("pr view failed; will retry", "run_id", rec.RunID, "found", found, "err", err)
			continue
		}
		body := protocol.PRUpdatedBody{URL: pr.URL, Number: pr.Number, State: pr.State, Checks: pr.Checks, Review: pr.Review}
		if pr.State == rec.PRState && pr.Checks == rec.PRChecks && pr.Review == rec.PRReview {
			continue
		}
		if pr.State == "MERGED" && rec.CleanupMerged {
			body.WorktreeRemoved = m.cleanupMerged(ctx, rec, git)
		}
		// 읽은 뒤 재시도가 이 기록을 superseded로 바꿨으면(compare-and-set 실패)
		// 주인이 아니다 — 이벤트를 내지 않는다.
		ok, err := m.cfg.Spool.UpdatePR(rec.RunID, rec.PRState, spool.PRUpdate{
			State: pr.State, Checks: pr.Checks, Review: pr.Review, WorktreeRemoved: body.WorktreeRemoved,
		})
		if err != nil {
			slog.Error("save run pr state failed", "run_id", rec.RunID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		m.emitPR(rec.RunID, body)
	}
}

// cleanupMerged는 merge된 Run의 worktree를 지운다(GH-10). 지우기 직전에 다시
// 확인한다: 그 workspace를 쓰는 활성 Run 없음, 미커밋 변경 없음, push 안 된
// 커밋 없음. 하나라도 걸리면 보존하고 로그 — 미커밋 변경 손실이 가장 빨리
// 신뢰를 무너뜨린다(§8.7.1). 브랜치는 남긴다.
func (m *Manager) cleanupMerged(ctx context.Context, rec spool.RunRecord, git *gitops.Manager) bool {
	wsID := rec.WorkspaceRunID
	if wsID == "" {
		wsID = rec.RunID
	}
	keep := func(why string, err error) bool {
		slog.Info("keeping merged worktree", "run_id", rec.RunID, "why", why, "err", err)
		return false
	}
	if m.workspaceInUse(wsID) {
		return keep("another run uses the workspace", nil)
	}
	st, err := git.Status(ctx, wsID)
	if err != nil {
		return keep("status failed", err)
	}
	if st.Dirty {
		return keep(fmt.Sprintf("uncommitted changes: %v", st.ChangedPaths), nil)
	}
	n, err := git.Unpushed(ctx, wsID)
	if err != nil {
		return keep("unpushed count failed", err)
	}
	if n > 0 {
		return keep(fmt.Sprintf("%d unpushed commit(s)", n), nil)
	}
	if err := git.Remove(ctx, wsID); err != nil {
		return keep("remove failed", err)
	}
	slog.Info("removed merged worktree", "run_id", rec.RunID, "workspace", wsID)
	return true
}

func (m *Manager) isActive(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[runID]
	return ok
}

func (m *Manager) workspaceInUse(wsID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.active {
		if h.spec.workspaceRunID == wsID {
			return true
		}
	}
	return false
}
