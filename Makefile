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
