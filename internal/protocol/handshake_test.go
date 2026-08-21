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
