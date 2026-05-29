<?php
/**
 * Admin: a minimal top-level "WPMgr" settings page.
 *
 * Lets a manage_options admin:
 *   - enter/normalize the control-plane base URL,
 *   - paste a pairing code and enroll,
 *   - view enrollment status (enrolled / site_id / last sync),
 *   - trigger an on-demand heartbeat + metadata push ("Sync now").
 *
 * All form posts go through admin-post.php with capability + nonce checks. The
 * pairing code is consumed in-request and never stored or logged.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * WordPress admin UI for configuration + enrollment.
 */
final class Admin
{
    /** Settings page slug. */
    public const PAGE_SLUG = 'wpmgr-agent';

    /** admin-post action: save the control-plane URL. */
    public const ACTION_SAVE_URL = 'wpmgr_agent_save_url';

    /** admin-post action: enroll using a pairing code. */
    public const ACTION_ENROLL = 'wpmgr_agent_enroll';

    /** admin-post action: sync now (heartbeat + metadata). */
    public const ACTION_SYNC = 'wpmgr_agent_sync';

    /**
     * admin-post action: disconnect from the current control plane. Wipes
     * site_id, tenant_id, CP public key, and this site's Ed25519 keypair so a
     * fresh enrollment (potentially against a different CP) generates a clean
     * identity. Intentionally preserves the age identity so prior backups stay
     * decryptable. Operator-confirmed via a JS confirm() on the submit button.
     */
    public const ACTION_DISCONNECT = 'wpmgr_agent_disconnect';

    /** Transient key for one-shot admin notices. */
    private const NOTICE_TRANSIENT = 'wpmgr_agent_notice';

    private Settings $settings;

    private Enrollment $enrollment;

    private Keystore $keystore;

    /**
     * @param Settings   $settings   Config/enrollment state.
     * @param Enrollment $enrollment Reporting/enrollment client.
     * @param Keystore   $keystore   Key store (cleared on Disconnect).
     */
    public function __construct(Settings $settings, Enrollment $enrollment, Keystore $keystore)
    {
        $this->settings   = $settings;
        $this->enrollment = $enrollment;
        $this->keystore   = $keystore;
    }

    /**
     * Register admin hooks. Bind only in admin context.
     *
     * @return void
     */
    public function registerHooks(): void
    {
        add_action('admin_menu', [$this, 'registerMenu']);
        add_action('admin_post_' . self::ACTION_SAVE_URL, [$this, 'handleSaveUrl']);
        add_action('admin_post_' . self::ACTION_ENROLL, [$this, 'handleEnroll']);
        add_action('admin_post_' . self::ACTION_SYNC, [$this, 'handleSync']);
        add_action('admin_post_' . self::ACTION_DISCONNECT, [$this, 'handleDisconnect']);
        add_action('admin_notices', [$this, 'renderNotice']);
    }

    /**
     * Register the top-level menu page.
     *
     * @return void
     */
    public function registerMenu(): void
    {
        add_menu_page(
            'WPMgr Agent',
            'WPMgr',
            'manage_options',
            self::PAGE_SLUG,
            [$this, 'renderPage'],
            'dashicons-cloud'
        );
    }

    /**
     * Render the settings page.
     *
     * @return void
     */
    public function renderPage(): void
    {
        if (!current_user_can('manage_options')) {
            return;
        }

        $cpUrl     = $this->settings->controlPlaneUrl();
        $enrolled  = $this->settings->isEnrolled();
        $siteId    = $this->settings->siteId();
        $tenantId  = $this->settings->tenantId();
        $lastBeat  = $this->settings->lastHeartbeat();
        $lastMeta  = $this->settings->lastMetadata();
        $actionUrl = esc_url(admin_url('admin-post.php'));

        echo '<div class="wrap">';
        echo '<h1>' . esc_html('WPMgr Agent') . '</h1>';

        // --- Status panel ---
        echo '<h2>' . esc_html('Status') . '</h2>';
        echo '<table class="form-table"><tbody>';
        echo '<tr><th>' . esc_html('Enrollment') . '</th><td>'
            . ($enrolled
                ? '<strong style="color:#1a7f37;">' . esc_html('Enrolled') . '</strong>'
                : '<strong style="color:#b32d2e;">' . esc_html('Not enrolled') . '</strong>')
            . '</td></tr>';
        echo '<tr><th>' . esc_html('Site ID') . '</th><td>' . esc_html($siteId !== '' ? $siteId : '—') . '</td></tr>';
        echo '<tr><th>' . esc_html('Tenant ID') . '</th><td>' . esc_html($tenantId !== '' ? $tenantId : '—') . '</td></tr>';
        echo '<tr><th>' . esc_html('Last heartbeat') . '</th><td>' . esc_html($this->formatTime($lastBeat)) . '</td></tr>';
        echo '<tr><th>' . esc_html('Last metadata sync') . '</th><td>' . esc_html($this->formatTime($lastMeta)) . '</td></tr>';
        echo '</tbody></table>';

        // --- Control-plane URL form ---
        echo '<h2>' . esc_html('Control plane') . '</h2>';
        echo '<form method="post" action="' . $actionUrl . '">';
        wp_nonce_field(self::ACTION_SAVE_URL);
        echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_SAVE_URL) . '" />';
        echo '<table class="form-table"><tbody><tr><th><label for="wpmgr_cp_url">'
            . esc_html('Control-plane URL') . '</label></th><td>';
        echo '<input type="url" id="wpmgr_cp_url" name="wpmgr_cp_url" class="regular-text" value="'
            . esc_attr($cpUrl) . '" placeholder="https://control-plane.example.com" />';
        echo '<p class="description">' . esc_html('Base URL of your WPMgr control plane (https in production).') . '</p>';
        echo '</td></tr></tbody></table>';
        submit_button('Save URL');
        echo '</form>';

        // --- Enrollment form ---
        if (!$enrolled) {
            echo '<h2>' . esc_html('Enroll') . '</h2>';
            echo '<form method="post" action="' . $actionUrl . '">';
            wp_nonce_field(self::ACTION_ENROLL);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_ENROLL) . '" />';
            echo '<table class="form-table"><tbody><tr><th><label for="wpmgr_pairing_code">'
                . esc_html('Pairing code') . '</label></th><td>';
            echo '<input type="text" id="wpmgr_pairing_code" name="wpmgr_pairing_code" class="regular-text" autocomplete="off" />';
            echo '<p class="description">' . esc_html('Paste the pairing code from your control-plane dashboard.') . '</p>';
            echo '</td></tr></tbody></table>';
            submit_button('Enroll');
            echo '</form>';
        } else {
            // --- Sync now ---
            echo '<h2>' . esc_html('Sync') . '</h2>';
            echo '<form method="post" action="' . $actionUrl . '">';
            wp_nonce_field(self::ACTION_SYNC);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_SYNC) . '" />';
            submit_button('Sync now', 'secondary');
            echo '</form>';

            // --- Disconnect (re-pair against a different CP / pairing code) ---
            // Renders only when enrolled. Clears site_id, tenant_id, the CP
            // public key, and this site's Ed25519 keypair. The age identity
            // (chunk-encryption secret) is preserved so prior ciphertext stays
            // decryptable; the user can wipe it manually if they want a true
            // clean slate. JS confirm() ensures an accidental click does not
            // strand the site mid-engagement.
            echo '<h2>' . esc_html('Re-enroll') . '</h2>';
            echo '<p class="description">'
                . esc_html('Clears this site\'s pairing with the current control plane so you can paste a fresh pairing code (e.g. when migrating to a new CP URL). Prior backups remain decryptable.')
                . '</p>';
            echo '<form method="post" action="' . $actionUrl . '" onsubmit="return confirm(\''
                . esc_js('Disconnect from the current control plane? You will need to paste a new pairing code to re-enroll.')
                . '\');">';
            wp_nonce_field(self::ACTION_DISCONNECT);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_DISCONNECT) . '" />';
            submit_button('Disconnect', 'delete');
            echo '</form>';
        }

        echo '</div>';
    }

    /**
     * Handle the "Save URL" post.
     *
     * @return void
     */
    public function handleSaveUrl(): void
    {
        $this->guard(self::ACTION_SAVE_URL);

        $raw = isset($_POST['wpmgr_cp_url']) ? (string) wp_unslash($_POST['wpmgr_cp_url']) : '';
        $stored = $this->settings->setControlPlaneUrl($raw);

        if ($stored === '' && $raw !== '') {
            $this->notice('error', 'That control-plane URL is not valid. Use an http(s) URL.');
        } else {
            $this->notice('success', 'Control-plane URL saved.');
        }

        $this->redirectBack();
    }

    /**
     * Handle the "Enroll" post.
     *
     * @return void
     */
    public function handleEnroll(): void
    {
        $this->guard(self::ACTION_ENROLL);

        // Pairing code: sanitize lightly, consume in-request, never store/log.
        $code = isset($_POST['wpmgr_pairing_code'])
            ? sanitize_text_field((string) wp_unslash($_POST['wpmgr_pairing_code']))
            : '';

        if ($code === '') {
            $this->notice('error', 'Enter a pairing code.');
            $this->redirectBack();
            return;
        }

        $result = $this->enrollment->enroll($code);
        unset($code);

        if ($result['ok']) {
            // Push metadata immediately on successful enrollment.
            $this->enrollment->pushMetadata();
            $this->notice('success', $result['message']);
        } else {
            $this->notice('error', $result['message']);
        }

        $this->redirectBack();
    }

    /**
     * Handle the "Sync now" post.
     *
     * @return void
     */
    public function handleSync(): void
    {
        $this->guard(self::ACTION_SYNC);

        if (!$this->settings->isEnrolled()) {
            $this->notice('error', 'Enroll before syncing.');
            $this->redirectBack();
            return;
        }

        $beat = $this->enrollment->sendHeartbeat();
        $meta = $this->enrollment->pushMetadata();

        if ($beat['ok'] && $meta['ok']) {
            $this->notice('success', 'Sync complete.');
        } else {
            $failed = !$beat['ok'] ? $beat : $meta;
            $msg    = 'Sync failed';
            if ($failed['status'] > 0) {
                $msg .= ' (HTTP ' . $failed['status'] . ')';
            }
            $msg .= ': ' . $failed['message'];
            $this->notice('error', $msg);
        }

        $this->redirectBack();
    }

    /**
     * Handle the "Disconnect" post.
     *
     * Wipes:
     *   - site_id + tenant_id (Settings::clearEnrollment)
     *   - CP public key + this site's Ed25519 keypair (Keystore::clearSiteIdentity)
     *   - last heartbeat + last metadata timestamps (cosmetic, so the status
     *     panel doesn't show stale data after re-enrollment)
     *
     * Intentionally does NOT clear the age identity. Removing it would orphan
     * any encrypted backups that still need to be restorable; advanced operators
     * can wipe it manually by deleting the wpmgr_agent_age_identity option.
     *
     * Idempotent and no-network: this is purely local cleanup. The CP-side
     * site record (and its row in the sites table) is not touched — that has
     * to be cleaned up separately on the CP if the operator wants the old
     * site row removed.
     *
     * @return void
     */
    public function handleDisconnect(): void
    {
        $this->guard(self::ACTION_DISCONNECT);

        $this->settings->clearEnrollment();
        $this->keystore->clearSiteIdentity();
        $this->settings->clearLastSyncTimestamps();

        $this->notice('success', 'Disconnected from the control plane. Paste a fresh pairing code to re-enroll.');
        $this->redirectBack();
    }

    /**
     * Capability + nonce gate for an admin-post handler.
     *
     * @param string $action Nonce/action name.
     * @return void
     */
    private function guard(string $action): void
    {
        if (!current_user_can('manage_options')) {
            wp_die('Insufficient permissions.', '', ['response' => 403]);
        }
        check_admin_referer($action);
    }

    /**
     * Queue a one-shot admin notice.
     *
     * @param string $type    'success' | 'error'.
     * @param string $message Human message.
     * @return void
     */
    private function notice(string $type, string $message): void
    {
        set_transient(self::NOTICE_TRANSIENT, ['type' => $type, 'message' => $message], 60);
    }

    /**
     * Render and clear any queued admin notice (on our page only).
     *
     * @return void
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
     * Redirect back to the settings page after a form post.
     *
     * @return void
     */
    private function redirectBack(): void
    {
        wp_safe_redirect(admin_url('admin.php?page=' . self::PAGE_SLUG));
        exit;
    }

    /**
     * Format a Unix timestamp for display, or a dash when zero.
     *
     * @param int $ts Unix timestamp.
     * @return string
     */
    private function formatTime(int $ts): string
    {
        if ($ts <= 0) {
            return 'never';
        }

        if (function_exists('wp_date')) {
            return (string) wp_date('Y-m-d H:i:s', $ts);
        }

        return gmdate('Y-m-d H:i:s', $ts) . ' UTC';
    }
}
