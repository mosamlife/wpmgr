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
 * Body is never persisted: the $mail array built here carries only `to` and
 * `subject`, never `message`/`body_html`/`body_text`, so EmailLogger::write()
 * has nothing to read into the `body` column regardless of the site's
 * store_body setting.
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
	 * WPMgr sees a failure even when it never routed the send.
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

		$error_message = $error->get_error_message();
		if ( $error_message === '' ) {
			$error_message = 'wp_mail_failed';
		}

		// Only `to` and `subject` -- never the message body, under any key,
		// regardless of the site's store_body setting (see class docblock).
		$mail = array(
			'to'      => ( is_array( $data ) && isset( $data['to'] ) ) ? $data['to'] : array(),
			'subject' => ( is_array( $data ) && isset( $data['subject'] ) ) ? (string) $data['subject'] : '',
		);

		$this->logger->write( $mail, 'wp_mail', 'failed', '', $error_message, '', 0, EmailConfig::load(), '' );
	}
}
