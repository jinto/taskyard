// Package lifecycle은 run.start 명령 하나를 실제 실행으로 바꾼다.
//
// worktree 보장 → Agent 기동 → 이벤트 정규화 및 발행 → 종료 처리 순이다.
// 명령 멱등성은 spool의 command_log가 담당하므로 같은 command_id가
// 다시 와도 두 번 실행되지 않는다(PRD §11.7).
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/agents/adapter/claudecode"
	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/spool"
)

// PublishFunc은 정규화 이벤트를 Server로 보낸다. 보통 link.Publish다.
type PublishFunc func(runID string, env protocol.Envelope) error

type Config struct {
	Spool        *spool.Spool
	Git          *gitops.Manager
	Broker       *approval.Broker
	BaseBranch   string
	BrokerURL    string
	BrokerToken  string
	ClaudeBinary string
	Publish      PublishFunc
}

type Manager struct {
	cfg Config

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(cfg Config) (*Manager, error) {
	switch {
	case cfg.Spool == nil:
		return nil, errors.New("lifecycle: Spool is required")
	case cfg.Git == nil:
		return nil, errors.New("lifecycle: Git is required")
	case cfg.Broker == nil:
		return nil, errors.New("lifecycle: Broker is required")
	case cfg.Publish == nil:
		return nil, errors.New("lifecycle: Publish is required")
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	return &Manager{cfg: cfg, active: map[string]context.CancelFunc{}}, nil
}

// eventBody는 lifecycle이 발행하는 모든 이벤트의 공통 껍데기다. Task 11의
// 렌더러는 이 모양(body/raw)만 이해하므로, Publish로 나가는 이벤트는
// 어디서 만들든 예외 없이 이 타입으로 감싼다.
type eventBody struct {
	Body map[string]any  `json:"body,omitempty"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

// Start는 브로커의 승인 요청을 Server로 올리는 루프다.
func (m *Manager) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-m.cfg.Broker.Requests():
			env, err := protocol.NewEvent(protocol.EvApprovalRequested, m.runIDForApproval(), 0, eventBody{
				Body: map[string]any{
					"request_id":  req.ID,
					"tool_name":   req.ToolName,
					"tool_use_id": req.ToolUseID,
					"input":       req.Input,
				},
			})
			if err != nil {
				slog.Error("build approval event failed", "err", err)
				continue
			}
			if err := m.cfg.Publish(env.RunID, env); err != nil {
				slog.Error("publish approval event failed", "err", err)
			}
		}
	}
}

// runIDForApproval은 스파이크의 단순화다. 동시에 한 Run만 실행하므로
// 현재 활성 Run을 승인의 주인으로 본다. Phase 1에서 브로커가 Run별
// 엔드포인트를 갖도록 바꾼다.
func (m *Manager) runIDForApproval() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for runID := range m.active {
		return runID
	}
	return "unknown"
}

// HandleCommand는 Server 명령 하나를 처리한다. run.start 외에는 모두
// 즉시 반환해야 한다 — link.readLoop가 동기로 호출하므로, 여기서
// 블록되면 그 연결의 ack 처리와 spool 정리가 함께 멈춘다.
func (m *Manager) HandleCommand(ctx context.Context, env protocol.Envelope) error {
	switch env.Type {
	case protocol.CmdRunStart:
		return m.handleRunStart(ctx, env)
	case protocol.CmdRunCancel:
		return m.handleRunCancel(env)
	case protocol.CmdApprovalDecision:
		return m.handleApprovalDecision(env)
	case protocol.CmdRunReconcile:
		// Task 10이 m.Reconcile을 구현하면 이 한 줄만 바뀐다.
		return errors.New("run.reconcile: not implemented until Task 10")
	default:
		return fmt.Errorf("lifecycle: unknown command %q", env.Type)
	}
}

type runStartBody struct {
	Prompt string `json:"prompt"`
}

func (m *Manager) handleRunStart(ctx context.Context, env protocol.Envelope) error {
	// 멱등성 관문. 이미 본 command_id면 아무것도 하지 않는다.
	_, first, err := m.cfg.Spool.RememberCommand(env.ID, []byte(`{"accepted":true}`))
	if err != nil {
		return fmt.Errorf("remember command: %w", err)
	}
	if !first {
		slog.Info("ignoring replayed run.start", "command_id", env.ID, "run_id", env.RunID)
		return nil
	}

	var body runStartBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return fmt.Errorf("unmarshal run.start body: %w", err)
	}

	ws, err := m.cfg.Git.Ensure(ctx, env.RunID, m.cfg.BaseBranch)
	if err != nil {
		return fmt.Errorf("ensure worktree: %w", err)
	}

	startedAt := time.Now().Unix()
	if err := m.cfg.Spool.SaveRun(spool.RunRecord{
		RunID:         env.RunID,
		State:         "running",
		Branch:        ws.Branch,
		WorktreePath:  ws.Path,
		StartedAtUnix: startedAt,
	}); err != nil {
		return fmt.Errorf("save run record: %w", err)
	}

	m.emitState(env.RunID, "running", "")

	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.active[env.RunID] = cancel
	m.mu.Unlock()

	go m.execute(runCtx, cancel, env.RunID, body.Prompt, startedAt, ws)
	return nil
}

func (m *Manager) execute(ctx context.Context, cancel context.CancelFunc, runID, prompt string, startedAt int64, ws gitops.Workspace) {
	defer func() {
		m.mu.Lock()
		delete(m.active, runID)
		m.mu.Unlock()
	}()

	args, err := claudecode.BuildArgs(claudecode.SpawnOptions{
		Prompt:      prompt,
		WorkDir:     ws.Path,
		BrokerURL:   m.cfg.BrokerURL,
		BrokerToken: m.cfg.BrokerToken,
	})
	if err != nil {
		m.fail(runID, fmt.Errorf("build args: %w", err))
		return
	}

	cmd := exec.CommandContext(ctx, m.cfg.ClaudeBinary, args...)
	cmd.Dir = ws.Path
	cmd.Env = claudecode.ScrubEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail(runID, fmt.Errorf("stdout pipe: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		m.fail(runID, fmt.Errorf("start agent: %w", err))
		return
	}

	parser := claudecode.NewParser()

	// 구독 경계 검사는 init 이벤트가 도착하는 즉시, 이 콜백 안에서 한다.
	// cmd.Wait() 뒤로 미루면 API 과금으로 이미 턴이 끝난 뒤에야 실패
	// 처리하게 되어 실질적 과금을 막지 못한다(PRD §13.2). billingChecked는
	// Parse가 단일 goroutine에서 emit을 순차 호출하므로 잠금 없이 안전하다.
	billingChecked := false
	parseErr := parser.Parse(stdout, func(e adapter.Event) error {
		if !billingChecked {
			if info := parser.Session(); info.SessionID != "" {
				billingChecked = true
				if info.UsesAPIKey() {
					cancel()
					return fmt.Errorf("agent ran on API billing (apiKeySource=%q); refusing to continue", info.APIKeySource)
				}
			}
		}

		env, err := protocol.NewEvent(e.Type, runID, 0, eventBody{Body: e.Body, Raw: e.Raw})
		if err != nil {
			return err
		}
		return m.cfg.Publish(runID, env)
	})

	waitErr := cmd.Wait()
	session := parser.Session()

	record := spool.RunRecord{
		RunID:         runID,
		SessionID:     session.SessionID,
		Branch:        ws.Branch,
		WorktreePath:  ws.Path,
		StartedAtUnix: startedAt,
	}

	switch {
	case parseErr != nil:
		record.State = "failed"
		_ = m.cfg.Spool.SaveRun(record)
		m.salvage(runID)
		m.fail(runID, fmt.Errorf("parse stream: %w", parseErr))
	case waitErr != nil:
		record.State = "failed"
		_ = m.cfg.Spool.SaveRun(record)
		m.salvage(runID)
		m.fail(runID, fmt.Errorf("agent exited: %w", waitErr))
	case session.SessionID == "":
		// init이 오기 전에 스트림이 끝났다. 어떤 신원으로 과금됐는지
		// 확인할 방법이 없으므로 성공으로 볼 수 없다(PRD §13.2).
		record.State = "failed"
		_ = m.cfg.Spool.SaveRun(record)
		m.salvage(runID)
		m.fail(runID, errors.New("stream ended before init; billing identity unverified"))
	default:
		record.State = "succeeded"
		_ = m.cfg.Spool.SaveRun(record)
		m.emitState(runID, "succeeded", "")
	}
}

// salvage는 종료 전 미커밋 변경을 보존한다(PRD §8.7.1).
func (m *Manager) salvage(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sha, saved, err := m.cfg.Git.Salvage(ctx, runID)
	if err != nil {
		slog.Error("salvage failed", "run_id", runID, "err", err)
		return
	}
	if !saved {
		return
	}

	env, err := protocol.NewEvent(protocol.EvFileChanged, runID, 0, eventBody{
		Body: map[string]any{
			"kind": "salvage",
			"sha":  sha,
		},
	})
	if err != nil {
		return
	}
	_ = m.cfg.Publish(runID, env)
}

func (m *Manager) handleRunCancel(env protocol.Envelope) error {
	m.mu.Lock()
	cancel, ok := m.active[env.RunID]
	m.mu.Unlock()

	if !ok {
		return nil
	}
	cancel()
	m.emitState(env.RunID, "cancelled", "cancelled by user")
	return nil
}

type approvalDecisionBody struct {
	RequestID string          `json:"request_id"`
	Allow     bool            `json:"allow"`
	Message   string          `json:"message"`
	Updated   json.RawMessage `json:"updated_input"`
}

func (m *Manager) handleApprovalDecision(env protocol.Envelope) error {
	var body approvalDecisionBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return fmt.Errorf("unmarshal approval decision: %w", err)
	}
	return m.cfg.Broker.Decide(body.RequestID, approval.Decision{
		Allow:        body.Allow,
		Message:      body.Message,
		UpdatedInput: body.Updated,
	})
}

func (m *Manager) emitState(runID, state, detail string) {
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, 0, eventBody{
		Body: map[string]any{
			"state":  state,
			"detail": detail,
		},
	})
	if err != nil {
		slog.Error("build state event failed", "err", err)
		return
	}
	if err := m.cfg.Publish(runID, env); err != nil {
		slog.Error("publish state event failed", "run_id", runID, "err", err)
	}
}

func (m *Manager) fail(runID string, cause error) {
	slog.Error("run failed", "run_id", runID, "err", cause)
	m.emitState(runID, "failed", cause.Error())
}
