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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jinto/taskyard/internal/agents/adapter/claudecode"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/launch"
	"github.com/jinto/taskyard/internal/server/pipeline"
	"github.com/jinto/taskyard/internal/server/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// keyPattern은 프로젝트 key의 문법이다. URL 경로 조각으로 그대로 쓰인다.
var keyPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// badgeClass는 이슈·Run·PR 상태를 색으로 옮긴다. 상태가 곧 정보다: 움직이는
// 것은 파랑, 사람의 차례는 주황, 끝난 것은 초록, 실패는 빨강, 쉬는 것은 회색.
func badgeClass(state string) string {
	switch state {
	case store.StateQueued, store.StateRunning, store.TaskInProgress, "OPEN":
		return "bg-sky-100 text-sky-900 dark:bg-sky-900/40 dark:text-sky-200"
	case store.StateWaitingApproval, store.StateNeedsAttention, store.TaskReview:
		return "bg-attention-soft text-attention dark:bg-attention/25 dark:text-orange-200"
	case store.StateSucceeded, store.TaskDone, "MERGED":
		return "bg-go-soft text-go dark:bg-go/25 dark:text-emerald-200"
	case store.StateFailed, store.StateOrphaned:
		return "bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-200"
	default:
		return "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300"
	}
}

// dotClass는 상태 점의 색이다. 촘촘한 보드에서는 배지 대신 점이 상태를 나른다 —
// 돌아가는 Run만 깜빡여서, 멈춘 것과 움직이는 것이 한눈에 갈린다.
func dotClass(state string) string {
	switch state {
	case store.StateRunning:
		return "bg-sky-500 animate-pulse"
	case store.StateQueued:
		return "bg-zinc-400"
	case store.StateSucceeded:
		return "bg-go"
	case store.StateFailed, store.StateOrphaned:
		return "bg-red-500"
	case store.StateNeedsAttention, store.StateWaitingApproval:
		return "bg-attention"
	default:
		return "bg-zinc-300 dark:bg-zinc-600"
	}
}

// since는 사람이 읽는 상대 시각이다. 보드에서는 절대 시각이 필요 없다 —
// "얼마나 오래 저러고 있나"만 알면 된다.
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "방금"
	case d < time.Hour:
		return fmt.Sprintf("%d분 전", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d시간 전", int(d.Hours()))
	default:
		return fmt.Sprintf("%d일 전", int(d.Hours()/24))
	}
}

// checkMark는 PR 체크 요약(none/pending/success/failure)을 기호 하나로 줄인다.
// 볼 것이 없으면 nil이라 템플릿의 with가 통째로 건너뛴다.
type checkMark struct{ Mark, Class string }

func prChecks(s string) *checkMark {
	switch s {
	case "success":
		return &checkMark{"✓", "text-go"}
	case "failure":
		return &checkMark{"✕", "text-red-600 dark:text-red-400"}
	case "pending":
		return &checkMark{"◌", "text-muted"}
	}
	return nil
}

type Server struct {
	st  *store.Store
	hub *hub.Hub

	launcher *launch.Launcher

	projects *template.Template
	project  *template.Template
	settings *template.Template
	issue    *template.Template
	run      *template.Template
	artifact *template.Template
}

func New(st *store.Store, h *hub.Hub) (*Server, error) {
	var err error
	page := func(name string) *template.Template {
		t, perr := template.New("layout.html").Funcs(template.FuncMap{
			"badge": badgeClass, "seg": url.PathEscape, "dot": dotClass,
			"since": since, "checks": prChecks, "upper": strings.ToUpper,
		}).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name)
		if perr != nil && err == nil {
			err = fmt.Errorf("parse %s: %w", name, perr)
		}
		return t
	}
	s := &Server{
		st: st, hub: h, launcher: &launch.Launcher{Store: st, Commander: h},
		projects: page("projects.html"),
		project:  page("project.html"),
		settings: page("settings.html"),
		issue:    page("issue.html"),
		run:      page("run.html"),
		artifact: page("artifact.html"),
	}
	return s, err
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleProjects)
	mux.HandleFunc("POST /projects", sameOrigin(s.handleProjectCreate))
	mux.HandleFunc("GET /projects/{key}", s.handleProject)
	mux.HandleFunc("GET /projects/{key}/settings", s.handleSettings)
	mux.HandleFunc("GET /projects/{key}/stream", s.handleBoardStream)
	mux.HandleFunc("POST /projects/{key}/template", sameOrigin(s.handleTemplateUpdate))
	mux.HandleFunc("POST /projects/{key}/issues", sameOrigin(s.handleIssueCreate))
	mux.HandleFunc("GET /projects/{key}/issues/{n}", s.handleIssue)
	mux.HandleFunc("POST /projects/{key}/issues/{n}/run", sameOrigin(s.handleIssueRun))
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /runs/{id}/artifacts/{name}", s.handleArtifact)
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
	pending, err := s.st.PendingApprovals()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, s.projects, map[string]any{"Title": "프로젝트", "Projects": projects, "Pending": pending})
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p := store.Project{
		Key:              r.FormValue("key"),
		Name:             r.FormValue("name"),
		RepoPath:         r.FormValue("repo_path"),
		DefaultBranch:    r.FormValue("default_branch"),
		ExecuteTemplate:  pipeline.DefaultExecuteTemplate,
		AllowedTools:     pipeline.DefaultAllowedTools,
		CreatePR:         r.FormValue("create_pr") != "",
		CleanupMerged:    r.FormValue("cleanup_merged") != "",
		AnalyzeEnabled:   true,
		AnalyzeSkipBelow: 200,
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

// column은 보드의 세로줄 하나다. 열은 이슈 상태와 1:1이고 순서가 곧 흐름이다.
type column struct {
	Status string
	Title  string
	Dot    string
	Cards  []card
}

// card는 이슈 하나와, 있다면 그 이슈의 가장 최근 Run이다. Run은 포인터다 —
// 템플릿의 with가 "Run이 없는 이슈"를 구분할 수 있어야 하기 때문이다.
type card struct {
	Task store.Task
	Run  *store.Run
	// Waiting은 이 카드의 Run이 지금 사람의 승인을 기다린다는 뜻이다. 보드에서
	// 유일하게 손이 필요한 카드이므로 눈에 띄게 그린다.
	Waiting bool
	// Live는 러너가 지금 이 이슈를 붙들고 있다는 뜻이다 — 아이콘이 돈다.
	// 사람을 기다리는 동안은 돌지 않는다: 그때 움직이는 것은 아무것도 없다.
	Live bool
	// Line은 카드가 말하는 한 줄이다. 돌아가는 중이면 방금 한 일, 기다리는
	// 중이면 무엇을 승인해 달라는지, 멈췄으면 왜 멈췄는지. SSE가 이 자리를
	// 새로 고친다.
	Line string
}

// stopLine은 멈춘 Run의 한 줄이다. 이유가 없으면 이름만 남는다.
func stopLine(label, detail string) string {
	if d := firstLine(detail, 90); d != "" {
		return label + " · " + d
	}
	return label
}

// handleProject는 프로젝트를 칸반 보드로 그린다. 열 사이로 카드를 끌어 옮기는
// 기능은 없다 — 이슈 상태는 사람이 정하는 것이 아니라 Run의 결과로 정해진다
// (PRD §7.5). 사람이 여기서 하는 일은 대기 중인 이슈를 실행시키는 것뿐이다.
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
	latest, err := s.st.LatestRunByTask(p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	waiting := map[string]string{}
	if pending, err := s.st.PendingApprovals(); err == nil {
		for _, a := range pending {
			waiting[a.RunID] = a.Command
		}
	}
	activity, err := s.st.LatestActivityByProject(p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cols := []column{
		{Status: store.TaskBacklog, Title: "대기", Dot: "bg-zinc-400"},
		{Status: store.TaskInProgress, Title: "진행", Dot: "bg-sky-500"},
		{Status: store.TaskReview, Title: "리뷰", Dot: "bg-attention"},
		{Status: store.TaskDone, Title: "완료", Dot: "bg-go"},
	}
	index := map[string]int{}
	for i, c := range cols {
		index[c.Status] = i
	}
	for _, task := range tasks {
		i, ok := index[task.Status]
		if !ok {
			i = 0 // 모르는 상태는 대기로 — 카드가 사라지는 것보다 낫다.
		}
		c := card{Task: task}
		if run, ok := latest[task.ID]; ok {
			c.Run = &run
			cmd, isWaiting := waiting[run.ID]
			switch {
			case isWaiting:
				// 배지가 이미 "승인 대기"라고 말한다. 한 줄은 무엇을 승인해
				// 달라는지만 말한다.
				c.Waiting = true
				c.Line = firstLine(cmd, 90)
			case run.State == store.StateNeedsAttention:
				// 에이전트가 스스로 멈추고 사람의 판단을 남겨 두었다(PRD §7.5).
				c.Line = stopLine("판단 필요", run.Detail)
			case run.State == store.StateFailed || run.State == store.StateOrphaned:
				c.Line = stopLine("멈춤", run.Detail)
			case run.State == store.StateRunning || run.State == store.StateQueued:
				c.Live = true
				if env, ok := activity[run.ID]; ok {
					c.Line = activityLine(env)
				}
			}
		}
		cols[i].Cards = append(cols[i].Cards, c)
	}

	s.render(w, s.project, map[string]any{
		"Title": p.Name, "Project": p, "Columns": cols, "Backlog": store.TaskBacklog, "Wide": true,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	s.render(w, s.settings, map[string]any{"Title": p.Name + " 설정", "Project": p})
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
	// 허용 도구는 줄 단위. 빈 줄과 양끝 공백은 버리고, 문법은 러너가 쓰는
	// 규칙(claudecode.CheckAllowedTools)으로 여기서 먼저 거른다 — 틀린 항목이
	// 러너까지 가서 Run을 실패시키지 않게. 틀리면 아무것도 바꾸지 않는다.
	var tools []string
	for _, line := range strings.Split(r.FormValue("allowed_tools"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tools = append(tools, line)
		}
	}
	if err := claudecode.CheckAllowedTools(tools); err != nil {
		http.Error(w, "허용 도구 형식이 잘못됐습니다: "+err.Error(), http.StatusBadRequest)
		return
	}
	repoPath := strings.TrimSpace(r.FormValue("repo_path"))
	if !filepath.IsAbs(repoPath) {
		http.Error(w, "저장소 경로는 러너 머신의 절대 경로여야 합니다", http.StatusBadRequest)
		return
	}
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		branch = "main"
	}
	skipBelow := 0
	if v := strings.TrimSpace(r.FormValue("analyze_skip_below")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "분석 생략 기준은 0 이상의 정수여야 합니다", http.StatusBadRequest)
			return
		}
		skipBelow = n
	}
	// 체크박스는 안 보내면 false — 폼이 항상 그 필드들을 다루므로 그대로 저장한다.
	if err := s.st.UpdateProjectSettings(p.Key, store.ProjectSettings{
		RepoPath:         repoPath,
		DefaultBranch:    branch,
		ExecuteTemplate:  r.FormValue("execute_template"),
		AllowedTools:     tools,
		CreatePR:         r.FormValue("create_pr") != "",
		CleanupMerged:    r.FormValue("cleanup_merged") != "",
		AnalyzeTemplate:  r.FormValue("analyze_template"),
		AnalyzeEnabled:   r.FormValue("analyze_enabled") != "",
		AnalyzeSkipBelow: skipBelow,
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Key+"/settings", http.StatusSeeOther)
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
	if _, err := s.st.CreateTask(store.Task{ProjectID: p.ID, Title: title, Body: r.FormValue("body")}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 이슈를 만든 사람은 보드를 보고 있었다. 상세 화면으로 끌고 가지 않는다 —
	// 새 카드는 보드에 이미 있고, 실행 버튼도 거기 있다.
	http.Redirect(w, r, "/projects/"+p.Key, http.StatusSeeOther)
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
	artifacts := map[string][]store.Artifact{}
	for _, run := range runs {
		if list, err := s.st.Artifacts(run.ID); err == nil && len(list) > 0 {
			artifacts[run.ID] = list
		}
	}
	data := map[string]any{
		"Title": fmt.Sprintf("#%d %s", task.Number, task.Title), "Project": p, "Task": task, "Runs": runs,
		"HasLatest": len(runs) > 0, "Done": task.Status == store.TaskDone, "Artifacts": artifacts,
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	o := launch.Options{Stage: launch.ChooseStage(p, task, r.FormValue("stage"))}
	if o.Stage == store.StageExecute {
		// "바로 실행"은 이슈의 가장 최근 성공한 1단계 보고서를 쓴다(없으면 없이).
		if a, ok, _ := s.st.LatestSucceededRun(task.ID, store.StageAnalyze); ok {
			o.ReportRunID = a.ID
		}
	}
	s.start(w, r, p, task, o)
}

// start는 Launcher.Start를 HTTP로 옮긴다: 오류를 상태 코드로, 성공을 Run 화면으로.
func (s *Server) start(w http.ResponseWriter, r *http.Request, p store.Project, task store.Task, o launch.Options) {
	run, err := s.launcher.Start(p, task, o)
	switch {
	case errors.Is(err, launch.ErrRunActive):
		http.Error(w, "이미 실행 중인 Run이 있습니다", http.StatusConflict)
	case errors.Is(err, launch.ErrRunnerUnavailable):
		http.Error(w, "runner is not connected", http.StatusServiceUnavailable)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, "/runs/"+run.ID, http.StatusSeeOther)
	}
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

	// 재시도는 이전 Run의 단계와 보고서 계보를 잇는다.
	o := launch.Options{Stage: prev.Stage, Previous: prev, Feedback: r.FormValue("feedback"), ReportRunID: prev.ReportRunID}
	if mode == "continue" {
		if prev.ProviderSessionID == "" {
			http.Error(w, "이어갈 세션이 없습니다 — 처음부터 재시도를 고르세요", http.StatusConflict)
			return
		}
		o.ResumeSession = prev.ProviderSessionID
		// worktree 주인은 체인의 첫 Run이다.
		o.WorkspaceRunID = prev.WorkspaceRunID
		if o.WorkspaceRunID == "" {
			o.WorkspaceRunID = prev.ID
		}
	}
	s.start(w, r, p, task, o)
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
	// Input은 도구 호출의 핵심 인자(Bash면 명령, 파일 도구면 경로)다. 사람이
	// 승인을 결정하려면 도구 이름만으로는 부족하다.
	Input string `json:"input,omitempty"`
	// State·Badge는 run.state_changed에만 있다. 페이지의 상태 배지를 새로고침
	// 없이 갱신하기 위해 서버가 배지 스타일까지 정해 보낸다.
	State string `json:"state,omitempty"`
	Badge string `json:"badge,omitempty"`
	// Rule은 승인 요청에서 만든 "앞으로 허용" 규칙이다. 만들 수 없으면 빈 값.
	Rule string `json:"rule,omitempty"`
}

// firstLine은 여러 줄 텍스트에서 비어 있지 않은 첫 줄을 max 글자까지 돌려준다.
// CRLF의 \r도 걷어낸다.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > max {
			return string(r[:max]) + "…"
		}
		return line
	}
	return ""
}

// describeInput은 도구 입력에서 사람이 볼 한 줄을 고른다. 모르는 도구는 JSON.
func describeInput(input any) string {
	m, ok := input.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	for _, key := range []string{"command", "file_path", "pattern", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	raw, _ := json.Marshal(m)
	if len(raw) > 200 {
		raw = append(raw[:200], "…"...)
	}
	return string(raw)
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
		if in := describeInput(outer.Body["input"]); in != "" {
			view.Summary += ": " + in
		}
	case protocol.EvToolFinished:
		view.Summary = "← 완료"
		if isErr, _ := outer.Body["is_error"].(bool); isErr {
			view.Summary = "← 오류"
		}
		if line := firstLine(fmt.Sprint(outer.Body["output"]), 160); line != "" && line != "<nil>" {
			view.Summary += ": " + line
		}
	case protocol.EvUsageUpdated:
		// 로그 행이 아니라 머리말 한 줄. 서버 렌더링(handleRun)과 SSE(JS)가
		// 같은 문장을 쓴다.
		status, _ := outer.Body["status"].(string)
		view.Summary = "사용량 " + status
		if resets, ok := outer.Body["resets_at"].(float64); ok && resets > 0 {
			view.Summary += " · 초기화 " + time.Unix(int64(resets), 0).Local().Format("01-02 15:04")
		}
	case protocol.EvRunStateChanged:
		state, _ := outer.Body["state"].(string)
		detail, _ := outer.Body["detail"].(string)
		view.State, view.Badge = state, badgeClass(state)
		view.Summary = state
		if detail != "" {
			view.Summary += " — " + detail
		}
	case protocol.EvApprovalRequested:
		name, _ := outer.Body["tool_name"].(string)
		view.RequestID, _ = outer.Body["request_id"].(string)
		view.ToolUseID, _ = outer.Body["tool_use_id"].(string)
		view.Input = describeInput(outer.Body["input"])
		input, _ := outer.Body["input"].(map[string]any)
		view.Rule = SuggestRule(name, input)
		view.Summary = "승인 요청: " + name
		if view.Input != "" {
			view.Summary += ": " + view.Input
		}
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
	artifacts, _ := s.st.Artifacts(id)
	views := make([]eventView, 0, len(stored))
	var usage string
	for _, env := range stored {
		view := summarize(env)
		if env.Type == protocol.EvUsageUpdated {
			usage = view.Summary
			continue
		}
		views = append(views, view)
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
		"Title": id, "Run": run, "Events": views, "Back": back, "IssueLabel": issueLabel, "Usage": usage, "Artifacts": artifacts,
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

// ---- 보드의 실시간 갱신 ----

// boardEvent는 보드가 카드 하나를 고쳐 그리는 데 필요한 전부다. SSE로 나간다.
type boardEvent struct {
	Task int    `json:"task"`
	Line string `json:"line,omitempty"`
	// State가 있으면 그 이슈는 열을 옮겼을 수 있다. 열을 정하는 것은 서버의
	// 일이므로 보드는 다시 그리지 않고 다시 받아온다.
	State string `json:"state,omitempty"`
	// Waiting은 사람을 기다리기 시작했다는 뜻, Resumed는 그 기다림이 끝났다는 뜻.
	Waiting bool `json:"waiting,omitempty"`
	Resumed bool `json:"resumed,omitempty"`
}

// activityLine은 진행 이벤트 하나를 카드의 한 줄로 줄인다. Run 상세와 같은
// 문장을 쓰되 카드에 들어갈 만큼만 자른다.
func activityLine(env protocol.Envelope) string {
	return firstLine(summarize(env).Summary, 90)
}

// boardView는 이벤트 하나를 카드 갱신으로 옮긴다. 보드가 신경 쓰지 않는
// 이벤트(사용량, 산출물 따위)면 ok가 거짓이다.
func boardView(env protocol.Envelope, task int) (boardEvent, bool) {
	ev := boardEvent{Task: task}
	switch env.Type {
	case protocol.EvMessageDelta, protocol.EvToolStarted, protocol.EvToolFinished:
		ev.Line = activityLine(env)
		// 도구가 끝났다는 것은 그 도구의 승인도 끝났다는 뜻이다(Run 상세와 같다).
		ev.Resumed = env.Type == protocol.EvToolFinished
	case protocol.EvApprovalRequested:
		view := summarize(env)
		line := view.Input
		if line == "" {
			line = view.Summary
		}
		ev.Line, ev.Waiting = firstLine(line, 90), true
	case protocol.EvRunStateChanged:
		if ev.State = summarize(env).State; ev.State == "" {
			return boardEvent{}, false
		}
	default:
		return boardEvent{}, false
	}
	return ev, true
}

// handleBoardStream은 이 프로젝트의 Run에서 나오는 것만 보드로 흘려보낸다.
// PRD §11.4.1은 실시간 화면을 Run 상세 하나로 두었지만, 보드에 카드가 여럿
// 돌아가는 지금은 보드도 살아 있어야 한다 — 어느 카드가 지금 무엇을 하는지
// 보려고 상세로 들어갔다 나오는 것이 일이 되어서는 안 된다.
func (s *Server) handleBoardStream(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
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

	// 구독이 붙었다는 것을 곧바로 알린다. 첫 이벤트까지 머리말을 붙들면
	// 연결이 섰는지 아무도 알 수 없다.
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	// run_id → 이슈 번호. 0은 "이 프로젝트의 Run이 아니다"라는 뜻으로 남긴다 —
	// 남의 프로젝트가 쏟아내는 이벤트마다 원장을 두드리지 않기 위해서다.
	number := map[string]int{}
	for {
		select {
		case <-r.Context().Done():
			return
		case env, ok := <-events:
			if !ok {
				return
			}
			if env.RunID == "" {
				continue
			}
			n, seen := number[env.RunID]
			if !seen {
				n = s.taskNumber(p.ID, env.RunID)
				number[env.RunID] = n
			}
			if n == 0 {
				continue
			}
			view, ok := boardView(env, n)
			if !ok {
				continue
			}
			raw, err := json.Marshal(view)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

// taskNumber는 Run이 이 프로젝트의 몇 번 이슈의 것인지를 찾는다. 아니면 0.
func (s *Server) taskNumber(projectID, runID string) int {
	run, err := s.st.GetRun(runID)
	if err != nil || run.TaskID == "" {
		return 0
	}
	task, err := s.st.GetTaskByID(run.TaskID)
	if err != nil || task.ProjectID != projectID {
		return 0
	}
	return task.Number
}

type approveRequest struct {
	RequestID string `json:"request_id"`
	Allow     bool   `json:"allow"`
	Message   string `json:"message"`
	// Remember가 참이면 이 도구 호출에 해당하는 규칙을 프로젝트 허용 목록에
	// 넣는다 — 다음 Run부터 묻지 않는다. 진행 중인 Run은 시작할 때 받은
	// 목록으로 돌므로 이번 실행에서는 계속 묻는다.
	Remember bool `json:"remember"`
}

// multiWordCommands는 첫 낱말만으로는 너무 넓은 명령이다. "git" 하나를 허용하면
// push·reset까지 허용된다 — 두 낱말까지 규칙에 넣는다.
var multiWordCommands = map[string]bool{
	"git": true, "go": true, "gh": true, "npm": true, "pnpm": true, "yarn": true,
	"cargo": true, "docker": true, "kubectl": true, "uv": true, "make": true, "brew": true,
}

// SuggestRule은 승인 요청 하나에서 "앞으로 허용" 규칙을 만든다. Bash는 명령의
// 첫 낱말(멀티플렉서면 둘째까지), 나머지는 도구 이름 그대로. 규칙으로 만들 수
// 없으면 빈 문자열 — 여러 명령을 묶은 것이 대표적이다(어떤 접두사 규칙에도
// 맞지 않으므로 허용해 봐야 소용이 없다).
func SuggestRule(toolName string, input map[string]any) string {
	if toolName == "" {
		return ""
	}
	if toolName != "Bash" {
		if claudecode.CheckAllowedTools([]string{toolName}) != nil {
			return ""
		}
		return toolName
	}
	command, _ := input["command"].(string)
	if strings.ContainsAny(command, ";&|\n") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	prefix := fields[0]
	if multiWordCommands[prefix] && len(fields) > 1 {
		prefix += " " + fields[1]
	}
	rule := "Bash(" + prefix + ":*)"
	if claudecode.CheckAllowedTools([]string{rule}) != nil {
		return ""
	}
	return rule
}

// rememberRule은 승인된 도구의 규칙을 프로젝트 허용 목록에 더한다. 이미 있으면
// 아무것도 하지 않는다.
func (s *Server) rememberRule(runID, requestID string) {
	run, err := s.st.GetRun(runID)
	if err != nil || run.TaskID == "" {
		return
	}
	events, err := s.st.Events(runID, 0, 500)
	if err != nil {
		return
	}
	rule := ""
	for _, env := range events {
		if env.Type != protocol.EvApprovalRequested {
			continue
		}
		var outer struct {
			Body map[string]any `json:"body"`
		}
		if json.Unmarshal(env.Body, &outer) != nil {
			continue
		}
		if id, _ := outer.Body["request_id"].(string); id != requestID {
			continue
		}
		name, _ := outer.Body["tool_name"].(string)
		input, _ := outer.Body["input"].(map[string]any)
		rule = SuggestRule(name, input)
	}
	if rule == "" {
		return
	}
	task, err := s.st.GetTaskByID(run.TaskID)
	if err != nil {
		return
	}
	p, err := s.st.GetProjectByID(task.ProjectID)
	if err != nil || slices.Contains(p.AllowedTools, rule) {
		return
	}
	if err := s.st.UpdateProjectSettings(p.Key, store.ProjectSettings{
		RepoPath: p.RepoPath, DefaultBranch: p.DefaultBranch,
		ExecuteTemplate: p.ExecuteTemplate, AllowedTools: append(p.AllowedTools, rule),
		CreatePR: p.CreatePR, CleanupMerged: p.CleanupMerged,
		AnalyzeTemplate: p.AnalyzeTemplate, AnalyzeEnabled: p.AnalyzeEnabled, AnalyzeSkipBelow: p.AnalyzeSkipBelow,
	}); err != nil {
		slog.Error("remember rule failed", "project", p.Key, "rule", rule, "err", err)
		return
	}
	slog.Info("remembered approval rule", "project", p.Key, "rule", rule)
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

	if body.Allow && body.Remember {
		s.rememberRule(id, body.RequestID)
	}
	// 화면에서 즉시 지운다 — 러너의 tool_finished를 기다리지 않는다.
	if err := s.st.ClearPendingApproval(id, body.RequestID); err != nil {
		slog.Error("clear pending approval failed", "run_id", id, "err", err)
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

// handleArtifact는 산출물 하나를 그대로 보인다(<pre>). 렌더링은 다음 항목.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.Artifact(r.PathValue("id"), r.PathValue("name"))
	if errors.Is(err, store.ErrArtifactNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, s.artifact, map[string]any{"Title": a.Name, "Artifact": a})
}
