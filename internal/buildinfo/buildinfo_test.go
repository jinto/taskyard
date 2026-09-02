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
