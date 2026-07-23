/**
 * format-progress — pure helper extracting display-ready values from a
 * `BackupSnapshot.progress` JSONB blob. Centralizing the type-narrowing here
 * keeps both surfaces (snapshot detail card AND site-detail table cell)
 * consuming the same canonical shape.
 *
 * The agent's progress payload is loosely typed by design (each phase carries
 * its own detail fields) — here we expose ONLY the fields the UI knows how to
 * render. Unknown fields are intentionally dropped.
 */
import type { BackupSnapshot } from "@wpmgr/api";

/** Canonical phase ids — closed set matching CP allowedProgressPhases. */
export const PHASE_IDS = [
  // Backup (ADR-033)
  "queued",
  "started",
  "dumping_db",
  "archiving_files",
  "compressing_files",
  "encrypting",
  "uploading",
  "encrypting_uploading",
  "submitting_manifest",
  // Incremental backup (ADR-048)
  "fetching_file_index",
  "scanning_files",
  "uploading_incremental",
  "incremental_fallback",
  // Restore (ADR-034)
  "preflight",
  "download_artifacts",
  "verify_artifacts",
  "maintenance_on",
  "stage_files",
  "swap_files",
  "restore_db",
  "migrate_db",
  "url_rewrite",
  "swap_db",
  "post_hooks",
  "maintenance_off",
  "cleanup",
  "rolled_back",
  // Shared terminal
  "completed",
  "failed",
] as const;

export type PhaseId = (typeof PHASE_IDS)[number];

export const PHASE_LABEL: Record<PhaseId, string> = {
  // Backup
  queued: "Queued",
  started: "Started",
  dumping_db: "Dumping database",
  archiving_files: "Archiving files",
  compressing_files: "Compressing files",
  encrypting: "Encrypting",
  uploading: "Uploading",
  encrypting_uploading: "Encrypting & uploading",
  submitting_manifest: "Finalising",
  // Incremental backup (ADR-048)
  fetching_file_index: "Fetching file index",
  scanning_files: "Scanning changes",
  uploading_incremental: "Uploading changed files",
  incremental_fallback: "Switching to full backup",
  // Restore (phase names match the agent's task-runner)
  preflight: "Pre-flight checks",
  download_artifacts: "Downloading chunks",
  verify_artifacts: "Verifying chunks",
  maintenance_on: "Maintenance mode on",
  stage_files: "Staging files",
  swap_files: "Swapping files in",
  restore_db: "Restoring database",
  migrate_db: "Migrating URLs",
  url_rewrite: "Rewriting URLs",
  swap_db: "Swapping database in",
  post_hooks: "Post-restore hooks",
  maintenance_off: "Maintenance mode off",
  cleanup: "Cleaning up",
  rolled_back: "Rolled back",
  // Shared terminal
  completed: "Completed",
  failed: "Failed",
};

/**
 * Backup stepper — 5 nodes, stable rhythm regardless of which kind ran.
 * Runner may skip nodes (kind=db skips archiving) but visual cadence is fixed.
 */
export const STEPPER_PHASES: PhaseId[] = [
  "queued",
  "dumping_db",
  "archiving_files",
  "encrypting_uploading",
  "submitting_manifest",
];

/**
 * Incremental-backup stepper (ADR-048). The incremental runner emits its own
 * lead-in phases (fetching_file_index → scanning_files → uploading_incremental)
 * that the full-backup STEPPER_PHASES does not contain, so without a dedicated
 * variant buildStepperPhases would find activeIdx = -1 and render no active dot
 * for an incremental run. The shared tail phase (submitting_manifest) is the
 * final node so the transition out of uploading_incremental stays coherent.
 */
export const INCREMENTAL_STEPPER_PHASES: PhaseId[] = [
  "queued",
  "fetching_file_index",
  "scanning_files",
  "uploading_incremental",
  "submitting_manifest",
];

/** Incremental-specific phases — used to pick the incremental stepper variant. */
export const INCREMENTAL_PHASE_IDS: ReadonlySet<PhaseId> = new Set<PhaseId>([
  "fetching_file_index",
  "scanning_files",
  "uploading_incremental",
  "incremental_fallback",
]);

/**
 * Restore stepper — collapsed 6-node summary of the 13-phase runner. We hide
 * the maintenance/cleanup nodes since they're sub-second; the bar is the
 * user-relevant timeline. The runner may skip some nodes (kind=files skips
 * restore_db + swap_db); we still render all 6 so the visual stays stable.
 */
export const RESTORE_STEPPER_PHASES: PhaseId[] = [
  "preflight",
  "download_artifacts",
  "stage_files",
  "restore_db",
  "swap_files",
  "cleanup",
];

/** Restore phase ids — used to detect "we're showing a restore, not a backup". */
export const RESTORE_PHASE_IDS: ReadonlySet<PhaseId> = new Set<PhaseId>([
  "preflight",
  "download_artifacts",
  "verify_artifacts",
  "maintenance_on",
  "stage_files",
  "swap_files",
  "restore_db",
  "migrate_db",
  "url_rewrite",
  "swap_db",
  "post_hooks",
  "maintenance_off",
  "cleanup",
  "rolled_back",
]);

/** Phases that emit a determinate percent we can render with a real bar. */
const DETERMINATE_PHASES: ReadonlySet<PhaseId> = new Set<PhaseId>([
  // Backup
  "archiving_files",
  "compressing_files",
  "encrypting_uploading",
  "uploading",
  // Restore
  "download_artifacts",
  "stage_files",
  "restore_db",
  "swap_db",
]);

/** Phases that are SUCCESSFULLY done when reached (mid-pipeline jumps OK). */
export const TERMINAL_PHASES: ReadonlySet<PhaseId> = new Set<PhaseId>([
  "completed",
  "failed",
  "rolled_back",
]);

export function isRestorePhase(phase: PhaseId): boolean {
  return RESTORE_PHASE_IDS.has(phase);
}

export function isIncrementalPhase(phase: PhaseId): boolean {
  return INCREMENTAL_PHASE_IDS.has(phase);
}

/**
 * True when the snapshot is actively running a RESTORE.
 *
 * A restore OVERLAYS a completed snapshot: snapshot.status stays "completed" for
 * the entire restore (ADR-034), so we MUST NOT use formatProgress().isTerminal
 * here — that flag is true whenever status === "completed" and would report
 * every live restore as already finished (the Batch-2 regression that hid the
 * restore progress card while the SSE stream was patching live phases). Gate on
 * the PHASE only: a restore is active while its phase is a restore phase that
 * has not reached a phase-level terminal (completed / failed / rolled_back).
 */
export function isRestoreActive(snapshot: BackupSnapshot): boolean {
  const fp = formatProgress(snapshot);
  const phaseTerminal =
    fp.phase === "completed" ||
    fp.phase === "failed" ||
    fp.phase === "rolled_back";
  return isRestorePhase(fp.phase) && !phaseTerminal;
}

/**
 * GH #279 — true only while a run is genuinely still active AND the CP's
 * two-tier watchdog has stamped `stalled_at` (gone quiet past the soft
 * threshold, not yet hard-failed). Gated on `status === "running"` so a
 * terminal snapshot never shows the hint, even if a stale `stalled_at`
 * value were somehow still present (the CP always clears it before failing
 * or completing a run, but the UI gate does not rely on that alone).
 */
export function isSnapshotStalled(
  snapshot: Pick<BackupSnapshot, "stalled_at" | "status">,
): boolean {
  return Boolean(snapshot.stalled_at) && snapshot.status === "running";
}

export interface FormattedProgress {
  phase: PhaseId;
  /** Render-ready phase label. */
  label: string;
  /** Determinate 0–100, or null for indeterminate / unknown. */
  percent: number | null;
  /** True if the bar should render as indeterminate (no measurable %). */
  isIndeterminate: boolean;
  /** Current archive part filename, e.g. "wp-content.part003.zip". */
  artifact: string | null;
  /** Currently-processing file path, e.g. "uploads/2024/foo.jpg". */
  currentFile: string | null;
  /** files_done. */
  filesDone: number | null;
  /** files_total. */
  filesTotal: number | null;
  /** chunks_done. */
  chunksDone: number | null;
  /** chunks_total. */
  chunksTotal: number | null;
  /** bytes_written or bytes_done (whichever is present). */
  bytesDone: number | null;
  /** True when the snapshot is in a terminal state (completed or failed). */
  isTerminal: boolean;
  /** Short error message for failed phase, surfaced from phase_detail.message. */
  errorMessage: string | null;
}

function num(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function str(v: unknown): string | null {
  return typeof v === "string" && v.length > 0 ? v : null;
}

function isPhaseId(v: unknown): v is PhaseId {
  return typeof v === "string" && (PHASE_IDS as readonly string[]).includes(v);
}

export function formatProgress(snapshot: BackupSnapshot): FormattedProgress {
  const raw = snapshot.progress;
  const rawPhase = raw?.phase;
  const phase: PhaseId = isPhaseId(rawPhase)
    ? rawPhase
    : snapshot.status === "completed"
      ? "completed"
      : snapshot.status === "failed"
        ? "failed"
        : "queued";

  const rawDetail = raw?.phase_detail;
  const detail: Record<string, unknown> =
    rawDetail && typeof rawDetail === "object"
      ? (rawDetail as Record<string, unknown>)
      : {};

  const filesDone = num(detail.files_done);
  const filesTotal = num(detail.files_total);
  const chunksDone = num(detail.chunks_done);
  const chunksTotal = num(detail.chunks_total);

  // Per-phase percent calculation. Different phases carry different
  // determinate counters in `phase_detail`:
  //   - archiving_files: files_done/files_total (from FilesArchiver)
  //   - encrypting_uploading / uploading / compressing_files: chunks_done/chunks_total (from EncryptAndUpload)
  //   - download_artifacts (restore): artifacts_done/artifacts_total
  //   - stage_files (restore): files_done/files_total (from FilesRestorer)
  //   - restore_db (restore): statements_done/statements_total (from DbRestorer)
  //   - swap_db (restore): tables_done/tables_total (from DbRestorer::swap)
  // Any phase not listed → percent stays null → indeterminate shimmer.
  let percent: number | null = null;
  const artifactsDone = num(detail.artifacts_done);
  const artifactsTotal = num(detail.artifacts_total);
  const statementsDone = num(detail.statements_done);
  const statementsTotal = num(detail.statements_total);
  const tablesDone = num(detail.tables_done);
  const tablesTotal = num(detail.tables_total);

  if (phase === "archiving_files" && filesTotal && filesTotal > 0) {
    percent = Math.min(100, Math.round(((filesDone ?? 0) / filesTotal) * 100));
  } else if (
    (phase === "encrypting_uploading" ||
      phase === "uploading" ||
      phase === "compressing_files") &&
    chunksTotal &&
    chunksTotal > 0
  ) {
    percent = Math.min(100, Math.round(((chunksDone ?? 0) / chunksTotal) * 100));
  } else if (phase === "download_artifacts" && artifactsTotal && artifactsTotal > 0) {
    percent = Math.min(100, Math.round(((artifactsDone ?? 0) / artifactsTotal) * 100));
  } else if (phase === "stage_files" && filesTotal && filesTotal > 0) {
    percent = Math.min(100, Math.round(((filesDone ?? 0) / filesTotal) * 100));
  } else if (phase === "restore_db" && statementsTotal && statementsTotal > 0) {
    percent = Math.min(100, Math.round(((statementsDone ?? 0) / statementsTotal) * 100));
  } else if (phase === "swap_db" && tablesTotal && tablesTotal > 0) {
    percent = Math.min(100, Math.round(((tablesDone ?? 0) / tablesTotal) * 100));
  } else if (phase === "completed") {
    percent = 100;
  }

  const isIndeterminate = percent === null && !DETERMINATE_PHASES.has(phase);

  return {
    phase,
    label: PHASE_LABEL[phase],
    percent,
    isIndeterminate,
    artifact: str(detail.current_artifact) ?? str(detail.artifact),
    currentFile: str(detail.current_file),
    filesDone,
    filesTotal,
    chunksDone,
    chunksTotal,
    bytesDone: num(detail.bytes_written) ?? num(detail.bytes_done),
    isTerminal: TERMINAL_PHASES.has(phase) || snapshot.status === "completed" || snapshot.status === "failed",
    errorMessage: str(detail.message),
  };
}

/** Build the {id, status} input for PhaseStepper from the active phase id. */
export function buildStepperPhases(
  active: PhaseId,
  overallStatus: BackupSnapshot["status"],
): { id: PhaseId; label: string; status: "completed" | "active" | "pending" | "failed" }[] {
  // Pick the stepper based on whether the active phase is a restore,
  // incremental-backup, or full-backup phase. The terminal phases
  // (completed/failed) are shared — for terminals we keep whichever stepper was
  // most recently relevant (default to backup because a snapshot starts as a
  // backup; restore is overlaid later).
  const isRestore = isRestorePhase(active);
  const isIncremental = !isRestore && isIncrementalPhase(active);
  const stepper = isRestore
    ? RESTORE_STEPPER_PHASES
    : isIncremental
      ? INCREMENTAL_STEPPER_PHASES
      : STEPPER_PHASES;

  // The runner emits literal phases, but the compact restore/incremental
  // steppers collapse several phases into a single visual node. Map the active
  // phase to the nearest stepper node so the right circle pulses.
  const collapsedActive: PhaseId = isRestore
    ? collapseToRestoreStepperNode(active)
    : isIncremental
      ? collapseToIncrementalStepperNode(active)
      : active;

  const activeIdx = stepper.indexOf(collapsedActive);
  return stepper.map((id, idx) => {
    let status: "completed" | "active" | "pending" | "failed" = "pending";
    if (overallStatus === "failed" && active === "failed") {
      // Snapshot truly failed: mark the active node failed; past stay completed.
      if (idx < activeIdx) status = "completed";
      else if (idx === activeIdx) status = "failed";
    } else if (active === "completed" || overallStatus === "completed" && !isRestore) {
      status = "completed";
    } else if (idx < activeIdx) {
      status = "completed";
    } else if (idx === activeIdx) {
      status = "active";
    }
    return { id, label: PHASE_LABEL[id], status };
  });
}

/**
 * Restore runner emits 13 phases but the stepper shows 6 collapsed nodes.
 * Map each runtime phase to the stepper node that best represents it so the
 * right circle pulses. Sub-second phases (maintenance_on/off, post_hooks)
 * collapse into adjacent nodes.
 */
function collapseToRestoreStepperNode(active: PhaseId): PhaseId {
  switch (active) {
    case "preflight":
      return "preflight";
    case "download_artifacts":
    case "verify_artifacts":
      return "download_artifacts";
    case "maintenance_on":
    case "stage_files":
      return "stage_files";
    case "restore_db":
    case "migrate_db":
    case "swap_db":
      return "restore_db";
    case "swap_files":
      return "swap_files";
    case "post_hooks":
    case "maintenance_off":
    case "cleanup":
      return "cleanup";
    default:
      return active;
  }
}

/**
 * Map an incremental-specific phase onto its incremental stepper node so the
 * right circle pulses. Only reached for the four INCREMENTAL_PHASE_IDS — the
 * shared tail phases (dumping_db / encrypting_uploading / submitting_manifest)
 * are not incremental phases, so they route to the full STEPPER_PHASES where
 * those nodes already exist. incremental_fallback collapses onto scanning_files
 * (the scan decided to fall back to a full backup before any upload).
 */
function collapseToIncrementalStepperNode(active: PhaseId): PhaseId {
  switch (active) {
    case "incremental_fallback":
      return "scanning_files";
    default:
      return active;
  }
}

/** Format bytes with iOS-Files-style buckets. */
export function formatBytesShort(bytes: number | null): string | null {
  if (bytes === null || bytes < 0) return null;
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(0)} KB`;
  if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  return `${(bytes / 1024 ** 3).toFixed(2)} GB`;
}
