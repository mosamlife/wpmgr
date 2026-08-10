package agentcmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// GH #380: an unresolved secret must leave the wire entirely. The agent reads a
// present-but-empty `secret` as an instruction it can act on; a missing one it
// cannot. This is the invariant that stops a config push from deleting a
// working credential, so it is asserted on the encoded bytes, not the struct.
func TestEmailConfigRequest_NilSecretIsAbsentFromTheWire(t *testing.T) {
	body, err := json.Marshal(EmailConfigRequest{Provider: "smtp"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"secret"`) {
		t.Errorf("a nil secret must not appear on the wire at all, got %s", body)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["secret"]; present {
		t.Error("a nil secret must leave no `secret` key for the agent to act on")
	}
}

func TestEmailConfigRequest_SecretIsCarriedWhenResolved(t *testing.T) {
	secret := "the-working-password"
	body, err := json.Marshal(EmailConfigRequest{Provider: "smtp", Secret: &secret})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["secret"] != secret {
		t.Errorf("expected the resolved secret on the wire, got %v", decoded["secret"])
	}
}

// The same rule applies per named connection: a connection whose secret could
// not be resolved is pushed without one rather than with a blank one.
func TestEmailConnectionWire_NilSecretIsAbsentFromTheWire(t *testing.T) {
	body, err := json.Marshal(EmailConfigRequest{
		Provider:    "smtp",
		Connections: map[string]EmailConnectionWire{"relay": {Provider: "smtp"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"secret"`) {
		t.Errorf("a nil connection secret must not appear on the wire, got %s", body)
	}
}
