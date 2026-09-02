package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postCtx(t *testing.T, h http.Handler, token, body string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInitializeAdvertisesTools(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "tok", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var resp struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Fatalf("initialize did not advertise a tools capability: %s", rec.Body)
	}
}

func TestToolsListExposesApprove(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "tok", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "approve" {
		t.Fatalf("tools = %+v, want exactly [approve]", resp.Result.Tools)
	}
}

func TestMissingTokenIsRejected(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// 실제 캡처에 들어 있는 tool_use_id. Task 11이 승인 요청과 tool_started
// 이벤트를 연결하는 유일한 키이므로 추측이 아니라 캡처 값 그대로 고정한다.
const capturedToolUseID = "toolu_01P1SZNbTpBrMuYYBCdig938"

// 실제 캡처를 그대로 흘려넣어 요청 파싱이 계약과 맞는지 확인한다.
func TestToolsCallFromRealCaptureSurfacesARequest(t *testing.T) {
	body, err := os.ReadFile("testdata/tools-call-approve.json")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	b := New("tok")
	done := make(chan struct{})

	go func() {
		defer close(done)
		select {
		case req := <-b.Requests():
			if req.ToolName == "" {
				t.Errorf("request has no tool name; check the arguments field tags against the capture")
			}
			if req.ToolUseID != capturedToolUseID {
				t.Errorf("ToolUseID = %q, want %q (from the capture)", req.ToolUseID, capturedToolUseID)
			}
			_ = b.Decide(req.ID, Decision{Allow: true})
		case <-time.After(5 * time.Second):
			t.Error("no approval request surfaced from a real tools/call capture")
		}
	}()

	rec := post(t, b.Handler(), "tok", string(body))
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestAllowDecisionBecomesPermissionResult(t *testing.T) {
	b := New("tok")
	call := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`

	go func() {
		req := <-b.Requests()
		_ = b.Decide(req.ID, Decision{Allow: true, UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)})
	}()

	rec := post(t, b.Handler(), "tok", call)

	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %+v, want one text block", resp.Result.Content)
	}

	var pr struct {
		Behavior     string          `json:"behavior"`
		UpdatedInput json.RawMessage `json:"updatedInput"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &pr); err != nil {
		t.Fatalf("permission result is not JSON: %v (%s)", err, resp.Result.Content[0].Text)
	}
	if pr.Behavior != "allow" {
		t.Errorf("behavior = %q, want allow", pr.Behavior)
	}
	// allow는 updatedInput을 반드시 포함해야 한다. 빠지면 옛 버전에서 거부된다.
	if len(pr.UpdatedInput) == 0 {
		t.Error("allow result omitted updatedInput")
	}
}

func TestDenyDecisionCarriesMessage(t *testing.T) {
	b := New("tok")
	call := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"rm -rf /"}}}}`

	go func() {
		req := <-b.Requests()
		_ = b.Decide(req.ID, Decision{Allow: false, Message: "사용자가 거절했습니다"})
	}()

	rec := post(t, b.Handler(), "tok", call)

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var pr struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Behavior != "deny" {
		t.Errorf("behavior = %q, want deny", pr.Behavior)
	}
	if pr.Message != "사용자가 거절했습니다" {
		t.Errorf("message = %q", pr.Message)
	}
}

func TestDecideOnUnknownRequestFails(t *testing.T) {
	b := New("tok")
	if err := b.Decide("nope", Decision{Allow: true}); err == nil {
		t.Fatal("Decide accepted an unknown request id")
	}
}

// HTTP 클라이언트가 사람의 결정을 기다리는 동안 연결을 끊으면, 대기 중인
// pending 항목이 브로커 수명 내내 누적되지 않고 정리되어야 한다.
func TestCancelWhileWaitingForDecisionCleansUpPending(t *testing.T) {
	b := New("tok")
	ctx, cancel := context.WithCancel(context.Background())
	call := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`

	var surfaced Request
	requestSeen := make(chan struct{})
	go func() {
		surfaced = <-b.Requests()
		close(requestSeen)
		cancel() // 사람이 결정하기 전에 클라이언트가 끊어진 상황을 흉내낸다.
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		postCtx(t, b.Handler(), "tok", call, ctx)
	}()

	select {
	case <-requestSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("request never surfaced")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancellation")
	}

	b.mu.Lock()
	_, stillPending := b.pending[surfaced.ID]
	b.mu.Unlock()
	if stillPending {
		t.Errorf("pending[%q] was not cleaned up after context cancellation", surfaced.ID)
	}
}

// HTTP 클라이언트가 사람에게 요청이 전달되기도 전에(요청 채널이 가득 찬
// 상태) 연결을 끊는 경우도 같은 방식으로 정리되어야 한다.
func TestCancelWhileQueueFullCleansUpPending(t *testing.T) {
	b := New("tok")
	capacity := cap(b.requests)
	for i := 0; i < capacity; i++ {
		b.requests <- Request{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	call := `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`

	done := make(chan struct{})
	go func() {
		defer close(done)
		postCtx(t, b.Handler(), "tok", call, ctx)
	}()

	// 핸들러가 pending에 항목을 등록할 때까지 기다린다.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.pending)
		b.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancellation while queue was full")
	}

	b.mu.Lock()
	pendingCount := len(b.pending)
	b.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after cancellation while queue was full, want 0", pendingCount)
	}
}
