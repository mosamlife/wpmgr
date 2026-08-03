<?php
/**
 * UpdateOutcome: decides, from a failed WordPress upgrade alone, whether core
 * can possibly have touched the destination directory yet (GitHub issue #328).
 *
 * THE FULL RATIONALE, INCLUDING THE TWO-TABLE BOUNDARY RULE AND THE STANDING
 * PROHIBITION ON wp_doing_cron(), IS BELOW THE DIRECT-ACCESS GUARD, not above
 * it. Plugin Check's direct-file-access detector only reads the first 50 raw
 * lines of a file when looking for the guard, so a long header comment above it
 * silently turns into a shipping-blocking ERROR. Keep the guard high and the
 * reasoning underneath.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * UpdateOutcome: decides, from a failed WordPress upgrade alone, whether core
 * can possibly have touched the destination directory yet (GitHub issue #328).
 *
 * WHY. When an agent-driven plugin/theme update fails, UpdateCommand restores
 * the pre-update snapshot over the live directory. That restore is itself a
 * non-atomic rename-plus-recursive-copy on a live site, so performing one over
 * a directory core never opened is pure added risk: it can only make a healthy
 * directory worse. Most upgrade failures happen BEFORE core touches the
 * destination at all (a download failure, a corrupt archive, a full disk while
 * unzipping into wp-content/upgrade/), and for those the correct action is to
 * report the failure and change nothing.
 *
 * THE BOUNDARY, read out of the bundled WordPress 7.0.2 tree
 * (wp-includes/version.php:19). WP_Upgrader::install_package() first alters the
 * destination at wp-admin/includes/class-wp-upgrader.php:607 to :608, where
 * move_to_temp_backup_dir() MOVES the live directory away. Plugin_Upgrader
 * always populates hook_extra['temp_backup'] (class-plugin-upgrader.php:234 to
 * :238), as does Theme_Upgrader (class-theme-upgrader.php:334 to :338), so
 * :607's `if ( ! empty( ... ) )` is always true on our path. Every WP_Error
 * core can return ABOVE that line leaves the destination untouched; everything
 * at or after it may not.
 *
 * TWO TABLES, ONE RULE. UNTOUCHED_CODES is exactly the set of codes reachable
 * above :608. TOUCHED_CODES is exactly the set reachable at or after it. A code
 * in NEITHER table (or a result shape we cannot read) yields null, and null
 * always means "restore, exactly as before this class existed". This release is
 * therefore a strict NARROWING of the restore set and can never widen it.
 *
 * THE ALLOWLIST CANNOT BE PROVEN COMPLETE, ONLY AUDITED. It is complete against
 * WordPress 7.0.2 as read in this pass. A future core release that adds an
 * error return above class-wp-upgrader.php:608, or that moves the
 * move_to_temp_backup_dir() call, changes the answer silently. Drift fails safe
 * (an unrecognised code goes to null and restores) and the per-code tests make
 * a table edit deliberate. STANDING INSTRUCTION, which lives here rather than
 * in anyone's memory: re-read install_package() on every WordPress major.
 *
 * STANDING PROHIBITION. Nothing in this class, or in anything that consumes it,
 * may set wp_doing_cron() true in order to reach core's cron-gated
 * disk-space precheck (wp-admin/includes/file.php:1728 and :1916). Pretending
 * to be cron inside a REST request re-enables WordPress's own background
 * updater on a path where we are already holding the site's update lock:
 * wp-includes/update.php:331 fires do_action('wp_maybe_auto_update') and :885
 * to :891 constructs WP_Automatic_Updater and runs it, and wp_version_check()
 * is reachable inside our own window. If disk figures are ever wanted, the
 * pre_unzip_file filter at file.php:1786 is the seam. Pinned by a test.
 */

/**
 * Pure classifier over an upgrader result plus its skin's collected errors.
 */
final class UpdateOutcome
{
    /**
     * Agent-set code for the one non-WP_Error failure shape core has: an
     * upgrade() that short-circuits because WordPress has no update entry for
     * the target. See classify()'s FACT 1.
     */
    public const CODE_NO_UPDATE_OFFERED = 'wpmgr_no_update_offered';

    /** Agent-set code for a WP core error code that fails our own charset check. */
    public const CODE_UNRECOGNIZED = 'wpmgr_unrecognized_code';

    /**
     * Every WP_Error code core can return with the destination directory still
     * completely untouched, i.e. from a point ABOVE
     * class-wp-upgrader.php:607 to :608 (see the class doc for the boundary).
     *
     * Grouped by the function that emits it, with the line it is emitted from
     * in the bundled WordPress 7.0.2 tree:
     *   WP_Upgrader::fs_connect()      :255 fs_unavailable, :259 fs_error,
     *                                  :266 fs_no_root_dir, :271 fs_no_content_dir,
     *                                  :276 fs_no_plugins_dir, :281 fs_no_themes_dir,
     *                                  :286 fs_no_folder
     *   WP_Upgrader::download_package():332 no_package, :340 download_failed.
     *                                  run() calls download_package($package, false, ...)
     *                                  at :849, so the signature soft-fail branch at
     *                                  :855 is unreachable for us and download_failed
     *                                  absorbs every download_url() code.
     *   WP_Upgrader::unpack_package()  :364 fs_no_content_dir, :396 incompatible_archive
     *                                  (a rewrap of unzip_file's own code)
     *   unzip_file(), which extracts into wp-content/upgrade/ and never into the
     *   destination: file.php:1607 fs_unavailable, :1685/:1883 incompatible_archive,
     *   :1695/:1800 stat_failed_ziparchive, :1771 mkdir_failed_ziparchive,
     *   :1820 extract_failed_ziparchive, :1825 copy_failed_ziparchive,
     *   :1887 empty_archive_pclzip, :1957 mkdir_failed_pclzip,
     *   :1984 copy_failed_pclzip, :1733/:1920 disk_full_unzip_file (cron-gated at
     *   :1728 and :1915 so unreachable for us; allowlisted harmlessly).
     *   WP_Upgrader::install_package() above the boundary: :569 source_read_failed,
     *   :581 incompatible_archive_empty, and everything the
     *   `upgrader_source_selection` filter can return at :601 to :604, which for our
     *   two upgraders is Plugin_Upgrader::check_package()
     *   (class-plugin-upgrader.php:490 incompatible_archive_no_plugins,
     *   :504 incompatible_php_required_version, :515 incompatible_wp_required_version)
     *   and Theme_Upgrader::check_package() (class-theme-upgrader.php:580
     *   incompatible_archive_theme_no_style, :605 incompatible_archive_theme_no_name,
     *   :627 incompatible_archive_theme_no_index, :653/:663 the two compatibility codes).
     *   WP_Upgrader::move_to_temp_backup_dir() ABOVE its own move_dir() at :1170:
     *   :1142 fs_no_content_dir, :1156 fs_temp_backup_mkdir.
     *
     * DELIBERATELY ABSENT, each for a reason a future reader must not re-litigate:
     *   `up_to_date` is not a skin CODE at all. class-plugin-upgrader.php:203
     *   passes the STRING and WP_Ajax_Upgrader_Skin::error() rewrites string errors
     *   to unknown_upgrade_error_N; that path is covered by classify()'s FACT 1.
     *   `bad_request` is in NEITHER table because it is emitted on BOTH sides of
     *   the boundary: class-wp-upgrader.php:541 (argument validation, above),
     *   class-plugin-upgrader.php:575 (deactivate_plugin_before_upgrade, above) and
     *   class-plugin-upgrader.php:684 (delete_old_plugin, hooked on
     *   upgrader_clear_destination, BELOW). One code, both sides, so it is
     *   unclassifiable by code alone and correctly resolves to null.
     *
     * @var array<int,string>
     */
    public const UNTOUCHED_CODES = [
        // fs_connect()
        'fs_unavailable',
        'fs_error',
        'fs_no_root_dir',
        'fs_no_content_dir',
        'fs_no_plugins_dir',
        'fs_no_themes_dir',
        'fs_no_folder',
        // download_package()
        'no_package',
        'download_failed',
        // unpack_package() + unzip_file(), all into wp-content/upgrade/
        'incompatible_archive',
        'stat_failed_ziparchive',
        'mkdir_failed_ziparchive',
        'extract_failed_ziparchive',
        'copy_failed_ziparchive',
        'empty_archive_pclzip',
        'mkdir_failed_pclzip',
        'copy_failed_pclzip',
        'disk_full_unzip_file',
        // install_package(), above the boundary
        'source_read_failed',
        'incompatible_archive_empty',
        'incompatible_archive_no_plugins',
        'incompatible_archive_theme_no_style',
        'incompatible_archive_theme_no_name',
        'incompatible_archive_theme_no_index',
        'incompatible_php_required_version',
        'incompatible_wp_required_version',
        // move_to_temp_backup_dir(), above its own move_dir()
        'fs_temp_backup_mkdir',
    ];

    /**
     * Every WP_Error code core can return once the destination directory may
     * already have been altered, i.e. from class-wp-upgrader.php:608 onward.
     *
     *   :1172 fs_temp_backup_move  (move_to_temp_backup_dir's own move_dir at :1170
     *                               has already run by then)
     *   :622  new_source_read_failed
     *   :676  folder_exists
     *   :699  mkdir_failed_destination
     *   class-plugin-upgrader.php:705 remove_old_failed (delete_old_plugin, on
     *         upgrader_clear_destination, which fires at :663 i.e. after :608)
     *   move_dir(): file.php:2099 source_destination_same_move_dir,
     *         :2104 destination_already_exists_move_dir,
     *         :2107 destination_not_deleted_move_dir, :2133 mkdir_failed_move_dir
     *   copy_dir(): file.php:2017 dirlist_failed_copy_dir,
     *         :2024 mkdir_destination_failed_copy_dir, :2042 copy_failed_copy_dir,
     *         :2050 mkdir_failed_copy_dir
     *   files_not_writable is emitted only by the language pack upgrader
     *         (class-language-pack-upgrader.php:476), which the agent never drives.
     *         Listed anyway because the conservative side of an unreachable code is
     *         "restore", and a future core that reused the name would land safely.
     *
     * fs_temp_backup_move stays here even though a directory move is arguably
     * atomic: it is the FIRST code at or after the boundary, and "the allowlist
     * is exactly what returns above :608" is a rule auditable in one read of one
     * function. The cost of the conservative choice is a restore that was
     * probably unnecessary, which is the safe direction.
     *
     * @var array<int,string>
     */
    public const TOUCHED_CODES = [
        'fs_temp_backup_move',
        'new_source_read_failed',
        'folder_exists',
        'mkdir_failed_destination',
        'remove_old_failed',
        'files_not_writable',
        'source_destination_same_move_dir',
        'destination_already_exists_move_dir',
        'destination_not_deleted_move_dir',
        'mkdir_failed_move_dir',
        'dirlist_failed_copy_dir',
        'mkdir_destination_failed_copy_dir',
        'copy_failed_copy_dir',
        'mkdir_failed_copy_dir',
    ];

    /**
     * Operator-facing stage LABEL per code. A SEPARATE function of the code
     * from destination_touched, and never the safety predicate.
     *
     * Pinned disagreement between the two, which is the whole reason they are
     * separate tables: fs_temp_backup_mkdir is stage `install` but touched
     * FALSE (it returns at class-wp-upgrader.php:1156, BEFORE the move_dir at
     * :1170), while fs_temp_backup_move is stage `install` and touched TRUE
     * (:1172, after it). Deriving either table from the other is wrong for
     * exactly these two codes, and a test pins that.
     *
     * fs_no_content_dir is labelled `preflight` even though it has three
     * emission sites (fs_connect at :271, unpack_package at :364,
     * move_to_temp_backup_dir at :1142) and is indistinguishable between them by
     * code alone. The label is a hint for a human; the safety verdict for that
     * code is decided by UNTOUCHED_CODES, where all three sites agree.
     *
     * @var array<string,string>
     */
    public const STAGE_BY_CODE = [
        'fs_unavailable'                      => 'preflight',
        'fs_error'                            => 'preflight',
        'fs_no_root_dir'                      => 'preflight',
        'fs_no_content_dir'                   => 'preflight',
        'fs_no_plugins_dir'                   => 'preflight',
        'fs_no_themes_dir'                    => 'preflight',
        'fs_no_folder'                        => 'preflight',
        'no_package'                          => 'download',
        'download_failed'                     => 'download',
        'incompatible_archive'                => 'unpack',
        'stat_failed_ziparchive'              => 'unpack',
        'mkdir_failed_ziparchive'             => 'unpack',
        'extract_failed_ziparchive'           => 'unpack',
        'copy_failed_ziparchive'              => 'unpack',
        'empty_archive_pclzip'                => 'unpack',
        'mkdir_failed_pclzip'                 => 'unpack',
        'copy_failed_pclzip'                  => 'unpack',
        'disk_full_unzip_file'                => 'unpack',
        'source_read_failed'                  => 'install',
        'incompatible_archive_empty'          => 'install',
        'incompatible_archive_no_plugins'     => 'install',
        'incompatible_archive_theme_no_style' => 'install',
        'incompatible_archive_theme_no_name'  => 'install',
        'incompatible_archive_theme_no_index' => 'install',
        'incompatible_php_required_version'   => 'install',
        'incompatible_wp_required_version'    => 'install',
        'fs_temp_backup_mkdir'                => 'install',
        'fs_temp_backup_move'                 => 'install',
        'new_source_read_failed'              => 'install',
        'folder_exists'                       => 'install',
        'mkdir_failed_destination'            => 'install',
        'remove_old_failed'                   => 'install',
        'files_not_writable'                  => 'install',
        'source_destination_same_move_dir'    => 'install',
        'destination_already_exists_move_dir' => 'install',
        'destination_not_deleted_move_dir'    => 'install',
        'mkdir_failed_move_dir'               => 'install',
        'dirlist_failed_copy_dir'             => 'install',
        'mkdir_destination_failed_copy_dir'   => 'install',
        'copy_failed_copy_dir'                => 'install',
        'mkdir_failed_copy_dir'               => 'install',
    ];

    /**
     * Codes that arrive AT OR AFTER the `upgrader_pre_install` filter
     * (class-wp-upgrader.php:556), which is where
     * Plugin_Upgrader::deactivate_plugin_before_upgrade() is hooked (registered
     * at class-plugin-upgrader.php:211, priority 10) and silently deactivates an
     * active plugin at :578 to :580 whenever wp_doing_cron() is false, i.e.
     * always on our path. Core does NOT put it back: active_after is cron-gated
     * at :639, and UpdateRunner only reactivates on a successful outcome.
     *
     * So a "clean" skipped restore can leave the directory byte-identical and
     * the plugin OFF unless the caller reactivates. That is what this third
     * output exists to tell it.
     *
     * fs_no_content_dir is in this set on the conservative side: it is emitted
     * both before (:364) and after (:1142) the filter and cannot be told apart
     * by code alone. Every download and unpack code is absent, because run()
     * returns at :888 to :894 before install_package() is even called at :898.
     *
     * @var array<int,string>
     */
    public const DEACTIVATION_POSSIBLE_CODES = [
        'fs_no_content_dir',
        'source_read_failed',
        'incompatible_archive_empty',
        'incompatible_archive_no_plugins',
        'incompatible_archive_theme_no_style',
        'incompatible_archive_theme_no_name',
        'incompatible_archive_theme_no_index',
        'incompatible_php_required_version',
        'incompatible_wp_required_version',
        'fs_temp_backup_mkdir',
        'fs_temp_backup_move',
        'new_source_read_failed',
        'folder_exists',
        'mkdir_failed_destination',
        'remove_old_failed',
        'files_not_writable',
        'bad_request',
        'source_destination_same_move_dir',
        'destination_already_exists_move_dir',
        'destination_not_deleted_move_dir',
        'mkdir_failed_move_dir',
        'dirlist_failed_copy_dir',
        'mkdir_destination_failed_copy_dir',
        'copy_failed_copy_dir',
        'mkdir_failed_copy_dir',
    ];

    /** Hard cap on the error data string carried into an operator-visible log. */
    private const MAX_DATA_CHARS = 200;

    /**
     * Classify a finished upgrade.
     *
     * Resolution order, FIRST MATCH WINS.
     *
     * FACT 1, and it MUST be first. `$result === false` by IDENTITY (not `==`,
     * not "falsy"). Plugin_Upgrader::upgrade() returns literal false at
     * class-plugin-upgrader.php:205 inside the `! isset( $current->response[
     * $plugin ] )` short circuit at :200 to :206, BEFORE run() at :224; the
     * Theme_Upgrader twin is :306 to :312 before :324. Nothing ran, so nothing
     * was touched. Without this rule a benign race (the control plane says an
     * update is pending, WordPress's own transient disagrees because a sibling
     * request just cleared it) lands unclassified and triggers a SPURIOUS
     * restore. `$result === 0`, `''`, `[]` and null must NOT match.
     *
     * The identity rule is sound only because Plugin_Upgrader::upgrade()
     * DISCARDS run()'s return at :224 and returns $this->result at :250 to
     * :251, and $this->result is shadowed as an uninitialised property at
     * class-plugin-upgrader.php:31 (parent default array() at
     * class-wp-upgrader.php:93). So run()'s own second `return false` at
     * class-wp-upgrader.php:831 (fs_connect not connected, which emits no skin
     * error) can never reach us AS false; it reaches us as null, which falls
     * through to FACT 3 and FACT 4.
     *
     * FACT 2. $result is a WP_Error: take its own code and data. This is the
     * Core_Upgrader shape, which returns install_package()'s WP_Error directly.
     *
     * FACT 3. Otherwise read the upgrader skin's collected errors
     * (WP_Ajax_Upgrader_Skin::get_errors(), populated by run() at :876, :889 and
     * :938). This is the plugin/theme shape: a pre-install failure leaves
     * $this->result unset, so upgrade() returns NULL and the codes live only on
     * the skin.
     *   touched = TRUE  when ANY code is in TOUCHED_CODES;
     *   touched = FALSE when there is at least one code and EVERY code is in
     *                   UNTOUCHED_CODES;
     *   touched = NULL  otherwise, including an empty list.
     * The asymmetry is deliberate: one touched code is enough to condemn, but
     * one unrecognised code is enough to withhold a clean bill of health.
     *
     * FACT 4. Anything else: null, stage unknown.
     *
     * @param mixed       $result   Upgrader result (bool|array|WP_Error|null).
     * @param object|null $upgrader The WP_Upgrader that produced it, when available.
     * @return array{codes:array<int,string>,code:string,data:string,stage:string,destination_touched:bool|null,may_have_deactivated:bool}
     */
    public static function classify($result, ?object $upgrader = null): array
    {
        try {
            return self::resolve($result, $upgrader);
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: update outcome classification threw: ' . $e->getMessage());

            return self::shape([], '', '', null);
        }
    }

    /**
     * The classification proper; classify() is the never-throwing wrapper.
     *
     * @param mixed       $result   Upgrader result.
     * @param object|null $upgrader Upgrader instance, when available.
     * @return array{codes:array<int,string>,code:string,data:string,stage:string,destination_touched:bool|null,may_have_deactivated:bool}
     */
    private static function resolve($result, ?object $upgrader): array
    {
        // FACT 1 - identity, and first. See the method doc.
        if ($result === false) {
            return self::shape([self::CODE_NO_UPDATE_OFFERED], self::CODE_NO_UPDATE_OFFERED, '', false, 'preflight', false);
        }

        // FACT 2 - a WP_Error result carries its own code (the Core_Upgrader shape).
        if (is_object($result) && method_exists($result, 'get_error_code')) {
            $code = (string) $result->get_error_code();
            $data = method_exists($result, 'get_error_data') ? $result->get_error_data() : null;

            return self::shape([$code], $code, is_string($data) ? $data : '', self::touchedFor([$code]));
        }

        // FACT 3 - the skin's collected errors (the Plugin/Theme_Upgrader shape).
        $codes = self::skinErrorCodes($upgrader);
        if ($codes !== []) {
            $code = self::preferredCode($codes);

            return self::shape($codes, $code, self::skinErrorData($upgrader, $code), self::touchedFor($codes));
        }

        // FACT 4 - nothing readable. Unclassified, which always means "restore".
        return self::shape([], '', '', null);
    }

    /**
     * TOUCHED wins over UNTOUCHED; an unrecognised code withholds the verdict.
     *
     * @param array<int,string> $codes Every code observed, in order.
     * @return bool|null
     */
    private static function touchedFor(array $codes): ?bool
    {
        if ($codes === []) {
            return null;
        }

        foreach ($codes as $code) {
            if (in_array($code, self::TOUCHED_CODES, true)) {
                return true;
            }
        }

        foreach ($codes as $code) {
            if (!in_array($code, self::UNTOUCHED_CODES, true)) {
                return null;
            }
        }

        return false;
    }

    /**
     * The most informative code in a list: the first one either table knows,
     * else the first one at all.
     *
     * @param array<int,string> $codes Observed codes, in order.
     * @return string
     */
    private static function preferredCode(array $codes): string
    {
        foreach ($codes as $code) {
            if (in_array($code, self::TOUCHED_CODES, true) || in_array($code, self::UNTOUCHED_CODES, true)) {
                return $code;
            }
        }

        return (string) ($codes[0] ?? '');
    }

    /**
     * Every error code the upgrader's skin collected, in order. Never throws:
     * an unexpected shape yields an empty list, which classifies as null.
     *
     * @param object|null $upgrader Upgrader instance, when available.
     * @return array<int,string>
     */
    private static function skinErrorCodes(?object $upgrader): array
    {
        $errors = self::skinErrors($upgrader);
        if ($errors === null || !method_exists($errors, 'get_error_codes')) {
            return [];
        }

        $codes = $errors->get_error_codes();
        if (!is_array($codes)) {
            return [];
        }

        $out = [];
        foreach ($codes as $code) {
            if (is_string($code) && $code !== '') {
                $out[] = $code;
            }
        }

        return $out;
    }

    /**
     * The string error data for one skin code, when it has any.
     *
     * Attacker-influenceable: get_error_data() for copy_failed_ziparchive is the
     * archive entry name (file.php:1825), which comes out of the package. Only
     * ever used as human log text, and sanitised in shape().
     *
     * @param object|null $upgrader Upgrader instance.
     * @param string      $code     Code to read data for.
     * @return string
     */
    private static function skinErrorData(?object $upgrader, string $code): string
    {
        $errors = self::skinErrors($upgrader);
        if ($errors === null || $code === '' || !method_exists($errors, 'get_error_data')) {
            return '';
        }

        $data = $errors->get_error_data($code);

        return is_string($data) ? $data : '';
    }

    /**
     * The WP_Error-shaped object a skin exposes through get_errors(), or null.
     *
     * @param object|null $upgrader Upgrader instance.
     * @return object|null
     */
    private static function skinErrors(?object $upgrader): ?object
    {
        if ($upgrader === null || !isset($upgrader->skin) || !is_object($upgrader->skin)) {
            return null;
        }

        $skin = $upgrader->skin;
        if (!method_exists($skin, 'get_errors')) {
            return null;
        }

        $errors = $skin->get_errors();

        return is_object($errors) ? $errors : null;
    }

    /**
     * Build the return shape, sanitising the two strings that leave this
     * process and land in an operator-visible log.
     *
     * @param array<int,string> $codes   Observed codes, in order.
     * @param string            $code    Resolved primary code.
     * @param string            $data    Resolved error data.
     * @param bool|null         $touched Destination verdict.
     * @param string|null       $stage   Stage override (FACT 1 only).
     * @param bool|null         $mayHaveDeactivated Deactivation override (FACT 1 only).
     * @return array{codes:array<int,string>,code:string,data:string,stage:string,destination_touched:bool|null,may_have_deactivated:bool}
     */
    private static function shape(
        array $codes,
        string $code,
        string $data,
        ?bool $touched,
        ?string $stage = null,
        ?bool $mayHaveDeactivated = null
    ): array {
        $safeCode = self::sanitizeCode($code);

        return [
            'codes'                => $codes,
            'code'                 => $safeCode,
            'data'                 => self::sanitizeData($data),
            'stage'                => $stage ?? (self::STAGE_BY_CODE[$safeCode] ?? 'unknown'),
            'destination_touched'  => $touched,
            'may_have_deactivated' => $mayHaveDeactivated ?? self::mayHaveDeactivated($codes),
        ];
    }

    /**
     * Did anything in this failure run past `upgrader_pre_install`, where core
     * silently deactivates an active plugin?
     *
     * @param array<int,string> $codes Observed codes.
     * @return bool
     */
    private static function mayHaveDeactivated(array $codes): bool
    {
        foreach ($codes as $code) {
            if (in_array($code, self::DEACTIVATION_POSSIBLE_CODES, true)) {
                return true;
            }
        }

        return false;
    }

    /**
     * A code must look like a WordPress error code. Anything else becomes
     * CODE_UNRECOGNIZED so a hostile or malformed value can never be echoed
     * verbatim into a log line, a DB column or a UI.
     *
     * @param string $code Raw code.
     * @return string
     */
    private static function sanitizeCode(string $code): string
    {
        if ($code === '') {
            return '';
        }

        return preg_match('#^[a-z0-9_]{1,64}$#', $code) === 1 ? $code : self::CODE_UNRECOGNIZED;
    }

    /**
     * Bound and de-fang the error data before it can reach an operator's screen.
     *
     * @param string $data Raw data.
     * @return string
     */
    private static function sanitizeData(string $data): string
    {
        if ($data === '') {
            return '';
        }

        $clean = (string) preg_replace('#[\x00-\x1F\x7F]+#', ' ', $data);
        $clean = trim($clean);
        if (strlen($clean) > self::MAX_DATA_CHARS) {
            $clean = substr($clean, 0, self::MAX_DATA_CHARS) . '...';
        }

        return $clean;
    }
}
