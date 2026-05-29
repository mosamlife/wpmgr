// Package httpclient centralizes the control plane's outbound HTTP client.
//
// Per ADR-009, ALL outbound calls to agent/site URLs (which are attacker
// -influenced) MUST go through an SSRF-hardened transport: the destination IP
// is checked at dial time via net.Dialer.Control (code.dny.dev/ssrf), rejecting
// private, loopback, link-local, and other non-public ranges atomically before
// connect — this defeats DNS-rebinding (TOCTTOU) because the same IP that is
// validated is the one connected to. The transport is wrapped with otelhttp for
// tracing, and Do applies bounded retries with exponential backoff for the
// transient/idempotent-failure cases.
//
// Centralizing this here is the SSRF guarantee: domains never construct their
// own http.Client for agent/site traffic; they take a *Client from this package.
package httpclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"code.dny.dev/ssrf"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Config tunes the SSRF-hardened client.
type Config struct {
	// Timeout is the overall per-attempt request timeout.
	Timeout time.Duration
	// MaxRetries is the number of additional attempts after the first on a
	// retryable failure (network error or 5xx). Zero disables retries.
	MaxRetries int
	// BackoffBase is the base delay for exponential backoff between retries.
	BackoffBase time.Duration
	// AllowedPorts, when non-empty, restricts outbound connections to these TCP
	// ports. Empty uses the SSRF library default (80, 443). Tests set this to
	// allow an httptest server's ephemeral port WITHOUT relaxing the IP guard.
	AllowedPorts []uint16
	// AllowPrivateNetworks, when true, disables the SSRF IP guard entirely. It
	// exists ONLY so integration tests can target a loopback fake-agent server;
	// it MUST never be enabled in production. Defaults to false (guard on).
	AllowPrivateNetworks bool
	// InsecureSkipTLSVerify, when true, disables TLS certificate verification on
	// the transport. It exists ONLY so tests can target an httptest TLS server
	// with a self-signed certificate; it MUST never be enabled in production.
	// Defaults to false (verification on).
	InsecureSkipTLSVerify bool
}

// DefaultConfig is the production default: a 30s timeout, two retries with a
// 200ms backoff base, ports restricted to 80/443, SSRF guard ON.
func DefaultConfig() Config {
	return Config{
		Timeout:     30 * time.Second,
		MaxRetries:  2,
		BackoffBase: 200 * time.Millisecond,
	}
}

// Client is an SSRF-hardened HTTP client with retries and tracing.
type Client struct {
	http        *http.Client
	maxRetries  int
	backoffBase time.Duration
}

// New builds an SSRF-hardened Client from cfg.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 200 * time.Millisecond
	}

	// Loud-log the test-only escape hatches at startup so a misconfigured prod
	// deployment can't silently lose SSRF/TLS protection. Neither flag is bound
	// to any config key, so this can only fire when test code constructs a
	// Client with them explicitly set.
	if cfg.AllowPrivateNetworks {
		slog.Warn("httpclient SSRF guard DISABLED — AllowPrivateNetworks=true (test-only escape hatch; never use in production)")
	}
	if cfg.InsecureSkipTLSVerify {
		slog.Warn("httpclient TLS verification DISABLED — InsecureSkipTLSVerify=true (test-only escape hatch; never use in production)")
	}

	var opts []ssrf.Option
	if len(cfg.AllowedPorts) > 0 {
		opts = append(opts, ssrf.WithPorts(cfg.AllowedPorts...))
	}
	if cfg.AllowPrivateNetworks {
		// Test-only escape hatch: explicitly ALLOW loopback/private prefixes (the
		// guard checks allowed prefixes before its deny list) and any port, so a
		// loopback fake-agent httptest server can be reached. Never set in
		// production (see Config docs).
		opts = append(opts,
			ssrf.WithAnyPort(),
			ssrf.WithAllowedV4Prefixes(
				netip.MustParsePrefix("127.0.0.0/8"),
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("172.16.0.0/12"),
				netip.MustParsePrefix("192.168.0.0/16"),
			),
			ssrf.WithAllowedV6Prefixes(
				netip.MustParsePrefix("::1/128"),
			),
		)
	}
	guardian := ssrf.New(opts...)

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs after DNS resolution with the resolved address; it rejects
		// prohibited destinations before the socket connects (resolve-then-pin).
		Control: guardian.Safe,
	}

	base := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// A small response-header timeout bounds slowloris-style agent stalls.
		ResponseHeaderTimeout: cfg.Timeout,
	}
	if cfg.InsecureSkipTLSVerify {
		// Test-only: trust a self-signed httptest TLS server. Never set in prod.
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only escape hatch
	}

	return &Client{
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: otelhttp.NewTransport(base),
		},
		maxRetries:  cfg.MaxRetries,
		backoffBase: cfg.BackoffBase,
	}
}

// HTTPClient exposes the underlying *http.Client for callers that need it (e.g.
// a single non-retried probe). The SSRF transport is preserved.
func (c *Client) HTTPClient() *http.Client { return c.http }

// Do executes req with bounded retries on transient failures. The request body,
// if any, must be re-readable across attempts; callers should pass a body via
// GetBody (http.NewRequestWithContext sets it for bytes/strings readers). A
// non-2xx/5xx response (e.g. 4xx) is returned without retry. The caller owns
// closing the returned response body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	var lastErr error
	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff: base * 2^(attempt-1), honoring ctx cancellation.
			delay := c.backoffBase << (attempt - 1)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
			// Rewind the body for the retry, if the request carries one.
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("rewind request body: %w", err)
				}
				req.Body = body
			}
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			// An SSRF rejection (prohibited IP/port/network) is permanent — never
			// retry it; surface it immediately so the caller (and tests) see it.
			if isProhibited(err) {
				return nil, err
			}
			continue
		}
		// Retry 5xx (transient server error); return everything else to the caller.
		if resp.StatusCode >= 500 && attempt < attempts-1 {
			lastErr = fmt.Errorf("server returned status %d", resp.StatusCode)
			drainClose(resp.Body)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("request failed")
	}
	return nil, lastErr
}

// DoOnce sends the request EXACTLY ONCE — no automatic retries on network
// errors or 5xx. Use this for non-idempotent calls whose payload is single-use
// (e.g. CP→agent signed commands, where the JWT's jti is consumed on the
// agent's first receive; an auto-retry of the same JWT would be a legitimate
// cross-request replay from the agent's POV and reject with 403). The SSRF
// dialer + bounded timeout + otelhttp still apply.
//
// Callers that want retries should do it at the right semantic layer (e.g.
// mint a fresh JWT in the next River job attempt).
func (c *Client) DoOnce(req *http.Request) (*http.Response, error) {
	return c.http.Do(req)
}

// IsSSRFBlocked reports whether err is (or wraps) an SSRF prohibition: a
// destination IP, port, or network the guardian refused to dial. Callers and
// tests use this to assert that an attacker-influenced URL was blocked.
func IsSSRFBlocked(err error) bool { return isProhibited(err) }

// isProhibited reports whether err (or a wrapped cause) is an SSRF prohibition.
// The guardian error surfaces wrapped in a *net.OpError from the dialer, but we
// also match it directly via errors.Is for robustness across wrapping.
func isProhibited(err error) bool {
	if errors.Is(err, ssrf.ErrProhibitedIP) ||
		errors.Is(err, ssrf.ErrProhibitedPort) ||
		errors.Is(err, ssrf.ErrProhibitedNetwork) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, ssrf.ErrProhibitedIP) ||
			errors.Is(opErr.Err, ssrf.ErrProhibitedPort) ||
			errors.Is(opErr.Err, ssrf.ErrProhibitedNetwork)
	}
	return false
}

func drainClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}
