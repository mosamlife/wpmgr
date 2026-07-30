<?php
/**
 * PHPUnit bootstrap: load Composer autoload (which classmaps the plugin source
 * and pulls in Brain Monkey + Yoast Polyfills) and define the minimal set of
 * WordPress runtime classes the autologin tests rely on.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

require_once dirname(__DIR__) . '/vendor/autoload.php';

// Activate Patchwork's stream wrapper before loading any stub files. This
// ensures every file included after this point is run through Patchwork's
// code-manipulation pipeline, making functions defined in those files
// redefinable by Brain Monkey via Functions\when() / Functions\expect()
// without throwing Patchwork\Exceptions\DefinedTooEarly.
//
// Brain Monkey's own setUp() also requires Patchwork (via patchwork-loader.php),
// but that only runs when the first test calls Monkey\setUp(). We require it
// here so that wp-stubs.php (loaded next) is already preprocessed.
if (!function_exists('Patchwork\redefine')) {
    require_once dirname(__DIR__) . '/vendor/antecedent/patchwork/Patchwork.php';
}

// wp-stubs.php must be required AFTER Patchwork is active so that every
// function defined there goes through Patchwork's stream wrapper and becomes
// redefinable. Brain Monkey tests override these defaults via Functions\when().
require_once __DIR__ . '/wp-stubs.php';

// ---------------------------------------------------------------------------
// Constants needed by the object-cache drop-in and engine files.
// ---------------------------------------------------------------------------

// ABSPATH is placed two levels deep so dirname(ABSPATH) resolves to a
// dedicated subdirectory of tmp rather than tmp itself. This keeps the
// keystore's legacy-file candidate path (.../wpmgr_wp_abspath/
// .wpmgr-agent-master.key) isolated from system tmp and away from any
// stale artefacts.
if (!defined('ABSPATH')) {
    define('ABSPATH', sys_get_temp_dir() . '/wpmgr_wp_abspath/site/');
}

// Ensure the ABSPATH parent directory exists so keystore writability checks
// do not fail on a missing path. The parent is what candidateKeyDirs() uses
// as the first fallback candidate.
$_absParent = dirname(rtrim((string) ABSPATH, '/\\'));
if (!is_dir($_absParent)) {
    @mkdir($_absParent, 0755, true);
}
unset($_absParent);

if (!defined('WPMGR_AGENT_DIR')) {
    define('WPMGR_AGENT_DIR', dirname(__DIR__));
}

// Bootstrap the object-cache engine class (global namespace, loaded via
// require_once; must come after ABSPATH is defined).
if (!class_exists('WPMgr_Object_Cache')) {
    require_once dirname(__DIR__) . '/includes/object-cache/class-object-cache-config.php';
    require_once dirname(__DIR__) . '/includes/object-cache/class-redis-connection.php';
    require_once dirname(__DIR__) . '/includes/object-cache/class-object-cache-engine.php';
}

// WP $wpdb result-format constants (used by PreloadQueue SELECTs). Real WP
// defines these in wp-db.php; declare them for the in-memory $wpdb doubles.
if (!defined('ARRAY_A')) {
    define('ARRAY_A', 'ARRAY_A');
}
if (!defined('ARRAY_N')) {
    define('ARRAY_N', 'ARRAY_N');
}
if (!defined('OBJECT')) {
    define('OBJECT', 'OBJECT');
}

// WP core's trivial always-same-answer callbacks. Several production files
// register these BY NAME as a literal hook callback (e.g.
// add_filter('xmlrpc_enabled', '__return_false')) rather than a closure —
// tests that capture the registered callback and actually invoke it (proving
// the hook's real OUTCOME, not just that some callback got registered) need
// the real function present. Semantics are fixed and identical to WP core's,
// so no Brain Monkey override seam is needed.
if (!function_exists('__return_false')) {
    function __return_false(): bool
    {
        return false;
    }
}
if (!function_exists('__return_true')) {
    function __return_true(): bool
    {
        return true;
    }
}

// ---------------------------------------------------------------------------
// Minimal WP runtime class doubles used by AutologinCommandTest.
//
// WordPress ships these as real classes, but at unit-test time we only need
// a tiny surface. Brain Monkey stubs FUNCTIONS, not classes, so we declare
// what we need here. Keep these intentionally small and dumb.
// ---------------------------------------------------------------------------

if (!class_exists('WP_Error')) {
    class WP_Error
    {
        /** @var array<string,string> */
        public array $errors = [];

        /** @var array<string,mixed> */
        public array $error_data = [];

        /**
         * @param string              $code    Error code.
         * @param string              $message Human message.
         * @param array<string,mixed> $data    Error data (status, etc).
         */
        public function __construct(string $code = '', string $message = '', array $data = [])
        {
            if ($code !== '') {
                $this->errors[$code] = $message;
                $this->error_data[$code] = $data;
            }
        }

        /**
         * @param string              $code    Error code.
         * @param string              $message Human message.
         * @param array<string,mixed> $data    Error data (status, etc).
         */
        public function add(string $code, string $message, array $data = []): void
        {
            $this->errors[$code]     = $message;
            $this->error_data[$code] = $data;
        }

        public function get_error_code(): string
        {
            $codes = array_keys($this->errors);
            return $codes === [] ? '' : (string) $codes[0];
        }

        public function get_error_message(?string $code = null): string
        {
            $code = $code ?? $this->get_error_code();
            return $this->errors[$code] ?? '';
        }

        /**
         * @return array<string,mixed>
         */
        public function get_error_data(?string $code = null): array
        {
            $code = $code ?? $this->get_error_code();
            $data = $this->error_data[$code] ?? [];
            return is_array($data) ? $data : [];
        }
    }
}

if (!class_exists('WP_REST_Request')) {
    /**
     * Faithful double for WP core's WP_REST_Request.
     *
     * The parameter model is NOT a flat array in WordPress. Core keeps one
     * bucket per source (URL / GET / POST / FILES / JSON / defaults) and
     * resolves reads and writes through get_parameter_order(), whose FIRST
     * entry is 'JSON' whenever the request carries a JSON Content-Type. That
     * detail is load bearing: set_param() with a key core has never seen
     * writes into order[0], so on a JSON request an internally-stashed param
     * lands inside the JSON bucket and comes straight back out of
     * get_json_params(). A flat-array double hides that entirely, which is
     * how a body the control plane genuinely sends reached production
     * untested. Mirror core here rather than simplifying.
     *
     * Legacy convenience kept for the existing suite: passing an array as the
     * first constructor argument seeds the URL bucket directly, which is what
     * the previous flat double did.
     */
    class WP_REST_Request
    {
        /** @var array<string,mixed> One bucket per parameter source, as in core. */
        private array $params = [
            'URL'      => [],
            'GET'      => [],
            'POST'     => [],
            'FILES'    => [],
            // Stays null until parse_json_params() runs, exactly as in core.
            'JSON'     => null,
            'defaults' => [],
        ];

        /** @var array<string,string> */
        private array $headers = [];

        private string $method = '';

        private string $route = '';

        private string $body = '';

        private bool $parsed_json = false;

        /**
         * @param array<string,mixed>|string $method Request method, or a legacy
         *                                           array of URL params.
         * @param string                     $route  Request route.
         */
        public function __construct($method = '', string $route = '')
        {
            if (is_array($method)) {
                $this->params['URL'] = $method;
            } else {
                $this->method = strtoupper($method);
            }
            $this->route = $route;
        }

        public function get_method(): string
        {
            return $this->method;
        }

        public function set_method(string $method): void
        {
            $this->method = strtoupper($method);
        }

        public function get_route(): string
        {
            return $this->route;
        }

        public function set_route(string $route): void
        {
            $this->route = $route;
        }

        public function get_body(): string
        {
            return $this->body;
        }

        public function set_body(string $body): void
        {
            $this->body        = $body;
            $this->parsed_json = false;
        }

        public function get_header(string $key): string
        {
            return $this->headers[strtolower($key)] ?? '';
        }

        public function set_header(string $key, string $value): void
        {
            $this->headers[strtolower($key)] = $value;
        }

        /**
         * @return array<string,string>|null
         */
        public function get_content_type(): ?array
        {
            $value = $this->get_header('Content-Type');
            if ($value === '') {
                return null;
            }

            $parameters = '';
            if (strpos($value, ';') !== false) {
                [$value, $parameters] = explode(';', $value, 2);
            }

            $value = strtolower($value);
            if (strpos($value, '/') === false) {
                return null;
            }

            [$type, $subtype] = explode('/', $value, 2);

            return array_map('trim', compact('value', 'type', 'subtype', 'parameters'));
        }

        public function is_json_content_type(): bool
        {
            $contentType = $this->get_content_type();

            return isset($contentType['value'])
                && preg_match('#^application/([a-z0-9\.\+\-]+\+)?json(\+oembed)?$#', $contentType['value']) === 1;
        }

        /**
         * @return array<string,mixed>
         */
        public function get_url_params(): array
        {
            $urlParams = $this->params['URL'];

            return is_array($urlParams) ? $urlParams : [];
        }

        /**
         * @param array<string,mixed> $params URL params.
         */
        public function set_url_params(array $params): void
        {
            $this->params['URL'] = $params;
        }

        public function get_param(string $key): mixed
        {
            foreach ($this->get_parameter_order() as $type) {
                if (isset($this->params[$type][$key])) {
                    return $this->params[$type][$key];
                }
            }

            return null;
        }

        /**
         * Mirrors core: update every bucket that already holds the key,
         * otherwise create it in the FIRST bucket of the parameter order.
         */
        public function set_param(string $key, mixed $value): void
        {
            $order    = $this->get_parameter_order();
            $foundKey = false;

            foreach ($order as $type) {
                if ($type !== 'defaults' && is_array($this->params[$type]) && array_key_exists($key, $this->params[$type])) {
                    $this->params[$type][$key] = $value;
                    $foundKey                  = true;
                }
            }

            if (!$foundKey) {
                $this->params[$order[0]][$key] = $value;
            }
        }

        /**
         * @return array<string,mixed>|null
         */
        public function get_json_params(): ?array
        {
            $this->parse_json_params();

            $json = $this->params['JSON'];

            return is_array($json) ? $json : null;
        }

        /**
         * @return array<int,string>
         */
        private function get_parameter_order(): array
        {
            $order = [];

            if ($this->is_json_content_type()) {
                $order[] = 'JSON';
            }

            $this->parse_json_params();

            if (in_array($this->method, ['POST', 'PUT', 'PATCH', 'DELETE'], true)) {
                $order[] = 'POST';
            }

            $order[] = 'GET';
            $order[] = 'URL';
            $order[] = 'defaults';

            return $order;
        }

        private function parse_json_params(): void
        {
            if ($this->parsed_json) {
                return;
            }

            $this->parsed_json = true;

            if (!$this->is_json_content_type() || $this->body === '') {
                return;
            }

            $decoded = json_decode($this->body, true);
            if ($decoded === null && json_last_error() !== JSON_ERROR_NONE) {
                $this->parsed_json = false;
                return;
            }

            $this->params['JSON'] = $decoded;
        }
    }
}

if (!class_exists('WP_REST_Response')) {
    class WP_REST_Response
    {
        /** @var mixed */
        public $data;

        public int $status;

        /** @var array<string,string> */
        public array $headers;

        /**
         * @param mixed                 $data    Response body.
         * @param int                   $status  HTTP status code.
         * @param array<string,string>  $headers Response headers.
         */
        public function __construct($data = null, int $status = 200, array $headers = [])
        {
            $this->data    = $data;
            $this->status  = $status;
            $this->headers = $headers;
        }

        public function get_status(): int
        {
            return $this->status;
        }

        /**
         * @return array<string,string>
         */
        public function get_headers(): array
        {
            return $this->headers;
        }
    }
}

if (!class_exists('WP_User')) {
    class WP_User
    {
        public int $ID = 0;

        public string $user_login = '';

        public string $user_email = '';

        public string $user_pass = '';

        public string $display_name = '';

        /** @var array<int,string> */
        public array $roles = [];
    }
}

// ---------------------------------------------------------------------------
// Minimal WP upgrader doubles for the CP-commanded agent self-update.
//
// The apply takes core's own WP_Upgrader lock and then runs Plugin_Upgrader
// over the agent's own directory, so a unit test needs all three names to
// exist. They live here rather than in a test file because class definitions
// are process-global either way, and bootstrap.php is where this suite keeps
// its WP class doubles (wp-stubs.php is functions only, by its own rule).
//
// DELIBERATELY NOT DEFINED: WP_Ajax_Upgrader_Skin, Theme_Upgrader and
// Core_Upgrader. UpdateRunner guards every one of its upgrader paths on the
// ajax skin as well as the upgrader class, so leaving that name absent keeps
// those tests seeing exactly the world they saw before these doubles existed.
// ---------------------------------------------------------------------------

if (!class_exists('WP_Upgrader')) {
    /**
     * Faithful double of the two static lock helpers, implemented over the
     * option store exactly as core does: the lock row holds the moment it was
     * taken, and a lock older than the release timeout is reclaimed rather than
     * respected. Everything else on the real class is out of scope here.
     */
    class WP_Upgrader
    {
        /**
         * @param string   $lock_name       Lock name.
         * @param int|null $release_timeout Seconds the lock is respected.
         * @return bool True when this caller now owns the lock.
         */
        public static function create_lock($lock_name, $release_timeout = null): bool
        {
            $releaseTimeout = (int) ($release_timeout ?: 3600);
            $option         = $lock_name . '.lock';
            $existing       = (int) get_option($option, 0);

            if ($existing > 0) {
                if ($existing > (time() - $releaseTimeout)) {
                    return false;
                }
                self::release_lock($lock_name);
            }

            update_option($option, time(), false);

            return true;
        }

        /**
         * @param string $lock_name Lock name.
         * @return bool
         */
        public static function release_lock($lock_name): bool
        {
            delete_option($lock_name . '.lock');

            return true;
        }
    }
}

if (!class_exists('Automatic_Upgrader_Skin')) {
    /** Placeholder: the agent only ever hands this to Plugin_Upgrader. */
    class Automatic_Upgrader_Skin
    {
    }
}

if (!class_exists('Plugin_Upgrader')) {
    /**
     * Programmable double. A test sets $behaviour to decide what upgrade()
     * does (return true, return a WP_Error, throw, register a shutdown
     * callback of its own), and reads $calls / $restoreCalls back afterwards.
     * Reset the three statics in set_up(): they are process-global.
     */
    class Plugin_Upgrader
    {
        /** @var callable|null Invoked by upgrade(); receives ($plugin, $upgrader). */
        public static $behaviour = null;

        /** @var array<int,string> Plugin keys passed to upgrade(). */
        public static array $calls = [];

        /** @var array<int,array<int,array<string,string>>> Arguments of every restore_temp_backup() call. */
        public static array $restoreCalls = [];

        /** @var object|null Skin handed to the constructor. */
        public $skin;

        /**
         * @param object|null $skin Upgrader skin.
         */
        public function __construct($skin = null)
        {
            $this->skin = $skin;
        }

        /**
         * @param string             $plugin Plugin key.
         * @param array<string,bool> $args   Upgrade arguments.
         * @return mixed Whatever $behaviour returns; true by default.
         */
        public function upgrade($plugin, $args = [])
        {
            self::$calls[] = $plugin;

            $behaviour = self::$behaviour;

            return $behaviour === null ? true : $behaviour($plugin, $this);
        }

        /**
         * Records the call and, like core's own restorer, puts the directory
         * back. Recreating it is what makes a second, later call a genuine
         * no-op, which is exactly the behaviour the agent's shutdown guard
         * relies on.
         *
         * @param array<int,array<string,string>> $temp_backups Backup descriptors.
         * @return bool
         */
        public function restore_temp_backup(array $temp_backups = [])
        {
            self::$restoreCalls[] = $temp_backups;

            foreach ($temp_backups as $backup) {
                if (!isset($backup['src'], $backup['slug'])) {
                    continue;
                }
                $destination = rtrim((string) $backup['src'], '/') . '/' . $backup['slug'];
                if (!is_dir($destination)) {
                    @mkdir($destination, 0755, true);
                }
            }

            return true;
        }

        /**
         * @param bool $enable Whether maintenance mode is being turned on.
         * @return void
         */
        public function maintenance_mode($enable = false): void
        {
        }
    }
}
