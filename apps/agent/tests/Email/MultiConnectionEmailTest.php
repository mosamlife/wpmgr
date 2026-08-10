<?php
/**
 * MultiConnectionEmailTest — validates the m62/v11 multi-connection and
 * attachment features across the agent email layer.
 *
 * Covers:
 *   - SyncEmailConfigCommand: parses connections + legacy (no-connections) payloads;
 *     strips per-connection secrets before wp-option write; stores them in keystore.
 *   - ProviderRouter: mapping-hit (string key), mapping-miss → default, named
 *     default_connection, fallback same-key guard, sender identity override,
 *     secrets map (named vs primary), fallback prefixes error.
 *   - EmailLogger: connection_key + attachments written; cap 50; JSON shape.
 *   - EmailLogReporter: new columns selected and emitted in payload.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Commands\SyncEmailConfigCommand;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\EmailLogReporter;
use WPMgr\Agent\Email\ProviderHandlerInterface;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Keystore;

/**
 * @covers \WPMgr\Agent\Commands\SyncEmailConfigCommand
 * @covers \WPMgr\Agent\Email\ProviderRouter
 * @covers \WPMgr\Agent\Email\EmailLogger
 * @covers \WPMgr\Agent\Email\EmailLogReporter
 * @covers \WPMgr\Agent\Email\EmailConfig
 */
class MultiConnectionEmailTest extends TestCase {

	protected function setUp(): void {
		parent::setUp();
		Monkey\setUp();
	}

	protected function tearDown(): void {
		Monkey\tearDown();
		parent::tearDown();
	}

	// =========================================================================
	// Helpers
	// =========================================================================

	/** @param array{ok:bool,message_id:string,error:string,provider_response:string} $result */
	private function make_handler( string $provider, array $result ): ProviderHandlerInterface {
		$handler = $this->createMock( ProviderHandlerInterface::class );
		$handler->method( 'provider' )->willReturn( $provider );
		$handler->method( 'send' )->willReturn( $result );
		return $handler;
	}

	private function make_keystore( string $secret = 'default-secret' ): FakeKeystore {
		return new FakeKeystore( $secret );
	}

	private function make_logger(): EmailLogger {
		$logger = $this->createMock( EmailLogger::class );
		$logger->method( 'write' )->willReturn( 1 );
		return $logger;
	}

	/** @return array<string,mixed> */
	private function base_mail( string $from = 'a@example.com' ): array {
		return array(
			'to'          => array( 'to@example.com' ),
			'cc'          => array(),
			'bcc'         => array(),
			'reply_to'    => array(),
			'from'        => $from,
			'from_name'   => 'Sender',
			'subject'     => 'Hello',
			'body_text'   => 'Body',
			'body_html'   => '',
			'charset'     => 'UTF-8',
			'headers'     => array(),
			'attachments' => array(),
			'return_path' => false,
			'x_site_id'   => 'site-uuid',
			'x_tenant_id' => 'tenant-uuid',
		);
	}

	// =========================================================================
	// A) SyncEmailConfigCommand — connections + legacy payloads
	// =========================================================================

	/**
	 * A v1 payload (no 'connections' key) should work exactly as before.
	 * No store_connection_secrets() call must be made.
	 */
	public function test_sync_legacy_payload_no_connections_field(): void {
		Functions\when( 'get_option' )->alias( fn( $k ) => $k === EmailConfig::OPTION ? [] : false );
		Functions\when( 'update_option' )->justReturn( true );

		$keystore = $this->make_keystore();
		$cmd      = new SyncEmailConfigCommand( $keystore );
		$result   = $cmd->execute( [], array( 'provider' => 'smtp', 'secret' => 'smtp-pass' ) );

		$this->assertTrue( $result['ok'] );
		// store_connection_secrets must NOT have been called when 'connections' absent.
		$this->assertCount( 0, $keystore->stored_conn_secrets, 'store_connection_secrets should not be called for legacy payloads' );
	}

	/**
	 * A m62 payload with connections map should:
	 *   - Strip per-connection secrets before the wp-option write.
	 *   - Store stripped secrets in the keystore.
	 *   - Persist non-secret wire fields (provider, config, from_address, from_name).
	 */
	public function test_sync_parses_connections_and_strips_secrets(): void {
		/** @var array<string,mixed>|null $saved_option */
		$saved_option = null;

		Functions\when( 'get_option' )->alias( fn( $k ) => $k === EmailConfig::OPTION ? [] : false );
		Functions\when( 'update_option' )->alias(
			function ( string $key, $value ) use ( &$saved_option ) {
				if ( $key === EmailConfig::OPTION ) {
					$saved_option = $value;
				}
				return true;
			}
		);
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		$keystore = $this->make_keystore();
		$cmd      = new SyncEmailConfigCommand( $keystore );
		$result   = $cmd->execute( [], array(
			'provider'   => 'sendgrid',
			'secret'     => 'primary-key',
			'connections' => array(
				'backup' => array(
					'provider'     => 'smtp',
					'config'       => array( 'host' => 'smtp.backup.com' ),
					'from_address' => 'backup@example.com',
					'from_name'    => 'Backup Sender',
					'secret'       => 'smtp-password',
				),
			),
			'fallback_connection' => 'backup',
		) );

		$this->assertTrue( $result['ok'] );

		// Secret must be stripped from stored connections.
		$this->assertIsArray( $saved_option );
		$this->assertArrayHasKey( 'connections', $saved_option );
		$this->assertArrayNotHasKey( 'secret', $saved_option['connections']['backup'] );
		// Non-secret fields must be preserved.
		$this->assertSame( 'smtp', $saved_option['connections']['backup']['provider'] );
		$this->assertSame( 'backup@example.com', $saved_option['connections']['backup']['from_address'] );

		// Per-connection secret must be stored in the keystore.
		$this->assertCount( 1, $keystore->stored_conn_secrets );
		$this->assertSame( 'smtp-password', $keystore->stored_conn_secrets[0]['backup'] );
	}

	/**
	 * A connections payload that says nothing about any secret must leave the
	 * stored map alone (GH #380). This test asserted the opposite until the
	 * secret lifecycle was fixed: it required store_connection_secrets([]),
	 * which deletes the option, so a routine registry re-push destroyed every
	 * connection credential. Clearing now needs a per-connection clear_secret.
	 */
	public function test_sync_connections_with_no_secrets_leaves_the_stored_map_alone(): void {
		Functions\when( 'get_option' )->alias( fn( $k ) => $k === EmailConfig::OPTION ? [] : false );
		Functions\when( 'update_option' )->justReturn( true );
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		$keystore = $this->make_keystore();
		$cmd      = new SyncEmailConfigCommand( $keystore );
		$cmd->execute( [], array(
			'provider'    => 'sendgrid',
			'connections' => array(
				'secondary' => array(
					'provider' => 'mailgun',
					'config'   => array( 'domain_name' => 'mg.example.com' ),
					// No 'secret' key.
				),
			),
		) );

		// The keystore must not be written at all: an empty map here would be a
		// delete_option(), and the read-back cannot prove the map is empty.
		$this->assertSame( array(), $keystore->stored_conn_secrets );
	}

	/**
	 * Rejects non-array connections value.
	 */
	public function test_sync_rejects_non_array_connections(): void {
		$cmd    = new SyncEmailConfigCommand( $this->make_keystore() );
		$result = $cmd->execute( [], array( 'connections' => 'not-an-array' ) );

		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'connections', $result['detail'] );
	}

	/**
	 * Rejects non-string default_connection value.
	 */
	public function test_sync_rejects_non_string_default_connection(): void {
		$cmd    = new SyncEmailConfigCommand( $this->make_keystore() );
		$result = $cmd->execute( [], array( 'default_connection' => 123 ) );

		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'default_connection', $result['detail'] );
	}

	/**
	 * Rejects non-string fallback_connection value.
	 */
	public function test_sync_rejects_non_string_fallback_connection(): void {
		$cmd    = new SyncEmailConfigCommand( $this->make_keystore() );
		$result = $cmd->execute( [], array( 'fallback_connection' => false ) );

		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'fallback_connection', $result['detail'] );
	}

	// =========================================================================
	// B) ProviderRouter — routing matrix
	// =========================================================================

	/**
	 * Mapping value is a STRING connection key (m62 CP sends key strings).
	 * Router must look up the key in the connections registry.
	 */
	public function test_router_resolves_mapping_string_key(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'    => 'sendgrid',
			'config'      => array(),
			'mappings'    => array( 'newsletter@example.com' => 'mailgun-conn' ),
			'connections' => array(
				'mailgun-conn' => array(
					'provider' => 'mailgun',
					'config'   => array( 'domain_name' => 'mg.example.com' ),
				),
			),
			'log_emails'  => false,
		) );

		$mailgun_handler = $this->make_handler( 'mailgun', array(
			'ok'                => true,
			'message_id'        => 'mg-001',
			'error'             => '',
			'provider_response' => '200',
		) );

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $mailgun_handler );

		$mail   = $this->base_mail( 'newsletter@example.com' );
		$result = $router->send( $mail, $cfg );

		$this->assertTrue( $result['ok'] );
		$this->assertSame( 'mg-001', $result['message_id'] );
	}

	/**
	 * When the from-mapping key is not in the registry, fall through to primary.
	 */
	public function test_router_mapping_miss_falls_to_primary(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'    => 'sendgrid',
			'mappings'    => array( 'unknown@example.com' => 'nonexistent-conn' ),
			'connections' => array(), // no registry entry for the key
			'log_emails'  => false,
		) );

		$sg_handler = $this->make_handler( 'sendgrid', array(
			'ok'                => true,
			'message_id'        => 'sg-fallback',
			'error'             => '',
			'provider_response' => '202',
		) );

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $sg_handler );

		$result = $router->send( $this->base_mail( 'unknown@example.com' ), $cfg );

		$this->assertTrue( $result['ok'] );
		$this->assertSame( 'sg-fallback', $result['message_id'] );
	}

	/**
	 * Named default_connection should be used when no FROM mapping resolves.
	 */
	public function test_router_uses_named_default_connection(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'           => 'sendgrid',
			'default_connection' => 'smtp-default',
			'connections'        => array(
				'smtp-default' => array(
					'provider' => 'smtp',
					'config'   => array( 'host' => 'smtp.example.com' ),
				),
			),
			'log_emails' => false,
		) );

		$smtp_handler = $this->make_handler( 'smtp', array(
			'ok'                => true,
			'message_id'        => 'smtp-001',
			'error'             => '',
			'provider_response' => 'OK',
		) );

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $smtp_handler );

		$result = $router->send( $this->base_mail(), $cfg );

		$this->assertTrue( $result['ok'] );
		$this->assertSame( 'smtp-001', $result['message_id'] );
	}

	/**
	 * Fallback same-key guard: when fallback_connection resolves to the SAME
	 * key as the primary attempt, no retry should be made.
	 */
	public function test_router_fallback_same_key_guard_prevents_self_retry(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'           => 'sendgrid',
			// fallback_connection 'default' resolves to the primary row — same key.
			'fallback_connection' => 'default',
			'log_emails'         => false,
		) );

		$handler = $this->make_handler( 'sendgrid', array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => 'API key invalid',
			'provider_response' => '401',
		) );

		// If the guard fires correctly, send() is called exactly ONCE.
		$mock_handler = $this->createMock( ProviderHandlerInterface::class );
		$mock_handler->method( 'provider' )->willReturn( 'sendgrid' );
		$mock_handler->expects( $this->once() )->method( 'send' )->willReturn( array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => 'API key invalid',
			'provider_response' => '401',
		) );

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $mock_handler );

		$result = $router->send( $this->base_mail(), $cfg );

		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'API key invalid', $result['detail'] );
	}

	/**
	 * Fallback with a NAMED connection (different key) should retry exactly once
	 * and prefix the error with "primary(<key>) failed: ... | ".
	 */
	public function test_router_fallback_named_connection_retries_once_and_prefixes_error(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'           => 'sendgrid',
			'fallback_connection' => 'smtp-backup',
			'connections'        => array(
				'smtp-backup' => array(
					'provider' => 'smtp',
					'config'   => array( 'host' => 'smtp.backup.com' ),
				),
			),
			'log_emails' => false,
		) );

		$sg_handler = $this->make_handler( 'sendgrid', array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => 'sendgrid-down',
			'provider_response' => '503',
		) );
		$smtp_handler = $this->make_handler( 'smtp', array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => 'smtp-auth-fail',
			'provider_response' => '',
		) );

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $sg_handler );
		$router->register( $smtp_handler );

		$result = $router->send( $this->base_mail(), $cfg );

		$this->assertFalse( $result['ok'] );
		// Error must contain the primary failure prefix.
		$this->assertStringContainsString( 'primary(default) failed:', $result['detail'] );
		$this->assertStringContainsString( 'sendgrid-down', $result['detail'] );
		$this->assertStringContainsString( 'smtp-auth-fail', $result['detail'] );
	}

	/**
	 * Per-connection from_address / from_name override should be applied
	 * when a named connection carries non-empty identity fields.
	 */
	public function test_router_applies_connection_sender_identity_override(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$cfg = new EmailConfig( array(
			'provider'           => 'sendgrid',
			'default_connection' => 'branded',
			'connections'        => array(
				'branded' => array(
					'provider'     => 'sendgrid',
					'config'       => array(),
					'from_address' => 'brand@example.com',
					'from_name'    => 'Brand Name',
				),
			),
			'log_emails' => false,
		) );

		/** @var array<string,mixed>|null $captured_mail */
		$captured_mail = null;
		$handler       = $this->createMock( ProviderHandlerInterface::class );
		$handler->method( 'provider' )->willReturn( 'sendgrid' );
		$handler->method( 'send' )->willReturnCallback(
			function ( array $mail ) use ( &$captured_mail ) {
				$captured_mail = $mail;
				return array(
					'ok'                => true,
					'message_id'        => 'sg-brand',
					'error'             => '',
					'provider_response' => '202',
				);
			}
		);

		$router = new ProviderRouter( $this->make_keystore(), $this->make_logger() );
		$router->register( $handler );

		$mail = $this->base_mail( 'original@example.com' );
		$router->send( $mail, $cfg );

		$this->assertNotNull( $captured_mail );
		$this->assertSame( 'brand@example.com', $captured_mail['from'] );
		$this->assertSame( 'Brand Name', $captured_mail['from_name'] );
	}

	/**
	 * Named connection secret is fetched from get_connection_secret() (not get_email_secret()).
	 */
	public function test_router_uses_named_connection_secret(): void {
		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );

		$keystore                         = $this->make_keystore( 'primary-secret' );
		$keystore->conn_secrets['backup'] = 'backup-secret';

		$cfg = new EmailConfig( array(
			'provider'           => 'sendgrid',
			'fallback_connection' => 'backup',
			'connections'        => array(
				'backup' => array(
					'provider' => 'smtp',
					'config'   => array( 'host' => 'smtp.backup.com' ),
				),
			),
			'log_emails' => false,
		) );

		// Primary always fails.
		$sg_handler = $this->make_handler( 'sendgrid', array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => 'sg-fail',
			'provider_response' => '',
		) );

		// Capture what secret the fallback handler receives.
		$captured_secret = null;
		$smtp_handler    = $this->createMock( ProviderHandlerInterface::class );
		$smtp_handler->method( 'provider' )->willReturn( 'smtp' );
		$smtp_handler->method( 'send' )->willReturnCallback(
			function ( array $mail, array $config, string $secret ) use ( &$captured_secret ) {
				$captured_secret = $secret;
				return array(
					'ok'                => true,
					'message_id'        => 'smtp-ok',
					'error'             => '',
					'provider_response' => 'OK',
				);
			}
		);

		$router = new ProviderRouter( $keystore, $this->make_logger() );
		$router->register( $sg_handler );
		$router->register( $smtp_handler );

		$result = $router->send( $this->base_mail(), $cfg );

		$this->assertTrue( $result['ok'] );
		$this->assertSame( 'backup-secret', $captured_secret );
	}

	// =========================================================================
	// C) Attachments — capture + cap 50 + reporter shape
	// =========================================================================

	/**
	 * EmailLogger::write() must persist connection_key and attachments JSON.
	 */
	public function test_email_logger_writes_connection_key_and_attachments(): void {
		global $wpdb;

		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		/** @var array<string,mixed>|null $captured_data */
		$captured_data = null;

		$fake_wpdb         = new class() {
			public string $prefix = 'wp_';

			/** @var array<string,mixed>|null */
			public ?array $captured = null;

			public int $insert_id = 99;

			/** @param array<string,mixed> $data */
			public function insert( string $table, array $data, array $formats ): bool {
				$this->captured = $data;
				return true;
			}
		};
		$GLOBALS['wpdb'] = $fake_wpdb;

		$cfg    = new EmailConfig( array( 'provider' => 'smtp', 'log_emails' => true ) );
		$logger = new EmailLogger();

		$mail = $this->base_mail();
		$mail['attachments'] = array(
			array( 'name' => 'invoice.pdf', 'path' => '/tmp/invoice.pdf', 'mime' => 'application/pdf', 'size_bytes' => 1024 ),
			array( 'name' => 'photo.jpg', 'path' => '/tmp/photo.jpg', 'mime' => 'image/jpeg', 'size_bytes' => 204800 ),
		);

		$logger->write( $mail, 'smtp', 'sent', 'msg-001', '', 'OK', 0, $cfg, 'my-conn' );

		$this->assertNotNull( $fake_wpdb->captured );
		$this->assertSame( 'my-conn', $fake_wpdb->captured['connection_key'] );

		// Attachments should be a JSON array of {name, size_bytes}.
		$this->assertNotNull( $fake_wpdb->captured['attachments'] );
		$decoded = json_decode( $fake_wpdb->captured['attachments'], true );
		$this->assertIsArray( $decoded );
		$this->assertCount( 2, $decoded );
		$this->assertSame( 'invoice.pdf', $decoded[0]['name'] );
		$this->assertSame( 1024, $decoded[0]['size_bytes'] );
		$this->assertSame( 'photo.jpg', $decoded[1]['name'] );

		unset( $GLOBALS['wpdb'] );
	}

	/**
	 * EmailLogger caps at 50 attachments and ignores entries beyond that.
	 */
	public function test_email_logger_caps_attachments_at_50(): void {
		global $wpdb;

		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		$fake_wpdb         = new class() {
			public string $prefix = 'wp_';

			/** @var array<string,mixed>|null */
			public ?array $captured = null;

			public int $insert_id = 1;

			/** @param array<string,mixed> $data */
			public function insert( string $table, array $data, array $formats ): bool {
				$this->captured = $data;
				return true;
			}
		};
		$GLOBALS['wpdb'] = $fake_wpdb;

		$cfg         = new EmailConfig( array( 'provider' => 'smtp', 'log_emails' => true ) );
		$logger      = new EmailLogger();
		$mail        = $this->base_mail();
		$attachments = array();
		for ( $i = 0; $i < 60; $i++ ) {
			$attachments[] = array(
				'name'       => 'file-' . $i . '.txt',
				'path'       => '/tmp/file-' . $i . '.txt',
				'mime'       => 'text/plain',
				'size_bytes' => $i * 100,
			);
		}
		$mail['attachments'] = $attachments;

		$logger->write( $mail, 'smtp', 'sent', 'msg-cap', '', 'OK', 0, $cfg, 'default' );

		$this->assertNotNull( $fake_wpdb->captured );
		$decoded = json_decode( (string) $fake_wpdb->captured['attachments'], true );
		$this->assertIsArray( $decoded );
		$this->assertCount( 50, $decoded, 'Attachments must be capped at 50' );

		unset( $GLOBALS['wpdb'] );
	}

	/**
	 * EmailLogger writes NULL for attachments when no valid attachment entries are present.
	 */
	public function test_email_logger_null_attachments_when_none(): void {
		global $wpdb;

		Functions\when( 'current_time' )->justReturn( '2026-01-01 00:00:00' );
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		$fake_wpdb = new class() {
			public string $prefix = 'wp_';

			/** @var array<string,mixed>|null */
			public ?array $captured = null;

			public int $insert_id = 1;

			/** @param array<string,mixed> $data */
			public function insert( string $table, array $data, array $formats ): bool {
				$this->captured = $data;
				return true;
			}
		};
		$GLOBALS['wpdb'] = $fake_wpdb;

		$cfg    = new EmailConfig( array( 'provider' => 'smtp', 'log_emails' => true ) );
		$logger = new EmailLogger();
		$logger->write( $this->base_mail(), 'smtp', 'sent', 'msg-none', '', 'OK', 0, $cfg, 'default' );

		$this->assertNotNull( $fake_wpdb->captured );
		$this->assertNull( $fake_wpdb->captured['attachments'], 'No attachments should store NULL' );

		unset( $GLOBALS['wpdb'] );
	}

	/**
	 * EmailLogReporter includes connection_key in each batch entry and
	 * includes attachments only when non-empty.
	 * Uses the same harness pattern as EmailLogReporterTest.
	 */
	public function test_reporter_includes_connection_key_and_attachments(): void {
		$this->run_reporter_test(
			array(
				'id'             => '5',
				'message_id'     => 'sg-001',
				'mail_to'        => 'to@example.com',
				'mail_from'      => 'from@example.com',
				'subject'        => 'Hello',
				'provider'       => 'sendgrid',
				'status'         => 'sent',
				'response'       => '',
				'error'          => '',
				'retries'        => '0',
				'resent_count'   => '0',
				'body_stored'    => '0',
				'body'           => null,
				'connection_key' => 'primary-sg',
				'attachments'    => '[{"name":"doc.pdf","size_bytes":2048}]',
				'created_at'     => '2026-06-10 12:00:00',
			),
			5,
			function ( array $entry ): void {
				$this->assertSame( 'primary-sg', $entry['connection_key'] );
				$this->assertArrayHasKey( 'attachments', $entry );
				$this->assertCount( 1, $entry['attachments'] );
				$this->assertSame( 'doc.pdf', $entry['attachments'][0]['name'] );
				$this->assertSame( 2048, $entry['attachments'][0]['size_bytes'] );
			}
		);
	}

	/**
	 * EmailLogReporter omits 'attachments' key when the column is empty/null.
	 */
	public function test_reporter_omits_attachments_when_empty(): void {
		$this->run_reporter_test(
			array(
				'id'             => '6',
				'message_id'     => 'sg-002',
				'mail_to'        => 'to@example.com',
				'mail_from'      => 'from@example.com',
				'subject'        => 'Hello',
				'provider'       => 'sendgrid',
				'status'         => 'sent',
				'response'       => '',
				'error'          => '',
				'retries'        => '0',
				'resent_count'   => '0',
				'body_stored'    => '0',
				'body'           => null,
				'connection_key' => '',
				'attachments'    => null,
				'created_at'     => '2026-06-10 13:00:00',
			),
			6,
			function ( array $entry ): void {
				$this->assertSame( '', $entry['connection_key'] );
				$this->assertArrayNotHasKey( 'attachments', $entry, 'attachments key must be absent when empty' );
			}
		);
	}

	/**
	 * Shared harness for EmailLogReporter payload tests.
	 * Mirrors the setUp() pattern in EmailLogReporterTest exactly.
	 *
	 * @param array<string,mixed>       $row      Single DB row to return from the fake wpdb.
	 * @param int                       $acked    acked_through value the CP mock returns.
	 * @param callable(array):void      $assert   Assertions on the first entry in the batch.
	 */
	private function run_reporter_test( array $row, int $acked, callable $assert ): void {
		/** @var array<string,mixed> $option_store */
		$option_store = array();

		Functions\when( 'get_option' )->alias( function ( $k, $d = false ) use ( &$option_store ) {
			return $option_store[ $k ] ?? $d;
		} );
		Functions\when( 'update_option' )->alias( function ( $k, $v ) use ( &$option_store ) {
			$option_store[ $k ] = $v;
			return true;
		} );
		Functions\when( 'delete_option' )->alias( function ( $k ) use ( &$option_store ) {
			unset( $option_store[ $k ] );
			return true;
		} );
		Functions\when( 'get_site_option' )->justReturn( '__wpmgr_settings_missing__' );
		Functions\when( 'is_multisite' )->justReturn( false );
		Functions\when( 'is_wp_error' )->justReturn( false );
		Functions\when( 'wp_remote_retrieve_response_code' )->justReturn( 200 );
		Functions\when( 'wp_remote_retrieve_body' )->justReturn( '{"acked_through":' . $acked . '}' );
		Functions\when( 'wp_json_encode' )->alias( static fn( $v ) => json_encode( $v ) );

		// Ensure WPMGR_AGENT_KEY_FILE constant is defined and the file is fresh.
		// The constant may already be defined by EmailLogReporterTest if run together.
		if ( ! defined( 'WPMGR_AGENT_KEY_FILE' ) ) {
			$key_path = sys_get_temp_dir() . '/wpmgr-mc-test-' . bin2hex( random_bytes( 4 ) ) . '.key';
			define( 'WPMGR_AGENT_KEY_FILE', $key_path );
		}
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test setup
		file_put_contents( (string) constant( 'WPMGR_AGENT_KEY_FILE' ), random_bytes( 32 ) );

		$keystore = new Keystore();
		$keystore->generateSiteKeypair();
		$signer = new Signer( $keystore );

		$option_store[ Settings::OPTION_SITE_ID ] = 'test-site-mc';
		$option_store[ Settings::OPTION_CP_URL ]  = 'https://cp.example.com';
		$settings = new Settings();

		$fake_wpdb = new class() {
			public string $prefix = 'wp_';

			/** @var array<int,array<string,mixed>> */
			public array $rows = array();

			public function prepare( string $query, ...$args ): string {
				return $query;
			}

			/** @return array<int,array<string,mixed>> */
			public function get_results( string $query, string $output = ARRAY_A ): array {
				$rows       = $this->rows;
				$this->rows = array();
				return $rows;
			}
		};
		$GLOBALS['wpdb'] = $fake_wpdb;
		$fake_wpdb->rows = array( $row );

		/** @var array<string,mixed>|null $captured */
		$captured = null;
		Functions\when( 'wp_remote_post' )->alias( function ( string $url, array $args ) use ( &$captured ) {
			$captured = $args;
			return array( 'response' => array( 'code' => 200 ) );
		} );

		$reporter = new EmailLogReporter( $settings, $signer );
		$reporter->push();

		unset( $GLOBALS['wpdb'] );

		$this->assertNotNull( $captured, 'wp_remote_post was not called — check that isEnrolled() returns true and the key file is readable' );
		$payload = json_decode( (string) $captured['body'], true );
		$this->assertIsArray( $payload );
		$this->assertArrayHasKey( 'entries', $payload );
		$this->assertCount( 1, $payload['entries'] );

		$assert( $payload['entries'][0] );
	}

	// =========================================================================
	// D) EmailConfig — connections field round-trip
	// =========================================================================

	public function test_email_config_connections_round_trip(): void {
		$raw = array(
			'provider'           => 'sendgrid',
			'connections'        => array(
				'backup' => array(
					'provider'     => 'smtp',
					'config'       => array( 'host' => 'smtp.backup.com' ),
					'from_address' => 'backup@example.com',
					'from_name'    => 'Backup',
				),
			),
			'default_connection'  => '',
			'fallback_connection' => 'backup',
		);

		$cfg = new EmailConfig( $raw );

		$this->assertArrayHasKey( 'backup', $cfg->connections );
		$this->assertSame( 'smtp', $cfg->connections['backup']['provider'] );
		$this->assertSame( '', $cfg->default_connection );
		$this->assertSame( 'backup', $cfg->fallback_connection );

		// to_array must include the new fields.
		$arr = $cfg->to_array();
		$this->assertArrayHasKey( 'connections', $arr );
		$this->assertArrayHasKey( 'default_connection', $arr );
		$this->assertArrayHasKey( 'fallback_connection', $arr );
		$this->assertSame( 'backup', $arr['fallback_connection'] );
	}

	/**
	 * Connections with non-array values must be silently ignored.
	 */
	public function test_email_config_connections_ignores_malformed_entries(): void {
		$cfg = new EmailConfig( array(
			'provider'    => 'smtp',
			'connections' => array(
				'good' => array( 'provider' => 'smtp', 'config' => array() ),
				'bad'  => 'not-an-array',
				42     => array( 'provider' => 'smtp' ), // numeric key ignored
			),
		) );

		$this->assertArrayHasKey( 'good', $cfg->connections );
		$this->assertArrayNotHasKey( 'bad', $cfg->connections );
	}
}
