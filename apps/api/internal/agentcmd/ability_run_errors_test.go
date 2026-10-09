package agentcmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestMinAgentVersionForRecoveryUndo_Pinned(t *testing.T) {
	const firstVersionWithRecoveryRevert = "0.61.157"
	if MinAgentVersionForRecoveryUndo != firstVersionWithRecoveryRevert {
		t.Fatalf("MinAgentVersionForRecoveryUndo = %q, want %q; if the floor is re-gated, update this pin in the same commit",
			MinAgentVersionForRecoveryUndo, firstVersionWithRecoveryRevert)
	}
}

// GH #824: the undo path tells a permanent answer from a lost one by these
// shapes. A non-2xx is a *CommandError carrying the status; a 2xx the control
// plane cannot use is ErrAbilityRunMalformed; a refusal is neither.
func TestAbilityRun_ErrorShapes(t *testing.T) {
	entry := []byte(`{"name":"wpmgr/page-create"}`)
	call := AbilityRunCall{Mode: AbilityRunModeRevert, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry)}

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusBadGateway} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"code":"x","message":"no"}`, status)
		}))
		_, err := realCommandClient(t).AbilityRun(context.Background(), uuid.New(), srv.URL, call)
		srv.Close()
		var ce *CommandError
		if !errors.As(err, &ce) || ce.Status != status {
			t.Fatalf("status %d: err = %v, want a *CommandError with that status", status, err)
		}
		if errors.Is(err, ErrAbilityRunMalformed) {
			t.Fatalf("status %d: marked malformed", status)
		}
	}

	bodies := map[string]string{
		"not json":   `<html>oops</html>`,
		"wrong mode": `{"ok":true,"outcome":"reverted","mode":"write"}`,
	}
	for name, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := realCommandClient(t).AbilityRun(context.Background(), uuid.New(), srv.URL, call)
		srv.Close()
		if !errors.Is(err, ErrAbilityRunMalformed) {
			t.Fatalf("%s: err = %v, want ErrAbilityRunMalformed", name, err)
		}
	}

	srv := fakeAbilityAgent(t, func(map[string]string) any {
		return map[string]any{"ok": false, "outcome": "refused", "code": "not_revertible", "detail": "x"}
	})
	defer srv.Close()
	_, err := realCommandClient(t).AbilityRun(context.Background(), uuid.New(), srv.URL, call)
	if errors.Is(err, ErrAbilityRunMalformed) {
		t.Fatalf("a refusal is marked malformed: %v", err)
	}
}
