# Phase 0 스파이크 — 이월 사항과 Phase 1 착수 전 과제

> 이 문서는 Phase 0 실행 중 발견됐으나 의도적으로 미룬 것들의 기록이다.
> 12개 Task 리뷰 + 최종 전체 브랜치 리뷰에서 나온 항목을 정리했다.
> 관련 계획: `docs/superpowers/plans/2026-08-21-taskyard-phase0-spike.md`

## §16.0 완료 판정 결과

| 판정 | 결과 |
|---|---|
| 연결 10회 단절에도 유실·중복 0 | **통과** — 정확히 200건, seq 1..200 연속. 라운드당 10건을 단절 구간에 발행해 재전송 경로를 구조적으로 강제 |
| Runner 재시작 → `resumable` 복구 | **통과** — spool close/reopen 실제 왕복. 복구 불가 run은 `lost` 판정 후 salvage 커밋 생성 |
| 승인 왕복 | **통과** (브로커 수준). 조립 경로는 별도 인수 테스트가 커버 |
| 왕복 지연 | **14.09ms** (50샘플, localhost). 임계값 없이 기록만 |

`make smoke`에서 실제 Claude Code CLI 1회 실행, `apiKeySource == "none"` 확인 —
**실행이 API가 아니라 구독으로 과금된다는 실증**(PRD §13.2, §22 Q16의 Claude Code 절반).

## Phase 1 착수 전에 처리해야 할 것 (우선순위 순)

### 1. Head-of-line blocking — 병렬 스케줄러보다 먼저
`OnCommand`가 `link.readLoop` 안에서 동기 실행되고, `handleRunStart`가 그 안에서
`Git.Ensure`(`git worktree add`)를 동기로 돈다. 같은 readLoop이 ack 처리와 spool
정리도 담당한다. 큰 저장소에서 수 초의 정지 → spool 미정리 → `drain`의 전체 창
재전송이 커짐 → 다음 패스가 더 느려짐. **정상 성공 경로에서 악화되는 유일한 항목.**
`CmdRunReconcile`은 같은 이유로 이미 고루틴으로 뺐다. `Ensure`는 남아 있다.

### 2. `ErrRunNotFound` livelock — C1 트리거를 배선하면서 도달 가능해졌다
`store.ApplyEvent`가 `ErrRunNotFound`를 반환하면 `hub.readLoop`이 **ack 없이**
로그만 남기고 넘어간다. `link.drain`은 seq 0부터 전체 창을 다시 읽는다. 아무도 트림하지
않아 spool이 무한히 자라고 같은 이벤트를 영원히 재전송한다. poison-message 경로도,
dead-letter도, 유계 재시도도 없다.
- 진입 경로: `lifecycle.runIDForApproval()`이 활성 run이 없을 때 문자열 `"unknown"`을 반환
- 일상적 진입: 러너 spool이 살아 있는 채로 `taskyard-server.db`를 지우면 모든 run이 unknown
- 수정: `errors.Is(err, store.ErrRunNotFound)`를 일시적 오류와 구분해 ack를 보내 트림 가능하게

### 3. `store.Run.State`가 이벤트로 갱신되지 않는다
production 코드에 `UpsertRun` 호출은 `handleRunCreate` 두 곳뿐이다. `run.state_changed`를
저장소에 반영하는 코드가 없어 목록 화면의 상태는 생성 후 영원히 `queued`다. 상세 페이지는
이벤트 스트림으로 실제 상태를 보여주므로 상세는 정상. **목록 뷰만 오해를 부른다.**

### 4. 승인 침묵 클러스터 — 셋을 한 작업으로 다뤄야 한다
- 승인 이벤트 `Publish` 실패 시 브로커 요청이 pending에 남아 에이전트가 영구 대기
- `handleApprove`가 모든 `SendCommand` 오류를 "runner is not connected"로 뭉갠다
- 취소와 `Decide`가 겹치는 좁은 창에서 결정이 조용히 버려진다

각각은 작은 정직성 문제지만, 합쳐지면 에이전트가 무기한 멈춘 채 UI는 그럴듯하지만 틀린
원인을 보여주고 운영자는 셋 중 무엇인지 알 방법이 없다.

## 굳어지기 전에 정의해야 할 추상

1. **`hub.conn`이 단일 연결이다.** 다중 Runner에 필요한 라우팅, Runner별 resume 지점,
   밀려난 연결의 펜싱이 전부 없다. `Welcome.ResumeFrom`은 Runner 범위가 없는 전역 맵이다.
   **병렬 스케줄러 이전에** 손봐야 한다.
2. **`runIDForApproval()`의 "활성 run은 정확히 하나" 가정.** 병렬 스케줄러가 이걸 죽인다.
   `Request.ToolUseID`가 이미 끝까지 보존되므로 run별 브로커 엔드포인트의 갈고리가 된다.
3. **`store.ApplyEvent`의 "run이 먼저 존재해야 한다" 계약**이 암묵적이고, 반환되는 오류를
   아무도 처리하지 않는다. 명시화(첫 이벤트에 자동 생성)하거나 거부를 처리해야 한다.
4. **`spool.RememberCommand`의 `result`가 항상 `{"accepted":true}`**다. 멱등성 원장의
   결과 컬럼이 정보를 담지 않아, 재전송된 명령이 "봤다"만 답하고 무엇이 일어났는지는 못 답한다.

## 보안 — 위협 모델 정정

**브로커 토큰이 `argv`에 노출된다**(`--mcp-config` JSON 안의 Bearer 토큰). Phase 0에서는
보류했으나 **최초 근거가 틀렸다.**

처음 쓴 근거는 "argv를 읽을 수 있는 로컬 프로세스는 이미 자격증명 파일을 읽을 수 있다"였다.
이는 **브로커가 제약하려는 당사자만 빼고** 참이다. 에이전트는 방관자가 아니라 피감시
주체이고, 정의상 자기 `argv`를 갖는다. Linux에서 `/proc/self/cmdline`은 평범한 파일이라
Read 도구로 읽히고 `--permission-mode default`는 Read를 묻지 않는다.

실제 성질: **승인된 도구 호출 한 번이면 토큰이 유출되고, 이후 자가 승인이 무제한·무음이다.**
권한 상승 래칫이다. 오늘 막아주는 건 브로커에 HTTP로 닿으려면 Bash나 WebFetch가 필요하고
둘 다 승인을 묻는다는 사실뿐이다.

올바른 완화는 "단일 사용자 머신이니 괜찮다"가 아니라 **"토큰이 샌드박스 안에서 복구
불가능해야 한다"** — run별 토큰이나 `tool_use_id`에 묶인 토큰을 가리킨다.
PRD 미해결 질문에 **에이전트를 공격자로 명시**해 올릴 것.

**웹 UI에 인증이 전혀 없다.** 페어링 토큰은 `/ws`만 지킨다. `POST /runs/{id}/approve`에는
인증도 CSRF 토큰도 origin 검사도 없고, `request_id`가 해당 run의 것인지도 확인하지 않는다.
기본값 `--addr 127.0.0.1:8080`이 막아줄 뿐이며 `--addr 0.0.0.0:8080` 한 번이면 사람의
승인 표면이 네트워크에 열린다. PRD §14.1이 "웹 터미널을 통한 권한 오용"을 명시한 위협이다.
최소 조치: 루프백이 아닌 주소 바인딩을 명시 플래그 없이는 거부(약 5줄).

## 기타 이월 (위험 낮음)

- **과금 조기 중단이 주석의 주장보다 한 턴 늦게 발동한다.** `parse.dispatch`가
  `system`/`init`에서 emit하지 않으므로 첫 emit은 첫 assistant 블록이다. 즉 1턴은 이미
  청구된 뒤다. 종결 백스톱은 유효하므로 실패 판정 자체는 정확하다.
  Task 7에 추가한 `Parser`의 `sync.RWMutex`는 "Task 9의 감독 고루틴"을 위한 것이었으나
  **그 고루틴은 만들어지지 않았다.** 이를 만들거나 `OnSession` 훅을 두면 진짜 조기 중단이 된다.
- **salvage가 `taskyard/salvage/<run_id>` ref가 아니라 run 브랜치에 커밋한다**(PRD §8.7.1
  위반). `Ensure`가 기존 브랜치에 재부착하므로 재시도된 run이 salvage 커밋을 평범한
  히스토리로 물려받아 에이전트 작업과 구분되지 않는다. `git update-ref`로 2줄.
- **`handleRunCancel`의 원장 기록이 `execute`의 종결 기록과 경합**해 덮어쓸 수 있다.
  오늘은 무해하나 `SessionID`를 지우므로 `--resume` 배선 시 실재하는 문제가 된다.
- **`--resume`이 배선되지 않았다.** §16.0 4번 항목이 명시하고 `BuildArgs`가 이미
  `ResumeSessionID`를 받는다. 배선하면 *분류*가 실제 *복구*가 된다.
- `link`에 자기 테스트 파일이 없다(다른 패키지 테스트가 간접 커버해 실제 커버리지는 높다).
  회귀 시 엉뚱한 곳에서 실패로 나타난다. `link_test.go`를 만들 것.
- `SpawnOptions.WorkDir`는 죽은 필드다. `BuildArgs`가 무시하고 `lifecycle`이 별도로
  `cmd.Dir`를 세팅해 우연히 동작한다. 삭제하거나 소비할 것.
- heartbeat이 `Kind: KindEvent`로 이동하고 수신측에서 타입으로 걸러진다. 동작하지만
  "kind"가 이름값을 못 하고, 이미 실제 버그를 하나 만들었다. 고유 kind를 줄 것.
- `Hub`에 shutdown/drain API가 없다. 실서버 종료 시 teardown ERROR 노이즈가 난다.
- 에이전트 자식 프로세스가 Runner 종료 시 의도적으로 고아가 된다(`execute`의 ctx가
  `context.Background()` 파생). reconcile 설계와 **일관되며 옳은 선택**이지만 어디에도
  기록돼 있지 않다. `main.go`에 주석 한 줄.

## UI 언어 규칙 (Phase 1에서 재검토)

Phase 0 규칙: **UI 산문은 한국어, 프로토콜 식별자는 원문 유지.** 상태값(`running`,
`waiting_approval`)을 번역하지 않은 이유는 **관객이 이 시스템의 개발자이기 때문**이다.
디버깅 도구에서 번역은 실제 값을 가린다.

Phase 1에서 디버깅하지 않는 사람 앞에 놓이는 순간 논리가 뒤집힌다. 그때의 올바른 형태는
한국어를 **표시**하고 와이어 값을 `title`/`data-state` 속성에 **보존**하는 것 — 택일이 아니라 둘 다.

## 프로세스에서 배운 것

- **실패하는 테스트를 먼저 독립 커밋으로 만들면** RED가 산문이 아니라 git 히스토리에 남아
  검증 가능해진다. Task 4부터 적용했고 이후 모든 Task에서 리뷰어가 RED 커밋의 순수성을
  독립 확인할 수 있었다.
- **순서나 fixture를 지시할 때는 그것이 지키려는 불변식을 함께 말해야 한다.** Task 9에서
  구현자가 두 번 지시를 따르지 않았고 두 번 다 옳았다 — 실제 코드 경로를 추적했기 때문이다.
  한 번은 지시대로 하면 원장이 매번 오염됐고, 한 번은 제안한 fixture 2개가 수정 전에도
  통과하는(이빨 없는) 테스트였다.
- **Task 단위 리뷰는 조립된 전체를 볼 수 없다.** 12개 Task가 각자의 브리프에 대해 전부
  통과했는데 완성품은 run을 시작할 수 없었다 — 12개 브리프가 집합적으로 트리거를
  빠뜨렸기 때문이다. 전체 브랜치 리뷰가 유일하게 잡을 수 있는 결함 종류다.
