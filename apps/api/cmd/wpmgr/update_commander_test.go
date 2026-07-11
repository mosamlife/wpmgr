package main

// update_commander_test.go — GH #208 Bug 2 regression lock: the update
// worker must dispatch Update/Rollback commands through a DEDICATED
// commander with a longer per-attempt HTTP timeout than the shared 30s
// `commander`/`ssrfClient` (RefreshCommander, scan, etc. keep using the
// shared one). A real update is heavy and synchronous on the agent
// (mandatory pre-update snapshot + download + extract + core DB migration,
// all inline in one request) and routinely exceeds 30s, which previously
// drove a spurious CP-recorded "Failed" even though the agent had actually
// finished the apply.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// fakeCommander is a distinguishable update.Commander stand-in for the
// "shared commander" the fallback branch must return unmodified.
type fakeCommander struct{}

func (fakeCommander) Update(context.Context, uuid.UUID, string, agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	return agentcmd.UpdateResponse{}, nil
}

func (fakeCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	return agentcmd.RollbackResponse{}, nil
}

func newTestCmdSigner(t *testing.T) *agentcmd.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// TestBuildUpdateApplyCommander_FallsBackWhenSigningDisabled verifies that
// with no CP signing key configured (cmdSigner == nil, mirroring the
// disabled-commander boot path), buildUpdateApplyCommander returns the SAME
// shared commander untouched rather than constructing a dedicated client —
// consistent with every other CP->agent command path when signing is off.
func TestBuildUpdateApplyCommander_FallsBackWhenSigningDisabled(t *testing.T) {
	shared := fakeCommander{}

	got := buildUpdateApplyCommander(shared, nil, 5*time.Minute)

	if got != update.Commander(shared) {
		t.Fatalf("buildUpdateApplyCommander(nil signer) = %v, want the shared commander returned unmodified", got)
	}
}

// TestBuildUpdateApplyCommander_BuildsDedicatedClientWhenSigningEnabled
// verifies that with a real CP signing key configured, the function returns
// a DISTINCT commander (not the shared one, not sharing the shared 30s
// client's timeout) — the concrete fix for GH #208 Bug 2.
func TestBuildUpdateApplyCommander_BuildsDedicatedClientWhenSigningEnabled(t *testing.T) {
	shared := fakeCommander{}
	signer := newTestCmdSigner(t)

	got := buildUpdateApplyCommander(shared, signer, 5*time.Minute)

	if got == update.Commander(shared) {
		t.Fatal("buildUpdateApplyCommander(real signer) returned the shared commander, want a dedicated client")
	}
	client, ok := got.(*agentcmd.Client)
	if !ok {
		t.Fatalf("buildUpdateApplyCommander(real signer) = %T, want *agentcmd.Client", got)
	}
	if client == nil {
		t.Fatal("buildUpdateApplyCommander(real signer) returned a nil *agentcmd.Client")
	}
}
