package model

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// MediaEncodeQueue is the dedicated, bounded River queue the media-encoder
// process subscribes to. The main API registers it with MaxWorkers=0
// (insert-only); the separate cmd/media-encoder binary runs the workers.
const MediaEncodeQueue = "media_encode"

// EncodeVariant is one variant (registered WP size) the encoder must produce
// for an attachment. The agent presigned-PUT the source bytes to
// media/<tenant>/<site>/<job>/src/<name>; the worker presigned-GETs it,
// encodes, and presigned-PUTs the result to media/<tenant>/<site>/<job>/out/<name>.
//
// NOTE: SourceMime is the agent's CLAIMED mime — it is informational only.
// The encoder detects the real source format from the magic bytes (ADR-043 §4).
type EncodeVariant struct {
	Name       string `json:"name"`        // 'full'|'thumbnail'|'medium'|...
	SourceSize int64  `json:"source_size"` // source byte length (bound check / progress)
	SourceMime string `json:"source_mime"` // claimed mime; NOT trusted for detection
}

// EncodeArgs is the River job payload for encoding ONE attachment's variants
// (≤10 per attachment per job — ADR-043 §3). It is a PURE-Go type (this package
// has no lilliput import) so the main API can client.Insert it without pulling
// in the CGO encoder.
//
// The worker re-reads authoritative job state from the DB on every attempt and
// returns nil early when the job is already terminal (dup-delivery safe),
// mirroring the scan worker's idempotency contract.
type EncodeArgs struct {
	TenantID      uuid.UUID       `json:"tenant_id"`
	SiteID        uuid.UUID       `json:"site_id"`
	JobID         string          `json:"job_id"`         // media_optimization_jobs.id (ULID)
	TargetFormat  string          `json:"target_format"`  // 'avif'|'webp'|'original'
	TargetQuality string          `json:"target_quality"` // 'lossy'|'lossless'
	Variants      []EncodeVariant `json:"variants"`
}

// Kind implements river.JobArgs.
func (EncodeArgs) Kind() string { return "media_encode" }

// InsertOpts pins every media_encode job to the dedicated bounded queue, so the
// CPU-heavy AVIF encode cannot starve the default queue. (Mirrors the scan
// worker's per-args queue selection.)
func (EncodeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: MediaEncodeQueue}
}
