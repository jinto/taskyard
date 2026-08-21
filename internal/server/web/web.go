// Package web은 Server의 HTML UI다.
//
// 보드와 목록은 서버 렌더링이고, Run 상세의 이벤트 스트림만 SSE를 구독하는
// JS 아일랜드다(PRD §11.4.1).
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	st   *store.Store
	hub  *hub.Hub
	runs *template.Template
	run  *template.Template
}

func New(st *store.Store, h *hub.Hub) (*Server, error) {
	runs, err := template.ParseFS(templateFS, "templates/layout.html", "templates/runs.html")
	if err != nil {
		return nil, fmt.Errorf("parse runs template: %w", err)
	}
	run, err := template.ParseFS(templateFS, "templates/layout.html", "templates/run.html")
	if err != nil {
		return nil, fmt.Errorf("parse run template: %w", err)
	}
	return &Server{st: st, hub: h, runs: runs, run: run}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /runs", s.handleRunCreate)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /runs/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /runs/{id}/approve", s.handleApprove)
	return mux
}

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

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	points, err := s.st.ResumePoints()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var runs []store.Run
	for id := range points {
		r, err := s.st.GetRun(id)
		if err != nil {
			continue
		}
		runs = append(runs, r)
	}

	s.render(w, s.runs, map[string]any{"Title": "실행 목록", "Runs": runs})
}

// handleRunCreate는 하드코딩된 Task 하나를 실제로 기동하는 유일한 문이다
// (PRD §16.0). 폼 필드로 받는 이유는 승인 패널과 달리 이 화면은 JS 없이도
// 동작해야 하는 순수 HTML 폼이기 때문이다.
//
// 명령을 보내기 전에 먼저 Run을 queued로 만들어야 한다: Runner가 run.start를
// 받는 즉시 이벤트를 낼 수 있는데, store.ApplyEvent는 runs 테이블에 행이
// 없으면 그 이벤트를 ErrRunNotFound로 버린다(store.go). 순서를 뒤집으면
// 첫 이벤트가 조용히 유실된다.
//
// Runner가 연결돼 있지 않으면 명령이 갈 곳이 없다. 방금 만든 행을 queued로
// 영영 남기면 사용자는 실행 중이라고 믿게 되므로, 그 자리에서 failed로
// 정리하고 503을 돌려준다 — 성공한 척 위장하지 않는다.
func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	prompt := r.FormValue("prompt")
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	runID := "run-" + uuid.NewString()

	cmd, err := protocol.NewCommand(protocol.CmdRunStart, runID, map[string]any{"prompt": prompt})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.st.UpsertRun(store.Run{ID: runID, State: store.StateQueued, Kind: "structured"}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.hub.SendCommand(cmd); err != nil {
		slog.Warn("run.start could not be delivered", "run_id", runID, "err", err)
		_ = s.st.UpsertRun(store.Run{ID: runID, State: store.StateFailed, Kind: "structured"})
		http.Error(w, "runner is not connected", http.StatusServiceUnavailable)
		return
	}

	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
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

	s.render(w, s.run, map[string]any{"Title": id, "Run": run, "Events": views})
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
