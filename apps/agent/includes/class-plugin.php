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
use WPMgr\Agent\Commands\ScanCommand;
use WPMgr\Agent\Commands\UpdateCommand;

/**
 * Top-level plugin orchestrator.
 */
final class Plugin
{
    private static ?Plugin $instance = null;

    private Keystore $keystore;

    private Connector $connector;

    private Router $router;

    /**
     * Private constructor wires the object graph.
     */
    private function __construct()
    {
        $this->keystore  = new Keystore();
        $this->connector = new Connector($this->keystore);
        $this->router    = new Router($this->connector, $this->commands());
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

        if (defined('WPMGR_AGENT_FILE')) {
            register_activation_hook(WPMGR_AGENT_FILE, [$this, 'activate']);
        }
    }

    /**
     * Activation hook: create the jti table and generate the site keypair.
     *
     * @return void
     */
    public function activate(): void
    {
        $this->createJtiTable();

        // Generate the site's own Ed25519 keypair on first activation only.
        if ($this->keystore->getSiteKeypair() === null) {
            $this->keystore->generateSiteKeypair();
        }
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
        return [
            new InfoCommand(),
            new BackupCommand(),
            new UpdateCommand(),
            new ScanCommand(),
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
