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
| 취소 라우트 | `POST /runs/{id}/cancel`. **활성 Run**이면 `run.cancel` 전송(러너의 `handleRunCancel`). **`needs_attention`** 이면 프로세스가 없으므로 서버가 직접 `cancelled`로 바꾸고 이슈를 backlog로(명령 없음 — 러너 원장의 needs_attention은 정착 상태라 조정 대상이 아니다). 종결(succeeded/failed/cancelled)이면 409 |
| 취소의 종결 이벤트 | **Phase 0 버그 수정.** `handleRunCancel`이 `cancelled`를 보낸 뒤 `execute`가 프로세스 종료 시 `m.fail`로 `failed`를 또 보내, 종결→종결 허용 규칙 때문에 서버가 `failed`로 끝난다. 종결 이벤트는 **`execute`가 한 곳에서** `record.State`(취소면 cancelled)로 보내고, `handleRunCancel`은 이벤트를 보내지 않는다 |
| 세션 없는 이어서 재시도 | **409.** `--resume`이 곧 "이어서"이므로(§7.6) 세션이 없으면 그 선택은 성립하지 않는다. 조용히 처음부터로 강등하지 않는다 — 사용자가 처음부터를 고르게 한다 |
| 재시도 뒤 이슈 상태 | 이전 Run이 무엇이었든 **`in_progress`**. `review`에서의 재시도도 허용한다(재시도 = 새 Run, PRD §6.2) |
| livelock | `hub.readLoop`에서 `store.ErrRunNotFound`면 **그 seq를 ack**(`Seq: env.Seq`)하고 경고 로그. 러너의 `spool.Ack(run, seq)`가 그 run의 ≤seq 행을 지우므로 spool이 비워진다. 원장에 없는 Run의 이벤트는 버린다 — 서버 DB를 지운 경우가 이 경로다 |
| PR 관련 §7.6 항목 | **명시적 보류.** 취소 시 "PR을 열어둘지 닫을지 묻기", 처음부터 재시도 시 "옛 PR close"는 PR 추적(GH-06)이 없어 이번에 하지 않는다. UI에는 그 선택지를 두지 않는다 |

**이슈 상태 전이 행렬** (Run 이벤트 또는 사람의 행동 → `tasks.status`)

| 사건 | 이슈 상태 |
|---|---|
| [실행] / 재시도 시작 | `in_progress` |
| Run `succeeded` | `review` |
| Run `failed` | 변화 없음 |
| Run `needs_attention` | 변화 없음 (`in_progress`) |
| Run `cancelled` (러너 이벤트 또는 서버 직접) | `backlog` |

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
- `applyStateChange`: body의 `detail`을 `runs.detail`에 쓴다(빈 문자열도 그대로 — 최신 사실). `session_id`는 **비어 있지 않을 때만** `runs.provider_session_id`에 쓴다 — 알던 세션을 빈 값으로 지우지 않는다. `cancelled`이고 `task_id != ''`이면 `tasks.status = backlog`. `needs_attention`·`failed`는 이슈 상태를 건드리지 않는다.
- `CancelSettledRun(runID) error`: `needs_attention`인 Run을 서버가 직접 `cancelled`로 바꾸고 이슈를 backlog로 옮긴다(한 tx). 다른 상태면 `ErrNotCancellable`. 취소 라우트가 쓴다.
- `UpsertRun`: 새 필드 INSERT·UPDATE. `RunsForTask`/`GetRun`이 새 필드를 읽는다.

- [ ] RED: `TestCancelledEventMovesTaskToBacklog`, `TestNeedsAttentionIsSettled`(뒤에 오는 running 무시, 이슈는 in_progress 유지), `TestStateEventRecordsDetailAndSessionID`, `TestEmptySessionIDDoesNotClearKnownOne`, `TestSettledToSettledApplies`(needs_attention → cancelled → 이슈 backlog), `TestCancelSettledRunMovesNeedsAttentionToCancelled`(needs_attention만 허용, running/succeeded는 ErrNotCancellable), `TestRunNewFieldsRoundTrip`, `TestOpenMigratesNewRunColumns`(PR #3 스키마로 만든 DB → Open → 필드 사용 가능).
- [ ] GREEN.

## Task 2: hub — 원장에 없는 Run의 이벤트는 ack하고 버린다

- `readLoop`: `errors.Is(err, store.ErrRunNotFound)`면 `slog.Warn`, ack(`Seq: env.Seq`), fanout 없음, 계속.
- [ ] RED: `TestEventsForUnknownRunAreAckedAndDropped` — 러너 link로 원장에 없는 run의 이벤트를 **세 개** Publish → spool `Pending(run) == 0`이 될 때까지 기다림, `store.Events(run)`은 비어 있음, `store.ResumePoints()`에 그 run이 없음(원장 행이 생기지 않음). 지금은 영원히 Pending이라 타임아웃(RED). ack 값이 `env.Seq`임은 spool이 비워진다는 사실로 증명된다(`spool.Ack(run, seq)`는 ≤seq를 지운다).
- [ ] GREEN.

## Task 3: protocol·runner — workspace 재사용, `--resume`, attention 파일

**Produces:**

```go
// protocol
type RunStartBody struct { …; WorkspaceRunID string `json:"workspace_run_id,omitempty"`; ResumeSessionID string `json:"resume_session_id,omitempty"` }
// spool
type RunRecord struct { …; WorkspaceRunID string }   // 컬럼 workspace_run_id
```

- **workspace id가 gitops 호출 전부에 흐른다.** `handleRunStart`: `wsID := body.WorkspaceRunID`가 비면 `env.RunID`. `git.Ensure(ctx, wsID, base)`. `runHandle`과 모든 `RunRecord`에 `WorkspaceRunID`. `execute(…, wsID)`; `salvage(wsID, git)`(현재 `runID`를 넘기는 세 곳 전부); `Reconcile`의 salvage는 `rec.WorkspaceRunID`(비면 `rec.RunID`). `Status`/`Diff`는 lifecycle이 호출하지 않는다(인수 테스트만 — fresh는 `wsID == runID`라 그대로 성립).
- `execute`의 `BuildArgs`에 `ResumeSessionID: body.ResumeSessionID`. `BuildArgs`는 이미 받는다. 손대지 않는다.
- **종결 이벤트는 `execute` 한 곳에서.** `emitTerminal(runID, state, detail, sessionID string)`을 새로 두고 `execute`의 네 분기(parseErr / waitErr / 과금 / 성공·attention)와 `failBeforeStart`·`recordEarlyFailure` 경로가 이것만 쓴다. `m.fail`은 `emitTerminal(runID, "failed", err.Error(), sessionID)`로 바뀐다. `handleRunCancel`은 원장에 cancelled를 남기고 프로세스를 죽이되 **이벤트를 보내지 않는다** — 프로세스가 죽으면 `execute`의 waitErr 분기가 `terminalState()`로 cancelled를 골라 보낸다. `running`은 `emitState` 그대로.
- **attention 파일.** `readMemory`를 `readWorktreeFile(worktree, rel string, max int) (string, bool)`로 일반화한다(심링크 탈출 거부, 상한, 없으면 false). 성공 분기(`default`)에서 `.taskyard/attention.md`를 4KiB로 읽는다. 내용이 비어 있지 않으면 → **파일을 지우고**(git 작업보다 먼저; 이어서 재시도가 같은 worktree를 쓴다) → `record.State = "needs_attention"`, `SaveRun`, `emitTerminal(…, "needs_attention", 내용, session)`. 읽기 실패·심링크 탈출·빈 파일은 attention 아님(정상 성공). 실패·취소·과금 분기에서는 보지 않는다. 에이전트가 파일을 커밋해 버린 경우: 삭제가 worktree의 미커밋 변경으로 남고 다음 Run이나 salvage가 그 삭제를 실어 간다 — 결정적이고 무해하므로 그대로 둔다.
- `Reconcile`: `terminalStates`(runner)에 `needs_attention` 추가 — 조정 대상이 아니다.

- [ ] RED: lifecycle: `TestRunStartReusesWorkspaceOfPreviousRun`(run-1 실행 후 run-2를 `WorkspaceRunID: run-1`로 → `ws.Path`가 `git.WorktreePath("run-1")`, 브랜치 `taskyard/run/run-1`, 원장 run-2의 WorkspaceRunID = run-1, run-1이 남긴 파일이 보임), `TestRunStartPassesResumeSessionID`(argv에 `--resume` `sess-x`가 인접), `TestAttentionFileEndsRunAsNeedsAttention`(detail == 내용, 원장 needs_attention, 파일 삭제됨, `session_id`가 이벤트에 있음), `TestAttentionFileIsIgnoredWhenAgentFails`(dirty-then-fail이 attention도 씀 → failed), `TestAttentionSymlinkEscapeIsIgnored`, `TestCancelledRunEndsAsCancelledNotFailed`(sleeper → cancel → **마지막 상태 이벤트가 cancelled**이고 failed 이벤트가 없음, session_id 포함), `TestTerminalStateEventCarriesSessionID`(성공 경로); reconcile: `TestReconcileSalvagesByWorkspaceRunID`, `TestReconcileSkipsNeedsAttention`; spool: `TestRunRecordRoundTripsWorkspaceRunID`, 마이그레이션.
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
| `POST /runs/{id}/cancel` | 활성(비settled) → `run.cancel` 전송, 러너 없으면 503. `needs_attention` → `store.CancelSettledRun`(명령 없음). 종결 → 409. 성공 303 → 이슈 |
| `POST /runs/{id}/retry` | 폼 `mode=continue\|fresh`, `feedback`. 이전 Run이 settled가 아니면 409. 이슈에 활성 Run이 있으면 409. **continue인데 이전 `ProviderSessionID`가 비어 있으면 409**("이어갈 세션이 없습니다 — 처음부터 재시도를 고르세요"). 새 Run: `PreviousRunID`, `Feedback`, `WorkspaceRunID`(continue: 이전 Run의 WorkspaceRunID, 비면 이전 Run ID; fresh: 새 Run ID), body에 `ResumeSessionID`(continue만). 프롬프트에 `previous_run`·`feedback` 변수. 이슈 → in_progress. 303 → 새 Run |
| `GET /projects/{key}/issues/{n}` | 최신 Run 기준: 비settled면 [취소]; settled면 detail(있으면)과 재시도 폼(모드 라디오 + feedback 한 줄). 모든 Run 행에 state·detail 첫 줄 |

- `handleIssueRun`도 `IsSettled`로 활성 판정(`IsTerminal` → `IsSettled`). `needs_attention` Run은 새 [실행]을 막지 않지만, 재시도 폼이 정석이다.
- same-origin 가드는 새 POST 둘에도.

- [ ] RED: `TestCancelSendsRunCancelAndRedirects`, `TestCancelNeedsAttentionRunWithoutRunner`(명령 없이 cancelled·backlog), `TestCancelTerminalRunIs409`, `TestRetryContinueReusesWorkspaceAndResumesSession`(hub에 가짜 러너 → 봉투 body: `WorkspaceRunID == prev.WorkspaceRunID`, `ResumeSessionID == "sess-1"`, Prompt에 previous_run 텍스트와 feedback; 새 Run 행의 PreviousRunID·Feedback·WorkspaceRunID), `TestRetryFreshUsesNewWorkspaceAndNoResume`, `TestRetryContinueWithoutSessionIs409AndCreatesNoRun`, `TestRetryWhileActiveIs409`, `TestRetryFromReviewMovesIssueToInProgress`, `TestIssuePageShowsCancelForActiveAndRetryForSettled`, `TestIssuePageShowsAttentionReason`, `TestCrossSitePostIsRejected`에 두 라우트 추가.
- [ ] GREEN.

## Task 6: 인수 테스트

- `TestCancelRunningRunReturnsIssueToBacklog`: sleeper fake → [실행] → running 확인 → `POST cancel` → `runs.state == cancelled`이고 **그 뒤 failed로 바뀌지 않음**(300ms 뒤 재확인), 이슈 `backlog`, 러너 원장 cancelled, 저장된 이벤트에 `failed`가 없음.
- `TestAttentionThenContinueRetryReusesWorktree`: fake claude ①은 `.taskyard/attention.md`를 쓰고 fixture 출력 → `runs.state == needs_attention`, `runs.detail`에 이유, `runs.provider_session_id == 00000000-0000-0000-0000-000000000001`(fixture), 이슈 상세에 이유와 재시도 폼 → `POST retry mode=continue feedback=…` → 러너가 받은 body: `WorkspaceRunID`가 첫 Run ID, `ResumeSessionID`가 그 세션, Prompt에 이유와 feedback → 두 번째 Run의 원장 WorkspaceRunID == 첫 Run ID, worktree 디렉터리가 하나뿐, attention 파일 없음 → 두 번째 Run succeeded → 이슈 `review`.
- `TestContinueRetryWithoutSessionIsRefused`: 첫 Run을 `failed`로 끝내되 세션 없이(no-init fixture) → `POST retry mode=continue` → 409, Run 수 그대로.
- fake claude에 "attention 파일 쓰기" 변형이 필요하다 — `newStack`에 바이너리 스크립트 본문을 바꿔 끼울 수 있게 옵션을 둔다(현재는 고정 문자열).

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

**Codex 리뷰 반영 (2026-09-03).** 초안에 major 6·minor 4. 전부 반영: needs_attention에서의 취소(서버 직접), 취소가 failed로 끝나는 Phase 0 버그(종결 이벤트를 execute 한 곳으로), workspace id를 gitops 호출 전부에, `emitTerminal` API와 빈 session_id 보호, attention 파일의 안전 리더·삭제 순서·커밋된 파일 처리, 세션 없는 continue는 409, hub 테스트 강화, 이슈 상태 전이 행렬, 테스트 단언 구체화, §7.6 PR 항목 명시적 보류.

**알려진 간극**
- merge와 "merge됨" 종료는 GH-06/merge 정책 PR에서.
- §7.6의 PR 관련 동작 — 취소 시 PR을 열어둘지 묻기, 처음부터 재시도 시 옛 PR close — 는 PR 추적이 없어 **이번에 하지 않는다.** UI에 그 선택지를 두지 않는다.
- `needs_attention` 상태에서 러너가 재시작되면 `Reconcile`이 그 기록을 어떻게 보는지: 원장 state가 needs_attention이면 `terminalStates`(runner)에 포함시켜 조정 대상에서 제외한다 — Task 3에 포함.
- 이어서 재시도 중 이전 세션이 실제로 재개 가능한지는 `claude --resume`의 몫이다. 실패하면 실패 이벤트로 끝난다.
