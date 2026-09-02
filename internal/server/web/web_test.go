package web_test

import (
	"context"
	"encoding/json"
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
	return got
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
	for _, want := range []string{"/projects/shop/issues/1", "/projects/shop/issues/2", `action="/projects/shop/issues"`, `action="/projects/shop/template"`, "{{issue}}"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q:\n%s", want, body)
		}
	}

	if rec := get(h, "/projects/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: status = %d, want 404", rec.Code)
	}
}

func TestUpdateTemplateFromProjectPage(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	rec := postForm(h, "/projects/shop/template", url.Values{"execute_template": {"새 템플릿 {{issue}}"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	p, _ := st.GetProject("shop")
	if p.ExecuteTemplate != "새 템플릿 {{issue}}" {
		t.Fatalf("template = %q", p.ExecuteTemplate)
	}
}

// ---- 이슈 ----

func TestCreateIssueAssignsNumber(t *testing.T) {
	st, h := newServer(t)
	seedProject(t, st, "shop", "/repos/shop")

	for i, want := range []string{"/projects/shop/issues/1", "/projects/shop/issues/2"} {
		rec := postForm(h, "/projects/shop/issues", url.Values{"title": {"이슈"}, "body": {"본문"}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Fatalf("issue %d: status = %d location = %q, want 303 to %s", i+1, rec.Code, rec.Header().Get("Location"), want)
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
