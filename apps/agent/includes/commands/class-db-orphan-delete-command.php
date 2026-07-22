<?php
/**
 * DbOrphanDeleteCommand — delete CP-supplied orphaned options, cron events,
 * and custom tables that the corpus classifier attributed to an UNINSTALLED plugin.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/db_orphan_delete
 *   Authorization: Bearer <Ed25519 JWT, cmd="db_orphan_delete", aud=<siteId>>
 *   Body: {
 *     "job_id":             "<UUID v4, required — single-use dedup key>",
 *     "items":              [{ "kind": "option|cron|table", "name": "...", "owner_slug": "..." }, ...],
 *     "progress_endpoint":  "<full URL or empty string, optional>"
 *   }
 *
 * Response (ACK — synchronous, sent before the async worker runs):
 *   { "ok": true,  "job_id": "<echoed uuid>" }
 *   { "ok": false, "detail": "<reason>" }       // on refusal
 *
 * Async model:
 *   execute() returns the ACK immediately via the REST HTTP response, then
 *   register_shutdown_function runs the actual deletions AFTER the response is
 *   flushed, via the SAPI-aware ConnectionFinisher (fastcgi/litespeed/
 *   fallback — see ConnectionFinisher). The full items[] array (kind + name +
 *   owner_slug) is captured into the closure by value — NOT just item names —
 *   so live re-verify has all fields available at delete time.
 *
 *   After each batch of up to BATCH_SIZE items the agent POSTs one progress push
 *   to progress_endpoint (signed with its Ed25519 key, same pattern as
 *   DbCleanCommand). The final push always has done=true.
 *
 * Safety model (agent-side enforcement at delete time):
 *   1. Re-derive the LIVE installed-plugin set (same union as
 *      DbCleanup::buildInstalledPluginsSnapshot) before the first delete.
 *   2. Per item: skip if owner_slug is now in the installed set (owner_installed).
 *   3. Per option: skip if option_name is in WP_CORE_OPTION_NAMES or starts with
 *      "wpmgr_" (wp_core_protected / wpmgr_protected).
 *   4. Per cron: skip if hook is in WP_CORE_CRON_HOOKS or starts with "wpmgr_"
 *      (wp_core_protected / wpmgr_protected).
 *   5. Per table: re-apply LAYER 2 (information_schema exact-match) + LAYER 1
 *      (classifyLive: refuse core and unknown owner_type) + wpmgr_ prefix guard.
 *   6. Scope is ONLY the CP-supplied allowlist; the worker never adds items.
 *   7. Max 500 items per call; the CP enforces the same cap pre-signing.
 *
 * Auth: the Router's permission_callback already enforced the signed JWT +
 * anti-replay contract (Connector::verifyCommand) before execute() runs.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Optimizer\DbCleanup;
use WPMgr\Agent\Support\ConnectionFinisher;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Orphan-delete command (async, progress-push, full allowlist capture).
 *
 * NEW standalone class — does NOT extend DbCleanCommand. The reason is the
 * db_clean ids-only capture bug: DbCleanCommand::execute() captures only
 * $capturedOnly (category-id strings) into the shutdown closure. Following
 * that pattern here and capturing only item names would lose the owner_slug
 * and kind fields required for live re-verify. This class captures the FULL
 * $capturedItems array of {kind,name,owner_slug} structs.
 */
final class DbOrphanDeleteCommand implements CommandInterface
{
    /** Maximum items accepted in a single call. */
    private const MAX_ITEMS = 500;

    /** Allowed kind values. */
    private const ALLOWED_KINDS = ['option', 'cron', 'table'];

    /** Items processed per progress POST. */
    private const BATCH_SIZE = 50;

    /** Maximum body size for progress POSTs. */
    private const MAX_BODY = 32768; // 32 KiB

    /** Timeout in seconds for each progress POST. */
    private const PROGRESS_TIMEOUT = 5;

    /**
     * WP core option names that must never be deleted regardless of the CP
     * allowlist. Mirrors DbCleanup::WP_CORE_OPTION_NAMES (private there).
     * Kept in sync manually; additions here are always safe (false negatives
     * only — the item is skipped with reason "wp_core_protected").
     *
     * @var list<string>
     */
    private const WP_CORE_OPTION_NAMES = [
        'siteurl', 'blogname', 'blogdescription', 'users_can_register',
        'admin_email', 'start_of_week', 'use_smilies', 'default_role',
        'comment_registration', 'close_comments_for_old_posts',
        'close_comments_days_old', 'thread_comments', 'thread_comments_depth',
        'page_comments', 'comments_per_page', 'default_comments_page',
        'comment_order', 'comments_notify', 'moderation_notify',
        'comment_moderation', 'require_name_email', 'comment_whitelist',
        'comment_max_links', 'moderation_keys', 'disallowed_keys',
        'blacklist_keys', 'default_pingback_flag', 'default_ping_status',
        'default_comment_status', 'blog_charset', 'date_format', 'time_format',
        'links_updated_date_format', 'stylesheet', 'template', 'posts_per_page',
        'what_to_show', 'posts_per_rss', 'rss_use_excerpt', 'mailserver_url',
        'mailserver_login', 'mailserver_pass', 'mailserver_port',
        'default_category', 'default_email_category', 'default_link_category',
        'show_on_front', 'page_on_front', 'page_for_posts', 'default_post_format',
        'upload_path', 'upload_url_path', 'thumbnail_size_w', 'thumbnail_size_h',
        'thumbnail_crop', 'medium_size_w', 'medium_size_h', 'large_size_w',
        'large_size_h', 'medium_large_size_w', 'medium_large_size_h',
        'image_default_link_type', 'image_default_size', 'image_default_align',
        'site_icon', 'permalink_structure', 'rewrite_rules', 'hack_file',
        'blog_public', 'ping_sites', 'active_plugins', 'category_base', 'tag_base',
        'db_version', 'db_upgraded', 'initial_db_version', 'wp_user_roles',
        'user_count', 'fresh_site', 'admin_user_id', 'wp_page_for_privacy_policy',
        'show_comments_cookies_opt_in', 'admin_email_lifespan', 'disallowed_keys',
        'comment_previously_approved', 'privacy_policy_content',
        'link_manager_enabled', 'finished_splitting_shared_terms',
        'https_detection_errors', 'auth_key', 'secure_auth_key', 'logged_in_key',
        'nonce_key', 'auth_salt', 'secure_auth_salt', 'logged_in_salt', 'nonce_salt',
        'wp_keys', 'wp_magic_link_secret',
        'auto_update_core_major', 'auto_update_core_minor', 'auto_update_core_dev',
        'auto_core_update_notified', 'update_core', 'update_plugins', 'update_themes',
        'update_translations', 'dismissed_update_core', 'can_compress_scripts',
        'active_sitewide_plugins', 'cron', 'doing_cron', 'auth_cookie',
        'recovery_keys', 'recovery_mode_email_rate_limit',
        'widget_pages', 'widget_calendar', 'widget_archives', 'widget_meta',
        'widget_search', 'widget_recent-posts', 'widget_recent-comments',
        'widget_links', 'widget_tag_cloud', 'widget_nav_menu', 'widget_custom_html',
        'widget_media_audio', 'widget_media_image', 'widget_media_gallery',
        'widget_media_video', 'widget_text', 'widget_rss', 'widget_categories',
        'sidebars_widgets', 'ssl_alloptions', 'wp_force_deactivated_plugins',
        'wp_attachment_pages_enabled', 'finished_updating_comment_type',
        'recently_edited', 'upload_filetypes', 'default_rol',
    ];

    /**
     * WP core WP-Cron hook names that must never be cleared.
     * Mirrors DbCleanup::WP_CORE_CRON_HOOKS (private there).
     *
     * @var list<string>
     */
    private const WP_CORE_CRON_HOOKS = [
        'wp_scheduled_delete',
        'wp_version_check',
        'wp_update_plugins',
        'wp_update_themes',
        'wp_update_user_counts',
        'wp_scheduled_auto_draft_delete',
        'recovery_mode_clean_expired_keys',
        'wp_privacy_delete_old_export_files',
        'wp_site_health_scheduled_check',
        'delete_expired_transients',
        'wp_update_comment_type_batch',
        'wp_delete_temp_updater_backups',
        'wp_https_detection',
        'wp_maybe_auto_update',
        'wp_split_shared_term_batch',
        'wp_clean_plugins_cache',
        'wp_auto_updates_maybe_update',
    ];

    private ?DbCleanup $cleanup;

    private ?Keystore $keystore;

    private ?Settings $settings;

    /** @var object|null Injected $wpdb (tests); defaults to global. */
    private ?object $wpdb;

    private ConnectionFinisher $finisher;

    /**
     * @param DbCleanup|null          $cleanup  Injected for tests; defaults to a live engine.
     * @param Keystore|null           $keystore Injected for tests; defaults to a fresh keystore.
     * @param Settings|null           $settings Injected for tests; defaults to a fresh settings.
     * @param object|null             $wpdb     Injected for tests; defaults to global $wpdb.
     * @param ConnectionFinisher|null $finisher Injected for tests; defaults to a real ladder.
     */
    public function __construct(
        ?DbCleanup $cleanup = null,
        ?Keystore $keystore = null,
        ?Settings $settings = null,
        ?object $wpdb = null,
        ?ConnectionFinisher $finisher = null
    ) {
        $this->cleanup  = $cleanup;
        $this->keystore = $keystore;
        $this->settings = $settings;
        $this->wpdb     = $wpdb ?? (isset($GLOBALS['wpdb']) && is_object($GLOBALS['wpdb']) ? $GLOBALS['wpdb'] : null);
        $this->finisher = $finisher ?? new ConnectionFinisher();
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'db_orphan_delete';
    }

    /**
     * Validate the request, register the async shutdown worker, and return the
     * frozen db_orphan_delete ACK immediately.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here — JWT
     *                                    already enforced by the Router).
     * @param array<string,mixed> $params {
     *   job_id:             string — required UUID v4.
     *   items:              list<array{kind:string,name:string,owner_slug:string}> — CP-signed allowlist.
     *   progress_endpoint?: string — optional full URL for async progress POSTs.
     * }
     * @return array{ok:bool,job_id?:string,detail?:string}
     */
    public function execute(array $claims, array $params): array
    {
        // --- Validate job_id (REQUIRED) ---------------------------------------
        $jobId = isset($params['job_id']) && is_string($params['job_id']) && $params['job_id'] !== ''
            ? $params['job_id']
            : '';

        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        // --- Validate items[] -------------------------------------------------
        if (!isset($params['items']) || !is_array($params['items']) || $params['items'] === []) {
            return ['ok' => false, 'detail' => 'items must be a non-empty array'];
        }

        $items = [];
        foreach ($params['items'] as $raw) {
            if (!is_array($raw)) {
                continue;
            }
            $kind  = isset($raw['kind'])       && is_string($raw['kind'])       ? $raw['kind']       : '';
            $name  = isset($raw['name'])       && is_string($raw['name'])       ? trim($raw['name']) : '';
            $owner = isset($raw['owner_slug']) && is_string($raw['owner_slug']) ? $raw['owner_slug'] : '';

            if (!in_array($kind, self::ALLOWED_KINDS, true) || $name === '' || $owner === '') {
                continue; // Silently skip malformed items; CP should not send these.
            }

            $items[] = ['kind' => $kind, 'name' => $name, 'owner_slug' => $owner];
        }

        if ($items === []) {
            return ['ok' => false, 'detail' => 'items contained no valid entries (kind must be option|cron|table, name and owner_slug must be non-empty)'];
        }

        // Cap at MAX_ITEMS — same cap the CP enforces pre-signing. Reject (do
        // not silently truncate) so the operator knows they need to batch.
        if (count($items) > self::MAX_ITEMS) {
            return [
                'ok'     => false,
                'detail' => sprintf('too many items in one call (%d, max %d)', count($items), self::MAX_ITEMS),
            ];
        }

        // --- Progress endpoint (optional) ------------------------------------
        $progressEndpoint = '';
        if (isset($params['progress_endpoint']) && is_string($params['progress_endpoint'])) {
            $progressEndpoint = trim($params['progress_endpoint']);
        }

        // --- Register async shutdown worker ----------------------------------
        // Capture the FULL items array (kind + name + owner_slug), NOT just names,
        // so the live re-verify step has all required fields at delete time.
        $capturedJobId    = $jobId;
        $capturedItems    = $items;         // full struct — avoids the db_clean ids-only bug
        $capturedEndpoint = $progressEndpoint;
        $capturedCleanup  = $this->cleanup;
        $capturedKeystore = $this->keystore;
        $capturedWpdb     = $this->wpdb;
        $capturedFinisher = $this->finisher;

        register_shutdown_function(
            static function () use (
                $capturedJobId,
                $capturedItems,
                $capturedEndpoint,
                $capturedCleanup,
                $capturedKeystore,
                $capturedWpdb,
                $capturedFinisher
            ): void {
                // Release the client connection (SAPI-aware: PHP-FPM,
                // OpenLiteSpeed — GH #274 — or a portable fallback) while
                // this worker process keeps running so the CP's ACK read
                // completes before the delete loop starts.
                $capturedFinisher->finish();

                // Expand execution budget — delete loops can be slow on large sites.
                if (function_exists('set_time_limit')) {
                    @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running orphan-delete loop must not hit max_execution_time; @-guarded, no-op when disabled
                }

                self::runAsync(
                    $capturedJobId,
                    $capturedItems,
                    $capturedEndpoint,
                    $capturedCleanup,
                    $capturedKeystore,
                    $capturedWpdb
                );
            }
        );

        return ['ok' => true, 'job_id' => $jobId];
    }

    // -------------------------------------------------------------------------
    // Async worker (static — runs in the shutdown callback after $this is gone)
    // -------------------------------------------------------------------------

    /**
     * Iterate the CP-signed allowlist, live-re-verify each item, delete the
     * survivors, and POST batched progress pushes to $progressEndpoint.
     *
     * @param string                                                            $jobId            UUID echoed from the original request.
     * @param list<array{kind:string,name:string,owner_slug:string}>            $items            Full item structs from the signed command.
     * @param string                                                            $progressEndpoint Full URL or empty string.
     * @param DbCleanup|null                                                    $cleanup          The classification engine.
     * @param Keystore|null                                                     $keystore         For signing progress POSTs.
     * @param object|null                                                       $wpdb             The DB handle.
     * @return void
     */
    private static function runAsync(
        string $jobId,
        array $items,
        string $progressEndpoint,
        ?DbCleanup $cleanup,
        ?Keystore $keystore,
        ?object $wpdb
    ): void {
        // Resolve $wpdb early — the global may be available in shutdown context.
        if ($wpdb === null && isset($GLOBALS['wpdb']) && is_object($GLOBALS['wpdb'])) {
            $wpdb = $GLOBALS['wpdb'];
        }

        // Build the LIVE installed-plugin set ONCE before the first delete.
        // Same union as DbCleanup::buildInstalledPluginsSnapshot():
        //   get_plugins() ∪ get_mu_plugins() ∪ get_dropins() ∪ network active_sitewide_plugins
        $installedSlugs = self::buildLiveInstalledSlugs();

        // Build the WP-core option/cron protection sets (O(1) lookup).
        $coreOptionSet = array_flip(self::WP_CORE_OPTION_NAMES);
        $coreCronSet   = array_flip(self::WP_CORE_CRON_HOOKS);

        // Prefix for wpmgr_ guard on tables.
        $tablePrefix = ($wpdb !== null && isset($wpdb->prefix) && is_string($wpdb->prefix))
            ? $wpdb->prefix
            : 'wp_';
        $wpmgrTablePrefix = $tablePrefix . 'wpmgr_';

        // Cumulative counters for the progress payload.
        $totalDeletedOptions = 0;
        $totalDeletedCron    = 0;
        $totalDeletedTables  = 0;
        $totalSkipped        = 0;

        // Batch accumulator: flush every BATCH_SIZE items (or at the end).
        $batchResults = [];
        $total        = count($items);

        foreach ($items as $idx => $item) {
            $kind  = $item['kind'];
            $name  = $item['name'];
            $owner = $item['owner_slug'];

            $result = self::processItem(
                $kind,
                $name,
                $owner,
                $installedSlugs,
                $coreOptionSet,
                $coreCronSet,
                $wpmgrTablePrefix,
                $cleanup,
                $wpdb
            );

            $batchResults[] = $result;

            // Update cumulative counters.
            if ($result['status'] === 'done') {
                match ($kind) {
                    'option' => ($totalDeletedOptions++),
                    'cron'   => ($totalDeletedCron++),
                    'table'  => ($totalDeletedTables++),
                    default  => null,
                };
            } elseif ($result['status'] === 'skipped') {
                $totalSkipped++;
            }

            $isLast       = (($idx + 1) === $total);
            $batchFull    = (count($batchResults) >= self::BATCH_SIZE);

            if ($progressEndpoint !== '' && ($batchFull || $isLast)) {
                self::postProgress(
                    $progressEndpoint,
                    $jobId,
                    $batchResults,
                    $totalDeletedOptions,
                    $totalDeletedCron,
                    $totalDeletedTables,
                    $totalSkipped,
                    $isLast,
                    $keystore
                );
                $batchResults = []; // Reset batch after push.

                // Release memory between batches.
                if (function_exists('gc_collect_cycles')) {
                    gc_collect_cycles();
                }
            }
        }

        // Edge case: empty items list passed all validation (should not happen
        // but be defensive). Emit a terminal done=true push.
        if ($total === 0 && $progressEndpoint !== '') {
            self::postProgress(
                $progressEndpoint,
                $jobId,
                [],
                0,
                0,
                0,
                0,
                true,
                $keystore
            );
        }
    }

    // -------------------------------------------------------------------------
    // Per-item processor
    // -------------------------------------------------------------------------

    /**
     * Live-re-verify and delete (or skip) a single item.
     *
     * @param string                 $kind            "option" | "cron" | "table"
     * @param string                 $name            Artefact identifier.
     * @param string                 $owner           owner_slug from the signed command.
     * @param array<string,bool>     $installedSlugs  Normalised slug → true (live set).
     * @param array<string,int>      $coreOptionSet   Flipped WP_CORE_OPTION_NAMES.
     * @param array<string,int>      $coreCronSet     Flipped WP_CORE_CRON_HOOKS.
     * @param string                 $wpmgrTablePrefix e.g. "wp_wpmgr_".
     * @param DbCleanup|null         $cleanup         Classification engine.
     * @param object|null            $wpdb            DB handle.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function processItem(
        string $kind,
        string $name,
        string $owner,
        array $installedSlugs,
        array $coreOptionSet,
        array $coreCronSet,
        string $wpmgrTablePrefix,
        ?DbCleanup $cleanup,
        ?object $wpdb
    ): array {
        // --- LIVE RE-VERIFY #1: owner_slug must NOT be currently installed ----
        $normOwner = strtolower(str_replace('-', '_', $owner));
        if (isset($installedSlugs[$normOwner])) {
            return self::skipped($kind, $name, 'owner_installed');
        }

        return match ($kind) {
            'option' => self::deleteOption($name, $coreOptionSet, $wpdb),
            'cron'   => self::deleteCron($name, $coreCronSet),
            'table'  => self::deleteTable($name, $wpmgrTablePrefix, $cleanup, $wpdb),
            default  => self::skipped($kind, $name, 'unknown_kind'),
        };
    }

    // -------------------------------------------------------------------------
    // Per-kind delete implementations
    // -------------------------------------------------------------------------

    /**
     * Delete one orphaned option row.
     *
     * Guards (in order):
     *   1. WP-core option exclusion list  → skip with "wp_core_protected"
     *   2. wpmgr_ prefix guard            → skip with "wpmgr_protected"
     *   3. Prepared DELETE with LIMIT 1   → done (rows_affected=0 treated as done)
     *
     * @param string            $name          option_name.
     * @param array<string,int> $coreOptionSet Flipped WP_CORE_OPTION_NAMES.
     * @param object|null       $wpdb          DB handle.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function deleteOption(string $name, array $coreOptionSet, ?object $wpdb): array
    {
        // Guard 2: WP-core option.
        if (isset($coreOptionSet[$name])) {
            return self::skipped('option', $name, 'wp_core_protected');
        }

        // Guard 3: wpmgr_ prefix.
        if (strncmp($name, 'wpmgr_', 6) === 0) {
            return self::skipped('option', $name, 'wpmgr_protected');
        }

        if ($wpdb === null || !method_exists($wpdb, 'prepare') || !method_exists($wpdb, 'query')) {
            return self::itemError('option', $name, 'wpdb unavailable');
        }

        if (!isset($wpdb->options) || !is_string($wpdb->options)) {
            return self::itemError('option', $name, 'wpdb->options unavailable');
        }

        $sql = $wpdb->prepare(
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared
            "DELETE FROM {$wpdb->options} WHERE option_name = %s LIMIT 1",
            $name
        );

        if (!is_string($sql)) {
            return self::itemError('option', $name, 'prepare failed');
        }

        $wpdb->query($sql); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared -- direct query on core options table; $sql is already $wpdb->prepare()-produced on the preceding line
        // rows_affected = 0 means already gone — treat as done (idempotent).

        return ['kind' => 'option', 'name' => $name, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Clear all scheduled events for one orphaned cron hook.
     *
     * Guards (in order):
     *   1. WP-core cron exclusion list → skip with "wp_core_protected"
     *   2. wpmgr_ prefix guard         → skip with "wpmgr_protected"
     *   3. wp_clear_scheduled_hook()   → done (removes ALL events for the hook)
     *
     * Using wp_clear_scheduled_hook() (rather than per-args-hash deletion)
     * matches the scanner's per-hook enumeration in DbCleanup::scanOrphanedCron().
     *
     * @param string            $hook        WP-Cron hook name.
     * @param array<string,int> $coreCronSet Flipped WP_CORE_CRON_HOOKS.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function deleteCron(string $hook, array $coreCronSet): array
    {
        // Guard 1: WP-core cron.
        if (isset($coreCronSet[$hook])) {
            return self::skipped('cron', $hook, 'wp_core_protected');
        }

        // Guard 2: wpmgr_ prefix.
        if (strncmp($hook, 'wpmgr_', 6) === 0) {
            return self::skipped('cron', $hook, 'wpmgr_protected');
        }

        if (!function_exists('wp_clear_scheduled_hook')) {
            return self::itemError('cron', $hook, 'wp_clear_scheduled_hook unavailable');
        }

        wp_clear_scheduled_hook($hook);

        return ['kind' => 'cron', 'name' => $hook, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Drop one orphaned custom table.
     *
     * Guards (in order):
     *   LAYER 2 — exact-match validation against information_schema.TABLES using
     *             a prepared statement; table name used in SQL comes from the
     *             DB catalog result, NOT raw input.
     *   LAYER 1 — classifyLive: owner_type must NOT be "core" or "unknown".
     *   wpmgr_ — refuse tables whose validated name starts with the wpmgr_ prefix.
     *   DROP TABLE IF EXISTS using only the validated name.
     *
     * @param string         $name             Raw table name from the command.
     * @param string         $wpmgrTablePrefix e.g. "wp_wpmgr_".
     * @param DbCleanup|null $cleanup          Classification engine.
     * @param object|null    $wpdb             DB handle.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function deleteTable(
        string $name,
        string $wpmgrTablePrefix,
        ?DbCleanup $cleanup,
        ?object $wpdb
    ): array {
        if ($wpdb === null || !method_exists($wpdb, 'prepare') || !method_exists($wpdb, 'get_var')) {
            return self::itemError('table', $name, 'wpdb unavailable');
        }

        // LAYER 2: information_schema exact-match — same as DbTableActionCommand.
        $validatedName = self::validateTableName($name, $wpdb);
        if ($validatedName === null) {
            return ['kind' => 'table', 'name' => $name, 'status' => 'not_found', 'detail' => 'table not found in information_schema'];
        }

        // LAYER 1: live classification — owner_type must not be core or unknown.
        $engine = $cleanup ?? new DbCleanup();
        [$ownerType] = self::classifyTableLive($validatedName, $engine, $wpdb);

        if ($ownerType === 'core') {
            return self::skipped('table', $validatedName, 'table_core');
        }
        if ($ownerType === 'unknown') {
            return self::skipped('table', $validatedName, 'table_unknown');
        }

        // wpmgr_ prefix guard on the validated name.
        if (strncmp($validatedName, $wpmgrTablePrefix, strlen($wpmgrTablePrefix)) === 0) {
            return self::skipped('table', $validatedName, 'wpmgr_protected');
        }

        // Execute DROP TABLE IF EXISTS using the information_schema-validated name.
        if (!method_exists($wpdb, 'query')) {
            return self::itemError('table', $validatedName, 'wpdb unavailable');
        }

        $result = $wpdb->query($wpdb->prepare('DROP TABLE IF EXISTS %i', $validatedName)); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.SchemaChange -- intentional DROP of an orphaned non-core table; identifier validated via information_schema and parameterized with the %i identifier placeholder (WP 6.2+)

        if ($result === false) {
            $lastError = isset($wpdb->last_error)
                && is_string($wpdb->last_error)
                && $wpdb->last_error !== ''
                ? $wpdb->last_error
                : 'unknown error';
            return self::itemError('table', $validatedName, $lastError);
        }

        return ['kind' => 'table', 'name' => $validatedName, 'status' => 'done', 'detail' => ''];
    }

    // -------------------------------------------------------------------------
    // LAYER 2: information_schema name validation (replicates DbTableActionCommand)
    // -------------------------------------------------------------------------

    /**
     * Validate a table name against the live information_schema.TABLES.
     *
     * Returns the catalog TABLE_NAME (authoritative, for use in SQL) if found,
     * or null if the table does not exist in the current database.
     *
     * @param string  $tableName Raw table name from the signed command.
     * @param object  $wpdb      DB handle.
     * @return string|null
     */
    private static function validateTableName(string $tableName, object $wpdb): ?string
    {
        $prepared = $wpdb->prepare(
            'SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = %s LIMIT 1',
            $tableName
        );

        if (!is_string($prepared)) {
            return null;
        }

        $result = $wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared -- direct information_schema catalog query; $prepared is already $wpdb->prepare()-produced; no caching (live catalog read required)
        return is_string($result) && $result !== '' ? $result : null;
    }

    // -------------------------------------------------------------------------
    // LAYER 1: live classification (replicates DbTableActionCommand::classifyLive)
    // -------------------------------------------------------------------------

    /**
     * Re-run classification at delete time so a table whose owning plugin was
     * installed between the scan and this command is not dropped.
     *
     * @param string     $validatedName Full table name from information_schema.
     * @param DbCleanup  $engine        Classification engine.
     * @param object     $wpdb          DB handle.
     * @return array{string,string} [owner_type, belongs_to]
     */
    private static function classifyTableLive(string $validatedName, DbCleanup $engine, object $wpdb): array
    {
        $prefix = isset($wpdb->prefix) && is_string($wpdb->prefix) ? $wpdb->prefix : 'wp_';

        $allPluginMeta     = self::getAllPluginMeta();
        $allThemeMeta      = self::getAllThemeMeta();
        $activePluginSlugs = self::getActivePluginSlugs();
        $activeThemeSlugs  = self::getActiveThemeSlugs();

        return $engine->classifyTable(
            $validatedName,
            $prefix,
            $activePluginSlugs,
            $allPluginMeta,
            $activeThemeSlugs,
            $allThemeMeta
        );
    }

    // -------------------------------------------------------------------------
    // Live installed-plugin set builder
    // -------------------------------------------------------------------------

    /**
     * Derive the set of normalised installed-plugin slugs from the LIVE WP state.
     *
     * Uses the same four-source union as DbCleanup::buildInstalledPluginsSnapshot():
     *   get_plugins() ∪ get_mu_plugins() ∪ get_dropins() ∪ network active_sitewide_plugins
     *
     * Returns a map of normalised_slug (hyphens→underscores, lowercase) → true
     * for O(1) membership tests. Both the original and normalised forms are added
     * so either hyphenated or underscored owner_slug values from the CP match.
     *
     * @return array<string,bool>
     */
    private static function buildLiveInstalledSlugs(): array
    {
        $slugs = [];

        $addSlug = static function (string $slug) use (&$slugs): void {
            $slug = trim($slug);
            if ($slug === '') {
                return;
            }
            // Add both the raw slug and the normalised (hyphen→underscore, lower)
            // form so "my-plugin" and "my_plugin" both match.
            $slugs[strtolower($slug)] = true;
            $norm = strtolower(str_replace('-', '_', $slug));
            $slugs[$norm] = true;
        };

        // Pass 1: regular plugins (get_plugins).
        if (function_exists('get_plugins') && is_callable('get_plugins')) {
            $all = get_plugins();
            if (is_array($all)) {
                foreach (array_keys($all) as $path) {
                    $path = (string) $path;
                    if ($path === '') {
                        continue;
                    }
                    $parts = explode('/', $path, 2);
                    $addSlug($parts[0]);
                }
            }
        }

        // Pass 2: must-use plugins (get_mu_plugins).
        if (function_exists('get_mu_plugins')) {
            $mu = get_mu_plugins();
            if (is_array($mu)) {
                foreach (array_keys($mu) as $file) {
                    $file = (string) $file;
                    if ($file === '') {
                        continue;
                    }
                    $addSlug(basename($file, '.php'));
                }
            }
        }

        // Pass 3: WordPress dropins (get_dropins).
        if (function_exists('get_dropins')) {
            $dropins = get_dropins();
            if (is_array($dropins)) {
                foreach (array_keys($dropins) as $filename) {
                    $filename = (string) $filename;
                    if ($filename === '') {
                        continue;
                    }
                    $addSlug(basename($filename, '.php'));
                }
            }
        }

        // Pass 4: network-activated plugins on multisite.
        if (function_exists('is_multisite') && is_multisite() && function_exists('get_site_option')) {
            $networkActive = get_site_option('active_sitewide_plugins');
            if (is_array($networkActive)) {
                foreach (array_keys($networkActive) as $path) {
                    $path = (string) $path;
                    if ($path === '') {
                        continue;
                    }
                    $parts = explode('/', $path, 2);
                    $addSlug($parts[0]);
                }
            }
        }

        return $slugs;
    }

    // -------------------------------------------------------------------------
    // WP helpers (mirrors DbTableActionCommand helpers; duplicated here so the
    // command is self-contained and does not depend on a non-public sibling).
    // -------------------------------------------------------------------------

    /**
     * @return array<string,string> slug → display name
     */
    private static function getAllPluginMeta(): array
    {
        if (!function_exists('get_plugins')) {
            return [];
        }
        $plugins = get_plugins();
        if (!is_array($plugins)) {
            return [];
        }
        $meta = [];
        foreach ($plugins as $path => $data) {
            if (!is_string($path) || $path === '') {
                continue;
            }
            $parts = explode('/', $path, 2);
            $slug  = $parts[0];
            if ($slug === '') {
                continue;
            }
            $name        = is_array($data) && isset($data['Name']) && is_string($data['Name']) && $data['Name'] !== ''
                ? $data['Name']
                : $slug;
            $meta[$slug] = $name;
        }
        return $meta;
    }

    /**
     * @return array<string,string> slug → display name
     */
    private static function getAllThemeMeta(): array
    {
        if (!function_exists('wp_get_themes')) {
            return [];
        }
        $themes = wp_get_themes();
        if (!is_array($themes)) {
            return [];
        }
        $meta = [];
        foreach ($themes as $slug => $theme) {
            if (!is_string($slug) || $slug === '') {
                continue;
            }
            $name = $slug;
            if (is_object($theme) && method_exists($theme, 'get')) {
                $n = $theme->get('Name');
                if (is_string($n) && $n !== '') {
                    $name = $n;
                }
            } elseif (is_array($theme) && isset($theme['Name']) && is_string($theme['Name']) && $theme['Name'] !== '') {
                $name = $theme['Name'];
            }
            $meta[$slug] = $name;
        }
        return $meta;
    }

    /**
     * @return list<string>
     */
    private static function getActivePluginSlugs(): array
    {
        if (!function_exists('get_option')) {
            return [];
        }
        $active = get_option('active_plugins');
        if (!is_array($active)) {
            return [];
        }
        $slugs = [];
        foreach ($active as $path) {
            if (!is_string($path) || $path === '') {
                continue;
            }
            $parts = explode('/', $path, 2);
            $slug  = $parts[0];
            if ($slug !== '') {
                $slugs[] = $slug;
            }
        }
        return $slugs;
    }

    /**
     * @return list<string>
     */
    private static function getActiveThemeSlugs(): array
    {
        $slugs = [];
        if (function_exists('get_stylesheet')) {
            $s = get_stylesheet();
            if (is_string($s) && $s !== '') {
                $slugs[] = $s;
            }
        }
        if (function_exists('get_template')) {
            $t = get_template();
            if (is_string($t) && $t !== '' && !in_array($t, $slugs, true)) {
                $slugs[] = $t;
            }
        }
        return $slugs;
    }

    // -------------------------------------------------------------------------
    // Progress POST
    // -------------------------------------------------------------------------

    /**
     * POST one batched progress push to the CP progress endpoint, signed with the
     * agent's Ed25519 key (same scheme as DbCleanCommand::postProgress()).
     *
     * Swallows ALL errors — a network hiccup must not halt the delete loop.
     *
     * @param string                                                      $endpoint        Full URL.
     * @param string                                                      $jobId           UUID.
     * @param list<array{kind:string,name:string,status:string,detail:string}> $results   Batch results.
     * @param int                                                         $deletedOptions  Cumulative options deleted.
     * @param int                                                         $deletedCron     Cumulative cron hooks cleared.
     * @param int                                                         $deletedTables   Cumulative tables dropped.
     * @param int                                                         $skipped         Cumulative skipped count.
     * @param bool                                                        $done            true on the final push.
     * @param Keystore|null                                               $keystore        For signing.
     * @return void
     */
    private static function postProgress(
        string $endpoint,
        string $jobId,
        array $results,
        int $deletedOptions,
        int $deletedCron,
        int $deletedTables,
        int $skipped,
        bool $done,
        ?Keystore $keystore
    ): void {
        if (!function_exists('wp_json_encode') || !function_exists('wp_remote_post')) {
            return;
        }

        $payload = [
            'job_id'          => $jobId,
            'results'         => $results,
            'deleted_options' => $deletedOptions,
            'deleted_cron'    => $deletedCron,
            'deleted_tables'  => $deletedTables,
            'skipped'         => $skipped,
            'done'            => $done,
        ];

        $body = (string) wp_json_encode($payload);

        $headers = ['Content-Type' => 'application/json', 'Accept' => 'application/json'];

        if ($keystore !== null) {
            try {
                $parsed = wp_parse_url($endpoint);
                $path   = isset($parsed['path']) && is_string($parsed['path'])
                    ? $parsed['path']
                    : '/agent/v1/db-orphan-delete/progress';
                if (isset($parsed['query']) && is_string($parsed['query']) && $parsed['query'] !== '') {
                    $path .= '?' . $parsed['query'];
                }
                $signer  = new Signer($keystore);
                $auth    = $signer->signHeaders('POST', $path, $body);
                $headers = array_merge($headers, $auth);
            } catch (\Throwable $e) {
                \WPMgr\Agent\Support\DebugLog::write(sprintf(
                    '[wpmgr] db_orphan_delete progress signing failed for job %s: %s',
                    $jobId,
                    $e->getMessage()
                ));
                return;
            }
        }

        // Guard against unexpectedly large payloads (large results[] arrays).
        if (strlen($body) > self::MAX_BODY) {
            // Log and skip; the final done=true push will still arrive when the
            // next batch is smaller (or at termination with done=true always sent).
            \WPMgr\Agent\Support\DebugLog::write(sprintf(
                '[wpmgr] db_orphan_delete job %s: progress payload too large (%d bytes), skipping batch push',
                $jobId,
                strlen($body)
            ));
            return;
        }

        try {
            wp_remote_post($endpoint, [
                'timeout'   => self::PROGRESS_TIMEOUT,
                'headers'   => $headers,
                'body'      => $body,
                'blocking'  => true,
                'sslverify' => true,
            ]);
        } catch (\Throwable $e) {
            // Swallow — a progress POST failure must not stop the delete loop.
        }
    }

    // -------------------------------------------------------------------------
    // Result shape helpers
    // -------------------------------------------------------------------------

    /**
     * @param string $kind   "option" | "cron" | "table"
     * @param string $name   Artefact identifier.
     * @param string $reason Skip reason constant.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function skipped(string $kind, string $name, string $reason): array
    {
        return ['kind' => $kind, 'name' => $name, 'status' => 'skipped', 'detail' => $reason];
    }

    /**
     * @param string $kind    "option" | "cron" | "table"
     * @param string $name    Artefact identifier.
     * @param string $message Error message.
     * @return array{kind:string,name:string,status:string,detail:string}
     */
    private static function itemError(string $kind, string $name, string $message): array
    {
        return ['kind' => $kind, 'name' => $name, 'status' => 'error', 'detail' => $message];
    }
}
