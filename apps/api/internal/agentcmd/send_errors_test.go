package agentcmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// A command whose site address cannot even form a URL is never sent.
func TestPostRaw_BadSiteURLIsNotSent(t *testing.T) {
	// A bare client: the URL is refused before the signer or transport is used.
	c := &Client{}
	_, err := c.postRaw(context.Background(), uuid.New(), "ftp://x.test", "cache_purge", struct{}{})
	if err == nil || !errors.Is(err, ErrCommandNotSent) {
		t.Fatalf("err = %v, want ErrCommandNotSent", err)
	}
	if err.Error() != `invalid site url scheme "ftp"` {
		t.Errorf("message changed: %q", err.Error())
	}
}

// A refused connection fails while dialling: nothing was written.
func TestPostRaw_DialFailureIsNotSent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := httptest.NewServer(http.NotFoundHandler())
	client := buildTestAgentClient(t, srv)
	srv.Close()
	_, err = client.postRaw(context.Background(), uuid.New(), "http://"+addr, "cache_purge", struct{}{})
	if err == nil || !errors.Is(err, ErrCommandNotSent) {
		t.Fatalf("err = %v, want ErrCommandNotSent", err)
	}
}

// A reply that arrived is never "not sent", whatever it says.
func TestPostRaw_ReplyIsNeverNotSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := buildTestAgentClient(t, srv)
	_, err := client.postRaw(context.Background(), uuid.New(), srv.URL, "cache_purge", struct{}{})
	if err == nil || errors.Is(err, ErrCommandNotSent) {
		t.Fatalf("err = %v, want a sent failure", err)
	}
}

// ok:false is typed, keeps its legacy message, and returns the reply.
func TestCachePurge_OKFalseIsAgentReportedFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"detail":"cache root missing"}`))
	}))
	defer srv.Close()
	client := buildTestAgentClient(t, srv)
	res, err := client.CachePurge(context.Background(), uuid.New(), srv.URL, CachePurgeRequest{Scope: "all", OriginOnly: true})
	if !errors.Is(err, ErrAgentReportedFailure) {
		t.Fatalf("err = %v, want ErrAgentReportedFailure", err)
	}
	if errors.Is(err, ErrCommandNotSent) {
		t.Fatal("a reply is never not-sent")
	}
	if err.Error() != "cache_purge rejected by agent: cache root missing" {
		t.Errorf("message changed: %q", err.Error())
	}
	if res.Detail != "cache root missing" {
		t.Errorf("reply not returned: %+v", res)
	}
}

func TestCachePurge_OriginOnlyReportDecodes(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"ok":true,"origin_only_honoured":true,"integrations":[{"slug":"kinsta","action":"skipped_reach_unconfirmed"}]}`))
	}))
	defer srv.Close()
	client := buildTestAgentClient(t, srv)
	res, err := client.CachePurge(context.Background(), uuid.New(), srv.URL, CachePurgeRequest{Scope: "all", OriginOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"scope":"all","origin_only":true}` {
		t.Errorf("body = %s", gotBody)
	}
	if res.OriginOnlyHonoured == nil || !*res.OriginOnlyHonoured || len(res.Integrations) != 1 || res.Integrations[0].Slug != "kinsta" {
		t.Errorf("res = %+v", res)
	}
}
