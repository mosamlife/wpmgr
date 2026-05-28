<?php
/**
 * Router: registers REST routes under the wpmgr/v1 namespace and dispatches
 * verified requests to command handlers.
 *
 * The ONLY public API surface is register_rest_route('wpmgr/v1', ...). No
 * admin-ajax, no custom rewrites. Every route's permission_callback runs the
 * Connector's Ed25519 + anti-replay verification before the handler executes.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Commands\CommandInterface;

/**
 * Registers and dispatches the agent's signed REST API.
 */
final class Router
{
    /** REST namespace. */
    public const NAMESPACE = 'wpmgr/v1';

    /** Request attribute key under which validated claims are stashed. */
    private const ATTR_CLAIMS = 'wpmgr_claims';

    private Connector $connector;

    /** @var array<string,CommandInterface> Map of command name => handler. */
    private array $commands;

    /**
     * @param Connector                       $connector Auth verifier.
     * @param array<int,CommandInterface>     $commands  Command handlers.
     */
    public function __construct(Connector $connector, array $commands)
    {
        $this->connector = $connector;

        $this->commands = [];
        foreach ($commands as $command) {
            $this->commands[$command->name()] = $command;
        }
    }

    /**
     * Hook point: register all REST routes. Bind to the rest_api_init action.
     *
     * @return void
     */
    public function registerRoutes(): void
    {
        // Read-only environment report.
        register_rest_route(
            self::NAMESPACE,
            '/info',
            [
                'methods'             => 'GET',
                'callback'            => [$this, 'handleInfo'],
                'permission_callback' => [$this, 'authorize'],
            ]
        );

        // Action commands (skeleton stubs) dispatched by name.
        register_rest_route(
            self::NAMESPACE,
            '/command/(?P<command>[a-z0-9_-]+)',
            [
                'methods'             => 'POST',
                'callback'            => [$this, 'handleCommand'],
                'permission_callback' => [$this, 'authorize'],
                'args'                => [
                    'command' => [
                        'required'          => true,
                        'sanitize_callback' => 'sanitize_key',
                    ],
                ],
            ]
        );
    }

    /**
     * Permission callback: verify the signed bearer token, then enforce
     * WordPress capability as defense-in-depth.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return bool|\WP_Error True when authorized, WP_Error otherwise.
     */
    public function authorize(\WP_REST_Request $request)
    {
        $token = $this->bearerToken($request);
        if ($token === null) {
            return $this->forbidden('missing_token');
        }

        // For the command route the {command} segment names the action being
        // invoked; thread it into verification so the token's `cmd` claim is
        // bound to THIS command and its `aud` claim is bound to THIS site.
        $command = $request->get_param('command');
        $command = is_string($command) ? $command : '';

        try {
            if ($command !== '') {
                $claims = $this->connector->verifyCommand($token, $command);
            } else {
                $claims = $this->connector->verify($token);
            }
        } catch (\Throwable $e) {
            // Log the EXACT reason to debug.log (admin-visible, not secret-bearing —
            // verifyCommand exceptions only contain category messages like "aud
            // mismatch", "signature verification failed", "exp expired", etc.) and
            // surface a non-secret CATEGORY in the response so the control plane
            // (and the human reading logs) can tell aud_mismatch from sig_failed
            // without giving an attacker any cryptographic oracle beyond what they
            // already get from the 403 itself.
            $category = $this->classifyTokenError($e->getMessage());
            error_log('WPMgr Agent: command authorize failed: command=' . $command . ' category=' . $category . ' reason=' . $e->getMessage());
            return $this->forbidden($category);
        }

        // Defense-in-depth: where a WP user context applies, require manage_options.
        if (function_exists('current_user_can') && function_exists('is_user_logged_in')) {
            if (is_user_logged_in() && !current_user_can('manage_options')) {
                return $this->forbidden('insufficient_capability');
            }
        }

        // Stash validated claims for the handler.
        $request->set_param(self::ATTR_CLAIMS, $claims);

        return true;
    }

    /**
     * GET /wpmgr/v1/info handler.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return \WP_REST_Response|\WP_Error
     */
    public function handleInfo(\WP_REST_Request $request)
    {
        $claims = $this->claims($request);

        return $this->dispatch('info', $claims, []);
    }

    /**
     * POST /wpmgr/v1/command/{command} handler.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return \WP_REST_Response|\WP_Error
     */
    public function handleCommand(\WP_REST_Request $request)
    {
        $claims = $this->claims($request);
        $name   = (string) $request->get_param('command');

        $params = $request->get_json_params();
        if (!is_array($params)) {
            $params = [];
        }

        return $this->dispatch($name, $claims, $params);
    }

    /**
     * Execute a named command and wrap the result in a REST response.
     *
     * @param string               $name   Command name.
     * @param array<string,mixed>  $claims Validated claims.
     * @param array<string,mixed>  $params Request params.
     * @return \WP_REST_Response|\WP_Error
     */
    private function dispatch(string $name, array $claims, array $params)
    {
        if (!isset($this->commands[$name])) {
            return new \WP_Error('wpmgr_unknown_command', 'Unknown command.', ['status' => 404]);
        }

        try {
            $result = $this->commands[$name]->execute($claims, $params);
        } catch (\Throwable $e) {
            return new \WP_Error('wpmgr_command_failed', 'Command execution failed.', ['status' => 500]);
        }

        return new \WP_REST_Response($result, 200);
    }

    /**
     * Extract a bearer token from the Authorization header.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return string|null
     */
    private function bearerToken(\WP_REST_Request $request): ?string
    {
        $header = (string) $request->get_header('authorization');
        if ($header === '') {
            return null;
        }

        if (stripos($header, 'Bearer ') !== 0) {
            return null;
        }

        $token = trim(substr($header, 7));

        return $token === '' ? null : $token;
    }

    /**
     * Retrieve validated claims previously stashed by authorize().
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return array<string,mixed>
     */
    private function claims(\WP_REST_Request $request): array
    {
        $claims = $request->get_param(self::ATTR_CLAIMS);

        return is_array($claims) ? $claims : [];
    }

    /**
     * Build a uniform 403 error.
     *
     * @param string $code Machine code.
     * @return \WP_Error
     */
    private function forbidden(string $code): \WP_Error
    {
        return new \WP_Error('wpmgr_' . $code, 'Forbidden.', ['status' => 403]);
    }

    /**
     * Map a Connector::verifyCommand RuntimeException message to a non-secret
     * public category. Exception messages are operator-facing category strings
     * ("aud mismatch", "signature verification failed", etc.) — exposing them as
     * codes gives no cryptographic oracle beyond what the 403 status itself
     * already gives, but it makes "agent rejected the command" diagnosable in
     * one shot from CP/agent logs.
     *
     * @param string $msg The RuntimeException message text.
     * @return string Short snake_case code (prefixed with `wpmgr_` by forbidden()).
     */
    private function classifyTokenError(string $msg): string
    {
        $needles = [
            'signature verification failed' => 'sig_failed',
            'invalid signature length'      => 'sig_failed',
            'invalid public key length'     => 'sig_failed',
            'malformed jwt'                 => 'malformed_jwt',
            'invalid alg'                   => 'malformed_jwt',
            'missing exp'                   => 'missing_exp',
            'expired'                       => 'token_expired',
            'too far in future'             => 'token_skew',
            'replay'                        => 'token_replay',
            'missing jti'                   => 'missing_jti',
            'site not enrolled'             => 'site_not_enrolled',
            'missing aud'                   => 'missing_aud',
            'aud mismatch'                  => 'aud_mismatch',
            'missing cmd'                   => 'missing_cmd',
            'cmd mismatch'                  => 'cmd_mismatch',
        ];
        $lower = strtolower($msg);
        foreach ($needles as $needle => $code) {
            if (strpos($lower, $needle) !== false) {
                return $code;
            }
        }
        return 'invalid_token';
    }
}
