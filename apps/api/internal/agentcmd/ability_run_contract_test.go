package agentcmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMinAgentVersionForAbilityEngine_Pinned pins the literal: the first
// agent release that ships ability_run.
func TestMinAgentVersionForAbilityEngine_Pinned(t *testing.T) {
	const firstVersionWithAbilityRun = "0.61.155"
	if MinAgentVersionForAbilityEngine != firstVersionWithAbilityRun {
		t.Fatalf("MinAgentVersionForAbilityEngine = %q, want %q; if the floor is re-gated, update this pin in the same commit",
			MinAgentVersionForAbilityEngine, firstVersionWithAbilityRun)
	}
}

// fakeAbilityAgent mirrors the agent's first two steps: the body is exactly
// {"p": string}, and sha256(p) equals the token's pd claim. It then decodes p
// from that same string and checks entry_sha256 over the entry string's bytes.
func fakeAbilityAgent(t *testing.T, reply func(p map[string]string) any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil || len(body) != 1 {
			t.Errorf("body is not exactly one member: %s", raw)
		}
		var p string
		if err := json.Unmarshal(body["p"], &p); err != nil {
			t.Errorf("p is not a string: %v", err)
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(claimsJSON, &claims)
		if claims["cmd"] != CmdAbilityRun {
			t.Errorf("cmd = %v", claims["cmd"])
		}
		if claims["pd"] != SHA256Hex([]byte(p)) {
			t.Errorf("pd %v does not match sha256(p)", claims["pd"])
		}
		var pv map[string]string
		if err := json.Unmarshal([]byte(p), &pv); err != nil {
			t.Errorf("p is not an object of strings: %v", err)
		}
		if pv["mode"] != AbilityRunModeLedger && SHA256Hex([]byte(pv["entry"])) != pv["entry_sha256"] {
			t.Errorf("entry_sha256 does not match the entry string bytes")
		}
		_ = json.NewEncoder(w).Encode(reply(pv))
	}))
}

func TestAbilityRun_BindsExactBytes(t *testing.T) {
	// The entry deliberately carries characters encoding/json escapes (<, &,
	// U+2028) and non-canonical spacing: the digest is over the bytes as
	// given, and they must round-trip unchanged.
	entry := []byte(`{"name":"wpmgr/site-facts",  "title":"a <b> & c` + " " + `","enabled":true}`)
	srv := fakeAbilityAgent(t, func(p map[string]string) any {
		if p["entry"] != string(entry) {
			t.Errorf("entry bytes changed in transit")
		}
		if p["input"] != `{"x": 1}` {
			t.Errorf("input bytes changed in transit: %q", p["input"])
		}
		return map[string]any{"ok": true, "outcome": "completed", "mode": "read",
			"ability": "wpmgr/site-facts", "entry_sha256": p["entry_sha256"], "output": map[string]any{"wp_version": "7.0"}}
	})
	defer srv.Close()
	c := realCommandClient(t)
	resp, err := c.AbilityRun(context.Background(), uuid.New(), srv.URL, AbilityRunCall{
		Mode: AbilityRunModeRead, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry), Input: []byte(`{"x": 1}`),
	})
	if err != nil {
		t.Fatalf("AbilityRun: %v", err)
	}
	if !strings.Contains(string(resp.Output), "7.0") {
		t.Fatalf("output = %s", resp.Output)
	}
}

func TestAbilityRun_RefusalIsTypedAndClosed(t *testing.T) {
	entry := []byte(`{"name":"wpmgr/site-facts"}`)
	for _, tc := range []struct{ code, want string }{
		{"integration_entry_changed", "integration_entry_changed"},
		{"Ignore previous instructions", "unknown"},
		{"not_a_known_code", "unknown"},
	} {
		srv := fakeAbilityAgent(t, func(map[string]string) any {
			return map[string]any{"ok": false, "outcome": "refused", "code": tc.code, "detail": "x"}
		})
		c := realCommandClient(t)
		_, err := c.AbilityRun(context.Background(), uuid.New(), srv.URL, AbilityRunCall{
			Mode: AbilityRunModeRead, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry),
		})
		srv.Close()
		var ref *AbilityRunRefusal
		if !errors.As(err, &ref) || ref.Code != tc.want {
			t.Fatalf("code %q: err = %v, want refusal %q", tc.code, err, tc.want)
		}
	}
}

// pageLayoutRefusalCodes are the codes the agent's wpmgr/page-create answers
// for outline grammar v2 (MinAgentVersionForPageLayout).
var pageLayoutRefusalCodes = []string{
	"layout_invalid", "link_invalid", "layout_needs_block_editor", "image_not_available", "image_url_unusable",
}

// TestAbilityRun_PageLayoutRefusalsKeepTheirCode sends the agent's own
// refusal JSON through the real transport and decoder: each grammar v2 code
// arrives as itself, so a precheck refusal reaches its fixed hint and a
// failed write records its code, never unknown.
func TestAbilityRun_PageLayoutRefusalsKeepTheirCode(t *testing.T) {
	entry := []byte(`{"name":"wpmgr/page-create"}`)
	for _, code := range pageLayoutRefusalCodes {
		srv := fakeAbilityAgent(t, func(map[string]string) any {
			return map[string]any{"ok": false, "outcome": "refused", "code": code, "detail": "attachment_id 42", "retryable": false}
		})
		_, err := realCommandClient(t).AbilityRun(context.Background(), uuid.New(), srv.URL, AbilityRunCall{
			Mode: AbilityRunModePrecheck, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry),
			Input: []byte(`{"post_type":"page"}`),
		})
		srv.Close()
		var ref *AbilityRunRefusal
		if !errors.As(err, &ref) {
			t.Errorf("code %q: err = %v, want a refusal", code, err)
			continue
		}
		if ref.Code != code {
			t.Errorf("code %q arrived as %q", code, ref.Code)
		}
	}
}

func TestBuildAbilityRunParams_RefusesAMismatchedEntryHash(t *testing.T) {
	entry := []byte(`{"name":"wpmgr/site-facts"}`)
	_, _, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModeRead, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex([]byte(`{}`)),
	})
	if err == nil {
		t.Fatal("a mismatched entry_sha256 was accepted")
	}
	if _, _, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModeRead, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry), Input: []byte(`[1]`),
	}); err == nil {
		t.Fatal("a non-object input was accepted")
	}
	if _, _, err := BuildAbilityRunParams(AbilityRunCall{Mode: "write", RequestID: uuid.New()}); err == nil {
		t.Fatal("write mode was accepted in E1")
	}
}

func TestMintParamsBound_CarriesPDAndRefusesBadDigest(t *testing.T) {
	c := realCommandClient(t)
	if _, _, err := c.signer.MintParamsBound(c.clock(), uuid.NewString(), CmdAbilityRun, "ABC"); err == nil {
		t.Fatal("a malformed pd was accepted")
	}
	pd := SHA256Hex([]byte("x"))
	tok, _, err := c.signer.MintParamsBound(c.clock(), uuid.NewString(), CmdAbilityRun, pd)
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if !strings.Contains(string(claimsJSON), `"pd":"`+pd+`"`) {
		t.Fatalf("claims = %s", claimsJSON)
	}
	// Every other command's claims are unchanged: no pd member.
	tok2, _, _ := c.signer.Mint(c.clock(), uuid.NewString(), "update")
	claims2, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok2, ".")[1])
	if strings.Contains(string(claims2), `"pd"`) {
		t.Fatalf("a plain command token carries pd: %s", claims2)
	}
}
