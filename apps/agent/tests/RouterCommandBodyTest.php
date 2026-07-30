<?php
/**
 * RouterCommandBodyTest: end-to-end coverage of the REQUEST BODY as it travels
 * from the wire to a command's execute($claims, $params).
 *
 * WHY THIS FILE EXISTS
 * --------------------
 * A production self-update rollout failed on its first wave with
 * "This command takes no parameters." even though the control plane sent a
 * literal `{}` body. Both halves were tested and both halves looked right:
 *
 *   - command tests called execute([], []) directly, proving only that the
 *     guard accepts a literal PHP [],
 *   - RouterTest built a request object by hand and never exercised a JSON
 *     body at all.
 *
 * Neither half tested the JOIN, and the defect lived exactly there.
 * WP_REST_Request::set_param() writes an unseen key into the FIRST bucket of
 * get_parameter_order(), which on a JSON request is the JSON bucket itself, so
 * the claims the Router stashes in its permission_callback came back out of
 * get_json_params() and made an empty body look non-empty.
 *
 * So these tests deliberately take the WHOLE path, with nothing simulated:
 * a real Ed25519 keypair, a real signed command JWT, a real Connector, a real
 * Router, a request built the way the REST server builds one (POST, route,
 * URL param, Content-Type: application/json, Bearer token, body), then
 * authorizeCommand() followed by handleCommand(). What a command receives is
 * asserted at the command boundary itself.
 *
 * Keep the parameterised body test general. It is worth more than a bespoke
 * test per command, because the bug was never about one command.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\RefreshInventoryCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Router
 * @covers \WPMgr\Agent\Commands\AgentSelfUpdateCommand
 * @covers \WPMgr\Agent\Commands\RefreshInventoryCommand
 */
final class RouterCommandBodyTest extends TestCase
{
    /** Route registered by Router::registerRoutes(), with the segment filled in. */
    private const ROUTE_BASE = '/wpmgr/v1/command/';

    private string $keyFile;

    /** @var array<string,mixed> In-memory wp-options. */
    private array $options = [];

    /** Control-plane Ed25519 secret key (the signer the CP holds). */
    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    /** @var array<string,ParamRecorderCommand> Probes by command name. */
    private array $probes = [];

    private Connector $connector;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-routerbody-' . bin2hex(random_bytes(8)) . '.key';
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
        Functions\when('register_rest_route')->justReturn(true);

        // Real control-plane keypair; the agent only ever holds the public half.
        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);

        $keystore = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));

        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->connector = new Connector($keystore, new Settings());

        // jti anti-replay store.
        $GLOBALS['wpdb'] = new FakeWpdb();

        // A stale stash would short-circuit Router::bearerToken() and make the
        // signed token under test irrelevant.
        $this->resetShieldStash();
    }

    protected function tear_down(): void
    {
        $this->resetShieldStash();
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        $this->probes = [];
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The general rule: a command receives EXACTLY what the control plane sent.
    // -------------------------------------------------------------------------

    /**
     * Every command the control plane invokes with a literal `{}` body must see
     * an EMPTY array at its execute() boundary. Not "an array containing only
     * internal keys", empty.
     *
     * This is the test that would have caught the production failure, and it is
     * parameterised rather than per-command because the defect was in the
     * shared router path, not in any one handler.
     *
     * @dataProvider provideEmptyBodyCommands
     */
    public function test_empty_json_body_reaches_command_as_empty_array(string $command): void
    {
        $router   = $this->routerWithProbes([$command]);
        $response = $this->dispatchSigned($router, $command, '{}');

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertSame(
            [],
            $this->probes[$command]->received,
            $command . ' received something the control plane never sent'
        );
    }

    /**
     * The control-plane commands whose Go request struct is empty, so the
     * marshalled body on the wire is exactly `{}`.
     *
     * @return array<string,array{0:string}>
     */
    public static function provideEmptyBodyCommands(): array
    {
        return [
            'agent_self_update' => ['agent_self_update'],
            'refresh_inventory' => ['refresh_inventory'],
            'ping'              => ['ping'],
            'metadata'          => ['metadata'],
        ];
    }

    /**
     * The same rule for a command that DOES take parameters: it gets its own
     * keys and nothing else. Internal router state must never appear alongside
     * them, or a command could act on a key the caller never sent.
     */
    public function test_populated_json_body_reaches_command_without_internal_keys(): void
    {
        $router   = $this->routerWithProbes(['cache_enable']);
        $response = $this->dispatchSigned($router, 'cache_enable', '{"enabled":true,"config_version":7}');

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertSame(
            ['enabled' => true, 'config_version' => 7],
            $this->probes['cache_enable']->received
        );
    }

    /**
     * A body-less POST that still declares a JSON Content-Type is the same
     * hazard: WordPress leaves the JSON bucket null, and set_param() then
     * creates it. The command boundary must still see an empty array.
     */
    public function test_absent_body_with_json_content_type_reaches_command_as_empty_array(): void
    {
        $router   = $this->routerWithProbes(['ping']);
        $response = $this->dispatchSigned($router, 'ping', '');

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertSame([], $this->probes['ping']->received);
    }

    /**
     * Pins the WordPress behaviour this whole file exists for, so the request
     * double can never be "simplified" back into a flat array without a red
     * test. After the permission_callback has stashed its claims, the request's
     * OWN json params really do contain the internal key. It is the Router that
     * must keep that out of a command, and this asserts both halves.
     */
    public function test_router_strips_the_claims_stash_that_wordpress_folds_into_json_params(): void
    {
        $router  = $this->routerWithProbes(['ping']);
        $request = $this->signedRequest('ping', '{}');

        $this->assertTrue($router->authorizeCommand($request, 'ping'));

        // WordPress itself now reports the stash as part of the JSON body.
        $rawJson = $request->get_json_params();
        $this->assertIsArray($rawJson);
        $this->assertArrayHasKey('wpmgr_claims', $rawJson);

        // The Router must not pass that on.
        $router->handleCommand($request);
        $this->assertSame([], $this->probes['ping']->received);
    }

    // -------------------------------------------------------------------------
    // The production regressions themselves, through the real handlers.
    // -------------------------------------------------------------------------

    /**
     * The exact production call: wave 0 of a self-update rollout. Before the
     * fix this returned status "error" with detail
     * "This command takes no parameters." and halted the wave.
     */
    public function test_agent_self_update_does_not_reject_the_control_plane_body(): void
    {
        $router = new Router($this->connector, [new AgentSelfUpdateCommand(null)]);

        $response = $this->dispatchSigned($router, 'agent_self_update', '{}');

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertIsArray($response->data);
        $this->assertNotSame(
            'error',
            $response->data['status'] ?? null,
            'agent_self_update rejected the body the control plane actually sends'
        );
        $this->assertStringNotContainsStringIgnoringCase(
            'takes no parameters',
            (string) ($response->data['detail'] ?? '')
        );
    }

    /**
     * The sibling defect. This one never failed loudly: the caller checks the
     * transport status and not the ok flag, so refresh_inventory answered
     * HTTP 200 with ok=false and silently refreshed nothing. Assert the work
     * actually happens, not merely that the response looks acceptable.
     */
    public function test_refresh_inventory_actually_runs_through_the_router(): void
    {
        $refreshed = false;
        $pushed    = false;

        $command = new RefreshInventoryCommand(
            static function () use (&$refreshed): void {
                $refreshed = true;
            },
            static function () use (&$pushed): array {
                $pushed = true;
                return ['ok' => true];
            }
        );

        $router   = new Router($this->connector, [$command]);
        $response = $this->dispatchSigned($router, 'refresh_inventory', '{}');

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertTrue($refreshed, 'inventory was never re-polled');
        $this->assertTrue($pushed, 'inventory was never pushed to the control plane');
        $this->assertTrue($response->data['ok'] ?? false);
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Build a Router whose handlers are recording probes, one per name.
     *
     * @param array<int,string> $names Command names to register.
     * @return Router
     */
    private function routerWithProbes(array $names): Router
    {
        $handlers = [];
        foreach ($names as $name) {
            $probe                = new ParamRecorderCommand($name);
            $this->probes[$name]  = $probe;
            $handlers[]           = $probe;
        }

        return new Router($this->connector, $handlers);
    }

    /**
     * Run the full authorize + handle path for a signed request.
     *
     * @param Router $router  Router under test.
     * @param string $command Command name.
     * @param string $body    Raw request body.
     * @return \WP_REST_Response|\WP_Error
     */
    private function dispatchSigned(Router $router, string $command, string $body)
    {
        $request = $this->signedRequest($command, $body);

        $authorized = $router->authorizeCommand($request, $command);
        $this->assertTrue($authorized, 'the signed request was not authorized');

        return $router->handleCommand($request);
    }

    /**
     * Build the request the way the REST server builds one for
     * POST /wp-json/wpmgr/v1/command/{command}: the {command} segment arrives
     * as a URL param, the control plane sets Content-Type: application/json and
     * an Ed25519 Bearer token, and the body is whatever it marshalled.
     *
     * @param string $command Command name (also the JWT cmd claim).
     * @param string $body    Raw request body.
     * @return \WP_REST_Request
     */
    private function signedRequest(string $command, string $body): \WP_REST_Request
    {
        $request = new \WP_REST_Request('POST', self::ROUTE_BASE . $command);
        $request->set_url_params(['command' => $command]);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $this->mintCommandToken($command));
        $request->set_body($body);

        return $request;
    }

    /**
     * Mint the compact Ed25519 JWT the control plane sends: bound to this site
     * (aud), to this command (cmd), single-use (jti), short-lived (exp).
     *
     * @param string $command Command name.
     * @return string
     */
    private function mintCommandToken(string $command): string
    {
        $segments = [
            $this->b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])),
            $this->b64((string) json_encode([
                'aud' => $this->siteId,
                'cmd' => $command,
                'jti' => bin2hex(random_bytes(8)),
                'exp' => time() + 30,
            ])),
        ];

        $segments[] = $this->b64(sodium_crypto_sign_detached(implode('.', $segments), $this->cpSecret));

        return implode('.', $segments);
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

/**
 * Command double that records exactly what reached its execute() boundary.
 *
 * Deliberately not an anonymous class: the parameterised test needs to reach
 * the recorded params from the test body, and a named type keeps that readable.
 */
final class ParamRecorderCommand implements CommandInterface
{
    /** @var array<string,mixed>|null What execute() was handed, verbatim. */
    public ?array $received = null;

    private string $commandName;

    public function __construct(string $commandName)
    {
        $this->commandName = $commandName;
    }

    public function name(): string
    {
        return $this->commandName;
    }

    /**
     * @param array<string,mixed> $claims Validated claims.
     * @param array<string,mixed> $params Request params as delivered.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        $this->received = $params;

        return ['ok' => true];
    }
}
