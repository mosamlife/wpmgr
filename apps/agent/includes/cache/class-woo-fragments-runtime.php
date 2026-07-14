<?php
/**
 * WooFragmentsRuntime — injects the cart-fragments JS compatibility shim into
 * the page HTML when BOTH of the following are true:
 *
 *   1. woo_cacheable_session is ON (the WooCommerce cacheable-shell feature is
 *      enabled).
 *   2. Our JS-delay transform is active (JsDelay::$method is 'interaction' or
 *      'idle') — the 'defer' method does not need the shim because it leaves the
 *      browser's native defer scheduling intact.
 *
 * When either condition is false nothing is emitted and the page is unmodified.
 * The shim is a tiny self-contained IIFE with no jQuery dependency; it is injected
 * once, just before </body>, as a plain inline <script> tag.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

/**
 * Injects the WooCommerce cart-fragments JS-delay compatibility shim.
 */
final class WooFragmentsRuntime
{
    /**
     * Attribute that marks the injected shim so it is never injected twice
     * (guards against a double Optimizer pass on the same buffer).
     */
    private const MARKER_ATTR = 'data-wpmgr-woo-frags';

    /** Cached shim bytes (read once per instance). */
    private ?string $shim = null;

    /**
     * Whether the shim should be injected at all. Computed once from config.
     *
     * @param bool   $wooCacheableSession Whether the woo_cacheable_session flag is on.
     * @param string $jsDelayMethod       The JS-delay method ('defer'|'interaction'|'idle'|'').
     */
    private bool $shouldInject;

    /**
     * @param bool   $wooCacheableSession Whether woo_cacheable_session is on.
     * @param string $jsDelayMethod       The active JS-delay method.
     */
    public function __construct(bool $wooCacheableSession, string $jsDelayMethod)
    {
        // Only inject when BOTH flags are on AND the delay method needs the shim.
        // 'defer' uses native browser deferral and does not mis-sequence jQuery
        // events, so no shim is needed. An empty method means JS-delay is off.
        $this->shouldInject = $wooCacheableSession
            && in_array($jsDelayMethod, ['interaction', 'idle'], true);
    }

    /**
     * Inject the shim into the page HTML if required. Returns the (possibly
     * modified) HTML. If the shim is already present (marker attribute), or
     * injection is not required, the HTML is returned unchanged.
     *
     * @param string $html Rendered page HTML.
     * @return string
     */
    public function maybeInject(string $html): string
    {
        if (!$this->shouldInject) {
            return $html;
        }

        // Idempotency guard: don't inject twice.
        if (strpos($html, self::MARKER_ATTR) !== false) {
            return $html;
        }

        $shim = $this->shimCode();
        if ($shim === '') {
            return $html;
        }

        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedScript -- this class only runs from Optimizer::run() (class-optimizer.php), called by CacheWriter's ob_start() callback (class-cache-writer.php) on the fully-rendered HTML string of a cacheable MISS, well after wp_footer has already printed; the shim is spliced into the already-buffered string, so it can only operate as a post-hoc string rewrite -- WP's enqueue API has no queue left to append to at this point
        $tag = '<script ' . self::MARKER_ATTR . '>' . $shim . '</script>';

        // Inject just before the closing </body> tag (whitespace-tolerant: accepts
        // </body>, </body >, </ body>, < /body>, etc.). If none is found, append.
        if (preg_match('/<\s*\/\s*body\s*>/i', $html)) {
            return (string) preg_replace('/<\s*\/\s*body\s*>(?![\s\S]*<\s*\/\s*body\s*>)/i', $tag . '$0', $html, 1);
        }
        return $html . $tag;
    }

    /**
     * Load the shim JS from the assets directory (cached after first read).
     *
     * @return string
     */
    private function shimCode(): string
    {
        if ($this->shim !== null) {
            return $this->shim;
        }
        $dir  = defined('WPMGR_AGENT_DIR') ? (string) constant('WPMGR_AGENT_DIR') : dirname(__DIR__, 2) . '/';
        $path = rtrim($dir, '/') . '/assets/wpmgr-woo-fragments.js';
        $bytes = is_file($path) ? @file_get_contents($path) : false;
        $this->shim = is_string($bytes) ? trim($bytes) : '';
        return $this->shim;
    }
}
