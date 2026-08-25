<?php
/**
 * DbCleanup — bounded, prepared-statement database housekeeping.
 *
 * Each task is optionally gated by its PerfConfig flag (the flag acts as a
 * fallback when the CP sends an empty tasks[] list), deletes ONLY the rows its
 * task targets, and returns a per-category result in the frozen wire shape:
 *   { rows_deleted: int, bytes_freed: int, state: "done"|"skipped"|"error",
 *     detail: string }
 *
 * When the CP provides a non-empty tasks[] list the flags are IGNORED for
 * gating — the CP is the authoritative selector of which tasks run (Defect 2
 * fix). The flags still gate the empty-list fallback path.
 *
 * The 14 canonical category ids and their runners:
 *   revisions                    DELETE posts WHERE post_type='revision'
 *   auto_drafts                  DELETE posts WHERE post_status='auto-draft'
 *   trashed_posts                DELETE posts WHERE post_status='trash'
 *   spam_comments                DELETE comments WHERE comment_approved='spam'
 *   trashed_comments             DELETE comments WHERE comment_approved='trash'
 *   expired_transients           DELETE expired _transient_timeout_* options
 *   optimize_tables              OPTIMIZE TABLE on non-InnoDB core tables
 *   orphaned_postmeta            DELETE postmeta WHERE post_id NOT IN (posts)
 *   orphaned_commentmeta         DELETE commentmeta WHERE comment_id NOT IN (comments)
 *   orphaned_term_relationships  DELETE term_relationships for missing objects
 *   oembed_cache                 DELETE posts WHERE post_type='oembed_cache'
 *   duplicate_postmeta           DELETE duplicate postmeta (keep lowest id per key)
 *   action_scheduler_completed   DELETE action_scheduler rows with status=complete
 *   action_scheduler_failed      DELETE action_scheduler rows with status=failed
 *
 * Post deletes cascade: orphaned postmeta + term_relationships are removed with
 * the parent post rows. Comment deletes cascade commentmeta. No correctness risk:
 * all targeted rows are disposable.
 *
 * Standard WordPress database-cleanup technique (transients, revisions, orphaned
 * meta, expired rows).
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Runs the gated database-cleanup tasks and reports per-category results.
 */
final class DbCleanup
{
    /** Transient holding the last OPTIMIZE-TABLE run time (Unix seconds). */
    public const OPTIMIZE_COOLDOWN_OPTION = 'wpmgr_db_optimize_last';

    /** Minimum gap between OPTIMIZE-TABLE passes, in seconds (12h). */
    public const OPTIMIZE_COOLDOWN_SECONDS = 12 * 3600;

    /**
     * Number of candidate primary-key rows to SELECT per iteration in the
     * SELECT→DELETE batching loop (Fix 1).
     */
    private const DELETE_SELECT_CHUNK = 2000;

    /**
     * Maximum SELECT→DELETE iterations before bailing with a partial result
     * (Fix 1 safety cap — prevents infinite loops on pathological tables).
     */
    private const DELETE_MAX_ITERATIONS = 1000;

    /**
     * Hard cap on the number of duplicate postmeta meta_ids collected in one
     * pass of runDeleteDuplicatePostmeta (Fix 1).
     */
    private const DUPLICATE_COLLECT_LIMIT = 50000;

    /**
     * All 14 canonical category ids in execution order.
     * Tasks not in this list are silently ignored.
     *
     * @var list<string>
     */
    public const KNOWN_TASKS = [
        'revisions',
        'auto_drafts',
        'trashed_posts',
        'spam_comments',
        'trashed_comments',
        'expired_transients',
        'optimize_tables',
        'orphaned_postmeta',
        'orphaned_commentmeta',
        'orphaned_term_relationships',
        'oembed_cache',
        'duplicate_postmeta',
        'action_scheduler_completed',
        'action_scheduler_failed',
    ];

    private PerfConfig $config;

    /** @var \wpdb|null WordPress DB handle. */
    private ?object $wpdb;

    /**
     * @param PerfConfig|null $config Optimization config (DB flags).
     * @param object|null     $wpdb   Injected $wpdb (tests); defaults to global.
     */
    public function __construct(?PerfConfig $config = null, ?object $wpdb = null)
    {
        $this->config = $config ?? PerfConfig::load();
        /** @var \wpdb|null $resolvedWpdb */
        $resolvedWpdb = $wpdb ?? (isset($GLOBALS['wpdb']) && is_object($GLOBALS['wpdb']) ? $GLOBALS['wpdb'] : null);
        $this->wpdb   = $resolvedWpdb;
    }

    /**
     * Bound on rows returned by bounded-COUNT fallback queries in scan().
     * If the fast information_schema estimate is unavailable, a SELECT COUNT(*)
     * is run with LIMIT on this value; when the count hits the cap the result
     * carries capped=true so the UI can show "10 000+".
     */
    private const SCAN_COUNT_CAP = 10000;

    /**
     * Maximum number of orphaned wp_options rows the agent will report.
     * False negatives (missing an orphan) are the safe direction; this cap prevents
     * enormous payloads on sites with many unknown options.
     */
    private const ORPHAN_OPTIONS_CAP = 500;

    /**
     * Maximum rows fetched from wp_options per paginated pass when scanning for
     * orphaned options. 1000 rows per page keeps each query fast.
     */
    private const ORPHAN_OPTIONS_PAGE = 1000;

    /**
     * Well-known WP core option names.
     * CONSERVATIVE: if an option is in this list it is SKIPPED (not an orphan).
     * Any omission is a safe false-negative.
     *
     * @var list<string>
     */
    private const WP_CORE_OPTION_NAMES = [
        // Core site settings
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
        'links_updated_date_format', 'blogdescr', 'links_recently_updated_append',
        'links_recently_updated_prepend', 'links_recently_updated_time',
        'newuser_siteurl', 'stylesheet', 'template', 'posts_per_page',
        'what_to_show', 'posts_per_rss', 'rss_use_excerpt', 'mailserver_url',
        'mailserver_login', 'mailserver_pass', 'mailserver_port',
        'default_category', 'default_email_category', 'default_link_category',
        'show_on_front', 'page_on_front', 'page_for_posts', 'default_post_format',
        'upload_path', 'upload_url_path', 'thumbnail_size_w', 'thumbnail_size_h',
        'thumbnail_crop', 'medium_size_w', 'medium_size_h', 'large_size_w',
        'large_size_h', 'medium_large_size_w', 'medium_large_size_h',
        'image_default_link_type', 'image_default_size', 'image_default_align',
        'site_icon', 'permalink_structure', 'rewrite_rules', 'hack_file',
        'blog_public', 'ping_sites', 'blogname', 'blog_charset', 'active_plugins',
        'category_base', 'tag_base', 'db_version', 'db_upgraded', 'initial_db_version',
        'wp_user_roles', 'user_count', 'fresh_site', 'admin_user_id',
        'wp_page_for_privacy_policy', 'show_comments_cookies_opt_in',
        'admin_email_lifespan', 'disallowed_keys', 'comment_previously_approved',
        'privacy_policy_content', 'link_manager_enabled', 'finished_splitting_shared_terms',
        'https_detection_errors', 'auth_key', 'secure_auth_key', 'logged_in_key',
        'nonce_key', 'auth_salt', 'secure_auth_salt', 'logged_in_salt', 'nonce_salt',
        'wp_keys', 'wp_magic_link_secret',
        // Plugin/update infrastructure
        'auto_update_core_major', 'auto_update_core_minor', 'auto_update_core_dev',
        'auto_core_update_notified', 'update_core', 'update_plugins', 'update_themes',
        'update_translations', 'dismissed_update_core', 'can_compress_scripts',
        'active_sitewide_plugins',
        // Cron / scheduler infrastructure
        'cron', 'doing_cron', 'rewrite_rules', 'auth_cookie',
        // Health / recovery
        'recovery_keys', 'recovery_mode_email_rate_limit',
        // Misc WP
        'widget_pages', 'widget_calendar', 'widget_archives', 'widget_meta',
        'widget_search', 'widget_recent-posts', 'widget_recent-comments',
        'widget_links', 'widget_tag_cloud', 'widget_nav_menu', 'widget_custom_html',
        'widget_media_audio', 'widget_media_image', 'widget_media_gallery',
        'widget_media_video', 'widget_text', 'widget_rss', 'widget_categories',
        'sidebars_widgets', 'theme_mods_twentytwentyfive', 'theme_mods_twentytwentyfour',
        'theme_mods_twentytwentythree', 'theme_mods_twentytwentytwo',
        'theme_mods_twentytwentyone', 'theme_mods_twentytwenty',
        'theme_mods_twentynineteen', 'theme_mods_twentyseventeen',
        'theme_mods_twentysixteen', 'theme_mods_twentyfifteen',
        'theme_mods_twentyfourteen', 'theme_mods_twentythirteen',
        'theme_mods_twentytwelve', 'theme_mods_twentyeleven', 'theme_mods_twentyten',
        'recently_edited', 'upload_filetypes', 'thumbnail_size_w', 'default_rol',
        'wp_attachment_pages_enabled', 'finished_updating_comment_type',
        'ssl_alloptions', 'wp_force_deactivated_plugins',
        'initial_db_version', 'wp_user_roles',
    ];

    /**
     * Well-known WP core WP-Cron hook names.
     * CONSERVATIVE: if a hook is in this list it is SKIPPED (not an orphan).
     * Any omission is a safe false-negative.
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

    /**
     * WP core bare table names (no prefix).
     * Used by the table-inventory ownership classifier.
     *
     * @var list<string>
     */
    private const WP_CORE_BARE_NAMES = [
        'terms',
        'term_taxonomy',
        'term_relationships',
        'commentmeta',
        'comments',
        'links',
        'options',
        'postmeta',
        'posts',
        'users',
        'usermeta',
        'sitecategories',
        'termmeta',
        'blogs',
        'blog_versions',
        'blogmeta',
        'registration_log',
        'signups',
        'site',
        'sitemeta',
    ];

    /**
     * READ-ONLY database scan: returns per-category {count, bytes} estimates
     * without deleting anything.  Uses information_schema metadata and bounded
     * SELECT COUNT(*) queries so it is fast on large tables.
     *
     * When $categories is non-empty, only those categories are scanned.
     * Any id not in KNOWN_TASKS is silently ignored.
     *
     * Returns an array shaped for the db_scan wire contract:
     *   [
     *     'categories'    => [ '<id>' => ['count'=>int, 'bytes'=>int, 'capped'=>bool, 'tables'=>[...]] ],
     *     'db_size_bytes' => int,
     *     'table_count'   => int,
     *     'scanned_at'    => int,  // Unix seconds
     *     'tables'        => [ ... per-table inventory rows ... ],
     *   ]
     *
     * @param list<string> $categories Category ids to scan; empty = all 14.
     * @return array{categories:array<string,array{count:int,bytes:int,capped?:bool,tables?:list<array<string,mixed>>}>,db_size_bytes:int,table_count:int,scanned_at:int,tables:list<array<string,mixed>>}
     */
    public function scan(array $categories = []): array
    {
        $target = $categories !== []
            ? array_values(array_filter($categories, static fn ($c) => in_array($c, self::KNOWN_TASKS, true)))
            : self::KNOWN_TASKS;

        $result = [
            'categories'    => [],
            'db_size_bytes' => 0,
            'table_count'   => 0,
            'scanned_at'    => time(),
            'tables'        => [],
        ];

        if ($this->wpdb === null) {
            return $result;
        }

        // Single fast query for total DB size + table count.
        [$dbSize, $tableCount] = $this->scanDbSummary();
        $result['db_size_bytes'] = $dbSize;
        $result['table_count']   = $tableCount;

        foreach ($target as $categoryId) {
            try {
                $result['categories'][$categoryId] = $this->scanCategory($categoryId);
            } catch (\Throwable $e) {
                $result['categories'][$categoryId] = ['count' => 0, 'bytes' => 0];
            }
        }

        // Per-table inventory: runs once, classifies every table, read-only.
        try {
            $result['tables'] = $this->scanTableInventory();
        } catch (\Throwable $e) {
            $result['tables'] = [];
        }

        return $result;
    }

    /**
     * Scan a single category and return {count, bytes[, capped][, tables]}.
     *
     * @param string $categoryId One of the 14 canonical ids.
     * @return array{count:int,bytes:int,capped?:bool,tables?:list<array<string,mixed>>}
     */
    private function scanCategory(string $categoryId): array
    {
        return match ($categoryId) {
            'revisions'                    => $this->scanPostsByType('revision'),
            'auto_drafts'                  => $this->scanPostsByStatus('auto-draft'),
            'trashed_posts'                => $this->scanPostsByStatus('trash'),
            'spam_comments'                => $this->scanCommentsByStatus('spam'),
            'trashed_comments'             => $this->scanCommentsByStatus('trash'),
            'expired_transients'           => $this->scanExpiredTransients(),
            'optimize_tables'              => $this->scanOptimizeTables(),
            'orphaned_postmeta'            => $this->scanOrphanedPostmeta(),
            'orphaned_commentmeta'         => $this->scanOrphanedCommentmeta(),
            'orphaned_term_relationships'  => $this->scanOrphanedTermRelationships(),
            'oembed_cache'                 => $this->scanPostsByType('oembed_cache'),
            'duplicate_postmeta'           => $this->scanDuplicatePostmeta(),
            'action_scheduler_completed'   => $this->scanActionScheduler('complete'),
            'action_scheduler_failed'      => $this->scanActionScheduler('failed'),
            default                        => ['count' => 0, 'bytes' => 0],
        };
    }

    // -------------------------------------------------------------------------
    // Per-category scan helpers — READ-ONLY, no writes, no OPTIMIZE TABLE.
    // -------------------------------------------------------------------------

    /**
     * @return array{count:int,bytes:int}
     */
    private function scanPostsByType(string $type): array
    {
        $posts = $this->table('posts');
        // Use information_schema TABLE_ROWS as the fast estimate for posts, then
        // fall back to bounded COUNT(*) only if the estimate is unavailable.
        $count = $this->boundedCount(
            "SELECT COUNT(*) FROM {$posts} WHERE post_type = %s",
            [$type]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * @return array{count:int,bytes:int}
     */
    private function scanPostsByStatus(string $status): array
    {
        $posts = $this->table('posts');
        $count = $this->boundedCount(
            "SELECT COUNT(*) FROM {$posts} WHERE post_status = %s",
            [$status]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * @return array{count:int,bytes:int}
     */
    private function scanCommentsByStatus(string $status): array
    {
        $comments = $this->table('comments');
        $count    = $this->boundedCount(
            "SELECT COUNT(*) FROM {$comments} WHERE comment_approved = %s",
            [$status]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Expired transients: count the timeout option rows whose value < now().
     * Bounded at SCAN_COUNT_CAP.
     *
     * @return array{count:int,bytes:int}
     */
    private function scanExpiredTransients(): array
    {
        $options = $this->table('options');
        $now     = time();
        $count   = $this->boundedCount(
            "SELECT COUNT(*) FROM {$options} WHERE option_name LIKE %s AND CAST(option_value AS UNSIGNED) < %d",
            ['\_transient\_timeout\_%', $now]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Optimize-tables: list non-InnoDB tables with DATA_FREE > 0.
     * Returns bytes = sum(DATA_FREE), tables = per-table detail list.
     * NEVER runs OPTIMIZE TABLE — reads information_schema only.
     *
     * @return array{count:int,bytes:int,tables:list<array<string,mixed>>}
     */
    private function scanOptimizeTables(): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'get_results')) {
            return ['count' => 0, 'bytes' => 0, 'tables' => []];
        }

        $sql = "SELECT TABLE_NAME, ENGINE, DATA_LENGTH, DATA_FREE
                FROM information_schema.TABLES
                WHERE TABLE_SCHEMA = DATABASE()
                  AND ENGINE IS NOT NULL
                  AND ENGINE <> 'InnoDB'
                  AND DATA_FREE > 0";

        $rows = $this->wpdb->get_results($sql, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- static catalog query against information_schema; no user input; table identifier validated
        if (!is_array($rows) || $rows === []) {
            return ['count' => 0, 'bytes' => 0, 'tables' => []];
        }

        $tables     = [];
        $totalFree  = 0;
        foreach ($rows as $row) {
            if (!is_array($row)) {
                continue;
            }
            $dataFree = (int) ($row['DATA_FREE'] ?? 0);
            $totalFree += $dataFree;
            $tables[] = [
                'name'        => (string) ($row['TABLE_NAME'] ?? ''),
                'engine'      => (string) ($row['ENGINE'] ?? ''),
                'data_length' => (int) ($row['DATA_LENGTH'] ?? 0),
                'data_free'   => $dataFree,
            ];
        }

        return [
            'count'  => count($tables),
            'bytes'  => $totalFree,
            'tables' => $tables,
        ];
    }

    /**
     * Orphaned postmeta: bounded count of meta rows whose post_id is absent.
     *
     * @return array{count:int,bytes:int,capped?:bool}
     */
    private function scanOrphanedPostmeta(): array
    {
        $postmeta = $this->table('postmeta');
        $posts    = $this->table('posts');
        $count    = $this->boundedCount(
            "SELECT COUNT(*) FROM (
                SELECT pm.meta_id FROM {$postmeta} pm
                LEFT JOIN {$posts} p ON pm.post_id = p.ID
                WHERE p.ID IS NULL
                LIMIT %d
            ) sub",
            [self::SCAN_COUNT_CAP]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Orphaned commentmeta: bounded count of meta rows whose comment_id is absent.
     *
     * @return array{count:int,bytes:int,capped?:bool}
     */
    private function scanOrphanedCommentmeta(): array
    {
        $commentmeta = $this->table('commentmeta');
        $comments    = $this->table('comments');
        $count       = $this->boundedCount(
            "SELECT COUNT(*) FROM (
                SELECT cm.meta_id FROM {$commentmeta} cm
                LEFT JOIN {$comments} c ON cm.comment_id = c.comment_ID
                WHERE c.comment_ID IS NULL
                LIMIT %d
            ) sub",
            [self::SCAN_COUNT_CAP]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Orphaned term_relationships: bounded count using the same post-taxonomy
     * safety filter as runDeleteOrphanedTermRelationships.
     *
     * @return array{count:int,bytes:int,capped?:bool}
     */
    private function scanOrphanedTermRelationships(): array
    {
        $termRel      = $this->table('term_relationships');
        $termTaxonomy = $this->table('term_taxonomy');
        $posts        = $this->table('posts');

        // Collect post-attached term_taxonomy_ids (exclude link_category).
        if ($this->wpdb === null || !method_exists($this->wpdb, 'get_col')) {
            return ['count' => 0, 'bytes' => 0];
        }
        $ttSql     = "SELECT term_taxonomy_id FROM {$termTaxonomy} WHERE taxonomy <> 'link_category'"; // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); no user input in values
        $ttIds     = $this->wpdb->get_col($ttSql); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- static catalog query; identifier is prefix+constant; no values to prepare
        if (!is_array($ttIds) || $ttIds === []) {
            return ['count' => 0, 'bytes' => 0];
        }

        $ttPlaceholders = implode(',', array_fill(0, count($ttIds), '%d'));
        $count          = $this->boundedCount(
            "SELECT COUNT(*) FROM (
                SELECT tr.object_id FROM {$termRel} tr
                WHERE tr.term_taxonomy_id IN ({$ttPlaceholders})
                  AND tr.object_id NOT IN (SELECT ID FROM {$posts})
                LIMIT %d
            ) sub",
            array_merge(array_map('intval', $ttIds), [self::SCAN_COUNT_CAP])
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Duplicate postmeta: bounded count of non-minimum meta_id duplicates.
     *
     * @return array{count:int,bytes:int,capped?:bool}
     */
    private function scanDuplicatePostmeta(): array
    {
        $postmeta = $this->table('postmeta');
        $count    = $this->boundedCount(
            "SELECT COUNT(*) FROM (
                SELECT pm.meta_id FROM {$postmeta} pm
                INNER JOIN (
                    SELECT post_id, meta_key, MIN(meta_id) AS keep_id
                    FROM {$postmeta}
                    GROUP BY post_id, meta_key
                    HAVING COUNT(*) > 1
                ) dup ON pm.post_id = dup.post_id
                      AND pm.meta_key = dup.meta_key
                      AND pm.meta_id <> dup.keep_id
                LIMIT %d
            ) sub",
            [self::SCAN_COUNT_CAP]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    /**
     * Action Scheduler completed/failed: count rows by status.
     * Silently returns 0 when the table does not exist.
     *
     * @param string $status 'complete' | 'failed'
     * @return array{count:int,bytes:int}
     */
    private function scanActionScheduler(string $status): array
    {
        $table = $this->table('actionscheduler_actions');
        if (!$this->tableExists($table)) {
            return ['count' => 0, 'bytes' => 0];
        }
        $count = $this->boundedCount(
            "SELECT COUNT(*) FROM {$table} WHERE status = %s",
            [$status]
        );
        return ['count' => $count['count'], 'bytes' => 0] + ($count['capped'] ? ['capped' => true] : []);
    }

    // -------------------------------------------------------------------------
    // Phase 3.3 — orphan enumeration (READ-ONLY, no deletes)
    // -------------------------------------------------------------------------

    /**
     * Enumerate wp_options rows that cannot be attributed to any installed
     * plugin, WP core, or WPMgr.  CONSERVATIVE: false negatives are safe;
     * false positives (flagging a live item) are dangerous.
     *
     * Pagination: fetches ORPHAN_OPTIONS_PAGE rows at a time until all rows
     * are inspected or ORPHAN_OPTIONS_CAP reported items are reached.
     *
     * @param list<array{slug:string,name:string,active:bool,source:string}> $installedPlugins
     *        The snapshot returned by buildInstalledPluginsSnapshot().
     * @return array{items:list<array{name:string,autoload:bool,size_bytes:int,guessed_prefix:string}>,capped:bool}
     */
    public function scanOrphanedOptions(array $installedPlugins): array
    {
        $items  = [];
        $capped = false;

        if ($this->wpdb === null || !method_exists($this->wpdb, 'get_results') || !method_exists($this->wpdb, 'prepare')) {
            return ['items' => [], 'capped' => false];
        }

        $options = $this->table('options');

        // Build normalised slug set for pass C attribution.
        $normalSlugs = $this->buildNormalisedSlugSet($installedPlugins);

        // Build source-scan map for pass D.
        $allPluginMeta = $this->getAllPluginMeta();
        $allThemeMeta  = $this->getAllThemeMeta();
        $sourceMap     = $this->buildPluginTableMap($allPluginMeta, $allThemeMeta);

        // Build a set of every string key in the source map for string-literal pass D.
        // We also need the raw PHP source string-literal check; we reuse
        // buildPluginSourceContent() for that.
        $pluginSourceIndex = $this->buildPluginSourceIndex($installedPlugins);

        $coreSet = array_flip(self::WP_CORE_OPTION_NAMES);

        $offset = 0;

        while (true) {
            if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_results') || !method_exists($this->wpdb, 'esc_like')) {
                break;
            }

            // Bind the transient-exclusion patterns as %s args so that wpdb::prepare()
            // handles the quoting and escaping correctly on all WP versions. Inlining
            // bare percent-literals in the SQL template triggers a _doing_it_wrong
            // notice on WP < 6.2 and, if the exclusion silently fails, causes
            // _transient_* rows to pass all subsequent attribution passes and surface
            // as false-positive orphans. The esc_like() + '%' suffix matches the same
            // pattern that scanExpiredTransients uses for the timeout LIKE.
            $transientPat     = $this->wpdb->esc_like('_transient_')     . '%';
            $siteTransientPat = $this->wpdb->esc_like('_site_transient_') . '%';

            $sql = "SELECT option_name, autoload, LENGTH(option_value) AS size_bytes
                    FROM {$options}
                    WHERE option_name NOT LIKE %s
                      AND option_name NOT LIKE %s
                    ORDER BY option_name
                    LIMIT %d OFFSET %d";

            $prepared = $this->wpdb->prepare($sql, $transientPat, $siteTransientPat, self::ORPHAN_OPTIONS_PAGE, $offset); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
            if (!is_string($prepared)) {
                break;
            }

            $rows = $this->wpdb->get_results($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
            if (!is_array($rows) || $rows === []) {
                break; // no more rows
            }

            foreach ($rows as $row) {
                if (!is_array($row)) {
                    continue;
                }

                $name     = (string) ($row['option_name'] ?? '');
                $autoload = ($row['autoload'] ?? 'no') === 'yes';
                $size     = (int) ($row['size_bytes'] ?? 0);

                if ($name === '') {
                    continue;
                }

                // PASS A — WP core exact match.
                if (isset($coreSet[$name])) {
                    continue;
                }

                // PASS B — wpmgr_ prefix.
                if (strncmp($name, 'wpmgr_', 6) === 0) {
                    continue;
                }

                // PASS B2 — theme_mods_ prefix (installed theme options are not orphans).
                if (strncmp($name, 'theme_mods_', 11) === 0) {
                    continue;
                }

                // Extract leading token (up to first underscore).
                $underscorePos = strpos($name, '_');
                $guessedPrefix = $underscorePos !== false ? substr($name, 0, $underscorePos) : '';

                // PASS C — installed-plugin prefix attribution.
                if ($this->isAttributableToInstalledPlugin($name, $guessedPrefix, $normalSlugs)) {
                    continue;
                }

                // PASS D — source-scan string-literal cross-check.
                if ($this->isOptionInPluginSource($name, $guessedPrefix, $pluginSourceIndex)) {
                    continue;
                }

                $items[] = [
                    'name'           => $name,
                    'autoload'       => $autoload,
                    'size_bytes'     => $size,
                    'guessed_prefix' => $guessedPrefix,
                ];

                if (count($items) >= self::ORPHAN_OPTIONS_CAP) {
                    $capped = true;
                    break 2; // exit both foreach and while
                }
            }

            if (count($rows) < self::ORPHAN_OPTIONS_PAGE) {
                break; // last page
            }

            $offset += self::ORPHAN_OPTIONS_PAGE;
        }

        return ['items' => $items, 'capped' => $capped];
    }

    /**
     * Enumerate WP-Cron events that cannot be attributed to any installed
     * plugin or WP core.
     *
     * Signal: NOT in WP_CORE_CRON_HOOKS, NOT wpmgr_ prefix, no installed slug
     * is a prefix of the hook name, hook name not found in plugin PHP source.
     * has_action() is NEVER consulted (wrong oracle at scan time).
     *
     * @param list<array{slug:string,name:string,active:bool,source:string}> $installedPlugins
     * @return list<array{hook:string,next_run_at:int,recurrence:string,args_hash:string,args_count:int}>
     */
    public function scanOrphanedCron(array $installedPlugins): array
    {
        $items = [];

        if (!function_exists('_get_cron_array')) {
            return $items;
        }

        /** @var array<int,array<string,array<string,array<string,mixed>>>> $cronArray */
        $cronArray = _get_cron_array();
        if (!is_array($cronArray)) {
            return $items;
        }

        $normalSlugs       = $this->buildNormalisedSlugSet($installedPlugins);
        $allPluginMeta     = $this->getAllPluginMeta();
        $allThemeMeta      = $this->getAllThemeMeta();
        $pluginSourceIndex = $this->buildPluginSourceIndex($installedPlugins);

        $coreHooksSet = array_flip(self::WP_CORE_CRON_HOOKS);

        foreach ($cronArray as $timestamp => $hooks) {
            if (!is_array($hooks)) {
                continue;
            }
            $nextRunAt = (int) $timestamp;

            foreach ($hooks as $hook => $events) {
                if (!is_string($hook) || !is_array($events)) {
                    continue;
                }

                // PASS A — WP core hook set.
                if (isset($coreHooksSet[$hook])) {
                    continue;
                }

                // PASS B — wpmgr_ prefix.
                if (strncmp($hook, 'wpmgr_', 6) === 0) {
                    continue;
                }

                // PASS C — installed-plugin slug-prefix attribution.
                $hookNorm = strtolower(str_replace('-', '_', $hook));
                $owned    = false;
                foreach ($normalSlugs as $slugNorm) {
                    if ($slugNorm === '') {
                        continue;
                    }
                    // Hook is owned when slug is a prefix of the hook name.
                    if (strncmp($hookNorm, $slugNorm, strlen($slugNorm)) === 0) {
                        $owned = true;
                        break;
                    }
                }
                if ($owned) {
                    continue;
                }

                // PASS D — source-scan string-literal cross-check.
                if ($this->isHookInPluginSource($hook, $pluginSourceIndex)) {
                    continue;
                }

                // Emit one OrphanedCronItem per (timestamp, hook, args_hash) triple.
                foreach ($events as $argsHash => $event) {
                    if (!is_array($event)) {
                        continue;
                    }
                    $schedule   = is_string($event['schedule'] ?? null) && $event['schedule'] !== false
                        ? (string) $event['schedule']
                        : '';
                    $args       = isset($event['args']) && is_array($event['args']) ? $event['args'] : [];
                    $argsCount  = count($args);
                    $argsHashStr = (string) $argsHash;

                    $items[] = [
                        'hook'        => $hook,
                        'next_run_at' => $nextRunAt,
                        'recurrence'  => $schedule,
                        'args_hash'   => $argsHashStr,
                        'args_count'  => $argsCount,
                    ];
                }
            }
        }

        return $items;
    }

    /**
     * Build the installed-plugins snapshot from FOUR WP APIs:
     *   1. get_plugins()                  — all regular plugins (active or inactive)
     *   2. get_mu_plugins()               — must-use plugins
     *   3. array_keys(get_dropins())      — WordPress dropins
     *   4. get_site_option('active_sitewide_plugins') — network-activated (multisite)
     *
     * active_plugins is NOT used as the installed oracle; it is only used to
     * populate the `active` flag on regular plugins. An installed-but-inactive
     * plugin is still included in the snapshot with active=false.
     *
     * @return list<array{slug:string,name:string,active:bool,source:string}>
     */
    public function buildInstalledPluginsSnapshot(): array
    {
        $snapshot   = [];
        $slugIndex  = []; // slug → snapshot array index

        // ── Pass 1: regular plugins from get_plugins() ──────────────────────────
        if (function_exists('get_plugins')) {
            /** @var array<string,array<string,string>> $allPlugins */
            $allPlugins = get_plugins();
            if (is_array($allPlugins)) {
                // Active plugins list (active_plugins is ONLY for the active flag).
                $activeList = [];
                if (function_exists('get_option')) {
                    $opt = get_option('active_plugins');
                    if (is_array($opt)) {
                        $activeList = array_flip($opt);
                    }
                }

                foreach ($allPlugins as $path => $data) {
                    if (!is_string($path) || $path === '') {
                        continue;
                    }
                    $parts = explode('/', $path, 2);
                    $slug  = $parts[0];
                    if ($slug === '') {
                        continue;
                    }
                    $name   = (is_array($data) && isset($data['Name']) && is_string($data['Name']) && $data['Name'] !== '')
                        ? $data['Name']
                        : $slug;
                    $active = isset($activeList[$path]);

                    $idx             = count($snapshot);
                    $slugIndex[$slug] = $idx;
                    $snapshot[]       = [
                        'slug'   => $slug,
                        'name'   => $name,
                        'active' => $active,
                        'source' => 'plugin',
                    ];
                }
            }
        }

        // ── Pass 2: must-use plugins from get_mu_plugins() ──────────────────────
        if (function_exists('get_mu_plugins')) {
            /** @var array<string,array<string,string>> $muPlugins */
            $muPlugins = get_mu_plugins();
            if (is_array($muPlugins)) {
                foreach ($muPlugins as $file => $data) {
                    if (!is_string($file) || $file === '') {
                        continue;
                    }
                    $slug = basename($file, '.php');
                    if ($slug === '') {
                        continue;
                    }
                    $name = (is_array($data) && isset($data['Name']) && is_string($data['Name']) && $data['Name'] !== '')
                        ? $data['Name']
                        : $slug;

                    $idx              = count($snapshot);
                    $slugIndex[$slug] = $idx;
                    $snapshot[]        = [
                        'slug'   => $slug,
                        'name'   => $name,
                        'active' => true, // mu-plugins are always active by definition
                        'source' => 'mu-plugin',
                    ];
                }
            }
        }

        // ── Pass 3: WordPress dropins ─────────────────────────────────────────────
        if (function_exists('get_dropins')) {
            $dropins = get_dropins();
            if (is_array($dropins)) {
                foreach (array_keys($dropins) as $filename) {
                    $filename = (string) $filename;
                    if ($filename === '') {
                        continue;
                    }
                    $slug = basename($filename, '.php');
                    if ($slug === '') {
                        continue;
                    }
                    // A dropin is active only when the file exists in wp-content.
                    $wpContentDir = defined('WP_CONTENT_DIR') ? (string) constant('WP_CONTENT_DIR') : '';
                    $dropin_exists = $wpContentDir !== '' && file_exists($wpContentDir . DIRECTORY_SEPARATOR . $filename);

                    $idx              = count($snapshot);
                    $slugIndex[$slug] = $idx;
                    $snapshot[]        = [
                        'slug'   => $slug,
                        'name'   => $slug, // dropins have no standard Name header via this API
                        'active' => $dropin_exists,
                        'source' => 'dropin',
                    ];
                }
            }
        }

        // ── Pass 4: network-activated plugins on multisite ────────────────────────
        if (function_exists('is_multisite') && is_multisite() && function_exists('get_site_option')) {
            $networkActive = get_site_option('active_sitewide_plugins');
            if (is_array($networkActive)) {
                foreach (array_keys($networkActive) as $path) {
                    $path = (string) $path;
                    if ($path === '') {
                        continue;
                    }
                    $parts = explode('/', $path, 2);
                    $slug  = $parts[0];
                    if ($slug === '') {
                        continue;
                    }

                    if (isset($slugIndex[$slug])) {
                        // Already in snapshot (from get_plugins()); update active + source.
                        $snapshot[$slugIndex[$slug]]['active'] = true;
                        $snapshot[$slugIndex[$slug]]['source'] = 'network';
                    } else {
                        // Edge case: plugin not found in get_plugins() scan.
                        $idx              = count($snapshot);
                        $slugIndex[$slug] = $idx;
                        $snapshot[]        = [
                            'slug'   => $slug,
                            'name'   => $slug,
                            'active' => true,
                            'source' => 'network',
                        ];
                    }
                }
            }
        }

        return $snapshot;
    }

    // -------------------------------------------------------------------------
    // Phase 3.3 — internal helpers for orphan attribution
    // -------------------------------------------------------------------------

    /**
     * Build the set of normalised slugs (hyphens→underscores, lowercase) from
     * the installed-plugins snapshot for efficient prefix checks.
     *
     * @param list<array{slug:string,name:string,active:bool,source:string}> $installedPlugins
     * @return list<string>
     */
    private function buildNormalisedSlugSet(array $installedPlugins): array
    {
        $set = [];
        foreach ($installedPlugins as $entry) {
            $slug = strtolower(str_replace('-', '_', (string) ($entry['slug'] ?? '')));
            if ($slug !== '') {
                $set[] = $slug;
            }
        }
        return array_values(array_unique($set));
    }

    /**
     * Build a simple index of all string literals found in the PHP source of
     * every installed plugin. Returns a flat set (value → true) of lowercased
     * string tokens (both single-quoted and double-quoted literals).
     *
     * This is distinct from buildPluginTableMap() which maps bare TABLE NAME
     * tokens. This index is used for hook-name and option-name string-literal
     * attribution checks.
     *
     * Reuses the same transient-cached walker infrastructure; only PHP files
     * under installed plugin directories are read. Capped at SOURCE_SCAN_MAX_FILE_BYTES.
     *
     * @param list<array{slug:string,name:string,active:bool,source:string}> $installedPlugins
     * @return array<string,true> Lowercased string literals found → true.
     */
    private function buildPluginSourceIndex(array $installedPlugins): array
    {
        static $cache = null;
        if ($cache !== null) {
            return $cache;
        }

        $transientKey = 'wpmgr_db_plugin_source_index';
        if (function_exists('get_transient')) {
            $cached = get_transient($transientKey);
            if (is_array($cached)) {
                $cache = $cached;
                return $cache;
            }
        }

        $index     = [];
        $pluginDir = defined('WP_PLUGIN_DIR') ? (string) constant('WP_PLUGIN_DIR') : '';

        foreach ($installedPlugins as $entry) {
            $slug = (string) ($entry['slug'] ?? '');
            if ($slug === '' || $pluginDir === '') {
                continue;
            }
            $dir = $pluginDir . DIRECTORY_SEPARATOR . $slug;
            if (!is_dir($dir)) {
                continue;
            }

            try {
                $iterator = new \RecursiveIteratorIterator(
                    new \RecursiveDirectoryIterator(
                        $dir,
                        \RecursiveDirectoryIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                    ),
                    \RecursiveIteratorIterator::LEAVES_ONLY
                );
            } catch (\Throwable $e) {
                continue;
            }

            foreach ($iterator as $file) {
                if (!($file instanceof \SplFileInfo) || $file->getExtension() !== 'php') {
                    continue;
                }
                $size = $file->getSize();
                if ($size === false || $size > self::SOURCE_SCAN_MAX_FILE_BYTES) {
                    continue;
                }
                $content = @file_get_contents($file->getPathname());
                if (!is_string($content) || $content === '') {
                    continue;
                }
                // Extract single-quoted and double-quoted string literals.
                if (preg_match_all('/[\'"]([a-z][a-z0-9_]{2,})[\'"]/', $content, $matches)) {
                    foreach ($matches[1] as $token) {
                        $index[strtolower((string) $token)] = true;
                    }
                }
            }
        }

        if (function_exists('set_transient')) {
            set_transient($transientKey, $index, 3600);
        }

        $cache = $index;
        return $index;
    }

    /**
     * Returns true when the option name (or its guessed prefix) is attributable
     * to an installed plugin via the slug-prefix method.
     *
     * Conservative: if any installed plugin's normalised slug is a prefix
     * substring of the guessed prefix token, the option is considered owned.
     *
     * @param string       $name          Full option_name.
     * @param string       $guessedPrefix Leading token (may be '').
     * @param list<string> $normalSlugs   Normalised slug set.
     * @return bool
     */
    private function isAttributableToInstalledPlugin(string $name, string $guessedPrefix, array $normalSlugs): bool
    {
        $nameNorm   = strtolower(str_replace('-', '_', $name));
        $prefixNorm = strtolower(str_replace('-', '_', $guessedPrefix));

        foreach ($normalSlugs as $slugNorm) {
            if ($slugNorm === '') {
                continue;
            }
            // Conservative: if the slug IS a prefix of the option name, it is owned.
            if (strncmp($nameNorm, $slugNorm, strlen($slugNorm)) === 0) {
                return true;
            }
            // Also: if the guessed prefix contains the slug as a substring (e.g. slug "yoast"
            // matching prefix "yoast" in "yoast_seo_version").
            if ($prefixNorm !== '' && strpos($prefixNorm, $slugNorm) !== false) {
                return true;
            }
        }
        return false;
    }

    /**
     * Returns true when the option_name appears as a string literal in any
     * installed plugin's PHP source (pass D).
     *
     * @param string              $name          Option name.
     * @param string              $guessedPrefix Leading token.
     * @param array<string,true>  $sourceIndex   Plugin source string-literal index.
     * @return bool
     */
    private function isOptionInPluginSource(string $name, string $guessedPrefix, array $sourceIndex): bool
    {
        $nameLower   = strtolower($name);
        $prefixLower = strtolower($guessedPrefix);

        if (isset($sourceIndex[$nameLower])) {
            return true;
        }
        if ($prefixLower !== '' && isset($sourceIndex[$prefixLower])) {
            return true;
        }
        return false;
    }

    /**
     * Returns true when the cron hook name appears as a string literal in any
     * installed plugin's PHP source (pass D).
     *
     * @param string              $hook        Hook name.
     * @param array<string,true>  $sourceIndex Plugin source string-literal index.
     * @return bool
     */
    private function isHookInPluginSource(string $hook, array $sourceIndex): bool
    {
        return isset($sourceIndex[strtolower($hook)]);
    }

    /**
     * Per-table inventory: fetches every table in the current database from
     * information_schema (one query, no LIMIT) and classifies each table using
     * the LOCAL ownership algorithm (WP core list + active plugins + themes +
     * orphan fallback). Read-only — no writes, no ANALYZE, no OPTIMIZE.
     *
     * Returned rows match the frozen contract shape:
     *   {
     *     name: string,          // full table name including prefix
     *     rows: int,             // TABLE_ROWS (InnoDB estimate)
     *     size_bytes: int,       // DATA_LENGTH + INDEX_LENGTH
     *     engine: string,        // e.g. "InnoDB"
     *     overhead_bytes: int,   // DATA_FREE
     *     belongs_to: string,    // "WordPress core" | plugin name | theme name | "Orphan"
     *     owner_type: string,    // core|plugin|theme|orphan|unknown
     *   }
     *
     * @return list<array<string,mixed>>
     */
    public function scanTableInventory(): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'get_results')) {
            return [];
        }

        $sql = "SELECT
                    TABLE_NAME        AS `name`,
                    TABLE_ROWS        AS `rows`,
                    (DATA_LENGTH + INDEX_LENGTH) AS `size_bytes`,
                    ENGINE            AS `engine`,
                    DATA_FREE         AS `overhead_bytes`
                FROM information_schema.TABLES
                WHERE TABLE_SCHEMA = DATABASE()
                ORDER BY (DATA_LENGTH + INDEX_LENGTH) DESC";

        $rows = $this->wpdb->get_results($sql, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- static catalog query against information_schema; no user input; no values to prepare
        if (!is_array($rows)) {
            return [];
        }

        // Collect plugin and theme metadata once (cheap in-memory WP reads).
        $prefix       = $this->prefix();
        $activePlugins = $this->getActivePluginSlugs();
        $allPlugins    = $this->getAllPluginMeta();
        $activeThemeSlugs = $this->getActiveThemeSlugs();
        $allThemeMeta  = $this->getAllThemeMeta();

        $inventory = [];
        foreach ($rows as $row) {
            if (!is_array($row)) {
                continue;
            }
            $name          = (string) ($row['name'] ?? '');
            $rowCount      = (int) ($row['rows'] ?? 0);
            $sizeBytes     = (int) ($row['size_bytes'] ?? 0);
            $engine        = (string) ($row['engine'] ?? '');
            $overheadBytes = (int) ($row['overhead_bytes'] ?? 0);

            [$ownerType, $belongsTo] = $this->classifyTable(
                $name,
                $prefix,
                $activePlugins,
                $allPlugins,
                $activeThemeSlugs,
                $allThemeMeta
            );

            $inventory[] = [
                'name'           => $name,
                'rows'           => $rowCount,
                'size_bytes'     => $sizeBytes,
                'engine'         => $engine,
                'overhead_bytes' => $overheadBytes,
                'belongs_to'     => $belongsTo,
                'owner_type'     => $ownerType,
            ];
        }

        return $inventory;
    }

    /**
     * Transient key for the source-scan plugin/theme → tables map.
     * TTL: 3600 s. Busted on plugin activate/deactivate via hooks registered
     * in Plugin::registerHooks() (via bustPluginTableMapCache()).
     */
    public const PLUGIN_TABLE_MAP_TRANSIENT = 'wpmgr_db_table_plugin_map';

    /**
     * Maximum PHP file size to read during source scan (2 MB). Files larger
     * than this are skipped to keep the scan fast and bounded.
     */
    private const SOURCE_SCAN_MAX_FILE_BYTES = 2 * 1024 * 1024;

    /**
     * Hardcoded own-agent bare table names (unprefixed). These are ALWAYS
     * classified as plugin/WPMgr Agent regardless of any other match.
     *
     * Derived from Schema class constants and Connector::JTI_TABLE /
     * ReplayCache::TABLE so they stay in sync with the actual table definitions.
     *
     * @var list<string>
     */
    private const WPMGR_OWN_BARE_NAMES = [
        'wpmgr_agent_jti',   // Connector::JTI_TABLE
        'wpmgr_autologin_jti', // ReplayCache::TABLE
        'wpmgr_backup_runs',
        'wpmgr_backup_tasks',
        'wpmgr_restore_runs',
        'wpmgr_restore_tasks',
        'wpmgr_php_errors',
        'wpmgr_diagnostics_runs',
        'wpmgr_activity_log',
        'wpmgr_login_events',
        'wpmgr_preload_queue',
    ];

    /**
     * Delete the source-scan transient so the next db_scan rebuilds it.
     * Called from Plugin::registerHooks() on activated_plugin / deactivated_plugin.
     *
     * @return void
     */
    public static function bustPluginTableMapCache(): void
    {
        if (function_exists('delete_transient')) {
            delete_transient(self::PLUGIN_TABLE_MAP_TRANSIENT);
        }
    }

    /**
     * Build (or return from transient) the source-scan map:
     *   bare_table_name → [slug => display_name]
     *
     * Scans PHP source files of every installed plugin and active theme for
     * table-registration patterns and records which plugin/theme slug references
     * each bare table name. See frozen contract Pass 2 for full details.
     *
     * @param array<string,string> $allPluginMeta Map of slug => display name.
     * @param array<string,string> $allThemeMeta  Map of slug => display name.
     * @return array<string,array<string,string>> bare_name => [slug => display]
     */
    private function buildPluginTableMap(array $allPluginMeta, array $allThemeMeta): array
    {
        // Try transient cache first.
        if (function_exists('get_transient')) {
            $cached = get_transient(self::PLUGIN_TABLE_MAP_TRANSIENT);
            if (is_array($cached)) {
                return $cached;
            }
        }

        $map = []; // bare_name => [slug => display_name]

        // Collect all plugins.
        $pluginDir = defined('WP_PLUGIN_DIR') ? (string) constant('WP_PLUGIN_DIR') : '';

        foreach ($allPluginMeta as $slug => $displayName) {
            if ($pluginDir === '' || $slug === '') {
                continue;
            }
            $dir = $pluginDir . DIRECTORY_SEPARATOR . $slug;
            if (!is_dir($dir)) {
                continue;
            }
            $this->scanSourceDir($dir, $slug, $displayName, $map);
        }

        // Collect all themes.
        if (function_exists('wp_get_themes')) {
            $themes = wp_get_themes();
            if (is_array($themes)) {
                foreach ($themes as $themeSlug => $themeObj) {
                    $themeSlug = (string) $themeSlug;
                    if ($themeSlug === '') {
                        continue;
                    }
                    $displayName = $allThemeMeta[$themeSlug] ?? $themeSlug;
                    $themeDir    = '';
                    if (is_object($themeObj) && method_exists($themeObj, 'get_stylesheet_directory')) {
                        $themeDir = (string) $themeObj->get_stylesheet_directory();
                    } elseif (is_object($themeObj) && isset($themeObj->theme_root, $themeObj->stylesheet)) {
                        $themeDir = $themeObj->theme_root . DIRECTORY_SEPARATOR . $themeObj->stylesheet;
                    }
                    if ($themeDir === '' || !is_dir($themeDir)) {
                        continue;
                    }
                    $this->scanSourceDir($themeDir, $themeSlug, $displayName, $map);
                }
            }
        }

        // Cache for 3600 s.
        if (function_exists('set_transient')) {
            set_transient(self::PLUGIN_TABLE_MAP_TRANSIENT, $map, 3600);
        }

        return $map;
    }

    /**
     * Recursively walk a plugin/theme directory, read each PHP file (up to
     * SOURCE_SCAN_MAX_FILE_BYTES), and look for table-registration patterns.
     * Any bare table name token found is recorded in $map under $slug.
     *
     * Patterns searched (case-insensitive):
     *   A) $wpdb->prefix . 'token'  or  $wpdb->prefix.'token'
     *   B) CREATE TABLE (IF NOT EXISTS)? ... literal token (after wp_ prefix)
     *   C) stripos fallback: if the bare candidate name appears as a literal string
     *      anywhere in the file — used below when applying the map, not here.
     *
     * @param string                                $dir         Directory to walk.
     * @param string                                $slug        Plugin/theme slug.
     * @param string                                $displayName Plugin/theme display name.
     * @param array<string,array<string,string>>   &$map        Output map (mutated).
     * @return void
     */
    private function scanSourceDir(string $dir, string $slug, string $displayName, array &$map): void
    {
        try {
            $iterator = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator(
                    $dir,
                    \RecursiveDirectoryIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                ),
                \RecursiveIteratorIterator::LEAVES_ONLY
            );
        } catch (\Throwable $e) {
            return;
        }

        foreach ($iterator as $file) {
            if (!($file instanceof \SplFileInfo)) {
                continue;
            }

            // Skip non-PHP, hidden files, and node_modules.
            $pathStr = $file->getPathname();
            if ($file->getExtension() !== 'php') {
                continue;
            }
            if (strpos($pathStr, DIRECTORY_SEPARATOR . '.') !== false) {
                continue;
            }
            if (strpos($pathStr, DIRECTORY_SEPARATOR . 'node_modules' . DIRECTORY_SEPARATOR) !== false) {
                continue;
            }

            // Skip oversized files.
            $size = $file->getSize();
            if ($size === false || $size > self::SOURCE_SCAN_MAX_FILE_BYTES) {
                continue;
            }

            $content = @file_get_contents($pathStr);
            if (!is_string($content) || $content === '') {
                continue;
            }

            // Pattern A: $wpdb->prefix . 'token'  or  $wpdb->prefix.'token'
            if (preg_match_all(
                '/\$wpdb\s*->\s*prefix\s*\.\s*[\'"]([a-z_][a-z0-9_]*)[\'"]/',
                $content,
                $matchesA,
                PREG_SET_ORDER
            )) {
                foreach ($matchesA as $m) {
                    $token = strtolower((string) ($m[1] ?? ''));
                    if ($token !== '') {
                        $map[$token][$slug] = $displayName;
                    }
                }
            }

            // Pattern B: CREATE TABLE [IF NOT EXISTS] `wp_token` or wp_token
            // Captures the part after a literal wp_ prefix in DDL.
            if (preg_match_all(
                '/CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:`?wp_`?|`?[a-z_]+_`?)([a-z_][a-z0-9_]*)`?\s*\(/i',
                $content,
                $matchesB,
                PREG_SET_ORDER
            )) {
                foreach ($matchesB as $m) {
                    $token = strtolower((string) ($m[1] ?? ''));
                    if ($token !== '') {
                        $map[$token][$slug] = $displayName;
                    }
                }
            }
        }
    }

    /**
     * LOCAL ownership classification (no cloud, no external HTTP).
     *
     * Four-pass algorithm (matches frozen contract):
     *   PASS 0 — Hardcoded WPMgr agent own tables → always plugin/WPMgr Agent.
     *   PASS 1 — WP core exact match → owner_type="core".
     *   PASS 2 — Source-scan map (transient-cached): bare name found in plugin/theme
     *            PHP source → owner_type="plugin" or "theme".
     *   PASS 3 — Slug-prefix fallback (last resort): the existing slug-prefix check
     *            for plugins whose slug IS a prefix of their table bare name.
     *   PASS 4 — Orphan fallback → owner_type="orphan".
     *
     * @param string               $fullName         Full table name (with prefix).
     * @param string               $prefix           Site table prefix (e.g. "wp_").
     * @param list<string>         $activePluginSlugs Active plugin directory slugs.
     * @param array<string,string> $allPluginMeta    Map of slug => display name.
     * @param list<string>         $activeThemeSlugs Active theme slugs (child + parent).
     * @param array<string,string> $allThemeMeta     Map of slug => display name.
     * @return array{string,string} [owner_type, belongs_to]
     */
    public function classifyTable(
        string $fullName,
        string $prefix,
        array $activePluginSlugs,
        array $allPluginMeta,
        array $activeThemeSlugs,
        array $allThemeMeta
    ): array {
        // Strip prefix to get bare name.
        $bareName = $prefix !== '' && strncmp($fullName, $prefix, strlen($prefix)) === 0
            ? substr($fullName, strlen($prefix))
            : $fullName;

        // MULTISITE SAFETY: secondary-blog tables are named <prefix><blogid>_<table>
        // (e.g. wp_2_posts). Strip a leading numeric blog-id segment so the WP-core
        // and own-agent checks still match — otherwise wp_2_posts would not match
        // "posts", be treated as non-core, and become EMPTY/DROP-eligible (which
        // would destroy a subsite). Over-matching here only ADDS protection, so it
        // is always the safe direction. Gated on is_multisite() so single-site
        // plugin tables that happen to start with "<n>_" are untouched.
        if (function_exists('is_multisite') && is_multisite()) {
            $deblog = preg_replace('/^[0-9]+_/', '', $bareName);
            if (is_string($deblog) && $deblog !== '') {
                $bareName = $deblog;
            }
        }

        $bareNameLower = strtolower($bareName);

        // PASS 0: Hardcoded WPMgr agent own tables (highest priority, always wins).
        if (in_array($bareNameLower, self::WPMGR_OWN_BARE_NAMES, true)) {
            return ['plugin', 'WPMgr Agent'];
        }

        // PASS 1: WP core exact match.
        if (in_array($bareNameLower, self::WP_CORE_BARE_NAMES, true)) {
            return ['core', 'WordPress core'];
        }

        // PASS 2: Source-scan map (built by scanning plugin/theme PHP source).
        $tableMap = $this->buildPluginTableMap($allPluginMeta, $allThemeMeta);

        if (isset($tableMap[$bareNameLower]) && is_array($tableMap[$bareNameLower]) && $tableMap[$bareNameLower] !== []) {
            $matches = $tableMap[$bareNameLower]; // [slug => display_name]

            // Prefer an active plugin slug when there are multiple matches.
            $activePluginSlugsLower = array_map('strtolower', $activePluginSlugs);
            foreach ($matches as $matchSlug => $matchDisplay) {
                if (in_array(strtolower($matchSlug), $activePluginSlugsLower, true)) {
                    $ownerType = isset($allThemeMeta[$matchSlug]) && !isset($allPluginMeta[$matchSlug])
                        ? 'theme'
                        : 'plugin';
                    return [$ownerType, $matchDisplay !== '' ? $matchDisplay : $matchSlug];
                }
            }

            // No active slug matched; pick the first alphabetically.
            ksort($matches);
            $firstSlug    = (string) array_key_first($matches);
            $firstDisplay = $matches[$firstSlug];
            $ownerType    = isset($allThemeMeta[$firstSlug]) && !isset($allPluginMeta[$firstSlug])
                ? 'theme'
                : 'plugin';
            return [$ownerType, $firstDisplay !== '' ? $firstDisplay : $firstSlug];
        }

        // PASS 3: Slug-prefix fallback (longest slug first, last resort).
        $pluginSlugs = array_keys($allPluginMeta);
        usort($pluginSlugs, static fn (string $a, string $b): int => strlen($b) - strlen($a));

        foreach ($pluginSlugs as $slug) {
            // Normalise hyphens → underscores for slug-to-table prefix comparison.
            $slugNorm = strtolower(str_replace('-', '_', $slug));
            if ($slugNorm !== '' && strncmp($bareNameLower, $slugNorm, strlen($slugNorm)) === 0) {
                $label = $allPluginMeta[$slug] ?? $slug;
                return ['plugin', $label];
            }
        }

        $themeSlugs = array_keys($allThemeMeta);
        usort($themeSlugs, static fn (string $a, string $b): int => strlen($b) - strlen($a));

        foreach ($themeSlugs as $slug) {
            $slugNorm = strtolower(str_replace('-', '_', $slug));
            if ($slugNorm !== '' && strncmp($bareNameLower, $slugNorm, strlen($slugNorm)) === 0) {
                $label = $allThemeMeta[$slug] ?? $slug;
                return ['theme', $label];
            }
        }

        // PASS 4: Orphan fallback.
        return ['orphan', 'Orphan'];
    }

    // -------------------------------------------------------------------------
    // Plugin + theme metadata collectors (in-memory WP reads, no DB queries).
    // -------------------------------------------------------------------------

    /**
     * Returns the prefix string from the wpdb object.
     *
     * @return string
     */
    private function prefix(): string
    {
        return ($this->wpdb !== null && isset($this->wpdb->prefix) && is_string($this->wpdb->prefix))
            ? $this->wpdb->prefix
            : 'wp_';
    }

    /**
     * Active plugin directory slugs from get_option('active_plugins').
     * Each path is "slug/slug.php"; we extract the directory part.
     *
     * @return list<string>
     */
    private function getActivePluginSlugs(): array
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
     * All installed plugin metadata: map of slug => display name.
     * Falls back to an empty array when get_plugins() is unavailable.
     *
     * @return array<string,string>
     */
    private function getAllPluginMeta(): array
    {
        if (!function_exists('get_plugins')) {
            return [];
        }
        /** @var array<string,array<string,string>> $plugins */
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
     * Active theme slugs: [get_stylesheet(), get_template()] (child + parent).
     * Deduplicates when child == parent (non-child themes).
     *
     * @return list<string>
     */
    private function getActiveThemeSlugs(): array
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

    /**
     * All installed themes: map of slug => display name.
     * Falls back to an empty array when wp_get_themes() is unavailable.
     *
     * @return array<string,string>
     */
    private function getAllThemeMeta(): array
    {
        if (!function_exists('wp_get_themes')) {
            return [];
        }
        /** @var array<string,mixed> $themes */
        $themes = wp_get_themes();
        if (!is_array($themes)) {
            return [];
        }
        $meta = [];
        foreach ($themes as $slug => $theme) {
            if (!is_string($slug) || $slug === '') {
                continue;
            }
            // WP_Theme objects expose get('Name'); arrays may carry 'Name' key.
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
     * Total database size (bytes) and table count from information_schema.
     *
     * @return array{int,int} [db_size_bytes, table_count]
     */
    private function scanDbSummary(): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'get_row')) {
            return [0, 0];
        }

        $sql = "SELECT
                    COALESCE(SUM(data_length + index_length), 0) AS db_size_bytes,
                    COUNT(*) AS table_count
                FROM information_schema.TABLES
                WHERE TABLE_SCHEMA = DATABASE()";

        $row = $this->wpdb->get_row($sql, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- static catalog query against information_schema; no user input; no values to prepare
        if (!is_array($row)) {
            return [0, 0];
        }
        return [(int) ($row['db_size_bytes'] ?? 0), (int) ($row['table_count'] ?? 0)];
    }

    /**
     * Run a bounded COUNT(*) query and return the count + whether it was capped.
     *
     * For queries that wrap a subquery with LIMIT %d as the last argument, the
     * cap is detected by comparing the result to SCAN_COUNT_CAP.
     *
     * For plain COUNT(*) queries without a subquery LIMIT, the raw count is
     * returned and capped is always false (the query is fast because it targets
     * indexed columns only).
     *
     * @param string           $sql  SQL with %s/%d placeholders.
     * @param array<int,mixed> $args Bound args (the LIMIT value must be last for
     *                               subquery-wrapped forms).
     * @return array{count:int,capped:bool}
     */
    private function boundedCount(string $sql, array $args): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_var')) {
            return ['count' => 0, 'capped' => false];
        }
        $prepared = $this->wpdb->prepare($sql, ...$args); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
        if (!is_string($prepared)) {
            return ['count' => 0, 'capped' => false];
        }
        $raw = $this->wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        $n   = is_numeric($raw) ? (int) $raw : 0;

        // Detect cap: the last arg is SCAN_COUNT_CAP for subquery-wrapped forms.
        $lastArg = $args !== [] ? end($args) : null;
        $capped  = ($lastArg === self::SCAN_COUNT_CAP && $n >= self::SCAN_COUNT_CAP);

        return ['count' => $n, 'capped' => $capped];
    }

    // -------------------------------------------------------------------------

    /**
     * Run each requested task and return per-category results in the wire shape.
     *
     * When $only is non-empty the CP is the authoritative task selector and
     * PerfConfig flags are NOT consulted for gating — only the KNOWN_TASKS guard
     * applies. When $only is empty, PerfConfig flags gate which tasks run (the
     * legacy / fallback path for callers that have not yet adopted the tasks[]
     * field).
     *
     * @param list<string> $only If non-empty, run only these task ids (filtered
     *                           against KNOWN_TASKS). Empty = use PerfConfig flags.
     * @return array<string,array{rows_deleted:int,bytes_freed:int,state:string,detail:string}>
     */
    public function run(array $only = []): array
    {
        $report = [];
        if ($this->wpdb === null) {
            return $report;
        }

        // Build the gated task map. When $only is non-empty the gate is always
        // true (CP decides); when $only is empty the PerfConfig flag gates.
        $cpDriven = $only !== [];

        $tasks = [
            'revisions' => [
                'enabled' => $cpDriven || $this->config->dbPostRevisions,
                'runner'  => fn (): array => $this->runDeletePostsByType('revision'),
            ],
            'auto_drafts' => [
                'enabled' => $cpDriven || $this->config->dbPostAutoDrafts,
                'runner'  => fn (): array => $this->runDeletePostsByStatus('auto-draft'),
            ],
            'trashed_posts' => [
                'enabled' => $cpDriven || $this->config->dbPostTrashed,
                'runner'  => fn (): array => $this->runDeletePostsByStatus('trash'),
            ],
            'spam_comments' => [
                'enabled' => $cpDriven || $this->config->dbCommentsSpam,
                'runner'  => fn (): array => $this->runDeleteCommentsByStatus('spam'),
            ],
            'trashed_comments' => [
                'enabled' => $cpDriven || $this->config->dbCommentsTrashed,
                'runner'  => fn (): array => $this->runDeleteCommentsByStatus('trash'),
            ],
            'expired_transients' => [
                'enabled' => $cpDriven || $this->config->dbTransientsExpired,
                'runner'  => fn (): array => $this->runDeleteExpiredTransients(),
            ],
            'optimize_tables' => [
                'enabled' => $cpDriven || $this->config->dbOptimizeTables,
                'runner'  => fn (): array => $this->runOptimizeTables(),
            ],
            'orphaned_postmeta' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteOrphanedPostmeta(),
            ],
            'orphaned_commentmeta' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteOrphanedCommentmeta(),
            ],
            'orphaned_term_relationships' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteOrphanedTermRelationships(),
            ],
            'oembed_cache' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteOembedCache(),
            ],
            'duplicate_postmeta' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteDuplicatePostmeta(),
            ],
            'action_scheduler_completed' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteActionScheduler('complete'),
            ],
            'action_scheduler_failed' => [
                'enabled' => $cpDriven,
                'runner'  => fn (): array => $this->runDeleteActionScheduler('failed'),
            ],
        ];

        foreach ($tasks as $key => $spec) {
            // Allow-list filter when $only is non-empty.
            if ($cpDriven && !in_array($key, $only, true)) {
                continue;
            }
            if (!$spec['enabled']) {
                continue;
            }
            try {
                $result = ($spec['runner'])();
                $report[$key] = $result;
            } catch (\Throwable $e) {
                $report[$key] = $this->errorResult('internal error: ' . $e->getMessage());
            }
        }

        return $report;
    }

    // -------------------------------------------------------------------------
    // Per-category runners — each returns the wire-shape result array.
    // -------------------------------------------------------------------------

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeletePostsByType(string $type): array
    {
        $posts = $this->table('posts');
        $ids   = $this->ids("SELECT ID FROM {$posts} WHERE post_type = %s", $type);
        $rows  = $this->deletePostIds($ids);
        return $this->doneResult($rows);
    }

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeletePostsByStatus(string $status): array
    {
        $posts = $this->table('posts');
        $ids   = $this->ids("SELECT ID FROM {$posts} WHERE post_status = %s", $status);
        $rows  = $this->deletePostIds($ids);
        return $this->doneResult($rows);
    }

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteCommentsByStatus(string $status): array
    {
        $comments    = $this->table('comments');
        $commentmeta = $this->table('commentmeta');

        $ids = $this->ids("SELECT comment_ID FROM {$comments} WHERE comment_approved = %s", $status);
        if ($ids === []) {
            return $this->doneResult(0);
        }
        $deleted = 0;
        foreach (array_chunk($ids, 500) as $chunk) {
            $placeholders = implode(',', array_fill(0, count($chunk), '%d'));
            $this->query("DELETE FROM {$commentmeta} WHERE comment_id IN ({$placeholders})", $chunk);
            $deleted += (int) $this->query("DELETE FROM {$comments} WHERE comment_ID IN ({$placeholders})", $chunk);
        }
        return $this->doneResult($deleted);
    }

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteExpiredTransients(): array
    {
        $options = $this->table('options');
        $now     = time();

        $rows = $this->getResults(
            "SELECT option_name FROM {$options} WHERE option_name LIKE %s AND option_value < %d",
            ['\_transient\_timeout\_%', $now]
        );
        if ($rows === []) {
            return $this->doneResult(0);
        }

        $names = [];
        foreach ($rows as $row) {
            $timeoutName = is_array($row) ? (string) ($row['option_name'] ?? '') : '';
            if ($timeoutName === '') {
                continue;
            }
            $names[] = $timeoutName;
            $names[] = '_transient_' . substr($timeoutName, strlen('_transient_timeout_'));
        }
        if ($names === []) {
            return $this->doneResult(0);
        }

        $deleted = 0;
        foreach (array_chunk($names, 500) as $chunk) {
            $placeholders = implode(',', array_fill(0, count($chunk), '%s'));
            $deleted += (int) $this->query("DELETE FROM {$options} WHERE option_name IN ({$placeholders})", $chunk);
        }
        return $this->doneResult($deleted);
    }

    /**
     * OPTIMIZE the core tables to reclaim space after deletes.
     *
     * Gated by a per-site cooldown (a transient): OPTIMIZE TABLE locks/rebuilds
     * each table and is expensive on large sites, so we run it at most once every
     * OPTIMIZE_COOLDOWN_SECONDS. A run inside the window returns state=skipped.
     *
     * bytes_freed is the DATA_FREE delta observed in information_schema BEFORE and
     * AFTER the OPTIMIZE pass — only non-InnoDB tables eligible.
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runOptimizeTables(): array
    {
        $now = time();
        if (function_exists('get_transient')) {
            $last = get_transient(self::OPTIMIZE_COOLDOWN_OPTION);
            if (is_numeric($last) && ($now - (int) $last) < self::OPTIMIZE_COOLDOWN_SECONDS) {
                return $this->skippedResult('within cooldown window');
            }
        }

        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return $this->doneResult(0);
        }

        $candidates = [];
        foreach (['posts', 'postmeta', 'options', 'comments', 'commentmeta', 'term_relationships'] as $name) {
            $candidates[] = $this->table($name);
        }

        // Restrict to non-InnoDB tables with DATA_FREE > 0; InnoDB OPTIMIZE TABLE
        // locks the whole table and can cause multi-second outages on large sites.
        $dataFreesBefore = $this->dataFreeMap($candidates);
        $optimizable     = array_keys(array_filter($dataFreesBefore, static fn ($v) => $v >= 0));

        $count = 0;
        foreach ($optimizable as $table) {
            $this->wpdb->query($this->wpdb->prepare('OPTIMIZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+)
            $count++;
        }

        $bytesFreed = 0;
        if ($count > 0) {
            $dataFreesAfter = $this->dataFreeMap($candidates);
            foreach ($dataFreesBefore as $tbl => $before) {
                $after = $dataFreesAfter[$tbl] ?? $before;
                if ($before > $after) {
                    $bytesFreed += $before - $after;
                }
            }
            if (function_exists('set_transient')) {
                set_transient(self::OPTIMIZE_COOLDOWN_OPTION, $now, self::OPTIMIZE_COOLDOWN_SECONDS);
            }
        }

        return [
            'rows_deleted' => $count,
            'bytes_freed'  => $bytesFreed,
            'state'        => 'done',
            'detail'       => '',
        ];
    }

    /**
     * Delete orphaned postmeta rows (post_id not in posts).
     *
     * Uses a bounded SELECT→DELETE loop instead of a single multi-table DELETE
     * JOIN to avoid long table locks on large databases (Fix 1).
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteOrphanedPostmeta(): array
    {
        $postmeta = $this->table('postmeta');
        $posts    = $this->table('posts');

        $sql = "SELECT pm.meta_id FROM {$postmeta} pm
                LEFT JOIN {$posts} p ON pm.post_id = p.ID
                WHERE p.ID IS NULL
                LIMIT %d";

        [$deleted, $detail] = $this->batchedSelectDelete(
            $sql,
            [self::DELETE_SELECT_CHUNK],
            $postmeta,
            'meta_id'
        );

        $result              = $this->doneResult($deleted);
        $result['detail']    = $detail;
        return $result;
    }

    /**
     * Delete orphaned commentmeta rows (comment_id not in comments).
     *
     * Uses a bounded SELECT→DELETE loop instead of a single multi-table DELETE
     * JOIN to avoid long table locks on large databases (Fix 1).
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteOrphanedCommentmeta(): array
    {
        $commentmeta = $this->table('commentmeta');
        $comments    = $this->table('comments');

        $sql = "SELECT cm.meta_id FROM {$commentmeta} cm
                LEFT JOIN {$comments} c ON cm.comment_id = c.comment_ID
                WHERE c.comment_ID IS NULL
                LIMIT %d";

        [$deleted, $detail] = $this->batchedSelectDelete(
            $sql,
            [self::DELETE_SELECT_CHUNK],
            $commentmeta,
            'meta_id'
        );

        $result           = $this->doneResult($deleted);
        $result['detail'] = $detail;
        return $result;
    }

    /**
     * Delete orphaned term_relationships rows for POST-attached taxonomies only.
     *
     * Fix 2: the previous implementation deleted every term_relationships row
     * whose object_id was absent from wp_posts.  That is wrong for taxonomies
     * such as 'link_category' where object_id is a wp_links link_id, not a
     * post id — those rows are legitimate and must not be removed.
     *
     * The corrected approach:
     *   1. Collect the term_taxonomy_ids that belong to taxonomies we can
     *      positively identify as post-attached (i.e. NOT 'link_category' and
     *      NOT in a conservative exclusion list).
     *   2. Among those tt_ids, SELECT the (object_id, term_taxonomy_id) pairs
     *      where object_id is absent from wp_posts — in chunks.
     *   3. DELETE those rows scoped to both object_id AND term_taxonomy_id so
     *      we never touch a link_category row even if an object_id coincidentally
     *      matches a missing post id in another taxonomy.
     *
     * Fix 1: uses the bounded SELECT→DELETE loop instead of one large JOIN DELETE.
     *
     * Conservatism: any taxonomy we cannot positively classify as post-attached
     * is left alone — we err on the side of NOT deleting.
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteOrphanedTermRelationships(): array
    {
        $termRel      = $this->table('term_relationships');
        $termTaxonomy = $this->table('term_taxonomy');
        $posts        = $this->table('posts');

        // Step 1 — Collect term_taxonomy_ids that are genuinely post-attached.
        // We exclude 'link_category' (object_id = link_id, not post_id) and any
        // other taxonomy we cannot safely classify.  We use NOT IN with a fixed
        // exclusion list rather than an inclusion list so future built-in
        // non-post object types are also protected until explicitly allow-listed.
        $nonPostTaxonomies = ['link_category'];
        $excludePlaceholders = implode(',', array_fill(0, count($nonPostTaxonomies), '%s'));

        $ttSql = "SELECT term_taxonomy_id FROM {$termTaxonomy}
                  WHERE taxonomy NOT IN ({$excludePlaceholders})";

        $postTtIds = $this->ids($ttSql, ...$nonPostTaxonomies);

        if ($postTtIds === []) {
            return $this->doneResult(0);
        }

        // Step 2 + 3 — Iteratively SELECT orphaned rows and DELETE them in chunks.
        // We page through SELECT results LIMIT DELETE_SELECT_CHUNK at a time until
        // fewer than a full chunk is returned (indicating all orphans are gone).
        $ttPlaceholders  = implode(',', array_fill(0, count($postTtIds), '%d'));
        $deleted         = 0;
        $iterations      = 0;
        $cappedPartial   = false;

        while (true) {
            if ($iterations >= self::DELETE_MAX_ITERATIONS) {
                $cappedPartial = true;
                break;
            }
            $iterations++;

            // SELECT one chunk of orphaned object_ids (within our safe tt_ids).
            // The tt_id %d placeholders come first in the IN clause, then the
            // LIMIT %d at the end — args are bound left-to-right by prepare().
            $selectSql  = "SELECT tr.object_id FROM {$termRel} tr
                           WHERE tr.term_taxonomy_id IN ({$ttPlaceholders})
                             AND tr.object_id NOT IN (SELECT ID FROM {$posts})
                           LIMIT %d";
            $selectArgs = array_merge($postTtIds, [self::DELETE_SELECT_CHUNK]);
            $objectIds  = $this->ids($selectSql, ...$selectArgs);

            if ($objectIds === []) {
                break;
            }

            // DELETE rows matching BOTH the safe tt_ids AND the orphan object_ids.
            // This ensures we never delete a link_category row even if its link_id
            // numerically coincides with an absent post_id in another taxonomy.
            $objPlaceholders = implode(',', array_fill(0, count($objectIds), '%d'));
            $deleteArgs      = array_merge($objectIds, $postTtIds);
            $deleted += $this->query(
                "DELETE FROM {$termRel}
                 WHERE object_id IN ({$objPlaceholders})
                   AND term_taxonomy_id IN ({$ttPlaceholders})",
                $deleteArgs
            );

            if (count($objectIds) < self::DELETE_SELECT_CHUNK) {
                break; // last page — all orphans processed
            }
        }

        $result           = $this->doneResult($deleted);
        $result['detail'] = $cappedPartial ? 'partial: iteration cap reached' : '';
        return $result;
    }

    /**
     * Delete posts of type 'oembed_cache' (WordPress-generated oEmbed previews).
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteOembedCache(): array
    {
        $posts = $this->table('posts');
        $ids   = $this->ids("SELECT ID FROM {$posts} WHERE post_type = %s", 'oembed_cache');
        $rows  = $this->deletePostIds($ids);
        return $this->doneResult($rows);
    }

    /**
     * Delete duplicate postmeta rows (same post_id + meta_key), keeping the
     * lowest meta_id per (post_id, meta_key) group.
     *
     * Fix 1: replaces a single unbounded multi-table DELETE JOIN (which cannot
     * take a LIMIT and can long-lock the table) with:
     *   1. A capped read-only SELECT to collect the meta_ids to delete
     *      (up to DUPLICATE_COLLECT_LIMIT rows in one pass).
     *   2. A chunked DELETE … WHERE meta_id IN (…) via deleteByColumn().
     *
     * Semantics are identical: keeps the MIN(meta_id) per (post_id, meta_key)
     * and removes all higher duplicates. If there are more duplicates than
     * DUPLICATE_COLLECT_LIMIT the detail field records 'partial: collect cap
     * reached' so the CP can schedule a follow-up run.
     *
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteDuplicatePostmeta(): array
    {
        $postmeta = $this->table('postmeta');

        // Collect all non-minimum meta_ids (read-only subquery — no JOIN DELETE).
        // LIMIT caps collection to avoid an enormous in-memory array on pathological
        // tables; the CP can re-run the task to remove additional batches.
        $collectSql = "SELECT pm.meta_id FROM {$postmeta} pm
                       INNER JOIN (
                           SELECT post_id, meta_key, MIN(meta_id) AS keep_id
                           FROM {$postmeta}
                           GROUP BY post_id, meta_key
                           HAVING COUNT(*) > 1
                       ) dup ON pm.post_id = dup.post_id
                             AND pm.meta_key = dup.meta_key
                             AND pm.meta_id <> dup.keep_id
                       LIMIT %d";

        $metaIds = $this->ids($collectSql, self::DUPLICATE_COLLECT_LIMIT);
        if ($metaIds === []) {
            return $this->doneResult(0);
        }

        $partial = count($metaIds) >= self::DUPLICATE_COLLECT_LIMIT;
        $deleted = $this->deleteByColumn($postmeta, 'meta_id', $metaIds);

        $result           = $this->doneResult($deleted);
        $result['detail'] = $partial ? 'partial: collect cap reached' : '';
        return $result;
    }

    /**
     * Delete Action Scheduler rows with the given status ('complete' or 'failed').
     * The Action Scheduler table may not exist — silently returns 0 in that case.
     *
     * @param string $status 'complete' | 'failed'
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function runDeleteActionScheduler(string $status): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'query')) {
            return $this->doneResult(0);
        }
        $table = $this->table('actionscheduler_actions');

        // Guard: only run when the table exists. query() on a non-existent table
        // produces a WordPress DB error; check information_schema cheaply first.
        if (!$this->tableExists($table)) {
            return $this->skippedResult('action_scheduler table not found');
        }

        $prepared = $this->wpdb->prepare(
            "DELETE FROM {$table} WHERE status = %s", // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            $status
        );
        if (!is_string($prepared)) {
            return $this->doneResult(0);
        }
        $result = $this->wpdb->query($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        return $this->doneResult(is_numeric($result) ? (int) $result : 0);
    }

    // -------------------------------------------------------------------------
    // Result shape helpers
    // -------------------------------------------------------------------------

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function doneResult(int $rowsDeleted, int $bytesFreed = 0): array
    {
        return [
            'rows_deleted' => $rowsDeleted,
            'bytes_freed'  => $bytesFreed,
            'state'        => 'done',
            'detail'       => '',
        ];
    }

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function skippedResult(string $detail): array
    {
        return [
            'rows_deleted' => 0,
            'bytes_freed'  => 0,
            'state'        => 'skipped',
            'detail'       => $detail,
        ];
    }

    /**
     * @return array{rows_deleted:int,bytes_freed:int,state:string,detail:string}
     */
    private function errorResult(string $detail): array
    {
        return [
            'rows_deleted' => 0,
            'bytes_freed'  => 0,
            'state'        => 'error',
            'detail'       => $detail,
        ];
    }

    // -------------------------------------------------------------------------
    // Internal helpers
    // -------------------------------------------------------------------------

    /**
     * Delete a set of post ids plus their orphaned postmeta + term relationships.
     *
     * @param list<int> $ids Post ids.
     * @return int Posts deleted.
     */
    private function deletePostIds(array $ids): int
    {
        if ($ids === []) {
            return 0;
        }
        $posts    = $this->table('posts');
        $postmeta = $this->table('postmeta');
        $termRel  = $this->table('term_relationships');

        $deleted = 0;
        foreach (array_chunk($ids, 500) as $chunk) {
            $placeholders = implode(',', array_fill(0, count($chunk), '%d'));
            $this->query("DELETE FROM {$postmeta} WHERE post_id IN ({$placeholders})", $chunk);
            $this->query("DELETE FROM {$termRel} WHERE object_id IN ({$placeholders})", $chunk);
            $deleted += (int) $this->query("DELETE FROM {$posts} WHERE ID IN ({$placeholders})", $chunk);
        }
        return $deleted;
    }

    /**
     * Return DATA_FREE values (bytes) for the given tables. Non-InnoDB tables
     * with DATA_FREE > 0 are the candidates for OPTIMIZE. InnoDB is excluded
     * because OPTIMIZE TABLE locks and rebuilds it. Returns a map of table =>
     * DATA_FREE; tables not found in information_schema are omitted.
     *
     * @param string[] $tables
     * @return array<string,int>
     */
    private function dataFreeMap(array $tables): array
    {
        if ($tables === [] || $this->wpdb === null || !method_exists($this->wpdb, 'get_col') || !method_exists($this->wpdb, 'prepare')) {
            return [];
        }
        $placeholders = implode(',', array_fill(0, count($tables), '%s'));
        $sql = "SELECT TABLE_NAME, DATA_FREE FROM information_schema.TABLES
                WHERE TABLE_SCHEMA = DATABASE()
                  AND ENGINE IS NOT NULL AND ENGINE <> 'InnoDB'
                  AND DATA_FREE > 0
                  AND TABLE_NAME IN ($placeholders)";
        /** @var string $prepared */
        $prepared = $this->wpdb->prepare($sql, $tables); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
        if (!is_string($prepared)) {
            return [];
        }
        if (!method_exists($this->wpdb, 'get_results')) {
            return [];
        }
        $rows = $this->wpdb->get_results($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        if (!is_array($rows)) {
            return [];
        }
        $out = [];
        foreach ($rows as $row) {
            if (is_array($row) && isset($row['TABLE_NAME'])) {
                $out[(string) $row['TABLE_NAME']] = (int) ($row['DATA_FREE'] ?? 0);
            }
        }
        return $out;
    }

    /**
     * Check whether a table exists in the current database.
     *
     * @param string $table Fully-qualified table name (with prefix).
     * @return bool
     */
    private function tableExists(string $table): bool
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_col')) {
            return false;
        }
        $prepared = $this->wpdb->prepare(
            "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = %s LIMIT 1",
            $table
        );
        if (!is_string($prepared)) {
            return false;
        }
        $col = $this->wpdb->get_col($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line; value is output of $wpdb->prepare()
        return is_array($col) && count($col) > 0;
    }

    /**
     * Run a raw (unprepared) DELETE statement directly and return affected rows.
     * Only for statements whose table names come from the trusted prefix (never
     * user input). No user-supplied values appear in these statements.
     *
     * @param string $sql Raw SQL with no placeholders.
     * @return int
     */
    private function directQuery(string $sql): int
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return 0;
        }
        $result = $this->wpdb->query($sql); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- table names from trusted prefix; no user input; intentionally raw for trusted callers
        return is_numeric($result) ? (int) $result : 0;
    }

    // -------------------------------------------------------------------------
    // wpdb helpers
    // -------------------------------------------------------------------------

    /**
     * Fully-qualified table name from the trusted prefix.
     *
     * @param string $name Unprefixed core table name.
     * @return string
     */
    private function table(string $name): string
    {
        $prefix = ($this->wpdb !== null && isset($this->wpdb->prefix) && is_string($this->wpdb->prefix) && $this->wpdb->prefix !== '')
            ? $this->wpdb->prefix
            : 'wp_';
        return $prefix . $name;
    }

    /**
     * Run a prepared SELECT and return an integer id column.
     *
     * @param string       $sql  SQL with %s/%d placeholders.
     * @param mixed        ...$args Bound args.
     * @return list<int>
     */
    private function ids(string $sql, ...$args): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_col')) {
            return [];
        }
        $prepared = $this->wpdb->prepare($sql, ...$args); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
        if (!is_string($prepared)) {
            return [];
        }
        $col = $this->wpdb->get_col($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        if (!is_array($col)) {
            return [];
        }
        return array_map('intval', $col);
    }

    /**
     * Run a prepared SELECT returning associative rows.
     *
     * @param string            $sql  SQL with placeholders.
     * @param array<int,mixed>  $args Bound args.
     * @return list<array<string,mixed>>
     */
    private function getResults(string $sql, array $args): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_results')) {
            return [];
        }
        $prepared = $this->wpdb->prepare($sql, ...$args); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
        if (!is_string($prepared)) {
            return [];
        }
        $rows = $this->wpdb->get_results($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        return is_array($rows) ? $rows : [];
    }

    /**
     * Run a prepared write and return the affected-row count.
     *
     * @param string           $sql  SQL with placeholders.
     * @param array<int,mixed> $args Bound args.
     * @return int
     */
    private function query(string $sql, array $args): int
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'query')) {
            return 0;
        }
        $prepared = $this->wpdb->prepare($sql, ...$args); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
        if (!is_string($prepared)) {
            return 0;
        }
        $result = $this->wpdb->query($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- already prepared on the preceding line; value is output of $wpdb->prepare()
        return is_numeric($result) ? (int) $result : 0;
    }

    /**
     * Chunked DELETE by a single integer column (Fix 1 helper).
     *
     * Deletes all rows in $table where $column IN ($ids), in chunks of 500
     * using prepared statements. Table and column names come exclusively from
     * trusted internal callers (never from user input).
     *
     * @param string    $table  Fully-qualified table name (trusted prefix).
     * @param string    $column Column name (trusted literal from caller).
     * @param list<int> $ids    Row ids to delete.
     * @return int Total rows deleted.
     */
    private function deleteByColumn(string $table, string $column, array $ids): int
    {
        if ($ids === []) {
            return 0;
        }
        $deleted = 0;
        foreach (array_chunk($ids, 500) as $chunk) {
            $placeholders = implode(',', array_fill(0, count($chunk), '%d'));
            $deleted += $this->query(
                "DELETE FROM {$table} WHERE {$column} IN ({$placeholders})",
                $chunk
            );
        }
        return $deleted;
    }

    /**
     * Bounded SELECT→DELETE loop (Fix 1 helper).
     *
     * Executes $selectSql (which must end with "LIMIT %d" as the LAST
     * placeholder, bound from $selectArgs) repeatedly, collecting integer
     * primary-key ids, then deletes them via deleteByColumn(). Loops until
     * fewer than DELETE_SELECT_CHUNK ids are returned or DELETE_MAX_ITERATIONS
     * is reached.
     *
     * @param string       $selectSql  Prepared SELECT SQL with LIMIT %d last.
     * @param list<mixed>  $selectArgs Args for $selectSql (LIMIT value last).
     * @param string       $table      Fully-qualified table name for DELETE.
     * @param string       $column     PK column name for DELETE.
     * @return array{int, string} [rows_deleted, detail_note]
     */
    private function batchedSelectDelete(
        string $selectSql,
        array $selectArgs,
        string $table,
        string $column
    ): array {
        $deleted    = 0;
        $iterations = 0;

        while (true) {
            if ($iterations >= self::DELETE_MAX_ITERATIONS) {
                return [$deleted, 'partial: iteration cap reached'];
            }
            $iterations++;

            $ids = $this->ids($selectSql, ...$selectArgs);
            if ($ids === []) {
                break;
            }

            $deleted += $this->deleteByColumn($table, $column, $ids);

            if (count($ids) < self::DELETE_SELECT_CHUNK) {
                break; // last page
            }
        }

        return [$deleted, ''];
    }
}
