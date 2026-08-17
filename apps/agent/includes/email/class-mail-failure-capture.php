<?php
/**
 * MailFailureCapture: hooks WordPress core's own `wp_mail_failed` action so a
 * failed send is logged even on sites WPMgr does not route.
 *
 * MailRouter::intercept() (class-mail-router.php) only sees a failure on
 * sites where EmailConfig::is_configured() is true -- when no provider is
 * configured it returns null immediately and never touches the send. On a
 * site sending through core PHPMailer, or through a third-party SMTP plugin
 * that never defers to WPMgr, that leaves WPMgr blind to every failure. Core
 * itself fires `wp_mail_failed` with a WP_Error on ANY failed send,
 * configured or not (wp-includes/pluggable.php), so this class listens there
 * unconditionally -- independent of EmailConfig -- and is the only path that
 * covers the unrouted case.
 *
 * No-double-log guard: when MailRouter's OWN provider send fails, it already
 * (a) logs the row itself via ProviderRouter::send() -> EmailLogger::write(),
 * and (b) fires `wp_mail_failed` itself (see class-mail-router.php,
 * intercept()) with error_data carrying a `wpmgr_provider_detail` key. That
 * key is unique to MailRouter's own fire -- core's native wp_mail_failed data
 * (to, subject, message, headers, attachments, optionally
 * phpmailer_exception_code) never carries it. So capture() treats that key's
 * presence as "already logged by the router path" and returns without
 * writing a second row.
 *
 * Consent governs WHAT is written, never WHETHER a row is written. Phase 1
 * exists specifically to capture failures on sites WPMgr does not route mail
 * for -- those sites are unconfigured by definition, so EmailConfig::$log_emails
 * is false on every one of them (it defaults false). Gating the write itself
 * on log_emails would mean this feature writes nothing on exactly the
 * population it exists for. Instead:
 *   - A row is ALWAYS written on a genuine (non-double-logged) failure:
 *     site, timestamp, provider='wp_mail', status='failed', the error
 *     string. None of that is personal data, and it is what the alert needs
 *     to exist at all.
 *   - The recipient address and subject line are included in that row ONLY
 *     when EmailConfig::load()->log_emails is true. When it is false, `to`
 *     and `subject` are written empty/redacted -- never the real values --
 *     so opting out still means something even though the row itself
 *     exists.
 * This is deliberately NOT the same gate ProviderRouter::maybe_log() applies
 * (that one skips the write entirely); the two differ on purpose and must
 * not be unified.
 *
 * Body is never persisted, unconditionally, regardless of log_emails or
 * store_body: the $mail array built here carries only `to` and `subject`,
 * never `message`/`body_html`/`body_text`, so EmailLogger::write() has
 * nothing to read into the `body` column.
 *
 * Defensive input handling: this class exists specifically to capture
 * failures from mailers WPMgr does not control, so `to`/`subject` in the
 * error data are validated defensively (never trusted to be well-typed) and
 * the write itself is wrapped in a try/catch -- an unenumerable malformed
 * shape from a third-party filter chain must never escape capture() and
 * fatal the request that triggered the mail send (checkout, password reset,
 * etc). Dropping the log write is strictly better than a fatal there.
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

/**
 * Captures core's wp_mail_failed action for sites WPMgr does not route.
 */
final class MailFailureCapture {

	private EmailLogger $logger;

	/**
	 * @param EmailLogger $logger Local send-event logger.
	 */
	public function __construct( EmailLogger $logger ) {
		$this->logger = $logger;
	}

	/**
	 * Register hooks. Called from Plugin::registerHooks().
	 *
	 * UNCONDITIONAL: this registration is not gated behind
	 * EmailConfig::is_configured() or any other config check. That is the
	 * entire point -- it must fire on sites with no provider configured, so
	 * WPMgr sees a failure even when it never routed the send. log_emails
	 * governs what capture() puts IN the row, never whether the row exists;
	 * see the class docblock.
	 *
	 * @return void
	 */
	public function register_hooks(): void {
		// phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- listening on core's own wp_mail_failed hook, not registering a global
		add_action( 'wp_mail_failed', array( $this, 'capture' ) );
	}

	/**
	 * `wp_mail_failed` action handler.
	 *
	 * WordPress core always passes a WP_Error, but the hook is typed mixed
	 * here (and guarded with instanceof) because a badly-behaved third-party
	 * filter chain could in theory pass something else through; core itself
	 * never does.
	 *
	 * @param mixed $error Expected WP_Error from core or MailRouter.
	 * @return void
	 */
	public function capture( $error ): void {
		if ( ! ( $error instanceof \WP_Error ) ) {
			return;
		}

		$data = $error->get_error_data();

		// MailRouter already logged this failure itself (see class docblock);
		// the wpmgr_provider_detail key only ever appears on a WP_Error that
		// MailRouter fired, never on core's own native wp_mail_failed data.
		// Logging again here would double-count the same send.
		if ( is_array( $data ) && array_key_exists( 'wpmgr_provider_detail', $data ) ) {
			return;
		}

		$cfg = EmailConfig::load();

		$error_message = $error->get_error_message();
		if ( $error_message === '' ) {
			$error_message = 'wp_mail_failed';
		}

		// -- Defensive `to` handling -------------------------------------
		// Core always sends an array of strings, but a third-party filter
		// further down the wp_mail_failed chain could hand us anything.
		// An array containing a non-string element (e.g. an object) would
		// fatal EmailLogger::write()'s implode(); filter those out. A bare
		// string is passed through as-is (EmailLogger casts it safely). Any
		// other shape (object, int, etc. as the whole `to` value) falls back
		// to an empty array rather than risk a fatal on cast.
		$to_raw = ( is_array( $data ) && isset( $data['to'] ) ) ? $data['to'] : array();
		if ( is_array( $to_raw ) ) {
			$to = array_values( array_filter( $to_raw, 'is_string' ) );
		} elseif ( is_string( $to_raw ) ) {
			$to = $to_raw;
		} else {
			$to = array();
		}

		// -- Defensive `subject` handling ---------------------------------
		// (string) on a non-scalar (e.g. an object with no __toString) would
		// also fatal; only cast when the value is safely castable.
		$subject_raw = ( is_array( $data ) && isset( $data['subject'] ) ) ? $data['subject'] : '';
		$subject     = is_scalar( $subject_raw ) ? (string) $subject_raw : '';

		// Consent governs WHAT is written, not WHETHER (see class docblock):
		// redact the recipient and subject to empty when the site has not
		// opted into email logging. The row is still written either way, so
		// the alert exists, but no real recipient address or subject line
		// leaves the site without log_emails being on.
		if ( ! $cfg->log_emails ) {
			$to      = array();
			$subject = '';
		}

		// Only `to` and `subject` -- never the message body, under any key,
		// regardless of the site's store_body setting (see class docblock).
		$mail = array(
			'to'      => $to,
			'subject' => $subject,
		);

		// Belt-and-suspenders: a malformed shape this validation didn't
		// anticipate must still never escape capture() and fatal the
		// request that triggered the mail send. Dropping the log write is
		// strictly better than a WSOD on checkout / password reset / etc.
		try {
			$this->logger->write( $mail, 'wp_mail', 'failed', '', $error_message, '', 0, $cfg, '' );
		} catch ( \Throwable $e ) {
			// Intentionally swallowed -- see comment above.
			unset( $e );
		}
	}
}
