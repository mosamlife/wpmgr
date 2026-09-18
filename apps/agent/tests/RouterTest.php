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
	// #754: a throwing command must be diagnosable, without leaking anything
	// -------------------------------------------------------------------------

	/**
	 * #754, the test that matters most: an exception message carrying an
	 * absolute filesystem path must not reach the response. The path is real in
	 * shape — FilesRestorer interpolates exactly this — and it discloses both
	 * the hosting layout and the customer identity.
	 */
	public function test_command_failure_response_does_not_leak_an_absolute_path(): void
	{
		$response = $this->dispatchThrowing(
			new \RuntimeException(
				'FilesRestorer: cannot create staging dir: /home/customer123/public_html/wp-content/uploads/wpmgr/staging'
			)
		);

		$this->assertInstanceOf( \WP_Error::class, $response );

		$wire = $this->wireBlob( $response );

		$this->assertStringNotContainsString( '/home/customer123', $wire );
		$this->assertStringNotContainsString( 'customer123', $wire );
		$this->assertStringNotContainsString( 'public_html', $wire );

		// Redacted, not merely truncated: the category survives so the response
		// is still worth reading.
		$this->assertStringContainsString( 'cannot create staging dir', $wire );
		$this->assertStringContainsString( '<path>', $wire );
	}

	/**
	 * #754: an opaque 32+ character run — the shape of a key, token or hash —
	 * is redacted out of the response.
	 */
	public function test_command_failure_response_redacts_an_opaque_secret_shaped_run(): void
	{
		$secret   = 'AKIAJ7Q2ZK4XN8PLW3RD6YTBVC5MHGFS9UEO1I0A';
		$response = $this->dispatchThrowing( new \RuntimeException( 'Keystore: bad key ' . $secret ) );

		$wire = $this->wireBlob( $response );

		$this->assertStringNotContainsString( $secret, $wire );
		$this->assertStringContainsString( '<redacted>', $wire );
	}

	/**
	 * #754: STANDARD base64 — the encoding the agent's own encrypted material
	 * is actually emitted in — must not survive. Its alphabet contains '/', so
	 * a rule whose alphabet stops at the separator sees two sub-threshold
	 * pieces instead of one token.
	 *
	 * Driven over many random keys, not one sample, because whether a '/'
	 * lands inside a given encoding is chance: a single fixed key can pass by
	 * luck and prove nothing.
	 *
	 * @return void
	 */
	public function test_redaction_catches_standard_base64_key_material(): void
	{
		$shapes = array(
			// The DB-fallback master key: base64_encode() of 32 raw bytes.
			'master key'   => static function (): string {
				return base64_encode( random_bytes( 32 ) );
			},
			// The age header: the same 32 bytes with padding stripped.
			'age header'   => static function (): string {
				return rtrim( base64_encode( random_bytes( 32 ) ), '=' );
			},
			// The at-rest envelope: iv . tag . ciphertext, ~92 raw bytes.
			'envelope'     => static function (): string {
				return base64_encode( random_bytes( 92 ) );
			},
		);

		foreach ( $shapes as $label => $make ) {
			for ( $i = 0; $i < 40; $i++ ) {
				$secret = $make();
				$wire   = $this->wireBlob(
					$this->dispatchThrowing( new \RuntimeException( 'Keystore: cannot unwrap ' . $secret ) )
				);

				$this->assertStringNotContainsString(
					$secret,
					$wire,
					sprintf( '%s survived redaction whole: %s', $label, $secret )
				);
				$this->assertStringContainsString( '<redacted>', $wire, $label . ' was not redacted' );
			}
		}
	}

	/**
	 * #754: the same thing without relying on chance. Each case puts a '/' at
	 * a position chosen to split a 44-character standard-base64 key into two
	 * pieces that are each BELOW the 32-character threshold — the exact shape
	 * that a slash-free alphabet cannot see.
	 *
	 * @return void
	 */
	public function test_redaction_catches_base64_with_a_slash_inside_the_window(): void
	{
		// A realistic 44-char padded standard-base64 body, case-mixed as random
		// bytes are, with no '/' of its own so the split position is exact.
		$body = 'aG7kQ2pXvR8nZtL4yB6cM1sEwD3fUhJ9KrTgNxVbPq0=';
		$this->assertSame( 44, strlen( $body ), 'fixture must be a full 44-char base64 body' );

		// A 44-char body split at offset N leaves pieces of N and 43-N. Both are
		// below the 32-character run threshold exactly when 12 <= N <= 31, so
		// every offset here is a case the slash-free rule provably cannot see.
		foreach ( array( 12, 15, 20, 22, 25, 31 ) as $at ) {
			$secret = substr( $body, 0, $at ) . '/' . substr( $body, $at + 1 );

			$this->assertLessThan( 32, $at, 'left piece must be below the run threshold' );
			$this->assertLessThan( 32, 43 - $at, 'right piece must be below the run threshold' );

			$wire = $this->wireBlob(
				$this->dispatchThrowing( new \RuntimeException( 'Keystore: cannot unwrap ' . $secret ) )
			);

			$this->assertStringNotContainsString(
				$secret,
				$wire,
				sprintf( 'base64 with a separator at offset %d survived whole', $at )
			);
			// No fragment of the key body escapes either: assert on the longer
			// of the two pieces, which is the one a threshold rule might keep.
			$left  = substr( $secret, 0, $at );
			$right = substr( $secret, $at + 1 );
			$piece = strlen( $left ) >= strlen( $right ) ? $left : $right;
			$this->assertStringNotContainsString(
				$piece,
				$wire,
				sprintf( 'a %d-char fragment of the key survived at offset %d', strlen( $piece ), $at )
			);
			$this->assertStringContainsString( '<redacted>', $wire );
		}
	}

	/**
	 * #754 over-fire control, and the reason the standard-base64 rule is a
	 * shape test rather than "add '/' to the alphabet": every path, table
	 * name, option key and command name the agent emits is single-case, and
	 * every one of them has to come through the redaction untouched.
	 *
	 * An earlier attempt that simply widened the alphabet turned the first of
	 * these into "<redacted>" — destroying the diagnostic #754 exists to
	 * deliver.
	 *
	 * @return void
	 */
	public function test_redaction_keeps_single_case_agent_identifiers(): void
	{
		$intact = array(
			'wp-content/uploads/wpmgr/keystore',
			'wp-content/uploads/wpmgr/keystore.json',
			'wp-content/uploads/wpmgr/restore-staging/files',
			'wp-content/plugins/wpmgr-agent/includes/class-router.php',
			'wp-content/uploads/wpmgr/snapshots/2026-09-18-full',
			'wp_wpmgr_command_log',
			'objectcache.apply_config',
		);

		foreach ( $intact as $identifier ) {
			$response = $this->dispatchThrowing(
				new \RuntimeException( 'cannot read ' . $identifier )
			);
			$message = $response->get_error_message();

			$this->assertStringContainsString(
				$identifier,
				$message,
				$identifier . ' was eaten by the redaction'
			);
			$this->assertStringNotContainsString( '<redacted>', $message, $identifier . ' was redacted' );
			$this->assertStringNotContainsString( '<path>', $message, $identifier . ' was treated as absolute' );
		}
	}

	/**
	 * #754: the redaction must not over-fire. A path already relative to the
	 * WordPress root is the diagnostic payload — it says WHICH file failed —
	 * and has to survive intact.
	 */
	public function test_redaction_keeps_a_root_relative_path(): void
	{
		$response = $this->dispatchThrowing(
			new \RuntimeException( 'cannot read keystore at wp-content/uploads/wpmgr/keystore.json' )
		);

		$message = $response->get_error_message();

		$this->assertStringContainsString( 'wp-content/uploads/wpmgr/keystore.json', $message );
		$this->assertStringNotContainsString( '<path>', $message );
	}

	/**
	 * #754: an absolute path UNDER the WordPress root is rewritten to the
	 * root-relative form rather than dropped — the host layout goes, the
	 * diagnostic stays.
	 */
	public function test_absolute_path_under_abspath_becomes_root_relative(): void
	{
		$abspath  = rtrim( (string) constant( 'ABSPATH' ), '/\\' );
		$response = $this->dispatchThrowing(
			new \RuntimeException( 'snapshots directory is not writable: ' . $abspath . '/wp-content/uploads/wpmgr/snapshots' )
		);

		$message = $response->get_error_message();

		$this->assertStringNotContainsString( $abspath, $message );
		$this->assertStringContainsString( 'wp-content/uploads/wpmgr/snapshots', $message );
	}

	/**
	 * #754: root stripping is ANCHORED on the separator. A sibling directory
	 * whose name merely starts with a known root ("<ABSPATH>2/secret") must not
	 * be half-stripped into a surviving fragment — an unanchored match would
	 * leave "2/secret", which no longer looks absolute and so escapes the
	 * absolute-path redaction entirely.
	 */
	public function test_root_stripping_is_anchored_on_the_separator(): void
	{
		$abspath  = rtrim( (string) constant( 'ABSPATH' ), '/\\' );
		$response = $this->dispatchThrowing(
			new \RuntimeException( 'cannot open ' . $abspath . '2/secret-sibling-dir/payload.txt' )
		);

		$wire = $this->wireBlob( $response );

		$this->assertStringNotContainsString( 'secret-sibling-dir', $wire );
		$this->assertStringNotContainsString( 'payload.txt', $wire );
		$this->assertStringContainsString( '<path>', $wire );
	}

	/**
	 * #754, the reporter's actual case: `RuntimeException: WPMgr Agent:
	 * ciphertext authentication failed.` must arrive as something an operator
	 * can act on, not as "Command execution failed."
	 */
	public function test_command_failure_response_carries_a_usable_reason(): void
	{
		$response = $this->dispatchThrowing(
			new \RuntimeException( 'WPMgr Agent: ciphertext authentication failed.' )
		);

		$this->assertInstanceOf( \WP_Error::class, $response );
		$this->assertSame( 'wpmgr_command_failed', $response->get_error_code() );

		$message = $response->get_error_message();
		$this->assertStringContainsString( 'ciphertext authentication failed', $message );
		$this->assertStringContainsString( 'RuntimeException', $message );

		$data = $response->get_error_data();
		$this->assertSame( 500, $data['status'] ?? null );
		$this->assertSame( 'boom', $data['command'] ?? null );
		$this->assertSame( 'RuntimeException', $data['exception'] ?? null );

		// The location is relative to the plugin root, never absolute.
		$at = (string) ( $data['at'] ?? '' );
		$this->assertMatchesRegularExpression( '~^[^/].*:\d+$~', $at, 'location must be relative and carry a line number' );
		$this->assertStringContainsString( 'RouterTest.php', $at );
	}

	/**
	 * #754: the reason is length-capped so it cannot blow the control plane's
	 * 512-byte body clamp.
	 */
	public function test_command_failure_reason_is_length_capped(): void
	{
		$response = $this->dispatchThrowing( new \RuntimeException( str_repeat( 'verbose failure. ', 200 ) ) );

		$message = $response->get_error_message();

		$this->assertStringContainsString( '...(truncated)', $message );
		$this->assertLessThan( 400, strlen( $message ), 'a capped reason must stay well inside the 512-byte body clamp' );
	}

	/**
	 * #754 over-fire control: a command that SUCCEEDS is completely unaffected —
	 * same 200, same payload, and no error surface at all.
	 */
	public function test_successful_command_is_unaffected_by_the_failure_path(): void
	{
		$response = $this->dispatchCommand( 'test_cmd' );

		$this->assertInstanceOf( \WP_REST_Response::class, $response );
		$this->assertNotInstanceOf( \WP_Error::class, $response );
		$this->assertSame( 200, $response->status );
		$this->assertSame( [ 'handled_by' => 'test_cmd' ], $response->data );
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
	 * Dispatch a command whose handler throws, through the real production
	 * path: handleCommand() -> dispatch() -> the catch under test.
	 *
	 * @param \Throwable $throwable What the handler throws.
	 * @return \WP_Error
	 */
	private function dispatchThrowing( \Throwable $throwable ): \WP_Error
	{
		$router = new Router( $this->connector, [ $this->makeThrowingCommand( 'boom', $throwable ) ] );

		$request = new \WP_REST_Request(
			[
				'command'      => 'boom',
				'wpmgr_claims' => $this->fakeClaims,
			]
		);

		$response = $router->handleCommand( $request );
		$this->assertInstanceOf( \WP_Error::class, $response );

		return $response;
	}

	/**
	 * Everything about a WP_Error that goes out on the wire, as one string:
	 * the message plus every value in the error data. A leak assertion must
	 * cover the data bag, not just the message.
	 *
	 * @param \WP_Error $error The error.
	 * @return string
	 */
	private function wireBlob( \WP_Error $error ): string
	{
		return (string) json_encode(
			[
				'code'    => $error->get_error_code(),
				'message' => $error->get_error_message(),
				'data'    => $error->get_error_data(),
			]
		);
	}

	/**
	 * Build a CommandInterface stub whose execute() throws.
	 *
	 * @param string     $name      Command name.
	 * @param \Throwable $throwable What execute() throws.
	 * @return CommandInterface
	 */
	private function makeThrowingCommand( string $name, \Throwable $throwable ): CommandInterface
	{
		return new class( $name, $throwable ) implements CommandInterface {
			private string $n;

			private \Throwable $t;

			public function __construct( string $n, \Throwable $t )
			{
				$this->n = $n;
				$this->t = $t;
			}

			public function name(): string
			{
				return $this->n;
			}

			/** @return CommandEffect A double that only throws reads nothing and changes nothing. */
			public function effect(): CommandEffect
			{
				return CommandEffect::Read;
			}

			/** @return CommandRepeatability Throwing the same exception every time converges. */
			public function repeatability(): CommandRepeatability
			{
				return CommandRepeatability::Idempotent;
			}

			/** @param array<string,mixed> $claims @param array<string,mixed> $params @return array<string,mixed> */
			public function execute( array $claims, array $params ): array
			{
				throw $this->t;
			}
		};
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
