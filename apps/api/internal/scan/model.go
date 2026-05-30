// Package scan implements the S3 Malware / File-Integrity Scan domain on the
// control-plane side.
//
// The CP pulls file hashes synchronously from the agent via the signed `scan`
// command (River multi-step driver loop), stages them in scan_run_hashes,
// compares against WordPress.org checksums, writes deduplicated findings to
// scan_findings, and exposes operator REST + a River worker loop.
//
// Design: docs/research/s3-malware-scan-design.md
package scan

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// Run status values.
const (
	StatusQueued   = "queued"
	StatusScanning = "scanning"
	StatusDiffing  = "diffing"
	StatusDone     = "done"
	StatusFailed   = "failed"
)

// Scan kind values.
const (
	KindCore  = "core"
	KindFiles = "files"
	KindFull  = "full"
)

// Finding type values.
const (
	FindingCoreModified        = "core_modified"
	FindingCoreMissing         = "core_missing"
	FindingCoreUnknownInjected = "core_unknown_injected"
)

// Severity values.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
)

// Run is one scan job row from scan_runs.
type Run struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	SiteID        uuid.UUID
	Kind          string
	Status        string
	Cursor        json.RawMessage // agentcmd.ScanCursor as JSON, or nil
	FilesScanned  int64
	WPVersion     string
	Locale        string
	Error         string
	FindingCounts map[string]int
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// HashRow is one row in scan_run_hashes.
type HashRow struct {
	ID       int64
	TenantID uuid.UUID
	RunID    uuid.UUID
	Path     string
	Size     int64
	MD5      string
	Mtime    int64
	IsLink   bool
}

// Finding is one row in scan_findings.
type Finding struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SiteID      uuid.UUID
	RunID       uuid.UUID
	FindingType string
	Path        string
	Severity    string
	ExpectedMD5 string
	ActualMD5   string
	DeduKey     string
	Ignored     bool
	IgnoredBy   string
	CreatedAt   time.Time
	LastSeenRun uuid.UUID
}

// AgentScanClient is the subset of agentcmd.Client the scan service/worker
// needs to issue scan and get_file commands. *agentcmd.Client satisfies it
// via its Scan and GetFile methods. Declared as an interface so tests can
// substitute a fake without spinning up the SSRF transport.
type AgentScanClient interface {
	Scan(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.ScanRequest) (agentcmd.ScanResponse, error)
	GetFile(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error)
}

// SiteLookup resolves site info needed by the scan worker/service.
// Wired in main via a narrow adapter, keeping this package free of the site import.
type SiteLookup interface {
	GetScanSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (ScanSiteInfo, error)
}

// ScanSiteInfo is the slim site projection the scan worker needs.
type ScanSiteInfo struct {
	URL       string
	WPVersion string
	Enrolled  bool
}
