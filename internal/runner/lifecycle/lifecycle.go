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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	Spool *spool.Spool
	// Repos는 허용 저장소 목록이다. run.start의 repo_path를 여기서 해석한다.
	Repos  *RepoResolver
	Broker *approval.Broker
	// BaseBranch는 run.start의 base_branch가 비어 있을 때의 기본값이다.
	BaseBranch   string
	BrokerURL    string
	BrokerToken  string
	ClaudeBinary string
	// GHBinary는 gh 실행 파일(비어 있으면 PATH의 gh). PRPollInterval은 PR 추적
	// 주기(비어 있으면 60s).
	GHBinary       string
	PRPollInterval time.Duration
	Publish        PublishFunc
}

type Manager struct {
	cfg Config

	mu     sync.Mutex
	active map[string]*runHandle
}

// runHandle은 실행 중인 Run 하나에 대해 Manager가 들고 있는 상태다.
// ws/startedAt은 handleRunCancel이 원장에 종결 기록을 남길 때 필요하고,
// cancelled는 execute의 종결 switch가 "failed"와 "cancelled"를 구분하는 데
// 쓴다 — 취소로 죽은 프로세스도 parseErr/waitErr로 나타나기 때문이다.
type runHandle struct {
	cancel    context.CancelFunc
	spec      runSpec
	cancelled bool
}

// runSpec은 Run 하나를 실행하는 데 필요한 전부다. 원장 기록의 공통 필드가
// 여기서 나오므로, 어느 경로에서 기록하든 RepoPath·WorkspaceRunID가 빠지지
// 않는다.
type runSpec struct {
	runID           string
	prompt          string
	resumeSessionID string
	allowedTools    []string
	baseBranch      string
	pr              *protocol.PRSpec // nil이면 PR을 만들지 않는다
	cleanupMerged   bool
	repoPath        string // 원장에 남기는 정규화 경로
	workspaceRunID  string // worktree·브랜치의 주인 Run. 이어서 재시도는 이전 Run
	startedAt       int64
	ws              gitops.Workspace
	git             *gitops.Manager // 이 Run의 저장소 관리자. salvage가 쓴다
}

func (s runSpec) record(state string) spool.RunRecord {
	return spool.RunRecord{
		RunID:          s.runID,
		State:          state,
		Branch:         s.ws.Branch,
		WorktreePath:   s.ws.Path,
		StartedAtUnix:  s.startedAt,
		RepoPath:       s.repoPath,
		WorkspaceRunID: s.workspaceRunID,
		CleanupMerged:  s.cleanupMerged,
	}
}

func New(cfg Config) (*Manager, error) {
	switch {
	case cfg.Spool == nil:
		return nil, errors.New("lifecycle: Spool is required")
	case cfg.Repos == nil:
		return nil, errors.New("lifecycle: Repos is required")
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
	return &Manager{cfg: cfg, active: map[string]*runHandle{}}, nil
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
		// Reconcile은 Run마다 git salvage를 돌릴 수 있어 수 초가 걸릴 수
		// 있다. link.readLoop가 HandleCommand를 동기 호출하므로 여기서
		// 기다리면 그 연결의 ack 처리와 spool 정리가 함께 멈춘다.
		go func() {
			if err := m.Reconcile(context.Background()); err != nil {
				slog.Error("reconcile failed", "err", err)
			}
		}()
		return nil
	default:
		return fmt.Errorf("lifecycle: unknown command %q", env.Type)
	}
}

func (m *Manager) handleRunStart(ctx context.Context, env protocol.Envelope) error {
	var body protocol.RunStartBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return fmt.Errorf("unmarshal run.start body: %w", err)
	}

	git, repoPath, err := m.cfg.Repos.resolve(body.RepoPath)
	if errors.Is(err, ErrRepoNotAllowed) {
		// 허용 목록 밖의 저장소는 명령 처리 오류가 아니라 이 Run의 실패다.
		return m.failBeforeStart(env, body, body.RepoPath, err)
	}
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	if m.otherRunActive(env.RunID) {
		return m.failBeforeStart(env, body, repoPath, errRunnerBusy)
	}

	baseBranch := body.BaseBranch
	if baseBranch == "" {
		baseBranch = m.cfg.BaseBranch
	}

	// worktree·브랜치는 workspace 주인 Run의 이름으로 정해진다. 이어서 재시도는
	// 이전 Run의 것을 그대로 쓰고, 그 밖에는 자기 자신이다(PRD §7.6).
	wsID := body.WorkspaceRunID
	if wsID == "" {
		wsID = env.RunID
	}
	ws, err := git.Ensure(ctx, wsID, baseBranch)
	if err != nil {
		return fmt.Errorf("ensure worktree: %w", err)
	}

	// 멱등성 관문. 실패할 수 있는 준비(본문 해석, worktree 보장)가 모두 끝난
	// 뒤에야 명령을 "적용됨"으로 기록한다. 더 앞에서 기록하면 그 사이의
	// 실패가 재전송을 영원히 무시당하게 만든다 — 명령 로그 자신이 재시도
	// 경로를 막아버리는 셈이다. Ensure는 멱등이므로(Task 6), 이 관문을
	// 통과하기 전에 같은 command_id가 다시 와도 같은 worktree를 재사용할
	// 뿐 새로 만들지 않는다.
	//
	// 이 관문을 SaveRun보다도 앞에 두는 이유는 따로 있다: 관문을 통과하는
	// 것 자체가 "이번이 이 command_id의 처음이자 유일한 적용"이라는 뜻이어야
	// 한다. SaveRun을 관문보다 먼저 하면, 이미 끝난 실행이 재전송됐을 때
	// SaveRun이 원장의 종결 상태(SessionID·PID 포함)를 "running"으로
	// 되돌려 쓴 뒤에야 관문에서 멈추게 된다.
	_, first, err := m.cfg.Spool.RememberCommand(env.ID, []byte(`{"accepted":true}`))
	if err != nil {
		return fmt.Errorf("remember command: %w", err)
	}
	if !first {
		slog.Info("ignoring replayed run.start", "command_id", env.ID, "run_id", env.RunID)
		return nil
	}

	spec := runSpec{
		runID:           env.RunID,
		resumeSessionID: body.ResumeSessionID,
		allowedTools:    body.AllowedTools,
		baseBranch:      baseBranch,
		pr:              body.PR,
		cleanupMerged:   body.CleanupMerged,
		repoPath:        repoPath,
		workspaceRunID:  wsID,
		startedAt:       time.Now().Unix(),
		ws:              ws,
		git:             git,
	}
	if err := m.cfg.Spool.SaveRun(spec.record("running")); err != nil {
		return fmt.Errorf("save run record: %w", err)
	}

	m.emitState(env.RunID, "running", "")

	// {{memory}}는 Server가 아니라 여기서 채운다 — 기억 파일은 저장소 안에 있고
	// Server에는 저장소가 없다(PRD §8.4 ME-01). 다른 토큰은 손대지 않는다.
	spec.prompt = replaceMemoryToken(body.Prompt, ws.Path)

	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.active[env.RunID] = &runHandle{cancel: cancel, spec: spec}
	m.mu.Unlock()

	go m.execute(runCtx, cancel, spec)
	return nil
}

// maxMemoryBytes는 프롬프트에 주입하는 기억 파일의 상한이다. 그 위는 잘라
// 내고 잘렸음을 알린다. maxAttentionBytes는 멈춤 보고의 상한이다.
const (
	maxMemoryBytes    = 64 * 1024
	maxAttentionBytes = 4 * 1024
	attentionFile     = ".taskyard/attention.md"
)

// replaceMemoryToken은 prompt의 {{memory}}를 worktree의 .taskyard/memory.md
// 내용으로 바꾼다. 파일이 없으면 빈 문자열이다. strings.ReplaceAll은 넣은
// 값을 다시 훑지 않으므로 기억 파일 안에 {{memory}}가 있어도 그대로다.
func replaceMemoryToken(prompt, worktree string) string {
	const token = "{{memory}}"
	if !strings.Contains(prompt, token) {
		return prompt
	}
	memory, truncated, _ := readWorktreeFile(worktree, ".taskyard/memory.md", maxMemoryBytes)
	if truncated {
		memory += "\n…(memory.md truncated at 64KiB)\n"
	}
	return strings.ReplaceAll(prompt, token, memory)
}

// readWorktreeFile은 worktree 안의 파일을 읽되 두 가지를 지킨다. 저장소는
// 에이전트가 쓰는 곳이므로 (1) worktree 밖을 가리키는 심링크는 읽지 않고
// (2) 크기를 max로 자른다. 없거나 읽을 수 없거나 밖을 가리키면 ok=false.
func readWorktreeFile(worktree, rel string, max int) (content string, truncated, ok bool) {
	path := filepath.Join(worktree, rel)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, false
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", false, false
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		slog.Warn("worktree file escapes the worktree; ignoring", "path", path, "target", real)
		return "", false, false
	}

	f, err := os.Open(real)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", false, false
	}
	if len(buf) > max {
		return string(buf[:max]), true, true
	}
	return string(buf), false, true
}

// takeAttention은 에이전트가 남긴 멈춤 보고(PRD §7.5)를 읽고 파일을 지운다.
// 지우는 이유: 이어서 재시도가 같은 worktree를 쓰므로 남기면 다음 Run이
// 즉시 다시 멈춘다. 파일이 없거나 비어 있거나 읽을 수 없으면 보고가 없는
// 것이다. 에이전트가 파일을 커밋해 버렸다면 삭제가 미커밋 변경으로 남고
// 다음 Run이나 salvage가 그 삭제를 실어 간다 — 결정적이고 무해하다.
func takeAttention(worktree string) (string, bool) {
	reason, truncated, ok := readWorktreeFile(worktree, attentionFile, maxAttentionBytes)
	if !ok {
		return "", false
	}
	// 비어 있어도 지운다 — 남기면 salvage가 커밋한다.
	if err := os.Remove(filepath.Join(worktree, attentionFile)); err != nil {
		slog.Warn("could not remove attention file", "err", err)
	}
	if strings.TrimSpace(reason) == "" {
		return "", false
	}
	if truncated {
		reason += "\n…(attention.md truncated at 4KiB)"
	}
	return reason, true
}

// errRunnerBusy는 활성 Run이 있는 동안 온 다른 run.start의 실패 이유다.
// 러너는 동시에 하나만 돌린다 — runIDForApproval이 그 가정에 기대고, 동시
// 실행 한도는 Phase 1 뒤쪽 항목이다(PRD EX-10).
var errRunnerBusy = errors.New("runner busy: another run is active (concurrency limit 1)")

// otherRunActive는 env.RunID가 아닌 Run이 돌고 있는지 본다. 같은 Run의
// 재전송은 바쁨이 아니라 멱등성 관문의 몫이다.
func (m *Manager) otherRunActive(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.active {
		if id != runID {
			return true
		}
	}
	return false
}

// failBeforeStart는 프로세스를 띄우기 전에 정해진 실패(허용 목록 밖 저장소,
// 바쁜 러너)를 기록한다. 관문을 먼저 통과시켜 재전송이 같은 실패를 다시
// 발행하지 않게 하고, 원장에는 요청받은 경로를 그대로 남긴다.
func (m *Manager) failBeforeStart(env protocol.Envelope, body protocol.RunStartBody, repoPath string, cause error) error {
	_, first, err := m.cfg.Spool.RememberCommand(env.ID, []byte(`{"accepted":true}`))
	if err != nil {
		return fmt.Errorf("remember command: %w", err)
	}
	if !first {
		return nil
	}
	wsID := body.WorkspaceRunID
	if wsID == "" {
		wsID = env.RunID
	}
	_ = m.cfg.Spool.SaveRun(spool.RunRecord{
		RunID:          env.RunID,
		State:          "failed",
		StartedAtUnix:  time.Now().Unix(),
		RepoPath:       repoPath,
		WorkspaceRunID: wsID,
	})
	m.emitTerminal(env.RunID, "failed", cause.Error(), "", "")
	return nil
}

func (m *Manager) execute(ctx context.Context, cancel context.CancelFunc, spec runSpec) {
	runID := spec.runID
	defer func() {
		m.mu.Lock()
		delete(m.active, runID)
		m.mu.Unlock()
	}()

	// Agent 프로세스를 아예 띄우지 못한 종결. 어떤 세션도 시작되기 전이므로
	// SessionID·PID는 비어 있다. 취소가 cmd.Start보다 먼저 도착하면 Start가
	// 취소된 ctx로 실패하는데, 그것은 실패가 아니라 취소다 — terminalState가
	// 가른다.
	earlyFail := func(err error) {
		state := m.terminalState(runID)
		detail := m.terminalDetail(runID, err)
		if state != "cancelled" {
			slog.Error("run failed", "run_id", runID, "err", err)
		}
		_ = m.cfg.Spool.SaveRun(spec.record(state))
		m.emitTerminal(runID, state, detail, "", "")
	}

	args, err := claudecode.BuildArgs(claudecode.SpawnOptions{
		Prompt:          spec.prompt,
		WorkDir:         spec.ws.Path,
		ResumeSessionID: spec.resumeSessionID,
		AllowedTools:    spec.allowedTools,
		BrokerURL:       m.cfg.BrokerURL,
		BrokerToken:     m.cfg.BrokerToken,
	})
	if err != nil {
		earlyFail(fmt.Errorf("build args: %w", err))
		return
	}

	cmd := exec.CommandContext(ctx, m.cfg.ClaudeBinary, args...)
	cmd.Dir = spec.ws.Path
	cmd.Env = claudecode.ScrubEnv(os.Environ())
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		earlyFail(fmt.Errorf("stdout pipe: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		earlyFail(fmt.Errorf("start agent: %w", err))
		return
	}

	// PID를 확보하는 즉시 원장에 반영한다. 재시작 후 조정은 이 PID로 프로세스
	// 생존을 판정하므로(PRD §11.7), 0으로 남아 있으면 살아 있는 Run을 영원히
	// 알아보지 못한다.
	pid := cmd.Process.Pid
	running := spec.record("running")
	running.PID = pid
	if err := m.cfg.Spool.SaveRun(running); err != nil {
		slog.Error("save run pid failed", "run_id", runID, "err", err)
	}

	parser := claudecode.NewParser()

	// 구독 경계 검사는 init 이벤트가 도착하는 즉시, 이 콜백 안에서 한다.
	// cmd.Wait() 뒤로 미루면 API 과금으로 이미 턴이 끝난 뒤에야 실패
	// 처리하게 되어 실질적 과금을 막지 못한다(PRD §13.2). billingChecked는
	// Parse가 단일 goroutine에서 emit을 순차 호출하므로 잠금 없이 안전하다.
	//
	// SessionID != ""는 init이 왔다는 것만 증명한다. 구독 로그인이라는
	// 증거는 apiKeySource == "none"뿐이다 — 비어 있거나 다른 값이면 어느
	// 신원으로 과금됐는지 증명되지 않은 것이므로 똑같이 막는다.
	billingChecked := false
	parseErr := parser.Parse(stdout, func(e adapter.Event) error {
		if !billingChecked {
			if info := parser.Session(); info.SessionID != "" {
				billingChecked = true
				if info.APIKeySource != "none" {
					cancel()
					return fmt.Errorf("agent's billing identity is not a verified subscription login (apiKeySource=%q); refusing to continue", info.APIKeySource)
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

	// 종결은 여기 한 곳에서만 결정하고 발행한다. 취소로 죽은 프로세스도
	// parseErr/waitErr로 나타나므로 terminalState()가 cancelled를 고른다 —
	// handleRunCancel은 이벤트를 보내지 않는다. 그렇지 않으면 cancelled 뒤에
	// failed가 따라와 서버가 failed로 끝난다(Phase 0 버그).
	//
	// 반드시 보존이 먼저다(Reconcile과 같은 순서). 여기서 죽으면 재시작 후
	// Reconcile이 이미 종결 상태를 보고 이 Run을 다시 살펴보지 않으므로,
	// salvage를 SaveRun보다 앞에 둬야 그 창에서 실행이 끊겨도 보존이
	// 유실되지 않는다.
	//
	// .taskyard/ 산출물(변경 설명, 멈춤 보고)은 salvage보다 먼저 거둔다 —
	// salvage는 git add -A라 그대로 두면 커밋에 들어간다.
	finish := func(state, detail string, salvageFirst bool) {
		summary := takeSummary(spec.ws.Path)
		attention, hasAttention := takeAttention(spec.ws.Path)
		if salvageFirst {
			m.salvage(runID, spec.workspaceRunID, spec.git)
		}
		// 정상 종료. 에이전트가 멈춤 보고를 남겼으면 성공이 아니라 사람의
		// 차례다(PRD §7.5). 실패·취소가 이 분기보다 앞서므로 실패가 우선한다.
		if hasAttention {
			if state == "succeeded" {
				state, detail = "needs_attention", attention
			} else {
				// 실패·취소가 우선한다. 보고는 버리지 않고 detail에 덧붙인다.
				detail += "\n\n에이전트 메모:\n" + attention
			}
		}
		rec := spec.record(state)
		rec.SessionID = session.SessionID
		rec.PID = pid
		if state == "succeeded" && spec.pr != nil {
			state, detail = m.publishPR(spec, &rec, summary)
			rec.State = state
		}
		_ = m.cfg.Spool.SaveRun(rec)
		if state != "succeeded" && state != "needs_attention" {
			slog.Error("run failed", "run_id", runID, "state", state, "detail", detail)
		}
		m.emitTerminal(runID, state, detail, session.SessionID, summary)
	}

	switch {
	case parseErr != nil:
		finish(m.terminalState(runID), m.terminalDetail(runID, fmt.Errorf("parse stream: %w", parseErr)), true)
	case waitErr != nil:
		finish(m.terminalState(runID), m.terminalDetail(runID, fmt.Errorf("agent exited: %w", waitErr)), true)
	case session.APIKeySource != "none":
		// init이 아예 없었거나(APIKeySource == "") 왔지만 구독 로그인임을
		// 증명하지 못했다. system/init은 emit하지 않으므로, emit 콜백의
		// 조기 검사가 한 번도 실행되지 못한 경우(예: init 다음에 곧바로
		// result만 오는 스트림)의 마지막 방어선이기도 하다(PRD §13.2).
		finish("failed", fmt.Sprintf("agent's billing identity is not a verified subscription login (apiKeySource=%q); refusing to continue", session.APIKeySource), true)
	default:
		finish("succeeded", "", false)
	}
}

// terminalState는 parseErr/waitErr로 끝난 Run이 사람이 취소해서 죽었는지,
// 아니면 정말로 실패했는지 구분한다. 취소된 Run도 프로세스가 죽으면서
// parseErr나 waitErr로 나타나므로, m.active에 남은 cancelled 플래그를 봐야만
// 구분할 수 있다.
func (m *Manager) terminalState(runID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.active[runID]; ok && h.cancelled {
		return "cancelled"
	}
	return "failed"
}

// terminalDetail은 취소면 사람의 행동을, 아니면 실패 원인을 설명한다.
func (m *Manager) terminalDetail(runID string, cause error) string {
	if m.terminalState(runID) == "cancelled" {
		return "cancelled by user"
	}
	return cause.Error()
}

// salvage는 종료 전 미커밋 변경을 보존한다(PRD §8.7.1). worktree는 wsID의
// 것이고, 보존 이벤트는 runID의 것이다.
func (m *Manager) salvage(runID, wsID string, git *gitops.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sha, saved, err := git.Salvage(ctx, wsID)
	if err != nil {
		slog.Error("salvage failed", "run_id", runID, "workspace", wsID, "err", err)
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
	h, ok := m.active[env.RunID]
	if ok {
		// cancel()보다 먼저 표시해야 한다. execute의 종결 switch가 이
		// 플래그로 "failed"와 "cancelled"를 가르기 때문이다.
		h.cancelled = true
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}
	h.cancel()
	// 원장에는 남기되 이벤트는 보내지 않는다. 종결 이벤트는 프로세스가 죽은
	// 뒤 execute가 한 곳에서 보낸다(cancelled 뒤에 failed가 따라오는 것을
	// 막는다). PID·SessionID는 execute의 기록이 이 기록 뒤에 와서 채운다.
	_ = m.cfg.Spool.SaveRun(h.spec.record("cancelled"))
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

// emitState는 비종결 상태(running)를 알린다.
func (m *Manager) emitState(runID, state, detail string) {
	m.publishState(runID, map[string]any{"state": state, "detail": detail})
}

// emitTerminal은 종결·정착 상태를 알린다. session_id를 함께 실어 Server가
// 이어서 재시도에 쓸 세션을 알게 한다. 비어 있으면 싣지 않는다 — Server는
// 알던 세션을 빈 값으로 지우지 않는다.
func (m *Manager) emitTerminal(runID, state, detail, sessionID, summary string) {
	body := map[string]any{"state": state, "detail": detail}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	if summary != "" {
		body["summary"] = summary
	}
	m.publishState(runID, body)
}

func (m *Manager) publishState(runID string, body map[string]any) {
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, 0, eventBody{Body: body})
	if err != nil {
		slog.Error("build state event failed", "err", err)
		return
	}
	if err := m.cfg.Publish(runID, env); err != nil {
		slog.Error("publish state event failed", "run_id", runID, "err", err)
	}
}
