<?php
/**
 * RouterTest: regression coverage for the widened [a-z0-9_.]+  command regex,
 * exact-match dispatch, and objectcache.* dot-notation routing.
 *
 * N2 requirement: add tests covering:
 *   - '..'-style / leading-dot names are rejected or safely sanitized by the
 *     sanitize_callback before they reach dispatch.
 *   - A command name that passes regex validation but has no registered handler
 *     produces a 404 WP_Error.
 *   - objectcache.* dot-notation commands reach the correct handler via the
 *     exact-match map.
 *
 * Design note: Connector is final and cannot be mocked or extended. We avoid
 * the constraint by:
 *   - For dispatch tests: calling handleCommand() directly with a WP_REST_Request
 *     that carries pre-seeded wpmgr_claims. handleCommand reads claims from
 *     request params (set by authorizeCommand after auth), not from Connector.
 *   - For auth tests: using paths that never call verifyCommand (missing bearer
 *     token, empty command name). These short-circuit inside authorizeCommand
 *     before any Connector call.
 *   - Building a Connector instance via ReflectionClass::newInstanceWithoutConstructor
 *     to satisfy Router's type-hint without executing the real constructor.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\CommandRepeatability;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Router;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Router
 */
final class RouterTest extends TestCase
{
	/** @var Router */
	private Router $router;

	/** Connector instance built without calling the real constructor. */
	private Connector $connector;

	/** @var array<string,mixed> */
	private array $fakeClaims = [ 'sub' => 'site-uuid', 'cmd' => 'test_cmd' ];

	protected function set_up(): void
	{
		parent::set_up();
		Monkey\setUp();

		// Stub WordPress functions used by Router.
		Functions\when( 'is_user_logged_in' )->justReturn( false );
		Functions\when( 'current_user_can' )->justReturn( true );
		Functions\when( 'register_rest_route' )->justReturn( true );

		// Build a real Connector instance without calling __construct so we
		// don't need Keystore + Settings (which are also final and hard to stub).
		// Tests that call handleCommand bypass authorizeCommand entirely, so
		// verifyCommand is never invoked in the happy path.
		$rc              = new ReflectionClass( Connector::class );
		$this->connector = $rc->newInstanceWithoutConstructor();

		$this->router = new Router(
			$this->connector,
			[
				$this->makeCommand( 'test_cmd' ),
				$this->makeCommand( 'objectcache.apply_config' ),
			]
		);
	}

	protected function tear_down(): void
	{
		Monkey\tearDown();
		parent::tear_down();
	}

	// -------------------------------------------------------------------------
	// sanitize_callback behaviour for the {command} route arg
	// -------------------------------------------------------------------------

	/**
	 * N2: the sanitize_callback strips everything outside [a-z0-9_.] and
	 * lowercases the result.
	 */
	public function test_sanitize_callback_strips_disallowed_chars(): void
	{
		$sanitize = $this->captureSanitizeCallback();

		// Slashes removed.
		$this->assertStringNotContainsString( '/', $sanitize( '../../etc/passwd' ) );

		// Uppercase lowercased.
		$this->assertSame( 'test_cmd', $sanitize( 'TEST_CMD' ) );

		// Disallowed chars stripped.
		$this->assertSame( 'abc', $sanitize( 'a%b@c' ) );

		// Result contains only [a-z0-9_.].
		$result = $sanitize( '..' );
		$this->assertMatchesRegularExpression( '/^[a-z0-9_.]*$/', $result );
	}

	/**
	 * N2: a leading-dot name like '.env' sanitizes cleanly (only valid chars)
	 * but does NOT map to a registered command — dispatch returns 404.
	 */
	public function test_leading_dot_name_does_not_dispatch(): void
	{
		$sanitize  = $this->captureSanitizeCallback();
		$sanitized = $sanitize( '.env' );

		// Sanitized value is [a-z0-9_.] only.
		$this->assertMatchesRegularExpression( '/^[a-z0-9_.]*$/', $sanitized );

		// Not a registered command.
		$response = $this->dispatchCommand( $sanitized );
		$this->assertInstanceOf( \WP_Error::class, $response );
		$this->assertSame( 'wpmgr_unknown_command', $response->get_error_code() );
	}

	// -------------------------------------------------------------------------
	// Dispatch: known and unknown commands
	// -------------------------------------------------------------------------

	/**
	 * N2: unknown command name produces a 404 WP_Error (cmd-binding mismatch).
	 */
	public function test_dispatch_returns_404_for_unknown_command(): void
	{
		$response = $this->dispatchCommand( 'no_such_command' );

		$this->assertInstanceOf( \WP_Error::class, $response );
		$this->assertSame( 'wpmgr_unknown_command', $response->get_error_code() );
		$data = $response->get_error_data();
		$this->assertSame( 404, $data['status'] ?? null );
	}

	/**
	 * N2: objectcache.* dot-notation command dispatches to the correct handler.
	 */
	public function test_objectcache_dot_notation_dispatches_to_correct_handler(): void
	{
		$response = $this->dispatchCommand( 'objectcache.apply_config' );

		$this->assertInstanceOf( \WP_REST_Response::class, $response );
		$this->assertSame( 200, $response->status );
		$this->assertSame( 'objectcache.apply_config', $response->data['handled_by'] ?? null );
	}

	/**
	 * N2: underscore command also dispatches correctly.
	 */
	public function test_underscore_command_dispatches_correctly(): void
	{
		$response = $this->dispatchCommand( 'test_cmd' );

		$this->assertInstanceOf( \WP_REST_Response::class, $response );
		$this->assertSame( 200, $response->status );
		$this->assertSame( 'test_cmd', $response->data['handled_by'] ?? null );
	}

	// -------------------------------------------------------------------------
	// authorizeCommand: guard paths that don't reach verifyCommand
	// -------------------------------------------------------------------------

	/**
	 * N2: a request with no Authorization header is rejected before verifyCommand.
	 */
	public function test_authorize_returns_forbidden_when_no_bearer_token(): void
	{
		$request = new \WP_REST_Request( [ 'command' => 'test_cmd' ] );
		// get_header returns '' for any key in the stub: no token present.
		$result = $this->router->authorizeCommand( $request, 'test_cmd' );

		$this->assertInstanceOf( \WP_Error::class, $result );
	}

	/**
	 * N2: an empty command name is rejected before verifyCommand.
	 */
	public function test_authorize_rejects_empty_command_name(): void
	{
		$request = new \WP_REST_Request( [ 'command' => '' ] );
		$result  = $this->router->authorizeCommand( $request, '' );

		$this->assertInstanceOf( \WP_Error::class, $result );
	}

	// -------------------------------------------------------------------------
	// Helpers
	// -------------------------------------------------------------------------

	/**
	 * Capture the sanitize_callback for the {command} route arg by aliasing
	 * register_rest_route and calling registerRoutes().
	 *
	 * @return callable
	 */
	private function captureSanitizeCallback(): callable
	{
		/** @var array<string,mixed>|null $captured */
		$captured = null;

		Functions\when( 'register_rest_route' )->alias(
			static function ( string $namespace, string $route, array $args ) use ( &$captured ): bool {
				if ( isset( $args['args']['command']['sanitize_callback'] ) ) {
					$captured = $args;
				}
				return true;
			}
		);

		$router = new Router( $this->connector, [] );
		$router->registerRoutes();

		$this->assertNotNull( $captured, 'register_rest_route not called for the command route' );
		$cb = $captured['args']['command']['sanitize_callback'] ?? null;
		$this->assertIsCallable( $cb );
		return $cb; // @phpstan-ignore-line
	}

	/**
	 * Invoke handleCommand() with a WP_REST_Request pre-seeded with verified
	 * claims — bypasses authorizeCommand entirely.
	 *
	 * @param string $command Command name.
	 * @return \WP_REST_Response|\WP_Error
	 */
	private function dispatchCommand( string $command )
	{
		$request = new \WP_REST_Request(
			[
				'command'      => $command,
				'wpmgr_claims' => $this->fakeClaims,
			]
		);
		return $this->router->handleCommand( $request );
	}

	/**
	 * Build a minimal CommandInterface stub.
	 *
	 * @param string $name Command name.
	 * @return CommandInterface
	 */
	private function makeCommand( string $name ): CommandInterface
	{
		return new class( $name ) implements CommandInterface {
			private string $n;

			public function __construct( string $n )
			{
				$this->n = $n;
			}

			public function name(): string
			{
				return $this->n;
			}

			/** @return CommandEffect A double that only echoes its own name reads nothing and changes nothing. */
			public function effect(): CommandEffect
			{
				return CommandEffect::Read;
			}

			/** @return CommandRepeatability Returning a constant string converges on every run. */
			public function repeatability(): CommandRepeatability
			{
				return CommandRepeatability::Idempotent;
			}

			/** @param array<string,mixed> $claims @param array<string,mixed> $params @return array<string,mixed> */
			public function execute( array $claims, array $params ): array
			{
				return [ 'handled_by' => $this->n ];
			}
		};
	}
}
