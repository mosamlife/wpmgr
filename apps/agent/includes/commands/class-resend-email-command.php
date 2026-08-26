<?php
/**
 * ResendEmailCommand: re-sends a previously logged email given its local
 * agent_seq (the auto-increment row id from the wpmgr_email_log table).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/resend_email
 *   Authorization: Bearer <Ed25519 JWT with cmd="resend_email", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: { "agent_seq": <int>, "message_id": "<string>" (optional) }
 *
 * Response (every branch carries all four keys):
 *   On success: { "ok": true, "detail": "resent", "message_id": "<string>", "verified": <bool> }
 *   Body not stored: { "ok": false, "detail": "body_not_stored", "verified": <bool> }
 *   Row not found:   { "ok": false, "detail": "log_row_not_found", "verified": false }
 *   Config missing:  { "ok": false, "detail": "no email config", "verified": false }
 *   Identity check:  { "ok": false, "detail": "message_id_mismatch", "verified": false }
 *
 * The command:
 *   1. Looks up the buffered row by agent_seq in wpmgr_email_log.
 *   2. Verifies the row's identity against the optional message_id (see below).
 *   3. If body_stored=0 returns ok=false with detail="body_not_stored".
 *   4. Rebuilds a minimal mail payload from the stored row + body.
 *   5. Sends via ProviderRouter (with suppress-check and fallback active).
 *   6. On success: increments resent_count and sets status='resent'.
 *
 * GH #528 — agent_seq alone is not a safe identity.
 *
 * agent_seq is the AUTO_INCREMENT id of this site's own wpmgr_email_log table.
 * Restoring the site's database (something wpmgr itself performs) rolls that
 * counter back, so later traffic re-uses ids the control plane has already
 * bound to different messages. The CP row that says "agent_seq 42 = invoice for
 * Alice" then points at whatever local send landed at id 42 after the restore,
 * and a resend of "Alice's invoice" delivers Bob's password reset to bob@.
 * Email cannot be recalled, so this must fail closed.
 *
 * The CP therefore sends the message_id it mirrored for that agent_seq at
 * ingest. When it is present we compare it to the loaded row's stored
 * message_id and refuse the send outright on any difference. The field is
 * OPTIONAL and additive: an older control plane that does not send it gets
 * exactly today's behaviour, because a resend is still better than no resend
 * and the pre-#528 CP has no id to offer.
 *
 * "Supplied" means a NON-EMPTY string. Absent, null and empty all mean the same
 * thing — the control plane has no id to offer — and all three proceed without
 * verifying. This matters more than it looks: message_id is NULL on the control
 * plane for every send that failed at the time (all five provider handlers
 * return '' on failure and the ingest only stores a non-empty pointer), and a
 * failed send is the single most common thing an operator resends. Reading ''
 * as "compare against empty" would refuse exactly the rows this fix exists to
 * protect. The control plane omits the key outright for those rows; treating ''
 * the same way means neither side can break the other by changing its mind
 * about how it encodes "nothing".
 *
 * The reverse is NOT symmetric and must not be: when the control plane DOES
 * supply an id and the local row has none, that is a refusal. The CP's copy is
 * a mirror of this row's own message_id, so presence on one side with absence
 * on the other means they are not the same row — which is precisely what a
 * rolled-back counter re-issuing an id to a later failed send looks like.
 *
 * The ids are compared as RAW BYTES, both sides untouched. A provider message
 * id is opaque: this plugin stores it and reports it without normalisation
 * anywhere else, so normalising it at the comparison alone would invent an
 * equality the rest of the system does not hold. Two ids that differ only in
 * surrounding whitespace are two different ids, and folding them together
 * weakens the guard in the single direction that matters — it lets a re-used
 * agent_seq match a different stored email and send it. Nothing here trims,
 * lowercases, unwraps angle brackets or otherwise "helps"; if some provider
 * ever genuinely needs it, it gets a narrow rule naming that provider, never a
 * blanket normalisation of both sides.
 *
 * The response carries "verified": THIS AGENT'S attestation that it performed
 * the comparison, and not an inference anyone else may draw.
 *
 * It is true on exactly one code path — a supplied message_id was compared
 * against the loaded row and matched — and false on every path that skipped the
 * comparison or was refused by it. The control plane must not conclude "the
 * agent checked" from "I sent the field", because an agent from before this
 * change ignores the field, resends happily and answers ok=true: inferring
 * verification from the request would record a confirmed resend and show an
 * operator, and the audit trail, a check that never ran. That is the same
 * defect this file exists to fix, one layer up. An older agent omits the key
 * altogether and the control plane reads an absent field as false, so silence
 * and "unverified" agree and the field is safe across versions.
 *
 * The refusal `detail` is the bare literal "message_id_mismatch" and nothing
 * else. It is a contract string the control plane pins and maps to whatever
 * operator-facing wording it chooses; appending a sentence to it here instead
 * would put raw agent text in front of a user, which is how GH #520 was
 * reported. From this agent's side the condition is exactly this: the row now
 * living at this agent_seq is not the row the control plane captured that
 * message_id from, most often because the site's database was restored and
 * the counter was reused for different mail.
 *
 * Note that this is why a successful resend no longer overwrites the row's
 * message_id: EmailLogReporter pages rows with `WHERE id > cursor`
 * (class-email-log-reporter.php), so an already-pushed row is never re-pushed
 * and the CP's copy stays at the original send's id forever. Rewriting it here
 * would desynchronise the two and make the SECOND resend of any row refuse
 * itself. The new send's id still reaches the CP — it is the command's return
 * value.
 *
 * Known bounded false positive: a pre-#541 agent still rewrote the row's
 * message_id on every successful resend, overwriting the value the control
 * plane had mirrored at ingest. Any row resent by such an agent between the
 * control-plane #520 fix deploying and this agent version landing on that
 * site now carries a message_id the control plane does not have, so every
 * later resend of that row is refused with "message_id_mismatch" forever.
 * That is the safe direction to fail in: it refuses to send rather than
 * risking the wrong mail.
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

	/**
	 * Exact `detail` returned when the control plane named a message_id that does
	 * not match the loaded log row.
	 *
	 * This is the whole detail string, not a prefix. The control plane pins the
	 * same literal (agentcmd.ResendDetailMessageIDMismatch) and owns the wording
	 * the operator actually reads; the two sides must agree byte for byte.
	 */
	public const DETAIL_MISMATCH = 'message_id_mismatch';

	/**
	 * Exact `detail` returned when message_id was present and non-null but was
	 * not a string — a request that cannot be verified and so is not sent.
	 *
	 * Unreachable from the control plane as built (it sends a string or omits
	 * the key); it exists so a malformed caller fails closed and nameably rather
	 * than falling through to an unverified send.
	 */
	public const DETAIL_INVALID = 'message_id_invalid';

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
	 * @param array<string,mixed> $params ResendEmailRequest fields
	 *                                    (agent_seq: int, message_id: ?string optional).
	 * @return array{ok:bool,detail:string,message_id:string,verified:bool}
	 */
	public function execute( array $claims, array $params ): array {
		// Validate agent_seq.
		if ( ! array_key_exists( 'agent_seq', $params ) ) {
			return $this->refuse( 'missing required field: agent_seq' );
		}

		$agent_seq = filter_var( $params['agent_seq'], FILTER_VALIDATE_INT );
		if ( $agent_seq === false || $agent_seq < 1 ) {
			return $this->refuse( 'agent_seq must be a positive integer' );
		}

		// Resolve the optional message_id (GH #528) before any work is done.
		// Absent, null and empty all mean "the control plane has no id to offer"
		// and leave $expected_message_id null, which skips verification — that is
		// the path an older control plane takes, and the path every failed send
		// takes (its CP-side message_id is NULL). A present, non-null,
		// non-string value is a request that cannot be verified at all, and an
		// unverifiable resend is refused rather than sent.
		$expected_message_id = null;
		if ( array_key_exists( 'message_id', $params ) && $params['message_id'] !== null ) {
			if ( ! is_string( $params['message_id'] ) ) {
				return $this->refuse( self::DETAIL_INVALID );
			}
			// The empty test is on the RAW string, deliberately. Trimming first
			// would reclassify a whitespace-only id as "nothing supplied" and
			// send the mail unverified; a value that is not the empty string is
			// an identifier, and identifiers get compared.
			if ( $params['message_id'] !== '' ) {
				$expected_message_id = $params['message_id'];
			}
		}

		// Load the email config, required before attempting a resend.
		$cfg = EmailConfig::load();
		if ( ! $cfg->is_configured() ) {
			return $this->refuse( 'no email config, run sync_email_config first' );
		}

		// Fetch the log row.
		$row = $this->fetch_row( $agent_seq );
		if ( $row === null ) {
			return $this->refuse( 'log_row_not_found' );
		}

		// GH #528: verify the loaded row IS the message the control plane named,
		// before anything is built and long before anything is sent. This runs
		// ahead of the body_stored check on purpose: "body_not_stored" about the
		// wrong row is a misleading answer to give an operator.
		//
		// $verified is this agent's own attestation and nothing else. It starts
		// false and is raised on exactly one line, below, after a supplied id has
		// been compared against this row and found identical. It must never be
		// derived from the presence of the request field: the control plane
		// already knows what it sent, and what it cannot know is whether THIS
		// agent looked. An agent from before this change omits the key entirely
		// and the control plane reads that absence as false, so an old agent's
		// silence and "unverified" say the same thing.
		$verified = false;
		if ( $expected_message_id !== null ) {
			// Compared as raw bytes, both sides untouched. A provider message id
			// is opaque and is stored and reported without normalisation
			// everywhere else in this plugin, so normalising it here would invent
			// an equality the rest of the system does not hold — and it would do
			// so in the one direction that matters, letting a re-used agent_seq
			// match a different stored email and send it.
			$stored_message_id = (string) ( $row['message_id'] ?? '' );
			if ( $stored_message_id !== $expected_message_id ) {
				return $this->refuse( self::DETAIL_MISMATCH );
			}
			$verified = true;
		}

		// Refuse resend when the body was not stored. The attestation is carried
		// through: if the comparison ran and matched, it matched, and `ok` is
		// what says the resend did not happen.
		if ( (int) ( $row['body_stored'] ?? 0 ) !== 1 ) {
			return $this->refuse( 'body_not_stored', $verified );
		}

		// Rebuild the mail payload from the stored row.
		$mail = $this->build_mail_from_row( $row, $cfg );

		// Send via the ProviderRouter (suppression check + fallback active).
		$result = $this->provider_router->send( $mail, $cfg );

		if ( $result['ok'] ) {
			// Update the log row: increment resent_count and mark it resent. The
			// row's message_id is deliberately left at the original send's value —
			// see the GH #528 note in the file header.
			$this->update_row_after_resend( $agent_seq );
		}

		return array(
			'ok'         => $result['ok'],
			'detail'     => $result['ok'] ? 'resent' : $result['detail'],
			'message_id' => $result['message_id'],
			'verified'   => $verified,
		);
	}

	// -------------------------------------------------------------------------
	// Private helpers
	// -------------------------------------------------------------------------

	/**
	 * Build a refusal, with the GH #528 attestation defaulted to false.
	 *
	 * Every refusal in execute() returns through here so that `verified` cannot
	 * be dropped from one of them by accident and cannot become true by default
	 * on any of them. The parameter exists for the single refusal that happens
	 * AFTER a successful comparison (body_not_stored), and the only other place
	 * `verified` is ever true is the compared-and-matched path in execute().
	 *
	 * @param string $detail   Contract `detail` string for the refusal.
	 * @param bool   $verified Whether a supplied message_id was compared against
	 *                         the loaded row and matched before this refusal.
	 * @return array{ok:bool,detail:string,message_id:string,verified:bool}
	 */
	private function refuse( string $detail, bool $verified = false ): array {
		return array(
			'ok'         => false,
			'detail'     => $detail,
			'message_id' => '',
			'verified'   => $verified,
		);
	}

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
				"SELECT id, message_id, mail_to, mail_from, subject, provider, body_stored, body, resent_count FROM {$table} WHERE id = %d LIMIT 1",
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
	 * The row's message_id is intentionally NOT rewritten here. It is this row's
	 * stable identity, mirrored to the control plane at ingest and never
	 * re-pushed afterwards (EmailLogReporter pages `WHERE id > cursor`), and it
	 * is what the GH #528 verification in execute() compares against. Rewriting
	 * it would make the second resend of any row refuse itself. The new send's
	 * message id is returned to the caller instead.
	 *
	 * @param int $agent_seq Log row id.
	 * @return void
	 */
	private function update_row_after_resend( int $agent_seq ): void {
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
				"UPDATE {$table} SET resent_count = resent_count + 1, status = 'resent' WHERE id = %d",
				$agent_seq
			)
		);
	}
}
