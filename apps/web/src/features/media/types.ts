// Media Optimizer — wire types (ADR-043 §7).
//
// These TS shapes are HAND-ROLLED to match the Go DTOs in
// apps/api/internal/media/handler/handler.go field-for-field (the scan-feature
// convention — these routes are NOT in the ogen-generated @wpmgr/api SDK). If a
// field changes in handler.go, change it here. Mapping notes:
//
//   assetDTO          → MediaAsset        (listAssets items)
//   summaryDTO        → MediaSummary      (listAssets summary)
//   listAssetsDTO     → ListAssetsResponse
//   jobDTO            → MediaJob          (jobs items / job detail base)
//   variantDTO        → MediaVariant      (job detail variants)
//   jobListDTO        → ListJobsResponse
//   jobDetailDTO      → MediaJobDetail    (jobDTO + variants)
//
// The eight asset statuses + the job/variant states mirror the Go const blocks
// in apps/api/internal/media/model/{asset,job,variant}.go.

/** The 8 lifecycle states of a site_media_assets row (model.AssetStatus). */
export type AssetStatus =
  | "pending"
  | "optimizing"
  | "optimized"
  | "failed"
  | "restoring"
  | "restored"
  | "excluded"
  | "originals_deleted";

/** The format optimized variants are in now (model.CurrentFormat). */
export type CurrentFormat = "original" | "webp" | "avif";

/** Target format an optimize job encodes to (media.ValidTargetFormat). */
export type TargetFormat = "avif" | "webp" | "original";

/** Target quality (media.ValidTargetQuality). */
export type TargetQuality = "lossy" | "lossless";

/** Job kind (model.JobKind). */
export type JobKind = "optimize" | "restore" | "delete_originals" | "sync";

/** Job lifecycle state (model.JobState). */
export type JobState =
  | "queued"
  | "in_progress"
  | "succeeded"
  | "partially_succeeded"
  | "failed"
  | "cancelled";

/** Per-variant encode outcome (model.VariantState). */
export type VariantState = "succeeded" | "failed" | "skipped";

/** assetDTO — one media library asset (handler.go:55). */
export interface MediaAsset {
  id: string;
  site_id: string;
  wp_attachment_id: number;
  title: string;
  original_url: string;
  original_mime: string;
  original_size_bytes: number;
  current_format: string;
  current_size_bytes: number;
  status: AssetStatus;
  generation: number;
  sizes_optimized?: string[];
  sizes_unoptimized?: Record<string, string>;
  last_optimized_at?: string;
}

/** summaryDTO — the dashboard rollup (handler.go:72). */
export interface MediaSummary {
  total: number;
  optimized: number;
  pending: number;
  failed: number;
  bytes_saved: number;
}

/** listAssetsDTO — GET /media/assets response (handler.go:80). */
export interface ListAssetsResponse {
  items: MediaAsset[];
  next_cursor?: string;
  total_count: number;
  summary: MediaSummary;
}

/** jobDTO — one media job (handler.go:87). */
export interface MediaJob {
  id: string;
  site_id: string;
  asset_id?: string;
  wp_attachment_id: number;
  kind: JobKind;
  target_format?: string;
  target_quality?: string;
  state: JobState;
  bytes_before?: number;
  bytes_after?: number;
  variants_total: number;
  variants_succeeded: number;
  variants_failed: number;
  error_reason?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
}

/** variantDTO — one per-size encode result (handler.go:107). */
export interface MediaVariant {
  variant_name: string;
  source_size_bytes: number;
  optimized_size_bytes?: number;
  source_mime: string;
  optimized_mime?: string;
  encode_ms?: number;
  state: VariantState;
  reason?: string;
}

/** jobListDTO — GET /media/jobs response (handler.go:118). */
export interface ListJobsResponse {
  items: MediaJob[];
  next_cursor?: string;
}

/** jobDetailDTO — GET /media/jobs/:jobId response (handler.go:123). */
export interface MediaJobDetail extends MediaJob {
  variants: MediaVariant[];
}

// ── POST response bodies (handler.go route handlers) ───────────────────────

/** POST /media/sync → 202 (handler.go:258). */
export interface SyncResponse {
  job_id: string;
  started_at: string;
}

/** POST /media/optimize|restore|delete-originals → 202 (handler.go:282…). */
export interface BatchResponse {
  batch_job_id: string;
  queued_count: number;
  /** Only present on delete-originals (always true). */
  irreversible?: boolean;
}

/** POST /media/cancel → 200 (handler.go:348). */
export interface CancelResponse {
  ok: boolean;
  cancelled_count: number;
}

/** Body for POST /media/optimize (handler.go optimizeBody:128). */
export interface OptimizeBody {
  asset_ids?: string[];
  all_pending?: boolean;
  target_format: TargetFormat;
  target_quality: TargetQuality;
}

/** Body for POST /media/restore and /media/delete-originals (assetSelectionBody:135). */
export interface AssetSelectionBody {
  asset_ids?: string[];
}

/** Terminal job states — a JobsDrawer row stops "running" once it reaches one. */
const TERMINAL_JOB_STATES: ReadonlySet<JobState> = new Set([
  "succeeded",
  "partially_succeeded",
  "failed",
  "cancelled",
]);

export function isTerminalJobState(state: JobState): boolean {
  return TERMINAL_JOB_STATES.has(state);
}
