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

        public function get_error_code(): string
        {
            $codes = array_keys($this->errors);
            return $codes === [] ? '' : (string) $codes[0];
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

        /** @var array<int,string> */
        public array $roles = [];
    }
}
