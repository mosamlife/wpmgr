<?php
/**
 * WordPress function stubs for the PHPUnit test suite.
 *
 * This file MUST be required AFTER vendor/autoload.php so that Patchwork's
 * stream wrapper is already active. Functions defined here are included in
 * Patchwork's redefine registry, which means Brain Monkey can override them
 * per-test via Functions\when() / Functions\expect() without throwing
 * Patchwork\Exceptions\DefinedTooEarly or
 * Brain\Monkey\Expectation\Exception\MissingFunctionExpectations.
 *
 * Rules:
 *   - Guard every definition with function_exists() so a test that defines its
 *     own global stub (outside Brain Monkey) does not collide.
 *   - Semantics mirror WP core closely enough for the test surface, but remain
 *     minimal — these are stubs, not re-implementations.
 *   - No class definitions, no constant definitions (those stay in bootstrap.php).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

// ---------------------------------------------------------------------------
// URL / path helpers
// ---------------------------------------------------------------------------

if (!function_exists('wp_parse_url')) {
    /**
     * Thin wrapper around parse_url() — mirrors the real WP implementation.
     *
     * @param string $url       The URL to parse.
     * @param int    $component Optional PHP_URL_* constant (-1 for full array).
     * @return mixed
     */
    function wp_parse_url(string $url, int $component = -1): mixed
    {
        return parse_url($url, $component);
    }
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

if (!function_exists('get_temp_dir')) {
    /**
     * Returns the directory WordPress uses for temporary files.
     * Mirrors the real WP implementation: honours WP_TEMP_DIR when defined,
     * then falls back to sys_get_temp_dir() with a trailing slash.
     *
     * @return string Absolute path with trailing slash.
     */
    function get_temp_dir(): string
    {
        if (defined('WP_TEMP_DIR') && is_string(WP_TEMP_DIR) && WP_TEMP_DIR !== '') {
            return rtrim((string) WP_TEMP_DIR, '/\\') . '/';
        }
        return rtrim(sys_get_temp_dir(), '/\\') . '/';
    }
}

if (!function_exists('wp_mkdir_p')) {
    /**
     * Recursively creates directories — mirrors the real WP implementation.
     *
     * @param string $target Directory path to create.
     * @return bool True on success or if the directory already exists.
     */
    function wp_mkdir_p(string $target): bool
    {
        if (is_dir($target)) {
            return true;
        }
        return mkdir($target, 0755, true);
    }
}

if (!function_exists('wp_delete_file')) {
    /**
     * Deletes a file — mirrors the real WP implementation (suppressed unlink).
     *
     * The return type is intentionally omitted (not declared void) so that tests
     * which mock this function via Functions\when()->alias() and return a bool
     * from @unlink() are accepted by Patchwork without NonNullToVoid errors.
     *
     * @param string $file Absolute path to the file to delete.
     */
    function wp_delete_file(string $file)
    {
        @unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test stub only
    }
}

// ---------------------------------------------------------------------------
// Random
// ---------------------------------------------------------------------------

if (!function_exists('wp_rand')) {
    /**
     * Returns a random integer — mirrors the real WP implementation.
     *
     * @param int $min Lower bound (inclusive).
     * @param int $max Upper bound (inclusive).
     * @return int
     */
    function wp_rand(int $min = 0, int $max = 0): int
    {
        if ($min === 0 && $max === 0) {
            $max = PHP_INT_MAX;
        }
        return random_int($min, $max);
    }
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

if (!function_exists('wp_strip_all_tags')) {
    /**
     * Strips all HTML tags — mirrors the real WP implementation closely
     * enough for the test surface (script/style contents are dropped
     * entirely, same as core; whitespace normalization is skipped since no
     * caller here relies on it).
     *
     * @param string $text Text to strip tags from.
     * @return string
     */
    function wp_strip_all_tags(string $text): string
    {
        $text = preg_replace('@<(script|style)[^>]*?>.*?</\\1>@si', '', $text) ?? $text;
        $text = strip_tags($text); // phpcs:ignore WordPress.WP.AlternativeFunctions.strip_tags_strip_tags -- test stub only, mirrors wp_strip_all_tags()'s own internal implementation
        return trim($text);
    }
}

// ---------------------------------------------------------------------------
// Input sanitization
// ---------------------------------------------------------------------------

if (!function_exists('wp_unslash')) {
    /**
     * Removes slashes from a value — mirrors the real WP implementation.
     *
     * @param mixed $value Value to unslash.
     * @return mixed
     */
    function wp_unslash(mixed $value): mixed
    {
        if (is_array($value)) {
            return array_map('wp_unslash', $value);
        }
        return is_string($value) ? stripslashes($value) : $value;
    }
}

if (!function_exists('sanitize_text_field')) {
    /**
     * Sanitizes a string from user input — approximates the real WP implementation.
     *
     * @param string $str Value to sanitize.
     * @return string
     */
    function sanitize_text_field(string $str): string
    {
        $filtered = strip_tags($str);
        $filtered = preg_replace('/[\r\n\t ]+/', ' ', $filtered) ?? '';
        return trim($filtered);
    }
}

if (!function_exists('sanitize_key')) {
    /**
     * Sanitizes a string key — lowercases and strips non-alphanumeric characters.
     *
     * @param string $key String key.
     * @return string
     */
    function sanitize_key(string $key): string
    {
        $sanitized = strtolower($key);
        $sanitized = preg_replace('/[^a-z0-9_\-]/', '', $sanitized) ?? '';
        return $sanitized;
    }
}

if (!function_exists('sanitize_email')) {
    /**
     * Strips out all characters not allowed in an email address.
     *
     * @param string $email Candidate email address.
     * @return string The sanitized address, or '' if invalid.
     */
    function sanitize_email(string $email): string
    {
        $email = trim($email);
        return filter_var($email, FILTER_VALIDATE_EMAIL) !== false ? $email : '';
    }
}

// ---------------------------------------------------------------------------
// Output escaping
// ---------------------------------------------------------------------------

if (!function_exists('esc_html')) {
    /**
     * Escapes text for safe use in HTML output — passthrough stub for tests.
     *
     * Real WP applies htmlspecialchars + charset encoding; for test purposes
     * returning the input unchanged is sufficient since tests verify logic,
     * not escaping fidelity.
     *
     * @param string $text Text to escape.
     * @return string
     */
    function esc_html(string $text): string
    {
        return $text;
    }
}

// ---------------------------------------------------------------------------
// WP option store (default: no stored value)
// ---------------------------------------------------------------------------

if (!function_exists('get_option')) {
    /**
     * Retrieves a WP option value — default stub returns $default.
     *
     * @param string $option  Option name.
     * @param mixed  $default Value to return when option is absent.
     * @return mixed
     */
    function get_option(string $option, mixed $default = false): mixed
    {
        return $default;
    }
}

if (!function_exists('get_site_option')) {
    /**
     * Retrieves a network-scoped option — default stub returns $default.
     *
     * @param string $option  Option name.
     * @param mixed  $default Value to return when option is absent.
     * @return mixed
     */
    function get_site_option(string $option, mixed $default = false): mixed
    {
        return $default;
    }
}

// ---------------------------------------------------------------------------
// WP conditional functions (conservative defaults)
// ---------------------------------------------------------------------------

if (!function_exists('is_multisite')) {
    /**
     * Whether the current WordPress installation is a multisite network.
     * Default stub returns false (single-site) for the test environment.
     *
     * @return bool
     */
    function is_multisite(): bool
    {
        return false;
    }
}

if (!function_exists('is_singular')) {
    /**
     * Whether the query is for an existing single post of any type.
     * Default stub returns false so no-cache checks do not trigger on a miss.
     *
     * @return bool
     */
    function is_singular(): bool
    {
        return false;
    }
}

if (!function_exists('is_user_logged_in')) {
    /**
     * Whether the current visitor is logged in.
     * Default stub returns false so the optimizer does not skip transforms.
     *
     * @return bool
     */
    function is_user_logged_in(): bool
    {
        return false;
    }
}

if (!function_exists('wp_suspend_cache_addition')) {
    /**
     * Whether cache addition is currently suspended.
     * Default stub returns false (cache additions proceed normally).
     *
     * @return bool
     */
    function wp_suspend_cache_addition(): bool
    {
        return false;
    }
}

// ---------------------------------------------------------------------------
// phpredis \Redis class stub (used when the phpredis extension is not loaded).
//
// Provides the class constants required by the object-cache engine and commands
// so that production code referencing \Redis::OPT_SCAN / \Redis::SCAN_RETRY etc.
// can be parsed and type-checked at test time without the extension installed.
// The class intentionally has NO method implementations; the engine always runs
// in array mode during unit tests (no live Redis connection).
// ---------------------------------------------------------------------------

if (!class_exists('Redis')) {
    /**
     * Minimal stub for the phpredis \Redis class.
     * Only the constants used by the WPMgr object-cache engine are declared.
     */
    class Redis
    {
        /** Option key for SCAN iteration behaviour (matches the real phpredis value). */
        public const OPT_SCAN = 4;

        /** SCAN option: retry automatically when a batch is empty. */
        public const SCAN_RETRY = 1;

        /** SCAN option: do not retry (caller handles empty batches). */
        public const SCAN_NORETRY = 0;

        /** Serializer: none (raw bytes). */
        public const SERIALIZER_NONE = 0;

        /** Serializer: PHP serialize(). */
        public const SERIALIZER_PHP = 1;

        /** Serializer: igbinary (requires the igbinary extension). */
        public const SERIALIZER_IGBINARY = 2;

        /** Serializer: msgpack (requires the msgpack extension). */
        public const SERIALIZER_MSGPACK = 3;

        /** Compression: none. */
        public const COMPRESSION_NONE = 0;

        /** Compression: LZF. */
        public const COMPRESSION_LZF = 1;

        /** Compression: ZSTD. */
        public const COMPRESSION_ZSTD = 3;

        /** Compression: LZ4. */
        public const COMPRESSION_LZ4 = 4;

        /** Option key for read timeout. */
        public const OPT_READ_TIMEOUT = 11;

        /** Option key for serializer. */
        public const OPT_SERIALIZER = 1;

        /** Option key for compression. */
        public const OPT_COMPRESSION = 7;

        /** flushDB async flag for phpredis >= 6.0. */
        public const FLUSHDB_ASYNC = true;
    }
}

if (!function_exists('get_core_updates')) {
    /**
     * Returns available WordPress core update objects — passthrough stub for tests.
     *
     * Real WP loads this from wp-admin/includes/update.php and queries the
     * update_core transient. The test stub returns an empty array so
     * MetadataCommand::coreUpdate() finds no pending updates without side effects.
     * Tests that need a specific response override via Functions\when().
     *
     * @return array<int,object> Array of update objects.
     */
    function get_core_updates(): array
    {
        return [];
    }
}

if (!function_exists('wp_upload_dir')) {
    /**
     * Returns information about the upload directory — passthrough stub for tests.
     *
     * Real WP reads wp-config and per-post upload path overrides. The test stub
     * returns an empty array so callers that use `isset($info['basedir'])` safely
     * get no value and skip any uploads-dir-dependent code path. Tests that need
     * a specific basedir must override via Functions\when('wp_upload_dir')->justReturn([...]).
     *
     * Defined here (Patchwork-transformable) so that once MetadataCommand or any
     * file-manager code triggers function_exists('wp_upload_dir') to return true
     * process-wide, Brain Monkey can still intercept calls to it in tests that
     * explicitly mock it — without throwing MissingFunctionExpectations in tests
     * that do not mock it (those get this safe empty-array default).
     *
     * @return array<string,string> Upload directory info, or empty on failure.
     */
    function wp_upload_dir(): array
    {
        return [];
    }
}

if (!function_exists('esc_sql')) {
    /**
     * Escapes data for use in a MySQL query — passthrough stub for tests.
     *
     * Real WP delegates to $wpdb->_escape(); for test purposes the inputs are
     * plugin-derived identifiers with no escapable characters, so returning
     * the value unchanged preserves the behavior under test.
     *
     * @param string|array $data Data to escape.
     * @return string|array
     */
    function esc_sql($data)
    {
        return $data;
    }
}

// ---------------------------------------------------------------------------
// WP HTTP API stubs (wp_remote_get / response helpers / is_wp_error / site_url
// / esc_url_raw). Required by BackupCommand::isLoopbackGated() and
// Watchdog::detectQueuedStallReason() for the membership-gate detection path.
// Tests that need specific responses override via Functions\when().
// ---------------------------------------------------------------------------

if (!function_exists('wp_remote_get')) {
    /**
     * Performs an HTTP GET request — default stub returns WP_Error.
     * Tests must stub this via Functions\when('wp_remote_get') to return
     * a specific response or error.
     *
     * @param string               $url  Request URL.
     * @param array<string,mixed>  $args Request arguments.
     * @return array<string,mixed>|\WP_Error Response array or WP_Error on failure.
     */
    function wp_remote_get(string $url, array $args = [])
    {
        return new \WP_Error('test_stub', 'wp_remote_get called without a test stub');
    }
}

if (!function_exists('wp_remote_retrieve_response_code')) {
    /**
     * Retrieve the response code from a wp_remote_get/post response.
     *
     * @param array<string,mixed>|\WP_Error $response Response array or WP_Error.
     * @return int HTTP response code.
     */
    function wp_remote_retrieve_response_code($response): int
    {
        if (is_array($response) && isset($response['response']['code'])) {
            return (int) $response['response']['code'];
        }
        return 0;
    }
}

if (!function_exists('wp_remote_retrieve_header')) {
    /**
     * Retrieve a specific header from a wp_remote response.
     *
     * @param array<string,mixed>|\WP_Error $response Response array or WP_Error.
     * @param string                         $header   Header name (lowercase).
     * @return string Header value or empty string.
     */
    function wp_remote_retrieve_header($response, string $header): string
    {
        if (is_array($response) && isset($response['headers'][$header])) {
            return (string) $response['headers'][$header];
        }
        return '';
    }
}

if (!function_exists('is_wp_error')) {
    /**
     * Whether a value is a WP_Error instance.
     *
     * @param mixed $thing Value to check.
     * @return bool
     */
    function is_wp_error(mixed $thing): bool
    {
        return $thing instanceof \WP_Error;
    }
}

if (!function_exists('site_url')) {
    /**
     * Returns the site URL — default stub returns a placeholder.
     * Tests must stub via Functions\when('site_url') when specific URLs matter.
     *
     * @param string $path Optional path to append.
     * @return string
     */
    function site_url(string $path = ''): string
    {
        return 'https://example.com' . $path;
    }
}

if (!function_exists('esc_url_raw')) {
    /**
     * Sanitizes a URL for use in a database or redirect.
     * Passthrough stub for tests.
     *
     * @param string $url URL to sanitize.
     * @return string
     */
    function esc_url_raw(string $url): string
    {
        return $url;
    }
}

if (!function_exists('wp_json_encode')) {
    /**
     * Encodes a value as JSON — thin wrapper around json_encode.
     *
     * @param mixed $data    Value to encode.
     * @param int   $options json_encode flags.
     * @param int   $depth   Maximum depth.
     * @return string|false JSON string or false on failure.
     */
    function wp_json_encode(mixed $data, int $options = 0, int $depth = 512): string|false
    {
        return json_encode($data, $options, $depth);
    }
}
