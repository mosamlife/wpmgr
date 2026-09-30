package perf

// assistant_purge_send_test.go drives the real SendAssistantPurge through the
// production agentcmd client (httpclient + Ed25519 signer) against an httptest
// fake agent, so what the site actually receives is what is asserted.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

type fakePurgeAgent struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
	bearer []string
}

func newFakePurgeAgent(t *testing.T, reply string) *fakePurgeAgent {
	t.Helper()
	f := &fakePurgeAgent{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f.mu.Lock()
		f.bodies = append(f.bodies, m)
		f.paths = append(f.paths, r.URL.Path)
		f.bearer = append(f.bearer, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type recordingCDN struct {
	calls [][]string
}

func (c *recordingCDN) Purge(_ context.Context, _ CDNCredentials, _ string, urls []string) error {
	c.calls = append(c.calls, urls)
	return nil
}

type plainDecryptor struct{}

func (plainDecryptor) Decrypt(b []byte) ([]byte, error) { return b, nil }
func (plainDecryptor) Encrypt(b []byte) ([]byte, error) { return b, nil }

func realPurgeService(t *testing.T) (*Service, *recordingCDN, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true})
	svc := NewService(nil, plainDecryptor{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetAgentClient(agentcmd.NewClient(hc, signer), nil)
	cdn := &recordingCDN{}
	svc.SetCDNPurger(cdn)
	return svc, cdn, pub
}

func configuredCDN() CDNCiphertext {
	return CDNCiphertext{Ciphertext: []byte(`{"provider":"cloudflare","api_token":"x","zone_id":"z"}`), Provider: "cloudflare"}
}

func TestSendAssistantPurge_SendsOriginOnlyThroughTheRealClient(t *testing.T) {
	agent := newFakePurgeAgent(t, `{"ok":true,"origin_only_honoured":true}`)
	svc, _, pub := realPurgeService(t)

	_, err := svc.SendAssistantPurge(context.Background(), uuid.New(), agent.srv.URL, CDNCiphertext{},
		AssistantPurge{Scope: "url", URL: "https://example.com/about/"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(agent.bodies) != 1 {
		t.Fatalf("agent got %d requests", len(agent.bodies))
	}
	b := agent.bodies[0]
	if b["origin_only"] != true {
		t.Errorf("origin_only = %v, want true; body=%v", b["origin_only"], b)
	}
	if b["scope"] != "url" || b["url"] != "https://example.com/about/" {
		t.Errorf("scope/url = %v / %v", b["scope"], b["url"])
	}
	if urls, _ := b["urls"].([]any); len(urls) != 1 || urls[0] != "https://example.com/about/" {
		t.Errorf("urls = %v", b["urls"])
	}
	if !strings.HasSuffix(agent.paths[0], "cache_purge") {
		t.Errorf("path = %q", agent.paths[0])
	}
	tok := strings.TrimPrefix(agent.bearer[0], "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a signed JWT: %q", agent.bearer[0])
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Errorf("command is not signed by the control-plane key")
	}
}

func TestSendAssistantPurge_AllScopeIsOriginOnlyAndSkipsTheCDN(t *testing.T) {
	agent := newFakePurgeAgent(t, `{"ok":true,"origin_only_honoured":true}`)
	svc, cdn, _ := realPurgeService(t)

	res, err := svc.SendAssistantPurge(context.Background(), uuid.New(), agent.srv.URL, configuredCDN(), AssistantPurge{Scope: "all"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	b := agent.bodies[0]
	if b["origin_only"] != true || b["scope"] != "all" {
		t.Errorf("body = %v", b)
	}
	if len(cdn.calls) != 0 {
		t.Errorf("CDN called for scope all: %v", cdn.calls)
	}
	if res.WpmgrCDN != AssistantCDNNotAttempted {
		t.Errorf("WpmgrCDN = %q", res.WpmgrCDN)
	}
}

func TestSendAssistantPurge_PageScopeClearsTheCDNOnlyAfterSuccess(t *testing.T) {
	agent := newFakePurgeAgent(t, `{"ok":true,"origin_only_honoured":true}`)
	svc, cdn, _ := realPurgeService(t)

	res, err := svc.SendAssistantPurge(context.Background(), uuid.New(), agent.srv.URL, configuredCDN(),
		AssistantPurge{Scope: "url", URL: "https://example.com/a/"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(cdn.calls) != 1 || len(cdn.calls[0]) != 1 || cdn.calls[0][0] != "https://example.com/a/" {
		t.Errorf("CDN calls = %v", cdn.calls)
	}
	if res.WpmgrCDN != AssistantCDNCleared {
		t.Errorf("WpmgrCDN = %q", res.WpmgrCDN)
	}
}

func TestSendAssistantPurge_NeverTouchesTheCDNAfterAgentFailure(t *testing.T) {
	agent := newFakePurgeAgent(t, `{"ok":false,"detail":"nope"}`)
	svc, cdn, _ := realPurgeService(t)

	res, err := svc.SendAssistantPurge(context.Background(), uuid.New(), agent.srv.URL, configuredCDN(),
		AssistantPurge{Scope: "url", URL: "https://example.com/a/"})
	if err == nil {
		t.Fatal("want the agent failure returned")
	}
	if len(cdn.calls) != 0 {
		t.Errorf("CDN called after ok:false: %v", cdn.calls)
	}
	if res.WpmgrCDN != AssistantCDNNotAttempted {
		t.Errorf("WpmgrCDN = %q", res.WpmgrCDN)
	}
}
