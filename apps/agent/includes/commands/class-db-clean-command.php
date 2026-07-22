<?php
/**
 * DbCleanCommand — database cleanup (Phase 4, M38 async model).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/db_clean
 *   Authorization: Bearer <Ed25519 JWT, cmd="db_clean", aud=<siteId>>
 *   Body: {
 *     "job_id":             "<UUID v4, required — single-use dedup key>",
 *     "tasks":              ["<category_id>", ...],   // required; empty = all enabled
 *     "progress_endpoint":  "<full URL or empty string, optional>"
 *   }
 *
 * Response (ACK — synchronous):
 *   { "ok": true, "job_id": "<echoed uuid>" }
 *   { "ok": false, "detail": "<reason>" }          // on refusal
 *
 * Async model:
 *   execute() returns the ACK immediately via the REST HTTP response, then
 *   register_shutdown_function runs the actual cleanup AFTER the response is
 *   flushed, via the SAPI-aware ConnectionFinisher (fastcgi/litespeed/
 *   fallback — see ConnectionFinisher). On PHP-FPM or OpenLiteSpeed the
 *   client connection is closed while the worker process keeps running; on
 *   any other SAPI the connection stays open until the shutdown callback
 *   completes (acceptable — the CP client's timeout covers only the ACK
 *   round-trip).
 *
 *   After each category the agent POSTs one progress push to progress_endpoint
 *   (signed with its Ed25519 key, mirroring PerfReporter).  The final push has
 *   done=true.  If progress_endpoint is empty the agent runs silently.
 *
 * Auth: the Router's permission_callback already enforced the signed JWT +
 * anti-replay contract (Connector::verifyCommand) before execute() runs.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Optimizer\DbCleanup;
use WPMgr\Agent\Support\ConnectionFinisher;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Database cleanup command (M38 async + progress-push).
 */
final class DbCleanCommand implements CommandInterface
{
    /**
     * All 14 canonical category ids (matches DbCleanup::KNOWN_TASKS).
     * Anything received from the CP that is NOT in this list is silently ignored.
     *
     * @var list<string>
     */
    private const KNOWN_TASKS = [
        'revisions',
        'auto_drafts',
        'trashed_posts',
        'spam_comments',
        'trashed_comments',
        'expired_transients',
        'optimize_tables',
        'orphaned_postmeta',
        'orphaned_commentmeta',
        'orphaned_term_relationships',
        'oembed_cache',
        'duplicate_postmeta',
        'action_scheduler_completed',
        'action_scheduler_failed',
    ];

    /** Maximum body size for progress POSTs (mirrors maxStatsBody convention). */
    private const MAX_BODY = 16384; // 16 KiB

    /** Timeout in seconds for each progress POST. */
    private const PROGRESS_TIMEOUT = 5;

    private ?DbCleanup $cleanup;

    private ?Keystore $keystore;

    private ?Settings $settings;

    private ConnectionFinisher $finisher;

    /**
     * @param DbCleanup|null          $cleanup  Injected for tests; defaults to a live engine.
     * @param Keystore|null           $keystore Injected for tests; defaults to a fresh keystore.
     * @param Settings|null           $settings Injected for tests; defaults to a fresh settings.
     * @param ConnectionFinisher|null $finisher Injected for tests; defaults to a real ladder.
     */
    public function __construct(
        ?DbCleanup $cleanup = null,
        ?Keystore $keystore = null,
        ?Settings $settings = null,
        ?ConnectionFinisher $finisher = null
    ) {
        $this->cleanup  = $cleanup;
        $this->keystore = $keystore;
        $this->settings = $settings;
        $this->finisher = $finisher ?? new ConnectionFinisher();
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'db_clean';
    }

    /**
     * Validate the request, register the async shutdown worker, and return the
     * frozen db_clean_ack immediately.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here).
     * @param array<string,mixed> $params { job_id: string, tasks: string[],
     *                                      progress_endpoint?: string }
     * @return array{ok:bool,job_id?:string,detail?:string}
     */
    public function execute(array $claims, array $params): array
    {
        // --- Validate job_id (REQUIRED) --------------------------------------
        $jobId = isset($params['job_id']) && is_string($params['job_id']) && $params['job_id'] !== ''
            ? $params['job_id']
            : '';

        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        // --- Validate + sanitise tasks[] -------------------------------------
        $only = [];
        if (isset($params['tasks']) && is_array($params['tasks'])) {
            foreach ($params['tasks'] as $task) {
                if (is_string($task) && in_array($task, self::KNOWN_TASKS, true)) {
                    $only[] = $task;
                }
            }
        }

        // --- Progress endpoint (optional) ------------------------------------
        $progressEndpoint = '';
        if (isset($params['progress_endpoint']) && is_string($params['progress_endpoint'])) {
            $progressEndpoint = trim($params['progress_endpoint']);
        }

        // --- Build the cleanup engine ----------------------------------------
        $engine = $this->cleanup ?? new DbCleanup();

        // --- Register async shutdown worker ----------------------------------
        // Capture everything by value so the closure holds no cyclic references.
        $capturedJobId    = $jobId;
        $capturedOnly     = $only;
        $capturedEndpoint = $progressEndpoint;
        $capturedEngine   = $engine;
        $capturedKeystore = $this->keystore;
        $capturedSettings = $this->settings;
        $capturedFinisher = $this->finisher;

        register_shutdown_function(
            static function () use (
                $capturedJobId,
                $capturedOnly,
                $capturedEndpoint,
                $capturedEngine,
                $capturedKeystore,
                $capturedSettings,
                $capturedFinisher
            ): void {
                // Release the client connection (SAPI-aware: PHP-FPM,
                // OpenLiteSpeed — GH #274 — or a portable fallback) while
                // this worker process keeps running, so the CP's ACK read
                // completes before the (potentially long) cleanup loop starts.
                $capturedFinisher->finish();

                // Expand execution budget — cleanup can be slow on large sites.
                if (function_exists('set_time_limit')) {
                    @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running cleanup loop must not hit max_execution_time; @-guarded, no-op when disabled
                }

                self::runAsync(
                    $capturedJobId,
                    $capturedOnly,
                    $capturedEndpoint,
                    $capturedEngine,
                    $capturedKeystore,
                    $capturedSettings
                );
            }
        );

        // --- Return ACK synchronously ----------------------------------------
        return ['ok' => true, 'job_id' => $jobId];
    }

    // -------------------------------------------------------------------------
    // Async worker (static so it can run in the shutdown callback without $this)
    // -------------------------------------------------------------------------

    /**
     * Run each requested category sequentially and POST one progress push per
     * category to $progressEndpoint (if set).  The final push carries done=true.
     *
     * Robustness improvements (Phase 2):
     *   1. Heartbeat BEFORE each category so the CP/UI always knows the current
     *      category even if the process dies mid-run.
     *   2. Memory guards (unset + gc_collect_cycles) between categories to
     *      reduce OOM risk on large sites.
     *   3. Inner register_shutdown_function that best-effort POSTs a failure
     *      progress (state='error', done=true) when the process is killed by a
     *      fatal error or OOM before the job completes normally.
     *
     * @param string         $jobId            UUID echoed from the original request.
     * @param list<string>   $only             Task id allow-list (may be empty).
     * @param string         $progressEndpoint Full URL or empty string.
     * @param DbCleanup      $engine           The cleanup engine.
     * @param Keystore|null  $keystore         For signing progress POSTs.
     * @param Settings|null  $settings         For the CP base URL (fallback).
     * @return void
     */
    private static function runAsync(
        string $jobId,
        array $only,
        string $progressEndpoint,
        DbCleanup $engine,
        ?Keystore $keystore,
        ?Settings $settings
    ): void {
        // Determine which tasks will actually run (mirrors DbCleanup::run logic
        // for the task list so we know count upfront for the done=true detection).
        // We run them category-by-category so we can POST one push each.
        $taskList = $only !== [] ? $only : DbCleanup::KNOWN_TASKS;

        $total     = count($taskList);
        $processed = 0;

        // Robustness #3 — track the category currently executing so the inner
        // shutdown handler can name it in the failure push. $jobCompleted flips
        // to true at normal exit so the shutdown handler is a no-op on success.
        $currentCategory = '';
        $jobCompleted    = false;

        // Capture scalars (not objects) for the shutdown closure so it holds no
        // cyclic references and can survive a PHP OOM partial free.
        $capturedJobId    = $jobId;
        $capturedEndpoint = $progressEndpoint;
        $capturedKeystore = $keystore;

        register_shutdown_function(
            static function () use (
                &$currentCategory,
                &$jobCompleted,
                $capturedJobId,
                $capturedEndpoint,
                $capturedKeystore
            ): void {
                if ($jobCompleted) {
                    return; // Normal exit — no failure to report.
                }
                $err = error_get_last();
                if ($err === null || !in_array(
                    $err['type'],
                    [E_ERROR, E_PARSE, E_CORE_ERROR, E_COMPILE_ERROR],
                    true
                )) {
                    return; // Not a fatal error; let the outer watchdog handle it.
                }
                if ($capturedEndpoint === '') {
                    return;
                }
                // Best-effort failure push: state='error', done=true so the CP
                // emits db.clean.failed immediately (HandleDBCleanProgress already
                // handles done+error → EventDbCleanFailed). The CP watchdog
                // remains as a backstop for SIGKILL cases where shutdown handlers
                // cannot run at all.
                self::postProgress(
                    $capturedEndpoint,
                    $capturedJobId,
                    $currentCategory,
                    0,
                    0,
                    'error',
                    'process died: ' . ($err['message'] ?? 'unknown fatal'),
                    true, // done=true → CP marks job failed
                    $capturedKeystore
                );
            }
        );

        foreach ($taskList as $categoryId) {
            $processed++;
            $isLast = ($processed === $total);

            // Robustness #3 — update the in-flight category before entering the
            // runner so the shutdown handler names the right category on crash.
            $currentCategory = $categoryId;

            // Robustness #1 — post a "running" heartbeat BEFORE entering the
            // category runner. This guarantees the CP and UI always know which
            // category is executing, even if the category itself hangs or OOMs
            // before returning. A running+done=false push emits a db.clean.progress
            // SSE frame which the UI uses to advance the "current category" indicator.
            if ($progressEndpoint !== '') {
                self::postProgress(
                    $progressEndpoint,
                    $jobId,
                    $categoryId,
                    0,
                    0,
                    'running',
                    '',
                    false,
                    $keystore
                );
            }

            try {
                $results = $engine->run([$categoryId]);
                $result  = $results[$categoryId] ?? [
                    'rows_deleted' => 0,
                    'bytes_freed'  => 0,
                    'state'        => 'skipped',
                    'detail'       => 'not run',
                ];
            } catch (\Throwable $e) {
                $result = [
                    'rows_deleted' => 0,
                    'bytes_freed'  => 0,
                    'state'        => 'error',
                    'detail'       => $e->getMessage(),
                ];
            }

            if ($progressEndpoint !== '') {
                self::postProgress(
                    $progressEndpoint,
                    $jobId,
                    $categoryId,
                    (int) ($result['rows_deleted'] ?? 0),
                    (int) ($result['bytes_freed'] ?? 0),
                    (string) ($result['state'] ?? 'done'),
                    (string) ($result['detail'] ?? ''),
                    $isLast,
                    $keystore
                );
            }

            // Robustness #2 — release the engine's result memory between categories
            // to reduce peak RSS on large sites. gc_collect_cycles() reclaims any
            // circular references that PHP's reference-counting missed.
            unset($results, $result);
            if (function_exists('gc_collect_cycles')) {
                gc_collect_cycles();
            }

            // Warn in the error log when we are approaching the memory limit.
            $memLimit = self::memoryLimitBytes();
            if ($memLimit > 0 && memory_get_usage(true) > (int) ($memLimit * 0.80)) {
                \WPMgr\Agent\Support\DebugLog::write(sprintf(
                    '[wpmgr] db_clean job %s: memory at %.0f%% after category %s',
                    $jobId,
                    memory_get_usage(true) / $memLimit * 100,
                    $categoryId
                ));
            }
        }

        // Mark normal exit BEFORE the shutdown function fires (which happens
        // immediately after this function returns in a shutdown context).
        $jobCompleted = true;

        // If the task list was empty (should not happen with valid input but be
        // defensive), emit a terminal done=true push so the CP can close the job.
        if ($total === 0 && $progressEndpoint !== '') {
            self::postProgress(
                $progressEndpoint,
                $jobId,
                '',
                0,
                0,
                'skipped',
                'no tasks',
                true,
                $keystore
            );
        }
    }

    /**
     * Parse the PHP memory_limit ini value and return it as bytes.
     * Returns 0 when the limit is -1 (unlimited) or cannot be parsed.
     *
     * @return int
     */
    private static function memoryLimitBytes(): int
    {
        $val = ini_get('memory_limit');
        if (!is_string($val) || $val === '' || $val === '-1') {
            return 0;
        }
        $trimmed = trim($val);
        $unit    = strtolower(substr($trimmed, -1));
        $num     = (int) $trimmed;
        return match ($unit) {
            'g'     => $num * 1024 * 1024 * 1024,
            'm'     => $num * 1024 * 1024,
            'k'     => $num * 1024,
            default => $num,
        };
    }

    /**
     * POST one per-category progress push to the CP progress endpoint, signed
     * with the agent's Ed25519 key (same scheme as PerfReporter::post()).
     *
     * The method swallows ALL errors — a network hiccup or CP restart must not
     * halt the cleanup loop.
     *
     * @param string      $endpoint     Full URL, e.g. https://cp.example.com/agent/v1/db-clean/progress.
     * @param string      $jobId        UUID of this db_clean job.
     * @param string      $category     Category id, e.g. "revisions".
     * @param int         $rowsDeleted  Rows removed for this category.
     * @param int         $bytesFreed   Bytes reclaimed (0 when not applicable).
     * @param string      $state        "done" | "skipped" | "error".
     * @param string      $detail       Human reason for skipped/error (may be "").
     * @param bool        $done         true on the FINAL push for this job.
     * @param Keystore|null $keystore   Agent keystore for signing.
     * @return void
     */
    private static function postProgress(
        string $endpoint,
        string $jobId,
        string $category,
        int $rowsDeleted,
        int $bytesFreed,
        string $state,
        string $detail,
        bool $done,
        ?Keystore $keystore
    ): void {
        if (!function_exists('wp_json_encode') || !function_exists('wp_remote_post')) {
            return;
        }

        $payload = [
            'job_id'       => $jobId,
            'category'     => $category,
            'rows_deleted' => $rowsDeleted,
            'bytes_freed'  => $bytesFreed,
            'state'        => $state,
            'done'         => $done,
        ];
        if ($detail !== '') {
            $payload['detail'] = $detail;
        }

        $body = (string) wp_json_encode($payload);

        // Build signed headers. We need the PATH component only for signing.
        $headers = ['Content-Type' => 'application/json', 'Accept' => 'application/json'];

        if ($keystore !== null) {
            try {
                $parsed = wp_parse_url($endpoint);
                $path   = isset($parsed['path']) && is_string($parsed['path']) ? $parsed['path'] : '/agent/v1/db-clean/progress';
                if (isset($parsed['query']) && is_string($parsed['query']) && $parsed['query'] !== '') {
                    $path .= '?' . $parsed['query'];
                }
                $signer = new Signer($keystore);
                $auth   = $signer->signHeaders('POST', $path, $body);
                $headers = array_merge($headers, $auth);
            } catch (\Throwable $e) {
                // Fix 3: a signing failure means we CANNOT authenticate the request.
                // Sending an unsigned progress push would produce a CP 401 anyway, and
                // exposes the payload without authentication — skip the POST entirely
                // and surface the failure in the agent error log for diagnostics.
                \WPMgr\Agent\Support\DebugLog::write(sprintf(
                    '[wpmgr] db_clean progress signing failed for job %s category %s: %s',
                    $jobId,
                    $category,
                    $e->getMessage()
                ));
                return;
            }
        }

        // Truncate body defensively (MAX_BODY = 16 KiB — well within normal).
        if (strlen($body) > self::MAX_BODY) {
            // This should never happen for a single-category push; guard anyway.
            return;
        }

        try {
            wp_remote_post($endpoint, [
                'timeout'   => self::PROGRESS_TIMEOUT,
                'headers'   => $headers,
                'body'      => $body,
                'blocking'  => true, // we want to tolerate CP errors but need to sequentialise
                'sslverify' => true,
            ]);
        } catch (\Throwable $e) {
            // Swallow — a progress POST failure must not stop the cleanup loop.
        }
    }
}
