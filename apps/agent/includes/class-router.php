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

    /**
     * Character budget for the exception reason echoed in a command-failure
     * response. The control plane clamps an agent error body to 512 bytes
     * (apps/api/internal/agentcmd/client.go), so anything longer is only cut
     * again there, mid-JSON; 200 leaves room for the code, the exception class
     * and the relative location alongside it.
     */
    private const MAX_REASON_CHARS = 200;

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
        // Read-only environment report. Uses authorizeCommand('info') so the
        // token is bound to both this site (aud) and this endpoint (cmd='info'),
        // matching the same binding that POST /command/info already enforces.
        register_rest_route(
            self::NAMESPACE,
            '/info',
            [
                'methods'             => 'GET',
                'callback'            => [$this, 'handleInfo'],
                'permission_callback' => fn ( \WP_REST_Request $r ) => $this->authorizeCommand( $r, 'info' ),
            ]
        );

        // Action commands dispatched by name. The {command} segment names the
        // action; it is threaded into verifyCommand() so the token is bound to
        // both the site (aud) and this specific command (cmd).
        // Pattern allows dots so dot-notation commands (e.g. objectcache.apply_config)
        // are reachable alongside underscore-only names (e.g. cache_purge).
        register_rest_route(
            self::NAMESPACE,
            '/command/(?P<command>[a-z0-9_.]+)',
            [
                'methods'             => 'POST',
                'callback'            => [$this, 'handleCommand'],
                'permission_callback' => fn ( \WP_REST_Request $r ) => $this->authorizeCommand( $r, (string) ( $r->get_param( 'command' ) ?? '' ) ),
                'args'                => [
                    'command' => [
                        'required'          => true,
                        // Strip anything outside the allowed set [a-z0-9_.] rather than
                        // using sanitize_key() which removes dots and breaks dot-notation
                        // command names like objectcache.apply_config.
                        'sanitize_callback' => static function ( string $v ): string {
                            return preg_replace( '/[^a-z0-9_.]/', '', strtolower( $v ) ) ?? '';
                        },
                    ],
                ],
            ]
        );
    }

    /**
     * Permission callback: verify the signed bearer token bound to a specific
     * command, then enforce WordPress capability as defense-in-depth.
     *
     * Every route must supply a non-empty $command so the token's `aud` (site)
     * and `cmd` (endpoint) claims are both checked. There is no unbound path —
     * the old verify()-only branch that allowed a token minted for any command
     * to reach /info has been removed (WP REST authorization best practice:
     * authenticate AND bind to the specific action).
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @param string                                $command Expected command name
     *                                                       (e.g. 'info', or the
     *                                                       {command} route param).
     * @return bool|\WP_Error True when authorized, WP_Error otherwise.
     */
    public function authorizeCommand(\WP_REST_Request $request, string $command)
    {
        $token = $this->bearerToken($request);
        if ($token === null) {
            return $this->forbidden('missing_token');
        }

        if ($command === '') {
            return $this->forbidden('missing_command');
        }

        try {
            // verifyCommand checks: Ed25519 signature, exp ≤ 60 s, jti anti-replay,
            // aud (this site's enrollment URL), AND cmd (this command name).
            $claims = $this->connector->verifyCommand($token, $command);
        } catch (\Throwable $e) {
            // Log the EXACT reason to debug.log (admin-visible, not secret-bearing —
            // verifyCommand exceptions only contain category messages like "aud
            // mismatch", "signature verification failed", "exp expired", etc.) and
            // surface a non-secret CATEGORY in the response so the control plane
            // (and the human reading logs) can tell aud_mismatch from sig_failed
            // without giving an attacker any cryptographic oracle beyond what they
            // already get from the 403 itself.
            $category = $this->classifyTokenError($e->getMessage());
            \WPMgr\Agent\Support\DebugLog::write('WPMgr Agent: command authorize failed: command=' . $command . ' category=' . $category . ' reason=' . $e->getMessage());
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

        // Strip the internal claims stash before any command sees the body.
        //
        // WP_REST_Request::set_param() does NOT write to a flat array. For a key
        // WordPress has not already seen it writes into the FIRST bucket of
        // get_parameter_order(), and on a request whose Content-Type is
        // application/json that first bucket is 'JSON' itself. authorizeCommand()
        // above stashes the verified claims with set_param(), so on every real
        // control-plane call (POST, Content-Type: application/json) the claims
        // land INSIDE the JSON bucket and come straight back out of
        // get_json_params(). Commands would then receive a wpmgr_claims key the
        // control plane never sent, which is exactly what made a `{}` body look
        // like a non-empty one. Commands must only ever see what was actually
        // transmitted.
        unset($params[self::ATTR_CLAIMS]);

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
            // Mirrors the authorizeCommand() catch above: log the EXACT reason
            // locally AND surface a non-secret summary in the response. Before
            // this, $e was caught and discarded outright, so every command
            // failure — backup, update, restore, anything — reached the control
            // plane as the same opaque "Command execution failed." and nothing
            // was written anywhere at all, not even under WPMGR_DEBUG. See #754.
            //
            // dispatch() runs only after authorizeCommand(), i.e. after Ed25519
            // signature, expiry, anti-replay, aud (this site) and cmd (this
            // endpoint) all verified — so this response goes to the control
            // plane, never to an anonymous caller.
            $class    = get_class($e);
            $location = self::relativeSourcePath($e->getFile()) . ':' . $e->getLine();

            // The local log is the site owner's own debug.log on their own
            // server, so it carries full detail, including the raw message and
            // the absolute file path PHP reports. That one line is what turns a
            // weeks-long investigation into a minute.
            \WPMgr\Agent\Support\DebugLog::write(
                'WPMgr Agent: command failed: command=' . $name
                . ' class=' . $class
                . ' at=' . $location
                . ' reason=' . $e->getMessage()
            );

            // The response is transmitted, stored by the control plane and
            // rendered in a dashboard, so it gets a REDACTED, length-capped
            // reason instead.
            //
            // Why sanitise rather than classify: authorizeCommand() can use a
            // needle table because Connector::verifyCommand throws a small
            // CLOSED set of category messages. Commands do not — they throw
            // from 200+ sites and roughly 40% of those interpolate a runtime
            // value, very often an absolute path (backup scratch base, restore
            // staging dir). A needle table over an open set would be guesswork
            // that silently goes stale, so the reason is sanitised and the
            // stable machine-readable part is the exception class.
            return new \WP_Error(
                'wpmgr_command_failed',
                'Command execution failed: ' . $class . ': ' . self::redactReason($e->getMessage()),
                [
                    'status'    => 500,
                    'command'   => $name,
                    'exception' => $class,
                    'at'        => $location,
                ]
            );
        }

        return new \WP_REST_Response($result, 200);
    }

    /**
     * Strip a known WordPress/plugin root off an absolute path, never returning
     * an absolute one.
     *
     * Matching is ANCHORED (strncmp against a root + separator), not a strpos
     * containment test: a path that merely mentions the root somewhere in the
     * middle is not under it.
     *
     * @param string $file Absolute path as reported by PHP.
     * @return string Root-relative path, or a bare filename when the file lives
     *                outside every known root.
     */
    private static function relativeSourcePath(string $file): string
    {
        if ($file === '') {
            return '(unknown)';
        }

        $normalised = str_replace('\\', '/', $file);

        foreach (self::knownRoots() as $root) {
            $prefix = str_replace('\\', '/', $root) . '/';
            if (strncmp($normalised, $prefix, strlen($prefix)) === 0) {
                return substr($normalised, strlen($prefix));
            }
        }

        // Outside every known root: a bare filename discloses no host layout.
        return basename($normalised);
    }

    /**
     * Reduce an exception message to something safe to transmit.
     *
     * Order matters: paths under a known root are rewritten to a useful
     * root-relative form FIRST, so that the catch-all absolute-path redaction
     * that follows only fires on paths outside the WordPress tree — which are
     * pure host-layout disclosure and carry no diagnostic value.
     *
     * @param string $msg Raw exception message.
     * @return string Redacted, single-line, length-capped message.
     */
    private static function redactReason(string $msg): string
    {
        // One line: a reason is not a stack trace. Patterns here are deliberately
        // byte-oriented (no /u): on invalid UTF-8 a /u pattern returns null and
        // would silently erase the whole message.
        $msg = self::pregOrKeep('/[\x00-\x1F\x7F]+/', ' ', $msg);
        $msg = trim(self::pregOrKeep('/\s+/', ' ', $msg));

        // Rewrite paths under a known root to a root-relative form, so
        // "wp-content/uploads/wpmgr/scratch" survives but the
        // "/home/customer123/public_html" that preceded it does not.
        // The separator is part of the needle, deliberately. Stripping a bare
        // root would be an UNANCHORED prefix match: with a root of
        // "/var/www/site", the unrelated "/var/www/site2/secret" would come out
        // as "2/secret" — a surviving path fragment that the absolute-path
        // redaction below can no longer catch, because its leading "/" is gone.
        // Requiring the separator leaves such a path fully absolute, so it
        // reaches that redaction and is dropped whole.
        foreach (self::knownRoots() as $root) {
            $msg = str_replace($root . '/', '', $msg);
        }

        // Anything STILL absolute is outside the WordPress tree: drop it whole.
        // Covers POSIX (/a/b) and Windows (C:\a\b). The negative lookbehind
        // keeps "and/or" and "HTTP 500" intact.
        $msg = self::pregOrKeep(
            '~(?<![A-Za-z0-9_.\-])(?:[A-Za-z]:[\\\\/]|/)[A-Za-z0-9_.\-]+[A-Za-z0-9_.\-/\\\\]*~',
            '<path>',
            $msg
        );

        // Redact long opaque runs: the shape of a key, token, hash or
        // ciphertext. Threshold-only, with no "looks random" refinement, and
        // that asymmetry is deliberate — a false positive costs one long
        // identifier that is still intact in the local debug log, a false
        // negative puts key material in a dashboard.
        //
        // '/' is deliberately NOT in the class. It was, and it ate any relative
        // path longer than the threshold ("wp-content/uploads/wpmgr/keystore"
        // is 33 characters of that alphabet), destroying the very diagnostic
        // this change exists to deliver. The alphabet covers the encodings the
        // agent's own secrets actually use: base64url, hex and bech32.
        //
        // This is a backstop, not a boundary. The boundary is that a command
        // must not put a secret in an exception message in the first place.
        $msg = self::pregOrKeep('~[A-Za-z0-9+=_\-]{32,}~', '<redacted>', $msg);

        if ($msg === '') {
            return '(no message)';
        }

        return self::clamp($msg, self::MAX_REASON_CHARS);
    }

    /**
     * preg_replace that keeps the subject when the engine bails (null return on
     * a backtrack limit or malformed input) instead of erasing it.
     *
     * @param string $pattern     Pattern.
     * @param string $replacement Replacement.
     * @param string $subject     Subject.
     * @return string
     */
    private static function pregOrKeep(string $pattern, string $replacement, string $subject): string
    {
        $out = preg_replace($pattern, $replacement, $subject);

        return is_string($out) ? $out : $subject;
    }

    /**
     * Truncate to a character budget without splitting a UTF-8 sequence, which
     * would make the enclosing REST response invalid JSON.
     *
     * @param string $value Value to clamp.
     * @param int    $max   Maximum length.
     * @return string
     */
    private static function clamp(string $value, int $max): string
    {
        if (function_exists('mb_strlen') && function_exists('mb_substr')) {
            if (mb_strlen($value, 'UTF-8') <= $max) {
                return $value;
            }
            return mb_substr($value, 0, $max, 'UTF-8') . '...(truncated)';
        }

        if (strlen($value) <= $max) {
            return $value;
        }

        // No mbstring: cut on bytes, then drop a trailing partial sequence.
        $cut = substr($value, 0, $max);

        return self::pregOrKeep('/[\xC0-\xFF][\x80-\xBF]*$/', '', $cut) . '...(truncated)';
    }

    /**
     * Absolute filesystem roots whose prefix may be stripped, longest first.
     *
     * A blank or filesystem-root value is rejected rather than used: stripping
     * '' or '/' as a prefix matches everything, which is the same class of
     * defect as an empty base-path fallback. The plugin's own directory is
     * self-resolved from __DIR__ so this works with or without the constant.
     *
     * @return array<int,string>
     */
    private static function knownRoots(): array
    {
        // includes/ -> plugin root. __DIR__ is always defined; no empty fallback.
        $roots = [dirname(__DIR__)];

        foreach (['WPMGR_AGENT_DIR', 'WP_CONTENT_DIR', 'ABSPATH'] as $name) {
            if (!defined($name)) {
                continue;
            }
            $value = constant($name);
            if (!is_string($value)) {
                continue;
            }
            $value = rtrim($value, '/\\');
            if ($value === '' || $value === '.') {
                continue;
            }
            $roots[] = $value;
        }

        $roots = array_values(array_unique($roots));

        // Longest first: the plugin dir sits under WP_CONTENT_DIR, which sits
        // under ABSPATH, so a shorter root must never win the replace.
        usort($roots, static function (string $a, string $b): int {
            return strlen($b) <=> strlen($a);
        });

        return $roots;
    }

    /**
     * Extract a bearer token from the Authorization header.
     *
     * Consults AuthHeaderShield first: on a normal request the header was
     * already relocated out of $_SERVER at plugin-include time (see
     * wpmgr-agent.php), specifically so third-party global auth filters never
     * see it, which also means WordPress never populated it back onto the
     * WP_REST_Request object. The live request header is still checked as a
     * fallback, unchanged, for any path that never went through the shield
     * (cookie-authenticated REST calls, or a test that injects the header
     * directly).
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return string|null
     */
    private function bearerToken(\WP_REST_Request $request): ?string
    {
        $stashed = \WPMgr\Agent\Support\AuthHeaderShield::bearer();
        if (is_string($stashed) && $stashed !== '') {
            return $stashed;
        }

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
