<?php
/**
 * DbScanCommand — read-only database scan (Phase 4, M39 synchronous model).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/db_scan
 *   Authorization: Bearer <Ed25519 JWT, cmd="db_scan", aud=<siteId>>
 *   Body: {
 *     "job_id":    "<UUID v4, required — single-use dedup key>",
 *     "categories": ["revisions", "auto_drafts", ...]  // optional; empty = all 14
 *   }
 *
 * Response (SYNCHRONOUS — full result in ACK body, no async push):
 *   {
 *     "ok":           true,
 *     "job_id":       "<echoed uuid>",
 *     "categories": {
 *       "revisions":    { "count": 1240, "bytes": 0 },
 *       ...
 *       "optimize_tables": {
 *         "count": 6,
 *         "bytes": 204800,
 *         "tables": [
 *           { "name": "wp_posts", "engine": "MyISAM",
 *             "data_length": 1048576, "data_free": 204800 }
 *         ]
 *       }
 *     },
 *     "db_size_bytes": 45088768,
 *     "table_count":   23,
 *     "scanned_at":    1748994000
 *   }
 *   { "ok": false, "detail": "<reason>" }   // on refusal
 *
 * Why synchronous:
 *   The scan is READ-ONLY (no deletes, no OPTIMIZE TABLE). It uses
 *   information_schema metadata + bounded COUNT queries so the total runtime on
 *   any realistic site is well under 5 seconds. A synchronous ACK eliminates the
 *   entire class of async-hang bugs that the Phase 1 db_clean run into
 *   (curvabykerline.in OOM/stall with no terminal event), and it means the CP
 *   gets the result (or a clean transport error) within the HTTP client timeout
 *   window — no watchdog stall window applies.
 *
 * Auth: the Router's permission_callback already enforced the signed JWT +
 * anti-replay contract (Connector::verifyCommand) before execute() runs.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Optimizer\DbCleanup;

/**
 * Read-only database scan command (M39 synchronous ACK with full result).
 */
final class DbScanCommand implements CommandInterface
{
    /**
     * All 14 canonical category ids. Anything outside this list is silently
     * ignored, mirroring the db_clean allow-list guard.
     *
     * @var list<string>
     */
    private const KNOWN_CATEGORIES = [
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

    private ?DbCleanup $cleanup;

    /**
     * @param DbCleanup|null $cleanup Injected for tests; defaults to a live engine.
     */
    public function __construct(?DbCleanup $cleanup = null)
    {
        $this->cleanup = $cleanup;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'db_scan';
    }

    /**
     * Run the read-only scan synchronously and return the full result in the ACK.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here).
     * @param array<string,mixed> $params {
     *   job_id:     string — required UUID v4 (single-use dedup key).
     *   categories: string[] — optional subset of the 14 canonical ids.
     * }
     * @return array<string,mixed> Full scan result or {ok:false, detail:string}.
     */
    public function execute(array $claims, array $params): array
    {
        // --- Validate job_id (REQUIRED) ----------------------------------------
        $jobId = isset($params['job_id']) && is_string($params['job_id']) && $params['job_id'] !== ''
            ? $params['job_id']
            : '';

        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        // --- Validate + sanitise categories[] -----------------------------------
        $categories = [];
        if (isset($params['categories']) && is_array($params['categories'])) {
            foreach ($params['categories'] as $cat) {
                if (is_string($cat) && in_array($cat, self::KNOWN_CATEGORIES, true)) {
                    $categories[] = $cat;
                }
            }
        }
        // Empty categories = scan all 14 (DbCleanup::scan honours []).

        // --- Run the scan synchronously -----------------------------------------
        $engine = $this->cleanup ?? new DbCleanup();

        try {
            $scanResult = $engine->scan($categories);
        } catch (\Throwable $e) {
            return [
                'ok'     => false,
                'job_id' => $jobId,
                'detail' => 'scan failed: ' . $e->getMessage(),
            ];
        }

        // --- Phase 3.3: installed-plugins snapshot + orphan enumeration ---------
        $installedPlugins = [];
        $orphanedOptions  = [];
        $orphanedCron     = [];

        try {
            $installedPlugins = $engine->buildInstalledPluginsSnapshot();
        } catch (\Throwable $e) {
            // Non-fatal: omit from ACK (omitempty on the Go side handles this).
            $installedPlugins = [];
        }

        try {
            $optResult        = $engine->scanOrphanedOptions($installedPlugins);
            $orphanedOptions  = $optResult['items'] ?? [];
        } catch (\Throwable $e) {
            $orphanedOptions = [];
        }

        try {
            $orphanedCron = $engine->scanOrphanedCron($installedPlugins);
        } catch (\Throwable $e) {
            $orphanedCron = [];
        }

        // --- Build the ACK body -------------------------------------------------
        // The wire shape for categories normalises each entry to the contract:
        //   { count: int, bytes: int[, capped: bool][, tables: [...]] }
        // capped and tables are omitted when not applicable (json_encode will
        // omit null fields; we strip the capped=false key for a clean wire).
        $wireCategories = [];
        foreach (($scanResult['categories'] ?? []) as $id => $data) {
            $entry = [
                'count' => (int) ($data['count'] ?? 0),
                'bytes' => (int) ($data['bytes'] ?? 0),
            ];
            if (!empty($data['capped'])) {
                $entry['capped'] = true;
            }
            if (isset($data['tables']) && is_array($data['tables']) && $data['tables'] !== []) {
                $entry['tables'] = $data['tables'];
            }
            $wireCategories[(string) $id] = $entry;
        }

        // Tables inventory — already classified at scan time; pass through as-is.
        $tables = isset($scanResult['tables']) && is_array($scanResult['tables'])
            ? $scanResult['tables']
            : [];

        $ack = [
            'ok'            => true,
            'job_id'        => $jobId,
            'detail'        => '',
            // Cast to object: an empty PHP array json_encodes as `[]`, which the
            // CP cannot unmarshal into its Go map[string]CategoryResult field
            // (categories is a keyed map on the wire; tables below is a slice
            // and stays an array).
            'categories'    => (object) $wireCategories,
            'db_size_bytes' => (int) ($scanResult['db_size_bytes'] ?? 0),
            'table_count'   => (int) ($scanResult['table_count'] ?? 0),
            'scanned_at'    => (int) ($scanResult['scanned_at'] ?? time()),
            'tables'        => $tables,
        ];

        // Phase 3.3: include orphan enumeration + snapshot only when non-empty
        // (omitempty on the Go struct; old CP versions that don't know these
        // fields will simply ignore them).
        if ($orphanedOptions !== []) {
            $ack['orphaned_options'] = $orphanedOptions;
        }
        if ($orphanedCron !== []) {
            $ack['orphaned_cron'] = $orphanedCron;
        }
        if ($installedPlugins !== []) {
            $ack['installed_plugins'] = $installedPlugins;
        }

        return $ack;
    }
}
