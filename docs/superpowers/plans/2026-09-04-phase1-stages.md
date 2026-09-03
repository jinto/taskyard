# Phase 1 — 산출물과 1단계(분석·설계)

> **For agentic workers:** Task 단위로 실행한다. 각 Task는 실패하는 테스트 커밋(RED) → 구현 커밋(GREEN). Task 사이에 리뷰. 커밋·PR에 attribution 트레일러를 넣지 않는다.

**Goal:** 파이프라인이 "이슈 → 실행" 한 단계에서 "이슈 → [분석·설계] → 실행"이 된다(PRD §7.2). 에이전트가 남기는 파일(보고서, 문서)을 러너가 거둬 이슈에 붙이고 웹에서 읽을 수 있다(ST-06). 1단계는 프로젝트 템플릿·on/off·생략 기준을 갖고(ST-01, ST-03), 성공하면 서버가 2단계를 자동으로 이어 실행하며 보고서를 `{{stage1_report}}`로 넣는다(ST-04). 관측(`docs/phase1-observations.md` §21 판단): 1단계 생략은 본문 길이 기준, 이슈 단위 덮어쓰기.

**Spec:** PRD v1.2 §7.2(단계 표·규칙), §8.3 ST-01/03/04/05/06, §9.3(Run: stage).

**Non-goals** (유혹 순): 설계 승인 관문(ST-07 — 다음 PR), 별도 회고 Run(ST-08), 1단계가 하위 이슈를 만드는 것(§7.3 — 보고서에 "쪼개야 한다"고 쓰는 데서 멈춘다), 자동 트리거(ST-02), 라벨, `{{diff}}`·`{{pr_url}}`, 웹에서 기억 파일 편집(ME-03), 산출물의 렌더링(Markdown → HTML; `<pre>`로 보인다), summary.md를 산출물 체계로 합치기(PR 본문 경로는 그대로 둔다).

---

## Global Constraints

PR #3·#4·#7 계획의 Global Constraints가 그대로 적용된다(Go 1.26, CGO 없음, fixture 기반 테스트, 한국어 주석·UI, RED→GREEN 두 커밋, 새 컬럼은 `sqlitex.AddMissingColumns`, **attribution 트레일러 없음**). 추가:

- **단계 사이의 맥락은 산출물로만 전달한다**(§7.2 규칙). 2단계 Run은 새 세션·새 worktree다. 1단계의 worktree·세션을 잇지 않는다.
- **`.taskyard/` 산출물은 salvage보다 먼저 거둔다**(PR #7 규칙). 산출물 디렉터리도 같다.
- **이어 실행은 이벤트가 아니라 조정(reconcile)이다.** "이슈의 가장 최근 Run이 `succeeded`한 1단계 Run이면 2단계를 연다"는 검사를 **언제 몇 번 돌려도 같은 결과**가 나오게 만들고, 이벤트·서버 시작·러너 재접속에서 그 검사를 돌린다. 이벤트를 놓쳐도(서버 재시작, 러너 끊김) 다음 기회에 이어진다. 특정 이벤트에 콜백을 거는 설계는 그 이벤트가 이미 적용된 뒤 재시작하면 영영 안 이어진다(Codex 리뷰).
- **hub의 readLoop는 막지 않는다.** readLoop는 ack를 처리하므로 그 안에서 store 쓰기나 명령 전송을 하면 러너의 이벤트 흐름이 멈춘다. hub는 크기 1의 신호 채널에 non-blocking send만 하고, 이어 실행은 별도 goroutine이 그 신호를 받아 돈다.
- **Run 생성은 "활성 Run 없음" 검사와 한 트랜잭션이다.** 이어 실행 goroutine과 사람의 [실행]이 동시에 와도 하나만 만들어진다.

---

## 설계 결정

| 결정 | 내용 |
|---|---|
| 산출물의 정의 | 에이전트가 worktree의 **`.taskyard/artifacts/`** 에 쓴 일반 파일. 이름은 base name만(하위 디렉터리 없음, `..`·구분자 있으면 무시), 파일당 **256KiB**(넘으면 잘라 `truncated`), Run당 **16개**·합계 **1MiB**(이름순으로 채우다 예산이 다하면 나머지는 무시하고 로그). symlink는 `readWorktreeFile`의 규칙(worktree 밖이면 무시). 구현 때 link의 websocket 읽기 한도가 봉투 하나(≤ 256KiB + 여유)를 받는지 확인한다 |
| 언제 거두나 | `finish`에서 summary·attention과 같은 자리 — **salvage 앞**, 에이전트가 돌았던 모든 종결 경로. 읽고 지운다. 실패한 Run의 보고서도 남는다(사람이 읽을 것이다). `earlyFail`·`failBeforeStart`는 에이전트가 시작되기 전이라 이 Run의 산출물이 없다 — 이어서 재시도의 workspace에 남은 것은 이전 Run의 finish가 이미 거뒀다. 발행 실패는 기존 정책대로 로그(spool 쓰기가 실패하는 경우뿐) |
| 어떻게 전달 | 파일마다 이벤트 **`artifact.added`** `{name, content, truncated}`, 종결 이벤트보다 먼저. 서버 `artifacts(run_id, name, content, truncated, created_at)`, `(run_id, name)` 유일 — 재전송은 무시 |
| 웹 | 이슈 화면의 Run마다 산출물 이름 목록 → `GET /runs/{id}/artifacts/{name}`이 `<pre>`로 보인다. Run 화면에도 목록 |
| summary.md는 | 그대로(PR 본문·`runs.summary`). 산출물 체계로 합치지 않는다 — 두 경로 다 "읽고 지운다"이고, 합치면 PR #7의 배선을 다시 건드린다 |
| 1단계 템플릿 | 프로젝트 `analyze_template`. 기본 템플릿: 코드를 읽고 이슈를 분석해 **`.taskyard/artifacts/analysis.md`** 에 보고서(문제·관련 코드·설계·검증 방법·위험·한 번에 끝내기에 크면 어떻게 쪼갈지)를 쓴다. **코드 변경·커밋 없음.** attention 절차는 2단계와 같다 |
| 1단계 on/off·생략 기준 | 프로젝트 `analyze_enabled`(기본 켬), `analyze_skip_below`(기본 **200**자; 이슈 본문의 rune 수가 이보다 작으면 생략, 0이면 생략 없음). [실행] 폼에 `stage` 선택: **자동**(설정 기준) / **분석 먼저** / **바로 실행**. 기본은 자동. 자동은 설정과 본문 길이만 본다 — 이전 1단계가 있어도 다시 분석한다(이슈나 저장소가 그 사이 바뀌었을 수 있다). 옛 보고서를 쓰고 싶으면 "바로 실행"을 고른다 |
| Run의 단계 | `runs.stage` ∈ {`analyze`, `execute`}(지금은 `execute`만 쓰인다). 재시도는 이전 Run의 단계를 잇는다 |
| 1단계 Run의 PR·정리 | `PR: nil`(코드 변경이 없다), `CleanupMerged`는 무의미. worktree는 새로 만든다 — 에이전트가 코드를 읽을 자리. 1단계 worktree는 정리 대상이 아니다(merge가 없다) — 쌓인다. 정리 정책은 별도 항목으로 남긴다 |
| 이어 실행의 규칙 | **이슈의 가장 최근 Run이 `stage=analyze`·`succeeded`이면 2단계 Run을 연다.** 그것뿐이다. 새 2단계 Run이 생기는 순간 "가장 최근 Run"이 바뀌므로 두 번 열리지 않고, 1단계를 재시도해 다시 성공하면 그것이 가장 최근이라 새 2단계가 열린다(Codex: 옛 2단계가 있다고 막으면 재시도가 영영 안 이어진다). `needs_attention`·`failed`·`cancelled`면 잇지 않는다 — 사람의 차례 |
| 이어 실행이 사는 곳 | 새 패키지 **`internal/server/launch`**. `Launcher.ChainPending()`이 위 규칙을 모든 이슈에 대해 돌린다. 부르는 곳 셋: (1) hub의 `Settled` 신호 채널(크기 1, non-blocking send — readLoop는 안 막힌다), (2) 서버 시작 직후, (3) 러너 접속 직후(hub의 접속 훅). `Launcher.Start`는 web의 `launch`를 옮긴 것이고 web 핸들러도 그것을 쓴다(HTTP 응답 변환만 web에 남는다) |
| 동시성 | `store.CreateRunIfIdle(run)`: 이슈에 정착하지 않은 Run이 없을 때만 INSERT, 한 트랜잭션(연결이 하나라 직렬). 있으면 `ErrRunActive`. 이어 실행 goroutine과 사람의 [실행]이 겹쳐도 하나만 생긴다 |
| 러너가 없으면 | 2단계 `run.start`를 못 보내면 Launcher가 새 Run을 `failed`로 정리한다(web과 같은 보상). 그러면 가장 최근 Run이 failed 2단계라 자동으로는 다시 안 이어진다 — 의도다(failed는 사람의 차례). 대신 **전송 실패는 Run을 만들기 전에 거른다**: `ChainPending`은 러너가 접속해 있을 때만 돈다(`hub.Connected()`), 러너 접속 훅이 다시 부른다. 그래도 새는 창(검사 직후 끊김)은 failed 2단계 + 사람이 [실행]에서 "바로 실행" |
| 2단계의 보고서 계보 | 2단계 Run은 자기가 쓴 1단계 Run을 **`runs.report_run_id`** 에 기록한다. 이어 실행은 그 1단계 Run, 사람의 "바로 실행"은 이슈의 가장 최근 `succeeded` 1단계 Run(없으면 빈 값). 2단계 Run의 재시도는 이전 Run의 `report_run_id`를 잇는다 — "가장 최근" 대신 계보를 따른다 |
| 이슈 상태 | 1단계 시작 → `in_progress`. 1단계 `succeeded` → 그대로 `in_progress`(2단계가 이어진다; `review`로 보내지 않는다). `applyStateChange`가 같은 트랜잭션에서 `runs.stage`를 함께 읽어 `moveTaskFor(tx, runState, stage, taskID)`에 넘긴다 — **`execute`의 succeeded만 review**. 1단계 `cancelled` → `backlog`(기존 규칙) |
| 기본 실행 템플릿 | `{{issue}}` 아래에 "1단계 보고서:\n{{stage1_report}}" 절 추가. 생략됐으면 비어 있다 |
| 옛 프로젝트 | 마이그레이션: `analyze_template`은 빈 문자열 → **읽을 때 비어 있으면 기본 템플릿**(pipeline.DefaultAnalyzeTemplate)으로 채운다. 이러면 PR #7의 관측(옛 템플릿이 그대로)과 같은 일이 1단계에는 생기지 않는다. `analyze_enabled` 기본 1, `analyze_skip_below` 기본 200 |
| 테스트 대역 | 가짜 claude 스크립트가 `.taskyard/artifacts/analysis.md`를 쓴다. 인수 테스트는 프롬프트에 보고서가 들어갔는지 두 번째 run.start의 `Prompt`로 본다 |

**이슈 상태 전이 추가/변경**

| 사건 | 이슈 상태 |
|---|---|
| 1단계 Run `succeeded` | `in_progress` 유지 (2단계 자동 시작) |
| 2단계 Run `succeeded` | `review` (기존) |

---

## File Structure

| 경로 | 변경 |
|---|---|
| `internal/protocol/envelope.go` | `EvArtifactAdded`, `ArtifactBody{Name, Content string; Truncated bool}` |
| `internal/server/store/store.go` | `artifacts` 테이블, `Artifact`, `Artifacts(runID)`, `Artifact(runID, name)`, `ApplyEvent`가 `artifact.added` 저장; `Project.AnalyzeTemplate`·`AnalyzeEnabled`·`AnalyzeSkipBelow`, `ProjectSettings` 확장; `StageAnalyze`·`StageExecute`; `runs.report_run_id`; `moveTaskFor`가 단계를 본다; `LatestSucceededRun(taskID, stage)`; `CreateRunIfIdle(run)`; `TasksAwaitingExecute()`(가장 최근 Run이 succeeded analyze인 이슈들) |
| `internal/server/pipeline/pipeline.go` | `DefaultAnalyzeTemplate`, 실행 템플릿에 `{{stage1_report}}` 절 |
| `internal/server/launch/launch.go` (신규) | `Launcher{Store, Hub}`, `Start(p, task, Options{Stage, Previous, Feedback, WorkspaceRunID, ResumeSession, ReportRunID}) (store.Run, error)`, `ErrRunActive`·`ErrRunnerUnavailable`; `ChooseStage(p, task, choice)`; `ChainPending()`; `Run(ctx)`(신호를 기다려 `ChainPending`) |
| `internal/server/hub/hub.go` | `Hub.Settled() <-chan struct{}` — accepted된 `run.state_changed`마다 non-blocking send(크기 1, 합쳐짐); `Hub.OnConnect func()` — 러너 접속 직후 호출(goroutine) |
| `internal/server/web/web.go`, `templates/issue.html`, `run.html`, `project.html`, `artifact.html`(신규) | 핸들러가 Launcher를 쓴다; [실행] 폼의 `stage` 선택; 설정 폼에 1단계 템플릿·on/off·생략 기준; Run 목록에 단계·산출물; `GET /runs/{id}/artifacts/{name}` |
| `internal/runner/lifecycle/artifacts.go` (신규) | `takeArtifacts(worktree) []protocol.ArtifactBody`, `finish`에서 호출·발행 |
| `cmd/taskyard-server/main.go` | Launcher를 만들어 web에 주고, `go launcher.Run(ctx)`; 시작 직후·`hub.OnConnect`에서 `ChainPending`. 인수 스택은 in-process라 이 배선은 리뷰로 본다(PR #7과 같음) |
| `acceptance/acceptance_test.go` | 긴 이슈 → 1단계 → 산출물 → 자동 2단계(보고서가 프롬프트에) → succeeded → review |
| `README.md`, `docs/phase1-observations.md` | 단계 안내, 관측 "다음 PR" 절 갱신 |

---

## Task 1: protocol·store — 산출물, 1단계 설정, 단계

**Produces:**

```go
// protocol
const EvArtifactAdded = "artifact.added"
type ArtifactBody struct { Name, Content string; Truncated bool }

// store
const ( StageAnalyze = "analyze"; StageExecute = "execute" )
type Artifact struct { RunID, Name, Content string; Truncated bool; CreatedAt time.Time }
func (s *Store) Artifacts(runID string) ([]Artifact, error)          // 이름순
func (s *Store) Artifact(runID, name string) (Artifact, error)        // ErrArtifactNotFound
func (s *Store) LatestSucceededRun(taskID, stage string) (Run, bool, error)
func (s *Store) CreateRunIfIdle(r Run) error                          // 정착하지 않은 Run이 있으면 ErrRunActive, 한 트랜잭션
func (s *Store) TasksAwaitingExecute() ([]Task, error)                // 가장 최근 Run이 succeeded analyze인 이슈
type Run struct { …; ReportRunID string }                             // 2단계가 쓴 1단계 Run
type Project struct { …; AnalyzeTemplate string; AnalyzeEnabled bool; AnalyzeSkipBelow int }
type ProjectSettings struct { …; AnalyzeTemplate string; AnalyzeEnabled bool; AnalyzeSkipBelow int }
```

`scanProject`: `AnalyzeTemplate == ""`이면 `pipeline.DefaultAnalyzeTemplate` — store가 pipeline을 import하게 되는데, pipeline은 store를 모르니 순환이 없다. (반대로 기본 실행 템플릿은 생성 시 채워 넣는 기존 방식 그대로 — 두 방식이 공존하는 것은 PR #7 관측의 결과이고, 실행 템플릿까지 바꾸는 것은 이번 범위 밖.)

**Tests (RED):**
- `TestArtifactEventIsStoredAndListed` — `artifact.added` 둘 → `Artifacts(run)` 이름순 둘, `Artifact(run, "analysis.md")` 내용·truncated; 같은 `(run, name)` 재전송(다른 seq) → 하나 유지(첫 내용).
- `TestAnalyzeSucceededKeepsTaskInProgress` — `stage=analyze` Run의 succeeded → 이슈 `in_progress`; `stage=execute`는 `review`(기존).
- `TestCreateRunIfIdle` — 활성 Run 있으면 `ErrRunActive`·행 없음; 정착 Run만 있으면 생성.
- `TestTasksAwaitingExecute` — succeeded analyze가 최근이면 포함; 그 뒤 execute가 생기면 제외; needs_attention analyze는 제외.
- `TestLatestSucceededRunByStage` — 이슈에 analyze(succeeded), execute(failed), analyze(succeeded, 더 최근) → 최근 것; 없으면 ok=false.
- `TestProjectAnalyzeSettingsRoundTripAndDefaults` — 생성 시 빈 템플릿 → 읽으면 기본 템플릿; 설정으로 바꾸면 유지; 옛 스키마(`cleanup_merged`까지) `Open` → enabled=true, skip_below=200, 템플릿=기본.

---

## Task 2: pipeline — 1단계 기본 템플릿, `{{stage1_report}}`

`DefaultAnalyzeTemplate`(코드 변경 없음·`analysis.md`·attention 절차·`{{issue}}`·`{{memory}}`). `DefaultExecuteTemplate`에 절 추가:

```
1단계 보고서(있으면):
{{stage1_report}}
```

**Tests:** 기본 1단계 템플릿에 `analysis.md`, "코드를 바꾸지", `{{issue}}`, `{{memory}}`, `attention.md`가 있고 `git commit`·`gh`가 없다. 기본 실행 템플릿에 `{{stage1_report}}`가 있다.

---

## Task 3: runner — 산출물 수집

**Produces:**

```go
func takeArtifacts(worktree string) []protocol.ArtifactBody   // .taskyard/artifacts/* 읽고 지운다
```

`finish`: `summary` → `attention` → **`artifacts`** → salvage → …. 발행은 종결 이벤트 앞, 이름순. 디렉터리가 비면 디렉터리도 지운다(남기면 `git status`에 안 잡히지만 깔끔히).

**Tests (RED):**
- `TestArtifactsAreCollectedBeforeTerminal` — 가짜 에이전트가 `analysis.md`·`notes.txt`를 씀 → `artifact.added` 둘(이름순), 종결 앞, 파일 없음.
- `TestArtifactsIgnoreSubdirsSymlinksAndCapSize` — 하위 디렉터리·worktree 밖 symlink 무시, 300KiB 파일은 256KiB + truncated, 17개면 16개.
- `TestFailureSalvageDoesNotCommitArtifacts` — 실패 경로에서도 거두고 salvage 커밋에 `.taskyard/artifacts`가 없다.

---

## Task 4: launch 패키지와 이어 실행

**Produces:**

```go
package launch
type Launcher struct { Store *store.Store; Hub *hub.Hub }
type Options struct { Stage string; Previous store.Run; Feedback, WorkspaceRunID, ResumeSession, ReportRunID string }
var ErrRunActive, ErrRunnerUnavailable error
func (l *Launcher) Start(p store.Project, task store.Task, o Options) (store.Run, error)
func (l *Launcher) ChooseStage(p store.Project, task store.Task, choice string) string   // "auto"|"analyze"|"execute"
func (l *Launcher) ChainPending()                                                        // 규칙을 모든 이슈에 적용, 멱등
func (l *Launcher) Run(ctx context.Context)                                              // hub.Settled()를 기다려 ChainPending
```

`Start`는 web의 `launch`를 옮긴 것: Run 생성은 `CreateRunIfIdle`(활성 검사와 한 트랜잭션) → 프롬프트 조립(단계별 템플릿; `{{stage1_report}}`는 `o.ReportRunID`의 `analysis.md`, 비어 있으면 빈 문자열) → `PR`은 execute만 → 이슈 `in_progress` → SendCommand(실패 시 보상: Run failed, 이슈 되돌림). `ChooseStage("auto")`: `!AnalyzeEnabled` → execute; 본문 rune 수 `< AnalyzeSkipBelow` → execute; 아니면 analyze. web의 "바로 실행"은 `ReportRunID = LatestSucceededRun(task, analyze)`. `ChainPending`: `hub.Connected()`가 아니면 즉시 반환; `TasksAwaitingExecute()`마다 `Start(execute, ReportRunID = 그 analyze Run)`; `ErrRunActive`는 정상(겹침), 그 밖은 로그.

hub: `readLoop`에서 accepted된 `run.state_changed`마다 `settled` 채널(크기 1)에 non-blocking send. 접속 직후 `OnConnect`를 goroutine으로.

**Tests (RED):**
- `TestChooseStage` — 표: enabled·본문 길이·choice 조합.
- `TestStartAnalyzeSendsNoPRAndRendersAnalyzeTemplate` — run.start의 `PR == nil`, 프롬프트가 1단계 템플릿, Run.Stage=analyze.
- `TestStartExecuteFillsStage1ReportFromReportRun` — `ReportRunID`의 `analysis.md`가 프롬프트에, `runs.report_run_id` 기록; 비어 있으면 빈 문자열.
- `TestChainPendingIsIdempotent` — analyze succeeded → `ChainPending` 두 번 → execute Run 하나(`report_run_id` = analyze Run); needs_attention → 없음; 러너 미접속 → 아무것도 안 만듦; analyze 재시도 성공(가장 최근) → 새 execute 하나 더.
- `TestChainPendingRacesWithManualStart` — 두 goroutine이 동시에 `Start`/`ChainPending` → Run 하나만(`CreateRunIfIdle`).
- hub: `TestSettledSignalDoesNotBlockReadLoop` — 아무도 `Settled()`를 읽지 않아도 이벤트 열 개가 전부 ack된다.

---

## Task 5: web

- 핸들러가 `Launcher`를 쓴다(오류 → 409/503 변환). `handleIssueRun`은 폼 `stage`(auto/analyze/execute)를 `ChooseStage`에.
- 이슈 화면: [실행] 폼에 라디오 셋(자동 기본, 설정 기준 설명 한 줄), Run 목록에 단계와 산출물 링크, 최신 Run이 analyze·succeeded면 "2단계 시작 중" 안내.
- 설정 폼: 1단계 템플릿 textarea, `analyze_enabled` 체크박스, `analyze_skip_below` 숫자.
- `GET /runs/{id}/artifacts/{name}` → `artifact.html`(`<pre>`, truncated 표시). Run 화면에 산출물 목록.

**Tests (RED):** `TestUpdateSettingsSavesAnalyzeFields`, `TestRunIssueStageChoice`(analyze 선택 → run.start 프롬프트가 1단계 템플릿; execute 선택 → 실행 템플릿), `TestArtifactPageShowsContent`(escape 확인 포함), `TestIssuePageListsArtifacts`.

---

## Task 6: 인수 테스트

인수 스택이 `main`처럼 `go launcher.Run(ctx)`와 `OnConnect`를 배선한다. 본문 300자 이슈, 자동 → 1단계 run.start(PR 없음) → 가짜 에이전트가 `analysis.md` 씀 → succeeded → 이슈 `in_progress` 유지 → 서버가 2단계 run.start를 보냄, 프롬프트에 보고서 내용, `report_run_id` → succeeded → `review`. 두 번째: 본문 20자 → 바로 2단계. 세 번째: 1단계 `needs_attention`(attention 파일) → 2단계 없음. 네 번째: 1단계 성공 뒤 서버 프로세스 대신 **Launcher를 새로 만들어 `ChainPending`** → 2단계가 열린다(재시작 회복).

---

## Self-Review

- 단계 맥락은 산출물로만? — 2단계는 새 Run·새 worktree, 보고서는 `{{stage1_report}}`.
- 이어 실행이 두 번 열릴 수 있나? — 규칙이 "가장 최근 Run"이라 새 2단계가 생기면 조건이 깨진다; 동시 생성은 `CreateRunIfIdle`.
- 놓치면? — 조정이라 서버 시작·러너 접속·다음 신호에서 다시 돈다. 인수 테스트 네 번째.
- readLoop가 막히나? — 신호 채널 non-blocking; 테스트로 고정.
- 옛 프로젝트가 1단계 없이 남나? — 빈 템플릿은 읽을 때 기본으로; 정책 기본은 DDL.
- 산출물이 salvage에 들어가나? — 수집이 salvage 앞; 테스트.
