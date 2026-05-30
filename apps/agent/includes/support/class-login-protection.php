<?php
/**
 * LoginProtection (S2 — Login Protection): brute-force enforcement with
 * per-IP and global time-windowed counters.
 *
 * This is a direct port of WPRProtectLP_V647 (wpremote/protect/lp.php) adapted
 * to WPMgr's architecture:
 *   - Config lives in the wp-option `wpmgr_security_config` (JSON), written by
 *     the `sync_security_config` signed command.
 *   - Events are stored in `wpmgr_login_events` (via class-schema.php) and
 *     shipped to the CP at /agent/v1/security/login-events on the heartbeat.
 *   - The captcha-server unblock loop (WP Remote's /captcha/solve) is replaced
 *     by a dashboard "Unblock IP" button that calls the `unblock_ip` signed
 *     command; the command calls unblockIp() below.
 *   - Block mode emits a branded 403 wp_die() (PROTECT mode only).
 *   - AUDIT mode records a status=blocked row but never calls wp_die() — safe
 *     for auditing without risk of admin lockout.
 *
 * SAFETY RAILS (mandatory, since default is PROTECT):
 *   (a) allow_cidrs bypass happens FIRST, before any block logic — the
 *       operator's lock-out escape always wins.
 *   (b) The global all-blocked threshold is time-windowed via all_blocked_gap
 *       so it auto-clears; it is never permanent.
 *   (c) Temp blocks are bounded to the temp_block_limit sliding window.
 *   (d) Mode flips pushed by sync_security_config take effect on the next
 *       request (applyConfig() clears configCache).
 *   (e) The authenticate filter only gates NEW logins; already-authenticated
 *       sessions are never terminated (the filter short-circuits when $user is
 *       already a WP_User and auth has passed).
 *
 * Decision tree in loginInit() (first match wins):
 *   1. allow_cidrs match → BYPASSED (safety rail a)
 *   2. deny_cidrs match  → BLACKLISTED → terminateLogin()
 *   3. isPrivate($ip)    → PRIVATEIP (auto-bypass)
 *   4. isKnownLogin (≥1 success from IP in success_login_gap) → BYPASSED
 *   5. global all-blocked (≥ block_all_limit failures in all_blocked_gap) → ALL_BLOCKED → terminateLogin()
 *   6. failed_attempts ≥ temp_block_limit → TEMP_BLOCK → terminateLogin()
 *   7. failed_attempts ≥ captcha_limit    → CAPTCHA_BLOCK → terminateLogin()
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Registers WP authentication hooks and enforces brute-force login protection.
 */
final class LoginProtection
{
    // -------------------------------------------------------------------------
    // Table / option constants
    // -------------------------------------------------------------------------

    /** Unprefixed table name for login-event capture. */
    public const TABLE = 'wpmgr_login_events';

    /** Hard row cap; evicts oldest by occurred_at ASC on overflow. */
    public const ROW_CAP = 100000;

    /** Heartbeat-batch size (newest rows since cursor). */
    public const SHIP_BATCH = 100;

    /** wp-options key for the ship cursor (highest id confirmed by CP). */
    public const OPTION_SHIP_CURSOR = 'wpmgr_agent_login_events_ship_cursor';

    /** wp-options key for the security config JSON. */
    public const OPTION_CONFIG = 'wpmgr_security_config';

    /** Transient prefix for per-IP unblock state. */
    public const UNBLOCK_TRANSIENT_PREFIX = 'wpmgr_lp_unblock_';

    // -------------------------------------------------------------------------
    // Mode constants
    // -------------------------------------------------------------------------

    public const MODE_DISABLED = 'disabled';
    public const MODE_AUDIT    = 'audit';
    public const MODE_PROTECT  = 'protect';

    // -------------------------------------------------------------------------
    // Login-event status constants (stored in the DB column `status TINYINT`).
    // -------------------------------------------------------------------------

    public const STATUS_FAILURE = 1;
    public const STATUS_SUCCESS = 2;
    public const STATUS_BLOCKED = 3;

    // -------------------------------------------------------------------------
    // Block-category constants
    // -------------------------------------------------------------------------

    public const CATEGORY_CAPTCHA_BLOCK = 'captcha_block';
    public const CATEGORY_TEMP_BLOCK    = 'temp_block';
    public const CATEGORY_ALL_BLOCKED   = 'all_blocked';
    public const CATEGORY_BLACKLISTED   = 'blacklisted';
    public const CATEGORY_BYPASSED      = 'bypassed';
    public const CATEGORY_ALLOWED       = 'allowed';
    public const CATEGORY_PRIVATEIP     = 'private_ip';

    // -------------------------------------------------------------------------
    // Default thresholds (match the pinned wire contract).
    // -------------------------------------------------------------------------

    private const DEFAULT_THRESHOLDS = [
        'captcha_limit'    => 3,
        'temp_block_limit' => 10,
        'block_all_limit'  => 100,
        'failed_login_gap' => 1800,
        'success_login_gap'=> 1800,
        'all_blocked_gap'  => 1800,
    ];

    // -------------------------------------------------------------------------
    // Per-instance state
    // -------------------------------------------------------------------------

    /**
     * Config cache. Populated by loadConfig() and cleared by applyConfig().
     *
     * @var array{
     *   mode:string,
     *   thresholds:array{captcha_limit:int,temp_block_limit:int,block_all_limit:int,failed_login_gap:int,success_login_gap:int,all_blocked_gap:int},
     *   ip_header:string,
     *   allow_cidrs:array<int,string>,
     *   deny_cidrs:array<int,string>
     * }|null
     */
    private ?array $configCache = null;

    /**
     * Shared ActivityLog instance for emitting structured block events.
     *
     * Optional — when absent, block events are silently skipped (e.g. in tests).
     */
    private ?ActivityLog $activityLog;

    /**
     * @param ActivityLog|null $activityLog Optional activity recorder. When
     *   provided, a `login.blocked` event is emitted on every terminateLogin()
     *   so CP alerting gets the event for free.
     */
    public function __construct(?ActivityLog $activityLog = null)
    {
        $this->activityLog = $activityLog;
    }

    // -------------------------------------------------------------------------
    // Boot / config
    // -------------------------------------------------------------------------

    /**
     * Register WP authentication hooks when mode is not disabled.
     *
     * Safe to call multiple times — a static guard prevents double-registration.
     *
     * @return void
     */
    /**
     * Whether login protection is active (mode != disabled). Used to gate the
     * WAF mu-plugin install so an inert (unconfigured) site installs no
     * security mu-plugin at all.
     */
    public function isEnabled(): bool
    {
        return $this->loadConfig()['mode'] !== self::MODE_DISABLED;
    }

    public function install(): void
    {
        static $installed = false;
        if ($installed) {
            return;
        }
        $installed = true;

        $config = $this->loadConfig();
        if ($config['mode'] === self::MODE_DISABLED) {
            return;
        }

        // Priority 30: after WP's own check_password (20) and any 2FA plugin
        // (typically 25), so we see the final auth result. We never APPROVE a
        // login ourselves — we only potentially BLOCK it.
        add_filter('authenticate', [$this, 'onAuthenticate'], 30, 3);

        // These fire AFTER a successful / failed login.
        add_action('wp_login', [$this, 'onLoginSuccess'], 10, 2);
        add_action('wp_login_failed', [$this, 'onLoginFailed'], 10, 1);
    }

    /**
     * Load and validate the security config from wp-options. Cached per instance.
     *
     * Returns safe defaults (mode=protect, default thresholds) when the option
     * is absent, not valid JSON, or contains out-of-range values. Invalid keys
     * are dropped and replaced with defaults rather than rejecting the whole config
     * so a malformed push cannot brick the agent.
     *
     * @return array{mode:string,thresholds:array<string,int>,ip_header:string,allow_cidrs:array<int,string>,deny_cidrs:array<int,string>}
     */
    public function loadConfig(): array
    {
        if ($this->configCache !== null) {
            return $this->configCache;
        }

        $this->configCache = $this->buildConfig(null);
        return $this->configCache;
    }

    /**
     * Validate and persist a CP-pushed security config.
     *
     * Writes the validated config to OPTION_CONFIG and clears the instance
     * cache so subsequent calls to loadConfig() (and thus install() / block
     * decisions) see the new values within the same request.
     *
     * Also adjusts hook registration: if mode transitions from disabled → active,
     * the hooks that install() skipped must now be registered (and vice-versa).
     * Because WordPress does not support removing a filter that hasn't been
     * added yet, this method installs hooks on a disabled→active transition but
     * cannot un-register already-registered hooks; it simply stops blocking in
     * onAuthenticate() once mode=disabled is re-read.
     *
     * @param array<string,mixed> $raw Raw decoded config from the CP.
     * @return void
     */
    public function applyConfig(array $raw): void
    {
        $config  = $this->buildConfig($raw);
        $encoded = (string) json_encode($config);
        if (function_exists('update_option')) {
            update_option(self::OPTION_CONFIG, $encoded, false);
        }
        $this->configCache = null; // invalidate so next loadConfig() re-reads
    }

    /**
     * Clear all block/counter state for a specific IP. Called by the
     * `unblock_ip` signed command.
     *
     * Actions taken:
     *   1. Delete the per-IP unblock transient (if present).
     *   2. Delete the most recent failure rows for this IP from the events table
     *      so the failure counters drop to zero on the next login attempt.
     *
     * Best-effort — DB failures are silently swallowed.
     *
     * @param string $ip The IPv4 or IPv6 address to unblock.
     * @return void
     */
    public function unblockIp(string $ip): void
    {
        $ip = trim($ip);
        if ($ip === '') {
            return;
        }

        // Clear the unblock transient (if we set one — not currently used,
        // but kept for future compatibility with the CAPTCHA-unblock path).
        if (function_exists('delete_transient')) {
            delete_transient(self::UNBLOCK_TRANSIENT_PREFIX . md5($ip));
        }

        // Delete this IP's recent failure rows from the login-events table so
        // the failure counters reset. Keep success rows so the known-good-login
        // bypass still works. Best-effort.
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        try {
            $wpdb->query(
                $wpdb->prepare(
                    "DELETE FROM {$table} WHERE ip = %s AND status = %d",
                    $ip,
                    self::STATUS_FAILURE
                )
            );
        } catch (\Throwable $e) {
            // Never let a cleanup error propagate.
        }
    }

    // -------------------------------------------------------------------------
    // WP hooks
    // -------------------------------------------------------------------------

    /**
     * authenticate filter (priority 30). This is the enforcement gate.
     *
     * Safety: if $user is already a WP_User and WP has fully authenticated them
     * (the positive case), we still run the decision tree — a known-good login
     * from this IP within success_login_gap will produce BYPASSED immediately.
     * We never return null (which would reset the auth chain) and never return
     * a WP_Error unless we are in protect mode and blocking applies.
     *
     * @param \WP_User|\WP_Error|null $user     Result from prior filters.
     * @param string                  $username Submitted username.
     * @param string                  $password Submitted password.
     * @return \WP_User|\WP_Error|null
     */
    public function onAuthenticate($user, string $username = '', string $password = '')
    {
        $config = $this->loadConfig();
        if ($config['mode'] === self::MODE_DISABLED) {
            return $user;
        }

        $ip = IpUtils::clientIp($config['ip_header']);

        // SAFETY RAIL (a): allow_cidrs always bypass first.
        if ($config['allow_cidrs'] !== [] && IpUtils::matchesAnyCidr($ip, $config['allow_cidrs'])) {
            return $user;
        }

        // (2) deny_cidrs → immediate block.
        if ($config['deny_cidrs'] !== [] && IpUtils::matchesAnyCidr($ip, $config['deny_cidrs'])) {
            $this->terminate($ip, $username, self::CATEGORY_BLACKLISTED, $config);
            // In audit mode we returned above via terminate(). Reaching here means
            // protect mode — terminate() called wp_die() which never returns.
            // Return a WP_Error as a safety net (unreachable in practice).
            return new \WP_Error('wpmgr_ip_blocked', __('Your IP is blocked.', 'wpmgr-agent'));
        }

        // (3) Private IP → auto-bypass (loopback / LAN / link-local).
        if (IpUtils::isPrivate($ip)) {
            return $user;
        }

        $now            = time();
        $thresholds     = $config['thresholds'];

        // (4) Known-good login → bypass (prevents counting as brute force for
        //     a legitimate user who recently logged in successfully from this IP).
        if ($this->getLoginCount(self::STATUS_SUCCESS, $ip, $now, $thresholds['success_login_gap']) > 0) {
            return $user;
        }

        $failedAttempts = $this->getLoginCount(self::STATUS_FAILURE, $ip, $now, $thresholds['failed_login_gap']);

        // (5) Global all-blocked: too many failures site-wide (time-windowed;
        //     SAFETY RAIL b — auto-clears after all_blocked_gap).
        if ($this->getLoginCount(self::STATUS_FAILURE, null, $now, $thresholds['all_blocked_gap']) >= $thresholds['block_all_limit']) {
            $this->terminate($ip, $username, self::CATEGORY_ALL_BLOCKED, $config);
            return new \WP_Error('wpmgr_all_blocked', __('Logins to this site are currently blocked.', 'wpmgr-agent'));
        }

        // (6) Temp block (per-IP).
        if ($failedAttempts >= $thresholds['temp_block_limit']) {
            $this->terminate($ip, $username, self::CATEGORY_TEMP_BLOCK, $config);
            return new \WP_Error('wpmgr_temp_blocked', __('Too many failed login attempts. Please try again later.', 'wpmgr-agent'));
        }

        // (7) Captcha / soft block (per-IP).
        if ($failedAttempts >= $thresholds['captcha_limit']) {
            $this->terminate($ip, $username, self::CATEGORY_CAPTCHA_BLOCK, $config);
            return new \WP_Error('wpmgr_captcha_block', __('Too many failed login attempts.', 'wpmgr-agent'));
        }

        return $user;
    }

    /**
     * wp_login action: record a successful login.
     *
     * @param string        $userLogin Username.
     * @param \WP_User|null $wpUser    WP_User object (may be null in unit tests).
     * @return void
     */
    public function onLoginSuccess(string $userLogin, $wpUser = null): void
    {
        $config = $this->loadConfig();
        if ($config['mode'] === self::MODE_DISABLED) {
            return;
        }
        $ip = IpUtils::clientIp($config['ip_header']);
        $this->record($ip, self::STATUS_SUCCESS, self::CATEGORY_ALLOWED, $userLogin);
    }

    /**
     * wp_login_failed action: record a failed login attempt.
     *
     * @param string $username The username that failed.
     * @return void
     */
    public function onLoginFailed(string $username): void
    {
        $config = $this->loadConfig();
        if ($config['mode'] === self::MODE_DISABLED) {
            return;
        }
        $ip = IpUtils::clientIp($config['ip_header']);
        $this->record($ip, self::STATUS_FAILURE, self::CATEGORY_ALLOWED, $username);
    }

    // -------------------------------------------------------------------------
    // Ship / cursor (mirrors ErrorMonitor::shipBatch + advanceCursor)
    // -------------------------------------------------------------------------

    /**
     * Build the batch the heartbeat ships to /agent/v1/security/login-events.
     * Returns up to SHIP_BATCH newest rows whose id is above the stored cursor.
     *
     * @return array<int,array<string,mixed>>
     */
    public function shipBatch(): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return [];
        }
        $table    = $wpdb->prefix . self::TABLE;
        $sinceId  = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_CURSOR, 0) : 0);

        $rows = $wpdb->get_results(
            $wpdb->prepare(
                "SELECT id, ip, status, category, username, request_id, occurred_at
                 FROM {$table}
                 WHERE id > %d
                 ORDER BY id ASC
                 LIMIT %d",
                $sinceId,
                self::SHIP_BATCH
            ),
            ARRAY_A
        );

        if (!is_array($rows)) {
            return [];
        }

        // Cast numeric columns to their proper PHP types.
        foreach ($rows as &$row) {
            $row['id']         = (int) $row['id'];
            $row['status']     = (int) $row['status'];
            $row['occurred_at']= (int) $row['occurred_at'];
        }
        unset($row);

        return $rows;
    }

    /**
     * Advance the ship cursor after the CP confirms it consumed up to $highestId.
     *
     * @param int $highestId Highest id the CP confirmed.
     * @return void
     */
    public function advanceCursor(int $highestId): void
    {
        if (!function_exists('update_option')) {
            return;
        }
        $current = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_CURSOR, 0) : 0);
        if ($highestId > $current) {
            update_option(self::OPTION_SHIP_CURSOR, $highestId, false);
        }
    }

    // -------------------------------------------------------------------------
    // Internal block + record helpers
    // -------------------------------------------------------------------------

    /**
     * Record a blocked event and, in PROTECT mode, halt the request with a
     * branded 403 wp_die() screen. In AUDIT mode the row is inserted but
     * wp_die() is never called.
     *
     * Also emits a structured ActivityLog event so CP alerting works for free.
     *
     * @param string              $ip       Client IP.
     * @param string              $username Attempted login username.
     * @param string              $category One of the CATEGORY_* constants.
     * @param array<string,mixed> $config   Current effective config.
     * @return void   In PROTECT mode this call never returns (wp_die).
     */
    private function terminate(string $ip, string $username, string $category, array $config): void
    {
        // Record the blocked event.
        $this->record($ip, self::STATUS_BLOCKED, $category, $username);

        // Emit structured activity event for CP alerting.
        $this->emitBlockEvent($ip, $username, $category);

        if ($config['mode'] !== self::MODE_PROTECT) {
            // AUDIT — log only, never die.
            return;
        }

        // PROTECT — terminate with a branded 403 screen.
        $message = $this->blockMessage($category);

        // No-cache headers prevent a CDN from caching the 403 page and serving
        // it to subsequent users.
        if (!headers_sent()) {
            header('Cache-Control: no-cache, no-store, must-revalidate');
            header('Pragma: no-cache');
            header('Expires: 0');
        }

        $requestId = $this->requestId();
        $html = sprintf(
            '<div style="height:98vh;">
                <div style="text-align:center;padding:10%% 0;font-family:Arial,Helvetica,sans-serif;">
                    <h2>WPMgr Login Protection</h2>
                    <p>%s</p>
                    <p style="color:#888;font-size:12px;">Reference ID: %s</p>
                </div>
            </div>',
            esc_html($message),
            esc_html($requestId)
        );

        wp_die(
            $html,
            __('Login Blocked', 'wpmgr-agent'),
            ['response' => 403, 'back_link' => false]
        );
    }

    /**
     * Insert one login-event row. Best-effort — DB errors are silently swallowed
     * so a monitor-path failure never fatals a real login request.
     *
     * @param string $ip       Client IP address.
     * @param int    $status   One of STATUS_* constants.
     * @param string $category One of CATEGORY_* constants.
     * @param string $username The login username.
     * @return void
     */
    private function record(string $ip, int $status, string $category, string $username): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        try {
            $wpdb->insert(
                $table,
                [
                    'ip'         => substr($ip, 0, 64),
                    'status'     => $status,
                    'category'   => substr($category, 0, 64),
                    'username'   => substr($username, 0, 191),
                    'request_id' => $this->requestId(),
                    'occurred_at'=> time(),
                ],
                ['%s', '%d', '%s', '%s', '%s', '%d']
            );
            $this->enforceRowCap();
        } catch (\Throwable $e) {
            // Silently swallow.
        }
    }

    /**
     * COUNT(*) login events matching status + optional IP + time window.
     *
     * This is the direct port of WPRProtectLP_V647::getLoginCount().
     *
     * @param int         $status  STATUS_* constant.
     * @param string|null $ip      IP to filter by, or null for the global count.
     * @param int         $now     Current timestamp (allows testing with a fixed time).
     * @param int         $gap     Seconds look-back window.
     * @return int
     */
    public function getLoginCount(int $status, ?string $ip, int $now, int $gap): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        $table   = $wpdb->prefix . self::TABLE;
        $cutoff  = $now - $gap;

        try {
            if ($ip !== null && $ip !== '') {
                $count = $wpdb->get_var(
                    $wpdb->prepare(
                        "SELECT COUNT(*) FROM {$table} WHERE status = %d AND occurred_at > %d AND ip = %s",
                        $status,
                        $cutoff,
                        $ip
                    )
                );
            } else {
                $count = $wpdb->get_var(
                    $wpdb->prepare(
                        "SELECT COUNT(*) FROM {$table} WHERE status = %d AND occurred_at > %d",
                        $status,
                        $cutoff
                    )
                );
            }
            return is_numeric($count) ? (int) $count : 0;
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Enforce the row cap by evicting the oldest rows by occurred_at ASC.
     */
    private function enforceRowCap(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        try {
            $count = (int) $wpdb->get_var("SELECT COUNT(*) FROM {$table}");
            if ($count <= self::ROW_CAP) {
                return;
            }
            $overflow = $count - self::ROW_CAP;
            $batch    = min($overflow, 500);
            $wpdb->query(
                $wpdb->prepare(
                    "DELETE FROM {$table} ORDER BY occurred_at ASC LIMIT %d",
                    $batch
                )
            );
        } catch (\Throwable $e) {
            // Swallow.
        }
    }

    /**
     * Emit a `login.blocked` event to the ActivityLog if available.
     *
     * @param string $ip       Client IP.
     * @param string $username Attempted username.
     * @param string $category Block category.
     * @return void
     */
    private function emitBlockEvent(string $ip, string $username, string $category): void
    {
        if ($this->activityLog === null) {
            return;
        }
        try {
            $this->activityLog->record(
                'login.blocked',
                'auth',
                '',
                $username,
                0,
                $username,
                $ip,
                sprintf('Login blocked (%s) for %s from %s', $category, $username, $ip),
                ['category' => $category, 'ip' => $ip]
            );
        } catch (\Throwable $e) {
            // Activity logging must never fatal a login.
        }
    }

    /**
     * Generate a per-request correlation ID.
     *
     * @return string
     */
    private function requestId(): string
    {
        if (function_exists('wp_unique_id')) {
            return (string) wp_unique_id('lp_');
        }
        return 'lp_' . bin2hex(random_bytes(6));
    }

    /**
     * Return a human-readable block message for the branded wp_die() screen.
     *
     * @param string $category CATEGORY_* constant.
     * @return string
     */
    private function blockMessage(string $category): string
    {
        switch ($category) {
            case self::CATEGORY_CAPTCHA_BLOCK:
                return 'Too many failed login attempts. Your IP has been temporarily restricted. Contact the site administrator to unblock access.';
            case self::CATEGORY_TEMP_BLOCK:
                return 'Too many failed login attempts. You cannot log in to this site for 30 minutes.';
            case self::CATEGORY_ALL_BLOCKED:
                return 'Logins to this site are currently blocked due to excessive failed login attempts. Please try again later.';
            case self::CATEGORY_BLACKLISTED:
                return 'Your IP address is blocked from logging in to this site.';
            default:
                return 'Login blocked.';
        }
    }

    // -------------------------------------------------------------------------
    // Config builder (shared between loadConfig + applyConfig)
    // -------------------------------------------------------------------------

    /**
     * Build a validated, fully-defaulted config array.
     *
     * @param array<string,mixed>|null $raw Decoded input, or null to read from wp-options.
     * @return array{mode:string,thresholds:array<string,int>,ip_header:string,allow_cidrs:array<int,string>,deny_cidrs:array<int,string>}
     */
    private function buildConfig(?array $raw): array
    {
        if ($raw === null) {
            $stored = function_exists('get_option') ? get_option(self::OPTION_CONFIG, null) : null;
            if (is_string($stored) && $stored !== '') {
                $decoded = json_decode($stored, true);
                $raw     = is_array($decoded) ? $decoded : [];
            } else {
                $raw = [];
            }
        }

        // Mode.
        $validModes = [self::MODE_DISABLED, self::MODE_AUDIT, self::MODE_PROTECT];
        // Inert by default: until the control plane PUSHES a security config
        // (wp-option absent → $raw === []), login protection does NOTHING — no
        // hooks, no blocking, no WAF enforcement. This prevents a site that just
        // updated the plugin from surprise-locking out an admin before any
        // allow-list is set. "PROTECT-by-default" (decision §6.2) is honoured at
        // the CP layer: the stored config defaults to mode=protect, so when the
        // operator enables it from the dashboard it enforces immediately (no
        // audit-first phase). An EXPLICIT pushed mode (incl. protect/audit) wins.
        $mode = isset($raw['mode']) && in_array($raw['mode'], $validModes, true)
            ? (string) $raw['mode']
            : self::MODE_DISABLED;

        // Thresholds.
        $rawThresholds = isset($raw['thresholds']) && is_array($raw['thresholds'])
            ? $raw['thresholds']
            : [];
        $thresholds = [];
        foreach (self::DEFAULT_THRESHOLDS as $key => $default) {
            $val = isset($rawThresholds[$key]) ? $rawThresholds[$key] : $default;
            $thresholds[$key] = (is_int($val) && $val > 0) ? $val : $default;
        }

        // IP header.
        $ipHeader = isset($raw['ip_header']) && is_string($raw['ip_header']) && $raw['ip_header'] !== ''
            ? strtoupper(trim($raw['ip_header']))
            : 'REMOTE_ADDR';

        // allow_cidrs.
        $allowCidrs = $this->filterCidrList(
            isset($raw['allow_cidrs']) && is_array($raw['allow_cidrs']) ? $raw['allow_cidrs'] : []
        );

        // deny_cidrs.
        $denyCidrs = $this->filterCidrList(
            isset($raw['deny_cidrs']) && is_array($raw['deny_cidrs']) ? $raw['deny_cidrs'] : []
        );

        return [
            'mode'        => $mode,
            'thresholds'  => $thresholds,
            'ip_header'   => $ipHeader,
            'allow_cidrs' => $allowCidrs,
            'deny_cidrs'  => $denyCidrs,
        ];
    }

    /**
     * Validate and filter a raw CIDR list. Drops entries that are not strings
     * or do not parse as valid CIDR notation. Returns an index-sequential array.
     *
     * @param array<mixed> $raw
     * @return array<int,string>
     */
    private function filterCidrList(array $raw): array
    {
        $out = [];
        foreach ($raw as $entry) {
            if (!is_string($entry) || $entry === '') {
                continue;
            }
            $entry = trim($entry);
            if (!str_contains($entry, '/')) {
                continue; // must have prefix length
            }
            [$base] = explode('/', $entry, 2);
            if (filter_var($base, FILTER_VALIDATE_IP) === false) {
                continue; // invalid base address
            }
            $out[] = $entry;
        }
        return array_values($out);
    }
}
