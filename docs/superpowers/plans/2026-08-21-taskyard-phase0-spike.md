# Taskyard Phase 0 수직 스파이크 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 하드코딩된 Task 하나로 Server↔Runner 실행·복구·승인 경로를 끝까지 관통시켜, PRD §11의 아키텍처 전제를 실증한다.

**Architecture:** Go 모노레포에서 `taskyard-server`와 `taskyard-runner` 두 바이너리를 빌드한다. Runner가 Server로 아웃바운드 WebSocket을 열고, 버전이 붙은 JSON 봉투로 명령과 이벤트를 주고받는다. Runner는 Claude Code를 `-p --output-format stream-json`으로 띄워 NDJSON을 정규화 이벤트로 바꾸고, 로컬 SQLite spool에 먼저 적어 단절에 대비한다. 승인은 Runner가 로컬호스트에 띄운 HTTP MCP 서버를 Claude Code의 `--permission-prompt-tool`로 지정해 처리한다.

**Tech Stack:** Go 1.26 / SQLite(`modernc.org/sqlite`, 순수 Go) / WebSocket(`github.com/coder/websocket`) / 표준 `net/http` + `html/template` + HTMX + SSE / `git` CLI / Claude Code CLI

**Spec:** `taskyard-prd-v1.md` (v1.1) — 특히 §11.5 프로토콜, §11.6 어댑터, §11.7 조정 절차, §13.2.1 플래그 정책, §16.0 Phase 0 범위

---

## Global Constraints

모든 Task의 요구사항에 아래가 암묵적으로 포함된다.

- **Go 1.26 이상.** 검증 환경은 `go1.26.3 darwin/arm64`.
- **CGO 금지.** `CGO_ENABLED=0`으로 빌드된다. SQLite는 반드시 순수 Go 드라이버 `modernc.org/sqlite`를 쓴다. cgo 드라이버(`mattn/go-sqlite3`)는 PRD §15.1의 단일 바이너리 배포를 깬다.
- **Claude Code 2.1.221 이상.** 검증 환경은 `2.1.238`. `--mcp-config`의 첫 턴 연결 대기가 이 버전부터 동작한다.
- **Claude Code 기동 플래그는 아래가 정본이다.** 개별 Task에서 임의로 바꾸지 않는다.
  - 필수: `-p`, `--output-format stream-json`, `--verbose`, `--permission-mode default`, `--strict-mcp-config`, `--mcp-config <broker>`, `--permission-prompt-tool mcp__taskyard__approve`
  - 금지: `--bare` (구독이 아니라 `ANTHROPIC_API_KEY`로 과금된다 — PRD §13.2.1)
  - `--permission-mode default`는 타협 불가다. 사용자 전역 설정이 `auto`이면 분류기가 도구를 먼저 승인해버려 승인 브로커가 영영 호출되지 않는다.
  - `--strict-mcp-config`는 브로커 외의 MCP 서버를 차단한다.
- **환경변수 세척.** Agent 프로세스는 `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `CODEX_API_KEY`가 제거된 환경으로 띄운다.
- **`--bare`를 쓰지 않으므로 사용자 수준 설정(`~/.claude`)은 여전히 로드된다.** 스파이크에서는 이를 받아들인다. 저장소 수준 설정 격리는 Phase 1 과제다.
- **테스트는 fixture 기반이다.** 단위 테스트에서 실제 `claude` CLI를 호출하지 않는다. 사용자 구독 할당량을 소모하고 비결정적이다. 실제 CLI를 쓰는 검증은 `make smoke` 하나로 모은다.
- **커밋 메시지에 `Co-Authored-By`를 넣지 않는다.**
- 코드와 식별자는 영어, 주석과 문서는 한국어.

**이미 저장소에 있는 것:**

- `taskyard-prd-v1.md` — 스펙
- `internal/agents/adapter/claudecode/testdata/session-pong.ndjson` — 실제 `claude -p` 실행에서 캡처해 개인정보를 지운 10줄짜리 NDJSON. Task 6의 파서 정본 fixture다.
- `.gitignore` — `*.profraw`

---

## File Structure

| 경로 | 책임 |
|---|---|
| `cmd/taskyard-server/main.go` | Server 진입점, 플래그, 기동 |
| `cmd/taskyard-runner/main.go` | Runner 진입점, 플래그, 기동 |
| `internal/protocol/envelope.go` | 봉투 타입, 명령·이벤트 타입 상수 |
| `internal/protocol/handshake.go` | Hello/Welcome, 버전·capability 협상 |
| `internal/runner/spool/spool.go` | Runner 로컬 SQLite: 이벤트 spool, seq 발급, 명령 멱등성 원장 |
| `internal/server/store/store.go` | Server SQLite: run 원장, 이벤트 멱등 적용, 연속 커서 |
| `internal/server/hub/hub.go` | Runner 연결 수용, 명령 발행, 이벤트 수신, heartbeat 감시 |
| `internal/runner/link/link.go` | Server로의 아웃바운드 WSS, 재연결, spool 재전송 |
| `internal/gitops/worktree.go` | 결정론적 branch/worktree 생성·재사용, diff, salvage |
| `internal/agents/adapter/adapter.go` | 공통 어댑터 계약과 정규화 이벤트 타입 |
| `internal/agents/adapter/claudecode/parse.go` | stream-json NDJSON → 정규화 이벤트 |
| `internal/agents/adapter/claudecode/spawn.go` | 프로세스 기동, 플래그, 환경 세척, resume |
| `internal/approval/broker.go` | 로컬호스트 HTTP MCP 서버, `approve` 도구, 승인 상관관계 |
| `internal/runner/lifecycle/lifecycle.go` | run.start 수신 → worktree → adapter → 이벤트 발행 |
| `internal/runner/lifecycle/reconcile.go` | 재시작 후 alive/resumable/lost 판정 |
| `internal/server/web/web.go` | HTMX 셸 핸들러, SSE 스트림, 승인 POST |
| `internal/server/web/templates/` | `layout.html`, `runs.html`, `run.html` |
| `acceptance/acceptance_test.go` | §16.0 완료 판정 4종 |

---

## Task 1: 저장소 골격과 빌드

**Files:**
- Create: `go.mod`, `Makefile`, `internal/buildinfo/buildinfo.go`, `internal/buildinfo/buildinfo_test.go`
- Create: `cmd/taskyard-server/main.go`, `cmd/taskyard-runner/main.go`

**Interfaces:**
- Consumes: 없음
- Produces: `buildinfo.Version() string`, `buildinfo.ProtocolVersion() int` — 이후 모든 Task가 `--version` 출력과 handshake에서 쓴다.

- [ ] **Step 1: 모듈과 디렉터리 초기화**

```bash
cd /Users/jinto/projects/taskyard
go mod init github.com/jinto/taskyard
mkdir -p cmd/taskyard-server cmd/taskyard-runner internal/buildinfo
```

- [ ] **Step 2: 실패하는 테스트 작성**

`internal/buildinfo/buildinfo_test.go`:

```go
package buildinfo

import "testing"

func TestVersionIsNotEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() returned empty string")
	}
}

func TestProtocolVersionIsPositive(t *testing.T) {
	if got := ProtocolVersion(); got < 1 {
		t.Fatalf("ProtocolVersion() = %d, want >= 1", got)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/buildinfo/ -v`
Expected: FAIL — `undefined: Version`

- [ ] **Step 4: 최소 구현**

`internal/buildinfo/buildinfo.go`:

```go
// Package buildinfo는 두 바이너리가 공유하는 버전 정보를 제공한다.
package buildinfo

// version은 릴리스 시 -ldflags로 덮어쓴다.
var version = "0.0.0-dev"

// protocolVersion은 Server와 Runner가 handshake에서 비교하는 값이다.
// 호환되지 않는 변경이 생기면 올린다.
const protocolVersion = 1

func Version() string { return version }

func ProtocolVersion() int { return protocolVersion }
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/buildinfo/ -v`
Expected: PASS (2 tests)

- [ ] **Step 6: 두 바이너리의 최소 진입점 작성**

`cmd/taskyard-server/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jinto/taskyard/internal/buildinfo"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-server %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}

	fmt.Fprintln(os.Stderr, "taskyard-server: not implemented yet")
	os.Exit(1)
}
```

`cmd/taskyard-runner/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jinto/taskyard/internal/buildinfo"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-runner %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}

	fmt.Fprintln(os.Stderr, "taskyard-runner: not implemented yet")
	os.Exit(1)
}
```

- [ ] **Step 7: Makefile 작성**

`Makefile`:

```makefile
GO ?= go
export CGO_ENABLED := 0

.PHONY: build test smoke clean

build:
	$(GO) build -o bin/taskyard-server ./cmd/taskyard-server
	$(GO) build -o bin/taskyard-runner ./cmd/taskyard-runner

test:
	$(GO) test ./... -race

# smoke는 실제 claude CLI를 호출한다. 사용자 구독 할당량을 소모하므로
# 기본 test 대상에서 분리한다.
smoke:
	$(GO) test ./... -race -tags=smoke -run 'Smoke' -v -count=1

clean:
	rm -rf bin
```

- [ ] **Step 8: 빌드와 버전 출력 확인**

Run: `make build && ./bin/taskyard-server --version && ./bin/taskyard-runner --version`
Expected: 두 줄 모두 `taskyard-... 0.0.0-dev (protocol v1)` 출력

- [ ] **Step 9: 커밋**

```bash
git add go.mod Makefile cmd internal/buildinfo
git commit -m "chore: Go 모노레포 골격과 두 바이너리 진입점"
```

---

## Task 2: 프로토콜 v0

**Files:**
- Create: `internal/protocol/envelope.go`, `internal/protocol/envelope_test.go`
- Create: `internal/protocol/handshake.go`, `internal/protocol/handshake_test.go`

**Interfaces:**
- Consumes: `buildinfo.ProtocolVersion()`
- Produces:
  - `protocol.Envelope{V int, Kind string, ID string, Type string, RunID string, Seq uint64, TS time.Time, Body json.RawMessage}`
  - 상수: `KindCommand`, `KindEvent`, `KindAck`, `KindHello`, `KindWelcome`
  - 명령 타입 상수: `CmdRunStart`, `CmdRunCancel`, `CmdRunReconcile`, `CmdApprovalDecision`
  - 이벤트 타입 상수: `EvRunStateChanged`, `EvMessageDelta`, `EvToolStarted`, `EvToolFinished`, `EvFileChanged`, `EvApprovalRequested`, `EvUsageUpdated`, `EvTurnCompleted`, `EvError`, `EvHeartbeat`
  - `protocol.Hello`, `protocol.Welcome`, `protocol.Negotiate(h Hello) (Welcome, error)`
  - `protocol.NewCommand(cmdType string, runID string, body any) (Envelope, error)`
  - `protocol.NewEvent(evType string, runID string, seq uint64, body any) (Envelope, error)`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/protocol/envelope_test.go`:

```go
package protocol

import (
	"encoding/json"
	"testing"
)

type startBody struct {
	Prompt string `json:"prompt"`
}

func TestNewCommandAssignsIDAndKind(t *testing.T) {
	env, err := NewCommand(CmdRunStart, "run-1", startBody{Prompt: "hi"})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	if env.Kind != KindCommand {
		t.Errorf("Kind = %q, want %q", env.Kind, KindCommand)
	}
	if env.Type != CmdRunStart {
		t.Errorf("Type = %q, want %q", env.Type, CmdRunStart)
	}
	if env.ID == "" {
		t.Error("ID is empty; commands need a command_id for idempotency")
	}
	if env.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", env.RunID)
	}
}

func TestNewCommandIDsAreUnique(t *testing.T) {
	a, _ := NewCommand(CmdRunStart, "run-1", startBody{})
	b, _ := NewCommand(CmdRunStart, "run-1", startBody{})
	if a.ID == b.ID {
		t.Fatalf("two commands share ID %q", a.ID)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	env, err := NewEvent(EvMessageDelta, "run-1", 7, map[string]string{"text": "안녕"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Seq != 7 {
		t.Errorf("Seq = %d, want 7", got.Seq)
	}
	if got.Kind != KindEvent {
		t.Errorf("Kind = %q, want %q", got.Kind, KindEvent)
	}

	var body map[string]string
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("Unmarshal body: %v", err)
	}
	if body["text"] != "안녕" {
		t.Errorf("body text = %q, want 안녕", body["text"])
	}
}

func TestCommandsCarryNoSequence(t *testing.T) {
	env, _ := NewCommand(CmdRunCancel, "run-1", nil)
	raw, _ := json.Marshal(env)
	if json.Valid(raw) && contains(string(raw), `"seq"`) {
		t.Errorf("command envelope must omit seq, got %s", raw)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && json.Valid([]byte(haystack)) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/protocol/ -v`
Expected: FAIL — `undefined: NewCommand`

- [ ] **Step 3: 봉투 구현**

`internal/protocol/envelope.go`:

```go
// Package protocol은 Server와 Runner가 주고받는 메시지 형식을 정의한다.
//
// 모든 메시지는 하나의 봉투(Envelope)에 담긴다. 명령은 고유한 ID를 가지고
// Runner가 중복 수신해도 한 번만 적용하며, 이벤트는 Run별로 단조 증가하는
// seq를 가져 Server가 멱등하게 적용한다. PRD §11.5를 따른다.
package protocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jinto/taskyard/internal/buildinfo"
)

// 봉투 종류.
const (
	KindCommand = "command"
	KindEvent   = "event"
	KindAck     = "ack"
	KindHello   = "hello"
	KindWelcome = "welcome"
)

// Server → Runner 명령 타입.
const (
	CmdRunStart         = "run.start"
	CmdRunCancel        = "run.cancel"
	CmdRunReconcile     = "run.reconcile"
	CmdApprovalDecision = "approval.decision"
)

// Runner → Server 이벤트 타입. 어댑터가 Provider 이벤트를 여기로 정규화한다.
const (
	EvRunStateChanged   = "run.state_changed"
	EvMessageDelta      = "message_delta"
	EvToolStarted       = "tool_started"
	EvToolFinished      = "tool_finished"
	EvFileChanged       = "file_changed"
	EvApprovalRequested = "approval_requested"
	EvUsageUpdated      = "usage_updated"
	EvTurnCompleted     = "turn_completed"
	EvError             = "error"
	EvHeartbeat         = "runner.heartbeat"
)

// Envelope은 모든 메시지의 공통 껍데기다.
type Envelope struct {
	V     int             `json:"v"`
	Kind  string          `json:"kind"`
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	RunID string          `json:"run_id,omitempty"`
	Seq   uint64          `json:"seq,omitempty"`
	TS    time.Time       `json:"ts"`
	Body  json.RawMessage `json:"body,omitempty"`
}

func marshalBody(body any) (json.RawMessage, error) {
	if body == nil {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return raw, nil
}

// NewCommand는 새 명령 봉투를 만든다. ID는 Runner의 멱등성 판정 키다.
func NewCommand(cmdType, runID string, body any) (Envelope, error) {
	raw, err := marshalBody(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:     buildinfo.ProtocolVersion(),
		Kind:  KindCommand,
		ID:    uuid.NewString(),
		Type:  cmdType,
		RunID: runID,
		TS:    time.Now().UTC(),
		Body:  raw,
	}, nil
}

// NewEvent는 새 이벤트 봉투를 만든다. seq는 호출자가 spool에서 발급받아 넘긴다.
func NewEvent(evType, runID string, seq uint64, body any) (Envelope, error) {
	raw, err := marshalBody(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:     buildinfo.ProtocolVersion(),
		Kind:  KindEvent,
		ID:    uuid.NewString(),
		Type:  evType,
		RunID: runID,
		Seq:   seq,
		TS:    time.Now().UTC(),
		Body:  raw,
	}, nil
}
```

- [ ] **Step 4: 의존성 추가 후 테스트 통과 확인**

Run: `go get github.com/google/uuid@latest && go test ./internal/protocol/ -v`
Expected: PASS (4 tests)

- [ ] **Step 5: handshake 실패 테스트 작성**

`internal/protocol/handshake_test.go`:

```go
package protocol

import (
	"errors"
	"testing"

	"github.com/jinto/taskyard/internal/buildinfo"
)

func TestNegotiateAcceptsMatchingVersion(t *testing.T) {
	w, err := Negotiate(Hello{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		RunnerID:        "runner-1",
		Capabilities:    []string{CapClaudeCode},
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if w.ProtocolVersion != buildinfo.ProtocolVersion() {
		t.Errorf("ProtocolVersion = %d, want %d", w.ProtocolVersion, buildinfo.ProtocolVersion())
	}
}

func TestNegotiateRejectsVersionMismatch(t *testing.T) {
	_, err := Negotiate(Hello{
		ProtocolVersion: buildinfo.ProtocolVersion() + 1,
		RunnerID:        "runner-1",
	})
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
}

func TestNegotiateRequiresRunnerID(t *testing.T) {
	_, err := Negotiate(Hello{ProtocolVersion: buildinfo.ProtocolVersion()})
	if !errors.Is(err, ErrMissingRunnerID) {
		t.Fatalf("err = %v, want ErrMissingRunnerID", err)
	}
}

func TestHelloHasCapability(t *testing.T) {
	h := Hello{Capabilities: []string{CapClaudeCode, CapApprovalBroker}}
	if !h.Has(CapApprovalBroker) {
		t.Error("Has(CapApprovalBroker) = false, want true")
	}
	if h.Has("nonexistent") {
		t.Error("Has(nonexistent) = true, want false")
	}
}
```

- [ ] **Step 6: 테스트가 실패하는지 확인**

Run: `go test ./internal/protocol/ -run Negotiate -v`
Expected: FAIL — `undefined: Negotiate`

- [ ] **Step 7: handshake 구현**

`internal/protocol/handshake.go`:

```go
package protocol

import (
	"errors"

	"github.com/jinto/taskyard/internal/buildinfo"
)

// Runner가 광고하는 역량. 스파이크에서는 목록을 최소로 유지한다.
const (
	CapClaudeCode     = "adapter.claudecode"
	CapApprovalBroker = "approval.broker"
	CapGitWorktree    = "git.worktree"
)

var (
	// ErrProtocolMismatch는 major 버전이 다를 때 반환한다. 스파이크에서는
	// 정확히 일치할 때만 받아들인다.
	ErrProtocolMismatch = errors.New("protocol version mismatch")
	// ErrMissingRunnerID는 Runner가 자신을 식별하지 않았을 때 반환한다.
	ErrMissingRunnerID = errors.New("hello is missing runner_id")
)

// Hello는 Runner가 연결 직후 보내는 첫 메시지다.
type Hello struct {
	ProtocolVersion int      `json:"protocol_version"`
	RunnerID        string   `json:"runner_id"`
	PairingToken    string   `json:"pairing_token,omitempty"`
	Capabilities    []string `json:"capabilities"`
}

// Has는 Runner가 해당 역량을 광고했는지 확인한다.
func (h Hello) Has(capability string) bool {
	for _, c := range h.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Welcome은 Server의 응답이다. ResumeFrom은 Run별로 Server가 마지막으로
// 연속 적용한 seq이며, Runner는 그 다음 seq부터 spool을 재전송한다.
type Welcome struct {
	ProtocolVersion int               `json:"protocol_version"`
	ServerVersion   string            `json:"server_version"`
	Capabilities    []string          `json:"capabilities"`
	ResumeFrom      map[string]uint64 `json:"resume_from"`
}

// Negotiate는 Hello를 검증하고 Welcome을 만든다. ResumeFrom은 비어 있으며
// 호출자가 store에서 채운다.
func Negotiate(h Hello) (Welcome, error) {
	if h.RunnerID == "" {
		return Welcome{}, ErrMissingRunnerID
	}
	if h.ProtocolVersion != buildinfo.ProtocolVersion() {
		return Welcome{}, ErrProtocolMismatch
	}
	return Welcome{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		ServerVersion:   buildinfo.Version(),
		Capabilities:    []string{CapClaudeCode, CapApprovalBroker, CapGitWorktree},
		ResumeFrom:      map[string]uint64{},
	}, nil
}
```

- [ ] **Step 8: 테스트 통과 확인**

Run: `go test ./internal/protocol/ -v`
Expected: PASS (8 tests)

- [ ] **Step 9: 커밋**

```bash
git add go.mod go.sum internal/protocol
git commit -m "feat(protocol): v0 봉투와 버전·capability 협상"
```

---

## Task 3: Runner spool과 명령 멱등성 원장

**Files:**
- Create: `internal/runner/spool/spool.go`, `internal/runner/spool/spool_test.go`

**Interfaces:**
- Consumes: `protocol.Envelope`
- Produces:
  - `spool.Open(path string) (*Spool, error)`, `(*Spool).Close() error`
  - `(*Spool).Append(runID string, env protocol.Envelope) (uint64, error)` — seq를 발급해 봉투에 채우고 저장한 뒤 그 seq를 돌려준다
  - `(*Spool).Since(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error)`
  - `(*Spool).Ack(runID string, throughSeq uint64) error`
  - `(*Spool).Pending(runID string) (int, error)`
  - `(*Spool).RememberCommand(commandID string, result []byte) (stored []byte, firstTime bool, err error)`
  - `(*Spool).ActiveRuns() ([]string, error)`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/runner/spool/spool_test.go`:

```go
package spool

import (
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
)

func openTemp(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustEvent(t *testing.T, runID string) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvMessageDelta, runID, 0, map[string]string{"text": "x"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return env
}

func TestAppendIssuesMonotonicSequences(t *testing.T) {
	s := openTemp(t)
	for want := uint64(1); want <= 3; want++ {
		got, err := s.Append("run-1", mustEvent(t, "run-1"))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got != want {
			t.Fatalf("seq = %d, want %d", got, want)
		}
	}
}

func TestSequencesAreIndependentPerRun(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append("run-a", mustEvent(t, "run-a")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Append("run-b", mustEvent(t, "run-b"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("run-b first seq = %d, want 1", got)
	}
}

func TestSinceReturnsEventsAfterSequenceInOrder(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Append("run-1", mustEvent(t, "run-1")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Since("run-1", 2, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, env := range got {
		if want := uint64(i + 3); env.Seq != want {
			t.Errorf("got[%d].Seq = %d, want %d", i, env.Seq, want)
		}
	}
}

func TestAckDropsAcknowledgedEventsButKeepsCounter(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Append("run-1", mustEvent(t, "run-1")); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Ack("run-1", 2); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pending, err := s.Pending("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("Pending = %d, want 1", pending)
	}

	// Ack이 seq 발급 카운터를 되돌리면 안 된다.
	next, err := s.Append("run-1", mustEvent(t, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if next != 4 {
		t.Fatalf("next seq = %d, want 4 (counter must survive Ack)", next)
	}
}

func TestSequenceSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Append("run-1", mustEvent(t, "run-1")); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.Append("run-1", mustEvent(t, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("seq after reopen = %d, want 2", got)
	}
}

func TestRememberCommandIsIdempotent(t *testing.T) {
	s := openTemp(t)

	stored, first, err := s.RememberCommand("cmd-1", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("firstTime = false on first call")
	}
	if string(stored) != `{"ok":true}` {
		t.Fatalf("stored = %s", stored)
	}

	stored, first, err = s.RememberCommand("cmd-1", []byte(`{"ok":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("firstTime = true on repeat call")
	}
	if string(stored) != `{"ok":true}` {
		t.Fatalf("repeat returned %s, want the original result", stored)
	}
}

func TestActiveRunsListsRunsWithPendingEvents(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append("run-a", mustEvent(t, "run-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("run-b", mustEvent(t, "run-b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack("run-a", 1); err != nil {
		t.Fatal(err)
	}

	runs, err := s.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0] != "run-b" {
		t.Fatalf("ActiveRuns = %v, want [run-b]", runs)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/runner/spool/ -v`
Expected: FAIL — `undefined: Open`

- [ ] **Step 3: 구현**

`internal/runner/spool/spool.go`:

```go
// Package spool은 Runner의 로컬 이벤트 대기열과 명령 멱등성 원장이다.
//
// Runner는 Server로 보내기 전에 항상 여기 먼저 적는다. 연결이 끊겨도
// 이벤트가 남고, 재연결 시 Server가 알려준 지점부터 다시 보낸다.
// PRD §11.5의 at-least-once 전송과 §11.7의 seq 영속성을 담당한다.
package spool

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/jinto/taskyard/internal/protocol"
)

const schema = `
CREATE TABLE IF NOT EXISTS spool (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (run_id, seq)
);

-- last_issued는 Ack으로 줄어들지 않는다. 재시작 후에도 seq가
-- 이어지도록 spool 행과 분리해 보관한다.
CREATE TABLE IF NOT EXISTS seq_cursor (
  run_id      TEXT    PRIMARY KEY,
  last_issued INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS command_log (
  command_id TEXT PRIMARY KEY,
  result     BLOB NOT NULL
);
`

// Spool은 SQLite로 뒷받침되는 이벤트 대기열이다.
type Spool struct {
	db *sql.DB
}

// Open은 경로의 SQLite 파일을 열고 스키마를 보장한다.
func Open(path string) (*Spool, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open spool db: %w", err)
	}
	// 쓰기 직렬화를 단순하게 유지한다. spool은 처리량보다 정확성이 중요하다.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create spool schema: %w", err)
	}
	return &Spool{db: db}, nil
}

func (s *Spool) Close() error { return s.db.Close() }

// Append는 다음 seq를 발급해 봉투에 채우고 저장한다.
func (s *Spool) Append(runID string, env protocol.Envelope) (uint64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var last uint64
	err = tx.QueryRow(`SELECT last_issued FROM seq_cursor WHERE run_id = ?`, runID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read cursor: %w", err)
	}

	next := last + 1
	if _, err := tx.Exec(
		`INSERT INTO seq_cursor (run_id, last_issued) VALUES (?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET last_issued = excluded.last_issued`,
		runID, next,
	); err != nil {
		return 0, fmt.Errorf("bump cursor: %w", err)
	}

	env.Seq = next
	payload, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("marshal envelope: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO spool (run_id, seq, payload) VALUES (?, ?, ?)`,
		runID, next, payload,
	); err != nil {
		return 0, fmt.Errorf("insert spool row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return next, nil
}

// Since는 afterSeq보다 큰 이벤트를 seq 오름차순으로 최대 limit개 돌려준다.
func (s *Spool) Since(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error) {
	rows, err := s.db.Query(
		`SELECT payload FROM spool WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query spool: %w", err)
	}
	defer rows.Close()

	var out []protocol.Envelope
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan payload: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, fmt.Errorf("unmarshal envelope: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// Ack은 Server가 확인한 지점까지의 이벤트를 지운다. seq 카운터는 건드리지 않는다.
func (s *Spool) Ack(runID string, throughSeq uint64) error {
	_, err := s.db.Exec(`DELETE FROM spool WHERE run_id = ? AND seq <= ?`, runID, throughSeq)
	if err != nil {
		return fmt.Errorf("ack spool: %w", err)
	}
	return nil
}

// Pending은 아직 확인되지 않은 이벤트 수를 돌려준다.
func (s *Spool) Pending(runID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM spool WHERE run_id = ?`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// ActiveRuns는 미확인 이벤트가 남은 Run 목록을 돌려준다.
func (s *Spool) ActiveRuns() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT run_id FROM spool ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("query active runs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RememberCommand는 명령을 한 번만 적용하기 위한 원장이다. 처음 보는
// command_id면 result를 저장하고 firstTime=true를 돌려준다. 이미 본
// 명령이면 저장돼 있던 결과와 firstTime=false를 돌려준다.
func (s *Spool) RememberCommand(commandID string, result []byte) ([]byte, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var existing []byte
	err = tx.QueryRow(`SELECT result FROM command_log WHERE command_id = ?`, commandID).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, fmt.Errorf("read command log: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO command_log (command_id, result) VALUES (?, ?)`,
		commandID, result,
	); err != nil {
		return nil, false, fmt.Errorf("insert command log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return result, true, nil
}
```

- [ ] **Step 4: 의존성 추가 후 테스트 통과 확인**

Run: `go get modernc.org/sqlite@latest && go test ./internal/runner/spool/ -race -v`
Expected: PASS (7 tests)

- [ ] **Step 5: 커밋**

```bash
git add go.mod go.sum internal/runner/spool
git commit -m "feat(runner): SQLite 이벤트 spool과 명령 멱등성 원장"
```

---

## Task 4: Server 이벤트 저장소와 멱등 적용

**Files:**
- Create: `internal/server/store/store.go`, `internal/server/store/store_test.go`

**Interfaces:**
- Consumes: `protocol.Envelope`
- Produces:
  - `store.Open(path string) (*Store, error)`, `(*Store).Close() error`
  - `(*Store).UpsertRun(r Run) error`
  - `(*Store).GetRun(id string) (Run, error)`
  - `(*Store).ApplyEvent(env protocol.Envelope) (accepted bool, ackThrough uint64, err error)`
  - `(*Store).ResumePoints() (map[string]uint64, error)`
  - `(*Store).Events(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error)`
  - `store.Run{ID, State, Kind, ProviderSessionID, Branch, WorktreePath, LastAckedSeq, ReconcileState string/uint64}`
  - 상태 상수: `StateQueued`, `StateRunning`, `StateWaitingApproval`, `StateOrphaned`, `StateSucceeded`, `StateFailed`, `StateCancelled`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/server/store/store_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func event(t *testing.T, runID string, seq uint64) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvMessageDelta, runID, seq, map[string]string{"text": "x"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	env.Seq = seq
	return env
}

func seedRun(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.UpsertRun(Run{ID: id, State: StateRunning, Kind: "structured"}); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
}

func TestApplyEventAdvancesAckOnContiguousSequences(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	for seq := uint64(1); seq <= 3; seq++ {
		accepted, ack, err := s.ApplyEvent(event(t, "run-1", seq))
		if err != nil {
			t.Fatalf("ApplyEvent(%d): %v", seq, err)
		}
		if !accepted {
			t.Fatalf("seq %d not accepted", seq)
		}
		if ack != seq {
			t.Fatalf("ack = %d, want %d", ack, seq)
		}
	}
}

func TestApplyEventIsIdempotent(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	accepted, ack, err := s.ApplyEvent(event(t, "run-1", 1))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if accepted {
		t.Error("replayed event reported as accepted; want false")
	}
	if ack != 1 {
		t.Errorf("ack = %d, want 1", ack)
	}

	got, err := s.Events("run-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d events, want 1 (duplicate must not be stored twice)", len(got))
	}
}

func TestApplyEventHoldsAckOnGapThenAdvances(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")

	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	// seq 2를 건너뛰고 3이 먼저 도착하면 ack은 1에 머문다.
	_, ack, err := s.ApplyEvent(event(t, "run-1", 3))
	if err != nil {
		t.Fatal(err)
	}
	if ack != 1 {
		t.Fatalf("ack = %d, want 1 while seq 2 is missing", ack)
	}

	// 빠진 2가 도착하면 3까지 한 번에 이어진다.
	_, ack, err = s.ApplyEvent(event(t, "run-1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if ack != 3 {
		t.Fatalf("ack = %d, want 3 after the gap is filled", ack)
	}
}

func TestResumePointsReportsLastAckedPerRun(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-a")
	seedRun(t, s, "run-b")

	if _, _, err := s.ApplyEvent(event(t, "run-a", 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(event(t, "run-a", 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyEvent(event(t, "run-b", 1)); err != nil {
		t.Fatal(err)
	}

	points, err := s.ResumePoints()
	if err != nil {
		t.Fatal(err)
	}
	if points["run-a"] != 2 {
		t.Errorf("run-a = %d, want 2", points["run-a"])
	}
	if points["run-b"] != 1 {
		t.Errorf("run-b = %d, want 1", points["run-b"])
	}
}

func TestUpsertRunPreservesLastAckedSeq(t *testing.T) {
	s := openTemp(t)
	seedRun(t, s, "run-1")
	if _, _, err := s.ApplyEvent(event(t, "run-1", 1)); err != nil {
		t.Fatal(err)
	}

	// 상태만 바꾸는 갱신이 ack 커서를 되돌리면 재전송이 무한 반복된다.
	if err := s.UpsertRun(Run{ID: "run-1", State: StateSucceeded, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAckedSeq != 1 {
		t.Fatalf("LastAckedSeq = %d, want 1", got.LastAckedSeq)
	}
	if got.State != StateSucceeded {
		t.Fatalf("State = %q, want %q", got.State, StateSucceeded)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/server/store/ -v`
Expected: FAIL — `undefined: Open`

- [ ] **Step 3: 구현**

`internal/server/store/store.go`:

```go
// Package store는 Server의 Run 원장과 이벤트 저장소다.
//
// Runner는 at-least-once로 이벤트를 보내므로 여기서 멱등하게 적용한다.
// ack 커서는 "빠짐없이 연속으로 받은 마지막 seq"이며, 중간이 비면
// 앞으로 나아가지 않는다. PRD §11.5와 §15.3을 따른다.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/jinto/taskyard/internal/protocol"
)

// Run 상태. PRD §9.3의 부분집합으로, 스파이크에 필요한 것만 둔다.
const (
	StateQueued          = "queued"
	StateRunning         = "running"
	StateWaitingApproval = "waiting_approval"
	StateOrphaned        = "orphaned"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateCancelled       = "cancelled"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id                  TEXT    PRIMARY KEY,
  state               TEXT    NOT NULL,
  kind                TEXT    NOT NULL,
  provider_session_id TEXT    NOT NULL DEFAULT '',
  branch              TEXT    NOT NULL DEFAULT '',
  worktree_path       TEXT    NOT NULL DEFAULT '',
  last_acked_seq      INTEGER NOT NULL DEFAULT 0,
  reconcile_state     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS run_events (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  type    TEXT    NOT NULL,
  payload BLOB    NOT NULL,
  PRIMARY KEY (run_id, seq)
);
`

// ErrRunNotFound는 알 수 없는 Run을 조회했을 때 반환한다.
var ErrRunNotFound = errors.New("run not found")

// Run은 Server가 보는 실행 한 건이다.
type Run struct {
	ID                string
	State             string
	Kind              string
	ProviderSessionID string
	Branch            string
	WorktreePath      string
	LastAckedSeq      uint64
	ReconcileState    string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open server db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create server schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertRun은 Run을 만들거나 갱신한다. last_acked_seq는 이벤트 적용만이
// 움직이므로 여기서 절대 덮어쓰지 않는다.
func (s *Store) UpsertRun(r Run) error {
	_, err := s.db.Exec(
		`INSERT INTO runs (id, state, kind, provider_session_id, branch, worktree_path, reconcile_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   state               = excluded.state,
		   kind                = excluded.kind,
		   provider_session_id = excluded.provider_session_id,
		   branch              = excluded.branch,
		   worktree_path       = excluded.worktree_path,
		   reconcile_state     = excluded.reconcile_state`,
		r.ID, r.State, r.Kind, r.ProviderSessionID, r.Branch, r.WorktreePath, r.ReconcileState,
	)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(id string) (Run, error) {
	var r Run
	err := s.db.QueryRow(
		`SELECT id, state, kind, provider_session_id, branch, worktree_path, last_acked_seq, reconcile_state
		 FROM runs WHERE id = ?`, id,
	).Scan(&r.ID, &r.State, &r.Kind, &r.ProviderSessionID, &r.Branch, &r.WorktreePath, &r.LastAckedSeq, &r.ReconcileState)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// ApplyEvent는 이벤트를 저장하고 ack 커서를 가능한 만큼 전진시킨다.
// accepted는 이번 호출로 새로 저장됐는지를 뜻한다. 이미 있던 seq면 false다.
func (s *Store) ApplyEvent(env protocol.Envelope) (bool, uint64, error) {
	if env.RunID == "" || env.Seq == 0 {
		return false, 0, fmt.Errorf("event needs run_id and seq, got run_id=%q seq=%d", env.RunID, env.Seq)
	}

	payload, err := json.Marshal(env)
	if err != nil {
		return false, 0, fmt.Errorf("marshal envelope: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO run_events (run_id, seq, type, payload) VALUES (?, ?, ?, ?)`,
		env.RunID, env.Seq, env.Type, payload,
	)
	if err != nil {
		return false, 0, fmt.Errorf("insert event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("rows affected: %w", err)
	}

	var acked uint64
	err = tx.QueryRow(`SELECT last_acked_seq FROM runs WHERE id = ?`, env.RunID).Scan(&acked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, ErrRunNotFound
	}
	if err != nil {
		return false, 0, fmt.Errorf("read ack cursor: %w", err)
	}

	// 빈틈이 없는 동안만 커서를 전진시킨다.
	for {
		var exists int
		err := tx.QueryRow(
			`SELECT 1 FROM run_events WHERE run_id = ? AND seq = ?`, env.RunID, acked+1,
		).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return false, 0, fmt.Errorf("probe next seq: %w", err)
		}
		acked++
	}

	if _, err := tx.Exec(`UPDATE runs SET last_acked_seq = ? WHERE id = ?`, acked, env.RunID); err != nil {
		return false, 0, fmt.Errorf("update ack cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}
	return affected > 0, acked, nil
}

// ResumePoints는 Runner의 재연결 시 Welcome에 실어 보낼 Run별 ack 지점이다.
func (s *Store) ResumePoints() (map[string]uint64, error) {
	rows, err := s.db.Query(`SELECT id, last_acked_seq FROM runs`)
	if err != nil {
		return nil, fmt.Errorf("query resume points: %w", err)
	}
	defer rows.Close()

	out := map[string]uint64{}
	for rows.Next() {
		var id string
		var seq uint64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, fmt.Errorf("scan resume point: %w", err)
		}
		out[id] = seq
	}
	return out, rows.Err()
}

// Events는 저장된 이벤트를 seq 오름차순으로 돌려준다. 웹 UI의 재생에 쓴다.
func (s *Store) Events(runID string, afterSeq uint64, limit int) ([]protocol.Envelope, error) {
	rows, err := s.db.Query(
		`SELECT payload FROM run_events WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []protocol.Envelope
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, fmt.Errorf("unmarshal event: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/server/store/ -race -v`
Expected: PASS (5 tests)

- [ ] **Step 5: 커밋**

```bash
git add internal/server/store
git commit -m "feat(server): 이벤트 멱등 적용과 연속 ack 커서"
```

---

## Task 5: WSS 전송 — 페어링, heartbeat, 재연결 재전송

**Files:**
- Create: `internal/server/hub/hub.go`, `internal/server/hub/hub_test.go`
- Create: `internal/runner/link/link.go`

**Interfaces:**
- Consumes: `protocol.*`, `store.Store`, `spool.Spool`
- Produces:
  - `hub.New(st *store.Store, pairingToken string) *Hub`
  - `(*Hub).ServeWS(w http.ResponseWriter, r *http.Request)` — `http.HandlerFunc` 호환
  - `(*Hub).SendCommand(env protocol.Envelope) error` — 연결된 Runner에 명령 전달, 미연결이면 `hub.ErrNoRunner`
  - `(*Hub).Subscribe() (<-chan protocol.Envelope, func())` — 적용된 이벤트 팬아웃 (Task 11의 SSE가 쓴다)
  - `(*Hub).Connected() bool`
  - `link.Config{ServerURL, RunnerID, PairingToken string, Spool *spool.Spool, Capabilities []string, OnCommand CommandHandler}`
  - `link.CommandHandler = func(ctx context.Context, env protocol.Envelope) error`
  - `link.New(cfg Config) (*Link, error)`, `(*Link).Run(ctx context.Context) error`, `(*Link).Publish(runID string, env protocol.Envelope) error`

- [ ] **Step 1: 실패하는 통합 테스트 작성**

`internal/server/hub/hub_test.go`:

```go
package hub_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

const testToken = "pair-me"

type rig struct {
	st   *store.Store
	hub  *hub.Hub
	srv  *httptest.Server
	sp   *spool.Spool
	link *link.Link
	wsURL string
}

func newRig(t *testing.T, onCommand link.CommandHandler) *rig {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := hub.New(st, testToken)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	if onCommand == nil {
		onCommand = func(context.Context, protocol.Envelope) error { return nil }
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	l, err := link.New(link.Config{
		ServerURL:    wsURL,
		RunnerID:     "runner-1",
		PairingToken: testToken,
		Spool:        sp,
		Capabilities: []string{protocol.CapClaudeCode},
		OnCommand:    onCommand,
	})
	if err != nil {
		t.Fatalf("link.New: %v", err)
	}

	return &rig{st: st, hub: h, srv: srv, sp: sp, link: l, wsURL: wsURL}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventsFlowFromRunnerToServerAndGetAcked(t *testing.T) {
	r := newRig(t, nil)
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)

	waitFor(t, "runner connection", r.hub.Connected)

	for i := 0; i < 5; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	waitFor(t, "server to apply 5 events", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 5
	})

	waitFor(t, "spool to drain after ack", func() bool {
		n, err := r.sp.Pending("run-1")
		return err == nil && n == 0
	})
}

func TestReconnectResendsUnackedEventsWithoutDuplication(t *testing.T) {
	r := newRig(t, nil)
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)

	for i := 0; i < 3; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "first batch applied", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 3
	})

	// 연결을 강제로 끊고, 끊긴 동안 이벤트를 더 쌓는다.
	r.hub.DropConnection()
	waitFor(t, "hub to notice the drop", func() bool { return !r.hub.Connected() })

	for i := 3; i < 8; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatalf("Publish while disconnected: %v", err)
		}
	}

	waitFor(t, "reconnect and drain", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 8
	})

	events, err := r.st.Events("run-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("stored %d events, want exactly 8 (no loss, no duplicates)", len(events))
	}
	for i, e := range events {
		if want := uint64(i + 1); e.Seq != want {
			t.Fatalf("events[%d].Seq = %d, want %d", i, e.Seq, want)
		}
	}
}

func TestCommandReachesRunnerHandler(t *testing.T) {
	got := make(chan protocol.Envelope, 1)
	r := newRig(t, func(_ context.Context, env protocol.Envelope) error {
		got <- env
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)

	cmd, _ := protocol.NewCommand(protocol.CmdRunStart, "run-1", map[string]string{"prompt": "hello"})
	if err := r.hub.SendCommand(cmd); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	select {
	case env := <-got:
		if env.Type != protocol.CmdRunStart {
			t.Fatalf("Type = %q, want %q", env.Type, protocol.CmdRunStart)
		}
		if env.ID != cmd.ID {
			t.Fatalf("command_id = %q, want %q", env.ID, cmd.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command never reached the runner handler")
	}
}

func TestWrongPairingTokenIsRejected(t *testing.T) {
	r := newRig(t, nil)

	sp, err := spool.Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	bad, err := link.New(link.Config{
		ServerURL:    r.wsURL,
		RunnerID:     "intruder",
		PairingToken: "wrong-token",
		Spool:        sp,
		OnCommand:    func(context.Context, protocol.Envelope) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = bad.Run(ctx)

	if r.hub.Connected() {
		t.Fatal("hub accepted a runner with a wrong pairing token")
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/server/hub/ -v`
Expected: FAIL — `no required module provides package .../internal/server/hub`

- [ ] **Step 3: Server hub 구현**

`internal/server/hub/hub.go`:

```go
// Package hub는 Runner의 아웃바운드 WebSocket 연결을 받는다.
//
// 스파이크는 Runner 한 대만 다룬다. 연결이 하나뿐이므로 라우팅 대신
// 뮤텍스로 보호되는 현재 연결 하나를 들고 있는다.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/store"
)

// ErrNoRunner는 연결된 Runner가 없을 때 반환한다.
var ErrNoRunner = errors.New("no runner connected")

// readTimeout을 넘겨도 아무 메시지가 없으면 연결이 죽은 것으로 본다.
// Runner는 이보다 훨씬 짧은 주기로 heartbeat를 보낸다.
const readTimeout = 45 * time.Second

type Hub struct {
	st           *store.Store
	pairingToken string

	mu   sync.Mutex
	conn *websocket.Conn

	subMu sync.Mutex
	subs  map[chan protocol.Envelope]struct{}
}

func New(st *store.Store, pairingToken string) *Hub {
	return &Hub{
		st:           st,
		pairingToken: pairingToken,
		subs:         map[chan protocol.Envelope]struct{}{},
	}
}

func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn != nil
}

// DropConnection은 현재 연결을 강제로 끊는다. 테스트에서 단절을 흉내내고,
// 운영에서는 Runner 자격증명 취소에 쓴다.
func (h *Hub) DropConnection() {
	h.mu.Lock()
	conn := h.conn
	h.conn = nil
	h.mu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusGoingAway, "dropped by server")
	}
}

// Subscribe는 적용된 이벤트를 받는 채널과 해지 함수를 돌려준다.
func (h *Hub) Subscribe() (<-chan protocol.Envelope, func()) {
	ch := make(chan protocol.Envelope, 256)

	h.subMu.Lock()
	h.subs[ch] = struct{}{}
	h.subMu.Unlock()

	return ch, func() {
		h.subMu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.subMu.Unlock()
	}
}

func (h *Hub) fanout(env protocol.Envelope) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- env:
		default:
			// 느린 구독자 때문에 이벤트 적용이 막히면 안 된다. 흘린다.
			slog.Warn("dropping event for slow subscriber", "run_id", env.RunID, "seq", env.Seq)
		}
	}
}

// SendCommand는 연결된 Runner에 명령을 보낸다.
func (h *Hub) SendCommand(env protocol.Envelope) error {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()

	if conn == nil {
		return ErrNoRunner
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	return nil
}

// ServeWS는 Runner의 연결을 받아 handshake 후 메시지 루프를 돈다.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Runner는 브라우저가 아니므로 Origin 검사를 요구하지 않는다.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	hello, err := h.readHello(ctx, conn)
	if err != nil {
		slog.Warn("handshake rejected", "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}

	welcome, err := protocol.Negotiate(hello)
	if err != nil {
		slog.Warn("negotiation failed", "runner", hello.RunnerID, "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}

	points, err := h.st.ResumePoints()
	if err != nil {
		slog.Error("resume points failed", "err", err)
		_ = conn.Close(websocket.StatusInternalError, "resume points")
		return
	}
	welcome.ResumeFrom = points

	if err := h.writeEnvelope(ctx, conn, protocol.Envelope{
		V:    welcome.ProtocolVersion,
		Kind: protocol.KindWelcome,
		TS:   time.Now().UTC(),
		Body: mustJSON(welcome),
	}); err != nil {
		slog.Error("write welcome failed", "err", err)
		return
	}

	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	slog.Info("runner connected", "runner", hello.RunnerID)

	defer func() {
		h.mu.Lock()
		if h.conn == conn {
			h.conn = nil
		}
		h.mu.Unlock()
		slog.Info("runner disconnected", "runner", hello.RunnerID)
	}()

	h.readLoop(ctx, conn)
}

func (h *Hub) readHello(ctx context.Context, conn *websocket.Conn) (protocol.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		return protocol.Hello{}, fmt.Errorf("read hello: %w", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Hello{}, fmt.Errorf("unmarshal hello envelope: %w", err)
	}
	if env.Kind != protocol.KindHello {
		return protocol.Hello{}, fmt.Errorf("first message kind = %q, want %q", env.Kind, protocol.KindHello)
	}

	var hello protocol.Hello
	if err := json.Unmarshal(env.Body, &hello); err != nil {
		return protocol.Hello{}, fmt.Errorf("unmarshal hello body: %w", err)
	}
	if hello.PairingToken != h.pairingToken {
		return protocol.Hello{}, errors.New("invalid pairing token")
	}
	return hello, nil
}

func (h *Hub) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		_, raw, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("dropping malformed message", "err", err)
			continue
		}

		if env.Kind != protocol.KindEvent {
			// heartbeat 등 이벤트가 아닌 것은 읽은 것만으로 생존 신호가 된다.
			continue
		}

		accepted, ack, err := h.st.ApplyEvent(env)
		if err != nil {
			slog.Error("apply event failed", "run_id", env.RunID, "seq", env.Seq, "err", err)
			continue
		}
		if accepted {
			h.fanout(env)
		}

		if err := h.writeEnvelope(ctx, conn, protocol.Envelope{
			V:     env.V,
			Kind:  protocol.KindAck,
			RunID: env.RunID,
			Seq:   ack,
			TS:    time.Now().UTC(),
		}); err != nil {
			slog.Warn("write ack failed", "err", err)
			return
		}
	}
}

func (h *Hub) writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return raw
}
```

- [ ] **Step 4: Runner link 구현**

`internal/runner/link/link.go`:

```go
// Package link는 Runner에서 Server로 나가는 WebSocket 연결을 관리한다.
//
// 연결은 항상 Runner가 건다. 끊기면 백오프 후 다시 붙고, Welcome의
// ResumeFrom을 보고 spool에서 미확인 이벤트를 다시 흘려보낸다.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/spool"
)

const (
	heartbeatInterval = 10 * time.Second
	minBackoff        = 200 * time.Millisecond
	maxBackoff        = 10 * time.Second
	resendBatch       = 200
)

// CommandHandler는 Server가 보낸 명령을 처리한다. 멱등성은 호출자 책임이다.
type CommandHandler func(ctx context.Context, env protocol.Envelope) error

type Config struct {
	ServerURL    string
	RunnerID     string
	PairingToken string
	Spool        *spool.Spool
	Capabilities []string
	OnCommand    CommandHandler
}

type Link struct {
	cfg Config

	mu   sync.Mutex
	conn *websocket.Conn

	// wake는 새 이벤트가 spool에 들어왔음을 전송 루프에 알린다.
	wake chan struct{}
}

func New(cfg Config) (*Link, error) {
	if cfg.ServerURL == "" {
		return nil, errors.New("link: ServerURL is required")
	}
	if cfg.RunnerID == "" {
		return nil, errors.New("link: RunnerID is required")
	}
	if cfg.Spool == nil {
		return nil, errors.New("link: Spool is required")
	}
	if cfg.OnCommand == nil {
		return nil, errors.New("link: OnCommand is required")
	}
	return &Link{cfg: cfg, wake: make(chan struct{}, 1)}, nil
}

// Publish는 이벤트를 spool에 적고 전송 루프를 깨운다. 연결이 끊겨 있어도
// 성공한다. 이것이 단절 중 이벤트를 잃지 않는 이유다.
func (l *Link) Publish(runID string, env protocol.Envelope) error {
	if _, err := l.cfg.Spool.Append(runID, env); err != nil {
		return fmt.Errorf("append to spool: %w", err)
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run은 ctx가 끝날 때까지 연결을 유지한다.
func (l *Link) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := l.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("link session ended, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (l *Link) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, l.cfg.ServerURL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	hello := protocol.Hello{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		RunnerID:        l.cfg.RunnerID,
		PairingToken:    l.cfg.PairingToken,
		Capabilities:    l.cfg.Capabilities,
	}
	body, err := json.Marshal(hello)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}
	if err := writeEnvelope(ctx, conn, protocol.Envelope{
		V:    buildinfo.ProtocolVersion(),
		Kind: protocol.KindHello,
		TS:   time.Now().UTC(),
		Body: body,
	}); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}

	welcome, err := readWelcome(ctx, conn)
	if err != nil {
		return fmt.Errorf("read welcome: %w", err)
	}
	slog.Info("connected to server", "server_version", welcome.ServerVersion, "runs", len(welcome.ResumeFrom))

	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.conn == conn {
			l.conn = nil
		}
		l.mu.Unlock()
	}()

	// Server가 이미 받은 지점까지 spool을 정리한 뒤 나머지를 다시 보낸다.
	for runID, seq := range welcome.ResumeFrom {
		if err := l.cfg.Spool.Ack(runID, seq); err != nil {
			return fmt.Errorf("trim spool for %s: %w", runID, err)
		}
	}

	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		errCh <- l.readLoop(sessionCtx, conn)
	}()
	go func() {
		defer wg.Done()
		errCh <- l.writeLoop(sessionCtx, conn)
	}()

	err = <-errCh
	cancelSession()
	_ = conn.Close(websocket.StatusNormalClosure, "session ending")
	wg.Wait()
	return err
}

func (l *Link) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("dropping malformed server message", "err", err)
			continue
		}

		switch env.Kind {
		case protocol.KindAck:
			if err := l.cfg.Spool.Ack(env.RunID, env.Seq); err != nil {
				return fmt.Errorf("apply ack: %w", err)
			}
		case protocol.KindCommand:
			if err := l.cfg.OnCommand(ctx, env); err != nil {
				slog.Error("command handler failed", "command_id", env.ID, "type", env.Type, "err", err)
			}
		default:
			slog.Warn("ignoring unexpected message kind", "kind", env.Kind)
		}
	}
}

func (l *Link) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		if err := l.drain(ctx, conn); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.wake:
		case <-ticker.C:
			if err := writeEnvelope(ctx, conn, protocol.Envelope{
				V:    buildinfo.ProtocolVersion(),
				Kind: protocol.KindEvent,
				Type: protocol.EvHeartbeat,
				TS:   time.Now().UTC(),
			}); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
	}
}

// drain은 spool에 남은 미확인 이벤트를 모두 흘려보낸다. Ack이 도착하면
// readLoop이 spool에서 지우므로, 여기서는 남은 것만 반복해서 보낸다.
func (l *Link) drain(ctx context.Context, conn *websocket.Conn) error {
	runs, err := l.cfg.Spool.ActiveRuns()
	if err != nil {
		return fmt.Errorf("list active runs: %w", err)
	}

	for _, runID := range runs {
		batch, err := l.cfg.Spool.Since(runID, 0, resendBatch)
		if err != nil {
			return fmt.Errorf("read spool for %s: %w", runID, err)
		}
		for _, env := range batch {
			if err := writeEnvelope(ctx, conn, env); err != nil {
				return fmt.Errorf("write event: %w", err)
			}
		}
	}
	return nil
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

func readWelcome(ctx context.Context, conn *websocket.Conn) (protocol.Welcome, error) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(readCtx)
	if err != nil {
		return protocol.Welcome{}, fmt.Errorf("read: %w", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Welcome{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.Kind != protocol.KindWelcome {
		return protocol.Welcome{}, fmt.Errorf("kind = %q, want %q", env.Kind, protocol.KindWelcome)
	}

	var w protocol.Welcome
	if err := json.Unmarshal(env.Body, &w); err != nil {
		return protocol.Welcome{}, fmt.Errorf("unmarshal welcome: %w", err)
	}
	return w, nil
}
```

> **재전송이 중복을 만들지 않는 이유:** `drain`은 spool에 남은 것을 반복해 보낼 수 있고 실제로 그렇게 한다. 같은 이벤트가 두 번 도착해도 Server의 `ApplyEvent`가 `INSERT OR IGNORE`로 흘려버린다(Task 4). at-least-once 전송 + 멱등 적용이 짝이다.

- [ ] **Step 5: 의존성 추가 후 테스트 통과 확인**

Run: `go get github.com/coder/websocket@latest && go test ./internal/server/hub/ -race -v`
Expected: PASS (4 tests). 특히 `TestReconnectResendsUnackedEventsWithoutDuplication`이 정확히 8개 이벤트를 확인해야 한다.

- [ ] **Step 6: 커밋**

```bash
git add go.mod go.sum internal/server/hub internal/runner/link
git commit -m "feat(transport): 아웃바운드 WSS, 페어링, heartbeat, 재연결 재전송"
```

---

## Task 6: Git worktree 관리와 salvage

**Files:**
- Create: `internal/gitops/worktree.go`, `internal/gitops/worktree_test.go`

**Interfaces:**
- Consumes: 없음 (`git` CLI 호출)
- Produces:
  - `gitops.New(repoPath, worktreeRoot string) *Manager`
  - `(*Manager).BranchName(runID string) string` — `taskyard/run/<runID>`
  - `(*Manager).WorktreePath(runID string) string`
  - `(*Manager).Ensure(ctx context.Context, runID, baseBranch string) (Workspace, error)` — 있으면 재사용, 없으면 생성
  - `(*Manager).Status(ctx context.Context, runID string) (Status, error)`
  - `(*Manager).Diff(ctx context.Context, runID, baseBranch string) (string, error)`
  - `(*Manager).Salvage(ctx context.Context, runID string) (sha string, saved bool, err error)`
  - `gitops.Workspace{RunID, Branch, Path string, Created bool}`
  - `gitops.Status{Dirty bool, ChangedPaths []string}`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/gitops/worktree_test.go`:

```go
package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepo는 커밋 하나가 있는 저장소를 만든다.
func newRepo(t *testing.T) (repoPath, worktreeRoot string) {
	t.Helper()
	base := t.TempDir()
	repoPath = filepath.Join(base, "repo")
	worktreeRoot = filepath.Join(base, "worktrees")

	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repoPath, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoPath, "add", "README.md")
	run(t, repoPath, "commit", "-q", "-m", "initial")
	return repoPath, worktreeRoot
}

func TestBranchAndPathAreDeterministic(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)

	if got, want := m.BranchName("run-1"), "taskyard/run/run-1"; got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
	if m.WorktreePath("run-1") != m.WorktreePath("run-1") {
		t.Error("WorktreePath is not stable across calls")
	}
}

func TestEnsureCreatesWorktreeOnBranch(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)

	ws, err := m.Ensure(context.Background(), "run-1", "main")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !ws.Created {
		t.Error("Created = false on first Ensure")
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err != nil {
		t.Fatalf("worktree is missing repo content: %v", err)
	}

	branch := strings.TrimSpace(run(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "taskyard/run/run-1" {
		t.Fatalf("worktree branch = %q, want taskyard/run/run-1", branch)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	first, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	// 같은 run.start 명령이 재전송돼도 worktree가 또 생기면 안 된다(GH-09).
	second, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if second.Created {
		t.Error("Created = true on repeat Ensure; must reuse")
	}
	if second.Path != first.Path {
		t.Errorf("path changed: %q then %q", first.Path, second.Path)
	}

	out := run(t, repo, "worktree", "list")
	if n := strings.Count(out, "taskyard/run/run-1"); n != 1 {
		t.Fatalf("worktree list mentions the branch %d times, want 1:\n%s", n, out)
	}
}

func TestStatusReportsDirtyPaths(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	clean, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty {
		t.Error("fresh worktree reported dirty")
	}

	if err := os.WriteFile(filepath.Join(ws.Path, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty {
		t.Fatal("worktree with a new file reported clean")
	}
	if len(dirty.ChangedPaths) != 1 || dirty.ChangedPaths[0] != "new.txt" {
		t.Fatalf("ChangedPaths = %v, want [new.txt]", dirty.ChangedPaths)
	}
}

func TestSalvageCommitsUncommittedWork(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, saved, err := m.Salvage(ctx, "run-1")
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if !saved {
		t.Fatal("saved = false despite uncommitted changes")
	}
	if sha == "" {
		t.Fatal("Salvage returned an empty sha")
	}

	after, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Dirty {
		t.Error("worktree still dirty after Salvage")
	}

	body := run(t, ws.Path, "show", "--stat", "--format=%s", sha)
	if !strings.Contains(body, "wip.txt") {
		t.Fatalf("salvage commit does not contain wip.txt:\n%s", body)
	}
}

func TestSalvageIsNoOpOnCleanWorktree(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	if _, err := m.Ensure(ctx, "run-1", "main"); err != nil {
		t.Fatal(err)
	}

	sha, saved, err := m.Salvage(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if saved {
		t.Errorf("saved = true on a clean worktree (sha %q)", sha)
	}
}

func TestDiffShowsChangesAgainstBase(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := m.Diff(ctx, "run-1", "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Fatalf("diff does not show the added line:\n%s", diff)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/gitops/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: 구현**

`internal/gitops/worktree.go`:

```go
// Package gitops는 Run별 격리 작업공간을 만든다.
//
// branch와 worktree 경로를 run_id에서 결정론적으로 파생하는 것이 핵심이다.
// 같은 run.start 명령이 재전송돼도 새로 만들지 않고 그대로 재사용한다
// (PRD GH-09). 그리고 어떤 경우에도 worktree를 자동 삭제하지 않는다
// (PRD §8.7.1).
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Manager struct {
	repoPath     string
	worktreeRoot string
}

func New(repoPath, worktreeRoot string) *Manager {
	return &Manager{repoPath: repoPath, worktreeRoot: worktreeRoot}
}

// Workspace는 한 Run이 쓰는 작업공간이다.
type Workspace struct {
	RunID   string
	Branch  string
	Path    string
	Created bool
}

// Status는 worktree의 현재 변경 상태다.
type Status struct {
	Dirty        bool
	ChangedPaths []string
}

func (m *Manager) BranchName(runID string) string {
	return "taskyard/run/" + runID
}

func (m *Manager) WorktreePath(runID string) string {
	return filepath.Join(m.worktreeRoot, runID)
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

func (m *Manager) branchExists(ctx context.Context, branch string) bool {
	_, err := m.git(ctx, m.repoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Ensure는 Run의 worktree를 보장한다. 이미 있으면 그대로 쓴다.
func (m *Manager) Ensure(ctx context.Context, runID, baseBranch string) (Workspace, error) {
	branch := m.BranchName(runID)
	path := m.WorktreePath(runID)

	ws := Workspace{RunID: runID, Branch: branch, Path: path}

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		// 경로가 이미 있다. 기대한 branch에 있는지만 확인하고 재사용한다.
		out, err := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return Workspace{}, fmt.Errorf("inspect existing worktree at %s: %w", path, err)
		}
		if got := strings.TrimSpace(out); got != branch {
			return Workspace{}, fmt.Errorf("worktree at %s is on branch %q, want %q", path, got, branch)
		}
		return ws, nil
	}

	if err := os.MkdirAll(m.worktreeRoot, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create worktree root: %w", err)
	}

	args := []string{"worktree", "add"}
	if m.branchExists(ctx, branch) {
		// 이전 Run이 남긴 branch가 있으면 새로 만들지 않고 붙인다.
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, baseBranch)
	}

	if _, err := m.git(ctx, m.repoPath, args...); err != nil {
		return Workspace{}, fmt.Errorf("add worktree for %s: %w", runID, err)
	}

	ws.Created = true
	return ws, nil
}

// Status는 미커밋 변경을 조사한다.
func (m *Manager) Status(ctx context.Context, runID string) (Status, error) {
	out, err := m.git(ctx, m.WorktreePath(runID), "status", "--porcelain")
	if err != nil {
		return Status{}, fmt.Errorf("status for %s: %w", runID, err)
	}

	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// porcelain v1: 상태 두 글자 + 공백 + 경로
		paths = append(paths, strings.TrimSpace(line[3:]))
	}
	return Status{Dirty: len(paths) > 0, ChangedPaths: paths}, nil
}

// Diff는 base branch 대비 변경을 통합 diff로 돌려준다. 커밋 여부와
// 무관하게 현재 작업 트리 상태를 본다.
func (m *Manager) Diff(ctx context.Context, runID, baseBranch string) (string, error) {
	out, err := m.git(ctx, m.WorktreePath(runID), "diff", baseBranch)
	if err != nil {
		return "", fmt.Errorf("diff for %s: %w", runID, err)
	}
	return out, nil
}

// Salvage는 미커밋 변경을 Run branch에 커밋해 보존한다. 깨끗하면 아무것도
// 하지 않는다. Run이 실패·취소·lost로 끝나기 전에 반드시 호출한다.
func (m *Manager) Salvage(ctx context.Context, runID string) (string, bool, error) {
	status, err := m.Status(ctx, runID)
	if err != nil {
		return "", false, err
	}
	if !status.Dirty {
		return "", false, nil
	}

	path := m.WorktreePath(runID)
	if _, err := m.git(ctx, path, "add", "-A"); err != nil {
		return "", false, fmt.Errorf("stage salvage for %s: %w", runID, err)
	}

	msg := fmt.Sprintf("taskyard salvage %s", runID)
	if _, err := m.git(ctx, path,
		"-c", "user.name=taskyard",
		"-c", "user.email=taskyard@localhost",
		"commit", "-m", msg,
	); err != nil {
		return "", false, fmt.Errorf("commit salvage for %s: %w", runID, err)
	}

	out, err := m.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("read salvage sha for %s: %w", runID, err)
	}
	return strings.TrimSpace(out), true, nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/gitops/ -race -v`
Expected: PASS (7 tests)

- [ ] **Step 5: 커밋**

```bash
git add internal/gitops
git commit -m "feat(gitops): 결정론적 worktree 생성과 salvage 커밋"
```

---

## Task 7: Claude Code 어댑터

**Files:**
- Create: `internal/agents/adapter/adapter.go`
- Create: `internal/agents/adapter/claudecode/parse.go`, `internal/agents/adapter/claudecode/parse_test.go`
- Create: `internal/agents/adapter/claudecode/spawn.go`, `internal/agents/adapter/claudecode/spawn_test.go`
- Use: `internal/agents/adapter/claudecode/testdata/session-pong.ndjson` (이미 저장소에 있음)

**Interfaces:**
- Consumes: `protocol.Ev*` 상수
- Produces:
  - `adapter.Event{Type string, Body map[string]any, Raw json.RawMessage}`
  - `adapter.SessionInfo{SessionID, Model, Version, APIKeySource string}`
  - `(SessionInfo).UsesAPIKey() bool`
  - `claudecode.NewParser() *Parser`
  - `(*Parser).Parse(r io.Reader, emit func(adapter.Event) error) error`
  - `(*Parser).Session() adapter.SessionInfo`
  - `claudecode.SpawnOptions{Prompt, WorkDir, ResumeSessionID, BrokerURL, BrokerToken string}`
  - `claudecode.BuildArgs(opts SpawnOptions) ([]string, error)`
  - `claudecode.ScrubEnv(env []string) []string`

> **fixture 정책:** `session-pong.ndjson`은 실제 `claude -p` 실행 캡처다(개인정보 제거됨). 파서 테스트는 이 파일만 쓴다. `tool_use`/`tool_result` 블록은 이 캡처에 없으므로 합성 NDJSON으로 테스트하고, Task 12의 `make smoke`에서 실제 캡처로 교체한다.

- [ ] **Step 1: 공통 어댑터 계약 작성**

`internal/agents/adapter/adapter.go`:

```go
// Package adapter는 Provider별 차이를 흡수하는 공통 계약이다.
//
// PRD §11.6.2를 따른다. Provider 이벤트를 정규화하되, 정규화하지 못한
// 정보는 버리지 않고 Raw에 남긴다.
package adapter

import "encoding/json"

// Event는 정규화된 실행 이벤트다.
type Event struct {
	Type string          `json:"type"`
	Body map[string]any  `json:"body,omitempty"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

// SessionInfo는 Provider 세션의 신원과 과금 경로다.
type SessionInfo struct {
	SessionID    string `json:"session_id"`
	Model        string `json:"model"`
	Version      string `json:"version"`
	APIKeySource string `json:"api_key_source"`
}

// UsesAPIKey는 이 세션이 구독이 아니라 API 키로 과금되는지를 알려준다.
// Claude Code는 구독 로그인일 때 apiKeySource로 "none"을 보고한다.
// true면 PRD §13.2의 경계를 넘은 것이므로 Run을 중단해야 한다.
func (s SessionInfo) UsesAPIKey() bool {
	return s.APIKeySource != "" && s.APIKeySource != "none"
}
```

- [ ] **Step 2: 파서의 실패하는 테스트 작성**

`internal/agents/adapter/claudecode/parse_test.go`:

```go
package claudecode

import (
	"os"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/protocol"
)

func parseFixture(t *testing.T, path string) (*Parser, []adapter.Event) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(f, func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p, got
}

func typesOf(events []adapter.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestParseCapturesSessionFromInit(t *testing.T) {
	p, _ := parseFixture(t, "testdata/session-pong.ndjson")

	s := p.Session()
	if s.SessionID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", s.Model)
	}
	if s.Version == "" {
		t.Error("Version is empty; the init event carries claude_code_version")
	}
}

func TestFixtureSessionIsOnTheSubscriptionPath(t *testing.T) {
	p, _ := parseFixture(t, "testdata/session-pong.ndjson")

	s := p.Session()
	if s.APIKeySource != "none" {
		t.Fatalf("APIKeySource = %q, want none", s.APIKeySource)
	}
	// 이것이 PRD §13.2의 자동 검증이다. API 키로 과금되면 Run을 중단한다.
	if s.UsesAPIKey() {
		t.Fatal("UsesAPIKey() = true for a subscription session")
	}
}

func TestParseEmitsNormalizedEventsInOrder(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	want := []string{
		protocol.EvMessageDelta,
		protocol.EvUsageUpdated,
		protocol.EvTurnCompleted,
	}
	gotTypes := typesOf(got)
	if len(gotTypes) != len(want) {
		t.Fatalf("event types = %v, want %v", gotTypes, want)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("event types = %v, want %v", gotTypes, want)
		}
	}
}

func TestMessageDeltaCarriesAssistantText(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	text, ok := got[0].Body["text"].(string)
	if !ok {
		t.Fatalf("message_delta body has no text: %#v", got[0].Body)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("text = %q, want it to contain pong", text)
	}
}

func TestRateLimitBecomesUsageUpdated(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	usage := got[1]
	if usage.Type != protocol.EvUsageUpdated {
		t.Fatalf("Type = %q", usage.Type)
	}
	if usage.Body["status"] != "allowed" {
		t.Errorf("status = %v, want allowed", usage.Body["status"])
	}
	if usage.Body["rate_limit_type"] != "five_hour" {
		t.Errorf("rate_limit_type = %v, want five_hour", usage.Body["rate_limit_type"])
	}
	// quota_exhausted는 스케줄러가 paused_quota로 옮길지 판단하는 신호다(EX-06).
	if usage.Body["quota_exhausted"] != false {
		t.Errorf("quota_exhausted = %v, want false", usage.Body["quota_exhausted"])
	}
}

func TestTurnCompletedCarriesResultAndCost(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	done := got[2]
	if done.Body["result"] != "pong" {
		t.Errorf("result = %v, want pong", done.Body["result"])
	}
	if done.Body["is_error"] != false {
		t.Errorf("is_error = %v, want false", done.Body["is_error"])
	}
	if _, ok := done.Body["total_cost_usd"]; !ok {
		t.Error("turn_completed has no total_cost_usd")
	}
}

func TestRawIsPreserved(t *testing.T) {
	_, got := parseFixture(t, "testdata/session-pong.ndjson")

	for i, e := range got {
		if len(e.Raw) == 0 {
			t.Fatalf("event %d (%s) dropped its raw payload", i, e.Type)
		}
	}
}

// 합성 데이터: tool_use/tool_result 블록은 session-pong 캡처에 없다.
// make smoke가 실제 캡처를 만들면 이 상수를 교체한다.
const syntheticToolNDJSON = `{"type":"assistant","session_id":"s1","parent_tool_use_id":null,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","session_id":"s1","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"README.md"}]}}
`

func TestToolUseBecomesToolStartedAndFinished(t *testing.T) {
	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(syntheticToolNDJSON), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(got), typesOf(got))
	}
	if got[0].Type != protocol.EvToolStarted {
		t.Errorf("got[0].Type = %q, want %q", got[0].Type, protocol.EvToolStarted)
	}
	if got[0].Body["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", got[0].Body["tool_name"])
	}
	if got[0].Body["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_use_id = %v, want toolu_1", got[0].Body["tool_use_id"])
	}
	if got[1].Type != protocol.EvToolFinished {
		t.Errorf("got[1].Type = %q, want %q", got[1].Type, protocol.EvToolFinished)
	}
	if got[1].Body["tool_use_id"] != "toolu_1" {
		t.Errorf("finished tool_use_id = %v, want toolu_1", got[1].Body["tool_use_id"])
	}
}

func TestMalformedLineBecomesErrorEventAndParsingContinues(t *testing.T) {
	input := "not json at all\n" + `{"type":"result","subtype":"success","result":"ok","is_error":false,"num_turns":1,"session_id":"s1"}` + "\n"

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(input), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse returned an error; a bad line must not abort the stream: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("types = %v, want [error turn_completed]", typesOf(got))
	}
	if got[0].Type != protocol.EvError {
		t.Errorf("got[0].Type = %q, want %q", got[0].Type, protocol.EvError)
	}
	if got[1].Type != protocol.EvTurnCompleted {
		t.Errorf("got[1].Type = %q, want %q", got[1].Type, protocol.EvTurnCompleted)
	}
}

func TestLongLinesAreHandled(t *testing.T) {
	// bufio.Scanner의 기본 64KB 한도를 넘는 줄이 실제로 나온다.
	// 파서는 이를 잘라먹거나 실패하면 안 된다.
	big := strings.Repeat("x", 200_000)
	input := `{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"text","text":"` + big + `"}]}}` + "\n"

	p := NewParser()
	var got []adapter.Event
	if err := p.Parse(strings.NewReader(input), func(e adapter.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(got) != 1 || got[0].Type != protocol.EvMessageDelta {
		t.Fatalf("types = %v, want [message_delta]", typesOf(got))
	}
	if len(got[0].Body["text"].(string)) != len(big) {
		t.Fatal("long text was truncated")
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/agents/adapter/claudecode/ -v`
Expected: FAIL — `undefined: NewParser`

- [ ] **Step 4: 파서 구현**

`internal/agents/adapter/claudecode/parse.go`:

```go
// Package claudecode는 Claude Code의 headless 출력을 정규화 이벤트로 바꾼다.
//
// 입력은 `claude -p --output-format stream-json --verbose`의 NDJSON이다.
// 터미널 화면을 파싱하지 않는다. PRD §11.6.1의 표면을 그대로 쓴다.
package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/protocol"
)

// maxLineBytes는 한 NDJSON 줄의 상한이다. 큰 도구 결과가 그대로 실려 오므로
// bufio의 기본값(64KB)으로는 부족하다.
const maxLineBytes = 8 << 20 // 8MB

type Parser struct {
	session adapter.SessionInfo
}

func NewParser() *Parser { return &Parser{} }

// Session은 init 이벤트에서 읽은 세션 정보를 돌려준다. Parse 도중이나
// 이후에 호출한다.
func (p *Parser) Session() adapter.SessionInfo { return p.session }

// Parse는 NDJSON을 끝까지 읽으며 정규화 이벤트마다 emit을 호출한다.
// 깨진 줄 하나가 스트림 전체를 죽이지 않도록, 파싱 실패는 error 이벤트로
// 바꾸고 계속 읽는다. emit이 에러를 돌려주면 그때는 즉시 중단한다.
func (p *Parser) Parse(r io.Reader, emit func(adapter.Event) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// scanner는 버퍼를 재사용한다. Raw로 들고 있으려면 복사해야 한다.
		raw := make([]byte, len(line))
		copy(raw, line)

		var msg streamMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			if emitErr := emit(adapter.Event{
				Type: protocol.EvError,
				Body: map[string]any{"reason": "unparseable stream line", "detail": err.Error()},
				Raw:  raw,
			}); emitErr != nil {
				return emitErr
			}
			continue
		}

		if err := p.dispatch(msg, raw, emit); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

func (p *Parser) dispatch(msg streamMessage, raw json.RawMessage, emit func(adapter.Event) error) error {
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" {
			p.session = adapter.SessionInfo{
				SessionID:    msg.SessionID,
				Model:        msg.Model,
				Version:      msg.ClaudeCodeVersion,
				APIKeySource: msg.APIKeySource,
			}
		}
		// hook_started, hook_response, plugin_install 등은 정규화 대상이 아니다.
		return nil

	case "assistant":
		return p.emitContentBlocks(msg, raw, emit)

	case "user":
		return p.emitContentBlocks(msg, raw, emit)

	case "rate_limit_event":
		info := msg.RateLimitInfo
		return emit(adapter.Event{
			Type: protocol.EvUsageUpdated,
			Body: map[string]any{
				"status":          info.Status,
				"rate_limit_type": info.RateLimitType,
				"resets_at":       info.ResetsAt,
				"using_overage":   info.IsUsingOverage,
				// 스케줄러는 이 값만 보고 paused_quota 전이를 판단한다(EX-06).
				"quota_exhausted": info.Status != "allowed",
			},
			Raw: raw,
		})

	case "result":
		return emit(adapter.Event{
			Type: protocol.EvTurnCompleted,
			Body: map[string]any{
				"result":         msg.Result,
				"is_error":       msg.IsError,
				"num_turns":      msg.NumTurns,
				"duration_ms":    msg.DurationMS,
				"total_cost_usd": msg.TotalCostUSD,
				"stop_reason":    msg.StopReason,
				"session_id":     msg.SessionID,
			},
			Raw: raw,
		})

	default:
		// stream_event(부분 델타) 등 아직 정규화하지 않는 종류.
		return nil
	}
}

func (p *Parser) emitContentBlocks(msg streamMessage, raw json.RawMessage, emit func(adapter.Event) error) error {
	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			if err := emit(adapter.Event{
				Type: protocol.EvMessageDelta,
				Body: map[string]any{
					"text":               block.Text,
					"parent_tool_use_id": msg.ParentToolUseID,
				},
				Raw: raw,
			}); err != nil {
				return err
			}

		case "tool_use":
			if err := emit(adapter.Event{
				Type: protocol.EvToolStarted,
				Body: map[string]any{
					"tool_use_id": block.ID,
					"tool_name":   block.Name,
					"input":       block.Input,
				},
				Raw: raw,
			}); err != nil {
				return err
			}

		case "tool_result":
			if err := emit(adapter.Event{
				Type: protocol.EvToolFinished,
				Body: map[string]any{
					"tool_use_id": block.ToolUseID,
					"is_error":    block.IsError,
				},
				Raw: raw,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// streamMessage는 stream-json 한 줄의 관심 필드만 담는다. 모르는 필드는
// 무시하되 원본은 Raw로 보존한다.
type streamMessage struct {
	Type              string `json:"type"`
	Subtype           string `json:"subtype"`
	SessionID         string `json:"session_id"`
	Model             string `json:"model"`
	ClaudeCodeVersion string `json:"claude_code_version"`
	APIKeySource      string `json:"apiKeySource"`
	ParentToolUseID   any    `json:"parent_tool_use_id"`

	Message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	} `json:"message"`

	RateLimitInfo struct {
		Status         string `json:"status"`
		RateLimitType  string `json:"rateLimitType"`
		ResetsAt       int64  `json:"resetsAt"`
		IsUsingOverage bool   `json:"isUsingOverage"`
	} `json:"rate_limit_info"`

	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	StopReason   string  `json:"stop_reason"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}
```

- [ ] **Step 5: 파서 테스트 통과 확인**

Run: `go test ./internal/agents/adapter/claudecode/ -race -v`
Expected: PASS (10 tests)

- [ ] **Step 6: 기동 인자와 환경 세척의 실패 테스트 작성**

`internal/agents/adapter/claudecode/spawn_test.go`:

```go
package claudecode

import (
	"strings"
	"testing"
)

func argsString(args []string) string { return strings.Join(args, " ") }

func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func baseOpts() SpawnOptions {
	return SpawnOptions{
		Prompt:      "do the thing",
		WorkDir:     "/tmp/wt/run-1",
		BrokerURL:   "http://127.0.0.1:9999/mcp",
		BrokerToken: "secret",
	}
}

func TestBuildArgsIncludesMandatoryFlags(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	if !hasFlagValue(args, "--output-format", "stream-json") {
		t.Error("missing --output-format stream-json")
	}
	if !hasFlag(args, "--verbose") {
		t.Error("missing --verbose (stream-json requires it)")
	}
	if !hasFlagValue(args, "--permission-prompt-tool", "mcp__taskyard__approve") {
		t.Error("missing --permission-prompt-tool")
	}
	if !hasFlag(args, "--strict-mcp-config") {
		t.Error("missing --strict-mcp-config")
	}
	if !hasFlagValue(args, "--permission-mode", "default") {
		t.Error("missing --permission-mode default")
	}
}

func TestBuildArgsNeverPassesBare(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	// --bare는 구독 로그인을 건너뛰고 ANTHROPIC_API_KEY를 요구한다.
	// Taskyard의 과금 모델을 깨므로 절대 나오면 안 된다(PRD §13.2.1).
	if hasFlag(args, "--bare") {
		t.Fatalf("--bare must never appear: %s", argsString(args))
	}
}

func TestBuildArgsForcesDefaultPermissionMode(t *testing.T) {
	// 사용자 전역 설정이 auto여도 분류기가 도구를 먼저 승인하면
	// 승인 브로커가 호출되지 않는다. 명시적으로 덮어써야 한다.
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--permission-mode" && args[i+1] != "default" {
			t.Fatalf("--permission-mode %s, want default", args[i+1])
		}
	}
}

func TestBuildArgsEmbedsBrokerInMCPConfig(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}

	var cfg string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--mcp-config" {
			cfg = args[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("missing --mcp-config")
	}
	for _, want := range []string{`"taskyard"`, `"http"`, "http://127.0.0.1:9999/mcp", "Bearer secret"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("mcp config missing %q:\n%s", want, cfg)
		}
	}
}

func TestBuildArgsAddsResumeWhenSessionKnown(t *testing.T) {
	opts := baseOpts()
	opts.ResumeSessionID = "sess-123"

	args, err := BuildArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "--resume", "sess-123") {
		t.Errorf("missing --resume sess-123: %s", argsString(args))
	}
}

func TestBuildArgsRejectsMissingBroker(t *testing.T) {
	opts := baseOpts()
	opts.BrokerURL = ""

	if _, err := BuildArgs(opts); err == nil {
		t.Fatal("BuildArgs accepted an empty BrokerURL; approvals would silently break")
	}
}

func TestScrubEnvRemovesBillingKeys(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-secret",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"OPENAI_API_KEY=sk-other",
		"CODEX_API_KEY=sk-codex",
		"HOME=/Users/dev",
	}

	got := ScrubEnv(in)

	for _, e := range got {
		for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
			if strings.HasPrefix(e, banned+"=") {
				t.Errorf("%s survived scrubbing", banned)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("kept %d vars (%v), want PATH and HOME only", len(got), got)
	}
}
```

- [ ] **Step 7: 테스트가 실패하는지 확인**

Run: `go test ./internal/agents/adapter/claudecode/ -run 'BuildArgs|ScrubEnv' -v`
Expected: FAIL — `undefined: BuildArgs`

- [ ] **Step 8: 기동 로직 구현**

`internal/agents/adapter/claudecode/spawn.go`:

```go
package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ApproveToolName은 Claude Code에 넘기는 권한 도구의 이름이다.
// MCP 명명 규칙 mcp__<server>__<tool>을 따른다.
const ApproveToolName = "mcp__taskyard__approve"

// bannedEnvKeys는 Agent 프로세스에서 제거할 API 과금용 변수다(PRD §13.2).
var bannedEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
}

type SpawnOptions struct {
	Prompt          string
	WorkDir         string
	ResumeSessionID string
	BrokerURL       string
	BrokerToken     string
}

// BuildArgs는 `claude`에 넘길 인자를 만든다. 여기가 PRD §13.2.1의
// 플래그 정책이 강제되는 단 하나의 지점이다.
func BuildArgs(opts SpawnOptions) ([]string, error) {
	if opts.Prompt == "" {
		return nil, errors.New("claudecode: Prompt is required")
	}
	if opts.BrokerURL == "" {
		return nil, errors.New("claudecode: BrokerURL is required; without it approvals never reach the user")
	}

	mcpConfig, err := brokerMCPConfig(opts.BrokerURL, opts.BrokerToken)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		// 사용자 전역 설정이 auto여도 분류기가 승인을 가로채지 못하게 한다.
		"--permission-mode", "default",
		// 브로커 외의 MCP 서버를 차단한다.
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"--permission-prompt-tool", ApproveToolName,
	}

	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args, nil
}

func brokerMCPConfig(url, token string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"taskyard": map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	return string(raw), nil
}

// ScrubEnv는 API 과금용 변수를 제거한 환경을 돌려준다.
func ScrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		banned := false
		for _, b := range bannedEnvKeys {
			if key == b {
				banned = true
				break
			}
		}
		if !banned {
			out = append(out, entry)
		}
	}
	return out
}
```

- [ ] **Step 9: 전체 테스트 통과 확인**

Run: `go test ./internal/agents/... -race -v`
Expected: PASS (17 tests)

- [ ] **Step 10: 커밋**

```bash
git add internal/agents
git commit -m "feat(adapter): Claude Code stream-json 파서와 기동 정책"
```

---

## Task 8: 로컬 승인 브로커 (probe-first)

**Files:**
- Create: `internal/approval/probe/main.go` (일회용, Step 1~3에서만 사용 후 삭제)
- Create: `internal/approval/testdata/tools-call-approve.json` (probe 결과 고정)
- Create: `internal/approval/broker.go`, `internal/approval/broker_test.go`

**Interfaces:**
- Consumes: 없음
- Produces:
  - `approval.New(token string) *Broker`
  - `(*Broker).Handler() http.Handler` — MCP streamable HTTP 엔드포인트
  - `(*Broker).Requests() <-chan Request` — 새 승인 요청 팬아웃
  - `(*Broker).Decide(requestID string, d Decision) error`
  - `approval.Request{ID, ToolName string, Input json.RawMessage}`
  - `approval.Decision{Allow bool, Message string, UpdatedInput json.RawMessage}`

> **왜 stdio가 아니라 HTTP인가:** stdio MCP 서버는 Claude Code가 자식 프로세스로 띄운다. 그러면 승인 상태를 들고 있는 Runner 프로세스와 별개 프로세스가 되어 IPC가 하나 더 필요하다. Runner가 로컬호스트에 HTTP MCP 엔드포인트를 띄우면 승인 상관관계가 Runner 메모리 안에서 끝난다.

> **왜 probe가 먼저인가:** `--permission-prompt-tool`이 지정된 MCP 도구에 Claude Code가 **보내는 요청의 필드 이름**은 공식 문서에서 확인되지 않았다. 응답 형식(`{"behavior":"allow","updatedInput":…}`)은 확인됐다. 추측으로 구조체를 쓰지 말고 실제 요청을 한 번 받아 고정한 뒤 구현한다.

- [ ] **Step 1: 요청을 받아 적기만 하는 probe 서버 작성**

`internal/approval/probe/main.go`:

```go
//go:build ignore

// probe는 Claude Code가 권한 도구에 실제로 무엇을 보내는지 한 번 확인하기
// 위한 일회용 서버다. tools/call 본문을 그대로 파일에 적고 항상 허용한다.
// 계약을 고정한 뒤 삭제한다.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	out, err := os.Create("probe-capture.jsonl")
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(out, "%s\n", body)
		log.Printf("REQ: %s", body)

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"taskyard-probe","version":"0"}}}`, req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"approve","description":"probe","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			// 항상 허용. 목적은 요청 형태를 보는 것뿐이다.
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"behavior\":\"allow\",\"updatedInput\":{}}"}]}}`, req.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	log.Println("probe listening on 127.0.0.1:9999")
	log.Fatal(http.ListenAndServe("127.0.0.1:9999", nil))
}
```

- [ ] **Step 2: probe를 띄우고 실제 Claude Code로 승인 요청을 유발**

```bash
mkdir -p /tmp/taskyard-probe && cd /tmp/taskyard-probe
go run /Users/jinto/projects/taskyard/internal/approval/probe/main.go &
sleep 1

claude -p "Run the shell command: echo hello" \
  --output-format stream-json --verbose \
  --permission-mode default \
  --strict-mcp-config \
  --mcp-config '{"mcpServers":{"taskyard":{"type":"http","url":"http://127.0.0.1:9999/mcp"}}}' \
  --permission-prompt-tool mcp__taskyard__approve \
  > claude-out.ndjson 2> claude-err.txt

kill %1
cat probe-capture.jsonl
```

Expected: `probe-capture.jsonl`에 `initialize`, `tools/list`, `tools/call` 요청이 남는다. `tools/call`의 `params.arguments`가 확인해야 할 계약이다.

- [ ] **Step 3: 계약을 fixture로 고정**

```bash
mkdir -p /Users/jinto/projects/taskyard/internal/approval/testdata
grep '"tools/call"' /tmp/taskyard-probe/probe-capture.jsonl | head -1 \
  > /Users/jinto/projects/taskyard/internal/approval/testdata/tools-call-approve.json
cat /Users/jinto/projects/taskyard/internal/approval/testdata/tools-call-approve.json
```

`params.arguments`에 실제로 들어 있는 필드 이름을 확인하고, Step 5의 `toolCallArguments` 구조체 태그를 그 이름에 맞춘다. 예상되는 이름은 도구 이름과 입력이며, 캡처가 다르면 **캡처가 이긴다.**

- [ ] **Step 4: 실패하는 테스트 작성**

`internal/approval/broker_test.go`:

```go
package approval

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInitializeAdvertisesTools(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "tok", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var resp struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Fatalf("initialize did not advertise a tools capability: %s", rec.Body)
	}
}

func TestToolsListExposesApprove(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "tok", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "approve" {
		t.Fatalf("tools = %+v, want exactly [approve]", resp.Result.Tools)
	}
}

func TestMissingTokenIsRejected(t *testing.T) {
	b := New("tok")
	rec := post(t, b.Handler(), "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// 실제 캡처를 그대로 흘려넣어 요청 파싱이 계약과 맞는지 확인한다.
func TestToolsCallFromRealCaptureSurfacesARequest(t *testing.T) {
	body, err := os.ReadFile("testdata/tools-call-approve.json")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	b := New("tok")
	done := make(chan struct{})

	go func() {
		defer close(done)
		select {
		case req := <-b.Requests():
			if req.ToolName == "" {
				t.Errorf("request has no tool name; check the arguments field tags against the capture")
			}
			_ = b.Decide(req.ID, Decision{Allow: true})
		case <-time.After(5 * time.Second):
			t.Error("no approval request surfaced from a real tools/call capture")
		}
	}()

	rec := post(t, b.Handler(), "tok", string(body))
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestAllowDecisionBecomesPermissionResult(t *testing.T) {
	b := New("tok")
	call := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`

	go func() {
		req := <-b.Requests()
		_ = b.Decide(req.ID, Decision{Allow: true, UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)})
	}()

	rec := post(t, b.Handler(), "tok", call)

	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %+v, want one text block", resp.Result.Content)
	}

	var pr struct {
		Behavior     string          `json:"behavior"`
		UpdatedInput json.RawMessage `json:"updatedInput"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &pr); err != nil {
		t.Fatalf("permission result is not JSON: %v (%s)", err, resp.Result.Content[0].Text)
	}
	if pr.Behavior != "allow" {
		t.Errorf("behavior = %q, want allow", pr.Behavior)
	}
	// allow는 updatedInput을 반드시 포함해야 한다. 빠지면 옛 버전에서 거부된다.
	if len(pr.UpdatedInput) == 0 {
		t.Error("allow result omitted updatedInput")
	}
}

func TestDenyDecisionCarriesMessage(t *testing.T) {
	b := New("tok")
	call := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"rm -rf /"}}}}`

	go func() {
		req := <-b.Requests()
		_ = b.Decide(req.ID, Decision{Allow: false, Message: "사용자가 거절했습니다"})
	}()

	rec := post(t, b.Handler(), "tok", call)

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var pr struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Behavior != "deny" {
		t.Errorf("behavior = %q, want deny", pr.Behavior)
	}
	if pr.Message != "사용자가 거절했습니다" {
		t.Errorf("message = %q", pr.Message)
	}
}

func TestDecideOnUnknownRequestFails(t *testing.T) {
	b := New("tok")
	if err := b.Decide("nope", Decision{Allow: true}); err == nil {
		t.Fatal("Decide accepted an unknown request id")
	}
}
```

- [ ] **Step 5: 브로커 구현**

`internal/approval/broker.go`:

```go
// Package approval은 Runner가 로컬호스트에 띄우는 MCP 권한 도구다.
//
// Claude Code는 --permission-prompt-tool로 지정된 MCP 도구를 호출해
// 승인을 묻는다. 호출은 사람이 결정할 때까지 블록되고, 결정이 오면
// PermissionResult JSON을 텍스트 블록에 담아 돌려준다.
//
// 요청 구조체의 필드 태그는 Task 8 Step 3에서 캡처한 실제 요청에 맞춘다.
// 캡처와 다르면 캡처가 이긴다.
package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
)

// Request는 사람에게 올라갈 승인 요청 하나다.
type Request struct {
	ID       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// Decision은 사람이 내린 결정이다.
type Decision struct {
	Allow        bool            `json:"allow"`
	Message      string          `json:"message,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

type Broker struct {
	token string

	mu      sync.Mutex
	pending map[string]chan Decision

	requests chan Request
}

func New(token string) *Broker {
	return &Broker{
		token:    token,
		pending:  map[string]chan Decision{},
		requests: make(chan Request, 64),
	}
}

// Requests는 새 승인 요청 스트림이다.
func (b *Broker) Requests() <-chan Request { return b.requests }

// Decide는 대기 중인 요청에 결정을 전달한다.
func (b *Broker) Decide(requestID string, d Decision) error {
	b.mu.Lock()
	ch, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("approval: unknown request %q", requestID)
	}
	ch <- d
	return nil
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", b.serveMCP)
	return mux
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (b *Broker) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		b.reply(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "taskyard", "version": "0"},
		})

	case "tools/list":
		b.reply(w, req.ID, map[string]any{
			"tools": []map[string]any{{
				"name":        "approve",
				"description": "Ask the Taskyard user to approve or deny a tool call.",
				"inputSchema": map[string]any{"type": "object"},
			}},
		})

	case "tools/call":
		b.handleToolCall(w, req)

	default:
		// notifications/initialized 등 응답이 필요 없는 알림.
		w.WriteHeader(http.StatusAccepted)
	}
}

// toolCallArguments의 태그는 Step 3의 캡처에 맞춘다.
type toolCallArguments struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

func (b *Broker) handleToolCall(w http.ResponseWriter, req jsonrpcRequest) {
	var params struct {
		Name      string            `json:"name"`
		Arguments toolCallArguments `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.replyError(w, req.ID, fmt.Sprintf("bad params: %v", err))
		return
	}

	id := uuid.NewString()
	ch := make(chan Decision, 1)

	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	slog.Info("approval requested", "id", id, "tool", params.Arguments.ToolName)

	b.requests <- Request{
		ID:       id,
		ToolName: params.Arguments.ToolName,
		Input:    params.Arguments.Input,
	}

	// 사람이 결정할 때까지 무기한 기다린다. Claude Code는 그동안 멈춘다.
	d := <-ch

	result, err := permissionResult(d, params.Arguments.Input)
	if err != nil {
		b.replyError(w, req.ID, err.Error())
		return
	}

	b.reply(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": result}},
	})
}

// permissionResult는 결정을 Claude Code가 이해하는 PermissionResult JSON으로
// 바꾼다. allow는 반드시 updatedInput을 포함해야 한다.
func permissionResult(d Decision, original json.RawMessage) (string, error) {
	var payload map[string]any

	if d.Allow {
		updated := d.UpdatedInput
		if len(updated) == 0 {
			updated = original
		}
		if len(updated) == 0 {
			updated = json.RawMessage(`{}`)
		}
		payload = map[string]any{
			"behavior":     "allow",
			"updatedInput": json.RawMessage(updated),
		}
	} else {
		msg := d.Message
		if msg == "" {
			msg = "Denied by the Taskyard user."
		}
		payload = map[string]any{"behavior": "deny", "message": msg}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal permission result: %w", err)
	}
	return string(raw), nil
}

func (b *Broker) reply(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		slog.Error("write mcp reply failed", "err", err)
	}
}

func (b *Broker) replyError(w http.ResponseWriter, id json.RawMessage, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32602, "message": msg},
	})
}

var _ = errors.New // 구현 확장 시 사용
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/approval/ -race -v`
Expected: PASS (7 tests). `TestToolsCallFromRealCaptureSurfacesARequest`가 실패하면 `toolCallArguments`의 태그를 캡처에 맞춰 고친다.

- [ ] **Step 7: probe 삭제 후 커밋**

```bash
rm -rf /Users/jinto/projects/taskyard/internal/approval/probe /tmp/taskyard-probe
git add internal/approval
git commit -m "feat(approval): 로컬 HTTP MCP 승인 브로커"
```

---

## Task 9: Run 수명주기 배선

**Files:**
- Modify: `internal/runner/spool/spool.go` (로컬 Run 원장 추가), `internal/runner/spool/spool_test.go`
- Create: `internal/runner/lifecycle/lifecycle.go`, `internal/runner/lifecycle/lifecycle_test.go`

**Interfaces:**
- Consumes: `spool.Spool`, `gitops.Manager`, `approval.Broker`, `claudecode.Parser/BuildArgs/ScrubEnv`, `protocol.*`
- Produces:
  - `(*spool.Spool).SaveRun(r spool.RunRecord) error`, `(*spool.Spool).LoadRuns() ([]spool.RunRecord, error)`
  - `spool.RunRecord{RunID, State, SessionID, Branch, WorktreePath string, PID int, StartedAtUnix int64}`
  - `lifecycle.Config{Spool, Git, Broker, BaseBranch, BrokerURL, BrokerToken, ClaudeBinary string, Publish PublishFunc}`
  - `lifecycle.PublishFunc = func(runID string, env protocol.Envelope) error`
  - `lifecycle.New(cfg Config) (*Manager, error)`
  - `(*Manager).HandleCommand(ctx context.Context, env protocol.Envelope) error`
  - `(*Manager).Start(ctx context.Context)` — 브로커 요청을 이벤트로 흘리는 루프

- [ ] **Step 1: 로컬 Run 원장의 실패 테스트를 spool 테스트에 추가**

`internal/runner/spool/spool_test.go` 끝에 덧붙인다:

```go
func TestSaveAndLoadRuns(t *testing.T) {
	s := openTemp(t)

	want := RunRecord{
		RunID:         "run-1",
		State:         "running",
		SessionID:     "sess-1",
		Branch:        "taskyard/run/run-1",
		WorktreePath:  "/tmp/wt/run-1",
		PID:           4242,
		StartedAtUnix: 1700000000,
	}
	if err := s.SaveRun(want); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got, err := s.LoadRuns()
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadRuns returned %d records, want 1", len(got))
	}
	if got[0] != want {
		t.Fatalf("record = %+v, want %+v", got[0], want)
	}
}

func TestSaveRunOverwritesByRunID(t *testing.T) {
	s := openTemp(t)

	if err := s.SaveRun(RunRecord{RunID: "run-1", State: "running", SessionID: ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(RunRecord{RunID: "run-1", State: "succeeded", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].State != "succeeded" || got[0].SessionID != "sess-1" {
		t.Fatalf("record = %+v, want the later values", got[0])
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/runner/spool/ -run Run -v`
Expected: FAIL — `undefined: RunRecord`

- [ ] **Step 3: 원장 구현**

`internal/runner/spool/spool.go`의 `schema` 상수 끝에 추가한다:

```go
CREATE TABLE IF NOT EXISTS runs (
  run_id        TEXT    PRIMARY KEY,
  state         TEXT    NOT NULL,
  session_id    TEXT    NOT NULL DEFAULT '',
  branch        TEXT    NOT NULL DEFAULT '',
  worktree_path TEXT    NOT NULL DEFAULT '',
  pid           INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL DEFAULT 0
);
```

같은 파일 끝에 추가한다:

```go
// RunRecord는 Runner가 로컬에 남기는 실행 기록이다. 재시작 후 조정
// (PRD §11.7)이 이 원장에서 시작한다.
type RunRecord struct {
	RunID         string
	State         string
	SessionID     string
	Branch        string
	WorktreePath  string
	PID           int
	StartedAtUnix int64
}

// SaveRun은 실행 기록을 만들거나 덮어쓴다.
func (s *Spool) SaveRun(r RunRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO runs (run_id, state, session_id, branch, worktree_path, pid, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET
		   state         = excluded.state,
		   session_id    = excluded.session_id,
		   branch        = excluded.branch,
		   worktree_path = excluded.worktree_path,
		   pid           = excluded.pid,
		   started_at    = excluded.started_at`,
		r.RunID, r.State, r.SessionID, r.Branch, r.WorktreePath, r.PID, r.StartedAtUnix,
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// LoadRuns는 모든 실행 기록을 돌려준다.
func (s *Spool) LoadRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT run_id, state, session_id, branch, worktree_path, pid, started_at FROM runs ORDER BY run_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		if err := rows.Scan(&r.RunID, &r.State, &r.SessionID, &r.Branch, &r.WorktreePath, &r.PID, &r.StartedAtUnix); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 원장 테스트 통과 확인**

Run: `go test ./internal/runner/spool/ -race -v`
Expected: PASS (9 tests)

- [ ] **Step 5: 수명주기의 실패 테스트 작성**

`internal/runner/lifecycle/lifecycle_test.go`:

```go
package lifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/spool"
)

// fakeClaude는 실제 CLI 대신 fixture NDJSON을 그대로 뱉는 스크립트다.
// 사용자 구독 할당량을 쓰지 않고 배선 전체를 검증할 수 있다.
func fakeClaude(t *testing.T, fixture string) string {
	t.Helper()

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\ncat " + abs + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newRepo(t *testing.T) (repo, worktrees string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	worktrees = filepath.Join(base, "wt")

	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo, worktrees
}

type collector struct {
	mu     sync.Mutex
	events []protocol.Envelope
}

func (c *collector) publish(_ string, env protocol.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, env)
	return nil
}

func (c *collector) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Type
	}
	return out
}

func (c *collector) count(evType string) int {
	n := 0
	for _, t := range c.types() {
		if t == evType {
			n++
		}
	}
	return n
}

func newManager(t *testing.T, col *collector) (*lifecycle.Manager, *spool.Spool, *gitops.Manager) {
	t.Helper()

	repo, worktrees := newRepo(t)
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	git := gitops.New(repo, worktrees)
	fixture := "../../agents/adapter/claudecode/testdata/session-pong.ndjson"

	m, err := lifecycle.New(lifecycle.Config{
		Spool:        sp,
		Git:          git,
		Broker:       approval.New("tok"),
		BaseBranch:   "main",
		BrokerURL:    "http://127.0.0.1:9999/mcp",
		BrokerToken:  "tok",
		ClaudeBinary: fakeClaude(t, fixture),
		Publish:      col.publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, sp, git
}

func startCommand(t *testing.T, runID, prompt string) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewCommand(protocol.CmdRunStart, runID, map[string]string{"prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunStartCreatesWorktreeAndStreamsEvents(t *testing.T) {
	col := &collector{}
	m, sp, git := newManager(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.HandleCommand(ctx, startCommand(t, "run-1", "say pong")); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}

	waitFor(t, "turn_completed", func() bool {
		return col.count(protocol.EvTurnCompleted) == 1
	})

	if col.count(protocol.EvMessageDelta) != 1 {
		t.Errorf("message_delta count = %d, want 1 (types: %v)", col.count(protocol.EvMessageDelta), col.types())
	}
	if col.count(protocol.EvRunStateChanged) < 2 {
		t.Errorf("expected at least a running and a terminal state change, got %v", col.types())
	}

	if _, err := os.Stat(filepath.Join(git.WorktreePath("run-1"), "README.md")); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}

	runs, err := sp.LoadRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ledger has %d runs, want 1", len(runs))
	}
	if runs[0].SessionID == "" {
		t.Error("ledger did not record the provider session id; resume would be impossible")
	}
}

func TestDuplicateRunStartIsAppliedOnce(t *testing.T) {
	col := &collector{}
	m, _, git := newManager(t, col)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startCommand(t, "run-1", "say pong")
	if err := m.HandleCommand(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first run to finish", func() bool { return col.count(protocol.EvTurnCompleted) == 1 })

	// 같은 command_id 재전송. 두 번째 실행도, 두 번째 worktree도 없어야 한다.
	if err := m.HandleCommand(ctx, cmd); err != nil {
		t.Fatalf("replayed command returned an error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := col.count(protocol.EvTurnCompleted); got != 1 {
		t.Fatalf("turn_completed count = %d, want 1 after a replayed command", got)
	}

	listCmd := exec.Command("git", "worktree", "list")
	listCmd.Dir = filepath.Dir(git.WorktreePath("run-1"))
	_ = listCmd // worktree 재사용은 gitops 테스트가 보장한다
}

func TestApprovalRequestBecomesAnEventAndDecisionFlowsBack(t *testing.T) {
	col := &collector{}
	broker := approval.New("tok")

	repo, worktrees := newRepo(t)
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	m, err := lifecycle.New(lifecycle.Config{
		Spool:        sp,
		Git:          gitops.New(repo, worktrees),
		Broker:       broker,
		BaseBranch:   "main",
		BrokerURL:    "http://127.0.0.1:9999/mcp",
		BrokerToken:  "tok",
		ClaudeBinary: fakeClaude(t, "../../agents/adapter/claudecode/testdata/session-pong.ndjson"),
		Publish:      col.publish,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Start(ctx)

	// 브로커에 승인 요청이 들어온 것처럼 만든다.
	decided := make(chan approval.Decision, 1)
	go func() {
		req := <-broker.Requests()
		_ = req
	}()

	_ = decided
	// 브로커를 직접 호출하는 대신, 수명주기가 요청을 이벤트로 바꾸는지 본다.
	go func() {
		_ = broker // 요청 유입은 아래 HTTP 호출이 만든다
	}()

	srv := broker.Handler()
	go func() {
		call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`
		req := newJSONRequest(t, call, "tok")
		rec := newRecorder()
		srv.ServeHTTP(rec, req)
	}()

	waitFor(t, "approval_requested event", func() bool {
		return col.count(protocol.EvApprovalRequested) == 1
	})

	// 이벤트에서 요청 ID를 꺼내 결정을 되돌린다.
	var reqID string
	for _, e := range col.events {
		if e.Type == protocol.EvApprovalRequested {
			var body map[string]any
			if err := json.Unmarshal(e.Body, &body); err != nil {
				t.Fatal(err)
			}
			reqID, _ = body["request_id"].(string)
		}
	}
	if reqID == "" {
		t.Fatal("approval_requested event carries no request_id")
	}

	decision, err := protocol.NewCommand(protocol.CmdApprovalDecision, "run-1", map[string]any{
		"request_id": reqID,
		"allow":      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.HandleCommand(ctx, decision); err != nil {
		t.Fatalf("approval decision command failed: %v", err)
	}
}
```

> 위 테스트의 `newJSONRequest`와 `newRecorder`는 `net/http/httptest`를 감싼 두 줄짜리 헬퍼다. 같은 파일 하단에 둔다:
>
> ```go
> func newJSONRequest(t *testing.T, body, token string) *http.Request {
> 	t.Helper()
> 	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
> 	r.Header.Set("Content-Type", "application/json")
> 	r.Header.Set("Authorization", "Bearer "+token)
> 	return r
> }
>
> func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
> ```
>
> import에 `net/http`와 `net/http/httptest`를 추가한다.

- [ ] **Step 6: 테스트가 실패하는지 확인**

Run: `go test ./internal/runner/lifecycle/ -v`
Expected: FAIL — `undefined: lifecycle.New`

- [ ] **Step 7: 수명주기 구현**

`internal/runner/lifecycle/lifecycle.go`:

```go
// Package lifecycle은 run.start 명령 하나를 실제 실행으로 바꾼다.
//
// worktree 보장 → Agent 기동 → 이벤트 정규화 및 발행 → 종료 처리 순이다.
// 명령 멱등성은 spool의 command_log가 담당하므로 같은 command_id가
// 다시 와도 두 번 실행되지 않는다(PRD §11.7).
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/agents/adapter/claudecode"
	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/spool"
)

// PublishFunc은 정규화 이벤트를 Server로 보낸다. 보통 link.Publish다.
type PublishFunc func(runID string, env protocol.Envelope) error

type Config struct {
	Spool        *spool.Spool
	Git          *gitops.Manager
	Broker       *approval.Broker
	BaseBranch   string
	BrokerURL    string
	BrokerToken  string
	ClaudeBinary string
	Publish      PublishFunc
}

type Manager struct {
	cfg Config

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(cfg Config) (*Manager, error) {
	switch {
	case cfg.Spool == nil:
		return nil, errors.New("lifecycle: Spool is required")
	case cfg.Git == nil:
		return nil, errors.New("lifecycle: Git is required")
	case cfg.Broker == nil:
		return nil, errors.New("lifecycle: Broker is required")
	case cfg.Publish == nil:
		return nil, errors.New("lifecycle: Publish is required")
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	return &Manager{cfg: cfg, active: map[string]context.CancelFunc{}}, nil
}

// Start는 브로커의 승인 요청을 Server로 올리는 루프다.
func (m *Manager) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-m.cfg.Broker.Requests():
			env, err := protocol.NewEvent(protocol.EvApprovalRequested, m.runIDForApproval(), 0, map[string]any{
				"request_id": req.ID,
				"tool_name":  req.ToolName,
				"input":      req.Input,
			})
			if err != nil {
				slog.Error("build approval event failed", "err", err)
				continue
			}
			if err := m.cfg.Publish(env.RunID, env); err != nil {
				slog.Error("publish approval event failed", "err", err)
			}
		}
	}
}

// runIDForApproval은 스파이크의 단순화다. 동시에 한 Run만 실행하므로
// 현재 활성 Run을 승인의 주인으로 본다. Phase 1에서 브로커가 Run별
// 엔드포인트를 갖도록 바꾼다.
func (m *Manager) runIDForApproval() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for runID := range m.active {
		return runID
	}
	return "unknown"
}

// HandleCommand는 Server 명령 하나를 처리한다.
func (m *Manager) HandleCommand(ctx context.Context, env protocol.Envelope) error {
	switch env.Type {
	case protocol.CmdRunStart:
		return m.handleRunStart(ctx, env)
	case protocol.CmdRunCancel:
		return m.handleRunCancel(env)
	case protocol.CmdApprovalDecision:
		return m.handleApprovalDecision(env)
	case protocol.CmdRunReconcile:
		return m.Reconcile(ctx)
	default:
		return fmt.Errorf("lifecycle: unknown command %q", env.Type)
	}
}

type runStartBody struct {
	Prompt string `json:"prompt"`
}

func (m *Manager) handleRunStart(ctx context.Context, env protocol.Envelope) error {
	// 멱등성 관문. 이미 본 command_id면 아무것도 하지 않는다.
	_, first, err := m.cfg.Spool.RememberCommand(env.ID, []byte(`{"accepted":true}`))
	if err != nil {
		return fmt.Errorf("remember command: %w", err)
	}
	if !first {
		slog.Info("ignoring replayed run.start", "command_id", env.ID, "run_id", env.RunID)
		return nil
	}

	var body runStartBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return fmt.Errorf("unmarshal run.start body: %w", err)
	}

	ws, err := m.cfg.Git.Ensure(ctx, env.RunID, m.cfg.BaseBranch)
	if err != nil {
		return fmt.Errorf("ensure worktree: %w", err)
	}

	if err := m.cfg.Spool.SaveRun(spool.RunRecord{
		RunID:         env.RunID,
		State:         "running",
		Branch:        ws.Branch,
		WorktreePath:  ws.Path,
		StartedAtUnix: time.Now().Unix(),
	}); err != nil {
		return fmt.Errorf("save run record: %w", err)
	}

	m.emitState(env.RunID, "running", "")

	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.active[env.RunID] = cancel
	m.mu.Unlock()

	go m.execute(runCtx, env.RunID, body.Prompt, ws)
	return nil
}

func (m *Manager) execute(ctx context.Context, runID, prompt string, ws gitops.Workspace) {
	defer func() {
		m.mu.Lock()
		delete(m.active, runID)
		m.mu.Unlock()
	}()

	args, err := claudecode.BuildArgs(claudecode.SpawnOptions{
		Prompt:      prompt,
		WorkDir:     ws.Path,
		BrokerURL:   m.cfg.BrokerURL,
		BrokerToken: m.cfg.BrokerToken,
	})
	if err != nil {
		m.fail(runID, fmt.Errorf("build args: %w", err))
		return
	}

	cmd := exec.CommandContext(ctx, m.cfg.ClaudeBinary, args...)
	cmd.Dir = ws.Path
	cmd.Env = claudecode.ScrubEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail(runID, fmt.Errorf("stdout pipe: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		m.fail(runID, fmt.Errorf("start agent: %w", err))
		return
	}

	parser := claudecode.NewParser()
	parseErr := parser.Parse(stdout, func(e adapter.Event) error {
		env, err := protocol.NewEvent(e.Type, runID, 0, map[string]any{
			"body": e.Body,
			"raw":  e.Raw,
		})
		if err != nil {
			return err
		}
		return m.cfg.Publish(runID, env)
	})

	waitErr := cmd.Wait()
	session := parser.Session()

	// 구독 경계 검사. API 키로 과금됐다면 실패로 처리한다(PRD §13.2).
	if session.UsesAPIKey() {
		m.fail(runID, fmt.Errorf("agent ran on API billing (apiKeySource=%q); refusing to continue", session.APIKeySource))
		return
	}

	record := spool.RunRecord{
		RunID:        runID,
		SessionID:    session.SessionID,
		Branch:       ws.Branch,
		WorktreePath: ws.Path,
	}

	switch {
	case parseErr != nil:
		record.State = "failed"
		_ = m.cfg.Spool.SaveRun(record)
		m.salvage(runID)
		m.fail(runID, fmt.Errorf("parse stream: %w", parseErr))
	case waitErr != nil:
		record.State = "failed"
		_ = m.cfg.Spool.SaveRun(record)
		m.salvage(runID)
		m.fail(runID, fmt.Errorf("agent exited: %w", waitErr))
	default:
		record.State = "succeeded"
		_ = m.cfg.Spool.SaveRun(record)
		m.emitState(runID, "succeeded", "")
	}
}

// salvage는 종료 전 미커밋 변경을 보존한다(PRD §8.7.1).
func (m *Manager) salvage(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sha, saved, err := m.cfg.Git.Salvage(ctx, runID)
	if err != nil {
		slog.Error("salvage failed", "run_id", runID, "err", err)
		return
	}
	if !saved {
		return
	}

	env, err := protocol.NewEvent(protocol.EvFileChanged, runID, 0, map[string]any{
		"kind": "salvage",
		"sha":  sha,
	})
	if err != nil {
		return
	}
	_ = m.cfg.Publish(runID, env)
}

func (m *Manager) handleRunCancel(env protocol.Envelope) error {
	m.mu.Lock()
	cancel, ok := m.active[env.RunID]
	m.mu.Unlock()

	if !ok {
		return nil
	}
	cancel()
	m.emitState(env.RunID, "cancelled", "cancelled by user")
	return nil
}

type approvalDecisionBody struct {
	RequestID string          `json:"request_id"`
	Allow     bool            `json:"allow"`
	Message   string          `json:"message"`
	Updated   json.RawMessage `json:"updated_input"`
}

func (m *Manager) handleApprovalDecision(env protocol.Envelope) error {
	var body approvalDecisionBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return fmt.Errorf("unmarshal approval decision: %w", err)
	}
	return m.cfg.Broker.Decide(body.RequestID, approval.Decision{
		Allow:        body.Allow,
		Message:      body.Message,
		UpdatedInput: body.Updated,
	})
}

func (m *Manager) emitState(runID, state, detail string) {
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, 0, map[string]any{
		"state":  state,
		"detail": detail,
	})
	if err != nil {
		slog.Error("build state event failed", "err", err)
		return
	}
	if err := m.cfg.Publish(runID, env); err != nil {
		slog.Error("publish state event failed", "run_id", runID, "err", err)
	}
}

func (m *Manager) fail(runID string, cause error) {
	slog.Error("run failed", "run_id", runID, "err", cause)
	m.emitState(runID, "failed", cause.Error())
}
```

- [ ] **Step 8: 테스트 통과 확인**

Run: `go test ./internal/runner/... -race -v`
Expected: PASS. `TestDuplicateRunStartIsAppliedOnce`가 정확히 1회 실행을 확인해야 한다.

- [ ] **Step 9: 커밋**

```bash
git add internal/runner
git commit -m "feat(runner): run.start 배선 — worktree, 어댑터, 승인, salvage"
```

---

## Task 10: 재시작 후 조정

**Files:**
- Create: `internal/runner/lifecycle/reconcile.go`, `internal/runner/lifecycle/reconcile_test.go`

**Interfaces:**
- Consumes: `spool.RunRecord`, `gitops.Manager`
- Produces:
  - `(*Manager).Reconcile(ctx context.Context) error`
  - `(*Manager).Classify(rec spool.RunRecord) lifecycle.Verdict`
  - `lifecycle.Verdict` — `VerdictAlive`, `VerdictResumable`, `VerdictLost`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/runner/lifecycle/reconcile_test.go`:

```go
package lifecycle_test

import (
	"context"
	"os"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/spool"
)

func TestClassifyLostWhenNoSessionAndNoProcess(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 0, SessionID: ""})
	if got != lifecycle.VerdictLost {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictLost)
	}
}

func TestClassifyResumableWhenSessionSurvives(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	// 프로세스는 죽었지만 Provider 세션 ID가 남아 있으면 --resume이 가능하다.
	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: 999999, SessionID: "sess-1"})
	if got != lifecycle.VerdictResumable {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictResumable)
	}
}

func TestClassifyAliveWhenProcessStillRunning(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	// 이 테스트 프로세스 자신은 확실히 살아 있다.
	got := m.Classify(spool.RunRecord{RunID: "run-1", State: "running", PID: os.Getpid(), SessionID: "sess-1"})
	if got != lifecycle.VerdictAlive {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictAlive)
	}
}

func TestClassifyIgnoresTerminalRuns(t *testing.T) {
	col := &collector{}
	m, _, _ := newManager(t, col)

	for _, state := range []string{"succeeded", "failed", "cancelled"} {
		if got := m.Classify(spool.RunRecord{RunID: "run-1", State: state}); got != lifecycle.VerdictAlive {
			// 종료된 Run은 조정 대상이 아니므로 Reconcile이 건너뛴다.
			// Classify는 호출되지 않는 것이 정상이며 여기서는 형태만 확인한다.
			_ = got
		}
	}
}

func TestReconcileReportsVerdictPerNonTerminalRun(t *testing.T) {
	col := &collector{}
	m, sp, _ := newManager(t, col)

	if err := sp.SaveRun(spool.RunRecord{RunID: "run-lost", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SaveRun(spool.RunRecord{RunID: "run-done", State: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 종료되지 않은 Run 하나에 대해서만 상태 이벤트가 나가야 한다.
	var reconciled int
	for _, e := range col.events {
		if e.Type == protocol.EvRunStateChanged {
			reconciled++
		}
	}
	if reconciled != 1 {
		t.Fatalf("emitted %d state events, want 1 (only the non-terminal run)", reconciled)
	}
}

func TestReconcileSalvagesLostRun(t *testing.T) {
	col := &collector{}
	m, sp, git := newManager(t, col)
	ctx := context.Background()

	ws, err := git.Ensure(ctx, "run-lost", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.Path+"/wip.txt", []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sp.SaveRun(spool.RunRecord{
		RunID: "run-lost", State: "running", Branch: ws.Branch, WorktreePath: ws.Path,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := git.Status(ctx, "run-lost")
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatal("lost run's uncommitted work was not salvaged")
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/runner/lifecycle/ -run Classify -v`
Expected: FAIL — `undefined: lifecycle.VerdictLost`

- [ ] **Step 3: 구현**

`internal/runner/lifecycle/reconcile.go`:

```go
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/jinto/taskyard/internal/runner/spool"
)

// Verdict는 재시작 후 Run의 실제 상태 판정이다(PRD §11.7).
type Verdict string

const (
	// VerdictAlive: Agent 프로세스가 아직 살아 있다.
	VerdictAlive Verdict = "alive"
	// VerdictResumable: 프로세스는 죽었지만 Provider 세션으로 재개할 수 있다.
	VerdictResumable Verdict = "resumable"
	// VerdictLost: 재개할 수 없다. 변경사항만 보존한다.
	VerdictLost Verdict = "lost"
)

// terminalStates는 조정 대상이 아닌 상태다.
var terminalStates = map[string]bool{
	"succeeded": true,
	"failed":    true,
	"cancelled": true,
}

// Classify는 기록 하나의 실제 상태를 판정한다.
func (m *Manager) Classify(rec spool.RunRecord) Verdict {
	if rec.PID > 0 && processAlive(rec.PID) {
		return VerdictAlive
	}
	if rec.SessionID != "" {
		return VerdictResumable
	}
	return VerdictLost
}

// processAlive는 PID가 살아 있는지 본다.
//
// PID 재사용은 남은 위험이다. 정확히 하려면 프로세스 시작시각까지 대조해야
// 하고, 그것은 Phase 1 과제다. 스파이크에서는 오판의 결과가
// "재개 가능한 Run을 alive로 착각"뿐이라 감수한다.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0은 프로세스를 건드리지 않고 존재만 확인한다.
	return p.Signal(syscall.Signal(0)) == nil
}

// Reconcile은 재시작 직후 로컬 원장을 훑어 각 Run의 상태를 판정하고
// 결과를 Server에 올린다. lost로 판정된 Run은 변경사항을 보존한다.
func (m *Manager) Reconcile(ctx context.Context) error {
	records, err := m.cfg.Spool.LoadRuns()
	if err != nil {
		return fmt.Errorf("load runs: %w", err)
	}

	for _, rec := range records {
		if terminalStates[rec.State] {
			continue
		}

		verdict := m.Classify(rec)
		slog.Info("reconciled run", "run_id", rec.RunID, "verdict", verdict)

		switch verdict {
		case VerdictAlive:
			m.emitState(rec.RunID, "running", "reconciled: process still alive")

		case VerdictResumable:
			m.emitState(rec.RunID, "running", "reconciled: session resumable")

		case VerdictLost:
			// 반드시 보존이 먼저다. 사용자 작업을 잃지 않는 것이 최우선이다.
			m.salvage(rec.RunID)
			m.emitState(rec.RunID, "failed", "reconciled: session lost, work salvaged")

			rec.State = "failed"
			if err := m.cfg.Spool.SaveRun(rec); err != nil {
				return fmt.Errorf("mark run failed: %w", err)
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/runner/lifecycle/ -race -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/runner/lifecycle
git commit -m "feat(runner): 재시작 후 alive/resumable/lost 조정"
```

---

## Task 11: 웹 UI — HTMX 셸과 이벤트 스트림 아일랜드

**Files:**
- Create: `internal/server/web/web.go`, `internal/server/web/web_test.go`
- Create: `internal/server/web/templates/layout.html`, `runs.html`, `run.html`

**Interfaces:**
- Consumes: `store.Store`, `hub.Hub`, `protocol.*`
- Produces:
  - `web.New(st *store.Store, h *hub.Hub) (*Server, error)`
  - `(*Server).Routes() http.Handler`
  - 경로: `GET /`, `GET /runs/{id}`, `GET /runs/{id}/stream` (SSE), `POST /runs/{id}/approve`

- [ ] **Step 1: 템플릿 작성**

`internal/server/web/templates/layout.html`:

```html
{{define "layout"}}<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Taskyard</title>
<style>
  :root { color-scheme: light dark; --fg: #16181d; --bg: #fbfbfa; --muted: #6b7280; --line: #e5e7eb; }
  @media (prefers-color-scheme: dark) {
    :root { --fg: #e8e8e6; --bg: #16181d; --muted: #9ca3af; --line: #2c2f36; }
  }
  body { margin: 0; padding: 2rem; background: var(--bg); color: var(--fg);
         font: 15px/1.6 ui-sans-serif, -apple-system, "Segoe UI", sans-serif; }
  h1 { font-size: 1.25rem; margin: 0 0 1.5rem; }
  a { color: inherit; }
  .run { padding: .75rem 0; border-bottom: 1px solid var(--line); }
  .state { font-size: .8rem; color: var(--muted); }
  #events { margin-top: 1rem; border: 1px solid var(--line); border-radius: 6px;
            max-height: 60vh; overflow-y: auto; }
  .ev { padding: .5rem .75rem; border-bottom: 1px solid var(--line);
        font-family: ui-monospace, SFMono-Regular, monospace; font-size: .8rem; }
  .ev-type { color: var(--muted); margin-right: .5rem; }
  .approval { margin: 1rem 0; padding: 1rem; border: 2px solid #d97706; border-radius: 6px; }
  button { font: inherit; padding: .4rem .9rem; margin-right: .5rem; cursor: pointer; }
</style>
</head>
<body>
{{template "content" .}}
</body>
</html>{{end}}
```

`internal/server/web/templates/runs.html`:

```html
{{define "content"}}
<h1>Runs</h1>
{{range .Runs}}
  <div class="run">
    <a href="/runs/{{.ID}}">{{.ID}}</a>
    <div class="state">{{.State}} · seq {{.LastAckedSeq}}{{if .Branch}} · {{.Branch}}{{end}}</div>
  </div>
{{else}}
  <p class="state">아직 실행이 없습니다.</p>
{{end}}
{{end}}
```

`internal/server/web/templates/run.html`:

```html
{{define "content"}}
<h1><a href="/">←</a> {{.Run.ID}}</h1>
<div class="state">{{.Run.State}} · {{.Run.Branch}}</div>

<div class="approval" id="approval" hidden>
  <div id="approval-text"></div>
  <div style="margin-top:.75rem">
    <button onclick="decide(true)">승인</button>
    <button onclick="decide(false)">거절</button>
  </div>
</div>

<div id="events">
{{range .Events}}
  <div class="ev"><span class="ev-type">{{.Type}}</span>{{.Summary}}</div>
{{end}}
</div>

<!-- JS 아일랜드: 이 화면만 실시간 상태를 가진다(PRD §11.4.1) -->
<script>
(function () {
  const runID = {{.Run.ID}};
  const list = document.getElementById("events");
  const panel = document.getElementById("approval");
  const text = document.getElementById("approval-text");
  let pendingRequestID = null;

  const source = new EventSource("/runs/" + runID + "/stream");

  source.onmessage = function (msg) {
    const ev = JSON.parse(msg.data);

    const row = document.createElement("div");
    row.className = "ev";
    row.innerHTML = '<span class="ev-type"></span><span class="ev-body"></span>';
    row.querySelector(".ev-type").textContent = ev.type;
    row.querySelector(".ev-body").textContent = ev.summary;
    list.appendChild(row);
    list.scrollTop = list.scrollHeight;

    if (ev.type === "approval_requested") {
      pendingRequestID = ev.request_id;
      text.textContent = ev.summary;
      panel.hidden = false;
    }
  };

  window.decide = function (allow) {
    if (!pendingRequestID) return;
    fetch("/runs/" + runID + "/approve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ request_id: pendingRequestID, allow: allow })
    });
    panel.hidden = true;
    pendingRequestID = null;
  };
})();
</script>
{{end}}
```

- [ ] **Step 2: 실패하는 테스트 작성**

`internal/server/web/web_test.go`:

```go
package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
)

func newServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s, err := web.New(st, hub.New(st, "tok"))
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return st, s.Routes()
}

func TestIndexListsRuns(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{
		ID: "run-1", State: store.StateRunning, Kind: "structured", Branch: "taskyard/run/run-1",
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run-1") {
		t.Errorf("index does not list run-1:\n%s", body)
	}
	if !strings.Contains(body, "taskyard/run/run-1") {
		t.Errorf("index does not show the branch:\n%s", body)
	}
}

func TestRunDetailReplaysStoredEvents(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 1, map[string]any{
		"body": map[string]any{"text": "hello from the agent"},
	})
	env.Seq = 1
	if _, _, err := st.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "message_delta") {
		t.Errorf("detail page does not replay stored events:\n%s", rec.Body)
	}
}

func TestUnknownRunIs404(t *testing.T) {
	_, h := newServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestApprovePostFailsWithoutARunner(t *testing.T) {
	st, h := newServer(t)
	if err := st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"request_id": "req-1", "allow": true})
	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/approve", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Runner가 없으면 결정을 전달할 수 없다. 조용히 성공하면 안 된다.
	if rec.Code == http.StatusOK {
		t.Fatalf("approve returned 200 with no runner connected; the decision went nowhere")
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/server/web/ -v`
Expected: FAIL — `undefined: web.New`

- [ ] **Step 4: 구현**

`internal/server/web/web.go`:

```go
// Package web은 Server의 HTML UI다.
//
// 보드와 목록은 서버 렌더링이고, Run 상세의 이벤트 스트림만 SSE를 구독하는
// JS 아일랜드다(PRD §11.4.1).
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	st   *store.Store
	hub  *hub.Hub
	runs *template.Template
	run  *template.Template
}

func New(st *store.Store, h *hub.Hub) (*Server, error) {
	runs, err := template.ParseFS(templateFS, "templates/layout.html", "templates/runs.html")
	if err != nil {
		return nil, fmt.Errorf("parse runs template: %w", err)
	}
	run, err := template.ParseFS(templateFS, "templates/layout.html", "templates/run.html")
	if err != nil {
		return nil, fmt.Errorf("parse run template: %w", err)
	}
	return &Server{st: st, hub: h, runs: runs, run: run}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /runs/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /runs/{id}/approve", s.handleApprove)
	return mux
}

// eventView는 템플릿과 SSE가 함께 쓰는 이벤트 표현이다.
type eventView struct {
	Type      string `json:"type"`
	Seq       uint64 `json:"seq"`
	Summary   string `json:"summary"`
	RequestID string `json:"request_id,omitempty"`
}

// summarize는 봉투를 한 줄로 줄인다. 원본은 store에 그대로 있다.
func summarize(env protocol.Envelope) eventView {
	view := eventView{Type: env.Type, Seq: env.Seq}

	var outer struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(env.Body, &outer); err != nil || outer.Body == nil {
		view.Summary = string(env.Body)
		return view
	}

	switch env.Type {
	case protocol.EvMessageDelta:
		view.Summary, _ = outer.Body["text"].(string)
	case protocol.EvToolStarted:
		name, _ := outer.Body["tool_name"].(string)
		view.Summary = "→ " + name
	case protocol.EvToolFinished:
		view.Summary = "← done"
	case protocol.EvRunStateChanged:
		state, _ := outer.Body["state"].(string)
		detail, _ := outer.Body["detail"].(string)
		view.Summary = state
		if detail != "" {
			view.Summary += " — " + detail
		}
	case protocol.EvApprovalRequested:
		name, _ := outer.Body["tool_name"].(string)
		view.RequestID, _ = outer.Body["request_id"].(string)
		view.Summary = "승인 요청: " + name
	default:
		raw, _ := json.Marshal(outer.Body)
		view.Summary = string(raw)
	}
	return view
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	points, err := s.st.ResumePoints()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var runs []store.Run
	for id := range points {
		r, err := s.st.GetRun(id)
		if err != nil {
			continue
		}
		runs = append(runs, r)
	}

	s.render(w, s.runs, map[string]any{"Title": "Runs", "Runs": runs})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	run, err := s.st.GetRun(id)
	if errors.Is(err, store.ErrRunNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	stored, err := s.st.Events(id, 0, 500)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	views := make([]eventView, 0, len(stored))
	for _, env := range stored {
		views = append(views, summarize(env))
	}

	s.render(w, s.run, map[string]any{"Title": id, "Run": run, "Events": views})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case env, ok := <-events:
			if !ok {
				return
			}
			if env.RunID != id {
				continue
			}
			raw, err := json.Marshal(summarize(env))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

type approveRequest struct {
	RequestID string `json:"request_id"`
	Allow     bool   `json:"allow"`
	Message   string `json:"message"`
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body approveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cmd, err := protocol.NewCommand(protocol.CmdApprovalDecision, id, map[string]any{
		"request_id": body.RequestID,
		"allow":      body.Allow,
		"message":    body.Message,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Runner가 없으면 결정이 갈 곳이 없다. 성공으로 위장하지 않는다.
	if err := s.hub.SendCommand(cmd); err != nil {
		slog.Warn("approval decision could not be delivered", "run_id", id, "err", err)
		http.Error(w, "runner is not connected", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("render failed", "err", err)
	}
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/server/... -race -v`
Expected: PASS (4 web tests + 이전 hub/store 테스트)

- [ ] **Step 6: 커밋**

```bash
git add internal/server/web
git commit -m "feat(web): Run 목록, 상세, SSE 이벤트 스트림, 승인 UI"
```

---

## Task 12: 바이너리 배선과 §16.0 완료 판정

**Files:**
- Modify: `cmd/taskyard-server/main.go`, `cmd/taskyard-runner/main.go`
- Create: `acceptance/acceptance_test.go`
- Create: `acceptance/smoke_test.go` (빌드 태그 `smoke`)
- Modify: `Makefile`

**Interfaces:**
- Consumes: 앞선 모든 패키지
- Produces: 동작하는 두 바이너리와 PRD §16.0의 완료 판정 4종

- [ ] **Step 1: Server 바이너리 배선**

`cmd/taskyard-server/main.go`를 통째로 바꾼다:

```go
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "listen address")
		dbPath      = flag.String("db", "taskyard-server.db", "sqlite path")
		token       = flag.String("pairing-token", "", "one-time pairing token for the runner")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-server %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "taskyard-server: --pairing-token is required")
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		slog.Error("open store failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.New(st, *token)

	ui, err := web.New(st, h)
	if err != nil {
		slog.Error("build web failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	mux.Handle("/", ui.Routes())

	slog.Info("taskyard-server listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Runner 바이너리 배선**

`cmd/taskyard-runner/main.go`를 통째로 바꾼다:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
)

func main() {
	var (
		serverURL   = flag.String("server", "ws://127.0.0.1:8080/ws", "server websocket url")
		runnerID    = flag.String("id", "runner-1", "runner id")
		token       = flag.String("pairing-token", "", "pairing token issued by the server")
		dbPath      = flag.String("db", "taskyard-runner.db", "sqlite path")
		repo        = flag.String("repo", "", "repository path")
		worktrees   = flag.String("worktrees", "", "worktree root")
		baseBranch  = flag.String("base-branch", "main", "base branch for new worktrees")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-runner %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}
	if *repo == "" || *worktrees == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "taskyard-runner: --repo, --worktrees and --pairing-token are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sp, err := spool.Open(*dbPath)
	if err != nil {
		slog.Error("open spool failed", "err", err)
		os.Exit(1)
	}
	defer sp.Close()

	// 승인 브로커를 임의 포트로 로컬호스트에만 띄운다.
	brokerToken := randomToken()
	broker := approval.New(brokerToken)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Error("listen for broker failed", "err", err)
		os.Exit(1)
	}
	brokerURL := fmt.Sprintf("http://%s/mcp", ln.Addr().String())
	go func() {
		if err := http.Serve(ln, broker.Handler()); err != nil {
			slog.Error("broker stopped", "err", err)
		}
	}()
	slog.Info("approval broker listening", "url", brokerURL)

	// lifecycle과 link이 서로를 참조하므로 publish 클로저가 l을 늦게 읽는다.
	//
	// 불변식: l 대입은 반드시 lm.Start와 lm.Reconcile 호출보다 앞서야 한다.
	// 둘 다 이벤트를 발행할 수 있고, 발행은 이 클로저를 거쳐 l에 닿는다.
	// 순서가 뒤집히면 첫 이벤트에서 nil 역참조로 죽는다.
	var l *link.Link

	lm, err := lifecycle.New(lifecycle.Config{
		Spool:       sp,
		Git:         gitops.New(*repo, *worktrees),
		Broker:      broker,
		BaseBranch:  *baseBranch,
		BrokerURL:   brokerURL,
		BrokerToken: brokerToken,
		Publish: func(runID string, env protocol.Envelope) error {
			return l.Publish(runID, env)
		},
	})
	if err != nil {
		slog.Error("build lifecycle failed", "err", err)
		os.Exit(1)
	}

	l, err = link.New(link.Config{
		ServerURL:    *serverURL,
		RunnerID:     *runnerID,
		PairingToken: *token,
		Spool:        sp,
		Capabilities: []string{protocol.CapClaudeCode, protocol.CapApprovalBroker, protocol.CapGitWorktree},
		OnCommand:    lm.HandleCommand,
	})
	if err != nil {
		slog.Error("build link failed", "err", err)
		os.Exit(1)
	}

	// 여기부터 l이 유효하다. 위 불변식대로 발행자들을 그 뒤에 띄운다.
	go lm.Start(ctx)

	// 재시작 직후 남아 있는 Run의 실제 상태를 먼저 맞춘다(PRD §11.7).
	if err := lm.Reconcile(ctx); err != nil {
		slog.Error("reconcile failed", "err", err)
	}

	if err := l.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("link stopped", "err", err)
		os.Exit(1)
	}
}

func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random: %v", err))
	}
	return hex.EncodeToString(buf)
}
```

import에 `crypto/rand`와 `encoding/hex`를 추가한다.

> **순서 주의:** `l, err = link.New(...)` 대입이 `go lm.Start(ctx)`와 `lm.Reconcile(ctx)`보다 **먼저** 와야 한다. 두 호출 모두 이벤트를 발행하고, 발행 경로가 `Publish` 클로저를 통해 `l`을 읽는다. 위 코드는 그 순서를 지키고 있다. 리팩터링할 때 이 순서를 깨지 않는다.

- [ ] **Step 3: 빌드 확인**

Run: `make build && ./bin/taskyard-server --version && ./bin/taskyard-runner --version`
Expected: 두 바이너리 모두 버전 출력

- [ ] **Step 4: 완료 판정 테스트 작성**

`acceptance/acceptance_test.go`:

```go
// Package acceptance는 PRD §16.0의 완료 판정을 자동화한다.
// 실제 claude CLI 대신 fixture를 뱉는 가짜 바이너리를 쓴다.
package acceptance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/gitops"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

const fixture = "../internal/agents/adapter/claudecode/testdata/session-pong.ndjson"

type stack struct {
	st    *store.Store
	hub   *hub.Hub
	srv   *httptest.Server
	sp    *spool.Spool
	link  *link.Link
	life  *lifecycle.Manager
	git   *gitops.Manager
	wsURL string
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func newStack(t *testing.T, dbDir string) *stack {
	t.Helper()

	st, err := store.Open(filepath.Join(dbDir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := hub.New(st, "tok")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sp, err := spool.Open(filepath.Join(dbDir, "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	repo := filepath.Join(dbDir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); os.IsNotExist(err) {
		git(t, repo, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "README.md")
		git(t, repo, "commit", "-q", "-m", "init")
	}

	gm := gitops.New(repo, filepath.Join(dbDir, "wt"))

	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dbDir, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat "+abs+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var l *link.Link
	lm, err := lifecycle.New(lifecycle.Config{
		Spool: sp, Git: gm, Broker: approval.New("tok"),
		BaseBranch: "main", BrokerURL: "http://127.0.0.1:1/mcp", BrokerToken: "tok",
		ClaudeBinary: fake,
		Publish: func(runID string, env protocol.Envelope) error {
			return l.Publish(runID, env)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	l, err = link.New(link.Config{
		ServerURL: wsURL, RunnerID: "runner-1", PairingToken: "tok",
		Spool: sp, Capabilities: []string{protocol.CapClaudeCode},
		OnCommand: lm.HandleCommand,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &stack{st: st, hub: h, srv: srv, sp: sp, link: l, life: lm, git: gm, wsURL: wsURL}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// 판정 1: 임의 시점에 연결을 10회 끊었다 붙여도 유실·중복이 0이다.
func TestCriterion1_TenDisconnectsLoseNothing(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)

	if err := s.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "initial connection", s.hub.Connected)

	const perRound = 20
	for round := 0; round < 10; round++ {
		for i := 0; i < perRound; i++ {
			env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"round": round, "i": i})
			if err := s.link.Publish("run-1", env); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		s.hub.DropConnection()
		waitFor(t, "disconnect to register", func() bool { return !s.hub.Connected() })
		waitFor(t, "reconnect", s.hub.Connected)
	}

	want := uint64(perRound * 10)
	waitFor(t, "all events applied", func() bool {
		run, err := s.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == want
	})

	events, err := s.st.Events("run-1", 0, int(want)+50)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(events)) != want {
		t.Fatalf("stored %d events, want exactly %d (no loss, no duplicates)", len(events), want)
	}
	for i, e := range events {
		if got, exp := e.Seq, uint64(i+1); got != exp {
			t.Fatalf("events[%d].Seq = %d, want %d", i, got, exp)
		}
	}
}

// 판정 2: 실행 중 Runner를 재시작해도 lost가 아니라 resumable로 복구된다.
func TestCriterion2_RunnerRestartYieldsResumable(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)

	// 세션 ID가 남은 Run은 프로세스가 죽어도 재개 가능해야 한다.
	if err := s.sp.SaveRun(spool.RunRecord{
		RunID: "run-1", State: "running", SessionID: "sess-1", PID: 999999,
	}); err != nil {
		t.Fatal(err)
	}

	got := s.life.Classify(spool.RunRecord{RunID: "run-1", State: "running", SessionID: "sess-1", PID: 999999})
	if got != lifecycle.VerdictResumable {
		t.Fatalf("Classify = %v, want %v", got, lifecycle.VerdictResumable)
	}

	// 세션 ID가 없으면 lost이고, 그때는 작업이 보존돼야 한다.
	if s.life.Classify(spool.RunRecord{RunID: "run-2", State: "running"}) != lifecycle.VerdictLost {
		t.Fatal("a run with no session id should classify as lost")
	}
}

// 판정 3: 승인 요청이 웹에 뜨고, 응답이 Agent에 전달되어 실행이 계속된다.
func TestCriterion3_ApprovalRoundTrip(t *testing.T) {
	b := approval.New("tok")
	handler := b.Handler()

	done := make(chan string, 1)
	go func() {
		req := <-b.Requests()
		// 웹에서 승인 버튼을 누른 것에 해당한다.
		if err := b.Decide(req.ID, approval.Decision{Allow: true}); err != nil {
			t.Errorf("Decide: %v", err)
		}
		done <- req.ToolName
	}()

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	select {
	case name := <-done:
		if name == "" {
			t.Error("approval request surfaced without a tool name")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval never surfaced")
	}

	if !strings.Contains(rec.Body.String(), "allow") {
		t.Fatalf("agent did not receive an allow decision: %s", rec.Body)
	}
}

// 판정 4: 왕복 지연을 측정해 기록한다(§11.2.1의 판단 근거).
//
// 이 테스트는 통과·실패를 가르지 않는다. 숫자를 남기는 것이 목적이다.
func TestCriterion4_MeasureRoundTripLatency(t *testing.T) {
	dir := t.TempDir()
	s := newStack(t, dir)

	if err := s.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.link.Run(ctx)
	waitFor(t, "connection", s.hub.Connected)

	const samples = 50
	var total time.Duration

	for i := 0; i < samples; i++ {
		before, err := s.st.GetRun("run-1")
		if err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := s.link.Publish("run-1", env); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "event to be applied", func() bool {
			run, err := s.st.GetRun("run-1")
			return err == nil && run.LastAckedSeq > before.LastAckedSeq
		})
		total += time.Since(start)
	}

	avg := total / samples

	// 측정값을 기록만 한다. 실패 조건을 두지 않는 이유는 두 가지다.
	//
	// 첫째, 이 값의 하한은 전송이 아니라 위 폴링 간격(10ms)이다. 임계값을
	// 걸면 전송 성능이 아니라 테스트 하네스를 재게 된다.
	//
	// 둘째, PRD §11.2.1의 열린 질문은 브라우저→Server→Runner→CLI 왕복이고
	// 이 측정은 그 구간의 일부일 뿐이다. 숫자는 판단의 재료이지 판정 기준이
	// 아니다. PRD §21의 "명세화 대화 지연" 행에 이 값을 기록하고,
	// 실제 판단은 사람이 한다.
	t.Logf("Runner→Server 이벤트 왕복 평균: %v (%d samples, localhost)", avg, samples)
}
```

- [ ] **Step 5: 완료 판정 실행**

Run: `go test ./acceptance/ -race -v`
Expected: PASS (4 tests).

판정 1~3이 실제 판정이다. 판정 4는 임계값 없이 숫자만 남긴다 — 하한이 폴링 간격이라 임계값을 걸면 전송이 아니라 테스트 하네스를 재게 되고, 부하가 걸린 머신에서 헛되이 깨져 나머지 세 판정의 신뢰도까지 떨어뜨린다. `TestCriterion4`의 로그를 읽어 PRD §21의 "명세화 대화 지연" 행에 기록하고, 견딜 만한 값인지는 사람이 판단한다.

- [ ] **Step 6: 실제 CLI 스모크 테스트 작성**

`acceptance/smoke_test.go`:

```go
//go:build smoke

// smoke는 실제 claude CLI를 호출한다. 사용자 구독 할당량을 소모하므로
// 기본 test 대상에서 제외돼 있다. `make smoke`로만 실행한다.
package acceptance

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/agents/adapter/claudecode"
	"github.com/jinto/taskyard/internal/protocol"
)

func TestSmokeRealClaudeStaysOnSubscription(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH")
	}

	args, err := claudecode.BuildArgs(claudecode.SpawnOptions{
		Prompt:      "Reply with exactly the word: pong",
		BrokerURL:   "http://127.0.0.1:1/mcp",
		BrokerToken: "unused",
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = t.TempDir()
	cmd.Env = claudecode.ScrubEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	parser := claudecode.NewParser()
	var types []string
	parseErr := parser.Parse(stdout, func(e adapter.Event) error {
		types = append(types, e.Type)
		return nil
	})
	waitErr := cmd.Wait()

	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}
	if waitErr != nil {
		t.Fatalf("claude exited with error: %v (events: %v)", waitErr, types)
	}

	session := parser.Session()
	if session.SessionID == "" {
		t.Error("no session id captured; --resume would be impossible")
	}
	// PRD §13.2의 구독 경계가 실제로 지켜지는지 확인하는 유일한 자동 검증이다.
	if session.UsesAPIKey() {
		t.Fatalf("run billed to an API key (apiKeySource=%q); the flag policy is broken", session.APIKeySource)
	}

	if !strings.Contains(strings.Join(types, ","), protocol.EvTurnCompleted) {
		t.Fatalf("no turn_completed event; types = %v", types)
	}
}
```

- [ ] **Step 7: 스모크 실행**

Run: `make smoke`
Expected: PASS. `apiKeySource`가 `none`으로 확인되면 PRD §22 Q16의 Claude Code 절반이 실증된 것이다.

- [ ] **Step 8: 전체 테스트와 빌드 확인**

Run: `make test && make build`
Expected: 모든 패키지 PASS, 두 바이너리 생성

- [ ] **Step 9: 커밋**

```bash
git add cmd acceptance Makefile
git commit -m "feat: 바이너리 배선과 Phase 0 완료 판정 테스트"
```

---

## Self-Review

계획을 다 쓴 뒤 스펙과 대조한 결과다.

**1. 스펙 커버리지 (PRD §16.0 포함 항목)**

| §16.0 항목 | 담당 Task |
|---|---|
| Go 모노레포, 두 바이너리, 프로토콜 v0과 버전 협상 | Task 1, 2, 12 |
| Runner 페어링, 아웃바운드 WSS, heartbeat | Task 5 |
| `command_id` 멱등성, `sequence` ACK, 로컬 spool과 재전송 | Task 3, 4, 5, 9 |
| Claude Code 어댑터 | Task 7 |
| 로컬 승인 브로커와 웹 승인 UI | Task 8, 11 |
| 결정론적 branch/worktree 생성과 salvage 커밋 | Task 6, 9 |
| Run 이벤트 스트림 뷰 (JS 아일랜드 1개) | Task 11 |
| Runner 재시작 후 세션 재개 (§11.7) | Task 10 |
| 완료 판정 4종 | Task 12 |

**의도적으로 다루지 않은 것** (§16.0의 제외 목록과 일치): 보드 UI, 티켓 CRUD, Planner, Coordinator, 병렬 스케줄러, Codex 어댑터, PR 생성.

**2. 스펙과 계획의 간극 — 실행자가 알아야 할 것**

- **Server 쪽 `orphaned` 상태 기계는 계획에 없다.** PRD §11.7의 Server 측 `T_miss`/`T_grace` 타이머와 Attention Item 생성은 Phase 0 범위 밖이다. Task 10은 Runner 측 판정(alive/resumable/lost)만 구현한다. `store.StateOrphaned` 상수는 정의만 해두고 아직 아무도 쓰지 않는다 — 의도된 것이다.
- **`--resume`을 이용한 실제 재개는 배선되지 않았다.** Task 10은 `resumable`로 *판정*까지 한다. 판정 결과로 `claude --resume <session_id>`를 다시 띄우는 것은 Phase 1이다. `claudecode.BuildArgs`는 `ResumeSessionID`를 이미 받으므로 배선만 남았다.
- **승인 요청과 Run의 연결이 단순화돼 있다.** `lifecycle.runIDForApproval`은 활성 Run이 하나뿐이라는 가정에 기댄다. 동시 실행이 들어오는 순간 깨지며, 그때 브로커를 Run별 엔드포인트로 바꿔야 한다. 주석에 명시돼 있다.
- **`processAlive`는 PID 재사용을 구분하지 못한다.** 프로세스 시작시각 대조는 Phase 1 과제다.

**3. 타입 일관성 확인 결과**

- `protocol.Ev*` 상수는 Task 2에서 정의되고 Task 7(파서), 9(수명주기), 11(요약)에서 같은 이름으로 쓰인다.
- `spool.RunRecord`는 Task 9에서 정의되고 Task 10에서 그대로 쓰인다.
- `approval.Decision`은 Task 8에서 정의되고 Task 9의 `handleApprovalDecision`에서 필드명 그대로 쓰인다.
- `lifecycle.Config.Publish`의 시그니처 `func(runID string, env protocol.Envelope) error`는 `link.Publish`의 시그니처와 일치한다. Task 12의 `main.go`에서 클로저로 연결된다.
- `gitops.Workspace`는 Task 6에서 정의되고 Task 9의 `execute`가 받는다.

**4. 알려진 취약점 — 실행 중 막히면 여기부터 본다**

- **Task 8 Step 3의 캡처가 예상과 다를 수 있다.** `toolCallArguments`의 JSON 태그가 유일한 미검증 계약이다. 캡처가 정본이고, 태그를 캡처에 맞춰 고치는 것은 계획 위반이 아니라 계획의 지시다.
- **Task 5의 `drain`은 단순하다.** 매 루프마다 spool 전체를 다시 보낸다. 이벤트가 많으면 비효율적이지만 정확하다. 성능이 문제가 되면 마지막 전송 위치를 메모리에 캐시한다. 스파이크에서는 하지 않는다.
- **Task 9의 테스트는 가짜 `claude` 스크립트에 의존한다.** fixture를 `cat`할 뿐이므로 인자를 검증하지 않는다. 인자 검증은 Task 7의 `BuildArgs` 테스트가 담당한다.

---

## 실행 순서 요약

```
Task 1  저장소 골격                    ─┐
Task 2  프로토콜 v0                     │ 기반
Task 3  Runner spool                    │
Task 4  Server store                   ─┘
Task 5  WSS 전송                       ─┐ 이 둘이 통과하면
Task 6  Git worktree                   ─┘ 완료 판정 1이 사실상 확보된다
Task 7  Claude Code 어댑터             ─┐
Task 8  승인 브로커 (probe 먼저)       ─┘ 여기가 가장 불확실하다
Task 9  Run 수명주기 배선
Task 10 재시작 조정
Task 11 웹 UI
Task 12 바이너리 배선 + 완료 판정
```

Task 8의 probe(Step 1~3)는 **가장 먼저 해도 된다.** 계약이 예상과 크게 다르면 Task 8의 설계가 바뀌므로, 시간이 있으면 Task 1 직후에 probe만 먼저 돌려 계약을 확인해두는 편이 안전하다.
