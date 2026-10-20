# Intent: Phase 1 — 종료 방식 5종과 재시도

- Date: 2026-09-03
- Status: retroactive — spec.md/plan.md에서 2026-10-20 재구성
- Next: plan.md

## Problem
척추(PR #3) 이후에도 실행이 성공 외의 방식으로 끝나면 사람이 할 수 있는 다음 행동이 없었다. 에이전트가 "멈추고 보고"할 길이 없고, 취소·재시도가 없으며, `--resume`이 쓰이지 않았다. 취소한 Run이 `failed`로 끝나는 Phase 0 버그와, 원장에 없는 Run의 이벤트가 영원히 재전송되는 livelock(findings 2번)도 남아 있었다.

## Proposed outcome
- PRD §7.6의 다섯 종료 중 merge를 뺀 넷 — 멈추고 보고, 취소, 이어서 재시도, 처음부터 재시도 — 이 동작한다.
- 멈추고 보고: 에이전트가 이유를 남기고 끝나면 Run은 `needs_attention`이 되고 이유가 이슈 화면에 보인다.
- 취소: Run은 `cancelled`로 끝나고(다시 `failed`로 바뀌지 않는다) 이슈는 `backlog`로 간다.
- 이어서 재시도: 같은 worktree·브랜치에서 이전 세션을 재개한다. 처음부터 재시도: 새 worktree·새 세션, 이전 worktree는 보존.
- 재시도 프롬프트에 이전 실행 결과(`{{previous_run}}`)와 사람의 한 줄 피드백(`{{feedback}}`)이 들어간다.
- 원장에 없는 Run의 이벤트는 받아서 버려 livelock이 사라진다.

## Affected users and systems
- 이슈 화면에서 실행 결과를 처리하는 사람(취소·재시도 UI).
- 서버(Run 상태·이슈 상태 전이), 러너(종결 판정, 세션 재개, worktree 재사용), 서버-러너 프로토콜.
- PRD §7.5, §7.6, §8.3 ST-04/05, §8.5 EX-05, §9.3, §12.

## Constraints
- `needs_attention`은 정착 상태다. 비정착으로 되돌리지 않고 사람의 취소·재시도로만 벗어난다.
- 재시도는 이전 Run이 정착 상태이고 이슈에 활성 Run이 없을 때만 가능하다.
- 세션이 없으면 이어서 재시도는 거부한다. 처음부터로 조용히 강등하지 않는다.
- 재시도는 이전 Run이 무엇이었든(`review` 포함) 이슈를 `in_progress`로 둔다.
- 기존 순서 불변식과 종결 상태 단조 규칙을 유지한다.

## Out of scope
merge 정책·PR 추적(GH-06), 취소 시 PR 처리 묻기와 처음부터 재시도 시 옛 PR 닫기(UI에도 두지 않음), Artifact, 분석·회고 단계, 동시 실행 N, Slack, worktree 정리 정책.

## Open questions
- 이전 세션을 실제로 재개할 수 있는지는 `claude --resume`에 달렸다. 실패하면 실패 이벤트로 끝난다.
