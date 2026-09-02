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
