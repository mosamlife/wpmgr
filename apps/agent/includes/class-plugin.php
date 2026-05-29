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

use WPMgr\Agent\Backup\FilesRestorer;
use WPMgr\Agent\Backup\RestoreWatchdog;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Commands\AutologinCommand;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\InfoCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\RefreshInventoryCommand;
use WPMgr\Agent\Commands\RestoreCommand;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Commands\ScanCommand;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\AgeIdentity;

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

    private ReplayCache $autologinReplay;

    private AutologinCommand $autologin;

    /**
     * Private constructor wires the object graph.
     */
    private function __construct()
    {
        $this->keystore         = new Keystore();
        $this->settings         = new Settings();
        $this->connector        = new Connector($this->keystore, $this->settings);
        $this->signer           = new Signer($this->keystore);
        // Enrollment + Scheduler must exist BEFORE commands() runs so the
        // refresh_inventory command can hold references to them — it triggers
        // a transient refresh + metadata push on demand.
        // Metadata pushes include the agent's age PUBLIC recipient so the CP can
        // register it on sites.age_recipient — M4 backups refuse otherwise.
        $this->enrollment       = new Enrollment($this->keystore, $this->settings, $this->signer, new MetadataCommand(new AgeIdentity($this->keystore)));
        $this->scheduler        = new Scheduler($this->settings, $this->enrollment);
        $this->router           = new Router($this->connector, $this->commands());
        $this->admin            = new Admin($this->settings, $this->enrollment, $this->keystore);
        $this->autologinReplay  = new ReplayCache();
        $this->autologin        = new AutologinCommand($this->connector, $this->autologinReplay, $this->signer, $this->settings);
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
        // Schema migration runner: WP does NOT run register_activation_hook on
        // a same-version re-upload, which previously left the M5.5 autologin
        // replay table missing on existing installs that re-uploaded the new
        // plugin zip. Wire Schema::ensureCurrent() to plugins_loaded so any
        // boot path (activation, re-upload, manual file replacement) heals
        // missing tables. The helper itself short-circuits with a single
        // get_option() lookup when the schema is already current.
        add_action('plugins_loaded', [$this, 'maybeRunSchemaMigrations']);

        add_action('rest_api_init', [$this->router, 'registerRoutes']);
        add_action('rest_api_init', [$this, 'registerAutologinRoute']);

        // Autologin replay-table maintenance: drop expired rows hourly. The
        // cron event is scheduled at activation; this hook binds the handler.
        // Wrap in a void closure: ReplayCache::prune() returns an int (rows
        // purged), but WP cron callbacks must not return anything.
        add_action(ReplayCache::HOOK_PRUNE, function (): void {
            $this->autologinReplay->prune();
        });

        // M5.6 / ADR-033 — backup task watchdog. BackupCommand schedules a
        // wp_schedule_single_event firing every ~120 s while a task is
        // active; the handler inspects the wpmgr_backup_tasks row and
        // either re-arms itself (alive) or re-enters TaskRunner::run()
        // (stalled). The single-arg signature matches the wp_schedule_single_event
        // $args = [$snapshot_id] shape.
        add_action(Watchdog::HOOK, [Watchdog::class, 'run'], 10, 1);

        // M5.6 / ADR-033 (v0.7.6) — backup-run cron event. BackupCommand
        // hands off the actual work to this via wp_schedule_single_event +
        // spawn_cron() so the original REST request can return its ACK in
        // milliseconds. /wp-cron.php fires in a SEPARATE FPM worker; this
        // handler dispatches TaskRunner UNCONDITIONALLY (vs Watchdog::run
        // which short-circuits unless the task is stalled — wrong for the
        // first-run dispatch path, since a freshly-queued task by
        // definition is not stalled yet).
        add_action('wpmgr_backup_run', [Watchdog::class, 'dispatch'], 10, 1);

        // M5.6 / ADR-034 — restore task watchdog (stall detection).
        // RestoreCommand schedules a wp_schedule_single_event firing every
        // ~120 s while a restore task is active; the handler inspects the
        // wpmgr_restore_tasks row and either re-arms itself (alive) or
        // re-enters RestoreRunner::run (stalled). The two-arg signature
        // matches wp_schedule_single_event $args = [$snapshotId,$restoreId].
        add_action(RestoreWatchdog::HOOK, [RestoreWatchdog::class, 'run'], 10, 2);

        // M5.6 / ADR-034 — restore-run cron event. RestoreCommand hands off
        // the actual work via wp_schedule_single_event + spawn_cron() so
        // the original REST request can return its ACK in milliseconds.
        // Dispatched UNCONDITIONALLY (mirrors the backup-side wpmgr_backup_run
        // contract).
        add_action(RestoreWatchdog::HOOK_RUN, [RestoreWatchdog::class, 'dispatch'], 10, 2);

        // M5.6 / ADR-034 — 24 h GC of `.wpmgr-old-files-*` and
        // `.wpmgr-staging-*` directories left behind by RestoreRunner so the
        // operator has a manual-rollback window. Scheduled by RestoreRunner
        // on cleanup; the handler sweeps anything older than 24 h.
        add_action('wpmgr_restore_oldfiles_gc', [FilesRestorer::class, 'gcOldFiles']);

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
        // Force schema sync on activation so a fresh install always lands
        // current, even if the migration-version option is somehow stale.
        Schema::ensureCurrent(true);

        $this->setupKeystore();

        // Record first-activation time and schedule reporting + safety events.
        $now = time();
        $this->settings->markActivated($now);
        $this->scheduler->scheduleEvents($now);

        // Hourly prune of the autologin replay table.
        if (function_exists('wp_next_scheduled') && function_exists('wp_schedule_event')
            && wp_next_scheduled(ReplayCache::HOOK_PRUNE) === false
        ) {
            wp_schedule_event($now + 60, 'hourly', ReplayCache::HOOK_PRUNE);
        }
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

        if (function_exists('wp_clear_scheduled_hook')) {
            wp_clear_scheduled_hook(ReplayCache::HOOK_PRUNE);
        }
    }

    /**
     * Register the GET /wpmgr/v1/autologin route. SEPARATE from the dispatch
     * router: this route is browser-initiated and the JWT (verified inside the
     * handler) is the authorization, so permission_callback is __return_true.
     *
     * @return void
     */
    public function registerAutologinRoute(): void
    {
        if (!function_exists('register_rest_route')) {
            return;
        }

        register_rest_route(
            Router::NAMESPACE,
            '/autologin',
            [
                'methods'             => 'GET',
                'callback'            => [$this->autologin, 'handle'],
                'permission_callback' => '__return_true',
                'args'                => [
                    'token'       => [
                        'required' => true,
                        'type'     => 'string',
                    ],
                    'redirect_to' => [
                        'required' => false,
                        'type'     => 'string',
                        'default'  => '',
                    ],
                ],
            ]
        );
    }

    /**
     * Run pending schema migrations on plugins_loaded. The helper itself
     * short-circuits with a single get_option() when the schema is already
     * current, so binding it to plugins_loaded is effectively zero-cost on
     * the hot path. The whole point is to catch re-uploads / same-version
     * installs where register_activation_hook does NOT fire.
     *
     * @return void
     */
    public function maybeRunSchemaMigrations(): void
    {
        Schema::ensureCurrent();
    }

    /**
     * Build the command registry.
     *
     * @return array<int,CommandInterface>
     */
    private function commands(): array
    {
        // Shared collaborators. The age identity manager owns the site's
        // PRIVATE backup key (in the encrypted keystore); BackupCommand uses
        // it for recipient-match validation, MetadataCommand for the public
        // recipient push. RestoreCommand instantiates its own seams inside
        // the cron worker so the REST entry point stays minimal.
        $ageIdentity = new AgeIdentity($this->keystore);

        return [
            new InfoCommand(),
            // M5.6 / ADR-033: BackupCommand validates the signed CP request,
            // dedups, seeds the wpmgr_backup_tasks row, schedules the
            // watchdog cron event, then hands off via wp_schedule_single_event
            // + spawn_cron() so the original REST request ACKs in ms.
            new BackupCommand($ageIdentity),
            // M5.6 / ADR-034: RestoreCommand mirrors the BackupCommand
            // pattern — dedup, seed wpmgr_restore_tasks, schedule the
            // wpmgr_restore_watchdog cron, hand off via wp_schedule_single_event
            // + spawn_cron() bound to wpmgr_restore_run. No collaborators here;
            // RestoreRunner builds its own (BackupTransport, AgeIdentity,
            // FilesRestorer, DbRestorer) inside the cron worker.
            new RestoreCommand(),
            new UpdateCommand(),
            new RollbackCommand(),
            new ScanCommand(),
            new MetadataCommand($ageIdentity),
            // v0.9.0 — on-demand refresh: re-poll WP update transients and
            // immediately push fresh metadata so the dashboard can render
            // available-update counts without waiting for the 30-min cron.
            // Closures are used so the command stays unit-testable without
            // doubling the `final` Enrollment / Scheduler classes.
            new RefreshInventoryCommand(
                function (): void {
                    $this->scheduler->refreshUpdateTransients(true);
                },
                fn (): array => $this->enrollment->pushMetadata(),
            ),
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
