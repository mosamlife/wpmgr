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

// Namespaced shadow of sodium_memzero(), so the suite can run as if this
// machine had no native libsodium extension (GH #709). Required here, not from
// a test case, so the function exists before any call site is first executed.
// Inert until a test flips WPMgr\Agent\Tests\SodiumPlatform::refuse(); see the
// file's header for why this is not a Patchwork redefinable-internal.
require_once __DIR__ . '/sodium-memzero-shim.php';

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

        /**
         * @return array<string,mixed>
         */
        public function get_query_params(): array
        {
            return $this->params['GET'];
        }

        /**
         * @param array<string,mixed> $params Query params.
         */
        public function set_query_params(array $params): void
        {
            $this->params['GET'] = $params;
        }

        /**
         * @return array<string,mixed>
         */
        public function get_body_params(): array
        {
            return $this->params['POST'];
        }

        /**
         * @param array<string,mixed> $params Body params.
         */
        public function set_body_params(array $params): void
        {
            $this->params['POST'] = $params;
        }

        /** @var array<string,mixed> */
        private array $attributes = [];

        /**
         * @param array<string,mixed> $attributes Handler attributes.
         */
        public function set_attributes(array $attributes): void
        {
            $this->attributes = $attributes;
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
         * @return mixed
         */
        public function get_data()
        {
            return $this->data;
        }

        /**
         * @param mixed $data Data.
         */
        public function set_data($data): void
        {
            $this->data = $data;
        }

        public string $matched_route = '';

        /** @var mixed */
        public $matched_handler = null;

        public function get_matched_route(): string
        {
            return $this->matched_route;
        }

        public function set_matched_route(string $route): void
        {
            $this->matched_route = $route;
        }

        /**
         * @return mixed
         */
        public function get_matched_handler()
        {
            return $this->matched_handler;
        }

        /**
         * @param mixed $handler Handler.
         */
        public function set_matched_handler($handler): void
        {
            $this->matched_handler = $handler;
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

if (!class_exists('WP_REST_Server')) {
    /**
     * Double for core's WP_REST_Server: dispatch() and respond_to_request()
     * run the four REST filters in core's order and with core's arguments
     * (WordPress 7.1 class-wp-rest-server.php). Hooks are read from
     * $GLOBALS['wp_filter'] the way the test registers them, in priority
     * order, insertion order within a priority.
     */
    class WP_REST_Server
    {
        /** @var array<string,list<array<string,mixed>>> */
        public array $routes = [];

        /**
         * @param string               $pattern Route regex.
         * @param array<string,mixed>  $handler Handler.
         */
        public function register_route(string $pattern, array $handler): void
        {
            $this->routes[$pattern][] = $handler + ['methods' => [], 'args' => [], 'permission_callback' => null];
        }

        /**
         * @param string $tag     Filter.
         * @param mixed  ...$args Value and arguments.
         * @return mixed
         */
        public static function applyHooks(string $tag, ...$args)
        {
            $value = $args[0] ?? null;
            $hook  = $GLOBALS['wp_filter'][$tag] ?? null;
            if (!is_object($hook) || !isset($hook->callbacks) || !is_array($hook->callbacks)) {
                return $value;
            }
            $callbacks = $hook->callbacks;
            ksort($callbacks);
            foreach ($callbacks as $priority => $bucket) {
                foreach ($bucket as $entry) {
                    // As core's WP_Hook: a callback removed while the hook
                    // runs does not run.
                    $live = false;
                    foreach ($hook->callbacks[$priority] ?? [] as $now) {
                        if ($now['function'] === $entry['function']) {
                            $live = true;
                        }
                    }
                    if (!$live) {
                        continue;
                    }
                    $args[0] = $value;
                    $value   = call_user_func_array($entry['function'], array_slice($args, 0, (int) $entry['accepted_args']));
                }
            }

            return $value;
        }

        /**
         * @param mixed $request Request.
         * @return mixed
         */
        public function dispatch($request)
        {
            $result = self::applyHooks('rest_pre_dispatch', null, $this, $request);
            if (!empty($result)) {
                if ($result instanceof WP_Error) {
                    return $this->error_to_response($result);
                }

                return $result instanceof WP_REST_Response ? $result : new WP_REST_Response($result);
            }
            $route   = null;
            $handler = null;
            foreach ($this->routes as $pattern => $handlers) {
                if (preg_match('@^' . $pattern . '$@i', $request->get_route(), $m) !== 1) {
                    continue;
                }
                foreach ($handlers as $h) {
                    if (empty($h['methods'][$request->get_method()])) {
                        continue;
                    }
                    $args = [];
                    foreach ($m as $k => $v) {
                        if (!is_int($k)) {
                            $args[$k] = $v;
                        }
                    }
                    $request->set_url_params($args);
                    $request->set_attributes($h);
                    $route   = $pattern;
                    $handler = $h;
                    break 2;
                }
            }
            if ($handler === null) {
                return $this->error_to_response(new WP_Error('rest_no_route', 'No route was found matching the URL and request method.', ['status' => 404]));
            }
            $error = isset($handler['validate']) ? call_user_func($handler['validate'], $request) : null;

            return $this->respond_to_request($request, (string) $route, $handler, $error);
        }

        /**
         * @param mixed               $request  Request.
         * @param string              $route    Route.
         * @param array<string,mixed> $handler  Handler.
         * @param mixed               $response Error so far.
         * @return WP_REST_Response
         */
        protected function respond_to_request($request, string $route, array $handler, $response)
        {
            $response = self::applyHooks('rest_request_before_callbacks', $response, $handler, $request);
            if (!($response instanceof WP_Error) && !empty($handler['permission_callback'])) {
                $permission = call_user_func($handler['permission_callback'], $request);
                if ($permission instanceof WP_Error) {
                    $response = $permission;
                } elseif (false === $permission || null === $permission) {
                    $response = new WP_Error('rest_forbidden', 'Sorry, you are not allowed to do that.', ['status' => 403]);
                }
            }
            if (!($response instanceof WP_Error)) {
                $dispatch_result = self::applyHooks('rest_dispatch_request', null, $request, $route, $handler);
                if (null !== $dispatch_result) {
                    $response = $dispatch_result;
                } else {
                    $response = call_user_func($handler['callback'], $request);
                }
            }
            $response = self::applyHooks('rest_request_after_callbacks', $response, $handler, $request);
            if ($response instanceof WP_Error) {
                $response = $this->error_to_response($response);
            } elseif (!($response instanceof WP_REST_Response)) {
                $response = new WP_REST_Response($response);
            }
            $response->set_matched_route($route);
            $response->set_matched_handler($handler);

            return $response;
        }

        /**
         * @param WP_Error $error Error.
         * @return WP_REST_Response
         */
        protected function error_to_response(WP_Error $error): WP_REST_Response
        {
            $data   = $error->get_error_data();
            $status = (int) ($data['status'] ?? 500);

            return new WP_REST_Response(['code' => $error->get_error_code(), 'message' => $error->get_error_message(), 'data' => $data], $status);
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
         * Make create_lock() fail the way core's own helper can fail for an
         * INFRASTRUCTURE reason rather than because the lock is held: its
         * `INSERT IGNORE` is MySQL syntax, so on a rewritten database backend
         * or an options table that refuses the write, $wpdb->query() returns
         * false and no lock row is ever created. Core then conflates that with
         * "locked" (class-wp-upgrader.php:1067 to :1072). SiteUpdateLock must
         * report UNAVAILABLE, not HELD_BY_OTHER, for that case; this switch is
         * the only way to reach it. Tests MUST reset it in tear-down, since
         * statics are process-global across this non-isolated suite.
         *
         * @var bool
         */
        public static bool $failCreateLock = false;

        /**
         * @param string   $lock_name       Lock name.
         * @param int|null $release_timeout Seconds the lock is respected.
         * @return bool True when this caller now owns the lock.
         */
        public static function create_lock($lock_name, $release_timeout = null): bool
        {
            if (self::$failCreateLock) {
                return false;
            }

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

// ---------------------------------------------------------------------------
// Abilities API doubles (WordPress 7.1 shapes).
//
// WP_Filter_Sentinel is core's per-call short-circuit default: an empty final
// class compared by identity. WP_Ability mirrors the parts of core's class the
// ownership check reads: the two protected callback properties and the
// execution-path methods, with the same names and visibility. Tests subclass
// it to model an ability_class override. Neither double carries behaviour the
// agent depends on beyond its shape.
// ---------------------------------------------------------------------------

if (!class_exists('WP_Filter_Sentinel')) {
    final class WP_Filter_Sentinel
    {
    }
}

if (!class_exists('WP_Ability')) {
    /**
     * Follows core 7.1's WP_Ability execution path method for method: the
     * same filters with the same arguments in the same order, the real
     * per-call sentinel, input passed to a callback only when an input schema
     * exists, callback throws turned into a WP_Error, and schema validation
     * through rest_validate_value_from_schema(). Translated messages are
     * replaced by fixed text; nothing the agent reads depends on them.
     */
    class WP_Ability
    {
        /** @var string */
        protected $name;

        /** @var array<string,mixed> */
        protected $input_schema = [];

        /** @var array<string,mixed> */
        protected $output_schema = [];

        /** @var callable */
        protected $execute_callback;

        /** @var callable */
        protected $permission_callback;

        /**
         * @param string              $name Ability name.
         * @param array<string,mixed> $args execute_callback, permission_callback,
         *                                  input_schema, output_schema.
         */
        public function __construct(string $name, array $args)
        {
            $this->name                = $name;
            $this->execute_callback    = $args['execute_callback'] ?? null;
            $this->permission_callback = $args['permission_callback'] ?? null;
            $this->input_schema        = $args['input_schema'] ?? [];
            $this->output_schema       = $args['output_schema'] ?? [];
        }

        public function get_name(): string
        {
            return $this->name;
        }

        /** @return array<string,mixed> */
        public function get_input_schema(): array
        {
            return $this->input_schema;
        }

        /** @return array<string,mixed> */
        public function get_output_schema(): array
        {
            return $this->output_schema;
        }

        /** @param mixed $input Input. @return mixed */
        public function normalize_input($input = null)
        {
            if (null === $input) {
                $input_schema = $this->get_input_schema();
                if (array_key_exists('default', $input_schema)) {
                    $input = $input_schema['default'];
                }
            }

            return apply_filters('wp_ability_normalize_input', $input, $this->name, $this);
        }

        /** @param mixed $input Input. @return mixed */
        public function validate_input($input = null)
        {
            $input_schema = $this->get_input_schema();
            if (empty($input_schema)) {
                if (null === $input) {
                    return true;
                }

                return new WP_Error('ability_missing_input_schema', 'no input schema');
            }
            $valid_input = rest_validate_value_from_schema($input, $input_schema, 'input');
            $is_valid    = is_wp_error($valid_input) ? new WP_Error('ability_invalid_input', 'invalid input') : true;

            $validity = apply_filters('wp_ability_validate_input', $is_valid, $input, $this->name);
            if (false === $validity) {
                return new WP_Error('ability_invalid_input', 'Invalid input.');
            }
            if (is_wp_error($validity)) {
                return $validity;
            }

            return true;
        }

        /** @param mixed $input Input. @return mixed */
        protected function invoke_callback(callable $callback, $input = null)
        {
            $args = [];
            if (!empty($this->get_input_schema())) {
                $args[] = $input;
            }
            try {
                return $callback(...$args);
            } catch (\Throwable $e) {
                return new WP_Error('ability_callback_exception', 'callback threw');
            }
        }

        /** @param mixed $input Input. @return mixed */
        public function check_permissions($input = null)
        {
            if (!is_callable($this->permission_callback)) {
                return new WP_Error('ability_invalid_permission_callback', 'no permission callback');
            }
            $permission = $this->invoke_callback($this->permission_callback, $input);
            $result     = apply_filters('wp_ability_permission_result', $permission, $this->name, $input, $this);
            if (!is_bool($result) && !is_wp_error($result)) {
                $result = false;
            }

            return $result;
        }

        /** @param mixed $input Input. @return mixed */
        protected function do_execute($input = null)
        {
            if (!is_callable($this->execute_callback)) {
                $result = new WP_Error('ability_invalid_execute_callback', 'no execute callback');
            } else {
                $result = $this->invoke_callback($this->execute_callback, $input);
            }

            return apply_filters('wp_ability_execute_result', $result, $this->name, $input, $this);
        }

        /** @param mixed $output Output. @return mixed */
        protected function validate_output($output)
        {
            $output_schema = $this->get_output_schema();
            if (empty($output_schema)) {
                $is_valid = true;
            } else {
                $valid    = rest_validate_value_from_schema($output, $output_schema, 'output');
                $is_valid = is_wp_error($valid) ? new WP_Error('ability_invalid_output', 'invalid output') : true;
            }
            $validity = apply_filters('wp_ability_validate_output', $is_valid, $output, $this->name);
            if (false === $validity) {
                return new WP_Error('ability_invalid_output', 'Invalid output.');
            }
            if (is_wp_error($validity)) {
                return $validity;
            }

            return true;
        }

        /** @param mixed $input Input. @return mixed */
        public function execute($input = null)
        {
            do_action('wp_ability_invoked', $this->name, $input, $this);

            $pre_execute_sentinel = new WP_Filter_Sentinel();

            $pre = apply_filters('wp_pre_execute_ability', $pre_execute_sentinel, $this->name, $input, $this);
            if ($pre !== $pre_execute_sentinel) {
                return $pre;
            }

            $input = $this->normalize_input($input);
            if (is_wp_error($input)) {
                return $input;
            }

            $is_valid = $this->validate_input($input);
            if (is_wp_error($is_valid)) {
                return $is_valid;
            }

            $has_permissions = $this->check_permissions($input);
            if (true !== $has_permissions) {
                return new WP_Error('ability_invalid_permissions', 'no permission');
            }

            do_action('wp_before_execute_ability', $this->name, $input, $this);

            $result = $this->do_execute($input);
            if (is_wp_error($result)) {
                return $result;
            }

            $is_valid = $this->validate_output($result);
            if (is_wp_error($is_valid)) {
                return $is_valid;
            }

            do_action('wp_after_execute_ability', $this->name, $input, $result, $this);

            return $result;
        }
    }
}
