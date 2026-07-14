<?php
/**
 * MediaRunStore — durable, chunked, idempotent background-run orchestration for
 * the bulk media commands (media_optimize / media_restore / media_delete_originals).
 *
 * THE PROBLEM IT SOLVES (scale bug):
 *   The media commands used to run the WHOLE batch synchronously inside the
 *   CP->agent REST request: for every job they enumerated sizes, called the CP
 *   /presign, PUT the source bytes to S3, then called /encode-ready. On a
 *   non-trivial batch (mega media libraries) that work blew past the control
 *   plane's HTTP client timeout — the CP cancelled the POST and marked EVERY
 *   job failed, even though the agent kept working and most jobs actually
 *   succeeded. Dashboard showed all-failed; reality was mostly-succeeded.
 *
 * THE FIX (mirrors the proven BackupCommand idiom, ADR-033):
 *   1. On receipt, the command persists the batch payload (jobs + endpoints +
 *      targets) into a transient keyed by a run id and returns
 *      {ok:true, detail:'accepted'} IMMEDIATELY — BEFORE any presign/upload.
 *   2. It schedules a wp_schedule_single_event(<hook>, [runId]) + @spawn_cron()
 *      so a SEPARATE FPM worker (fired by /wp-cron.php) does the heavy lifting.
 *      The REST request ACKs in milliseconds; the CP never times out.
 *   3. The background worker processes a BOUNDED CHUNK of jobs per run, persists
 *      the remaining jobs back to the transient, and reschedules itself until
 *      the batch is drained. A mega batch therefore can NEVER wedge a single
 *      request — each cron run does a few jobs then yields.
 *
 * This class is the storage + draining engine shared by all three commands so
 * the chunking/idempotency/cron logic lives in ONE audited place. Each command
 * supplies a callable that processes a single job; this class owns everything
 * else (persist, claim, chunk, reschedule, cleanup).
 *
 * Durability: a WordPress transient. Transients survive the request, are shared
 * across FPM workers (object cache or wp_options), and self-expire so a wedged
 * run can't leak storage forever. We do NOT add a DB table: the media path has
 * none today, and a transient is exactly the right durability/TTL shape for a
 * bounded background queue.
 *
 * Idempotency: a re-delivered command for an already-known run id will find the
 * transient already present and refuse to re-seed (claim() returns false), so a
 * CP retry of a lost ACK never double-processes. Within the worker, each job is
 * popped off the persisted queue before it runs and is never re-enqueued, so a
 * duplicate cron firing for the same run drains the SAME queue cooperatively
 * rather than re-running finished jobs.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\Support\LongRunningJob;

/**
 * Persist + drain a bulk media batch in bounded background chunks.
 */
final class MediaRunStore
{
    /**
     * Transient key prefix. The full key is `<prefix><runId>`. WordPress caps
     * option names at 191 chars on utf8mb4 installs; our runId is a 32-char hex
     * token so we stay well under.
     */
    private const KEY_PREFIX = 'wpmgr_media_run_';

    /**
     * Transient TTL. Long enough that a mega batch draining a few jobs per
     * visitor-fired cron tick has time to finish on a low-traffic site, short
     * enough that a permanently-wedged run self-cleans. 6 hours mirrors the
     * outer bound we allow other agent background work.
     */
    private const TTL_SECONDS = 6 * 3600;

    /**
     * Inner-batch size: jobs processed between persists of the remaining queue.
     * This is the CRASH-SAFETY granularity, NOT the per-run cap — one background
     * run loops over many of these batches until TIME_BUDGET_SECONDS is spent
     * (see drain()). A small value bounds how many in-flight jobs are left
     * un-reported if the worker dies mid-batch (the CP's per-job watchdog
     * reconciles those). Per-job memory is freed inside the callback (each
     * optimize job unsets its source bytes before the next), so the batch size
     * does NOT drive peak memory — the time budget bounds wall cost instead.
     */
    public const DEFAULT_CHUNK = 3;

    /**
     * Wall-clock budget for ONE background run. The drain loop keeps popping
     * inner batches until this elapses, then reschedules the remainder for the
     * next cron-fired worker.
     *
     * WHY THIS EXISTS: rescheduling after every tiny chunk is pathologically
     * slow because @spawn_cron() is gated by WordPress's `doing_cron` transient
     * lock (WP_CRON_LOCK_TIMEOUT, ~60s) — so the next chunk cannot fire for up to
     * a minute. With a 3-job chunk that capped throughput at ~3 jobs/MINUTE,
     * which is fine for a handful of heavy optimize uploads but absurd for fast
     * local restores / deletes of a mega library. Draining a whole time-slice
     * per run collapses the number of 60s lock gaps from N/3 to N/(jobs-per-slice).
     *
     * 20s stays comfortably under any sane request_terminate_timeout while
     * letting a single run clear ~100+ local restores or ~10-20 S3-bound
     * optimizes. A bounded set_time_limit() + ignore_user_abort(true) keep the worker
     * alive for the full slice.
     */
    private const TIME_BUDGET_SECONDS = 20.0;

    /**
     * Hard ceiling on background runs for one batch — a safety valve against an
     * infinite reschedule loop. Each run now drains a whole time-slice (many
     * jobs), so 1000 runs is an enormous amount of work; this is purely a
     * runaway backstop, never a real-world limit.
     */
    private const MAX_RUNS = 1000;

    /**
     * Seed a new run. Persists the payload under `runId` and returns true if WE
     * created it (won the claim). Returns false if a run with this id already
     * exists — the idempotency guard against a re-delivered command.
     *
     * @param string              $runId   Caller-generated unique run id (hex token).
     * @param array<string,mixed> $payload Arbitrary batch payload. MUST contain a
     *                                      'jobs' list; the rest (endpoints, targets)
     *                                      is opaque to this class and handed back to
     *                                      the per-job callback via the run array.
     * @return bool True when seeded (claim won); false when a run already exists.
     */
    public function claim(string $runId, array $payload): bool
    {
        if ($runId === '') {
            return false;
        }
        // Idempotency: if a run already exists for this id, a duplicate command
        // was delivered (CP retried a lost ACK). Refuse to re-seed so the
        // in-flight worker keeps draining the original queue undisturbed.
        if ($this->get($runId) !== null) {
            return false;
        }

        $payload['run_id']    = $runId;
        $payload['runs']      = 0;
        $payload['seeded_at'] = time();

        return $this->put($runId, $payload);
    }

    /**
     * Schedule the background worker for a run and kick wp-cron so a SEPARATE
     * FPM worker picks it up. Mirrors BackupCommand's handoff exactly:
     * wp_schedule_single_event(now) + @spawn_cron(). The REST request returns
     * its ACK immediately after calling this; the actual work runs in the
     * cron-fired worker.
     *
     * @param string $hook  The cron action name bound in Plugin::registerHooks.
     * @param string $runId The run id (sole event arg).
     * @return void
     */
    public function scheduleNext(string $hook, string $runId): void
    {
        if ($runId === '' || $hook === '') {
            return;
        }
        if (function_exists('wp_schedule_single_event')) {
            wp_schedule_single_event(time(), $hook, [$runId]);
        }
        if (function_exists('spawn_cron')) {
            @spawn_cron();
        }
    }

    /**
     * Drain a run for up to TIME_BUDGET_SECONDS, then reschedule the remainder.
     *
     * One background run loops over inner batches of `$chunk` jobs each. Before
     * processing each batch it persists the SHORTENED queue (crash-safety, see
     * below), runs `$processJob` for every job (an error in one NEVER aborts the
     * rest), and keeps going until either the queue empties or the wall-clock
     * budget is spent. Only THEN — if work remains — does it reschedule via
     * $hook. When the queue is empty the transient is deleted.
     *
     * This replaces the old one-chunk-per-run design, whose reschedule-after-3
     * pattern was throttled to ~3 jobs/minute by WordPress's `doing_cron` lock
     * (see TIME_BUDGET_SECONDS). Looping within a single run keeps the cheap,
     * fast path (a few local restores) from paying a 60s cron-lock tax per chunk.
     *
     * Crash-safety: the remaining queue is persisted BEFORE each batch is
     * processed, so if the worker dies mid-batch (FPM recycle, OOM) the next
     * cron tick resumes from the already-shortened queue — the in-flight batch's
     * jobs won't be re-attempted. Each job reports to the CP independently; a
     * dead worker simply leaves those few un-reported, which the CP's per-job
     * watchdog reconciles, rather than risking double-processing the library.
     *
     * The per-job callback receives ($job, $run) where $run is the full persisted
     * payload (so the callback can read endpoints/targets). Its return value is
     * ignored; it reports success/failure to the CP itself (encode-ready /
     * job-status / restore-status), exactly as the synchronous code did — the
     * wire contract is UNCHANGED.
     *
     * @param string                                                   $hook       Cron hook to reschedule under.
     * @param string                                                   $runId      Run id to drain.
     * @param int                                                      $chunk      Inner-batch (persist) size.
     * @param callable(array<string,mixed>,array<string,mixed>):void   $processJob Per-job worker.
     * @return void
     */
    public function drain(string $hook, string $runId, int $chunk, callable $processJob): void
    {
        $run = $this->get($runId);
        if ($run === null) {
            // Already drained + deleted, or never existed. Idempotent no-op —
            // a duplicate/stale cron firing lands here and returns cleanly.
            return;
        }

        // Safety valve: never let a pathological run reschedule forever.
        $runs = (int) ($run['runs'] ?? 0);
        if ($runs >= self::MAX_RUNS) {
            $this->delete($runId);
            \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr Media: run %s exceeded max background runs (%d); abandoning queue', $runId, self::MAX_RUNS));
            return;
        }

        $jobs = isset($run['jobs']) && is_array($run['jobs']) ? array_values($run['jobs']) : [];
        if ($jobs === []) {
            // Nothing left — terminal. Clean up the transient.
            $this->delete($runId);
            return;
        }

        // Lift PHP's per-request caps: this is a detached cron worker, not the
        // REST request, so it may run for the full time budget. ignore_user_abort
        // keeps it alive even if the loopback spawn_cron connection is dropped.
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running media background run must not hit max_execution_time; @-guarded, no-op when disabled
        }
        if (function_exists('ignore_user_abort')) {
            @ignore_user_abort(true);
        }

        $chunk    = $chunk > 0 ? $chunk : self::DEFAULT_CHUNK;
        $deadline = microtime(true) + self::TIME_BUDGET_SECONDS;

        // Drain inner batches until the queue empties or the time budget is spent.
        // `runs` increments ONCE per background run (not per inner batch): it
        // counts reschedules, and we reschedule at most once below.
        while ($jobs !== []) {
            $batch = array_splice($jobs, 0, $chunk);

            if ($jobs !== []) {
                // More work after this batch: persist the shortened queue first
                // (crash-safety) so a mid-batch death resumes from here.
                $remaining               = $run;
                $remaining['jobs']       = array_values($jobs);
                $remaining['runs']       = $runs + 1;
                $remaining['drained_at'] = time();
                $this->put($runId, $remaining);
            } else {
                // Final batch: delete the transient up front so a duplicate cron
                // firing finds nothing. We still process this batch from local state.
                $this->delete($runId);
            }

            foreach ($batch as $job) {
                if (!is_array($job)) {
                    continue;
                }
                try {
                    $processJob($job, $run);
                } catch (\Throwable $e) {
                    // A job that errors mid-batch MUST NOT abort the rest. Swallow,
                    // log without secrets, continue. The callback is responsible
                    // for reporting its own per-job failure to the CP.
                    \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr Media: run %s job error: %s', $runId, substr($e->getMessage(), 0, 200)));
                }
            }

            // Spent our wall-clock slice with work still queued? Hand the rest to
            // the next cron-fired worker. The shortened queue is already persisted
            // (above), so scheduleNext just kicks the worker.
            if ($jobs !== [] && microtime(true) >= $deadline) {
                $this->scheduleNext($hook, $runId);
                return;
            }
        }
    }

    /**
     * Generate a unique, URL-safe run id (32 hex chars). Used by the commands as
     * the transient key + the cron event arg.
     *
     * @return string
     */
    public static function newRunId(): string
    {
        try {
            return bin2hex(random_bytes(16));
        } catch (\Throwable $e) {
            // random_bytes can only fail if the platform has no CSPRNG, which on
            // a WP host is effectively never. Fall back to a uniqid-based token
            // so we still produce SOMETHING unique rather than throwing.
            return md5(uniqid('wpmgr_media', true));
        }
    }

    /**
     * Read a run payload, or null when absent/expired.
     *
     * @param string $runId
     * @return array<string,mixed>|null
     */
    public function get(string $runId): ?array
    {
        if ($runId === '' || !function_exists('get_transient')) {
            return null;
        }
        $value = get_transient(self::KEY_PREFIX . $runId);
        return is_array($value) ? $value : null;
    }

    /**
     * Persist a run payload under its TTL.
     *
     * @param string              $runId
     * @param array<string,mixed> $payload
     * @return bool
     */
    private function put(string $runId, array $payload): bool
    {
        if ($runId === '' || !function_exists('set_transient')) {
            return false;
        }
        return (bool) set_transient(self::KEY_PREFIX . $runId, $payload, self::TTL_SECONDS);
    }

    /**
     * Delete a run payload (terminal cleanup).
     *
     * @param string $runId
     * @return void
     */
    public function delete(string $runId): void
    {
        if ($runId === '' || !function_exists('delete_transient')) {
            return;
        }
        delete_transient(self::KEY_PREFIX . $runId);
    }
}
