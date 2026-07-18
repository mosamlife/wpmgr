<?php
/**
 * AdminBarPurge — adds a WPMgr Cache node to the WordPress admin bar with
 * one-click purge controls for the page cache.
 *
 * Registers two admin_post_* handlers (purge entire cache, purge this page)
 * gated on manage_options + a per-action nonce (check_admin_referer).
 * The admin bar node tree is only added for users who hold manage_options;
 * the 'Purge this page' child node is only added on singular/front-end views
 * where a concrete cacheable URL can be resolved.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

use WPMgr\Agent\Settings;

/**
 * WordPress admin-bar purge controls.
 */
final class AdminBarPurge
{
    /** admin-post action: purge the entire page cache. */
    public const ACTION_PURGE_ALL = 'wpmgr_cache_purge_all';

    /** admin-post action: purge the cache entry for a single URL. */
    public const ACTION_PURGE_URL = 'wpmgr_cache_purge_url';

    /** Transient key for the one-shot admin notice. TTL: 60 seconds. */
    private const NOTICE_TRANSIENT = 'wpmgr_abp_notice';

    public function __construct(
        private readonly CacheManager $cacheManager,
        private readonly Settings $settings,
    ) {}

    /**
     * Register all WordPress hooks. Called from Plugin::registerHooks() on
     * every request (not only admin ones) because admin_bar_menu fires on BOTH
     * the front end and admin when show_admin_bar() is true — the 'Purge this
     * page' node is front-end-only, so the hook must be bound there too. The
     * admin_post_*, admin_notices, and plugin-row hooks bound here only fire in
     * their own admin contexts; addBarNodes self-gates on manage_options.
     */
    public function registerHooks(): void
    {
        add_action('admin_bar_menu', [$this, 'addBarNodes'], 100);
        add_action('admin_post_' . self::ACTION_PURGE_ALL, [$this, 'handlePurgeAll']);
        add_action('admin_post_' . self::ACTION_PURGE_URL, [$this, 'handlePurgeUrl']);
        add_action('admin_notices', [$this, 'renderNotice']);
        add_filter('plugin_action_links_' . $this->pluginBasename(), [$this, 'addPluginRowLink']);
    }

    /**
     * Add the WPMgr Cache node tree to the admin bar.
     * Gated on manage_options; omits 'Purge this page' on admin screens
     * and omits 'Manage in WPMgr' when the site is not enrolled.
     */
    public function addBarNodes(\WP_Admin_Bar $bar): void
    {
        if (!current_user_can('manage_options')) {
            return;
        }

        // Top-level node — no href (it is a menu parent).
        $bar->add_node([
            'id'    => 'wpmgr-cache',
            'title' => 'WPMgr Cache',
            'href'  => false,
        ]);

        // Purge entire cache — always available.
        $bar->add_node([
            'parent' => 'wpmgr-cache',
            'id'     => 'wpmgr-purge-all',
            'title'  => 'Purge entire cache',
            'href'   => wp_nonce_url(
                admin_url('admin-post.php?action=' . self::ACTION_PURGE_ALL),
                self::ACTION_PURGE_ALL
            ),
        ]);

        // Purge this page — only on singular/home front-end views.
        if (!is_admin() && (is_singular() || is_front_page() || is_home())) {
            $current = $this->currentFrontEndUrl();
            if ($current !== '') {
                $bar->add_node([
                    'parent' => 'wpmgr-cache',
                    'id'     => 'wpmgr-purge-url',
                    'title'  => 'Purge this page',
                    'href'   => wp_nonce_url(
                        add_query_arg(
                            'url',
                            rawurlencode($current),
                            admin_url('admin-post.php?action=' . self::ACTION_PURGE_URL)
                        ),
                        self::ACTION_PURGE_URL
                    ),
                ]);
            }
        }

        // Deep link to cache config in the dashboard (control-plane owned).
        $dash = $this->dashboardCacheUrl();
        if ($dash !== '') {
            $bar->add_node([
                'parent' => 'wpmgr-cache',
                'id'     => 'wpmgr-manage',
                'title'  => 'Manage in WPMgr',
                'href'   => $dash,
                'meta'   => ['target' => '_blank', 'rel' => 'noopener'],
            ]);
        }
    }

    /**
     * Handle admin_post_wpmgr_cache_purge_all.
     * Capability + nonce gated. Purges the entire page cache.
     */
    public function handlePurgeAll(): void
    {
        $this->guard(self::ACTION_PURGE_ALL);
        $result = $this->cacheManager->purge('');
        $ok     = $result['ok'] ?? false;
        $this->notice(
            $ok ? 'success' : 'error',
            $ok ? 'Entire cache purged.' : 'Cache purge failed.'
        );
        $this->redirectAfterPurge();
    }

    /**
     * Handle admin_post_wpmgr_cache_purge_url.
     * Capability + nonce gated. Purges the cache entry for the given URL.
     */
    public function handlePurgeUrl(): void
    {
        $this->guard(self::ACTION_PURGE_URL);
        // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- nonce + capability verified in guard()/check_admin_referer() at top of handler
        $raw = isset($_GET['url']) ? rawurldecode(sanitize_text_field(wp_unslash($_GET['url']))) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- nonce + capability verified in guard()/check_admin_referer() at top of handler
        $url = $this->safeSameHostUrl($raw);
        if ($url === '') {
            $this->notice('error', 'Could not determine the page to purge.');
            $this->redirectAfterPurge();
            return;
        }
        $this->cacheManager->purge($url);
        $this->notice('success', 'This page was purged from the cache.');
        $this->redirectAfterPurge();
    }

    /**
     * Render and clear any queued one-shot admin notice.
     */
    public function renderNotice(): void
    {
        $notice = get_transient(self::NOTICE_TRANSIENT);
        if (!is_array($notice) || !isset($notice['type'], $notice['message'])) {
            return;
        }
        delete_transient(self::NOTICE_TRANSIENT);
        $class = $notice['type'] === 'success' ? 'notice-success' : 'notice-error';
        echo '<div class="notice ' . esc_attr($class) . ' is-dismissible"><p>'
            . esc_html((string) $notice['message']) . '</p></div>';
    }

    /**
     * Add a 'Purge cache' link to this plugin's row on the Plugins screen.
     *
     * @param list<string> $links Existing plugin action links.
     * @return list<string>
     */
    public function addPluginRowLink(array $links): array
    {
        if (!current_user_can('manage_options')) {
            return $links;
        }
        $url  = wp_nonce_url(
            admin_url('admin-post.php?action=' . self::ACTION_PURGE_ALL),
            self::ACTION_PURGE_ALL
        );
        array_unshift($links, '<a href="' . esc_url($url) . '">Purge cache</a>');
        return $links;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Capability + nonce gate — identical contract to Admin::guard().
     *
     * @param string $action Nonce/action name.
     */
    private function guard(string $action): void
    {
        if (!current_user_can('manage_options')) {
            wp_die('Insufficient permissions.', '', ['response' => 403]);
        }
        check_admin_referer($action);
    }

    /**
     * Queue a one-shot admin notice (60-second transient).
     *
     * @param string $type    'success' | 'error'.
     * @param string $message Human-readable message.
     */
    private function notice(string $type, string $message): void
    {
        set_transient(self::NOTICE_TRANSIENT, ['type' => $type, 'message' => $message], 60);
    }

    /**
     * Redirect to the HTTP Referer after a purge action.
     * Falls back to the WordPress admin dashboard.
     */
    private function redirectAfterPurge(): void
    {
        wp_safe_redirect(wp_get_referer() ?: admin_url());
        exit;
    }

    /**
     * Resolve the canonical front-end URL of the queried object.
     * Returns '' when no concrete URL can be determined.
     */
    private function currentFrontEndUrl(): string
    {
        $obj = get_queried_object();
        if ($obj instanceof \WP_Post) {
            $link = get_permalink($obj);
            if (is_string($link) && $link !== '') {
                return $link;
            }
        }
        if (is_front_page() || is_home()) {
            return home_url('/');
        }
        return '';
    }

    /**
     * Validate that $candidate is an absolute http(s) URL on this site's host.
     * Returns '' if the URL is malformed, cross-host, or empty.
     *
     * @param string $candidate Raw URL string (already rawurldecoded + unslashed).
     * @return string Sanitized URL or ''.
     */
    private function safeSameHostUrl(string $candidate): string
    {
        $candidate = esc_url_raw(trim($candidate));
        if ($candidate === '') {
            return '';
        }
        $host = wp_parse_url($candidate, PHP_URL_HOST);
        $self = wp_parse_url(home_url(), PHP_URL_HOST);
        if (!is_string($host) || !is_string($self) || strcasecmp($host, $self) !== 0) {
            return '';
        }
        return $candidate;
    }

    /**
     * Build the deep-link URL to the cache section in the WPMgr dashboard.
     * Returns '' when the site is not enrolled or the CP URL is not set.
     */
    private function dashboardCacheUrl(): string
    {
        $cpUrl  = $this->settings->controlPlaneUrl();
        $siteId = $this->settings->siteId();
        if ($cpUrl === '' || $siteId === '') {
            return '';
        }
        return rtrim($cpUrl, '/') . '/sites/' . rawurlencode($siteId) . '/cache';
    }

    /**
     * Return the plugin basename for use in the plugin_action_links filter.
     * Gracefully handles test contexts where WPMGR_AGENT_FILE is not defined.
     */
    private function pluginBasename(): string
    {
        if (!defined('WPMGR_AGENT_FILE')) {
            return '';
        }
        return plugin_basename((string) constant('WPMGR_AGENT_FILE'));
    }
}
