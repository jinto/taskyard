# Intent: Phase 1 — PR 생성·추적과 merge 후 정리

- Date: 2026-09-03
- Status: retroactive — spec.md/plan.md에서 2026-10-20 재구성
- Next: plan.md

## Problem
지금까지는 에이전트가 템플릿 지시에 따라 직접 PR을 열었고, Taskyard는 PR의 상태(체크·리뷰·merge)를 알지 못했다. 그래서 merge돼도 이슈가 끝나지 않았고 worktree는 쌓이기만 했다. 원격이 없는 저장소에서는 PR 단계가 막혔다(관측 6번).

## Proposed outcome
- 실행 Run이 성공하고 변경 커밋이 있으면 러너가 사용자의 `git`·`gh` 로그인으로 브랜치를 push하고 PR을 만든다(이미 있으면 그 PR을 쓴다). PR 제목은 이슈 제목, 본문은 에이전트가 쓴 변경 설명이다.
- 러너가 PR 상태를 주기적으로 추적해 서버에 알린다. 이슈·Run 화면에 PR 링크와 상태·체크·리뷰, 변경 설명이 보인다.
- PR이 merge되면 이슈는 `done`이 되고, 정책이 켜져 있으면 worktree가 정리된다.
- 프로젝트 설정으로 PR 생성(`create_pr`)과 merge 후 정리(`cleanup_merged`)를 켜고 끈다. 원격 없는 저장소는 PR 생성을 끄면 된다.
- merge 자체는 사람이 GitHub에서 한다.

## Affected users and systems
- 이슈를 리뷰하고 merge하는 사람, 원격 없는 저장소를 쓰는 사용자.
- 러너(push·PR 생성·추적·정리), 서버(PR 상태 저장, 이슈 `done` 전이), 웹(설정·PR 표시), GitHub.
- PRD §7.6, §8.7 GH-05/06/10, §8.7.1, §16.1-7.

## Constraints
- merge되지 않은 worktree는 어떤 경우에도 자동 삭제하지 않는다. 지우기 직전에 미커밋 변경, push 안 된 커밋, 활성 Run이 없는지 다시 확인한다. 브랜치는 남긴다.
- push·PR 생성 실패는 새 상태가 아니라 `needs_attention`(이유 포함)으로 끝난다.
- `git`·`gh`는 사용자의 실제 환경으로 돌되, 프롬프트를 띄우지 않고 타임아웃이 있어 러너를 멈추지 않는다.
- 이슈를 `done`으로 옮기는 것은 이슈의 최신 Run의 merge뿐이고, `done`은 [실행]·재시도로만 벗어난다.
- 정책 값은 Run 시작 시점에 고정된다.
- 어느 단계에서 러너가 죽어도 재시작·이어서 재시도로 회복된다(PR 생성은 멱등).

## Out of scope
자동 merge와 Taskyard의 merge 버튼, 취소·처음부터 재시도 때 옛 PR 닫기, `{{pr_url}}`·`{{diff}}` 템플릿 변수, 리뷰 코멘트를 재시도 프롬프트에 넣기, 브랜치 삭제, 체크·리뷰 상태의 세부 모델링.

## Open questions
- 처음부터 재시도로 생긴 옛 PR은 닫지 않고 둘 다 추적한다. 사람이 GitHub에서 닫는다(명시적 보류).
