# Taskyard

> Run your coding agents like a team.

티켓을 작업 단위로 삼아, 개발자 머신에서 실행되는 Claude Code를 배정하고 개입하며
GitHub PR까지 관리하는 개발 에이전트 관제 시스템입니다. 웹 서버(`taskyard-server`)와
에이전트 러너(`taskyard-runner`) 두 개의 Go 데몬으로 이루어지며, 러너는 서버로
아웃바운드 연결만 겁니다.

**상태: Phase 1 척추.** Phase 0이 아키텍처 전제(이벤트 무손실 전달, 러너 재시작
복구, 사람의 도구 승인, 구독 과금 경계)를 실증했고, 지금은 그 배관 위에
"프로젝트 → 이슈 → [실행] → 에이전트 → 결과가 이슈에 남음"이 도는 단계입니다.

- 스펙: [`taskyard-prd-v1.md`](taskyard-prd-v1.md)
- Phase 0 결과와 이월 사항: [`docs/phase0-findings.md`](docs/phase0-findings.md)

```sh
make build   # bin/taskyard-server, bin/taskyard-runner
make test    # fixture 기반, 실제 claude CLI를 호출하지 않음
make smoke   # 실제 claude CLI 1회 호출 (구독 할당량 소모)
```

## 돌려 보기

```sh
bin/taskyard-server --pairing-token secret
bin/taskyard-runner --pairing-token secret \
  --allow-repo /Users/me/code/shop --allow-repo /Users/me/code/blog \
  --worktrees /Users/me/.taskyard/worktrees
```

브라우저에서 `http://127.0.0.1:8080` → 프로젝트 만들기(저장소 경로는 러너의
`--allow-repo` 중 하나) → 이슈 만들기 → [실행]. 프로젝트의 실행 템플릿이 이슈로
채워져 에이전트에게 넘어가고, 이슈 상세에서 Run과 상태를 봅니다. 승인 요청이
잦으면 프로젝트 설정의 "승인 없이 허용할 도구"에 `Edit`, `Bash(go test:*)`처럼
한 줄에 하나씩 적습니다(`--allowedTools`로 넘어갑니다).

실행이 성공하면 러너가 브랜치를 `origin`에 push하고 `gh`로 PR을 엽니다(사용자의
`gh auth login`을 그대로 씁니다). 본문은 에이전트가 `.taskyard/summary.md`에 남긴
변경 설명입니다. 러너가 PR 상태를 주기적으로 보고(`--pr-poll`, 기본 1분), merge가
확인되면 이슈는 done이 되고 worktree는 정리됩니다. 원격이 없는 저장소는 프로젝트
설정에서 "PR 만들기"를 끄세요.

## 이름에 대하여

npm의 `taskyard`(kolbyjayce — 에이전트용 todo MCP 서버)와는 무관한 별개 프로젝트입니다.
Taskyard라는 이름과 로고는 아래 라이선스에 포함되지 않습니다.

## 라이선스

[Functional Source License 1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2).

- 사용·수정·기여·사내 사용은 자유입니다.
- 실질적으로 같은 기능을 **상업 제품이나 서비스**로 제공하는 것만 금지됩니다.
- 각 버전은 공개 2년 뒤 Apache-2.0으로 자동 전환됩니다.
