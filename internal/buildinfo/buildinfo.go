// Package buildinfo는 두 바이너리가 공유하는 버전 정보를 제공한다.
package buildinfo

// version은 릴리스 시 -ldflags로 덮어쓴다.
var version = "0.0.0-dev"

// protocolVersion은 Server와 Runner가 handshake에서 비교하는 값이다.
// 호환되지 않는 변경이 생기면 올린다.
const protocolVersion = 1

func Version() string { return version }

func ProtocolVersion() int { return protocolVersion }
