// Package media implements the Media Optimizer control-plane domain (ADR-043):
// the CP-side orchestration of JPEG/PNG → WebP/AVIF optimization that runs the
// actual encode in a SEPARATE optional media-encoder service. This file holds
// the exported errors, the object-key helpers, and the wiring constants shared
// by the repo/service/worker/handler subpackages.
//
// CGO isolation (load-bearing): NOTHING in this package or its subpackages
// (except internal/media/encoder, imported only by cmd/media-encoder) may import
// lilliput. The River job type EncodeArgs lives in internal/media/model so the
// main API can enqueue without the encoder.
package media

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Domain-level sentinel/typed errors. Service-layer typed errors (domain.Error)
// map to HTTP in the handler via httpx.Error; these sentinels classify internal
// orchestration failures.
var (
	// ErrJobNotFound is returned when a media job id does not resolve in tenant scope.
	ErrJobNotFound = errors.New("media: job not found")
	// ErrAssetNotFound is returned when an asset id does not resolve in tenant scope.
	ErrAssetNotFound = errors.New("media: asset not found")
	// ErrJobNotInFlight is returned when a callback targets a terminal/absent job.
	ErrJobNotInFlight = errors.New("media: job is not in flight")
)

// Target format values (the requested output format).
const (
	TargetAVIF     = "avif"
	TargetWebP     = "webp"
	TargetOriginal = "original"
)

// Target quality values.
const (
	QualityLossy    = "lossy"
	QualityLossless = "lossless"
)

// MaxVariantsPerJob caps the variants in one optimize job (ADR-043 §3).
const MaxVariantsPerJob = 10

// MaxSyncBatch caps one agent sync-batch page (ADR-043 — bulk pages of ≤200).
const MaxSyncBatch = 200

// ValidTargetFormat reports whether f is an accepted target format.
func ValidTargetFormat(f string) bool {
	switch f {
	case TargetAVIF, TargetWebP, TargetOriginal:
		return true
	}
	return false
}

// ValidTargetQuality reports whether q is an accepted target quality. Empty
// defaults to lossy at the service layer, so "" is accepted here.
func ValidTargetQuality(q string) bool {
	switch q {
	case "", QualityLossy, QualityLossless:
		return true
	}
	return false
}

// ValidVariantName reports whether v is a safe WP image-size token to embed in an
// object key. Rejects empty, >64 chars, and anything outside [A-Za-z0-9_-] —
// notably '.' and '/' which a hostile agent could use to escape the job prefix
// (storage-key path traversal). WP registered size names are a small token set,
// so a strict allowlist is safe; callers MUST validate before SrcKey/OutKey.
func ValidVariantName(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Object-key helpers — media/<tenant>/<site>/<job>/src|out/<variant>
//
// Tenant+site+job-prefixing means a presigned URL can never target another
// tenant's (or job's) media prefix. Media outputs are NOT content-addressed
// (not dedup-able across sites), unlike backup chunks. All temp objects under
// JobPrefix are deleted when the job ends (success, failure, or cancel).
// ---------------------------------------------------------------------------

// JobPrefix is the per-job temp-object namespace.
func JobPrefix(tenantID, siteID uuid.UUID, jobID string) string {
	return fmt.Sprintf("media/%s/%s/%s", tenantID, siteID, jobID)
}

// SrcKey is the storage key of a job's source variant (agent presigned-PUTs here;
// the encoder presigned-GETs it).
func SrcKey(tenantID, siteID uuid.UUID, jobID, variant string) string {
	return JobPrefix(tenantID, siteID, jobID) + "/src/" + variant
}

// OutKey is the storage key of a job's optimized output variant (the encoder
// presigned-PUTs here; the agent presigned-GETs it to apply on disk).
func OutKey(tenantID, siteID uuid.UUID, jobID, variant string) string {
	return JobPrefix(tenantID, siteID, jobID) + "/out/" + variant
}
