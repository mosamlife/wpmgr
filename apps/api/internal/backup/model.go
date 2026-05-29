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
)

// Schedule cadences.
const (
	CadenceDaily   = "daily"
	CadenceWeekly  = "weekly"
	CadenceMonthly = "monthly"
)

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
	Progress            []byte
	ProgressUpdatedAt   *time.Time
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
	NextRunAt          time.Time
	LastRunAt          *time.Time
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

// validCadence reports whether c is a known schedule cadence.
func validCadence(c string) bool {
	switch c {
	case CadenceDaily, CadenceWeekly, CadenceMonthly:
		return true
	default:
		return false
	}
}

// nextRun computes the next run time for a cadence from a base time.
func nextRun(base time.Time, cadence string) time.Time {
	switch cadence {
	case CadenceWeekly:
		return base.AddDate(0, 0, 7)
	case CadenceMonthly:
		return base.AddDate(0, 1, 0)
	default: // daily
		return base.AddDate(0, 0, 1)
	}
}

// chunkS3Key returns the content-addressed, tenant-namespaced object key for a
// ciphertext chunk. Namespacing by tenant ensures a tenant's presigned URL can
// never target another tenant's chunk prefix.
func chunkS3Key(tenantID uuid.UUID, blake3 string) string {
	return "chunks/" + tenantID.String() + "/" + blake3
}
