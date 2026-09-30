<?php
/**
 * Purge — deletes cached page files from disk.
 *
 * Three granularities:
 *   purgeUrl($url)      — delete every variant (.html.gz) under that exact URL's
 *                         directory (non-recursive: removes mobile/role/query
 *                         variants for the path, but not child paths).
 *   purgeSite($host)    — recursively delete every .html.gz under a host's tree
 *                         (keeps the directory skeleton; removes emptied dirs).
 *   purgeEverything()   — remove the entire cache root and recreate it empty.
 *
 * All paths are contained under the cache root; a computed target outside the
 * root is refused (defence against a malformed host/URL).
 *
 * Standard WordPress disk-cache purge technique.
 *
 * Every purge operation also fires a pair of WordPress actions
 * (`wpmgr_purge_*:before` / `:after`) so that host/edge-cache integrations can
 * hook in and clear server-side caches (Varnish, Kinsta, WP Engine, …) in lock
 * step with WPMgr's own on-disk cache. The integration loader is booted lazily
 * the first time a Purge is constructed, so the agent never serves stale pages
 * on a managed host. File-deletion behaviour is unchanged — the actions only
 * wrap it.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

use WPMgr\Agent\Integrations\Integrations;

/**
 * Filesystem cache purger.
 */
final class Purge
{
    /** Absolute cache root (…/cache/wpmgr). */
    private string $cacheRoot;

    /** Whether the host/edge-cache integrations have been booted this request. */
    private static bool $integrationsBooted = false;

    /**
     * @param string $cacheRoot Absolute cache root.
     */
    public function __construct(string $cacheRoot)
    {
        $this->cacheRoot = rtrim($cacheRoot, '/\\');
        self::bootIntegrations();
    }

    /**
     * Boot the host/edge-cache integration loader exactly once per request.
     *
     * Each integration registers itself on the `wpmgr_purge_*:before` actions in
     * its own constructor and no-ops when its host is not detected, so booting
     * here is cheap and side-effect-free on un-managed hosts. We stay in our lane:
     * Purge owns the integration bootstrap (the loader lives under our
     * includes/integrations/ tree), nothing else wires it.
     *
     * @return void
     */
    private static function bootIntegrations(): void
    {
        if (self::$integrationsBooted) {
            return;
        }
        self::$integrationsBooted = true;

        if (class_exists(Integrations::class)) {
            (new Integrations())->boot();
        }
    }

    /**
     * Fire a WordPress action if the runtime provides do_action (no-op in unit
     * tests / non-WP contexts that have not stubbed it).
     *
     * @param string       $hook Action hook name.
     * @param array<mixed> $args Positional args passed to the action.
     * @return void
     */
    private function fire(string $hook, array $args = []): void
    {
        if (function_exists('do_action')) {
            \do_action($hook, ...$args); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.DynamicHooknameFound -- $hook is always a literal wpmgr_-prefixed action passed by in-class callers
        }
    }

    /**
     * Action arguments: the given leading arguments, plus the purge options
     * when there are any. With no options the action receives exactly what it
     * received before options existed.
     *
     * @param array<mixed>        $args    Leading action arguments.
     * @param array<string,mixed> $options Purge options.
     * @return array<mixed>
     */
    private static function withOptions(array $args, array $options): array
    {
        if ($options !== []) {
            $args[] = $options;
        }
        return $args;
    }

    /**
     * Delete every cached variant for a single URL's directory (non-recursive).
     *
     * @param string              $url     Absolute URL (scheme://host/path) or path-only.
     * @param array<string,mixed> $options Purge options passed to the purge actions,
     *                                     e.g. ['origin_only' => true].
     * @return int Number of .html.gz files removed.
     */
    public function purgeUrl(string $url, array $options = []): int
    {
        $urls = [$url];
        $this->fire('wpmgr_purge_urls:before', self::withOptions([$urls], $options));

        $host = self::bucketHost($url);

        // Derive the path component. wp_parse_url() returns NULL for the path when
        // the URL is just scheme://host with no path (e.g. "https://example.com"
        // or "https://example.com?x=1"). The old "?? $url" fallback then fed the
        // ENTIRE URL in as the path, which normalizePath() mangled into a phantom
        // "/https:/host" subdirectory — so the homepage's real cache file at
        // {root}/{host}/index.html.gz was never matched and survived every purge
        // (this is exactly why scope=url purges of the home page were no-ops on
        // nginx/Apache static-fast-path setups). Resolve a host-present-but-
        // path-less URL to the site root ("/"); only fall back to the raw string
        // for a genuinely path-only input like "/blog/".
        $rawPath = wp_parse_url($url, PHP_URL_PATH);
        if (is_string($rawPath) && $rawPath !== '') {
            $path = $rawPath;
        } elseif ($host !== '') {
            $path = '/';
        } else {
            $path = $url;
        }

        $host = $this->sanitizeHost($host);
        $dir  = $this->cacheRoot . '/' . $host . CacheKey::normalizePath($path);

        if (!$this->isContained($dir)) {
            $this->fire('wpmgr_purge_urls:after', self::withOptions([$urls], $options));
            return 0;
        }

        $removed = 0;
        $glob = @glob($dir . '/*' . CacheKey::EXTENSION);
        if (is_array($glob)) {
            foreach ($glob as $file) {
                if (@is_file($file)) {
                    wp_delete_file($file);
                    if (!@is_file($file)) {
                        $removed++;
                    }
                }
            }
        }
        // Tidy an emptied directory.
        $this->removeIfEmpty($dir);

        $this->fire('wpmgr_purge_urls:after', self::withOptions([$urls], $options));

        return $removed;
    }

    /**
     * The host part of a purge URL's cache bucket, before sanitizeHost().
     *
     * The cache writer buckets a page by the request's Host header, which
     * carries a port only when it is not the scheme's default (browsers omit
     * :443 for https and :80 for http). So a non-default port is part of the
     * bucket and a default one never is. A URL without an http(s) scheme gives
     * the bare host.
     *
     * @param string $url Absolute URL, or path-only.
     * @return string Host, with ':port' when the port is not the scheme's default.
     */
    private static function bucketHost(string $url): string
    {
        $host   = (string) (wp_parse_url($url, PHP_URL_HOST) ?? '');
        $port   = wp_parse_url($url, PHP_URL_PORT);
        $scheme = strtolower((string) (wp_parse_url($url, PHP_URL_SCHEME) ?? ''));
        if ($host !== '' && is_int($port)
            && (($scheme === 'https' && $port !== 443) || ($scheme === 'http' && $port !== 80))
        ) {
            $host .= ':' . $port;
        }
        return $host;
    }

    /**
     * Recursively delete every .html.gz under a host tree.
     *
     * @param string $host Host (or full URL — host is extracted).
     * @return int Number of files removed.
     */
    public function purgeSite(string $host): int
    {
        if (str_contains($host, '://') || str_contains($host, '/')) {
            $parsed = (string) (wp_parse_url($host, PHP_URL_HOST) ?? '');
            if ($parsed !== '') {
                $host = $parsed;
            }
        }
        $host = $this->sanitizeHost($host);

        // The "pages" granularity clears a whole host tree; integrations that can
        // only purge by URL receive the host root as a single-element list.
        $urls = $host !== '' ? ['https://' . $host . '/'] : [];
        $this->fire('wpmgr_purge_pages:before', [$urls]);

        $base = $this->cacheRoot . '/' . $host;

        if (!$this->isContained($base) || !@is_dir($base)) {
            $this->fire('wpmgr_purge_pages:after', [$urls]);
            return 0;
        }

        $removed = $this->recursiveUnlinkHtml($base);

        $this->fire('wpmgr_purge_pages:after', [$urls]);

        return $removed;
    }

    /**
     * Remove the entire cache tree and recreate an empty root.
     *
     * @param array<string,mixed> $options Purge options passed to the purge actions,
     *                                     e.g. ['origin_only' => true].
     * @return bool True on success.
     */
    public function purgeEverything(array $options = []): bool
    {
        $this->fire('wpmgr_purge_everything:before', self::withOptions([], $options));

        if ($this->cacheRoot === '' || !$this->looksLikeCacheRoot($this->cacheRoot)) {
            $this->fire('wpmgr_purge_everything:after', self::withOptions([], $options));
            return false;
        }

        if (@is_dir($this->cacheRoot)) {
            $this->recursiveRemoveDir($this->cacheRoot);
        }

        $ok = wp_mkdir_p($this->cacheRoot) || @is_dir($this->cacheRoot);

        $this->fire('wpmgr_purge_everything:after', self::withOptions([], $options));

        // Stamp the "Last purge" gauge for EVERY full-cache clear (operator purge,
        // auto-purge on content changes, host-integration flush). This is the one
        // chokepoint for whole-cache purges; per-URL purges (incl. RUCSS reheat
        // pre-purges) deliberately do not record here. The next stats heartbeat
        // reports it; the CP reconciles with its own operator-purge stamp via
        // GREATEST, so this never regresses a newer dashboard purge time.
        if ($ok) {
            PerfReporter::persistLastPurge(time(), 'all');
        }

        return $ok;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Recursively delete *.html.gz files (only) under $dir, pruning emptied dirs.
     *
     * @param string $dir Directory to walk.
     * @return int Files removed.
     */
    private function recursiveUnlinkHtml(string $dir): int
    {
        $removed = 0;
        $entries = @scandir($dir);
        if ($entries === false) {
            return 0;
        }

        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $full = $dir . '/' . $entry;
            if (@is_dir($full) && !@is_link($full)) {
                $removed += $this->recursiveUnlinkHtml($full);
                $this->removeIfEmpty($full);
            } elseif (substr($entry, -strlen(CacheKey::EXTENSION)) === CacheKey::EXTENSION) {
                wp_delete_file($full);
                if (!@is_file($full)) {
                    $removed++;
                }
            }
        }

        return $removed;
    }

    /**
     * Recursively remove an entire directory tree.
     *
     * @param string $dir Directory.
     * @return void
     */
    private function recursiveRemoveDir(string $dir): void
    {
        $entries = @scandir($dir);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $full = $dir . '/' . $entry;
            if (@is_dir($full) && !@is_link($full)) {
                $this->recursiveRemoveDir($full);
            } else {
                wp_delete_file($full);
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived cache dir; WP_Filesystem not initialized
    }

    /**
     * Remove a directory only if it is empty.
     *
     * @param string $dir Directory.
     * @return void
     */
    private function removeIfEmpty(string $dir): void
    {
        if (!@is_dir($dir)) {
            return;
        }
        $entries = @scandir($dir);
        if (is_array($entries) && count($entries) <= 2) {
            @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived cache dir; WP_Filesystem not initialized
        }
    }

    /**
     * Whether a resolved path is safely contained within the cache root (after
     * normalising away any traversal).
     *
     * @param string $path Candidate path.
     * @return bool
     */
    private function isContained(string $path): bool
    {
        $root = rtrim($this->cacheRoot, '/\\');
        $norm = $this->collapse($path);
        return $norm === $root || strpos($norm, $root . '/') === 0;
    }

    /**
     * A weak guard that the configured root really is a wpmgr cache dir so
     * purgeEverything() can never nuke an arbitrary directory.
     *
     * @param string $path Cache root.
     * @return bool
     */
    private function looksLikeCacheRoot(string $path): bool
    {
        return str_contains(str_replace('\\', '/', $path), '/cache/wpmgr');
    }

    /**
     * Collapse ../ and ./ segments in a path string (no filesystem access).
     *
     * @param string $path Path.
     * @return string
     */
    private function collapse(string $path): string
    {
        $path  = str_replace('\\', '/', $path);
        $parts = explode('/', $path);
        $out   = [];
        foreach ($parts as $p) {
            if ($p === '..') {
                array_pop($out);
            } elseif ($p !== '.' && $p !== '') {
                $out[] = $p;
            }
        }
        $prefix = ($path !== '' && $path[0] === '/') ? '/' : '';
        return $prefix . implode('/', $out);
    }

    /**
     * Sanitise a host into a single safe path component.
     *
     * @param string $host Raw host.
     * @return string
     */
    private function sanitizeHost(string $host): string
    {
        $host = strtolower($host);
        $host = preg_replace('/[^a-z0-9\.\-:]/', '', $host) ?? '';
        $host = str_replace([':', '..'], ['_', ''], $host);
        return $host;
    }
}
