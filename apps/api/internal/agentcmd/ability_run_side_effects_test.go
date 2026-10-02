package agentcmd

import (
	"encoding/json"
	"testing"
)

func TestDecodeSideEffects_NewShape(t *testing.T) {
	se := decodeSideEffects(json.RawMessage(`{"options":["o"],"posts":1,"roles":2,"users":3,"post_meta":4,"terms":5,"http_hosts":["h"],"blocked":["user_meta_unknown"]}`))
	if se == nil {
		t.Fatal("new shape refused")
	}
	if se.PostMeta != 4 || se.Terms != 5 || se.Users != 3 {
		t.Fatalf("counts = %+v", se)
	}
	if len(se.Blocked) != 1 || se.Blocked[0] != "user_meta_unknown" {
		t.Fatalf("blocked = %v", se.Blocked)
	}
}

func TestDecodeSideEffects_OldShapeAbsentIsZero(t *testing.T) {
	se := decodeSideEffects(json.RawMessage(`{"options":[],"posts":1,"roles":0,"users":0,"http_hosts":[],"blocked":[]}`))
	if se == nil || se.PostMeta != 0 || se.Terms != 0 || se.Posts != 1 {
		t.Fatalf("old shape = %+v", se)
	}
}

func TestDecodeSideEffects_UnknownMemberAndNegativesRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown":      `{"options":[],"posts":0,"roles":0,"users":0,"post_meta":0,"terms":0,"http_hosts":[],"blocked":[],"extra":1}`,
		"neg_postmeta": `{"options":[],"posts":0,"roles":0,"users":0,"post_meta":-1,"terms":0,"http_hosts":[],"blocked":[]}`,
		"neg_terms":    `{"options":[],"posts":0,"roles":0,"users":0,"post_meta":0,"terms":-1,"http_hosts":[],"blocked":[]}`,
	} {
		if se := decodeSideEffects(json.RawMessage(raw)); se != nil {
			t.Errorf("%s: accepted %+v", name, se)
		}
	}
}
