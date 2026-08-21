// Package acceptance는 PRD §16.0의 완료 판정을 자동화한다.
// 실제 claude CLI 대신 fixture를 뱉는 가짜 바이너리를 쓴다.
package acceptance

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
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

const fixture = "../internal/agents/adapter/claudecode/testdata/session-pong.ndjson"

type stack struct {
	st    *store.Store
	hub   *hub.Hub
	srv   *httptest.Server
	sp    *spool.Spool
	link  *link.Link
	life  *lifecycle.Manager
	git   *gitops.Manager
	wsURL string
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func initRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); os.IsNotExist(err) {
		git(t, repo, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "README.md")
		git(t, repo, "commit", "-q", "-m", "init")
	}
}

func newStack(t *testing.T, dbDir string) *stack {
	t.Helper()

	st, err := store.Open(filepath.Join(dbDir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := hub.New(st, "tok")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sp, err := spool.Open(filepath.Join(dbDir, "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	repo := filepath.Join(dbDir, "repo")
	initRepo(t, repo)

	gm := gitops.New(repo, filepath.Join(dbDir, "wt"))

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dbDir, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat "+abs+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var l *link.Link
	lm, err := lifecycle.New(lifecycle.Config{
		Spool: sp, Git: gm, Broker: approval.New("tok"),
		BaseBranch: "main", BrokerURL: "http://127.0.0.1:1/mcp", BrokerToken: "tok",
		ClaudeBinary: fake,
		Publish: func(runID string, env protocol.Envelope) error {
			return l.Publish(runID, env)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	l, err = link.New(link.Config{
		ServerURL: wsURL, RunnerID: "runner-1", PairingToken: "tok",
		Spool: sp, Capabilities: []string{protocol.CapClaudeCode},
		OnCommand: lm.HandleCommand,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &stack{st: st, hub: h, srv: srv, sp: sp, link: l, life: lm, git: gm, wsURL: wsURL}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// 판정 1: 임의 시점에 연결을 10회 끊었다 붙여도 유실·중복이 0이다.
func TestCriterion1_TenDisconnectsLoseNothing(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)

	if err := s.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "initial connection", s.hub.Connected)

	const perRound = 20
	for round := 0; round < 10; round++ {
		for i := 0; i < perRound; i++ {
			env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"round": round, "i": i})
			if err := s.link.Publish("run-1", env); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		s.hub.DropConnection()
		waitFor(t, "disconnect to register", func() bool { return !s.hub.Connected() })
		waitFor(t, "reconnect", s.hub.Connected)
	}

	want := uint64(perRound * 10)
	waitFor(t, "all events applied", func() bool {
		run, err := s.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == want
	})

	events, err := s.st.Events("run-1", 0, int(want)+50)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(events)) != want {
		t.Fatalf("stored %d events, want exactly %d (no loss, no duplicates)", len(events), want)
	}
	for i, e := range events {
		if got, exp := e.Seq, uint64(i+1); got != exp {
			t.Fatalf("events[%d].Seq = %d, want %d", i, got, exp)
		}
	}
}

// stateOf는 lifecycle이 발행하는 이벤트 껍데기({"body":{...}})를 벗겨
// run.state_changed의 state/detail을 꺼낸다.
func stateOf(t *testing.T, env protocol.Envelope) (state, detail string) {
	t.Helper()
	var outer struct {
		Body struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil {
		t.Fatalf("unmarshal event body: %v", err)
	}
	return outer.Body.State, outer.Body.Detail
}

// 판정 2: 실행 중 Runner를 재시작해도 lost가 아니라 resumable로 복구된다.
//
// Classify를 직접 호출하는 것만으로는 순수 함수를 테스트할 뿐 복구를
// 테스트하지 않는다. §16.0의 기준은 "Runner를 실행 도중 재시작해도 Run이
// lost가 아니라 resumable로 복구된다"이므로, 여기서는 실제 재시작을
// 재현한다: spool을 열고 미종결 기록을 남긴 뒤 닫고, 같은 경로로 다시
// 열어(재시작) 새 lifecycle.Manager로 Reconcile을 돌린다.
func TestCriterion2_RunnerRestartYieldsResumable(t *testing.T) {
	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "runner.db")
	repo := filepath.Join(dir, "repo")
	initRepo(t, repo)
	gm := gitops.New(repo, filepath.Join(dir, "wt"))

	const deadPID = 999999 // 이 값이 실제 프로세스일 가능성은 무시할 만큼 낮다.

	// --- 재시작 전: 첫 인스턴스가 두 개의 미종결 Run을 남기고 죽는다. ---
	sp1, err := spool.Open(spoolPath)
	if err != nil {
		t.Fatal(err)
	}

	// run-1: 세션 ID가 남아 있다 → 프로세스가 죽어도 재개 가능해야 한다.
	if err := sp1.SaveRun(spool.RunRecord{
		RunID: "run-1", State: "running", SessionID: "sess-1", PID: deadPID,
	}); err != nil {
		t.Fatal(err)
	}

	// run-2: 세션 ID가 없다 → lost로 판정되고, 미커밋 변경은 salvage로
	// 보존돼야 한다. salvage가 실제로 커밋할 것이 있도록 worktree를 만들고
	// 더럽힌다.
	ws2, err := gm.Ensure(context.Background(), "run-2", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws2.Path, "uncommitted.txt"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sp1.SaveRun(spool.RunRecord{
		RunID: "run-2", State: "running", Branch: ws2.Branch, WorktreePath: ws2.Path, PID: deadPID,
	}); err != nil {
		t.Fatal(err)
	}

	// 직접 Classify로도 판정 규칙 자체를 확인해 둔다(§16.0의 판정 기준 그 자체).
	life1, err := lifecycle.New(lifecycle.Config{
		Spool: sp1, Git: gm, Broker: approval.New("tok"),
		BaseBranch: "main", BrokerURL: "http://127.0.0.1:1/mcp", BrokerToken: "tok",
		Publish: func(string, protocol.Envelope) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := life1.Classify(spool.RunRecord{RunID: "run-1", State: "running", SessionID: "sess-1", PID: deadPID}); got != lifecycle.VerdictResumable {
		t.Fatalf("Classify(run-1) = %v, want %v", got, lifecycle.VerdictResumable)
	}
	if got := life1.Classify(spool.RunRecord{RunID: "run-2", State: "running", PID: deadPID}); got != lifecycle.VerdictLost {
		t.Fatalf("Classify(run-2) = %v, want %v", got, lifecycle.VerdictLost)
	}

	// --- 재시작: 첫 인스턴스가 죽었다고 보고, 같은 경로를 다시 연다. ---
	if err := sp1.Close(); err != nil {
		t.Fatal(err)
	}

	sp2, err := spool.Open(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp2.Close() })

	var mu sync.Mutex
	published := map[string]protocol.Envelope{}
	life2, err := lifecycle.New(lifecycle.Config{
		Spool: sp2, Git: gm, Broker: approval.New("tok"),
		BaseBranch: "main", BrokerURL: "http://127.0.0.1:1/mcp", BrokerToken: "tok",
		Publish: func(runID string, env protocol.Envelope) error {
			mu.Lock()
			defer mu.Unlock()
			published[runID] = env
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := life2.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 발행된 상태 이벤트로 확인한다.
	mu.Lock()
	ev1, ok1 := published["run-1"]
	ev2, ok2 := published["run-2"]
	mu.Unlock()

	if !ok1 {
		t.Fatal("no state event published for run-1")
	}
	if state, detail := stateOf(t, ev1); state != "running" || detail != "reconciled: session resumable" {
		t.Fatalf("run-1 reconciled as state=%q detail=%q, want state=running detail=\"reconciled: session resumable\"", state, detail)
	}

	if !ok2 {
		t.Fatal("no state event published for run-2")
	}
	if state, detail := stateOf(t, ev2); state != "failed" || detail != "reconciled: session lost, work salvaged" {
		t.Fatalf("run-2 reconciled as state=%q detail=%q, want state=failed detail=\"reconciled: session lost, work salvaged\"", state, detail)
	}

	// 원장(ledger)도 확인한다: resumable은 running을 유지하고, lost는 failed로 바뀐다.
	records, err := sp2.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]spool.RunRecord{}
	for _, r := range records {
		byID[r.RunID] = r
	}
	if got := byID["run-1"].State; got != "running" {
		t.Fatalf("run-1 ledger state = %q, want running", got)
	}
	if got := byID["run-2"].State; got != "failed" {
		t.Fatalf("run-2 ledger state = %q, want failed", got)
	}

	// run-2의 미커밋 변경이 salvage로 커밋됐는지 확인한다.
	status, err := gm.Status(context.Background(), "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatalf("run-2 worktree still dirty after salvage: %v", status.ChangedPaths)
	}
	log, err := exec.Command("git", "-C", ws2.Path, "log", "--oneline", "-1").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, log)
	}
	if !strings.Contains(string(log), "taskyard salvage run-2") {
		t.Fatalf("salvage commit missing, git log: %s", log)
	}
}

// 판정 3: 승인 요청이 웹에 뜨고, 응답이 Agent에 전달되어 실행이 계속된다.
func TestCriterion3_ApprovalRoundTrip(t *testing.T) {
	b := approval.New("tok")
	handler := b.Handler()

	done := make(chan string, 1)
	go func() {
		req := <-b.Requests()
		// 웹에서 승인 버튼을 누른 것에 해당한다.
		if err := b.Decide(req.ID, approval.Decision{Allow: true}); err != nil {
			t.Errorf("Decide: %v", err)
		}
		done <- req.ToolName
	}()

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	select {
	case name := <-done:
		if name == "" {
			t.Error("approval request surfaced without a tool name")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval never surfaced")
	}

	if !strings.Contains(rec.Body.String(), "allow") {
		t.Fatalf("agent did not receive an allow decision: %s", rec.Body)
	}
}

// 판정 4: 왕복 지연을 측정해 기록한다(§11.2.1의 판단 근거).
//
// 이 테스트는 통과·실패를 가르지 않는다. 숫자를 남기는 것이 목적이다.
func TestCriterion4_MeasureRoundTripLatency(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)

	if err := s.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "connection", s.hub.Connected)

	const samples = 50
	var total time.Duration

	for i := 0; i < samples; i++ {
		before, err := s.st.GetRun("run-1")
		if err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := s.link.Publish("run-1", env); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "event to be applied", func() bool {
			run, err := s.st.GetRun("run-1")
			return err == nil && run.LastAckedSeq > before.LastAckedSeq
		})
		total += time.Since(start)
	}

	avg := total / samples

	// 측정값을 기록만 한다. 실패 조건을 두지 않는 이유는 두 가지다.
	//
	// 첫째, 이 값의 하한은 전송이 아니라 위 폴링 간격(10ms)이다. 임계값을
	// 걸면 전송 성능이 아니라 테스트 하네스를 재게 된다.
	//
	// 둘째, PRD §11.2.1의 열린 질문은 브라우저→Server→Runner→CLI 왕복이고
	// 이 측정은 그 구간의 일부일 뿐이다. 숫자는 판단의 재료이지 판정 기준이
	// 아니다. PRD §21의 "명세화 대화 지연" 행에 이 값을 기록하고,
	// 실제 판단은 사람이 한다.
	t.Logf("Runner→Server 이벤트 왕복 평균: %v (%d samples, localhost)", avg, samples)
}
