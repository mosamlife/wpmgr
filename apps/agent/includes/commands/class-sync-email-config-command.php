<?php
/**
 * SyncEmailConfigCommand — receives a per-site email config from the control
 * plane and persists it into wp-options + the agent keystore.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_email_config
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_email_config", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: EmailConfigRequest (see apps/api/internal/agentcmd/email_contract.go)
 *
 * Response: { "ok": bool, "detail": string }
 *
 * The secret field travels in the signed JWT-protected body over HTTPS.
 * The agent immediately AES-256-GCM-encrypts it into the keystore option
 * wpmgr_agent_email_secret and never echoes or logs it.
 *
 * SECRET LIFECYCLE (GH #380). An absent or empty `secret` means "this push
 * carries no credential", NOT "delete the stored credential": a control plane
 * that cannot resolve a site's secret sends exactly the same empty value, so
 * treating it as a delete destroyed working passwords on routine config
 * pushes. The stored secret is replaced only by a non-empty value, and removed
 * only on an explicit `clear_secret: true`. Named connections follow the same
 * rule via their own per-entry `clear_secret` flag.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailKeystoreInterface;

/**
 * Persists a CP-pushed email config into wp-options + keystore.
 */
final class SyncEmailConfigCommand implements CommandInterface {

	/** Recognised non-secret config keys (whitelist). */
	private const KNOWN_KEYS = array(
		'provider',
		'from_address',
		'from_name',
		'force_from_email',
		'force_from_name',
		'return_path',
		'config',
		'mappings',
		'connections',
		'default_connection',
		'fallback_connection',
		'log_emails',
		'store_body',
		'retention_days',
	);

	private EmailKeystoreInterface $keystore;

	/**
	 * @param EmailKeystoreInterface $keystore Agent keystore (for secret storage/removal).
	 */
	public function __construct( EmailKeystoreInterface $keystore ) {
		$this->keystore = $keystore;
	}

	/** @inheritDoc */
	public function name(): string {
		return 'sync_email_config';
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params EmailConfigRequest fields.
	 * @return array{ok:bool,detail:string}
	 */
	public function execute( array $claims, array $params ): array {
		// Validate provider if present.
		if ( array_key_exists( 'provider', $params ) ) {
			$provider = $params['provider'];
			if ( ! is_string( $provider ) ) {
				return array( 'ok' => false, 'detail' => 'provider must be a string' );
			}
			if ( ! in_array( $provider, EmailConfig::PROVIDERS, true ) ) {
				return array( 'ok' => false, 'detail' => 'provider must be one of: ' . implode( ', ', EmailConfig::PROVIDERS ) );
			}
		}

		// Validate config is an object/array if present.
		if ( array_key_exists( 'config', $params ) && ! is_array( $params['config'] ) ) {
			return array( 'ok' => false, 'detail' => 'config must be an object' );
		}

		// Validate mappings is an object/array if present.
		if ( array_key_exists( 'mappings', $params ) && ! is_array( $params['mappings'] ) ) {
			return array( 'ok' => false, 'detail' => 'mappings must be an object' );
		}

		// Validate connections is an object/array if present.
		if ( array_key_exists( 'connections', $params ) && ! is_array( $params['connections'] ) ) {
			return array( 'ok' => false, 'detail' => 'connections must be an object' );
		}

		// Validate default_connection if present.
		if ( array_key_exists( 'default_connection', $params ) && ! is_string( $params['default_connection'] ) ) {
			return array( 'ok' => false, 'detail' => 'default_connection must be a string' );
		}

		// Validate fallback_connection if present.
		if ( array_key_exists( 'fallback_connection', $params ) && ! is_string( $params['fallback_connection'] ) ) {
			return array( 'ok' => false, 'detail' => 'fallback_connection must be a string' );
		}

		// Extract the primary secret BEFORE building the config array; it is never
		// stored in the wp-option, only in the keystore.
		//
		// $secret stays null unless this push carries something to act on:
		//   null           leave whatever is already stored untouched
		//   '<non-empty>'  replace the stored secret
		//   ''             remove the stored secret (explicit clear_secret only)
		$secret = null;
		if ( array_key_exists( 'secret', $params ) ) {
			if ( ! is_string( $params['secret'] ) ) {
				return array( 'ok' => false, 'detail' => 'secret must be a string' );
			}
			if ( $params['secret'] !== '' ) {
				$secret = $params['secret'];
			}
		}
		if ( isset( $params['clear_secret'] ) && $params['clear_secret'] === true ) {
			$secret = '';
		}

		// Extract and validate per-connection secrets from the connections map.
		// Secrets are stripped from the config before writing to wp-options;
		// they are persisted separately via store_connection_secrets().
		// The secrets travel only in the signed JWT body over HTTPS; never logged.
		// store_connection_secrets() has replace-all semantics, so a connection
		// whose secret this push did not carry has its currently stored secret
		// read back and carried forward. A connection dropped from the registry
		// still loses its secret; one merely pushed without a secret does not.
		$conn_secrets = array();
		if ( array_key_exists( 'connections', $params ) && is_array( $params['connections'] ) ) {
			foreach ( $params['connections'] as $conn_key => $wire ) {
				if ( ! is_array( $wire ) ) {
					continue;
				}
				$key   = (string) $conn_key;
				$clear = isset( $wire['clear_secret'] ) && $wire['clear_secret'] === true;
				if ( isset( $wire['secret'] ) && is_string( $wire['secret'] ) && $wire['secret'] !== '' ) {
					$conn_secrets[ $key ] = $wire['secret'];
				} elseif ( ! $clear ) {
					$existing = $this->keystore->get_connection_secret( $key );
					if ( $existing !== '' ) {
						$conn_secrets[ $key ] = $existing;
					}
				}
				// Strip the secret from the wire payload before it reaches the wp-option.
				unset( $params['connections'][ $conn_key ]['secret'], $params['connections'][ $conn_key ]['clear_secret'] );
			}
		}

		// Build a clean config map from the known keys only.
		$current = EmailConfig::load()->to_array();
		$clean   = $current;
		foreach ( self::KNOWN_KEYS as $key ) {
			if ( array_key_exists( $key, $params ) ) {
				$clean[ $key ] = $params[ $key ];
			}
		}

		// Persist the non-secret config.
		try {
			$cfg = new EmailConfig( $clean );
			if ( function_exists( 'update_option' ) ) {
				update_option( EmailConfig::OPTION, $cfg->to_array(), false );
			}
		} catch ( \Throwable $e ) {
			return array( 'ok' => false, 'detail' => 'failed to persist email config' );
		}

		// Persist the primary secret into the keystore, but only when this push
		// actually carried one (or explicitly asked to clear it). The secret was
		// transmitted only in the signed JWT body over HTTPS; we never log it.
		if ( $secret !== null ) {
			try {
				$this->keystore->storeEmailSecret( $secret );
			} catch ( \Throwable $e ) {
				// Secret storage failure is non-fatal for the config itself; the
				// operator will see send failures and can retry.
				return array( 'ok' => true, 'detail' => 'email config saved; secret storage failed: keystore unavailable' );
			}
		}

		// Persist per-connection secrets atomically (replace-all semantics).
		// If the payload has no connections key at all, leave the existing
		// connection secrets untouched (old-CP compat: payload has no 'connections').
		if ( array_key_exists( 'connections', $params ) ) {
			try {
				$this->keystore->store_connection_secrets( $conn_secrets );
			} catch ( \Throwable $e ) {
				// Non-fatal: connections will still work; secrets missing means
				// sends will attempt with empty credential (provider will error).
			}
		}

		$detail = $secret === '' ? 'email config saved; secret cleared' : 'email config saved';
		if ( $secret === null ) {
			$detail = 'email config saved; stored secret preserved';
		}
		return array( 'ok' => true, 'detail' => $detail );
	}
}
