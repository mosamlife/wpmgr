<?php
/**
 * Settings: thin typed accessor over the plugin's wp-options.
 *
 * Holds non-secret enrollment state and configuration:
 *   - The control-plane base URL (admin-entered, normalized).
 *   - The enrolled site_id and tenant_id returned by /enroll.
 *   - First-activation timestamp + last heartbeat/metadata sync timestamps.
 *
 * Secrets (keys) never live here; they stay in the encrypted Keystore.
 *
 * Sprint 1 (ADR-037) — multisite two-tier resolution. WPRemote's wp_settings.php
 * pattern: every read tries `get_site_option` (network-scoped) first and falls
 * back to `get_option` (single-site). Writes/deletes branch on `is_multisite()`
 * — on multisite networks we PERSIST to the network store so a per-site
 * `get_option` from any blog in the network resolves correctly. Pre-sprint
 * builds used plain `get_option`/`update_option` and were effectively broken on
 * multisite (enrolled state would not be visible from non-primary blogs).
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Typed wrapper around the agent's wp-options.
 */
final class Settings
{
    /** Control-plane base URL (e.g. https://cp.example.com). */
    public const OPTION_CP_URL = 'wpmgr_agent_cp_url';

    /** Enrolled site identifier. */
    public const OPTION_SITE_ID = 'wpmgr_agent_site_id';

    /** Enrolled tenant identifier. */
    public const OPTION_TENANT_ID = 'wpmgr_agent_tenant_id';

    /** Unix timestamp of first plugin activation. */
    public const OPTION_ACTIVATED_AT = 'wpmgr_agent_activated_at';

    /** Unix timestamp of last successful heartbeat. */
    public const OPTION_LAST_HEARTBEAT = 'wpmgr_agent_last_heartbeat';

    /** Unix timestamp of last successful metadata push. */
    public const OPTION_LAST_METADATA = 'wpmgr_agent_last_metadata';

    /**
     * Sentinel used by {@see get()} to distinguish a missing network-scoped
     * option from one whose stored value is literally null/false/''. The
     * default argument to get_site_option is returned verbatim when the option
     * is absent, so we use a value no caller would ever store.
     */
    private const MISSING_SENTINEL = '__wpmgr_settings_missing__';

    /**
     * Two-tier option read. Tries the network-scoped store first
     * (`get_site_option`) on multisite, falls back to the per-blog store
     * (`get_option`). Matches WPRemote's wp_settings.php pattern.
     *
     * Single-site installs return the get_site_option value too (it transparently
     * proxies to get_option there), so the codepath is uniform.
     *
     * @param string $key     Option key.
     * @param mixed  $default Value to return when neither store has the key.
     * @return mixed
     */
    private function get(string $key, $default = null)
    {
        if (function_exists('get_site_option')) {
            $value = get_site_option($key, self::MISSING_SENTINEL);
            if ($value !== self::MISSING_SENTINEL) {
                return $value;
            }
        }
        if (function_exists('get_option')) {
            return get_option($key, $default);
        }
        return $default;
    }

    /**
     * Two-tier option write. Persists to the network-scoped store on
     * multisite (`update_site_option`) and to the per-blog store on
     * single-site (`update_option`). This ensures a subsequent `get_option`
     * from a secondary blog in the network resolves through the
     * get_site_option fallback in {@see get()}.
     *
     * @param string $key   Option key.
     * @param mixed  $value Value to persist.
     * @return bool True on success.
     */
    private function update(string $key, $value): bool
    {
        if (function_exists('is_multisite') && is_multisite() && function_exists('update_site_option')) {
            return (bool) update_site_option($key, $value);
        }
        if (function_exists('update_option')) {
            // The third arg ($autoload=false) only exists for update_option,
            // not update_site_option; keep it on the single-site branch where
            // it matters for the wp_options autoload set.
            return (bool) update_option($key, $value, false);
        }
        return false;
    }

    /**
     * Two-tier option delete. Same branch logic as {@see update()}.
     *
     * @param string $key Option key.
     * @return bool True on success.
     */
    private function delete(string $key): bool
    {
        if (function_exists('is_multisite') && is_multisite() && function_exists('delete_site_option')) {
            return (bool) delete_site_option($key);
        }
        if (function_exists('delete_option')) {
            return (bool) delete_option($key);
        }
        return false;
    }

    /**
     * Get the configured control-plane base URL, or empty string if unset.
     *
     * @return string
     */
    public function controlPlaneUrl(): string
    {
        $value = $this->get(self::OPTION_CP_URL, '');

        return is_string($value) ? $value : '';
    }

    /**
     * Persist a normalized control-plane base URL.
     *
     * @param string $url Raw admin input.
     * @return string The stored, normalized URL ('' if invalid).
     */
    public function setControlPlaneUrl(string $url): string
    {
        $normalized = self::normalizeUrl($url);
        $this->update(self::OPTION_CP_URL, $normalized);

        return $normalized;
    }

    /**
     * Normalize a control-plane base URL: trim, require http(s), strip trailing
     * slash and any path-noise we should not keep. Returns '' if invalid.
     *
     * @param string $url Raw URL.
     * @return string
     */
    public static function normalizeUrl(string $url): string
    {
        $url = trim($url);
        if ($url === '') {
            return '';
        }

        // esc_url_raw is the WordPress-canonical sanitizer for stored URLs.
        if (function_exists('esc_url_raw')) {
            $url = esc_url_raw($url, ['http', 'https']);
        }

        $parts = parse_url($url);
        if ($parts === false || !isset($parts['scheme'], $parts['host'])) {
            return '';
        }
        if (!in_array(strtolower($parts['scheme']), ['http', 'https'], true)) {
            return '';
        }

        return rtrim($url, '/');
    }

    /**
     * @return string Enrolled site_id, or '' if not enrolled.
     */
    public function siteId(): string
    {
        $value = $this->get(self::OPTION_SITE_ID, '');

        return is_string($value) ? $value : '';
    }

    /**
     * @return string Enrolled tenant_id, or '' if not enrolled.
     */
    public function tenantId(): string
    {
        $value = $this->get(self::OPTION_TENANT_ID, '');

        return is_string($value) ? $value : '';
    }

    /**
     * Whether enrollment has completed (site_id + CP URL present).
     *
     * @return bool
     */
    public function isEnrolled(): bool
    {
        return $this->siteId() !== '' && $this->controlPlaneUrl() !== '';
    }

    /**
     * Persist enrollment identifiers returned by /enroll.
     *
     * @param string $siteId   Site identifier.
     * @param string $tenantId Tenant identifier.
     * @return void
     */
    public function setEnrollment(string $siteId, string $tenantId): void
    {
        $this->update(self::OPTION_SITE_ID, $siteId);
        $this->update(self::OPTION_TENANT_ID, $tenantId);
    }

    /**
     * Clear enrollment identifiers (does not touch keys).
     *
     * @return void
     */
    public function clearEnrollment(): void
    {
        $this->delete(self::OPTION_SITE_ID);
        $this->delete(self::OPTION_TENANT_ID);
    }

    /**
     * @return int First-activation Unix timestamp, or 0 if unset.
     */
    public function activatedAt(): int
    {
        $value = $this->get(self::OPTION_ACTIVATED_AT, 0);

        return is_numeric($value) ? (int) $value : 0;
    }

    /**
     * Record the first-activation timestamp if not already set.
     *
     * @param int $now Current timestamp.
     * @return void
     */
    public function markActivated(int $now): void
    {
        if ($this->activatedAt() === 0) {
            $this->update(self::OPTION_ACTIVATED_AT, $now);
        }
    }

    /**
     * @return int Last heartbeat Unix timestamp, or 0 if never.
     */
    public function lastHeartbeat(): int
    {
        $value = $this->get(self::OPTION_LAST_HEARTBEAT, 0);

        return is_numeric($value) ? (int) $value : 0;
    }

    /**
     * @param int $now Current timestamp.
     * @return void
     */
    public function setLastHeartbeat(int $now): void
    {
        $this->update(self::OPTION_LAST_HEARTBEAT, $now);
    }

    /**
     * @return int Last metadata-push Unix timestamp, or 0 if never.
     */
    public function lastMetadata(): int
    {
        $value = $this->get(self::OPTION_LAST_METADATA, 0);

        return is_numeric($value) ? (int) $value : 0;
    }

    /**
     * @param int $now Current timestamp.
     * @return void
     */
    public function setLastMetadata(int $now): void
    {
        $this->update(self::OPTION_LAST_METADATA, $now);
    }

    /**
     * Clear the last-heartbeat / last-metadata timestamps. Used by the admin
     * Disconnect flow so the status panel doesn't show a stale "last sync"
     * after the agent is repointed at a different control plane.
     *
     * @return void
     */
    public function clearLastSyncTimestamps(): void
    {
        $this->delete(self::OPTION_LAST_HEARTBEAT);
        $this->delete(self::OPTION_LAST_METADATA);
    }
}
