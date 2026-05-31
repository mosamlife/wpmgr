<?php
/**
 * DbRewriter: the post_content + postmeta URL rewriter — the single
 * highest-risk surface in the Media Optimizer.
 *
 * Ports FlyingPress §6 behavior-for-behavior (NOT line-for-line):
 * reference/flying-press-patterns.md §6 + reference/flying-press/src/ImageOptimizer.php:
 *   - replace_images_in_post_content() : :291-338
 *   - replace_images_in_postmeta()     : :340-405
 *   - recursive_replace()              : :413-437
 *
 * Non-negotiables this class implements (patterns doc §6 "non-negotiables"):
 *   1. Trailing boundary lookahead `(?=([^0-9A-Za-z]|$))` on EVERY URL pattern
 *      (and the SQL REGEXP) so `banner.jpg` is NOT rewritten inside
 *      `banner.jpg.bak` / `banner.jpg2` (ImageOptimizer.php:320,355,417).
 *   2. (De)serialize ROUND-TRIPS for serialized PHP so `s:NN:` length prefixes
 *      stay valid after a string length change — a plain str_replace on
 *      serialized data corrupts it (ImageOptimizer.php:378-380). We unserialize
 *      with allowed_classes=false (no hostile class materialization), recurse,
 *      then re-serialize.
 *   3. JSON-aware decode/encode for Elementor / Beaver / Gutenberg JSON-in-meta
 *      (ImageOptimizer.php:382-386).
 *   4. The skip-list of core/optimizer-owned meta keys — never string-rewrite
 *      the canonical attachment records or the restore anchor
 *      (ImageOptimizer.php:367).
 *   5. Bounded LIMIT per pass (100 posts / 200 meta) with a "needs more" flag so
 *      a huge site rewrites incrementally (ImageOptimizer.php:313,368).
 *   6. wp_suspend_cache_addition(true) around the writes so the rewrite of a
 *      large object set doesn't balloon the object cache.
 *
 * `replace_images($map)` rewrites old_url => new_url; the reverse direction
 * (restore) is just the same call with the map flipped, so the agent calls
 * replace_images(array_flip($map)).
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\MediaKeystore;

/**
 * Serialized/JSON-safe, boundary-guarded URL rewriter over posts + postmeta.
 */
final class DbRewriter
{
    /**
     * Meta keys that are NEVER string-rewritten: WordPress core's canonical
     * attachment records + our own optimization blob (the restore anchor).
     * Mirrors FlyingPress's skip-list (ImageOptimizer.php:367) with our blob key
     * substituted for theirs.
     *
     * @var list<string>
     */
    private const SKIP_META_KEYS = [
        '_wp_attached_file',
        '_wp_attachment_metadata',
        MediaKeystore::KEY,
        '_wp_attachment_backup_sizes',
    ];

    /** Per-pass post_content row cap (FlyingPress ImageOptimizer.php:313). */
    private const POST_LIMIT = 100;

    /** Per-pass postmeta row cap (FlyingPress ImageOptimizer.php:368). */
    private const META_LIMIT = 200;

    /**
     * Rewrite every occurrence of each old URL to its new URL across
     * post_content and postmeta, serialized/JSON-safe and boundary-guarded.
     *
     * @param array<string,string> $map old_url => new_url (non-empty keys/values).
     * @return array{post_content_rows:int,postmeta_rows:int,needs_more:bool}
     *         Affected row counts + whether another pass is needed (a full page
     *         of either limit was returned).
     */
    public function replaceImages(array $map): array
    {
        $map = $this->cleanMap($map);
        if ($map === []) {
            return ['post_content_rows' => 0, 'postmeta_rows' => 0, 'needs_more' => false];
        }

        $suspend = function_exists('wp_suspend_cache_addition');
        if ($suspend) {
            wp_suspend_cache_addition(true);
        }

        try {
            $content = $this->replaceInPostContent($map);
            $meta    = $this->replaceInPostmeta($map);
        } finally {
            if ($suspend) {
                wp_suspend_cache_addition(false);
            }
        }

        return [
            'post_content_rows' => $content['rows'],
            'postmeta_rows'     => $meta['rows'],
            'needs_more'        => $content['full'] || $meta['full'],
        ];
    }

    /**
     * Convenience: rewrite in REVERSE (restore). Flips the map and rewrites.
     *
     * @param array<string,string> $map old_url => new_url (the forward map).
     * @return array{post_content_rows:int,postmeta_rows:int,needs_more:bool}
     */
    public function reverseImages(array $map): array
    {
        $flipped = [];
        foreach ($this->cleanMap($map) as $from => $to) {
            $flipped[$to] = $from;
        }

        return $this->replaceImages($flipped);
    }

    /**
     * Recursively rewrite string leaves of an unserialized/decoded structure
     * with the boundary-guarded patterns. Public so it is unit-testable in
     * isolation against page-builder fixtures. Mirrors FlyingPress
     * recursive_replace() (ImageOptimizer.php:413-437).
     *
     * @param mixed                $data     Any value (string / array / object).
     * @param array<string,string> $map      old_url => new_url.
     * @param list<string>         $patterns Pre-built regex patterns (parallel to map keys).
     * @return mixed Same shape with string leaves rewritten.
     */
    public function recursiveReplace($data, array $map, array $patterns)
    {
        $replacements = array_values($map);

        if (is_string($data)) {
            $out = preg_replace($patterns, $replacements, $data);

            return is_string($out) ? $out : $data;
        }

        if (is_array($data)) {
            foreach ($data as $key => $value) {
                $data[$key] = $this->recursiveReplace($value, $map, $patterns);
            }

            return $data;
        }

        if (is_object($data)) {
            // __PHP_Incomplete_Class can't be mutated safely (no class def).
            if ($data instanceof \__PHP_Incomplete_Class) {
                return $data;
            }
            foreach (get_object_vars($data) as $key => $value) {
                // Skip NUL-prefixed protected/private property names — rewriting
                // them through the public set corrupts the re-serialized blob.
                if (is_string($key) && strpos($key, "\0") === 0) {
                    continue;
                }
                $data->$key = $this->recursiveReplace($value, $map, $patterns);
            }

            return $data;
        }

        return $data;
    }

    /**
     * Rewrite a single meta_value string with full serialized/JSON awareness.
     * Public + pure (no DB) so the SAME logic is unit-tested against an
     * Elementor-style serialized fixture. Mirrors FlyingPress
     * replace_images_in_postmeta()'s per-row branch (ImageOptimizer.php:378-387).
     *
     * @param string               $value The raw meta_value.
     * @param array<string,string> $map   old_url => new_url.
     * @return string The rewritten value (unchanged if nothing matched).
     */
    public function rewriteValue(string $value, array $map): string
    {
        $map = $this->cleanMap($map);
        if ($map === [] || $value === '') {
            return $value;
        }

        $patterns = $this->buildPatterns(array_keys($map));

        // Serialized PHP: unserialize -> recurse -> re-serialize so the s:NN:
        // length prefixes stay correct (ImageOptimizer.php:378-380).
        if ($this->isSerialized($value)) {
            $data = @unserialize($value, ['allowed_classes' => false]);
            if ($data === false && $value !== 'b:0;') {
                // Claimed serialized but failed under allowed_classes=false:
                // fall back to a flat (boundary-guarded) string rewrite.
                $out = preg_replace($patterns, array_values($map), $value);

                return is_string($out) ? $out : $value;
            }
            $rewritten = $this->recursiveReplace($data, $map, $patterns);

            return serialize($rewritten);
        }

        // JSON-in-postmeta (Elementor / Beaver / Gutenberg): decode -> recurse
        // -> encode (ImageOptimizer.php:382-385).
        $decoded = json_decode($value, true);
        if (json_last_error() === JSON_ERROR_NONE && is_array($decoded)) {
            $rewritten = $this->recursiveReplace($decoded, $map, $patterns);
            $encoded   = function_exists('wp_json_encode') ? wp_json_encode($rewritten) : json_encode($rewritten);

            return is_string($encoded) ? $encoded : $value;
        }

        // Plain string fallback — still boundary-guarded (ImageOptimizer.php:386
        // uses str_replace, but we keep the boundary lookahead for safety).
        $out = preg_replace($patterns, array_values($map), $value);

        return is_string($out) ? $out : $value;
    }

    // ------------------------------------------------------------------
    // post_content
    // ------------------------------------------------------------------

    /**
     * @param array<string,string> $map
     * @return array{rows:int,full:bool}
     */
    private function replaceInPostContent(array $map): array
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return ['rows' => 0, 'full' => false];
        }

        $likes = [];
        foreach (array_keys($map) as $url) {
            // Quoted LIKE biases toward attribute/JSON occurrences
            // (ImageOptimizer.php:300-301).
            $likes[] = $wpdb->prepare('post_content LIKE %s', '%"' . $wpdb->esc_like($url) . '"%');
        }

        $postTypes = $this->publicPostTypes();
        if ($postTypes === []) {
            return ['rows' => 0, 'full' => false];
        }
        $placeholders = implode(',', array_fill(0, count($postTypes), '%s'));
        $postsTable   = $wpdb->posts;

        // @phpstan-ignore-next-line — wpdb runtime seam; $postsTable + LIMIT const are trusted.
        $sql = $wpdb->prepare(
            'SELECT ID, post_content FROM ' . $postsTable
            . ' WHERE (' . implode(' OR ', $likes) . ')'
            . " AND post_type IN ($placeholders) AND post_status = 'publish'"
            . ' ORDER BY post_date DESC LIMIT ' . self::POST_LIMIT,
            ...$postTypes
        );
        if (!is_string($sql)) {
            return ['rows' => 0, 'full' => false];
        }

        $rows = $wpdb->get_results($sql);
        if (!is_array($rows)) {
            return ['rows' => 0, 'full' => false];
        }

        $patterns     = $this->buildPatterns(array_keys($map));
        $replacements = array_values($map);
        $affected     = 0;

        foreach ($rows as $row) {
            if (!is_object($row) || !isset($row->post_content)) {
                continue;
            }
            $updated = preg_replace($patterns, $replacements, (string) $row->post_content);
            if (is_string($updated) && $updated !== $row->post_content) {
                $wpdb->update($wpdb->posts, ['post_content' => $updated], ['ID' => (int) $row->ID]);
                $affected++;
            }
        }

        return ['rows' => $affected, 'full' => count($rows) >= self::POST_LIMIT];
    }

    // ------------------------------------------------------------------
    // postmeta
    // ------------------------------------------------------------------

    /**
     * @param array<string,string> $map
     * @return array{rows:int,full:bool}
     */
    private function replaceInPostmeta(array $map): array
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return ['rows' => 0, 'full' => false];
        }

        $conds = [];
        foreach (array_keys($map) as $url) {
            // Quoted LIKE + boundary-guarded REGEXP so serialized/unquoted
            // occurrences are caught too (ImageOptimizer.php:349-356).
            $conds[] = $wpdb->prepare('pm.meta_value LIKE %s', '%"' . $wpdb->esc_like($url) . '"%');
            $conds[] = $wpdb->prepare(
                'pm.meta_value REGEXP %s',
                $wpdb->esc_like(str_replace('/', '\/', $url)) . '([^0-9A-Za-z]|$)'
            );
        }

        $postTypes = $this->publicPostTypes();
        if ($postTypes === []) {
            return ['rows' => 0, 'full' => false];
        }
        $placeholders = implode(',', array_fill(0, count($postTypes), '%s'));

        $skip = "'" . implode("','", array_map([$this, 'sqlQuote'], self::SKIP_META_KEYS)) . "'";

        $postsTable    = $wpdb->posts;
        $postmetaTable = $wpdb->postmeta;

        // @phpstan-ignore-next-line — wpdb runtime seam; table names + const skip-list are trusted.
        $sql = $wpdb->prepare(
            "SELECT pm.meta_id, pm.post_id, pm.meta_value FROM $postmetaTable pm"
            . " INNER JOIN $postsTable p ON pm.post_id = p.ID WHERE ("
            . implode(' OR ', $conds) . ')'
            . " AND pm.meta_key NOT IN ($skip)"
            . " AND p.post_status = 'publish' AND p.post_type IN ($placeholders)"
            . ' ORDER BY pm.meta_id DESC LIMIT ' . self::META_LIMIT,
            ...$postTypes
        );
        if (!is_string($sql)) {
            return ['rows' => 0, 'full' => false];
        }

        $rows = $wpdb->get_results($sql, ARRAY_A);
        if (!is_array($rows)) {
            return ['rows' => 0, 'full' => false];
        }

        $affected = 0;
        foreach ($rows as $row) {
            if (!is_array($row) || !isset($row['meta_value'])) {
                continue;
            }
            $original = (string) $row['meta_value'];
            $updated  = $this->rewriteValue($original, $map);
            if ($updated !== $original) {
                $wpdb->update($wpdb->postmeta, ['meta_value' => $updated], ['meta_id' => (int) $row['meta_id']]);
                $affected++;
            }
        }

        return ['rows' => $affected, 'full' => count($rows) >= self::META_LIMIT];
    }

    // ------------------------------------------------------------------
    // helpers
    // ------------------------------------------------------------------

    /**
     * Build the boundary-guarded regex pattern for each URL. The trailing
     * lookahead `(?=([^0-9A-Za-z]|$))` is the crux (ImageOptimizer.php:417).
     *
     * @param list<string> $urls
     * @return list<string>
     */
    private function buildPatterns(array $urls): array
    {
        $patterns = [];
        foreach ($urls as $url) {
            $patterns[] = '/' . preg_quote($url, '/') . '(?=([^0-9A-Za-z]|$))/';
        }

        return $patterns;
    }

    /**
     * Drop empty keys/values and self-mappings from the replacement map.
     *
     * @param array<string,string> $map
     * @return array<string,string>
     */
    private function cleanMap(array $map): array
    {
        $clean = [];
        foreach ($map as $from => $to) {
            if (!is_string($from) || !is_string($to)) {
                continue;
            }
            if ($from === '' || $to === '' || $from === $to) {
                continue;
            }
            $clean[$from] = $to;
        }

        return $clean;
    }

    /**
     * Whether a value is a serialized PHP blob. Prefers WP's is_serialized;
     * falls back to a conservative local check outside a WP runtime (tests).
     *
     * @param string $value
     * @return bool
     */
    private function isSerialized(string $value): bool
    {
        if (function_exists('is_serialized')) {
            return is_serialized($value);
        }
        // Minimal fallback: recognizable serialized prefixes. unserialize() is
        // still gated by allowed_classes=false at the call site.
        if ($value === 'N;' || $value === 'b:0;' || $value === 'b:1;') {
            return true;
        }
        return (bool) preg_match('/^(a|O|s|i|d|b):[0-9]/', $value);
    }

    /**
     * Escape a string for safe single-quoted SQL literal use (skip-list keys
     * are constants, but escape defensively).
     *
     * @param string $value
     * @return string
     */
    private function sqlQuote(string $value): string
    {
        return str_replace("'", "''", $value);
    }

    /**
     * Public post types (object cache hit; restricts the rewrite to surfaces
     * that actually render URLs — ImageOptimizer.php:305,359).
     *
     * @return list<string>
     */
    private function publicPostTypes(): array
    {
        if (!function_exists('get_post_types')) {
            return ['post', 'page'];
        }
        $types = get_post_types(['public' => true]);

        return is_array($types) ? array_values($types) : ['post', 'page'];
    }

    /**
     * The WordPress DB handle, or null outside a WP runtime.
     *
     * @return \wpdb|null
     */
    private function wpdb(): ?object
    {
        if (!isset($GLOBALS['wpdb']) || !is_object($GLOBALS['wpdb'])) {
            return null;
        }

        /** @var \wpdb $wpdb */
        $wpdb = $GLOBALS['wpdb'];

        return $wpdb;
    }
}
