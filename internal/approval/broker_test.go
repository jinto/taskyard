package approval

import (
	"bytes"
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
