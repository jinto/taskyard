package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
)

func newServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s, err := web.New(st, hub.New(st, "tok"))
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return st, s.Routes()
}

func TestIndexListsRuns(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{
		ID: "run-1", State: store.StateRunning, Kind: "structured", Branch: "taskyard/run/run-1",
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run-1") {
		t.Errorf("index does not list run-1:\n%s", body)
	}
	if !strings.Contains(body, "taskyard/run/run-1") {
		t.Errorf("index does not show the branch:\n%s", body)
	}
}

// TestRunDetailReplaysStoredEvents는 run.state_changed 이벤트를 저장한 뒤
// 상세 페이지가 그 봉투를 봉투->body 래핑을 풀어(unwrap) 렌더링하는지
// 확인한다. 이 래핑은 lifecycle의 모든 publish 지점(state change, parser
// events, approval, salvage)이 공통으로 쓰는 형태이며, 승인 이벤트가 아닌
// 평범한 이벤트에서 먼저 깨지는 회귀를 잡기 위해 approval 경로가 아닌
// message_delta를 검증한다.
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

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_delta") {
		t.Errorf("detail page does not replay stored events:\n%s", body)
	}
	// 이게 이 테스트의 핵심이다: 타입 라벨만으로는 봉투가 실제로
	// 언랩됐다는 증거가 안 된다. body.text가 그대로 문자열로 남아있으면
	// (예: {"body":{"text":"..."}} 전체가 그대로 출력되면) 렌더러가
	// unwrap을 안 한 것이다. 실제 요약 문구가 나와야 unwrap이 된 것이다.
	if !strings.Contains(body, "hello from the agent") {
		t.Errorf("detail page did not unwrap the envelope body; raw summary missing:\n%s", body)
	}
}

func TestUnknownRunIs404(t *testing.T) {
	_, h := newServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestApprovalEventUnwrapsToolUseID는 승인 요청 이벤트에서 tool_use_id가
// eventView까지 살아서 상세 페이지에 노출되는지 확인한다. 병렬 tool call
// 상황에서 request_id만으로는 어느 tool_started와 짝인지 알 수 없으므로
// tool_use_id를 빠뜨리면 안 된다.
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

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

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
