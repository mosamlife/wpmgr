<?php
/**
 * AutoOptimizeUpload — ADR-044 Phase A agent implementation.
 *
 * Observes fresh WordPress media uploads and asynchronously notifies the
 * control plane so it can enqueue the new attachment through the existing
 * media_optimize pipeline without blocking the upload request.
 *
 * Architecture (ADR-044 §1, §2, §3, §6):
 *
 *   1. UPLOAD HOOK — `wp_generate_attachment_metadata` filter at priority 9999.
 *      Runs AFTER core has generated every registered sub-size. Bails early and
 *      ALWAYS returns `$metadata` unchanged (this is a filter, not an action).
 *      Acts ONLY when $context === 'create' (a fresh upload, never a thumbnail
 *      regeneration or a WPMgr metadata update).
 *
 *   2. PENDING BUFFER — a deduplicated integer list stored in the WordPress
 *      options table under OPTION_PENDING. The filter appends the attachment id
 *      and schedules exactly ONE debounced drain event. WP's built-in
 *      wp_schedule_single_event deduplication collapses repeated schedules
 *      (e.g. 50 simultaneous uploads) into a single pending event, so the drain
 *      always issues a SINGLE batched POST to the CP regardless of upload volume.
 *
 *   3. DRAIN — `wpmgr_autoopt_drain` cron event (HOOK_DRAIN). Reads + clears the
 *      buffer, deduplicates, and for each id calls MediaAttachmentRow::build()
 *      to produce the full syncBatchAttachmentDTO-shaped row. The batch is then
 *      POSTed as `{ "attachments": [ <row>, ... ] }` to
 *      `/agent/v1/media/auto-optimize` via the signed shipPayload primitive (the
 *      same one used by delete_attachment / diagnostics). Ids whose attachment
 *      has been deleted between upload and drain are silently skipped (build()
 *      returns null). On a non-2xx response the ids are KEPT in the buffer and
 *      the drain reschedules itself with a short back-off so an offline CP
 *      cannot lose uploads.
 *
 *   4. RE-ENTRANCY GUARD — a process-scoped static flag (self::$guard) that any
 *      apply / restore / delete_originals execution can set via
 *      AutoOptimizeUpload::setGuard(true) before calling
 *      wp_update_attachment_metadata and clear via setGuard(false) after. The
 *      upload-hook callback checks the flag and bails, preventing the loop
 *      described in ADR-044 §6.
 *
 *      The guard is complemented by three additional layers (ADR-044 §6):
 *        a. applyOptimizedMetadata() uses wp_update_attachment_metadata (the
 *           SETTER), NOT wp_generate_attachment_metadata (the generator the
 *           filter hooks). This is the primary, load-bearing invariant.
 *        b. `$context !== 'create'` rejects the regenerate/update class entirely.
 *        c. The MediaKeystore::isOptimized() idempotency gate makes any stray
 *           re-fire a no-op (the attachment is already flagged optimized).
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Media\MediaAttachmentRow;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Observes uploads, buffers attachment ids, and drains via a signed async POST.
 */
final class AutoOptimizeUpload
{
    // -------------------------------------------------------------------------
    // Constants
    // -------------------------------------------------------------------------

    /**
     * WordPress options key for the pending-upload attachment id buffer.
     * Non-autoloaded (the filter writes it inline; the drain reads it in a
     * separate cron request).
     */
    public const OPTION_PENDING = 'wpmgr_autoopt_pending';

    /**
     * WP-Cron hook name for the debounced drain event. Arg-less so WP's
     * built-in deduplication collapses repeated schedules into one pending
     * event (multiple uploads within the DEBOUNCE window = one drain call).
     */
    public const HOOK_DRAIN = 'wpmgr_autoopt_drain';

    /**
     * Debounce window in seconds. Uploads within this window are coalesced
     * into a single batched POST. ADR-044 §2 recommends 20-30s.
     */
    private const DEBOUNCE = 25;

    /**
     * Back-off window for the retry schedule when the CP is unreachable.
     * Short (90s) so the ids are retried well within the next WP-Cron tick.
     */
    private const BACKOFF = 90;

    /**
     * Maximum batch size per drain call. A very large pending buffer (e.g. an
     * import storm that bypassed the debounce) is capped here; excess ids stay
     * in the buffer for the next drain tick.
     */
    private const MAX_BATCH = 200;

    /**
     * Hard ceiling on the persisted pending buffer. Bounds wp_options growth on a
     * site that drops a large import while the CP is unreachable (the drain keeps
     * re-merging its ≤MAX_BATCH and new uploads keep appending). Overflow is
     * dropped from the OLDEST end; the periodic media_sync + operator
     * "optimize pending" reconcile anything dropped, so no work is lost.
     */
    private const MAX_PENDING_BUFFER = 2000;

    /**
     * Optimizable source MIME types — MUST match MediaOptimizeCommand::OPTIMIZABLE_MIMES
     * and ADR-044 §5 (image/gif now included per GIF support build).
     */
    private const OPTIMIZABLE_MIMES = ['image/jpeg', 'image/jpg', 'image/png', 'image/gif'];

    // -------------------------------------------------------------------------
    // Re-entrancy guard (ADR-044 §6, mitigation 2)
    // -------------------------------------------------------------------------

    /**
     * Process-scoped re-entrancy guard.
     *
     * Set to TRUE by the agent's own apply/restore/delete_originals execution
     * paths (via AutoOptimizeUpload::setGuard(true)) BEFORE they call
     * wp_update_attachment_metadata, and reset to FALSE afterwards. The upload-
     * hook callback bails immediately when this flag is set so a stray
     * wp_generate_attachment_metadata re-fire during our own work cannot enqueue
     * the same attachment for a second optimization pass.
     *
     * This is the PRIMARY ACTIVE GUARD. The metadata setter invariant
     * (wp_update_attachment_metadata rather than wp_generate_attachment_metadata)
     * is the load-bearing structural invariant; this flag is the belt-and-
     * suspenders layer for any edge case where a filter chain re-invokes the
     * generate path despite that structural invariant.
     *
     * @var bool
     */
    private static bool $guard = false;

    /**
     * Set or clear the process-scoped re-entrancy guard.
     *
     * Call this from apply/restore/delete_originals background workers BEFORE
     * any wp_update_attachment_metadata call, then call with false after:
     *
     *   AutoOptimizeUpload::setGuard(true);
     *   try {
     *       $this->meta->applyOptimizedMetadata(...);
     *   } finally {
     *       AutoOptimizeUpload::setGuard(false);
     *   }
     *
     * @param bool $active TRUE to arm the guard, FALSE to release it.
     * @return void
     */
    public static function setGuard(bool $active): void
    {
        self::$guard = $active;
    }

    /**
     * Whether the re-entrancy guard is currently armed.
     *
     * @return bool
     */
    public static function isGuarded(): bool
    {
        return self::$guard;
    }

    // -------------------------------------------------------------------------
    // Dependencies
    // -------------------------------------------------------------------------

    private Settings $settings;

    /**
     * Closure that performs the signed POST to the CP. Signature:
     *   fn(string $path, array $payload): array{ok:bool,status:int}
     * Matches Plugin::shipPayload exactly; injected so the drain can be tested
     * without a live Signer/Keystore.
     *
     * @var \Closure(string,array<string,mixed>):array{ok:bool,status:int}
     */
    private \Closure $shipper;

    /**
     * @param Settings $settings  Typed settings accessor.
     * @param \Closure $shipper   Signed-POST primitive (Plugin::shipPayload).
     */
    public function __construct(Settings $settings, \Closure $shipper)
    {
        $this->settings = $settings;
        $this->shipper  = $shipper;
    }

    // -------------------------------------------------------------------------
    // Upload hook (Task 1)
    // -------------------------------------------------------------------------

    /**
     * `wp_generate_attachment_metadata` filter callback — priority 9999.
     *
     * MUST ALWAYS return $metadata unchanged (this is a filter, not an action).
     * We only observe and schedule; we NEVER modify attachment metadata inline.
     *
     * Early-bail conditions (in order — cheapest first):
     *   1. $metadata pass-through guard (filter contract non-negotiable).
     *   2. $context !== 'create' — reject thumbnail regeneration / update paths.
     *   3. Re-entrancy guard — bail when the agent's own media work is active.
     *   4. Feature toggle — bail when auto-optimize is disabled in settings.
     *   5. MIME gate — bail for non-optimizable MIMEs (WebP, AVIF, SVG, etc.).
     *   6. Idempotency gate — bail when the attachment is already optimized.
     *
     * @param mixed  $metadata     The attachment metadata array from WP core.
     * @param int    $attachmentId WP attachment post id.
     * @param string $context      'create' (fresh upload) or 'update'/'edit'.
     * @return mixed The unmodified $metadata value.
     */
    public function onGenerateMetadata($metadata, int $attachmentId, string $context)
    {
        // --- Guard 1: filter contract — ALWAYS return $metadata. ---------------
        // (All early bails are structured as returns of $metadata so this is
        // enforced by the structure, not by a single return at the bottom only.)

        // --- Guard 2: fresh upload only ----------------------------------------
        if ($context !== 'create') {
            return $metadata; // thumbnail regeneration / update path — ignore
        }

        // --- Guard 3: re-entrancy guard ----------------------------------------
        // The agent's own apply/restore/delete_originals work has set this flag.
        // Bail to prevent an infinite optimize loop (ADR-044 §6, mitigation 2).
        if (self::$guard) {
            return $metadata;
        }

        // --- Guard 4: feature enabled? -----------------------------------------
        if (!$this->settings->mediaAutoOptimize()) {
            return $metadata; // opt-in feature is off for this site
        }

        // --- Guard 5: MIME gate ------------------------------------------------
        if ($attachmentId <= 0) {
            return $metadata;
        }
        $mime = '';
        if (function_exists('get_post_mime_type')) {
            $raw = get_post_mime_type($attachmentId);
            $mime = is_string($raw) ? strtolower($raw) : '';
        }
        if (!in_array($mime, self::OPTIMIZABLE_MIMES, true)) {
            return $metadata; // e.g. image/webp, image/avif, image/svg+xml, video/*
        }

        // --- Guard 6: idempotency (already optimized) --------------------------
        if ((new MediaKeystore())->isOptimized($attachmentId)) {
            return $metadata;
        }

        // -------------------------------------------------------------------
        // Buffer the id and schedule one debounced drain.
        // DO ZERO network/encode work here — the upload request must return fast.
        // -------------------------------------------------------------------
        $this->appendPending($attachmentId);
        $this->scheduleDebounced();

        // ALWAYS return the unmodified metadata — non-negotiable filter contract.
        return $metadata;
    }

    // -------------------------------------------------------------------------
    // Drain handler (Task 2)
    // -------------------------------------------------------------------------

    /**
     * WP-Cron drain callback — bound to HOOK_DRAIN in Plugin::registerHooks.
     *
     * Reads and atomically clears the pending buffer (up to MAX_BATCH ids),
     * deduplicates, resolves each id into a full syncBatchAttachmentDTO-shaped
     * row via MediaAttachmentRow::build(), and POSTs:
     *
     *   POST /agent/v1/media/auto-optimize
     *   { "attachments": [ { wp_attachment_id, title, original_path,
     *                         original_url, original_mime, original_width,
     *                         original_height, original_size_bytes }, ... ] }
     *
     * The full row lets the CP upsert the asset record before optimizing —
     * fresh uploads are not yet present in the CP's asset table (no real-time
     * sync). Ids whose attachment was deleted between upload and drain are
     * silently skipped (MediaAttachmentRow::build returns null).
     *
     * On a non-2xx response the ids are written BACK to the buffer and the
     * drain reschedules itself with a short back-off so an offline CP cannot
     * silently drop pending uploads.
     *
     * Runs under a bounded set_time_limit() + ignore_user_abort(true) because
     * this is a cron worker in a separate FPM request — it must not be killed
     * mid-POST.
     *
     * @return void
     */
    public function drain(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }

        // Lift the PHP execution ceiling — we are in a dedicated cron worker.
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        }
        if (function_exists('ignore_user_abort')) {
            ignore_user_abort(true);
        }

        // Read + clear the pending buffer in one atomic swap.
        $all = $this->popPending();
        if ($all === []) {
            return; // Nothing to drain.
        }

        // Deduplicate and cap the batch.
        $batch    = array_values(array_unique($all));
        $overflow = [];
        if (count($batch) > self::MAX_BATCH) {
            $overflow = array_slice($batch, self::MAX_BATCH);
            $batch    = array_slice($batch, 0, self::MAX_BATCH);
        }

        // PUT any overflow back immediately (before the network call, so a kill
        // mid-POST does not lose the overflow).
        if ($overflow !== []) {
            $this->mergePending($overflow);
        }

        // Build the full syncBatchAttachmentDTO-shaped rows so the CP can upsert
        // the asset row before optimizing (fresh uploads are not yet synced to the
        // CP in real time — the CP needs the full row to create the asset record).
        // Ids whose attachment was deleted between upload and drain are skipped
        // (MediaAttachmentRow::build returns null for missing attachments).
        $rows = [];
        foreach ($batch as $id) {
            $row = MediaAttachmentRow::build($id);
            if ($row !== null) {
                $rows[] = $row;
            }
        }

        // POST the batch to the CP via the signed shipPayload primitive.
        // Body shape: { "attachments": [ <syncBatchAttachmentDTO>, ... ] }
        $result = ($this->shipper)(
            '/agent/v1/media/auto-optimize',
            ['attachments' => $rows]
        );

        if (!($result['ok'] ?? false)) {
            // CP unreachable or returned an error — keep the ids buffered and
            // reschedule a retry with back-off (ADR-044 §7 offline resilience).
            $this->mergePending($batch);
            $this->scheduleBackoff();
        }
        // On success the buffer was already cleared above (popPending); done.
    }

    // -------------------------------------------------------------------------
    // Buffer helpers (private)
    // -------------------------------------------------------------------------

    /**
     * Append a single attachment id to the pending buffer. Uses a DB transaction
     * guard via get/update (WP has no native atomic append, but wp-cron runs in
     * its own FPM request so concurrent appends from the SAME cron call are
     * impossible; concurrent appends from DIFFERENT upload requests are safe
     * because WP serialises DB writes — worst case a duplicated id, deduped at
     * drain time).
     *
     * @param int $attachmentId
     * @return void
     */
    private function appendPending(int $attachmentId): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }
        $current = get_option(self::OPTION_PENDING, []);
        if (!is_array($current)) {
            $current = [];
        }
        $current[] = $attachmentId;
        // Bound the buffer so a sustained import storm with an unreachable CP
        // can't bloat wp_options on this site (security review L1). Dropped
        // overflow is reconciled by the periodic media_sync + the operator
        // "optimize pending" backstop (ADR-044 §7) — never silently lost work.
        if (count($current) > self::MAX_PENDING_BUFFER) {
            $current = array_slice($current, -self::MAX_PENDING_BUFFER);
        }
        update_option(self::OPTION_PENDING, $current, false);
    }

    /**
     * Atomically read and clear the pending buffer. Returns the ids that were
     * stored, or [] when the buffer was empty.
     *
     * @return list<int>
     */
    private function popPending(): array
    {
        if (!function_exists('get_option') || !function_exists('delete_option')) {
            return [];
        }
        $current = get_option(self::OPTION_PENDING, []);
        if (!is_array($current) || $current === []) {
            return [];
        }
        delete_option(self::OPTION_PENDING);

        return array_values(array_filter(array_map('intval', $current)));
    }

    /**
     * Merge ids back into the pending buffer (used by the drain on non-2xx and
     * for overflow). Deduplicates against what is already in the buffer.
     *
     * @param list<int> $ids
     * @return void
     */
    private function mergePending(array $ids): void
    {
        if ($ids === [] || !function_exists('get_option') || !function_exists('update_option')) {
            return;
        }
        $current = get_option(self::OPTION_PENDING, []);
        if (!is_array($current)) {
            $current = [];
        }
        $merged = array_values(array_unique(array_merge($current, $ids)));
        update_option(self::OPTION_PENDING, $merged, false);
    }

    // -------------------------------------------------------------------------
    // Schedule helpers (private)
    // -------------------------------------------------------------------------

    /**
     * Schedule the debounced drain event. wp_schedule_single_event's built-in
     * 10-minute deduplication ensures that repeated calls (one per uploaded file
     * in a bulk upload) collapse into a single pending event.
     *
     * @return void
     */
    private function scheduleDebounced(): void
    {
        if (!function_exists('wp_schedule_single_event')) {
            return;
        }
        // arg-less hook so WP's dedup logic treats all these schedules as
        // identical and keeps only the first one pending.
        wp_schedule_single_event(time() + self::DEBOUNCE, self::HOOK_DRAIN);
    }

    /**
     * Schedule a retry drain after a short back-off.
     *
     * @return void
     */
    private function scheduleBackoff(): void
    {
        if (!function_exists('wp_schedule_single_event')) {
            return;
        }
        wp_schedule_single_event(time() + self::BACKOFF, self::HOOK_DRAIN);
    }
}
