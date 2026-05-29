<?php
/**
 * UrlRewriter: P0 of ADR-036 — siteurl / home / content / upload URL rewrite
 * with serialization safety.
 *
 * Ports the battle-tested WPvivid algorithm (`replace_string_v2()` and
 * friends in `wpvivid-backuprestore/includes/new_backup/class-wpvivid-restore-db-2.php`).
 * The reason we mirror WPvivid line-by-line rather than rolling our own:
 *
 *   - PHP `serialize()` writes the byte length of every string as a prefix
 *     (`s:NN:"..."`). A naive `str_replace` on a serialized blob silently
 *     produces a value whose declared length no longer matches the actual
 *     string body, and `unserialize()` then returns `false` — silently
 *     bricking the operator's options table.
 *   - WordPress core, plugins, and themes routinely store URLs in MANY
 *     forms: raw, JSON-escaped (`https:\/\/`), URL-encoded
 *     (`https%3A%2F%2F`), scheme-relative (`//example.com`), and the
 *     JSON-escaped scheme-relative form (`\/\/example.com`). A rewriter
 *     that handles only the raw form misses the URLs hiding in JSON
 *     blobs (Elementor data, Gutenberg blocks, settings serialized as
 *     JSON inside a PHP-serialized array, etc).
 *
 * This class is the SAFETY-CRITICAL bottleneck for every cross-environment
 * restore (dev->prod, staging->prod, agency handoff). It is intentionally
 * pure-static and fully unit-testable in isolation — no $wpdb dependency.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/**
 * URL rewriter. Builds a `[$from, $to]` replacement pair per URL pair (raw +
 * JSON-escaped + URL-encoded + scheme-relative + scheme-relative JSON-escaped,
 * each in both http/https variants), walks PHP-serialized blobs recursively
 * to re-serialize with correct length prefixes, and exposes the skip-table /
 * skip-column denylists ported verbatim from WPvivid.
 */
final class UrlRewriter
{
    /**
     * Tables whose contents are NEVER URL-rewritten — analytics / log /
     * stats / firewall / cache tables where rewriting would silently corrupt
     * binary blobs, hit counters, or cross-table references that aren't URLs.
     * Ported verbatim from WPvivid `skip_tables()` (class-wpvivid-restore-db-2.php
     * lines 2993-3054). Names are bare (no prefix); checked against the
     * portion of the table name AFTER the new prefix.
     *
     * @var list<string>
     */
    public const DENYLIST_TABLES = [
        'adrotate_stats',
        'login_security_solution_fail',
        'icl_strings',
        'icl_string_positions',
        'icl_string_translations',
        'icl_languages_translations',
        'slim_stats',
        'slim_stats_archive',
        'es_online',
        'ahm_download_stats',
        'woocommerce_order_items',
        'woocommerce_sessions',
        'redirection_404',
        'redirection_logs',
        'wbz404_logs',
        'wbz404_redirects',
        'Counterize',
        'Counterize_UserAgents',
        'Counterize_Referers',
        'et_bloom_stats',
        'term_relationships',
        'lbakut_activity_log',
        'simple_feed_stats',
        'svisitor_stat',
        'itsec_log',
        'relevanssi_log',
        'wysija_email_user_stat',
        'wponlinebackup_generations',
        'blc_instances',
        'wp_rp_tags',
        'statpress',
        'wfHits',
        'wp_wfFileMods',
        'tts_trafficstats',
        'tts_referrer_stats',
        'dmsguestbook',
        'relevanssi',
        'wfFileMods',
        'learnpress_sessions',
        'icl_string_pages',
        'webarx_event_log',
        'duplicator_packages',
        'wsal_metadata',
        'wsal_occurrences',
    ];

    /**
     * Column types whose contents are NEVER URL-rewritten — binary blobs
     * where any byte substitution risks producing invalid binary data
     * (image bytes, encrypted blobs, etc).
     *
     * @var list<string>
     */
    private const SKIP_COLUMN_TYPES = ['blob', 'mediumblob', 'longblob', 'tinyblob', 'binary', 'varbinary'];

    /**
     * Build the parallel `[$from, $to]` arrays for a single restore's URL
     * rewrite. Mirrors WPvivid `replace_string_v2()` (2792-2991) line-by-line,
     * EXCEPT: where WPvivid switches behaviour based on the table currently
     * being rewritten (posts/postmeta/options vs others), we emit the SUPERSET
     * — every variant for every URL pair. Why: the agent calls this once per
     * restore and feeds the same replacement set into every table; the cost
     * of a longer `$from`/`$to` is one extra `str_replace` pass per row, which
     * is dwarfed by the unserialize/serialize overhead.
     *
     * For each URL pair (siteUrl, homeUrl, contentUrl, uploadUrl) we emit, in
     * order:
     *   1. Raw                                "https://old"        -> "https://new"
     *   2. JSON-escaped                        "https:\/\/old"      -> "https:\/\/new"
     *   3. URL-encoded                         "https%3A%2F%2Fold"  -> "https%3A%2F%2Fnew"
     *   4. Scheme-relative                     "//old"              -> "//new"
     *   5. Scheme-relative JSON-escaped        "\/\/old"            -> "\/\/new"
     *   6. Cross-scheme raw (http<->https)     "http://old"         -> "https://new"
     *   7. Cross-scheme JSON-escaped           "http:\/\/old"       -> "https:\/\/new"
     *
     * 7 variants per URL pair × 4 URL pairs = up to 28 entries. Empty pairs
     * (old == new) and trivially-empty inputs are skipped.
     *
     * @param string $oldSiteUrl    The siteurl recorded at backup time.
     * @param string $newSiteUrl    The siteurl of the live target environment.
     * @param string $oldHomeUrl    The home_url recorded at backup time.
     * @param string $newHomeUrl    The home_url of the live target environment.
     * @param string $oldContentUrl WP_CONTENT_URL at backup time.
     * @param string $newContentUrl WP_CONTENT_URL at the live target.
     * @param string $oldUploadUrl  wp_upload_dir()['baseurl'] at backup time.
     * @param string $newUploadUrl  wp_upload_dir()['baseurl'] at the live target.
     * @return array{0: list<string>, 1: list<string>} [$from, $to] — parallel.
     */
    public static function build_replacements(
        string $oldSiteUrl,
        string $newSiteUrl,
        string $oldHomeUrl,
        string $newHomeUrl,
        string $oldContentUrl,
        string $newContentUrl,
        string $oldUploadUrl,
        string $newUploadUrl
    ): array {
        $from = [];
        $to   = [];

        $pairs = [
            [$oldSiteUrl, $newSiteUrl],
            [$oldHomeUrl, $newHomeUrl],
            [$oldContentUrl, $newContentUrl],
            [$oldUploadUrl, $newUploadUrl],
        ];

        $seen = [];
        foreach ($pairs as $pair) {
            [$old, $new] = $pair;
            $old = self::untrailingslashit($old);
            $new = self::untrailingslashit($new);
            if ($old === '' || $new === '' || $old === $new) {
                continue;
            }
            $key = $old . "\0" . $new;
            if (isset($seen[$key])) {
                continue;
            }
            $seen[$key] = true;
            self::appendVariants($from, $to, $old, $new);
        }

        return [$from, $to];
    }

    /**
     * Mirror of WPvivid `replace_row_data()` (2661-2681). Tries `unserialize`
     * with `allowed_classes => false` (so a hostile blob can't materialize an
     * arbitrary class on the agent); on success, walks the structure
     * recursively and re-`serialize()`s. On failure (the most common case —
     * the value is NOT a serialized blob), does a flat `str_replace`.
     *
     * The `@` suppresses the deprecation notice PHP 8.x emits when
     * unserialize gets non-serialized input (very common — most string
     * columns are NOT serialized).
     *
     * @param string                       $oldData      The raw column value.
     * @param array{0: list<string>, 1: list<string>} $replacements Output of `build_replacements`.
     * @return string Rewritten value (same shape as input).
     */
    public static function rewrite_row_data(string $oldData, array $replacements): string
    {
        [$from, $to] = $replacements;
        if ($from === []) {
            return $oldData;
        }

        // Cheap fast path: if no $from substring appears anywhere in the
        // value, we'd produce identical output and re-serializing the value
        // is pure overhead. This is the dominant path in practice — most
        // rows in most tables have nothing to rewrite.
        $needsWork = false;
        foreach ($from as $f) {
            if ($f !== '' && strpos($oldData, $f) !== false) {
                $needsWork = true;
                break;
            }
        }
        if (!$needsWork) {
            return $oldData;
        }

        try {
            $unserialized = @unserialize($oldData, ['allowed_classes' => false]);
            if ($unserialized === false && $oldData !== 'b:0;') {
                // Not a serialized blob (or a hostile one with disallowed
                // classes that failed under allowed_classes=false). Either
                // way: flat str_replace is the right answer.
                return str_replace($from, $to, $oldData);
            }
            $rewritten = self::rewrite_serialize_data($unserialized, $replacements);
            return serialize($rewritten);
        } catch (\Throwable $e) {
            // `unserialize` can also raise an Error on malformed input under
            // some PHP builds; fall back to the flat str_replace so a single
            // corrupt row doesn't crash the whole restore.
            return str_replace($from, $to, $oldData);
        }
    }

    /**
     * Mirror of WPvivid `replace_serialize_data()` (2683-2755). Recursively
     * walks arrays/objects, only rewrites string leaves (so e.g. an int
     * column-count or float coordinate is never mangled), skips object keys
     * that begin with NUL (PHP's encoding for protected/private property
     * names — those have type prefixes baked in and rewriting them produces
     * unserialize-fail garbage), and skips `__PHP_Incomplete_Class` entirely
     * (no safe way to mutate one without its class definition).
     *
     * For each string leaf we ALSO try unserialize (one level deeper) — many
     * options are serialized arrays whose values are themselves serialized
     * strings (the "double-encoded" pattern). WPvivid does the same and it's
     * the only way to catch nested cases without producing length-prefix
     * corruption.
     *
     * @param mixed                                  $data         Any unserialized value.
     * @param array{0: list<string>, 1: list<string>} $replacements Output of `build_replacements`.
     * @return mixed Same shape, with string leaves rewritten.
     */
    public static function rewrite_serialize_data($data, array $replacements)
    {
        [$from, $to] = $replacements;

        if (is_string($data)) {
            // String leaf — might be a serialized blob, might be a plain
            // string. Try unserialize one level deeper; on failure, flat
            // str_replace. Mirrors WPvivid lines 2685-2696.
            $inner = @unserialize($data, ['allowed_classes' => false]);
            if ($inner === false && $data !== 'b:0;') {
                return str_replace($from, $to, $data);
            }
            return serialize(self::rewrite_serialize_data($inner, $replacements));
        }

        if (is_array($data)) {
            foreach ($data as $key => $value) {
                if (is_string($value)) {
                    $data[$key] = self::rewriteScalarString($value, $replacements);
                } elseif (is_array($value) || is_object($value)) {
                    $data[$key] = self::rewrite_serialize_data($value, $replacements);
                }
            }
            return $data;
        }

        if (is_object($data)) {
            // __PHP_Incomplete_Class can't be mutated safely — its class
            // definition isn't loaded so we can't reconstruct property
            // visibility on re-serialize. WPvivid skips these (2725-2728).
            if ($data instanceof \__PHP_Incomplete_Class) {
                return $data;
            }
            $props = get_object_vars($data);
            foreach ($props as $key => $value) {
                // Protected/private property names are encoded with a leading
                // NUL byte (\0*\0name for protected, \0Class\0name for
                // private). Rewriting them through the public set would
                // produce an unserialize-fail blob. Skip — operators
                // restoring legacy plugin data with private props on
                // options have to handle those manually. WPvivid line 2734.
                if (strpos($key, "\0") === 0) {
                    continue;
                }
                if (is_string($value)) {
                    $data->$key = self::rewriteScalarString($value, $replacements);
                } elseif (is_array($value) || is_object($value)) {
                    $data->$key = self::rewrite_serialize_data($value, $replacements);
                }
            }
            return $data;
        }

        // Scalar non-string (int/float/bool/null) — return as-is.
        return $data;
    }

    /**
     * Whether the given table should be excluded from URL rewriting based on
     * the DENYLIST_TABLES denylist. The check compares the table's bare name
     * (the part after `$prefix`) against the denylist.
     *
     * @param string $table  Full table name (e.g. `wp_wfHits`).
     * @param string $prefix Active prefix (e.g. `wp_` or `tmpAB12_`).
     */
    public static function should_skip_table(string $table, string $prefix): bool
    {
        if ($prefix !== '' && strpos($table, $prefix) === 0) {
            $bare = substr($table, strlen($prefix));
        } else {
            $bare = $table;
        }
        return in_array($bare, self::DENYLIST_TABLES, true);
    }

    /**
     * Whether the given (table, column) pair should be excluded from URL
     * rewriting:
     *   - posts.guid: WP relies on guid being an immutable identifier; the
     *     codex explicitly warns against rewriting it. Ported from WPvivid
     *     `skip_rows()` (3056-3072).
     *   - any blob/binary column type: silent binary corruption otherwise.
     *
     * @param string $table      Full table name.
     * @param string $column     Column name.
     * @param string $prefix     Active prefix (used to compare the bare table name).
     * @param string $columnType Column SQL type slug (lower-cased, e.g. "longblob", "varchar(255)").
     */
    public static function should_skip_column(string $table, string $column, string $prefix, string $columnType): bool
    {
        if ($prefix !== '' && strpos($table, $prefix) === 0) {
            $bare = substr($table, strlen($prefix));
        } else {
            $bare = $table;
        }
        if ($bare === 'posts' && $column === 'guid') {
            return true;
        }
        $columnType = strtolower($columnType);
        foreach (self::SKIP_COLUMN_TYPES as $skipType) {
            // Exact slug or e.g. "blob" inside "mediumblob(N)" — anchor at
            // either start or "($digits)" boundary to avoid matching
            // "varchar" patterns by accident.
            if ($columnType === $skipType || strpos($columnType, $skipType . '(') === 0) {
                return true;
            }
        }
        return false;
    }

    /**
     * Append the 7-variant replacement set for ONE old/new URL pair into the
     * parallel `$from` / `$to` accumulators. Both URLs are pre-trimmed of any
     * trailing slash by the caller.
     *
     * @param list<string> $from
     * @param list<string> $to
     */
    private static function appendVariants(array &$from, array &$to, string $old, string $new): void
    {
        // 1. Raw
        $from[] = $old;
        $to[]   = $new;

        // 2. JSON-escaped (forward slashes escaped as \/)
        $from[] = self::jsonEscape($old);
        $to[]   = self::jsonEscape($new);

        // 3. URL-encoded (rawurlencode-style: : -> %3A, / -> %2F)
        $from[] = self::urlEncodeSchemeAndSlashes($old);
        $to[]   = self::urlEncodeSchemeAndSlashes($new);

        // 4. Scheme-relative
        $oldRel = self::stripScheme($old);
        $newRel = self::stripScheme($new);
        if ($oldRel !== null && $newRel !== null) {
            $from[] = '//' . $oldRel;
            $to[]   = '//' . $newRel;
            // 5. Scheme-relative JSON-escaped
            $from[] = '\/\/' . $oldRel;
            $to[]   = '\/\/' . $newRel;
        }

        // 6. Cross-scheme raw. If old was https and new is http, also emit
        //    http://old -> http://new (and the converse). This handles the
        //    common case of a content row that hard-coded a specific scheme
        //    against an origin whose canonical scheme has since flipped.
        $oldScheme = self::detectScheme($old);
        $newScheme = self::detectScheme($new);
        if ($oldScheme !== null && $newScheme !== null && $oldRel !== null && $newRel !== null) {
            // Always emit the OPPOSITE-scheme rewrite of $old as well.
            $oppositeScheme = ($oldScheme === 'https') ? 'http' : 'https';
            $from[] = $oppositeScheme . '://' . $oldRel;
            $to[]   = $newScheme . '://' . $newRel;
            // 7. Cross-scheme JSON-escaped variant.
            $from[] = $oppositeScheme . ':\/\/' . $oldRel;
            $to[]   = $newScheme . ':\/\/' . $newRel;
        }
    }

    /**
     * Per-leaf string rewrite. Encapsulates the "try unserialize then walk;
     * fall back to flat str_replace" decision for ONE string value found
     * during array/object recursion. Returning the rewritten string (or the
     * input unchanged if no `$from` substring is present).
     *
     * @param array{0: list<string>, 1: list<string>} $replacements
     */
    private static function rewriteScalarString(string $value, array $replacements): string
    {
        [$from, $to] = $replacements;
        if ($value === '' || $from === []) {
            return $value;
        }
        // Fast-path: no needle present? Don't bother with unserialize.
        $needsWork = false;
        foreach ($from as $f) {
            if ($f !== '' && strpos($value, $f) !== false) {
                $needsWork = true;
                break;
            }
        }
        if (!$needsWork) {
            return $value;
        }

        $inner = @unserialize($value, ['allowed_classes' => false]);
        if ($inner === false && $value !== 'b:0;') {
            return str_replace($from, $to, $value);
        }
        return serialize(self::rewrite_serialize_data($inner, $replacements));
    }

    /**
     * Forward-slashes to `\/` (mirrors the JSON-escape WordPress emits for
     * URLs in JSON-encoded option values, REST API payloads, etc).
     */
    private static function jsonEscape(string $url): string
    {
        return str_replace('/', '\/', $url);
    }

    /**
     * `https://example.com` -> `https%3A%2F%2Fexample.com`. Matches the
     * encoding WPvivid produces (2889-2893) so the URL-encoded form found
     * in option values (e.g. URLs inside JSON-encoded REST payloads stored
     * in `wp_options`) is also rewritten.
     */
    private static function urlEncodeSchemeAndSlashes(string $url): string
    {
        $url = str_replace(':', '%3A', $url);
        $url = str_replace('/', '%2F', $url);
        return $url;
    }

    /**
     * Strip the URL scheme + "://" prefix; null if the URL has neither.
     */
    private static function stripScheme(string $url): ?string
    {
        if (stripos($url, 'https://') === 0) {
            return substr($url, 8);
        }
        if (stripos($url, 'http://') === 0) {
            return substr($url, 7);
        }
        return null;
    }

    /**
     * Detect a URL's scheme ('http' or 'https'); null if neither.
     */
    private static function detectScheme(string $url): ?string
    {
        if (stripos($url, 'https://') === 0) {
            return 'https';
        }
        if (stripos($url, 'http://') === 0) {
            return 'http';
        }
        return null;
    }

    /**
     * Mirror of WordPress's `untrailingslashit()` — strip ONE trailing
     * forward-slash. Defensive against operators / agent code passing URLs
     * with or without trailing slashes.
     */
    private static function untrailingslashit(string $url): string
    {
        return rtrim($url, '/');
    }
}
