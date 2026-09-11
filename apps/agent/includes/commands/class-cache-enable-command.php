<?php
/**
 * CacheEnableCommand — turns page caching on for this site.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/cache_enable
 *   Authorization: Bearer <Ed25519 JWT, cmd="cache_enable", aud=<siteId>>
 *   Body: {} (optional config keys may be included; applied before enabling)
 *
 * Response: { "ok": <bool>, "detail": "<text>", "steps": {...}, "stats": {...} }
 *
 * Auth: the Router's permission_callback already enforced the signed JWT +
 * anti-replay contract (Connector::verifyCommand) before execute() runs.
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

/**
 * Enables the page cache (WP_CACHE + drop-in + .htaccess).
 */
final class CacheEnableCommand implements CommandInterface
{
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
        return 'cache_enable';
    }

    /**
     * Effect: writes the WP_CACHE define, installs the drop-in and the .htaccess block.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: enabling an already-enabled cache converges on the same state.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
    }

    /**
     * Optionally apply a pushed config, then enable caching.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here).
     * @param array<string,mixed> $params Decoded JSON body.
     * @return array{ok:bool,detail:string,steps?:array<string,bool>,stats?:array<string,mixed>}
     */
    public function execute(array $claims, array $params): array
    {
        try {
            // Pre-flight: disk-path caching keys the cache file on the request URL
            // path, which plain permalinks (?p=123) do not provide. Enabling on a
            // plain-permalink site would silently cache nothing and look broken, so
            // refuse with a clear, actionable reason instead.
            if (function_exists('get_option') && (string) \get_option('permalink_structure', '') === '') {
                return [
                    'ok'     => false,
                    'detail' => 'Caching requires pretty permalinks. Set Settings > Permalinks to anything other than Plain, then enable caching again.',
                ];
            }

            // Apply any inline config first so enable() installs with it.
            if ($params !== [] && $this->looksLikeConfig($params)) {
                $this->cache->applyConfig($params);
            }

            // Persist the config_version from the CP payload when present.
            if (isset($params['config_version'])) {
                PerfReporter::persistConfigVersion((int) $params['config_version']);
            }

            $result = $this->cache->enable();
            $result['stats'] = $this->cache->stats();

            // Additive top-level install-state fields the CP dashboard reads
            // directly from the command response (in addition to steps.*).
            $result = $this->addInstallStateFields($result);

            // Fire-and-forget async report so the dashboard is updated even if
            // the CP doesn't re-query via config-ack separately.
            if ($this->reporter !== null) {
                $this->reporter->reportInstallState();
                $this->reporter->reportStats();
            }

            return $result;
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'cache enable failed'];
        }
    }

    /**
     * Append top-level install-state fields to the enable/update response so
     * the CP dashboard card can show the real state without a separate config-ack
     * round-trip. Fields are additive — existing ok/detail/steps/stats are kept.
     *
     * Also surfaces a clear WP_CACHE remediation hint when wp-config.php is not
     * writable: the agent cannot patch a non-writable file, so we tell the
     * operator exactly which line to add manually.
     *
     * @param array<string,mixed> $result Current response envelope.
     * @return array<string,mixed>
     */
    private function addInstallStateFields(array $result): array
    {
        // server_software — raw SERVER_SOFTWARE header.
        $result['server_software'] = isset($_SERVER['SERVER_SOFTWARE']) && is_string($_SERVER['SERVER_SOFTWARE'])
            ? sanitize_text_field(wp_unslash($_SERVER['SERVER_SOFTWARE'])) // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.ValidatedSanitizedInput.MissingUnslash -- sanitize_text_field+wp_unslash applied; server-env read-only value reported to CP
            : '';

        // dropin_installed — our drop-in is on disk and signed by us.
        $dropin     = new DropinInstaller();
        $dropinPath = $dropin->dropinPath();
        $dropinInstalled = false;
        if ($dropinPath !== '' && @is_file($dropinPath)) {
            $content = @file_get_contents($dropinPath);
            $dropinInstalled = $content !== false && strpos($content, DropinInstaller::SIGNATURE) !== false;
        }
        $result['dropin_installed'] = $dropinInstalled;

        // wp_cache_constant_set — surface both the step result AND the runtime
        // constant presence; a non-writable wp-config.php will have steps['wp_cache']
        // === false but the constant might still be true from a previous manual add.
        $stepOk = (bool) ($result['steps']['wp_cache'] ?? false);
        $result['wp_cache_constant_set'] = $stepOk || (defined('WP_CACHE') && (bool) constant('WP_CACHE'));

        // htaccess_managed — block is present (false on nginx; expected).
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

        // WP_CACHE remediation: if the step failed and wp-config.php is not
        // writable, tell the operator exactly what to add manually.
        if (!$stepOk) {
            $wpCache = new WpCacheConstant();
            if (!$wpCache->isWritable()) {
                $result['detail'] = ($result['detail'] ?? '')
                    . " Caching is on but WP_CACHE could not be written to wp-config.php"
                    . " — add: define('WP_CACHE', true);";
            }
        }

        return $result;
    }

    /**
     * Whether the params map carries any recognised config key (so we only
     * call applyConfig when there is something to apply).
     *
     * @param array<string,mixed> $params Request params.
     * @return bool
     */
    private function looksLikeConfig(array $params): bool
    {
        $keys = [
            'enabled', 'cache_logged_in', 'cache_mobile', 'auto_purge',
            'refresh_interval', 'include_queries', 'include_cookies',
            'bypass_urls', 'bypass_cookies',
        ];
        foreach ($keys as $key) {
            if (array_key_exists($key, $params)) {
                return true;
            }
        }
        return false;
    }
}
