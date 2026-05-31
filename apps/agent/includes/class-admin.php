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

use WPMgr\Agent\Support\UpdateChecker;

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
     * admin-post action: explicit Re-enroll (ADR-041). A single deliberate,
     * confirmed action that wipes the existing keys + enrollment FIRST, then
     * runs the normal enroll flow against a freshly pasted pairing code. This
     * is NOT a hidden side effect of pasting a new code — it is its own button.
     */
    public const ACTION_REENROLL = 'wpmgr_agent_reenroll';

    /**
     * admin-post action: disconnect from the current control plane. Wipes
     * site_id, tenant_id, CP public key, and this site's Ed25519 keypair so a
     * fresh enrollment (potentially against a different CP) generates a clean
     * identity. Intentionally preserves the age identity so prior backups stay
     * decryptable. Operator-confirmed via a JS confirm() on the submit button.
     */
    public const ACTION_DISCONNECT = 'wpmgr_agent_disconnect';

    /**
     * ADR-037 Sprint 1, 1E — connection-key pairing UX.
     *
     * admin-post action: mint a single-use, 15-minute connection key. Used by
     * operators on firewalled / private-network sites where the CP cannot
     * reach the agent for a normal pairing handshake. The key encodes the
     * site URL and current agent version so the CP can verify both at accept
     * time.
     */
    public const ACTION_MINT_CONNECTION_KEY = 'wpmgr_agent_mint_connection_key';

    /**
     * admin-post action: revoke an existing un-used connection key. Lets the
     * operator clear a leaked key without waiting for the 15-minute TTL.
     */
    public const ACTION_REVOKE_CONNECTION_KEY = 'wpmgr_agent_revoke_connection_key';

    /**
     * admin-post action: force an immediate update check (ADR-042 Phase 2).
     * Flushes the 12h manifest transient and re-fetches from the CP so the
     * operator can see the current update status without waiting for cache expiry.
     */
    public const ACTION_CHECK_UPDATE = 'wpmgr_agent_check_update';

    /** Option key for the minted connection-key record. */
    public const OPTION_CONNECTION_KEY = 'wpmgr_agent_connection_key';

    /** Connection-key TTL in seconds. */
    private const CONNECTION_KEY_TTL = 15 * 60;

    /** Transient key for one-shot admin notices. */
    private const NOTICE_TRANSIENT = 'wpmgr_agent_notice';

    private Settings $settings;

    private Enrollment $enrollment;

    private Keystore $keystore;

    private Lifecycle $lifecycle;

    private UpdateChecker $updateChecker;

    /**
     * @param Settings      $settings      Config/enrollment state.
     * @param Enrollment    $enrollment    Reporting/enrollment client.
     * @param Keystore      $keystore      Key store (cleared on Disconnect/Re-enroll).
     * @param Lifecycle     $lifecycle     Connection lifecycle (immediate post-enroll
     *                                     heartbeat + revoked-marker accessors).
     * @param UpdateChecker $updateChecker CP self-update checker (ADR-042).
     */
    public function __construct(Settings $settings, Enrollment $enrollment, Keystore $keystore, Lifecycle $lifecycle, UpdateChecker $updateChecker)
    {
        $this->settings       = $settings;
        $this->enrollment     = $enrollment;
        $this->keystore       = $keystore;
        $this->lifecycle      = $lifecycle;
        $this->updateChecker  = $updateChecker;
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
        add_action('admin_post_' . self::ACTION_REENROLL, [$this, 'handleReenroll']);
        add_action('admin_post_' . self::ACTION_MINT_CONNECTION_KEY, [$this, 'handleMintConnectionKey']);
        add_action('admin_post_' . self::ACTION_REVOKE_CONNECTION_KEY, [$this, 'handleRevokeConnectionKey']);
        add_action('admin_post_' . self::ACTION_CHECK_UPDATE, [$this, 'handleCheckUpdate']);
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

        // --- Revoked notice (ADR-039) ---
        // If the control plane disconnected this site from the dashboard, the
        // Lifecycle revoke_self() flow left a marker. Surface it prominently so
        // the operator understands why the agent went quiet, and offer a path
        // back (paste a fresh code into the Enroll/Re-enroll form below).
        $this->renderRevokedNotice();

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

            // --- Check for updates (ADR-042) ---
            echo '<h2>' . esc_html('Agent update') . '</h2>';
            echo '<p class="description">'
                . esc_html('Force an immediate check for a new WPMgr agent version. The result appears in Plugins > Updates.')
                . '</p>';
            echo '<form method="post" action="' . $actionUrl . '">';
            wp_nonce_field(self::ACTION_CHECK_UPDATE);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_CHECK_UPDATE) . '" />';
            submit_button('Check for updates', 'secondary');
            echo '</form>';

            // --- Re-enroll (ADR-041) ---
            // An explicit, deliberate action: wipes the current keys + pairing
            // and enrolls fresh against a newly pasted pairing code. The
            // dashboard mints the fresh code via its re-enroll endpoint; the
            // agent just needs the wipe-then-enroll. Distinct from Disconnect
            // (which only clears, leaving the site unenrolled) and never a
            // hidden side effect of editing the code field. JS confirm() guards
            // an accidental click.
            echo '<h2>' . esc_html('Re-enroll') . '</h2>';
            echo '<p class="description">'
                . esc_html('Wipes this site\'s current pairing and re-enrolls with a fresh identity using a new pairing code (mint one from your dashboard). Prior backups remain decryptable.')
                . '</p>';
            echo '<form method="post" action="' . $actionUrl . '" onsubmit="return confirm(\''
                . esc_js('Re-enroll this site? The current pairing and keys will be wiped, then re-enrolled with the new code.')
                . '\');">';
            wp_nonce_field(self::ACTION_REENROLL);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_REENROLL) . '" />';
            echo '<table class="form-table"><tbody><tr><th><label for="wpmgr_reenroll_code">'
                . esc_html('New pairing code') . '</label></th><td>';
            echo '<input type="text" id="wpmgr_reenroll_code" name="wpmgr_pairing_code" class="regular-text" autocomplete="off" />';
            echo '</td></tr></tbody></table>';
            submit_button('Re-enroll', 'primary');
            echo '</form>';

            // --- Disconnect (clear pairing without re-pairing) ---
            // Clears site_id, tenant_id, the CP public key, and this site's
            // Ed25519 keypair. The age identity (chunk-encryption secret) is
            // preserved so prior ciphertext stays decryptable.
            echo '<h2>' . esc_html('Disconnect') . '</h2>';
            echo '<p class="description">'
                . esc_html('Clears this site\'s pairing with the current control plane without re-enrolling. Prior backups remain decryptable.')
                . '</p>';
            echo '<form method="post" action="' . $actionUrl . '" onsubmit="return confirm(\''
                . esc_js('Disconnect from the current control plane? You will need to paste a new pairing code to re-enroll.')
                . '\');">';
            wp_nonce_field(self::ACTION_DISCONNECT);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_DISCONNECT) . '" />';
            submit_button('Disconnect', 'delete');
            echo '</form>';
        }

        // --- Connection key (ADR-037 Sprint 1, 1E) -----------------------
        // Always available, even before enrollment — operators on firewalled
        // hosts often need to mint the key before they've finished setting
        // up the CP-side site.
        $this->renderConnectionKeySection($actionUrl);

        echo '</div>';
    }

    /**
     * ADR-037 Sprint 1, 1E — connection-key pairing section.
     *
     * Renders one of three states:
     *   - No key minted: shows the "Mint key" button.
     *   - Key minted, still valid: shows the key (in a copy-able code block)
     *     plus a countdown-style "expires in N minutes" line and a "Revoke" form.
     *   - Key expired or used: cleared by the next read; renders the empty state.
     *
     * The key shape is `wpmgr:v1:<32-byte-base64url-token>:<base64-site_url>:<agent_version>`.
     * The CP accepts this blob in the "Add site from connection key" flow
     * (out of scope for this sprint — CP-side acceptance lands later).
     *
     * @param string $actionUrl The admin-post URL for form submissions.
     * @return void
     */
    private function renderConnectionKeySection(string $actionUrl): void
    {
        echo '<h2>' . esc_html('Connection key') . '</h2>';
        echo '<p class="description">'
            . esc_html('For control planes that cannot reach this site directly (firewalled hosts, private networks). '
                     . 'Click "Mint key" to generate a one-time pairing code valid for 15 minutes. '
                     . 'Paste it into the CP\'s "Add site from connection key" flow.')
            . '</p>';

        $record = $this->readConnectionKey();
        $now    = time();

        if ($record !== null && (int) ($record['expires_at'] ?? 0) > $now && empty($record['used_at'])) {
            // Active, unexpired, unused key — show it.
            $blob    = $this->formatConnectionKeyBlob((string) $record['token']);
            $expires = (int) $record['expires_at'];
            $remaining = max(0, $expires - $now);
            $mm = (int) floor($remaining / 60);
            $ss = $remaining % 60;

            echo '<div class="notice notice-warning inline" style="padding:12px;margin:10px 0;">';
            echo '<p style="margin:0 0 8px;"><strong>'
                . esc_html('Anyone with this key can re-pair your site to a different control plane. Treat it like a password.')
                . '</strong></p>';
            echo '<p style="margin:0 0 8px;">'
                . esc_html(sprintf('Key expires in %d:%02d.', $mm, $ss))
                . '</p>';
            echo '<textarea readonly rows="3" cols="80" '
                . 'style="font-family:monospace;font-size:12px;width:100%;max-width:720px;" '
                . 'onclick="this.select();" data-testid="wpmgr-connection-key">'
                . esc_textarea($blob)
                . '</textarea>';
            echo '</div>';

            // Revoke form — clears the key immediately.
            echo '<form method="post" action="' . $actionUrl . '" style="display:inline-block;">';
            wp_nonce_field(self::ACTION_REVOKE_CONNECTION_KEY);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_REVOKE_CONNECTION_KEY) . '" />';
            submit_button('Revoke', 'delete', 'submit', false);
            echo '</form>';
        } else {
            // No active key. If the existing record is expired/used, advise.
            if ($record !== null && !empty($record['used_at'])) {
                echo '<p>' . esc_html('Previous key was accepted by the control plane.') . '</p>';
            } elseif ($record !== null) {
                echo '<p>' . esc_html('Previous key has expired.') . '</p>';
            }

            echo '<form method="post" action="' . $actionUrl . '">';
            wp_nonce_field(self::ACTION_MINT_CONNECTION_KEY);
            echo '<input type="hidden" name="action" value="' . esc_attr(self::ACTION_MINT_CONNECTION_KEY) . '" />';
            submit_button('Mint key', 'secondary');
            echo '</form>';
        }
    }

    /**
     * Read the stored connection-key record from wp_options. Returns null when
     * none has been minted yet. Shape:
     *   { token: <hex>, created_at: <unix>, expires_at: <unix>, used_at: <unix|null> }
     *
     * @return array<string,mixed>|null
     */
    private function readConnectionKey(): ?array
    {
        if (!function_exists('get_option')) {
            return null;
        }
        $raw = get_option(self::OPTION_CONNECTION_KEY, null);
        if (!is_array($raw)) {
            return null;
        }
        if (!isset($raw['token']) || !is_string($raw['token'])) {
            return null;
        }
        return $raw;
    }

    /**
     * Format the public connection-key blob from a stored token.
     *
     * Shape: `wpmgr:v1:<32-byte-base64url-token>:<base64(site_url)>:<agent_version>`
     */
    private function formatConnectionKeyBlob(string $token): string
    {
        $siteUrl    = function_exists('get_site_url') ? (string) get_site_url() : '';
        $urlEncoded = $this->base64UrlEncode($siteUrl);
        $version    = defined('WPMGR_AGENT_VERSION') ? (string) WPMGR_AGENT_VERSION : '0.0.0';
        return 'wpmgr:v1:' . $token . ':' . $urlEncoded . ':' . $version;
    }

    /**
     * URL-safe base64 encode without padding. Used for the token + site_url
     * components of the connection-key blob so the result is safe to ship in
     * URLs, headers, and paste-into-form inputs without further escaping.
     */
    private function base64UrlEncode(string $bytes): string
    {
        return rtrim(strtr(base64_encode($bytes), '+/', '-_'), '=');
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

        // Pairing code: sanitize lightly, trim whitespace, consume in-request,
        // never store/log. Trimming guards the common paste-with-trailing-space
        // case the CP would otherwise reject with a 403/invalid-signature.
        $code = isset($_POST['wpmgr_pairing_code'])
            ? trim(sanitize_text_field((string) wp_unslash($_POST['wpmgr_pairing_code'])))
            : '';

        if ($code === '') {
            $this->notice('error', 'Enter a pairing code.');
            $this->redirectBack();
            return;
        }

        $result = $this->enroll($code);
        unset($code);

        if ($result['ok']) {
            $this->notice('success', $result['message']);
        } else {
            $this->notice('error', $result['message']);
        }

        $this->redirectBack();
    }

    /**
     * Shared enroll routine for both the first-time Enroll form and the
     * explicit Re-enroll button. On success it:
     *   - clears any stale "revoked" marker (the operator is re-connecting),
     *   - pushes metadata immediately (so inventory is fresh on the CP), and
     *   - fires ONE synchronous heartbeat so the dashboard flips
     *     pending_enrollment→connected within ~1s instead of waiting out the
     *     first 60s cron tick. Both follow-ups are best-effort: a failure does
     *     not undo a successful enroll (the 60s cron is the backstop).
     *
     * @param string $code Trimmed pairing code.
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    private function enroll(string $code): array
    {
        $result = $this->enrollment->enroll($code);

        if ($result['ok']) {
            Lifecycle::clearRevokedMarker();

            // Push metadata immediately on successful enrollment.
            $this->enrollment->pushMetadata();

            // ADR-039 — immediate post-enroll heartbeat. Wrapped so a failed
            // beat never turns a successful enroll into a failure. heartbeatNow()
            // returns both the instruction list and the signed revoke proof; the
            // proof is threaded into handleInstructions so a first-beat revoke is
            // gated by signature verification (Phase-6 finding B), never acted on
            // from the response body alone.
            $beat = $this->lifecycle->heartbeatNow();
            $instructions = isset($beat['instructions']) && is_array($beat['instructions'])
                ? $beat['instructions']
                : [];
            $revokeToken = isset($beat['revoke_token']) && is_string($beat['revoke_token'])
                ? $beat['revoke_token']
                : '';
            if ($instructions !== []) {
                $this->lifecycle->handleInstructions($instructions, $revokeToken);
            }
        }

        return $result;
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
     * Sends a best-effort SIGNED last-will to the control plane FIRST (so the
     * dashboard flips the site to disconnected immediately instead of waiting for
     * the ~6-min heartbeat timeout), THEN wipes the local identity. Ordering is
     * load-bearing: the notify must happen while the Ed25519 keypair still exists,
     * because signing the last-will requires it.
     *
     * Intentionally does NOT clear the age identity (see above).
     *
     * @return void
     */
    public function handleDisconnect(): void
    {
        $this->guard(self::ACTION_DISCONNECT);

        // Notify the CP BEFORE wiping keys (the signed last-will needs the key).
        // Advisory/best-effort: a failure must not block the local cleanup.
        $notified = false;
        try {
            $this->enrollment->disconnect('user_initiated');
            $notified = true;
        } catch (\Throwable $e) {
            // swallow — the CP also learns via the heartbeat-timeout sweeper.
        }

        $this->settings->clearEnrollment();
        $this->keystore->clearSiteIdentity();
        $this->settings->clearLastSyncTimestamps();

        $msg = $notified
            ? 'Disconnected and notified the control plane. Paste a fresh pairing code to re-enroll.'
            : 'Disconnected locally (the control plane will catch up shortly). Paste a fresh pairing code to re-enroll.';
        $this->notice('success', $msg);
        $this->redirectBack();
    }

    /**
     * Handle the explicit "Re-enroll" post (ADR-041).
     *
     * A single deliberate, confirmed action: wipe the existing site identity
     * (CP public key + this site's Ed25519 keypair) and enrollment state FIRST,
     * then run the normal enroll flow against the freshly pasted pairing code.
     * Re-enrolling is therefore NEVER a hidden side effect of pasting a new code
     * into the first-time form — it is its own button with its own confirm.
     *
     * The age identity is preserved (Keystore::clearSiteIdentity contract) so
     * prior backups stay decryptable. A fresh site keypair is generated by the
     * enroll flow (Signer::agentPublicKey regenerates on demand when absent).
     *
     * @return void
     */
    public function handleReenroll(): void
    {
        $this->guard(self::ACTION_REENROLL);

        $code = isset($_POST['wpmgr_pairing_code'])
            ? trim(sanitize_text_field((string) wp_unslash($_POST['wpmgr_pairing_code'])))
            : '';

        if ($code === '') {
            $this->notice('error', 'Enter a fresh pairing code to re-enroll.');
            $this->redirectBack();
            return;
        }

        // ADR-041: wipe keys FIRST, then enroll fresh. clearSiteIdentity drops
        // the CP key + site keypair; clearEnrollment drops site_id/tenant_id;
        // clearLastSyncTimestamps clears the stale status panel. The new
        // keypair is regenerated lazily inside the enroll flow.
        $this->keystore->clearSiteIdentity();
        $this->settings->clearEnrollment();
        $this->settings->clearLastSyncTimestamps();

        $result = $this->enroll($code);
        unset($code);

        if ($result['ok']) {
            $this->notice('success', 'Re-enrolled with a fresh identity. ' . $result['message']);
        } else {
            $this->notice(
                'error',
                'Re-enroll failed (previous pairing was already cleared): ' . $result['message']
            );
        }

        $this->redirectBack();
    }

    /**
     * Handle the "Mint key" post.
     *
     * Refuses to mint if an existing unexpired+unused key is on file (operator
     * must wait for it to expire, or explicit Revoke). Generates 32 bytes via
     * random_bytes() and URL-safe base64-encodes them. Stores the record under
     * OPTION_CONNECTION_KEY with created_at, expires_at, used_at=null.
     *
     * @return void
     */
    public function handleMintConnectionKey(): void
    {
        $this->guard(self::ACTION_MINT_CONNECTION_KEY);

        $existing = $this->readConnectionKey();
        $now      = time();
        if ($existing !== null
            && (int) ($existing['expires_at'] ?? 0) > $now
            && empty($existing['used_at'])
        ) {
            $this->notice(
                'error',
                'A connection key is already active. Revoke it or wait for it to expire before minting another.'
            );
            $this->redirectBack();
            return;
        }

        try {
            $rawBytes = random_bytes(32);
        } catch (\Throwable $e) {
            $this->notice('error', 'Could not generate a secure random token: ' . $e->getMessage());
            $this->redirectBack();
            return;
        }
        $token = $this->base64UrlEncode($rawBytes);

        $record = [
            'token'      => $token,
            'created_at' => $now,
            'expires_at' => $now + self::CONNECTION_KEY_TTL,
            'used_at'    => null,
        ];
        update_option(self::OPTION_CONNECTION_KEY, $record, false);

        $this->notice('success', 'Connection key minted. Valid for 15 minutes.');
        $this->redirectBack();
    }

    /**
     * Handle the "Revoke" post — clears any minted connection key.
     *
     * @return void
     */
    public function handleRevokeConnectionKey(): void
    {
        $this->guard(self::ACTION_REVOKE_CONNECTION_KEY);

        delete_option(self::OPTION_CONNECTION_KEY);
        $this->notice('success', 'Connection key revoked.');
        $this->redirectBack();
    }

    /**
     * Handle the "Check for updates" post (ADR-042 Phase 2).
     *
     * Capability + nonce gated (guard()). Delegates to UpdateChecker::checkNow()
     * which flushes both transients and re-fetches a fresh manifest from the CP.
     * The operator can then see the update status in Plugins > Updates.
     *
     * @return void
     */
    public function handleCheckUpdate(): void
    {
        $this->guard(self::ACTION_CHECK_UPDATE);

        if (!$this->settings->isEnrolled()) {
            $this->notice('error', 'Enroll before checking for updates.');
            $this->redirectBack();
            return;
        }

        $this->updateChecker->checkNow();
        $this->notice('success', 'Checked for updates.');
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
     * Render the "this site was disconnected from the dashboard" notice when a
     * revoked marker is present (set by Lifecycle::revokeSelf). Explains the
     * disconnect and points the operator at the Enroll/Re-enroll form. Output
     * is fully escaped; no secrets are involved.
     *
     * @return void
     */
    private function renderRevokedNotice(): void
    {
        $marker = Lifecycle::revokedMarker();
        if ($marker === null) {
            return;
        }

        $when = $marker['at'] > 0 ? ' on ' . $this->formatTime($marker['at']) : '';

        echo '<div class="notice notice-warning"><p><strong>'
            . esc_html('This site was disconnected from your WPMgr dashboard') . esc_html($when) . '.</strong> '
            . esc_html('The agent stopped reporting and was deactivated. To reconnect, paste a fresh pairing code into the Enroll form below.')
            . '</p></div>';
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
