<?php
/**
 * PerfConfigUpdateCommand — applies a new page-cache configuration.
 *
 * Re-renders the inlined drop-in config and re-evaluates the .htaccess mobile
 * flag, and re-arms the scheduled refresh interval — without toggling the
 * enabled state unless the payload explicitly sets it.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/perf_config_update
 *   Authorization: Bearer <Ed25519 JWT, cmd="perf_config_update", aud=<siteId>>
 *   Body: {
 *     "enabled":          <bool>?,    // optional; preserves current if omitted
 *     "cache_logged_in":  <bool>,
 *     "cache_mobile":     <bool>,
 *     "auto_purge":       <bool>,
 *     "refresh_interval": <int seconds>,
 *     "include_queries":  string[],
 *     "include_cookies":  string[],
 *     "bypass_urls":      string[],
 *     "bypass_cookies":   string[]
 *   }
 *
 * Response: { "ok": <bool>, "detail": "<text>", "stats": {...} }
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\DropinInstaller;
use WPMgr\Agent\Cache\HtaccessManager;
use WPMgr\Agent\Cache\PerfReporter;
use WPMgr\Agent\Cache\WpCacheConstant;
use WPMgr\Agent\Optimizer\PerfConfig;

/**
 * Re-renders the page-cache config (drop-in + .htaccess flag + refresh cron).
 */
final class PerfConfigUpdateCommand implements CommandInterface
{
    /** Recognised config keys (anything else is ignored). */
    private const KNOWN_KEYS = [
        'enabled', 'cache_logged_in', 'cache_mobile', 'auto_purge',
        'refresh_interval', 'include_queries', 'include_cookies',
        'bypass_urls', 'bypass_cookies', 'woo_cacheable_session',
        'fonts_transcode_woff2',
    ];

    /** Hard ceiling on the refresh interval (30 days) to reject absurd values. */
    private const MAX_REFRESH = 2592000;

    private CacheManager $cache;

    private ?PerfReporter $reporter;

    /**
     * @param CacheManager      $cache    Page-cache orchestrator.
     * @param PerfReporter|null $reporter Optional reporter for async state push.
     */
    public function __construct(CacheManager $cache, ?PerfReporter $reporter = null)
    {
        $this->cache    = $cache;
        $this->reporter = $reporter;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'perf_config_update';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Config map (see header).
     * @return array{ok:bool,detail:string,stats?:array<string,mixed>}
     */
    public function execute(array $claims, array $params): array
    {
        // Phase 4 — persist the OPTIMIZATION-layer config (CSS/JS/font/image/
        // bloat/CDN/DB flags) into its own wp-option when present. These keys are
        // disjoint from the cache keys below; PerfConfig drops anything unknown.
        // Done first + independently so an optimization-only push (no cache keys)
        // still lands and the request-path optimizer picks it up on next render.
        $optimizationApplied = $this->maybePersistOptimizationConfig($params);

        // Whitelist the keys so a malformed push cannot inject unknown state.
        $clean = [];
        foreach (self::KNOWN_KEYS as $key) {
            if (array_key_exists($key, $params)) {
                $clean[$key] = $params[$key];
            }
        }

        // Persist the config_version from the CP payload when present.
        if (isset($params['config_version'])) {
            PerfReporter::persistConfigVersion((int) $params['config_version']);
        }

        if ($clean === []) {
            if ($optimizationApplied) {
                // Fire-and-forget state report after optimization-only push.
                if ($this->reporter !== null) {
                    $this->reporter->reportInstallState();
                    $this->reporter->reportStats();
                }
                return ['ok' => true, 'detail' => 'optimization config applied'];
            }
            return ['ok' => false, 'detail' => 'no recognised config fields'];
        }

        // Bound the refresh interval.
        if (isset($clean['refresh_interval'])) {
            $interval = (int) $clean['refresh_interval'];
            if ($interval < 0) {
                $interval = 0;
            }
            if ($interval > self::MAX_REFRESH) {
                $interval = self::MAX_REFRESH;
            }
            $clean['refresh_interval'] = $interval;
        }

        try {
            $result = $this->cache->applyConfig($clean);
            $result['stats'] = $this->cache->stats();

            // Additive top-level install-state fields.
            $result = $this->addInstallStateFields($result);

            // Fire-and-forget async report.
            if ($this->reporter !== null) {
                $this->reporter->reportInstallState();
                $this->reporter->reportStats();
            }

            return $result;
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'perf config update failed'];
        }
    }

    /**
     * Append top-level install-state fields to the response envelope (mirrors
     * the same helper in CacheEnableCommand, kept here so both commands are
     * independently complete with no shared base class).
     *
     * @param array<string,mixed> $result Current response envelope.
     * @return array<string,mixed>
     */
    private function addInstallStateFields(array $result): array
    {
        $result['server_software'] = isset($_SERVER['SERVER_SOFTWARE']) && is_string($_SERVER['SERVER_SOFTWARE'])
            ? sanitize_text_field(wp_unslash($_SERVER['SERVER_SOFTWARE']))
            : '';

        $dropin      = new DropinInstaller();
        $dropinPath  = $dropin->dropinPath();
        $dropinInstalled = false;
        if ($dropinPath !== '' && @is_file($dropinPath)) {
            $content = @file_get_contents($dropinPath);
            $dropinInstalled = $content !== false && strpos($content, DropinInstaller::SIGNATURE) !== false;
        }
        $result['dropin_installed'] = $dropinInstalled;

        $stepOk = (bool) ($result['steps']['wp_cache'] ?? false);
        $result['wp_cache_constant_set'] = $stepOk || (defined('WP_CACHE') && (bool) constant('WP_CACHE'));

        $htaccess = new HtaccessManager();
        $htaccessManaged = false;
        if (!$htaccess->isNginx()) {
            $root = '';
            if (function_exists('get_home_path')) {
                $candidate = get_home_path();
                if (is_string($candidate) && $candidate !== '') {
                    $root = $candidate;
                }
            }
            if ($root === '' && defined('ABSPATH')) {
                $root = (string) constant('ABSPATH');
            }
            if ($root !== '') {
                $path = rtrim($root, '/\\') . DIRECTORY_SEPARATOR . '.htaccess';
                if (@is_file($path)) {
                    $content = @file_get_contents($path);
                    $htaccessManaged = $content !== false && strpos($content, HtaccessManager::BEGIN) !== false;
                }
            }
        }
        $result['htaccess_managed'] = $htaccessManaged;

        if ($stepOk === false && isset($result['steps']['wp_cache'])) {
            $wpCache = new WpCacheConstant();
            if (!$wpCache->isWritable()) {
                $result['detail'] = ($result['detail'] ?? '')
                    . " WP_CACHE could not be written to wp-config.php"
                    . " — add: define('WP_CACHE', true);";
            }
        }

        return $result;
    }

    /**
     * Persist the optimization-layer config when the push carries any of its
     * keys. PerfConfig normalises the payload (clamps enums, drops unknowns), so
     * a malformed push cannot inject state. Merges over the stored config so a
     * partial push only changes the supplied fields.
     *
     * @param array<string,mixed> $params Raw request params.
     * @return bool Whether an optimization config was written.
     */
    private function maybePersistOptimizationConfig(array $params): bool
    {
        $current = PerfConfig::load()->toArray();
        // Only the keys PerfConfig recognises (intersection with the payload).
        $intersection = array_intersect_key($params, $current);
        if ($intersection === []) {
            return false;
        }

        // GH #174 defense-in-depth: array_intersect_key keys on PRESENCE, not
        // value, so a present-but-EMPTY rum_beacon_key would silently clobber a
        // good stored key with "". The CP relies on omitempty to drop the field
        // when unchanged, but the agent must be robust regardless of what the
        // wire actually sends. Treat an empty incoming beacon key as "leave the
        // stored value unchanged" — mirroring the CP's omit-means-unchanged
        // contract. A non-empty incoming key still overwrites normally
        // (rotation / first mint).
        if (array_key_exists('rum_beacon_key', $intersection)
            && $intersection['rum_beacon_key'] === ''
            && ($current['rum_beacon_key'] ?? '') !== ''
        ) {
            unset($intersection['rum_beacon_key']);
        }

        $merged = new PerfConfig(array_merge($current, $intersection));
        if (function_exists('update_option')) {
            update_option(PerfConfig::OPTION, $merged->toArray(), false);
        }
        return true;
    }
}
