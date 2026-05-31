// Package sitedestination is the per-site backup-destination domain
// (ADR-036 P1). A SiteDestination tells the control plane WHERE a site's
// backup chunks should land:
//
//   - Kind=cp        — WPMgr-managed bucket (default; legacy behaviour).
//   - Kind=local     — agent writes chunks straight to wp-content/wpmgr-backups
//     on the same webserver. The CP only sees the manifest.
//   - Kind=s3_compat — customer-owned S3 bucket (AWS S3, Wasabi, B2, DO Spaces,
//     MinIO, …). CP holds the AWS credentials age-encrypted at
//     rest and mints presigned PUT URLs against the bucket on
//     the agent's behalf, so the agent never sees the keys.
//
// The Snapshot row carries a `destination_id` FK back to this table so the
// presign service can route to the right Store (see blobstore.Registry).
package sitedestination

import (
	"time"

	"github.com/google/uuid"
)

// Kind is the destination backend type.
type Kind string

const (
	// KindCP is the legacy CP-managed bucket — the historical default.
	KindCP Kind = "cp"
	// KindLocal writes ciphertext to wp-content/wpmgr-backups on the agent host.
	KindLocal Kind = "local"
	// KindS3Compat is a customer-owned S3-compatible bucket.
	KindS3Compat Kind = "s3_compat"
)

// SiteDestination is one configured destination for a site (or, when SiteID is
// nil, a tenant-default that fans out to every site lacking an override). V1
// is strictly per-site — the SiteID column is nullable for future expansion.
type SiteDestination struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	// SiteID is the site this destination is bound to. Nullable in the schema
	// but always non-nil in V1.
	SiteID uuid.UUID

	Kind  Kind
	Label string // operator-facing nickname; not used in routing.

	// S3-compat fields — empty for cp / local.
	Endpoint       string
	Region         string
	Bucket         string
	PathPrefix     string
	AccessKeyID    string
	SecretKeyEnc   []byte // age-encrypted (recipient = CP key); never exposed.
	ForcePathStyle bool

	// IsDefault: exactly one destination per (tenant_id, site_id) may carry
	// IsDefault=true. Enforced by a partial unique index. The presign service
	// picks the default when a snapshot's destination_id is unset.
	IsDefault bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidKind reports whether k is one of the allowed kinds.
func ValidKind(k Kind) bool {
	switch k {
	case KindCP, KindLocal, KindS3Compat:
		return true
	default:
		return false
	}
}

// PresignableConfig is the subset of fields a blobstore.Store needs to mint
// presigned PUT/GET URLs for an S3-compatible destination. Local destinations
// don't go through a Store at all (the agent writes the bytes itself); CP
// destinations use the global Store wired in main.
type PresignableConfig struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKeyID    string
	SecretKeyPlain string // decrypted at use-site; never persisted in clear.
	ForcePathStyle bool
	PathPrefix     string
}
