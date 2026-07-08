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
    class WP_REST_Request
    {
        /** @var array<string,mixed> */
        private array $params = [];

        /** @var array<string,string> */
        private array $headers = [];

        /**
         * @param array<string,mixed> $params Initial params.
         */
        public function __construct(array $params = [])
        {
            $this->params = $params;
        }

        public function get_param(string $key): mixed
        {
            return $this->params[$key] ?? null;
        }

        public function set_param(string $key, mixed $value): void
        {
            $this->params[$key] = $value;
        }

        public function get_header(string $key): string
        {
            return $this->headers[strtolower($key)] ?? '';
        }

        /**
         * @return array<string,mixed>
         */
        public function get_json_params(): array
        {
            return [];
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
