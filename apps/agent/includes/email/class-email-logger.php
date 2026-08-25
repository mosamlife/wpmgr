<?php
/**
 * EmailLogger — writes send events to the local wpmgr_email_log table and
 * schedules the retention-pruner cron (via class-schema.php's EMAIL_LOG_TABLE).
 *
 * The logger is write-only in v1 (Phase 3 adds the CP-ingest cursor push). The
 * table is designed for a keyset-cursor push: rows have an auto-increment `id`
 * and a `created_at` DATETIME so the CP can page through
 * `WHERE (created_at, id) > (cursor_created_at, cursor_id)`.
 *
 * Body storage is OFF by default (store_body=false in EmailConfig).
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

use WPMgr\Agent\Schema;

/**
 * Local email-send event log.
 */
class EmailLogger {

	/** Cron hook for the retention pruner. */
	public const HOOK_PRUNE = 'wpmgr_email_log_prune';

	/** Maximum rows before the pruner emergency-evicts beyond retention_days. */
	private const ROW_CAP = 50000;

	/**
	 * Write a send event to the local log table.
	 *
	 * @param array<string,mixed> $mail           Normalised mail payload (from ProviderRouter).
	 * @param string              $provider       Provider slug.
	 * @param string              $status         'sent' or 'failed'.
	 * @param string              $message_id     Provider message-id (empty on failure).
	 * @param string              $error          Human-readable error (empty on success).
	 * @param string              $response       Raw provider response body / summary.
	 * @param int                 $retries        Number of retry attempts consumed.
	 * @param EmailConfig         $cfg            Runtime config (store_body flag etc.).
	 * @param string              $connection_key Named connection key used ('default' = primary).
	 * @return int|false Inserted row ID or false on failure.
	 */
	public function write(
		array $mail,
		string $provider,
		string $status,
		string $message_id,
		string $error,
		string $response,
		int $retries,
		EmailConfig $cfg,
		string $connection_key = ''
	) {
		global $wpdb;
		if ( ! is_object( $wpdb ) ) {
			return false;
		}
		/** @var \wpdb $wpdb */

		$table = $wpdb->prefix . Schema::EMAIL_LOG_TABLE;

		$to_raw = $mail['to'] ?? array();
		$to_str = is_array( $to_raw ) ? implode( ', ', $to_raw ) : (string) $to_raw;

		$from_str = isset( $mail['from'] ) ? (string) $mail['from'] : '';
		if ( isset( $mail['from_name'] ) && $mail['from_name'] !== '' ) {
			$from_str = $mail['from_name'] . ' <' . $from_str . '>';
		}

		$body_stored = false;
		$body        = null;
		if ( $cfg->store_body ) {
			$body_stored = true;
			$body_html   = isset( $mail['body_html'] ) ? (string) $mail['body_html'] : '';
			$body_text   = isset( $mail['body_text'] ) ? (string) $mail['body_text'] : '';
			$body        = $body_html !== '' ? $body_html : $body_text;
		}

		// Build attachment metadata: [{name, size_bytes}], capped at 50 entries.
		// Paths are never stored; only the basename (already set by MailRouter) and size.
		$attachments_json = null;
		$raw_attachments  = isset( $mail['attachments'] ) && is_array( $mail['attachments'] )
			? $mail['attachments'] : array();
		if ( $raw_attachments !== array() ) {
			$attach_meta = array();
			foreach ( $raw_attachments as $att ) {
				if ( count( $attach_meta ) >= 50 ) {
					break;
				}
				if ( ! is_array( $att ) ) {
					continue;
				}
				$att_name = isset( $att['name'] ) ? substr( (string) $att['name'], 0, 255 ) : '';
				if ( $att_name === '' ) {
					continue;
				}
				$att_size = isset( $att['size_bytes'] ) && is_int( $att['size_bytes'] ) && $att['size_bytes'] >= 0
					? $att['size_bytes'] : 0;
				$attach_meta[] = array(
					'name'       => $att_name,
					'size_bytes' => $att_size,
				);
			}
			if ( $attach_meta !== array() ) {
				$attachments_json = wp_json_encode( $attach_meta );
			}
		}

		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; no WP API equivalent; anti-replay/security log must not be cached
		$result = $wpdb->insert(
			$table,
			array(
				'message_id'      => $message_id,
				'mail_to'         => substr( $to_str, 0, 500 ),
				'mail_from'       => substr( $from_str, 0, 255 ),
				'subject'         => substr( (string) ( $mail['subject'] ?? '' ), 0, 500 ),
				'provider'        => $provider,
				'status'          => $status,
				'response'        => substr( $response, 0, 1000 ),
				'error'           => substr( $error, 0, 1000 ),
				'retries'         => $retries,
				'resent_count'    => 0,
				'body_stored'     => $body_stored ? 1 : 0,
				'body'            => $body,
				'connection_key'  => substr( $connection_key, 0, 32 ),
				'attachments'     => $attachments_json,
				'created_at'      => current_time( 'mysql', true ),
			),
			array( '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%d', '%d', '%d', '%s', '%s', '%s', '%s' )
		);

		if ( $result === false ) {
			return false;
		}

		return $wpdb->insert_id;
	}

	/**
	 * Pruner cron handler: delete rows older than retention_days and enforce
	 * the ROW_CAP by evicting the oldest rows when exceeded.
	 *
	 * @param int $retention_days Rows older than this many days are deleted.
	 * @return int Total rows deleted.
	 */
	public function prune( int $retention_days = 14 ): int {
		global $wpdb;
		if ( ! is_object( $wpdb ) ) {
			return 0;
		}
		/** @var \wpdb $wpdb */

		$days  = max( 1, $retention_days );
		$table = $wpdb->prefix . Schema::EMAIL_LOG_TABLE;
		$deleted = 0;

		// Delete rows older than the retention window.
		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; caching would defeat the pruner's purpose
		$result = $wpdb->query(
			$wpdb->prepare(
				// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
				"DELETE FROM {$table} WHERE created_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL %d DAY)",
				$days
			)
		);
		if ( is_int( $result ) ) {
			$deleted += $result;
		}

		// Emergency eviction: keep at most ROW_CAP rows.
		// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; row cap enforcement must see live count
		$count = (int) $wpdb->get_var(
			// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
			"SELECT COUNT(*) FROM {$table}"
		);

		if ( $count > self::ROW_CAP ) {
			$excess = $count - self::ROW_CAP;
			// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- plugin-owned table; eviction must see live data
			$evicted = $wpdb->query(
				$wpdb->prepare(
					// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->prefix + a hard-coded constant, not user input
					"DELETE FROM {$table} ORDER BY created_at ASC, id ASC LIMIT %d",
					$excess
				)
			);
			if ( is_int( $evicted ) ) {
				$deleted += $evicted;
			}
		}

		return $deleted;
	}

	/**
	 * Schedule the hourly pruner cron event if not already scheduled.
	 * Called from Plugin::activate() and Plugin::maybeRescheduleCron().
	 *
	 * @param int $now Current Unix timestamp.
	 * @return void
	 */
	public static function schedule_prune( int $now ): void {
		if ( ! function_exists( 'wp_next_scheduled' ) || ! function_exists( 'wp_schedule_event' ) ) {
			return;
		}
		if ( wp_next_scheduled( self::HOOK_PRUNE ) !== false ) {
			return;
		}
		wp_schedule_event( $now + 3600, 'daily', self::HOOK_PRUNE );
	}
}
