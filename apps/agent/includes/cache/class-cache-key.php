<?php
/**
 * CacheKey — deterministic cache file-name builder.
 *
 * The page-cache stores each variant of a URL as a gzip-compressed HTML file on
 * disk. The file NAME encodes every dimension the response can vary on, so the
 * PHP serving drop-in (assets/wpmgr-advanced-cache.php) and the PHP writer
 * (CacheWriter) MUST produce byte-identical names for the same request state.
 *
 * Name recipe (ordered — order is load-bearing):
 *
 *   index                              base
 *   [-logged-in]                       if a wordpress_logged_in_* cookie present
 *   [-{role}]                          from the wpmgr_logged_in_roles cookie
 *   [-{cookieValue}]...                for each configured include-cookie that is set
 *   [-mobile]                          if mobile caching on AND mobile UA
 *   [-{md5(serialize(nonMarketingQuery))}]  if any cache-varying query remains
 *   .html.gz                           extension
 *
 * The query hash uses md5(serialize($map)) over the query array with the
 * marketing/ignore params removed. PHP's serialize() is insertion-ordered, so
 * the drop-in and the writer both sort the surviving keys before serialising to
 * guarantee a stable hash regardless of arrival order.
 *
 * This is the standard WordPress disk-cache key technique: a host + path +
 * variant composite that uniquely identifies each cacheable request state.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

/**
 * Builds the on-disk cache file name and the cache file path.
 */
final class CacheKey
{
    /** Non-HTTPOnly cookie the agent sets at login carrying the user's roles. */
    public const ROLE_COOKIE = 'wpmgr_logged_in_roles';

    /** Mobile user-agent detection pattern (shared with the drop-in). */
    public const MOBILE_UA_PATTERN =
        '/Mobile|Android|Silk\/|Kindle|BlackBerry|Opera (Mini|Mobi)|iPhone|iPad|iPod|IEMobile/i';

    /** File-name suffix for cached HTML. */
    public const EXTENSION = '.html.gz';

    /**
     * Maximum number of distinct cache-varying query keys a request may carry and
     * still be cacheable. A request with more is treated as NON-cacheable (served
     * via PHP, never written to disk) so an attacker cannot mint unbounded cache
     * files by appending arbitrary distinct params (disk-exhaustion DoS). The
     * drop-in enforces the identical cap.
     */
    public const MAX_QUERY_KEYS = 12;

    /**
     * Build the cache file name for a request, given fully-resolved inputs. No
     * superglobal access here so this is unit-testable without WP.
     *
     * @param array<string,mixed> $cookies        The request cookie map (name => value).
     * @param array<string,mixed> $query          The request query map ($_GET shape).
     * @param string              $userAgent      The request User-Agent header.
     * @param bool                $cacheLoggedIn  Whether logged-in caching is enabled.
     * @param bool                $cacheMobile    Whether a separate mobile bucket is enabled.
     * @param list<string>        $includeCookies Ordered cookie names whose values fragment the cache.
     * @param list<string>        $extraInclude   Extra cache-varying query params (operator-configured).
     * @return string|null The file name (with .html.gz), or null when the request
     *                     must NOT be cached at the key level (logged-in but
     *                     logged-in caching disabled).
     */
    public function build(
        array $cookies,
        array $query,
        string $userAgent,
        bool $cacheLoggedIn,
        bool $cacheMobile,
        array $includeCookies = [],
        array $extraInclude = []
    ): ?string {
        $name = 'index';

        // 1. logged-in segment.
        $isLoggedIn = $this->hasLoggedInCookie($cookies);
        if ($isLoggedIn) {
            if (!$cacheLoggedIn) {
                return null; // serve via PHP, never write/read a disk variant
            }
            $name .= '-logged-in';
        }

        // 2. role segment (from the agent-set non-HTTPOnly role cookie).
        if ($isLoggedIn && isset($cookies[self::ROLE_COOKIE]) && is_string($cookies[self::ROLE_COOKIE])) {
            $role = $this->sanitizeSegment($cookies[self::ROLE_COOKIE]);
            if ($role !== '') {
                $name .= '-' . $role;
            }
        }

        // 3. include-cookie segments, in configured order.
        foreach ($includeCookies as $cookieName) {
            if (!is_string($cookieName) || $cookieName === '') {
                continue;
            }
            if (isset($cookies[$cookieName]) && is_scalar($cookies[$cookieName])) {
                $value = $this->sanitizeSegment((string) $cookies[$cookieName]);
                if ($value !== '') {
                    $name .= '-' . $value;
                }
            }
        }

        // 4. mobile segment.
        if ($cacheMobile && $this->isMobile($userAgent)) {
            $name .= '-mobile';
        }

        // 5. query-hash segment (non-marketing query only). Over the key cap the
        // request is non-cacheable: signal skip so the writer never persists it.
        $kept = $this->keptQuery($query);
        if (count($kept) > self::MAX_QUERY_KEYS) {
            return null;
        }
        $hash = $this->queryHash($query, $extraInclude);
        if ($hash !== '') {
            $name .= '-' . $hash;
        }

        return $name . self::EXTENSION;
    }

    /**
     * Compute the md5 hash segment for the cache-varying portion of the query,
     * or '' when nothing varies the cache.
     *
     * Marketing/ignore params are removed. The surviving keys are sorted so the
     * serialised representation (and thus the md5) is independent of arrival
     * order — the drop-in performs the identical sort.
     *
     * @param array<string,mixed> $query        Raw query map.
     * @param list<string>        $extraInclude Operator-configured extra cache-varying params.
     * @return string The 32-char md5 hash, or '' if no cache-varying query.
     */
    public function queryHash(array $query, array $extraInclude = []): string
    {
        $kept = $this->keptQuery($query);

        if ($kept === []) {
            return '';
        }

        // Over the cap the request is non-cacheable; return '' so a caller using
        // the hash in isolation never produces an unbounded fan-out of keys.
        if (count($kept) > self::MAX_QUERY_KEYS) {
            return '';
        }

        ksort($kept);

        return md5(serialize($kept));
    }

    /**
     * The cache-varying portion of the query: the raw map with marketing/ignore
     * params removed. Shared by build() (for the key cap) and queryHash().
     *
     * @param array<string,mixed> $query Raw query map.
     * @return array<string,mixed>
     */
    private function keptQuery(array $query): array
    {
        $kept = [];
        foreach ($query as $key => $value) {
            $key = (string) $key;
            if (MarketingParams::isIgnored($key)) {
                continue; // tracking param — never fragments
            }
            $kept[$key] = $value;
        }
        return $kept;
    }

    /**
     * Resolve the absolute cache file path for a host/URI/name triple.
     *
     * Layout: {cacheRoot}/{host}/{normalised-path}/{fileName}
     * where the path is lowercased + urldecoded (mirrors the drop-in).
     *
     * @param string $cacheRoot Absolute cache root (…/cache/wpmgr).
     * @param string $host      Request host (HTTP_HOST).
     * @param string $uriPath   Request URI path (no query string).
     * @param string $fileName  The file name from build().
     * @return string Absolute path to the cache file.
     */
    public function path(string $cacheRoot, string $host, string $uriPath, string $fileName): string
    {
        $host = $this->sanitizeHost($host);
        $dir  = self::normalizePath($uriPath);

        $base = rtrim($cacheRoot, '/\\');

        return $base . '/' . $host . $dir . '/' . $fileName;
    }

    /**
     * Whether any cookie marks the visitor as a logged-in WordPress user.
     *
     * @param array<string,mixed> $cookies Request cookie map.
     * @return bool
     */
    public function hasLoggedInCookie(array $cookies): bool
    {
        foreach (array_keys($cookies) as $name) {
            if (preg_match('/^wordpress_logged_in_/i', (string) $name) === 1) {
                return true;
            }
        }
        return false;
    }

    /**
     * Mobile UA test (shared pattern with the drop-in).
     *
     * @param string $userAgent Request User-Agent.
     * @return bool
     */
    public function isMobile(string $userAgent): bool
    {
        if ($userAgent === '') {
            return false;
        }
        return preg_match(self::MOBILE_UA_PATTERN, $userAgent) === 1;
    }

    /**
     * Normalise a URI path the same way the drop-in does: strip the query, lower-
     * case, urldecode, ensure a single leading slash and no trailing slash noise.
     *
     * Returns '' for the site root so the path joins cleanly (host + '' + '/').
     *
     * @param string $uriPath Raw request URI (may include a query string).
     * @return string Normalised path segment beginning with '/', or '' for root.
     */
    public static function normalizePath(string $uriPath): string
    {
        // Drop any query string.
        $q = strpos($uriPath, '?');
        if ($q !== false) {
            $uriPath = substr($uriPath, 0, $q);
        }

        $uriPath = rawurldecode($uriPath);
        $uriPath = strtolower($uriPath);

        // The traversal-containment step below is duplicated verbatim in the
        // advanced-cache drop-in, which runs before WordPress loads and so can
        // never call this class. Bridge through the drop-in's variable name so
        // the two copies are byte-identical once dedented; a divergence would
        // store a page under one key and look it up under another.
        // tests/CacheNormalizeParityTest.php enforces that mechanically.
        $wpmgr_path = $uriPath;

        // WPMGR-NORMALIZE-PATH-SHARED BEGIN
        // Resolve the path one SEGMENT at a time, and classify each segment by
        // its whole value. A pattern strip over the joined string is the wrong
        // instrument here: removing a substring can fuse the two neighbours it
        // sat between into a token that was in neither of them, so the result
        // depends on how many times the strip is applied and no single pass is
        // trustworthy. Splitting on the separator first cannot fuse anything,
        // which makes the outcome correct by construction rather than by
        // iterating a substitution until it stops changing.
        //
        // An empty segment (a repeated or trailing separator) carries no name.
        // Neither does a segment that is nothing but dots: one dot is the
        // directory itself, two dots is its parent, and longer runs are not
        // portable filenames. Two dots pops one level first; popping at the
        // root is deliberately a no-op, so the result can never name anything
        // above the bucket it belongs to. Every other segment is preserved
        // exactly as it arrived.
        $wpmgr_path = str_replace(['\\', "\0"], ['/', ''], $wpmgr_path);
        $wpmgr_segments = [];
        foreach (explode('/', $wpmgr_path) as $wpmgr_segment) {
            if ($wpmgr_segment === '') {
                continue;
            }
            if (trim($wpmgr_segment, '.') === '') {
                if ($wpmgr_segment === '..') {
                    array_pop($wpmgr_segments);
                }
                continue;
            }
            $wpmgr_segments[] = $wpmgr_segment;
        }
        $wpmgr_path = $wpmgr_segments === [] ? '' : '/' . implode('/', $wpmgr_segments);
        // WPMGR-NORMALIZE-PATH-SHARED END

        return $wpmgr_path; // '' for root
    }

    /**
     * Canonical cache bucket name for a request host, or '' when the host must
     * not participate in caching at all.
     *
     * Returning '' rather than a placeholder is the point. A placeholder is a
     * single bucket that every rejected host shares, and a shared bucket is
     * both readable and writable by all of them — one rejected request could be
     * answered with a page cached for a different rejected request. Callers
     * must treat '' as "do not store, do not serve, boot WordPress".
     *
     * The advanced-cache drop-in carries this block verbatim, for the same
     * reason the path block is duplicated: it runs before WordPress loads and
     * cannot call this class. Bridge through its variable name so the two
     * copies are byte-identical once dedented.
     * tests/CacheNormalizeParityTest.php enforces that mechanically.
     *
     * @param string $host Raw HTTP_HOST.
     * @return string Bucket name, or '' when the host is not cacheable.
     */
    public static function normalizeHost(string $host): string
    {
        $wpmgr_host = $host;

        // WPMGR-NORMALIZE-HOST-SHARED BEGIN
        // Validate the whole value against an anchored pattern, then transform.
        // Validation decides only two things — cacheable, or not — so the two
        // sides cannot disagree about which bucket a host lands in: either they
        // both compute the same name, or they both decline to cache.
        //
        // The pattern requires the host to begin and end with an alphanumeric,
        // which rejects a value that is only separators. Such a value would
        // otherwise pass a charset test and then name a relative directory
        // rather than a bucket. Consecutive dots are rejected for the same
        // reason: an empty label is not a host, and it is not a directory
        // either.
        //
        // A port belongs to the identity of the site, so it stays in the bucket
        // name. ':' is not portable in a path segment, so both sides spell it
        // '_' — the alternative, discarding the port, would key two different
        // sites onto one bucket.
        $wpmgr_host = strtolower($wpmgr_host);
        if ($wpmgr_host === ''
            || strpos($wpmgr_host, '..') !== false
            || preg_match('/^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$/', $wpmgr_host) !== 1
        ) {
            $wpmgr_host = '';
        } else {
            $wpmgr_host = str_replace(':', '_', $wpmgr_host);
        }
        // WPMGR-NORMALIZE-HOST-SHARED END

        return $wpmgr_host; // '' when the host must not be cached
    }

    /**
     * Bucket name for path building. Callers that must not cache an unusable
     * host check {@see normalizeHost()} directly; this keeps a stable directory
     * name for path arithmetic (purge scans and the like), and nothing on the
     * write path stores into it.
     *
     * @param string $host Raw HTTP_HOST.
     * @return string
     */
    private function sanitizeHost(string $host): string
    {
        $host = self::normalizeHost($host);
        return $host === '' ? 'unknown-host' : $host;
    }

    /**
     * Make a value safe to use as a single file-name segment (roles, cookie
     * values): collapse to a conservative slug so a malicious cookie can never
     * write outside the cache tree or inject separators.
     *
     * @param string $value Raw segment value.
     * @return string
     */
    private function sanitizeSegment(string $value): string
    {
        $value = strtolower($value);
        $value = preg_replace('/[^a-z0-9\-_]/', '', $value) ?? '';
        return $value;
    }
}
