<?php
/**
 * ResendEmailCommand: re-sends a previously logged email given its local
 * agent_seq (the auto-increment row id from the wpmgr_email_log table).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/resend_email
 *   Authorization: Bearer <Ed25519 JWT with cmd="resend_email", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: { "agent_seq": <int> }
 *
 * Response:
 *   On success: { "ok": true, "detail": "resent", "message_id": "<string>" }
 *   Body not stored: { "ok": false, "detail": "body_not_stored" }
 *   Row not found:   { "ok": false, "detail": "log_row_not_found" }
 *   Config missing:  { "ok": false, "detail": "no email config" }
 *
 * The command:
 *   1. Looks up the buffered row by agent_seq in wpmgr_email_log.
 *   2. If body_stored=0 returns ok=false with detail="body_not_stored".
 *   3. Rebuilds a minimal mail payload from the stored row + body.
 *   4. Sends via ProviderRouter (with suppress-check and fallback active).
 *   5. On success: increments resent_count and updates the log row's status,
 *      message_id, and response to reflect the new send.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Email\AddressParser;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Schema;

/**
 * Re-sends a buffered email by its local log row id (agent_seq).
 */
final class ResendEmailCommand implements CommandInterface {

	private ProviderRouter $provider_router;

	/**
	 * @param ProviderRouter $provider_router The agent's shared provider router.
	 */
	public function __construct( ProviderRouter $provider_router ) {
		$this->provider_router = $provider_router;
	}

	/** @inheritDoc */
	public function name(): string {
		return 'resend_email';
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params ResendEmailRequest fields (agent_seq: int).
	 * @return array{ok:bool,detail:string,message_id:string}
	 */
	public function execute( array $claims, array $params ): array {
		// Validate agent_seq.
		if ( ! array_key_exists( 'agent_seq', $params ) ) {
			return array( 'ok' => false, 'detail' => 'missing required field: agent_seq', 'message_id' => '' );
		}

		$agent_seq = filter_var( $params['agent_seq'], FILTER_VALIDATE_INT );
		if ( $agent_seq === false || $agent_seq < 1 ) {
			return array( 'ok' => false, 'detail' => 'agent_seq must be a positive integer', 'message_id' => '' );
		}

		// Load the email config, required before attempting a resend.
		$cfg = EmailConfig::load();
		if ( ! $cfg->is_configured() ) {
			return array( 'ok' => false, 'detail' => 'no email config, run sync_email_config first', 'message_id' => '' );
		}

		// Fetch the log row.
		$row = $this->fetch_row( $agent_seq );
		if ( $row === null ) {
			return array( 'ok' => false, 'detail' => 'log_row_not_found', 'message_id' => '' );
		}

		// Refuse resend when the body was not stored.
		if ( (int) ( $row['body_stored'] ?? 0 ) !== 1 ) {
			return array( 'ok' => false, 'detail' => 'body_not_stored', 'message_id' => '' );
		}

		// Rebuild the mail payload from the stored row.
		$mail = $this->build_mail_from_row( $row, $cfg );

		// Send via the ProviderRouter (suppression check + fallback active).
		$result = $this->provider_router->send( $mail, $cfg );

		if ( $result['ok'] ) {
			// Update the log row: increment resent_count, refresh status/message_id/response.
			$this->update_row_after_resend( $agent_seq, $result['message_id'] );
		}

		return array(
			'ok'         => $result['ok'],
			'detail'     => $result['ok'] ? 'resent' : $result['detail'],
			'message_id' => $result['message_id'],
		);
	}

	// -------------------------------------------------------------------------
	// Private helpers
	// -------------------------------------------------------------------------

	/**
	 * Fetch a single email log row by its local id.
	 *
	 * @param int $agent_seq Row id.
	 * @return array<string,mixed>|null The row as an associative array, or null if absent.
	 */
	private function fetch_row( int $agent_seq ): ?array {
		global $wpdb;
		if ( ! is_object( $wpdb ) ) {
			return null;
		}
		/** @var \wpdb $wpdb */

		$table = $wpdb->prefix . Schema::EMAIL_LOG_TABLE;

		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; resend must read the live row, not a stale cache
		$row = $wpdb->get_row(
			$wpdb->prepare(
				// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
				"SELECT id, mail_to, mail_from, subject, provider, body_stored, body, resent_count FROM {$table} WHERE id = %d LIMIT 1",
				$agent_seq
			),
			ARRAY_A
		);

		if ( ! is_array( $row ) ) {
			return null;
		}
		return $row;
	}

	/**
	 * Build a minimal normalised mail payload from a stored log row.
	 *
	 * The stored row contains a comma-delimited mail_to string, a mail_from
	 * string (optionally "Name <addr>" format), and the stored body (HTML or
	 * plain text). We reconstruct the minimum required by ProviderRouter::send().
	 *
	 * @param array<string,mixed> $row Stored log row.
	 * @param EmailConfig         $cfg Current email config (for return_path + site_id).
	 * @return array<string,mixed> Normalised mail payload.
	 */
	private function build_mail_from_row( array $row, EmailConfig $cfg ): array {
		// AddressParser::split_list() is quote-aware (unlike the bare comma/
		// semicolon regex this replaced), so a stored recipient whose display
		// name itself contained a comma round-trips as one entry, not two.
		$mail_to_raw = (string) ( $row['mail_to'] ?? '' );
		$to          = $mail_to_raw !== '' ? AddressParser::split_list( $mail_to_raw ) : array();

		// Parse "Name <addr>" format for the From field.
		$mail_from_raw = (string) ( $row['mail_from'] ?? '' );
		$from_entry    = AddressParser::parse_one( $mail_from_raw );
		$from          = $from_entry !== null ? $from_entry['address'] : trim( $mail_from_raw );
		$from_name     = $from_entry !== null ? $from_entry['name'] : '';

		$body = (string) ( $row['body'] ?? '' );
		// Detect HTML body by looking for opening tags.
		$body_html = '';
		$body_text = '';
		if ( preg_match( '/<[a-z][\s\S]*>/i', $body ) ) {
			$body_html = $body;
		} else {
			$body_text = $body;
		}

		$site_id   = function_exists( 'get_option' ) ? (string) get_option( 'wpmgr_agent_site_id', '' ) : '';
		$tenant_id = function_exists( 'get_option' ) ? (string) get_option( 'wpmgr_agent_tenant_id', '' ) : '';

		return array(
			'to'          => $to,
			'cc'          => array(),
			'bcc'         => array(),
			'reply_to'    => array(),
			'from'        => $from !== '' ? $from : $cfg->from_address,
			'from_name'   => $from_name !== '' ? $from_name : $cfg->from_name,
			'subject'     => (string) ( $row['subject'] ?? '' ),
			'body_text'   => $body_text,
			'body_html'   => $body_html,
			'charset'     => 'UTF-8',
			'headers'     => array(),
			'attachments' => array(),
			'return_path' => $cfg->return_path,
			'x_site_id'   => $site_id !== '' ? $site_id : 'unknown',
			'x_tenant_id' => $tenant_id,
		);
	}

	/**
	 * Increment resent_count and update the status to 'resent' on the log row.
	 *
	 * @param int    $agent_seq  Log row id.
	 * @param string $message_id New provider message id.
	 * @return void
	 */
	private function update_row_after_resend( int $agent_seq, string $message_id ): void {
		global $wpdb;
		if ( ! is_object( $wpdb ) ) {
			return;
		}
		/** @var \wpdb $wpdb */

		$table = $wpdb->prefix . Schema::EMAIL_LOG_TABLE;

		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; must update the live row after a successful resend
		$wpdb->query(
			$wpdb->prepare(
				// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
				"UPDATE {$table} SET resent_count = resent_count + 1, status = 'resent', message_id = %s WHERE id = %d",
				$message_id,
				$agent_seq
			)
		);
	}
}
