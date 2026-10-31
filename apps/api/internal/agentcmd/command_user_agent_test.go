package agentcmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCommandClientUserAgent(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "direct"
		if upgrade {
			name = "https-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			type received struct {
				method, path, userAgent, contentType, accept, auth string
			}
			requests := make(chan received, 2)
			srv := newDualServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ string) {
				requests <- received{r.Method, r.URL.Path, r.UserAgent(), r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("Authorization")}
				if upgrade && r.TLS == nil {
					w.Header().Set("Location", "https://"+r.Host+r.URL.Path)
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			client, pub := newRealSignerClient(t)
			out, err := client.Diagnostics(context.Background(), uuid.New(), "http://127.0.0.1:"+srv.Port, DiagnosticsRequest{})
			if err != nil {
				t.Fatal("signed diagnostics command failed")
			}
			if string(out) != `{"ok":true}` {
				t.Error("command response changed")
			}
			wantRequests := 1
			if upgrade {
				wantRequests = 2
			}
			seen := srv.requests()
			if len(seen) != wantRequests {
				t.Fatalf("received %d requests, want %d", len(seen), wantRequests)
			}
			var firstAuth string
			for i := 0; i < wantRequests; i++ {
				got := <-requests
				if got.userAgent != "WPMgr-CommandClient/1.0" {
					t.Errorf("User-Agent = %q, want WPMgr-CommandClient/1.0", got.userAgent)
				}
				if got.method != http.MethodPost || got.path != "/wp-json/wpmgr/v1/command/diagnostics" {
					t.Error("command method or path changed")
				}
				if got.contentType != "application/json" || got.accept != "application/json" {
					t.Error("existing JSON headers changed")
				}
				if !bytes.Equal(seen[i].Body, []byte(`{}`)) {
					t.Error("command payload changed")
				}
				if !strings.HasPrefix(got.auth, "Bearer ") {
					t.Fatal("Bearer authorization missing")
				}
				if i == 0 {
					firstAuth = got.auth
				} else if got.auth != firstAuth {
					t.Error("redirect changed authorization")
				}
				parts := strings.Split(strings.TrimPrefix(got.auth, "Bearer "), ".")
				if len(parts) != 3 {
					t.Fatal("invalid JWT structure")
				}
				sig, err := base64.RawURLEncoding.DecodeString(parts[2])
				if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
					t.Fatal("invalid Ed25519 signature")
				}
			}
		})
	}
}
