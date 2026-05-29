<?php
/**
 * Metadata command: collects the full site inventory pushed to the control
 * plane's /agent/v1/metadata endpoint.
 *
 * Gathers WP/PHP versions, server software, the active theme, all installed
 * plugins + versions, all installed themes + versions, and the multisite flag.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\AgeIdentity;

/**
 * Builds the site metadata payload.
 */
final class MetadataCommand implements CommandInterface
{
    /**
     * @param AgeIdentity|null $ageIdentity Optional. When provided, the agent's
     *   per-site age PUBLIC recipient is included in the payload so the control
     *   plane stores it on sites.age_recipient (M4 backups need it). Null is
     *   accepted for back-compat with code paths that don't have the keystore
     *   wired (existing tests).
     */
    public function __construct(private ?AgeIdentity $ageIdentity = null)
    {
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'metadata';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters (unused).
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        return $this->collect();
    }

    /**
     * Collect the full metadata payload.
     *
     * The `available_update` per-plugin / per-theme field and the top-level
     * `core_update` field are added in v0.9.0 to surface what WordPress's own
     * update transients have already discovered (the agent does not initiate
     * the WP.org calls here — Scheduler force-refreshes them with a 5-minute
     * lock so the data is fresh without thrashing). Both fields are always
     * present and EXPLICITLY null when no update is known (never absent), so
     * the CP can tell "no update" apart from "agent too old to report".
     *
     * @return array{
     *     wp_version:string,
     *     php_version:string,
     *     server_info:string,
     *     multisite:bool,
     *     active_theme:string,
     *     plugins:array<int,array{slug:string,name:string,version:string,active:bool,available_update:?array{new_version:string,package:?string,tested:?string,requires_php:?string}}>,
     *     themes:array<int,array{slug:string,name:string,version:string,active:bool,available_update:?array{new_version:string,package:?string,tested:?string,requires_php:?string}}>,
     *     core_update:?array{new_version:string,current_version:string},
     *     age_recipient?:string
     * }
     */
    public function collect(): array
    {
        $payload = [
            'wp_version'   => $this->wpVersion(),
            'php_version'  => PHP_VERSION,
            'server_info'  => $this->serverSoftware(),
            'multisite'    => function_exists('is_multisite') ? is_multisite() : false,
            'active_theme' => $this->activeTheme(),
            'plugins'      => $this->plugins(),
            'themes'       => $this->themes(),
            'core_update'  => $this->coreUpdate(),
        ];
        // Surface the agent's age PUBLIC recipient so the CP can register it on
        // sites.age_recipient (M4 backups refuse otherwise). Best-effort: a
        // failing ensureRecipient just leaves the key absent for this push.
        if ($this->ageIdentity !== null) {
            try {
                $recipient = $this->ageIdentity->ensureRecipient();
                if ($recipient !== '') {
                    $payload['age_recipient'] = $recipient;
                }
            } catch (\Throwable $e) {
                // Swallow — telemetry must not fail the sync.
            }
        }
        return $payload;
    }

    /**
     * Resolve the WordPress core version.
     *
     * @return string
     */
    private function wpVersion(): string
    {
        if (function_exists('get_bloginfo')) {
            $version = get_bloginfo('version');
            if (is_string($version) && $version !== '') {
                return $version;
            }
        }

        return isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])
            ? (string) $GLOBALS['wp_version']
            : 'unknown';
    }

    /**
     * Read the web-server software string (never trusted as input).
     *
     * @return string
     */
    private function serverSoftware(): string
    {
        $value = $_SERVER['SERVER_SOFTWARE'] ?? '';

        return is_string($value) ? $value : '';
    }

    /**
     * Ensure get_plugins()/wp_get_themes() are available by loading the admin
     * plugin helpers when running outside an admin request.
     *
     * @return void
     */
    private function ensurePluginApi(): void
    {
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
    }

    /**
     * Collect all installed plugins with version + active flag + pending update.
     *
     * `available_update` is null when no WP.org update is pending and the
     * shape `{new_version,package,tested,requires_php}` when one is. Values
     * come straight from the `update_plugins` site transient — the Scheduler
     * force-refreshes that transient before the metadata push so dashboards
     * see fresh data within the 30-minute cadence.
     *
     * @return array<int,array{slug:string,name:string,version:string,active:bool,available_update:?array{new_version:string,package:?string,tested:?string,requires_php:?string}}>
     */
    private function plugins(): array
    {
        $this->ensurePluginApi();

        $active = function_exists('get_option') ? get_option('active_plugins') : [];
        if (!is_array($active)) {
            $active = [];
        }
        $activeSet = [];
        foreach ($active as $a) {
            if (is_string($a)) {
                $activeSet[$a] = true;
            }
        }

        $all = function_exists('get_plugins') ? get_plugins() : [];
        if (!is_array($all)) {
            $all = [];
        }

        $updates = $this->pluginUpdateMap();

        $out = [];
        foreach ($all as $file => $meta) {
            if (!is_string($file)) {
                continue;
            }
            $meta   = is_array($meta) ? $meta : [];
            $update = $updates[$file] ?? null;
            $out[] = [
                'slug'             => $file,
                'name'             => isset($meta['Name']) ? (string) $meta['Name'] : $file,
                'version'          => isset($meta['Version']) ? (string) $meta['Version'] : 'unknown',
                'active'           => isset($activeSet[$file]),
                'available_update' => $this->normalizeAvailableUpdate($update),
            ];
        }

        return $out;
    }

    /**
     * Collect all installed themes with versions + pending update.
     *
     * `available_update` mirrors the plugin shape. Theme transient entries
     * are ARRAYS (not objects like plugins), so the keys are accessed via
     * array-index in {@see normalizeAvailableUpdate()}.
     *
     * @return array<int,array{slug:string,name:string,version:string,active:bool,available_update:?array{new_version:string,package:?string,tested:?string,requires_php:?string}}>
     */
    private function themes(): array
    {
        if (!function_exists('wp_get_themes')) {
            return [];
        }

        $themes = wp_get_themes();
        if (!is_array($themes)) {
            return [];
        }

        $activeStylesheet = function_exists('get_stylesheet') ? (string) get_stylesheet() : '';
        $updates          = $this->themeUpdateMap();

        $out = [];
        foreach ($themes as $stylesheet => $theme) {
            if (!is_object($theme) || !method_exists($theme, 'get')) {
                continue;
            }
            $slug   = (string) $stylesheet;
            $update = $updates[$slug] ?? null;
            $out[]  = [
                'slug'             => $slug,
                'name'             => (string) $theme->get('Name'),
                'version'          => (string) $theme->get('Version'),
                'active'           => $slug === $activeStylesheet,
                'available_update' => $this->normalizeAvailableUpdate($update),
            ];
        }

        return $out;
    }

    /**
     * Read the `update_plugins` site transient's `response` map. Plugin entries
     * are typically stdClass objects with `new_version`, `package`, `tested`,
     * `requires_php` fields.
     *
     * @return array<string,object|array<string,mixed>>
     */
    private function pluginUpdateMap(): array
    {
        if (!function_exists('get_site_transient')) {
            return [];
        }
        $transient = get_site_transient('update_plugins');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return [];
        }
        $out = [];
        foreach ($transient->response as $file => $entry) {
            if (!is_string($file)) {
                continue;
            }
            if (is_object($entry) || is_array($entry)) {
                $out[$file] = $entry;
            }
        }
        return $out;
    }

    /**
     * Read the `update_themes` site transient's `response` map. Theme entries
     * are typically associative arrays (not objects), e.g.
     *   ['theme' => 'slug', 'new_version' => '...', 'package' => '...'].
     *
     * @return array<string,object|array<string,mixed>>
     */
    private function themeUpdateMap(): array
    {
        if (!function_exists('get_site_transient')) {
            return [];
        }
        $transient = get_site_transient('update_themes');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return [];
        }
        $out = [];
        foreach ($transient->response as $stylesheet => $entry) {
            if (!is_string($stylesheet)) {
                continue;
            }
            if (is_object($entry) || is_array($entry)) {
                $out[$stylesheet] = $entry;
            }
        }
        return $out;
    }

    /**
     * Normalize a per-item update entry (object OR array) into the contract
     * shape, or null when no entry is present / lacks a usable `new_version`.
     *
     * @param object|array<string,mixed>|null $entry The transient entry, if any.
     * @return array{new_version:string,package:?string,tested:?string,requires_php:?string}|null
     */
    private function normalizeAvailableUpdate(object|array|null $entry): ?array
    {
        if ($entry === null) {
            return null;
        }
        $get = static function (string $key) use ($entry) {
            if (is_object($entry)) {
                return $entry->{$key} ?? null;
            }
            return $entry[$key] ?? null;
        };

        $newVersion = $get('new_version');
        if (!is_string($newVersion) && !is_numeric($newVersion)) {
            return null;
        }
        $newVersion = (string) $newVersion;
        if ($newVersion === '') {
            return null;
        }

        $package     = $get('package');
        $tested      = $get('tested');
        $requiresPhp = $get('requires_php');

        return [
            'new_version'  => $newVersion,
            'package'      => is_scalar($package)     && (string) $package     !== '' ? (string) $package     : null,
            'tested'       => is_scalar($tested)      && (string) $tested      !== '' ? (string) $tested      : null,
            'requires_php' => is_scalar($requiresPhp) && (string) $requiresPhp !== '' ? (string) $requiresPhp : null,
        ];
    }

    /**
     * Top-level core update summary. Returns null when no upgrade is offered.
     *
     * Reads `get_core_updates()` (admin-side helper) when available; falls
     * back to scanning the `update_core` site transient so this method works
     * during non-admin REST/cron contexts too. The "current_version" field is
     * resolved via {@see wpVersion()} so dashboards can render a "x.y.z to
     * a.b.c" diff without a second metadata lookup.
     *
     * @return array{new_version:string,current_version:string}|null
     */
    private function coreUpdate(): ?array
    {
        $current = $this->wpVersion();

        $candidates = [];

        if (!function_exists('get_core_updates')
            && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/update.php')
        ) {
            require_once ABSPATH . 'wp-admin/includes/update.php';
        }

        if (function_exists('get_core_updates')) {
            $cores = get_core_updates();
            if (is_array($cores)) {
                $candidates = $cores;
            }
        }

        // Fallback: scan the raw transient if get_core_updates is unavailable
        // or returned no rows (typical on non-admin paths).
        if ($candidates === [] && function_exists('get_site_transient')) {
            $transient = get_site_transient('update_core');
            if (is_object($transient) && isset($transient->updates) && is_array($transient->updates)) {
                $candidates = $transient->updates;
            }
        }

        foreach ($candidates as $update) {
            if (!is_object($update)) {
                continue;
            }
            $response = isset($update->response) ? (string) $update->response : '';
            if ($response !== 'upgrade') {
                continue;
            }
            $newVersion = isset($update->version) && is_scalar($update->version)
                ? (string) $update->version
                : (isset($update->current) && is_scalar($update->current) ? (string) $update->current : '');
            if ($newVersion === '') {
                continue;
            }
            return [
                'new_version'     => $newVersion,
                'current_version' => $current,
            ];
        }

        return null;
    }

    /**
     * The active theme as a STRING (its stylesheet slug). The contract's
     * active_theme field is a string; previously this returned an array, which
     * serialized as a JSON object and was rejected by the control plane (422).
     */
    private function activeTheme(): string
    {
        if (function_exists('get_stylesheet')) {
            $slug = (string) get_stylesheet();
            if ($slug !== '') {
                return $slug;
            }
        }

        if (function_exists('wp_get_theme')) {
            $theme = wp_get_theme();
            if (is_object($theme) && method_exists($theme, 'get_stylesheet')) {
                return (string) $theme->get_stylesheet();
            }
        }

        return 'unknown';
    }
}
