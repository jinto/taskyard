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

// DefaultExecuteTemplate은 프로젝트를 만들 때 채워지는 범용 실행 템플릿이다.
// 특정 사용자의 스킬에 의존하지 않는다. 프로젝트 설정에서 덮어쓴다.
const DefaultExecuteTemplate = `다음 이슈를 해결하라.

{{issue}}

프로젝트 기억:
{{memory}}

절차:
1. 관련 코드를 읽고 변경 범위를 정한다.
2. 구현하고, 기존 테스트 방식에 맞춰 테스트를 추가·수정한다.
3. 테스트와 린트를 통과시킨다.
4. 의미 단위로 커밋한다. 커밋 메시지에 이슈 번호를 넣는다.
5. ` + "`gh pr create`" + `로 PR을 연다. 본문에 무엇을 왜 바꿨는지 적는다.

멈추고 보고해야 하는 경우 — 계속 시도하지 말고 이유를 마지막 메시지에 남긴 뒤 종료하라:
- 테스트나 CI가 반복 실패하고 원인을 모르겠을 때
- 이슈가 요구하는 것이 코드베이스와 모순되거나 제품 동작을 바꿔야 할 때
- 데이터 손실, 비가역 마이그레이션, 보안, 비용에 영향이 있는 결정이 필요할 때
- 범위가 이슈보다 크게 커질 때
`
