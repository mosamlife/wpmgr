// Package backup implements the M4 incremental backup & restore feature.
//
// Files (and a `wp db export` stream) are split into ~4 MiB chunks. Encryption
// is CLIENT-SIDE on the agent: each chunk is age-encrypted to the site's age
// PUBLIC recipient, then content-addressed by the BLAKE3 hash of its CIPHERTEXT
// at s3 key chunks/<tenant>/<blake3>. A chunk is uploaded only if its hash is
// not already stored for the tenant (incremental dedup via backup_chunks with a
// refcount). The control plane and S3 store ONLY ciphertext and NEVER a
// decryption key — the CP cannot decrypt backups by default (see ADR / trust
// model in agentcmd/backup_contract.go).
//
// Upload uses presigned S3 PUT URLs the CP mints for the not-yet-stored chunk
// hashes; the agent uploads ciphertext directly to S3. Restore mints presigned
// GET URLs + the ordered manifest; the agent downloads, decrypts (its own age
// identity), verifies BLAKE3, and reassembles.
//
// Every query is tenant-scoped both explicitly (tenant_id) and by Postgres RLS.
package backup

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Snapshot kinds.
const (
	KindFiles = "files"
	KindDB    = "db"
	KindFull  = "full"
)

// Snapshot statuses.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Manifest entry kinds.
//
// Track 5 (0.9.6) split EntryKindFile — the legacy lumped wp-content artifact
// kind — into four mutually exclusive component buckets so an operator can
// restore plugins / themes / uploads / wp-content-others independently. The
// agent's FilesArchiver emits one rotating part sequence per component:
//
//	EntryKindPlugin    -> plugins.partNNN.zip   (wp-content/plugins/*)
//	EntryKindTheme     -> themes.partNNN.zip    (wp-content/themes/*)
//	EntryKindUpload    -> uploads.partNNN.zip   (wp-content/uploads/*)
//	EntryKindWPContent -> wp-content.partNNN.zip (everything else: mu-plugins,
//	                                              languages, drop-ins, custom dirs)
//	EntryKindCore      -> core.partNNN.zip      (ABSPATH: wp-admin, wp-includes,
//	                                              root PHP files incl. wp-config.php)
//
// EntryKindFile is RETAINED for backward compat: pre-Track-5 snapshots have
// every files-part tagged 'file', and the restorer routes those through the
// whole-wp-content swap path. New snapshots never emit 'file'.
//
// EntryKindInspection lives in sqlinspect_handler.go (kept local to the
// inspection feature on purpose). It is NOT redeclared here.
const (
	EntryKindDB        = "db"
	EntryKindFile      = "file" // legacy — pre-Track-5 lumped files
	EntryKindPlugin    = "plugin"
	EntryKindTheme     = "theme"
	EntryKindUpload    = "upload"
	EntryKindWPContent = "wp-content" // catch-all: NOT plugin/theme/upload
	// EntryKindCore is the A2 WordPress-core archive component: wp-admin,
	// wp-includes, and root PHP files (wp-config.php, wp-login.php, etc.)
	// rooted at ABSPATH, emitted only when include_core=true.
	EntryKindCore = "core"
	// ADR-051 archive-delta increments emit two extra manifest-entry kinds:
	//   files-list:  ONE entry per snapshot, the relpath\tsize\tmtime list that
	//                seeds the NEXT increment's diff (chunked like a part).
	//   tombstones:  ONE entry per path whose deleted state changed since the
	//                parent (path=<relpath>, chunk_hashes empty, mode carries the
	//                delete/re-add delta). The restore overlay reads these via
	//                ListManifest with no CP-side chunk fetch.
	EntryKindFilesList  = "files-list"
	EntryKindTombstones = "tombstones"
)

// Schedule cadences.
const (
	CadenceHourly      = "hourly"
	CadenceEveryNHours = "every_n_hours"
	CadenceDaily       = "daily"
	CadenceWeekly      = "weekly"
	CadenceMonthly     = "monthly"
)

// BackupMaxChainDepth is the maximum number of incremental generations before
// the CP forces a new full base. Configurable via env BACKUP_MAX_CHAIN_DEPTH.
const BackupMaxChainDepth = 6

// BackupBaseWindowDays is the number of days after which a chain is considered
// stale and the next run falls back to a full base.
const BackupBaseWindowDays = 7

// Snapshot is one backup of a site.
type Snapshot struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	CreatedBy    *uuid.UUID
	Kind         string
	Status       string
	AgeRecipient string
	TotalSize    int64
	ChunkCount   int64
	Error        string
	Archived     bool
	StartedAt    *time.Time
	FinishedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Progress is the M5.6 phpbu runner's latest phase payload (raw JSONB).
	// Shape: {"phase": "...", "phase_detail": {...}}. Empty {} until the first
	// runner POST lands. The UI renders this; the watchdog scans
	// ProgressUpdatedAt to detect stalled runs.
	Progress          []byte
	ProgressUpdatedAt *time.Time
	// StalledAt (m104 / GH #279) is set by the watchdog when a running
	// snapshot has gone quiet past the soft threshold but is not yet
	// hard-failed. Nil means healthy. Cleared by the next proof of life (a
	// presign, manifest submit, or progress POST).
	StalledAt *time.Time
	// P0 URL rewriter (ADR-036): siteurl / home / content / upload recorded
	// at backup time. Drives the restore's URL_REWRITE phase when restoring
	// to a different environment (dev->prod, staging->prod). Empty strings
	// for pre-ADR-036 snapshots — the agent then defensively reads the URLs
	// out of the dump's banner comments instead.
	SourceSiteURL    string
	SourceHomeURL    string
	SourceContentURL string
	SourceUploadURL  string
	// P1 storage adapter (ADR-036): which per-site destination this snapshot's
	// chunks belong to. uuid.Nil routes to the legacy CP-global bucket and is
	// the value used by every pre-P1 snapshot row.
	DestinationID uuid.UUID
	// ADR-048 incremental backup fields. All zero/nil/false for pre-m44 rows
	// and for full-base snapshots.
	IsIncremental      bool
	ParentSnapshotID   *uuid.UUID
	BaseSnapshotID     *uuid.UUID
	ChainID            *uuid.UUID
	Generation         int
	CycleFilesScanned  int64
	CycleFilesChanged  int64
	CycleFilesDeleted  int64
	CycleBytesUploaded int64
	// Track C (m49): operator-set lock preventing the retention GC from pruning
	// this snapshot. A locked snapshot is never auto-deleted regardless of
	// retention_days or keep_last; it must be explicitly unlocked first.
	Locked bool
}

// ---------------------------------------------------------------------------
// Fleet backup models
// ---------------------------------------------------------------------------

// FleetBackupHealthStatus is the server-derived protection status for one site.
type FleetBackupHealthStatus string

const (
	// HealthStatusUnprotected means no completed backup has ever been recorded for the site.
	HealthStatusUnprotected FleetBackupHealthStatus = "unprotected"
	// HealthStatusFailed means the most recent completed event was a failure
	// (last_failed_at > last_completed_at, or only failures).
	HealthStatusFailed FleetBackupHealthStatus = "failed"
	// HealthStatusStale means a completed backup exists but it is older than
	// 2× the schedule cadence (or >48h if no schedule is set).
	HealthStatusStale FleetBackupHealthStatus = "stale"
	// HealthStatusInFlight means at least one pending/running backup exists and
	// there is no recent completed backup to anchor protection status.
	HealthStatusInFlight FleetBackupHealthStatus = "in_flight"
	// HealthStatusProtected means a recent completed backup exists and no failure
	// has occurred since.
	HealthStatusProtected FleetBackupHealthStatus = "protected"
)

// FleetBackupHealthItem is the health summary for one site in the fleet health response.
type FleetBackupHealthItem struct {
	SiteID          uuid.UUID               `json:"site_id"`
	SiteName        string                  `json:"site_name"`
	SiteURL         string                  `json:"site_url"`
	Status          FleetBackupHealthStatus `json:"status"`
	LastCompletedAt *time.Time              `json:"last_completed_at,omitempty"`
	LastFailedAt    *time.Time              `json:"last_failed_at,omitempty"`
	LatestSizeBytes int64                   `json:"latest_size_bytes"`
	InFlightCount   int64                   `json:"in_flight_count"`
	ScheduleCadence *string                 `json:"schedule_cadence,omitempty"`
	NextRunAt       *time.Time              `json:"next_run_at,omitempty"`
}

// FleetListFilter holds the parsed fleet snapshot list filters (passed from handler
// to service to avoid re-parsing at the service layer).
type FleetListFilter struct {
	SiteIDs       []uuid.UUID // nil = all sites the principal can access
	Status        string      // "" = no filter
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Limit         int32
	Offset        int32
}

// FleetSnapshotPage is the paginated result of a fleet snapshot list query.
type FleetSnapshotPage struct {
	Items      []Snapshot
	NextOffset *int64 // nil when there are no more pages
}

// FileIndexEntry is one row of the backup_file_index table: a single file's
// content fingerprint for a completed snapshot. Used by the NDJSON streaming
// endpoint and by SubmitIncrementalManifest inserts.
type FileIndexEntry struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SnapshotID  uuid.UUID
	FilePath    string
	FileSize    int64
	FileMtime   int64
	FileBlake3  string
	ChunkHashes []string
	IsTombstone bool
	CreatedAt   time.Time
}

// ManifestEntry is one file/db entry of a snapshot: an ordered list of
// ciphertext-chunk BLAKE3 hashes that reassemble the path.
type ManifestEntry struct {
	ID          uuid.UUID
	SnapshotID  uuid.UUID
	TenantID    uuid.UUID
	Path        string
	EntryKind   string
	TableName   string
	ChunkHashes []string
	Size        int64
	Mode        int32
	CreatedAt   time.Time
}

// Chunk is a content-addressed ciphertext chunk in object storage with a
// refcount for incremental dedup + GC.
type Chunk struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Blake3    string
	S3Key     string
	Size      int64
	Refcount  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Schedule is a per-site backup schedule.
type Schedule struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	Cadence            string
	Kind               string
	Enabled            bool
	RetentionDays      int32
	MonthlyArchiveKeep int32
	RunHour            int32
	RunMinute          int32
	DayOfWeek          *int32
	DayOfMonth         *int32
	FrequencyHours     *int32
	Timezone           string  // resolved IANA name (or fixed-offset label) for DTO display
	GmtOffset          float64 // GMT offset hours from the site's wp_gmt_offset (for DTO display)
	KeepLast           int32
	// ADR-048 P5: when true, scheduled and run-now backups for this site consult
	// the auto-base chain rule and may take incremental snapshots.
	IncrementalEnabled bool
	// Optional per-schedule override of BackupBaseWindowDays. nil = use the
	// constant.
	BaseWindowDays *int32

	NextRunAt time.Time
	LastRunAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SiteBackupSettings holds the Track-A (backup scope/exclusions) and Track-B
// (notification settings) per-site backup settings, decoupled from
// backup_schedules in m50. A missing row means safe defaults apply:
// all components, no exclusions, never notify.
type SiteBackupSettings struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	// Track A: backup scope / exclusions.
	// BackupComponents is the set of components to archive. nil = all components
	// (full backup). Valid values: plugin|theme|upload|wp-content|db|core.
	BackupComponents  []string
	IncludeCore       bool
	ExcludePaths      []string
	ExcludeExtensions []string
	// ExcludeFileSizeMB, when > 0, skips files strictly larger than this (MiB).
	// 0 = no size filter (maps to DB NULL via CHECK (exclude_file_size_mb > 0)).
	ExcludeFileSizeMB int32
	// Track B: notification settings.
	NotifyOnCompletion string   // "always"|"on_failure"|"never"
	NotifyRecipients   []string // email addresses
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// validKind reports whether kind is a known snapshot kind.
func validKind(kind string) bool {
	switch kind {
	case KindFiles, KindDB, KindFull:
		return true
	default:
		return false
	}
}

// validBackupComponent reports whether c is a valid backup component value.
// Uses the SINGULAR canonical vocabulary matching manifest entry_kind:
//
//	plugin | theme | upload | wp-content | db | core
func validBackupComponent(c string) bool {
	switch c {
	case EntryKindPlugin, EntryKindTheme, EntryKindUpload, EntryKindWPContent, EntryKindDB, EntryKindCore:
		return true
	default:
		return false
	}
}

// isValidEmail performs a lightweight structural check: non-empty, exactly one
// "@" with non-empty local and domain parts, domain contains at least one ".".
// This is intentionally conservative — rejecting obviously malformed addresses
// while accepting any plausible email without requiring a full RFC 5322 parse.
func isValidEmail(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at >= len(addr)-1 {
		return false
	}
	local := addr[:at]
	domain := addr[at+1:]
	if local == "" || domain == "" {
		return false
	}
	// Domain must contain at least one dot with non-empty parts on both sides.
	dot := strings.LastIndex(domain, ".")
	if dot <= 0 || dot >= len(domain)-1 {
		return false
	}
	return true
}

// validCadence reports whether c is a known schedule cadence.
func validCadence(c string) bool {
	switch c {
	case CadenceHourly, CadenceEveryNHours, CadenceDaily, CadenceWeekly, CadenceMonthly:
		return true
	default:
		return false
	}
}

// nextRun computes the next run time for a cadence from a base time.
// Deprecated: use nextOccurrence instead. Kept for backward-compatibility with
// code paths not yet migrated (service.go PutSchedule / EnqueueScheduledBackup).
func nextRun(base time.Time, cadence string) time.Time {
	switch cadence {
	case CadenceWeekly:
		return base.AddDate(0, 0, 7)
	case CadenceMonthly:
		return base.AddDate(0, 1, 0)
	default: // daily (and unrecognised cadences)
		return base.AddDate(0, 0, 1)
	}
}

// resolveLocation converts a WordPress timezone string (IANA) or a numeric GMT
// offset (hours, possibly fractional e.g. 5.5 for +05:30) into a *time.Location.
//
// Resolution order:
//  1. IANA name via time.LoadLocation (DST-aware, preferred).
//  2. Fixed zone from gmtOffset hours (handles half-hour offsets like +05:30).
//  3. time.UTC as final fallback.
func resolveLocation(wpTimezone string, gmtOffset float64) *time.Location {
	if wpTimezone != "" {
		if loc, err := time.LoadLocation(wpTimezone); err == nil {
			return loc
		}
	}
	// Fixed zone: convert fractional hours to whole seconds. Format the label
	// with the half-hour component so +05:30 sites (e.g. India) are labeled
	// "UTC+05:30" rather than the truncated "UTC+5".
	offsetSec := int(gmtOffset * 3600)
	if offsetSec != 0 {
		sign, abs := "+", offsetSec
		if abs < 0 {
			sign, abs = "-", -abs
		}
		name := fmt.Sprintf("UTC%s%02d:%02d", sign, abs/3600, (abs%3600)/60)
		return time.FixedZone(name, offsetSec)
	}
	return time.UTC
}

// validateTimezone returns an error when the IANA timezone name cannot be
// loaded. An empty name is accepted (falls back to gmtOffset / UTC).
func validateTimezone(name string) error {
	if name == "" {
		return nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("unknown timezone %q: %w", name, err)
	}
	return nil
}

// validateSchedule enforces cadence–field consistency. dow must be set iff
// weekly; dom must be set iff monthly; freqHours must be set iff every_n_hours.
func validateSchedule(cadence string, dow, dom, freqHours *int32) error {
	switch cadence {
	case CadenceWeekly:
		if dow == nil {
			return fmt.Errorf("day_of_week is required for weekly cadence")
		}
		if *dow < 0 || *dow > 6 {
			return fmt.Errorf("day_of_week must be between 0 (Sun) and 6 (Sat)")
		}
	case CadenceMonthly:
		if dom == nil {
			return fmt.Errorf("day_of_month is required for monthly cadence")
		}
		if *dom < 1 || *dom > 28 {
			return fmt.Errorf("day_of_month must be between 1 and 28")
		}
	case CadenceEveryNHours:
		if freqHours == nil {
			return fmt.Errorf("frequency_hours is required for every_n_hours cadence")
		}
		if *freqHours < 1 || *freqHours > 24 {
			return fmt.Errorf("frequency_hours must be between 1 and 24")
		}
	}
	return nil
}

// nextOccurrence computes the next scheduled occurrence after now for the given
// cadence. All computation is performed in loc; the return value is UTC,
// truncated to the minute. jitterMinutes (0–15, deterministic per site) is
// added as a sub-minute offset so that sites with the same config do not all
// fire at the exact same second.
//
// Callers derive jitterMinutes from the site_id hash:
//
//	jitter := int(fnvHash(siteID) % 16)
//
// Callers pass now from the clock — this function never reads the wall clock.
func nextOccurrence(now time.Time, cadence string, hour, minute int, dow, dom, freqHours *int, jitterMinutes int, loc *time.Location) time.Time {
	// Clamp jitter to [0, 15].
	if jitterMinutes < 0 {
		jitterMinutes = 0
	}
	if jitterMinutes > 15 {
		jitterMinutes = 15
	}

	switch cadence {
	case CadenceHourly:
		return nextHourly(now, minute, jitterMinutes, loc)
	case CadenceEveryNHours:
		fh := 1
		if freqHours != nil {
			fh = *freqHours
		}
		return nextEveryNHours(now, hour, minute, fh, jitterMinutes, loc)
	case CadenceWeekly:
		wd := 0
		if dow != nil {
			wd = *dow
		}
		return nextWeekly(now, hour, minute, wd, jitterMinutes, loc)
	case CadenceMonthly:
		d := 1
		if dom != nil {
			d = *dom
		}
		return nextMonthly(now, hour, minute, d, jitterMinutes, loc)
	default: // CadenceDaily and any unrecognised value
		return nextDaily(now, hour, minute, jitterMinutes, loc)
	}
}

// SiteJitter computes a deterministic per-site jitter in [0, 15] minutes from
// the site UUID. The result is stable: same site_id always yields the same
// offset. Exported so the scheduler can use it without re-implementing the hash.
func SiteJitter(siteID uuid.UUID) int {
	h := fnv.New32a()
	_, _ = h.Write(siteID[:])
	return int(h.Sum32() % 16)
}

// nextDaily returns the next daily occurrence strictly after now.
func nextDaily(now time.Time, hour, minute, jitterMinutes int, loc *time.Location) time.Time {
	nowLoc := now.In(loc)
	candidate := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), hour, minute+jitterMinutes, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate.UTC().Truncate(time.Minute)
}

// nextWeekly returns the next occurrence whose weekday matches wd (0=Sun..6=Sat).
func nextWeekly(now time.Time, hour, minute, wd, jitterMinutes int, loc *time.Location) time.Time {
	nowLoc := now.In(loc)
	candidate := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), hour, minute+jitterMinutes, 0, 0, loc)
	// Advance until we land on the right weekday.
	target := time.Weekday(wd)
	for candidate.Weekday() != target || !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate.UTC().Truncate(time.Minute)
}

// nextMonthly returns the next occurrence on day d (capped at 28) of the month.
func nextMonthly(now time.Time, hour, minute, d, jitterMinutes int, loc *time.Location) time.Time {
	if d < 1 {
		d = 1
	}
	if d > 28 {
		d = 28
	}
	nowLoc := now.In(loc)
	candidate := time.Date(nowLoc.Year(), nowLoc.Month(), d, hour, minute+jitterMinutes, 0, 0, loc)
	if !candidate.After(now) {
		candidate = time.Date(nowLoc.Year(), nowLoc.Month()+1, d, hour, minute+jitterMinutes, 0, 0, loc)
	}
	return candidate.UTC().Truncate(time.Minute)
}

// nextHourly returns the next occurrence at :minute past the hour strictly
// after now.
func nextHourly(now time.Time, minute, jitterMinutes int, loc *time.Location) time.Time {
	nowLoc := now.In(loc)
	candidate := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), nowLoc.Hour(), minute+jitterMinutes, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.Add(time.Hour)
	}
	return candidate.UTC().Truncate(time.Minute)
}

// nextEveryNHours returns the next slot in the daily sequence anchored at
// hour:minute, stepping by freqHours, strictly after now.
func nextEveryNHours(now time.Time, hour, minute, freqHours, jitterMinutes int, loc *time.Location) time.Time {
	if freqHours <= 0 {
		freqHours = 1
	}
	nowLoc := now.In(loc)
	// Anchor: hour:minute on the current day in loc.
	anchor := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), hour, minute+jitterMinutes, 0, 0, loc)
	// Walk forward by freqHours steps starting from anchor until we are strictly
	// past now. If anchor is already past now it is our candidate.
	candidate := anchor
	for !candidate.After(now) {
		candidate = candidate.Add(time.Duration(freqHours) * time.Hour)
	}
	return candidate.UTC().Truncate(time.Minute)
}

// chunkS3Key returns the content-addressed, tenant-namespaced object key for a
// ciphertext chunk. Namespacing by tenant ensures a tenant's presigned URL can
// never target another tenant's chunk prefix.
func chunkS3Key(tenantID uuid.UUID, blake3 string) string {
	return "chunks/" + tenantID.String() + "/" + blake3
}
