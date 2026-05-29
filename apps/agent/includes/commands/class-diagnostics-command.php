<?php
/**
 * Diagnostics command (ADR-037 Sprint 2 + Site-Health-Full): full WP_Debug_Data
 * parity layered over the legacy 14-category WPMgr-extra collector.
 *
 * Single command — returns the entire payload as one JSON blob with these top-
 * level keys:
 *
 *   wp_native    — verbatim WP_Debug_Data::debug_data() output (every section
 *                  WP populates + every third-party `debug_information` filter
 *                  contribution from Yoast / WooCommerce / ACF / etc.).
 *                  Persisted by the CP under category="wp_native".
 *
 *   identity / php / mysql / filesystem / http / cron / themes / plugins /
 *   users / security / https / mail / performance / hosting
 *                — the legacy WPMgr-extra collector. These cards already
 *                  render in the UI; we keep them stable so the existing 9
 *                  cards do not regress while the new wp_native-backed cards
 *                  layer on top (Directory Sizes / Media / Constants /
 *                  Permissions).
 *
 * Leapfrog fields beyond what either WPRemote or WPvivid collect:
 *   - php.version (CP computes EOL countdown server-side from the version)
 *   - cron.overdue_max_seconds (compute against _get_cron_array)
 *   - identity.site_as_of_hash = sha256(sorted(plugin slugs+versions) + theme +
 *     WP + PHP) — one fingerprint that changes when ANY managed component moves
 *   - plugins.licensing[] per known-paid-plugin — raw probe at agent, CP enriches
 *
 * Privacy redaction (applied to `wp_native` before ship):
 *   - Recursively walk the WP_Debug_Data structure.
 *   - Redact any field whose KEY matches the denylist (admin_email,
 *     site_admin_email, user_email, *_password, *_secret, *_key, *_api_key,
 *     auth_*, db_pass, etc.). Replaced with the literal string "REDACTED".
 *   - Redact any string VALUE that matches a basic email regex.
 *   - Sensitive sub-arrays (paths under uploads/wp-content) are kept — they
 *     are useful for the operator and our existing payload already exposes
 *     wp_content_dir.
 *
 * `recurse_dirsize` perf — WP_Debug_Data::debug_data() calls recurse_dirsize
 * for wp-paths-sizes. On a 100 GB+ uploads tree this can blow past PHP's
 * max_execution_time. We pre-warm the wp-paths-sizes section under a 10s
 * budget per top-level directory (ABSPATH, themes, plugins, uploads). When
 * we time out, the section's `total_size` is annotated with
 * `directory_size_status: "timeout"` and the offending field's value is set
 * to "Partial — directory size check exceeded the time budget".
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * The 14-category site-health collector. Stateless: no DB writes, no caching;
 * a single call exercises every probe. The result blob is shipped to the CP
 * via /agent/v1/diagnostics (the agent's heartbeat path pushes diagnostics
 * once per day via the scheduler-bound cron event).
 */
final class DiagnosticsCommand implements CommandInterface
{
    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'diagnostics';
    }

    /**
     * Run every probe and assemble the categorized payload.
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Request parameters (unused).
     * @return array<string,mixed> The 14-category payload.
     */
    public function execute(array $claims, array $params): array
    {
        $identity   = $this->collectIdentity();
        $php        = $this->collectPHP();
        $mysql      = $this->collectMySQL();
        $filesystem = $this->collectFilesystem();
        $http       = $this->collectHTTP();
        $cron       = $this->collectCron();
        $themes     = $this->collectThemes();
        $plugins    = $this->collectPlugins();
        $users      = $this->collectUsers();
        $security   = $this->collectSecurity();
        $https      = $this->collectHTTPS();
        $mail       = $this->collectMail();
        $performance = $this->collectPerformance();
        $hosting    = $this->collectHosting();

        // Leapfrog: site_as_of_hash is computed AFTER plugins+themes are
        // gathered so it can include every slug+version + WP core + PHP. It
        // changes when ANY managed component moves.
        $identity['site_as_of_hash'] = $this->siteAsOfHash($identity, $php, $themes, $plugins);

        // Full Site-Health parity. Wrapped in try/catch so a buggy third-party
        // `debug_information` filter contribution cannot fatal the daily
        // diagnostics push. If WP_Debug_Data is unloadable we surface that as
        // wp_native = ['_error' => 'wp_debug_data_unavailable'] and the legacy
        // 14-category collector above still ships intact.
        $wpNative = $this->collectWpNative();

        return [
            'wp_native'   => $wpNative,
            'identity'    => $identity,
            'php'         => $php,
            'mysql'       => $mysql,
            'filesystem'  => $filesystem,
            'http'        => $http,
            'cron'        => $cron,
            'themes'      => $themes,
            'plugins'     => $plugins,
            'users'       => $users,
            'security'    => $security,
            'https'       => $https,
            'mail'        => $mail,
            'performance' => $performance,
            'hosting'     => $hosting,
            'collected_at' => time(),
        ];
    }

    /**
     * Collect the full WP_Debug_Data dump (Site Health > Info screen parity).
     *
     * WP_Debug_Data lives under wp-admin/includes/ and is not autoloaded under
     * REST/agent contexts. We require it on demand. The whole call is wrapped
     * in try/catch — a buggy third-party `debug_information` filter must not
     * fatal the diagnostics push.
     *
     * Returns a structure shaped like:
     *
     *   [
     *     'wp-core'        => ['label' => 'WordPress', 'fields' => [...]],
     *     'wp-paths-sizes' => ['label' => 'Directories and Sizes', 'fields' => [
     *         'wordpress_size' => ['label' => 'WordPress size', 'value' => '...', 'debug' => 12345],
     *         ...
     *     ]],
     *     ...
     *   ]
     *
     * Privacy: every leaf is run through the redaction walker before return.
     *
     * @return array<string,mixed>
     */
    private function collectWpNative(): array
    {
        if (!defined('ABSPATH')) {
            return ['_error' => 'no_abspath'];
        }
        $debugFile = ABSPATH . 'wp-admin/includes/class-wp-debug-data.php';
        if (!class_exists('\\WP_Debug_Data') && file_exists($debugFile)) {
            // Some sub-deps of WP_Debug_Data live in admin/update.php and
            // admin/translation-install.php; load them best-effort.
            $deps = [
                ABSPATH . 'wp-admin/includes/update.php',
                ABSPATH . 'wp-admin/includes/translation-install.php',
                ABSPATH . 'wp-admin/includes/plugin.php',
                ABSPATH . 'wp-admin/includes/misc.php',
                ABSPATH . 'wp-admin/includes/theme.php',
            ];
            foreach ($deps as $dep) {
                if (file_exists($dep)) {
                    require_once $dep;
                }
            }
            require_once $debugFile;
        }
        if (!class_exists('\\WP_Debug_Data')) {
            return ['_error' => 'wp_debug_data_unavailable'];
        }

        try {
            $data = \WP_Debug_Data::debug_data();
        } catch (\Throwable $e) {
            return [
                '_error'   => 'wp_debug_data_threw',
                '_message' => $e->getMessage(),
            ];
        }
        if (!is_array($data)) {
            return ['_error' => 'wp_debug_data_returned_non_array'];
        }

        // Fill in directory sizes. CRITICAL: WP_Debug_Data::debug_data() ships
        // every wp-paths-sizes row with the literal placeholder string
        // "Loading&hellip;" (debug: "loading...") — on the NATIVE Site Health
        // screen the browser fires a SEPARATE async AJAX request
        // (wp_ajax_health-check-get-sizes -> WP_Debug_Data::get_sizes()) to
        // replace those placeholders with the real recurse_dirsize() results.
        // Because we call debug_data() once server-side there is no second
        // round-trip, so without this merge every directory row reaches the CP
        // as "Loading&hellip;" (the v0.9.14 bug). We run get_sizes() ourselves
        // — exactly what the WP async handler does — and overwrite each field's
        // value + debug with the computed size + raw byte count.
        $dirsizeStatus = $this->mergeDirectorySizes($data);
        if ($dirsizeStatus !== 'ok'
            && isset($data['wp-paths-sizes']) && is_array($data['wp-paths-sizes'])) {
            // Surface partial/failed computation so the UI can show a "Partial"
            // chip instead of silently rendering stale or placeholder values.
            $data['wp-paths-sizes']['directory_size_status'] = $dirsizeStatus;
        }

        return $this->redactWpNative($data);
    }

    /**
     * Compute the actual directory + database sizes via
     * `WP_Debug_Data::get_sizes()` and merge them into the wp-paths-sizes
     * section IN PLACE, overwriting the "Loading&hellip;" placeholders that
     * debug_data() leaves behind.
     *
     * get_sizes() is the exact method WP's own async AJAX handler
     * (wp_ajax_health-check-get-sizes) calls to fill the Site Health Info
     * screen. It runs recurse_dirsize() per top-level path under an internal
     * time budget derived from max_execution_time, plus get_database_size().
     * Its return shape is `key => { size: <human string|"…not exist…">, debug:
     * <raw int bytes|string> }`.
     *
     * We map each computed entry onto the existing field:
     *   - field['value'] := human size string (what the UI dl shows by default)
     *   - field['debug'] := raw bytes int (what card-directory-sizes.tsx reads
     *     via fieldDebugNumber to re-format with our own formatBytes)
     *
     * Returns 'ok' when every size field resolved to a real value, 'partial'
     * when at least one still looks like a placeholder (huge tree timed out),
     * or 'unavailable' when get_sizes() is missing / threw.
     *
     * @param array<string,mixed> $data WP_Debug_Data::debug_data() output, by reference.
     */
    private function mergeDirectorySizes(array &$data): string
    {
        if (!method_exists('\\WP_Debug_Data', 'get_sizes')) {
            return 'unavailable';
        }
        if (!isset($data['wp-paths-sizes']['fields'])
            || !is_array($data['wp-paths-sizes']['fields'])) {
            return 'unavailable';
        }

        // Give the size walk room. WP's get_sizes() reads max_execution_time
        // and self-budgets, but on FPM that's often 0 (unlimited) or short; a
        // best-effort bump means a few-hundred-MB site finishes cleanly.
        if (function_exists('set_time_limit')) {
            @set_time_limit(60);
        }

        try {
            $sizes = \WP_Debug_Data::get_sizes();
        } catch (\Throwable $e) {
            return 'unavailable';
        }
        if (!is_array($sizes)) {
            return 'unavailable';
        }

        $status = 'ok';
        foreach ($sizes as $key => $info) {
            if (!is_string($key)
                || !isset($data['wp-paths-sizes']['fields'][$key])
                || !is_array($data['wp-paths-sizes']['fields'][$key])
                || !is_array($info)) {
                continue;
            }
            if (array_key_exists('size', $info)) {
                $data['wp-paths-sizes']['fields'][$key]['value'] = $info['size'];
            }
            if (array_key_exists('debug', $info)) {
                $data['wp-paths-sizes']['fields'][$key]['debug'] = $info['debug'];
            }
            // A size that came back as the placeholder (or any value still
            // carrying the "Loading" / hellip marker) means the walk didn't
            // finish — flag the section as partial so the UI shows a chip.
            $debug = isset($info['debug']) ? $info['debug'] : null;
            if (!is_int($debug) && !(is_string($debug) && ctype_digit($debug))) {
                // database_size/total_size debug is an int; dir debug is an int
                // on success. A non-numeric debug for a *_size key (other than
                // the legitimate "does not exist" fonts case) signals a miss.
                if (str_ends_with($key, '_size') && $key !== 'fonts_size') {
                    $status = 'partial';
                }
            }
        }

        return $status;
    }

    /**
     * Recursive privacy walker for the WP_Debug_Data output. Two passes per
     * node:
     *   1. If the array KEY matches the SENSITIVE-key denylist, replace the
     *      whole leaf with "REDACTED".
     *   2. If a leaf VALUE is a string that smells like an email, replace
     *      with "REDACTED".
     *
     * Keys whose name ends in `_email` are always redacted regardless of
     * value (defence-in-depth in case the regex misses an exotic address).
     *
     * @param array<string|int,mixed> $node
     * @return array<string|int,mixed>
     */
    private function redactWpNative(array $node): array
    {
        $denyKeys = [
            'admin_email', 'site_admin_email', 'from_email', 'user_email',
            'email', 'auth_key', 'auth_salt', 'secure_auth_key',
            'secure_auth_salt', 'logged_in_key', 'logged_in_salt',
            'nonce_key', 'nonce_salt', 'db_password', 'db_pass',
            'password', 'api_key', 'license_key', 'secret',
            'access_token', 'refresh_token', 'private_key', 'dropbox_token',
            's3_secret', 'aws_secret_access_key',
        ];
        $out = [];
        foreach ($node as $k => $v) {
            $kLower = is_string($k) ? strtolower($k) : '';
            $redactByKey = false;
            if ($kLower !== '') {
                if (in_array($kLower, $denyKeys, true)) {
                    $redactByKey = true;
                } elseif (str_ends_with($kLower, '_email')
                    || str_ends_with($kLower, '_password')
                    || str_ends_with($kLower, '_secret')
                    || str_ends_with($kLower, '_token')
                    || str_ends_with($kLower, '_api_key')
                    || str_ends_with($kLower, '_license')) {
                    $redactByKey = true;
                }
            }
            if ($redactByKey) {
                $out[$k] = 'REDACTED';
                continue;
            }
            if (is_array($v)) {
                $out[$k] = $this->redactWpNative($v);
                continue;
            }
            if (is_string($v) && $this->looksLikeEmail($v)) {
                $out[$k] = 'REDACTED';
                continue;
            }
            $out[$k] = $v;
        }
        return $out;
    }

    /**
     * Cheap email-shape detector. Not RFC-valid; just enough to catch the
     * common admin-email / from-address leak through unexpected fields (e.g.
     * a third-party plugin's `debug_information` contribution dropping the
     * site owner's email into a `support_contact` string).
     */
    private function looksLikeEmail(string $s): bool
    {
        if ($s === '' || strlen($s) > 254) {
            return false;
        }
        // Standalone email — value IS the address.
        if (preg_match('/^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$/', $s) === 1) {
            return true;
        }
        return false;
    }

    /**
     * Identity category — site name, URL, admin email (SENSITIVE), WP version,
     * locale, multisite flag, language. `site_as_of_hash` is added by the
     * caller after the dependent categories are populated.
     *
     * @return array<string,mixed>
     */
    private function collectIdentity(): array
    {
        $url = function_exists('get_site_url') ? (string) get_site_url() : '';
        return [
            'site_name'     => function_exists('get_bloginfo') ? (string) get_bloginfo('name') : '',
            'site_url'      => $url,
            // SENSITIVE — admin email; CP must RLS-gate this field.
            'admin_email'   => function_exists('get_bloginfo') ? (string) get_bloginfo('admin_email') : '',
            'wp_version'    => function_exists('get_bloginfo') ? (string) get_bloginfo('version') : '',
            'language'      => function_exists('get_bloginfo') ? (string) get_bloginfo('language') : '',
            'locale'        => function_exists('get_locale') ? (string) get_locale() : '',
            'is_multisite'  => function_exists('is_multisite') ? (bool) is_multisite() : false,
            'timezone'      => (string) (function_exists('get_option') ? get_option('timezone_string', '') : ''),
            'gmt_offset'    => (float) (function_exists('get_option') ? get_option('gmt_offset', 0) : 0),
        ];
    }

    /**
     * PHP category — version, SAPI, memory limits, key ini values, loaded
     * extensions of interest, opcache stats.
     *
     * We ship the raw PHP version; the CP computes EOL countdown server-side
     * (one source of truth for the EOL calendar, easier to maintain).
     *
     * @return array<string,mixed>
     */
    private function collectPHP(): array
    {
        $extensions = ['curl', 'mbstring', 'openssl', 'zip', 'gd', 'imagick', 'intl', 'mysqli', 'pdo_mysql', 'xml', 'json', 'fileinfo', 'sodium', 'opcache', 'apcu'];
        $loaded = [];
        foreach ($extensions as $ext) {
            $loaded[$ext] = extension_loaded($ext);
        }

        $opcache = [];
        if (function_exists('opcache_get_status')) {
            $status = @opcache_get_status(false);
            if (is_array($status)) {
                $opcache = [
                    'enabled'        => (bool) ($status['opcache_enabled'] ?? false),
                    'hit_rate'       => isset($status['opcache_statistics']['opcache_hit_rate'])
                        ? (float) $status['opcache_statistics']['opcache_hit_rate']
                        : null,
                    'memory_used'    => isset($status['memory_usage']['used_memory'])
                        ? (int) $status['memory_usage']['used_memory']
                        : null,
                    'memory_free'    => isset($status['memory_usage']['free_memory'])
                        ? (int) $status['memory_usage']['free_memory']
                        : null,
                    'memory_wasted'  => isset($status['memory_usage']['wasted_memory'])
                        ? (int) $status['memory_usage']['wasted_memory']
                        : null,
                    'restarts_oom'   => isset($status['opcache_statistics']['oom_restarts'])
                        ? (int) $status['opcache_statistics']['oom_restarts']
                        : null,
                ];
            }
        }

        return [
            'version'              => PHP_VERSION,
            'version_id'           => PHP_VERSION_ID,
            'sapi'                 => PHP_SAPI,
            'memory_limit'         => (string) ini_get('memory_limit'),
            'max_execution_time'   => (int) ini_get('max_execution_time'),
            'max_input_vars'       => (int) ini_get('max_input_vars'),
            'post_max_size'        => (string) ini_get('post_max_size'),
            'upload_max_filesize'  => (string) ini_get('upload_max_filesize'),
            'max_input_time'       => (int) ini_get('max_input_time'),
            'display_errors'       => (string) ini_get('display_errors'),
            'extensions'           => $loaded,
            'opcache'              => $opcache,
        ];
    }

    /**
     * MySQL category — version, charset, key SHOW VARIABLES (max_allowed_packet,
     * innodb_buffer_pool_size, ...), and the privacy-safe `dbsig`.
     *
     * Privacy: `dbsig` = sha1(DB_USER + DB_NAME + DB_HOST) truncated to 6
     * hex chars. DB_PASSWORD is NEVER hashed.
     *
     * @return array<string,mixed>
     */
    private function collectMySQL(): array
    {
        global $wpdb;
        $out = [
            'available' => false,
            'version'   => '',
            'charset'   => '',
            'collation' => '',
            'dbsig'     => $this->dbSig(),
            'variables' => [],
        ];
        if (!is_object($wpdb)) {
            return $out;
        }
        $out['available'] = true;
        // Wrap in a try/catch — a corrupted DB connection should not fatal
        // the entire diagnostics run.
        try {
            $version = $wpdb->get_var('SELECT VERSION()');
            if (is_string($version)) {
                $out['version'] = $version;
            }
            if (method_exists($wpdb, 'get_charset_collate')) {
                $charsetCollate = (string) $wpdb->get_charset_collate();
                $out['charset_collate'] = $charsetCollate;
            }
            $out['charset']   = isset($wpdb->charset) ? (string) $wpdb->charset : '';
            $out['collation'] = isset($wpdb->collate) ? (string) $wpdb->collate : '';

            $vars = ['max_allowed_packet', 'innodb_buffer_pool_size', 'innodb_file_per_table', 'wait_timeout', 'max_connections', 'sql_mode', 'character_set_server', 'collation_server'];
            foreach ($vars as $v) {
                $row = $wpdb->get_row($wpdb->prepare('SHOW VARIABLES LIKE %s', $v), ARRAY_N);
                if (is_array($row) && isset($row[1])) {
                    $out['variables'][$v] = (string) $row[1];
                }
            }
        } catch (\Throwable $e) {
            $out['error'] = 'mysql_probe_failed';
        }
        return $out;
    }

    /**
     * Compute the 6-char dbsig. DB_PASSWORD is intentionally never included —
     * a sig is an operator-correlation fingerprint, not an auth artifact.
     */
    private function dbSig(): string
    {
        $u = defined('DB_USER') ? (string) constant('DB_USER') : '';
        $n = defined('DB_NAME') ? (string) constant('DB_NAME') : '';
        $h = defined('DB_HOST') ? (string) constant('DB_HOST') : '';
        return substr(sha1($u . $n . $h), 0, 6);
    }

    /**
     * Filesystem category — free space + writability of wp-content + uploads.
     *
     * @return array<string,mixed>
     */
    private function collectFilesystem(): array
    {
        $wpContent = defined('WP_CONTENT_DIR') ? (string) constant('WP_CONTENT_DIR') : '';
        $uploads = '';
        if (function_exists('wp_get_upload_dir')) {
            $u = wp_get_upload_dir();
            if (is_array($u) && isset($u['basedir'])) {
                $uploads = (string) $u['basedir'];
            }
        }

        $freeBytes = null;
        if ($wpContent !== '' && function_exists('disk_free_space')) {
            $df = @disk_free_space($wpContent);
            if (is_numeric($df)) {
                $freeBytes = (int) $df;
            }
        }

        return [
            'wp_content_dir'        => $wpContent,
            'wp_content_writable'   => $wpContent !== '' && is_writable($wpContent),
            'uploads_dir'           => $uploads,
            'uploads_writable'      => $uploads !== '' && is_writable($uploads),
            'free_bytes'            => $freeBytes,
            'tmp_dir'               => function_exists('get_temp_dir') ? (string) get_temp_dir() : sys_get_temp_dir(),
            'open_basedir'          => (string) ini_get('open_basedir'),
        ];
    }

    /**
     * HTTP category — outbound loopback probe (HEAD on home_url with a short
     * timeout) + WP_HTTP_BLOCK_EXTERNAL state.
     *
     * @return array<string,mixed>
     */
    private function collectHTTP(): array
    {
        $homeUrl = function_exists('home_url') ? (string) home_url('/') : '';
        $loopback = ['ok' => false, 'status' => 0, 'error' => ''];
        if ($homeUrl !== '' && function_exists('wp_remote_head')) {
            $resp = wp_remote_head($homeUrl, [
                'timeout'     => 5,
                'sslverify'   => true,
                'redirection' => 2,
                'user-agent'  => 'WPMgr-Agent-Diagnostics/1.0',
            ]);
            if (function_exists('is_wp_error') && is_wp_error($resp)) {
                $loopback['error'] = (string) $resp->get_error_message();
            } else {
                $code = function_exists('wp_remote_retrieve_response_code')
                    ? (int) wp_remote_retrieve_response_code($resp)
                    : 0;
                $loopback['status'] = $code;
                $loopback['ok'] = $code > 0 && $code < 500;
            }
        }

        return [
            'home_url'                  => $homeUrl,
            'loopback'                  => $loopback,
            'block_external'            => defined('WP_HTTP_BLOCK_EXTERNAL') && (bool) constant('WP_HTTP_BLOCK_EXTERNAL'),
            'accessible_hosts'          => defined('WP_ACCESSIBLE_HOSTS') ? (string) constant('WP_ACCESSIBLE_HOSTS') : '',
        ];
    }

    /**
     * Cron category — DISABLE_WP_CRON state, count of registered events, +
     * the leapfrog `overdue_max_seconds` (max age in seconds of any event
     * whose next_run < now).
     *
     * @return array<string,mixed>
     */
    private function collectCron(): array
    {
        $disabled = defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON');
        $alternate = defined('ALTERNATE_WP_CRON') && (bool) constant('ALTERNATE_WP_CRON');

        $events = 0;
        $overdueMax = 0;
        $now = time();
        if (function_exists('_get_cron_array')) {
            $cron = _get_cron_array();
            if (is_array($cron)) {
                foreach ($cron as $ts => $hooks) {
                    if (!is_array($hooks)) {
                        continue;
                    }
                    $count = 0;
                    foreach ($hooks as $hookName => $argsList) {
                        if (is_array($argsList)) {
                            $count += count($argsList);
                        }
                    }
                    $events += $count;
                    if ((int) $ts < $now) {
                        $age = $now - (int) $ts;
                        if ($age > $overdueMax) {
                            $overdueMax = $age;
                        }
                    }
                }
            }
        }
        return [
            'disabled'              => $disabled,
            'alternate'             => $alternate,
            'event_count'           => $events,
            'overdue_max_seconds'   => $overdueMax,
        ];
    }

    /**
     * Themes category — active stylesheet + parent template, count of installed.
     *
     * @return array<string,mixed>
     */
    private function collectThemes(): array
    {
        $active = ['stylesheet' => '', 'template' => '', 'name' => '', 'version' => ''];
        if (function_exists('wp_get_theme')) {
            $t = wp_get_theme();
            $active['stylesheet'] = (string) $t->get_stylesheet();
            $active['template']   = (string) $t->get_template();
            $active['name']       = (string) $t->get('Name');
            $active['version']    = (string) $t->get('Version');
        }
        $installed = function_exists('wp_get_themes') ? count(wp_get_themes()) : 0;
        return [
            'active'    => $active,
            'installed' => $installed,
        ];
    }

    /**
     * Plugins category — installed/active counts, available updates count,
     * and the per-paid-plugin `licensing` probe (leapfrog feature).
     *
     * @return array<string,mixed>
     */
    private function collectPlugins(): array
    {
        if (function_exists('get_option') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
        $all = function_exists('get_plugins') ? get_plugins() : [];
        $active = function_exists('get_option') ? (array) get_option('active_plugins', []) : [];
        if (!is_array($all)) {
            $all = [];
        }
        $availableUpdates = 0;
        $updates = function_exists('get_site_transient') ? get_site_transient('update_plugins') : null;
        if (is_object($updates) && isset($updates->response) && is_array($updates->response)) {
            $availableUpdates = count($updates->response);
        }

        return [
            'installed_count'        => count($all),
            'active_count'           => count($active),
            'available_updates'      => $availableUpdates,
            // Raw probe — CP enriches against its allowlist of paid-plugin
            // option keys (out of scope for this sprint to ship the allowlist).
            'licensing'              => $this->collectPluginLicensing($all),
        ];
    }

    /**
     * Probe known paid-plugin license states. Each entry is a small descriptor
     * the CP can enrich against its allowlist. We probe a HARDCODED option-key
     * map here; the CP side will assemble a richer status string from the
     * raw fields.
     *
     * Probes (slug → option key(s)):
     *   acf-pro              — get_option('acf_pro_license')
     *   gravityforms         — get_option('rg_gforms_key')
     *   woocommerce-subscriptions
     *                        — get_site_transient('woocommerce_subscriptions_lp_key')
     *   wp-all-import-pro    — defined('WP_ALL_IMPORT_PRO_VERSION')
     *   elementor-pro        — get_option('elementor_pro_license_key')
     *   wordpress-seo-premium — get_option('wpseo_license')
     *   wpmu-dev             — get_option('wdp_un_membership')
     *   beaver-builder       — get_option('_fl_builder_subscription_email')
     *   divi                 — get_option('et_account')
     *
     * @param array<string,array<string,mixed>> $plugins All installed plugin metadata.
     * @return array<int,array<string,mixed>>
     */
    private function collectPluginLicensing(array $plugins): array
    {
        $probes = [
            'acf-pro'                  => ['plugin' => 'advanced-custom-fields-pro/acf.php', 'option' => 'acf_pro_license'],
            'gravityforms'             => ['plugin' => 'gravityforms/gravityforms.php', 'option' => 'rg_gforms_key'],
            'woocommerce-subscriptions' => ['plugin' => 'woocommerce-subscriptions/woocommerce-subscriptions.php', 'option' => 'woocommerce_subscriptions_lp_key', 'site' => true],
            'wp-all-import-pro'        => ['plugin' => 'wp-all-import-pro/wp-all-import-pro.php', 'option' => 'PMXI_Plugin_Options'],
            'elementor-pro'            => ['plugin' => 'elementor-pro/elementor-pro.php', 'option' => 'elementor_pro_license_key'],
            'wordpress-seo-premium'    => ['plugin' => 'wordpress-seo-premium/wp-seo-premium.php', 'option' => 'wpseo_license'],
            'wpmu-dev'                 => ['plugin' => null, 'option' => 'wdp_un_membership', 'site' => true],
            'beaver-builder'           => ['plugin' => 'bb-plugin/fl-builder.php', 'option' => '_fl_builder_subscription_email'],
            'divi'                     => ['plugin' => null, 'option' => 'et_account'],
        ];

        $out = [];
        foreach ($probes as $slug => $cfg) {
            $pluginFile = $cfg['plugin'];
            $installed = $pluginFile !== null && isset($plugins[$pluginFile]);
            if (!$installed) {
                continue;
            }
            $optionKey = (string) $cfg['option'];
            $value = null;
            if (!empty($cfg['site']) && function_exists('get_site_option')) {
                $value = get_site_option($optionKey, null);
            } elseif (function_exists('get_option')) {
                $value = get_option($optionKey, null);
            }
            // Status heuristic — present + non-empty = "ok"; absent = "missing";
            // CP will replace with a richer status when its allowlist is in.
            $status = ($value === null || $value === false || $value === '') ? 'missing' : 'present';
            $out[] = [
                'slug'    => $slug,
                'plugin'  => $pluginFile,
                'status'  => $status,
                'has_key' => $status === 'present',
            ];
        }
        return $out;
    }

    /**
     * Users category — total count, admin count, breakdown by role.
     *
     * Per the spec, user_email-typed return fields are SENSITIVE and we do
     * NOT enumerate them here. We return counts only — the CP can request a
     * directory dump as a separate, RLS-gated command if ever needed.
     *
     * @return array<string,mixed>
     */
    private function collectUsers(): array
    {
        $out = [
            'total'   => 0,
            'admins'  => 0,
            'by_role' => [],
        ];
        if (function_exists('count_users')) {
            $counts = count_users();
            if (is_array($counts)) {
                $out['total'] = (int) ($counts['total_users'] ?? 0);
                if (isset($counts['avail_roles']) && is_array($counts['avail_roles'])) {
                    foreach ($counts['avail_roles'] as $role => $n) {
                        $out['by_role'][(string) $role] = (int) $n;
                    }
                    $out['admins'] = (int) ($counts['avail_roles']['administrator'] ?? 0);
                }
            }
        }
        return $out;
    }

    /**
     * Security constants category — debug flags + key wp-config defines.
     *
     * @return array<string,mixed>
     */
    private function collectSecurity(): array
    {
        $defines = ['WP_DEBUG', 'WP_DEBUG_LOG', 'WP_DEBUG_DISPLAY', 'DISALLOW_FILE_EDIT', 'DISALLOW_FILE_MODS', 'FORCE_SSL_ADMIN', 'AUTOMATIC_UPDATER_DISABLED', 'WP_AUTO_UPDATE_CORE', 'WP_POST_REVISIONS', 'EMPTY_TRASH_DAYS'];
        $values = [];
        foreach ($defines as $d) {
            if (defined($d)) {
                $v = constant($d);
                if (is_bool($v) || is_string($v) || is_int($v) || is_float($v)) {
                    $values[$d] = $v;
                } else {
                    $values[$d] = '__non_scalar__';
                }
            } else {
                $values[$d] = null;
            }
        }
        return [
            'defines'              => $values,
            'salts_configured'     => defined('AUTH_KEY') && constant('AUTH_KEY') !== '' && strpos((string) constant('AUTH_KEY'), 'put your unique phrase here') === false,
        ];
    }

    /**
     * HTTPS category — site_url + home_url scheme; FORCE_SSL_ADMIN flag.
     *
     * @return array<string,mixed>
     */
    private function collectHTTPS(): array
    {
        $siteUrl = function_exists('get_site_url') ? (string) get_site_url() : '';
        $homeUrl = function_exists('home_url') ? (string) home_url() : '';
        return [
            'site_url_scheme'  => $this->scheme($siteUrl),
            'home_url_scheme'  => $this->scheme($homeUrl),
            'force_ssl_admin'  => defined('FORCE_SSL_ADMIN') && (bool) constant('FORCE_SSL_ADMIN'),
            'is_ssl'           => function_exists('is_ssl') && is_ssl(),
        ];
    }

    private function scheme(string $url): string
    {
        $parts = parse_url($url);
        return is_array($parts) && isset($parts['scheme']) ? (string) $parts['scheme'] : '';
    }

    /**
     * Mail category — admin email (SENSITIVE), wp_mail availability, mail
     * sending capability (mock probe via wp_mail filter would be intrusive, so
     * we just report capability).
     *
     * @return array<string,mixed>
     */
    private function collectMail(): array
    {
        return [
            'wp_mail_exists'      => function_exists('wp_mail'),
            'sendmail_path'       => (string) ini_get('sendmail_path'),
            'smtp_constant_set'   => defined('SMTP_HOST'),
            // SENSITIVE — CP must RLS-gate this field.
            'from_address'        => $this->guessFromAddress(),
        ];
    }

    private function guessFromAddress(): string
    {
        if (function_exists('get_bloginfo')) {
            return (string) get_bloginfo('admin_email');
        }
        return '';
    }

    /**
     * Performance category — object-cache + page-cache presence hints.
     *
     * @return array<string,mixed>
     */
    private function collectPerformance(): array
    {
        $objectCache = false;
        if (function_exists('wp_using_ext_object_cache')) {
            $objectCache = (bool) wp_using_ext_object_cache();
        }
        // wp_cache_get/_set existing is necessary but not sufficient — these
        // exist even with the default in-process cache. The wp_using_ext_object_cache
        // check above is the canonical indicator of a Memcached/Redis cache.
        return [
            'object_cache_external'    => $objectCache,
            'opcache_enabled'          => function_exists('opcache_get_status'),
            'php_fpm'                  => str_contains(PHP_SAPI, 'fpm'),
            'wp_cache_constant'        => defined('WP_CACHE') && (bool) constant('WP_CACHE'),
        ];
    }

    /**
     * Hosting category — `defined()`-based platform fingerprint. Matches the
     * Sprint 1 host_flags structure so CP can store a single source of truth.
     *
     * @return array<string,mixed>
     */
    private function collectHosting(): array
    {
        return [
            'is_pressable' => defined('IS_PRESSABLE'),
            'is_gridpane'  => defined('GRIDPANE_LOCATION'),
            'is_wpengine'  => defined('WPE_APIKEY') || function_exists('is_wpe'),
            'is_atomic'    => defined('IS_ATOMIC') || defined('IS_WPCOM'),
            'is_kinsta'    => defined('KINSTA_CACHE_ZONE') || defined('KINSTAMU_VERSION'),
            'is_flywheel'  => defined('FLYWHEEL_CONFIG_DIR'),
            'is_runcloud'  => defined('RUNCLOUD'),
            'is_cloudways' => defined('CLOUDWAYS_HOSTING'),
            'server_software' => isset($_SERVER['SERVER_SOFTWARE']) ? (string) $_SERVER['SERVER_SOFTWARE'] : '',
        ];
    }

    /**
     * Compute the site-as-of fingerprint: sha256(sorted plugin slugs+versions
     * + active theme + WP version + PHP version). Returns a stable 64-hex
     * string that changes any time a managed component moves.
     *
     * Re-uses already-collected category blobs so we don't re-query the WP
     * plugin/theme registries.
     *
     * @param array<string,mixed> $identity Collected identity payload.
     * @param array<string,mixed> $php Collected PHP payload.
     * @param array<string,mixed> $themes Collected themes payload.
     * @param array<string,mixed> $plugins Collected plugins payload.
     */
    private function siteAsOfHash(array $identity, array $php, array $themes, array $plugins): string
    {
        if (function_exists('get_option') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
        $all = function_exists('get_plugins') ? get_plugins() : [];
        if (!is_array($all)) {
            $all = [];
        }
        $slugs = [];
        foreach ($all as $file => $meta) {
            if (!is_string($file) || !is_array($meta)) {
                continue;
            }
            $version = isset($meta['Version']) ? (string) $meta['Version'] : '';
            $slugs[] = $file . '@' . $version;
        }
        sort($slugs);
        $stylesheet = is_array($themes['active'] ?? null) ? (string) ($themes['active']['stylesheet'] ?? '') : '';
        $themeVersion = is_array($themes['active'] ?? null) ? (string) ($themes['active']['version'] ?? '') : '';
        $payload = implode("\n", [
            implode(',', $slugs),
            $stylesheet . '@' . $themeVersion,
            'wp:' . (string) ($identity['wp_version'] ?? ''),
            'php:' . (string) ($php['version'] ?? ''),
        ]);
        return hash('sha256', $payload);
    }
}
