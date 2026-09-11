<?php
/**
 * RecordManagedFilesCommand — CP pushes a list of ABSPATH-relative paths that
 * WPMgr itself manages (object-cache.php, advanced-cache.php, .htaccess, mu-plugin
 * loaders, the wp-config managed region, etc.) and the agent returns the current
 * md5_file() hash for each readable path so the CP can upsert site_managed_files
 * and suppress those paths from file-integrity false positives.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/record_managed_files
 *   Authorization: Bearer <Ed25519 JWT cmd="record_managed_files">
 *   Content-Type: application/json
 *   Body: {
 *     "paths": [
 *       "object-cache.php",
 *       "wp-content/advanced-cache.php",
 *       ".htaccess",
 *       "wp-content/mu-plugins/wpmgr-loader.php"
 *     ]
 *   }
 *   (Paths are ABSPATH-relative, forward-slash separated.)
 *
 * Response (200 OK):
 *   {
 *     "ok":      true,
 *     "files":   [ {"path":"object-cache.php","md5":"<32hex>"}, ... ],
 *     "missing": ["<absent-or-unreadable-or-rejected paths>", ...]
 *   }
 *
 * Security invariants:
 *   1. Path containment: every path is realpath-resolved; the result must be
 *      strictly inside realpath(ABSPATH). Paths that escape (traversal, symlink
 *      out of root, absolute /etc/… input) are placed in `missing`, never read.
 *   2. No symlink follow: symlinks pointing outside ABSPATH go to `missing`.
 *   3. md5 only — never returns file contents.
 *   4. Path cap: at most MAX_PATHS paths are processed; extras are skipped.
 *   5. Non-string / empty / NUL-containing entries are silently skipped.
 *   6. The entire command is wrapped in try/catch — a bad payload never fatals.
 *
 * Auth: Router's permission_callback enforces the Ed25519 + anti-replay JWT
 * contract (Connector::verifyCommand) before execute() is ever called.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Returns md5 hashes for CP-managed files, confined strictly to ABSPATH.
 */
final class RecordManagedFilesCommand implements CommandInterface
{
    /**
     * Maximum number of paths accepted in a single request.
     * Guards against CP-driven DoS (huge path lists hashing every file on disk).
     */
    private const MAX_PATHS = 500;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'record_managed_files';
    }

    /**
     * Effect: md5s the control-plane-supplied list of managed paths and returns the hashes. It writes none
     * of them.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Read;
    }

    /**
     * Repeatability: a read.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (Router enforced aud + cmd).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,files:list<array{path:string,md5:string}>,missing:list<string>}
     */
    public function execute(array $claims, array $params): array
    {
        /** @var list<array{path:string,md5:string}> $files */
        $files = [];
        /** @var list<string> $missing */
        $missing = [];

        try {
            // ------------------------------------------------------------------
            // 1. Validate the top-level payload.
            // ------------------------------------------------------------------

            if (!array_key_exists('paths', $params) || !is_array($params['paths'])) {
                return ['ok' => false, 'files' => [], 'missing' => []];
            }

            // An empty path list has nothing to resolve — return cleanly WITHOUT
            // touching ABSPATH. (Resolving ABSPATH first made the empty case
            // depend on ABSPATH being a real on-disk dir, which is order-dependent
            // under test/CLI and is irrelevant when there are no paths.)
            if ($params['paths'] === []) {
                return ['ok' => true, 'files' => [], 'missing' => []];
            }

            // ------------------------------------------------------------------
            // 2. Resolve ABSPATH (empty-base-path guard: bail if unresolvable).
            // ------------------------------------------------------------------

            $absPathRaw = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
            if ($absPathRaw === '') {
                return ['ok' => false, 'files' => [], 'missing' => []];
            }

            // Canonicalise via realpath so symlinked mount points (e.g. macOS
            // /tmp → /private/tmp) do not cause false containment mismatches.
            $absRoot = realpath($absPathRaw);
            if ($absRoot === false || $absRoot === '') {
                return ['ok' => false, 'files' => [], 'missing' => []];
            }
            $absRoot = str_replace('\\', '/', $absRoot);

            // The containment prefix we compare against (with trailing slash so
            // "ABSPATH/foo" prefix-matches but "ABSPATHfoo" does not).
            $containmentPrefix = $absRoot . '/';
            $prefixLen         = strlen($containmentPrefix);

            // ------------------------------------------------------------------
            // 3. Process each requested path.
            // ------------------------------------------------------------------

            $processed = 0;
            foreach ($params['paths'] as $rawPath) {
                // Cap: extras beyond MAX_PATHS are silently ignored (not added
                // to missing — the CP sent too many, we just truncate silently).
                if ($processed >= self::MAX_PATHS) {
                    break;
                }

                // Skip non-string or empty entries.
                if (!is_string($rawPath) || $rawPath === '') {
                    continue;
                }

                // Reject NUL bytes (path injection).
                if (strpos($rawPath, "\0") !== false) {
                    $missing[] = $rawPath;
                    continue;
                }

                // Normalise separators; strip leading slash so we always
                // treat the path as relative before joining.
                $relPath = str_replace('\\', '/', ltrim($rawPath, '/\\'));

                // Reject any path segment equal to '..' to block obvious
                // traversal before we even try realpath.
                $segments = explode('/', $relPath);
                $hasDotDot = false;
                foreach ($segments as $seg) {
                    if ($seg === '..') {
                        $hasDotDot = true;
                        break;
                    }
                }
                if ($hasDotDot) {
                    $missing[] = $rawPath;
                    continue;
                }

                // Join to produce the candidate absolute path.
                $candidate = $absRoot . '/' . $relPath;

                // ------------------------------------------------------------------
                // Path-containment check via realpath.
                //
                // realpath() resolves all symlinks and '..' components. If the
                // resolved path does not start with $containmentPrefix the file
                // either lives outside ABSPATH or a symlink pointed elsewhere.
                // In both cases we add it to `missing` — we never read it.
                // ------------------------------------------------------------------

                $real = realpath($candidate);
                if ($real === false) {
                    // File does not exist or is not readable — expected for
                    // absent managed paths; goes to missing.
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                $real = str_replace('\\', '/', $real);

                // Strict containment: realpath must start with absRoot + '/'.
                // This also rejects realpath($absRoot) itself (no file component).
                if (strncmp($real, $containmentPrefix, $prefixLen) !== 0) {
                    // Escapes ABSPATH (symlink escape or absolute path outside root).
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                // Refuse directories (a managed "file" path should be a file).
                if (is_dir($real)) {
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                // Refuse symlinks: even if the symlink itself is inside ABSPATH,
                // we only trust a regular file. A symlink pointing outside was
                // already caught by the containment check above; this catches
                // symlinks whose targets are also inside ABSPATH (e.g. a crafted
                // loop or redirect to a sensitive file).
                if (is_link($candidate)) {
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                // Readability check.
                if (!is_readable($real)) {
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                // Compute md5. md5_file() returns false on failure.
                $hash = @md5_file($real);
                if ($hash === false) {
                    $missing[] = $rawPath;
                    ++$processed;
                    continue;
                }

                // Return the CP-supplied relative path (normalised) so the CP
                // can match it against the paths it sent, not our internal form.
                $files[]   = ['path' => $rawPath, 'md5' => $hash];
                ++$processed;
            }
        } catch (\Throwable $e) {
            // A fatal in path processing must never crash the REST response.
            return ['ok' => false, 'files' => $files, 'missing' => $missing];
        }

        return ['ok' => true, 'files' => $files, 'missing' => $missing];
    }
}
