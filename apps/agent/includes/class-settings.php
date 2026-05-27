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
     * Get the configured control-plane base URL, or empty string if unset.
     *
     * @return string
     */
    public function controlPlaneUrl(): string
    {
        $value = get_option(self::OPTION_CP_URL, '');

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
        update_option(self::OPTION_CP_URL, $normalized, false);

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
        $value = get_option(self::OPTION_SITE_ID, '');

        return is_string($value) ? $value : '';
    }

    /**
     * @return string Enrolled tenant_id, or '' if not enrolled.
     */
    public function tenantId(): string
    {
        $value = get_option(self::OPTION_TENANT_ID, '');

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
        update_option(self::OPTION_SITE_ID, $siteId, false);
        update_option(self::OPTION_TENANT_ID, $tenantId, false);
    }

    /**
     * Clear enrollment identifiers (does not touch keys).
     *
     * @return void
     */
    public function clearEnrollment(): void
    {
        delete_option(self::OPTION_SITE_ID);
        delete_option(self::OPTION_TENANT_ID);
    }

    /**
     * @return int First-activation Unix timestamp, or 0 if unset.
     */
    public function activatedAt(): int
    {
        $value = get_option(self::OPTION_ACTIVATED_AT, 0);

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
            update_option(self::OPTION_ACTIVATED_AT, $now, false);
        }
    }

    /**
     * @return int Last heartbeat Unix timestamp, or 0 if never.
     */
    public function lastHeartbeat(): int
    {
        $value = get_option(self::OPTION_LAST_HEARTBEAT, 0);

        return is_numeric($value) ? (int) $value : 0;
    }

    /**
     * @param int $now Current timestamp.
     * @return void
     */
    public function setLastHeartbeat(int $now): void
    {
        update_option(self::OPTION_LAST_HEARTBEAT, $now, false);
    }

    /**
     * @return int Last metadata-push Unix timestamp, or 0 if never.
     */
    public function lastMetadata(): int
    {
        $value = get_option(self::OPTION_LAST_METADATA, 0);

        return is_numeric($value) ? (int) $value : 0;
    }

    /**
     * @param int $now Current timestamp.
     * @return void
     */
    public function setLastMetadata(int $now): void
    {
        update_option(self::OPTION_LAST_METADATA, $now, false);
    }
}
