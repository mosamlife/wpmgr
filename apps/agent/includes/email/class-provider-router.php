<?php
/**
 * ProviderRouter: resolves the right provider handler for a given message,
 * drives send + one fallback retry, and delegates logging to EmailLogger.
 *
 * Resolution order:
 *   1. Suppression check: any recipient whose sha256(lower(email)) is in the
 *      local SuppressionCache is filtered out before dispatch. If ALL recipients
 *      are suppressed the send is failed immediately (logged as 'suppressed').
 *   2. Per-FROM mapping: cfg->mappings[from_address] is looked up. In v1 the
 *      value is a connection_key STRING (CP sends key strings after m62). Old
 *      agents that received an inline-array value are handled by the is_array()
 *      back-compat branch for zero-downtime rollouts.
 *   3. Resolved connection key -> registry lookup in cfg->connections.
 *   4. cfg->default_connection if set and exists in the registry.
 *   5. Primary row (cfg->provider / cfg->config / primary keystore secret).
 *
 * On failure the router retries ONCE via the configured fallback_connection IFF:
 *   - $disable_fallback is false, AND
 *   - fallback_connection resolves to a valid registered connection, AND
 *   - the resolved fallback key is DIFFERENT from the key just attempted.
 *
 * The final log row records:
 *   - The FINAL attempt's provider + connection_key.
 *   - retries = 1 when the fallback was used.
 *   - When a fallback was used, the error field is prefixed with
 *     "primary(<key>) failed: <error> | " so the CP log surfaces both legs.
 *
 * Per-connection from_address / from_name: when a resolved named connection
 * carries non-empty from_address or from_name, those values OVERRIDE the
 * mail payload's from/from_name BEFORE the provider handler sees them.
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

use WPMgr\Agent\Email\EmailKeystoreInterface;

/**
 * Resolves the outgoing-mail handler and drives send + log for a single message.
 */
class ProviderRouter {

	private EmailKeystoreInterface $keystore;

	private EmailLogger $logger;

	/** @var array<string,ProviderHandlerInterface> Slug => handler instance. */
	private array $handlers = array();

	/** Optional suppression checker for pre-send recipient checks. */
	private ?SuppressionCheckerInterface $suppression_cache = null;

	/**
	 * @param EmailKeystoreInterface          $keystore          Agent keystore (for secret retrieval).
	 * @param EmailLogger                     $logger            Local send-event logger.
	 * @param SuppressionCheckerInterface|null $suppression_cache Optional local suppression checker.
	 */
	public function __construct(
		EmailKeystoreInterface $keystore,
		EmailLogger $logger,
		?SuppressionCheckerInterface $suppression_cache = null
	) {
		$this->keystore          = $keystore;
		$this->logger            = $logger;
		$this->suppression_cache = $suppression_cache;
	}

	/**
	 * Register a provider handler. Called during Plugin boot.
	 *
	 * @param ProviderHandlerInterface $handler Provider handler.
	 * @return void
	 */
	public function register( ProviderHandlerInterface $handler ): void {
		$this->handlers[ $handler->provider() ] = $handler;
	}

	/**
	 * Send a normalised mail payload via the resolved provider handler.
	 *
	 * Before dispatching, any recipient whose sha256(lower(email)) is in the
	 * local SuppressionCache is removed. When ALL recipients are suppressed the
	 * send fails immediately and a log row with status='suppressed' is written.
	 *
	 * Routing: per-FROM mapping -> named connection -> default_connection ->
	 * primary row. On failure: exactly one retry via fallback_connection when
	 * it resolves to a different key than the one just attempted.
	 *
	 * @param array<string,mixed> $mail            Normalised mail payload.
	 * @param EmailConfig         $cfg             Current email config.
	 * @param bool                $disable_fallback When true, skip the fallback retry.
	 * @return array{ok:bool,message_id:string,detail:string}
	 */
	public function send( array $mail, EmailConfig $cfg, bool $disable_fallback = false ): array {
		// -- Suppression check ------------------------------------------------
		$filtered = $this->filter_suppressed_recipients( $mail );
		if ( $filtered['all_suppressed'] ) {
			$error = 'recipient suppressed';
			// Log with 'default' key and the configured primary provider.
			$this->maybe_log( $mail, 'default', $cfg->provider, 'suppressed', '', $error, '', 0, $cfg );
			return array( 'ok' => false, 'message_id' => '', 'detail' => $error );
		}
		// Replace the To list with the filtered (non-suppressed) set.
		$mail = $filtered['mail'];

		// Resolve the primary connection for this outgoing From address.
		$connection = $this->resolve_connection( (string) ( $mail['from'] ?? '' ), $cfg );

		// Per-connection from_address / from_name override.
		$mail = $this->apply_connection_identity( $mail, $connection );

		$secret  = $this->get_secret_for_connection( $connection );
		$handler = $this->handlers[ $connection['provider'] ] ?? null;
		if ( $handler === null ) {
			$error = 'no handler registered for provider: ' . $connection['provider'];
			$this->maybe_log( $mail, $connection['key'], $connection['provider'], 'failed', '', $error, '', 0, $cfg );
			return array( 'ok' => false, 'message_id' => '', 'detail' => $error );
		}

		$result = $handler->send( $mail, $connection['config'], $secret );

		if ( $result['ok'] ) {
			$this->maybe_log( $mail, $connection['key'], $connection['provider'], 'sent', $result['message_id'], '', (string) ( $result['provider_response'] ?? '' ), 0, $cfg );
			return array( 'ok' => true, 'message_id' => $result['message_id'], 'detail' => '' );
		}

		$primary_error    = (string) ( $result['error'] ?? '' );
		$primary_response = (string) ( $result['provider_response'] ?? '' );

		// First attempt failed, try fallback unless disabled.
		if ( ! $disable_fallback ) {
			$fallback = $this->resolve_fallback( $cfg, $connection['key'] );
			if ( $fallback !== null ) {
				// Per-connection from override for the fallback connection.
				$fb_mail    = $this->apply_connection_identity( $mail, $fallback );
				$fb_secret  = $this->get_secret_for_connection( $fallback );
				$fb_handler = $this->handlers[ $fallback['provider'] ] ?? null;
				if ( $fb_handler !== null ) {
					$fb_result   = $fb_handler->send( $fb_mail, $fallback['config'], $fb_secret );
					$fb_ok       = (bool) ( $fb_result['ok'] ?? false );
					$fb_status   = $fb_ok ? 'sent' : 'failed';
					$fb_response = (string) ( $fb_result['provider_response'] ?? '' );
					// Prefix the error with the primary failure detail so the CP log shows both legs.
					$fb_error = $fb_ok ? ''
						: 'primary(' . $connection['key'] . ') failed: ' . $primary_error . ' | ' . (string) ( $fb_result['error'] ?? '' );
					$this->maybe_log( $fb_mail, $fallback['key'], $fallback['provider'], $fb_status, (string) ( $fb_result['message_id'] ?? '' ), $fb_error, $fb_response, 1, $cfg );
					return array(
						'ok'         => $fb_ok,
						'message_id' => (string) ( $fb_result['message_id'] ?? '' ),
						'detail'     => $fb_ok ? '' : $fb_error,
					);
				}
			}
		}

		// No fallback or fallback handler missing, log the original failure.
		$this->maybe_log( $mail, $connection['key'], $connection['provider'], 'failed', '', $primary_error, $primary_response, 0, $cfg );
		return array( 'ok' => false, 'message_id' => '', 'detail' => $primary_error );
	}

	/**
	 * Send via a specific connection key, bypassing the FROM-address resolution.
	 * Used by send_test_email when the caller specifies a connection explicitly.
	 * Fallback is always disabled for test sends (disable_fallback is fixed true).
	 *
	 * @param array<string,mixed> $mail           Normalised mail payload.
	 * @param EmailConfig         $cfg            Current email config.
	 * @param string              $connection_key Named connection key ('' or 'default' = primary).
	 * @param bool                $disable_fallback Always true for test sends.
	 * @return array{ok:bool,message_id:string,detail:string}
	 */
	public function send_via( array $mail, EmailConfig $cfg, string $connection_key, bool $disable_fallback = true ): array {
		// Suppression check.
		$filtered = $this->filter_suppressed_recipients( $mail );
		if ( $filtered['all_suppressed'] ) {
			$error = 'recipient suppressed';
			$this->maybe_log( $mail, 'default', $cfg->provider, 'suppressed', '', $error, '', 0, $cfg );
			return array( 'ok' => false, 'message_id' => '', 'detail' => $error );
		}
		$mail = $filtered['mail'];

		// Resolve the requested connection.
		if ( $connection_key === '' || $connection_key === 'default' ) {
			$connection = $this->primary_connection( $cfg );
		} else {
			$connection = $this->lookup_registry( $connection_key, $cfg );
			if ( $connection === null ) {
				$error = 'named connection not found: ' . $connection_key;
				$this->maybe_log( $mail, $connection_key, $cfg->provider, 'failed', '', $error, '', 0, $cfg );
				return array( 'ok' => false, 'message_id' => '', 'detail' => $error );
			}
		}

		$mail    = $this->apply_connection_identity( $mail, $connection );
		$secret  = $this->get_secret_for_connection( $connection );
		$handler = $this->handlers[ $connection['provider'] ] ?? null;

		if ( $handler === null ) {
			$error = 'no handler registered for provider: ' . $connection['provider'];
			$this->maybe_log( $mail, $connection['key'], $connection['provider'], 'failed', '', $error, '', 0, $cfg );
			return array( 'ok' => false, 'message_id' => '', 'detail' => $error );
		}

		$result   = $handler->send( $mail, $connection['config'], $secret );
		$ok       = (bool) ( $result['ok'] ?? false );
		$status   = $ok ? 'sent' : 'failed';
		$err_str  = $ok ? '' : (string) ( $result['error'] ?? '' );
		$resp_str = (string) ( $result['provider_response'] ?? '' );
		$msg_id   = (string) ( $result['message_id'] ?? '' );
		$this->maybe_log( $mail, $connection['key'], $connection['provider'], $status, $msg_id, $err_str, $resp_str, 0, $cfg );

		return array(
			'ok'         => $ok,
			'message_id' => $msg_id,
			'detail'     => $ok ? '' : $err_str,
		);
	}

	/**
	 * Resolve the active connection for a given From address.
	 *
	 * Returns an array with keys: key, provider, config, from_address, from_name.
	 * 'key' is the resolved connection key string ('default' for the primary row).
	 *
	 * Resolution order:
	 *   1. mappings[from_address] as a connection key STRING (m62+ CP).
	 *      Back-compat: if the mapped value is an inline array (pre-m62 agent
	 *      style), it is used as a legacy inline connection (key='default').
	 *   2. Registry lookup by the resolved key (cfg->connections).
	 *   3. cfg->default_connection if set and non-empty (not 'default').
	 *   4. Primary row.
	 *
	 * @param string      $from_address Resolved From address.
	 * @param EmailConfig $cfg          Current email config.
	 * @return array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string}
	 */
	public function resolve_connection( string $from_address, EmailConfig $cfg ): array {
		// 1. Per-FROM mapping.
		if ( $from_address !== '' && isset( $cfg->mappings[ $from_address ] ) ) {
			$mapped = $cfg->mappings[ $from_address ];

			// m62+ CP sends a connection key STRING.
			if ( is_string( $mapped ) && $mapped !== '' ) {
				$conn = $this->lookup_registry( $mapped, $cfg );
				if ( $conn !== null ) {
					return $conn;
				}
				// Key not in registry, fall through to default.
			}

			// Back-compat: pre-m62 inline array shape {provider, config}.
			if ( is_array( $mapped ) && isset( $mapped['provider'] ) && is_string( $mapped['provider'] ) ) {
				return array(
					'key'          => 'default',
					'provider'     => $mapped['provider'],
					'config'       => is_array( $mapped['config'] ?? null ) ? $mapped['config'] : array(),
					'from_address' => '',
					'from_name'    => '',
				);
			}
		}

		// 2. Named default_connection (non-empty, non-'default').
		if ( $cfg->default_connection !== '' && $cfg->default_connection !== 'default' ) {
			$conn = $this->lookup_registry( $cfg->default_connection, $cfg );
			if ( $conn !== null ) {
				return $conn;
			}
		}

		// 3. Primary row.
		return $this->primary_connection( $cfg );
	}

	/**
	 * Resolve the fallback connection, if any, and only when its key differs
	 * from the key that just failed (prevents self-retry).
	 *
	 * @param EmailConfig $cfg         Current email config.
	 * @param string      $tried_key   The connection key of the failed attempt.
	 * @return array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string}|null
	 */
	private function resolve_fallback( EmailConfig $cfg, string $tried_key ): ?array {
		$fallback_key = $cfg->fallback_connection;
		if ( $fallback_key === '' ) {
			return null;
		}

		// Resolve the fallback connection.
		if ( $fallback_key === 'default' ) {
			$conn = $this->primary_connection( $cfg );
		} else {
			$conn = $this->lookup_registry( $fallback_key, $cfg );
			if ( $conn === null ) {
				return null;
			}
		}

		// Guard: do not retry the same connection we just tried.
		if ( $conn['key'] === $tried_key ) {
			return null;
		}

		return $conn;
	}

	/**
	 * Look up a named connection from the registry by key.
	 * Returns null when the key is absent or the registry entry is malformed.
	 *
	 * @param string      $key Connection slug.
	 * @param EmailConfig $cfg Current email config.
	 * @return array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string}|null
	 */
	private function lookup_registry( string $key, EmailConfig $cfg ): ?array {
		if ( ! isset( $cfg->connections[ $key ] ) || ! is_array( $cfg->connections[ $key ] ) ) {
			return null;
		}
		$wire = $cfg->connections[ $key ];

		$provider = isset( $wire['provider'] ) && is_string( $wire['provider'] ) ? $wire['provider'] : '';
		if ( $provider === '' || ! in_array( $provider, EmailConfig::PROVIDERS, true ) ) {
			return null;
		}

		return array(
			'key'          => $key,
			'provider'     => $provider,
			'config'       => ( isset( $wire['config'] ) && is_array( $wire['config'] ) ) ? $wire['config'] : array(),
			'from_address' => ( isset( $wire['from_address'] ) && is_string( $wire['from_address'] ) ) ? $wire['from_address'] : '',
			'from_name'    => ( isset( $wire['from_name'] ) && is_string( $wire['from_name'] ) ) ? $wire['from_name'] : '',
		);
	}

	/**
	 * Build the primary connection descriptor from the top-level config row.
	 *
	 * @param EmailConfig $cfg Current email config.
	 * @return array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string}
	 */
	private function primary_connection( EmailConfig $cfg ): array {
		return array(
			'key'          => 'default',
			'provider'     => $cfg->provider,
			'config'       => $cfg->config,
			'from_address' => '',
			'from_name'    => '',
		);
	}

	/**
	 * Apply a connection's from_address / from_name override to the mail payload.
	 * Only overrides when the connection carries non-empty identity fields.
	 *
	 * @param array<string,mixed>                                                              $mail       Mail payload.
	 * @param array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string} $connection Resolved connection.
	 * @return array<string,mixed> Updated mail payload.
	 */
	private function apply_connection_identity( array $mail, array $connection ): array {
		if ( $connection['from_address'] !== '' ) {
			$mail['from'] = $connection['from_address'];
		}
		if ( $connection['from_name'] !== '' ) {
			$mail['from_name'] = $connection['from_name'];
		}
		return $mail;
	}

	/**
	 * Retrieve the secret for a resolved connection descriptor.
	 * For the primary row ('default'), reads the legacy OPTION_EMAIL_SECRET.
	 * For named connections, reads from the per-connection secrets map.
	 *
	 * @param array{key:string,provider:string,config:array<string,mixed>,from_address:string,from_name:string} $connection Resolved connection.
	 * @return string Decrypted secret, or '' when absent.
	 */
	private function get_secret_for_connection( array $connection ): string {
		if ( $connection['key'] === 'default' ) {
			return $this->keystore->get_email_secret();
		}
		return $this->keystore->get_connection_secret( $connection['key'] );
	}

	/**
	 * Filter suppressed recipients from the mail payload.
	 *
	 * Returns:
	 *   - all_suppressed: true when every To address is in the suppression list.
	 *   - mail: the payload with the To list reduced to non-suppressed addresses.
	 *
	 * When no SuppressionCheckerInterface is wired (null), no recipients are filtered.
	 *
	 * @param array<string,mixed> $mail Original mail payload.
	 * @return array{all_suppressed:bool,mail:array<string,mixed>}
	 */
	private function filter_suppressed_recipients( array $mail ): array {
		if ( $this->suppression_cache === null ) {
			return array( 'all_suppressed' => false, 'mail' => $mail );
		}

		$to = (array) ( $mail['to'] ?? array() );
		if ( $to === array() ) {
			return array( 'all_suppressed' => false, 'mail' => $mail );
		}

		$allowed = array();
		foreach ( $to as $addr ) {
			// $addr may carry the "Display Name <addr>" form; the suppression
			// list is keyed by bare address only, so resolve to a bare address
			// for the check while keeping the ORIGINAL entry (name included) in
			// the mail payload passed on to the provider handler.
			$parsed_addr = AddressParser::parse_one( (string) $addr );
			$bare_addr   = $parsed_addr !== null ? $parsed_addr['address'] : (string) $addr;
			if ( ! $this->suppression_cache->is_suppressed( $bare_addr ) ) {
				$allowed[] = $addr;
			}
		}

		if ( $allowed === array() ) {
			// Every recipient suppressed.
			return array( 'all_suppressed' => true, 'mail' => $mail );
		}

		$mail['to'] = array_values( $allowed );
		return array( 'all_suppressed' => false, 'mail' => $mail );
	}

	/**
	 * Write a log row if email logging is enabled, OR unconditionally when
	 * the outcome is a failure.
	 *
	 * log_emails is a volume-and-retention preference about keeping a
	 * record of mail that WORKED; it is not a request to be blinded to
	 * failures. By the time this is called, send() has already DETECTED the
	 * failure and computed its error string -- discarding that here would
	 * leave no local row, nothing for the push to ship, and nothing for the
	 * control plane to alert on, purely because the owner asked for a
	 * quieter log of successful mail. So the early return only applies when
	 * BOTH log_emails is off AND the outcome is not a failure ('sent' is the
	 * only non-failure status this method ever receives; 'failed' and
	 * 'suppressed' are both incidents and are logged regardless).
	 *
	 * This does not touch privacy: store_body is gated independently inside
	 * EmailLogger::write() and still defaults false there, so turning
	 * log_emails off still means no message body is ever stored, and a
	 * successful send is still not recorded at all.
	 *
	 * @param array<string,mixed> $mail            Normalised mail payload.
	 * @param string              $connection_key  Resolved connection key ('default' for primary).
	 * @param string              $provider        Provider slug.
	 * @param string              $status          'sent', 'failed', or 'suppressed'.
	 * @param string              $msg_id          Provider message-id.
	 * @param string              $error           Error string (empty on success).
	 * @param string              $response        Raw provider response.
	 * @param int                 $retries         Retry count consumed.
	 * @param EmailConfig         $cfg             Runtime config.
	 * @return void
	 */
	private function maybe_log(
		array $mail,
		string $connection_key,
		string $provider,
		string $status,
		string $msg_id,
		string $error,
		string $response,
		int $retries,
		EmailConfig $cfg
	): void {
		$is_failure = ( 'sent' !== $status );
		if ( ! $cfg->log_emails && ! $is_failure ) {
			return;
		}
		$this->logger->write( $mail, $provider, $status, $msg_id, $error, $response, $retries, $cfg, $connection_key );
	}
}
