<?php
/**
 * JsDelay — defer JavaScript execution until the visitor interacts (or the
 * browser goes idle), then inject the small runtime that swaps the scripts back.
 *
 * Two methods (PerfConfig::$jsDelayMethod):
 *   - 'defer'       : add the standard `defer` attribute to external scripts
 *                     (no runtime needed) — the lightest, most compatible mode.
 *   - 'interaction' : rewrite `<script src>` -> `<script data-wpmgr-src>` (and
 *                     blank inline script bodies into data-wpmgr-src) so nothing
 *                     runs until the runtime swaps them on first user input.
 *   - 'idle'        : same rewrite, but the runtime swaps on requestIdleCallback.
 *
 * Excluded scripts (PerfConfig::$jsDelayExcludes substrings) are left untouched.
 * Scripts that are clearly structural (type=application/ld+json, the speculation-
 * rules block, the delay runtime itself) are never delayed. The runtime is
 * injected exactly once, before </body>.
 *
 * Standard JavaScript interaction-delay technique.
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Delays script execution and injects the swap runtime.
 */
final class JsDelay
{
    /** Attribute the runtime looks for when swapping a delayed external src. */
    public const SRC_ATTR = 'data-wpmgr-src';

    /** Marker attribute set on the loading method (idle|interaction). */
    public const METHOD_ATTR = 'data-wpmgr-method';

    /** @var list<string> Substrings whose script tags are never delayed. */
    private array $excludes;

    /** @var string Delay method: defer|interaction|idle. */
    private string $method;

    /** @var string|null Cached runtime JS (read once). */
    private ?string $runtime = null;

    /**
     * @param string       $method   defer|interaction|idle.
     * @param list<string> $excludes Exclusion substrings.
     */
    public function __construct(string $method = 'interaction', array $excludes = [])
    {
        $this->method   = in_array($method, ['defer', 'interaction', 'idle'], true) ? $method : 'interaction';
        $this->excludes = $excludes;
    }

    /**
     * Apply the delay transform to the page.
     *
     * @param string $html Full page HTML.
     * @return string
     */
    public function process(string $html): string
    {
        if (!preg_match_all('/<script\b[^>]*>(?:.*?<\/script>)?/is', $html, $tags)) {
            return $html;
        }

        $delayedAny = false;
        foreach ($tags[0] as $tag) {
            if ($this->shouldSkip($tag)) {
                continue;
            }

            if ($this->method === 'defer') {
                // Only external scripts can take a bare defer.
                if (TagHelper::attr($tag, 'src') === null) {
                    continue;
                }
                if (TagHelper::hasAttr($tag, 'defer') || TagHelper::hasAttr($tag, 'async')) {
                    continue;
                }
                $newTag = TagHelper::setAttr($tag, 'defer', '');
                $html = str_replace($tag, $newTag, $html);
                continue;
            }

            // interaction | idle: rewrite src -> data-wpmgr-src.
            $newTag = $this->delayTag($tag);
            if ($newTag === null) {
                continue;
            }
            $html = str_replace($tag, $newTag, $html);
            $delayedAny = true;
        }

        if ($delayedAny) {
            $html = $this->injectRuntime($html);
        }
        return $html;
    }

    /**
     * Rewrite a single script tag for the interaction/idle method.
     *
     * @param string $tag Script tag string.
     * @return string|null New tag, or null when nothing to do.
     */
    private function delayTag(string $tag): ?string
    {
        $src = TagHelper::attr($tag, 'src');
        if ($src !== null && $src !== '') {
            $newTag = TagHelper::renameAttr($tag, 'src', self::SRC_ATTR);
            $newTag = TagHelper::setAttr($newTag, self::METHOD_ATTR, $this->method);
            // Neutralise the type so the browser will not execute it as JS.
            $newTag = TagHelper::setAttr($newTag, 'type', 'wpmgr/javascript');
            return $newTag;
        }

        // Inline script: stash its body and blank the executable type.
        if (preg_match('/^(<script\b[^>]*>)(.*?)(<\/script>)$/is', $tag, $m)) {
            $body = $m[2];
            if (trim($body) === '') {
                return null;
            }
            $open = TagHelper::setAttr($m[1], self::METHOD_ATTR, $this->method);
            $open = TagHelper::setAttr($open, 'type', 'wpmgr/javascript');
            return $open . $body . $m[3];
        }
        return null;
    }

    /**
     * Whether a script tag must never be delayed.
     *
     * @param string $tag Script tag string.
     * @return bool
     */
    private function shouldSkip(string $tag): bool
    {
        if (TagHelper::matchesAny($this->excludes, $tag)) {
            return true;
        }
        // Already delayed.
        if (TagHelper::hasAttr($tag, self::SRC_ATTR) || TagHelper::hasAttr($tag, self::METHOD_ATTR)) {
            return true;
        }
        $type = TagHelper::attr($tag, 'type');
        if ($type !== null && $type !== '') {
            $t = strtolower($type);
            // Leave structured-data, speculation rules, importmaps, templates.
            if ($t !== 'text/javascript' && $t !== 'application/javascript' && $t !== 'module') {
                return true;
            }
        }
        return false;
    }

    /**
     * Inject the delay runtime once, before </body>.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function injectRuntime(string $html): string
    {
        if (strpos($html, 'data-wpmgr-delay-runtime') !== false) {
            return $html;
        }
        $runtime = $this->runtime();
        if ($runtime === '') {
            return $html;
        }
        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedScript -- this class only runs from Optimizer::run(), called by CacheWriter's ob_start() callback (class-cache-writer.php) on the fully-rendered HTML string of a cacheable MISS, well after wp_footer has already printed; the delay runtime is spliced into the already-buffered string, so it can only operate as a post-hoc string rewrite -- WP's enqueue API has no queue left to append to at this point
        $tag = '<script data-wpmgr-delay-runtime>' . $runtime . '</script>';
        if (stripos($html, '</body>') !== false) {
            return (string) preg_replace('/<\/body>(?![\s\S]*<\/body>)/i', $tag . '</body>', $html, 1);
        }
        return $html . $tag;
    }

    /**
     * Load the runtime JS from assets/wpmgr-delay.min.js (cached per instance).
     *
     * @return string
     */
    private function runtime(): string
    {
        if ($this->runtime !== null) {
            return $this->runtime;
        }
        $dir = defined('WPMGR_AGENT_DIR') ? (string) constant('WPMGR_AGENT_DIR') : dirname(__DIR__, 2) . '/';
        $path = rtrim($dir, '/') . '/assets/wpmgr-delay.min.js';
        $bytes = is_file($path) ? @file_get_contents($path) : false;
        $this->runtime = is_string($bytes) ? trim($bytes) : '';
        return $this->runtime;
    }
}
