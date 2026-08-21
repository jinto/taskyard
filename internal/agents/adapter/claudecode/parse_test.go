package claudecode

import (
	"os"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/protocol"
)

func parseFixture(t *testing.T, path string) (*Parser, []adapter.Event) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(f, func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p, got
}

func typesOf(events []adapter.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestParseCapturesSessionFromInit(t *testing.T) {
	p, _ := parseFixture(t, "testdata/session-pong.ndjson")

	s := p.Session()
	if s.SessionID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", s.Model)
	}
	if s.Version == "" {
		t.Error("Version is empty; the init event carries claude_code_version")
	}
}

func TestFixtureSessionIsOnTheSubscriptionPath(t *testing.T) {
	p, _ := parseFixture(t, "testdata/session-pong.ndjson")

	s := p.Session()
	if s.APIKeySource != "none" {
		t.Fatalf("APIKeySource = %q, want none", s.APIKeySource)
	}
	// 이것이 PRD §13.2의 자동 검증이다. API 키로 과금되면 Run을 중단한다.
	if s.UsesAPIKey() {
		t.Fatal("UsesAPIKey() = true for a subscription session")
	}
}

func TestParseEmitsNormalizedEventsInOrder(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	want := []string{
		protocol.EvMessageDelta,
		protocol.EvUsageUpdated,
		protocol.EvTurnCompleted,
	}
	gotTypes := typesOf(got)
	if len(gotTypes) != len(want) {
		t.Fatalf("event types = %v, want %v", gotTypes, want)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("event types = %v, want %v", gotTypes, want)
		}
	}
}

func TestMessageDeltaCarriesAssistantText(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	text, ok := got[0].Body["text"].(string)
	if !ok {
		t.Fatalf("message_delta body has no text: %#v", got[0].Body)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("text = %q, want it to contain pong", text)
	}
}

func TestRateLimitBecomesUsageUpdated(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	usage := got[1]
	if usage.Type != protocol.EvUsageUpdated {
		t.Fatalf("Type = %q", usage.Type)
	}
	if usage.Body["status"] != "allowed" {
		t.Errorf("status = %v, want allowed", usage.Body["status"])
	}
	if usage.Body["rate_limit_type"] != "five_hour" {
		t.Errorf("rate_limit_type = %v, want five_hour", usage.Body["rate_limit_type"])
	}
	// quota_exhausted는 스케줄러가 paused_quota로 옮길지 판단하는 신호다(EX-06).
	if usage.Body["quota_exhausted"] != false {
		t.Errorf("quota_exhausted = %v, want false", usage.Body["quota_exhausted"])
	}
}

func TestTurnCompletedCarriesResultAndCost(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	done := got[2]
	if done.Body["result"] != "pong" {
		t.Errorf("result = %v, want pong", done.Body["result"])
	}
	if done.Body["is_error"] != false {
		t.Errorf("is_error = %v, want false", done.Body["is_error"])
	}
	if _, ok := done.Body["total_cost_usd"]; !ok {
		t.Error("turn_completed has no total_cost_usd")
	}
}

func TestRawIsPreserved(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	for i, e := range got {
		if len(e.Raw) == 0 {
			t.Fatalf("event %d (%s) dropped its raw payload", i, e.Type)
		}
	}
}

// 합성 데이터: tool_use/tool_result 블록은 session-pong 캡처에 없다.
// make smoke가 실제 캡처를 만들면 이 상수를 교체한다.
const syntheticToolNDJSON = `{"type":"assistant","session_id":"s1","parent_tool_use_id":null,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","session_id":"s1","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"README.md"}]}}
`

func TestToolUseBecomesToolStartedAndFinished(t *testing.T) {
	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(syntheticToolNDJSON), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(got), typesOf(got))
	}
	if got[0].Type != protocol.EvToolStarted {
		t.Errorf("got[0].Type = %q, want %q", got[0].Type, protocol.EvToolStarted)
	}
	if got[0].Body["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", got[0].Body["tool_name"])
	}
	if got[0].Body["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_use_id = %v, want toolu_1", got[0].Body["tool_use_id"])
	}
	if got[1].Type != protocol.EvToolFinished {
		t.Errorf("got[1].Type = %q, want %q", got[1].Type, protocol.EvToolFinished)
	}
	if got[1].Body["tool_use_id"] != "toolu_1" {
		t.Errorf("finished tool_use_id = %v, want toolu_1", got[1].Body["tool_use_id"])
	}
}

func TestMalformedLineBecomesErrorEventAndParsingContinues(t *testing.T) {
	input := "not json at all\n" + `{"type":"result","subtype":"success","result":"ok","is_error":false,"num_turns":1,"session_id":"s1"}` + "\n"

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(input), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse returned an error; a bad line must not abort the stream: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("types = %v, want [error turn_completed]", typesOf(got))
	}
	if got[0].Type != protocol.EvError {
		t.Errorf("got[0].Type = %q, want %q", got[0].Type, protocol.EvError)
	}
	if got[1].Type != protocol.EvTurnCompleted {
		t.Errorf("got[1].Type = %q, want %q", got[1].Type, protocol.EvTurnCompleted)
	}
}

func TestLongLinesAreHandled(t *testing.T) {
	// bufio.Scanner의 기본 64KB 한도를 넘는 줄이 실제로 나온다.
	// 파서는 이를 잘라먹거나 실패하면 안 된다.
	big := strings.Repeat("x", 200_000)
	input := `{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"text","text":"` + big + `"}]}}` + "\n"

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(input), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(got) != 1 || got[0].Type != protocol.EvMessageDelta {
		t.Fatalf("types = %v, want [message_delta]", typesOf(got))
	}
	if len(got[0].Body["text"].(string)) != len(big) {
		t.Fatal("long text was truncated")
	}
}
