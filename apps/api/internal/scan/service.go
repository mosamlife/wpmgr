package scan

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service orchestrates the scan domain: repo + River enqueuer + agentcmd client.
type Service struct {
	repo       *Repo
	enqueuer   Reenqueuer
	cmd        AgentScanClient
	siteLookup SiteLookup
	audit      *audit.Recorder
}

// NewService builds a Service.
func NewService(repo *Repo, rec *audit.Recorder) *Service {
	return &Service{repo: repo, audit: rec}
}

// SetEnqueuer wires the River enqueuer after River has started.
func (s *Service) SetEnqueuer(e Reenqueuer) { s.enqueuer = e }

// SetAgentClient wires the agentcmd client for FetchFile.
func (s *Service) SetAgentClient(cmd AgentScanClient, sites SiteLookup) {
	s.cmd = cmd
	s.siteLookup = sites
}

// StartRun inserts a queued scan_run row and enqueues a River job.
// Returns the new run so the handler can return 202 {run_id, status}.
func (s *Service) StartRun(ctx context.Context, tenantID, siteID uuid.UUID, kind string) (Run, error) {
	switch kind {
	case KindCore, KindFiles, KindFull:
		// valid
	case "":
		kind = KindCore
	default:
		return Run{}, domain.Validation("invalid_scan_kind",
			fmt.Sprintf("kind must be 'core', 'files', or 'full'; got %q", kind))
	}

	run, err := s.repo.InsertRun(ctx, tenantID, siteID, kind)
	if err != nil {
		return Run{}, err
	}

	if s.enqueuer == nil {
		return Run{}, domain.ServiceUnavailable("scan_enqueuer_unwired", "scan enqueuer is not wired")
	}
	if err := s.enqueuer.EnqueueScanRun(ctx, ScanRunArgs{
		TenantID: tenantID,
		SiteID:   siteID,
		RunID:    run.ID,
	}); err != nil {
		// Purge the orphan run rather than leaving it stuck queued.
		_, _ = s.repo.MarkFailed(ctx, tenantID, run.ID, "enqueue failed: "+err.Error())
		return Run{}, domain.Internal("scan_enqueue_failed", "failed to enqueue scan job").WithCause(err)
	}

	if s.audit != nil {
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorSystem,
			Action:     "scan.created",
			TargetType: "site",
			TargetID:   siteID.String(),
			Metadata: map[string]any{
				"run_id": run.ID.String(),
				"kind":   kind,
			},
		})
	}

	return run, nil
}

// GetRun returns a single scan run.
func (s *Service) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error) {
	return s.repo.GetRun(ctx, tenantID, runID)
}

// ListRuns returns scan runs for a site, ordered by created_at DESC.
func (s *Service) ListRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]Run, error) {
	return s.repo.ListRuns(ctx, tenantID, siteID, limit)
}

// ListFindings returns findings for a site. ignoredFilter: nil=all.
func (s *Service) ListFindings(ctx context.Context, tenantID, siteID uuid.UUID, limit int, ignoredFilter *bool) ([]Finding, error) {
	return s.repo.ListFindings(ctx, tenantID, siteID, limit, ignoredFilter)
}

// ListFindingsForRun returns findings for a specific run.
func (s *Service) ListFindingsForRun(ctx context.Context, tenantID, siteID, runID uuid.UUID, limit int) ([]Finding, error) {
	return s.repo.ListFindingsForRun(ctx, tenantID, siteID, runID, limit)
}

// IgnoreFinding sets the ignored flag on a finding and records an audit event.
// ignoredBy is the actor's ID (email or API key ID) for the audit trail.
func (s *Service) IgnoreFinding(ctx context.Context, tenantID, findingID uuid.UUID, ignored bool, ignoredBy string, p domain.Principal) (Finding, error) {
	// This route has no :siteId for RequireSiteAccess to guard, so resolve the
	// finding's site and gate on the caller's site access BEFORE mutating —
	// otherwise a site-scoped collaborator could ignore findings on any site.
	existing, err := s.repo.GetFinding(ctx, tenantID, findingID)
	if err != nil {
		return Finding{}, err
	}
	if !p.CanAccessSite(existing.SiteID) {
		return Finding{}, domain.Forbidden("forbidden", "you do not have access to this site")
	}
	f, err := s.repo.SetFindingIgnored(ctx, tenantID, findingID, ignored, ignoredBy)
	if err != nil {
		return Finding{}, err
	}

	if s.audit != nil {
		actType := audit.ActorUser
		if p.Type == domain.PrincipalAPIKey {
			actType = audit.ActorAPIKey
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  actType,
			ActorID:    p.ActorID(),
			Action:     "scan_finding.ignore",
			TargetType: "scan_finding",
			TargetID:   findingID.String(),
			Metadata: map[string]any{
				"ignored":      ignored,
				"finding_type": f.FindingType,
				"path":         f.Path,
				"site_id":      f.SiteID.String(),
			},
		})
	}
	return f, nil
}

// FetchFile calls the agent's get_file command for a path that is already a
// stored finding (server-side guard). Returns the GetFileResponse base64 content.
// Access is audited with scan.file_fetched.
func (s *Service) FetchFile(ctx context.Context, tenantID, findingID uuid.UUID, p domain.Principal) (agentcmd.GetFileResponse, error) {
	if s.cmd == nil || s.siteLookup == nil {
		return agentcmd.GetFileResponse{}, domain.ServiceUnavailable("scan_agent_unwired", "scan agent client is not wired")
	}

	// Server-side guard: the path must already be a stored finding.
	finding, err := s.repo.GetFinding(ctx, tenantID, findingID)
	if err != nil {
		return agentcmd.GetFileResponse{}, err
	}
	// The finding is resolved by id; gate on the caller's site access so a
	// site-scoped collaborator cannot exfiltrate file contents from a finding
	// on a site outside their allowlist (the :siteId in the path is not bound
	// to the finding's actual site).
	if !p.CanAccessSite(finding.SiteID) {
		return agentcmd.GetFileResponse{}, domain.Forbidden("forbidden", "you do not have access to this site")
	}

	siteInfo, err := s.siteLookup.GetScanSiteInfo(ctx, tenantID, finding.SiteID)
	if err != nil {
		return agentcmd.GetFileResponse{}, err
	}
	if !siteInfo.Enrolled {
		return agentcmd.GetFileResponse{}, domain.ServiceUnavailable("site_not_enrolled", "site is not enrolled")
	}

	resp, err := s.cmd.GetFile(ctx, finding.SiteID, siteInfo.URL, agentcmd.GetFileRequest{
		Path:     finding.Path,
		MaxBytes: 262144,
	})
	if err != nil {
		return agentcmd.GetFileResponse{}, domain.Internal("scan_get_file_failed", "get_file command failed").WithCause(err)
	}

	if s.audit != nil {
		actType := audit.ActorUser
		if p.Type == domain.PrincipalAPIKey {
			actType = audit.ActorAPIKey
		}
		_, _ = s.audit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  actType,
			ActorID:    p.ActorID(),
			Action:     ActionFileFetched,
			TargetType: "scan_finding",
			TargetID:   findingID.String(),
			Metadata: map[string]any{
				"path":    finding.Path,
				"site_id": finding.SiteID.String(),
				"ok":      resp.OK,
			},
		})
	}

	return resp, nil
}
