// Package pipeline은 이슈와 프로젝트 템플릿으로 에이전트 프롬프트를 조립한다.
//
// PRD §7.2와 §8.3(ST-04)을 따른다. 템플릿은 로직이 없는 문자열 치환이다 —
// 로직이 필요해지는 순간이 오면 그때 text/template로 바꾼다.
package pipeline

import (
	"fmt"
	"regexp"
)

// tokenPattern이 토큰의 전부다. 공백, 중첩, 짝 안 맞는 중괄호는 토큰이
// 아니며 바이트 그대로 남는다.
var tokenPattern = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

// Render는 template의 {{name}} 토큰을 vars로 치환한다. vars에 없는 토큰은
// 그대로 남긴다 — 러너가 치환할 {{memory}}가 서버를 통과해야 하고, 오타 난
// 토큰은 프롬프트에 보이는 편이 조용히 사라지는 것보다 낫다.
//
// 입력을 한 번만 훑는다. 치환된 값 안은 다시 훑지 않으므로 이슈 본문에
// {{issue}}가 들어 있어도 그대로다. 정규식 치환이 이 성질을 그대로 준다;
// 토큰마다 strings.ReplaceAll을 반복하면 앞 치환이 넣은 값 안의 토큰을
// 다음 반복이 치환해 버린다.
func Render(template string, vars map[string]string) string {
	return tokenPattern.ReplaceAllStringFunc(template, func(tok string) string {
		name := tok[2 : len(tok)-2]
		if v, ok := vars[name]; ok {
			return v
		}
		return tok
	})
}

// IssueText는 {{issue}}에 들어갈 본문이다. 번호와 제목 한 줄, 빈 줄, 본문.
// 본문이 없으면 제목 줄만.
func IssueText(number int, title, body string) string {
	head := fmt.Sprintf("#%d %s", number, title)
	if body == "" {
		return head
	}
	return head + "\n\n" + body
}

// PreviousRunText는 {{previous_run}}에 들어갈 본문이다(PRD §7.6). 첫 실행이면
// 빈 문자열 — 템플릿의 그 자리가 비어 에이전트가 무시한다.
func PreviousRunText(id, state, detail string) string {
	if id == "" {
		return ""
	}
	head := fmt.Sprintf("이전 실행 %s: %s", id, state)
	if detail == "" {
		return head
	}
	return head + "\n" + detail
}

// DefaultAllowedTools는 새 프로젝트가 시작하는 허용 도구다. 빈 목록으로
// 시작하면 모든 도구 호출이 사람을 기다려 실행이 사실상 멈춘다(2026-09-04 관측).
// 되돌릴 수 없는 것(push, reset, rm, 네트워크)은 넣지 않는다 — 사람이 직접
// 골라 넣는다.
var DefaultAllowedTools = []string{
	"Read", "Edit", "Write", "Glob", "Grep",
	"Bash(ls:*)", "Bash(cat:*)", "Bash(git status:*)", "Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)",
}

// DefaultAnalyzeTemplate은 1단계(분석·설계)의 기본 템플릿이다(PRD §7.2). 코드를
// 바꾸지 않고 보고서만 남긴다. 프로젝트의 analyze_template이 비어 있으면 이것.
const DefaultAnalyzeTemplate = `다음 이슈를 분석하고 설계하라. 코드를 바꾸지 말고 커밋도 하지 마라 — 이 단계의 산출물은 보고서 하나다.

{{issue}}

프로젝트 기억:
{{memory}}

절차:
1. 관련 코드를 읽고 이슈가 실제로 무엇을 요구하는지 정리한다.
2. 보고서를 ` + "`.taskyard/artifacts/analysis.md`" + `에 쓴다. 담을 것:
   - 문제: 무엇이 왜 문제인지, 한 문단
   - 관련 코드: 파일과 함수, 바꿔야 할 자리
   - 설계: 어떻게 바꿀지, 대안이 있으면 왜 이쪽인지
   - 검증: 어떤 테스트로 확인할지
   - 위험: 데이터·호환성·성능에 미치는 영향
   - 크기: 한 번의 실행으로 끝낼 수 있는지. 아니면 어떻게 쪼갤지
3. 보고서를 쓰면 정상 종료한다.

명령은 하나씩 실행한다. 세미콜론이나 && 로 여러 명령을 묶으면 승인 규칙에 걸려 사람을 기다리게 된다.

멈추고 보고해야 하는 경우 — 이유를 ` + "`.taskyard/attention.md`" + ` 파일에 쓴 뒤 정상 종료하라:
- 이슈가 요구하는 것이 코드베이스와 모순되거나 제품 동작을 바꿔야 할 때
- 이슈만으로는 무엇을 원하는지 알 수 없을 때
`

// DefaultExecuteTemplate은 프로젝트를 만들 때 채워지는 범용 실행 템플릿이다.
// 특정 사용자의 스킬에 의존하지 않는다. 프로젝트 설정에서 덮어쓴다.
const DefaultExecuteTemplate = `다음 이슈를 해결하라.

{{issue}}

1단계 보고서(있으면):
{{stage1_report}}

프로젝트 기억:
{{memory}}

절차:
1. 관련 코드를 읽고 변경 범위를 정한다. 1단계 보고서가 있으면 그 설계를 따른다.
2. 구현하고, 기존 테스트 방식에 맞춰 테스트를 추가·수정한다.
3. 테스트와 린트를 통과시킨다.
4. 의미 단위로 커밋한다. 커밋 메시지에 이슈 번호를 넣는다.
5. 변경 설명을 ` + "`.taskyard/summary.md`" + `에 쓴다 — 무엇을 왜 바꿨는지, 리뷰어가 먼저 볼 곳. 이 파일은 커밋하지 않는다(PR 본문이 된다).

명령은 하나씩 실행한다. 세미콜론이나 && 로 여러 명령을 묶으면 승인 규칙에 걸려 사람을 기다리게 된다.

멈추고 보고해야 하는 경우 — 계속 시도하지 말고, 이유를 ` + "`.taskyard/attention.md`" + ` 파일에 쓴 뒤 정상 종료하라. 사람이 읽고 다음 행동을 정한다:
- 테스트나 CI가 반복 실패하고 원인을 모르겠을 때
- 이슈가 요구하는 것이 코드베이스와 모순되거나 제품 동작을 바꿔야 할 때
- 데이터 손실, 비가역 마이그레이션, 보안, 비용에 영향이 있는 결정이 필요할 때
- 범위가 이슈보다 크게 커질 때

재시도라면 아래에 이전 실행의 결과와 사람의 메모가 있다. 비어 있으면 첫 실행이니 무시하라.
{{previous_run}}
{{feedback}}
`
