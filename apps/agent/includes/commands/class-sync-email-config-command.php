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
 * pushes. The three signals are therefore:
 *
 *   secret: "<non-empty>"   replace the stored secret
 *   clear_secret: true      remove the stored secret
 *   neither of those        leave the stored secret exactly as it is
 *
 * A non-empty `secret` wins if both arrive in one push: it is the newer
 * credential and it is bound to the settings travelling beside it. Named
 * connections follow the same three rules via their own per-entry
 * `clear_secret` flag.
 *
 * WHY THE CONTROL PLANE MUST SEND clear_secret. The stored secret is a single
 * keystore entry with no binding to the provider, host or username it was
 * issued for, and this command cannot see whether a pushed setting is a
 * correction or a move to a different account. So a push that changes the
 * authenticating identity (the provider slug, or for a host-based provider its
 * host, port or username) and carries no new secret MUST also carry
 * `clear_secret: true`, or the old credential is offered to the new endpoint.
 * The control plane holds both the previous and the incoming settings and is
 * the only side that can compare them; it owns that decision. Re-pushing
 * identical settings changes no identity and so never clears anything, which
 * is what keeps the #380 fix intact.
 *
 * WRITE ORDER. These are two separate stores with no transaction between them,
 * so the order decides what a half-finished push leaves behind. The credential
 * is settled in the keystore first and the settings are written only once that
 * succeeded, because the failure that ordering produces is a site still running
 * on the settings its credential was issued for. The reverse order produces the
 * opposite: settings already pointing at a new endpoint while the keystore
 * still holds the credential the clear was meant to retire, which is exactly
 * the rebinding clear_secret exists to prevent. A keystore write this push
 * asked for is therefore fatal to the whole command: nothing is persisted, the
 * control plane sees the failure and retries. The one exception is a pure
 * carry-forward of connection secrets, which rewrites the values already
 * stored and so cannot diverge from them by failing.
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
		// Only a strict boolean true clears; a truthy string or 1 is a
		// serialisation accident, and guessing at one deletes a credential.
		// A secret supplied in the same push takes precedence (see the class docblock).
		if ( $secret === null && ( $params['clear_secret'] ?? null ) === true ) {
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
		// True once this push says something of its own about a connection
		// secret: it supplied one, or it asked for one to be cleared. While this
		// stays false every entry in $conn_secrets came from a read-back, and an
		// empty result means the read-back found nothing rather than that the
		// operator removed anything.
		$conn_push_is_authoritative = false;
		$conn_entries               = 0;
		if ( array_key_exists( 'connections', $params ) && is_array( $params['connections'] ) ) {
			foreach ( $params['connections'] as $conn_key => $wire ) {
				if ( ! is_array( $wire ) ) {
					continue;
				}
				++$conn_entries;
				$key   = (string) $conn_key;
				$clear = ( $wire['clear_secret'] ?? null ) === true;
				if ( isset( $wire['secret'] ) && is_string( $wire['secret'] ) && $wire['secret'] !== '' ) {
					$conn_secrets[ $key ]       = $wire['secret'];
					$conn_push_is_authoritative = true;
				} elseif ( $clear ) {
					$conn_push_is_authoritative = true;
				} else {
					$existing = $this->keystore->get_connection_secret( $key );
					if ( $existing !== '' ) {
						$conn_secrets[ $key ] = $existing;
					}
				}
				// Strip the secret from the wire payload before it reaches the wp-option.
				unset( $params['connections'][ $conn_key ]['secret'], $params['connections'][ $conn_key ]['clear_secret'] );
			}
			// An emptied registry drops every stored connection secret, which is
			// a deliberate replace-all rather than a read-back that found
			// nothing. Nothing is carried forward, so nothing can be lost.
			if ( $conn_entries === 0 ) {
				$conn_push_is_authoritative = true;
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

		// Build and validate the config object before anything is persisted.
		// This is a pure construction: a malformed push is rejected here, with
		// neither store touched.
		try {
			$cfg = new EmailConfig( $clean );
		} catch ( \Throwable $e ) {
			return array( 'ok' => false, 'detail' => 'failed to persist email config' );
		}

		// Settle the primary secret in the keystore, but only when this push
		// actually carried one (or explicitly asked to clear it). The secret was
		// transmitted only in the signed JWT body over HTTPS; we never log it.
		// This runs before the settings are written: see WRITE ORDER above.
		if ( $secret !== null ) {
			try {
				$this->keystore->storeEmailSecret( $secret );
			} catch ( \Throwable $e ) {
				return array(
					'ok'     => false,
					'detail' => $secret === ''
						? 'email config not saved; the stored secret could not be cleared'
						: 'email config not saved; secret storage failed',
				);
			}
		}

		// Persist per-connection secrets atomically (replace-all semantics).
		// If the payload has no connections key at all, leave the existing
		// connection secrets untouched (old-CP compat: payload has no 'connections').
		//
		// One case must not be written: connections were pushed, none of them
		// said anything about a secret, and the carry-forward read-back came up
		// empty. The keystore returns '' both for "nothing stored" and for
		// "stored but it would not decrypt", so that result may mean the whole
		// map is still there and merely unreadable this request. Writing the
		// empty map deletes the option, which is the very destruction the
		// carry-forward exists to prevent. Leaving it alone costs nothing: a
		// genuinely empty store stays empty.
		$carry_forward_came_up_empty = $conn_entries > 0
			&& ! $conn_push_is_authoritative
			&& $conn_secrets === array();
		if ( array_key_exists( 'connections', $params ) && ! $carry_forward_came_up_empty ) {
			try {
				$this->keystore->store_connection_secrets( $conn_secrets );
			} catch ( \Throwable $e ) {
				if ( $conn_push_is_authoritative ) {
					// This push meant to supply or retire a connection
					// credential and could not. Writing the settings now would
					// leave the map this push meant to change bound to
					// endpoints it did not choose, so nothing is written.
					return array( 'ok' => false, 'detail' => 'email config not saved; connection secrets could not be stored' );
				}
				// A pure carry-forward rewrites what is already stored, so
				// failing it leaves the stored map exactly where the settings
				// expect it. Sends keep working; the config write goes ahead.
			}
		}

		// Persist the non-secret config, now that every credential this push
		// asked to change is settled in the keystore.
		try {
			if ( function_exists( 'update_option' ) ) {
				update_option( EmailConfig::OPTION, $cfg->to_array(), false );
			}
		} catch ( \Throwable $e ) {
			return array( 'ok' => false, 'detail' => 'failed to persist email config' );
		}

		if ( $secret === null ) {
			$detail = 'email config saved; stored secret preserved';
		} elseif ( $secret === '' ) {
			$detail = 'email config saved; secret cleared';
		} else {
			$detail = 'email config saved';
		}
		return array( 'ok' => true, 'detail' => $detail );
	}
}
