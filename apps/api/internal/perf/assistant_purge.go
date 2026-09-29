package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// The AI request path's cache clear.
//
// The dashboard Purge records, sends and stamps in one call, each step in its
// own transaction. The AI request path cannot use it: its attempt record, its
// gauge stamp and its reads of the site's CDN settings must each join a
// transaction the caller already holds under a single-site principal, and the
// network send must happen with no transaction open. So the path is split:
//
//   - GetCDNCredentialsCiphertextTx reads the CDN settings on the caller's
//     transaction (the reservation);
//   - SendAssistantPurge sends the clear and touches no database at all;
//   - MarkCachePurgedTx stamps the gauge on the caller's transaction (the
//     outcome transaction);
//   - PublishAssistantPurge emits the SSE pair after that commits.
// ---------------------------------------------------------------------------

// CDNCiphertext is a site's stored CDN settings, still encrypted, as read on
// the caller's transaction. The zero value means none are configured.
type CDNCiphertext struct {
	Ciphertext []byte
	Provider   string
}

// Configured reports whether there is anything to decrypt.
func (c CDNCiphertext) Configured() bool { return len(c.Ciphertext) > 0 && c.Provider != "" }

// AssistantPurge is one approved AI clear. Scope is "all" or "url"; URL is the
// stored, rebuilt page address for "url" and empty for "all".
type AssistantPurge struct {
	Scope string
	URL   string
}

// The closed set of CDN results an AI clear records.
const (
	AssistantCDNNotAttempted  = "not_attempted"
	AssistantCDNCleared       = "cleared"
	AssistantCDNFailed        = "failed"
	AssistantCDNNotConfigured = "not_configured"
)

// AssistantPurgeResult is what the site answered and what happened at the CDN.
type AssistantPurgeResult struct {
	Agent agentcmd.CachePurgeResult
	// WpmgrCDN is one of the AssistantCDN* values.
	WpmgrCDN string
}

// ErrAssistantPurgeNotWired is returned when this service has no agent client.
// Nothing was sent.
var ErrAssistantPurgeNotWired = errors.New("perf: the agent command client is not wired")

// SendAssistantPurge sends one origin-only cache clear to the site and, for a
// page clear, removes that one address from a CDN configured in WPMgr. It opens
// no transaction and reads nothing from the database: everything it needs
// arrives as arguments.
//
// A whole-site clear never touches a CDN. A page clear touches the CDN only
// after the site answered with success, and a CDN error is returned in the
// result as "failed" rather than swallowed.
//
// The agent error is returned unchanged, with whatever reply was decoded, so
// the caller can classify it.
func (s *Service) SendAssistantPurge(ctx context.Context, siteID uuid.UUID, siteURL string, cdn CDNCiphertext, req AssistantPurge) (AssistantPurgeResult, error) {
	if s.agent == nil {
		return AssistantPurgeResult{}, ErrAssistantPurgeNotWired
	}
	if req.Scope != string(PurgeKindAll) && req.Scope != string(PurgeKindURL) {
		return AssistantPurgeResult{}, fmt.Errorf("assistant purge: scope %q is not all or url", req.Scope)
	}
	if req.Scope == string(PurgeKindURL) && req.URL == "" {
		return AssistantPurgeResult{}, errors.New("assistant purge: a page clear needs its url")
	}
	cmd := agentcmd.CachePurgeRequest{Scope: req.Scope, OriginOnly: true}
	if req.Scope == string(PurgeKindURL) {
		cmd.URL = req.URL
		cmd.URLs = []string{req.URL}
	}
	res, err := s.agent.CachePurge(ctx, siteID, siteURL, cmd)
	out := AssistantPurgeResult{Agent: res, WpmgrCDN: AssistantCDNNotAttempted}
	if err != nil {
		return out, err
	}
	if req.Scope == string(PurgeKindURL) {
		out.WpmgrCDN = s.purgeAssistantCDN(ctx, siteURL, cdn, req.URL)
	}
	return out, nil
}

// purgeAssistantCDN removes one address from the site's CDN and says what
// happened. It never decides the clear's outcome.
func (s *Service) purgeAssistantCDN(ctx context.Context, siteURL string, cdn CDNCiphertext, pageURL string) string {
	if !cdn.Configured() || s.cdn == nil || s.decryptor == nil {
		return AssistantCDNNotConfigured
	}
	plain, err := s.decryptor.Decrypt(cdn.Ciphertext)
	if err != nil {
		return AssistantCDNFailed
	}
	var creds CDNCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return AssistantCDNFailed
	}
	if creds.Provider == "" {
		creds.Provider = cdn.Provider
	}
	if err := s.cdn.Purge(ctx, creds, siteURL, []string{pageURL}); err != nil {
		return AssistantCDNFailed
	}
	return AssistantCDNCleared
}

// PublishAssistantPurge emits cache.purge.started and cache.purge.completed
// for an AI clear whose outcome has committed. It touches no database.
func (s *Service) PublishAssistantPurge(ctx context.Context, tenantID, siteID uuid.UUID, scope string) {
	urls := 0
	if scope == string(PurgeKindURL) {
		urls = 1
	}
	s.publish(ctx, tenantID, siteID, site.EventCachePurgeStarted, map[string]any{
		"kind": scope, "urls_count": urls, "delete_everything": false,
	})
	s.publish(ctx, tenantID, siteID, site.EventCachePurgeCompleted, map[string]any{
		"kind": scope, "urls_count": urls,
	})
}

// GetCDNCredentialsCiphertextTx reads a site's stored CDN settings on the
// caller's transaction. It never decrypts. No settings row, or no stored
// credentials, is the zero CDNCiphertext and no error.
func (r *Repo) GetCDNCredentialsCiphertextTx(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID) (CDNCiphertext, error) {
	if tx == nil {
		return CDNCiphertext{}, errors.New("get cdn credentials: no transaction")
	}
	row, err := sqlc.New(tx).GetPerfConfig(ctx, siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CDNCiphertext{}, nil
	}
	if err != nil {
		return CDNCiphertext{}, fmt.Errorf("get cdn credentials: %w", err)
	}
	if row.TenantID != tenantID {
		return CDNCiphertext{}, errors.New("get cdn credentials: settings row belongs to another organisation")
	}
	return CDNCiphertext{Ciphertext: row.CdnCredentialsEncrypted, Provider: derefStr(row.CdnProvider)}, nil
}

// MarkCachePurgedTx stamps the "Last purge" gauge on the caller's transaction,
// so the stamp commits or rolls back with the caller's own writes.
func (r *Repo) MarkCachePurgedTx(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID, kind string) error {
	if tx == nil {
		return errors.New("mark cache purged: no transaction")
	}
	return sqlc.New(tx).MarkCachePurged(ctx, sqlc.MarkCachePurgedParams{
		SiteID:        siteID,
		TenantID:      tenantID,
		LastPurgeKind: strPtr(kind),
	})
}
