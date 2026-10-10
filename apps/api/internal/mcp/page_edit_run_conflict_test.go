package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// pageEditRunInput passes every check the control plane makes before the
// site is asked: one set_text on one ref of post 418.
const pageEditRunInput = `{"post_id":418,"base_fingerprint":"` + targetTestFingerprint + `","operations":[` +
	`{"op":"set_text","ref":"3c4d5e6","field":"text","text":"One"}]}`

// newAnsweringAgent answers every call with reply, the agent's own JSON,
// sent over HTTP and read back by the production client and decoder.
func newAnsweringAgent(t *testing.T, reply map[string]any) *wireRefusingAgent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	// Test-only: the agent is a loopback httptest server.
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true})
	return &wireRefusingAgent{srv: srv, client: agentcmd.NewClient(hc, signer)}
}

// inputDraftStore names the input's own post as a draft WPMgr created.
type inputDraftStore struct{}

func (inputDraftStore) EligibleDraftIDs(_ context.Context, _ domain.Principal, _ uuid.UUID, postID int64) ([]int64, error) {
	return []int64{postID}, nil
}

// pageEditPrecheckRefusal runs pageEditRunInput through runPageEdit against
// an agent answering reply to the precheck, and returns the refusal.
func pageEditPrecheckRefusal(t *testing.T, reply map[string]any) *toolRefusal {
	t.Helper()
	agent := newAnsweringAgent(t, reply)
	e := catalogueRow(AbilityPageEdit, "wpmgr", "write")
	e.ApprovalMode, e.Snapshot = "per_call", "builder_document"
	e.Limits = []byte(`{"builders_enabled":["elementor"]}`)
	_, sum, _ := testEntryEncoder(e)
	e.EntrySha256 = &sum
	eng := &abilityEngine{agent: agent, entry: testEntryEncoder, readLimit: newAbilityReadLimiter(), drafts: inputDraftStore{}}
	site := abilitySite{row: sqlc.Site{ID: uuid.New(), Url: agent.srv.URL}}
	_, err := (&Service{}).runPageEdit(context.Background(), AuthorizedRequest{GrantID: uuid.New()}, eng, site, &e,
		"example.com", []byte(pageEditRunInput))
	if len(agent.calls) != 1 || agent.calls[0].Mode != agentcmd.AbilityRunModePrecheck {
		t.Fatalf("the precheck was not sent once: %d calls", len(agent.calls))
	}
	var r *toolRefusal
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want a refusal", err)
	}
	return r
}

// carriesText reports whether wire holds s as it is or as JSON writes it
// (json.Marshal escapes <, > and &, so a raw search alone would miss it).
func carriesText(wire []byte, s string) bool {
	esc, _ := json.Marshal(s)
	return bytes.Contains(wire, []byte(s)) || bytes.Contains(wire, bytes.Trim(esc, `"`))
}

func refusalDetails(t *testing.T, r *toolRefusal) map[string]any {
	t.Helper()
	var de *domain.Error
	if !errors.As(r.err, &de) {
		t.Fatalf("refusal error is %T", r.err)
	}
	return de.Details
}

// TestPageEditPrecheckConflictNamesTheReasonAndHint: a page-edit precheck the
// site refuses as a conflict tells the AI which conflict it was (a member of
// the closed set only), a fixed hint for it, and whether a retry can help.
// The control plane decides retryable from the reason, whatever the agent's
// flag says, and the site's own detail text never reaches the answer.
func TestPageEditPrecheckConflictNamesTheReasonAndHint(t *testing.T) {
	// Positive control: carriesText finds a detail that is on the wire.
	if leaked, _ := json.Marshal(map[string]any{"detail": "<b>x"}); !carriesText(leaked, "<b>x") {
		t.Fatal("carriesText cannot see an escaped detail")
	}
	cases := []struct {
		name      string
		detail    string
		agentSays bool
		reason    string // "" when the answer names no reason
		retryable bool
		hint      []string
	}{
		{"editor open, the agent says not retryable", "editor_open", false, "editor_open", true,
			[]string{"open in Elementor", "Wait three minutes", "three times"}},
		{"editor open, the agent says retryable", "editor_open", true, "editor_open", true,
			[]string{"open in Elementor", "Wait three minutes", "three times"}},
		{"changed since read, the agent says retryable", "changed_since_read", true, "changed_since_read", false,
			[]string{"changed after you read it", "wpmgr/page-structure"}},
		{"autosave pending, the agent says retryable", "autosave_pending", true, "autosave_pending", false,
			[]string{"save or discard", "wpmgr/page-structure"}},
		{"markup outside the set", "<b>x", true, "", false, []string{"wpmgr/page-structure"}},
		{"an instruction outside the set", plantedInstruction, true, "", false, []string{"wpmgr/page-structure"}},
		{"a near miss of a member", "editor_open ", true, "", false, []string{"wpmgr/page-structure"}},
	}
	hints := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := pageEditPrecheckRefusal(t, map[string]any{
				"ok": false, "outcome": "refused", "code": "conflict", "detail": c.detail, "retryable": c.agentSays,
			})
			d := refusalDetails(t, r)
			if d["code"] != "conflict" || d["retryable"] != c.retryable {
				t.Fatalf("details = %v, want code conflict and retryable %v", d, c.retryable)
			}
			reason, named := d["reason"]
			if c.reason == "" && named {
				t.Fatalf("a detail outside the closed set was named as a reason: %v", d)
			}
			if c.reason != "" && reason != c.reason {
				t.Fatalf("reason = %v, want %s", reason, c.reason)
			}
			hint, _ := d["hint"].(string)
			for _, w := range c.hint {
				if !strings.Contains(hint, w) {
					t.Fatalf("hint %q does not say %q", hint, w)
				}
			}
			hints[c.reason] = hint
			if c.reason == "" && carriesText(refusalWireBytes(t, r), c.detail) {
				t.Fatalf("the site's detail reached the AI: %s", refusalWireBytes(t, r))
			}
			if r.reason != reasonAbilityPrecheckRefused || r.meta["code"] != "conflict" {
				t.Fatalf("the audit record changed: reason %s meta %v", r.reason, r.meta)
			}
		})
	}
	seen := map[string]string{}
	for reason, hint := range hints {
		if prev, dup := seen[hint]; dup {
			t.Fatalf("reasons %q and %q share one hint: %s", prev, reason, hint)
		}
		seen[hint] = reason
	}
	if len(hints) != 4 {
		t.Fatalf("want a hint for each of the three reasons and one for none, got %v", hints)
	}
}
