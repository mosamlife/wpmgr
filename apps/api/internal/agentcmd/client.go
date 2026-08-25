package agentcmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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

// IncrementalBackup sends the signed `backup` command with an incremental
// payload (ADR-048). The command name on the wire is still "backup"; the agent
// detects is_incremental=true in the body and runs the incremental pipeline.
// file_index_endpoint carries the URL the agent GETs to stream the previous
// snapshot's NDJSON file index. An empty file_index_endpoint signals the agent
// to fall back to a full-base run (AUTO-BASE).
func (c *Client) IncrementalBackup(ctx context.Context, siteID uuid.UUID, siteURL string, req IncrementalBackupRequest) (BackupResponse, error) {
	var out BackupResponse
	if err := c.post(ctx, siteID, siteURL, "backup", req, &out); err != nil {
		return BackupResponse{}, err
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

// AgentSelfUpdate sends the signed `agent_self_update` command to the site's
// agent, beat 1 (ARM) of the three-beat self-update protocol (see
// agent_self_update_contract.go). siteID is the target site's stable
// enrollment UUID, bound into the JWT's aud claim.
//
// This call NEVER applies anything: a 200 with status "scheduled" only means
// the agent verified a newer build and applied it inside this same request
// it in a separate WordPress bootstrap. Success is established later and
// elsewhere, by the NEW code reporting its version (see
// update.AgentConfirmWorker); callers must never record a "scheduled" ack as a
// successful upgrade.
func (c *Client) AgentSelfUpdate(ctx context.Context, siteID uuid.UUID, siteURL string, req AgentSelfUpdateRequest) (AgentSelfUpdateResponse, error) {
	var out AgentSelfUpdateResponse
	if err := c.post(ctx, siteID, siteURL, "agent_self_update", req, &out); err != nil {
		return AgentSelfUpdateResponse{}, err
	}
	return out, nil
}

// SyncErrorConfig sends the signed `sync_error_config` command to the site's
// agent, pushing the per-site PHP error-level mask and ignore-list. siteID is
// the target site's stable enrollment UUID, bound into the JWT's aud claim.
// An ok=false response (with a 200 HTTP status) is treated as an error with
// the agent's detail message — the caller should propagate it as a transient
// failure rather than storing the config as successfully applied.
func (c *Client) SyncErrorConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req ErrorConfigRequest) (ErrorConfigResult, error) {
	var out ErrorConfigResult
	if err := c.post(ctx, siteID, siteURL, "sync_error_config", req, &out); err != nil {
		return ErrorConfigResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_error_config rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// SyncSecurityConfig sends the signed `sync_security_config` command to the
// site's agent, pushing the per-site login-protection mode, thresholds, and
// CIDR allow/deny lists. siteID is the target site's stable enrollment UUID,
// bound into the JWT's aud claim.  An ok=false response (with a 200 HTTP
// status) is treated as an error with the agent's detail message.
func (c *Client) SyncSecurityConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req SecurityConfigRequest) (SecurityConfigResult, error) {
	var out SecurityConfigResult
	if err := c.post(ctx, siteID, siteURL, "sync_security_config", req, &out); err != nil {
		return SecurityConfigResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_security_config rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// UnblockIP sends the signed `unblock_ip` command to the site's agent,
// removing any active block for the given IP address. siteID is the target
// site's stable enrollment UUID, bound into the JWT's aud claim. An ok=false
// response (with a 200 HTTP status) is treated as an error.
func (c *Client) UnblockIP(ctx context.Context, siteID uuid.UUID, siteURL string, req UnblockIPRequest) (UnblockIPResult, error) {
	var out UnblockIPResult
	if err := c.post(ctx, siteID, siteURL, "unblock_ip", req, &out); err != nil {
		return UnblockIPResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("unblock_ip rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// SyncSecurityHardening sends the signed `sync_security_hardening` command to
// the site's agent, pushing the full per-site hardening config snapshot and the
// current ban list (ADR-057 Phase 1). siteID is the target site's stable
// enrollment UUID, bound into the JWT's aud claim. An ok=false response (HTTP
// 200) is treated as an error with the agent's detail message.
func (c *Client) SyncSecurityHardening(ctx context.Context, siteID uuid.UUID, siteURL string, req HardeningRequest) (HardeningResult, error) {
	var out HardeningResult
	if err := c.post(ctx, siteID, siteURL, "sync_security_hardening", req, &out); err != nil {
		return HardeningResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_security_hardening rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// SyncSecurityPolicy sends the signed `sync_security_policy` command to the
// site's agent, pushing the full per-site user 2FA + password + hide-backend
// policy snapshot (ADR-059 Phase 3). siteID is the target site's stable
// enrollment UUID, bound into the JWT's aud claim. An ok=false response (HTTP
// 200) is treated as an error with the agent's detail message. DoOnce is used
// because the JWT jti is single-use.
func (c *Client) SyncSecurityPolicy(ctx context.Context, siteID uuid.UUID, siteURL string, req SecurityPolicyRequest) (SecurityPolicyResult, error) {
	var out SecurityPolicyResult
	if err := c.post(ctx, siteID, siteURL, "sync_security_policy", req, &out); err != nil {
		return SecurityPolicyResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_security_policy rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// SyncLoginBrand sends the signed `sync_login_brand` command to the site's
// agent, pushing the per-site login brand config (logo URL, logo link,
// message). siteID is the target site's stable enrollment UUID, bound into the
// JWT's aud claim. An ok=false response (with a 200 HTTP status) is treated as
// an error with the agent's detail message.
func (c *Client) SyncLoginBrand(ctx context.Context, siteID uuid.UUID, siteURL string, req LoginBrandRequest) (LoginBrandResult, error) {
	var out LoginBrandResult
	if err := c.post(ctx, siteID, siteURL, "sync_login_brand", req, &out); err != nil {
		return LoginBrandResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_login_brand rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// Scan sends the signed `scan` command to the site's agent, which walks the
// filesystem and returns an inline batch of file hashes. siteID is bound into
// the JWT's aud claim. DoOnce is used because the JWT jti is single-use; River
// retries with a fresh JWT. An old agent without the scan route returns a 404
// which surfaces here as the canonical "status 404" error; the worker maps it
// to "agent_too_old". The response body can be up to ~4 MiB (core ≈ 2.5k
// files ≈ 225 KB; full-site batches fit within the configured 512-file cap).
func (c *Client) Scan(ctx context.Context, siteID uuid.UUID, siteURL string, req ScanRequest) (ScanResponse, error) {
	var out ScanResponse
	if err := c.post(ctx, siteID, siteURL, "scan", req, &out); err != nil {
		return ScanResponse{}, err
	}
	return out, nil
}

// GetFile sends the signed `get_file` command to the site's agent, fetching
// up to max_bytes of a single file as base64. siteID is bound into the JWT's
// aud claim. The CP only calls this for paths that are already stored findings
// (server-side guard). DoOnce is used because the JWT jti is single-use.
func (c *Client) GetFile(ctx context.Context, siteID uuid.UUID, siteURL string, req GetFileRequest) (GetFileResponse, error) {
	var out GetFileResponse
	if err := c.post(ctx, siteID, siteURL, "get_file", req, &out); err != nil {
		return GetFileResponse{}, err
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

// Metadata sends the signed `metadata` command to the site's agent and returns
// the RAW JSON body (the same shape the agent pushes via POST /agent/v1/metadata
// on its own schedule). siteID is bound into the JWT's aud claim. Unlike the
// typed commands, the response is NOT decoded into a struct here — the raw bytes
// are fed into the site.Service metadata ingester (ApplyAgentMetadata → site
// inventory update + last_seen_at bump + optional connected-state recovery).
//
// The agent's metadata command is SYNCHRONOUS: it collects the full plugin/theme
// inventory and returns it in the 200 body. This is used by the Re-check
// connection endpoint (M58) to force an immediate liveness check from the CP.
func (c *Client) Metadata(ctx context.Context, siteID uuid.UUID, siteURL string, _ MetadataRequest) ([]byte, error) {
	return c.postRaw(ctx, siteID, siteURL, "metadata", struct{}{})
}

// MetadataRaw satisfies site.AgentRechecker: sends the signed `metadata`
// command and returns the raw JSON response body. This thin wrapper exists so
// *Client satisfies the site package's AgentRechecker interface without
// the site package needing to know agentcmd internals.
func (c *Client) MetadataRaw(ctx context.Context, siteID uuid.UUID, siteURL string) ([]byte, error) {
	return c.Metadata(ctx, siteID, siteURL, MetadataRequest{})
}

// Ping sends the signed `ping` command to the site's agent and returns the
// parsed response (0.44.0 active liveness probe). siteID is bound into the
// JWT's aud claim. An old agent without the ping route returns a 404 from the
// REST API — callers should fall back to MetadataRaw for backward compatibility.
// Ping never logs the response body; only the ok field is inspected.
func (c *Client) Ping(ctx context.Context, siteID uuid.UUID, siteURL string) (PingResponse, error) {
	var out PingResponse
	if err := c.post(ctx, siteID, siteURL, "ping", PingRequest{}, &out); err != nil {
		return PingResponse{}, err
	}
	return out, nil
}

// ReachabilityReason classifies WHY VerifyReachableWithReason concluded alive
// or not-alive (GH #291 Task 3). Under the old bool-only contract an agent
// that is merely UNINSTALLED (nothing answers the wpmgr routes at all) looked
// identical to one whose SITE IS BROKEN (the plugin is there but PHP is
// fatal-ing), both collapsed to alive=false with no way to tell them apart.
// That ambiguity would produce false alarms in a later alerting phase, so the
// reason is classified here, up front, even though this phase does not yet
// act on it.
type ReachabilityReason string

const (
	// ReasonAlive: a check succeeded (ping 2xx ok=true, or the metadata
	// fallback returned a recognizably agent-shaped 2xx).
	ReasonAlive ReachabilityReason = "alive"
	// ReasonAgentAbsent404: ping AND the metadata fallback both 404'd,
	// meaning nothing that looks like the plugin is installed at this URL at
	// all, as opposed to an installed-but-broken agent.
	ReasonAgentAbsent404 ReachabilityReason = "agent_absent_404"
	// ReasonHTTP4xx: the agent (or something at the URL) answered with a
	// non-404 client error (400/401/403/etc). The server responded, so
	// something booted, but the request was rejected.
	ReasonHTTP4xx ReachabilityReason = "http_4xx"
	// ReasonHTTP5xx: the agent responded but with a server error, the
	// strongest "installed but broken" signal short of a hard transport
	// failure, and the case GH #291 needs to be able to name.
	ReasonHTTP5xx ReachabilityReason = "http_5xx"
	// ReasonTimeout: the dial or read exceeded the caller's deadline.
	ReasonTimeout ReachabilityReason = "timeout"
	// ReasonTLSError: the TLS handshake failed (expired/mismatched/untrusted
	// certificate, or a non-TLS response on an https port).
	ReasonTLSError ReachabilityReason = "tls_error"
	// ReasonNotAgentShaped: something answered 2xx but the body was not
	// agent-shaped JSON (captive portal, generic maintenance splash), so the
	// URL is reachable, but not by our agent.
	ReasonNotAgentShaped ReachabilityReason = "not_agent_shaped"
	// ReasonUnreachable is the catch-all transport failure (connection
	// refused, DNS failure, SSRF-blocked, JWT-mint failure, etc) that does
	// not match any of the more specific classifications above.
	ReasonUnreachable ReachabilityReason = "unreachable"
)

// VerifyReachable implements the 0.44.0 liveness-fallback contract:
//
//  1. Try the `ping` command (new agents).  A 2xx response means alive.
//  2. When ping returns a 404 or 400 (old agent / unknown command), fall back
//     to MetadataRaw.  A 2xx metadata response means alive.
//  3. Anything else (transport error, 5xx, both-fail) means not alive.
//
// The function returns (true, nil) when the site is reachable, (false, nil)
// when it is not, and (false, err) only on unexpected infrastructure failures
// (e.g. JWT-mint failure). Response bodies are discarded beyond the minimal
// decode needed to detect the error; nothing is logged by this function, // callers should emit structured log lines with the returned outcome.
//
// This is a thin wrapper around VerifyReachableWithReason that discards the
// classified reason, so the two implementations can never drift and every
// existing caller (the connection Sweeper's AgentVerifier interface, and its
// tests) is unaffected by the addition of the reason.
func (c *Client) VerifyReachable(ctx context.Context, siteID uuid.UUID, siteURL string) (alive bool, fallbackUsed bool, err error) {
	alive, fallbackUsed, _, err = c.VerifyReachableWithReason(ctx, siteID, siteURL)
	return alive, fallbackUsed, err
}

// VerifyReachableWithReason is VerifyReachable's typed-reason superset (GH
// #291 Task 3): identical alive/fallbackUsed/err outcomes for every case
// VerifyReachable already handles, plus a ReachabilityReason classification
// of WHY. Persisting or alerting on the reason is intentionally out of scope
// for this phase; it is classified here so a later phase does not have to
// re-derive it from scratch.
func (c *Client) VerifyReachableWithReason(ctx context.Context, siteID uuid.UUID, siteURL string) (alive bool, fallbackUsed bool, reason ReachabilityReason, err error) {
	out, pingErr := c.Ping(ctx, siteID, siteURL)
	if pingErr == nil && out.OK {
		return true, false, ReasonAlive, nil
	}

	// Fall back to the metadata command when ping looks like an old agent
	// (404/400 unknown command) or when something answered 2xx without the
	// agent-shaped ok field (captive portal, generic maintenance page): the
	// metadata shape check below settles whether a real agent is behind it.
	fallback := pingErr == nil && !out.OK
	if pingErr != nil {
		pingErrStr := pingErr.Error()
		fallback = strings.Contains(pingErrStr, "status 404") ||
			strings.Contains(pingErrStr, "status 400")
	}
	if fallback {
		raw, metaErr := c.MetadataRaw(ctx, siteID, siteURL)
		if metaErr == nil && looksLikeAgentMetadata(raw) {
			return true, true, ReasonAlive, nil
		}
		// Both ping and shape-checked metadata failed, so not alive. Classify
		// using whichever error is available: a hard 404 on ping means
		// "nothing here at all", while a 2xx-but-wrong-shape ping means
		// something answered, just not our agent.
		if pingErr != nil {
			if status, ok := extractHTTPStatus(pingErr); ok && status == http.StatusNotFound {
				return false, true, ReasonAgentAbsent404, nil
			}
			return false, true, classifyTransportErr(pingErr), nil
		}
		return false, true, ReasonNotAgentShaped, nil
	}

	// Hard transport / 5xx failure on ping, so not alive. The JWT-mint path
	// returns a format error (no status code), so it falls here too: treat it
	// as not alive rather than an infra error, to avoid blocking the sweeper.
	return false, false, classifyTransportErr(pingErr), nil
}

// httpStatusPattern extracts the numeric status code from the canonical
// "…rejected by agent: status NNN body=…" error format built by postRaw, and
// from the "…transport: %w" wrapping used for lower-level failures (which
// never contains a "status NNN" substring, so the pattern simply does not
// match there).
var httpStatusPattern = regexp.MustCompile(`status (\d{3})\b`)

// extractHTTPStatus pulls the HTTP status code out of an agentcmd error, when
// present. ok=false means the error is a transport-level failure with no HTTP
// status at all (connection refused, DNS failure, timeout, TLS error, etc).
func extractHTTPStatus(err error) (status int, ok bool) {
	if err == nil {
		return 0, false
	}
	m := httpStatusPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	n, cerr := strconv.Atoi(m[1])
	if cerr != nil {
		return 0, false
	}
	return n, true
}

// classifyTransportErr classifies a non-nil VerifyReachable failure (from
// either the ping attempt directly, or after a failed metadata fallback) into
// a ReachabilityReason. It never returns ReasonAlive.
func classifyTransportErr(err error) ReachabilityReason {
	if err == nil {
		return ReasonUnreachable
	}
	if status, ok := extractHTTPStatus(err); ok {
		switch {
		case status == http.StatusNotFound:
			return ReasonAgentAbsent404
		case status >= 500:
			return ReasonHTTP5xx
		case status >= 400:
			return ReasonHTTP4xx
		}
	}
	if isTimeoutErr(err) {
		return ReasonTimeout
	}
	if isTLSErr(err) {
		return ReasonTLSError
	}
	return ReasonUnreachable
}

// isTimeoutErr reports whether err is (or wraps) a deadline/timeout failure,
// either the net.Error Timeout() signal from the transport, or the context
// package's own deadline-exceeded error.
func isTimeoutErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// IsTimeoutErr is the exported form of isTimeoutErr, for callers outside this
// package that need to distinguish "the control plane's own read/deadline
// expired" from every other command failure. The update package's agent
// self-update worker uses it: on a non-detaching SAPI the agent's ack is
// written only after the whole apply, so the control plane's read timing out
// on that specific command is not evidence the upgrade failed, only that this
// request could not wait for the answer (see agent_worker.go). Every other
// caller of this predicate (retry/backoff classification, reachability
// probing) continues to use the unexported form; export only widens visibility,
// it does not change what the predicate matches.
func IsTimeoutErr(err error) bool { return isTimeoutErr(err) }

// isTLSErr reports whether err is (or wraps) a TLS handshake failure: an
// invalid, hostname-mismatched, or untrusted certificate, or a malformed TLS
// record (e.g. a plaintext response on an https port).
func isTLSErr(err error) bool {
	var certErr x509.CertificateInvalidError
	if errors.As(err, &certErr) {
		return true
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return true
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return true
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	return false
}

// looksLikeAgentMetadata is the liveness shape check for the metadata
// fallback: a 2xx alone must not count as alive (a captive portal or generic
// 200 page would pass), so require the one field every agent version's
// metadata command has always returned. Deliberately local — the tolerant
// decoder in internal/agent accepts {} and would defeat the check (and
// importing it would cycle: internal/agent imports this package).
func looksLikeAgentMetadata(raw []byte) bool {
	var probe struct {
		WPVersion    string `json:"wp_version"`
		AgentVersion string `json:"agent_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.WPVersion != "" || probe.AgentVersion != ""
}

// MediaOptimize sends the signed `media_optimize` command to the site's agent
// (ADR-043). siteID is bound into the JWT's aud claim. The agent presigned-PUTs
// each job's source variants and calls back to the CP's encode-ready endpoint;
// this response is just the command ack. An old agent without the route returns
// a 404 surfaced as the canonical "rejected by agent: status 404 …" error.
func (c *Client) MediaOptimize(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaOptimizeRequest) (MediaOptimizeResponse, error) {
	var out MediaOptimizeResponse
	if err := c.post(ctx, siteID, siteURL, "media_optimize", req, &out); err != nil {
		return MediaOptimizeResponse{}, err
	}
	return out, nil
}

// MediaApply sends the signed `media_apply` command: the encoder finished an
// attachment's variants and the agent should presigned-GET out/* and apply them
// on disk, then call back to the job-status endpoint. siteID is bound into aud.
func (c *Client) MediaApply(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaApplyRequest) (MediaApplyResponse, error) {
	var out MediaApplyResponse
	if err := c.post(ctx, siteID, siteURL, "media_apply", req, &out); err != nil {
		return MediaApplyResponse{}, err
	}
	return out, nil
}

// MediaSync sends the signed `media_sync` command: the agent enumerates its
// media library and pushes pages to the CP's sync-batch endpoint. siteID is
// bound into aud.
func (c *Client) MediaSync(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaSyncRequest) (MediaSyncResponse, error) {
	var out MediaSyncResponse
	if err := c.post(ctx, siteID, siteURL, "media_sync", req, &out); err != nil {
		return MediaSyncResponse{}, err
	}
	return out, nil
}

// MediaRestore sends the signed `media_restore` command: revert the attachments
// behind the job ids to their pre-optimization state. siteID is bound into aud.
func (c *Client) MediaRestore(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaRestoreRequest) (MediaRestoreResponse, error) {
	var out MediaRestoreResponse
	if err := c.post(ctx, siteID, siteURL, "media_restore", req, &out); err != nil {
		return MediaRestoreResponse{}, err
	}
	return out, nil
}

// MediaDeleteOriginals sends the signed `media_delete_originals` command:
// IRREVERSIBLY delete the archived originals behind the job ids. siteID is
// bound into aud.
func (c *Client) MediaDeleteOriginals(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaDeleteOriginalsRequest) (MediaDeleteOriginalsResponse, error) {
	var out MediaDeleteOriginalsResponse
	if err := c.post(ctx, siteID, siteURL, "media_delete_originals", req, &out); err != nil {
		return MediaDeleteOriginalsResponse{}, err
	}
	return out, nil
}

// SyncMediaConfig sends the signed `sync_media_config` command to the site's
// agent, pushing the per-site auto-optimize settings (enabled toggle,
// target_format, target_quality). siteID is the target site's stable enrollment
// UUID, bound into the JWT's aud claim. An ok=false response (with a 200 HTTP
// status) is treated as an error with the agent's detail message (ADR-044 §4).
func (c *Client) SyncMediaConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaConfigRequest) (MediaConfigResult, error) {
	var out MediaConfigResult
	if err := c.post(ctx, siteID, siteURL, "sync_media_config", req, &out); err != nil {
		return MediaConfigResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_media_config rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// SyncPerfConfig sends the signed `perf_config_update` command to the site's
// agent, pushing the per-site performance config so the agent mirrors it on its
// request fast-path (ADR-046). siteID is bound into the JWT's aud claim. CDN
// credentials are NEVER included in the request — the control plane holds the
// only decrypted copy. An ok=false response (HTTP 200) is treated as an error.
func (c *Client) SyncPerfConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req PerfConfigRequest) (PerfConfigResult, error) {
	var out PerfConfigResult
	if err := c.post(ctx, siteID, siteURL, "perf_config_update", req, &out); err != nil {
		return PerfConfigResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("perf_config_update rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// RucssCompute sends the signed `rucss_compute` command: the agent self-fetches
// the given URLs (or the home page) out-of-band so the optimizer runs the
// Remove-Unused-CSS stage, which posts each page to the CP and enqueues a compute
// job. siteID is bound into aud. An ok=false response (HTTP 200) is an error.
func (c *Client) RucssCompute(ctx context.Context, siteID uuid.UUID, siteURL string, req RucssComputeRequest) (RucssComputeResult, error) {
	var out RucssComputeResult
	if err := c.post(ctx, siteID, siteURL, "rucss_compute", req, &out); err != nil {
		return RucssComputeResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("rucss_compute rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// CacheEnable sends the signed `cache_enable` command: install the cache
// drop-in/.htaccess block and turn page caching on. siteID is bound into aud.
func (c *Client) CacheEnable(ctx context.Context, siteID uuid.UUID, siteURL string, req CacheEnableRequest) (CacheEnableResult, error) {
	var out CacheEnableResult
	if err := c.post(ctx, siteID, siteURL, "cache_enable", req, &out); err != nil {
		return CacheEnableResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("cache_enable rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// CacheDisable sends the signed `cache_disable` command: remove the cache
// drop-in/.htaccess block and turn page caching off. siteID is bound into aud.
func (c *Client) CacheDisable(ctx context.Context, siteID uuid.UUID, siteURL string, req CacheDisableRequest) (CacheDisableResult, error) {
	var out CacheDisableResult
	if err := c.post(ctx, siteID, siteURL, "cache_disable", req, &out); err != nil {
		return CacheDisableResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("cache_disable rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// CachePurge sends the signed `cache_purge` command: purge the whole cache
// (scope=all) or specific URLs (scope=url). siteID is bound into aud.
func (c *Client) CachePurge(ctx context.Context, siteID uuid.UUID, siteURL string, req CachePurgeRequest) (CachePurgeResult, error) {
	var out CachePurgeResult
	if err := c.post(ctx, siteID, siteURL, "cache_purge", req, &out); err != nil {
		return CachePurgeResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("cache_purge rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// CachePreload sends the signed `cache_preload` command: start a background
// warm pass over the sitemap. siteID is bound into aud.
func (c *Client) CachePreload(ctx context.Context, siteID uuid.UUID, siteURL string, req CachePreloadRequest) (CachePreloadResult, error) {
	var out CachePreloadResult
	if err := c.post(ctx, siteID, siteURL, "cache_preload", req, &out); err != nil {
		return CachePreloadResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("cache_preload rejected by agent: %s", out.Detail)
	}
	return out, nil
}

// DBClean sends the signed `db_clean` command to the site's agent. The agent
// ACKs immediately (ok+job_id) and then runs cleanup async, posting per-category
// progress to the ProgressEndpoint in the request body. siteID is bound into aud.
//
// Unlike most commands, DBClean does NOT wrap ok=false in an error — the caller
// (perf.Service.DBClean) must inspect the result and emit db.clean.failed SSE.
// A transport or non-2xx error IS returned as err (retryable).
func (c *Client) DBClean(ctx context.Context, siteID uuid.UUID, siteURL string, req DBCleanRequest) (DBCleanResult, error) {
	var out DBCleanResult
	if err := c.post(ctx, siteID, siteURL, "db_clean", req, &out); err != nil {
		return DBCleanResult{}, err
	}
	return out, nil
}

// DBScan sends the signed `db_scan` command to the site's agent. Unlike
// DBClean, the scan is SYNCHRONOUS: the full per-category result is returned
// in the ACK body (HTTP 200) without async progress pushes. siteID is bound
// into the JWT's aud claim. The CP should use a 60-second context timeout for
// this call (scan is READ-ONLY and fast by design — information_schema
// estimates — so a 60s cut-off is generous).
//
// DBScan does NOT wrap ok=false in an error — the caller (perf.Service.DBScan)
// must inspect the result and emit db.scan.failed SSE if ok=false.
func (c *Client) DBScan(ctx context.Context, siteID uuid.UUID, siteURL string, req DBScanRequest) (DBScanResult, error) {
	var out DBScanResult
	if err := c.post(ctx, siteID, siteURL, "db_scan", req, &out); err != nil {
		return DBScanResult{}, err
	}
	return out, nil
}

// DBTableAction sends the signed `db_table_action` command to the site's agent.
// The command is SYNCHRONOUS: optimize, repair, drop, empty, analyze, or
// convert_innodb is applied to one or more tables and the full per-table result
// is returned in the ACK body.
//
// DBTableAction does NOT wrap ok=false in an error — the caller
// (perf.Service.DBTableAction) must inspect the result and surface the per-table
// statuses. A transport error or non-2xx HTTP status IS returned as err.
func (c *Client) DBTableAction(ctx context.Context, siteID uuid.UUID, siteURL string, req DBTableActionRequest) (DBTableActionResult, error) {
	var out DBTableActionResult
	if err := c.post(ctx, siteID, siteURL, "db_table_action", req, &out); err != nil {
		return DBTableActionResult{}, err
	}
	return out, nil
}

// SearchReplace sends the signed `search_replace` command to the site's agent.
// The command is SYNCHRONOUS: the full per-table result is returned in the ACK
// body. dry_run=true returns the matched-row count without mutating any data.
//
// SearchReplace does NOT wrap ok=false in an error — the caller
// (perf.Service.SearchReplace) must inspect the result. A transport error or
// non-2xx HTTP status IS returned as err.
func (c *Client) SearchReplace(ctx context.Context, siteID uuid.UUID, siteURL string, req SearchReplaceRequest) (SearchReplaceResult, error) {
	var out SearchReplaceResult
	if err := c.post(ctx, siteID, siteURL, "search_replace", req, &out); err != nil {
		return SearchReplaceResult{}, err
	}
	return out, nil
}

// DbSnapshot sends the signed `db_snapshot` command to the site's agent.
// The command is SYNCHRONOUS for all four actions (create/list/revert/delete):
// the full result is returned in the ACK body.
//
// DbSnapshot does NOT wrap ok=false in an error — the caller must inspect the
// result. A transport error or non-2xx HTTP status IS returned as err.
func (c *Client) DbSnapshot(ctx context.Context, siteID uuid.UUID, siteURL string, req DbSnapshotRequest) (DbSnapshotResult, error) {
	var out DbSnapshotResult
	if err := c.post(ctx, siteID, siteURL, "db_snapshot", req, &out); err != nil {
		return DbSnapshotResult{}, err
	}
	return out, nil
}

// DBOrphanDelete sends the signed `db_orphan_delete` command to the site's
// agent. The command is ASYNC: the agent ACKs immediately with {ok, job_id}
// then processes the allowlist in the background, posting batched progress
// results to ProgressEndpoint. The CP never adds items to the allowlist; the
// agent may only SKIP items that fail live re-verification.
//
// DBOrphanDelete does NOT wrap ok=false in an error — the caller
// (perf.Service.DBOrphanDelete) must inspect the result and emit the
// appropriate SSE events. A transport error or non-2xx HTTP status IS returned
// as err.
func (c *Client) DBOrphanDelete(ctx context.Context, siteID uuid.UUID, siteURL string, req DBOrphanDeleteRequest) (DBOrphanDeleteResult, error) {
	var out DBOrphanDeleteResult
	if err := c.post(ctx, siteID, siteURL, "db_orphan_delete", req, &out); err != nil {
		return DBOrphanDeleteResult{}, err
	}
	return out, nil
}

// MediaClean sends the signed `media_clean` command to the site's agent.
// The command is SYNCHRONOUS for all four actions (scan/isolate/restore/delete):
// the full result is returned in the ACK body.
//
// MediaClean does NOT wrap ok=false in an error — the caller must inspect the
// result. A transport error or non-2xx HTTP status IS returned as err.
func (c *Client) MediaClean(ctx context.Context, siteID uuid.UUID, siteURL string, req MediaCleanRequest) (MediaCleanResult, error) {
	var out MediaCleanResult
	if err := c.post(ctx, siteID, siteURL, "media_clean", req, &out); err != nil {
		return MediaCleanResult{}, err
	}
	return out, nil
}

// Do is the public generic version of post, used by domain packages that issue
// a variety of commands without type-specific wrapper methods. It mints a fresh
// JWT bound to siteID, POSTs body to the named command endpoint, and decodes
// the JSON response into out. A non-2xx HTTP response is an error.
func (c *Client) Do(ctx context.Context, siteID uuid.UUID, siteURL, command string, body, out any) error {
	return c.post(ctx, siteID, siteURL, command, body, out)
}

// post mints a fresh JWT bound to siteID (aud) and command (cmd), POSTs body to
// the named command endpoint at siteURL, and decodes the JSON response into
// out. A non-2xx response is an error.
//
// post does NOT inspect the decoded body's ok field, and CANNOT: out is an
// untyped any, and the response shapes are not uniform (some commands carry no
// ok at all, the file_* family reports failure through an Error envelope, and
// the update family reports it per item). The ok=false-is-an-error rule is
// therefore applied by the individual wrapper methods, and only by the config
// PUSH family: SyncErrorConfig, SyncSecurityConfig, SyncSecurityHardening,
// SyncSecurityPolicy, SyncLoginBrand, SyncMediaConfig, SyncPerfConfig,
// SyncEmailConfig, UnblockIP, RucssCompute, CacheEnable, CacheDisable,
// CachePurge and CachePreload. Those are fire-and-forget pushes with no
// callback, so folding ok=false into the error is the only way their callers
// can tell the push did not land.
//
// Every OTHER wrapper returns the decoded response untouched and leaves the ok
// field to the caller. That split is incidental (each command added its own
// check, or did not, as it was written) rather than principled, and it is a
// trap: a caller of one of those methods that branches only on the returned
// error will silently treat a refusal as a success. A refresh_inventory that
// had been rejected on every call since it shipped stayed invisible in exactly
// that way. If you add a command here, either check ok in the wrapper or make
// sure the caller does, and say in a comment which one you chose.
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

// ResendEmail sends the signed `resend_email` command to the site's agent,
// asking it to re-send an email from its OWN log row, named by agent_seq.
// The CP's near-end preconditions (row exists, body captured, agent_seq known)
// are enforced by the email service before this call so a request that cannot
// succeed never spends a signed command.
//
// An agent too old to route the command answers 404, which post() returns as a
// transport error; the CP surfaces that as ok=false with a descriptive detail
// rather than a hard error.
func (c *Client) ResendEmail(ctx context.Context, siteID uuid.UUID, siteURL string, req ResendEmailRequest) (ResendEmailResult, error) {
	var out ResendEmailResult
	if err := c.post(ctx, siteID, siteURL, "resend_email", req, &out); err != nil {
		return ResendEmailResult{OK: false, Detail: err.Error()}, nil
	}
	return out, nil
}

// SyncEmailConfig sends the signed `sync_email_config` command to the site's
// agent, pushing the full per-site email configuration including the DECRYPTED
// provider secret so the agent can store it in its own keystore (m59, Phase 1
// foundation / Phase 2 implementation by the wp-agent-engineer).
// siteID is bound into the JWT's aud claim. An ok=false response (HTTP 200) is
// treated as an error with the agent's detail message.
//
// TODO(phase2-agent): agent must implement the sync_email_config command handler.
func (c *Client) SyncEmailConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req EmailConfigRequest) (EmailConfigResult, error) {
	var out EmailConfigResult
	if err := c.post(ctx, siteID, siteURL, "sync_email_config", req, &out); err != nil {
		return EmailConfigResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("sync_email_config rejected by agent: %s", out.Detail)
	}
	// An ok=true detail is otherwise discarded. A push that DELETED the site's
	// stored credential must not be recorded as a silent success (GH #380):
	// that is how a fleet lost working SMTP passwords without a single warning.
	if strings.Contains(out.Detail, "secret cleared") {
		slog.WarnContext(ctx, "email: agent reports its stored email secret was cleared by this config push",
			slog.String("site_id", siteID.String()),
			slog.String("detail", out.Detail),
		)
	}
	return out, nil
}

// SendTestEmail sends the signed `send_test_email` command to the site's agent,
// asking it to dispatch a test message using its current email config.
// sync_email_config must be called first so the agent has credentials.
// siteID is bound into the JWT's aud claim.
//
// Phase 1: the agent does not yet implement this command. The CP dispatches it
// anyway and surfaces the agent's "command not found" (404) response gracefully
// — callers should treat an error here as ok=false + non-fatal detail.
//
// TODO(phase2-agent): agent must implement the send_test_email command handler.
func (c *Client) SendTestEmail(ctx context.Context, siteID uuid.UUID, siteURL string, req SendTestEmailRequest) (SendTestEmailResult, error) {
	var out SendTestEmailResult
	if err := c.post(ctx, siteID, siteURL, "send_test_email", req, &out); err != nil {
		return SendTestEmailResult{}, err
	}
	return out, nil
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
	// CacheHit is true when the response carries a header (or Age value) that
	// identifies it as served from a page/edge cache rather than freshly
	// rendered, even though Get() appended a cache-busting query parameter
	// (see addCacheBuster). A cache that is keyed on path only, ignoring the
	// query string entirely (Cloudflare's "Ignore Query String" page rule,
	// Varnish configured that way, nginx keyed on $uri), defeats the buster
	// with one click, so this is the honest backstop: a caller MUST NOT treat
	// a CacheHit response as proof of anything about the current backend
	// state (GH #291 Phase 4 Change 3).
	CacheHit bool
	// Detail is a short reason string for logging/audit.
	Detail string
}

// Healthy reports whether the probe indicates a healthy site: a non-5xx status
// and no fatal-error signature. This is the ORIGINAL, unchanged reachability
// check: 401/403/404/410 all count as healthy here, which is deliberately
// lenient for a generic "did something answer" question. It is intentionally
// NOT used for the post-update rollback decision (see
// internal/update.classifyPostUpdateProbe), which needs a narrower rule (a
// 404/410 on the site root after an update IS a broken-update signal, a
// 401/403 is NOT). Adding that narrower rule as a separate, explicit
// classification next to its one caller, rather than changing what Healthy()
// means, keeps every other/future caller of this shared helper unaffected.
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

// cacheBusterParam is the query parameter Get appends to every probed URL
// (GH #291 Phase 4 Change 3). Deliberately NOT utm-adjacent: some managed
// hosts (WP Engine among them) strip utm_* params server-side before PHP ever
// sees them, which would silently neutralize a buster named that way.
//
// This IS a deliberate departure from the frozen 60s uptime reachability
// probe (internal/uptime), which never busts its cache for good reasons (see
// docs/security/uptime-app-health-design-2026-07-27.md section 2): a
// recurring 60s probe with a buster would mint ~1,440 uncached full renders
// per site per day and would corrupt the perf-timing numbers it also
// collects. NEITHER objection applies here: this Probe fires at most a
// handful of times, once, right after an update is applied, not on a
// recurring schedule, and it collects no timing metrics. Do not remove this
// on the assumption it is the same mistake the uptime probe design rejected.
const cacheBusterParam = "wpmgr_hc"

// addCacheBuster appends a fresh, unpredictable-enough query parameter to
// targetURL so a query-string-keyed cache treats this request as a new
// object. It is not a security token; it only needs to vary per call.
func addCacheBuster(targetURL string) (string, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("invalid probe url: %w", err)
	}
	q := u.Query()
	q.Set(cacheBusterParam, strconv.FormatInt(time.Now().UnixNano(), 36))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// cacheHitHeaders are the response headers various page/edge caches use to
// report that a response was served from cache rather than freshly rendered.
// Checked case-insensitively via http.Header.Get's own canonicalization. This
// list can never be exhaustive - see cacheHitShapeRe below for the fallback
// that catches a vendor not named here.
var cacheHitHeaders = []string{
	"Cf-Cache-Status",   // Cloudflare
	"X-Litespeed-Cache", // LiteSpeed / OpenLiteSpeed built-in page cache
	"X-Kinsta-Cache",    // Kinsta
	"X-Proxy-Cache",     // common nginx/Varnish proxy_cache naming
	"X-Cache",           // Varnish, Fastly, many generic reverse proxies
	"X-Cache-Status",    // the standard nginx add_header X-Cache-Status $upstream_cache_status form
}

// cacheHitShapeRe is the FALLBACK for a cache-status header not named in
// cacheHitHeaders. Appending vendor names to that list forever is a losing
// game - a real-world fleet was found running a stack whose cache-status
// header matched neither list, so DetectCacheHit would have silently missed
// a cache HIT on exactly the setup that motivates this check (GH #291 Phase
// 2). Rather than chase every vendor, this also matches the common SHAPE of a
// cache-status header name: "x-<something>-cache" (X-Swarm-Cache,
// X-Rocket-Cache, ...) or "x-cache-<something>" (X-Cache-Hits, ...),
// case-insensitively. Anchored on the full header name (not a substring
// search) so an unrelated header merely containing "cache" somewhere in a
// longer name never false-positives.
var cacheHitShapeRe = regexp.MustCompile(`(?i)^x-[a-z0-9]+-cache$|^x-cache-[a-z0-9]+$`)

// cacheHitValueRe matches a header value that reads as an affirmative
// cache-status verdict: a bare HIT, plus the handful of near-hit states real
// caches emit that still mean "this response did not come from a fresh
// backend render" (STALE served while revalidating, UPDATING served the old
// object while refreshing, REVALIDATED served after a conditional check).
// MISS/BYPASS/EXPIRED/DYNAMIC are deliberately NOT matched - those mean the
// cache was NOT the source of this response, which is exactly the "prove the
// bypass worked" case this whole mechanism exists for.
var cacheHitValueRe = regexp.MustCompile(`(?i)\b(HIT|STALE|UPDATING|REVALIDATED)\b`)

// detectCacheHit reports whether h carries a cache-status header (or an Age
// value greater than zero) indicating the response was served from cache. A
// cache that ignores query strings when computing its cache key defeats
// addCacheBuster with one click of configuration (see cacheBusterParam), so
// this is the honest backstop that turns a silently-stale 200 into a
// detectable, reportable CacheHit instead of trusted proof of health.
//
// Two layers, checked in order: the explicit cacheHitHeaders list (known
// vendors, cheapest and most precise), then cacheHitShapeRe (a header this
// deployment's cache emits under a name not in that list, but whose shape and
// value still say HIT/STALE/UPDATING/REVALIDATED). The Age > 0 backstop
// applies regardless of either header check.
func DetectCacheHit(h http.Header) (hit bool, detail string) {
	for _, name := range cacheHitHeaders {
		v := h.Get(name)
		if v == "" {
			continue
		}
		if strings.Contains(strings.ToUpper(v), "HIT") {
			return true, fmt.Sprintf("cache hit (%s: %s)", name, v)
		}
	}
	for name, values := range h {
		if !cacheHitShapeRe.MatchString(name) {
			continue
		}
		for _, v := range values {
			if v != "" && cacheHitValueRe.MatchString(v) {
				return true, fmt.Sprintf("cache hit (%s: %s)", name, v)
			}
		}
	}
	if age := strings.TrimSpace(h.Get("Age")); age != "" {
		if n, err := strconv.Atoi(age); err == nil && n > 0 {
			return true, fmt.Sprintf("cache hit (Age: %s)", age)
		}
	}
	return false, ""
}

// Get probes targetURL and classifies the result. A transport error (including
// an SSRF block) is returned as err; a reachable-but-broken site is a non-error
// ProbeResult with Healthy()==false. The URL is cache-busted (see
// cacheBusterParam) and the response headers are checked for a cache-status
// signal (see DetectCacheHit / ProbeResult.CacheHit) before the caller decides
// what the result proves.
func (p *Probe) Get(ctx context.Context, targetURL string) (ProbeResult, error) {
	bustedURL, err := addCacheBuster(targetURL)
	if err != nil {
		return ProbeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bustedURL, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("User-Agent", "WPMgr-HealthProbe/1.0")

	resp, err := p.http.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody)); _ = resp.Body.Close() }()

	cacheHit, cacheDetail := DetectCacheHit(resp.Header)

	// Read a bounded prefix to scan for fatal-error signatures.
	const scanLimit = 64 << 10
	body, _ := io.ReadAll(io.LimitReader(resp.Body, scanLimit))
	lower := strings.ToLower(string(body))
	res := ProbeResult{StatusCode: resp.StatusCode, CacheHit: cacheHit}
	for _, sig := range fatalSignatures {
		if strings.Contains(lower, sig) {
			res.Fatal = true
			res.Detail = "fatal-error signature in response body"
			return res, nil
		}
	}
	if resp.StatusCode >= 500 {
		res.Detail = fmt.Sprintf("server returned status %d", resp.StatusCode)
		return res, nil
	}
	if cacheHit {
		res.Detail = cacheDetail
	}
	return res, nil
}
