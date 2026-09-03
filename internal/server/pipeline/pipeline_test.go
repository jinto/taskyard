package pipeline

import (
	"strings"
	"testing"
)

func TestRenderReplacesKnownTokens(t *testing.T) {
	got := Render("A {{issue}} B {{memory}} C", map[string]string{"issue": "X", "memory": "Y"})
	if got != "A X B Y C" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderReplacesRepeatedToken(t *testing.T) {
	got := Render("{{issue}} and again {{issue}}", map[string]string{"issue": "X"})
	if got != "X and again X" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderLeavesUnknownTokens(t *testing.T) {
	// {{memory}}는 서버가 모르고 러너가 채운다. 오타 난 토큰도 프롬프트에
	// 보이는 편이 조용히 사라지는 것보다 낫다.
	got := Render("{{issue}} / {{memory}} / {{typo}}", map[string]string{"issue": "X"})
	if got != "X / {{memory}} / {{typo}}" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderLeavesMalformedTokensUntouched(t *testing.T) {
	vars := map[string]string{"issue": "X"}
	for _, in := range []string{"{{issue", "issue}}", "{{ issue }}", "{{is sue}}", "{{}}", "{{issue}"} {
		if got := Render(in, vars); got != in {
			t.Errorf("Render(%q) = %q, want unchanged", in, got)
		}
	}
	// 중첩: 안쪽 {{issue}}만 토큰이고 바깥 중괄호는 그대로 남는다.
	if got := Render("{{{issue}}}", vars); got != "{X}" {
		t.Errorf("Render({{{issue}}}) = %q, want {X}", got)
	}
}

func TestRenderDoesNotRescanValues(t *testing.T) {
	// 이슈 본문에 토큰처럼 생긴 글자가 있어도 치환된 값 안은 다시 훑지 않는다.
	got := Render("{{issue}}", map[string]string{"issue": "see {{memory}} and {{issue}}", "memory": "M"})
	if got != "see {{memory}} and {{issue}}" {
		t.Fatalf("Render = %q (value was rescanned)", got)
	}
}

func TestIssueTextWithAndWithoutBody(t *testing.T) {
	if got := IssueText(12, "제목", "본문 첫 줄\n둘째 줄"); got != "#12 제목\n\n본문 첫 줄\n둘째 줄" {
		t.Fatalf("IssueText with body = %q", got)
	}
	if got := IssueText(3, "제목만", ""); got != "#3 제목만" {
		t.Fatalf("IssueText without body = %q", got)
	}
}

func TestPreviousRunTextFormats(t *testing.T) {
	if got := PreviousRunText("", "", ""); got != "" {
		t.Fatalf("first run should render empty, got %q", got)
	}
	if got := PreviousRunText("run-1", "failed", ""); got != "이전 실행 run-1: failed" {
		t.Fatalf("without detail = %q", got)
	}
	if got := PreviousRunText("run-1", "needs_attention", "CI가 반복 실패"); got != "이전 실행 run-1: needs_attention\nCI가 반복 실패" {
		t.Fatalf("with detail = %q", got)
	}
}

func TestDefaultTemplateMentionsAttentionFileAndRetryTokens(t *testing.T) {
	for _, want := range []string{".taskyard/attention.md", "{{previous_run}}", "{{feedback}}"} {
		if !strings.Contains(DefaultExecuteTemplate, want) {
			t.Errorf("DefaultExecuteTemplate lacks %q", want)
		}
	}
}

func TestDefaultTemplateMentionsIssueAndMemory(t *testing.T) {
	for _, token := range []string{"{{issue}}", "{{memory}}"} {
		if !strings.Contains(DefaultExecuteTemplate, token) {
			t.Errorf("DefaultExecuteTemplate lacks %s", token)
		}
	}
	// 범용 템플릿이다. 특정 사용자의 스킬 이름에 의존하지 않는다.
	for _, forbidden := range []string{"explain-diff-html", "ponytail", "codex", "works"} {
		if strings.Contains(DefaultExecuteTemplate, forbidden) {
			t.Errorf("DefaultExecuteTemplate depends on user-specific skill %q", forbidden)
		}
	}
	// 멈춤 조건이 들어 있다(PRD §7.5).
	if !strings.Contains(DefaultExecuteTemplate, "멈추") {
		t.Error("DefaultExecuteTemplate has no stop-and-report instruction")
	}
}
