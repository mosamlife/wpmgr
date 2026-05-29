<?php
/**
 * ActivityLog (ADR-037 Sprint 3): WordPress activity capture, hash-chained
 * tamper-evidence, and batched shipping to the control plane.
 *
 * WHAT IT DOES
 *   - registerHooks() binds ~30 WordPress action hooks (posts, comments, users,
 *     auth, plugins, themes, core, terms, a security-relevant option allowlist,
 *     and — only when WooCommerce is present — order/product events).
 *   - record() appends a row to `wpmgr_activity_log`, computing a SHA-256 hash
 *     chain so any later tampering with a stored row breaks every subsequent
 *     `this_hash`. Both this agent and the CP compute the chain identically.
 *   - ship() batches up to 200 unshipped rows (seq ASC) and POSTs them to
 *     /agent/v1/activity via the agent's signed-POST helper, marking them
 *     shipped on a 2xx.
 *
 * ┌──────────────────────────────────────────────────────────────────────────┐
 * │ HASH-CHAIN CANONICALIZATION (BYTE-FOR-BYTE — CP MUST MATCH)                │
 * ├──────────────────────────────────────────────────────────────────────────┤
 * │ Genesis prev_hash = str_repeat('0', 64)  (64 ASCII zero chars).           │
 * │                                                                            │
 * │ this_hash = hash('sha256', PREIMAGE) where PREIMAGE is the concatenation   │
 * │ of the following nine fields joined by a single "\n" (0x0A) separator:     │
 * │                                                                            │
 * │   prev_hash . "\n" .                                                       │
 * │   seq . "\n" .              // integer, decimal, no padding                │
 * │   event_type . "\n" .                                                      │
 * │   object_type . "\n" .                                                     │
 * │   object_id . "\n" .                                                       │
 * │   actor_user_id . "\n" .    // integer, decimal, no padding                │
 * │   occurred_at . "\n" .      // RFC3339 UTC string EXACTLY as shipped       │
 * │   wp_json_encode($meta)     // compact JSON, NO flags; empty meta => "{}"  │
 * │                                                                            │
 * │ NOTE the trailing field has NO terminating "\n".                           │
 * │ NOTE empty meta is encoded as the JSON OBJECT "{}" (cast (object) [] /     │
 * │ new \stdClass) — NOT the array literal "[]" that wp_json_encode([]) emits. │
 * │ occurred_at is gmdate('Y-m-d\TH:i:s\Z', $ts) — e.g. 2026-05-29T10:00:00Z.  │
 * └──────────────────────────────────────────────────────────────────────────┘
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Records WordPress activity into wpmgr_activity_log with a SHA-256 hash chain
 * and a batched, signed ship cadence.
 */
final class ActivityLog
{
    /** Unprefixed table name for activity capture. */
    public const TABLE = 'wpmgr_activity_log';

    /** wp-options key for the per-site monotonic sequence counter. */
    public const OPTION_SEQ = 'wpmgr_agent_activity_seq';

    /** Hard row cap; evicts oldest SHIPPED rows on overflow. */
    public const ROW_CAP = 10000;

    /** Max rows shipped per batch (seq ASC). */
    public const SHIP_BATCH = 200;

    /** Genesis prev_hash: 64 ASCII zero chars. */
    public const GENESIS_HASH = '0000000000000000000000000000000000000000000000000000000000000000';

    /**
     * Security-relevant option keys we capture on updated_option. Capturing
     * ALL option writes would flood the table — these are the keys whose change
     * is operationally/security-significant.
     *
     * @var array<int,string>
     */
    public const OPTION_ALLOWLIST = [
        'siteurl',
        'home',
        'template',
        'stylesheet',
        'users_can_register',
        'default_role',
        'admin_email',
        'permalink_structure',
        'blog_public',
        'WPLANG',
    ];

    /**
     * Option keys whose change is HIGH severity (subset of the allowlist).
     *
     * @var array<int,string>
     */
    private const OPTION_HIGH_SEVERITY = [
        'admin_email',
        'siteurl',
        'home',
        'users_can_register',
        'default_role',
    ];

    /** Severity buckets. */
    public const SEV_HIGH = 'high';
    public const SEV_MEDIUM = 'medium';
    public const SEV_LOW = 'low';

    /**
     * Bind all activity hooks. Idempotent within a request (static guard).
     *
     * @return void
     */
    public function registerHooks(): void
    {
        static $bound = false;
        if ($bound) {
            return;
        }
        $bound = true;

        if (!function_exists('add_action')) {
            return;
        }

        // ---- Posts --------------------------------------------------------
        add_action('save_post', [$this, 'onSavePost'], 10, 3);
        add_action('wp_trash_post', [$this, 'onTrashPost'], 10, 1);
        add_action('untrash_post', [$this, 'onUntrashPost'], 10, 1);
        add_action('delete_post', [$this, 'onDeletePost'], 10, 1);

        // ---- Comments -----------------------------------------------------
        add_action('wp_insert_comment', [$this, 'onInsertComment'], 10, 2);
        add_action('edit_comment', [$this, 'onEditComment'], 10, 1);
        add_action('transition_comment_status', [$this, 'onCommentStatus'], 10, 3);

        // ---- Users --------------------------------------------------------
        add_action('user_register', [$this, 'onUserRegister'], 10, 1);
        add_action('profile_update', [$this, 'onProfileUpdate'], 10, 1);
        add_action('delete_user', [$this, 'onDeleteUser'], 10, 1);
        add_action('set_user_role', [$this, 'onSetUserRole'], 10, 3);

        // ---- Auth ---------------------------------------------------------
        add_action('wp_login', [$this, 'onLogin'], 10, 2);
        add_action('wp_login_failed', [$this, 'onLoginFailed'], 10, 1);
        add_action('wp_logout', [$this, 'onLogout'], 10, 0);
        add_action('after_password_reset', [$this, 'onPasswordReset'], 10, 1);

        // ---- Plugins ------------------------------------------------------
        add_action('activated_plugin', [$this, 'onPluginActivated'], 10, 1);
        add_action('deactivated_plugin', [$this, 'onPluginDeactivated'], 10, 1);
        add_action('deleted_plugin', [$this, 'onPluginDeleted'], 10, 2);

        // ---- Themes -------------------------------------------------------
        add_action('switch_theme', [$this, 'onThemeSwitched'], 10, 1);
        add_action('deleted_theme', [$this, 'onThemeDeleted'], 10, 2);

        // ---- Plugin/theme install + update (shared hook) ------------------
        add_action('upgrader_process_complete', [$this, 'onUpgraderComplete'], 10, 2);

        // ---- Core ---------------------------------------------------------
        add_action('_core_updated_successfully', [$this, 'onCoreUpdated'], 10, 1);

        // ---- Terms --------------------------------------------------------
        add_action('created_term', [$this, 'onTermCreated'], 10, 3);
        add_action('edited_term', [$this, 'onTermEdited'], 10, 3);
        add_action('delete_term', [$this, 'onTermDeleted'], 10, 4);

        // ---- Options (allowlisted) ----------------------------------------
        add_action('updated_option', [$this, 'onUpdatedOption'], 10, 3);

        // ---- WooCommerce (only when present) ------------------------------
        if (class_exists('WooCommerce')) {
            add_action('woocommerce_order_status_changed', [$this, 'onWooOrderStatus'], 10, 4);
            add_action('woocommerce_update_product', [$this, 'onWooProductSaved'], 10, 1);
            add_action('woocommerce_new_product', [$this, 'onWooProductSaved'], 10, 1);
        }
    }

    // =========================================================================
    // Hook handlers
    // =========================================================================

    /**
     * save_post: distinguish create vs update via $update. Skips autosaves,
     * revisions, and auto-draft placeholders to avoid noise.
     *
     * @param int          $postId Post ID.
     * @param object|mixed $post   WP_Post.
     * @param bool         $update Whether this is an update.
     * @return void
     */
    public function onSavePost($postId, $post = null, $update = false): void
    {
        $postId = (int) $postId;
        if ($postId <= 0) {
            return;
        }
        if (function_exists('wp_is_post_revision') && wp_is_post_revision($postId)) {
            return;
        }
        if (function_exists('wp_is_post_autosave') && wp_is_post_autosave($postId)) {
            return;
        }
        if (defined('DOING_AUTOSAVE') && constant('DOING_AUTOSAVE')) {
            return;
        }
        $status = is_object($post) && isset($post->post_status) ? (string) $post->post_status : '';
        if ($status === 'auto-draft') {
            return;
        }
        $title = is_object($post) && isset($post->post_title) ? (string) $post->post_title : '';
        $type  = is_object($post) && isset($post->post_type) ? (string) $post->post_type : 'post';
        $event = $update ? 'post.updated' : 'post.created';
        $verb  = $update ? 'Updated' : 'Created';
        $this->record(
            $event,
            'post',
            (string) $postId,
            $title,
            ['post_type' => $type, 'status' => $status, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * wp_trash_post.
     *
     * @param int $postId Post ID.
     * @return void
     */
    public function onTrashPost($postId): void
    {
        $postId = (int) $postId;
        $this->record('post.trashed', 'post', (string) $postId, $this->postTitle($postId), ['severity' => self::SEV_LOW]);
    }

    /**
     * untrash_post.
     *
     * @param int $postId Post ID.
     * @return void
     */
    public function onUntrashPost($postId): void
    {
        $postId = (int) $postId;
        $this->record('post.restored', 'post', (string) $postId, $this->postTitle($postId), ['severity' => self::SEV_LOW]);
    }

    /**
     * delete_post.
     *
     * @param int $postId Post ID.
     * @return void
     */
    public function onDeletePost($postId): void
    {
        $postId = (int) $postId;
        // delete_post fires for revisions/autosaves too; skip those.
        if (function_exists('get_post_type')) {
            $pt = (string) get_post_type($postId);
            if ($pt === 'revision' || $pt === '') {
                return;
            }
        }
        $this->record('post.deleted', 'post', (string) $postId, $this->postTitle($postId), ['severity' => self::SEV_LOW]);
    }

    /**
     * wp_insert_comment.
     *
     * @param int          $commentId Comment ID.
     * @param object|mixed $comment   WP_Comment.
     * @return void
     */
    public function onInsertComment($commentId, $comment = null): void
    {
        $commentId = (int) $commentId;
        $author = is_object($comment) && isset($comment->comment_author) ? (string) $comment->comment_author : '';
        $this->record('comment.created', 'comment', (string) $commentId, $author, ['severity' => self::SEV_LOW]);
    }

    /**
     * edit_comment.
     *
     * @param int $commentId Comment ID.
     * @return void
     */
    public function onEditComment($commentId): void
    {
        $commentId = (int) $commentId;
        $this->record('comment.edited', 'comment', (string) $commentId, '', ['severity' => self::SEV_LOW]);
    }

    /**
     * transition_comment_status.
     *
     * @param string       $new     New status.
     * @param string       $old     Old status.
     * @param object|mixed $comment WP_Comment.
     * @return void
     */
    public function onCommentStatus($new, $old, $comment = null): void
    {
        // Skip no-op transitions WP fires on every insert.
        if ((string) $new === (string) $old) {
            return;
        }
        $commentId = is_object($comment) && isset($comment->comment_ID) ? (int) $comment->comment_ID : 0;
        $this->record(
            'comment.status_changed',
            'comment',
            (string) $commentId,
            '',
            ['from' => (string) $old, 'to' => (string) $new, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * user_register.
     *
     * @param int $userId User ID.
     * @return void
     */
    public function onUserRegister($userId): void
    {
        $userId = (int) $userId;
        $this->record(
            'user.registered',
            'user',
            (string) $userId,
            $this->userLogin($userId),
            ['severity' => self::SEV_MEDIUM]
        );
    }

    /**
     * profile_update.
     *
     * @param int $userId User ID.
     * @return void
     */
    public function onProfileUpdate($userId): void
    {
        $userId = (int) $userId;
        $this->record(
            'user.updated',
            'user',
            (string) $userId,
            $this->userLogin($userId),
            ['severity' => self::SEV_LOW]
        );
    }

    /**
     * delete_user.
     *
     * @param int $userId User ID.
     * @return void
     */
    public function onDeleteUser($userId): void
    {
        $userId = (int) $userId;
        $this->record(
            'user.deleted',
            'user',
            (string) $userId,
            $this->userLogin($userId),
            ['severity' => self::SEV_HIGH]
        );
    }

    /**
     * set_user_role. HIGH severity when the new role is administrator.
     *
     * @param int                 $userId   User ID.
     * @param string              $role     New role.
     * @param array<int,string>|mixed $oldRoles Old roles.
     * @return void
     */
    public function onSetUserRole($userId, $role = '', $oldRoles = []): void
    {
        $userId = (int) $userId;
        $role   = (string) $role;
        // WP fires set_user_role during user_register with the default role;
        // only log genuine role CHANGES (old roles present and differ).
        $old = is_array($oldRoles) ? array_map('strval', $oldRoles) : [];
        if ($old === [] || in_array($role, $old, true)) {
            return;
        }
        $severity = ($role === 'administrator') ? self::SEV_HIGH : self::SEV_MEDIUM;
        $this->record(
            'user.role_changed',
            'user',
            (string) $userId,
            $this->userLogin($userId),
            ['from' => $old, 'to' => $role, 'severity' => $severity]
        );
    }

    /**
     * wp_login.
     *
     * @param string       $login User login.
     * @param object|mixed $user  WP_User.
     * @return void
     */
    public function onLogin($login, $user = null): void
    {
        $userId = is_object($user) && isset($user->ID) ? (int) $user->ID : 0;
        $this->record(
            'user.login',
            'user',
            (string) $userId,
            (string) $login,
            ['severity' => self::SEV_LOW]
        );
    }

    /**
     * wp_login_failed. Tracked + marked (HIGH) so the CP can fire a brute-force
     * alert on a burst.
     *
     * @param string $login Attempted login.
     * @return void
     */
    public function onLoginFailed($login): void
    {
        $this->record(
            'user.login_failed',
            'user',
            '0',
            (string) $login,
            ['severity' => self::SEV_HIGH]
        );
    }

    /**
     * wp_logout.
     *
     * @return void
     */
    public function onLogout(): void
    {
        $userId = function_exists('get_current_user_id') ? (int) get_current_user_id() : 0;
        $this->record(
            'user.logout',
            'user',
            (string) $userId,
            $this->userLogin($userId),
            ['severity' => self::SEV_LOW]
        );
    }

    /**
     * after_password_reset.
     *
     * @param object|mixed $user WP_User.
     * @return void
     */
    public function onPasswordReset($user = null): void
    {
        $userId = is_object($user) && isset($user->ID) ? (int) $user->ID : 0;
        $login  = is_object($user) && isset($user->user_login) ? (string) $user->user_login : $this->userLogin($userId);
        $this->record(
            'user.password_reset',
            'user',
            (string) $userId,
            $login,
            ['severity' => self::SEV_LOW]
        );
    }

    /**
     * activated_plugin.
     *
     * @param string $plugin Plugin file (relative to plugins dir).
     * @return void
     */
    public function onPluginActivated($plugin): void
    {
        $plugin = (string) $plugin;
        $this->record(
            'plugin.activated',
            'plugin',
            $plugin,
            $this->pluginLabel($plugin),
            ['severity' => self::SEV_MEDIUM]
        );
    }

    /**
     * deactivated_plugin.
     *
     * @param string $plugin Plugin file.
     * @return void
     */
    public function onPluginDeactivated($plugin): void
    {
        $plugin = (string) $plugin;
        $this->record(
            'plugin.deactivated',
            'plugin',
            $plugin,
            $this->pluginLabel($plugin),
            ['severity' => self::SEV_MEDIUM]
        );
    }

    /**
     * deleted_plugin.
     *
     * @param string $plugin  Plugin file.
     * @param bool   $deleted Whether deletion succeeded.
     * @return void
     */
    public function onPluginDeleted($plugin, $deleted = true): void
    {
        if ($deleted === false) {
            return;
        }
        $plugin = (string) $plugin;
        $this->record(
            'plugin.deleted',
            'plugin',
            $plugin,
            $this->pluginLabel($plugin),
            ['severity' => self::SEV_HIGH]
        );
    }

    /**
     * switch_theme.
     *
     * @param string $name New theme name.
     * @return void
     */
    public function onThemeSwitched($name): void
    {
        $name = (string) $name;
        $stylesheet = function_exists('get_stylesheet') ? (string) get_stylesheet() : $name;
        $this->record(
            'theme.switched',
            'theme',
            $stylesheet,
            $name,
            ['severity' => self::SEV_MEDIUM]
        );
    }

    /**
     * deleted_theme.
     *
     * @param string $stylesheet Theme stylesheet slug.
     * @param bool   $deleted    Whether deletion succeeded.
     * @return void
     */
    public function onThemeDeleted($stylesheet, $deleted = true): void
    {
        if ($deleted === false) {
            return;
        }
        $this->record(
            'theme.deleted',
            'theme',
            (string) $stylesheet,
            (string) $stylesheet,
            ['severity' => self::SEV_HIGH]
        );
    }

    /**
     * upgrader_process_complete — distinguish install vs update, plugin vs theme.
     *
     * @param object|mixed         $upgrader The WP_Upgrader instance.
     * @param array<string,mixed>|mixed $data Hook extra data (action/type/...).
     * @return void
     */
    public function onUpgraderComplete($upgrader, $data = []): void
    {
        if (!is_array($data)) {
            return;
        }
        $action = (string) ($data['action'] ?? '');
        $type   = (string) ($data['type'] ?? '');

        if ($type !== 'plugin' && $type !== 'theme') {
            return;
        }
        if ($action !== 'install' && $action !== 'update') {
            return;
        }

        $event = $type . '.' . ($action === 'install' ? 'installed' : 'updated');

        // Collect affected slugs (update bulk → 'plugins'/'themes'; install → 'plugin'/'theme').
        $items = [];
        if ($type === 'plugin') {
            if (isset($data['plugins']) && is_array($data['plugins'])) {
                $items = array_map('strval', $data['plugins']);
            } elseif (isset($data['plugin'])) {
                $items = [(string) $data['plugin']];
            }
        } else {
            if (isset($data['themes']) && is_array($data['themes'])) {
                $items = array_map('strval', $data['themes']);
            } elseif (isset($data['theme'])) {
                $items = [(string) $data['theme']];
            }
        }

        if ($items === []) {
            // Install path often lacks the slug; record a single generic event.
            $this->record($event, $type, '', '', ['action' => $action, 'severity' => self::SEV_MEDIUM]);
            return;
        }

        foreach ($items as $slug) {
            $label = $type === 'plugin' ? $this->pluginLabel($slug) : $slug;
            $this->record(
                $event,
                $type,
                $slug,
                $label,
                ['action' => $action, 'severity' => self::SEV_MEDIUM]
            );
        }
    }

    /**
     * _core_updated_successfully.
     *
     * @param string $version New WordPress version.
     * @return void
     */
    public function onCoreUpdated($version): void
    {
        $version = (string) $version;
        $this->record(
            'core.updated',
            'core',
            $version,
            'WordPress ' . $version,
            ['version' => $version, 'severity' => self::SEV_MEDIUM]
        );
    }

    /**
     * created_term.
     *
     * @param int    $termId   Term ID.
     * @param int    $ttId     Term-taxonomy ID.
     * @param string $taxonomy Taxonomy.
     * @return void
     */
    public function onTermCreated($termId, $ttId = 0, $taxonomy = ''): void
    {
        $this->record(
            'term.created',
            'term',
            (string) (int) $termId,
            $this->termName((int) $termId, (string) $taxonomy),
            ['taxonomy' => (string) $taxonomy, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * edited_term.
     *
     * @param int    $termId   Term ID.
     * @param int    $ttId     Term-taxonomy ID.
     * @param string $taxonomy Taxonomy.
     * @return void
     */
    public function onTermEdited($termId, $ttId = 0, $taxonomy = ''): void
    {
        $this->record(
            'term.edited',
            'term',
            (string) (int) $termId,
            $this->termName((int) $termId, (string) $taxonomy),
            ['taxonomy' => (string) $taxonomy, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * delete_term.
     *
     * @param int          $termId   Term ID.
     * @param int          $ttId     Term-taxonomy ID.
     * @param string       $taxonomy Taxonomy.
     * @param object|mixed $deleted  The deleted term object.
     * @return void
     */
    public function onTermDeleted($termId, $ttId = 0, $taxonomy = '', $deleted = null): void
    {
        $name = is_object($deleted) && isset($deleted->name) ? (string) $deleted->name : '';
        $this->record(
            'term.deleted',
            'term',
            (string) (int) $termId,
            $name,
            ['taxonomy' => (string) $taxonomy, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * updated_option — FILTERED to the security-relevant allowlist. Captures
     * the key only (never the value, which may contain secrets).
     *
     * @param string $option Option name.
     * @param mixed  $old    Old value.
     * @param mixed  $new    New value.
     * @return void
     */
    public function onUpdatedOption($option, $old = null, $new = null): void
    {
        $option = (string) $option;
        if (!in_array($option, self::OPTION_ALLOWLIST, true)) {
            return;
        }
        $severity = in_array($option, self::OPTION_HIGH_SEVERITY, true) ? self::SEV_HIGH : self::SEV_MEDIUM;
        $this->record(
            'option.updated',
            'option',
            $option,
            $option,
            ['severity' => $severity]
        );
    }

    /**
     * woocommerce_order_status_changed.
     *
     * @param int          $orderId Order ID.
     * @param string       $from    Old status.
     * @param string       $to      New status.
     * @param object|mixed $order   WC_Order.
     * @return void
     */
    public function onWooOrderStatus($orderId, $from = '', $to = '', $order = null): void
    {
        $orderId = (int) $orderId;
        $this->record(
            'woocommerce.order_status_changed',
            'woocommerce',
            (string) $orderId,
            'Order #' . $orderId,
            ['from' => (string) $from, 'to' => (string) $to, 'severity' => self::SEV_LOW]
        );
    }

    /**
     * woocommerce_update_product / woocommerce_new_product.
     *
     * @param int $productId Product ID.
     * @return void
     */
    public function onWooProductSaved($productId): void
    {
        $productId = (int) $productId;
        $this->record(
            'woocommerce.product_saved',
            'woocommerce',
            (string) $productId,
            $this->postTitle($productId),
            ['severity' => self::SEV_LOW]
        );
    }

    // =========================================================================
    // Recorder core
    // =========================================================================

    /**
     * Record one activity event: bump seq, link the hash chain, insert, evict.
     *
     * @param string               $eventType   e.g. 'plugin.activated'.
     * @param string               $objectType  e.g. 'plugin'.
     * @param string               $objectId    Stable object identifier.
     * @param string               $objectLabel Human label.
     * @param array<string,mixed>  $meta        Extra context (carries severity).
     * @return void
     */
    public function record(string $eventType, string $objectType, string $objectId, string $objectLabel, array $meta = []): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }

        try {
            $table = $wpdb->prefix . self::TABLE;

            // Single timestamp so the hashed RFC3339 string and the stored
            // DATETIME (reconstructed on ship) are byte-identical — they must
            // not straddle a second boundary across two gmdate() calls.
            $ts = time();
            // RFC3339 UTC, exactly as shipped + hashed.
            $occurredAt   = gmdate('Y-m-d\TH:i:s\Z', $ts);
            $occurredAtDb = gmdate('Y-m-d H:i:s', $ts);

            $actorUserId = function_exists('get_current_user_id') ? (int) get_current_user_id() : 0;
            $actorLogin  = $this->userLogin($actorUserId);
            $actorIp     = $this->resolveActorIp();

            $summary = $this->summarize($eventType, $objectType, $objectLabel, $objectId);

            // Atomically bump the monotonic per-site sequence counter.
            $seq = $this->nextSeq();

            $prevHash = $this->lastHash();
            $thisHash = self::computeHash($prevHash, $seq, $eventType, $objectType, $objectId, $actorUserId, $occurredAt, $meta);

            // For storage we keep meta as the canonical JSON we hashed so the
            // CP can re-derive the same preimage from the persisted/shipped row.
            $metaJson = self::encodeMeta($meta);

            $wpdb->insert(
                $table,
                [
                    'seq'           => $seq,
                    'event_type'    => substr($eventType, 0, 64),
                    'object_type'   => substr($objectType, 0, 32),
                    'object_id'     => substr($objectId, 0, 255),
                    'object_label'  => substr($objectLabel, 0, 255),
                    'actor_user_id' => $actorUserId,
                    'actor_login'   => substr($actorLogin, 0, 191),
                    'actor_ip'      => substr($actorIp, 0, 64),
                    'summary'       => substr($summary, 0, 255),
                    'meta'          => $metaJson,
                    'prev_hash'     => $prevHash,
                    'this_hash'     => $thisHash,
                    'occurred_at'   => $occurredAtDb,
                    'shipped'       => 0,
                ],
                ['%d', '%s', '%s', '%s', '%s', '%d', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%d']
            );

            $this->enforceRowCap();
        } catch (\Throwable $e) {
            // Never let activity capture fatal the request that triggered it.
        }
    }

    /**
     * Compute the chain hash per the canonicalization documented in the class
     * docblock. PUBLIC + static so the CP-side test fixtures and any agent
     * verifier can call it identically.
     *
     * @param string              $prevHash    Previous row's this_hash (or genesis).
     * @param int                 $seq         Monotonic sequence.
     * @param string              $eventType   Event type.
     * @param string              $objectType  Object type.
     * @param string              $objectId    Object id.
     * @param int                 $actorUserId Actor user id.
     * @param string              $occurredAt  RFC3339 UTC string as shipped.
     * @param array<string,mixed> $meta        Meta map.
     * @return string Lowercase hex SHA-256.
     */
    public static function computeHash(
        string $prevHash,
        int $seq,
        string $eventType,
        string $objectType,
        string $objectId,
        int $actorUserId,
        string $occurredAt,
        array $meta
    ): string {
        $preimage = $prevHash . "\n"
            . $seq . "\n"
            . $eventType . "\n"
            . $objectType . "\n"
            . $objectId . "\n"
            . $actorUserId . "\n"
            . $occurredAt . "\n"
            . self::encodeMeta($meta);

        return hash('sha256', $preimage);
    }

    /**
     * Canonical compact JSON for meta. Empty meta encodes to the JSON OBJECT
     * "{}" (NOT the array literal "[]"). Mirrors the wire contract exactly.
     *
     * @param array<string,mixed> $meta Meta map.
     * @return string Compact JSON.
     */
    public static function encodeMeta(array $meta): string
    {
        if ($meta === []) {
            $value = new \stdClass();
        } else {
            $value = $meta;
        }
        if (function_exists('wp_json_encode')) {
            $json = wp_json_encode($value);
        } else {
            $json = json_encode($value);
        }
        return is_string($json) ? $json : '{}';
    }

    /**
     * Atomically increment the per-site sequence counter and return the new
     * value. Uses a single UPDATE … = +1 on the autoload option row when
     * available; falls back to a guarded get/update.
     *
     * @return int The new sequence number for this event.
     */
    private function nextSeq(): int
    {
        global $wpdb;
        // Fast atomic path: a single SQL increment on the option value avoids
        // a read-modify-write race when two hooks fire in concurrent requests.
        if (is_object($wpdb) && function_exists('get_option')) {
            $optTable = $wpdb->options;
            $name     = self::OPTION_SEQ;
            // Ensure the row exists (autoload no, so it isn't loaded every request).
            if (get_option($name, null) === null && function_exists('add_option')) {
                add_option($name, '0', '', 'no');
            }
            $updated = $wpdb->query(
                $wpdb->prepare(
                    "UPDATE {$optTable} SET option_value = option_value + 1 WHERE option_name = %s",
                    $name
                )
            );
            if (is_int($updated) && $updated > 0) {
                $val = $wpdb->get_var(
                    $wpdb->prepare("SELECT option_value FROM {$optTable} WHERE option_name = %s", $name)
                );
                // Bust the option cache so subsequent get_option sees the new value.
                if (function_exists('wp_cache_delete')) {
                    wp_cache_delete($name, 'options');
                }
                return (int) $val;
            }
        }

        // Fallback (non-WP or no $wpdb): best-effort get/update.
        $current = function_exists('get_option') ? (int) get_option(self::OPTION_SEQ, 0) : 0;
        $next    = $current + 1;
        if (function_exists('update_option')) {
            update_option(self::OPTION_SEQ, (string) $next, false);
        }
        return $next;
    }

    /**
     * Fetch the most recent row's this_hash, or the genesis hash if the table
     * is empty (ordered by seq DESC so it tracks the chain head, not insert id).
     *
     * @return string 64-char hex hash.
     */
    private function lastHash(): string
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return self::GENESIS_HASH;
        }
        $table = $wpdb->prefix . self::TABLE;
        $hash = $wpdb->get_var("SELECT this_hash FROM {$table} ORDER BY seq DESC LIMIT 1");
        if (is_string($hash) && strlen($hash) === 64) {
            return $hash;
        }
        return self::GENESIS_HASH;
    }

    /**
     * Enforce the 10k row cap by evicting the oldest SHIPPED rows first (so we
     * never drop a row that hasn't reached the CP). Mirrors the error-monitor
     * cap; bounds delete batches to keep the work small.
     *
     * @return void
     */
    private function enforceRowCap(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        $count = (int) $wpdb->get_var("SELECT COUNT(*) FROM {$table}");
        if ($count <= self::ROW_CAP) {
            return;
        }
        $overflow = $count - self::ROW_CAP;
        $batch    = min($overflow, 500);
        // Oldest SHIPPED first (shipped=1 ordered by id ASC). If somehow all
        // rows are unshipped and we are over cap, fall back to oldest overall
        // to bound table growth (rare; means the CP has been unreachable a
        // very long time).
        $deleted = $wpdb->query(
            $wpdb->prepare(
                "DELETE FROM {$table} WHERE shipped = 1 ORDER BY id ASC LIMIT %d",
                $batch
            )
        );
        if ((!is_int($deleted) || $deleted === 0)) {
            $wpdb->query(
                $wpdb->prepare(
                    "DELETE FROM {$table} ORDER BY id ASC LIMIT %d",
                    $batch
                )
            );
        }
    }

    // =========================================================================
    // Shipper
    // =========================================================================

    /**
     * Build the batch of unshipped rows the caller ships to /agent/v1/activity.
     * Returns up to SHIP_BATCH rows ordered by seq ASC, decoded into the wire
     * shape. The caller (Plugin) does the signed POST and, on 2xx, calls
     * markShipped() with the seq values it sent.
     *
     * @return array<int,array<string,mixed>>
     */
    public function pendingBatch(): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return [];
        }
        $table = $wpdb->prefix . self::TABLE;
        $rows = $wpdb->get_results(
            $wpdb->prepare(
                "SELECT seq, event_type, object_type, object_id, object_label,
                        actor_user_id, actor_login, actor_ip, summary, meta,
                        prev_hash, this_hash, occurred_at
                 FROM {$table}
                 WHERE shipped = 0
                 ORDER BY seq ASC
                 LIMIT %d",
                self::SHIP_BATCH
            ),
            ARRAY_A
        );
        if (!is_array($rows)) {
            return [];
        }

        $events = [];
        foreach ($rows as $row) {
            $metaRaw = (string) ($row['meta'] ?? '{}');
            $meta = json_decode($metaRaw, true);
            if (!is_array($meta)) {
                $meta = [];
            }
            $events[] = [
                'seq'           => (int) $row['seq'],
                'event_type'    => (string) $row['event_type'],
                'object_type'   => (string) $row['object_type'],
                'object_id'     => (string) $row['object_id'],
                'object_label'  => (string) $row['object_label'],
                'actor_user_id' => (int) $row['actor_user_id'],
                'actor_login'   => (string) $row['actor_login'],
                'actor_ip'      => (string) $row['actor_ip'],
                'summary'       => (string) $row['summary'],
                // Re-emit meta as a stdClass when empty so the wire payload and
                // the CP's recomputed preimage both see "{}", not "[]".
                'meta'          => ($meta === []) ? new \stdClass() : $meta,
                'prev_hash'     => (string) $row['prev_hash'],
                'this_hash'     => (string) $row['this_hash'],
                // occurred_at stored as DATETIME; re-emit as the RFC3339 string
                // that was hashed (UTC Z suffix).
                'occurred_at'   => $this->datetimeToRfc3339((string) $row['occurred_at']),
            ];
        }
        return $events;
    }

    /**
     * Mark rows shipped after a 2xx from the CP.
     *
     * @param array<int,int> $seqs Seq values confirmed shipped.
     * @return void
     */
    public function markShipped(array $seqs): void
    {
        global $wpdb;
        if (!is_object($wpdb) || $seqs === []) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        $ints = array_map('intval', $seqs);
        $placeholders = implode(',', array_fill(0, count($ints), '%d'));
        $sql = "UPDATE {$table} SET shipped = 1 WHERE seq IN ({$placeholders})";
        $wpdb->query($wpdb->prepare($sql, ...$ints));
    }

    /**
     * Ship a batch of unshipped rows. Builds the batch, hands the signed POST
     * to $poster, and marks rows shipped on a 2xx. $poster mirrors
     * Plugin::shipPayload's contract: fn(string $path, array $payload):
     * array{ok:bool,status:int}.
     *
     * @param callable(string,array<string,mixed>):array{ok:bool,status:int} $poster Signed-POST helper.
     * @param string $agentVersion Agent version string for the payload.
     * @return array{shipped:int,status:int} Count shipped + last HTTP status.
     */
    public function ship(callable $poster, string $agentVersion): array
    {
        $events = $this->pendingBatch();
        if ($events === []) {
            return ['shipped' => 0, 'status' => 0];
        }

        $chainStart = (int) ($events[0]['seq'] ?? 0);
        $payload = [
            'events'          => $events,
            'chain_start_seq' => $chainStart,
            'agent_version'   => $agentVersion,
        ];

        $result = $poster('/agent/v1/activity', $payload);
        if (is_array($result) && ($result['ok'] ?? false)) {
            $seqs = array_map(static fn ($e): int => (int) $e['seq'], $events);
            $this->markShipped($seqs);
            return ['shipped' => count($seqs), 'status' => (int) ($result['status'] ?? 0)];
        }
        return ['shipped' => 0, 'status' => is_array($result) ? (int) ($result['status'] ?? 0) : 0];
    }

    // =========================================================================
    // Helpers
    // =========================================================================

    /**
     * Resolve the best-effort actor IP. Order: X-Forwarded-For (first hop) →
     * X-Real-IP → REMOTE_ADDR. Every candidate is validated with
     * FILTER_VALIDATE_IP; an unvalidated header value is NEVER stored.
     *
     * @return string A validated IP or '' if none.
     */
    private function resolveActorIp(): string
    {
        $candidates = [];

        if (isset($_SERVER['HTTP_X_FORWARDED_FOR']) && is_string($_SERVER['HTTP_X_FORWARDED_FOR'])) {
            // First hop is the original client; the rest are proxies.
            $parts = explode(',', (string) $_SERVER['HTTP_X_FORWARDED_FOR']);
            $candidates[] = trim((string) ($parts[0] ?? ''));
        }
        if (isset($_SERVER['HTTP_X_REAL_IP']) && is_string($_SERVER['HTTP_X_REAL_IP'])) {
            $candidates[] = trim((string) $_SERVER['HTTP_X_REAL_IP']);
        }
        if (isset($_SERVER['REMOTE_ADDR']) && is_string($_SERVER['REMOTE_ADDR'])) {
            $candidates[] = trim((string) $_SERVER['REMOTE_ADDR']);
        }

        foreach ($candidates as $ip) {
            if ($ip !== '' && filter_var($ip, FILTER_VALIDATE_IP) !== false) {
                return $ip;
            }
        }
        return '';
    }

    /**
     * Resolve a user's login by id (best effort).
     *
     * @param int $userId User ID.
     * @return string Login or ''.
     */
    private function userLogin(int $userId): string
    {
        if ($userId <= 0 || !function_exists('get_userdata')) {
            return '';
        }
        $user = get_userdata($userId);
        if (is_object($user) && isset($user->user_login)) {
            return (string) $user->user_login;
        }
        return '';
    }

    /**
     * Best-effort post title.
     *
     * @param int $postId Post ID.
     * @return string
     */
    private function postTitle(int $postId): string
    {
        if ($postId <= 0 || !function_exists('get_the_title')) {
            return '';
        }
        $title = (string) get_the_title($postId);
        return $title;
    }

    /**
     * Best-effort plugin label from its header data.
     *
     * @param string $pluginFile Plugin file relative to plugins dir.
     * @return string
     */
    private function pluginLabel(string $pluginFile): string
    {
        if ($pluginFile === '') {
            return '';
        }
        if (function_exists('get_plugin_data') && defined('WP_PLUGIN_DIR')) {
            $path = constant('WP_PLUGIN_DIR') . '/' . $pluginFile;
            if (is_file($path)) {
                $data = @get_plugin_data($path, false, false);
                if (is_array($data) && !empty($data['Name'])) {
                    return (string) $data['Name'];
                }
            }
        }
        // Fall back to the slug (dir or basename).
        $dir = strpos($pluginFile, '/') !== false ? substr($pluginFile, 0, strpos($pluginFile, '/')) : $pluginFile;
        return $dir;
    }

    /**
     * Best-effort term name.
     *
     * @param int    $termId   Term ID.
     * @param string $taxonomy Taxonomy.
     * @return string
     */
    private function termName(int $termId, string $taxonomy): string
    {
        if ($termId <= 0 || !function_exists('get_term')) {
            return '';
        }
        $term = get_term($termId, $taxonomy);
        if (is_object($term) && isset($term->name)) {
            return (string) $term->name;
        }
        return '';
    }

    /**
     * Build a short human summary for the row.
     *
     * @param string $eventType   Event type.
     * @param string $objectType  Object type.
     * @param string $objectLabel Object label.
     * @param string $objectId    Object id.
     * @return string
     */
    private function summarize(string $eventType, string $objectType, string $objectLabel, string $objectId): string
    {
        $what = $objectLabel !== '' ? $objectLabel : ($objectId !== '' ? $objectId : $objectType);
        // e.g. "plugin.activated" → "Plugin activated: Akismet Anti-Spam"
        $verb = str_replace(['.', '_'], [' ', ' '], $eventType);
        return ucfirst($verb) . ($what !== '' ? ': ' . $what : '');
    }

    /**
     * Convert a stored DATETIME ('Y-m-d H:i:s', UTC) back to the RFC3339 UTC
     * string that was hashed. The stored DATETIME is always UTC (we write
     * gmdate), so we just swap the space for 'T' and append 'Z'.
     *
     * @param string $datetime Stored DATETIME.
     * @return string RFC3339 UTC string.
     */
    private function datetimeToRfc3339(string $datetime): string
    {
        $datetime = trim($datetime);
        if ($datetime === '') {
            return gmdate('Y-m-d\TH:i:s\Z');
        }
        return str_replace(' ', 'T', $datetime) . 'Z';
    }
}
