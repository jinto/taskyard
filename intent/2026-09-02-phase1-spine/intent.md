# Intent: Phase 1 척추 — 프로젝트·이슈에서 실행까지

- Date: 2026-09-02
- Status: done — PR #3 merge 214843e (2026-09-03), "이슈에서 실행까지 척추 관통" b616fcc
- Origin: retroactive — spec.md/plan.md에서 2026-10-20 재구성
- Next: plan.md

## Problem
Phase 0에서는 실행이 프롬프트 한 줄을 직접 받아 시작됐다. 프로젝트·이슈라는 작업 단위가 없어 "무엇을 왜 실행하는지"가 시스템에 남지 않았고, Run의 상태도 이벤트로 갱신되지 않았다(findings 3번).

## Proposed outcome
- 사용자가 프로젝트(이름·키·저장소 경로·기본 브랜치)와 이슈(제목, 선택적 본문)를 만든다.
- 이슈의 [실행]이 "프로젝트의 실행 템플릿 + 이슈"로 조립한 프롬프트로 Run을 시작한다. 템플릿은 `{{issue}}`, `{{memory}}`를 지원한다.
- 이슈 상세에서 Run 상태가 보이고, 이슈는 `backlog` → `in_progress` → (Run 성공 시) `review`로 움직인다. 실패·취소는 이슈 상태를 되돌리지 않는다.
- 그 밖의 실행 배관(worktree → claude → 이벤트 → 승인 → diff)은 Phase 0 그대로다.

## Affected users and systems
- 로컬에서 Taskyard를 쓰는 개발자(웹 UI: 프로젝트 목록·프로젝트·이슈 화면).
- 서버(프로젝트·이슈 저장, 프롬프트 조립, 실행 트리거)와 러너(지정 저장소에서 실행, 허용 목록 검증).
- PRD v1.2 PM-01/04/05, ST-01/02/04, RN-03.

## Constraints
- 러너는 허용된 저장소 목록에 있는 경로에서만 실행한다. 목록 밖이면 Run 실패로 끝난다.
- 기본 실행 템플릿은 범용이며 사용자 스킬에 의존하지 않는다.
- 동시 실행은 1개 유지.
- 종결 상태(`succeeded`/`failed`/`cancelled`)는 비종결 상태로 되돌리지 않는다.
- 기존 Phase 0 DB를 그대로 열 수 있어야 한다.
- 웹 UI에는 인증이 없다(수용된 노출). 기본 바인딩 `127.0.0.1`과 모든 상태 변경 POST의 same-origin 가드가 최소 방어선이다.
- Phase 0의 직접 실행 경로와 `--repo` 플래그는 호환 없이 제거한다.

## Out of scope
분석·회고 단계, 설계 승인 관문, Artifact 첨부, 종료 방식 5종 UI(취소·재시도), 칸반 보드, merge 정책, Slack, 프로젝트 기억 편집, 동시 실행 N, 인증.

## Open questions
- `{{memory}}`는 러너가 치환하므로 최종 프롬프트가 어디에도 기록되지 않는다. 러너가 최종 프롬프트를 올릴지 다음 PR에서 정한다.
- 저장소 불허로 실패한 Run의 이슈는 `in_progress`에 머문다. 종료 방식 UI와 함께 다룬다.
