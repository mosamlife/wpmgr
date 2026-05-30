<?php
/**
 * ScanCommand (S3): malware / file-integrity scan via resumable DFS hash stream.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/scan
 *   Authorization: Bearer <Ed25519 JWT cmd="scan">
 *   Body: {
 *     "run_id":                  <string>,
 *     "kind":                    "core"|"files"|"full",
 *     "include_md5":             true,
 *     "time_budget_s":           12,
 *     "paths_limit":             4000,
 *     "batch_size":              512,
 *     "traversal_stack_max_size":100,
 *     "resume_cursor":           { "dir": <string>, "traversal_stack": [[name,offset],...], "folder_offset": <int> } | null
 *   }
 *
 * Response (200 OK):
 *   {
 *     "ok":           true,
 *     "run_id":       <string>,
 *     "kind":         "core"|"files"|"full",
 *     "status":       "partial"|"done",
 *     "files_scanned":<int>,
 *     "next_cursor":  { "dir": <string>, "traversal_stack": [[name,offset],...], "folder_offset": <int> } | null,
 *     "links":        [ <site-relative path>, ... ],
 *     "hashes":       [
 *       { "path": <site-relative fwd-slash>, "size": <int>, "md5": <32hex|"">, "mtime": <int>, "mode": <int>, "is_link": <bool> }
 *       | { "path": <...>, "error": "NOT_READABLE" }
 *       ...
 *     ]
 *   }
 *
 * Scan roots by kind:
 *   core  → ABSPATH, excluding the wp-content/ subtree (root *.php + wp-admin/ + wp-includes/)
 *   files → wp-content/ (excludes: wpmgr-snapshots, wpmgr-agent, cache, upgrade)
 *   full  → ABSPATH (agent scratch dirs excluded)
 *
 * The command is SYNCHRONOUS within a single HTTP call. No cron handoff.
 * The CP drives the resumption loop by re-sending with the returned cursor.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\FileScanner;

/**
 * Resumable file-system hash walker for the S3 security scan.
 */
final class ScanCommand implements CommandInterface
{
    /**
     * Directories excluded when scanning the "full" or "core" roots.
     * Agent scratch space that should never appear in integrity results.
     *
     * @var list<string>
     */
    private const EXCLUDE_FULL = ['wpmgr-snapshots', 'wpmgr-agent'];

    /**
     * Default parameter values matching the wire contract.
     */
    private const DEFAULTS = [
        'include_md5'              => true,
        'time_budget_s'            => 12,
        'paths_limit'              => 4000,
        'batch_size'               => 512,
        'traversal_stack_max_size' => 100,
    ];

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'scan';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (aud, cmd already enforced by Router).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        // ------------------------------------------------------------------
        // 1. Validate required parameters.
        // ------------------------------------------------------------------

        if (!array_key_exists('run_id', $params) || !is_string($params['run_id']) || $params['run_id'] === '') {
            return ['ok' => false, 'error' => 'missing required field: run_id'];
        }
        $runId = $params['run_id'];

        $allowedKinds = ['core', 'files', 'full'];
        if (!array_key_exists('kind', $params) || !in_array($params['kind'], $allowedKinds, true)) {
            return ['ok' => false, 'error' => 'kind must be one of: core, files, full'];
        }
        /** @var "core"|"files"|"full" $kind */
        $kind = (string) $params['kind'];

        // ------------------------------------------------------------------
        // 2. Coerce optional parameters (use wire-contract defaults when absent).
        // ------------------------------------------------------------------

        $includeMd5   = isset($params['include_md5'])   ? (bool) $params['include_md5']   : self::DEFAULTS['include_md5'];
        $timeBudgetS  = isset($params['time_budget_s'])  ? (float) $params['time_budget_s']  : (float) self::DEFAULTS['time_budget_s'];
        $pathsLimit   = isset($params['paths_limit'])   ? (int) $params['paths_limit']    : self::DEFAULTS['paths_limit'];
        $batchSize    = isset($params['batch_size'])    ? (int) $params['batch_size']     : self::DEFAULTS['batch_size'];
        $stackMax     = isset($params['traversal_stack_max_size'])
            ? (int) $params['traversal_stack_max_size']
            : self::DEFAULTS['traversal_stack_max_size'];

        // Sanity clamps: prevent absurd values from stalling the server.
        if ($timeBudgetS < 1.0) {
            $timeBudgetS = 1.0;
        }
        if ($timeBudgetS > 60.0) {
            $timeBudgetS = 60.0;
        }
        if ($pathsLimit < 0) {
            $pathsLimit = 0;
        }
        if ($batchSize < 1) {
            $batchSize = 1;
        }
        if ($stackMax < 1) {
            $stackMax = 1;
        }

        $resumeCursor = isset($params['resume_cursor']) && is_array($params['resume_cursor'])
            ? $params['resume_cursor']
            : null;

        // ------------------------------------------------------------------
        // 3. Resolve scan roots from kind.
        // ------------------------------------------------------------------

        $absPath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        if ($absPath === '') {
            return ['ok' => false, 'error' => 'ABSPATH not defined'];
        }

        $wpContentDir = defined('WP_CONTENT_DIR') ? rtrim((string) constant('WP_CONTENT_DIR'), '/\\') : '';

        [$absRoot, $relStartDir, $excludeDirs] = $this->resolveRoots($kind, $absPath, $wpContentDir);

        // ------------------------------------------------------------------
        // 4. Run the scanner.
        // ------------------------------------------------------------------

        $scanner = new FileScanner();

        try {
            $result = $scanner->scan(
                $absRoot,
                $relStartDir,
                $includeMd5,
                $timeBudgetS,
                $pathsLimit,
                $batchSize,
                $stackMax,
                $resumeCursor,
                $excludeDirs
            );
        } catch (\Throwable $e) {
            // A fatal in the scanner must never crash the REST response.
            return ['ok' => false, 'error' => 'scanner_error'];
        }

        // ------------------------------------------------------------------
        // 5. Build response.
        // ------------------------------------------------------------------

        $response = [
            'ok'           => true,
            'run_id'       => $runId,
            'kind'         => $kind,
            'status'       => $result['status'],
            'files_scanned'=> $result['files_scanned'],
            'links'        => $result['links'],
            'hashes'       => $result['hashes'],
        ];

        if ($result['status'] === 'partial') {
            $response['next_cursor'] = $result['next_cursor'];
        } else {
            $response['next_cursor'] = null;
        }

        return $response;
    }

    // ------------------------------------------------------------------
    // Private helpers
    // ------------------------------------------------------------------

    /**
     * Resolve absRoot, relStartDir, and excludeDirs for the given scan kind.
     *
     * Scan roots by kind:
     *   core  → ABSPATH root, exclude wp-content/ top-level dir + agent scratch
     *   files → wp-content/, exclude wpmgr-snapshots, wpmgr-agent, cache, upgrade
     *   full  → ABSPATH root, exclude only agent scratch dirs
     *
     * @param "core"|"files"|"full" $kind
     * @param string                $absPath      ABSPATH (no trailing slash).
     * @param string                $wpContentDir WP_CONTENT_DIR (no trailing slash, may be empty).
     *
     * @return array{0:string, 1:string, 2:list<string>}
     *   [absRoot, relStartDir, excludeTopDirs]
     */
    private function resolveRoots(string $kind, string $absPath, string $wpContentDir): array
    {
        switch ($kind) {
            case 'files':
                // Scan only wp-content/; BackupSource::EXCLUDE_DIRS apply.
                $root     = $wpContentDir !== '' ? $wpContentDir : $absPath . '/wp-content';
                $excludes = ['wpmgr-snapshots', 'wpmgr-agent', 'cache', 'upgrade'];
                return [$root, '', $excludes];

            case 'core':
                // Scan ABSPATH but exclude the entire wp-content/ subtree. We
                // derive the wp-content top-level dirname so it works even when
                // WP_CONTENT_DIR is customised.
                $wpContentTopDir = '';
                if ($wpContentDir !== '') {
                    // e.g. /var/www/html/wp-content → "wp-content"
                    $wpContentTopDir = basename($wpContentDir);
                }
                $excludes = self::EXCLUDE_FULL;
                if ($wpContentTopDir !== '' && !in_array($wpContentTopDir, $excludes, true)) {
                    $excludes[] = $wpContentTopDir;
                }
                return [$absPath, '', $excludes];

            case 'full':
            default:
                return [$absPath, '', self::EXCLUDE_FULL];
        }
    }
}
