<?php
/**
 * EmailLogReporter: fire-and-forget CP push for the local email-send log.
 *
 * Every 5 minutes (wp-cron, HOOK_PUSH) AND opportunistically right after a
 * send, pages UNPUSHED rows above a stored high-water cursor and POSTs them to:
 *
 *   POST {control_plane_url}/agent/v1/email/log
 *
 * The request is signed with the agent's Ed25519 key via Signer::signHeaders,
 * exactly mirroring PerfReporter and BackupTransport.
 *
 * Response contract (CP returns):
 *   { "acked_through": <max agent_seq> }
 *
 * On a 2xx the cursor (wp-option wpmgr_email_log_cursor) is advanced to
 * acked_through. On any failure the tick is silently skipped and the next
 * scheduled run retries from the same cursor.
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

use WPMgr\Agent\Schema;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;

/**
 * Pushes buffered email-log rows to the control plane.
 */
final class EmailLogReporter {

	/** Cron hook for the 5-minute push. */
	public const HOOK_PUSH = 'wpmgr_email_log_push';

	/** wp-option key storing the last acknowledged local row id (cursor). */
	public const OPTION_CURSOR = 'wpmgr_email_log_cursor';

	/** CP endpoint path. */
	private const PATH = '/agent/v1/email/log';

	/** Maximum rows to include in a single POST. */
	private const BATCH_SIZE = 200;

	/**
	 * Maximum push iterations per call to push(). Each iteration sends one
	 * BATCH_SIZE POST; this cap prevents an unbounded catch-up loop on a site
	 * that was offline for a long time.
	 */
	private const MAX_ITERATIONS = 10;

	private Settings $settings;

	private Signer $signer;

	/**
	 * @param Settings $settings Enrollment / CP-URL state.
	 * @param Signer   $signer   Agent Ed25519 signer.
	 */
	public function __construct( Settings $settings, Signer $signer ) {
		$this->settings = $settings;
		$this->signer   = $signer;
	}

	// -------------------------------------------------------------------------
	// Public API
	// -------------------------------------------------------------------------

	/**
	 * Page through unpushed rows and POST them to the CP until caught up or
	 * the per-call iteration cap is reached. Fire-and-forget: never throws,
	 * never returns a meaningful value.
	 *
	 * @return void
	 */
	public function push(): void {
		if ( ! $this->settings->isEnrolled() ) {
			return;
		}
		if ( ! function_exists( 'wp_remote_post' ) || ! function_exists( 'wp_json_encode' ) ) {
			return;
		}

		try {
			$this->doPush();
		} catch ( \Throwable $e ) {
			// Fire-and-forget: swallow all errors.
		}
	}

	/**
	 * Schedule the 5-minute push cron event if not already scheduled.
	 * Called from Plugin::activate() and Plugin::maybeRescheduleCron().
	 *
	 * @param int $now Current Unix timestamp.
	 * @return void
	 */
	public static function schedule_push( int $now ): void {
		if ( ! function_exists( 'wp_next_scheduled' ) || ! function_exists( 'wp_schedule_event' ) ) {
			return;
		}
		if ( wp_next_scheduled( self::HOOK_PUSH ) !== false ) {
			return;
		}
		wp_schedule_event( $now + 300, 'wpmgr_agent_5min', self::HOOK_PUSH );
	}

	// -------------------------------------------------------------------------
	// Private helpers
	// -------------------------------------------------------------------------

	/**
	 * Inner loop: fetch → POST → advance cursor, bounded by MAX_ITERATIONS.
	 *
	 * @return void
	 */
	private function doPush(): void {
		global $wpdb;
		if ( ! is_object( $wpdb ) ) {
			return;
		}

		$base = $this->settings->controlPlaneUrl();
		if ( $base === '' ) {
			return;
		}

		$cursor = (int) ( function_exists( 'get_option' )
			? get_option( self::OPTION_CURSOR, 0 )
			: 0 );

		$table = $wpdb->prefix . Schema::EMAIL_LOG_TABLE;

		for ( $i = 0; $i < self::MAX_ITERATIONS; $i++ ) {
			$rows = $this->fetchBatch( $wpdb, $table, $cursor );
			if ( $rows === [] ) {
				break;
			}

			$entries = $this->buildEntries( $rows );
			$payload = [ 'entries' => $entries ];

			$body = (string) wp_json_encode( $payload );

			try {
				$auth = $this->signer->signHeaders( 'POST', self::PATH, $body );
			} catch ( \Throwable $e ) {
				break;
			}

			$headers = array_merge(
				[ 'Content-Type' => 'application/json', 'Accept' => 'application/json' ],
				$auth
			);

			$response = wp_remote_post(
				$base . self::PATH,
				[
					'timeout'   => 10,
					'headers'   => $headers,
					'body'      => $body,
					'sslverify' => true,
				]
			);

			if ( function_exists( 'is_wp_error' ) && is_wp_error( $response ) ) {
				break;
			}

			$status = function_exists( 'wp_remote_retrieve_response_code' )
				? (int) wp_remote_retrieve_response_code( $response )
				: 0;

			if ( $status < 200 || $status >= 300 ) {
				break;
			}

			// Parse acked_through from the response body.
			$raw_body = function_exists( 'wp_remote_retrieve_body' )
				? (string) wp_remote_retrieve_body( $response )
				: '';

			$decoded = json_decode( $raw_body, true );
			$acked   = isset( $decoded['acked_through'] ) ? (int) $decoded['acked_through'] : 0;

			if ( $acked > $cursor ) {
				$cursor = $acked;
				if ( function_exists( 'update_option' ) ) {
					update_option( self::OPTION_CURSOR, $cursor, false );
				}
			}

			// If the batch was smaller than BATCH_SIZE, we are caught up.
			if ( count( $rows ) < self::BATCH_SIZE ) {
				break;
			}
		}
	}

	/**
	 * Fetch up to BATCH_SIZE rows from the email log above the cursor.
	 *
	 * @param object $wpdb   WordPress DB object (typed as object for testability;
	 *                       at runtime this is always a \wpdb instance).
	 * @param string $table  Fully-qualified table name.
	 * @param int    $cursor Last acknowledged row id.
	 * @return array<int,array<string,mixed>>
	 */
	private function fetchBatch( object $wpdb, string $table, int $cursor ): array {
		// Identifier defense in depth: $table is $wpdb->prefix plus a hard-coded
		// constant, but escape it anyway so static analysis can verify the query.
		$table = esc_sql( $table );
		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; keyset cursor reads must not be cached (stale data would permanently skip rows)
		$rows = $wpdb->get_results(
			$wpdb->prepare(
				// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
				"SELECT id, message_id, mail_to, mail_from, subject, provider, status, response, error, retries, resent_count, body_stored, body, connection_key, attachments, created_at FROM {$table} WHERE id > %d ORDER BY id ASC LIMIT %d",
				$cursor,
				self::BATCH_SIZE
			),
			ARRAY_A
		);
		if ( ! is_array( $rows ) ) {
			return [];
		}
		/** @var array<int,array<string,mixed>> $rows */
		return $rows;
	}

	/**
	 * Transform raw DB rows into the CP wire format.
	 *
	 * Mapping:
	 *   id          -> agent_seq  (int)
	 *   mail_to     -> to_addresses  (string split on comma/semicolon -> array)
	 *   mail_from   -> from_address  (string)
	 *   body        -> included ONLY when body_stored = 1
	 *   created_at  -> RFC3339 UTC
	 *
	 * @param array<int,array<string,mixed>> $rows Raw DB rows.
	 * @return array<int,array<string,mixed>>
	 */
	private function buildEntries( array $rows ): array {
		$entries = [];
		foreach ( $rows as $row ) {
			$body_stored = (int) ( $row['body_stored'] ?? 0 );

			// Split comma/semicolon-delimited recipient string back into an array.
			$mail_to_raw  = (string) ( $row['mail_to'] ?? '' );
			$to_addresses = $this->splitAddresses( $mail_to_raw );

			// Decode the response column: stored as either a JSON object string or a
			// plain provider-response string. The CP contract requires an object (never
			// a bare scalar), so wrap a non-array decode into { "summary": "<value>" }.
			// null is sent when the raw value is empty (CP maps null/absent to {}).
			$response_raw    = isset( $row['response'] ) ? (string) $row['response'] : '';
			$response_decoded = null;
			if ( $response_raw !== '' ) {
				$maybe = json_decode( $response_raw, true );
				$response_decoded = is_array( $maybe ) ? $maybe : array( 'summary' => $response_raw );
			}

			$entry = [
				'agent_seq'      => (int) ( $row['id'] ?? 0 ),
				'message_id'     => (string) ( $row['message_id'] ?? '' ),
				'to_addresses'   => $to_addresses,
				'from_address'   => (string) ( $row['mail_from'] ?? '' ),
				'subject'        => (string) ( $row['subject'] ?? '' ),
				'provider'       => (string) ( $row['provider'] ?? '' ),
				'status'         => (string) ( $row['status'] ?? '' ),
				'response'       => $response_decoded,
				'error'          => (string) ( $row['error'] ?? '' ),
				'retries'        => (int) ( $row['retries'] ?? 0 ),
				'resent_count'   => (int) ( $row['resent_count'] ?? 0 ),
				'body_stored'    => (bool) $body_stored,
				'connection_key' => (string) ( $row['connection_key'] ?? '' ),
				'created_at'     => $this->toRfc3339( (string) ( $row['created_at'] ?? '' ) ),
			];

			// Include body ONLY when body_stored=1 (privacy: default is OFF).
			if ( $body_stored === 1 ) {
				$entry['body'] = isset( $row['body'] ) ? (string) $row['body'] : null;
			}

			// Include attachments only when the column is non-null and non-empty.
			$attachments_raw = isset( $row['attachments'] ) ? (string) $row['attachments'] : '';
			if ( $attachments_raw !== '' ) {
				$decoded_attach = json_decode( $attachments_raw, true );
				if ( is_array( $decoded_attach ) && $decoded_attach !== [] ) {
					$entry['attachments'] = $decoded_attach;
				}
			}

			$entries[] = $entry;
		}
		return $entries;
	}

	/**
	 * Split a stored mail_to string (comma- or semicolon-delimited) into a
	 * trimmed array of non-empty address entries.
	 *
	 * Delegates to AddressParser::split_list() (quote/angle-bracket aware)
	 * rather than a bare regex split, so a quoted display name that itself
	 * contains a comma (e.g. '"Rossi, Andrea" <a@x.com>') is reported back to
	 * the CP as ONE recipient, not shredded into two bogus ones.
	 *
	 * @param string $raw Raw mail_to value from the DB.
	 * @return string[]
	 */
	private function splitAddresses( string $raw ): array {
		if ( $raw === '' ) {
			return [];
		}
		return AddressParser::split_list( $raw );
	}

	/**
	 * Convert a MySQL UTC DATETIME string (Y-m-d H:i:s) to an RFC3339 UTC
	 * timestamp (e.g. 2026-06-10T12:34:56Z). Falls back to the current UTC time
	 * in RFC3339 on empty input or parse failure so the CP always receives a
	 * valid RFC3339 string, never a bare MySQL datetime or an empty string.
	 *
	 * @param string $mysql MySQL DATETIME string.
	 * @return string RFC3339 UTC timestamp.
	 */
	private function toRfc3339( string $mysql ): string {
		if ( $mysql === '' ) {
			return gmdate( 'Y-m-d\TH:i:s\Z' );
		}
		try {
			$dt = new \DateTimeImmutable( $mysql, new \DateTimeZone( 'UTC' ) );
			return $dt->format( \DateTime::RFC3339 );
		} catch ( \Throwable $e ) {
			return gmdate( 'Y-m-d\TH:i:s\Z' );
		}
	}
}
