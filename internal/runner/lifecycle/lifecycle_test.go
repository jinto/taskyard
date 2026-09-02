package lifecycle_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/spool"
)

const pongFixture = "../../agents/adapter/claudecode/testdata/session-pong.ndjson"

// fakeClaude는 실제 CLI 대신 fixture NDJSON을 그대로 뱉는 스크립트다.
// 사용자 구독 할당량을 쓰지 않고 배선 전체를 검증할 수 있다.
func fakeClaude(t *testing.T, fixture string) string {
	t.Helper()
	return fakeClaudeRecording(t, fixture, "")
}

// fakeClaudeRecording은 fakeClaude와 같되, 받은 인자 전부를 argsFile에
// 한 줄씩 남긴다. 러너가 실제로 어떤 프롬프트를 넘겼는지 검증할 때 쓴다.
func fakeClaudeRecording(t *testing.T, fixture, argsFile string) string {
	t.Helper()

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n"
	if argsFile != "" {
		// 프롬프트에 줄바꿈이 들어가므로 인자 구분자는 NUL이다.
		script += "printf '%s\\0' \"$@\" > " + argsFile + "\n"
	}
	script += "cat " + abs + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newRepo는 커밋 하나가 있는 저장소와 빈 worktree 루트를 만든다.
func newRepo(t *testing.T) (repo, worktrees string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	worktrees = filepath.Join(base, "wt")

	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "config", "user.name", "Test")
	gitIn(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "README.md")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	return repo, worktrees
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// commitFile은 저장소의 현재 브랜치에 파일 하나를 커밋한다.
func commitFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", rel)
	gitIn(t, repo, "commit", "-q", "-m", "add "+rel)
}

// canonical은 RepoResolver가 원장에 남기는 형태의 경로다.
func canonical(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

type collector struct {
	mu     sync.Mutex
	events []protocol.Envelope
}

func (c *collector) publish(_ string, env protocol.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, env)
	return nil
}

func (c *collector) snapshot() []protocol.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.Envelope, len(c.events))
	copy(out, c.events)
	return out
}

func (c *collector) types() []string {
	events := c.snapshot()
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func (c *collector) count(evType string) int {
	n := 0
	for _, t := range c.types() {
		if t == evType {
			n++
		}
	}
	return n
}

// harness는 저장소 하나(또는 여럿)를 허용하는 Manager와 그 주변 부품이다.
type harness struct {
	m         *lifecycle.Manager
	sp        *spool.Spool
	repos     *lifecycle.RepoResolver
	repo      string // 첫 번째 허용 저장소
	worktrees string
	git       *gitops.Manager // repo의 관리자
	broker    *approval.Broker
}

type harnessOpt func(*harnessOpts)

type harnessOpts struct {
	binary     string
	extraRepos []string
	baseBranch string
}

func withBinary(path string) harnessOpt   { return func(o *harnessOpts) { o.binary = path } }
func withRepos(paths ...string) harnessOpt { return func(o *harnessOpts) { o.extraRepos = paths } }
func withBaseBranch(b string) harnessOpt   { return func(o *harnessOpts) { o.baseBranch = b } }

func newHarness(t *testing.T, col *collector, opts ...harnessOpt) *harness {
	t.Helper()
	o := harnessOpts{baseBranch: "main"}
	for _, opt := range opts {
		opt(&o)
	}

	repo, worktrees := newRepo(t)
	if o.binary == "" {
		o.binary = fakeClaude(t, pongFixture)
	}

	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	repos, err := lifecycle.NewRepoResolver(append([]string{repo}, o.extraRepos...), worktrees)
	if err != nil {
		t.Fatal(err)
	}
	git, err := repos.Manager(repo)
	if err != nil {
		t.Fatal(err)
	}

	broker := approval.New("tok")
	m, err := lifecycle.New(lifecycle.Config{
		Spool:        sp,
		Repos:        repos,
		Broker:       broker,
		BaseBranch:   o.baseBranch,
		BrokerURL:    "http://127.0.0.1:9999/mcp",
		BrokerToken:  "tok",
		ClaudeBinary: o.binary,
		Publish:      col.publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{m: m, sp: sp, repos: repos, repo: repo, worktrees: worktrees, git: git, broker: broker}
}

// start는 이 harness의 첫 저장소를 가리키는 run.start 명령이다.
func (h *harness) start(t *testing.T, runID, prompt string) protocol.Envelope {
	t.Helper()
	return startCommand(t, runID, protocol.RunStartBody{Prompt: prompt, RepoPath: h.repo})
}

func startCommand(t *testing.T, runID string, body protocol.RunStartBody) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewCommand(protocol.CmdRunStart, runID, body)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// failureDetail은 발행된 이벤트에서 종결 "failed" 상태의 detail을 꺼낸다.
// 없으면 빈 문자열이다(예: 이 run이 성공으로 끝난 경우).
func failureDetail(t *testing.T, col *collector) string {
	t.Helper()
	for _, e := range col.snapshot() {
		if e.Type != protocol.EvRunStateChanged {
			continue
		}
		var wrapper struct {
			Body map[string]any `json:"body"`
		}
		if err := json.Unmarshal(e.Body, &wrapper); err != nil {
			t.Fatal(err)
		}
		if state, _ := wrapper.Body["state"].(string); state == "failed" {
			detail, _ := wrapper.Body["detail"].(string)
			return detail
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitTerminal은 running 뒤의 종결 상태 변경까지 기다린다. turn_completed는
// 스트림 처리 중에 나오지만 종결 상태와 원장 저장은 cmd.Wait() 뒤에 따로
// 오므로, 곧바로 확인하지 않고 여기서 기다린다.
func waitTerminal(t *testing.T, col *collector) {
	t.Helper()
	waitFor(t, "terminal state change", func() bool {
		return col.count(protocol.EvRunStateChanged) >= 2
	})
}

func loadOnlyRun(t *testing.T, sp *spool.Spool) spool.RunRecord {
	t.Helper()
	runs, err := sp.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ledger has %d runs, want 1: %+v", len(runs), runs)
	}
	return runs[0]
}

func TestRunStartCreatesWorktreeAndStreamsEvents(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}

	waitFor(t, "turn_completed", func() bool {
		return col.count(protocol.EvTurnCompleted) == 1
	})
	waitTerminal(t, col)

	if col.count(protocol.EvMessageDelta) != 1 {
		t.Errorf("message_delta count = %d, want 1 (types: %v)", col.count(protocol.EvMessageDelta), col.types())
	}

	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), "README.md")); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}

	rec := loadOnlyRun(t, h.sp)
	if rec.SessionID == "" {
		t.Error("ledger did not record the provider session id; resume would be impossible")
	}
	if rec.PID == 0 {
		t.Error("ledger did not record the agent PID; restart reconcile classifies liveness from it")
	}
}

func TestDuplicateRunStartIsAppliedOnce(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := h.start(t, "run-1", "say pong")
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first run to finish", func() bool { return col.count(protocol.EvTurnCompleted) == 1 })

	// 같은 command_id 재전송. 두 번째 실행도, 두 번째 worktree도 없어야 한다.
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("replayed command returned an error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := col.count(protocol.EvTurnCompleted); got != 1 {
		t.Fatalf("turn_completed count = %d, want 1 after a replayed command", got)
	}
}

// TestRunFailsWhenAgentBillsToAPIKey는 init이 API 키 등 구독이 아닌 경로로
// 과금됐다고 밝히는 경우를 검증한다. emit 콜백의 조기 검사가 첫 emit 대상
// 이벤트(여기서는 result 한 줄) 이전에 취소해야 하므로, turn_completed도
// message_delta도 하나도 발행되지 않아야 한다.
func TestRunFailsWhenAgentBillsToAPIKey(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(fakeClaude(t, "testdata/session-apikey-billing.ndjson")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	if got := col.count(protocol.EvTurnCompleted); got != 0 {
		t.Errorf("turn_completed count = %d, want 0 (billing abort must land before the event is published)", got)
	}
	if got := col.count(protocol.EvMessageDelta); got != 0 {
		t.Errorf("message_delta count = %d, want 0", got)
	}
	if detail := failureDetail(t, col); !strings.Contains(detail, "billing") {
		t.Errorf("failure detail = %q, want it to name the billing boundary", detail)
	}
	if rec := loadOnlyRun(t, h.sp); rec.State != "failed" {
		t.Fatalf("ledger = %+v, want a failed run", rec)
	}
}

// TestRunFailsWhenStreamEndsWithoutInit은 init이 전혀 오지 않은 채 스트림이
// 끝나는 경우를 검증한다. 어떤 신원으로 과금됐는지 확인할 방법이 없으므로
// 성공으로 볼 수 없다.
func TestRunFailsWhenStreamEndsWithoutInit(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(fakeClaude(t, "testdata/session-no-init.ndjson")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	if detail := failureDetail(t, col); detail == "" {
		t.Fatal("expected a failure reason; run must not succeed without an established session")
	}
	if rec := loadOnlyRun(t, h.sp); rec.State != "failed" {
		t.Fatalf("ledger = %+v, want a failed run", rec)
	}
}

// TestRunFailsWhenInitOmitsAPIKeySource는 init은 왔지만 apiKeySource 필드
// 자체가 없는(빈 문자열인) 경우를 검증한다. SessionID != ""만으로는 구독
// 과금인지 증명하지 못한다 — apiKeySource == "none"이어야 증명된다.
func TestRunFailsWhenInitOmitsAPIKeySource(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(fakeClaude(t, "testdata/session-init-missing-apikeysource.ndjson")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	if detail := failureDetail(t, col); detail == "" {
		t.Fatal("expected a failure reason; apiKeySource missing from init proves nothing about billing")
	}
	if rec := loadOnlyRun(t, h.sp); rec.State != "failed" {
		t.Fatalf("ledger = %+v, want a failed run", rec)
	}
}

// dirtyThenFail은 worktree를 더럽힌 뒤 0이 아닌 코드로 종료하는 가짜 Agent다.
func dirtyThenFail(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "dirty-then-fail")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho dirty > uncommitted.txt\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestRunSalvagesUncommittedWorkWhenAgentExitsNonZero은 execute의 waitErr
// 분기를 검증한다. salvage가 실제로 커밋을 남기는지 확인해, SaveRun보다
// salvage를 앞에 두는 순서에서도 보존 자체는 여전히 일어남을 보장한다.
func TestRunSalvagesUncommittedWorkWhenAgentExitsNonZero(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(dirtyThenFail(t)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	if rec := loadOnlyRun(t, h.sp); rec.State != "failed" {
		t.Fatalf("ledger = %+v, want a failed run", rec)
	}

	log, err := exec.Command("git", "-C", h.git.WorktreePath("run-1"), "log", "--oneline", "-1").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, log)
	}
	if !strings.Contains(string(log), "taskyard salvage run-1") {
		t.Fatalf("salvage commit missing, git log: %s", log)
	}
}

// ---- Phase 1 척추: 저장소 지정, 허용 목록, base branch, {{memory}} ----

func TestRunStartUsesRepoFromBody(t *testing.T) {
	col := &collector{}
	second, _ := newRepo(t)
	h := newHarness(t, col, withRepos(second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startCommand(t, "run-1", protocol.RunStartBody{Prompt: "say pong", RepoPath: second})
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	secondGit, err := h.repos.Manager(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(secondGit.WorktreePath("run-1"), "README.md")); err != nil {
		t.Fatalf("worktree was not created under the second repo: %v", err)
	}
	if _, err := os.Stat(h.git.WorktreePath("run-1")); err == nil {
		t.Fatal("a worktree was also created under the first repo")
	}
	if rec := loadOnlyRun(t, h.sp); rec.RepoPath != canonical(t, second) {
		t.Fatalf("ledger RepoPath = %q, want %q", rec.RepoPath, canonical(t, second))
	}
}

func TestRunStartWithEmptyRepoUsesFirstAllowed(t *testing.T) {
	col := &collector{}
	second, _ := newRepo(t)
	h := newHarness(t, col, withRepos(second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// repo_path 없이 온 Phase 0 형식의 명령. 러너의 첫 허용 저장소가 기본값이다.
	cmd := startCommand(t, "run-1", protocol.RunStartBody{Prompt: "say pong"})
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	waitTerminal(t, col)

	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), "README.md")); err != nil {
		t.Fatalf("worktree was not created under the first repo: %v", err)
	}
	if rec := loadOnlyRun(t, h.sp); rec.RepoPath != canonical(t, h.repo) {
		t.Fatalf("ledger RepoPath = %q, want %q", rec.RepoPath, canonical(t, h.repo))
	}
}

func TestRunStartRejectsRepoOutsideAllowList(t *testing.T) {
	col := &collector{}
	marker := filepath.Join(t.TempDir(), "agent-ran")
	h := newHarness(t, col, withBinary(fakeClaudeRecording(t, pongFixture, marker)))
	outsider, _ := newRepo(t) // 실재하지만 허용 목록에 없는 저장소

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startCommand(t, "run-1", protocol.RunStartBody{Prompt: "say pong", RepoPath: outsider})
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("HandleCommand returned %v; a disallowed repo is the Run's failure, not a command error", err)
	}

	waitFor(t, "failed state event", func() bool { return failureDetail(t, col) != "" })
	if detail := failureDetail(t, col); !strings.Contains(detail, "not allowed") {
		t.Fatalf("failure detail = %q, want it to say the repository is not allowed", detail)
	}
	if got := col.count(protocol.EvRunStateChanged); got != 1 {
		t.Fatalf("state events = %d, want exactly 1 (failed) — no running before it", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent was started although the repository is not allowed")
	}

	rec := loadOnlyRun(t, h.sp)
	if rec.State != "failed" || rec.RepoPath != outsider {
		t.Fatalf("ledger = %+v, want failed with the requested (unresolved) RepoPath", rec)
	}

	// 재전송은 같은 결과를 조용히 낸다 — 두 번째 failed 이벤트가 없다.
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := col.count(protocol.EvRunStateChanged); got != 1 {
		t.Fatalf("replay produced another state event (%d total)", got)
	}
}

func TestRunStartUsesBodyBaseBranchWhenSet(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	// dev 브랜치에만 있는 파일.
	gitIn(t, h.repo, "checkout", "-q", "-b", "dev")
	commitFile(t, h.repo, "only-on-dev.txt", "dev\n")
	gitIn(t, h.repo, "checkout", "-q", "main")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startCommand(t, "run-1", protocol.RunStartBody{Prompt: "say pong", RepoPath: h.repo, BaseBranch: "dev"})
	if err := h.m.HandleCommand(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, col)

	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), "only-on-dev.txt")); err != nil {
		t.Fatal("worktree was not based on the branch named in the command body")
	}
}

func TestRunStartFallsBackToConfigBaseBranch(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBaseBranch("dev"))

	gitIn(t, h.repo, "checkout", "-q", "-b", "dev")
	commitFile(t, h.repo, "only-on-dev.txt", "dev\n")
	gitIn(t, h.repo, "checkout", "-q", "main")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, col)

	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), "only-on-dev.txt")); err != nil {
		t.Fatal("empty body base branch did not fall back to the configured default")
	}
}

// recordedPrompt는 fakeClaudeRecording이 남긴 인자 목록에서 -p 다음 값을 꺼낸다.
func recordedPrompt(t *testing.T, argsFile string) string {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("agent args were not recorded: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no -p in recorded args: %q", args)
	return ""
}

func TestMemoryTokenIsReplacedFromWorktreeFile(t *testing.T) {
	col := &collector{}
	argsFile := filepath.Join(t.TempDir(), "args")
	h := newHarness(t, col, withBinary(fakeClaudeRecording(t, pongFixture, argsFile)))
	commitFile(t, h.repo, ".taskyard/memory.md", "항상 uv를 쓴다\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "기억:\n{{memory}}\n끝")); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, col)

	if got := recordedPrompt(t, argsFile); got != "기억:\n항상 uv를 쓴다\n\n끝" {
		t.Fatalf("prompt handed to the agent = %q", got)
	}
}

func TestMemoryTokenBecomesEmptyWhenFileMissing(t *testing.T) {
	col := &collector{}
	argsFile := filepath.Join(t.TempDir(), "args")
	h := newHarness(t, col, withBinary(fakeClaudeRecording(t, pongFixture, argsFile)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "기억:{{memory}}끝 {{issue}}")); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, col)

	// {{memory}}만 러너의 몫이다. 다른 토큰은 손대지 않는다.
	if got := recordedPrompt(t, argsFile); got != "기억:끝 {{issue}}" {
		t.Fatalf("prompt handed to the agent = %q", got)
	}
}

func TestTerminalAndCancelRecordsCarryRepoPath(t *testing.T) {
	want := func(t *testing.T, h *harness) {
		t.Helper()
		if rec := loadOnlyRun(t, h.sp); rec.RepoPath != canonical(t, h.repo) {
			t.Fatalf("ledger RepoPath = %q, want %q (state %s)", rec.RepoPath, canonical(t, h.repo), rec.State)
		}
	}

	t.Run("succeeded", func(t *testing.T) {
		col := &collector{}
		h := newHarness(t, col)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
			t.Fatal(err)
		}
		waitTerminal(t, col)
		want(t, h)
	})

	t.Run("failed", func(t *testing.T) {
		col := &collector{}
		h := newHarness(t, col, withBinary(dirtyThenFail(t)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
			t.Fatal(err)
		}
		waitTerminal(t, col)
		want(t, h)
	})

	t.Run("cancelled", func(t *testing.T) {
		col := &collector{}
		sleeper := filepath.Join(t.TempDir(), "sleeper")
		if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, col, withBinary(sleeper))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := h.m.HandleCommand(ctx, h.start(t, "run-1", "say pong")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "pid recorded", func() bool {
			runs, _ := h.sp.LoadRuns()
			return len(runs) == 1 && runs[0].PID != 0
		})
		cancelCmd, err := protocol.NewCommand(protocol.CmdRunCancel, "run-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.m.HandleCommand(ctx, cancelCmd); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "cancelled record", func() bool {
			runs, _ := h.sp.LoadRuns()
			return len(runs) == 1 && runs[0].State == "cancelled" && runs[0].PID != 0
		})
		want(t, h)
	})
}

func newJSONRequest(t *testing.T, body, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestApprovalRequestBecomesAnEventAndDecisionFlowsBack(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.m.Start(ctx)

	// 브로커에 tools/call 하나를 직접 흘려보내 승인 요청을 만든다.
	// 병렬 도구 호출과 구분하려면 tool_use_id가 있어야 하므로 인자에 담는다.
	srv := h.broker.Handler()
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","tool_use_id":"tu-1","input":{"command":"ls"}}}}`
	rec := newRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, newJSONRequest(t, call, "tok"))
	}()

	waitFor(t, "approval_requested event", func() bool {
		return col.count(protocol.EvApprovalRequested) == 1
	})

	// 이벤트에서 요청 ID와 tool_use_id를 꺼내 확인하고 결정을 되돌린다.
	var reqID string
	for _, e := range col.snapshot() {
		if e.Type != protocol.EvApprovalRequested {
			continue
		}
		var wrapper struct {
			Body map[string]any `json:"body"`
		}
		if err := json.Unmarshal(e.Body, &wrapper); err != nil {
			t.Fatal(err)
		}
		reqID, _ = wrapper.Body["request_id"].(string)
		if toolName, _ := wrapper.Body["tool_name"].(string); toolName != "Bash" {
			t.Errorf("tool_name = %q, want Bash", toolName)
		}
		if toolUseID, _ := wrapper.Body["tool_use_id"].(string); toolUseID != "tu-1" {
			t.Errorf("tool_use_id = %q, want tu-1", toolUseID)
		}
	}
	if reqID == "" {
		t.Fatal("approval_requested event carries no request_id")
	}

	decision, err := protocol.NewCommand(protocol.CmdApprovalDecision, "run-1", map[string]any{
		"request_id": reqID,
		"allow":      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.HandleCommand(ctx, decision); err != nil {
		t.Fatalf("approval decision command failed: %v", err)
	}

	<-done
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpcResp); err != nil {
		t.Fatalf("unmarshal tools/call response: %v (body: %s)", err, rec.Body.String())
	}
	if len(rpcResp.Result.Content) != 1 {
		t.Fatalf("tools/call response has %d content blocks, want 1 (body: %s)", len(rpcResp.Result.Content), rec.Body.String())
	}
	if !strings.Contains(rpcResp.Result.Content[0].Text, `"behavior":"allow"`) {
		t.Fatalf("blocked tools/call did not resolve to an allow result: %s", rpcResp.Result.Content[0].Text)
	}
}
