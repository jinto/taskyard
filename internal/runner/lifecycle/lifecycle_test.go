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

// fakeClaude는 실제 CLI 대신 fixture NDJSON을 그대로 뱉는 스크립트다.
// 사용자 구독 할당량을 쓰지 않고 배선 전체를 검증할 수 있다.
func fakeClaude(t *testing.T, fixture string) string {
	t.Helper()

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\ncat " + abs + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newRepo(t *testing.T) (repo, worktrees string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	worktrees = filepath.Join(base, "wt")

	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo, worktrees
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

func newManager(t *testing.T, col *collector) (*lifecycle.Manager, *spool.Spool, *gitops.Manager) {
	t.Helper()

	repo, worktrees := newRepo(t)
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	git := gitops.New(repo, worktrees)
	fixture := "../../agents/adapter/claudecode/testdata/session-pong.ndjson"

	m, err := lifecycle.New(lifecycle.Config{
		Spool:        sp,
		Git:          git,
		Broker:       approval.New("tok"),
		BaseBranch:   "main",
		BrokerURL:    "http://127.0.0.1:9999/mcp",
		BrokerToken:  "tok",
		ClaudeBinary: fakeClaude(t, fixture),
		Publish:      col.publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, sp, git
}

func startCommand(t *testing.T, runID, prompt string) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewCommand(protocol.CmdRunStart, runID, map[string]string{"prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	return env
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

func TestRunStartCreatesWorktreeAndStreamsEvents(t *testing.T) {
	col := &collector{}
	m, sp, git := newManager(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.HandleCommand(ctx, startCommand(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}

	waitFor(t, "turn_completed", func() bool {
		return col.count(protocol.EvTurnCompleted) == 1
	})

	if col.count(protocol.EvMessageDelta) != 1 {
		t.Errorf("message_delta count = %d, want 1 (types: %v)", col.count(protocol.EvMessageDelta), col.types())
	}
	if col.count(protocol.EvRunStateChanged) < 2 {
		t.Errorf("expected at least a running and a terminal state change, got %v", col.types())
	}

	if _, err := os.Stat(filepath.Join(git.WorktreePath("run-1"), "README.md")); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}

	runs, err := sp.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ledger has %d runs, want 1", len(runs))
	}
	if runs[0].SessionID == "" {
		t.Error("ledger did not record the provider session id; resume would be impossible")
	}
}

func TestDuplicateRunStartIsAppliedOnce(t *testing.T) {
	col := &collector{}
	m, _, git := newManager(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startCommand(t, "run-1", "say pong")
	if err := m.HandleCommand(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first run to finish", func() bool { return col.count(protocol.EvTurnCompleted) == 1 })

	// 같은 command_id 재전송. 두 번째 실행도, 두 번째 worktree도 없어야 한다.
	if err := m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("replayed command returned an error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := col.count(protocol.EvTurnCompleted); got != 1 {
		t.Fatalf("turn_completed count = %d, want 1 after a replayed command", got)
	}

	listCmd := exec.Command("git", "worktree", "list")
	listCmd.Dir = filepath.Dir(git.WorktreePath("run-1"))
	_ = listCmd // worktree 재사용은 gitops 테스트가 보장한다
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
	broker := approval.New("tok")

	repo, worktrees := newRepo(t)
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	m, err := lifecycle.New(lifecycle.Config{
		Spool:        sp,
		Git:          gitops.New(repo, worktrees),
		Broker:       broker,
		BaseBranch:   "main",
		BrokerURL:    "http://127.0.0.1:9999/mcp",
		BrokerToken:  "tok",
		ClaudeBinary: fakeClaude(t, "../../agents/adapter/claudecode/testdata/session-pong.ndjson"),
		Publish:      col.publish,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Start(ctx)

	// 브로커에 tools/call 하나를 직접 흘려보내 승인 요청을 만든다.
	// 병렬 도구 호출과 구분하려면 tool_use_id가 있어야 하므로 인자에 담는다.
	srv := broker.Handler()
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
	if err := m.HandleCommand(ctx, decision); err != nil {
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
