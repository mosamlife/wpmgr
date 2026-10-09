package abilityrequest

import (
	"errors"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// fakeRouteEncoder hashes the title only, which is enough to model an edit.
func fakeRouteEncoder(r sqlc.RestRouteCatalogue) ([]byte, string, error) {
	b := []byte(`{"route_id":"` + r.RouteID + `","title":"` + r.Title + `"}`)
	return b, agentcmd.SHA256Hex(b), nil
}

func w1Route(title string) (sqlc.RestRouteCatalogue, string) {
	r := sqlc.RestRouteCatalogue{RouteID: "wp-v2-pages-update-fields", Class: "write", Enabled: true, Title: title}
	_, sum, _ := fakeRouteEncoder(r)
	r.RouteSha256 = &sum
	return r, sum
}

const w1Input = `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"x"}}`

// TestE3RouteChangedUnit is the Go layer of route W1: the worker sends only
// the bytes whose hash is the APPROVED route_sha256.
func TestE3RouteChangedUnit(t *testing.T) {
	approvedRow, approved := w1Route("Change a page's title")

	b, sum, why := routeSendable(fakeRouteEncoder, approvedRow, approved, w1Input)
	if why != "" || sum != approved || len(b) == 0 {
		t.Fatalf("unchanged route: why=%q sum=%s", why, sum)
	}

	// An admin edit: the row and its stored hash both moved.
	edited, _ := w1Route("Change a page's title (edited)")
	if _, _, why := routeSendable(fakeRouteEncoder, edited, approved, w1Input); why != ReasonRouteChanged {
		t.Fatalf("edited route: why=%q, want route_changed", why)
	}

	// An out-of-band edit that kept the old stored hash: the reproduced
	// bytes no longer hash to it.
	sneaky := approvedRow
	sneaky.Title = "Something else"
	if _, _, why := routeSendable(fakeRouteEncoder, sneaky, approved, w1Input); why != ReasonRouteChanged {
		t.Fatalf("out-of-band edit: why=%q, want route_changed", why)
	}

	// The stored hash was moved back to the approved value over different
	// bytes: still refused, the bytes are what is sent.
	forged := edited
	forged.RouteSha256 = &approved
	if _, _, why := routeSendable(fakeRouteEncoder, forged, approved, w1Input); why != ReasonRouteChanged {
		t.Fatalf("forged stored hash: why=%q, want route_changed", why)
	}

	disabled := approvedRow
	disabled.Enabled = false
	if _, _, why := routeSendable(fakeRouteEncoder, disabled, approved, w1Input); why != ReasonRouteDisabled {
		t.Fatalf("disabled route: why=%q, want route_disabled", why)
	}
	read := approvedRow
	read.Class = "read"
	if _, _, why := routeSendable(fakeRouteEncoder, read, approved, w1Input); why != ReasonRouteDisabled {
		t.Fatalf("read route: why=%q, want route_disabled", why)
	}

	// The input must still name the route it was approved against.
	other := `{"route_id":"wp-v2-posts-update-fields","path":{"id":412},"body":{"title":"x"}}`
	if _, _, why := routeSendable(fakeRouteEncoder, approvedRow, approved, other); why != ReasonRouteChanged {
		t.Fatalf("input names another route: why=%q, want route_changed", why)
	}

	failing := func(sqlc.RestRouteCatalogue) ([]byte, string, error) { return nil, "", errors.New("unstamped") }
	if _, _, why := routeSendable(failing, approvedRow, approved, w1Input); why != ReasonRouteChanged {
		t.Fatalf("encoder refusal: why=%q, want route_changed", why)
	}
}

// TestRestWriteOutcomes: the agent's "updated" is applied with the undo
// window open, from the reply and from the ledger.
func TestRestWriteOutcomes(t *testing.T) {
	now := time.Now()
	oc := classifyWrite("", agentcmd.AbilityRunResponse{OK: true, Outcome: "updated", Mode: "write"}, nil, now)
	if oc.outcome != OutcomeApplied || !oc.undoUntil.Valid || oc.createdPostID != nil {
		t.Fatalf("updated reply: %+v", oc)
	}
	oc, ok := outcomeFromStored("", []byte(`{"ok":true,"outcome":"updated","post_id":412}`), now)
	if !ok || oc.outcome != OutcomeApplied {
		t.Fatalf("updated ledger: %+v ok=%v", oc, ok)
	}
	oc = classifyWrite("", agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "post_touched"}, now)
	if oc.outcome != OutcomeRefused || oc.code == nil || *oc.code != "post_touched" {
		t.Fatalf("refusal: %+v", oc)
	}
	if got := undoResultFor(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "post_touched"}); got != UndoRefusedConflict {
		t.Fatalf("post_touched undo = %s, want %s", got, UndoRefusedConflict)
	}
}

// TestWriteExpectedPerAbility: a route write sends the base fingerprint, a
// page-create the preview digest, never both.
func TestWriteExpectedPerAbility(t *testing.T) {
	route := "wp-v2-pages-update-fields"
	prev := "p"
	r := writeExpected(dispatchPlan{row: sqlc.AssistantAbilityRequest{RouteID: &route, PrecheckDigest: "a", BaseFingerprint: "b", PreviewDigest: &prev}})
	if r.BaseFingerprint != "b" || r.PreviewDigest != "" {
		t.Fatalf("route write expected: %+v", r)
	}
	r = writeExpected(dispatchPlan{row: sqlc.AssistantAbilityRequest{PrecheckDigest: "a", BaseFingerprint: "b", PreviewDigest: &prev}})
	if r.BaseFingerprint != "" || r.PreviewDigest != "p" {
		t.Fatalf("page-create expected: %+v", r)
	}
}
