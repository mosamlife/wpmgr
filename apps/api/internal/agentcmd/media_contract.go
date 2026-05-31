package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the Media
// Optimizer (ADR-043). The wp-agent-engineer mirrors these shapes in the agent
// (Phase 4: class-media-optimize-command.php / class-media-apply-command.php).
// Field names are JSON wire names; do not rename without updating both sides.
//
// Transport — media_optimize:
//   POST {site_url}/wp-json/wpmgr/v1/command/media_optimize
//   Header: Authorization: Bearer <minted EdDSA JWT>  (cmd="media_optimize",
//           aud=<siteId>)
//   Body:   application/json — MediaOptimizeRequest below.
//   Response: 200 with MediaOptimizeResponse; ok=false = semantic refusal.
//
// Transport — media_apply:
//   POST {site_url}/wp-json/wpmgr/v1/command/media_apply
//   Header: Authorization: Bearer <minted EdDSA JWT>  (cmd="media_apply",
//           aud=<siteId>)
//   Body:   application/json — MediaApplyRequest below.
//   Response: 200 with MediaApplyResponse.
//
// Transport — media_sync / media_restore / media_delete_originals mirror
// media_optimize (one job per attachment; the agent acts on its next poll OR
// immediately on the pushed command).
//
// ===========================  BYTE-TRANSPORT  ===============================
// No image bytes move through the CP. The agent presigned-PUTs source variants
// to media/<tenant>/<site>/<job>/src/<name>, the media-encoder presigned-GETs
// them, encodes, and presigned-PUTs outputs to media/.../out/<name>; the agent
// presigned-GETs each output and applies it on disk. The CP holds metadata only.
// ============================================================================

// MediaOptimizeRequest is the POST body for the `media_optimize` command. The
// agent enumerates each job's attachment, presigned-PUTs every registered size
// to src/<name> (≤10 per attachment — ADR-043 §3), then calls back
// POST /agent/v1/media/encode-ready so the CP enqueues the encode jobs.
//
//	job_ids         the CP job ids (ULIDs) to act on — one per attachment.
//	target_format   "avif" | "webp" | "original".
//	target_quality  "lossy" | "lossless".
//	presign_endpoint the agent->CP endpoint to request presigned PUT URLs.
//	ready_endpoint   the agent->CP endpoint to signal sources are uploaded.
type MediaOptimizeRequest struct {
	JobIDs          []string `json:"job_ids"`
	TargetFormat    string   `json:"target_format"`
	TargetQuality   string   `json:"target_quality"`
	PresignEndpoint string   `json:"presign_endpoint"`
	ReadyEndpoint   string   `json:"ready_endpoint"`
}

// MediaOptimizeResponse is the agent's ack of the `media_optimize` command.
type MediaOptimizeResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MediaApplyVariant is one optimized output the agent must download + apply.
//
//	name           registered size name ('full'|'thumbnail'|...).
//	get_url        presigned GET URL for media/.../out/<name> (bearer; never logged).
//	optimized_mime the output mime (e.g. "image/avif").
//	optimized_size optimized byte length (for verification / progress).
type MediaApplyVariant struct {
	Name          string `json:"name"`
	GetURL        string `json:"get_url"`
	OptimizedMime string `json:"optimized_mime"`
	OptimizedSize int64  `json:"optimized_size"`
}

// MediaApplyRequest is the POST body for the `media_apply` command: the encoder
// finished an attachment's variants and the agent should download out/* and
// apply them on disk, then call back POST /agent/v1/media/job-status.
//
//	job_id           the CP job (ULID) being applied.
//	target_format    echoed format (so the agent records compression_level/blob).
//	target_quality   echoed quality.
//	status_endpoint  the agent->CP endpoint to report the apply result.
//	variants         the optimized outputs with presigned GET URLs.
type MediaApplyRequest struct {
	JobID          string              `json:"job_id"`
	TargetFormat   string              `json:"target_format"`
	TargetQuality  string              `json:"target_quality"`
	StatusEndpoint string              `json:"status_endpoint"`
	Variants       []MediaApplyVariant `json:"variants"`
}

// MediaApplyResponse is the agent's ack of the `media_apply` command.
type MediaApplyResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MediaSyncRequest is the POST body for the `media_sync` command: the agent
// enumerates its media library and pushes pages to POST /agent/v1/media/sync-batch.
//
//	job_id        the sync job (ULID) for cross-reference.
//	batch_endpoint the agent->CP endpoint to push each sync page.
type MediaSyncRequest struct {
	JobID         string `json:"job_id"`
	BatchEndpoint string `json:"batch_endpoint"`
}

// MediaSyncResponse is the agent's ack of the `media_sync` command.
type MediaSyncResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MediaRestoreRequest is the POST body for the `media_restore` command: revert
// the attachments behind job_ids to their pre-optimization state, then call back
// POST /agent/v1/media/restore-status.
type MediaRestoreRequest struct {
	JobIDs         []string `json:"job_ids"`
	StatusEndpoint string   `json:"status_endpoint"`
}

// MediaRestoreResponse is the agent's ack of the `media_restore` command.
type MediaRestoreResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MediaDeleteOriginalsRequest is the POST body for the `media_delete_originals`
// command: IRREVERSIBLY delete the archived originals behind job_ids, then call
// back POST /agent/v1/media/job-status.
type MediaDeleteOriginalsRequest struct {
	JobIDs         []string `json:"job_ids"`
	StatusEndpoint string   `json:"status_endpoint"`
}

// MediaDeleteOriginalsResponse is the agent's ack of the command.
type MediaDeleteOriginalsResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}
