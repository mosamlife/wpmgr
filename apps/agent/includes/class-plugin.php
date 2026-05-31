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
use WPMgr\Agent\Commands\DiagnosticsCommand;
use WPMgr\Agent\Commands\InfoCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\RefreshInventoryCommand;
use WPMgr\Agent\Commands\RestoreCommand;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Commands\GetFileCommand;
use WPMgr\Agent\Commands\ScanCommand;
use WPMgr\Agent\Commands\SyncErrorConfigCommand;
use WPMgr\Agent\Commands\SyncLoginBrandCommand;
use WPMgr\Agent\Commands\SyncSecurityConfigCommand;
use WPMgr\Agent\Commands\UnblockIpCommand;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Diagnostics\SizeProbe;
use WPMgr\Agent\Support\ActivityLog;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\ErrorMonitor;
use WPMgr\Agent\Support\LoginBrand;
use WPMgr\Agent\Support\LoginProtection;
use WPMgr\Agent\Support\MuPluginInstaller;

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

    /**
     * v0.9.13 — Unix timestamp of the most recent SUCCESSFUL diagnostics push
     * to the CP via /agent/v1/diagnostics. Updated at the end of
     * Plugin::runDiagnostics() after a 2xx response from shipPayload. Read by
     * the heartbeat backstop (Scheduler::runHeartbeat) which schedules a
     * one-shot diagnostics push when more than 6h have elapsed since the last
     * known push, so a fresh install does not have to wait out the jittered
     * daily cron's 0-4h offset before the operator sees any data in the
     * Health tab.
     */
    public const OPTION_LAST_DIAGNOSTICS_AT = 'wpmgr_agent_last_diagnostics_at';

    private static ?Plugin $instance = null;

    private Keystore $keystore;

    private Connector $connector;

    private Router $router;

    private Settings $settings;

    private Signer $signer;

    private Enrollment $enrollment;

    private Scheduler $scheduler;

    private Lifecycle $lifecycle;

    private Admin $admin;

    private ReplayCache $autologinReplay;

    private AutologinCommand $autologin;

    /**
     * ADR-037 Sprint 2 — error monitor. Installed during boot so the agent
     * captures PHP errors that fire during plugins_loaded and later. Errors
     * captured BEFORE the agent boots are queued by the mu-plugin loader and
     * drained by the monitor's install() call.
     */
    private ErrorMonitor $errorMonitor;

    /**
     * ADR-037 Sprint 2 — mu-plugin installer. Copies the error-trap loader
     * into wp-content/mu-plugins/ on activation + on plugins_loaded (idempotent).
     */
    private MuPluginInstaller $muInstaller;

    /**
     * ADR-037 Sprint 3 — hash-chained WP activity recorder. Binds ~30 WP hooks
     * (posts/comments/users/auth/plugins/themes/core/terms/allowlisted options
     * + WooCommerce when present), appends rows to wpmgr_activity_log, and ships
     * batches to /agent/v1/activity on the 5-min cron + heartbeat backstop.
     */
    private ActivityLog $activityLog;

    /**
     * S2 — login-protection engine. Registers the authenticate/wp_login/
     * wp_login_failed hooks when mode != disabled; records login events to
     * wpmgr_login_events and ships batches to /agent/v1/security/login-events
     * on the heartbeat.
     */
    private LoginProtection $loginProtection;

    /**
     * Login Whitelabel — cosmetic login-page branding pushed from the CP.
     * Applies logo, logo link, and a short message to wp-login.php via WP
     * hooks when at least one brand field is non-empty.
     */
    private LoginBrand $loginBrand;

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
        // Connection lifecycle (ADR-039/040/041): owns revoke-self, the
        // immediate post-enroll heartbeat, and the deactivate/uninstall
        // last-wills. The Scheduler delegates heartbeat-instruction handling
        // here; the Admin uses heartbeatNow() after a successful re-enroll.
        $this->lifecycle        = new Lifecycle($this->keystore, $this->settings, $this->enrollment);
        $this->scheduler        = new Scheduler($this->settings, $this->enrollment, $this->lifecycle);

        // Monitors/recorders/engines that COMMAND HANDLERS hold references to must
        // be constructed BEFORE commands() runs. commands() executes inside the
        // Router constructor below; sync_error_config / sync_security_config /
        // unblock_ip pass $this->errorMonitor and $this->loginProtection into their
        // handler constructors, and reading an uninitialised typed property there
        // is a fatal ("must not be accessed before initialization"). Order:
        //   ADR-037 S2 error monitor + mu-plugin installer,
        //   ADR-037 S3 activity recorder,
        //   S2 login-protection engine (depends on the activity recorder).
        $this->errorMonitor = new ErrorMonitor();
        $pluginDir = defined('WPMGR_AGENT_DIR') ? (string) constant('WPMGR_AGENT_DIR') : '';
        $this->muInstaller = new MuPluginInstaller($pluginDir);
        $this->activityLog = new ActivityLog();
        // The ActivityLog is passed so block events are emitted as structured
        // activity rows for free (CP alerting picks them up on the next ship).
        $this->loginProtection = new LoginProtection($this->activityLog);
        // Login Whitelabel — cosmetic branding pushed from the CP. No external
        // dependencies; constructed here so sync_login_brand can hold a reference.
        $this->loginBrand = new LoginBrand();

        $this->router           = new Router($this->connector, $this->commands());
        $this->admin            = new Admin($this->settings, $this->enrollment, $this->keystore, $this->lifecycle);
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
        // Bound to a real method, NOT a closure: a Closure captured in
        // $wp_filter can trigger "Serialization of 'Closure' is not allowed"
        // when a persistent object cache or a cron-inspector plugin serializes
        // the hook table. pruneAutologinReplay() swallows prune()'s int return
        // (WP cron callbacks must not return a value).
        add_action(ReplayCache::HOOK_PRUNE, [$this, 'pruneAutologinReplay']);

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

        // ADR-037 Sprint 2 — diagnostics cron handler. Scheduler::scheduleEvents
        // sets up the cron event; the handler runs the on-demand DiagnosticsCommand
        // and pushes its result to the CP at /agent/v1/diagnostics. Kept here
        // rather than inside Scheduler so Sprint 1's lock on class-scheduler.php
        // (append-only) is respected.
        add_action(Scheduler::HOOK_DIAGNOSTICS, [$this, 'runDiagnostics']);

        // Reliable-diagnostics — dedicated size-refresh cron handler. Runs the
        // SizeProbe walk under set_time_limit(0) so recurse_dirsize/du has no
        // ceiling imposed by the push request's max_execution_time. Plugin owns
        // the binding; Scheduler owns the schedule (additive).
        add_action(Scheduler::HOOK_SIZES, [$this, 'runSizeProbe']);

        // Register the pre_recurse_dirsize filter (WP 5.6+) so WP core's own
        // Site Health screen AND our PHP fallback both short-circuit to du when
        // exec is available. Installed once per boot; idempotent.
        (new SizeProbe())->registerPreRecurseFilter();

        // ADR-037 Sprint 3 — bind the ~30 activity-capture WP hooks. The
        // recorder writes to wpmgr_activity_log locally; it does NOT ship
        // inline (too chatty). Shipping is batched via the dedicated 5-min
        // cron (HOOK_ACTIVITY_SHIP) + the heartbeat backstop below.
        $this->activityLog->registerHooks();

        // ADR-037 Sprint 3 — dedicated activity-ship cron handler. Scheduler
        // owns the 5-min schedule; this binds the callback (Plugin owns
        // ActivityLog + shipPayload, so the binding lives here to keep the
        // Scheduler edit additive, mirroring HOOK_DIAGNOSTICS).
        add_action(Scheduler::HOOK_ACTIVITY_SHIP, [$this, 'shipActivity']);

        // ADR-037 Sprint 3 — heartbeat backstop: also drain a batch right
        // after a successful heartbeat (mirrors the diagnostics/error backstop)
        // so activity reaches the CP even if the dedicated cron event was lost.
        add_action(Scheduler::HOOK_HEARTBEAT, [$this, 'shipActivity'], 20);

        // S2 — heartbeat backstop for login events: drain a batch right after
        // each heartbeat so login-event rows reach the CP within 5 minutes
        // even on sites where the daily diagnostics cron fires infrequently.
        // Priority 25 so it runs after the activity ship (priority 20).
        add_action(Scheduler::HOOK_HEARTBEAT, [$this, 'shipLoginEventsPublic'], 25);

        // PHP-error ship: dedicated 5-min cron (HOOK_ERRORS_SHIP) + heartbeat
        // backstop (priority 30, after login events at 25). Previously errors
        // shipped ONLY on the daily diagnostics cron, so they reached the
        // dashboard hours late / "randomly"; this gives them the same 5-min
        // cadence as activity + login events. A fatal also schedules a one-shot
        // ship onto HOOK_ERRORS_SHIP for sub-minute latency (ErrorMonitor).
        add_action(Scheduler::HOOK_ERRORS_SHIP, [$this, 'shipErrors']);
        add_action(Scheduler::HOOK_HEARTBEAT, [$this, 'shipErrors'], 30);

        // ADR-037 Sprint 2 — install the error monitor + heal the mu-plugin
        // copy on every boot. install() is idempotent; the mu-installer
        // short-circuits via a sha1_file content match.
        $this->errorMonitor->install();
        $this->muInstaller->install();

        // S2 — install login-protection hooks when mode != disabled. The call
        // is idempotent (static guard inside LoginProtection::install). We also
        // install the WAF mu-plugin so the early IP-deny gate is armed on the
        // next request even before WordPress fully boots.
        $this->loginProtection->install();
        // Only arm the early IP-deny WAF mu-plugin when protection is actually
        // enabled. An inert (unconfigured) site installs no security mu-plugin,
        // so a fresh plugin update cannot affect the request path at all.
        if ($this->loginProtection->isEnabled()) {
            $this->muInstaller->installWaf();
        }

        // Login Whitelabel — bind login_head/login_headerurl/login_message hooks
        // only when at least one brand field is non-empty (self-gating). The
        // call is idempotent (static guard inside LoginBrand::install).
        $this->loginBrand->install();

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
            // ADR-040 — signed last-will on uninstall. The uninstall callback
            // MUST be a static method (no plugin instance / $this exists when
            // WordPress runs the uninstall hook); Lifecycle::on_uninstall builds
            // its own object graph, posts the signed disconnect, then wipes
            // keys + drops the agent's options/transients.
            if (function_exists('register_uninstall_hook')) {
                register_uninstall_hook(WPMGR_AGENT_FILE, [Lifecycle::class, 'on_uninstall']);
            }
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

        // ADR-037 Sprint 2 — install the error-trap mu-plugin loader. Best-
        // effort: a host where wp-content/mu-plugins/ is not writable will
        // surface this through the diagnostics endpoint rather than fatal
        // the activation.
        $this->muInstaller->install();

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

        // v0.9.13 — push diagnostics within ~30s of activation rather than
        // waiting out the jittered daily cron's 0..4h first-fire offset
        // (Scheduler::diagnosticsJitter). The single-event below fires the
        // SAME HOOK_DIAGNOSTICS hook ONCE, sooner, on top of the recurring
        // schedule that Scheduler::scheduleEvents installed above.
        // runDiagnostics is a no-op pre-enrollment (it checks
        // $settings->isEnrolled()), so arming this before pairing is safe; the
        // FIRST wp-cron tick after pairing will then push real data.
        // wp_schedule_single_event itself dedupes any duplicate hook+args
        // within a 10-minute window, so calling it unconditionally on every
        // activation is safe across re-uploads.
        if (function_exists('wp_schedule_single_event')) {
            wp_schedule_single_event($now + 30, Scheduler::HOOK_DIAGNOSTICS);
            // Prime the size probe ~20s before the diagnostics prime so the
            // push at +30s already has a persisted last-good to read from.
            // wp_schedule_single_event self-dedupes within 10 min; safe on
            // re-upload. isEnrolled() guard is inside runSizeProbe().
            wp_schedule_single_event($now + 10, Scheduler::HOOK_SIZES);
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
     * Deactivation hook (ADR-040): send a SIGNED best-effort last-will
     * disconnect (reason=deactivated, 3s budget) so the CP flips the site to
     * `disconnected` immediately, THEN clear all scheduled cron events.
     *
     * Deliberately does NOT wipe keys — a deactivate may be temporary, and a
     * later re-activation should resume against the same enrollment. (Key
     * wiping happens on revoke and on uninstall, not here.) The last-will is
     * best-effort: deactivation must complete even if the CP is unreachable.
     *
     * @return void
     */
    public function deactivate(): void
    {
        // Best-effort signed last-will FIRST (while keys still exist), bounded
        // to 3s so an unreachable CP cannot hang the deactivation request.
        $this->lifecycle->onDeactivate();

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
            // S3 — on-demand single-file fetch for scan findings inspection.
            // The CP only calls this for a path already stored as a finding
            // (server-side guard); agent enforces containment + dir/symlink/
            // size guards here independently.
            new GetFileCommand(),
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
            // ADR-037 Sprint 2 — on-demand 14-category site-health collector.
            // Single REST verb: POST /wp-json/wpmgr/v1/command/diagnostics
            // returns the full payload synchronously. The CP also pulls a
            // daily push via the wpmgr_agent_diagnostics_daily cron event,
            // routed through runDiagnostics() below.
            new DiagnosticsCommand(),
            // S1.2 — per-site error config sync. The CP pushes an error_level
            // bitmask + ignore_md5s fingerprint list; the agent writes it to
            // OPTION_CONFIG and ErrorMonitor honours it on the next record().
            new SyncErrorConfigCommand($this->errorMonitor),
            // S2 — security config sync. The CP pushes mode + thresholds +
            // ip_header + allow_cidrs + deny_cidrs; LoginProtection::applyConfig
            // validates, writes wpmgr_security_config, and clears the instance
            // cache so block decisions in this request see the new values.
            new SyncSecurityConfigCommand($this->loginProtection),
            // S2 — IP unblock. The CP sends a single IP; LoginProtection::unblockIp
            // deletes its failure rows so the failure counter resets to zero.
            new UnblockIpCommand($this->loginProtection),
            // Login Whitelabel — cosmetic branding sync. The CP pushes logo_url,
            // logo_link, and message; LoginBrand::applyConfig validates and writes
            // wpmgr_login_brand; the login-page hooks pick it up on next request.
            new SyncLoginBrandCommand($this->loginBrand),
        ];
    }

    /**
     * Cron handler for the daily diagnostics push. Builds the 14-category
     * blob via DiagnosticsCommand and forwards it to the CP through the
     * existing Enrollment client (which signs the request with the site's
     * Ed25519 key).
     *
     * Wired in registerHooks() to Scheduler::HOOK_DIAGNOSTICS. The Scheduler
     * computes the per-site jitter at schedule time; here we just execute.
     *
     * @return void
     */
    public function runDiagnostics(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        $payload = (new DiagnosticsCommand())->execute([], []);
        $result  = $this->shipPayload('/agent/v1/diagnostics', $payload);
        // v0.9.13 — only record the timestamp on a 2xx so the heartbeat
        // backstop (Scheduler::runHeartbeat) re-arms a one-shot push if the
        // CP was unreachable on this tick. Storing on ALL ship attempts would
        // mask a 5xx run and delay recovery by up to 6 hours.
        if (is_array($result) && ($result['ok'] ?? false)) {
            update_option(self::OPTION_LAST_DIAGNOSTICS_AT, time(), false);
        }

        // Also drain any pending PHP-error batch on this tick. The PRIMARY
        // cadence is now the dedicated 5-min HOOK_ERRORS_SHIP cron + heartbeat
        // backstop (shipErrors()); keeping the call here too is harmless.
        $this->shipErrors();

        // S2 — ship any pending login-event batch on this same cron tick.
        // LoginProtection::shipBatch returns up to SHIP_BATCH (100) newest rows
        // above the stored cursor. We POST the batch and advance the local
        // cursor to the highest id we sent on a 2xx, mirroring the error-ship
        // block above.
        $this->shipLoginEvents();

        // Reliable-diagnostics opportunistic warm: if the PHP-FPM fast-finish
        // hook is available, release the HTTP response to the CP first, then
        // run a size probe to warm the cache for the next push. On a kill mid-
        // walk (request_terminate_timeout) the previously-persisted last-good
        // remains intact. On non-FPM SAPIs the probe still runs in-process but
        // is in a non-blocking position (response already shipped via cron).
        if (function_exists('fastcgi_finish_request')) {
            fastcgi_finish_request();
        }
        if (function_exists('set_time_limit')) {
            @set_time_limit(0);
        }
        (new SizeProbe())->compute();
    }

    /**
     * Cron handler for the dedicated directory-size refresh event
     * (Scheduler::HOOK_SIZES). Runs set_time_limit(0) so the du / recurse_dirsize
     * walk has no time ceiling, then delegates to SizeProbe::compute() which
     * persists the result to the non-autoloaded wp_option wpmgr_agent_dir_sizes.
     * A WP-Cron kill mid-walk leaves the previously-persisted last-good intact
     * (SizeProbe::compute() writes atomically via update_option at the end).
     *
     * No isEnrolled() guard here — computing sizes is safe at any time and the
     * push-side mergeDirectorySizes() reads the result regardless of enrollment
     * state. The priming single-event at activation fires this before enrollment
     * so the first push after pairing already has data.
     *
     * @return void
     */
    public function runSizeProbe(): void
    {
        if (function_exists('set_time_limit')) {
            @set_time_limit(0);
        }
        (new SizeProbe())->compute();
    }

    /**
     * Cron handler bound to ReplayCache::HOOK_PRUNE (hourly): drop expired
     * autologin replay rows. A real public method (not a closure) so the WP
     * hook table never holds a Closure. ReplayCache::prune() returns an int
     * (rows purged); we discard it because WP cron callbacks must return void.
     *
     * @return void
     */
    public function pruneAutologinReplay(): void
    {
        $this->autologinReplay->prune();
    }

    /**
     * Drain and ship any pending PHP-error batch to /agent/v1/errors. Bound to
     * the dedicated 5-min HOOK_ERRORS_SHIP cron AND the heartbeat backstop
     * (priority 30), and also called from runDiagnostics() — mirroring how
     * activity + login events ship, so captured errors reach the dashboard
     * within ~5 min (or seconds for a fatal that scheduled a one-shot ship)
     * instead of riding the daily diagnostics cron. shipBatch() returns up to
     * 50 rows above the cursor; we POST them and advance the cursor on a 2xx.
     *
     * @return void
     */
    public function shipErrors(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        $errors = $this->errorMonitor->shipBatch();
        if ($errors === []) {
            return;
        }
        $highest     = 0;
        $maxLastSeen = 0;
        foreach ($errors as $row) {
            $id = (int) ($row['id'] ?? 0);
            if ($id > $highest) {
                $highest = $id;
            }
            $ls = (int) ($row['last_seen'] ?? 0);
            if ($ls > $maxLastSeen) {
                $maxLastSeen = $ls;
            }
        }
        $result = $this->shipPayload('/agent/v1/errors', ['errors' => $errors]);
        if (is_array($result) && ($result['ok'] ?? false)) {
            $this->errorMonitor->advanceCursor($highest);
            $this->errorMonitor->advanceShipTs($maxLastSeen);
        }
    }

    /**
     * Public wrapper bound to the HOOK_HEARTBEAT action (priority 25). Delegates
     * to shipLoginEvents() after verifying enrollment. WP action callbacks must
     * be public; the private helper keeps the logic contained.
     *
     * @return void
     */
    public function shipLoginEventsPublic(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        $this->shipLoginEvents();
    }

    /**
     * Ship a batch of pending login events to /agent/v1/security/login-events.
     * No-op until enrolled and until the batch is non-empty. Mirrors the error-
     * ship block in runDiagnostics() and is also called from the heartbeat
     * backstop (shipActivity priority 20) so events drain even if the daily
     * diagnostics cron fires infrequently.
     *
     * @return void
     */
    private function shipLoginEvents(): void
    {
        $loginEvents = $this->loginProtection->shipBatch();
        if ($loginEvents === []) {
            return;
        }
        $highest = 0;
        foreach ($loginEvents as $row) {
            $id = (int) ($row['id'] ?? 0);
            if ($id > $highest) {
                $highest = $id;
            }
        }
        $result = $this->shipPayload(
            '/agent/v1/security/login-events',
            ['login_events' => $loginEvents]
        );
        if (is_array($result) && ($result['ok'] ?? false)) {
            $this->loginProtection->advanceCursor($highest);
        }
    }

    /**
     * ADR-037 Sprint 3 — ship a batch of unshipped activity rows to the CP at
     * /agent/v1/activity. No-op until enrolled. Bound to both the dedicated
     * 5-min HOOK_ACTIVITY_SHIP cron and (priority 20) the heartbeat as a
     * backstop. ActivityLog::ship builds the batch, hands the signed POST to
     * shipPayload, and marks rows shipped on a 2xx (so a 5xx leaves them
     * pending for the next tick).
     *
     * @return void
     */
    public function shipActivity(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        $version = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '';
        $this->activityLog->ship(
            fn (string $path, array $payload): array => $this->shipPayload($path, $payload),
            $version
        );
    }

    /**
     * Expose the ActivityLog (e.g. for tooling or tests).
     *
     * @return ActivityLog
     */
    public function activityLog(): ActivityLog
    {
        return $this->activityLog;
    }

    /**
     * Sign-and-POST a JSON payload to the control plane. Local replacement
     * for Enrollment::signedPost so Sprint 2 does not need to touch
     * class-enrollment.php (Sprint 1 has parallel work there).
     *
     * @param string $path Request path (e.g. /agent/v1/diagnostics).
     * @param array<string,mixed> $payload Payload to JSON-encode and sign.
     * @return array{ok:bool,status:int}
     */
    private function shipPayload(string $path, array $payload): array
    {
        if (!function_exists('wp_json_encode') || !function_exists('wp_remote_post')) {
            return ['ok' => false, 'status' => 0];
        }
        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return ['ok' => false, 'status' => 0];
        }
        $body = (string) wp_json_encode($payload);
        try {
            $headers = $this->signer->signHeaders('POST', $path, $body);
        } catch (\Throwable $e) {
            return ['ok' => false, 'status' => 0];
        }
        $response = wp_remote_post(
            $base . $path,
            [
                'timeout' => 10,
                'headers' => array_merge(
                    ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
                    $headers
                ),
                'body'    => $body,
            ]
        );
        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return ['ok' => false, 'status' => 0];
        }
        $status = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;
        return ['ok' => $status >= 200 && $status < 300, 'status' => $status];
    }

    /**
     * Expose the ErrorMonitor so the Scheduler's heartbeat can drain its
     * ship-batch into the next /agent/v1/errors call.
     *
     * @return ErrorMonitor
     */
    public function errorMonitor(): ErrorMonitor
    {
        return $this->errorMonitor;
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
