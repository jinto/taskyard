// Package launch는 Run을 시작하는 한 곳이다. 사람의 [실행]·재시도(web)와
// 1단계 성공 뒤의 자동 이어 실행(ChainPending)이 같은 Start를 쓴다(PRD §7.2).
//
// 이어 실행은 이벤트가 아니라 조정이다: "이슈의 가장 최근 Run이 succeeded한
// 1단계 Run이면 2단계를 연다"를 언제 몇 번 돌려도 같은 결과가 나오게 만들고,
// hub의 정착 신호·서버 시작·러너 접속에서 돌린다. 이벤트를 놓쳐도 다음
// 기회에 이어진다.
package launch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/pipeline"
	"github.com/jinto/taskyard/internal/server/store"
)

var (
	// ErrRunActive: 이슈에 정착하지 않은 Run이 있다.
	ErrRunActive = errors.New("a run is still active for this task")
	// ErrRunnerUnavailable: run.start를 러너에 보내지 못했다. 새 Run은 failed로
	// 정리됐고 이슈 상태는 되돌렸다.
	ErrRunnerUnavailable = errors.New("runner is not connected")
)

// Commander는 hub 중 Launcher가 쓰는 부분이다.
type Commander interface {
	Connected() bool
	SendCommand(env protocol.Envelope) error
}

// Signaler는 정착 신호를 내는 hub다(Run에서 기다린다).
type Signaler interface {
	Settled() <-chan struct{}
}

type Launcher struct {
	Store     *store.Store
	Commander Commander
}

// Options는 Start 한 번의 재료다. Previous가 있으면 재시도다.
type Options struct {
	Stage          string
	Previous       store.Run
	Feedback       string
	WorkspaceRunID string // 비어 있으면 새 Run 자신
	ResumeSession  string
	ReportRunID    string // 2단계가 쓸 1단계 Run. 비어 있으면 보고서 없음
}

// ChooseStage는 [실행] 폼의 선택을 단계로 옮긴다. "auto"는 프로젝트 설정과
// 이슈 본문 길이만 본다 — 이전 1단계가 있어도 다시 분석한다(이슈나 저장소가
// 그 사이 바뀌었을 수 있다). 옛 보고서를 쓰려면 "execute"를 고른다.
func ChooseStage(p store.Project, task store.Task, choice string) string {
	switch choice {
	case store.StageAnalyze, store.StageExecute:
		return choice
	}
	if !p.AnalyzeEnabled {
		return store.StageExecute
	}
	if p.AnalyzeSkipBelow > 0 && utf8.RuneCountInString(task.Body) < p.AnalyzeSkipBelow {
		return store.StageExecute
	}
	return store.StageAnalyze
}

// Start는 Run 하나를 만들고 run.start를 보낸다. 순서 불변식(PR #3): Run 생성 →
// 이슈 in_progress → SendCommand. 전송이 실패하면 Run을 failed로 정리하고
// 이슈 상태를 되돌린다 — queued로 남겨 실행 중이라고 믿게 하지 않는다.
func (l *Launcher) Start(p store.Project, task store.Task, o Options) (*store.Run, error) {
	stage := o.Stage
	if stage == "" {
		stage = store.StageExecute
	}
	runID := "run-" + uuid.NewString()
	wsID := o.WorkspaceRunID
	if wsID == "" {
		wsID = runID
	}

	run := store.Run{
		ID: runID, State: store.StateQueued, Kind: "structured", TaskID: task.ID, Stage: stage,
		PreviousRunID: o.Previous.ID, Feedback: o.Feedback, WorkspaceRunID: wsID, ReportRunID: o.ReportRunID,
	}
	if err := l.Store.CreateRunIfIdle(run); err != nil {
		if errors.Is(err, store.ErrRunActive) {
			return nil, ErrRunActive
		}
		return nil, err
	}

	cmd, err := l.command(p, task, run, o)
	if err != nil {
		return nil, err
	}

	// 보상: 이 시작이 올린 in_progress 만 되돌린다 — 그 사이 다른 전이(옛 PR의
	// merge 등)가 들어왔으면 그것이 맞다.
	abort := func(cause error) error {
		run.State = store.StateFailed
		_ = l.Store.UpsertRun(run)
		_ = l.Store.UpdateTaskStatusIf(task.ID, store.TaskInProgress, task.Status)
		return cause
	}
	if err := l.Store.UpdateTaskStatus(task.ID, store.TaskInProgress); err != nil {
		return nil, abort(err)
	}
	if err := l.Commander.SendCommand(cmd); err != nil {
		slog.Warn("run.start could not be delivered", "run_id", runID, "task_id", task.ID, "err", err)
		return nil, abort(ErrRunnerUnavailable)
	}
	return &run, nil
}

// command는 단계별 템플릿으로 프롬프트를 조립해 run.start를 만든다.
func (l *Launcher) command(p store.Project, task store.Task, run store.Run, o Options) (protocol.Envelope, error) {
	report := ""
	if run.ReportRunID != "" {
		if a, err := l.Store.Artifact(run.ReportRunID, "analysis.md"); err == nil {
			report = a.Content
		}
	}
	tmpl := p.ExecuteTemplate
	if run.Stage == store.StageAnalyze {
		tmpl = p.AnalyzeTemplate
	}
	prompt := pipeline.Render(tmpl, map[string]string{
		"issue":         pipeline.IssueText(task.Number, task.Title, task.Body),
		"stage1_report": report,
		"previous_run":  pipeline.PreviousRunText(o.Previous.ID, o.Previous.State, o.Previous.Detail),
		"feedback":      o.Feedback,
	})
	// 이 PR 이전에 만든 프로젝트의 실행 템플릿에는 {{stage1_report}}가 없다.
	// 그러면 1단계가 세션 하나를 쓰고 보고서는 조용히 버려진다 — 토큰이 없으면
	// 보고서를 프롬프트 끝에 덧붙인다.
	if report != "" && run.Stage == store.StageExecute && !strings.Contains(tmpl, "{{stage1_report}}") {
		prompt += "\n\n1단계 보고서:\n" + report + "\n"
	}

	// PR은 2단계만. 제목은 이슈 제목, 본문은 에이전트의 변경 설명이 없을 때의
	// 대체 — 이슈 번호·Run·이슈 본문. 러너는 이슈를 모른다(GH-05).
	var pr *protocol.PRSpec
	if p.CreatePR && run.Stage == store.StageExecute {
		pr = &protocol.PRSpec{
			Title: task.Title,
			Body:  fmt.Sprintf("Taskyard 이슈 #%d · Run %s\n\n%s", task.Number, run.ID, task.Body),
		}
	}
	return protocol.NewCommand(protocol.CmdRunStart, run.ID, protocol.RunStartBody{
		Prompt:          prompt,
		RepoPath:        p.RepoPath,
		BaseBranch:      p.DefaultBranch,
		WorkspaceRunID:  run.WorkspaceRunID,
		ResumeSessionID: o.ResumeSession,
		AllowedTools:    p.AllowedTools,
		PR:              pr,
		CleanupMerged:   p.CleanupMerged,
	})
}

// ChainPending은 이어 실행의 규칙을 모든 이슈에 적용한다. 멱등이다: 2단계
// Run이 생기는 순간 그 이슈는 대상에서 빠진다. 러너가 없으면 아무것도 하지
// 않는다 — 만들자마자 failed가 될 Run은 안 만든다; 접속 훅이 다시 부른다.
func (l *Launcher) ChainPending() {
	if !l.Commander.Connected() {
		return
	}
	pending, err := l.Store.TasksAwaitingExecute()
	if err != nil {
		slog.Error("chain: list pending failed", "err", err)
		return
	}
	for _, w := range pending {
		p, err := l.Store.GetProjectByID(w.Task.ProjectID)
		if err != nil {
			continue
		}
		run, err := l.Start(p, w.Task, Options{Stage: store.StageExecute, ReportRunID: w.AnalyzeRun.ID})
		switch {
		case errors.Is(err, ErrRunActive):
			// 겹침 — 다른 쪽이 먼저 만들었다. 정상.
		case err != nil:
			slog.Error("chain: start execute failed", "task_id", w.Task.ID, "err", err)
		default:
			slog.Info("chained execute run", "task_id", w.Task.ID, "run_id", run.ID, "report_run_id", w.AnalyzeRun.ID)
		}
	}
}

// Run은 hub의 정착 신호마다 ChainPending을 돈다. 시작할 때 한 번 먼저.
func (l *Launcher) Run(ctx context.Context, sig Signaler) {
	l.ChainPending()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig.Settled():
			l.ChainPending()
		}
	}
}
