package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
)

func newServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, _, h := newServerWithHub(t)
	return st, h
}

func newServerWithHub(t *testing.T) (*store.Store, *hub.Hub, http.Handler) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hb := hub.New(st, "tok")
	s, err := web.New(st, hb)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return st, hb, s.Routes()
}

// attachRunner는 실제 link.Link로 가짜 러너를 hub에 붙인다. 받은 명령은
// got 채널로 나온다. 웹 트리거가 실제로 어떤 봉투를 보내는지 확인할 때 쓴다.
func attachRunner(t *testing.T, hb *hub.Hub, routes http.Handler) <-chan protocol.Envelope {
	got, _, _ := attachRunnerLink(t, hb, routes)
	return got
}

// attachRunnerLink는 붙인 러너의 Link와 서버 주소까지 돌려준다. 러너가 실제로
// 이벤트를 올리는 흐름(러너 → hub → 화면)을 시험할 때 쓴다.
func attachRunnerLink(t *testing.T, hb *hub.Hub, routes http.Handler) (<-chan protocol.Envelope, *link.Link, string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hb.ServeWS)
	mux.Handle("/", routes)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	got := make(chan protocol.Envelope, 4)
	l, err := link.New(link.Config{
		ServerURL:    "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		RunnerID:     "runner-1",
		PairingToken: "tok",
		Spool:        sp,
		Capabilities: []string{protocol.CapClaudeCode},
		OnCommand: func(_ context.Context, env protocol.Envelope) error {
			got <- env
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for !hb.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("runner never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got, l, srv.URL
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func seedProject(t *testing.T, st *store.Store, key, repo string) store.Project {
	t.Helper()
	p, err := st.CreateProject(store.Project{Key: key, Name: key + " 프로젝트", RepoPath: repo, DefaultBranch: "main", ExecuteTemplate: "다음 이슈:\n{{issue}}\n기억:{{memory}}"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func seedTask(t *testing.T, st *store.Store, p store.Project, title, body string) store.Task {
	t.Helper()
	task, err := st.CreateTask(store.Task{ProjectID: p.ID, Title: title, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// ---- 프로젝트 ----

func TestIndexListsProjects(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")
	seedProject(t, st, "blog", "/repos/blog")

	rec := get(h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/projects/shop", "/projects/blog", "shop 프로젝트"} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q:\n%s", want, body)
		}
	}
	// 프로젝트 생성 폼이 있다.
	if !strings.Contains(body, `action="/projects"`) {
		t.Errorf("index has no project creation form:\n%s", body)
	}
}

func TestCreateProjectRedirectsAndRejectsBadKey(t *testing.T) {
	st, h := newServer(t)

	rec := postForm(h, "/projects", url.Values{"key": {"shop"}, "name": {"쇼핑몰"}, "repo_path": {"/repos/shop"}, "default_branch": {""}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/shop" {
		t.Fatalf("status = %d location = %q, want 303 to /projects/shop (body: %s)", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	p, err := st.GetProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	if p.DefaultBranch != "main" {
		t.Errorf("empty default_branch should become main, got %q", p.DefaultBranch)
	}
	if !strings.Contains(p.ExecuteTemplate, "{{issue}}") {
		t.Errorf("new project should get the default execute template, got %q", p.ExecuteTemplate)
	}

	for name, form := range map[string]url.Values{
		"uppercase key":      {"key": {"Shop"}, "name": {"x"}, "repo_path": {"/r"}},
		"key with space":     {"key": {"my shop"}, "name": {"x"}, "repo_path": {"/r"}},
		"relative repo path": {"key": {"ok"}, "name": {"x"}, "repo_path": {"repos/shop"}},
		"missing name":       {"key": {"ok"}, "name": {""}, "repo_path": {"/r"}},
	} {
		if rec := postForm(h, "/projects", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}

	if rec := postForm(h, "/projects", url.Values{"key": {"shop"}, "name": {"again"}, "repo_path": {"/r"}}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate key: status = %d, want 409", rec.Code)
	}
}

func TestProjectPageListsIssuesNewestFirst(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	seedTask(t, st, p, "첫 이슈", "")
	seedTask(t, st, p, "둘째 이슈", "")

	rec := get(h, "/projects/shop")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	first, second := strings.Index(body, "첫 이슈"), strings.Index(body, "둘째 이슈")
	if first < 0 || second < 0 {
		t.Fatalf("issues missing from project page:\n%s", body)
	}
	if second > first {
		t.Errorf("issues are not newest first")
	}
	for _, want := range []string{"/projects/shop/issues/1", "/projects/shop/issues/2", `action="/projects/shop/issues"`} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q:\n%s", want, body)
		}
	}

	if rec := get(h, "/projects/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: status = %d, want 404", rec.Code)
	}
}

func TestUpdateSettingsSavesAllowedTools(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	rec := postForm(h, "/projects/shop/template", url.Values{
		"execute_template": {"t {{issue}}"}, "repo_path": {"/repos/shop"},
		"allowed_tools": {"Edit\r\n\r\n  Bash(go test:*)  \r\n"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	p, _ := st.GetProject("shop")
	if len(p.AllowedTools) != 2 || p.AllowedTools[0] != "Edit" || p.AllowedTools[1] != "Bash(go test:*)" {
		t.Fatalf("AllowedTools = %q (blank lines and spaces must be dropped)", p.AllowedTools)
	}
	if p.ExecuteTemplate != "t {{issue}}" {
		t.Fatalf("template = %q", p.ExecuteTemplate)
	}

	body := get(h, "/projects/shop/settings").Body.String()
	if !strings.Contains(body, `name="allowed_tools"`) || !strings.Contains(body, "Bash(go test:*)") {
		t.Fatalf("project page does not show allowed tools:\n%s", body)
	}
}

func TestUpdateSettingsRejectsMalformedAllowedTools(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	rec := postForm(h, "/projects/shop/template", url.Values{
		"execute_template": {"t"}, "repo_path": {"/repos/shop"},
		"allowed_tools": {"Edit\n--dangerously-skip-permissions"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	p, _ := st.GetProject("shop")
	if len(p.AllowedTools) != 0 || p.ExecuteTemplate == "t" {
		t.Fatalf("a rejected form must change nothing: %+v", p)
	}
}

func TestRunIssueSendsAllowedTools(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p, err := st.CreateProject(store.Project{Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main", ExecuteTemplate: "{{issue}}", AllowedTools: []string{"Edit", "Bash(go test:*)"}})
	if err != nil {
		t.Fatal(err)
	}
	seedTask(t, st, p, "이슈", "")

	if rec := postForm(h, "/projects/shop/issues/1/run", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	_, body := decodeStart(t, got, protocol.CmdRunStart)
	if len(body.AllowedTools) != 2 || body.AllowedTools[0] != "Edit" || body.AllowedTools[1] != "Bash(go test:*)" {
		t.Fatalf("run.start allowed_tools = %q", body.AllowedTools)
	}
}

func TestUpdateTemplateFromProjectPage(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	rec := postForm(h, "/projects/shop/template", url.Values{"execute_template": {"새 템플릿 {{issue}}"}, "repo_path": {"/repos/shop"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	p, _ := st.GetProject("shop")
	if p.ExecuteTemplate != "새 템플릿 {{issue}}" {
		t.Fatalf("template = %q", p.ExecuteTemplate)
	}
}

// ---- 이슈 ----

func TestCreateIssueAssignsNumberAndStaysOnTheBoard(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	// 이슈를 만든 사람은 보드를 보고 있었다. 상세 화면으로 끌고 가지 않는다 —
	// 연달아 여러 개를 적어 넣는 것이 보드에서 하는 가장 흔한 일이다.
	for i := 1; i <= 2; i++ {
		rec := postForm(h, "/projects/shop/issues", url.Values{"title": {"이슈"}, "body": {"본문"}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/shop" {
			t.Fatalf("issue %d: status = %d location = %q, want 303 to /projects/shop", i, rec.Code, rec.Header().Get("Location"))
		}
	}
	body := get(h, "/projects/shop").Body.String()
	for _, want := range []string{"SHOP-1", "SHOP-2"} {
		if !strings.Contains(body, want) {
			t.Errorf("보드에 %q 가 없다:\n%s", want, body)
		}
	}
	if rec := postForm(h, "/projects/shop/issues", url.Values{"title": {""}}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty title: status = %d, want 400", rec.Code)
	}
}

func TestIssuePageShowsRunsWithState(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "로그인 이메일 대소문자 무시", "Foo@Example.com 문제")
	_ = st.UpsertRun(store.Run{ID: "run-old", State: store.StateFailed, Kind: "structured", TaskID: task.ID, Stage: "execute"})
	_ = st.UpsertRun(store.Run{ID: "run-new", State: store.StateSucceeded, Kind: "structured", TaskID: task.ID, Stage: "execute"})

	rec := get(h, "/projects/shop/issues/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"#1", "로그인 이메일 대소문자 무시", "Foo@Example.com 문제", "backlog", "/runs/run-old", "/runs/run-new", "succeeded", "failed", `action="/projects/shop/issues/1/run"`} {
		if !strings.Contains(body, want) {
			t.Errorf("issue page lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "run-new") > strings.Index(body, "run-old") {
		t.Errorf("runs are not newest first")
	}

	if rec := get(h, "/projects/shop/issues/99"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown issue: status = %d, want 404", rec.Code)
	}
}

// ---- 실행 트리거 ----

func TestRunIssueSendsRunStartWithRepoAndAssembledPrompt(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "이메일 정규화", "소문자로 저장한다")

	rec := postForm(h, "/projects/shop/issues/1/run", nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/runs/run-") {
		t.Fatalf("status = %d location = %q (body: %s)", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	runID := strings.TrimPrefix(rec.Header().Get("Location"), "/runs/")

	var env protocol.Envelope
	select {
	case env = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never received run.start")
	}
	if env.Type != protocol.CmdRunStart || env.RunID != runID {
		t.Fatalf("got %s for %s, want run.start for %s", env.Type, env.RunID, runID)
	}
	var body protocol.RunStartBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body is not RunStartBody: %v (%s)", err, env.Body)
	}
	if body.RepoPath != "/repos/shop" || body.BaseBranch != "main" {
		t.Errorf("repo/base = %q/%q, want /repos/shop/main", body.RepoPath, body.BaseBranch)
	}
	want := "다음 이슈:\n#1 이메일 정규화\n\n소문자로 저장한다\n기억:{{memory}}"
	if body.Prompt != want {
		t.Errorf("prompt = %q\nwant  %q", body.Prompt, want)
	}

	run, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.TaskID != task.ID || run.Stage != "execute" || run.State != store.StateQueued {
		t.Errorf("run = %+v", run)
	}
	tk, _ := st.GetTask(p.ID, 1)
	if tk.Status != store.TaskInProgress {
		t.Errorf("task status = %q, want in_progress", tk.Status)
	}
}

func TestRunIssueWithoutRunnerMarksRunFailedAndKeepsIssueBacklog(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	seedTask(t, st, p, "이슈", "")

	rec := postForm(h, "/projects/shop/issues/1/run", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	tk, _ := st.GetTask(p.ID, 1)
	if tk.Status != store.TaskBacklog {
		t.Errorf("task status = %q, want backlog (nothing was started)", tk.Status)
	}
	runs, _ := st.RunsForTask(tk.ID)
	if len(runs) != 1 || runs[0].State != store.StateFailed {
		t.Errorf("runs = %+v, want one failed run", runs)
	}
}

func TestRunPageLinksBackToIssue(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "이슈", "")
	_ = st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute"})

	rec := get(h, "/runs/run-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="/projects/shop/issues/1"`) {
		t.Errorf("run page does not link back to its issue:\n%s", rec.Body.String())
	}
	// html/template은 <script> 안의 문자열을 JSON으로 인용한다. 이 줄이 따옴표
	// 없이 나오면 SSE 아일랜드 전체가 초기화되지 않는다.
	if !strings.Contains(rec.Body.String(), `const runID = "run-1";`) {
		t.Errorf("run id is not a quoted JS string:\n%s", rec.Body.String())
	}
}

func TestRunIssueRejectsWhenARunIsActive(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "이슈", "")
	_ = st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured", TaskID: task.ID, Stage: "execute"})

	// 러너 유무와 무관하게, 활성 Run이 있는 이슈는 다시 시작하지 않는다.
	if rec := postForm(h, "/projects/shop/issues/1/run", nil); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	runs, _ := st.RunsForTask(task.ID)
	if len(runs) != 1 {
		t.Fatalf("a second run was created: %+v", runs)
	}

	// 종결된 Run만 있으면 다시 시작할 수 있다(재시도 = 새 Run).
	_ = st.UpsertRun(store.Run{ID: "run-1", State: store.StateFailed, Kind: "structured", TaskID: task.ID, Stage: "execute"})
	if rec := postForm(h, "/projects/shop/issues/1/run", nil); rec.Code == http.StatusConflict {
		t.Fatal("a terminal run must not block a retry")
	}
}

// ---- 종료 방식: 취소·재시도 ----

// retryTemplate은 재시도 변수를 쓰는 실행 템플릿이다.
const retryTemplate = "이슈:{{issue}}\n이전:{{previous_run}}\n메모:{{feedback}}"

func seedRetryProject(t *testing.T, st *store.Store) (store.Project, store.Task) {
	t.Helper()
	p, err := st.CreateProject(store.Project{Key: "shop", Name: "쇼핑몰", RepoPath: "/repos/shop", DefaultBranch: "main", ExecuteTemplate: retryTemplate})
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, st, p, "이메일 정규화", "")
	return p, task
}

func seedRun(t *testing.T, st *store.Store, task store.Task, id, state, session, detail string) {
	t.Helper()
	if err := st.UpsertRun(store.Run{
		ID: id, State: state, Kind: "structured", TaskID: task.ID, Stage: "execute",
		ProviderSessionID: session, Detail: detail,
	}); err != nil {
		t.Fatal(err)
	}
}

func decodeStart(t *testing.T, got <-chan protocol.Envelope, wantType string) (protocol.Envelope, protocol.RunStartBody) {
	t.Helper()
	select {
	case env := <-got:
		if env.Type != wantType {
			t.Fatalf("runner got %s, want %s", env.Type, wantType)
		}
		var body protocol.RunStartBody
		if wantType == protocol.CmdRunStart {
			if err := json.Unmarshal(env.Body, &body); err != nil {
				t.Fatal(err)
			}
		}
		return env, body
	case <-time.After(5 * time.Second):
		t.Fatalf("runner never received %s", wantType)
		return protocol.Envelope{}, protocol.RunStartBody{}
	}
}

func TestCancelSendsRunCancelAndRedirects(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")

	rec := postForm(h, "/runs/run-1/cancel", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/shop/issues/1" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	env, _ := decodeStart(t, got, protocol.CmdRunCancel)
	if env.RunID != "run-1" {
		t.Fatalf("cancel for %s, want run-1", env.RunID)
	}
}

func TestCancelNeedsAttentionRunWithoutRunner(t *testing.T) {
	st, h := newServer(t)
	p, task := seedRetryProject(t, st)
	_ = st.UpdateTaskStatus(task.ID, store.TaskInProgress)
	seedRun(t, st, task, "run-1", store.StateNeedsAttention, "sess-1", "why")

	if rec := postForm(h, "/runs/run-1/cancel", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (no runner needed for a stopped run)", rec.Code)
	}
	run, _ := st.GetRun("run-1")
	if run.State != store.StateCancelled {
		t.Fatalf("run state = %q, want cancelled", run.State)
	}
	tk, _ := st.GetTask(p.ID, 1)
	if tk.Status != store.TaskBacklog {
		t.Fatalf("task status = %q, want backlog", tk.Status)
	}
}

func TestCancelTerminalRunIs409(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateSucceeded, "sess-1", "")
	if rec := postForm(h, "/runs/run-1/cancel", nil); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestRetryContinueReusesWorkspaceAndResumesSession(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p, task := seedRetryProject(t, st)
	_ = st.UpdateTaskStatus(task.ID, store.TaskInProgress)
	seedRun(t, st, task, "run-prev", store.StateNeedsAttention, "sess-1", "CI가 반복 실패")

	rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"continue"}, "feedback": {"캐시를 지우고 다시"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/runs/run-") {
		t.Fatalf("status = %d location = %q body = %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	newID := strings.TrimPrefix(rec.Header().Get("Location"), "/runs/")

	env, body := decodeStart(t, got, protocol.CmdRunStart)
	if env.RunID != newID {
		t.Fatalf("run.start for %s, want %s", env.RunID, newID)
	}
	if body.WorkspaceRunID != "run-prev" || body.ResumeSessionID != "sess-1" {
		t.Fatalf("body workspace=%q resume=%q, want run-prev / sess-1", body.WorkspaceRunID, body.ResumeSessionID)
	}
	want := "이슈:#1 이메일 정규화\n이전:이전 실행 run-prev: needs_attention\nCI가 반복 실패\n메모:캐시를 지우고 다시"
	if body.Prompt != want {
		t.Fatalf("prompt = %q\nwant  %q", body.Prompt, want)
	}

	run, err := st.GetRun(newID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PreviousRunID != "run-prev" || run.Feedback != "캐시를 지우고 다시" || run.WorkspaceRunID != "run-prev" || run.TaskID != task.ID {
		t.Fatalf("new run = %+v", run)
	}
	tk, _ := st.GetTask(p.ID, 1)
	if tk.Status != store.TaskInProgress {
		t.Fatalf("task status = %q, want in_progress", tk.Status)
	}
}

func TestRetryContinueChainsWorkspaceOfEarlierRun(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	_, task := seedRetryProject(t, st)
	// run-2 는 run-1 의 worktree 를 이어받은 재시도였다. 그것을 또 이어가면
	// worktree 주인은 여전히 run-1 이다.
	seedRun(t, st, task, "run-1", store.StateFailed, "sess-1", "")
	_ = st.UpsertRun(store.Run{ID: "run-2", State: store.StateFailed, Kind: "structured", TaskID: task.ID, Stage: "execute",
		ProviderSessionID: "sess-2", PreviousRunID: "run-1", WorkspaceRunID: "run-1"})

	if rec := postForm(h, "/runs/run-2/retry", url.Values{"mode": {"continue"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	_, body := decodeStart(t, got, protocol.CmdRunStart)
	if body.WorkspaceRunID != "run-1" || body.ResumeSessionID != "sess-2" {
		t.Fatalf("body workspace=%q resume=%q, want run-1 / sess-2", body.WorkspaceRunID, body.ResumeSessionID)
	}
}

func TestRetryFreshUsesNewWorkspaceAndNoResume(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-prev", store.StateFailed, "sess-1", "boom")

	rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"fresh"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	newID := strings.TrimPrefix(rec.Header().Get("Location"), "/runs/")
	_, body := decodeStart(t, got, protocol.CmdRunStart)
	if body.WorkspaceRunID != "" && body.WorkspaceRunID != newID {
		t.Fatalf("fresh retry must not reuse a workspace, got %q", body.WorkspaceRunID)
	}
	if body.ResumeSessionID != "" {
		t.Fatalf("fresh retry must not resume, got %q", body.ResumeSessionID)
	}
	if !strings.Contains(body.Prompt, "이전 실행 run-prev: failed\nboom") {
		t.Fatalf("prompt lacks previous run text:\n%s", body.Prompt)
	}
	run, _ := st.GetRun(newID)
	if run.WorkspaceRunID != newID || run.PreviousRunID != "run-prev" {
		t.Fatalf("new run = %+v", run)
	}
}

func TestRetryContinueWithoutSessionIs409AndCreatesNoRun(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-prev", store.StateFailed, "", "no init")

	if rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"continue"}}); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	runs, _ := st.RunsForTask(task.ID)
	if len(runs) != 1 {
		t.Fatalf("a run was created despite the refusal: %+v", runs)
	}
}

func TestRetryIsRefusedUnlessSettled(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-prev", store.StateSucceeded, "sess-1", "")
	seedRun(t, st, task, "run-live", store.StateRunning, "", "")

	// 이슈에 활성 Run 이 있으면 어떤 재시도도 안 된다.
	if rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"fresh"}}); rec.Code != http.StatusConflict {
		t.Fatalf("retry while another run is active: status = %d, want 409", rec.Code)
	}
	// 정착되지 않은 Run 자체도 재시도 대상이 아니다.
	if rec := postForm(h, "/runs/run-live/retry", url.Values{"mode": {"fresh"}}); rec.Code != http.StatusConflict {
		t.Fatalf("retry of a running run: status = %d, want 409", rec.Code)
	}
	if rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"sideways"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: status = %d, want 400", rec.Code)
	}
}

func TestRetryFromReviewMovesIssueToInProgress(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p, task := seedRetryProject(t, st)
	_ = st.UpdateTaskStatus(task.ID, store.TaskReview)
	seedRun(t, st, task, "run-prev", store.StateSucceeded, "sess-1", "")

	if rec := postForm(h, "/runs/run-prev/retry", url.Values{"mode": {"fresh"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	decodeStart(t, got, protocol.CmdRunStart)
	tk, _ := st.GetTask(p.ID, 1)
	if tk.Status != store.TaskInProgress {
		t.Fatalf("task status = %q, want in_progress", tk.Status)
	}
}

func TestIssuePageShowsCancelForActiveAndRetryForSettled(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)

	// Run이 없으면 [실행]만. 템플릿의 or/not 조합이 빈 맵 키에서도 렌더돼야 한다.
	rec := get(h, "/projects/shop/issues/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/projects/shop/issues/1/run"`) {
		t.Fatalf("issue page without runs: status = %d body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/cancel") || strings.Contains(rec.Body.String(), "/retry") {
		t.Fatalf("issue page without runs offers cancel/retry:\n%s", rec.Body.String())
	}

	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	body := get(h, "/projects/shop/issues/1").Body.String()
	if !strings.Contains(body, `action="/runs/run-1/cancel"`) {
		t.Errorf("active run has no cancel action:\n%s", body)
	}
	if strings.Contains(body, `action="/runs/run-1/retry"`) {
		t.Errorf("active run must not offer retry:\n%s", body)
	}

	seedRun(t, st, task, "run-1", store.StateNeedsAttention, "sess-1", "CI가 반복 실패")
	body = get(h, "/projects/shop/issues/1").Body.String()
	for _, want := range []string{`action="/runs/run-1/retry"`, `name="mode"`, `value="continue"`, `value="fresh"`, `name="feedback"`, `action="/runs/run-1/cancel"`, "CI가 반복 실패"} {
		if !strings.Contains(body, want) {
			t.Errorf("settled run page lacks %q:\n%s", want, body)
		}
	}
}

func TestPostRunsIsGone(t *testing.T) {
	_, h := newServer(t)
	if rec := postForm(h, "/runs", url.Values{"prompt": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /runs status = %d, want 404 (the spike door is closed)", rec.Code)
	}
}

// ---- 안전 ----

func TestTemplatesEscapeHTML(t *testing.T) {
	st, h := newServer(t)
	const payload = `<script>alert(1)</script>`
	p, err := st.CreateProject(store.Project{Key: "shop", Name: payload, RepoPath: "/r" + payload, DefaultBranch: "main", ExecuteTemplate: payload})
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, st, p, payload, payload)
	_ = st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured", TaskID: task.ID, Branch: payload})
	env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 1, map[string]any{"body": map[string]any{"text": payload}})
	env.Seq = 1
	if _, _, err := st.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/projects/shop", "/projects/shop/issues/1", "/runs/run-1"} {
		rec := get(h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), payload) {
			t.Errorf("%s renders raw HTML from user input", path)
		}
	}
}

func TestCrossSitePostIsRejected(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	seedTask(t, st, p, "이슈", "")

	paths := []string{"/projects", "/projects/shop/template", "/projects/shop/issues", "/projects/shop/issues/1/run", "/runs/run-1/approve", "/runs/run-1/cancel", "/runs/run-1/retry"}
	send := func(path string, headers map[string]string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("title=x&key=k&name=n&repo_path=/r"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "127.0.0.1:8080"
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range paths {
		if code := send(path, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
			t.Errorf("%s with Sec-Fetch-Site: cross-site → %d, want 403", path, code)
		}
		if code := send(path, map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
			t.Errorf("%s with foreign Origin → %d, want 403", path, code)
		}
		if code := send(path, map[string]string{"Sec-Fetch-Site": "same-origin"}); code == http.StatusForbidden {
			t.Errorf("%s with Sec-Fetch-Site: same-origin was rejected", path)
		}
		if code := send(path, map[string]string{"Origin": "http://127.0.0.1:8080"}); code == http.StatusForbidden {
			t.Errorf("%s with matching Origin was rejected", path)
		}
		if code := send(path, nil); code == http.StatusForbidden {
			t.Errorf("%s without fetch metadata (curl) was rejected", path)
		}
	}
}

// ---- Run 상세 (Phase 0에서 이어짐) ----

// TestRunDetailReplaysStoredEvents는 run.state_changed 이벤트를 저장한 뒤
// 상세 페이지가 그 봉투를 봉투->body 래핑을 풀어(unwrap) 렌더링하는지
// 확인한다.
func TestRunDetailReplaysStoredEvents(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 1, map[string]any{
		"body": map[string]any{"text": "hello from the agent"},
	})
	env.Seq = 1
	if _, _, err := st.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}

	rec := get(h, "/runs/run-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_delta") {
		t.Errorf("detail page does not replay stored events:\n%s", body)
	}
	if !strings.Contains(body, "hello from the agent") {
		t.Errorf("detail page did not unwrap the envelope body; raw summary missing:\n%s", body)
	}
}

func TestUnknownRunIs404(t *testing.T) {
	_, h := newServer(t)
	if rec := get(h, "/runs/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestApprovalEventUnwrapsToolUseID는 승인 요청 이벤트에서 tool_use_id가
// eventView까지 살아서 상세 페이지에 노출되는지 확인한다.
func TestApprovalEventUnwrapsToolUseID(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{ID: "run-1", State: store.StateWaitingApproval, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	env, _ := protocol.NewEvent(protocol.EvApprovalRequested, "run-1", 1, map[string]any{
		"body": map[string]any{
			"request_id":  "req-1",
			"tool_name":   "Bash",
			"tool_use_id": "toolu_abc123",
			"input":       json.RawMessage(`{"command":"ls"}`),
		},
	})
	env.Seq = 1
	if _, _, err := st.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}

	rec := get(h, "/runs/run-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "toolu_abc123") {
		t.Errorf("detail page dropped tool_use_id:\n%s", rec.Body.String())
	}
}

func TestApprovePostFailsWithoutARunner(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"request_id": "req-1", "allow": true})
	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/approve", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Runner가 없으면 결정을 전달할 수 없다. 조용히 성공하면 안 된다.
	if rec.Code == http.StatusOK {
		t.Fatalf("approve returned 200 with no runner connected; the decision went nowhere")
	}
}

// ---- PR 생성·추적 (계획 2026-09-03-phase1-pr) ----

func TestUpdateSettingsSavesPolicies(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	// 체크박스 둘 다 켬.
	rec := postForm(h, "/projects/shop/template", url.Values{
		"execute_template": {"t"}, "repo_path": {"/repos/shop"}, "create_pr": {"on"}, "cleanup_merged": {"on"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	p, _ := st.GetProject("shop")
	if !p.CreatePR || !p.CleanupMerged {
		t.Fatalf("policies not saved: %+v", p)
	}
	// 체크박스는 안 보내면 false다.
	rec = postForm(h, "/projects/shop/template", url.Values{"execute_template": {"t"}, "repo_path": {"/repos/shop"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	p, _ = st.GetProject("shop")
	if p.CreatePR || p.CleanupMerged {
		t.Fatalf("unchecked boxes must save false: %+v", p)
	}
	body := get(h, "/projects/shop/settings").Body.String()
	if !strings.Contains(body, `name="create_pr"`) || !strings.Contains(body, `name="cleanup_merged"`) {
		t.Fatalf("project page lacks policy checkboxes:\n%s", body)
	}
}

func TestCreateProjectDefaultsPoliciesOn(t *testing.T) {
	st, h := newServer(t)
	rec := postForm(h, "/projects", url.Values{
		"key": {"shop"}, "name": {"s"}, "repo_path": {"/repos/shop"}, "default_branch": {"main"},
		"create_pr": {"on"}, "cleanup_merged": {"on"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	p, _ := st.GetProject("shop")
	if !p.CreatePR || !p.CleanupMerged {
		t.Fatalf("create form did not save policies: %+v", p)
	}
	// 생성 폼은 기본 체크다.
	index := get(h, "/").Body.String()
	if !strings.Contains(index, `name="create_pr" checked`) {
		t.Fatalf("create form does not default create_pr on:\n%s", index)
	}
}

func TestRunIssueSendsPRSpec(t *testing.T) {
	for _, createPR := range []bool{true, false} {
		st, hb, h := newServerWithHub(t)
		got := attachRunner(t, hb, h)
		p, err := st.CreateProject(store.Project{Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main", ExecuteTemplate: "{{issue}}", CreatePR: createPR, CleanupMerged: true})
		if err != nil {
			t.Fatal(err)
		}
		seedTask(t, st, p, "Shout 추가", "대문자로 인사한다")

		if rec := postForm(h, "/projects/shop/issues/1/run", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d", rec.Code)
		}
		env, body := decodeStart(t, got, protocol.CmdRunStart)
		if !createPR {
			if body.PR != nil {
				t.Fatalf("create_pr off but run.start has pr: %+v", body.PR)
			}
			continue
		}
		if body.PR == nil || body.PR.Title != "Shout 추가" || !body.CleanupMerged {
			t.Fatalf("run.start pr = %+v cleanup = %v", body.PR, body.CleanupMerged)
		}
		if !strings.Contains(body.PR.Body, "#1") || !strings.Contains(body.PR.Body, env.RunID) || !strings.Contains(body.PR.Body, "대문자로 인사한다") {
			t.Fatalf("pr body = %q; want issue number, run id and issue body", body.PR.Body)
		}
	}
}

func TestIssuePageShowsPRAndSummary(t *testing.T) {
	st, h := newServer(t)
	p, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	events := []protocol.Envelope{
		mustEvent(t, protocol.EvRunStateChanged, "run-1", 1, map[string]any{"state": "succeeded", "detail": "", "summary": "Shout를 추가했다"}),
		mustEvent(t, protocol.EvPRUpdated, "run-1", 2, map[string]any{"url": "https://github.com/o/r/pull/7", "number": 7, "state": "OPEN", "checks": "pending"}),
	}
	for _, e := range events {
		if _, _, err := st.ApplyEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	body := get(h, "/projects/"+p.Key+"/issues/1").Body.String()
	for _, want := range []string{`href="https://github.com/o/r/pull/7"`, "#7", "OPEN", "pending", "Shout를 추가했다"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue page lacks %q", want)
		}
	}

	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvPRUpdated, "run-1", 3, map[string]any{"url": "https://github.com/o/r/pull/7", "number": 7, "state": "MERGED"})); err != nil {
		t.Fatal(err)
	}
	body = get(h, "/projects/"+p.Key+"/issues/1").Body.String()
	if !strings.Contains(body, "MERGED") || !strings.Contains(body, "done") {
		t.Fatalf("issue page after merge lacks MERGED/done:\n%s", body)
	}
	if strings.Contains(body, `/issues/1/run"`) {
		t.Fatal("done issue should not offer [실행]")
	}
}

func mustEvent(t *testing.T, evType, runID string, seq uint64, body map[string]any) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(evType, runID, seq, map[string]any{"body": body})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	return env
}

func TestRunPageShowsToolInputForApprovalAndToolStart(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	events := []protocol.Envelope{
		mustEvent(t, protocol.EvToolStarted, "run-1", 1, map[string]any{"tool_name": "Edit", "input": map[string]any{"file_path": "/wt/greet.go", "old_string": "x"}}),
		mustEvent(t, protocol.EvApprovalRequested, "run-1", 2, map[string]any{"tool_name": "Bash", "request_id": "r1", "tool_use_id": "t1", "input": map[string]any{"command": "go test ./... -v"}}),
		mustEvent(t, protocol.EvToolFinished, "run-1", 3, map[string]any{"tool_use_id": "t1", "is_error": false, "output": "ok  \tplayground\t0.4s\nPASS"}),
		mustEvent(t, protocol.EvToolFinished, "run-1", 4, map[string]any{"tool_use_id": "t2", "is_error": true, "output": "\r\n  Error calling tool (Bash): The operation timed out.\r\n"}),
		mustEvent(t, protocol.EvUsageUpdated, "run-1", 5, map[string]any{"status": "allowed_warning", "rate_limit_type": "seven_day", "resets_at": 1788584400, "quota_exhausted": true}),
	}
	for _, e := range events {
		if _, _, err := st.ApplyEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	body := get(h, "/runs/run-1").Body.String()
	for _, want := range []string{"→ Edit: /wt/greet.go", "승인 요청: Bash: go test ./... -v", "← 완료: ok  \tplayground\t0.4s", "← 오류: Error calling tool (Bash)"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
	// 사용량은 로그 행이 아니라 머리말 한 줄이다.
	start := strings.Index(body, `id="events"`)
	eventsHTML := body[start : start+strings.Index(body[start:], "<script>")]
	if strings.Contains(eventsHTML, "usage_updated") {
		t.Error("usage_updated should not be an event row")
	}
	if !strings.Contains(body, `id="usage"`) || !strings.Contains(body, "allowed_warning") {
		t.Errorf("run page lacks usage header:\n%s", body)
	}
}

// ---- 산출물과 1단계 (계획 2026-09-04-phase1-stages) ----

func TestUpdateSettingsSavesAnalyzeFields(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")
	rec := postForm(h, "/projects/shop/template", url.Values{
		"execute_template": {"t"}, "repo_path": {"/repos/shop"}, "analyze_template": {"분석 {{issue}}"}, "analyze_skip_below": {"120"},
		// analyze_enabled 체크 안 함 → false
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	p, _ := st.GetProject("shop")
	if p.AnalyzeTemplate != "분석 {{issue}}" || p.AnalyzeEnabled || p.AnalyzeSkipBelow != 120 {
		t.Fatalf("analyze settings not saved: %+v", p)
	}
	body := get(h, "/projects/shop/settings").Body.String()
	for _, want := range []string{`name="analyze_template"`, `name="analyze_enabled"`, `name="analyze_skip_below"`, "분석 {{issue}}"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q", want)
		}
	}
}

func TestRunIssueStageChoice(t *testing.T) {
	long := strings.Repeat("가", 300)
	cases := []struct {
		name, body, choice string
		wantAnalyze        bool
	}{
		{"auto long body analyzes", long, "", true},
		{"auto short body executes", "짧다", "", false},
		{"explicit analyze", "짧다", "analyze", true},
		{"explicit execute", long, "execute", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, hb, h := newServerWithHub(t)
			got := attachRunner(t, hb, h)
			p, err := st.CreateProject(store.Project{Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main",
				ExecuteTemplate: "실행 {{issue}}", AnalyzeTemplate: "분석 {{issue}}", AnalyzeEnabled: true, AnalyzeSkipBelow: 200, CreatePR: true})
			if err != nil {
				t.Fatal(err)
			}
			seedTask(t, st, p, "제목", tc.body)
			form := url.Values{}
			if tc.choice != "" {
				form.Set("stage", tc.choice)
			}
			if rec := postForm(h, "/projects/shop/issues/1/run", form); rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			_, body := decodeStart(t, got, protocol.CmdRunStart)
			if tc.wantAnalyze {
				if !strings.HasPrefix(body.Prompt, "분석 ") || body.PR != nil {
					t.Fatalf("want analyze run.start, got prompt=%q pr=%v", body.Prompt, body.PR)
				}
				runs, _ := st.RunsForTask(func() string { task, _ := st.GetTask(p.ID, 1); return task.ID }())
				if runs[0].Stage != store.StageAnalyze {
					t.Fatalf("run stage = %q", runs[0].Stage)
				}
			} else if !strings.HasPrefix(body.Prompt, "실행 ") || body.PR == nil {
				t.Fatalf("want execute run.start, got prompt=%q pr=%v", body.Prompt, body.PR)
			}
		})
	}
}

func TestArtifactPageAndIssueListing(t *testing.T) {
	st, h := newServer(t)
	p, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	for _, e := range []protocol.Envelope{
		mustEvent(t, protocol.EvArtifactAdded, "run-1", 1, map[string]any{"name": "analysis.md", "content": "# 분석 <script>alert(1)</script>", "truncated": true}),
		mustEvent(t, protocol.EvRunStateChanged, "run-1", 2, map[string]any{"state": "succeeded"}),
	} {
		if _, _, err := st.ApplyEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	page := get(h, "/runs/run-1/artifacts/analysis.md")
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "# 분석 &lt;script&gt;") || strings.Contains(body, "<script>alert") {
		t.Fatalf("artifact content not escaped:\n%s", body)
	}
	if !strings.Contains(body, "잘렸") {
		t.Error("truncated artifact should say so")
	}
	if get(h, "/runs/run-1/artifacts/nope.md").Code != http.StatusNotFound {
		t.Error("unknown artifact should be 404")
	}
	issue := get(h, "/projects/"+p.Key+"/issues/1").Body.String()
	if !strings.Contains(issue, `href="/runs/run-1/artifacts/analysis.md"`) {
		t.Fatalf("issue page lacks artifact link:\n%s", issue)
	}
	// 이름에 ?·#·공백이 있어도 링크가 그 파일을 가리킨다.
	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvArtifactAdded, "run-1", 3, map[string]any{"name": "note s#1?.md", "content": "odd"})); err != nil {
		t.Fatal(err)
	}
	issue = get(h, "/projects/"+p.Key+"/issues/1").Body.String()
	if !strings.Contains(issue, `href="/runs/run-1/artifacts/note%20s%231%3F.md"`) {
		t.Fatalf("odd artifact name not escaped in link:\n%s", issue)
	}
	if got := get(h, "/runs/run-1/artifacts/note%20s%231%3F.md"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "odd") {
		t.Fatalf("escaped link does not resolve: %d", got.Code)
	}
	run := get(h, "/runs/run-1").Body.String()
	if !strings.Contains(run, `href="/runs/run-1/artifacts/analysis.md"`) {
		t.Fatal("run page lacks artifact link")
	}
}

func TestRetryInheritsStageAndReport(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p, err := st.CreateProject(store.Project{Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main",
		ExecuteTemplate: "실행 {{issue}}\n보고서:\n{{stage1_report}}", AnalyzeTemplate: "분석 {{issue}}", AnalyzeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, st, p, "제목", "b")
	// 성공한 1단계와 그 보고서, 그리고 실패한 2단계(보고서 계보 있음).
	_ = st.UpsertRun(store.Run{ID: "a1", State: store.StateSucceeded, Kind: "structured", TaskID: task.ID, Stage: store.StageAnalyze})
	_, _, _ = st.ApplyEvent(mustEvent(t, protocol.EvArtifactAdded, "a1", 1, map[string]any{"name": "analysis.md", "content": "설계 A"}))
	_ = st.UpsertRun(store.Run{ID: "e1", State: store.StateFailed, Kind: "structured", TaskID: task.ID, Stage: store.StageExecute, ReportRunID: "a1", CreatedAt: time.Now().Add(time.Second)})

	if rec := postForm(h, "/runs/e1/retry", url.Values{"mode": {"fresh"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	_, body := decodeStart(t, got, protocol.CmdRunStart)
	if !strings.HasPrefix(body.Prompt, "실행 ") || !strings.Contains(body.Prompt, "보고서:\n설계 A") {
		t.Fatalf("execute retry lost the report lineage: %q", body.Prompt)
	}
	runs, _ := st.RunsForTask(task.ID)
	if runs[0].Stage != store.StageExecute || runs[0].ReportRunID != "a1" {
		t.Fatalf("retry run = %+v", runs[0])
	}
}

func TestSuggestRuleFromApproval(t *testing.T) {
	cases := []struct {
		tool, command, want string
	}{
		{"Bash", "go test ./...", "Bash(go test:*)"},
		{"Bash", "git commit -m x", "Bash(git commit:*)"},
		{"Bash", "ls -la", "Bash(ls:*)"},
		{"Bash", "gofmt -l .", "Bash(gofmt:*)"},
		{"Bash", "go test ./... && go vet ./...", ""}, // 묶은 명령은 규칙으로 못 만든다
		{"Bash", "", ""},
		{"Edit", "", "Edit"},
		{"mcp__x__y", "", "mcp__x__y"},
		{"", "", ""},
	}
	for _, tc := range cases {
		var input map[string]any
		if tc.command != "" {
			input = map[string]any{"command": tc.command}
		}
		if got := web.SuggestRule(tc.tool, input); got != tc.want {
			t.Errorf("SuggestRule(%q, %q) = %q, want %q", tc.tool, tc.command, got, tc.want)
		}
	}
}

func TestApproveWithRememberAddsRuleToProject(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	_ = attachRunner(t, hb, h)
	p, err := st.CreateProject(store.Project{Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main",
		ExecuteTemplate: "{{issue}}", AllowedTools: []string{"Read"}})
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, st, p, "제목", "b")
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvApprovalRequested, "run-1", 1, map[string]any{
		"request_id": "r1", "tool_use_id": "t1", "tool_name": "Bash", "input": map[string]any{"command": "go test ./..."},
	})); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/approve", strings.NewReader(`{"request_id":"r1","allow":true,"remember":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetProject("shop")
	if len(got.AllowedTools) != 2 || got.AllowedTools[1] != "Bash(go test:*)" {
		t.Fatalf("AllowedTools = %q; want the remembered rule appended", got.AllowedTools)
	}

	// 같은 규칙을 두 번 기억해도 늘어나지 않는다.
	req2 := httptest.NewRequest(http.MethodPost, "/runs/run-1/approve", strings.NewReader(`{"request_id":"r1","allow":true,"remember":true}`))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req2)
	got, _ = st.GetProject("shop")
	if len(got.AllowedTools) != 2 {
		t.Fatalf("duplicate rule was added again: %q", got.AllowedTools)
	}
}

func TestRunPageOffersRememberOnApproval(t *testing.T) {
	st, h := newServer(t)
	_, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvApprovalRequested, "run-1", 1, map[string]any{
		"request_id": "r1", "tool_name": "Bash", "input": map[string]any{"command": "go test ./..."},
	})); err != nil {
		t.Fatal(err)
	}
	body := get(h, "/runs/run-1").Body.String()
	if !strings.Contains(body, "Bash(go test:*)") {
		t.Fatalf("run page does not surface the rule to remember:\n%s", body)
	}
}

func TestCreateProjectStartsWithDefaultAllowedTools(t *testing.T) {
	st, h := newServer(t)
	rec := postForm(h, "/projects", url.Values{"key": {"shop"}, "name": {"s"}, "repo_path": {"/repos/shop"}, "default_branch": {"main"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	p, _ := st.GetProject("shop")
	if len(p.AllowedTools) == 0 {
		t.Fatal("a new project should start with a working allow list")
	}
}

func TestUpdateSettingsChangesRepoPathAndBranch(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")
	rec := postForm(h, "/projects/shop/template", url.Values{
		"execute_template": {"t"}, "repo_path": {"/repos/새경로"}, "default_branch": {"develop"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	p, _ := st.GetProject("shop")
	if p.RepoPath != "/repos/새경로" || p.DefaultBranch != "develop" {
		t.Fatalf("repo settings not saved: %+v", p)
	}

	// 상대 경로는 거부하고 아무것도 바꾸지 않는다.
	rec = postForm(h, "/projects/shop/template", url.Values{"execute_template": {"바뀌면 안 됨"}, "repo_path": {"relative/path"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	p, _ = st.GetProject("shop")
	if p.RepoPath != "/repos/새경로" || p.ExecuteTemplate == "바뀌면 안 됨" {
		t.Fatalf("a rejected form must change nothing: %+v", p)
	}
	// 브랜치를 비우면 main.
	if rec := postForm(h, "/projects/shop/template", url.Values{"execute_template": {"t"}, "repo_path": {"/repos/새경로"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if p, _ = st.GetProject("shop"); p.DefaultBranch != "main" {
		t.Fatalf("empty branch should fall back to main, got %q", p.DefaultBranch)
	}
	body := get(h, "/projects/shop/settings").Body.String()
	if !strings.Contains(body, `name="repo_path"`) || !strings.Contains(body, `name="default_branch"`) {
		t.Fatal("project page lacks the repository fields")
	}
}

func TestIndexListsPendingApprovals(t *testing.T) {
	st, h := newServer(t)
	p, task := seedRetryProject(t, st)
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvApprovalRequested, "run-1", 1, map[string]any{
		"request_id": "r1", "tool_use_id": "t1", "tool_name": "Bash", "input": map[string]any{"command": "go test ./..."},
	})); err != nil {
		t.Fatal(err)
	}
	body := get(h, "/").Body.String()
	for _, want := range []string{"승인", "go test ./...", `href="/runs/run-1"`, "#" + fmt.Sprint(task.Number)} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q:\n%s", want, body)
		}
	}
	_ = p

	// 승인하면 사라진다.
	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/approve", strings.NewReader(`{"request_id":"r1","allow":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if body := get(h, "/").Body.String(); strings.Contains(body, "go test ./...") {
		t.Fatal("index still shows the approval after it was answered")
	}
}

// ---- 칸반 보드 (linear/jira 식 화면) ----

func TestProjectBoardPutsEachIssueInItsStatusColumn(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	for _, tc := range []struct{ title, status string }{
		{"대기 이슈", store.TaskBacklog},
		{"진행 이슈", store.TaskInProgress},
		{"리뷰 이슈", store.TaskReview},
		{"완료 이슈", store.TaskDone},
	} {
		task := seedTask(t, st, p, tc.title, "")
		if err := st.UpdateTaskStatus(task.ID, tc.status); err != nil {
			t.Fatal(err)
		}
	}

	body := get(h, "/projects/shop").Body.String()
	// 칸반은 순서가 곧 뜻이다: 왼쪽에서 오른쪽으로 대기 → 진행 → 리뷰 → 완료.
	// 각 이슈는 자기 열 머리글 뒤, 다음 열 머리글 앞에 있어야 한다.
	order := []string{"대기", "대기 이슈", "진행", "진행 이슈", "리뷰", "리뷰 이슈", "완료", "완료 이슈"}
	at := -1
	for _, want := range order {
		i := strings.Index(body[at+1:], want)
		if i < 0 {
			t.Fatalf("보드에 %q 가 순서대로 없다:\n%s", want, body)
		}
		at += 1 + i
	}
}

func TestProjectBoardCardShowsLatestRun(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "돌아가는 이슈", "")
	if err := st.UpdateTaskStatus(task.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRun(store.Run{ID: "run-9", State: store.StateRunning, Kind: "structured", TaskID: task.ID, Stage: store.StageExecute, Branch: "taskyard/1"}); err != nil {
		t.Fatal(err)
	}

	body := get(h, "/projects/shop").Body.String()
	for _, want := range []string{"/runs/run-9", store.StateRunning, store.StageExecute, "taskyard/1", "방금"} {
		if !strings.Contains(body, want) {
			t.Errorf("카드에 %q 가 없다:\n%s", want, body)
		}
	}
}

func TestProjectBoardBacklogCardStartsARun(t *testing.T) {
	st, hb, h := newServerWithHub(t)
	got := attachRunner(t, hb, h)
	p := seedProject(t, st, "shop", "/repos/shop")
	seedTask(t, st, p, "아직 대기", "")

	body := get(h, "/projects/shop").Body.String()
	if !strings.Contains(body, `action="/projects/shop/issues/1/run"`) {
		t.Fatalf("대기 카드에 실행 폼이 없다:\n%s", body)
	}
	if rec := postForm(h, "/projects/shop/issues/1/run", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	decodeStart(t, got, protocol.CmdRunStart)
}

func TestSettingsMovesOffTheBoard(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	board := get(h, "/projects/shop")
	if strings.Contains(board.Body.String(), `name="execute_template"`) {
		t.Error("보드에 설정 폼이 남아 있다 — 보드는 일감만 보여준다")
	}
	if !strings.Contains(board.Body.String(), "/projects/shop/settings") {
		t.Error("보드에 설정으로 가는 링크가 없다")
	}

	rec := get(h, "/projects/shop/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{`action="/projects/shop/template"`, `name="execute_template"`, "{{issue}}"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("설정 화면에 %q 가 없다", want)
		}
	}

	// 저장하면 설정 화면으로 돌아온다 — 방금 고친 값을 확인할 수 있어야 한다.
	saved := postForm(h, "/projects/shop/template", url.Values{"execute_template": {"t"}, "repo_path": {"/repos/shop"}})
	if loc := saved.Header().Get("Location"); loc != "/projects/shop/settings" {
		t.Errorf("Location = %q, want /projects/shop/settings", loc)
	}
}

func TestProjectBoardMarksCardsWaitingForApproval(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "승인을 기다리는 이슈", "")
	if err := st.UpdateTaskStatus(task.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRun(store.Run{ID: "run-7", State: store.StateRunning, Kind: "structured", TaskID: task.ID, Stage: store.StageExecute}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyEvent(mustEvent(t, protocol.EvApprovalRequested, "run-7", 1, map[string]any{
		"request_id": "r1", "tool_use_id": "t1", "tool_name": "Bash", "input": map[string]any{"command": "go test ./..."},
	})); err != nil {
		t.Fatal(err)
	}

	// 돌아가는 카드와 사람을 기다리는 카드는 한눈에 달라야 한다 — 기다리는
	// 쪽이 보드에서 유일하게 사람의 손을 필요로 하는 카드다.
	body := get(h, "/projects/shop").Body.String()
	if !strings.Contains(body, `class="card wait"`) {
		t.Fatalf("기다리는 카드가 도드라지지 않는다:\n%s", body)
	}
	if !strings.Contains(body, `>승인 대기</a>`) || strings.Contains(body, ` hidden>승인 대기</a>`) {
		t.Fatalf("승인 대기 배지가 서 있지 않다:\n%s", body)
	}
	if !strings.Contains(body, "go test ./...") {
		t.Errorf("무엇을 승인해 달라는지가 없다:\n%s", body)
	}

	// 그냥 돌아가는 카드에는 배지가 숨어 있다 — 스트림이 켜기 전까지는.
	quiet := seedTask(t, st, p, "조용히 도는 이슈", "")
	if err := st.UpdateTaskStatus(quiet.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRun(store.Run{ID: "run-8", State: store.StateRunning, Kind: "structured", TaskID: quiet.ID, Stage: store.StageExecute}); err != nil {
		t.Fatal(err)
	}
	if body := get(h, "/projects/shop").Body.String(); !strings.Contains(body, ` hidden>승인 대기</a>`) {
		t.Errorf("기다리지 않는 카드에 배지 자리가 없다:\n%s", body)
	}
}

// ---- 보드는 살아 있다 ----

func TestProjectBoardCardShowsWhatTheAgentIsDoing(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	task := seedTask(t, st, p, "돌아가는 이슈", "")
	if err := st.UpdateTaskStatus(task.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	seedRun(t, st, task, "run-1", store.StateRunning, "", "")
	for _, ev := range []protocol.Envelope{
		mustEvent(t, protocol.EvToolStarted, "run-1", 1, map[string]any{"tool_name": "Read", "input": map[string]any{"file_path": "/repo/README.md"}}),
		mustEvent(t, protocol.EvToolStarted, "run-1", 2, map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "go test ./..."}}),
	} {
		if _, _, err := st.ApplyEvent(ev); err != nil {
			t.Fatal(err)
		}
	}

	body := get(h, "/projects/shop").Body.String()
	// 카드는 마지막 한 걸음만 말한다 — 지금 무엇을 하는 중인지.
	if !strings.Contains(body, "go test ./...") {
		t.Errorf("카드가 지금 하는 일을 보여주지 않는다:\n%s", body)
	}
	if strings.Contains(body, "README.md") {
		t.Error("지나간 도구 호출이 카드에 남아 있다 — 카드는 한 줄이다")
	}
	if !strings.Contains(body, `class="spinner`) {
		t.Error("도는 아이콘이 없다 — 돌아가는 카드가 멈춘 것처럼 보인다")
	}
	if !strings.Contains(body, `data-task="1"`) {
		t.Error("카드에 이슈 번호가 표시되지 않았다 — 실시간 갱신이 카드를 찾지 못한다")
	}
}

func TestProjectBoardCardShowsWhyItStopped(t *testing.T) {
	st, h := newServer(t)
	p := seedProject(t, st, "shop", "/repos/shop")
	stuck := seedTask(t, st, p, "판단이 필요한 이슈", "")
	if err := st.UpdateTaskStatus(stuck.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	seedRun(t, st, stuck, "run-1", store.StateNeedsAttention, "", "결제 모듈을 갈아엎을지 정해 주세요")
	broken := seedTask(t, st, p, "깨진 이슈", "")
	if err := st.UpdateTaskStatus(broken.ID, store.TaskInProgress); err != nil {
		t.Fatal(err)
	}
	seedRun(t, st, broken, "run-2", store.StateFailed, "", "go build 실패")

	body := get(h, "/projects/shop").Body.String()
	for _, want := range []string{"판단 필요", "결제 모듈을 갈아엎을지 정해 주세요", "멈춤", "go build 실패"} {
		if !strings.Contains(body, want) {
			t.Errorf("보드에 %q 가 없다:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="spinner`) {
		t.Error("멈춘 카드가 아직 돌고 있다")
	}
}

func TestBoardStreamCarriesOnlyThisProjectsRuns(t *testing.T) {
	st, hb, routes := newServerWithHub(t)
	_, l, base := attachRunnerLink(t, hb, routes)

	shop := seedProject(t, st, "shop", "/repos/shop")
	mine := seedTask(t, st, shop, "우리 이슈", "")
	seedRun(t, st, mine, "run-mine", store.StateRunning, "", "")
	other := seedProject(t, st, "wms", "/repos/wms")
	theirs := seedTask(t, st, other, "남의 이슈", "")
	seedRun(t, st, theirs, "run-theirs", store.StateRunning, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/projects/shop/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 스트림은 열리자마자 머리말을 흘려보낸다 — 그래서 여기까지 오면 구독이
	// 붙었다는 뜻이고, 뒤이어 내는 이벤트를 놓칠 일이 없다.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if err := l.Publish("run-theirs", mustEvent(t, protocol.EvToolStarted, "run-theirs", 1,
		map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "남의 명령"}})); err != nil {
		t.Fatal(err)
	}
	if err := l.Publish("run-mine", mustEvent(t, protocol.EvToolStarted, "run-mine", 1,
		map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "go test ./..."}})); err != nil {
		t.Fatal(err)
	}

	var ev struct {
		Task int    `json:"task"`
		Line string `json:"line"`
	}
	line := readSSE(t, resp.Body)
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("스트림이 보낸 것을 읽을 수 없다 (%v): %q", err, line)
	}
	if ev.Task != mine.Number {
		t.Errorf("task = %d, want %d", ev.Task, mine.Number)
	}
	if !strings.Contains(ev.Line, "go test ./...") {
		t.Errorf("line = %q", ev.Line)
	}
	if strings.Contains(ev.Line, "남의 명령") {
		t.Error("남의 프로젝트 이벤트가 이 보드로 새어 들어왔다")
	}
}

// readSSE는 스트림에서 다음 data: 줄 하나를 읽는다. 스트림은 스스로 끝나지
// 않으므로 기다리는 시간에 끝을 둔다.
func readSSE(t *testing.T, r io.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if after, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				got <- after
				return
			}
		}
	}()
	select {
	case line := <-got:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("보드 스트림에서 아무것도 오지 않았다")
		return ""
	}
}
