<?php
/**
 * EmailLogReporterTest — validates the batch-payload shape, cursor advancement,
 * and the "nothing to push" short-circuit, all without any real network I/O.
 *
 * Stubs: wp_remote_post (Brain Monkey Functions), global $wpdb replaced with a
 * lightweight FakeEmailWpdb double defined at the bottom of this file.
 *
 * Both Settings and Signer are `final` and cannot be mocked via PHPUnit's
 * createMock(), so we use real instances:
 *   - Settings  backed by a Brain Monkey get_option stub (option store array).
 *   - Signer    backed by a real Keystore with a temp master-key file (mirroring
 *               LifecycleTest).
 *
 * Important: the `fetchBatch()` method uses `object $wpdb` (not `\wpdb`) so a
 * lightweight fake can be passed without triggering a TypeError in strict mode.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\EmailLogReporter;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;

/**
 * @covers \WPMgr\Agent\Email\EmailLogReporter
 */
class EmailLogReporterTest extends TestCase {

	/** Temp file for the master key used by the test Keystore. */
	private string $keyFile = '';

	/** @var array<string,mixed> */
	private array $optionStore = [];

	private Settings $settings;

	private Signer $signer;

	protected function setUp(): void {
		parent::setUp();
		Monkey\setUp();

		$this->optionStore = [];

		Functions\when( 'get_option' )->alias(
			fn ( $k, $d = false ) => $this->optionStore[ $k ] ?? $d
		);
		Functions\when( 'update_option' )->alias( function ( $k, $v ) {
			$this->optionStore[ $k ] = $v;
			return true;
		} );
		Functions\when( 'delete_option' )->alias( function ( $k ) {
			unset( $this->optionStore[ $k ] );
			return true;
		} );

		// Two-tier Settings reads try get_site_option first; return the sentinel
		// so the fallback path uses our get_option stub above.
		Functions\when( 'get_site_option' )->justReturn( '__wpmgr_settings_missing__' );
		Functions\when( 'is_multisite' )->justReturn( false );

		Functions\when( 'is_wp_error' )->justReturn( false );
		Functions\when( 'wp_remote_retrieve_response_code' )->justReturn( 200 );
		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":0}' );

		// wp_json_encode must be stubbed in setUp so function_exists() returns true
		// when the reporter's guard check runs inside doPush().
		Functions\when( 'wp_json_encode' )->alias( static fn ( $v ) => json_encode( $v ) );

		// Build a real Signer from a temp-file Keystore, mirroring LifecycleTest.
		// WPMGR_AGENT_KEY_FILE is a PHP constant, defined ONCE per process. After
		// the first test's tearDown deletes the file, subsequent setUp calls must
		// re-write fresh key bytes to the constant path so Keystore::masterKey()
		// finds a valid file and re-derives the same key used to encrypt the keypair.
		$this->keyFile = sys_get_temp_dir() . '/wpmgr-email-reporter-test-' . bin2hex( random_bytes( 8 ) ) . '.key';
		if ( ! defined( 'WPMGR_AGENT_KEY_FILE' ) ) {
			define( 'WPMGR_AGENT_KEY_FILE', $this->keyFile );
		}
		// Write a fresh 32-byte master key to whichever path the constant points to.
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test-only setup, no WP_Filesystem context
		file_put_contents( (string) constant( 'WPMGR_AGENT_KEY_FILE' ), random_bytes( 32 ) );

		$keystore = new Keystore();
		\WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
		$this->signer = new Signer( $keystore );

		// Pre-enroll: Settings::isEnrolled() requires site_id + cp_url both non-empty.
		$this->optionStore[ Settings::OPTION_SITE_ID ] = 'test-site-uuid';
		$this->optionStore[ Settings::OPTION_CP_URL ]  = 'https://cp.example.com';
		$this->settings = new Settings();
	}

	protected function tearDown(): void {
		unset( $GLOBALS['wpdb'] );
		if ( $this->keyFile !== '' && is_file( $this->keyFile ) ) {
			@unlink( $this->keyFile ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only cleanup
		}
		Monkey\tearDown();
		parent::tearDown();
	}

	// -------------------------------------------------------------------------
	// Helpers
	// -------------------------------------------------------------------------

	/**
	 * Install a FakeEmailWpdb with the given rows as global $wpdb.
	 *
	 * @param array<int,array<string,mixed>> $rows
	 * @return FakeEmailWpdb
	 */
	private function makeWpdb( array $rows ): FakeEmailWpdb {
		$wpdb              = new FakeEmailWpdb();
		$wpdb->rows        = $rows;
		$GLOBALS['wpdb']   = $wpdb;
		return $wpdb;
	}

	// -------------------------------------------------------------------------
	// Tests
	// -------------------------------------------------------------------------

	/**
	 * When there are no rows above the cursor the reporter must not call
	 * wp_remote_post at all.
	 */
	public function test_push_nothing_when_caught_up(): void {
		$this->makeWpdb( [] );

		Functions\expect( 'wp_remote_post' )->never();

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		// Cursor must remain absent (no update_option call for cursor key).
		$this->assertArrayNotHasKey( EmailLogReporter::OPTION_CURSOR, $this->optionStore );
	}

	/**
	 * Given buffered rows above the cursor, the reporter should:
	 *   - call wp_remote_post exactly once (batch < BATCH_SIZE),
	 *   - include id → agent_seq mapping,
	 *   - split mail_to into to_addresses array,
	 *   - omit body when body_stored = 0,
	 *   - advance the cursor to acked_through from the response.
	 */
	public function test_push_builds_correct_payload_and_advances_cursor(): void {
		$this->makeWpdb( [
			[
				'id'           => '5',
				'message_id'   => 'sg-abc-001',
				'mail_to'      => 'alice@example.com, bob@example.com',
				'mail_from'    => 'sender@example.com',
				'subject'      => 'Hello world',
				'provider'     => 'sendgrid',
				'status'       => 'sent',
				'response'     => '{"id":"sg-abc-001"}',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '2026-06-10 12:34:56',
			],
		] );

		// CP acknowledges through seq 5 — override the setUp default (acked_through:0).
		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":5}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		// wp_remote_post was called.
		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );

		// Decode the body that was sent.
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$this->assertIsArray( $payload );
		$this->assertArrayHasKey( 'entries', $payload );
		$this->assertCount( 1, $payload['entries'] );

		$entry = $payload['entries'][0];

		// id → agent_seq mapping.
		$this->assertSame( 5, $entry['agent_seq'] );

		// mail_to split into to_addresses array.
		$this->assertSame( [ 'alice@example.com', 'bob@example.com' ], $entry['to_addresses'] );

		// from_address mapping.
		$this->assertSame( 'sender@example.com', $entry['from_address'] );

		// message_id passthrough.
		$this->assertSame( 'sg-abc-001', $entry['message_id'] );

		// body omitted when body_stored = 0.
		$this->assertFalse( $entry['body_stored'] );
		$this->assertArrayNotHasKey( 'body', $entry );

		// created_at in RFC3339 UTC format.
		$this->assertSame( '2026-06-10T12:34:56+00:00', $entry['created_at'] );

		// Cursor advanced to acked_through.
		$this->assertSame( 5, $this->optionStore[ EmailLogReporter::OPTION_CURSOR ] );
	}

	/**
	 * When body_stored = 1 the entry MUST include the body field.
	 */
	public function test_push_includes_body_when_body_stored(): void {
		$this->makeWpdb( [
			[
				'id'           => '7',
				'message_id'   => 'mg-xyz-002',
				'mail_to'      => 'carol@example.com',
				'mail_from'    => 'noreply@example.com',
				'subject'      => 'Newsletter',
				'provider'     => 'mailgun',
				'status'       => 'sent',
				'response'     => '',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '1',
				'body'         => '<p>Hello Carol</p>',
				'created_at'   => '2026-06-10 09:00:00',
			],
		] );

		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":7}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$entry   = $payload['entries'][0];

		$this->assertTrue( $entry['body_stored'] );
		$this->assertArrayHasKey( 'body', $entry );
		$this->assertSame( '<p>Hello Carol</p>', $entry['body'] );
	}

	/**
	 * When the CP returns a non-2xx the cursor must NOT be advanced.
	 */
	public function test_push_does_not_advance_cursor_on_http_error(): void {
		$this->makeWpdb( [
			[
				'id'           => '3',
				'message_id'   => '',
				'mail_to'      => 'dave@example.com',
				'mail_from'    => '',
				'subject'      => '',
				'provider'     => 'smtp',
				'status'       => 'failed',
				'response'     => '',
				'error'        => 'Connection refused',
				'retries'      => '1',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '2026-06-10 08:00:00',
			],
		] );

		// CP returns 500.
		Functions\when( 'wp_remote_retrieve_response_code' )->justReturn( 500 );
		Functions\when( 'wp_remote_post' )->justReturn( [ 'response' => [ 'code' => 500 ] ] );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertArrayNotHasKey(
			EmailLogReporter::OPTION_CURSOR,
			$this->optionStore,
			'Cursor must not be updated on a CP error response'
		);
	}

	/**
	 * push() is a no-op when the site is not enrolled.
	 */
	public function test_push_no_op_when_not_enrolled(): void {
		Functions\expect( 'wp_remote_post' )->never();

		// Unenrolled settings: remove the site_id so isEnrolled() returns false.
		unset( $this->optionStore[ Settings::OPTION_SITE_ID ] );
		$unenrolledSettings = new Settings();

		$reporter = new EmailLogReporter( $unenrolledSettings, $this->signer );
		$reporter->push();

		// No cursor should be stored.
		$this->assertArrayNotHasKey(
			EmailLogReporter::OPTION_CURSOR,
			$this->optionStore,
			'Cursor must not be written when not enrolled'
		);
	}

	/**
	 * Cursor option key is the expected constant value.
	 */
	public function test_option_cursor_key_is_correct(): void {
		$this->assertSame( 'wpmgr_email_log_cursor', EmailLogReporter::OPTION_CURSOR );
	}

	/**
	 * Hook name constant is correct.
	 */
	public function test_hook_push_name_is_correct(): void {
		$this->assertSame( 'wpmgr_email_log_push', EmailLogReporter::HOOK_PUSH );
	}

	// -------------------------------------------------------------------------
	// Response-field contract: always an object (or null), never a bare string
	// -------------------------------------------------------------------------

	/**
	 * A plain-string response column value (e.g. "SMTP send OK") must be wrapped
	 * into { "summary": "<value>" } — never forwarded as a bare string.
	 */
	public function test_string_response_is_wrapped_as_summary_object(): void {
		$this->makeWpdb( [
			[
				'id'           => '10',
				'message_id'   => 'smtp-001',
				'mail_to'      => 'user@example.com',
				'mail_from'    => 'sender@example.com',
				'subject'      => 'Test',
				'provider'     => 'smtp',
				'status'       => 'sent',
				'response'     => 'SMTP send OK',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '2026-06-10 10:00:00',
			],
		] );

		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":10}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$entry   = $payload['entries'][0];

		// Must be an array (JSON object), not a bare string.
		$this->assertIsArray( $entry['response'], 'response must be an array/object, not a scalar' );
		$this->assertArrayHasKey( 'summary', $entry['response'] );
		$this->assertSame( 'SMTP send OK', $entry['response']['summary'] );
	}

	/**
	 * A JSON-object response column value must be preserved as-is (decoded array).
	 */
	public function test_json_object_response_is_preserved(): void {
		$this->makeWpdb( [
			[
				'id'           => '11',
				'message_id'   => 'sg-002',
				'mail_to'      => 'user@example.com',
				'mail_from'    => 'sender@example.com',
				'subject'      => 'Test',
				'provider'     => 'sendgrid',
				'status'       => 'sent',
				'response'     => '{"id":"sg-002","status":"delivered"}',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '2026-06-10 11:00:00',
			],
		] );

		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":11}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$entry   = $payload['entries'][0];

		$this->assertIsArray( $entry['response'] );
		$this->assertSame( 'sg-002', $entry['response']['id'] );
		$this->assertSame( 'delivered', $entry['response']['status'] );
		// Must NOT have been double-wrapped.
		$this->assertArrayNotHasKey( 'summary', $entry['response'] );
	}

	// -------------------------------------------------------------------------
	// created_at contract: always valid RFC3339, never empty or raw MySQL string
	// -------------------------------------------------------------------------

	/**
	 * An empty created_at column must produce a valid RFC3339 UTC timestamp,
	 * not an empty string.
	 */
	public function test_empty_created_at_produces_rfc3339_fallback(): void {
		$this->makeWpdb( [
			[
				'id'           => '20',
				'message_id'   => 'smtp-empty-ts',
				'mail_to'      => 'user@example.com',
				'mail_from'    => 'sender@example.com',
				'subject'      => 'Test',
				'provider'     => 'smtp',
				'status'       => 'sent',
				'response'     => '',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '',
			],
		] );

		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":20}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$entry   = $payload['entries'][0];

		// Must be a non-empty RFC3339 string (YYYY-MM-DDTHH:...).
		$this->assertNotSame( '', $entry['created_at'], 'created_at must not be empty' );
		$this->assertMatchesRegularExpression(
			'/^\d{4}-\d{2}-\d{2}T/',
			$entry['created_at'],
			'created_at must match RFC3339 pattern'
		);
	}

	/**
	 * A normal MySQL DATETIME string (Y-m-d H:i:s, UTC) must convert to the
	 * correct RFC3339 UTC representation.
	 */
	public function test_mysql_datetime_converts_to_rfc3339(): void {
		$this->makeWpdb( [
			[
				'id'           => '21',
				'message_id'   => 'smtp-normal-ts',
				'mail_to'      => 'user@example.com',
				'mail_from'    => 'sender@example.com',
				'subject'      => 'Test',
				'provider'     => 'smtp',
				'status'       => 'sent',
				'response'     => '',
				'error'        => '',
				'retries'      => '0',
				'resent_count' => '0',
				'body_stored'  => '0',
				'body'         => null,
				'created_at'   => '2026-06-10 15:30:00',
			],
		] );

		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":21}' );

		/** @var array<string,mixed>|null $capturedArgs */
		$capturedArgs = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$capturedArgs ) {
			$capturedArgs = $args;
			return [ 'response' => [ 'code' => 200 ] ];
		} );

		$reporter = new EmailLogReporter( $this->settings, $this->signer );
		$reporter->push();

		$this->assertNotNull( $capturedArgs, 'wp_remote_post was not called' );
		$payload = json_decode( (string) $capturedArgs['body'], true );
		$entry   = $payload['entries'][0];

		// DateTimeImmutable with DateTime::RFC3339 produces "+00:00" suffix for UTC.
		$this->assertSame( '2026-06-10T15:30:00+00:00', $entry['created_at'] );
	}
}

// ---------------------------------------------------------------------------
// Lightweight wpdb double — used instead of getMockBuilder(\wpdb::class) to
// avoid Patchwork's "MethodCannotBeConfiguredException" on wpdb::prepare.
// EmailLogReporter::fetchBatch() accepts `object $wpdb` (not `\wpdb`) so this
// plain class passes the type check even with declare(strict_types=1).
// ---------------------------------------------------------------------------

/**
 * Minimal wpdb double for EmailLogReporter tests.
 */
class FakeEmailWpdb {

	/** @var string */
	public string $prefix = 'wp_';

	/** @var array<int,array<string,mixed>> */
	public array $rows = [];

	/**
	 * Naive prepare: return the SQL with placeholders intact.
	 * The reporter reads from the DB but does not inspect the exact SQL string
	 * in tests, so this is safe.
	 *
	 * @param string $query  SQL with placeholders.
	 * @param mixed  ...$args Bound arguments (ignored).
	 * @return string
	 */
	public function prepare( string $query, ...$args ): string {
		return $query;
	}

	/**
	 * Return the pre-loaded rows on the first call; return empty on subsequent
	 * calls to simulate "caught up" after the first batch.
	 *
	 * @param string $query   Prepared SQL (ignored).
	 * @param string $output  Output format (ignored).
	 * @return array<int,array<string,mixed>>
	 */
	public function get_results( string $query, string $output = ARRAY_A ): array {
		$rows       = $this->rows;
		$this->rows = [];
		return $rows;
	}
}
