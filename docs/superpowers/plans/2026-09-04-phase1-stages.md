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
- **이어 실행은 멱등이다.** 같은 1단계 성공 이벤트가 두 번 적용되거나(재전송은 `accepted=false`라 애초에 안 옴), 이미 2단계 Run이 있으면 다시 열지 않는다.
- **서버의 반응 로직은 hub 잠금 밖에서 돈다.** 2단계 시작은 hub를 통해 명령을 보내므로, hub가 잠금을 든 채 콜백을 부르면 교착한다.

---

## 설계 결정

| 결정 | 내용 |
|---|---|
| 산출물의 정의 | 에이전트가 worktree의 **`.taskyard/artifacts/`** 에 쓴 일반 파일. 이름은 base name만(하위 디렉터리 없음, `..`·구분자 있으면 무시), 파일당 **256KiB**(넘으면 잘라 `truncated`), Run당 **16개**까지. symlink는 `readWorktreeFile`의 규칙(worktree 밖이면 무시) |
| 언제 거두나 | `finish`에서 summary·attention과 같은 자리 — **salvage 앞**, 모든 종결 경로. 읽고 지운다. 실패한 Run의 보고서도 남는다(사람이 읽을 것이다) |
| 어떻게 전달 | 파일마다 이벤트 **`artifact.added`** `{name, content, truncated}`, 종결 이벤트보다 먼저. 서버 `artifacts(run_id, name, content, truncated, created_at)`, `(run_id, name)` 유일 — 재전송은 무시 |
| 웹 | 이슈 화면의 Run마다 산출물 이름 목록 → `GET /runs/{id}/artifacts/{name}`이 `<pre>`로 보인다. Run 화면에도 목록 |
| summary.md는 | 그대로(PR 본문·`runs.summary`). 산출물 체계로 합치지 않는다 — 두 경로 다 "읽고 지운다"이고, 합치면 PR #7의 배선을 다시 건드린다 |
| 1단계 템플릿 | 프로젝트 `analyze_template`. 기본 템플릿: 코드를 읽고 이슈를 분석해 **`.taskyard/artifacts/analysis.md`** 에 보고서(문제·관련 코드·설계·검증 방법·위험·한 번에 끝내기에 크면 어떻게 쪼갈지)를 쓴다. **코드 변경·커밋 없음.** attention 절차는 2단계와 같다 |
| 1단계 on/off·생략 기준 | 프로젝트 `analyze_enabled`(기본 켬), `analyze_skip_below`(기본 **200**자; 이슈 본문의 rune 수가 이보다 작으면 생략, 0이면 생략 없음). [실행] 폼에 `stage` 선택: **자동**(설정 기준) / **분석 먼저** / **바로 실행**. 기본은 자동 |
| Run의 단계 | `runs.stage` ∈ {`analyze`, `execute`}(지금은 `execute`만 쓰인다). 재시도는 이전 Run의 단계를 잇는다 |
| 1단계 Run의 PR·정리 | `PR: nil`(코드 변경이 없다), `CleanupMerged`는 무의미. worktree는 새로 만든다 — 에이전트가 코드를 읽을 자리. 1단계 worktree는 정리 대상이 아니다(merge가 없다) — 쌓인다. 정리 정책은 별도 항목으로 남긴다 |
| 이어 실행 | 1단계 Run이 **`succeeded`** 로 정착하면 서버가 2단계 Run을 **자동으로** 연다. 같은 이슈·`previous_run_id` 없음·`stage=execute`·새 worktree. `{{stage1_report}}` = 그 Run의 `analysis.md` 산출물(없으면 빈 문자열 + 프롬프트의 절 제목만 남는다). `needs_attention`·`failed`·`cancelled`면 잇지 않는다 — 사람의 차례 |
| 이어 실행이 사는 곳 | hub가 이벤트를 적용한 뒤 잠금 밖에서 `OnRunSettled(run)` 콜백을 부른다(적용이 `accepted`이고 상태가 바뀐 경우만). 콜백은 새 패키지 **`internal/server/launch`** 의 `Launcher.Start`를 부른다 — web의 `launch`를 그대로 옮긴 것. web 핸들러도 같은 Launcher를 쓴다(HTTP 응답 변환만 web에 남는다) |
| 이어 실행의 멱등 | Launcher.Start는 이슈에 정착하지 않은 Run이 있으면 `ErrRunActive`. 콜백은 그 위에 하나 더: 이 1단계 Run보다 뒤에 만들어진 Run이 이미 있으면 열지 않는다 |
| 러너가 없으면 | 2단계 `run.start`를 못 보내면 Launcher가 새 Run을 `failed`로 정리한다(web과 같은 보상). 이슈는 `in_progress`에 남고 1단계 산출물은 있다 — 사람이 [실행]으로 2단계를 다시 시작한다(자동 기준이 다시 1단계를 고르지 않도록, **최근 성공한 1단계가 있으면 자동은 2단계를 고른다**) |
| 2단계 재시도의 보고서 | 2단계 Run의 재시도(이어서·처음부터)도 `{{stage1_report}}`를 채운다 — 이슈의 가장 최근 `succeeded` 1단계 Run의 `analysis.md` |
| 이슈 상태 | 1단계 시작 → `in_progress`. 1단계 `succeeded` → 그대로 `in_progress`(2단계가 이어진다; `review`로 보내지 않는다 — **`moveTaskFor`는 `execute` 단계의 succeeded만 review로**). 1단계 `cancelled` → `backlog`(기존 규칙) |
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
| `internal/server/store/store.go` | `artifacts` 테이블, `Artifact`, `Artifacts(runID)`, `Artifact(runID, name)`, `ApplyEvent`가 `artifact.added` 저장; `Project.AnalyzeTemplate`·`AnalyzeEnabled`·`AnalyzeSkipBelow`, `ProjectSettings` 확장; `StageAnalyze`·`StageExecute`; `moveTaskFor`가 단계를 본다; `LatestSucceededRun(taskID, stage)` |
| `internal/server/pipeline/pipeline.go` | `DefaultAnalyzeTemplate`, 실행 템플릿에 `{{stage1_report}}` 절 |
| `internal/server/launch/launch.go` (신규) | `Launcher{Store, Hub}`, `Start(p, task, Options{Stage, Previous, Feedback, WorkspaceRunID, ResumeSession}) (store.Run, error)`, `ErrRunActive`·`ErrRunnerUnavailable`; `ChooseStage(p, task, choice)`; `OnRunSettled(run)` |
| `internal/server/hub/hub.go` | `Hub.OnRunSettled func(store.Run)` — 적용이 accepted이고 `run.state_changed`가 정착 상태로 바꿨을 때 잠금 밖에서 호출 |
| `internal/server/web/web.go`, `templates/issue.html`, `run.html`, `project.html`, `artifact.html`(신규) | 핸들러가 Launcher를 쓴다; [실행] 폼의 `stage` 선택; 설정 폼에 1단계 템플릿·on/off·생략 기준; Run 목록에 단계·산출물; `GET /runs/{id}/artifacts/{name}` |
| `internal/runner/lifecycle/artifacts.go` (신규) | `takeArtifacts(worktree) []protocol.ArtifactBody`, `finish`에서 호출·발행 |
| `cmd/taskyard-server/main.go` | Launcher를 만들어 hub 콜백과 web에 준다 |
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
type Project struct { …; AnalyzeTemplate string; AnalyzeEnabled bool; AnalyzeSkipBelow int }
type ProjectSettings struct { …; AnalyzeTemplate string; AnalyzeEnabled bool; AnalyzeSkipBelow int }
```

`scanProject`: `AnalyzeTemplate == ""`이면 `pipeline.DefaultAnalyzeTemplate` — store가 pipeline을 import하게 되는데, pipeline은 store를 모르니 순환이 없다. (반대로 기본 실행 템플릿은 생성 시 채워 넣는 기존 방식 그대로 — 두 방식이 공존하는 것은 PR #7 관측의 결과이고, 실행 템플릿까지 바꾸는 것은 이번 범위 밖.)

**Tests (RED):**
- `TestArtifactEventIsStoredAndListed` — `artifact.added` 둘 → `Artifacts(run)` 이름순 둘, `Artifact(run, "analysis.md")` 내용·truncated; 같은 `(run, name)` 재전송(다른 seq) → 하나 유지(첫 내용).
- `TestAnalyzeSucceededKeepsTaskInProgress` — `stage=analyze` Run의 succeeded → 이슈 `in_progress`; `stage=execute`는 `review`(기존).
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
type Options struct { Stage string; Previous store.Run; Feedback, WorkspaceRunID, ResumeSession string }
var ErrRunActive, ErrRunnerUnavailable error
func (l *Launcher) Start(p store.Project, task store.Task, o Options) (store.Run, error)
func (l *Launcher) ChooseStage(p store.Project, task store.Task, choice string) string   // "auto"|"analyze"|"execute"
func (l *Launcher) OnRunSettled(run store.Run)                                           // hub 콜백
```

`Start`는 web의 `launch`를 옮긴 것: 활성 Run 검사 → Run 생성(`Stage`) → 프롬프트 조립(단계별 템플릿, `{{stage1_report}}`는 `LatestSucceededRun(task, analyze)`의 `analysis.md`) → `PR`은 execute만 → UpsertRun → 이슈 `in_progress` → SendCommand(실패 시 보상). `ChooseStage("auto")`: `!AnalyzeEnabled` → execute; 최근 성공한 1단계가 있으면 execute; 본문 rune 수 `< AnalyzeSkipBelow` → execute; 아니면 analyze. `OnRunSettled`: `Stage == analyze && State == succeeded`이고, 그 Run 뒤에 만들어진 Run이 없으면 `Start(execute)`; 오류는 로그.

hub: `OnRunSettled`를 `readLoop`에서 `ApplyEvent`가 accepted이고 타입이 `run.state_changed`일 때, 저장된 Run을 읽어 정착 상태면 **fanout 뒤, 잠금 밖에서** 호출. 콜백이 nil이면 아무것도 안 한다.

**Tests (RED):**
- `TestChooseStage` — 표: enabled·본문 길이·최근 1단계 유무·choice 조합.
- `TestStartAnalyzeSendsNoPRAndRendersAnalyzeTemplate` — run.start의 `PR == nil`, 프롬프트가 1단계 템플릿, Run.Stage=analyze.
- `TestStartExecuteFillsStage1Report` — 성공한 analyze Run의 `analysis.md`가 프롬프트에; 없으면 빈 문자열.
- `TestOnRunSettledChainsExecuteOnce` — analyze succeeded → execute Run 하나 생김; 같은 호출 다시 → 둘째 없음; needs_attention → 없음.
- hub: `TestHubCallsOnRunSettledOutsideLock` — 콜백 안에서 `SendCommand`를 불러도 교착 없음(타임아웃 테스트).

---

## Task 5: web

- 핸들러가 `Launcher`를 쓴다(오류 → 409/503 변환). `handleIssueRun`은 폼 `stage`(auto/analyze/execute)를 `ChooseStage`에.
- 이슈 화면: [실행] 폼에 라디오 셋(자동 기본, 설정 기준 설명 한 줄), Run 목록에 단계와 산출물 링크, 최신 Run이 analyze·succeeded면 "2단계 시작 중" 안내.
- 설정 폼: 1단계 템플릿 textarea, `analyze_enabled` 체크박스, `analyze_skip_below` 숫자.
- `GET /runs/{id}/artifacts/{name}` → `artifact.html`(`<pre>`, truncated 표시). Run 화면에 산출물 목록.

**Tests (RED):** `TestUpdateSettingsSavesAnalyzeFields`, `TestRunIssueStageChoice`(analyze 선택 → run.start 프롬프트가 1단계 템플릿; execute 선택 → 실행 템플릿), `TestArtifactPageShowsContent`(escape 확인 포함), `TestIssuePageListsArtifacts`.

---

## Task 6: 인수 테스트

본문 300자 이슈, 자동 → 1단계 run.start(PR 없음) → 가짜 에이전트가 `analysis.md` 씀 → succeeded → 이슈 `in_progress` 유지 → 서버가 2단계 run.start를 보냄, 프롬프트에 보고서 내용 → succeeded → `review`. 두 번째: 본문 20자 → 바로 2단계. 세 번째: 1단계 `needs_attention`(attention 파일) → 2단계 없음.

---

## Self-Review

- 단계 맥락은 산출물로만? — 2단계는 새 Run·새 worktree, 보고서는 `{{stage1_report}}`.
- 이어 실행이 두 번 열릴 수 있나? — 재전송은 accepted=false; 콜백은 "뒤에 만들어진 Run 없음" + `ErrRunActive`.
- 교착? — hub 콜백은 fanout 뒤 잠금 밖; 테스트로 고정.
- 옛 프로젝트가 1단계 없이 남나? — 빈 템플릿은 읽을 때 기본으로; 정책 기본은 DDL.
- 산출물이 salvage에 들어가나? — 수집이 salvage 앞; 테스트.
