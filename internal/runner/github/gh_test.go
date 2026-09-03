package github

import (
	"encoding/json"
	"testing"
)

func TestSummarizeChecks(t *testing.T) {
	cases := []struct {
		name  string
		items []string
		want  string
	}{
		{"none", nil, "none"},
		{"all success", []string{`{"status":"COMPLETED","conclusion":"SUCCESS"}`, `{"state":"SUCCESS"}`}, "success"},
		{"skipped counts as success", []string{`{"status":"COMPLETED","conclusion":"SKIPPED"}`}, "success"},
		{"one pending", []string{`{"status":"COMPLETED","conclusion":"SUCCESS"}`, `{"status":"IN_PROGRESS","conclusion":""}`}, "pending"},
		{"status context pending", []string{`{"state":"PENDING"}`}, "pending"},
		{"failure wins over pending", []string{`{"status":"IN_PROGRESS"}`, `{"status":"COMPLETED","conclusion":"FAILURE"}`}, "failure"},
		{"status context error", []string{`{"state":"ERROR"}`}, "failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []json.RawMessage
			for _, it := range tc.items {
				raw = append(raw, json.RawMessage(it))
			}
			if got := summarizeChecks(raw); got != tc.want {
				t.Fatalf("summarizeChecks = %q, want %q", got, tc.want)
			}
		})
	}
}
