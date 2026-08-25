<?php
/**
 * Bloat — remove well-known WordPress front-end bloat by un-registering core
 * actions/filters, gated per toggle.
 *
 * Unlike the rest of the optimizer (which runs inside the output buffer on a
 * cache MISS), bloat removal is REGISTERED ON `init` — each enabled toggle calls
 * remove_action / add_filter at the right phase so the unwanted markup/scripts
 * are never enqueued in the first place. This is why it cannot live in the
 * ob_start pipeline: by buffer-flush time the emoji script et al are already in
 * the HTML; the only clean removal is to stop core emitting them.
 *
 * Each toggle reads its PerfConfig flag and is otherwise a no-op. The tests
 * assert removal via has_action() after register().
 *
 * Standard bloat-removal technique (many optimization plugins).
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Registers the per-toggle core de-bloat hooks.
 */
final class Bloat
{
    private PerfConfig $config;

    /**
     * @param PerfConfig|null $config Optimization config (default: loaded).
     */
    public function __construct(?PerfConfig $config = null)
    {
        $this->config = $config ?? PerfConfig::load();
    }

    /**
     * Bind the enabled de-bloat hooks. Called from plugin boot on `init` (admin
     * paths are excluded by the individual callbacks where it matters).
     *
     * @return void
     */
    public function register(): void
    {
        if (!$this->config->anyBloatEnabled() || !function_exists('add_action')) {
            return;
        }

        if ($this->config->bloatDisableEmojis) {
            $this->disableEmojis();
        }
        if ($this->config->bloatDisableBlockCss) {
            add_action('wp_enqueue_scripts', [$this, 'dequeueBlockCss'], 100);
        }
        if ($this->config->bloatDisableDashicons) {
            add_action('wp_enqueue_scripts', [$this, 'dequeueDashicons'], 100);
        }
        if ($this->config->bloatDisableJqueryMigrate) {
            // wp_default_scripts is fired via do_action_ref_array() in core — an
            // action, not a filter. $scripts is mutated by reference; the
            // callback has nothing to return.
            add_action('wp_default_scripts', [$this, 'removeJqueryMigrate']);
        }
        if ($this->config->bloatDisableXmlRpc) {
            add_filter('xmlrpc_enabled', '__return_false');
        }
        if ($this->config->bloatDisableRssFeed) {
            $this->disableRssFeed();
        }
        if ($this->config->bloatDisableOembeds) {
            $this->disableOembeds();
        }
        if ($this->config->bloatHeartbeatControl) {
            add_filter('heartbeat_settings', [$this, 'throttleHeartbeat']);
        }
        if ($this->config->bloatPostRevisionsControl) {
            add_filter('wp_revisions_to_keep', [$this, 'capRevisions'], 10, 2);
        }
    }

    /**
     * Strip the WP emoji detection script + styles + feed filters.
     *
     * @return void
     */
    private function disableEmojis(): void
    {
        remove_action('wp_head', 'print_emoji_detection_script', 7);
        remove_action('wp_print_styles', 'print_emoji_styles');
        remove_action('admin_print_scripts', 'print_emoji_detection_script');
        remove_action('admin_print_styles', 'print_emoji_styles');
        remove_filter('the_content_feed', 'wp_staticize_emoji');
        remove_filter('comment_text_rss', 'wp_staticize_emoji');
        remove_filter('wp_mail', 'wp_staticize_emoji_for_email');
        add_filter('emoji_svg_url', '__return_false');
    }

    /**
     * Remove RSS/Atom feed links from <head> and short-circuit feed requests.
     *
     * @return void
     */
    private function disableRssFeed(): void
    {
        remove_action('wp_head', 'feed_links', 2);
        remove_action('wp_head', 'feed_links_extra', 3);
    }

    /**
     * Remove oEmbed discovery + host JS + the front-end embed script.
     *
     * @return void
     */
    private function disableOembeds(): void
    {
        remove_action('wp_head', 'wp_oembed_add_discovery_links');
        remove_action('wp_head', 'wp_oembed_add_host_js');
        remove_action('rest_api_init', 'wp_oembed_register_route');
        remove_filter('oembed_dataparse', 'wp_filter_oembed_result', 10);
    }

    // -------------------------------------------------------------------------
    // Hook callbacks (public — bound to WP hooks)
    // -------------------------------------------------------------------------

    /**
     * Dequeue wp-block-library front-end CSS (+ WooCommerce block styles).
     *
     * @return void
     */
    public function dequeueBlockCss(): void
    {
        if (!function_exists('wp_dequeue_style')) {
            return;
        }
        wp_dequeue_style('wp-block-library');
        wp_dequeue_style('wp-block-library-theme');
        wp_dequeue_style('global-styles');
        if (class_exists('WooCommerce')) {
            wp_dequeue_style('wc-blocks-vendors-style');
            wp_dequeue_style('wc-all-blocks-style');
        }
    }

    /**
     * Dequeue dashicons for anonymous visitors (the admin bar needs them when
     * logged in, so we keep them then).
     *
     * @return void
     */
    public function dequeueDashicons(): void
    {
        if (function_exists('is_user_logged_in') && is_user_logged_in()) {
            return;
        }
        if (function_exists('wp_dequeue_style')) {
            wp_dequeue_style('dashicons');
        }
    }

    /**
     * Drop jquery-migrate from jQuery's dependency chain (front end only).
     *
     * @param object $scripts WP_Scripts instance.
     * @return void
     */
    public function removeJqueryMigrate($scripts): void
    {
        if (function_exists('is_admin') && is_admin()) {
            return;
        }
        if (!is_object($scripts) || !isset($scripts->registered) || !is_array($scripts->registered)) {
            return;
        }
        $jquery = $scripts->registered['jquery'] ?? null;
        if (is_object($jquery) && isset($jquery->deps) && is_array($jquery->deps)) {
            $jquery->deps = array_values(array_diff($jquery->deps, ['jquery-migrate']));
        }
    }

    /**
     * Throttle the Heartbeat API to a 60s interval.
     *
     * @param array<string,mixed> $settings Heartbeat settings.
     * @return array<string,mixed>
     */
    public function throttleHeartbeat($settings): array
    {
        if (!is_array($settings)) {
            $settings = [];
        }
        $settings['interval'] = 60;
        return $settings;
    }

    /**
     * Cap the number of stored post revisions.
     *
     * @param int|bool $num  Current limit.
     * @param mixed    $post Post object (unused).
     * @return int
     */
    public function capRevisions($num, $post = null): int
    {
        $limit = is_numeric($num) ? (int) $num : 5;
        return ($limit < 0 || $limit > 5) ? 5 : $limit;
    }
}
