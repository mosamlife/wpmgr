package mcp

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

const targetTestFingerprint = "7787514f2667f5577151c00759c33e4553edc1d75f96e0dc1ddcdc23989a5ea7"

// TestBuilderEditRefusalAuditNamesAbilityAndPost: the audit row of a
// builder-edit refusal names the ability and the post the AI asked about,
// for the connection's activity, while the AI's answer keeps the same bytes.
func TestBuilderEditRefusalAuditNamesAbilityAndPost(t *testing.T) {
	input := []byte(`{"post_id":418,"base_fingerprint":"` + targetTestFingerprint + `","operations":[]}`)
	for _, name := range []string{AbilityPageStructure, AbilityPageEdit} {
		plain := builderEditIneligibleRefusal(name, &agentcmd.AbilityRunRefusal{Code: "target_not_eligible", Detail: "not_draft"})
		before := refusalWireBytes(t, plain)
		err := withBuilderEditTarget(plain, name, input)
		var got *toolRefusal
		if !errors.As(err, &got) {
			t.Fatalf("%s: got %T, want a refusal", name, err)
		}
		if got.meta["ability"] != name || got.meta["post_id"] != int64(418) {
			t.Fatalf("%s: the audit record does not name the ability and the post: %v", name, got.meta)
		}
		if got.meta["detail"] != "not_draft" || got.meta["code"] == nil || got.meta["agent_code"] != "target_not_eligible" {
			t.Fatalf("%s: the refusal's own keys were lost: %v", name, got.meta)
		}
		if after := refusalWireBytes(t, got); !bytes.Equal(after, before) {
			t.Fatalf("%s: the AI's answer changed:\n%s\n%s", name, after, before)
		}
		if bytes.Contains(refusalWireBytes(t, got), []byte("418")) {
			t.Fatalf("%s: the post id reached the AI's answer", name)
		}
		if _, leaked := plain.meta["post_id"]; leaked {
			t.Fatalf("%s: the refusal it was given was changed: %v", name, plain.meta)
		}
	}
	for _, bad := range []string{`{"post_id":0}`, `{"post_id":"418"}`, `{"post_id":-3}`, `not json`, `{}`} {
		err := withBuilderEditTarget(builderEditIneligibleRefusal(AbilityPageEdit, nil), AbilityPageEdit, []byte(bad))
		var got *toolRefusal
		if !errors.As(err, &got) {
			t.Fatalf("%s: got %T", bad, err)
		}
		if _, named := got.meta["post_id"]; named {
			t.Fatalf("an input with no positive post id named one: %s -> %v", bad, got.meta)
		}
		if got.meta["ability"] != AbilityPageEdit {
			t.Fatalf("%s: the ability is missing: %v", bad, got.meta)
		}
	}
	other := errors.New("not a refusal")
	if err := withBuilderEditTarget(other, AbilityPageEdit, input); err != other {
		t.Fatalf("another error was changed: %v", err)
	}
	if err := withBuilderEditTarget(nil, AbilityPageEdit, input); err != nil {
		t.Fatalf("no error became %v", err)
	}
}

// TestPageEditRunRefusalNamesThePost: a page-edit refused before the site is
// asked (a ref named twice) leaves an audit record that names the ability,
// the post and the code, through runPageEdit itself.
func TestPageEditRunRefusalNamesThePost(t *testing.T) {
	input := []byte(`{"post_id":418,"base_fingerprint":"` + targetTestFingerprint + `","operations":[` +
		`{"op":"set_text","ref":"3c4d5e6","field":"text","text":"One"},` +
		`{"op":"set_text","ref":"3c4d5e6","field":"text","text":"Two"}]}`)
	_, err := (&Service{}).runPageEdit(context.Background(), AuthorizedRequest{}, nil, abilitySite{}, nil, "", input)
	var got *toolRefusal
	if !errors.As(err, &got) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if got.meta["code"] != pageEditOpsInvalid || got.meta["ability"] != AbilityPageEdit || got.meta["post_id"] != int64(418) {
		t.Fatalf("the audit record is %v, want code ops_invalid, the ability and post 418", got.meta)
	}
}
