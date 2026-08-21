package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestResolveTracesEndpointURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			// The shape infra/docker-compose.yml ships. Without the default
			// path this POSTs to the collector root and the spans vanish.
			name:     "no path gets the default signal path",
			endpoint: "http://otel-lgtm:4318",
			want:     "http://otel-lgtm:4318/v1/traces",
		},
		{
			name:     "bare slash counts as no path",
			endpoint: "http://otel-lgtm:4318/",
			want:     "http://otel-lgtm:4318/v1/traces",
		},
		{
			// The double-append trap: this must not become
			// ".../v1/traces/v1/traces".
			name:     "explicit default path is left alone",
			endpoint: "http://otel-lgtm:4318/v1/traces",
			want:     "http://otel-lgtm:4318/v1/traces",
		},
		{
			name:     "custom path is left alone",
			endpoint: "https://collector.example.com/otlp/v1/traces",
			want:     "https://collector.example.com/otlp/v1/traces",
		},
		{
			name:     "custom path that is not a traces path is still left alone",
			endpoint: "https://collector.example.com:4318/ingest",
			want:     "https://collector.example.com:4318/ingest",
		},
		{
			name:     "https with no path gets the default signal path",
			endpoint: "https://collector.example.com",
			want:     "https://collector.example.com/v1/traces",
		},
		{
			name:     "query string is preserved alongside the added path",
			endpoint: "http://otel-lgtm:4318?tenant=acme",
			want:     "http://otel-lgtm:4318/v1/traces?tenant=acme",
		},
		{
			// Not a URL we can reason about: url.Parse yields scheme
			// "otel-lgtm", opaque "4318", empty host. Hand it back untouched
			// so the exporter reports its own error rather than us inventing
			// an endpoint the operator never configured.
			name:     "opaque host:port form is passed through untouched",
			endpoint: "otel-lgtm:4318",
			want:     "otel-lgtm:4318",
		},
		{
			name:     "unparseable input is passed through untouched",
			endpoint: "http://[::1]:no-port/v1/traces",
			want:     "http://[::1]:no-port/v1/traces",
		},
		{
			name:     "empty stays empty",
			endpoint: "",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveTracesEndpointURL(tt.endpoint); got != tt.want {
				t.Errorf("resolveTracesEndpointURL(%q)\n got: %q\nwant: %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

// TestInitExportsToResolvedPath drives Init exactly as cmd/wpmgr does and
// asserts on the path the exporter actually POSTs to, rather than trusting the
// resolver in isolation. This is what catches an exporter-side default
// changing underneath us: the resolver could be correct and the request still
// land on the wrong path.
func TestInitExportsToResolvedPath(t *testing.T) {
	tests := []struct {
		name string
		// suffix is appended to the httptest server's base URL to build the
		// configured endpoint.
		suffix string
		want   string
	}{
		{
			name:   "path-less endpoint posts to the default traces path",
			suffix: "",
			want:   "/v1/traces",
		},
		{
			name:   "trailing-slash endpoint posts to the default traces path",
			suffix: "/",
			want:   "/v1/traces",
		},
		{
			name:   "configured custom path is honoured verbatim",
			suffix: "/otlp/ingest",
			want:   "/otlp/ingest",
		},
		{
			name:   "configured default path is not doubled",
			suffix: "/v1/traces",
			want:   "/v1/traces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPaths []string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPaths = append(gotPaths, r.URL.Path)
				mu.Unlock()
				// An empty body unmarshals to an empty
				// ExportTraceServiceResponse, which the exporter accepts.
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			ctx := context.Background()
			p, err := Init(ctx, Config{
				ServiceName:  "telemetry-test",
				OTLPEndpoint: srv.URL + tt.suffix,
			})
			if err != nil {
				t.Fatalf("Init: %v", err)
			}

			_, span := p.tracerProvider.Tracer("telemetry-test").Start(ctx, "probe")
			span.End()

			// Shutdown force-flushes the batch processor, so the export has
			// completed by the time it returns.
			if err := p.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(gotPaths) == 0 {
				t.Fatalf("collector received no export request; endpoint was %q", srv.URL+tt.suffix)
			}
			for _, got := range gotPaths {
				if got != tt.want {
					t.Errorf("endpoint %q exported to path %q, want %q", srv.URL+tt.suffix, got, tt.want)
				}
			}
		})
	}
}
