# Taskyard

> Run your coding agents like a team.

티켓을 작업 단위로 삼아, 개발자 머신에서 실행되는 Claude Code를 배정하고 개입하며
GitHub PR까지 관리하는 개발 에이전트 관제 시스템입니다. 웹 서버(`taskyard-server`)와
에이전트 러너(`taskyard-runner`) 두 개의 Go 데몬으로 이루어지며, 러너는 서버로
아웃바운드 연결만 겁니다.

**상태: Phase 0 수직 스파이크.** 기능이 아니라 아키텍처 전제(이벤트 무손실 전달,
러너 재시작 복구, 사람의 도구 승인, 구독 과금 경계)를 실증한 단계입니다.

- 스펙: [`taskyard-prd-v1.md`](taskyard-prd-v1.md)
- Phase 0 결과와 이월 사항: [`docs/phase0-findings.md`](docs/phase0-findings.md)

```sh
make build   # bin/taskyard-server, bin/taskyard-runner
make test    # fixture 기반, 실제 claude CLI를 호출하지 않음
make smoke   # 실제 claude CLI 1회 호출 (구독 할당량 소모)
```

## 이름에 대하여

npm의 `taskyard`(kolbyjayce — 에이전트용 todo MCP 서버)와는 무관한 별개 프로젝트입니다.
Taskyard라는 이름과 로고는 아래 라이선스에 포함되지 않습니다.

## 라이선스

[Functional Source License 1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2).

- 사용·수정·기여·사내 사용은 자유입니다.
- 실질적으로 같은 기능을 **상업 제품이나 서비스**로 제공하는 것만 금지됩니다.
- 각 버전은 공개 2년 뒤 Apache-2.0으로 자동 전환됩니다.
