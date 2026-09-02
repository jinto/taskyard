package protocol

import (
	"encoding/json"
	"strings"
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
	if strings.Contains(string(raw), `"seq"`) {
		t.Errorf("command envelope must omit seq, got %s", raw)
	}
}
