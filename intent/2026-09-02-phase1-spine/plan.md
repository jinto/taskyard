# Phase 1 척추 — 프로젝트·이슈에서 실행까지

> **For agentic workers:** 이 계획은 Task 단위로 실행한다. 각 Task는 실패하는 테스트 커밋(RED) → 구현 커밋(GREEN) 두 커밋으로 남긴다. Task 사이에 리뷰를 둔다.

**Goal:** Phase 0의 `POST /runs`가 받던 프롬프트 한 줄을 "프로젝트의 실행 템플릿 + 이슈"로 조립한 결과로 바꾼다. 프로젝트와 이슈가 생기고, 이슈의 [실행] 버튼이 Run을 만들며, 이슈 상세에서 Run의 상태가 보인다. 나머지 배관(worktree → claude → 이벤트 → 승인 → diff)은 Phase 0 그대로다.

**Spec:** `taskyard-prd-v1.md` v1.2 — §7.2(단계), §8.1(PM-01, 04, 05), §8.3(ST-01, 02, 04), §8.5(EX-10은 1 유지), §12(데이터 모델), §16.1 첫 문단.

**Non-goals (다음 PR):** 분석·회고 단계, 설계 승인 관문, Artifact 첨부, 종료 방식 5종 UI(취소·재시도), 칸반 보드, merge 정책, Slack, 프로젝트 기억 편집, 동시 실행 N.

---

## Global Constraints

Phase 0 계획의 Global Constraints가 그대로 적용된다. 요점:

- Go 1.26, `CGO_ENABLED=0`, `modernc.org/sqlite`.
- Claude Code 기동 플래그는 `claudecode.BuildArgs`가 정본이다. 건드리지 않는다.
- 테스트는 fixture 기반. 실제 `claude`를 호출하지 않는다(`make smoke`만 예외).
- 코드와 식별자는 영어, 주석과 문서와 UI 산문은 한국어. 프로토콜 식별자와 상태값은 원문 유지.
- 각 Task는 RED 커밋 → GREEN 커밋. RED 커밋은 컴파일이 안 돼도 된다(테스트 파일만).
- 절대 하지 않는 것: worktree 삭제 프리미티브 추가, `--bare`, `Co-Authored-By` 제거(하네스가 붙인다).

**기존 DB 호환.** `runs`(server)와 `runs`(spool) 테이블에 컬럼을 추가한다. `CREATE TABLE IF NOT EXISTS`만으로는 기존 DB에 컬럼이 생기지 않는다. `Open`에서 `PRAGMA table_info(<table>)`로 현재 컬럼을 읽고, **없는 컬럼만** `ALTER TABLE … ADD COLUMN`한다. 드라이버 오류 문자열("duplicate column name")에 의존하지 않는다. 마이그레이션 테스트는 옛 스키마로 만든 DB와 현재 스키마 DB 둘 다에 `Open`을 돌려 컬럼이 있고 두 번째 `Open`이 멱등임을 확인한다.

**보안 노출(수용).** 웹 UI에는 인증이 없다(`docs/phase0-findings.md`). 이 PR은 상태를 바꾸는 POST 라우트를 넷 더 연다(프로젝트 생성, 템플릿 수정, 이슈 생성, 실행 트리거). 기본 바인딩이 `127.0.0.1`이라는 사실만이 방어선이다. 이 PR에서 하는 최소 조치: **모든 POST 라우트(기존 `/runs/{id}/approve` 포함)에 same-origin 가드**를 건다 — `Sec-Fetch-Site` 헤더가 있으면 `same-origin`/`none`만 통과, 없으면 `Origin` 헤더의 host가 요청 `Host`와 같을 때만 통과, 둘 다 없으면(curl 등) 통과. 위반은 403. 인증 자체는 Phase 1 보안 기본선 항목이며 이 PR 범위 밖이다.

**`l` 초기화 순서(불변식).** `cmd/taskyard-runner/main.go`의 순서 — `lifecycle.New(Publish 클로저가 l을 늦게 읽음)` → `l, err = link.New(...)` → `go lm.Start(ctx)` → `lm.Reconcile(ctx)` → `l.Run(ctx)` — 를 그대로 유지한다. Task 3이 `Config.Git`을 `Config.Repos`로 바꿀 때 이 순서를 건드리지 않는다. `lm.Start`와 `Reconcile`은 이벤트를 발행하고, 발행은 클로저를 거쳐 `l`에 닿는다.

---

## 설계 결정 (사용자 확정, 2026-09-02)

| 결정 | 내용 |
|---|---|
| 범위 | 척추만. 실행 단계 하나 |
| 저장소 지정 | 프로젝트에 러너 머신의 절대 경로. 러너는 `--allow-repo` 허용 목록으로 검증 |
| 기본 템플릿 | 범용. 사용자 스킬에 의존하지 않음 |
| 이슈 UI | 리스트 + 상세. 보드 없음 |
| 템플릿 변수 | 문자열 치환. `{{issue}}`는 서버가, `{{memory}}`는 러너가(worktree의 `.taskyard/memory.md`) 치환. 모르는 토큰은 그대로 둔다 |
| 이슈 상태 | `backlog` → [실행] → `in_progress` → Run `succeeded` → `review`. 실패·취소는 상태를 되돌리지 않는다 |
| Run 상태 갱신 | `run.state_changed` 이벤트가 `runs.state`를 갱신한다(findings 3번). 이슈 상태 `review` 전이도 같은 트랜잭션 |
| 동시 실행 | 1 유지 |

---

## File Structure

| 경로 | 변경 | 책임 |
|---|---|---|
| `internal/server/store/store.go` | 수정 | projects, tasks 테이블. runs에 task_id·stage·created_at. 상태 이벤트 반영 |
| `internal/server/store/store_test.go` | 수정 | 위 전부 |
| `internal/server/pipeline/pipeline.go` | 신규 | 템플릿 렌더링, 이슈 텍스트, 기본 실행 템플릿 |
| `internal/server/pipeline/pipeline_test.go` | 신규 | |
| `internal/protocol/envelope.go` | 수정 | `RunStartBody` 타입을 protocol로 올림 (서버·러너 공유) |
| `internal/runner/lifecycle/lifecycle.go` | 수정 | run.start의 repo_path·base_branch, 저장소 해석기, `{{memory}}` 치환 |
| `internal/runner/lifecycle/reconcile.go` | 수정 | RunRecord.RepoPath로 관리자 해석 |
| `internal/runner/lifecycle/repos.go` | 신규 | 허용 목록 → `*gitops.Manager` 해석기 |
| `internal/runner/lifecycle/*_test.go` | 수정 | |
| `internal/runner/spool/spool.go` | 수정 | RunRecord.RepoPath |
| `cmd/taskyard-runner/main.go` | 수정 | `--repo` → `--allow-repo`(반복) |
| `internal/server/web/web.go` | 수정 | 프로젝트·이슈 라우트, 실행 트리거. `POST /runs` 제거 |
| `internal/server/web/templates/{projects,project,issue}.html` | 신규 | |
| `internal/server/web/templates/{runs,run}.html` | 수정 | `runs.html` 삭제(인덱스가 프로젝트 목록), `run.html`에 이슈 링크 |
| `internal/server/web/web_test.go` | 수정 | |
| `acceptance/acceptance_test.go` | 수정 | 트리거 테스트를 이슈 경로로 |
| `README.md`, `docs/phase0-findings.md` | 수정 | 플래그, findings 3번 해소 표시 |

---

## Task 1: store — projects, tasks, Run 연결, 상태 반영

**Files:** `internal/server/store/store.go`, `store_test.go`

**Interfaces (Produces):**

```go
type Project struct {
	ID              string
	Key             string // URL 식별자. [a-z0-9-]{1,32}
	Name            string
	RepoPath        string // 러너 머신의 절대 경로
	DefaultBranch   string
	ExecuteTemplate string
	CreatedAt       time.Time
}

type Task struct {
	ID        string
	ProjectID string
	Number    int    // 프로젝트 안에서 1부터 증가
	Title     string
	Body      string
	Status    string // TaskBacklog / TaskInProgress / TaskReview
	CreatedAt time.Time
}

const (
	TaskBacklog    = "backlog"
	TaskInProgress = "in_progress"
	TaskReview     = "review"
)

var ErrProjectNotFound, ErrTaskNotFound, ErrDuplicateKey error

func (s *Store) CreateProject(p Project) (Project, error)          // ID 없으면 발급. Key 중복이면 ErrDuplicateKey
func (s *Store) GetProject(key string) (Project, error)
func (s *Store) ListProjects() ([]Project, error)                  // created_at 순
func (s *Store) UpdateProjectTemplate(key, executeTemplate string) error
func (s *Store) CreateTask(t Task) (Task, error)                   // 같은 tx에서 다음 number 발급
func (s *Store) GetTask(projectID string, number int) (Task, error)
func (s *Store) ListTasks(projectID string) ([]Task, error)        // number 역순
func (s *Store) UpdateTaskStatus(taskID, status string) error
func (s *Store) RunsForTask(taskID string) ([]Run, error)          // created_at 역순
```

`Run`에 `TaskID`, `Stage`, `CreatedAt` 필드를 추가한다. `UpsertRun`은 세 필드를 INSERT에 포함하되 **`created_at`은 `ON CONFLICT`에서 덮어쓰지 않는다**(last_acked_seq와 같은 이유).

**스키마 추가:**

```sql
CREATE TABLE IF NOT EXISTS projects (
  id               TEXT PRIMARY KEY,
  key              TEXT NOT NULL UNIQUE,
  name             TEXT NOT NULL,
  repo_path        TEXT NOT NULL,
  default_branch   TEXT NOT NULL,
  execute_template TEXT NOT NULL,
  created_at       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  number     INTEGER NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (project_id, number)
);
-- runs: ALTER TABLE로 추가 (기존 DB 호환)
--   task_id TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0
```

**상태 반영.** `ApplyEvent`에서 `accepted && env.Type == protocol.EvRunStateChanged`이면 body(`{"body":{"state":…}}`)의 `state`로 `runs.state`를 갱신한다. 갱신된 state가 `succeeded`이고 `runs.task_id != ''`이면 `tasks.status = 'review'`로 바꾼다. 둘 다 같은 tx.

**단조 규칙.** `accepted`만으로는 부족하다. 같은 seq의 재전송은 `INSERT OR IGNORE`가 걸러 주지만, Runner 재시작 후 `Reconcile`이 **새 seq**로 `running`을 발행할 수 있고(`reconcile.go`의 alive/resumable 분기), 그것이 저장된 `succeeded`를 덮어쓰면 안 된다. 규칙: **종결 상태(`succeeded`/`failed`/`cancelled`)는 비종결 상태로 되돌리지 않는다.** 이벤트는 그대로 저장하되(원장은 사실을 기록한다) `runs.state`와 `tasks.status`만 건너뛴다. 종결→종결(예: `failed`→`cancelled`)은 허용한다 — 순서대로 온 마지막 사실이다.

- [ ] **Step 1: 실패하는 테스트** — `TestCreateProjectAssignsIDAndRejectsDuplicateKey`, `TestCreateTaskNumbersPerProject`(프로젝트 둘, 각각 1·2·3), `TestRunsForTaskOrdersNewestFirst`, `TestStateChangedEventUpdatesRunState`, `TestSucceededEventMovesTaskToReview`, `TestSameSeqResendDoesNotChangeState`(succeeded 뒤 같은 seq 재전송 → accepted=false, 상태 불변), `TestNewerRunningEventDoesNotRegressSucceededState`(succeeded 뒤 **더 큰 seq**의 running → 이벤트는 저장되고 accepted=true지만 `runs.state`는 succeeded, `tasks.status`는 review 유지), `TestOpenMigratesExistingRunsTable`(옛 스키마로 DB를 만든 뒤 Open → 컬럼 존재), `TestOpenIsIdempotentOnCurrentSchema`.
- [ ] **Step 2: RED 커밋** — `test(store): 프로젝트·이슈·상태 반영 실패 테스트`
- [ ] **Step 3: 구현**
- [ ] **Step 4: `go test ./internal/server/store/ -race -count=1`**
- [ ] **Step 5: GREEN 커밋** — `feat(store): 프로젝트·이슈 테이블과 run.state_changed 반영`

---

## Task 2: pipeline — 프롬프트 조립

**Files:** `internal/server/pipeline/pipeline.go`, `pipeline_test.go`

**Interfaces (Produces):**

```go
// Render는 template의 {{name}} 토큰을 vars로 치환한다. vars에 없는 토큰은
// 그대로 남긴다 — 러너가 치환할 {{memory}}가 서버를 통과해야 하고,
// 오타난 토큰은 프롬프트에 보이는 편이 조용히 사라지는 것보다 낫다.
//
// 토큰 문법: 정규식 `\{\{([A-Za-z0-9_]+)\}\}`. 그 밖의 것 — `{{` 뒤에 `}}`가
// 없거나, `{{ issue }}`처럼 공백이 있거나, `{{{x}}}`처럼 중첩되거나 — 은
// 토큰이 아니며 바이트 그대로 남는다. 입력을 왼쪽에서 오른쪽으로 한 번만
// 훑고, 치환된 값 안은 다시 훑지 않는다(값에 `{{issue}}`가 있어도 그대로).
// 같은 토큰이 여러 번 나오면 전부 치환한다.
func Render(template string, vars map[string]string) string

// IssueText는 {{issue}}에 들어갈 본문이다.
//   "#12 로그인 폼에서 이메일 대소문자 무시\n\n<body>"
// body가 비어 있으면 제목 줄만.
func IssueText(number int, title, body string) string

// DefaultExecuteTemplate은 프로젝트 생성 시 채워지는 범용 실행 템플릿이다.
const DefaultExecuteTemplate = `...`
```

`DefaultExecuteTemplate` 내용(한국어, 스킬 의존 없음):

```
다음 이슈를 해결하라.

{{issue}}

프로젝트 기억:
{{memory}}

절차:
1. 관련 코드를 읽고 변경 범위를 정한다.
2. 구현하고, 기존 테스트 방식에 맞춰 테스트를 추가·수정한다.
3. 테스트와 린트를 통과시킨다.
4. 의미 단위로 커밋한다. 커밋 메시지에 이슈 번호를 넣는다.
5. `gh pr create`로 PR을 연다. 본문에 무엇을 왜 바꿨는지 적는다.

멈추고 보고해야 하는 경우 — 계속 시도하지 말고 이유를 마지막 메시지에 남긴 뒤 종료하라:
- 테스트나 CI가 반복 실패하고 원인을 모르겠을 때
- 이슈가 요구하는 것이 코드베이스와 모순되거나 제품 동작을 바꿔야 할 때
- 데이터 손실, 비가역 마이그레이션, 보안, 비용에 영향이 있는 결정이 필요할 때
- 범위가 이슈보다 크게 커질 때
```

- [ ] **Step 1: 실패하는 테스트** — `TestRenderReplacesKnownTokens`, `TestRenderReplacesRepeatedToken`(`{{issue}}` 두 번), `TestRenderLeavesUnknownTokens`(`{{memory}}` 유지), `TestRenderLeavesMalformedTokensUntouched`(`{{issue`, `{{ issue }}`, `{{{issue}}}` → 입력과 바이트 동일, 단 `{{{issue}}}`는 안쪽 `{{issue}}`가 토큰이므로 `{`+값+`}`), `TestRenderDoesNotRescanValues`(값에 `{{issue}}`가 있어도 그대로), `TestIssueTextWithAndWithoutBody`, `TestDefaultTemplateMentionsIssueAndMemory`.
- [ ] **Step 2: RED 커밋** — `test(pipeline): 프롬프트 조립 실패 테스트`
- [ ] **Step 3: 구현** — `regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)`의 `ReplaceAllStringFunc`. 정규식 치환은 한 번 훑고 결과를 다시 훑지 않으므로 위 문법을 그대로 만족한다. `strings.ReplaceAll`을 토큰마다 반복하지 않는다(값 안의 토큰이 다음 반복에서 치환된다).
- [ ] **Step 4: 테스트 통과**
- [ ] **Step 5: GREEN 커밋** — `feat(pipeline): 템플릿 치환과 기본 실행 템플릿`

---

## Task 3: 프로토콜·러너 — run.start에 저장소, 허용 목록, `{{memory}}`

**Files:** `internal/protocol/envelope.go`, `internal/runner/spool/spool.go`, `internal/runner/lifecycle/{lifecycle,reconcile,repos}.go`, `cmd/taskyard-runner/main.go`, 테스트들

**Interfaces:**

```go
// protocol — 서버와 러너가 같은 타입을 쓴다. 지금은 러너 쪽 비공개 타입.
type RunStartBody struct {
	Prompt     string `json:"prompt"`
	RepoPath   string `json:"repo_path"`
	BaseBranch string `json:"base_branch"`
}

// spool
type RunRecord struct { …; RepoPath string }   // 컬럼 repo_path, ALTER로 추가

// lifecycle/repos.go
type RepoResolver struct{ … }
// NewRepoResolver는 허용 목록의 각 경로를 정규화해 보관한다. 정규화 =
// filepath.Abs → filepath.EvalSymlinks. 비절대 경로나 존재하지 않는 경로가
// 목록에 있으면 오류(기동 실패). worktree 루트는
// <worktreeRoot>/<base name>-<sha1(정규화 경로)[:8]> — 이름이 같은 저장소 둘이
// 충돌하지 않고, 경로에 슬래시가 들어가지 않는다.
func NewRepoResolver(allowed []string, worktreeRoot string) (*RepoResolver, error)
// Manager는 repoPath를 같은 방식으로 정규화한 뒤 목록과 비교한다. 정규화에
// 실패하거나(비절대, 부재) 목록에 없으면 ErrRepoNotAllowed. 같은 정규화
// 경로면 같은 *gitops.Manager 인스턴스를 돌려준다(lazy, mutex).
// macOS의 /tmp ↔ /private/tmp 처럼 심링크로 같은 디렉터리를 가리키는 두
// 표기가 같은 저장소로 풀려야 한다 — 테스트가 t.TempDir()를 쓰므로 실제로
// 걸리는 경우다.
func (r *RepoResolver) Manager(repoPath string) (*gitops.Manager, error)
// First는 허용 목록의 첫 저장소다. RepoPath가 비어 있는 옛 원장 기록에만 쓴다.
func (r *RepoResolver) First() *gitops.Manager

// lifecycle.Config: Git *gitops.Manager → Repos *RepoResolver. BaseBranch는 body.BaseBranch가 비었을 때의 기본값.
```

**동작 변경:**

- `handleRunStart`: body.RepoPath를 해석한다. `ErrRepoNotAllowed`면 RememberCommand로 관문을 통과시킨 뒤(재전송돼도 같은 결과) `failed` 상태 이벤트("repository not allowed: <path>")를 발행하고 원장에 failed(RepoPath 포함)로 남기고 `nil`을 돌려준다 — 명령 처리 오류가 아니라 Run의 실패다. 프로세스는 띄우지 않는다.
- **base branch:** `body.BaseBranch`가 비어 있지 않으면 그것, 비어 있으면 `cfg.BaseBranch`. `Ensure`에 그 값을 넘긴다.
- **`runHandle`에 `git *gitops.Manager`와 `repoPath string`을 둔다.** `execute`, `handleRunCancel`, `recordEarlyFailure`가 만드는 **모든 `RunRecord`에 `RepoPath`를 채운다** — 종결(succeeded/failed/cancelled), 조기 실패, 취소, PID 기록 전부. `salvage`는 `(runID, git *gitops.Manager)`를 받아 핸들의 관리자를 쓴다.
- `{{memory}}` 치환: `Ensure` 뒤에 `<ws.Path>/.taskyard/memory.md`를 읽어 prompt의 `{{memory}}`를 그 내용(없으면 빈 문자열)으로 바꾼다. 서버의 `pipeline.Render`와 같은 토큰 규칙(정규식 `\{\{memory\}\}`, 한 번 훑기)이지만 패키지 의존을 만들지 않기 위해 러너 쪽에 작은 함수(`replaceMemoryToken`)를 둔다.
- `Reconcile`: `RunRecord.RepoPath`로 `Repos.Manager`를 해석해 salvage한다. 해석 실패(허용 목록에서 빠진 저장소)는 로그를 남기고 salvage를 건너뛰되 상태 판정은 그대로 한다. RepoPath가 빈 옛 기록은 `Repos.First()`를 쓴다 — Phase 0 DB 호환.
- `cmd/taskyard-runner`: `--repo` 제거, `--allow-repo`(반복 가능, 최소 1개, `flag.Value` 구현) 추가. `--worktrees`, `--base-branch` 유지. `NewRepoResolver` 오류는 기동 실패(exit 2).

- [ ] **Step 1: 실패하는 테스트** — lifecycle: `TestRunStartUsesRepoFromBody`(허용 저장소 둘, body가 두 번째를 가리키면 그곳에 worktree), `TestRunStartRejectsRepoOutsideAllowList`(failed 이벤트, 원장 failed + RepoPath, 프로세스 미기동 — fake claude가 실행되면 파일을 남기게 해서 부재 확인), `TestRunStartUsesBodyBaseBranchWhenSet` / `TestRunStartFallsBackToConfigBaseBranch`, `TestMemoryTokenIsReplacedFromWorktreeFile`(fake claude가 받은 프롬프트를 파일로 남기게 해서 확인), `TestMemoryTokenBecomesEmptyWhenFileMissing`, `TestTerminalAndCancelRecordsCarryRepoPath`(성공·실패·취소 세 경로의 마지막 `RunRecord.RepoPath`가 채워짐); reconcile: `TestReconcileSalvagesUsingRecordedRepo`(허용 저장소 둘, 기록이 두 번째를 가리키면 그곳에 salvage 커밋), `TestReconcileUsesFirstRepoForLegacyRecord`; spool: `TestRunRecordRoundTripsRepoPath`, `TestSpoolOpenMigratesRepoPathColumn`; repos: `TestResolverReturnsSameManagerForSamePath`, `TestResolverResolvesSymlinkAlias`(`t.TempDir()` 안에 저장소를 만들고 `os.Symlink`로 별칭을 만들어 둘 다 같은 Manager), `TestResolverRejectsRelativeAndMissingPaths`, `TestNewResolverFailsOnBadAllowList`.
- [ ] **Step 2: RED 커밋** — `test(runner): run.start 저장소 지정·허용 목록·memory 치환 실패 테스트`
- [ ] **Step 3: 구현.** 기존 lifecycle/acceptance 테스트의 `Config{Git: …}`를 `Repos: NewRepoResolver([]string{repo}, wt)`로 바꾼다.
- [ ] **Step 4: `go test ./internal/runner/... ./acceptance/ -race -count=1`** — 인수 테스트는 아직 `POST /runs`를 쓰므로 body에 repo_path를 넣도록 최소 수정만 한다(Task 5에서 교체).
- [ ] **Step 5: GREEN 커밋** — `feat(runner): run.start의 저장소 지정과 --allow-repo 허용 목록`

---

## Task 4: web — 프로젝트·이슈 화면과 실행 트리거

**Files:** `internal/server/web/web.go`, `templates/{projects,project,issue,run}.html`, `web_test.go`. `templates/runs.html` 삭제.

**라우트:**

| 메서드·경로 | 동작 |
|---|---|
| `GET /{$}` | 프로젝트 목록 + 생성 폼 (`projects.html`) |
| `POST /projects` | key, name, repo_path, default_branch(기본 main). 템플릿은 `pipeline.DefaultExecuteTemplate`. 성공 → 303 `/projects/{key}` |
| `GET /projects/{key}` | 이슈 목록(번호·제목·상태), 이슈 생성 폼, 실행 템플릿 textarea (`project.html`) |
| `POST /projects/{key}/template` | execute_template 저장 → 303 |
| `POST /projects/{key}/issues` | title, body → 303 `/projects/{key}/issues/{n}` |
| `GET /projects/{key}/issues/{n}` | 제목·본문·상태, [실행] 버튼, Run 목록(상태·시각·링크) (`issue.html`) |
| `POST /projects/{key}/issues/{n}/run` | 아래 |
| `GET /runs/{id}` 등 | 유지. `run.html`에 "← 이슈 #n" 링크 (TaskID가 있을 때) |

`POST /runs`와 `runs.html`은 제거한다 — 척추의 문이 이슈로 옮겨갔다. 옛 경로는 의도적으로 **404**다. 호환 응답이나 리다이렉트를 두지 않는다 — Phase 0 스파이크 외에 클라이언트가 없다.

**same-origin 가드.** Global Constraints의 보안 노출 항목대로, `Routes()`가 모든 POST 핸들러를 `sameOrigin(h)` 미들웨어로 감싼다. 기존 `POST /runs/{id}/approve`도 포함한다(`run.html`의 `fetch`는 same-origin이라 통과한다).

**실행 트리거 순서** (Phase 0 `handleRunCreate`의 순서 불변식을 지킨다):

1. 프로젝트·이슈 로드. 없으면 404.
2. `prompt := pipeline.Render(project.ExecuteTemplate, {"issue": pipeline.IssueText(...)})`
3. `runID := "run-" + uuid`
4. `UpsertRun(Run{ID, State: queued, Kind: "structured", TaskID, Stage: "execute"})` — **명령 전송보다 먼저** (ApplyEvent의 ErrRunNotFound 회피)
5. `SendCommand(run.start, RunStartBody{Prompt, RepoPath: project.RepoPath, BaseBranch: project.DefaultBranch})`
6. 실패(`hub.ErrNoRunner`)면 Run을 failed로 바꾸고 503. 이슈 상태는 건드리지 않는다.
7. 성공이면 `UpdateTaskStatus(in_progress)` → 303 `/runs/{id}`

폼 검증: key는 `^[a-z0-9-]{1,32}$`, repo_path는 절대 경로, title 필수. 위반은 400과 한국어 메시지.

- [ ] **Step 1: 실패하는 테스트** — `TestIndexListsProjects`, `TestCreateProjectRedirectsAndRejectsBadKey`, `TestProjectPageListsIssuesNewestFirst`, `TestCreateIssueAssignsNumber`, `TestIssuePageShowsRunsWithState`, `TestRunIssueSendsRunStartWithRepoAndAssembledPrompt`(hub에 가짜 러너를 붙여 받은 봉투의 body를 `protocol.RunStartBody`로 디코드해 `Prompt`에 이슈 번호·제목·본문이, `RepoPath`·`BaseBranch`에 프로젝트 값이 들어 있는지 — 기존 web_test의 hub 사용 방식을 따른다), `TestRunIssueWithoutRunnerMarksRunFailedAndKeepsIssueBacklog`, `TestRunPageLinksBackToIssue`, `TestTemplatesEscapeHTML`(제목·본문·키·경로·템플릿에 `<script>`를 넣고 응답에 이스케이프된 형태로만 나타남), `TestCrossSitePostIsRejected`(`Sec-Fetch-Site: cross-site` → 403; `Origin`이 다른 host → 403; 헤더 없음 → 통과; same-origin → 통과. 네 라우트와 approve 전부).
- [ ] **Step 2: RED 커밋** — `test(web): 프로젝트·이슈·실행 트리거 실패 테스트`
- [ ] **Step 3: 구현.** 템플릿은 `layout.html`의 스타일을 재사용한다. 새 CSS는 폼 한 덩어리 이내.
- [ ] **Step 4: `go test ./internal/server/... -race -count=1`**
- [ ] **Step 5: GREEN 커밋** — `feat(web): 프로젝트·이슈 화면과 템플릿 기반 실행 트리거`

---

## Task 5: 인수 테스트와 바이너리 배선

**Files:** `acceptance/acceptance_test.go`, `cmd/taskyard-server/main.go`(변경 없을 수 있음), `README.md`, `docs/phase0-findings.md`

- `TestPostRunsTriggersAssembledRunAndYieldsDiff` → `TestIssueRunTriggersAssembledRunAndYieldsDiff`: 프로젝트 생성(POST, repo_path = 테스트 저장소; `t.TempDir()` 경로를 그대로 넣어 심링크 정규화가 실제로 작동함을 겸해 확인) → 이슈 생성 → `POST …/run` → Run 종결 대기 → 단언: (a) 러너가 받은 `run.start`가 `protocol.RunStartBody`로 디코드되고 `RepoPath`가 프로젝트 값, `Prompt`에 이슈 제목 포함, (b) worktree가 그 저장소의 루트 아래에 생김, (c) `store.GetRun`의 `State == succeeded`, (d) `GET /projects/{key}/issues/1`에 Run이 `succeeded`로, 이슈가 `review`로 보임, (e) `gitops.Diff`가 비어 있지 않음. fake claude는 기존대로 README에 한 줄을 덧붙인다.
- `TestIssueRunWithoutRunnerLeavesTaskInBacklog`: 러너를 붙이지 않고 `POST …/run` → 503, Run은 failed, 이슈는 `backlog`.
- `newStack`의 lifecycle Config를 `Repos`로. 기존 판정 1~4는 프롬프트를 직접 `run.start`로 보내던 곳이 있으면 `protocol.RunStartBody`로 바꾼다.
- README: 러너 플래그(`--allow-repo`가 `--repo`를 대체)와 화면 흐름 세 줄만. 그 밖의 README 수정은 하지 않는다.
- findings: "3. `store.Run.State`가 이벤트로 갱신되지 않는다" 항목에 "해소 — 척추 PR" 한 줄만. 다른 항목은 건드리지 않는다.

- [ ] **Step 1: 실패하는 인수 테스트** — RED 커밋 `test(acceptance): 이슈 경로 실행 트리거 실패 테스트`
- [ ] **Step 2: 구현·배선·문서**
- [ ] **Step 3: `go test ./... -race -count=1`, `make build`**
- [ ] **Step 4: GREEN 커밋** — `feat: 이슈에서 실행까지 척추 관통`

---

## Self-Review

**스펙 커버리지**

| PRD | Task |
|---|---|
| PM-01 프로젝트 생성(이름·키·저장소·기본 브랜치) | 1, 4 |
| PM-04 제목만으로 카드 | 1, 4 |
| PM-05 설명 편집 | 4 (본문 필드. Markdown 렌더링은 다음) |
| ST-01 실행 템플릿(단계 하나) | 1, 2, 4 |
| ST-02 수동 트리거 | 4 |
| ST-04 `{{issue}}`, `{{memory}}` | 2, 3 |
| ST-05 새 세션 | Phase 0 그대로 |
| RN-03 프로젝트별 허용 저장소 | 3 |
| findings 3 `runs.state` 갱신 | 1 |

**Codex 리뷰 반영 (2026-09-02).** 계획 초안을 Codex가 코드베이스와 대조해 major 7·minor 5를 냈고 전부 반영했다: 종결 상태 단조 규칙(Task 1), `PRAGMA table_info` 마이그레이션(Global), 저장소 경로 심링크 정규화와 `RepoPath` 전 기록 필수(Task 3), `l` 초기화 순서 명시(Global), same-origin 가드와 노출 수용 명시(Global, Task 4), 템플릿 토큰 문법(Task 2), base branch 폴백(Task 3), HTML 이스케이프·인수 테스트 단언 보강(Task 4, 5), `POST /runs` 404 명시(Task 4).

**알려진 간극**

- `{{memory}}`를 러너가 치환하므로 서버의 Run 상세에는 치환 전 프롬프트가 남는다. 프롬프트 자체를 이벤트로 남기지 않으므로 지금은 어디에도 최종 프롬프트가 기록되지 않는다 — 다음 PR에서 러너가 `run.state_changed(running)`의 detail이나 별도 이벤트로 최종 프롬프트를 올릴지 결정한다.
- `UpdateTaskStatus(in_progress)`가 SendCommand 뒤에 오므로, 그 사이 서버가 죽으면 Run은 queued인데 이슈는 backlog인 상태가 남는다. 다음 요청에서 사용자가 다시 [실행]을 누르면 새 Run이 생긴다. 허용한다.
- 러너가 `failed(repository not allowed)`를 발행하면 이슈는 `in_progress`에 머문다. 5종 종료 UI가 들어올 때 함께 다룬다.
- Phase 0의 `--repo` 플래그를 쓰던 스크립트는 깨진다. README에 적는다.
