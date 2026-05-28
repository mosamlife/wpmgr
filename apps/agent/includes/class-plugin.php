<?php
/**
 * Plugin bootstrap: singleton that wires the keystore, connector, router, and
 * commands into WordPress, and handles activation (keypair + jti table).
 *
 * The plugin is silent: no frontend output, no admin notices, no telemetry.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\InfoCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\RestoreCommand;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Commands\ScanCommand;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\BackupSource;
use WPMgr\Agent\Support\BackupTransport;

/**
 * Top-level plugin orchestrator.
 */
final class Plugin
{
    /**
     * Option flag set when keystore initialization failed during activation,
     * so admin pages can surface a fix-it notice and a lazy retry can run.
     */
    public const OPTION_KEYSTORE_ERROR = 'wpmgr_agent_keystore_error';

    private static ?Plugin $instance = null;

    private Keystore $keystore;

    private Connector $connector;

    private Router $router;

    private Settings $settings;

    private Signer $signer;

    private Enrollment $enrollment;

    private Scheduler $scheduler;

    private Admin $admin;

    /**
     * Private constructor wires the object graph.
     */
    private function __construct()
    {
        $this->keystore   = new Keystore();
        $this->settings   = new Settings();
        $this->connector  = new Connector($this->keystore, $this->settings);
        $this->signer     = new Signer($this->keystore);
        $this->router     = new Router($this->connector, $this->commands());
        $this->enrollment = new Enrollment($this->keystore, $this->settings, $this->signer, new MetadataCommand());
        $this->scheduler  = new Scheduler($this->settings, $this->enrollment);
        $this->admin      = new Admin($this->settings, $this->enrollment);
    }

    /**
     * Boot the plugin: return the singleton and register hooks once.
     *
     * @return Plugin
     */
    public static function boot(): Plugin
    {
        if (self::$instance === null) {
            self::$instance = new self();
            self::$instance->registerHooks();
        }

        return self::$instance;
    }

    /**
     * Register WordPress hooks. REST routes are the only public surface.
     *
     * @return void
     */
    private function registerHooks(): void
    {
        add_action('rest_api_init', [$this->router, 'registerRoutes']);

        $this->scheduler->registerHooks();

        if (function_exists('is_admin') && is_admin()) {
            $this->admin->registerHooks();
            // Lazily retry keystore setup and surface a fix-it notice if it
            // could not be established during activation.
            add_action('admin_init', [$this, 'ensureKeystoreReady']);
            add_action('admin_notices', [$this, 'renderKeystoreNotice']);
        }

        if (defined('WPMGR_AGENT_FILE')) {
            register_activation_hook(WPMGR_AGENT_FILE, [$this, 'activate']);
            register_deactivation_hook(WPMGR_AGENT_FILE, [$this, 'deactivate']);
        }
    }

    /**
     * Activation hook: create the jti table and generate the site keypair.
     *
     * Keystore setup is best-effort and MUST NOT fatal the activation: a
     * non-writable host or missing salts would otherwise white-screen the site.
     * On failure we record a persistent flag, show an admin notice, and let the
     * plugin activate so it can retry lazily on later admin loads.
     *
     * @return void
     */
    public function activate(): void
    {
        $this->createJtiTable();

        $this->setupKeystore();

        // Record first-activation time and schedule reporting + safety events.
        $now = time();
        $this->settings->markActivated($now);
        $this->scheduler->scheduleEvents($now);
    }

    /**
     * Best-effort keystore initialization: ensure the site keypair exists.
     * Returns true on success. Never throws; on failure it persists an error
     * flag so the admin can be notified and a retry can run later.
     *
     * @return bool Whether the keystore is ready.
     */
    private function setupKeystore(): bool
    {
        try {
            // Generate the site's own Ed25519 keypair on first activation only.
            // getSiteKeypair() also exercises master-key decryption, so a bad
            // key source surfaces here rather than at request time.
            if ($this->keystore->getSiteKeypair() === null) {
                $this->keystore->generateSiteKeypair();
            }

            // Provision the site's age backup-encryption identity (PRIVATE key
            // stored encrypted; only the PUBLIC recipient is ever shared). Doing
            // it here means the recipient is available to the admin/CP before the
            // first backup, and the private key is generated long before any
            // backup command can run.
            if (!$this->keystore->hasAgeIdentity()) {
                (new AgeIdentity($this->keystore))->ensureRecipient();
            }

            delete_option(self::OPTION_KEYSTORE_ERROR);

            return true;
        } catch (\Throwable $e) {
            update_option(
                self::OPTION_KEYSTORE_ERROR,
                'WPMgr Agent could not establish its encryption key. Define WPMGR_AGENT_KEY_FILE '
                . 'in wp-config.php pointing to a writable path, or ensure your wp-config.php '
                . 'secret salts (AUTH_KEY, ...) are set. The plugin is active but inactive until '
                . 'this is resolved.',
                false
            );

            return false;
        }
    }

    /**
     * Lazily retry keystore setup on admin loads when a prior attempt failed.
     * Bound to admin_init.
     *
     * @return void
     */
    public function ensureKeystoreReady(): void
    {
        if (get_option(self::OPTION_KEYSTORE_ERROR) === false) {
            return;
        }

        $this->setupKeystore();
    }

    /**
     * Render the persistent keystore-failure admin notice, if any.
     * Bound to admin_notices.
     *
     * @return void
     */
    public function renderKeystoreNotice(): void
    {
        $message = get_option(self::OPTION_KEYSTORE_ERROR);
        if (!is_string($message) || $message === '') {
            return;
        }
        if (function_exists('current_user_can') && !current_user_can('manage_options')) {
            return;
        }

        echo '<div class="notice notice-error"><p><strong>'
            . esc_html('WPMgr Agent: setup incomplete.') . '</strong> '
            . esc_html($message) . '</p></div>';
    }

    /**
     * Deactivation hook: clear all scheduled cron events.
     *
     * @return void
     */
    public function deactivate(): void
    {
        $this->scheduler->clearEvents();
    }

    /**
     * Create the anti-replay jti table via dbDelta.
     *
     * @return void
     */
    private function createJtiTable(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }

        $table   = (isset($wpdb->prefix) ? (string) $wpdb->prefix : 'wp_') . Connector::JTI_TABLE;
        $charset = method_exists($wpdb, 'get_charset_collate') ? $wpdb->get_charset_collate() : '';

        $sql = "CREATE TABLE {$table} (
            id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
            jti_hash CHAR(64) NOT NULL,
            expires_at BIGINT UNSIGNED NOT NULL,
            created_at BIGINT UNSIGNED NOT NULL,
            PRIMARY KEY  (id),
            UNIQUE KEY jti_hash (jti_hash),
            KEY expires_at (expires_at)
        ) {$charset};";

        if (defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/upgrade.php')) {
            require_once ABSPATH . 'wp-admin/includes/upgrade.php';
        }

        if (function_exists('dbDelta')) {
            dbDelta($sql);
        }
    }

    /**
     * Build the command registry.
     *
     * @return array<int,CommandInterface>
     */
    private function commands(): array
    {
        // Shared backup/restore collaborators. The age identity manager owns the
        // site's PRIVATE backup key (in the encrypted keystore); the transport
        // reuses the M2 Ed25519 Signer for the CP callbacks.
        $ageIdentity = new AgeIdentity($this->keystore);
        $source      = new BackupSource();
        $transport   = new BackupTransport($this->signer);

        return [
            new InfoCommand(),
            new BackupCommand($ageIdentity, $source, $transport),
            new RestoreCommand($ageIdentity, $source, $transport),
            new UpdateCommand(),
            new RollbackCommand(),
            new ScanCommand(),
            new MetadataCommand(),
        ];
    }

    /**
     * Expose the keystore (e.g. for provisioning tooling).
     *
     * @return Keystore
     */
    public function keystore(): Keystore
    {
        return $this->keystore;
    }
}
