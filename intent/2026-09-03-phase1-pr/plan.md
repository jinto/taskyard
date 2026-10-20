# Phase 1 — PR 생성·추적과 merge 후 정리

> **For agentic workers:** Task 단위로 실행한다. 각 Task는 실패하는 테스트 커밋(RED) → 구현 커밋(GREEN). Task 사이에 리뷰. 커밋·PR에 attribution 트레일러를 넣지 않는다.

**Goal:** 실행 Run이 성공하면 러너가 브랜치를 push하고 PR을 만든다. 러너가 PR 상태(체크·리뷰·merge)를 추적해 서버에 알리고, merge가 확인되면 이슈는 `done`이 되고 worktree는 정책에 따라 정리된다. merge 자체는 사람이 GitHub에서 한다(PRD §7.6의 "merge는 사람이 직접" 정책). 관측 6번(`docs/phase1-observations.md`: 원격 없는 저장소에서 PR 단계가 막힘)을 프로젝트 설정으로 닫는다.

**Spec:** PRD v1.2 §7.6(종료 방식 "merge됨", merge 정책), §8.7 GH-05(사용자의 `git`·`gh` 로그인으로 push·PR), GH-06(PR 상태 추적), GH-10·§8.7.1(merge 후 worktree 정리), §16.1-7.

**Non-goals** (유혹 순): 자동 merge와 "승인 후 merge"(Taskyard의 merge 버튼), 취소·처음부터 재시도 때 옛 PR 닫기, `{{pr_url}}`·`{{diff}}` 템플릿 변수, 리뷰 코멘트를 `{{previous_run}}`에 넣기, 브랜치 삭제, 체크·리뷰 상태의 세부 모델링(문자열 하나로 둔다). §7.6이 이미 있는 것처럼 적은 항목들이지만 이번에 만들지 않는다.

---

## Global Constraints

PR #3·#4 계획의 Global Constraints가 그대로 적용된다(Go 1.26, CGO 없음, fixture 기반 테스트, 한국어 주석·UI, RED→GREEN 두 커밋, 새 컬럼은 `sqlitex.AddMissingColumns`, **attribution 트레일러 없음**). 추가:

- **종결 발행 지점은 하나.** PR 생성은 `execute`의 `finish` 안, `emitTerminal` 앞에서 한다. push·`gh pr create`가 도는 동안 Run은 `running`이다. 실패하면 종결 상태가 `needs_attention`이 된다 — 새 상태를 만들지 않는다.
- **`git`·`gh`는 사용자의 실제 환경으로 돈다.** GH-05가 사용자 로그인에 기댄다. `ScrubEnv`를 적용하지 않는다. 대신 `GIT_TERMINAL_PROMPT=0`, `GH_PROMPT_DISABLED=1`과 타임아웃 컨텍스트로 — 로그인 안 된 `gh`가 러너를 영원히 세우지 않게.
- **worktree는 merge된 것만 지운다.** §8.7.1: merge되지 않은 worktree는 어떤 경우에도 자동 삭제하지 않는다. CLOSED(merge 없이 닫힘)는 merge가 아니다.
- **크래시 창마다 회복 규칙이 있다.** finish의 어느 단계에서 러너가 죽어도 (a) 재시작 후 `Reconcile`이 오늘처럼 판정하고(살아 있음/재개 가능/lost → failed+salvage), (b) PR 생성은 `view`-before-`create`라 멱등이므로 이어서 재시도가 같은 finish를 다시 돌면 이미 만든 PR에 붙고, (c) PR 필드는 이벤트 발행보다 먼저 spool에 저장되므로 저장된 순간부터 Run 상태와 무관하게 추적된다. 원자성 기계를 새로 만들지 않는다.
- **`.taskyard/` 산출물은 salvage보다 먼저 거둔다.** `Salvage`는 `git add -A`로 미추적 파일까지 커밋한다. summary.md·attention.md를 먼저 읽고 지우지 않으면 PR 본문이 커밋에 들어가고, summary만 있는 worktree가 "앞선 커밋"이 된다.

---

## 설계 결정

| 결정 | 내용 |
|---|---|
| PR은 누가 | **러너가 Run 종료 후.** 에이전트는 코드를 커밋만 한다. 결정론적이고, 승인·토큰이 안 들며, 원격이 없는 저장소는 설정 하나로 끈다. 기본 템플릿의 "gh pr create" 지시는 사라진다 |
| 언제 | `finish`에서 상태가 `succeeded`이고, 브랜치가 base보다 앞선 커밋이 있고, `run.start`에 PR 스펙이 있을 때. 커밋이 없으면 PR 없이 `succeeded`(detail: "변경 커밋 없음 — PR 없음") |
| 어떻게 | `git push -u origin <branch>` → `gh pr view <branch>`로 **이미 있으면 재사용**(이어서 재시도는 같은 브랜치) → 없으면 `gh pr create --base <base> --head <branch> --title … --body-file …` → **`SaveRun`(PR 필드)** → `pr.updated` 발행. 원격 이름은 `origin` 고정 |
| 실패하면 | push·gh 실패는 **`needs_attention`**, detail = "PR 생성 실패: <stderr 한 줄>". 작업은 브랜치에 남아 있다. 사람이 원격·로그인을 고치고 이어서 재시도하면 같은 `finish` 경로가 다시 돈다 |
| 제목 | 서버가 `run.start`의 `pr.title`로 보낸다 = 이슈 제목. 러너는 이슈 제목을 모른다 |
| 본문 | 에이전트가 worktree의 **`.taskyard/summary.md`** 에 쓴 변경 설명(PRD §7.2 2단계 산출물 "변경 설명"). attention.md와 같은 패턴 — 러너가 **읽고 지운다**(16KiB 상한, symlink 탈출 금지). 없으면 서버가 보낸 `pr.body`(이슈 본문)만. 본문 머리에 "Taskyard 이슈 #N · Run <id>" 한 줄 |
| summary는 PR 없어도 | 종결 `run.state_changed` body에 `summary`로 실어 `runs.summary`에 저장, 이슈 화면에 보인다. PR을 끈 프로젝트에서도 변경 설명은 남는다 |
| 추적 | 러너의 **별도 goroutine + ticker**(`Config.PRPollInterval`, 기본 60s), `main`이 `go lm.TrackPRs(ctx)`로 띄운다(`Start`·`Reconcile`과 나란히). `Start`의 select에 넣지 않는다 — 그 루프는 승인 요청을 나르므로 네트워크 I/O가 막으면 안 된다. 추적 집합 = **`spool.LoadRuns()`** 의 기록 중 `PRNumber > 0`이고 `PRState == OPEN`인 것 — Run 상태와 무관하다. `ActiveRuns`(미전송 이벤트가 있는 Run)는 쓰지 않는다. MERGED·CLOSED에서 추적 종료. gh 일시 오류는 로그만, 이벤트 없음 |
| 추적의 주인 | 키는 **브랜치**. 이어서 재시도는 같은 브랜치·같은 PR을 쓰므로 기록이 둘 이상일 수 있다 — 그중 `StartedAtUnix`가 가장 큰 Run이 주인이고, 폴링·`pr.updated`·정리는 그 Run으로만 한다. 재사용 시점에 옛 기록의 `PRState`를 `superseded`로 바꿔 추적에서 뺀다 |
| `pr.updated` | 새 이벤트 타입. body: `url`, `number`, `state`(OPEN/MERGED/CLOSED), `checks`(pending/success/failure/none — `statusCheckRollup` 요약), `review`(`reviewDecision` 그대로: APPROVED/CHANGES_REQUESTED/REVIEW_REQUIRED/""), `worktree_removed`(bool). 같은 값이면 발행하지 않는다 — 1분마다 이벤트가 쌓이지 않게 |
| 서버 반영 | `runs`에 `pr_url`·`pr_number`·`pr_state`·`pr_checks`·`pr_review`. **MERGED만, 그리고 그 Run이 이슈의 최신 Run일 때만 이슈를 `done`으로 옮긴다.** 최신이 아닌 Run의 MERGED는 PR 필드만 저장한다(재시도가 진행 중인 이슈를 옛 PR이 끝내지 않게). **`moveTaskFor`는 현재 `done`인 이슈를 건드리지 않는다** — 옛 Run의 늦은 `succeeded`가 `review`로 되돌리지 못한다. `done`에서 나가는 길은 [실행]·재시도(`launch`가 `in_progress`로)뿐 |
| checks 축약 | `statusCheckRollup` 배열(CheckRun: `status`·`conclusion`, StatusContext: `state`). 비어 있음 → `none`; 하나라도 conclusion이 FAILURE/ERROR/TIMED_OUT/CANCELLED/ACTION_REQUIRED 또는 state가 FAILURE/ERROR → `failure`; 아니면 하나라도 status != COMPLETED 또는 state PENDING/EXPECTED → `pending`; 나머지(SUCCESS/NEUTRAL/SKIPPED) → `success` |
| `SaveRun` 실패 | 기존과 같이 로그만 남기고 계속한다(spool 이벤트는 별도로 내구적이다). PR 필드 저장이 실패하면 추적에서 빠질 뿐 PR은 서버 이벤트로 남고, 이어서 재시도가 다시 붙인다. 재시도 기계를 만들지 않는다 |
| `TaskDone` | `done` 상태 추가(PRD §9.1). 이슈 목록에 표시 |
| 정리(GH-10) | **MERGED에서만.** 정책 `cleanup_merged`(기본 켬)일 때, 지우기 직전에 다시 확인: `git status --porcelain` 비어 있음(무시 파일은 애초에 안 나온다 — `default.profraw` 관측 5번), push 안 된 커밋 0(`rev-list @{u}..HEAD`), 그 workspace를 쓰는 활성 Run 없음. 통과하면 `git worktree remove`; 하나라도 걸리면 보존하고 로그. **브랜치는 남긴다** |
| 정책 스냅샷 | `create_pr`·`cleanup_merged`는 `run.start`에 실린 값으로 그 Run 내내 고정. 프로젝트 설정을 나중에 바꿔도 진행 중 Run에는 영향 없다 |
| 프로젝트 설정 | `create_pr`(기본 **켬** — PRD §7.2의 2단계는 PR을 만든다), `cleanup_merged`(기본 켬 — §8.7.1 기본 삭제). 기본값이 사는 곳은 둘: 마이그레이션 DDL `INTEGER NOT NULL DEFAULT 1`(옛 행)과 프로젝트 생성 폼의 체크박스(새 행, 기본 체크). `CreateProject`는 주어진 값을 그대로 저장한다 — 꺼진 값도 정당하다. 설정 폼(`/template`)에 체크박스 둘. playground처럼 원격 없는 저장소는 `create_pr`을 끈다 — 끄지 않으면 push 실패로 `needs_attention`이 된다. 이 경로는 테스트로 고정한다 |
| 처음부터 재시도의 옛 PR | **명시적 보류**(outcomes 계획과 같음). 옛 PR은 닫지 않고, 둘 다 추적된다. 사람이 GitHub에서 닫으면 CLOSED로 추적이 끝난다 |
| `.taskyard/` 파일과 git | attention.md·summary.md는 러너가 읽고 지우므로 남지 않는다. 에이전트가 커밋해 버리는 경우는 기본 템플릿이 "커밋하지 않는다"로 막는다. `.git/info/exclude`는 건드리지 않는다(memory.md는 추적되는 파일이다) |
| 테스트 대역 | `Config.GHBinary`(기본 `gh`) — `ClaudeBinary`와 같은 방식. 가짜 `gh` 스크립트는 인자를 기록하고, 제어 파일에서 상태(OPEN/MERGED/CLOSED)를 읽어 `pr view` JSON을 돌려준다. 원격은 `t.TempDir()`의 bare 저장소를 `origin`으로 — `git push`가 오프라인에서 진짜로 돈다 |

**이슈 상태 전이 추가** (outcomes 계획의 행렬에 한 줄)

| 사건 | 이슈 상태 |
|---|---|
| `pr.updated` state=MERGED | `done` |

---

## File Structure

| 경로 | 변경 |
|---|---|
| `internal/protocol/envelope.go` | `RunStartBody.PR *PRSpec{Title, Body}`, `RunStartBody.CleanupMerged`, `EvPRUpdated`, `PRUpdatedBody` |
| `internal/server/store/store.go` | `Project.CreatePR`·`CleanupMerged`, `UpdateProjectSettings`에 두 필드, `runs`에 `summary`·`pr_*`, `TaskDone`, `ApplyEvent`가 `pr.updated`와 종결 body의 `summary` 반영 |
| `internal/gitops/worktree.go` | `Push`, `AheadOf(base)`, `Unpushed()`, `Remove(runID)` |
| `internal/runner/github/gh.go` (신규) | `gh pr view`·`gh pr create` 얇은 래퍼, `Binary` 주입, 타임아웃·프롬프트 금지 env |
| `internal/runner/lifecycle/pr.go` (신규) | `createPR`(finish에서 호출), `trackPRs` 루프, `cleanupMerged`, `takeSummary` |
| `internal/runner/lifecycle/lifecycle.go` | `runSpec`에 `pr`·`cleanupMerged`, `finish`에서 `createPR`, `Config.GHBinary`·`PRPollInterval` |
| `internal/runner/spool/spool.go` | `RunRecord.PRURL`·`PRNumber`·`PRState`·`WorktreeRemoved` |
| `internal/server/pipeline/pipeline.go` | 기본 템플릿 5단계를 `.taskyard/summary.md` 작성으로 바꾼다(`gh pr create` 지시 제거) |
| `internal/server/web/web.go`, `templates/project.html`, `issue.html`, `run.html` | 설정 체크박스 둘, `launch`가 PR 스펙·정책 전달, 이슈·Run 화면에 PR 링크·상태·summary |
| `cmd/taskyard-runner/main.go` | `--gh`, `--pr-poll` 플래그, `go lm.TrackPRs(ctx)` 배선 |
| `acceptance/acceptance_test.go` | 성공 → push → PR → MERGED → `done` → worktree 삭제 관통 |
| `README.md`, `docs/phase1-observations.md` | 설정 안내 한 줄, 관측 6번 해소 표기 |

---

## Task 1: protocol·store — PR 스펙, `pr.updated`, 프로젝트 정책, `done`

**Produces:**

```go
// protocol
type PRSpec struct { Title, Body string }
type RunStartBody struct { …; PR *PRSpec `json:"pr,omitempty"`; CleanupMerged bool `json:"cleanup_merged,omitempty"` }
const EvPRUpdated = "pr.updated"
type PRUpdatedBody struct { URL string; Number int; State, Checks, Review string; WorktreeRemoved bool }

// store
const TaskDone = "done"
type Project struct { …; CreatePR, CleanupMerged bool }
type ProjectSettings struct { ExecuteTemplate string; AllowedTools []string; CreatePR, CleanupMerged bool }
func (s *Store) UpdateProjectSettings(key string, st ProjectSettings) error
type Run struct { …; Summary, PRURL string; PRNumber int; PRState, PRChecks, PRReview string }
```

**Tests (RED):**
- `TestProjectPolicyRoundTrip` — `CreateProject`에 `CreatePR: true, CleanupMerged: false`를 주면 그대로 읽힌다; `UpdateProjectSettings`로 바꾸면 유지; 옛 스키마(`allowed_tools`까지만 있는 projects)를 `Open`하면 옛 행은 둘 다 켬.
- `TestApplyPRUpdatedStoresAndMovesTaskToDone` — `review` 이슈의 Run에 `pr.updated{state: OPEN}` → `runs.pr_*` 저장, 이슈 그대로; `MERGED` → 이슈 `done`; 그 뒤 옛 Run의 `OPEN` → 이슈 `done` 유지.
- `TestDoneDoesNotRegress` — `done` 이슈의 옛 Run에 늦은 `succeeded` → `done` 유지; 최신이 아닌 Run의 `MERGED` → PR 필드만, 이슈 상태 그대로.
- `TestApplyTerminalStoresSummary` — 종결 `run.state_changed` body의 `summary` → `runs.summary`.
- 기존 `UpdateProjectSettings` 호출 테스트를 새 시그니처로.

---

## Task 2: gitops — push·앞선 커밋·미push·삭제

**Produces:**

```go
func (m *Manager) Push(ctx, runID string) error                 // git -C <wt> push -u origin <branch>
func (m *Manager) AheadOf(ctx, runID, base string) (int, error)  // rev-list --count <base>..HEAD
func (m *Manager) Unpushed(ctx, runID string) (int, error)       // rev-list --count @{u}..HEAD; upstream 없으면 오류
func (m *Manager) Remove(ctx, runID string) error                // git worktree remove <path>
```

`git`은 `GIT_TERMINAL_PROMPT=0`으로 돈다(기존 `m.git` 헬퍼에 추가).

**Tests (RED):** bare 저장소를 `origin`으로 둔 temp repo. `AheadOf`=0 → 커밋 → 1; `Push` 뒤 bare에 브랜치 존재, `Unpushed`=0; 커밋 하나 더 → `Unpushed`=1; `Remove` 뒤 경로 없음, `git worktree list`에 없음; 원격 없는 repo의 `Push`는 오류.

---

## Task 3: runner — `gh` 래퍼, PR 생성, 추적, 정리

**Produces:**

```go
// internal/runner/github
type Client struct { Binary string; Timeout time.Duration }
type PR struct { URL string; Number int; State, Checks, Review string }
func (c Client) View(ctx, dir, branch string) (PR, bool, error)      // gh pr view <branch> --json …; 없으면 ok=false
func (c Client) Create(ctx, dir, base, head, title, body string) (PR, error)

// internal/runner/lifecycle (pr.go)
func takeSummary(worktree string) (string, bool)                     // .taskyard/summary.md 읽고 지움, 16KiB
func (m *Manager) createPR(ctx, spec runSpec) (protocol.PRUpdatedBody, error)
func (m *Manager) trackPRs(ctx)                                       // ticker 루프; Start와 별도로 main이 띄운다
func (m *Manager) cleanupMerged(ctx, rec spool.RunRecord) bool
```

`finish` 순서: **summary·attention 수집(읽고 삭제)** → salvage(실패·취소 경로만, 기존대로) → (succeeded && spec.pr != nil && AheadOf>0) push → view/create(재사용이면 옛 기록 `superseded`) → `SaveRun`(PR 필드) → `pr.updated` 발행 → `SaveRun`(종결) → `emitTerminal(…, summary)`. push·gh 실패면 state=`needs_attention`, detail="PR 생성 실패: …". 종결 발행은 여전히 `emitTerminal` 한 번.

`TrackPRs`(exported — `main`이 띄운다): tick마다 `LoadRuns()`에서 OPEN PR 기록을 브랜치별 최신 하나로 추려 순회 → `View` → 값이 바뀌었으면 `SaveRun` + `pr.updated`. MERGED이고 `rec.CleanupMerged`면 `cleanupMerged` → 성공 시 `WorktreeRemoved=true`를 같은 이벤트에 실음. 활성 Run이 그 workspace를 쓰면 정리를 건너뛴다.

**Tests (RED):**
- `TestFinishCreatesPRAndPublishesUpdate` — 가짜 claude가 커밋 하나 만들고 `.taskyard/summary.md`를 씀. bare origin. 가짜 gh: `pr view` → 없음, `pr create` → URL. 결과: origin에 브랜치, gh 인자에 `--title <이슈 제목>`, body 파일에 summary와 머리말, `pr.updated{OPEN}` 뒤 `run.state_changed{succeeded, summary}`, worktree에 summary.md 없음.
- `TestFinishReusesExistingPR` — 가짜 gh `pr view`가 PR을 돌려주면 `create` 호출 없음.
- `TestFinishWithoutCommitsSkipsPR` — 커밋 없이 `.taskyard/summary.md`만 있음 → summary는 거두고(이벤트 body에), salvage 커밋 없음, push·gh 호출 없음, `succeeded`, detail에 "PR 없음".
- `TestFailureSalvageDoesNotCommitTaskyardFiles` — 에이전트가 summary.md를 쓰고 비정상 종료 → salvage 커밋에 `.taskyard/summary.md`가 없다.
- `TestFinishWithoutRemoteNeedsAttention` — origin 없음, `create_pr` 켬 → `needs_attention`, detail에 "PR 생성 실패".
- `TestFinishWithoutPRSpecSkipsPR` — `PR == nil` → push 없음(playground 경로).
- `TestTrackPRsEmitsOnChangeAndCleansMerged` — `PRPollInterval` 20ms. 제어 파일 OPEN → 이벤트 없음(변화 없음) → MERGED → `pr.updated{MERGED, worktree_removed: true}`, worktree 경로 없음, 추적 종료(그 뒤 gh 호출 없음).
- `TestCleanupKeepsDirtyOrUnpushedWorktree` — 미커밋 파일 있음 → 보존; push 안 된 커밋 있음 → 보존; 이벤트의 `worktree_removed=false`.
- `TestTrackPRsReloadsFromSpoolAfterRestart` — 이벤트가 전부 ack된 뒤(`ActiveRuns` 비어 있음) `New`를 다시 만들어도 OPEN PR이 추적된다.
- `TestTrackPRsFollowsLatestRunOnSharedBranch` — 이어서 재시도로 같은 브랜치 기록이 둘 → 옛 기록은 `superseded`, gh 호출과 `pr.updated`는 새 Run으로만.
- `TestCrashAfterPushReattachesOnRetry` — 가짜 gh가 `create` 뒤 러너를 죽인 것처럼 기록을 `running`으로 남김 → 재시작 `Reconcile`이 lost로 판정(failed) → 이어서 재시도 → `pr view`가 기존 PR을 돌려주므로 `create` 호출 없음, 같은 PR 번호로 `pr.updated`.

---

## Task 4: pipeline — 기본 템플릿

5단계를 바꾼다:

```
5. 변경 설명을 `.taskyard/summary.md`에 쓴다 — 무엇을 왜 바꿨는지, 리뷰어가 먼저 볼 곳. 이 파일은 커밋하지 않는다(PR 본문이 된다).
```

**Tests:** 기본 템플릿에 `gh pr create`가 없고 `summary.md`가 있다.

---

## Task 5: web — 설정, `launch`, 화면

- 설정 폼(`POST /projects/{key}/template`)에 `create_pr`·`cleanup_merged` 체크박스. 체크박스는 안 보내면 false — 폼이 항상 두 필드를 다루므로 그대로 저장.
- `launch`: `p.CreatePR`이면 `PR: &PRSpec{Title: task.Title, Body: task.Body}`, `CleanupMerged: p.CleanupMerged`.
- 이슈 화면: 최신 Run의 PR 링크·state·checks·review, summary. Run 목록 행에 PR 링크. 이슈 상태 `done`이면 [실행] 대신 "완료" 표시(재시도 폼은 남긴다 — PRD §6.2 재시도는 새 Run).
- Run 화면: PR 줄.

**Tests (RED):** `TestUpdateSettingsSavesPolicies`, `TestRunIssueSendsPRSpec`(`create_pr` 켬 → body.PR.Title == 이슈 제목; 끔 → nil), `TestIssuePageShowsPR`(pr.updated 적용 뒤 링크와 상태가 HTML에).

---

## Task 6: 인수 테스트

`newStackWith`에 bare origin과 가짜 gh. 인수 스택은 in-process 러너라 `main`의 배선 자체는 증명하지 못한다 — 스택이 `main`과 같은 모양으로 `go lm.TrackPRs(ctx)`를 띄우고, `main`의 한 줄은 리뷰로 본다. 이슈 실행 → 가짜 claude 커밋 + summary → `pr.updated{OPEN}` → `runs.pr_url` → 이슈 `review` → 제어 파일 MERGED → `pr.updated{MERGED}` → 이슈 `done`, worktree 삭제. 두 번째 시나리오: `create_pr` 끈 프로젝트 → push 없음, `succeeded`, summary 저장.

---

## Self-Review

- 종결 발행 지점 하나 유지? — `createPR`은 `finish` 안에서 상태만 바꾸고 `emitTerminal`은 그대로 한 번.
- 정착 → 비정착 없음? — PR 생성 중 Run은 `running`(비정착)이고 종결은 그 뒤. `pr.updated`는 상태를 건드리지 않는다.
- `done`을 되돌리는 경로 없음? — `moveTaskFor`가 `done`을 건드리지 않고, MERGED는 최신 Run일 때만 `done`으로. `done`에서 나가는 전이는 [실행]·재시도(`launch`의 `in_progress`)뿐.
- 크래시 창? — 회복 규칙(Global Constraints)과 `TestCrashAfterPushReattachesOnRetry`.
- salvage가 산출물을 커밋하나? — 수집이 먼저. `TestFailureSalvageDoesNotCommitTaskyardFiles`.
- 원격 없는 저장소가 조용히 실패하지 않나? — `needs_attention` + detail. 끄는 설정이 있다.
- 러너가 멈출 수 있나? — gh·git 모두 타임아웃과 프롬프트 금지.
