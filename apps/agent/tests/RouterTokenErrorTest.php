<?php
/**
 * RouterTokenErrorTest: #679, a refused command token answers with the code
 * for the check that actually refused it.
 *
 * The control plane turns that code into the next step it shows a person:
 * fix the server clock, reconnect the site, or report a fault. A code that
 * lumps several causes together leaves that person with nothing to act on.
 *
 * Everything here takes the production path: a real Ed25519 keypair, a real
 * Keystore, Settings and Connector, a real Router, and a request built the way
 * the REST server builds one, handed to Router::authorizeCommand(), which is
 * the route's permission_callback.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use ReflectionProperty;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use WPMgr\Agent\TokenFailure;
use WPMgr\Agent\TokenRejected;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Router
 * @covers \WPMgr\Agent\Connector
 * @covers \WPMgr\Agent\TokenFailure
 * @covers \WPMgr\Agent\TokenRejected
 */
final class RouterTokenErrorTest extends TestCase
{
    /** Command every scenario's token is minted for and presented to. */
    private const CMD = 'update';

    /** Route registered by Router::registerRoutes(), with the segment filled in. */
    private const ROUTE_BASE = '/wpmgr/v1/command/';

    private string $keyFile;

    /** @var array<string,mixed> In-memory wp-options. */
    private array $options = [];

    /** Control-plane Ed25519 secret key (the signer the control plane holds). */
    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    /** The control-plane key envelope as set_up() stored it, for restoring. */
    private string $storedKey = '';

    private Router $router;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-tokenerr-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });

        // Router's defense-in-depth capability check.
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('current_user_can')->justReturn(true);

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);

        $keystore = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->storedKey = (string) $this->options[Keystore::OPTION_CP_PUBLIC_KEY];

        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->router = new Router(new Connector($keystore, new Settings()), []);

        // jti anti-replay store.
        $GLOBALS['wpdb'] = new FakeWpdb();

        // The verified-jti cache is static and this process never changes
        // REQUEST_TIME_FLOAT, so a jti verified by an earlier test would
        // otherwise short-circuit the checks under test here.
        Connector::resetRequestCacheForTesting();

        // A stale include-time stash would short-circuit Router::bearerToken()
        // and make the token under test irrelevant.
        $this->resetShieldStash();
    }

    protected function tear_down(): void
    {
        $this->resetShieldStash();
        Connector::resetRequestCacheForTesting();
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The reported case
    // -------------------------------------------------------------------------

    /**
     * #679: a site whose clock runs behind the control plane's sees a token
     * that expires further ahead than it accepts. That is a clock problem, and
     * the answer must say so instead of the catch-all.
     */
    public function test_a_token_that_expires_too_far_ahead_answers_token_skew(): void
    {
        $token = $this->sign($this->claims(['exp' => time() + 90]));

        $result = $this->authorize($token);

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('wpmgr_token_skew', $result->get_error_code());
        $this->assertSame(['status' => 403], $result->get_error_data());
    }

    // -------------------------------------------------------------------------
    // Every refusal Connector can make
    // -------------------------------------------------------------------------

    /**
     * Each check that can refuse a token answers with its own code, through
     * the permission_callback, and none of them is the catch-all.
     *
     * @dataProvider provideRefusals
     */
    public function test_each_refusal_answers_its_own_code(string $scenario, string $category, string $code): void
    {
        $result = $this->authorize($this->arrange($scenario));

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('wpmgr_' . $code, $result->get_error_code(), $scenario);
        $this->assertNotSame('wpmgr_invalid_token', $result->get_error_code(), $scenario);
        $this->assertSame('Forbidden.', $result->get_error_message());
        $this->assertSame(['status' => 403], $result->get_error_data());
    }

    /**
     * The same scenarios one layer down: Connector names the failed check as
     * a typed category rather than leaving the caller to guess from a message.
     *
     * @dataProvider provideRefusals
     */
    public function test_connector_names_the_category_of_each_refusal(string $scenario, string $category, string $code): void
    {
        $token     = $this->arrange($scenario);
        $connector = $this->connector();

        try {
            $connector->verifyCommand($token, self::CMD);
            $this->fail('verifyCommand accepted the token for scenario ' . $scenario);
        } catch (TokenRejected $e) {
            $this->assertSame($category, $e->failure()->value, $scenario);
            $this->assertSame($code, $e->failure()->responseCode(), $scenario);
        }
    }

    /**
     * Every refusal, by the check that makes it.
     *
     * Columns: the scenario arrange() builds, the category Connector reports
     * (and the debug log records), and the code the response carries.
     *
     * @return array<string,array{0:string,1:string,2:string}>
     */
    public static function provideRefusals(): array
    {
        return [
            'not three segments'           => ['not_three_segments', 'malformed_jwt', 'malformed_jwt'],
            'no control-plane key stored'  => ['key_not_provisioned', 'key_not_provisioned', 'sig_failed'],
            'stored key cannot be read'    => ['key_unreadable', 'key_unreadable', 'sig_failed'],
            'signature of the wrong size'  => ['signature_wrong_length', 'sig_failed', 'sig_failed'],
            'signature from another key'   => ['signature_from_another_key', 'sig_failed', 'sig_failed'],
            'algorithm is not EdDSA'       => ['unexpected_algorithm', 'malformed_jwt', 'malformed_jwt'],
            'header is not a JSON object'  => ['header_not_json', 'malformed_jwt', 'malformed_jwt'],
            'payload is not a JSON object' => ['payload_not_json', 'malformed_jwt', 'malformed_jwt'],
            'no exp claim'                 => ['missing_exp', 'missing_exp', 'missing_exp'],
            'expired'                      => ['expired', 'token_expired', 'token_expired'],
            'expires too far ahead'        => ['exp_too_far_ahead', 'token_skew', 'token_skew'],
            'no jti claim'                 => ['missing_jti', 'missing_jti', 'missing_jti'],
            'jti already used'             => ['replayed', 'token_replay', 'token_replay'],
            'site has no enrolled id'      => ['site_not_enrolled', 'site_not_enrolled', 'site_not_enrolled'],
            'no aud claim'                 => ['missing_aud', 'missing_aud', 'missing_aud'],
            'aud names another site'       => ['aud_mismatch', 'aud_mismatch', 'aud_mismatch'],
            'no cmd claim'                 => ['missing_cmd', 'missing_cmd', 'missing_cmd'],
            'cmd names another command'    => ['cmd_mismatch', 'cmd_mismatch', 'cmd_mismatch'],
        ];
    }

    /**
     * Connector also refuses to verify against an empty expected command. The
     * Router never reaches that check, because it refuses an empty command
     * itself first; both layers must give the one condition one code. It is
     * not a token without a cmd claim, and it is certainly not a missing exp.
     */
    public function test_an_empty_expected_command_is_missing_command_at_both_layers(): void
    {
        $token = $this->sign($this->claims());

        try {
            $this->connector()->verifyCommand($token, '');
            $this->fail('verifyCommand accepted an empty expected command');
        } catch (TokenRejected $e) {
            $this->assertSame(TokenFailure::MissingCommand, $e->failure());
            $this->assertSame('missing_command', $e->failure()->responseCode());
        }

        $result = $this->router->authorizeCommand($this->request($token), '');

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('wpmgr_missing_command', $result->get_error_code());
    }

    /**
     * The provider is the exhaustive list: every category Connector can report
     * is reached by at least one row, so a category added without a scenario
     * turns this red. MissingCommand is the one exception, because the Router
     * never lets a request reach that check; the test above pins it.
     */
    public function test_the_provider_reaches_every_category(): void
    {
        $reached = array_values(array_unique(array_column(self::provideRefusals(), 1)));
        sort($reached);

        $declared = [];
        foreach (TokenFailure::cases() as $failure) {
            if ($failure !== TokenFailure::MissingCommand) {
                $declared[] = $failure->value;
            }
        }
        sort($declared);

        $this->assertNotEmpty($declared, 'no categories declared: this guard would pass vacuously');
        $this->assertSame($declared, $reached);
    }

    /**
     * Every response code is one the control plane will map: lower-case
     * letters and underscores, short, and never the catch-all.
     */
    public function test_every_response_code_is_a_short_snake_case_code(): void
    {
        $this->assertNotEmpty(TokenFailure::cases());

        foreach (TokenFailure::cases() as $failure) {
            $code = $failure->responseCode();
            $this->assertMatchesRegularExpression('/^[a-z_]{1,40}$/', $code, $failure->name);
            $this->assertNotSame('invalid_token', $code, $failure->name);
        }
    }

    /**
     * A new refusal added to Connector as a bare exception would quietly fall
     * through to the catch-all again. Every throw in the file constructs the
     * typed exception.
     */
    public function test_every_throw_in_connector_is_typed(): void
    {
        $source = (string) file_get_contents(dirname(__DIR__) . '/includes/class-connector.php');

        preg_match_all('/\bthrow\s+new\s+\\\\?([A-Za-z_\\\\]+)\s*\(/', $source, $matches);
        $thrown = $matches[1];

        // Positive control: a broken pattern must not pass by finding nothing.
        $this->assertGreaterThanOrEqual(count(self::provideRefusals()), count($thrown));
        $this->assertSame(0, preg_match('/\bthrow\s+\$/', $source), 'Connector rethrows an untyped exception');

        foreach ($thrown as $class) {
            $this->assertSame('TokenRejected', $class);
        }
    }

    // -------------------------------------------------------------------------
    // What stays generic
    // -------------------------------------------------------------------------

    /**
     * Anything that is not Connector's own typed refusal still answers the
     * catch-all: here, a Connector whose collaborators were never set up, so
     * reading its key fails with an Error rather than a refusal.
     */
    public function test_a_throwable_that_is_not_a_typed_refusal_answers_invalid_token(): void
    {
        $broken = (new ReflectionClass(Connector::class))->newInstanceWithoutConstructor();
        $router = new Router($broken, []);

        $result = $router->authorizeCommand($this->request($this->sign($this->claims())), self::CMD);

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('wpmgr_invalid_token', $result->get_error_code());
        $this->assertSame(['status' => 403], $result->get_error_data());
    }

    /**
     * Before the signature is checked, the caller can be anyone. What such a
     * caller is told must not depend on whether this site holds a usable
     * control-plane key: a site with a key, a site with none, and a site whose
     * stored key cannot be read give an identical answer to the same forged
     * token, for both forgery shapes.
     */
    public function test_a_refusal_before_the_signature_does_not_depend_on_key_state(): void
    {
        $forgeries = [
            'garbage signature of the right size' => $this->segments($this->claims()) . '.' . $this->b64(random_bytes(SODIUM_CRYPTO_SIGN_BYTES)),
            'signature of the wrong size'         => $this->segments($this->claims()) . '.' . $this->b64('short'),
        ];

        foreach ($forgeries as $shape => $forgery) {
            $answers = [];

            $answers['key held'] = $this->describe($this->authorize($forgery));

            unset($this->options[Keystore::OPTION_CP_PUBLIC_KEY]);
            $answers['no key'] = $this->describe($this->authorize($forgery));

            $this->options[Keystore::OPTION_CP_PUBLIC_KEY] = $this->undecryptableEnvelope();
            $answers['key unreadable'] = $this->describe($this->authorize($forgery));

            $this->assertSame(
                ['wpmgr_sig_failed', 'Forbidden.', ['status' => 403]],
                $answers['key held'],
                $shape
            );
            $this->assertSame($answers['key held'], $answers['no key'], $shape);
            $this->assertSame($answers['key held'], $answers['key unreadable'], $shape);

            $this->options[Keystore::OPTION_CP_PUBLIC_KEY] = $this->storedKey;
        }
    }

    /**
     * The response is generic about key state; the site owner's own debug log
     * is not. It names the precise category, the code that was sent, and the
     * reason, including why a stored key could not be read.
     */
    public function test_the_debug_log_names_the_precise_category_behind_a_generic_answer(): void
    {
        $lines   = [];
        $handles = [
            \Patchwork\redefine('WPMgr\Agent\Support\DebugLog::isEnabled', static fn (): bool => true),
            \Patchwork\redefine('WPMgr\Agent\Support\DebugLog::write', static function (string $line) use (&$lines): void {
                $lines[] = $line;
            }),
        ];

        try {
            unset($this->options[Keystore::OPTION_CP_PUBLIC_KEY]);
            $this->authorize($this->sign($this->claims()));

            $this->options[Keystore::OPTION_CP_PUBLIC_KEY] = $this->undecryptableEnvelope();
            $this->authorize($this->sign($this->claims()));
        } finally {
            foreach ($handles as $handle) {
                \Patchwork\restore($handle);
            }
        }

        $lines = array_values(array_filter(
            $lines,
            static fn (string $line): bool => strpos($line, 'command authorize failed') !== false
        ));
        $this->assertCount(2, $lines, implode("\n", $lines));

        $this->assertStringContainsString('WPMgr Agent: command authorize failed: command=update ', $lines[0]);
        $this->assertStringContainsString(' category=key_not_provisioned ', $lines[0]);
        $this->assertStringContainsString(' code=wpmgr_sig_failed ', $lines[0]);
        $this->assertStringContainsString(' reason=WPMgr Agent: control-plane key not provisioned.', $lines[0]);

        $this->assertStringContainsString(' category=key_unreadable ', $lines[1]);
        $this->assertStringContainsString(' code=wpmgr_sig_failed ', $lines[1]);
        $this->assertStringContainsString('ciphertext authentication failed', $lines[1]);
    }

    /**
     * The log line's reason passes through the same key-material redaction as
     * the command-failure line, including the reason of whatever caused the
     * refusal and the message of a Throwable that is not a typed refusal.
     */
    public function test_the_debug_log_redacts_key_material_from_the_reason(): void
    {
        $padded = 'JpXsrsvpkJ4QU9D43mEqo/DWxzeE2nX9GPs/Zi7BpTA=';
        $throw  = new \RuntimeException('replaced before every call');

        $lines   = [];
        $handles = [
            \Patchwork\redefine('WPMgr\Agent\Support\DebugLog::isEnabled', static fn (): bool => true),
            \Patchwork\redefine('WPMgr\Agent\Support\DebugLog::write', static function (string $line) use (&$lines): void {
                $lines[] = $line;
            }),
            // Not static: Patchwork binds an instance method's replacement
            // to the instance, and a static closure cannot be bound.
            \Patchwork\redefine('WPMgr\Agent\Keystore::getControlPlanePublicKey', function () use (&$throw): void {
                throw $throw;
            }),
        ];

        try {
            // A keystore failure: typed as key_unreadable, its reason chained.
            $throw  = new \RuntimeException('unwrap failed for ' . $padded);
            $result = $this->authorize($this->sign($this->claims()));
            $this->assertSame('wpmgr_sig_failed', $result instanceof \WP_Error ? $result->get_error_code() : null);

            // Not a RuntimeException, so not a typed refusal: the catch-all.
            $throw  = new \LogicException('unwrap failed for ' . $padded);
            $result = $this->authorize($this->sign($this->claims()));
            $this->assertSame('wpmgr_invalid_token', $result instanceof \WP_Error ? $result->get_error_code() : null);
        } finally {
            foreach ($handles as $handle) {
                \Patchwork\restore($handle);
            }
        }

        $lines = array_values(array_filter(
            $lines,
            static fn (string $line): bool => strpos($line, 'command authorize failed') !== false
        ));
        $this->assertCount(2, $lines, implode("\n", $lines));

        $this->assertStringContainsString(
            ' category=key_unreadable code=wpmgr_sig_failed reason=WPMgr Agent: control-plane key unreadable. Caused by RuntimeException: unwrap failed for <redacted>',
            $lines[0]
        );
        $this->assertStringContainsString(
            ' category=invalid_token code=wpmgr_invalid_token reason=LogicException: unwrap failed for <redacted>',
            $lines[1]
        );
        foreach ($lines as $line) {
            $this->assertStringNotContainsString('JpXsrsvpkJ4QU9D43mEqo', $line);
        }
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Put the site into the state a scenario needs and return its token.
     *
     * @param string $scenario Scenario key from provideRefusals().
     * @return string Compact token.
     */
    private function arrange(string $scenario): string
    {
        $now = time();

        switch ($scenario) {
            case 'not_three_segments':
                return 'only.two';
            case 'key_not_provisioned':
                unset($this->options[Keystore::OPTION_CP_PUBLIC_KEY]);
                return $this->sign($this->claims());
            case 'key_unreadable':
                $this->options[Keystore::OPTION_CP_PUBLIC_KEY] = $this->undecryptableEnvelope();
                return $this->sign($this->claims());
            case 'signature_wrong_length':
                return $this->segments($this->claims()) . '.' . $this->b64('short');
            case 'signature_from_another_key':
                return $this->sign($this->claims(), sodium_crypto_sign_secretkey(sodium_crypto_sign_keypair()));
            case 'unexpected_algorithm':
                return $this->signRaw((string) json_encode(['alg' => 'HS256', 'typ' => 'JWT']), (string) json_encode($this->claims()));
            case 'header_not_json':
                return $this->signRaw('not json', (string) json_encode($this->claims()));
            case 'payload_not_json':
                return $this->signRaw((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT']), 'not json');
            case 'missing_exp':
                return $this->sign($this->claims(['exp' => null]));
            case 'expired':
                return $this->sign($this->claims(['exp' => $now - 5]));
            case 'exp_too_far_ahead':
                return $this->sign($this->claims(['exp' => $now + 90]));
            case 'missing_jti':
                return $this->sign($this->claims(['jti' => null]));
            case 'replayed':
                $token = $this->sign($this->claims());
                $this->assertTrue($this->authorize($token), 'the first presentation was not authorized');
                // The second presentation arrives in a new HTTP request.
                Connector::resetRequestCacheForTesting();
                return $token;
            case 'site_not_enrolled':
                $this->options[Settings::OPTION_SITE_ID] = '';
                return $this->sign($this->claims());
            case 'missing_aud':
                return $this->sign($this->claims(['aud' => null]));
            case 'aud_mismatch':
                return $this->sign($this->claims(['aud' => 'ffffffff-0000-1111-2222-333333333333']));
            case 'missing_cmd':
                return $this->sign($this->claims(['cmd' => null]));
            case 'cmd_mismatch':
                return $this->sign($this->claims(['cmd' => 'rollback']));
        }

        $this->fail('unknown scenario ' . $scenario);
    }

    /**
     * A valid command claim set, with overrides; a null override drops the
     * claim entirely.
     *
     * @param array<string,mixed> $overrides Claim overrides.
     * @return array<string,mixed>
     */
    private function claims(array $overrides = []): array
    {
        $claims = [
            'aud' => $this->siteId,
            'cmd' => self::CMD,
            'jti' => bin2hex(random_bytes(8)),
            'exp' => time() + 30,
        ];

        foreach ($overrides as $name => $value) {
            if ($value === null) {
                unset($claims[$name]);
                continue;
            }
            $claims[$name] = $value;
        }

        return $claims;
    }

    /**
     * Mint a compact Ed25519 JWT the way the control plane does.
     *
     * @param array<string,mixed> $claims Payload claims.
     * @param string|null         $secret Signing key; the control plane's by default.
     * @return string
     */
    private function sign(array $claims, ?string $secret = null): string
    {
        return $this->signRaw((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT']), (string) json_encode($claims), $secret);
    }

    /**
     * Sign arbitrary header and payload bytes, so a malformed segment still
     * carries a valid signature and reaches the checks after it.
     *
     * @param string      $header  Header bytes.
     * @param string      $payload Payload bytes.
     * @param string|null $secret  Signing key; the control plane's by default.
     * @return string
     */
    private function signRaw(string $header, string $payload, ?string $secret = null): string
    {
        $input = $this->b64($header) . '.' . $this->b64($payload);

        return $input . '.' . $this->b64(sodium_crypto_sign_detached($input, $secret ?? $this->cpSecret));
    }

    /**
     * The header and payload segments of a token, unsigned.
     *
     * @param array<string,mixed> $claims Payload claims.
     * @return string
     */
    private function segments(array $claims): string
    {
        return $this->b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])) . '.' . $this->b64((string) json_encode($claims));
    }

    /**
     * A stored key envelope of the right shape that this install's master key
     * cannot open: what a site sees when the key it encrypted under is gone.
     *
     * @return string
     */
    private function undecryptableEnvelope(): string
    {
        return base64_encode(random_bytes(12 + 16 + SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES));
    }

    /**
     * The Connector the Router under test uses.
     *
     * @return Connector
     */
    private function connector(): Connector
    {
        $prop = new ReflectionProperty(Router::class, 'connector');

        /** @var Connector $connector */
        $connector = $prop->getValue($this->router);

        return $connector;
    }

    /**
     * Run the permission_callback for a command request carrying $token.
     *
     * @param string $token Compact token.
     * @return bool|\WP_Error
     */
    private function authorize(string $token)
    {
        return $this->router->authorizeCommand($this->request($token), self::CMD);
    }

    /**
     * Build the request the REST server builds for POST /command/{command}.
     *
     * @param string $token Compact token.
     * @return \WP_REST_Request
     */
    private function request(string $token): \WP_REST_Request
    {
        $request = new \WP_REST_Request('POST', self::ROUTE_BASE . self::CMD);
        $request->set_url_params(['command' => self::CMD]);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $token);
        $request->set_body('{}');

        return $request;
    }

    /**
     * Everything a caller can observe about a refusal.
     *
     * @param bool|\WP_Error $result Permission-callback result.
     * @return array{0:int|string,1:string,2:mixed}
     */
    private function describe($result): array
    {
        $this->assertInstanceOf(\WP_Error::class, $result);

        return [$result->get_error_code(), $result->get_error_message(), $result->get_error_data()];
    }

    private function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    /**
     * Clear the include-time bearer stash so each test starts from a request
     * that carries its own Authorization header.
     */
    private function resetShieldStash(): void
    {
        $prop = new ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }
}
