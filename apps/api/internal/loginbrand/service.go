package loginbrand

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// maxMessageLen is the maximum allowed length (in runes) for the login page
// message field. The agent displays this text below the logo; very long values
// break the layout.
const maxMessageLen = 2000

// AgentLoginBrandClient is the subset of agentcmd.Client the service needs to
// push a login brand config to the agent. *agentcmd.Client satisfies it via
// its SyncLoginBrand method. Declared as an interface so tests can substitute a
// fake without spinning up the SSRF transport.
type AgentLoginBrandClient interface {
	SyncLoginBrand(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.LoginBrandRequest) (agentcmd.LoginBrandResult, error)
}

// SiteLookup resolves a site's agent URL from the site service (wired in main
// via a narrow adapter, keeping this package free of the site import).
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
}

// Service orchestrates the login-brand domain: repo + optional agentcmd client.
type Service struct {
	repo        *Repo
	agentClient AgentLoginBrandClient
	siteLookup  SiteLookup
}

// NewService builds a Service.
func NewService(repo *Repo) *Service {
	return &Service{repo: repo}
}

// SetAgentClient wires the agentcmd client for pushing login brand config to
// the agent. The SiteLookup is required alongside it so the service can resolve
// the site URL without a hard dependency on the site package. Must be called
// before any SaveConfig that should push to the agent.
func (s *Service) SetAgentClient(client AgentLoginBrandClient, sites SiteLookup) {
	s.agentClient = client
	s.siteLookup = sites
}

// GetConfig returns the stored login brand config for (tenantID, siteID). When
// no row exists yet it returns the all-empty default (all fields ""), which the
// agent treats as "no override".
func (s *Service) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (LoginBrand, error) {
	cfg, found, err := s.repo.Get(ctx, tenantID, siteID)
	if err != nil {
		return LoginBrand{}, err
	}
	if !found {
		return LoginBrand{
			TenantID: tenantID,
			SiteID:   siteID,
		}, nil
	}
	return cfg, nil
}

// SaveConfig validates the incoming config, upserts it in the database, and
// pushes it to the agent via the sync_login_brand command. Returns the stored
// config.
//
// Validation rules:
//   - logo_url and logo_link must be either empty or a valid http/https URL
//     (other schemes are rejected).
//   - message must be at most maxMessageLen (2000) runes.
//
// If the agentcmd client is not wired, the upsert still succeeds and the config
// is stored — the push is skipped. If the agent push fails after a successful
// store, the stored config is returned with a wrapped error so the handler can
// surface it as an X-Agent-Push-Warning rather than a hard failure.
func (s *Service) SaveConfig(ctx context.Context, tenantID, siteID uuid.UUID, cfg LoginBrand) (LoginBrand, error) {
	// --- validation ---
	if err := validateURL(cfg.LogoURL, "logo_url"); err != nil {
		return LoginBrand{}, err
	}
	if err := validateURL(cfg.LogoLink, "logo_link"); err != nil {
		return LoginBrand{}, err
	}
	if len([]rune(cfg.Message)) > maxMessageLen {
		return LoginBrand{}, domain.Validation("message_too_long",
			fmt.Sprintf("message must be at most %d characters", maxMessageLen))
	}

	cfg.TenantID = tenantID
	cfg.SiteID = siteID

	// --- persist ---
	saved, err := s.repo.Upsert(ctx, cfg)
	if err != nil {
		return LoginBrand{}, err
	}

	// --- push to agent (best-effort) ---
	if s.agentClient != nil && s.siteLookup != nil {
		siteURL, lookupErr := s.siteLookup.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr == nil {
			if _, pushErr := s.agentClient.SyncLoginBrand(ctx, siteID, siteURL, agentcmd.LoginBrandRequest{
				LogoURL:  saved.LogoURL,
				LogoLink: saved.LogoLink,
				Message:  saved.Message,
			}); pushErr != nil {
				// Non-fatal: the config is already persisted. The agent will pick it
				// up on next sync or when the operator re-saves. Return the stored
				// config + the push error wrapped so the handler can surface it as an
				// X-Agent-Push-Warning.
				return saved, fmt.Errorf("config stored but agent push failed: %w", pushErr)
			}
		}
		// Site URL lookup failure is also non-fatal — config is still stored.
	}

	return saved, nil
}

// validateURL accepts an empty string (no override) or a valid http/https URL.
// Any other scheme is rejected with a validation error.
func validateURL(raw, field string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return domain.Validation("invalid_"+field,
			fmt.Sprintf("%s must be a valid URL: %v", field, err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return domain.Validation("invalid_"+field,
			fmt.Sprintf("%s must be an http or https URL, got scheme %q", field, u.Scheme))
	}
	if u.Host == "" {
		return domain.Validation("invalid_"+field,
			fmt.Sprintf("%s must have a host", field))
	}
	return nil
}
