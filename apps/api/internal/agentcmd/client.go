package agentcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// commandPathFormat is the agent's signed-command REST route (class-router.php).
const commandPathFormat = "/wp-json/wpmgr/v1/command/%s"

// maxRespBody bounds the agent response we will read into memory.
const maxRespBody = 4 << 20 // 4 MiB

// Doer is the subset of the SSRF client the command client needs. *httpclient.Client
// satisfies it; tests can substitute a fake.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
	// DoOnce sends the request EXACTLY ONCE — no automatic retries. Required
	// for signed-command POSTs: the JWT's jti is single-use on the agent, so
	// a network-level retry of the SAME JWT would (correctly) be rejected as
	// token_replay. *httpclient.Client implements both Do (retrying) and
	// DoOnce; test doubles can route both to the same single-attempt call.
	DoOnce(req *http.Request) (*http.Response, error)
}

// Client POSTs signed CP->agent commands over the SSRF-hardened transport.
type Client struct {
	http   Doer
	signer *Signer
	clock  func() time.Time
}

// NewClient builds a command Client. http MUST be the SSRF-hardened client
// (ADR-009) because site URLs are attacker-influenced.
func NewClient(http Doer, signer *Signer) *Client {
	return &Client{http: http, signer: signer, clock: time.Now}
}

// SetClock overrides the time source (tests).
func (c *Client) SetClock(now func() time.Time) { c.clock = now }

// Update sends the signed `update` command to the site's agent and returns the
// parsed response. siteID is the target site's stable enrollment UUID; it is
// bound into the JWT's aud claim so the agent rejects a token minted for a
// different site (anti cross-tenant replay).
func (c *Client) Update(ctx context.Context, siteID uuid.UUID, siteURL string, req UpdateRequest) (UpdateResponse, error) {
	var out UpdateResponse
	if err := c.post(ctx, siteID, siteURL, "update", req, &out); err != nil {
		return UpdateResponse{}, err
	}
	return out, nil
}

// Rollback sends the signed `rollback` command to the site's agent. siteID is
// the target site's stable enrollment UUID, bound into the JWT's aud claim.
func (c *Client) Rollback(ctx context.Context, siteID uuid.UUID, siteURL string, req RollbackRequest) (RollbackResponse, error) {
	var out RollbackResponse
	if err := c.post(ctx, siteID, siteURL, "rollback", req, &out); err != nil {
		return RollbackResponse{}, err
	}
	return out, nil
}

// Backup sends the signed `backup` command to the site's agent. siteID is the
// target site's stable enrollment UUID, bound into the JWT's aud claim. The
// request carries the age PUBLIC recipient and CP callback endpoints — NEVER a
// decryption key (see backup_contract.go, trust model).
func (c *Client) Backup(ctx context.Context, siteID uuid.UUID, siteURL string, req BackupRequest) (BackupResponse, error) {
	var out BackupResponse
	if err := c.post(ctx, siteID, siteURL, "backup", req, &out); err != nil {
		return BackupResponse{}, err
	}
	return out, nil
}

// Restore sends the signed `restore` command to the site's agent. siteID is the
// target site's stable enrollment UUID, bound into the JWT's aud claim. The
// request carries presigned GET URLs + the ordered manifest — NEVER a decryption
// key; the agent decrypts with the age identity it alone holds.
func (c *Client) Restore(ctx context.Context, siteID uuid.UUID, siteURL string, req RestoreRequest) (RestoreResponse, error) {
	var out RestoreResponse
	if err := c.post(ctx, siteID, siteURL, "restore", req, &out); err != nil {
		return RestoreResponse{}, err
	}
	return out, nil
}

// RefreshInventory sends the signed `refresh_inventory` command to the site's
// agent. siteID is the target site's stable enrollment UUID, bound into the
// JWT's aud claim. The agent re-reads its plugin/theme inventory + update
// transients and pushes the result back over /agent/v1/metadata; this call
// only acks the command. Old agents that don't implement the route will return
// a 404 from the REST API — callers (the refresh worker) should treat that as
// an old-agent fallback and not a retryable error.
func (c *Client) RefreshInventory(ctx context.Context, siteID uuid.UUID, siteURL string, req RefreshInventoryRequest) (RefreshInventoryResponse, error) {
	var out RefreshInventoryResponse
	if err := c.post(ctx, siteID, siteURL, "refresh_inventory", req, &out); err != nil {
		return RefreshInventoryResponse{}, err
	}
	return out, nil
}

// Diagnostics sends the signed `diagnostics` command to the site's agent and
// returns the RAW 14-category JSON body. siteID is bound into the JWT's aud
// claim. Unlike the typed commands above, the response is NOT decoded into a
// struct here — the diagnostics blob is consumed directly by
// diagnostics.Service.IngestDiagnostics, which walks the top-level keys and
// upserts one row per category. Returning the raw bytes keeps the CP free of
// the agent's 14-category schema (it can evolve without a contract change on
// the CP). An old agent without the `diagnostics` route surfaces here as the
// canonical "rejected by agent: status 404 …" error from c.post; the caller
// (diagnostics.RefreshEnqueuerImpl) maps that to its own audit signal.
func (c *Client) Diagnostics(ctx context.Context, siteID uuid.UUID, siteURL string, _ DiagnosticsRequest) ([]byte, error) {
	return c.postRaw(ctx, siteID, siteURL, "diagnostics", struct{}{})
}

// post mints a fresh JWT bound to siteID (aud) and command (cmd), POSTs body to
// the named command endpoint at siteURL, and decodes the JSON response into
// out. A non-2xx response is an error.
func (c *Client) post(ctx context.Context, siteID uuid.UUID, siteURL, command string, body, out any) error {
	data, err := c.postRaw(ctx, siteID, siteURL, command, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", command, err)
	}
	return nil
}

// postRaw mints a fresh JWT bound to siteID (aud) and command (cmd), POSTs
// body to the named command endpoint at siteURL, and returns the raw 2xx
// response body. Callers needing typed decoding go through post(); callers
// that want to pass the body straight to a downstream ingester (diagnostics)
// use postRaw directly. A non-2xx response is wrapped in the canonical
// "rejected by agent: status NNN body=…" error format.
func (c *Client) postRaw(ctx context.Context, siteID uuid.UUID, siteURL, command string, body any) ([]byte, error) {
	endpoint, err := joinCommandURL(siteURL, command)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal %s command: %w", command, err)
	}

	token, _, err := c.signer.Mint(c.clock(), siteID.String(), command)
	if err != nil {
		return nil, fmt.Errorf("mint command jwt: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", command, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// The agent's permission_callback expects a Bearer token (class-router.php
	// bearerToken()). NEVER log this header value (it is a credential).
	httpReq.Header.Set("Authorization", "Bearer "+token)

	// DoOnce (no auto-retry): the JWT's jti is single-use on the agent — if the
	// first attempt's response is lost, an httpclient-level retry of the SAME
	// request would re-present the same JWT and the agent would (correctly) 403
	// with token_replay. Retries belong at the River job layer, which mints a
	// FRESH jti on the next attempt.
	resp, err := c.http.DoOnce(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s command transport: %w", command, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody)); _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", command, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Include a CLAMPED snippet of the agent's response body in the error so
		// operators can see the WP_Error code (e.g. wpmgr_aud_mismatch). The
		// agent never includes the bearer/JWT in its responses, so this is safe
		// to surface — it's the only way to tell "why" a 403/4xx happened
		// without round-tripping into the site's debug.log.
		snippet := string(data)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…(truncated)"
		}
		return nil, fmt.Errorf("%s command rejected by agent: status %d body=%s", command, resp.StatusCode, snippet)
	}
	return data, nil
}

// joinCommandURL builds {siteURL}/wp-json/wpmgr/v1/command/{command}, tolerating
// a trailing slash on the site URL and rejecting a non-http(s) scheme.
func joinCommandURL(siteURL, command string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(siteURL))
	if err != nil {
		return "", fmt.Errorf("invalid site url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid site url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("site url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + fmt.Sprintf(commandPathFormat, command)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Probe is a post-update site health probe over the SSRF-hardened client. It
// fetches the given URL with a short timeout and reports the HTTP status and a
// best-effort fatal-error signature found in the (bounded) response body.
type Probe struct {
	http *httpclient.Client
}

// NewProbe builds a Probe around the SSRF-hardened client.
func NewProbe(client *httpclient.Client) *Probe { return &Probe{http: client} }

// ProbeResult is the outcome of a health probe.
type ProbeResult struct {
	StatusCode int
	// Fatal is true when the response looks like a PHP fatal / WSOD even with a
	// 200 status (WordPress sometimes returns 200 with a fatal-error body).
	Fatal bool
	// Detail is a short reason string for logging/audit.
	Detail string
}

// Healthy reports whether the probe indicates a healthy site: a non-5xx status
// and no fatal-error signature.
func (r ProbeResult) Healthy() bool {
	return r.StatusCode > 0 && r.StatusCode < 500 && !r.Fatal
}

// fatalSignatures are substrings that indicate a broken WordPress page after an
// update (a white-screen-of-death / PHP fatal). Matched case-insensitively over
// the first chunk of the body.
var fatalSignatures = []string{
	"fatal error",
	"there has been a critical error on this website",
	"parse error: syntax error",
	"uncaught error",
	"cannot redeclare",
}

// Get probes targetURL and classifies the result. A transport error (including
// an SSRF block) is returned as err; a reachable-but-broken site is a non-error
// ProbeResult with Healthy()==false.
func (p *Probe) Get(ctx context.Context, targetURL string) (ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("User-Agent", "WPMgr-HealthProbe/1.0")

	resp, err := p.http.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody)); _ = resp.Body.Close() }()

	// Read a bounded prefix to scan for fatal-error signatures.
	const scanLimit = 64 << 10
	body, _ := io.ReadAll(io.LimitReader(resp.Body, scanLimit))
	lower := strings.ToLower(string(body))
	res := ProbeResult{StatusCode: resp.StatusCode}
	for _, sig := range fatalSignatures {
		if strings.Contains(lower, sig) {
			res.Fatal = true
			res.Detail = "fatal-error signature in response body"
			return res, nil
		}
	}
	if resp.StatusCode >= 500 {
		res.Detail = fmt.Sprintf("server returned status %d", resp.StatusCode)
	}
	return res, nil
}
