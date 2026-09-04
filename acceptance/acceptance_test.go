// Package acceptance는 PRD §16.0의 완료 판정을 자동화한다.
// 실제 claude CLI 대신 fixture를 뱉는 가짜 바이너리를 쓴다.
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/jinto/taskyard/internal/server/launch"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
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
	cmds  chan protocol.Envelope // 러너가 받은 명령의 사본. 트리거 테스트가 본다
	// launcher는 main처럼 이어 실행을 돌린다. 테스트가 go s.launcher.Run(ctx, s.hub)
	// 로 띄운다 — 띄우지 않으면 재시작 회복 시나리오처럼 손으로 ChainPending.
	launcher *launch.Launcher
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

// defaultAgentScript는 README.md에 한 줄을 덧붙이고 fixture를 뱉는 가짜 Agent다.
// 이 Run은 base 대비 실제 diff를 남기므로, Diff 회수를 검증하는 판정이 빈
// 문자열이 아니라 실제 변경을 보고 확인한다. FIXTURE는 newStackWith가 채운다.
const defaultAgentScript = "#!/bin/sh\necho agent-work >> README.md\ncat FIXTURE\n"

func newStack(t *testing.T, dbDir string) *stack {
	t.Helper()
	return newStackWith(t, dbDir, defaultAgentScript)
}

// newStackWith는 가짜 Agent 스크립트를 바꿔 끼운다. 스크립트 안의 FIXTURE는
// pong fixture의 절대 경로로 치환된다.
func newStackWith(t *testing.T, dbDir, script string) *stack {
	t.Helper()
	return buildStack(t, dbDir, script, stackOpts{})
}

// stackOpts는 PR 경로에 필요한 추가 배선이다: 가짜 gh, 빠른 폴링, bare origin.
type stackOpts struct {
	gh     string
	prPoll time.Duration
	origin bool
}

func buildStack(t *testing.T, dbDir, script string, o stackOpts) *stack {
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
	if o.origin {
		bare := filepath.Join(dbDir, "origin.git")
		git(t, dbDir, "init", "-q", "--bare", bare)
		git(t, repo, "remote", "add", "origin", bare)
	}

	repos, err := lifecycle.NewRepoResolver([]string{repo}, filepath.Join(dbDir, "wt"))
	if err != nil {
		t.Fatal(err)
	}
	gm, err := repos.Manager(repo)
	if err != nil {
		t.Fatal(err)
	}

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dbDir, "fake-claude")
	if err := os.WriteFile(fake, []byte(strings.ReplaceAll(script, "FIXTURE", abs)), 0o755); err != nil {
		t.Fatal(err)
	}

	cmds := make(chan protocol.Envelope, 16)
	var l *link.Link
	lm, err := lifecycle.New(lifecycle.Config{
		Spool: sp, Repos: repos, Broker: approval.New("tok"),
		BaseBranch: "main", BrokerURL: "http://127.0.0.1:1/mcp", BrokerToken: "tok",
		ClaudeBinary: fake, GHBinary: o.gh, PRPollInterval: o.prPoll,
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
		OnCommand: func(ctx context.Context, env protocol.Envelope) error {
			select {
			case cmds <- env:
			default:
			}
			return lm.HandleCommand(ctx, env)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return &stack{st: st, hub: h, srv: srv, sp: sp, link: l, life: lm, git: gm, wsURL: wsURL, cmds: cmds,
		launcher: &launch.Launcher{Store: st, Commander: h}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, 20*time.Second, cond)
}

// waitForWithin은 waitFor과 같지만 호출자가 데드라인을 고른다. link의
// 재연결 backoff는 정상 초기화되면 대개 minBackoff 근방이지만, 벨트-앤-브레이스로
// 여유를 더 주고 싶은 지점(예: 판정 1의 재연결 대기)에 쓴다.
func waitForWithin(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

	// perRound개 중 outagePerRound개는 연결이 끊긴 동안 발행한다. link.Publish는
	// 끊겨 있어도 spool에 적히기만 하고 성공한다(link.go의 "연결이 끊겨 있어도
	// 성공한다") — 그 이벤트들은 재연결 후 drain의 재전송 경로를 통해서만
	// Server에 닿을 수 있다. 발행을 재연결 뒤로만 몰아두면 빠른 기계에서는
	// drop 신호가 도착하기 전에 send→apply→ack이 다 끝나버려 재전송 경로가
	// 한 번도 실제로 실행되지 않고도 테스트가 통과할 수 있다. 발행을 단절
	// 구간에 걸치게 해 재전송 경로를 우연이 아니라 구조로 강제한다.
	const perRound = 20
	const outagePerRound = perRound / 2
	for round := 0; round < 10; round++ {
		s.hub.DropConnection()
		waitFor(t, "disconnect to register", func() bool { return !s.hub.Connected() })

		for i := 0; i < outagePerRound; i++ {
			env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]any{"round": round, "i": i, "phase": "outage"})
			if err := s.link.Publish("run-1", env); err != nil {
				t.Fatalf("publish during outage: %v", err)
			}
		}

		// backoff 리셋 수정 이후로는 빠르게 재연결되지만, 벨트-앤-브레이스로
		// 여유 있는 데드라인을 둔다.
		waitForWithin(t, "reconnect", 30*time.Second, s.hub.Connected)

		for i := outagePerRound; i < perRound; i++ {
			env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]any{"round": round, "i": i, "phase": "connected"})
			if err := s.link.Publish("run-1", env); err != nil {
				t.Fatalf("publish after reconnect: %v", err)
			}
		}
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
	repos, err := lifecycle.NewRepoResolver([]string{repo}, filepath.Join(dir, "wt"))
	if err != nil {
		t.Fatal(err)
	}
	gm, err := repos.Manager(repo)
	if err != nil {
		t.Fatal(err)
	}

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
		Spool: sp1, Repos: repos, Broker: approval.New("tok"),
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
		Spool: sp2, Repos: repos, Broker: approval.New("tok"),
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

// postForm은 브라우저가 폼을 제출하듯 UI에 POST한다.
func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createProjectAndIssue는 브라우저가 할 법한 두 POST로 프로젝트와 이슈 #1을
// 만든다. 저장소 경로는 t.TempDir() 아래의 테스트 저장소 그대로다 — macOS에서
// /var → /private/var 심링크를 지나므로 러너의 경로 정규화까지 겸해 검증된다.
func createProjectAndIssue(t *testing.T, ui http.Handler, repo string) {
	t.Helper()
	rec := postForm(ui, "/projects", url.Values{
		"key": {"shop"}, "name": {"쇼핑몰"}, "repo_path": {repo}, "default_branch": {"main"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects status = %d, body=%s", rec.Code, rec.Body)
	}
	rec = postForm(ui, "/projects/shop/issues", url.Values{
		"title": {"README에 한 줄 추가"}, "body": {"agent-work라는 줄을 덧붙인다"},
	})
	// 이슈를 만들면 보드로 돌아온다 — 만든 사람이 보고 있던 화면이다.
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/shop" {
		t.Fatalf("POST issue status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestIssueRunTriggersAssembledRunAndYieldsDiff는 §16.1의 척추를 문 하나로
// 들어가 끝까지 움직인다: 브라우저가 누를 법한 프로젝트 생성 → 이슈 생성 →
// [실행]이 실행 템플릿과 이슈로 조립한 run.start를 실제로 발행하고, Runner가
// 그 저장소에 worktree를 만들어 Agent를 띄우고, 종결 뒤 Run과 이슈 상태가
// 화면에 보이며 gitops.Diff로 변경을 회수할 수 있는지까지 확인한다.
func TestIssueRunTriggersAssembledRunAndYieldsDiff(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)
	repo := filepath.Join(dir, "repo")

	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)

	createProjectAndIssue(t, ui, repo)

	rec := postForm(ui, "/projects/shop/issues/1/run", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST run status = %d, body=%s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	runID := strings.TrimPrefix(loc, "/runs/")
	if runID == "" || runID == loc {
		t.Fatalf("redirect location missing run id: %q", loc)
	}

	// (a) 러너가 받은 명령은 RunStartBody이고, 프로젝트의 저장소와 조립된
	// 프롬프트를 담는다.
	var cmd protocol.Envelope
	select {
	case cmd = <-s.cmds:
	case <-time.After(10 * time.Second):
		t.Fatal("runner never received run.start")
	}
	var body protocol.RunStartBody
	if err := json.Unmarshal(cmd.Body, &body); err != nil {
		t.Fatalf("run.start body is not RunStartBody: %v (%s)", err, cmd.Body)
	}
	if cmd.Type != protocol.CmdRunStart || cmd.RunID != runID || body.RepoPath != repo || body.BaseBranch != "main" {
		t.Fatalf("run.start = %s/%s repo=%q base=%q, want %s repo=%q base=main", cmd.Type, cmd.RunID, body.RepoPath, body.BaseBranch, runID, repo)
	}
	for _, want := range []string{"#1 README에 한 줄 추가", "agent-work라는 줄을 덧붙인다"} {
		if !strings.Contains(body.Prompt, want) {
			t.Fatalf("assembled prompt lacks %q:\n%s", want, body.Prompt)
		}
	}
	if strings.Contains(body.Prompt, "{{issue}}") {
		t.Fatalf("{{issue}} was not rendered:\n%s", body.Prompt)
	}

	waitFor(t, "terminal run.state_changed event", func() bool {
		events, err := s.st.Events(runID, 0, 100)
		if err != nil {
			return false
		}
		for _, env := range events {
			if env.Type != protocol.EvRunStateChanged {
				continue
			}
			if state, _ := stateOf(t, env); state == "succeeded" || state == "failed" || state == "cancelled" {
				return true
			}
		}
		return false
	})

	events, err := s.st.Events(runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	var finalState string
	sawMessageDelta := false
	for _, env := range events {
		switch env.Type {
		case protocol.EvRunStateChanged:
			if state, _ := stateOf(t, env); state == "succeeded" || state == "failed" || state == "cancelled" {
				finalState = state
			}
		case protocol.EvMessageDelta:
			sawMessageDelta = true
		}
	}
	if finalState != "succeeded" {
		t.Fatalf("run did not reach succeeded, final state = %q", finalState)
	}
	if !sawMessageDelta {
		t.Fatal("no message_delta event recorded; agent output never arrived through the assembled path")
	}

	// (b) worktree는 그 저장소의 루트 아래에 생겼다.
	if _, err := os.Stat(filepath.Join(s.git.WorktreePath(runID), "README.md")); err != nil {
		t.Fatalf("worktree for %s missing under the project's repo: %v", runID, err)
	}

	// (c) Server의 Run 원장이 이벤트로 갱신됐다 — 목록 화면이 이 값을 보여준다.
	waitFor(t, "runs.state = succeeded", func() bool {
		run, err := s.st.GetRun(runID)
		return err == nil && run.State == store.StateSucceeded
	})

	// (d) 이슈 상세에 Run이 succeeded로, 이슈가 review로 보인다.
	page := httptest.NewRecorder()
	ui.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/projects/shop/issues/1", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("issue page status = %d", page.Code)
	}
	for _, want := range []string{runID, "succeeded", "review"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("issue page lacks %q:\n%s", want, page.Body.String())
		}
	}

	// (e) §16.0은 "diff를 회수한다"로 끝난다. newStack의 fake Agent가
	// README.md에 한 줄을 덧붙이므로, 회수한 diff에 그 흔적이 있어야
	// gitops.Diff가 이 실행에 실제로 연결됐다는 증거가 된다.
	diff, err := s.git.Diff(context.Background(), runID, "main")
	if err != nil {
		t.Fatalf("gitops.Diff: %v", err)
	}
	if !strings.Contains(diff, "agent-work") {
		t.Fatalf("diff does not contain the agent's change:\n%s", diff)
	}
}

// TestIssueRunWithoutRunnerLeavesTaskInBacklog: 러너가 붙지 않은 채 [실행]을
// 누르면 503이고, Run은 failed로 정리되며, 이슈는 backlog 그대로다.
func TestIssueRunWithoutRunnerLeavesTaskInBacklog(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir) // link.Run을 돌리지 않는다 — 러너 없음
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()
	createProjectAndIssue(t, ui, filepath.Join(dir, "repo"))

	if rec := postForm(ui, "/projects/shop/issues/1/run", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	p, err := s.st.GetProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.st.GetTask(p.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskBacklog {
		t.Fatalf("task status = %q, want backlog", task.Status)
	}
	runs, _ := s.st.RunsForTask(task.ID)
	if len(runs) != 1 || runs[0].State != store.StateFailed {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
}

// ---- 종료 방식 (PRD §7.6) ----

// runIDFrom은 303 응답의 Location에서 run id를 꺼낸다.
func runIDFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rec.Header().Get("Location")
	id := strings.TrimPrefix(loc, "/runs/")
	if rec.Code != http.StatusSeeOther || id == "" || id == loc {
		t.Fatalf("status = %d location = %q body = %s", rec.Code, loc, rec.Body.String())
	}
	return id
}

// waitRunState는 서버 원장의 runs.state가 want가 될 때까지 기다린다.
func waitRunState(t *testing.T, s *stack, runID, want string) {
	t.Helper()
	waitFor(t, "runs.state = "+want, func() bool {
		run, err := s.st.GetRun(runID)
		return err == nil && run.State == want
	})
}

// startFor는 러너가 받은 명령 중 runID의 run.start를 찾는다.
func startFor(t *testing.T, s *stack, runID string) protocol.RunStartBody {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case env := <-s.cmds:
			if env.Type == protocol.CmdRunStart && env.RunID == runID {
				var body protocol.RunStartBody
				if err := json.Unmarshal(env.Body, &body); err != nil {
					t.Fatal(err)
				}
				return body
			}
		case <-deadline:
			t.Fatalf("runner never received run.start for %s", runID)
		}
	}
}

func taskOf(t *testing.T, s *stack) store.Task {
	t.Helper()
	p, err := s.st.GetProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.st.GetTask(p.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestCancelRunningRunReturnsIssueToBacklog: 실행 중인 Run을 [취소]하면
// cancelled로 끝나고(뒤따르는 failed 없음), 이슈는 backlog로 돌아간다.
func TestCancelRunningRunReturnsIssueToBacklog(t *testing.T) {
	dir := t.TempDir()
	s := newStackWith(t, dir, "#!/bin/sh\nsleep 30\n")
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)
	createProjectAndIssue(t, ui, filepath.Join(dir, "repo"))

	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	waitRunState(t, s, runID, store.StateRunning)

	rec := postForm(ui, "/runs/"+runID+"/cancel", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/shop/issues/1" {
		t.Fatalf("cancel: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	waitRunState(t, s, runID, store.StateCancelled)
	// 러너 원장의 종결 기록(PID 채워짐)이 보이면 execute의 종결 switch가 끝난
	// 것이다. 뒤따를 failed가 있었다면 그 전에 발행됐고 spool을 거쳐 서버에
	// 도착했을 테니, 서버 이벤트를 한 번 더 기다린 뒤 확인한다.
	waitFor(t, "runner ledger settled", func() bool {
		ledger, _ := s.sp.LoadRuns()
		return len(ledger) == 1 && ledger[0].State == "cancelled" && ledger[0].PID != 0
	})
	waitFor(t, "spool drained", func() bool {
		n, err := s.sp.Pending(runID)
		return err == nil && n == 0
	})

	run, _ := s.st.GetRun(runID)
	if run.State != store.StateCancelled {
		t.Fatalf("run state = %q after settling, want cancelled", run.State)
	}
	events, _ := s.st.Events(runID, 0, 100)
	for _, env := range events {
		if env.Type == protocol.EvRunStateChanged {
			if state, _ := stateOf(t, env); state == store.StateFailed {
				t.Fatal("a failed event followed the cancellation")
			}
		}
	}
	if task := taskOf(t, s); task.Status != store.TaskBacklog {
		t.Fatalf("task status = %q, want backlog", task.Status)
	}
	ledger, _ := s.sp.LoadRuns()
	if len(ledger) != 1 || ledger[0].State != "cancelled" {
		t.Fatalf("runner ledger = %+v, want cancelled", ledger)
	}
}

// TestAttentionThenContinueRetryReusesWorktree: 에이전트가 멈추고 보고하면
// needs_attention이 되고, 이어서 재시도는 같은 worktree와 세션으로 새 Run을
// 돌려 끝까지 간다.
func TestAttentionThenContinueRetryReusesWorktree(t *testing.T) {
	dir := t.TempDir()
	// 첫 실행만 attention 파일을 남긴다(MARK로 구분). 두 번째는 정상 성공.
	mark := filepath.Join(dir, "first-run-done")
	script := "#!/bin/sh\nif [ ! -f " + mark + " ]; then touch " + mark + "; mkdir -p .taskyard; printf 'CI가 반복 실패한다\\n' > .taskyard/attention.md; fi\necho agent-work >> README.md\ncat FIXTURE\n"
	s := newStackWith(t, dir, script)
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)
	createProjectAndIssue(t, ui, filepath.Join(dir, "repo"))

	first := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	startFor(t, s, first)
	waitRunState(t, s, first, store.StateNeedsAttention)

	run, _ := s.st.GetRun(first)
	const fixtureSession = "00000000-0000-0000-0000-000000000001"
	if !strings.Contains(run.Detail, "CI가 반복 실패한다") || run.ProviderSessionID != fixtureSession {
		t.Fatalf("first run = %+v, want attention detail and the fixture session", run)
	}
	if task := taskOf(t, s); task.Status != store.TaskInProgress {
		t.Fatalf("task status = %q, want in_progress while attention is pending", task.Status)
	}
	page := get(ui, "/projects/shop/issues/1")
	for _, want := range []string{"CI가 반복 실패한다", `action="/runs/` + first + `/retry"`, `value="continue"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("issue page lacks %q:\n%s", want, page)
		}
	}

	second := runIDFrom(t, postForm(ui, "/runs/"+first+"/retry", url.Values{"mode": {"continue"}, "feedback": {"캐시를 지우고 다시"}}))
	body := startFor(t, s, second)
	if body.WorkspaceRunID != first || body.ResumeSessionID != fixtureSession {
		t.Fatalf("retry body workspace=%q resume=%q, want %s / %s", body.WorkspaceRunID, body.ResumeSessionID, first, fixtureSession)
	}
	for _, want := range []string{"CI가 반복 실패한다", "캐시를 지우고 다시", "이전 실행 " + first} {
		if !strings.Contains(body.Prompt, want) {
			t.Fatalf("retry prompt lacks %q:\n%s", want, body.Prompt)
		}
	}

	waitRunState(t, s, second, store.StateSucceeded)
	if task := taskOf(t, s); task.Status != store.TaskReview {
		t.Fatalf("task status = %q, want review", task.Status)
	}

	// worktree는 하나뿐이고(둘째가 첫째의 것을 씀), attention 파일은 사라졌다.
	ledger, _ := s.sp.LoadRuns()
	for _, rec := range ledger {
		if rec.RunID == second && rec.WorkspaceRunID != first {
			t.Fatalf("second run ledger = %+v, want workspace %s", rec, first)
		}
	}
	if _, err := os.Stat(s.git.WorktreePath(second)); err == nil {
		t.Fatal("the retry created a second worktree instead of reusing the first")
	}
	if _, err := os.Stat(filepath.Join(s.git.WorktreePath(first), ".taskyard", "attention.md")); err == nil {
		t.Fatal("attention.md survived the retry")
	}
}

// TestContinueRetryWithoutSessionIsRefused: 세션 없이 실패한 Run은 이어갈 수
// 없다. 409이고 Run은 생기지 않는다.
func TestContinueRetryWithoutSessionIsRefused(t *testing.T) {
	dir := t.TempDir()
	noInit, err := filepath.Abs("../internal/runner/lifecycle/testdata/session-no-init.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	s := newStackWith(t, dir, "#!/bin/sh\ncat "+noInit+"\n")
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)
	createProjectAndIssue(t, ui, filepath.Join(dir, "repo"))

	first := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	waitRunState(t, s, first, store.StateFailed)

	if rec := postForm(ui, "/runs/"+first+"/retry", url.Values{"mode": {"continue"}}); rec.Code != http.StatusConflict {
		t.Fatalf("continue without a session: status = %d, want 409", rec.Code)
	}
	if runs, _ := s.st.RunsForTask(taskOf(t, s).ID); len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (refusal must not create a run)", len(runs))
	}
}

func get(h http.Handler, path string) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Body.String()
}

// ---- PR 생성·추적·정리 (계획 2026-09-03-phase1-pr) ----

// fakeGH는 제어 파일 state(없으면 PR 없음)로 조종하는 가짜 gh다. pr create는
// state를 OPEN으로 만들고, pr view는 state를 그대로 돌려준다.
func fakeGH(t *testing.T, dir string) (bin, stateFile string) {
	t.Helper()
	stateFile = filepath.Join(dir, "gh-state")
	bin = filepath.Join(dir, "fake-gh")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + filepath.Join(dir, "gh-calls") + `
case "$1 $2" in
  "pr view")
    S=$(cat ` + stateFile + ` 2>/dev/null || echo none)
    [ "$S" = none ] && { echo "no pull requests found" >&2; exit 1; }
    printf '{"number":7,"url":"https://example.test/pull/7","state":"%s","statusCheckRollup":[],"reviewDecision":""}\n' "$S" ;;
  "pr create") echo OPEN > ` + stateFile + `; echo https://example.test/pull/7 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, stateFile
}

// 커밋 하나와 변경 설명을 남기는 가짜 Agent.
const committingAgentScript = "#!/bin/sh\nmkdir -p .taskyard; printf 'README에 한 줄을 더했다\\n' > .taskyard/summary.md\necho agent-work >> README.md\ngit add README.md\ngit -c user.name=t -c user.email=t@t commit -q -m 'agent: add line'\ncat FIXTURE\n"

func createProjectAndIssueWithPR(t *testing.T, ui http.Handler, repo string, createPR bool) {
	t.Helper()
	form := url.Values{"key": {"shop"}, "name": {"쇼핑몰"}, "repo_path": {repo}, "default_branch": {"main"}, "cleanup_merged": {"on"}}
	if createPR {
		form.Set("create_pr", "on")
	}
	if rec := postForm(ui, "/projects", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects status = %d, body=%s", rec.Code, rec.Body)
	}
	rec := postForm(ui, "/projects/shop/issues", url.Values{"title": {"README에 한 줄 추가"}, "body": {"agent-work라는 줄을 덧붙인다"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST issue status = %d", rec.Code)
	}
}

// TestIssueRunCreatesPRAndMergeCompletesIssue: [실행] → Agent 커밋 + 변경 설명 →
// 러너가 push·PR 생성 → 서버에 PR 필드, 이슈 review → 사람이 merge(제어 파일)
// → 추적이 감지 → 이슈 done, worktree 삭제. main처럼 TrackPRs를 나란히 띄운다.
func TestIssueRunCreatesPRAndMergeCompletesIssue(t *testing.T) {
	dir := t.TempDir()
	gh, stateFile := fakeGH(t, dir)
	s := buildStack(t, dir, committingAgentScript, stackOpts{gh: gh, prPoll: 30 * time.Millisecond, origin: true})
	repo := filepath.Join(dir, "repo")
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	go s.life.TrackPRs(ctx)
	waitFor(t, "runner connection", s.hub.Connected)

	createProjectAndIssueWithPR(t, ui, repo, true)
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	if body := startFor(t, s, runID); body.PR == nil || body.PR.Title != "README에 한 줄 추가" || !body.CleanupMerged {
		t.Fatalf("run.start pr = %+v", body.PR)
	}

	waitRunState(t, s, runID, store.StateSucceeded)
	waitFor(t, "pr fields on server", func() bool {
		run, _ := s.st.GetRun(runID)
		return run.PRState == "OPEN" && run.PRNumber == 7
	})
	run, _ := s.st.GetRun(runID)
	if run.Summary != "README에 한 줄을 더했다\n" {
		t.Fatalf("summary = %q", run.Summary)
	}
	if task := taskOf(t, s); task.Status != store.TaskReview {
		t.Fatalf("task after PR = %s, want review", task.Status)
	}
	if page := get(ui, "/projects/shop/issues/1"); !strings.Contains(page, "https://example.test/pull/7") {
		t.Fatalf("issue page lacks PR link:\n%s", page)
	}
	// origin에 브랜치가 실제로 올라갔다.
	git(t, filepath.Join(dir, "origin.git"), "rev-parse", "--verify", "refs/heads/"+s.git.BranchName(runID))

	// 사람이 GitHub에서 merge했다.
	if err := os.WriteFile(stateFile, []byte("MERGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "task done", func() bool { return taskOf(t, s).Status == store.TaskDone })
	run, _ = s.st.GetRun(runID)
	if run.PRState != "MERGED" {
		t.Fatalf("run pr state = %q", run.PRState)
	}
	waitFor(t, "worktree removed", func() bool {
		_, err := os.Stat(s.git.WorktreePath(runID))
		return os.IsNotExist(err)
	})
	if page := get(ui, "/projects/shop/issues/1"); !strings.Contains(page, "done") || strings.Contains(page, "/issues/1/run\"") {
		t.Fatalf("done issue page:\n%s", page)
	}
}

// TestIssueRunWithoutPRKeepsSummary: PR 만들기를 끈 프로젝트 — push도 gh도
// 없이 succeeded, 변경 설명은 남는다(playground처럼 원격 없는 저장소의 경로).
func TestIssueRunWithoutPRKeepsSummary(t *testing.T) {
	dir := t.TempDir()
	gh, _ := fakeGH(t, dir)
	s := buildStack(t, dir, committingAgentScript, stackOpts{gh: gh})
	repo := filepath.Join(dir, "repo")
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ui := uiServer.Routes()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)

	createProjectAndIssueWithPR(t, ui, repo, false)
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	if body := startFor(t, s, runID); body.PR != nil {
		t.Fatalf("create_pr off but run.start has pr: %+v", body.PR)
	}
	waitRunState(t, s, runID, store.StateSucceeded)
	waitFor(t, "summary on server", func() bool {
		run, _ := s.st.GetRun(runID)
		return run.Summary != ""
	})
	if _, err := os.Stat(filepath.Join(dir, "gh-calls")); !os.IsNotExist(err) {
		t.Fatal("gh must not be called when PR creation is off")
	}
	if run, _ := s.st.GetRun(runID); run.PRURL != "" {
		t.Fatalf("unexpected PR fields: %+v", run)
	}
}

// ---- 산출물과 1단계 (계획 2026-09-04-phase1-stages) ----

// stagedAgentScript는 프롬프트에 analysis.md 가 있으면 1단계처럼 보고서만 쓰고,
// 아니면 2단계처럼 커밋한다. 2단계 프롬프트에 보고서가 들어왔는지는 run.start
// 로 본다.
const stagedAgentScript = `#!/bin/sh
if printf '%s' "$*" | grep -q 'analysis.md'; then
  mkdir -p .taskyard/artifacts
  printf '# 설계\n작게 고친다\n' > .taskyard/artifacts/analysis.md
else
  echo agent-work >> README.md
  git add README.md
  git -c user.name=t -c user.email=t@t commit -q -m 'agent: add line'
fi
cat FIXTURE
`

const attentionAnalyzeScript = "#!/bin/sh\nmkdir -p .taskyard; printf '이슈가 모호하다\\n' > .taskyard/attention.md\ncat FIXTURE\n"

// chain이 참이면 main처럼 이어 실행을 배선한다 — 러너가 붙기 전에, 그래야
// 접속 훅이 경합 없이 잡힌다.
func newStageStack(t *testing.T, script string, chain bool) (*stack, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	s := buildStack(t, dir, script, stackOpts{})
	uiServer, err := web.New(s.st, s.hub)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if chain {
		s.hub.OnConnect = s.launcher.ChainPending
		go s.launcher.Run(ctx, s.hub)
	}
	go s.link.Run(ctx)
	waitFor(t, "runner connection", s.hub.Connected)
	return s, uiServer.Routes(), filepath.Join(dir, "repo")
}

func createIssueWithBody(t *testing.T, ui http.Handler, repo, body string) {
	t.Helper()
	form := url.Values{"key": {"shop"}, "name": {"쇼핑몰"}, "repo_path": {repo}, "default_branch": {"main"}}
	if rec := postForm(ui, "/projects", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects status = %d, body=%s", rec.Code, rec.Body)
	}
	if rec := postForm(ui, "/projects/shop/issues", url.Values{"title": {"README에 한 줄 추가"}, "body": {body}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST issue status = %d", rec.Code)
	}
}

// TestLongIssueAnalyzesThenChainsExecute: 긴 이슈 → 1단계(PR 없음) → 보고서 산출물
// → 이슈 in_progress 유지 → 서버가 2단계를 자동으로 열고 보고서를 프롬프트에 →
// succeeded → review.
func TestLongIssueAnalyzesThenChainsExecute(t *testing.T) {
	s, ui, repo := newStageStack(t, stagedAgentScript, true)

	createIssueWithBody(t, ui, repo, strings.Repeat("자세한 요구사항. ", 40))
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	first := startFor(t, s, runID)
	if first.PR != nil || !strings.Contains(first.Prompt, "analysis.md") {
		t.Fatalf("first run.start should be the analyze stage: pr=%v prompt=%q", first.PR, first.Prompt[:80])
	}
	waitRunState(t, s, runID, store.StateSucceeded)
	if task := taskOf(t, s); task.Status != store.TaskInProgress {
		t.Fatalf("task after analyze = %s, want in_progress", task.Status)
	}
	arts, _ := s.st.Artifacts(runID)
	if len(arts) != 1 || arts[0].Name != "analysis.md" {
		t.Fatalf("artifacts = %+v", arts)
	}

	// 2단계가 자동으로 열렸고 보고서가 프롬프트에 들어 있다.
	var second protocol.RunStartBody
	var secondID string
	waitFor(t, "chained execute run", func() bool {
		runs, _ := s.st.RunsForTask(taskOf(t, s).ID)
		if len(runs) < 2 || runs[0].Stage != store.StageExecute {
			return false
		}
		secondID = runs[0].ID
		return runs[0].ReportRunID == runID
	})
	second = startFor(t, s, secondID)
	if !strings.Contains(second.Prompt, "1단계 보고서(있으면):\n# 설계\n작게 고친다") {
		t.Fatalf("execute run.start lacks the report: %q", second.Prompt)
	}
	waitRunState(t, s, secondID, store.StateSucceeded)
	waitFor(t, "task review", func() bool { return taskOf(t, s).Status == store.TaskReview })
	if runs, _ := s.st.RunsForTask(taskOf(t, s).ID); len(runs) != 2 {
		t.Fatalf("chain must open exactly one execute run, got %d", len(runs))
	}
}

// TestShortIssueSkipsAnalysis: 본문이 기준(200자) 미만이면 바로 2단계.
func TestShortIssueSkipsAnalysis(t *testing.T) {
	s, ui, repo := newStageStack(t, stagedAgentScript, false)
	createIssueWithBody(t, ui, repo, "한 줄 추가")
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	if body := startFor(t, s, runID); strings.Contains(body.Prompt, "analysis.md") {
		t.Fatal("short issue should go straight to execute")
	}
	waitRunState(t, s, runID, store.StateSucceeded)
	waitFor(t, "task review", func() bool { return taskOf(t, s).Status == store.TaskReview })
}

// TestAnalyzeAttentionDoesNotChain: 1단계가 멈추고 보고하면 2단계는 열리지 않는다.
func TestAnalyzeAttentionDoesNotChain(t *testing.T) {
	s, ui, repo := newStageStack(t, attentionAnalyzeScript, true)

	createIssueWithBody(t, ui, repo, strings.Repeat("자세한 요구사항. ", 40))
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	waitRunState(t, s, runID, store.StateNeedsAttention)
	time.Sleep(300 * time.Millisecond)
	if runs, _ := s.st.RunsForTask(taskOf(t, s).ID); len(runs) != 1 {
		t.Fatalf("needs_attention must not chain: %+v", runs)
	}
	if task := taskOf(t, s); task.Status != store.TaskInProgress {
		t.Fatalf("task = %s", task.Status)
	}
}

// TestChainRecoversWithoutTheSignal: 1단계 성공을 아무도 듣지 못했어도(서버 재시작),
// 새 Launcher의 ChainPending이 2단계를 연다.
func TestChainRecoversWithoutTheSignal(t *testing.T) {
	s, ui, repo := newStageStack(t, stagedAgentScript, false) // 이어 실행 배선 없음
	createIssueWithBody(t, ui, repo, strings.Repeat("자세한 요구사항. ", 40))
	runID := runIDFrom(t, postForm(ui, "/projects/shop/issues/1/run", nil))
	waitRunState(t, s, runID, store.StateSucceeded)
	if runs, _ := s.st.RunsForTask(taskOf(t, s).ID); len(runs) != 1 {
		t.Fatalf("nothing should chain without a launcher: %+v", runs)
	}
	fresh := &launch.Launcher{Store: s.st, Commander: s.hub}
	fresh.ChainPending()
	runs, _ := s.st.RunsForTask(taskOf(t, s).ID)
	if len(runs) != 2 || runs[0].Stage != store.StageExecute || runs[0].ReportRunID != runID {
		t.Fatalf("recovery did not chain: %+v", runs)
	}
	fresh.ChainPending()
	if runs, _ := s.st.RunsForTask(taskOf(t, s).ID); len(runs) != 2 {
		t.Fatal("second ChainPending must be a no-op")
	}
}
