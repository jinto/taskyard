# Phase 1 — 종료 방식 5종과 재시도

> **For agentic workers:** Task 단위로 실행한다. 각 Task는 실패하는 테스트 커밋(RED) → 구현 커밋(GREEN). Task 사이에 리뷰. 커밋·PR에 attribution 트레일러를 넣지 않는다.

**Goal:** 척추(PR #3) 위에서 실행이 어떻게 끝나든 사람이 다음 행동을 할 수 있게 한다. PRD §7.6의 다섯 종료 — merge / 멈추고 보고 / 취소 / 이어서 재시도 / 처음부터 재시도 — 중 merge를 뺀 넷을 배선한다. `--resume`이 실제로 쓰이고, findings 2번(`ErrRunNotFound` livelock)을 함께 닫는다.

**Spec:** PRD v1.2 §7.5(멈추고 보고), §7.6(종료 방식·재시도·세션), §8.3 ST-04/05, §8.5 EX-05, §9.3(`needs_attention`), §12(Run: `previous_run_id`, `feedback`, `session_mode`).

**Non-goals:** merge 정책·PR 추적(GH-06), Artifact, 분석·회고 단계, 동시 실행 N, Slack, worktree 정리 정책.

---

## Global Constraints

PR #3 계획의 Global Constraints가 그대로 적용된다(Go 1.26, CGO 없음, fixture 기반 테스트, 한국어 주석·UI, `BuildArgs`가 플래그 정본, RED→GREEN 두 커밋, **attribution 트레일러 없음**). 추가:

- **순서 불변식 유지.** `UpsertRun` → 이슈 상태 → `SendCommand`(web), `Ensure` → 관문 → `SaveRun` → `emitState` → `execute`(runner), salvage → `SaveRun`(종결).
- **종결 상태 단조 규칙 확장.** `needs_attention`은 "정착(settled)" 상태다 — 종결처럼 비종결으로 되돌리지 않고, 사람이 취소·재시도로만 벗어난다.
- 새 컬럼은 전부 `sqlitex.AddMissingColumns`로 붙인다.

---

## 설계 결정

| 결정 | 내용 |
|---|---|
| 멈추고 보고의 신호 | 에이전트가 worktree의 **`.taskyard/attention.md`** 에 이유를 쓰고 정상 종료한다. 러너는 종료 후 그 파일이 있으면 `needs_attention`(detail = 내용, 4KiB 상한)으로 끝내고 **파일을 지운다** — 이어서 재시도가 같은 worktree를 쓰므로 남기면 다시 걸린다. 기본 템플릿이 이 절차를 지시한다 |
| 왜 파일인가 | 최종 메시지 파싱보다 명시적이고, 에이전트가 이미 파일을 쓸 권한을 갖고 있으며, fixture로 결정적으로 테스트된다 |
| 취소 뒤 이슈 상태 | `backlog` (PRD §7.6) |
| `needs_attention` 뒤 이슈 상태 | `in_progress` 유지, Run 상태로 표시 |
| 이어서 재시도 | 새 Run, **같은 worktree·브랜치**(`workspace_run_id` = 이전 Run의 workspace), `--resume <이전 session_id>` |
| 처음부터 재시도 | 새 Run, 새 worktree·브랜치, 새 세션. 이전 worktree는 보존 |
| 재시도 가능 조건 | 이전 Run이 정착 상태(succeeded/failed/cancelled/needs_attention). 활성 Run이 있으면 409 |
| `{{previous_run}}` | `"이전 실행 <id> — <state>: <detail>"`. detail은 실패 이유 또는 attention 내용. 첫 실행이면 빈 문자열 |
| `{{feedback}}` | 재시도 폼의 한 줄. 없으면 빈 문자열 |
| session_id를 서버가 아는 법 | 러너가 **종결 `run.state_changed`의 body에 `session_id`** 를 싣고, `store.ApplyEvent`가 `runs.provider_session_id`에 저장한다. 별도 이벤트 타입을 만들지 않는다 |
| 취소 라우트 | `POST /runs/{id}/cancel` → `run.cancel`. 러너의 `handleRunCancel`은 이미 있다 |
| livelock | `hub.readLoop`에서 `store.ErrRunNotFound`면 **그 seq를 ack**하고 경고 로그. 러너가 spool을 비울 수 있게. 원장에 없는 Run의 이벤트는 버린다 — 서버 DB를 지운 경우가 이 경로다 |

---

## File Structure

| 경로 | 변경 |
|---|---|
| `internal/protocol/envelope.go` | `RunStartBody`에 `WorkspaceRunID`, `ResumeSessionID` |
| `internal/server/store/store.go` | `StateNeedsAttention`, settled 규칙, `runs`에 `detail`·`previous_run_id`·`workspace_run_id`·`feedback`, 이벤트에서 `session_id`·`detail` 반영, cancelled → backlog |
| `internal/server/pipeline/pipeline.go` | `PreviousRunText`, 기본 템플릿에 attention 절차·재시도 절 |
| `internal/server/web/web.go`, `templates/issue.html`, `run.html` | 취소·재시도 라우트, 최신 Run 기준 행동 UI |
| `internal/server/hub/hub.go` | `ErrRunNotFound` ack |
| `internal/runner/lifecycle/lifecycle.go` | workspace id, `--resume`, attention 파일 |
| `internal/runner/lifecycle/reconcile.go` | salvage를 workspace id로 |
| `internal/runner/spool/spool.go` | `RunRecord.WorkspaceRunID` |
| `acceptance/acceptance_test.go` | 취소·attention·이어서 재시도 관통 |
| 각 `_test.go` | |

---

## Task 1: store — 정착 상태, 취소 전이, 이벤트에서 세션·detail

**Produces:**

```go
const StateNeedsAttention = "needs_attention"
func IsSettled(state string) bool   // terminal || needs_attention. IsTerminal은 그대로 둔다
type Run struct { …; Detail, PreviousRunID, WorkspaceRunID, Feedback string }
```

- `settledStates` = terminal + needs_attention. 단조 규칙은 settled → 비settled 금지. settled → settled 허용.
- `applyStateChange`: body의 `detail`을 `runs.detail`에, `session_id`(있으면)를 `runs.provider_session_id`에 쓴다. `cancelled`이고 `task_id != ''`이면 `tasks.status = backlog`. `needs_attention`은 이슈 상태를 건드리지 않는다.
- `UpsertRun`: 새 필드 INSERT·UPDATE. `RunsForTask`/`GetRun`이 새 필드를 읽는다.

- [ ] RED: `TestCancelledEventMovesTaskToBacklog`, `TestNeedsAttentionIsSettled`(뒤에 오는 running 무시, 이슈는 in_progress 유지), `TestStateEventRecordsDetailAndSessionID`, `TestSettledToSettledApplies`(needs_attention → cancelled), `TestRunNewFieldsRoundTrip`, `TestOpenMigratesNewRunColumns`(PR #3 스키마로 만든 DB → Open → 필드 사용 가능).
- [ ] GREEN.

## Task 2: hub — 원장에 없는 Run의 이벤트는 ack하고 버린다

- `readLoop`: `errors.Is(err, store.ErrRunNotFound)`면 `slog.Warn`, ack(`Seq: env.Seq`), fanout 없음, 계속.
- [ ] RED: `TestEventForUnknownRunIsAckedAndDropped` — 러너 link로 원장에 없는 run의 이벤트를 Publish → spool `Pending == 0`이 될 때까지 기다림, `store.Events`는 비어 있음. 지금은 영원히 Pending이라 타임아웃(RED).
- [ ] GREEN.

## Task 3: protocol·runner — workspace 재사용, `--resume`, attention 파일

**Produces:**

```go
// protocol
type RunStartBody struct { …; WorkspaceRunID string `json:"workspace_run_id,omitempty"`; ResumeSessionID string `json:"resume_session_id,omitempty"` }
// spool
type RunRecord struct { …; WorkspaceRunID string }   // 컬럼 workspace_run_id
```

- `handleRunStart`: `wsID := body.WorkspaceRunID`가 비면 `env.RunID`. `git.Ensure(ctx, wsID, base)`. 핸들·기록에 `WorkspaceRunID`. `execute`의 `BuildArgs`에 `ResumeSessionID: body.ResumeSessionID`. `salvage(wsID, git)`.
- 종료 처리(`execute`의 default 분기 앞): `<ws.Path>/.taskyard/attention.md`가 있고 비어 있지 않으면 → 내용(4KiB 상한)을 읽고 파일을 지운 뒤 `needs_attention` 기록·이벤트(detail = 내용). 실패·취소 경로에서는 보지 않는다(실패가 우선).
- 모든 종결 `emitState`에 `session_id`를 싣는다: `emitTerminal(runID, state, detail, sessionID)`. 비종결(`running`)은 그대로.
- `Reconcile`: salvage에 `rec.WorkspaceRunID`(비면 `rec.RunID`).
- `BuildArgs`는 이미 `ResumeSessionID`를 받는다. 손대지 않는다.

- [ ] RED: lifecycle: `TestRunStartReusesWorkspaceOfPreviousRun`(run-1 실행 후 run-2를 `WorkspaceRunID: run-1`로 → 같은 worktree 경로, 원장 run-2의 WorkspaceRunID = run-1, run-1 브랜치의 변경이 보임), `TestRunStartPassesResumeSessionID`(fakeClaudeRecording으로 argv에 `--resume sess-x`), `TestAttentionFileEndsRunAsNeedsAttention`(fake claude가 `.taskyard/attention.md`를 쓰고 0으로 종료 → `needs_attention` 이벤트 detail에 내용, 원장 state needs_attention, 파일 삭제됨), `TestAttentionFileIsIgnoredWhenAgentFails`, `TestTerminalStateEventCarriesSessionID`; reconcile: `TestReconcileSalvagesByWorkspaceRunID`; spool: `TestRunRecordRoundTripsWorkspaceRunID`, 마이그레이션.
- [ ] GREEN.

## Task 4: pipeline — 이전 실행 텍스트와 기본 템플릿

```go
func PreviousRunText(run store.Run) string   // "" if run.ID == ""
```
- 형식: `이전 실행 run-…: <state>` + (detail이 있으면 `\n` + detail).
- `DefaultExecuteTemplate`에 두 절 추가: 멈추고 보고 절차("`.taskyard/attention.md`에 이유를 쓰고 종료"), 재시도 절(`{{previous_run}}`, `{{feedback}}` — 비어 있으면 무시하라고 명시).
- pipeline이 store를 import하지 않도록 `PreviousRunText(id, state, detail string)`로 받는다.

- [ ] RED: `TestPreviousRunTextFormats`, `TestDefaultTemplateMentionsAttentionFileAndRetryTokens`.
- [ ] GREEN.

## Task 5: web — 취소·재시도 라우트와 이슈 화면

| 라우트 | 동작 |
|---|---|
| `POST /runs/{id}/cancel` | Run이 settled면 409. `run.cancel` 전송. 러너 없으면 503. 성공 303 → 이슈 |
| `POST /runs/{id}/retry` | 폼 `mode=continue\|fresh`, `feedback`. 이전 Run이 settled가 아니면 409. 이슈에 활성 Run이 있으면 409. 새 Run: `PreviousRunID`, `Feedback`, `WorkspaceRunID`(continue: 이전 Run의 WorkspaceRunID 또는 그 ID; fresh: 새 ID), body에 `ResumeSessionID`(continue: 이전 `ProviderSessionID`; 비어 있으면 **fresh로 강등하고** 응답에 알림 대신 `slog.Warn` — 세션이 없는데 continue는 불가능). 프롬프트에 `previous_run`·`feedback` 변수. 이슈 → in_progress. 303 → 새 Run |
| `GET /projects/{key}/issues/{n}` | 최신 Run 기준: 비settled면 [취소]; settled면 detail(있으면)과 재시도 폼(모드 라디오 + feedback 한 줄). 모든 Run 행에 state·detail 첫 줄 |

- `handleIssueRun`도 `IsSettled`로 활성 판정(`IsTerminal` → `IsSettled`). `needs_attention` Run은 새 [실행]을 막지 않지만, 재시도 폼이 정석이다.
- same-origin 가드는 새 POST 둘에도.

- [ ] RED: `TestCancelSendsRunCancelAndRedirects`, `TestCancelSettledRunIs409`, `TestRetryContinueReusesWorkspaceAndResumesSession`(hub에 가짜 러너 → 봉투 body: `WorkspaceRunID == prev`, `ResumeSessionID == "sess-1"`, Prompt에 previous_run·feedback), `TestRetryFreshUsesNewWorkspaceAndNoResume`, `TestRetryContinueWithoutSessionFallsBackToFresh`, `TestRetryWhileActiveIs409`, `TestIssuePageShowsCancelForActiveAndRetryForSettled`, `TestIssuePageShowsAttentionReason`, `TestCrossSitePostIsRejected`에 두 라우트 추가.
- [ ] GREEN.

## Task 6: 인수 테스트

- `TestCancelRunningRunReturnsIssueToBacklog`: sleeper fake → [실행] → running 확인 → `POST cancel` → `runs.state == cancelled`, 이슈 `backlog`, 러너 원장 cancelled.
- `TestAttentionThenContinueRetryReusesWorktree`: fake claude ①은 `.taskyard/attention.md`를 쓰고 fixture 출력 → `needs_attention`, 이슈 상세에 이유 → `POST retry mode=continue feedback=…` → 러너가 받은 body의 `WorkspaceRunID`가 첫 Run, `ResumeSessionID`가 첫 Run의 session_id(fixture의 `00000000-…-0001`), 두 번째 Run의 worktree 경로 == 첫 Run의 것, attention 파일은 사라짐, 두 번째 Run succeeded → 이슈 `review`.
- fake claude에 "attention 파일 쓰기" 변형이 필요하다 — `newStack`에 바이너리를 바꿔 끼울 수 있게 옵션을 둔다.

- [ ] RED / GREEN.

---

## Self-Review

| PRD | Task |
|---|---|
| §7.5 멈추고 보고 → `needs_attention` + 이유 | 1, 3, 5 |
| §7.6 취소 → cancelled, 이슈 backlog, worktree 보존 | 1, 5 (보존은 gitops가 원래 안 지움) |
| §7.6 이어서 재시도 = 같은 worktree + `--resume` | 3, 5 |
| §7.6 처음부터 재시도 = 새 worktree·세션 | 5 |
| §7.6 `{{previous_run}}`, `{{feedback}}` | 4, 5 |
| §16.1 4 `--resume` 배선 | 3 |
| findings 2 livelock | 2 |

**알려진 간극**
- merge와 "merge됨" 종료는 GH-06/merge 정책 PR에서.
- 취소된 Run의 열린 PR을 닫을지 묻는 것(§7.6 표)은 PR 추적이 없어 다음으로.
- `needs_attention` 상태에서 러너가 재시작되면 `Reconcile`이 그 기록을 어떻게 보는지: 원장 state가 needs_attention이면 `terminalStates`(runner)에 포함시켜 조정 대상에서 제외한다 — Task 3에 포함.
- 이어서 재시도 중 이전 세션이 실제로 재개 가능한지는 `claude --resume`의 몫이다. 실패하면 실패 이벤트로 끝난다.
