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

use WPMgr\Agent\Backup\BackupJanitor;
use WPMgr\Agent\Backup\FilesRestorer;
use WPMgr\Agent\Backup\RestoreRunner;
use WPMgr\Agent\Backup\RestoreWatchdog;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Commands\AutologinCommand;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\DiagnosticsCommand;
use WPMgr\Agent\Commands\InfoCommand;
use WPMgr\Agent\Commands\MediaApplyCommand;
use WPMgr\Agent\Commands\MediaDeleteOriginalsCommand;
use WPMgr\Agent\Commands\MediaOptimizeCommand;
use WPMgr\Agent\Commands\MediaRestoreCommand;
use WPMgr\Agent\Commands\MediaStatsCommand;
use WPMgr\Agent\Commands\MediaSyncCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\RefreshInventoryCommand;
use WPMgr\Agent\Commands\RestoreCommand;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Commands\FileArchiveCreateCommand;
use WPMgr\Agent\Commands\FileChmodCommand;
use WPMgr\Agent\Commands\FileDeleteCommand;
use WPMgr\Agent\Commands\FileDownloadPrepareCommand;
use WPMgr\Agent\Commands\FileExtractCommand;
use WPMgr\Agent\Commands\FileListCommand;
use WPMgr\Agent\Commands\FileMkdirCommand;
use WPMgr\Agent\Commands\FileReadCommand;
use WPMgr\Agent\Commands\FileRenameCommand;
use WPMgr\Agent\Commands\FileSearchCommand;
use WPMgr\Agent\Commands\FileUploadApplyCommand;
use WPMgr\Agent\Commands\FileVersionRestoreCommand;
use WPMgr\Agent\Commands\FileVersionsListCommand;
use WPMgr\Agent\Commands\FileWriteCommand;
use WPMgr\Agent\Commands\GetFileCommand;
use WPMgr\Agent\Commands\ScanCommand;
use WPMgr\Agent\Commands\SyncErrorConfigCommand;
use WPMgr\Agent\Commands\SyncLoginBrandCommand;
use WPMgr\Agent\Commands\SyncMediaConfigCommand;
use WPMgr\Agent\Commands\SyncSecurityConfigCommand;
use WPMgr\Agent\Commands\UnblockIpCommand;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Commands\CacheEnableCommand;
use WPMgr\Agent\Commands\CacheDisableCommand;
use WPMgr\Agent\Commands\CachePurgeCommand;
use WPMgr\Agent\Commands\CachePreloadCommand;
use WPMgr\Agent\Commands\PerfConfigUpdateCommand;
use WPMgr\Agent\Commands\RucssComputeCommand;
use WPMgr\Agent\Cache\PerfReporter;
use WPMgr\Agent\Commands\DbCleanCommand;
use WPMgr\Agent\Commands\DbOrphanDeleteCommand;
use WPMgr\Agent\Commands\DbScanCommand;
use WPMgr\Agent\Commands\DbSnapshotCommand;
use WPMgr\Agent\Commands\DbTableActionCommand;
use WPMgr\Agent\Commands\MediaCleanCommand;
use WPMgr\Agent\Commands\SearchReplaceCommand;
use WPMgr\Agent\Cache\AdminBarPurge;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\PreloadQueue;
use WPMgr\Agent\Commands\CachePreloadQueueStatusCommand;
use WPMgr\Agent\Commands\CachePreloadQueueRetryFailedCommand;
use WPMgr\Agent\Commands\CachePreloadQueueClearCommand;
use WPMgr\Agent\Commands\CachePreloadQueueTestRestCommand;
use WPMgr\Agent\Commands\ObjectcacheApplyConfigCommand;
use WPMgr\Agent\Commands\ObjectcacheDisableCommand;
use WPMgr\Agent\Commands\ObjectcacheEnableCommand;
use WPMgr\Agent\Commands\ObjectcacheFlushCommand;
use WPMgr\Agent\Commands\ObjectcacheTestCommand;
use WPMgr\Agent\Commands\PingCommand;
use WPMgr\Agent\Commands\ResendEmailCommand;
use WPMgr\Agent\Commands\SendTestEmailCommand;
use WPMgr\Agent\Commands\SyncEmailConfigCommand;
use WPMgr\Agent\Commands\SyncSecurityHardeningCommand;
use WPMgr\Agent\Commands\SyncSecurityPolicyCommand;
use WPMgr\Agent\Commands\RecordManagedFilesCommand;
use WPMgr\Agent\Security\BackupCodesProvider;
use WPMgr\Agent\Security\EmailCodeProvider;
use WPMgr\Agent\Security\HardeningModule;
use WPMgr\Agent\Security\HideBackendModule;
use WPMgr\Agent\Security\PasswordPolicyModule;
use WPMgr\Agent\Security\SecurityPolicy;
use WPMgr\Agent\Security\Site2faModule;
use WPMgr\Agent\Security\TotpProvider;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\EmailLogReporter;
use WPMgr\Agent\Email\Handlers\MailgunHandler;
use WPMgr\Agent\Email\Handlers\PostmarkHandler;
use WPMgr\Agent\Email\Handlers\SendgridHandler;
use WPMgr\Agent\Email\Handlers\SesHandler;
use WPMgr\Agent\Email\Handlers\SmtpHandler;
use WPMgr\Agent\Email\MailRouter;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Email\SuppressionCache;
use WPMgr\Agent\Optimizer\Bloat;
use WPMgr\Agent\Optimizer\DbCleanup;
use WPMgr\Agent\Optimizer\PerfConfig;
use WPMgr\Agent\Optimizer\RumInjector;
use WPMgr\Agent\Diagnostics\SizeProbe;
use WPMgr\Agent\Media\DiskWriter;
use WPMgr\Agent\Media\HtaccessInstaller;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\Rename;
use WPMgr\Agent\Support\ActivityLog;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\ConnectionFinisher;
use WPMgr\Agent\Support\ErrorMonitor;
use WPMgr\Agent\Support\LoginBrand;
use WPMgr\Agent\Support\LoginProtection;
use WPMgr\Agent\Support\LongRunningJob;
use WPMgr\Agent\Support\MuPluginInstaller;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateChecker;
use WPMgr\Agent\Support\UpdateInFlight;
use WPMgr\Agent\Support\UpdateWatchdogMarker;
use WPMgr\Agent\AutoOptimizeUpload;
use WPMgr\Agent\Webhooks\MediaModalInjector;

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

    /**
     * GitHub issue #232 — Unix timestamp of the last stalled-backup reaper
     * sweep (Watchdog::sweepStalled()), stamped BEFORE the sweep runs. Read
     * by maybeSweepStalledBackups() to throttle the sweep to once per
     * BACKUP_SWEEP_THROTTLE_SECONDS. Mirrors
     * SnapshotManager::OPTION_GC_LAST's throttle pattern exactly. Owned by
     * the agent — removed on uninstall via Lifecycle::ownedOptions().
     */
    public const OPTION_BACKUP_SWEEP_LAST = 'wpmgr_backup_sweep_last';

    /**
     * Throttle window for maybeSweepStalledBackups(): the actual sweep (a
     * bounded SELECT + at most 20 TaskRunner re-entries, each lock-guarded)
     * runs at most once per this many seconds; every other request pays only
     * one cheap get_option() read.
     */
    private const BACKUP_SWEEP_THROTTLE_SECONDS = 60;

    /**
     * GitHub issue #256: Unix timestamp of the last WP-Cron-independent
     * backup-scratch GC sweep (BackupJanitor::gcRuns()), stamped BEFORE the
     * sweep runs. Read by maybeGcBackupRuns() to throttle the sweep to once
     * per BACKUP_JANITOR_THROTTLE_SECONDS. Mirrors
     * SnapshotManager::OPTION_GC_LAST / OPTION_BACKUP_SWEEP_LAST's throttle
     * pattern exactly. Owned by the agent, removed on uninstall via
     * Lifecycle::ownedOptions().
     */
    public const OPTION_BACKUP_JANITOR_LAST = 'wpmgr_backup_janitor_gc_last';

    /**
     * Throttle window for maybeGcBackupRuns(): gcRuns() itself is cheap per
     * invocation (GH #256: a constant read plus one is_dir() on a site with
     * no backups, one scandir() on a tidy one; BackupJanitor::RUNS_GC_AGE_SECONDS
     * (6h) already bounds each top-level entry to single digits), so this
     * does not need the 60s cadence maybeSweepStalledBackups() uses to
     * recover an ACTIVELY stalled, still-time-sensitive run. It also does not
     * need to run every request: gcRuns() only ever reclaims directories that
     * are already at least RUNS_GC_AGE_SECONDS (6h) stale, so an hourly
     * cadence adds at most ~1h of extra opportunistic-reclaim delay on top of
     * that 6h floor, negligible relative to the leak this closes (which was
     * previously "never", not "up to 7h"). Matches
     * SnapshotManager::maybeGc()'s hourly throttle exactly, since both are
     * opportunistic backstops for the same class of job (a daily cron GC that
     * a quiet/DISABLE_WP_CRON site may never fire).
     */
    private const BACKUP_JANITOR_THROTTLE_SECONDS = 3600;

    /**
     * GitHub issue #256: Unix timestamp of the last WP-Cron-independent
     * restore-rollback-material GC sweep (FilesRestorer::gcOldFiles() +
     * RestoreRunner::gcPreRestoreDumps()), stamped BEFORE the sweep runs.
     * Read by maybeGcRestoreArtifacts() to throttle the sweep to once per
     * RESTORE_GC_THROTTLE_SECONDS. Owned by the agent, removed on uninstall
     * via Lifecycle::ownedOptions().
     */
    public const OPTION_RESTORE_GC_LAST = 'wpmgr_restore_gc_last';

    /**
     * Throttle window for maybeGcRestoreArtifacts(). FilesRestorer's default
     * retention window (OLDFILES_GC_AGE_SECONDS, 1h) is 6x shorter than
     * BackupJanitor::RUNS_GC_AGE_SECONDS (6h), so this uses a shorter
     * throttle than BACKUP_JANITOR_THROTTLE_SECONDS to keep the worst-case
     * extra opportunistic-reclaim delay proportionate to that shorter
     * window, while remaining cheap even at that cadence (both sweeps are a
     * handful of glob() calls against directories that typically match
     * nothing).
     */
    private const RESTORE_GC_THROTTLE_SECONDS = 900;

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
     * ADR-042 Phase 2 — CP-driven agent self-update. Hooks into the WordPress
     * plugin-update machinery and enforces the full security verification chain
     * before any bytes are swapped to disk. Self-hosted/SaaS builds only.
     * Null when WPMGR_WPORG_BUILD is true (wp.org distribution build, where
     * this whole subsystem is excluded and updates come from WordPress.org).
     */
    private ?UpdateChecker $updateChecker = null;

    /**
     * ADR-044 — Auto-optimize on upload. Owns the wp_generate_attachment_metadata
     * filter (priority 9999), the pending-id buffer, and the debounced drain
     * cron callback that POSTs the batch to the CP via shipPayload.
     */
    private AutoOptimizeUpload $autoOptimizeUpload;

    /**
     * Phase 3 — page-cache orchestrator. Owns the cache config, the request-path
     * hooks (output-buffer writer, auto-purge, role cookie, refresh cron), and
     * the high-level enable/disable/purge/preload/applyConfig operations the
     * cache command handlers call. Constructed BEFORE commands() so the handlers
     * can hold a reference; its registerHooks() is wired in registerHooks().
     */
    private CacheManager $cacheManager;

    /**
     * Admin-bar purge controls. Registers the WPMgr Cache node tree and the
     * two admin_post handlers for purge-all and purge-url.
     */
    private AdminBarPurge $adminBarPurge;

    /**
     * Per-site outgoing-mail router. Owns the `pre_wp_mail` hook, resolves
     * the correct provider handler, applies force-from and Return-Path, stamps
     * the X-WPMgr-Site correlation header, and writes every send to the local
     * email log when log_emails is enabled. Constructed BEFORE commands() so the
     * send_test_email command can hold a reference to the ProviderRouter.
     */
    private MailRouter $mailRouter;

    /**
     * Provider-level send dispatcher. Instantiated with all five v1 handlers
     * (smtp/ses/sendgrid/mailgun/postmark) and shared by MailRouter +
     * SendTestEmailCommand.
     */
    private ProviderRouter $providerRouter;

    /**
     * Phase 3b — email-log CP push reporter. Pages unpushed rows above the
     * stored cursor and POSTs them to /agent/v1/email/log every 5 min
     * (HOOK_PUSH) and opportunistically after a send via pushEmailLog().
     */
    private EmailLogReporter $emailLogReporter;

    /**
     * Phase 4b — local suppression cache. Pulls deltas from the CP every
     * 15 min (HOOK_PULL) and provides a pre-send is_suppressed() check to
     * ProviderRouter. Keeps suppressed address hashes in a wp-option; no
     * plaintext email addresses are stored locally.
     */
    private SuppressionCache $suppressionCache;

    /**
     * Connection-liveness catch-up sender. Hooks onto 'shutdown' (priority
     * 9999) and sends one heartbeat when the last recorded heartbeat is more
     * than 120 s overdue, fixing false disconnects on page-cached idle sites
     * where WP-Cron never fires.
     */
    private HeartbeatCatchup $heartbeatCatchup;

    /**
     * Security hardening module. Receives the config from
     * SyncSecurityHardeningCommand, persists it, and registers the WordPress
     * hooks that enforce each enabled toggle on every subsequent request.
     */
    private HardeningModule $hardeningModule;

    /**
     * Security Suite Phase 3 — site-user 2FA interstitial module. Loads the
     * stored policy and registers the wp_login / login_init hooks that
     * intercept primary-auth completions and require a second factor for
     * configured roles. Default-OFF until a sync_security_policy push enables it.
     */
    private Site2faModule $site2faModule;

    /**
     * Security Suite Phase 3 — password policy module. Enforces strength,
     * HIBP breach check, reuse block, and expiry requirements on password
     * set/change/reset hooks. Default-OFF until a policy push activates it.
     */
    private PasswordPolicyModule $passwordPolicyModule;

    /**
     * Security Suite Phase 3 — hide-backend module. Intercepts at setup_theme
     * to block the canonical wp-login/wp-admin for logged-out visitors and
     * serve the secret slug instead. Default-OFF.
     */
    private HideBackendModule $hideBackendModule;

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

        // Security hardening module. Constructed BEFORE commands() so
        // SyncSecurityHardeningCommand can hold a reference.
        $this->hardeningModule = new HardeningModule();

        // Security Suite Phase 3 — site-user auth policy modules. All three are
        // constructed BEFORE commands() so SyncSecurityPolicyCommand has no wiring
        // dependency (it writes the wp-option; modules read it at install() time).
        // Each module loads the stored policy from wp-options; a missing option
        // returns defaults (all-OFF) so fresh installs enforce nothing.
        $ageIdentityForTotp       = new AgeIdentity($this->keystore);
        $policyForModules         = SecurityPolicy::load();
        $this->site2faModule      = new Site2faModule(
            $policyForModules,
            [
                new TotpProvider($ageIdentityForTotp),
                new EmailCodeProvider(),
                new BackupCodesProvider(),
            ]
        );
        $this->passwordPolicyModule = new PasswordPolicyModule($policyForModules, $this->settings, $this->signer);
        $this->hideBackendModule    = new HideBackendModule($policyForModules);

        // ADR-042 Phase 2 — self-update checker. Shares the Signer, Settings,
        // Keystore, and a fresh ReplayCache (the autologin replay table — the
        // same jti table is used for manifest replay prevention, which is safe
        // because both use the same single-use semantics and non-overlapping jti
        // namespaces via different issuers).
        // Skipped for the wp.org distribution build (WPMGR_WPORG_BUILD constant).
        if (!defined('WPMGR_WPORG_BUILD') || !WPMGR_WPORG_BUILD) {
            $this->updateChecker = new UpdateChecker(
                $this->signer,
                $this->settings,
                $this->keystore,
                new ReplayCache()
            );
        }

        // ADR-044 — Auto-optimize on upload. The AutoOptimizeUpload instance is
        // constructed BEFORE commands() so the hooks can reference it. The
        // shipPayload closure captures $this safely — it is only invoked from the
        // HOOK_DRAIN cron callback, long after construction is complete.
        $this->autoOptimizeUpload = new AutoOptimizeUpload(
            $this->settings,
            fn (string $path, array $payload): array => $this->shipPayload($path, $payload)
        );

        // Phase 3 — page-cache orchestrator. Must exist BEFORE commands() so the
        // six cache command handlers (cache_enable/disable/purge/preload,
        // perf_config_update) can hold a reference. Default-constructed (it
        // builds its own WP_CACHE editor / drop-in installer / .htaccess manager
        // / nginx helper); inert until a cache_enable command flips it on.
        $this->cacheManager     = new CacheManager();
        $this->adminBarPurge    = new AdminBarPurge($this->cacheManager, $this->settings);

        // Email (Phase 2) — per-site outgoing-mail pipeline. Must exist BEFORE
        // commands() so SendTestEmailCommand can hold a ProviderRouter reference.
        // The ProviderRouter is inert until sync_email_config pushes a provider
        // config; MailRouter's pre_wp_mail filter returns null (leaves WP default
        // mail path untouched) when no email config is stored.
        $emailLogger              = new EmailLogger();
        $this->suppressionCache   = new SuppressionCache($this->settings, $this->signer);
        $this->providerRouter     = new ProviderRouter($this->keystore, $emailLogger, $this->suppressionCache);
        $this->providerRouter->register(new SmtpHandler());
        $this->providerRouter->register(new SesHandler());
        $this->providerRouter->register(new SendgridHandler());
        $this->providerRouter->register(new MailgunHandler());
        $this->providerRouter->register(new PostmarkHandler());
        $this->mailRouter        = new MailRouter($this->providerRouter, $this->settings);
        $this->emailLogReporter  = new EmailLogReporter($this->settings, $this->signer);

        $this->router              = new Router($this->connector, $this->commands());
        $this->heartbeatCatchup    = new HeartbeatCatchup($this->settings, $this->enrollment);
        $this->admin               = new Admin($this->settings, $this->enrollment, $this->keystore, $this->lifecycle, $this->updateChecker);
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
     * Reset the singleton for testing purposes.
     *
     * PHPUnit tests that call boot() must call this in their tear_down() to
     * prevent the constructed singleton (with its Keystore, cached master key,
     * and Brain Monkey stubs) from leaking into subsequent tests in the suite.
     * Never call this outside of a test context.
     *
     * @internal For use by PluginActivationTest only.
     * @return void
     */
    public static function resetForTesting(): void
    {
        self::$instance = null;
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

        // Opcache invalidation for the object-cache engine files: fires on any
        // boot where the stored plugin version differs from the current version.
        // This ensures stale bytecode from the previous version cannot keep
        // executing after an agent update, even when the drop-in installer is
        // not re-run (e.g. a background auto-update that does not re-enable).
        add_action('plugins_loaded', [$this, 'maybeInvalidateEngineOpcache']);

        // Cron self-heal (mirrors maybeRunSchemaMigrations). register_activation_hook
        // does NOT fire on a plugin UPDATE / same-version re-upload, but the
        // update's deactivate step DOES fire register_deactivation_hook →
        // Scheduler::clearEvents() wipes EVERY reporting cron. Net effect: after
        // an in-place agent update the heartbeat/metadata/diagnostics/activity/
        // error crons silently vanish and never return, so the agent stops
        // calling home and the CP heartbeat-timeout sweeper marks the site
        // disconnected (even though CP→agent pushes like backups still succeed
        // and briefly bump last_seen). This rebinds the recurring schedule on
        // any boot once the heartbeat event is found missing.
        add_action('plugins_loaded', [$this, 'maybeRescheduleCron']);

        // GitHub issue #210, MEDIUM-1b (security review follow-up) — the
        // "principled disarm" for the update-watchdog mu-plugin. Reaching
        // plugins_loaded at all is proof the WHOLE plugin-loading loop
        // (every active plugin, including any one an armed marker entry is
        // watching) completed this request without the every-request
        // bootstrap fatal GitHub issue #210 exists to catch — see
        // UpdateWatchdogMarker::disarmHealthy()'s own doc for the full
        // reasoning and its fail-closed guarantee. Gated internally on the
        // marker file existing (one is_file() stat), so this costs nothing
        // extra on the overwhelmingly common no-marker boot.
        add_action('plugins_loaded', [$this, 'maybeDisarmUpdateWatchdog']);

        // GitHub issue #226 — WP-Cron-INDEPENDENT, throttled reclaim trigger
        // for the wpmgr-snapshots/ store. Root cause: the only existing
        // reclaim paths — pruneForSlug() (runs only at the SAME slug's next
        // capture()) and gcExpired() (bound to the daily wpmgr_snapshot_gc
        // cron event, or invoked opportunistically at the start of an
        // `update` command) — both require something else to happen again
        // later. A fleet that bulk-updates once and goes quiet (or has
        // DISABLE_WP_CRON set) never sees either again, so a successful
        // update's own snapshot never gets reclaimed. Binding this to
        // plugins_loaded fires SnapshotManager::maybeGc() on every request
        // the agent serves post-enrollment — including the control plane's
        // own uptime probes and every signed command — so reclaim no longer
        // depends on cron or another update ever running. maybeGc() itself
        // throttles the actual sweep to once per hour via a stored option, so
        // this costs one cheap get_option() read on every other request.
        add_action('plugins_loaded', [$this, 'maybeGcSnapshots']);

        // GitHub issue #232 — WP-Cron-INDEPENDENT, throttled reaper trigger
        // for `wpmgr_backup_tasks` rows stalled at phase=queued (or any other
        // non-terminal phase). Root cause: BackupCommand's ONLY cron-
        // independent starter used to be gated on isLoopbackGated(), and its
        // recovery path (Watchdog's own `wpmgr_backup_watchdog` cron event)
        // is itself just as WP-Cron-dependent — so on a non-gated,
        // low-traffic/off-peak site (or DISABLE_WP_CRON) a freshly-seeded
        // task row could get stuck at `queued` forever with nothing left to
        // ever re-check it. Binding this to plugins_loaded — mirroring
        // maybeGcSnapshots() immediately above — fires
        // Watchdog::sweepStalled() on every request the agent serves
        // post-enrollment, including the control plane's own ~60s
        // heartbeat/uptime/diagnostic REST hits, so recovery no longer
        // depends on WP-Cron ticking at all. maybeSweepStalledBackups()
        // itself throttles the actual sweep to once per 60s via a stored
        // option (stamped BEFORE the sweep runs), so this costs one cheap
        // get_option() read on every other request.
        add_action('plugins_loaded', [$this, 'maybeSweepStalledBackups']);

        // GitHub issue #256: WP-Cron-INDEPENDENT, throttled reclaim trigger
        // for BackupJanitor::gcRuns() (the backup pipeline's
        // wpmgr-agent/runs/ scratch-dir GC backstop). Root cause: gcRuns()
        // was bound ONLY to its daily wpmgr_backup_runs_gc cron event, the
        // exact same WP-Cron-dependency gap that maybeGcSnapshots() and
        // maybeSweepStalledBackups() (both immediately above) already close
        // for their own stores. A quiet/low-traffic site, or one with
        // DISABLE_WP_CRON set, could delete a failed backup from the
        // dashboard and never again see a request that fires this event, so
        // the on-host scratch (DB dump, zip parts, chunk files) leaked
        // forever regardless of the age gate. Mirrors maybeGcSnapshots() /
        // maybeSweepStalledBackups() exactly; see maybeGcBackupRuns()'s own
        // doc for the full gating/throttle/safety contract.
        add_action('plugins_loaded', [$this, 'maybeGcBackupRuns']);

        // GitHub issue #256: WP-Cron-INDEPENDENT, throttled reclaim trigger
        // for the restore pipeline's deferred rollback material:
        // FilesRestorer::gcOldFiles() (`.wpmgr-old-*` / `.wpmgr-staging-*`
        // trees) and RestoreRunner::gcPreRestoreDumps() (the pre-restore DB
        // dump), both previously reachable ONLY via a wp_schedule_single_event
        // ONE-SHOT scheduled at the end of a successful RestoreRunner::runCleanup(),
        // worse than a recurring cron gap: a crashed/aborted restore never
        // schedules the event at all, and even a scheduled-but-unfired event
        // (WP-Cron never ticked) stays in the cron array and silently
        // suppresses every later restore's attempt to schedule a replacement
        // (see the one-shot fix in RestoreRunner::runCleanup() for that half).
        // Both GC targets are gated by their own on-disk `.expires` markers
        // (written well after the live swap completes), so calling them from
        // every request is safe by construction; see maybeGcRestoreArtifacts()'s
        // own doc.
        add_action('plugins_loaded', [$this, 'maybeGcRestoreArtifacts']);

        // M14: Guard against Performance Lab disabling our object-cache drop-in.
        // When the OC is configured (config file exists), register the filter so
        // Performance Lab cannot suppress it.
        $ocInstaller = new \WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller();
        if ($ocInstaller->state() === \WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller::STATE_OURS_CURRENT) {
            add_filter('perflab_disable_object_cache_dropin', '__return_true');
        }

        add_action('rest_api_init', [$this->router, 'registerRoutes']);
        add_action('rest_api_init', [$this, 'registerAutologinRoute']);
        // Task #171 — unsigned self-HMAC loopback runner route for the preload
        // queue. SEPARATE from the signed dispatch router: it is a fire-and-forget
        // loopback kick from the agent to itself and carries no command authority
        // (only drains an already-queued, same-host URL set). See §1.10.
        add_action('rest_api_init', [$this, 'registerPreloadRunRoute']);

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

        // Media Optimizer (scale fix) — background-run cron events for the three
        // bulk media commands. Each command's execute() now ACKs the CP in
        // milliseconds (returns 'accepted' after persisting the batch via
        // MediaRunStore + wp_schedule_single_event + spawn_cron), then a SEPARATE
        // FPM worker fired by /wp-cron.php drains the batch in bounded chunks
        // (rescheduling itself until empty). This mirrors wpmgr_backup_run above
        // and fixes the timeout where a synchronous bulk presign/upload loop blew
        // past the CP's HTTP client timeout, marking succeeded jobs as failed.
        // The single-arg signature matches wp_schedule_single_event $args=[$runId].
        add_action(MediaOptimizeCommand::RUN_HOOK, [MediaOptimizeCommand::class, 'runBackground'], 10, 1);
        add_action(MediaRestoreCommand::RUN_HOOK, [MediaRestoreCommand::class, 'runBackground'], 10, 1);
        add_action(MediaDeleteOriginalsCommand::RUN_HOOK, [MediaDeleteOriginalsCommand::class, 'runBackground'], 10, 1);

        // M5.6 / ADR-034 — 24 h GC of `.wpmgr-old-files-*` and
        // `.wpmgr-staging-*` directories left behind by RestoreRunner so the
        // operator has a manual-rollback window. Scheduled by RestoreRunner
        // on cleanup; the handler sweeps anything older than 24 h.
        add_action('wpmgr_restore_oldfiles_gc', [FilesRestorer::class, 'gcOldFiles']);

        // GH #146 — same cron event also GCs the forced pre-restore DB dump
        // (see RestoreRunner::capturePreRestoreDbDump()/runCleanup()) once
        // its retention window elapses, so both halves of the health-check
        // rollback material age out together.
        add_action('wpmgr_restore_oldfiles_gc', [RestoreRunner::class, 'gcPreRestoreDumps']);

        // M1 (issue #131 adversarial review) — recurring GC backstop for the
        // wpmgr-snapshots/ store UpdateCommand's pre-update snapshots live
        // in. Bounds disk even for orphans the per-slug prune in
        // SnapshotManager::capture() cannot reach (a since-uninstalled/
        // renamed slug, a core meta-only snapshot, a crashed capture).
        // Scheduled daily by SnapshotManager::scheduleGc() (activate() below
        // + maybeRescheduleCron()'s self-heal).
        add_action(SnapshotManager::HOOK_GC, [SnapshotManager::class, 'gcExpired']);

        // S4 (issue #131 adversarial review) — recurring reconcile sweep for
        // update-in-flight markers left stale by a PHP-FPM
        // request_terminate_timeout hard-kill (the one case
        // UpdateGuard's register_shutdown_function() backstop cannot catch).
        // Scheduled hourly by UpdateInFlight::scheduleGc() (activate() below
        // + maybeRescheduleCron()'s self-heal); see UpdateInFlight's class
        // doc for the full design.
        add_action(UpdateInFlight::HOOK_GC, [UpdateInFlight::class, 'gcSweep']);

        // GH #151 — recurring GC backstop for the backup pipeline's
        // wpmgr-agent/runs/ scratch directory. TaskRunner's success path
        // cleans this up on `completed`, but the failure path deletes the
        // task row WITHOUT cleaning scratch — see BackupJanitor's class doc
        // for the full leak + age-gate/task-row-veto design. Scheduled daily
        // by BackupJanitor::scheduleGc() (activate() below + maybeRescheduleCron()'s
        // self-heal).
        add_action(BackupJanitor::HOOK_GC, [BackupJanitor::class, 'gcRuns']);

        // Media Optimizer — WP attachment-deletion cleanup. When an attachment
        // is deleted (wp-admin, programmatic, WP-CLI, or REST), WordPress purges
        // ONLY the files it tracks in _wp_attachment_metadata; WPMgr's own
        // untracked originals (the *.wpmgr-original.<ext> archive in REPLACE
        // mode, the original-ext twin in COEXIST mode) would otherwise be left
        // orphaned on disk and the CP would never learn the asset is gone. NOT
        // gated behind is_admin so programmatic/WP-CLI/REST deletes fire too.
        // EARLY priority (5) so we run before other plugins/core touch uploads;
        // the handler reads the blob BEFORE WP purges postmeta (delete_attachment
        // fires first), unlinks only our untracked paths (uploads-confined), and
        // best-effort notifies the CP. It never blocks or fails the WP delete.
        add_action('delete_attachment', [$this, 'onDeleteAttachment'], 5, 1);

        // ADR-044 — Auto-optimize on upload.
        //
        // FILTER (not action): wp_generate_attachment_metadata fires after core
        // has generated every registered sub-size. Priority 9999 so any resize/
        // regenerate plugins finish first. THREE args: ($metadata, $attachment_id,
        // $context) — the third arg distinguishes 'create' from 'update' and is
        // non-optional here. The callback ALWAYS returns $metadata unchanged.
        //
        // DRAIN: wpmgr_autoopt_drain is the arg-less scheduled hook; WP's
        // wp_schedule_single_event deduplication collapses repeated schedules from
        // bulk uploads into one pending event (DEBOUNCE ≈ 25s).
        add_filter('wp_generate_attachment_metadata', [$this->autoOptimizeUpload, 'onGenerateMetadata'], 9999, 3);
        add_action(AutoOptimizeUpload::HOOK_DRAIN, [$this, 'drainAutoOptimize']);

        $this->scheduler->registerHooks();

        // Phase 3 — page-cache request hooks. registerHooks() reads the cache
        // config once: it always arms the login role-cookie + the preload cron +
        // the refresh-cron schedule, and ONLY opens the output-buffer writer +
        // auto-purge hooks when caching is actually enabled (an inert site pays
        // just a single option read). Self-skips on preload warming requests.
        $this->cacheManager->registerHooks();

        // Task #171 — preload-queue watchdog. Bind the cron handler (re-kicks any
        // queue whose loopback runner chain died) and ensure the 60s recurring
        // event is scheduled (reuses the agent's existing wpmgr_60sec interval).
        // A real method (not a closure) keeps the hook table serialization-safe.
        add_action(PreloadQueue::WATCHDOG_HOOK, [$this, 'runPreloadWatchdog']);
        add_action('init', [$this, 'maybeSchedulePreloadWatchdog']);

        // Phase 4 — bloat-removal hooks. Unlike the rest of the optimizer (which
        // runs inside the cache writer's ob_start on a MISS), de-bloat must
        // UN-register core actions/filters at the right phase so the unwanted
        // markup is never emitted. We bind on `init`; Bloat::register() reads the
        // perf config once and no-ops entirely when no toggle is enabled, so an
        // inert site pays just a single option read. A real method (not a
        // closure) keeps the hook table serialization-safe.
        add_action('init', [$this, 'registerBloatHooks'], 0);

        // RUM (Real User Monitoring) beacon injection (GH #154 fix). Like
        // de-bloat, this must be registered INDEPENDENTLY of the cache-writer's
        // ob_start pipeline — the collector was previously injected only inside
        // that buffer, so a site with WPMgr page caching OFF (the norm when a
        // third-party page cache serves the site) or served from a third-party
        // cache HIT never received the collector and silently reported zero
        // data. wp_enqueue_scripts fires on every WordPress-rendered response
        // regardless of any cache path, so binding there is the cache-independent
        // fix (and, per the 2026-07 wp.org review fix, lets the collector be
        // enqueued via wp_enqueue_script() instead of echoed). Bound on `init`;
        // registerRumHooks() reads the perf config once and only adds the
        // wp_enqueue_scripts hook when rumEnabled is on, so an inert site pays
        // just a single option read. A real method (not a closure) keeps the
        // hook table serialization-safe.
        add_action('init', [$this, 'registerRumHooks'], 0);

        // DB-classify source-scan cache busting. When a plugin is activated or
        // deactivated the plugin-to-table-name source-scan map (stored in the
        // wpmgr_db_table_plugin_map transient) may be stale. Delete the transient
        // so the next db_scan rebuilds it fresh. Static method reference keeps
        // the hook table serialization-safe (no Closure holding $this).
        add_action('activated_plugin', [DbCleanup::class, 'bustPluginTableMapCache']);
        add_action('deactivated_plugin', [DbCleanup::class, 'bustPluginTableMapCache']);

        // Woo cart-fragments probe reset. Theme and plugin changes can alter
        // whether the active theme exposes wc-cart-fragments. Delete the stored
        // probe state so the next front-end render re-probes from scratch.
        // Static method reference keeps the hook table serialization-safe.
        add_action('switch_theme', [\WPMgr\Agent\Cache\WooFragmentsProbe::class, 'resetState']);
        add_action('activated_plugin', [\WPMgr\Agent\Cache\WooFragmentsProbe::class, 'resetState']);
        add_action('deactivated_plugin', [\WPMgr\Agent\Cache\WooFragmentsProbe::class, 'resetState']);

        // ADR-037 Sprint 2 — diagnostics cron handler. Scheduler::scheduleEvents
        // sets up the cron event; the handler runs the on-demand DiagnosticsCommand
        // and pushes its result to the CP at /agent/v1/diagnostics. Kept here
        // rather than inside Scheduler so Sprint 1's lock on class-scheduler.php
        // (append-only) is respected.
        add_action(Scheduler::HOOK_DIAGNOSTICS, [$this, 'runDiagnostics']);

        // Reliable-diagnostics — dedicated size-refresh cron handler. Runs the
        // SizeProbe walk under a bounded LongRunningJob::TIME_LIMIT_SECONDS cap
        // so recurse_dirsize/du has a generous but non-infinite ceiling, well
        // past the push request's normal max_execution_time. Plugin owns the
        // binding; Scheduler owns the schedule (additive).
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

        // Performance Suite — heartbeat backstop for cache stats + install-state.
        // Priority 35 so it runs after errors (30). Fire-and-forget: a failed
        // report must never interfere with the heartbeat itself.
        add_action(Scheduler::HOOK_HEARTBEAT, [$this, 'shipPerfReport'], 35);

        // ADR-037 Sprint 2 — install the error monitor. install() (and the
        // record() it leads to) is itself gated on the operator's explicit
        // opt-in (enabled=true in the stored config), so a fresh plugin
        // activation installs no handler and writes zero rows without
        // consent, on top of the mu-plugin FILE write being gated the same
        // way. On opt-out or when the flag has never been set, remove a
        // stale mu-plugin copy.
        $this->errorMonitor->install();
        if ($this->errorMonitor->isEnabled()) {
            $this->muInstaller->install();
        } elseif ($this->muInstaller->isInstalled()) {
            $this->muInstaller->uninstall();
        }

        // P4a — page-cache drop-in self-heal on version mismatch. When the
        // installed advanced-cache.php carries an older WPMGR_PAGE_CACHE_DROPIN_VERSION
        // than the template, silently reinstall it with the current config so
        // existing sites get the new cron-kick logic without requiring a manual
        // cache enable/disable cycle. Idempotent: needsRefresh() short-circuits
        // to false when the installed version already matches the template.
        $this->cacheManager->maybeRefreshDropin();

        // S2 — install login-protection hooks when mode != disabled. The call
        // is idempotent (static guard inside LoginProtection::install). We also
        // install the WAF mu-plugin so the early IP-deny gate is armed on the
        // next request even before WordPress fully boots.
        $this->loginProtection->install();
        // Only arm the early IP-deny WAF mu-plugin when protection is actually
        // enabled. An inert (unconfigured) site installs no security mu-plugin,
        // so a fresh plugin update cannot affect the request path at all.
        // When protection is disabled (opt-out), remove a previously-written WAF
        // file so no executable mu-plugin lingers after an operator disables it.
        if ($this->loginProtection->isEnabled()) {
            $this->muInstaller->installWaf();
        } elseif ($this->muInstaller->isWafInstalled()) {
            $this->muInstaller->uninstallWaf();
        }

        // GitHub issue #210 — install the update-watchdog mu-plugin whenever
        // the site is enrolled (the only state in which UpdateCommand can
        // ever run at all, and therefore the only state in which its marker
        // could ever be armed). The mu-plugin itself is inert on every
        // request until a marker actually exists (a single is_file() stat —
        // see its own class doc), so no separate feature opt-in is needed
        // beyond enrollment. Remove a previously-installed copy on
        // un-enrollment so no executable mu-plugin lingers for a site the
        // control plane no longer manages.
        if ($this->settings->isEnrolled()) {
            $this->muInstaller->installUpdateWatchdog();
        } elseif ($this->muInstaller->isUpdateWatchdogInstalled()) {
            $this->muInstaller->uninstallUpdateWatchdog();
        }

        // Login Whitelabel — bind login_head/login_headerurl/login_message hooks
        // only when at least one brand field is non-empty (self-gating). The
        // call is idempotent (static guard inside LoginBrand::install).
        $this->loginBrand->install();

        // Security hardening — bind WP hooks for each enabled toggle (xmlrpc,
        // REST restrict, login-identifier, author enum, force-ssl, ban filters).
        // Only registers hooks for toggles that are actually on; an unconfigured
        // site pays just a single get_option() read. Idempotent (static guard).
        $this->hardeningModule->install();

        // Security Suite Phase 3 — site-user 2FA interstitial + password policy
        // + hide-backend. All three are inert when the stored policy is absent or
        // has the master switch off. Idempotent (static guard inside each install()).
        // LOCKOUT-PROOF: if define('WPMGR_DISABLE_SITE_2FA', true) is in wp-config,
        // neither 2FA nor password policy enforcement fires. The autologin path
        // bypasses the 2FA interstitial by construction (it never fires wp_login).
        $this->site2faModule->install();
        $this->passwordPolicyModule->install();
        $this->hideBackendModule->install();

        // ADR-042 Phase 2 — bind the CP self-update hooks. Self-gates on
        // isEnrolled() inside UpdateChecker::install(); idempotent (static guard).
        // Skipped for the wp.org distribution build (WPMGR_WPORG_BUILD constant).
        if ($this->updateChecker !== null) {
            $this->updateChecker->install();
        }

        // Email (Phase 2) — register the pre_wp_mail interception hook.
        // The filter is non-destructive: when no email config is stored it
        // returns null immediately, leaving the default WP mail path untouched.
        $this->mailRouter->register_hooks();

        // Email log retention pruner — runs daily via wp-cron.
        add_action(EmailLogger::HOOK_PRUNE, [$this, 'pruneEmailLog']);

        // Phase 3b — email-log CP push: runs every 5 min (HOOK_PUSH) AND as a
        // heartbeat backstop (priority 40, after perf report at 35) so rows
        // drain even if the dedicated cron event was lost. Fire-and-forget.
        add_action(EmailLogReporter::HOOK_PUSH, [$this, 'pushEmailLog']);
        add_action(Scheduler::HOOK_HEARTBEAT, [$this, 'pushEmailLog'], 40);

        // Phase 4b — suppression-cache pull: runs every 15 min (HOOK_PULL) so
        // the local hash store stays current without a per-send CP dependency.
        add_action(SuppressionCache::HOOK_PULL, [$this, 'pullSuppressionCache']);

        // Connection-liveness catch-up: send a heartbeat on shutdown when the
        // last recorded heartbeat is overdue by more than 120 s. Fixes false
        // disconnects on page-cached idle sites where WP-Cron never fires.
        $this->heartbeatCatchup->register();

        // Admin-bar cache purge controls — register on EVERY request, not just
        // admin ones, so the "Purge this page" node can appear on the front-end
        // admin bar (where is_admin() is false). Every bound hook self-gates:
        // addBarNodes checks current_user_can('manage_options'); the
        // admin_post_*, admin_notices, and plugin-row hooks only fire in their
        // own admin contexts.
        $this->adminBarPurge->registerHooks();

        if (function_exists('is_admin') && is_admin()) {
            $this->admin->registerHooks();

            // Per-page cache/optimization controls — "WPMgr Cache" side meta box
            // on all public post types. Admin-only; zero front-end cost.
            (new \WPMgr\Agent\Cache\PageCacheControlMetaBox())->registerHooks();

            // Media Optimizer (Phase 4) — surface per-attachment optimization
            // stats in the Media Library modal + the attachment edit meta box.
            // Read-only, admin-only; the injected HTML is escaped by
            // StatsRenderer (XSS-safe). The htaccess Accept-fallback block is
            // installed lazily on the first different-ext apply (MediaApplyCommand),
            // NOT here — an inert site touches no server config.
            (new MediaModalInjector())->registerHooks();
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

        // ADR-037 Sprint 2 — the error-trap mu-plugin FILE write is gated on the
        // operator's explicit opt-in (enabled=true in sync_error_config), so a
        // fresh activation deliberately writes ZERO mu-plugin files. The boot path
        // (registerHooks) handles the conditional install/uninstall on every load.

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

        // Daily email-log retention pruner.
        EmailLogger::schedule_prune($now);

        // Phase 3b — email-log CP push: 5-minute recurring event.
        EmailLogReporter::schedule_push($now);

        // Phase 4b — suppression-cache pull: 15-minute recurring event.
        SuppressionCache::schedule_pull($now);

        // M1 (issue #131 adversarial review) — daily snapshot-store GC
        // backstop (bounded disk for wpmgr-snapshots/).
        SnapshotManager::scheduleGc($now);

        // S4 (issue #131 adversarial review) — hourly update-in-flight
        // reconcile sweep (recovers a PHP-FPM hard-killed apply that never
        // reached UpdateGuard's own shutdown-function backstop).
        UpdateInFlight::scheduleGc($now);

        // GH #151 — daily GC backstop for the backup wpmgr-agent/runs/
        // scratch directory (reclaims scratch a failed backup run leaks).
        BackupJanitor::scheduleGc($now);

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
            update_option(self::OPTION_KEYSTORE_ERROR, $this->keystoreErrorMessage($e), false);

            return false;
        }
    }

    /**
     * Build a cause-specific, plain-text admin message for a keystore setup
     * failure. Distinguishes three situations rather than showing one fixed
     * opaque message for every failure (GH #257):
     *
     *   (a) A required crypto PHP extension (sodium or openssl) is missing.
     *   (b) A previously-pinned master-key source has become unavailable (an
     *       operator removed WPMGR_AGENT_KEY_FILE, edited/removed wp-config
     *       salts, or deleted a key file/option) — the underlying
     *       RuntimeException message from the pinned-source honour block in
     *       Keystore::resolveMasterKey() already names the specific cause, so
     *       it is surfaced directly instead of a generic message.
     *   (c) First-time establishment failure: every tier (constant, salts,
     *       every file candidate, AND the database fallback) was unusable.
     *       Only reachable when WPMGR_AGENT_DISABLE_DB_KEY is defined (the
     *       operator opted out of the last-resort tier) or the database
     *       write itself failed.
     *
     * The return value is plain text (no markup); renderKeystoreNotice()
     * escapes it with esc_html() at the point of output.
     *
     * @param \Throwable $e The exception caught while setting up the keystore.
     * @return string Human-readable failure message.
     */
    private function keystoreErrorMessage(\Throwable $e): string
    {
        $suffix = ' The plugin is active but inactive until this is resolved.';

        // (a) Missing crypto extension. Checked independently of $e's content
        // (a missing extension typically surfaces as an "undefined function"
        // \Error rather than a message that names the extension).
        $missingExtensions = [];
        if (!extension_loaded('sodium')) {
            $missingExtensions[] = 'sodium';
        }
        if (!extension_loaded('openssl')) {
            $missingExtensions[] = 'openssl';
        }
        if ($missingExtensions !== []) {
            $plural = count($missingExtensions) > 1;
            return 'WPMgr Agent requires the PHP ' . implode(' and ', $missingExtensions)
                . ' extension' . ($plural ? 's' : '') . ', which ' . ($plural ? 'are' : 'is')
                . ' not available on this server. Ask your host to enable '
                . ($plural ? 'them' : 'it') . '.' . $suffix;
        }

        $message = $e->getMessage();

        // (b) Pinned-source drift: every honour-block RuntimeException in
        // Keystore::resolveMasterKey() names the source in its message and
        // contains the word "pinned".
        if (strpos($message, 'pinned') !== false) {
            return 'WPMgr Agent could not re-establish its encryption key: ' . $message . $suffix;
        }

        // (c) First-time establishment failure (every tier, including the
        // database fallback, was unusable).
        return 'WPMgr Agent could not establish its encryption key. Define WPMGR_AGENT_KEY_FILE '
            . 'in wp-config.php pointing to a writable path, ensure your wp-config.php secret salts '
            . '(AUTH_KEY, ...) are set, or (if WPMGR_AGENT_DISABLE_DB_KEY is defined) remove that '
            . 'constant so the database-stored fallback key can be used.' . $suffix;
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
            wp_clear_scheduled_hook(EmailLogger::HOOK_PRUNE);
            wp_clear_scheduled_hook(EmailLogReporter::HOOK_PUSH);
            wp_clear_scheduled_hook(SuppressionCache::HOOK_PULL);
            // M1 / S4 (issue #131 adversarial review) — snapshot-store GC
            // backstop + update-in-flight reconcile sweep.
            wp_clear_scheduled_hook(SnapshotManager::HOOK_GC);
            wp_clear_scheduled_hook(UpdateInFlight::HOOK_GC);
            // GH #151 — backup runs/ scratch-dir GC backstop.
            wp_clear_scheduled_hook(BackupJanitor::HOOK_GC);
        }

        // Phase 3 — page-cache teardown. Cleanly reverse every server-side
        // artefact (.htaccess block, advanced-cache.php drop-in, the WP_CACHE
        // define) and purge the disk cache, so a deactivated plugin never leaves
        // an orphaned drop-in trying to serve from an empty cache. disable() is
        // idempotent and best-effort; a failure here must not block deactivation.
        try {
            $this->cacheManager->disable();
        } catch (\Throwable $e) {
            // Swallow — deactivation must always complete.
        }
        if (function_exists('wp_clear_scheduled_hook')) {
            wp_clear_scheduled_hook(\WPMgr\Agent\Cache\Preload::HOOK);
            wp_clear_scheduled_hook(\WPMgr\Agent\Cache\CacheRefreshCron::HOOK);
            // Task #171 — clear the preload-queue watchdog cron.
            wp_clear_scheduled_hook(PreloadQueue::WATCHDOG_HOOK);
        }

        // Best-effort removal of all THREE mu-plugins on deactivation so no
        // executable PHP file installed by this plugin lingers in
        // wp-content/mu-plugins/ after the plugin is deactivated. All calls
        // are idempotent and best-effort; a failure must never block
        // deactivation.
        try {
            $this->muInstaller->uninstallWaf();
            $this->muInstaller->uninstallUpdateWatchdog();
            $this->muInstaller->uninstall();
        } catch (\Throwable $e) {
            // Swallow — deactivation must always complete.
        }
    }

    /**
     * Register the GET /wpmgr/v1/autologin route. SEPARATE from the dispatch
     * router: this route is browser-initiated and the Ed25519 JWT is the
     * authorization.
     *
     * permission_callback performs a READ-ONLY signature + claims check
     * (Connector::validateCommandSignature) — it verifies the Ed25519 signature,
     * algorithm, temporal bounds, aud, and cmd WITHOUT touching the jti anti-replay
     * table. This satisfies the wp.org requirement that permission_callback does
     * real authorization, while preserving single-use semantics: the authoritative
     * full verify (including jti recording) runs inside handle() as before.
     *
     * @return void
     */
    public function registerAutologinRoute(): void
    {
        if (!function_exists('register_rest_route')) {
            return;
        }

        $connector = $this->connector;

        register_rest_route(
            Router::NAMESPACE,
            '/autologin',
            [
                'methods'             => 'GET',
                'callback'            => [$this->autologin, 'handle'],
                'permission_callback' => static function (\WP_REST_Request $request) use ($connector): bool {
                    $token = $request->get_param('token');
                    $token = is_string($token) ? $token : '';
                    if ($token === '') {
                        return false;
                    }
                    return $connector->validateCommandSignature($token, 'autologin');
                },
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
     * Task #171 — register the POST /wpmgr/v1/preload/run loopback runner route.
     *
     * SEPARATE from the signed dispatch router (Router::registerRoutes): this is a
     * fire-and-forget LOOPBACK kick from the agent to itself and carries no command
     * authority — it only drains an already-queued, SSRF-filtered, same-host URL
     * set. Authentication is a self-HMAC handshake (NOT Ed25519 Connector signing):
     * the body's `token` is verified against hash_hmac over wp_salt('auth').
     *
     * permission_callback performs the same HMAC verification via
     * PreloadQueue::verifyRunnerToken() — a pure hash_equals with no side effects.
     * The handler (runFromRest) re-verifies and is the authoritative gate; the
     * permission_callback is the early-rejection layer satisfying wp.org requirements.
     * WP nonces/auth cookies are unavailable on a non-blocking loopback POST.
     * See the §1.10 security-review checklist.
     *
     * @return void
     */
    public function registerPreloadRunRoute(): void
    {
        if (!function_exists('register_rest_route')) {
            return;
        }

        $queue = PreloadQueue::fromConfig();

        register_rest_route(
            PreloadQueue::REST_NAMESPACE,
            PreloadQueue::REST_RUN_ROUTE,
            [
                'methods'             => 'POST',
                'callback'            => [$queue, 'runFromRest'],
                'permission_callback' => static function (\WP_REST_Request $request) use ($queue): bool {
                    $group    = (string) $request->get_param('group');
                    $callback = (string) $request->get_param('callback');
                    $token    = (string) $request->get_param('token');
                    // sanitize_text_field applied by args before the callback runs,
                    // but apply here too for defence-in-depth in the early gate.
                    if (function_exists('sanitize_text_field')) {
                        $group    = sanitize_text_field($group);
                        $callback = sanitize_text_field($callback);
                        $token    = sanitize_text_field($token);
                    }
                    return $queue->verifyRunnerToken($group, $callback, $token);
                },
                'args'                => [
                    'group' => [
                        'required'          => true,
                        'type'              => 'string',
                        'sanitize_callback' => 'sanitize_text_field',
                    ],
                    'callback' => [
                        'required'          => true,
                        'type'              => 'string',
                        'sanitize_callback' => 'sanitize_text_field',
                    ],
                    'token' => [
                        'required'          => true,
                        'type'              => 'string',
                        'sanitize_callback' => 'sanitize_text_field',
                    ],
                ],
            ]
        );
    }

    /**
     * Task #171 — cron handler for the preload-queue watchdog
     * (PreloadQueue::WATCHDOG_HOOK). Re-kicks any queue whose loopback runner chain
     * died (the non-blocking POST was dropped). A real public method (not a closure)
     * keeps the WP hook table serialization-safe.
     *
     * @return void
     */
    public function runPreloadWatchdog(): void
    {
        try {
            PreloadQueue::fromConfig()->runWatchdog();
        } catch (\Throwable $e) {
            // Best-effort: a watchdog failure must never fatal a cron tick.
        }
    }

    /**
     * Task #171 — ensure the 60-second preload-queue watchdog event is scheduled.
     * Bound to `init`; reuses the agent's existing wpmgr_60sec cron interval
     * (registered by Scheduler::addSchedules). Reschedule-if-missing pattern,
     * mirroring maybeRescheduleCron. Cheap on the hot path (one wp_next_scheduled
     * read of the already-loaded cron option).
     *
     * @return void
     */
    public function maybeSchedulePreloadWatchdog(): void
    {
        if (!function_exists('wp_next_scheduled') || !function_exists('wp_schedule_event')) {
            return;
        }
        if (wp_next_scheduled(PreloadQueue::WATCHDOG_HOOK) !== false) {
            return;
        }
        wp_schedule_event(time() + 60, Scheduler::SCHEDULE_60SEC, PreloadQueue::WATCHDOG_HOOK);
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
     * GitHub issue #210, MEDIUM-1b — clear the update-watchdog marker entry
     * for any armed slug that just proved itself healthy on this exact
     * request. See UpdateWatchdogMarker::disarmHealthy()'s own doc for the
     * full reasoning; this method is only the plugins_loaded binding point.
     *
     * @return void
     */
    public function maybeDisarmUpdateWatchdog(): void
    {
        UpdateWatchdogMarker::disarmHealthy();
    }

    /**
     * GitHub issue #226 — WP-Cron-independent GC trigger for the
     * wpmgr-snapshots/ store. See registerHooks()'s binding comment for the
     * root cause this closes; SnapshotManager::maybeGc() itself throttles the
     * actual sweep to once per hour via a stored option and never lets a GC
     * failure propagate, so this is cheap and safe to call on every request.
     *
     * Gated on isEnrolled(), mirroring maybeRescheduleCron() /
     * maybeDisarmUpdateWatchdog(): a not-yet-enrolled site has never had an
     * update (and therefore never a snapshot) to reclaim, and skipping keeps
     * this from perturbing the one-shot auto-deactivate safety window that
     * only matters while not enrolled.
     *
     * @return void
     */
    public function maybeGcSnapshots(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }

        SnapshotManager::maybeGc();
    }

    /**
     * GitHub issue #232 — WP-Cron-independent reaper trigger for stalled
     * `wpmgr_backup_tasks` rows. See registerHooks()'s binding comment (next
     * to maybeGcSnapshots(), immediately above) for the root cause this
     * closes.
     *
     * Mirrors maybeGcSnapshots() / SnapshotManager::maybeGc() exactly:
     *   - Gated on isEnrolled(): a not-yet-enrolled site has never had a
     *     backup scheduled (and therefore never a stalled row) to reap.
     *   - Throttled to once per BACKUP_SWEEP_THROTTLE_SECONDS (60s) via a
     *     stored option, stamped BEFORE the sweep runs — a slow or failing
     *     sweep can never be retried on every single request within the
     *     window; the next throttled attempt gets a fresh try regardless of
     *     whether this one succeeded.
     *   - Watchdog::sweepStalled() is wrapped in try/catch: a reaper sweep
     *     is best-effort recovery housekeeping, never a condition that may
     *     break the request (or hook) that happened to trigger it.
     *
     * @return void
     */
    public function maybeSweepStalledBackups(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $now  = time();
        $last = (int) get_option(self::OPTION_BACKUP_SWEEP_LAST, 0);
        if ($now - $last < self::BACKUP_SWEEP_THROTTLE_SECONDS) {
            return;
        }

        // Stamp BEFORE running — see this method's doc for why.
        update_option(self::OPTION_BACKUP_SWEEP_LAST, $now, true);

        try {
            Watchdog::sweepStalled();
        } catch (\Throwable $e) {
            // A reaper sweep failure must never break the caller (a request
            // handler bound to plugins_loaded); the next throttled attempt
            // 60s from now gets a fresh try.
        }
    }

    /**
     * GitHub issue #256: WP-Cron-independent GC trigger for
     * BackupJanitor::gcRuns() (the backup pipeline's wpmgr-agent/runs/
     * scratch-dir GC backstop). See registerHooks()'s binding comment for
     * the root cause this closes.
     *
     * Mirrors maybeGcSnapshots() / maybeSweepStalledBackups() exactly:
     *   - Gated on isEnrolled(): a not-yet-enrolled site has never had a
     *     backup run (and therefore never a leaked run directory) to reap.
     *   - Throttled to once per BACKUP_JANITOR_THROTTLE_SECONDS (1h) via a
     *     stored option, stamped BEFORE the sweep runs: a slow or failing
     *     sweep can never be retried on every single request within the
     *     window; the next throttled attempt gets a fresh try regardless of
     *     whether this one succeeded.
     *   - BackupJanitor::gcRuns() is wrapped in try/catch: a GC sweep is
     *     best-effort disk-hygiene housekeeping, never a condition that may
     *     break the request (or hook) that happened to trigger it.
     *
     * SAFETY: gcRuns() itself already enforces the two guards that make this
     * safe to call opportunistically (the RUNS_GC_AGE_SECONDS (6h) mtime
     * gate and the isActiveRun() task-row veto, see BackupJanitor's own
     * design notes); this method adds NEITHER new deletion logic NOR a
     * weaker gate. It only adds a cron-independent trigger for the exact
     * same, already-guarded sweep the daily cron already ran.
     *
     * @return void
     */
    public function maybeGcBackupRuns(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $now  = time();
        $last = (int) get_option(self::OPTION_BACKUP_JANITOR_LAST, 0);
        if ($now - $last < self::BACKUP_JANITOR_THROTTLE_SECONDS) {
            return;
        }

        // Stamp BEFORE running, see this method's doc for why.
        update_option(self::OPTION_BACKUP_JANITOR_LAST, $now, true);

        try {
            BackupJanitor::gcRuns();
        } catch (\Throwable $e) {
            // A GC sweep failure must never break the caller (a request
            // handler bound to plugins_loaded); the next throttled attempt
            // gets a fresh try.
        }
    }

    /**
     * GitHub issue #256: WP-Cron-independent GC trigger for the restore
     * pipeline's deferred rollback material: FilesRestorer::gcOldFiles()
     * (`.wpmgr-old-*` / `.wpmgr-staging-*` trees) and
     * RestoreRunner::gcPreRestoreDumps() (the pre-restore DB dump). See
     * registerHooks()'s binding comment for the root cause this closes.
     *
     * Mirrors maybeGcBackupRuns() / maybeGcSnapshots() / maybeSweepStalledBackups():
     *   - Gated on isEnrolled(): a not-yet-enrolled site has never run a
     *     restore (and therefore never has rollback material to reap).
     *   - Throttled to once per RESTORE_GC_THROTTLE_SECONDS (15 min) via a
     *     stored option, stamped BEFORE the sweep runs.
     *   - Each GC call is independently wrapped in try/catch so a failure in
     *     one (e.g. gcOldFiles()) can never suppress the other
     *     (gcPreRestoreDumps()), and neither can ever break the caller.
     *
     * SAFETY (corrected, GH #256 post-ship review: the prior wording here
     * overstated this): RestoreRunner::gcPreRestoreDumps() IS gated
     * entirely by its own on-disk `<path>.expires` sidecar markers; it
     * skips any per-restore directory that has none at all. RestoreRunner
     * writes each marker with a future unix timestamp only once the live
     * swap has already completed and the restore has moved past the point
     * that material is needed for an in-flight rollback, and the method
     * itself refuses to touch anything whose marker has not yet expired.
     *
     * FilesRestorer::gcOldFiles() is marker-gated for the same `.expires`
     * paths, but it ALSO carries a separate legacy/crash-only fallback
     * (gcByMtimeLongThreshold(), FilesRestorer::OLDFILES_GC_AGE_SECONDS_LONG,
     * 24h) for `.wpmgr-old-*` directories written before the marker scheme
     * existed and for `.wpmgr-staging-*` directories left behind by a
     * restore that crashed before ever reaching runCleanup() (and therefore
     * never wrote a marker at all). That fallback is NOT marker-gated; its
     * only gate is the directory's own mtime (via `filemtime($dir)` on the
     * directory itself) being more than 24h stale, and the prior wording
     * here should not have described both GC targets as "gated entirely by
     * their own on-disk markers."
     *
     * SAFETY (corrected AGAIN, GH #256 post-ship re-review: the wording that
     * replaced the sentence above was itself still wrong): this fallback can
     * race a restore that is still actively writing into its own staging
     * directory. A directory's mtime advances only when an entry is added,
     * removed, or renamed within it, never when an existing file's
     * CONTENTS are written, and never when a change happens only in a
     * nested subdirectory several levels down. A restore stage that is deep
     * into copying large files (overwriting existing paths in place) or
     * working several directories below the staged root can leave the
     * staged root's own mtime unchanged well past the 24h threshold while
     * still genuinely in progress. This fallback is a real,
     * marker-independent deletion path with a gap the mtime check alone
     * does not close; it is documented here rather than silently trusted.
     * Calling both sweeps opportunistically from every request still adds
     * no new deletion condition beyond what the daily cron already runs; it
     * only adds a cron-independent trigger for the same conditions.
     *
     * @return void
     */
    public function maybeGcRestoreArtifacts(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $now  = time();
        $last = (int) get_option(self::OPTION_RESTORE_GC_LAST, 0);
        if ($now - $last < self::RESTORE_GC_THROTTLE_SECONDS) {
            return;
        }

        // Stamp BEFORE running, see this method's doc for why.
        update_option(self::OPTION_RESTORE_GC_LAST, $now, true);

        try {
            FilesRestorer::gcOldFiles();
        } catch (\Throwable $e) {
            // A GC sweep failure must never break the caller; the next
            // throttled attempt gets a fresh try. Independent of the
            // gcPreRestoreDumps() call below so one failing target can never
            // suppress the other.
        }
        try {
            RestoreRunner::gcPreRestoreDumps();
        } catch (\Throwable $e) {
            // See above.
        }
    }

    /**
     * Opcache-invalidate the object-cache engine and its sibling class files
     * when the stored plugin version differs from the current WPMGR_AGENT_VERSION.
     *
     * After a self-update WordPress swaps the plugin files, but the object-cache
     * drop-in (in wp-content) is not replaced and the engine files (inside the
     * plugin directory) have stale opcache entries. The next wp-cron or REST
     * request would execute the old bytecode despite the new files being on disk.
     * This runs once-per-version on plugins_loaded to force a recompile.
     *
     * Cheap on the hot path: one get_option() + one constant read + at most
     * three opcache_invalidate() calls, only on the version-change boot.
     *
     * @return void
     */
    public function maybeInvalidateEngineOpcache(): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $currentVersion = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '';
        if ($currentVersion === '') {
            return;
        }

        $storedVersion = (string) get_option('wpmgr_agent_engine_opcache_version', '');
        if ($storedVersion === $currentVersion) {
            // Already invalidated for this version — nothing to do.
            return;
        }

        // Version changed (or first boot): invalidate all object-cache opcache
        // entries — the generated artifact in assets/, the installed drop-in in
        // wp-content/, and the engine source files inside the plugin.
        $installer = new \WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller();
        $installer->invalidateEngineFiles();

        update_option('wpmgr_agent_engine_opcache_version', $currentVersion, false);
    }

    /**
     * Cron self-heal: re-arm the recurring reporting crons when they have gone
     * missing (the canonical case being an in-place plugin update — see the
     * plugins_loaded binding in registerHooks for the full failure mode).
     *
     * Gated on isEnrolled(): every recurring job no-ops before enrollment, and
     * skipping the not-enrolled case keeps us from perturbing the one-shot
     * auto-deactivate safety window (which only matters WHILE not enrolled).
     *
     * Cheap on the hot path: one in-memory wp_next_scheduled() read of the
     * already-loaded `cron` option. The heartbeat event is the canary — if it
     * is scheduled, the rest were installed in the same scheduleEvents() pass
     * and are present too, so we short-circuit. When it is missing we re-run
     * the idempotent scheduleEvents() (each event is guarded by its own
     * wp_next_scheduled check, so only the truly-absent ones get re-created)
     * and restore the hourly autologin-replay prune, which lives in activate()
     * rather than scheduleEvents() and is likewise wiped by clearEvents().
     *
     * @return void
     */
    public function maybeRescheduleCron(): void
    {
        if (!function_exists('wp_next_scheduled')) {
            return;
        }
        if (!$this->settings->isEnrolled()) {
            return;
        }
        // Canary: heartbeat present ⇒ the whole recurring set is present.
        if (wp_next_scheduled(Scheduler::HOOK_HEARTBEAT) !== false) {
            return;
        }

        $now = time();
        $this->scheduler->scheduleEvents($now);

        // Hourly autologin-replay prune is scheduled in activate(), not in
        // scheduleEvents(), so re-arm it here too (clearEvents()/deactivate()
        // wipes it alongside the rest).
        if (function_exists('wp_schedule_event')
            && wp_next_scheduled(ReplayCache::HOOK_PRUNE) === false
        ) {
            wp_schedule_event($now + 60, 'hourly', ReplayCache::HOOK_PRUNE);
        }

        // Email log retention pruner — re-arm when missing.
        EmailLogger::schedule_prune($now);

        // Phase 3b — email-log CP push — re-arm when missing.
        EmailLogReporter::schedule_push($now);

        // Phase 4b — suppression-cache pull — re-arm when missing.
        SuppressionCache::schedule_pull($now);

        // M1 (issue #131 adversarial review) — snapshot-store GC backstop —
        // re-arm when missing.
        SnapshotManager::scheduleGc($now);

        // S4 (issue #131 adversarial review) — update-in-flight reconcile
        // sweep — re-arm when missing.
        UpdateInFlight::scheduleGc($now);

        // GH #151 — backup runs/ scratch-dir GC backstop — re-arm when missing.
        BackupJanitor::scheduleGc($now);
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

        // Media Optimizer (Phase 4). The MediaUploader is the single signed
        // agent->CP + presigned-S3 transport seam (mirrors BackupTransport),
        // built from the same Signer every other agent->CP call uses. The six
        // commands map 1:1 to the CP contract:
        //   media_sync             -> /agent/v1/media/sync-batch
        //   media_optimize         -> /agent/v1/media/presign + /encode-ready
        //   media_apply            -> /agent/v1/media/job-status
        //   media_restore          -> /agent/v1/media/restore-status
        //   media_delete_originals -> /agent/v1/media/job-status
        //   media_stats            -> local read (no CP callback)
        $mediaUploader = new MediaUploader($this->signer);

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
            // ADR-044 — Auto-optimize config sync. The CP dispatches this on
            // settings save (PUT /api/v1/sites/{id}/media/settings). Payload field
            // names match media_config_contract.go: enabled, target_format,
            // target_quality. Persists to typed Settings accessors so the upload
            // filter can read the enable flag on the fast path.
            new SyncMediaConfigCommand($this->settings),
            // Media Optimizer (Phase 4) — the six CP->agent commands. Each shares
            // the MediaUploader transport; the apply/restore/delete commands build
            // their own AttachmentMeta/DbRewriter/Rename/DiskWriter seams (default
            // ctor args), so the REST entry point stays minimal.
            new MediaSyncCommand($mediaUploader),
            new MediaOptimizeCommand($mediaUploader),
            new MediaApplyCommand($mediaUploader, null, null, null, new HtaccessInstaller()),
            new MediaRestoreCommand($mediaUploader),
            new MediaDeleteOriginalsCommand($mediaUploader),
            new MediaStatsCommand(),
            // Phase 3 — page-caching engine. The six CP->agent commands all share
            // the single CacheManager orchestrator:
            //   cache_enable        -> WP_CACHE define + drop-in + .htaccess block
            //   cache_disable       -> reverse all three cleanly + purge
            //   cache_purge         -> all | per-URL
            //   cache_preload       -> queue background warm (desktop+mobile UA)
            //   perf_config_update  -> re-render drop-in config + .htaccess mobile flag
            //   db_clean            -> Phase 4 stub (signed surface wired now)
            new CacheEnableCommand($this->cacheManager, $this->cacheManager->makePerfReporter()),
            new CacheDisableCommand($this->cacheManager),
            new CachePurgeCommand($this->cacheManager),
            new CachePreloadCommand($this->cacheManager),
            new PerfConfigUpdateCommand($this->cacheManager, $this->cacheManager->makePerfReporter()),
            new DbCleanCommand(null, $this->keystore, $this->settings),
            // M39 — read-only database scan. Synchronous: full per-category
            // COUNT + reclaimable-bytes result is returned in the ACK body so
            // the operator sees the preview before committing to a db_clean.
            new DbScanCommand(),
            // Phase 2.2 — per-table actions (optimize, repair, drop, empty).
            // Synchronous; gated by orphan-only check (LAYER 1) + information_schema
            // exact-match validation (LAYER 2) for DROP/EMPTY. Optimize/repair are
            // always allowed (no data loss possible). Type-to-confirm and
            // PermSiteManage gates live in the CP handler layer.
            new DbTableActionCommand(),
            // P3.8 — destructive orphan delete. Deletes ONLY the CP-signed
            // allowlist (options / cron / tables). Agent live-re-verifies every
            // item against the LIVE installed-plugin set before acting. Async
            // (shutdown function), progress POSTs to progress_endpoint.
            new DbOrphanDeleteCommand(null, $this->keystore, $this->settings),
            // #188 — standalone serialization-safe search-replace tool.
            // Reuses the UrlRewriter engine from the restore pipeline.
            // Synchronous: full result returned in the ACK body.
            new SearchReplaceCommand(),
            // #189 — local database snapshot tool: create/list/revert/delete.
            // Snapshots are stored on the WP server filesystem (not encrypted/
            // uploaded), designed for "capture before a risky change, revert
            // in one click". Reuses DbDumper (same engine as full backups) for
            // the SQL dump; uses DbRestorer tmp-prefix+swap for the import.
            new DbSnapshotCommand(),
            // #190 — Unused Media Cleaner: scan, isolate (quarantine), restore,
            // delete. Builds an exhaustive conservative reference index before
            // flagging any attachment as unused, and uses a reversible quarantine
            // directory (wp-content/wpmgr-quarantine/media) before any permanent
            // deletion. The command is synchronous; scan results are paginated.
            new MediaCleanCommand(),
            new RucssComputeCommand($this->cacheManager),
            // Task #171 — signed preload-queue status + maintenance commands for
            // the React viewer. These go through the SIGNED wpmgr/v1/command/{cmd}
            // dispatcher (NOT the unsigned loopback /preload/run route):
            //   cache_preload_queue_status       -> per-status tallies + a page of rows
            //   cache_preload_queue_retry_failed -> revive failed -> pending
            //   cache_preload_queue_clear        -> clearQueue()
            //   cache_preload_queue_test_rest    -> loopback-reachability self-test
            new CachePreloadQueueStatusCommand($this->cacheManager),
            new CachePreloadQueueRetryFailedCommand($this->cacheManager),
            new CachePreloadQueueClearCommand($this->cacheManager),
            new CachePreloadQueueTestRestCommand($this->cacheManager),
            // Security hardening sync. The CP pushes the full config + ban list;
            // the agent persists it atomically, applies the DISALLOW_FILE_EDIT
            // define to wp-config, refreshes the .htaccess security block, and
            // merges IP/range bans into the WAF mu-plugin's deny_cidrs.
            new SyncSecurityHardeningCommand($this->hardeningModule),
            // Security Suite Phase 3 — site-user auth policy sync. The CP pushes
            // the full policy snapshot (2FA config + groups + force-list); the
            // agent persists it atomically and returns an enrollment summary so
            // the dashboard can show per-role 2FA coverage. Default-OFF.
            new SyncSecurityPolicyCommand(),
            // Phase 2 file integrity — CP pushes the list of ABSPATH-relative
            // paths it manages (object-cache.php, advanced-cache.php, .htaccess,
            // mu-plugin loaders, wp-config region, etc.); agent responds with
            // md5_file() for each readable, ABSPATH-contained path so the CP
            // can upsert site_managed_files and suppress false positives from
            // the file-integrity diff (file_changed/file_added on managed files).
            new RecordManagedFilesCommand(),
            // Email (Phase 2) — per-site outgoing-mail configuration + test.
            // sync_email_config: receives the full provider config (including the
            //   DECRYPTED secret) from the CP and stores it in the agent keystore.
            // send_test_email:   sends a test message via the current provider
            //   config with the fallback DISABLED so real provider errors surface.
            new SyncEmailConfigCommand($this->keystore),
            new SendTestEmailCommand($this->providerRouter),
            // Phase 4b — re-sends a buffered email by its local agent_seq when
            // the body was stored (body_stored=1). Returns body_not_stored when
            // the body was not captured so the CP/UI can surface a clear reason.
            new ResendEmailCommand($this->providerRouter),
            // Object Cache Phase 2 — five CP->agent commands for the Redis
            // persistent object cache. All commands ride the existing signed
            // Ed25519 channel:
            //   objectcache.apply_config -> persist 0600 config file
            //   objectcache.test         -> probe candidate config (no persist)
            //   objectcache.enable       -> install object-cache.php drop-in
            //   objectcache.disable      -> remove drop-in + optional flush
            //   objectcache.flush        -> FLUSHDB or SCAN+MATCH+UNLINK
            new ObjectcacheApplyConfigCommand(),
            new ObjectcacheTestCommand(),
            new ObjectcacheEnableCommand(),
            new ObjectcacheDisableCommand(),
            new ObjectcacheFlushCommand(),
            // Connection-liveness hardening (0.44.0): cheap CP active-verify dial.
            // Answers ok/agent_version/php_time/wp_cron_disabled/heartbeat_overdue_sec
            // and kicks spawn_cron() so every verify dial drains overdue cron events
            // on page-cached idle sites.
            new PingCommand(),
            // P1 read-only file manager (v1). Jail root = WPMGR_FILE_JAIL_ROOT
            // constant (defaults to ABSPATH). Every path goes through the
            // FileScanner realpath+strncmp containment guard. Sensitive-file
            // deny-list (T6) applies to file_read and file_download_prepare.
            //   file_list             -> one-level directory listing
            //   file_read             -> base64 preview (≤ 256 KiB)
            //   file_download_prepare -> stream file to CP-minted presigned S3 PUTs
            new FileListCommand(),
            new FileReadCommand(),
            new FileDownloadPrepareCommand(),
            // P2 guarded write / upload (v1.1). All paths go through jailPath();
            // all writes enforce the T3 base-unresolved guard (throw before write).
            // The executable-write prevention (T1, the core RCE control) covers:
            //   extension deny-list + double-extension + content sniff + web-dir
            //   — confirmed by the CP with confirm_executable_write=true (owner only).
            //   file_write          -> atomic temp→rename write of ≤ 256 KiB text
            //   file_mkdir          -> hardened directory creation
            //   file_rename         -> atomic rename with guards on BOTH src+dst
            //   file_delete         -> protected-root guard (T13) + recursive flag
            //   file_chmod          -> safe-mode allowlist (no setuid/world-write)
            //   file_upload_apply   -> stream-reassemble presigned-GET chunks + sniff
            new FileWriteCommand(),
            new FileMkdirCommand(),
            new FileRenameCommand(),
            new FileDeleteCommand(),
            new FileChmodCommand(),
            new FileUploadApplyCommand(),
            // P3 advanced file operations (v1.2).
            //   file_archive_create  -> zip one or more jailed paths and stage to S3
            //   file_extract         -> extract a jailed .zip into a jailed destination
            //                          (zip-slip + zip-bomb + exec + sensitive guards;
            //                           quarantine → validate → atomic-swap pattern)
            //   file_search          -> recursive literal-substring search within jail
            //   file_versions_list   -> list pre-write staged backups for a jailed path
            //   file_version_restore -> atomic restore of a staged version (pre-restore backup first)
            new FileArchiveCreateCommand(),
            new FileExtractCommand(),
            new FileSearchCommand(),
            new FileVersionsListCommand(),
            new FileVersionRestoreCommand(),
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

        // Performance Suite — also ship cache stats + install-state on the
        // daily diagnostics push so the dashboard stays current even if the
        // heartbeat backstop missed a cycle.
        $this->shipPerfReport();

        // S2 — ship any pending login-event batch on this same cron tick.
        // LoginProtection::shipBatch returns up to SHIP_BATCH (100) newest rows
        // above the stored cursor. We POST the batch and advance the local
        // cursor to the highest id we sent on a 2xx, mirroring the error-ship
        // block above.
        $this->shipLoginEvents();

        // Reliable-diagnostics opportunistic warm: release the HTTP response
        // to the CP first via the SAPI-aware ConnectionFinisher (fastcgi on
        // PHP-FPM, litespeed_finish_request on OpenLiteSpeed — GH #274 — or a
        // portable fallback), then run a size probe to warm the cache for the
        // next push. On a kill mid-walk (request_terminate_timeout) the
        // previously-persisted last-good remains intact. This is a cron
        // loopback, not a CP-facing request, so there is no CP-side 504 risk
        // here either way — adopted for symmetry with the other early-ACK
        // sites.
        (new ConnectionFinisher())->finish();
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        }
        (new SizeProbe())->compute();
    }

    /**
     * Cron handler for the dedicated directory-size refresh event
     * (Scheduler::HOOK_SIZES). Runs under a bounded LongRunningJob::TIME_LIMIT_SECONDS
     * cap so the du / recurse_dirsize walk has a generous but non-infinite
     * ceiling, then delegates to SizeProbe::compute() which persists the
     * result to the non-autoloaded wp_option wpmgr_agent_dir_sizes.
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
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        }
        (new SizeProbe())->compute();
    }

    /**
     * Cron handler bound to EmailLogger::HOOK_PRUNE (daily): delete email-log
     * rows older than the configured retention period and enforce the row cap.
     * Reads retention_days from the current EmailConfig. A real public method
     * (not a closure) keeps the hook table serialization-safe.
     *
     * @return void
     */
    public function pruneEmailLog(): void
    {
        $cfg  = \WPMgr\Agent\Email\EmailConfig::load();
        $days = $cfg->retention_days > 0 ? $cfg->retention_days : 14;
        (new EmailLogger())->prune($days);
    }

    /**
     * Phase 3b — cron handler bound to EmailLogReporter::HOOK_PUSH (5-min)
     * AND heartbeat backstop (priority 40). Pages unpushed email-log rows above
     * the stored cursor and POSTs them to /agent/v1/email/log. Fire-and-forget.
     * A real public method (not a closure) keeps the hook table serialization-safe.
     *
     * @return void
     */
    public function pushEmailLog(): void
    {
        $this->emailLogReporter->push();
    }

    /**
     * Phase 4b — cron handler bound to SuppressionCache::HOOK_PULL (15-min).
     * Pulls suppression-list deltas from the CP and updates the local hash store.
     * Fire-and-forget: never throws. A real public method (not a closure) so the
     * WP hook table never holds a Closure.
     *
     * @return void
     */
    public function pullSuppressionCache(): void
    {
        $this->suppressionCache->pull();
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
     * Media Optimizer — `delete_attachment` hook handler. Fires BEFORE WP purges
     * the attachment's postmeta (so the optimization blob is still readable),
     * for ALL deletion paths (wp-admin, programmatic, WP-CLI, REST).
     *
     * WordPress deletes only the files it tracks in _wp_attachment_metadata (the
     * in-place optimized file in REPLACE mode, the .avif/.webp in COEXIST mode).
     * WPMgr additionally created UNTRACKED originals that WP knows nothing about —
     * the *.wpmgr-original.<ext> archive (REPLACE) and the original-ext twin
     * (COEXIST). This handler removes ONLY those untracked, blob-derived paths,
     * confined to the uploads basedir, then best-effort notifies the CP so the
     * site_media_assets row is reconciled.
     *
     * SAFETY: only paths derived from OUR blob are deleted; deletes are confined
     * to wp_get_upload_dir()['basedir'] (realpath + str_starts_with via
     * wp_delete_file_from_directory when available, DiskWriter::delete otherwise);
     * the original_deleted guard skips the archive deletes already purged by a
     * prior media_delete_originals; and the CP notify is best-effort — a failed
     * POST never blocks or fails the user's WP delete (the CP sync sweep + the
     * sync-finalize reconciliation are the backstops).
     *
     * @param int $postId The attachment (post) id being deleted.
     * @return void
     */
    public function onDeleteAttachment(int $postId): void
    {
        if ($postId <= 0) {
            return;
        }

        // Read the blob INSIDE the hook: delete_attachment fires before WP purges
        // postmeta, so it is still present. An empty blob means this attachment
        // was never WPMgr-optimized — let WordPress handle its own deletion.
        $blob = (new MediaKeystore())->get($postId);
        if ($blob === []) {
            return;
        }

        // Compute WPMgr's untracked deletable originals via the SAME enumeration
        // the media_delete_originals command uses, so the two paths can never
        // drift. originalPathsFor() only exercises the (pure, WP-free) Rename
        // seam, so a throwaway MediaUploader/keystore here is harmless.
        $paths = (new MediaDeleteOriginalsCommand(new MediaUploader($this->signer)))
            ->originalPathsFor($blob);

        // Honor the same original_deleted guard deleteOne() uses: when a prior
        // media_delete_originals already purged the *.wpmgr-original archives,
        // skip those same archive paths (they no longer exist; the COEXIST twins
        // were never archived, so they still warrant removal). REPLACE entries
        // are the only ones originalPathsFor() emits with the archive marker, so
        // dropping paths that carry `.wpmgr-original.` skips exactly those.
        if ((int) ($blob['original_deleted'] ?? 0) === 1) {
            $marker = '.' . Rename::SUFFIX . '.';
            $paths  = array_values(array_filter(
                $paths,
                static fn (string $path): bool => strpos($path, $marker) === false
            ));
        }

        $this->deleteConfinedToUploads($paths);

        // Best-effort CP notify via the agent's OWN signed-POST primitive (the
        // same one /agent/v1/diagnostics + /agent/v1/errors use; it signs over a
        // FIXED path with the CP base from settings). NOT MediaUploader — its
        // callbacks need a CP-supplied endpoint URL that only exists during an
        // in-flight command, and a WP-core-initiated delete has none. A failed
        // POST is fine: the CP sync sweep is the backstop.
        if ($this->settings->isEnrolled()) {
            $this->shipPayload('/agent/v1/media/asset-deleted', ['wp_attachment_id' => (int) $postId]);
        }
    }

    /**
     * Unlink each path, confined to the uploads basedir. Prefers WP core's
     * wp_delete_file_from_directory($abs, $basedir) — which realpath-resolves
     * both and refuses anything that does not str_starts_with the directory, so
     * a `../` escape in a blob path cannot delete outside uploads — and falls
     * back to DiskWriter::delete() (wp_delete_file, no-op on missing) when that
     * core helper is unavailable. NEVER raw unlink(); NEVER globs a directory.
     *
     * @param list<string> $paths Absolute, blob-derived candidate paths.
     * @return void
     */
    private function deleteConfinedToUploads(array $paths): void
    {
        if ($paths === []) {
            return;
        }

        $basedir = '';
        if (function_exists('wp_get_upload_dir')) {
            $uploads = wp_get_upload_dir();
            if (is_array($uploads) && isset($uploads['basedir']) && is_string($uploads['basedir'])) {
                $basedir = (string) $uploads['basedir'];
            }
        }

        $writer = new DiskWriter();
        foreach ($paths as $path) {
            if (!is_string($path) || $path === '') {
                continue;
            }
            if ($basedir !== '' && function_exists('wp_delete_file_from_directory')) {
                // Core guard: realpath + str_starts_with($basedir). Returns false
                // on a containment violation; we deliberately do NOT then fall
                // back to DiskWriter (that would defeat the confinement).
                wp_delete_file_from_directory($path, $basedir);
                continue;
            }
            // Fallback: confine ourselves before deleting. realpath() resolves
            // any `..` segments; only delete when the resolved path is inside the
            // resolved basedir.
            if ($basedir !== '') {
                $realBase = realpath($basedir);
                $realPath = realpath($path);
                if ($realBase === false || $realPath === false) {
                    continue;
                }
                $realBase = rtrim($realBase, '/\\') . DIRECTORY_SEPARATOR;
                if (strpos($realPath, $realBase) !== 0) {
                    continue;
                }
            }
            $writer->delete($path);
        }
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
     * ADR-044 — Cron handler for the debounced auto-optimize drain event
     * (AutoOptimizeUpload::HOOK_DRAIN). Bound to the action in registerHooks().
     * A real public method (not a closure) so the WP hook table never holds a
     * Closure (which would trigger "Serialization of 'Closure' is not allowed"
     * on hosts that persist the hook table via object cache or cron inspector).
     *
     * Delegates immediately to AutoOptimizeUpload::drain() which owns all the
     * buffer-read / POST / retry logic.
     *
     * @return void
     */
    public function drainAutoOptimize(): void
    {
        $this->autoOptimizeUpload->drain();
    }

    /**
     * Heartbeat backstop for the Performance Suite: push fresh cache stats and
     * install-state to the CP so the dashboard "Server status / Verify" card
     * reflects reality even without a recent cache_enable command. Fire-and-forget.
     *
     * Bound to HOOK_HEARTBEAT at priority 35 (after errors at 30).
     *
     * @return void
     */
    public function shipPerfReport(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        try {
            $reporter = $this->cacheManager->makePerfReporter();
            if ($reporter === null) {
                return;
            }
            $reporter->reportStats();
            $reporter->reportInstallState();
        } catch (\Throwable $e) {
            // Fire-and-forget: swallow.
        }
    }

    /**
     * Phase 4 — bind the enabled de-bloat hooks. Bound to `init` (priority 0) so
     * the per-toggle remove_action/add_filter calls land before core enqueues
     * the targeted scripts/styles. Bloat::register() self-no-ops when no toggle
     * is enabled (single perf-config read), so an inert site is unaffected. A
     * real public method (not a closure) keeps the hook table serialization-safe
     * on hosts that persist it via object cache.
     *
     * @return void
     */
    public function registerBloatHooks(): void
    {
        (new Bloat())->register();
    }

    /**
     * RUM (Real User Monitoring) — bind the beacon-injection callback.
     *
     * Current implementation: bound to `wp_enqueue_scripts` (which WordPress
     * core itself fires from inside `wp_head` at priority 1), and RumInjector
     * enqueues the collector via wp_enqueue_script()/wp_add_inline_script() —
     * see {@see RumInjector::enqueue()}. Reads the perf config once and only
     * registers the hook when rumEnabled is on, so an inert site pays just a
     * single option read.
     *
     * Cache-independent by design (GH #154): the RUM collector used to be
     * injected only inside the page-cache/optimizer output buffer (Optimizer
     * stage 11), so a site with WPMgr page caching OFF — the norm when a
     * third-party page cache serves the site — or served from a third-party
     * cache HIT never got the collector, and rum_rollup stayed empty with no
     * warning. wp_enqueue_scripts is independent of any cache path: it fires
     * on every WordPress-rendered response, so binding the injector there
     * fixes the gap. A prior iteration of this fix bound at a late wp_head
     * priority and printed the collector's markup directly instead of using
     * the enqueue APIs; that has since been replaced by the enqueue-based
     * approach described above.
     *
     * @return void
     */
    public function registerRumHooks(): void
    {
        $config = PerfConfig::load();
        if (!$config->rumEnabled || !function_exists('add_action')) {
            return;
        }
        add_action('wp_enqueue_scripts', [$this, 'renderRumHead']);
    }

    /**
     * wp_enqueue_scripts callback bound by {@see registerRumHooks()}.
     * Builds a fresh RumInjector (it loads PerfConfig itself) and delegates to
     * its per-request guard chain (anonymous/GET/200/CSP/etc — see
     * RumInjector::renderHead()). A real method (not a closure) keeps the hook
     * table serialization-safe.
     *
     * @return void
     */
    public function renderRumHead(): void
    {
        (new RumInjector())->renderHead();
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

    /**
     * Expose the UpdateChecker so Admin can call checkNow().
     * Returns null in the wp.org distribution build (WPMGR_WPORG_BUILD).
     *
     * @return UpdateChecker|null
     */
    public function updateChecker(): ?UpdateChecker
    {
        return $this->updateChecker;
    }
}
