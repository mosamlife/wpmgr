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
            'wp_version'    => $this->wpVersion(),
            'php_version'   => PHP_VERSION,
            'agent_version' => defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '',
            'server_info'   => $this->serverSoftware(),
            'multisite'     => function_exists('is_multisite') ? is_multisite() : false,
            'active_theme'  => $this->activeTheme(),
            'plugins'      => $this->plugins(),
            'themes'       => $this->themes(),
            'core_update'  => $this->coreUpdate(),
            // ADR-037 Sprint 1, 1C — sparse-metadata expansion. All fields
            // below are OPTIONAL on the wire: CP tolerantly decodes (additive,
            // backward-compatible). Adds hosting-platform fingerprint,
            // user-count signals, and a sampled disk-usage snapshot so the CP
            // can render a Health card without a separate diagnostics call.
            'host_flags'   => $this->hostFlags(),
            'disk'         => $this->diskUsage(),
            'user_count'   => $this->userCount(),
            'admin_count'  => $this->adminCount(),
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

        // Outcome of the last self-update APPLY beat, when there is one. The
        // apply runs in a cron request that has no CP response to ride on, so
        // the next metadata push is how a FAILED apply reaches the control
        // plane at all. Only ever present on a site that actually staged one.
        $selfUpdate = $this->selfUpdateResult();
        if ($selfUpdate !== null) {
            $payload['agent_self_update'] = $selfUpdate;
        }

        return $payload;
    }

    /**
     * Read the last self-update apply outcome, if any.
     *
     * Reads the plain wp-option by name rather than going through the
     * self-updater: that class is not shipped in the wp.org distribution build,
     * and this collector runs in both builds. The option key constant lives on
     * AgentSelfUpdateCommand, which is present in both.
     *
     * @return array{status:string,from_version:string,to_version:string,detail:string,at:int}|null
     */
    private function selfUpdateResult(): ?array
    {
        if (!function_exists('get_option')) {
            return null;
        }

        $stored = get_option(AgentSelfUpdateCommand::OPTION_RESULT, null);
        if (!is_array($stored)) {
            return null;
        }

        $status = isset($stored['status']) && is_string($stored['status']) ? $stored['status'] : '';
        if ($status === '') {
            return null;
        }

        return [
            'status'       => $status,
            'from_version' => isset($stored['from_version']) && is_string($stored['from_version']) ? $stored['from_version'] : '',
            'to_version'   => isset($stored['to_version'])   && is_string($stored['to_version'])   ? $stored['to_version']   : '',
            'detail'       => isset($stored['detail'])       && is_string($stored['detail'])       ? $stored['detail']       : '',
            'at'           => isset($stored['at']) && is_numeric($stored['at']) ? (int) $stored['at'] : 0,
        ];
    }

    /**
     * ADR-037 Sprint 1, 1C — hosting-platform auto-detect.
     *
     * Best-effort defined()-based probes for the eight major managed-WP
     * platforms. None of these constants are documented public API — they're
     * "fingerprint" defines hosts ship in the mu-plugin layer or wp-config
     * for their own internal use. Probing them in this order is the same
     * pattern used by leading site-management plugins and is robust against
     * private variants of the same constant name.
     *
     * @return array<string,bool>
     */
    private function hostFlags(): array
    {
        return [
            'is_pressable' => defined('IS_PRESSABLE') || defined('PRESSABLE_SITE'),
            'is_gridpane'  => defined('GRIDPANE') || defined('GRIDPANE_VERSION'),
            'is_wpengine'  => defined('WPE_APIKEY') || defined('WPE_PLUGIN_BASE') || function_exists('is_wpe'),
            'is_atomic'    => defined('IS_ATOMIC') || defined('ATOMIC_SITE_ID'),
            'is_kinsta'    => defined('KINSTAMU_VERSION') || defined('KINSTA_CACHE_ZONE'),
            'is_flywheel'  => defined('FLYWHEEL_CONFIG_DIR') || defined('FLYWHEEL_PLUGIN_DIR'),
            'is_runcloud'  => defined('RUNCLOUD') || @is_dir('/var/lib/runcloud'),
            'is_cloudways' => defined('CLOUDWAYS') || isset($_SERVER['CW_HOSTNAME']),
        ];
    }

    /**
     * Sampled disk-usage snapshot. Walks wp-content and uploads with a 2-second
     * wall-clock cap each — representative for the Health card, not exhaustive.
     *
     * @return array{wp_content_bytes:int,uploads_bytes:int,free_bytes:int}
     */
    private function diskUsage(): array
    {
        $wpContent = defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '';
        $uploadsDir = '';
        if (function_exists('wp_upload_dir')) {
            $upl = wp_upload_dir();
            if (is_array($upl) && isset($upl['basedir']) && is_string($upl['basedir'])) {
                $uploadsDir = $upl['basedir'];
            }
        }
        $freeBytes = 0;
        if ($wpContent !== '' && function_exists('disk_free_space')) {
            $free = @disk_free_space($wpContent);
            if ($free !== false) {
                $freeBytes = (int) $free;
            }
        }
        return [
            'wp_content_bytes' => $wpContent !== '' ? $this->duCapped($wpContent, 2) : 0,
            'uploads_bytes'    => $uploadsDir !== '' ? $this->duCapped($uploadsDir, 2) : 0,
            'free_bytes'       => $freeBytes,
        ];
    }

    /**
     * Capped recursive disk-usage walk. Bails at $maxSeconds; returns 0 on
     * iterator failure. Identical pattern to BackupCommand::duCapped.
     */
    private function duCapped(string $path, int $maxSeconds): int
    {
        if (!is_dir($path)) {
            return 0;
        }
        $bytes = 0;
        $deadline = microtime(true) + $maxSeconds;
        try {
            $it = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator($path, \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::FOLLOW_SYMLINKS),
                \RecursiveIteratorIterator::LEAVES_ONLY,
                \RecursiveIteratorIterator::CATCH_GET_CHILD
            );
            foreach ($it as $file) {
                if (microtime(true) > $deadline) {
                    break;
                }
                if ($file instanceof \SplFileInfo && $file->isFile()) {
                    $bytes += (int) $file->getSize();
                }
            }
        } catch (\Throwable $e) {
            // Swallow.
        }
        return $bytes;
    }

    /**
     * Total user count (every role). Returns 0 on a non-WP runtime.
     */
    private function userCount(): int
    {
        if (!function_exists('get_users')) {
            return 0;
        }
        try {
            $users = get_users(['fields' => 'ID', 'number' => -1]);
            return is_array($users) ? count($users) : 0;
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Administrator-role user count. Surfaced separately from `user_count`
     * so the CP can render an at-risk indicator (e.g. "12 administrators" is
     * an audit signal of a sprawling site).
     */
    private function adminCount(): int
    {
        if (!function_exists('get_users')) {
            return 0;
        }
        try {
            $admins = get_users(['role' => 'administrator', 'fields' => 'ID', 'number' => -1]);
            return is_array($admins) ? count($admins) : 0;
        } catch (\Throwable $e) {
            return 0;
        }
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
        $value = isset($_SERVER['SERVER_SOFTWARE']) ? sanitize_text_field(wp_unslash($_SERVER['SERVER_SOFTWARE'])) : '';

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
            $installedVersion = isset($meta['Version']) ? (string) $meta['Version'] : 'unknown';
            $out[] = [
                'slug'             => $file,
                'name'             => isset($meta['Name']) ? (string) $meta['Name'] : $file,
                'version'          => $installedVersion,
                'active'           => isset($activeSet[$file]),
                'available_update' => $this->normalizeAvailableUpdate($update, $installedVersion),
                // ADR-037 Sprint 1, 1C — sparse-metadata expansion. Optional
                // fields surfaced from get_plugins(); empty string when the
                // plugin header omits them. CP tolerantly decodes.
                'plugin_uri'       => isset($meta['PluginURI']) ? (string) $meta['PluginURI'] : '',
                'update_uri'       => isset($meta['UpdateURI']) ? (string) $meta['UpdateURI'] : '',
                'author_uri'       => isset($meta['AuthorURI']) ? (string) $meta['AuthorURI'] : '',
                'network'          => isset($meta['Network']) ? (bool) $meta['Network'] : false,
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
            $installedVersion = (string) $theme->get('Version');
            $out[]  = [
                'slug'             => $slug,
                'name'             => (string) $theme->get('Name'),
                'version'          => $installedVersion,
                'active'           => $slug === $activeStylesheet,
                'available_update' => $this->normalizeAvailableUpdate($update, $installedVersion),
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
     * shape, or null when no entry is present / lacks a usable `new_version`,
     * OR when the offered `new_version` is not strictly newer than what is
     * already installed (GH #211 — WordPress's own update transients can
     * carry a stale/duplicate response entry whose `new_version` equals or
     * trails the installed version; surfacing that as an "available update"
     * is a phantom the operator cannot act on).
     *
     * Both sides are lightly normalized before comparison (trimmed, a single
     * leading `v`/`V` stripped) so a header like `v1.5.1` compares equal to a
     * transient `1.5.1`. No further normalization is applied — a prerelease
     * or build suffix (`1.5.1-beta`) is left intact for version_compare() to
     * arbitrate, so a genuinely newer prerelease still surfaces.
     *
     * An installed version that is empty or the `'unknown'` sentinel (see
     * plugins()) cannot be compared, so the guard fails OPEN in that case and
     * keeps the entry — over-reporting is preferable to hiding a real update
     * because the installed version was unreadable.
     *
     * @param object|array<string,mixed>|null $entry            The transient entry, if any.
     * @param string                          $installedVersion The currently-installed version, for the phantom-update guard.
     * @return array{new_version:string,package:?string,tested:?string,requires_php:?string}|null
     */
    private function normalizeAvailableUpdate(object|array|null $entry, string $installedVersion): ?array
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

        if ($installedVersion !== '' && $installedVersion !== 'unknown') {
            $normNew       = $this->normalizeVersionForCompare($newVersion);
            $normInstalled = $this->normalizeVersionForCompare($installedVersion);
            if (version_compare($normNew, $normInstalled, '<=')) {
                return null;
            }
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
     * Lightly normalize a version string for the phantom-update comparison:
     * trims whitespace and strips a single leading `v`/`V` (e.g. `v1.5.1` ->
     * `1.5.1`). Deliberately does NOT strip prerelease/build suffixes — those
     * must remain visible to version_compare() so a real prerelease bump
     * still surfaces as an update.
     *
     * @param string $version Raw version string.
     * @return string Normalized version string.
     */
    private function normalizeVersionForCompare(string $version): string
    {
        $trimmed = trim($version);
        if ($trimmed !== '' && ($trimmed[0] === 'v' || $trimmed[0] === 'V')) {
            $trimmed = substr($trimmed, 1);
        }
        return $trimmed;
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

            // GH #211 sibling guard: only surface a core update when the
            // offered version is strictly newer than what's installed, // mirrors normalizeAvailableUpdate()'s phantom-suppression so a
            // stale/duplicate 'upgrade' response entry does not report a
            // same-version "update". Fails OPEN (keeps the entry) when
            // $current is empty/unreadable.
            if ($current !== '' && $current !== 'unknown') {
                $normNew     = $this->normalizeVersionForCompare($newVersion);
                $normCurrent = $this->normalizeVersionForCompare($current);
                if (version_compare($normNew, $normCurrent, '<=')) {
                    continue;
                }
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
