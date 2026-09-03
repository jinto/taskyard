// Package web은 Server의 HTML UI다.
//
// 프로젝트·이슈 화면은 서버 렌더링이고, Run 상세의 이벤트 스트림만 SSE를
// 구독하는 JS 아일랜드다(PRD §11.4.1). 이슈의 [실행] 버튼이 파이프라인의
// 유일한 문이다 — 프로젝트의 실행 템플릿과 이슈로 프롬프트를 조립해
// run.start를 보낸다(PRD §7.2, §16.1).
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/google/uuid"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/pipeline"
	"github.com/jinto/taskyard/internal/server/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// keyPattern은 프로젝트 key의 문법이다. URL 경로 조각으로 그대로 쓰인다.
var keyPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

type Server struct {
	st  *store.Store
	hub *hub.Hub

	projects *template.Template
	project  *template.Template
	issue    *template.Template
	run      *template.Template
}

func New(st *store.Store, h *hub.Hub) (*Server, error) {
	var err error
	page := func(name string) *template.Template {
		t, perr := template.ParseFS(templateFS, "templates/layout.html", "templates/"+name)
		if perr != nil && err == nil {
			err = fmt.Errorf("parse %s: %w", name, perr)
		}
		return t
	}
	s := &Server{
		st: st, hub: h,
		projects: page("projects.html"),
		project:  page("project.html"),
		issue:    page("issue.html"),
		run:      page("run.html"),
	}
	return s, err
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleProjects)
	mux.HandleFunc("POST /projects", sameOrigin(s.handleProjectCreate))
	mux.HandleFunc("GET /projects/{key}", s.handleProject)
	mux.HandleFunc("POST /projects/{key}/template", sameOrigin(s.handleTemplateUpdate))
	mux.HandleFunc("POST /projects/{key}/issues", sameOrigin(s.handleIssueCreate))
	mux.HandleFunc("GET /projects/{key}/issues/{n}", s.handleIssue)
	mux.HandleFunc("POST /projects/{key}/issues/{n}/run", sameOrigin(s.handleIssueRun))
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /runs/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /runs/{id}/approve", sameOrigin(s.handleApprove))
	mux.HandleFunc("POST /runs/{id}/cancel", sameOrigin(s.handleCancel))
	mux.HandleFunc("POST /runs/{id}/retry", sameOrigin(s.handleRetry))
	return mux
}

// sameOrigin은 상태를 바꾸는 POST를 다른 사이트의 페이지가 브라우저를 시켜
// 보내는 것(CSRF)을 막는다. 웹 UI에는 인증이 없으므로(findings) 이것이
// 유일한 방어선이다. 브라우저가 붙이는 Sec-Fetch-Site를 우선 보고, 없으면
// Origin의 host를 요청 Host와 비교한다. 둘 다 없는 요청(curl)은 통과한다 —
// 브라우저가 아닌 클라이언트는 CSRF의 대상이 아니다.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			if site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		} else if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// ---- 프로젝트 ----

func (s *Server) handleProjects(w http.ResponseWriter, _ *http.Request) {
	projects, err := s.st.ListProjects()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, s.projects, map[string]any{"Title": "프로젝트", "Projects": projects})
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p := store.Project{
		Key:             r.FormValue("key"),
		Name:            r.FormValue("name"),
		RepoPath:        r.FormValue("repo_path"),
		DefaultBranch:   r.FormValue("default_branch"),
		ExecuteTemplate: pipeline.DefaultExecuteTemplate,
	}
	switch {
	case !keyPattern.MatchString(p.Key):
		http.Error(w, "키는 소문자·숫자·하이픈 1~32자여야 합니다", http.StatusBadRequest)
		return
	case p.Name == "":
		http.Error(w, "이름은 비울 수 없습니다", http.StatusBadRequest)
		return
	case !filepath.IsAbs(p.RepoPath):
		http.Error(w, "저장소 경로는 러너 머신의 절대 경로여야 합니다", http.StatusBadRequest)
		return
	}
	if p.DefaultBranch == "" {
		p.DefaultBranch = "main"
	}

	if _, err := s.st.CreateProject(p); err != nil {
		if errors.Is(err, store.ErrDuplicateKey) {
			http.Error(w, "이미 쓰이는 키입니다", http.StatusConflict)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Key, http.StatusSeeOther)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	tasks, err := s.st.ListTasks(p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, s.project, map[string]any{"Title": p.Name, "Project": p, "Tasks": tasks})
}

func (s *Server) handleTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.st.UpdateProjectTemplate(p.Key, r.FormValue("execute_template")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Key, http.StatusSeeOther)
}

// ---- 이슈 ----

func (s *Server) handleIssueCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	title := r.FormValue("title")
	if title == "" {
		http.Error(w, "제목은 비울 수 없습니다", http.StatusBadRequest)
		return
	}
	task, err := s.st.CreateTask(store.Task{ProjectID: p.ID, Title: title, Body: r.FormValue("body")})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, issuePath(p.Key, task.Number), http.StatusSeeOther)
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	p, task, ok := s.loadIssue(w, r)
	if !ok {
		return
	}
	runs, err := s.st.RunsForTask(task.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 화면의 행동은 최신 Run 하나를 기준으로 한다: 활성이면 [취소], 정착이면
	// 재시도 폼(과 needs_attention이면 [취소]도), 없으면 [실행].
	data := map[string]any{
		"Title": fmt.Sprintf("#%d %s", task.Number, task.Title), "Project": p, "Task": task, "Runs": runs,
		"HasLatest": len(runs) > 0,
	}
	if len(runs) > 0 {
		latest := runs[0]
		data["Latest"] = latest
		data["LatestSettled"] = store.IsSettled(latest.State)
		data["LatestCancellable"] = !store.IsSettled(latest.State) || latest.State == store.StateNeedsAttention
		data["LatestResumable"] = latest.ProviderSessionID != ""
	}
	s.render(w, s.issue, data)
}

// handleIssueRun은 파이프라인의 유일한 문이다. 실행 템플릿과 이슈로
// 프롬프트를 조립해 run.start를 보낸다.
//
// 순서가 중요하다(Phase 0 handleRunCreate의 불변식):
//  1. Run 행을 queued로 먼저 만든다. Runner는 run.start를 받는 즉시 이벤트를
//     낼 수 있는데, store.ApplyEvent는 runs에 행이 없으면 ErrRunNotFound로
//     버린다. 순서를 뒤집으면 첫 이벤트가 조용히 유실된다.
//  2. 이슈를 in_progress로 옮긴다 — 명령을 보내기 *전에*. 뒤에 하면 빠른
//     Runner가 succeeded를 먼저 보내 이슈가 review가 된 다음 in_progress로
//     되돌아간다.
//  3. 명령을 보낸다. Runner가 없으면 방금 만든 행을 failed로 정리하고 이슈
//     상태를 되돌리고 503 — queued로 영영 남겨 사용자가 실행 중이라고 믿게
//     하지 않는다.
//
// 활성 Run이 있는 이슈는 다시 시작하지 않는다(409). 재시도는 정착된 뒤의
// 새 Run이다(PRD §7.6, handleRetry).
func (s *Server) handleIssueRun(w http.ResponseWriter, r *http.Request) {
	p, task, ok := s.loadIssue(w, r)
	if !ok {
		return
	}
	s.launch(w, r, p, task, launch{})
}

// launch는 새 Run 하나를 시작하는 데 필요한 것이다. previous가 있으면 재시도다.
type launch struct {
	previous       store.Run
	feedback       string
	workspaceRunID string // 비어 있으면 새 Run 자신
	resumeSession  string
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request, p store.Project, task store.Task, l launch) {
	runs, err := s.st.RunsForTask(task.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, existing := range runs {
		if !store.IsSettled(existing.State) {
			http.Error(w, "이미 실행 중인 Run이 있습니다: "+existing.ID, http.StatusConflict)
			return
		}
	}

	runID := "run-" + uuid.NewString()
	wsID := l.workspaceRunID
	if wsID == "" {
		wsID = runID
	}

	prompt := pipeline.Render(p.ExecuteTemplate, map[string]string{
		"issue":        pipeline.IssueText(task.Number, task.Title, task.Body),
		"previous_run": pipeline.PreviousRunText(l.previous.ID, l.previous.State, l.previous.Detail),
		"feedback":     l.feedback,
	})

	cmd, err := protocol.NewCommand(protocol.CmdRunStart, runID, protocol.RunStartBody{
		Prompt:          prompt,
		RepoPath:        p.RepoPath,
		BaseBranch:      p.DefaultBranch,
		WorkspaceRunID:  wsID,
		ResumeSessionID: l.resumeSession,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	run := store.Run{
		ID: runID, State: store.StateQueued, Kind: "structured", TaskID: task.ID, Stage: "execute",
		PreviousRunID: l.previous.ID, Feedback: l.feedback, WorkspaceRunID: wsID,
	}
	if err := s.st.UpsertRun(run); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 여기서부터 실패하면 방금 만든 Run을 failed로 정리하고 이슈 상태를 되돌린다 —
	// queued로 영영 남겨 사용자가 실행 중이라고 믿게 하지 않는다.
	abort := func(status int, msg string) {
		run.State = store.StateFailed
		_ = s.st.UpsertRun(run)
		_ = s.st.UpdateTaskStatus(task.ID, task.Status)
		http.Error(w, msg, status)
	}
	if err := s.st.UpdateTaskStatus(task.ID, store.TaskInProgress); err != nil {
		abort(http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.hub.SendCommand(cmd); err != nil {
		slog.Warn("run.start could not be delivered", "run_id", runID, "task_id", task.ID, "err", err)
		abort(http.StatusServiceUnavailable, "runner is not connected")
		return
	}
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

// handleCancel은 Run을 취소한다(PRD §7.6). 활성이면 러너에게 run.cancel을
// 보내고 종결 이벤트를 기다린다. needs_attention이면 프로세스가 없으므로
// 서버가 직접 cancelled로 옮긴다. 종결이면 취소할 것이 없다.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.GetRun(r.PathValue("id"))
	if errors.Is(err, store.ErrRunNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch {
	case run.State == store.StateNeedsAttention:
		if err := s.st.CancelSettledRun(run.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	case store.IsSettled(run.State):
		http.Error(w, "이미 끝난 Run입니다: "+run.State, http.StatusConflict)
		return
	default:
		cmd, err := protocol.NewCommand(protocol.CmdRunCancel, run.ID, nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.hub.SendCommand(cmd); err != nil {
			slog.Warn("run.cancel could not be delivered", "run_id", run.ID, "err", err)
			http.Error(w, "runner is not connected", http.StatusServiceUnavailable)
			return
		}
	}
	http.Redirect(w, r, s.backTo(run), http.StatusSeeOther)
}

// handleRetry는 정착된 Run 뒤에 새 Run을 단다(PRD §7.6). mode=continue는 같은
// worktree와 세션을 잇고, mode=fresh는 새 worktree·새 세션이다. 세션이 없는데
// continue를 고르면 409 — 조용히 fresh로 바꾸지 않는다.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	prev, err := s.st.GetRun(r.PathValue("id"))
	if errors.Is(err, store.ErrRunNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mode := r.FormValue("mode")
	if mode != "continue" && mode != "fresh" {
		http.Error(w, "mode는 continue 또는 fresh여야 합니다", http.StatusBadRequest)
		return
	}
	if !store.IsSettled(prev.State) {
		http.Error(w, "아직 끝나지 않은 Run은 재시도할 수 없습니다", http.StatusConflict)
		return
	}
	if prev.TaskID == "" {
		http.Error(w, "이슈 없이 만들어진 Run은 재시도할 수 없습니다", http.StatusConflict)
		return
	}
	task, err := s.st.GetTaskByID(prev.TaskID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p, err := s.st.GetProjectByID(task.ProjectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	l := launch{previous: prev, feedback: r.FormValue("feedback")}
	if mode == "continue" {
		if prev.ProviderSessionID == "" {
			http.Error(w, "이어갈 세션이 없습니다 — 처음부터 재시도를 고르세요", http.StatusConflict)
			return
		}
		l.resumeSession = prev.ProviderSessionID
		// worktree 주인은 체인의 첫 Run이다.
		l.workspaceRunID = prev.WorkspaceRunID
		if l.workspaceRunID == "" {
			l.workspaceRunID = prev.ID
		}
	}
	s.launch(w, r, p, task, l)
}

// backTo는 Run이 속한 이슈 페이지, 이슈가 없으면 Run 페이지다.
func (s *Server) backTo(run store.Run) string {
	if run.TaskID != "" {
		if task, err := s.st.GetTaskByID(run.TaskID); err == nil {
			if p, err := s.st.GetProjectByID(task.ProjectID); err == nil {
				return issuePath(p.Key, task.Number)
			}
		}
	}
	return "/runs/" + run.ID
}

func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	p, err := s.st.GetProject(r.PathValue("key"))
	if errors.Is(err, store.ErrProjectNotFound) {
		http.NotFound(w, r)
		return store.Project{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Project{}, false
	}
	return p, true
}

func (s *Server) loadIssue(w http.ResponseWriter, r *http.Request) (store.Project, store.Task, bool) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return store.Project{}, store.Task{}, false
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return store.Project{}, store.Task{}, false
	}
	task, err := s.st.GetTask(p.ID, n)
	if errors.Is(err, store.ErrTaskNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.Task{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Project{}, store.Task{}, false
	}
	return p, task, true
}

func issuePath(key string, number int) string {
	return fmt.Sprintf("/projects/%s/issues/%d", key, number)
}

// ---- Run ----

// eventView는 템플릿과 SSE가 함께 쓰는 이벤트 표현이다.
type eventView struct {
	Type      string `json:"type"`
	Seq       uint64 `json:"seq"`
	Summary   string `json:"summary"`
	RequestID string `json:"request_id,omitempty"`
	// ToolUseID는 승인 요청을 그 승인이 속한 tool_started와 잇는 유일한
	// 키다. Claude Code는 tool call을 병렬로 낼 수 있어 순서로는 구분이
	// 안 된다. 승인 패널이 이걸 보여줄 수 있도록 끝까지 들고 간다.
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// summarize는 봉투를 한 줄로 줄인다. 원본은 store에 그대로 있다.
//
// Runner가 발행하는 모든 이벤트의 body는 {"body": {...}, "raw": ...}로
// 감싸여 온다(lifecycle.eventBody). 여기서 그 겉껍질을 벗겨야 필드가
// 보인다 — 벗기지 않으면 모든 행이 빈 채로 렌더링된다.
func summarize(env protocol.Envelope) eventView {
	view := eventView{Type: env.Type, Seq: env.Seq}

	var outer struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body == nil {
		view.Summary = string(env.Body)
		return view
	}

	switch env.Type {
	case protocol.EvMessageDelta:
		view.Summary, _ = outer.Body["text"].(string)
	case protocol.EvToolStarted:
		name, _ := outer.Body["tool_name"].(string)
		view.Summary = "→ " + name
	case protocol.EvToolFinished:
		view.Summary = "← 완료"
	case protocol.EvRunStateChanged:
		state, _ := outer.Body["state"].(string)
		detail, _ := outer.Body["detail"].(string)
		view.Summary = state
		if detail != "" {
			view.Summary += " — " + detail
		}
	case protocol.EvApprovalRequested:
		name, _ := outer.Body["tool_name"].(string)
		view.RequestID, _ = outer.Body["request_id"].(string)
		view.ToolUseID, _ = outer.Body["tool_use_id"].(string)
		view.Summary = "승인 요청: " + name
		if view.ToolUseID != "" {
			view.Summary += " (" + view.ToolUseID + ")"
		}
	default:
		raw, _ := json.Marshal(outer.Body)
		view.Summary = string(raw)
	}
	return view
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	run, err := s.st.GetRun(id)
	if errors.Is(err, store.ErrRunNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 초기 렌더링은 항상 store에서 온다. SSE는 느린 구독자의 이벤트를
	// 흘릴 수 있으므로(hub.fanout), 새로고침하면 store의 내구성 있는
	// 기록으로 그 이벤트가 다시 보여야 한다.
	stored, err := s.st.Events(id, 0, 500)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]eventView, 0, len(stored))
	for _, env := range stored {
		views = append(views, summarize(env))
	}

	// 이슈에서 시작된 Run이면 돌아갈 곳을 붙인다. Phase 0 시절의 Run은 없다.
	back := "/"
	var issueLabel string
	if run.TaskID != "" {
		if task, err := s.st.GetTaskByID(run.TaskID); err == nil {
			if p, err := s.st.GetProjectByID(task.ProjectID); err == nil {
				back = issuePath(p.Key, task.Number)
				issueLabel = fmt.Sprintf("#%d %s", task.Number, task.Title)
			}
		}
	}

	s.render(w, s.run, map[string]any{
		"Title": id, "Run": run, "Events": views, "Back": back, "IssueLabel": issueLabel,
	})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case env, ok := <-events:
			if !ok {
				return
			}
			if env.RunID != id {
				continue
			}
			raw, err := json.Marshal(summarize(env))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

type approveRequest struct {
	RequestID string `json:"request_id"`
	Allow     bool   `json:"allow"`
	Message   string `json:"message"`
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body approveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cmd, err := protocol.NewCommand(protocol.CmdApprovalDecision, id, map[string]any{
		"request_id": body.RequestID,
		"allow":      body.Allow,
		"message":    body.Message,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Runner가 없으면 결정이 갈 곳이 없다. 성공으로 위장하지 않는다 —
	// 그러면 사용자는 답했다고 믿지만 에이전트는 영원히 막혀 있게 된다.
	if err := s.hub.SendCommand(cmd); err != nil {
		slog.Warn("approval decision could not be delivered", "run_id", id, "err", err)
		http.Error(w, "runner is not connected", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("render failed", "err", err)
	}
}
