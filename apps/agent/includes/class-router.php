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
     * Character cap on the redacted reason, applied first. It is a readability
     * cap, not the size guarantee: characters are not bytes on the wire. The
     * guarantee is MAX_FAILURE_BODY_BYTES.
     */
    private const MAX_REASON_CHARS = 200;

    /**
     * Byte budget for the WHOLE command-failure response body, as the REST
     * server encodes it. The control plane keeps only the first 512 bytes of a
     * non-2xx agent response body (apps/api/internal/agentcmd/client.go), and
     * the encoding escapes every '/' and every non-ASCII character, so a
     * 200-character reason can still be several times that. fitFailureBody()
     * keeps the body inside this budget, shortening the reason first, so that
     * data.exception and data.at are what survive.
     */
    private const MAX_FAILURE_BODY_BYTES = 512;

    /** WP_Error code of a command-failure response. */
    private const FAILURE_CODE = 'wpmgr_command_failed';

    /**
     * Minimum upper- and lower-case letters a run needs before redactReason()
     * will treat it as encoded material rather than a path. See isEncodedRun().
     */
    private const MIN_MIXED_CASE = 6;

    /**
     * Length, '/' included, at which a case-mixed run that contains a '/'
     * counts as encoded material with no digit, '+' or '=' in it. See
     * isEncodedRun().
     */
    private const ENCODED_SLASH_RUN = 40;

    /**
     * What a command-failure response carries in place of the reason when a
     * redaction pass in redactReason() cannot complete, and what the local
     * log line carries in place of the message when logReason() cannot. The
     * response still names the exception class and the relative location;
     * only the message is withheld.
     */
    private const REASON_WITHHELD = '(reason withheld: redaction could not complete)';

    /**
     * What a router log line becomes when escapeControlChars() cannot
     * complete. Fixed and self-contained — never built from any part of the
     * line that failed to escape, because that line is exactly the untrusted
     * value the pass could not make safe to write.
     */
    private const LOG_LINE_WITHHELD = 'WPMgr Agent: log line withheld (control-character escaping could not complete).';

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
            // Build and escape the line only when the channel would actually
            // write it — escapeControlChars() runs a preg pass over the whole
            // line, and there is no reason to pay for that on a production
            // install with debug logging off.
            if (\WPMgr\Agent\Support\DebugLog::isEnabled()) {
                \WPMgr\Agent\Support\DebugLog::write(
                    self::escapeControlChars(
                        'WPMgr Agent: command authorize failed: command=' . $command . ' category=' . $category . ' reason=' . $e->getMessage()
                    ) ?? self::LOG_LINE_WITHHELD
                );
            }
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
            $class    = self::exceptionClass($e);
            $location = self::validUtf8(self::relativeSourcePath($e->getFile()) . ':' . $e->getLine());

            // The local log is the site owner's own debug.log, so it carries
            // more than the response: the command, the class, the location and
            // the message with its paths as thrown, absolute ones included.
            // Key material is redacted from it by the same passes the response
            // uses (see logReason()), because a debug.log can be readable over
            // the web. That one line is what turns a weeks-long investigation
            // into a minute.
            //
            // One failure is one log line. Every control character that could
            // pass for a line break, hide output on a terminal, or truncate the
            // line is written as a visible escape by escapeControlChars(): the
            // text survives, and nothing in it can start a log line of its own
            // or make part of a real one disappear.
            // Build and escape the line only when the channel would actually
            // write it — logReason() and escapeControlChars() each run preg
            // passes over the whole line, and a huge control-heavy message
            // must not pay for that on a production install with debug
            // logging off.
            if (\WPMgr\Agent\Support\DebugLog::isEnabled()) {
                \WPMgr\Agent\Support\DebugLog::write(
                    self::escapeControlChars(
                        'WPMgr Agent: command failed: command=' . $name
                        . ' class=' . $class
                        . ' at=' . $location
                        . ' reason=' . self::logReason($e->getMessage())
                    ) ?? self::LOG_LINE_WITHHELD
                );
            }

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
            [$message, $data] = self::fitFailureBody(
                $class,
                self::redactReason($e->getMessage()),
                [
                    'status'    => 500,
                    'command'   => $name,
                    'exception' => $class,
                    'at'        => $location,
                ]
            );

            return new \WP_Error(self::FAILURE_CODE, $message, $data);
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
     * Fit a command-failure response inside MAX_FAILURE_BODY_BYTES.
     *
     * Contract: for any input, the body as the REST server encodes it — this
     * code, message and data, JSON with no flags — is at most
     * MAX_FAILURE_BODY_BYTES. The reason gives way first: it is shortened, UTF-8
     * safely, to the longest prefix that fits, then dropped, then the class is
     * dropped from the message. data.exception and data.at are sent intact
     * whenever the body fits with a bare message; only a class name or
     * location that would overflow the budget on its own is shortened, from
     * the front, keeping its tail.
     *
     * @param string $class  Exception class, already valid UTF-8.
     * @param string $reason Redacted reason.
     * @param array{status:int,command:string,exception:string,at:string} $data Error data.
     * @return array{0:string,1:array{status:int,command:string,exception:string,at:string}}
     */
    private static function fitFailureBody(string $class, string $reason, array $data): array
    {
        $reason = self::validUtf8($reason);
        $prefix = 'Command execution failed: ' . $class;

        $message = $prefix . ': ' . $reason;
        if (self::failureBodyBytes($message, $data) <= self::MAX_FAILURE_BODY_BYTES) {
            return [$message, $data];
        }

        // The longest reason prefix that fits. Encoded size never shrinks as
        // the prefix grows, so a binary search over its length is exact.
        $best = null;
        $low  = 0;
        $high = self::charLength($reason) - 1;
        while ($low <= $high) {
            $mid       = intdiv($low + $high, 2);
            $candidate = $prefix . ': ' . self::clamp($reason, $mid);
            if (self::failureBodyBytes($candidate, $data) <= self::MAX_FAILURE_BODY_BYTES) {
                $best = $candidate;
                $low  = $mid + 1;
            } else {
                $high = $mid - 1;
            }
        }
        if ($best !== null) {
            return [$best, $data];
        }

        foreach ([$prefix, 'Command execution failed.'] as $message) {
            if (self::failureBodyBytes($message, $data) <= self::MAX_FAILURE_BODY_BYTES) {
                return [$message, $data];
            }
        }

        // Only a class name or location that alone overflows the budget gets
        // here. Keep the tail of the longer one, halving until the body fits.
        $original = ['exception' => $data['exception'], 'at' => $data['at']];
        $keep     = [
            'exception' => self::charLength($data['exception']),
            'at'        => self::charLength($data['at']),
        ];
        while (self::failureBodyBytes($message, $data) > self::MAX_FAILURE_BODY_BYTES) {
            $key = $keep['exception'] >= $keep['at'] ? 'exception' : 'at';
            if ($keep[$key] === 0) {
                break;
            }
            $keep[$key] = intdiv($keep[$key], 2);
            $data[$key] = '...' . self::tail($original[$key], $keep[$key]);
        }

        return [$message, $data];
    }

    /**
     * Size in bytes of a command-failure body as the REST server sends it: an
     * error response is this code, message and data, JSON-encoded with no
     * flags, so every '/' and every non-ASCII character is escaped.
     *
     * @param string $message Message.
     * @param array{status:int,command:string,exception:string,at:string} $data Error data.
     * @return int
     */
    private static function failureBodyBytes(string $message, array $data): int
    {
        $body = [
            'code'    => self::FAILURE_CODE,
            'message' => $message,
            'data'    => $data,
        ];
        $json = wp_json_encode($body);

        return is_string($json) ? strlen($json) : PHP_INT_MAX;
    }

    /**
     * The exception's class name as it may be reported. An anonymous class's
     * runtime name carries a NUL byte followed by where it was declared; only
     * the part before the NUL names the class.
     *
     * @param \Throwable $e Exception.
     * @return string
     */
    private static function exceptionClass(\Throwable $e): string
    {
        $class = get_class($e);
        $nul   = strpos($class, "\0");

        return self::validUtf8($nul === false ? $class : substr($class, 0, $nul));
    }

    /**
     * Write CR and LF as the visible sequences \r and \n, so a value occupies
     * exactly one log line and keeps its full text.
     *
     * @param string $line Line to fold.
     * @return string
     */
    private static function foldLineBreaks(string $line): string
    {
        return strtr($line, ["\r\n" => '\r\n', "\r" => '\r', "\n" => '\n']);
    }

    /**
     * Make a router log line safe to write as one line to a plain-text file,
     * whatever an exception message threw into it. Shared by every router log
     * line that carries one — the command-failure line and the
     * authorize-failure line both call this and nothing else.
     *
     * CR and LF fold first, via foldLineBreaks(), to the two-character visible
     * sequences \r and \n. Every other character that a log reader, a
     * terminal, or PHP's own error_log() could treat specially is then
     * escaped: the rest of the C0 control range (tab excepted — a literal tab
     * is harmless and common in a message), DEL, the C1 control range
     * (U+0080-U+009F, which includes NEL, itself a line break to some
     * readers), and the two Unicode line/paragraph separators U+2028 and
     * U+2029, which some terminals and log viewers also treat as breaking a
     * line. A C0/DEL byte is written as \xHH; the others, which only exist as
     * multi-byte UTF-8 sequences, are written as \u{HHHH}.
     *
     * NUL is included for a second reason beyond "looks like a line break":
     * PHP's error_log() writes to a C string under the hood, so a raw NUL
     * silently truncates everything written after it. Escaping it here is
     * what keeps the rest of the line from being lost.
     *
     * Byte-oriented throughout, like every pattern in redactReason(): the
     * match patterns are literal byte sequences, not a /u pattern, so the
     * pass runs the same whether or not $line is valid UTF-8, and a stray
     * byte elsewhere in the line (e.g. inside a CJK character or a truncated
     * multi-byte sequence) is never mistaken for one of these because the
     * multi-byte alternatives only match their exact lead-byte-and-successor
     * shape.
     *
     * A C1 control only matches the pattern above in its valid two-byte UTF-8
     * encoding, \xC2 followed by 0x80-0x9F. A lone invalid byte in that same
     * numeric range — e.g. a bare 0x85 (NEL) or 0x9B (an 8-bit CSI
     * introducer), neither preceded by a \xC2 lead byte — is not that
     * sequence and would otherwise pass through unescaped, and some
     * terminals and log readers act on such a byte directly regardless of
     * whether the surrounding text is valid UTF-8. So $line is checked for
     * UTF-8 validity first (the same preg_match('//u', ...) idiom
     * validUtf8() uses); on an invalid line every byte >= 0x80 is escaped as
     * \xHH, in addition to the C0/C1/separator classes above — including any
     * byte that is part of what would otherwise be a legitimate multi-byte
     * sequence elsewhere on that same line, since a line that failed
     * validation cannot be trusted to parse correctly byte-by-byte at all.
     * A line that IS valid UTF-8 never takes this branch, so ordinary
     * multi-byte text (CJK, emoji) is left untouched exactly as before.
     *
     * Fails closed like the redaction passes it sits next to: null when the
     * pass cannot complete, so the caller can withhold a fixed marker instead
     * of writing a line this function was unable to make safe.
     *
     * @param string $line Line to escape.
     * @return string|null Null when the pass did not complete.
     */
    private static function escapeControlChars(string $line): ?string
    {
        $line = self::foldLineBreaks($line);

        $pattern = preg_match('//u', $line) === 1
            ? '/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]|\xC2[\x80-\x9F]|\xE2\x80[\xA8\xA9]/'
            : '/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]|\xC2[\x80-\x9F]|\xE2\x80[\xA8\xA9]|[\x80-\xFF]/';

        return self::pregCallbackOrNull(
            $pattern,
            static function (array $m): string {
                $seq = $m[0];

                if (strlen($seq) === 1) {
                    // C0 control (tab, CR, LF excepted), DEL, or — on a line
                    // that failed UTF-8 validation only — any other single
                    // byte >= 0x80 caught by the catch-all alternative.
                    return sprintf('\\x%02X', ord($seq));
                }

                if (strlen($seq) === 2) {
                    // C1 control, UTF-8 encoded as \xC2 followed by a byte
                    // that is numerically the codepoint itself (0x80-0x9F).
                    return sprintf('\\u{%04X}', ord($seq[1]));
                }

                // U+2028 LINE SEPARATOR or U+2029 PARAGRAPH SEPARATOR.
                return $seq[2] === "\xA8" ? '\\u{2028}' : '\\u{2029}';
            },
            $line
        );
    }

    /**
     * Return the value unchanged when it is valid UTF-8; otherwise with each
     * invalid sequence replaced by U+FFFD, so it encodes to JSON at a size
     * that can be measured in advance.
     *
     * @param string $value Value.
     * @return string
     */
    private static function validUtf8(string $value): string
    {
        if (preg_match('//u', $value) === 1) {
            return $value;
        }

        $json    = wp_json_encode($value, JSON_INVALID_UTF8_SUBSTITUTE);
        $decoded = is_string($json) ? json_decode($json) : null;

        return is_string($decoded) ? $decoded : '';
    }

    /**
     * Length in characters when mbstring is present, else in bytes — the
     * same unit clamp() cuts in.
     *
     * @param string $value Value.
     * @return int
     */
    private static function charLength(string $value): int
    {
        return function_exists('mb_strlen') ? (int) mb_strlen($value, 'UTF-8') : strlen($value);
    }

    /**
     * The last $chars characters of a value, without splitting a UTF-8
     * sequence.
     *
     * @param string $value Value.
     * @param int    $chars Characters to keep.
     * @return string
     */
    private static function tail(string $value, int $chars): string
    {
        if ($chars <= 0) {
            return '';
        }
        if (function_exists('mb_substr')) {
            return (string) mb_substr($value, -$chars, null, 'UTF-8');
        }

        // No mbstring: cut on bytes, then drop leading continuation bytes.
        return ltrim(substr($value, -$chars), "\x80..\xBF");
    }

    /**
     * Reduce an exception message to something safe to transmit.
     *
     * Order matters throughout, and each step's comment says why it sits where
     * it does. In outline: paths under a known root are rewritten to a useful
     * root-relative form FIRST, so that the catch-all absolute-path redaction
     * only fires on paths outside the WordPress tree — which are pure
     * host-layout disclosure and carry no diagnostic value. Encoded material is
     * then decided BEFORE that path redaction, so a token that contains a
     * separator is disposed of whole rather than partly rewritten.
     *
     * Fails closed. If any of the three redaction passes cannot complete, the
     * caller gets REASON_WITHHELD in place of the whole reason — never the
     * message as it stood before that pass, and never a partly redacted one.
     *
     * @param string $msg Raw exception message.
     * @return string Redacted, single-line, length-capped message, or
     *                REASON_WITHHELD.
     */
    private static function redactReason(string $msg): string
    {
        // One line: a reason is not a stack trace. Patterns here are deliberately
        // byte-oriented (no /u): on invalid UTF-8 a /u pattern returns null and
        // would silently erase the whole message.
        //
        // These two only reshape whitespace, so they may keep their input if
        // the engine bails: every redaction pass after them still runs, and
        // those fail closed.
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
        // Requiring the separator leaves such a path fully absolute, so the
        // absolute-path redaction still recognises it from its root.
        // Both separators, because the needle has to match the message as PHP
        // wrote it. On Windows a root arrives as "C:\wp\site" and the message
        // carries backslashes, so a '/'-only needle never matches and the whole
        // path falls through to the absolute-path rule below — correct, but it
        // throws away the root-relative remainder that is the useful half. The
        // message is NOT globally normalised to do this: rewriting every '\'
        // would turn a namespaced class name into something the absolute-path
        // rule then eats.
        foreach (self::knownRoots() as $root) {
            $unix = str_replace('\\', '/', $root);
            $msg  = str_replace($unix . '/', '', $msg);
            $msg  = str_replace(str_replace('/', '\\', $unix) . '\\', '', $msg);
        }

        // Pass 1 — STANDARD base64, whose alphabet includes '/'. The agent's
        // own encrypted material is emitted in it (the keystore envelope, the
        // DB-fallback master key, the age header), so a rule whose alphabet
        // stops at '/' does not cover it: a run is cut at the separator rather
        // than seen whole. This pass matches over an alphabet that DOES include
        // '/' and then decides per run, so that a '/'-separated high-entropy
        // run is judged as one token.
        //
        // It runs BEFORE the absolute-path rule below, and that ordering is
        // load-bearing rather than incidental. Both rules can match the same
        // '/'-bearing run, and whichever fires first owns it. The path rule
        // matches from a separator to the end of a path-ish run, which is a
        // SUB-run of an encoded value, not the whole of it — so letting it go
        // first replaces the middle of a token and leaves the rest standing.
        // Deciding encoded material first means a token is disposed of whole,
        // and the path rule only ever sees what is genuinely a path.
        //
        // The decision is a shape test on the whole run, set out in
        // isEncodedRun(): a run that ends in base64 padding is always encoded
        // material; otherwise it is only when the run is substantially
        // case-MIXED, over the same 32-character budget (slashes excluded
        // from the count), and either carries a digit, '+' or '=', or is a
        // long run that contains a '/'. That
        // predicate is chosen for what it CANNOT match. Every path, table name,
        // option key and command name this plugin emits is single-case and none
        // ends in '=', so none of them can satisfy it, and
        // "wp-content/uploads/wpmgr/keystore" survives whole — which is the
        // diagnostic this change exists to deliver, and which an earlier attempt
        // destroyed by simply adding '/' to the alphabet below.
        //
        // Stated honestly in both directions. Not caught here: an unpadded run
        // that is not case-mixed, or that falls under the budget once its
        // slashes are set aside, or a short one that carries no digit, '+' or
        // '='. Caught here: a case-mixed path that carries a digit, '+' or '=',
        // a long case-mixed path with a '/' in it, and any run that ends in
        // '='; isEncodedRun() gives the exact lengths. The second is the
        // cheaper error — a redacted identifier costs one diagnostic; key
        // material in a dashboard or a web-readable log is not recoverable
        // from.
        //
        // A redacted run that starts with '/' keeps that '/', and the
        // absolute-path rule below takes "<redacted>" as a path component. So a
        // redaction never cuts an absolute path short: that rule reaches at
        // least as far along the path as it would with no redaction in it, and
        // the remainder goes with it rather than surviving as a relative-looking
        // tail. Neither pass can be abandoned part-way through the message:
        // each completes, or the whole reason is withheld.
        $msg = self::redactEncodedRuns($msg);
        if ($msg === null) {
            return self::REASON_WITHHELD;
        }

        // Anything STILL absolute is outside the WordPress tree: redact it,
        // from its root through every character that follows it and can sit
        // in a path here — letters, digits, '_', '.', '-', '+', both
        // separators, and "<redacted>". Covers POSIX (/a/b)
        // and Windows (C:\a\b). The negative lookbehind keeps "and/or" and
        // "HTTP 500" intact. "<redacted>" is accepted as a path component for
        // the reason given above pass 1.
        //
        // Stated as a limit: a component that carries any other character, a
        // space for one, ends the match there, and the rest of that path is
        // not recognised as absolute and survives. A space cannot be accepted
        // without also eating the prose that surrounds a path.
        //
        // Every quantifier is possessive: each piece has exactly one way to
        // match, so the rule has nothing to backtrack over.
        $msg = self::pregOrNull(
            '~(?<![A-Za-z0-9_.\-])(?:[A-Za-z]:[\\\\/]|/)(?:[A-Za-z0-9_.+\-]++|<redacted>)++(?:[A-Za-z0-9_.+\-/\\\\]++|<redacted>)*+~',
            '<path>',
            $msg
        );
        if ($msg === null) {
            return self::REASON_WITHHELD;
        }

        // Pass 2 — long opaque runs over the slash-free alphabet: the shape of
        // a key, token, hash or ciphertext. Threshold-only, with no "looks
        // random" refinement, and that asymmetry is deliberate — a false
        // positive costs one long identifier, a false negative puts key
        // material in a dashboard.
        //
        // '/' is deliberately NOT in this class. It was, and it ate any
        // relative path longer than the threshold
        // ("wp-content/uploads/wpmgr/keystore" is 33 characters of that
        // alphabet). The alphabet here covers base64url, hex and bech32; it
        // does NOT cover standard base64, which is pass 1's job.
        //
        // Both passes are a backstop, not a boundary. The boundary is that a
        // command must not put a secret in an exception message in the first
        // place.
        $msg = self::redactOpaqueRuns($msg);
        if ($msg === null) {
            return self::REASON_WITHHELD;
        }

        if ($msg === '') {
            return '(no message)';
        }

        return self::clamp($msg, self::MAX_REASON_CHARS);
    }

    /**
     * Pass 1 of redactReason(): the standard-base64 run decision. The comment
     * above its call there says why it exists and why it runs where it does.
     *
     * @param string $msg Message.
     * @return string|null Null when the pass did not complete.
     */
    private static function redactEncodedRuns(string $msg): ?string
    {
        return self::pregCallbackOrNull(
            '~[A-Za-z0-9+/=_\-]{32,}~',
            static function (array $m): string {
                if (!self::isEncodedRun($m[0])) {
                    return $m[0];
                }

                return ($m[0][0] === '/' ? '/' : '') . '<redacted>';
            },
            $msg
        );
    }

    /**
     * Pass 2 of redactReason(): long opaque runs over the slash-free alphabet.
     *
     * @param string $msg Message.
     * @return string|null Null when the pass did not complete.
     */
    private static function redactOpaqueRuns(string $msg): ?string
    {
        return self::pregOrNull('~[A-Za-z0-9+=_\-]{32,}~', '<redacted>', $msg);
    }

    /**
     * The exception message as the local debug log line records it.
     *
     * Contract: the log runs the SAME key-material passes the response gets
     * from redactReason() — redactEncodedRuns() then redactOpaqueRuns(), in
     * that order — minus the absolute-path rule that sits between them there
     * and collapses a whole path to "<path>". Without that rule, a path
     * stays as thrown here, absolute ones included: it is the site owner's
     * own diagnostic, and PHP writes it to the same log.
     *
     * That "stays as thrown" is a default, not a guarantee, and the two
     * passes decide it the same way they decide it for the response: over
     * the whole '/'-joined run, not one path segment at a time (see
     * isEncodedRun()). When a run is judged to be encoded material, the
     * WHOLE run is redacted — every path segment inside it, not only the
     * part that made it qualify — because pass 1's alphabet includes '/' and
     * treats a path and the token appended to it as one candidate. A run
     * that is not judged encoded material is left exactly as thrown. Control
     * characters are not this function's job; see escapeControlChars(). If
     * either pass cannot complete, the line carries REASON_WITHHELD in place
     * of the message.
     *
     * @param string $msg Raw exception message.
     * @return string
     */
    private static function logReason(string $msg): string
    {
        $msg = self::redactEncodedRuns($msg);
        if ($msg === null) {
            return self::REASON_WITHHELD;
        }

        return self::redactOpaqueRuns($msg) ?? self::REASON_WITHHELD;
    }

    /**
     * Decide whether a run over the standard-base64 alphabet is encoded
     * material rather than a path.
     *
     * Two ways to qualify. A run that ends in base64 padding ('=') qualifies
     * whatever its case mix, so a padded key — a 32-byte key encodes to 44
     * characters ending in '=' — is caught whenever it stands as its own run.
     * Any other run qualifies only when it is case-mixed (at least
     * MIN_MIXED_CASE letters of each case) over a 32-character budget that
     * ignores '/', AND either carries a digit, '+' or '=', or contains a '/'
     * and is at least ENCODED_SLASH_RUN characters long.
     *
     * What that leaves uncaught is an unpadded run with fewer than
     * MIN_MIXED_CASE letters of either case; one with fewer than 32 characters
     * once '/' is set aside; and one shorter than ENCODED_SLASH_RUN that
     * carries no digit, '+' or '='. An unpadded 32-byte key is long enough
     * that only the first two shapes apply to it. A run with no '/' at all
     * is left to the slash-free pass, which takes any 32-character run.
     *
     * The over-fire this accepts: a case-mixed run of ENCODED_SLASH_RUN
     * characters or more that contains a '/' qualifies with no digit or symbol
     * at all, so a long relative path with at least MIN_MIXED_CASE letters of
     * each case — a third-party class path, say — comes out as <redacted>.
     *
     * A single-case run qualifies only through the padding branch. Every path,
     * table name, option key and command name this plugin emits is single-case
     * and none ends in '=', so none of them qualifies.
     *
     * Byte-oriented by design, like every pattern in redactReason(): a
     * multibyte-aware test would have to trust the message to be valid UTF-8,
     * and an exception message is not guaranteed to be.
     *
     * @param string $run Candidate run.
     * @return bool True when the run should be redacted.
     */
    private static function isEncodedRun(string $run): bool
    {
        // Padding first, and before any count: nothing this plugin names ends
        // in '=', and the case-mix gate below is a likelihood, not a
        // guarantee — a padded key with fewer than MIN_MIXED_CASE letters of
        // one case would fail it.
        if (substr($run, -1) === '=') {
            return true;
        }

        $len   = 0;
        $upper = 0;
        $lower = 0;
        $digit = 0;

        for ($i = 0, $n = strlen($run); $i < $n; $i++) {
            $c = $run[$i];
            if ($c === '/') {
                continue;
            }
            $len++;
            if ($c >= 'A' && $c <= 'Z') {
                $upper++;
            } elseif ($c >= 'a' && $c <= 'z') {
                $lower++;
            } elseif ($c >= '0' && $c <= '9') {
                $digit++;
            }
        }

        if ($len < 32 || $upper < self::MIN_MIXED_CASE || $lower < self::MIN_MIXED_CASE) {
            return false;
        }

        // A digit, or '+' or '=' anywhere in the run, or a '/' in a run of at
        // least ENCODED_SLASH_RUN characters. Only the padding check above is
        // decisive on its own; this clause, like the gate before it, is a
        // likelihood.
        return $digit >= 1
            || strpbrk($run, '+=') !== false
            || (strlen($run) >= self::ENCODED_SLASH_RUN && strpos($run, '/') !== false);
    }

    /**
     * preg_replace_callback for a REDACTION pass: null when the engine bails,
     * so the caller withholds the reason instead of sending what the pass was
     * meant to redact.
     *
     * @param string               $pattern  Pattern.
     * @param callable(array<int,string>):string $callback Per-match decision.
     * @param string               $subject  Subject.
     * @return string|null Null when the pass did not complete.
     */
    private static function pregCallbackOrNull(string $pattern, callable $callback, string $subject): ?string
    {
        $out = preg_replace_callback($pattern, $callback, $subject);

        return is_string($out) ? $out : null;
    }

    /**
     * preg_replace for a REDACTION pass: null when the engine bails, so the
     * caller withholds the reason instead of sending what the pass was meant
     * to redact.
     *
     * @param string $pattern     Pattern.
     * @param string $replacement Replacement.
     * @param string $subject     Subject.
     * @return string|null Null when the pass did not complete.
     */
    private static function pregOrNull(string $pattern, string $replacement, string $subject): ?string
    {
        $out = preg_replace($pattern, $replacement, $subject);

        return is_string($out) ? $out : null;
    }

    /**
     * preg_replace that keeps the subject when the engine bails (null return on
     * a backtrack limit or malformed input) instead of erasing it. Only for
     * steps that redact nothing themselves: whitespace reshaping ahead of the
     * redaction passes, and trimming a clamped result that is already redacted.
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
     * A blank or filesystem-root value is rejected rather than used: '', '.',
     * '/' and a bare drive root such as 'D:\' or 'D:/'. Stripping a filesystem
     * root as a prefix matches every absolute path on that filesystem and
     * leaves each as a relative-looking remainder, which is the same class of
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
            // '/' trims to ''; a drive root ('D:\', 'D:/') trims to 'D:'.
            $value = rtrim($value, '/\\');
            if ($value === '' || $value === '.' || preg_match('/^[A-Za-z]:$/', $value) === 1) {
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
