# Intent: Taskyard Phase 0 수직 스파이크

- Date: 2026-08-21
- Status: done — PR #1 merge 60a060c (2026-09-02); "Phase 0 완료 판정 통과" ce08f26
- Origin: retroactive — spec.md/plan.md에서 2026-10-20 재구성
- Next: plan.md

## Problem
PRD(`taskyard-prd-v1.md` v1.1) §11의 아키텍처 전제 — Server↔Runner 분리, 단절에도 유실·중복 없는 전송, Runner 재시작 후 복구, 웹을 통한 승인 — 가 아직 실증되지 않았다. 또한 PRD §11.2.1의 왕복 지연 질문과 §22 Q16(구독 과금 경계가 실제로 지켜지는가)의 Claude Code 쪽 절반이 열린 채로 남아 있다.

## Proposed outcome
하드코딩된 Task 하나로 Server↔Runner 실행·복구·승인 경로를 끝까지 관통시킨다. PRD §16.0 완료 판정으로 확인한다.
- 판정 1: 임의 시점에 연결을 10회 끊었다 붙여도 이벤트 유실·중복이 0이다.
- 판정 2: 실행 중 Runner를 재시작해도 Run이 lost가 아니라 resumable로 판정된다.
- 판정 3: 승인 요청이 웹에 뜨고, 응답이 Agent에 전달되어 실행이 계속된다.
- 판정 4: 왕복 지연을 측정해 기록만 한다(임계값 없음). 견딜 만한지는 사람이 판단한다.
- 실제 Claude Code 실행이 API 키가 아니라 구독으로 과금됨을 한 번 확인한다.

## Affected users and systems
- Taskyard Server와 Runner 두 바이너리, 그 사이의 프로토콜 v0
- Runner가 띄우는 Claude Code CLI, Run마다 쓰는 Git branch/worktree
- 웹에서 Run 이벤트를 보고 승인하는 사용자

## Constraints
- 단일 바이너리 배포(PRD §15.1)를 지키려면 CGO 없이 빌드해야 한다.
- 구독 과금 경계(PRD §13.2, §13.2.1): API 키로 과금되는 실행 모드는 금지하고, Agent 환경에서 API 키 변수를 제거한다. API 키로 과금되면 Run을 중단한다.
- 승인 브로커가 우회되지 않도록 Claude Code의 권한 모드를 기본값으로 고정해야 한다. 이 설정은 타협하지 않는다.
- worktree는 어떤 경우에도 자동 삭제하지 않고, 미커밋 변경은 보존한다(PRD §8.7.1).
- 단위 테스트는 실제 `claude` CLI를 호출하지 않는다(구독 할당량 소모, 비결정성). 실제 CLI 검증은 스모크 하나로 모은다.
- 승인 요청 필드 형식은 공식 문서로 확인되지 않았으므로 실제 요청을 캡처해 고정한다. 캡처가 기준이다.

## Out of scope
- 보드 UI, 티켓 CRUD, Planner, Coordinator, 병렬 스케줄러, Codex 어댑터, PR 생성(§16.0 제외 목록)
- Server 측 orphaned 상태 기계, 타이머, Attention Item
- `resumable` 판정 이후 실제 세션 재개, 동시 실행 Run의 승인 연결, PID 재사용 구분, 저장소 수준 설정 격리(모두 Phase 1)

## Open questions
- 판정 4의 측정값이 견딜 만한가. 이 측정은 브라우저→Server→Runner→CLI 왕복의 일부 구간일 뿐이다.
- `--permission-prompt-tool`이 보내는 요청 필드 이름이 예상과 다르면 승인 브로커 설계가 바뀐다.
