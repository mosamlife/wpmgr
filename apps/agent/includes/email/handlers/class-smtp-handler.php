<?php
/**
 * SmtpHandler: sends via WordPress's bundled PHPMailer over SMTP.
 *
 * PHPMailer is loaded by WP in wp-includes/PHPMailer/. We construct a fresh
 * instance (avoiding any global state from the `phpmailer_init` filter used
 * by WP's own wp_mail() path, since we are intercepting before that path).
 *
 * Config shape (non-secret, from EmailConfig::$config):
 *   host        string  SMTP hostname.
 *   port        int     SMTP port (default 587).
 *   encryption  string  'none'|'ssl'|'tls' (default 'tls').
 *   auth        bool    Whether to use SMTP AUTH (default true).
 *   username    string  SMTP username.
 *   auto_tls    bool    Whether PHPMailer auto-negotiates TLS (default true).
 *
 * Secret (from keystore): SMTP password.
 *
 * @package WPMgr\Agent\Email\Handlers
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email\Handlers;

use WPMgr\Agent\Email\AddressParser;
use WPMgr\Agent\Email\ProviderHandlerInterface;

/**
 * SMTP provider handler using WP's bundled PHPMailer.
 */
final class SmtpHandler implements ProviderHandlerInterface {

	/** @inheritDoc */
	public function provider(): string {
		return 'smtp';
	}

	/** @inheritDoc */
	public function send( array $mail, array $config, string $secret ): array {
		// Ensure PHPMailer is available (WP loads it in wp-includes/PHPMailer/).
		if ( ! class_exists( 'PHPMailer\\PHPMailer\\PHPMailer' ) ) {
			if ( defined( 'ABSPATH' ) ) {
				$src = ABSPATH . 'wp-includes/PHPMailer/PHPMailer.php';
				if ( is_file( $src ) ) {
					require_once $src;
					require_once ABSPATH . 'wp-includes/PHPMailer/Exception.php';
					require_once ABSPATH . 'wp-includes/PHPMailer/SMTP.php';
				}
			}
			if ( ! class_exists( 'PHPMailer\\PHPMailer\\PHPMailer' ) ) {
				return $this->failure( 'PHPMailer class not available' );
			}
		}

		try {
			$phpmailer = new \PHPMailer\PHPMailer\PHPMailer( true );
			$phpmailer->isSMTP();
			$phpmailer->CharSet = isset( $mail['charset'] ) ? (string) $mail['charset'] : 'UTF-8';

			// -- SMTP connection settings ------------------------------------
			$host = isset( $config['host'] ) && is_string( $config['host'] ) ? trim( $config['host'] ) : '';
			if ( $host === '' ) {
				return $this->failure( 'SMTP host not configured' );
			}
			$phpmailer->Host = $host;
			$phpmailer->Port = isset( $config['port'] ) ? (int) $config['port'] : 587;

			$encryption = isset( $config['encryption'] ) && is_string( $config['encryption'] )
				? strtolower( $config['encryption'] ) : 'tls';

			switch ( $encryption ) {
				case 'ssl':
					$phpmailer->SMTPSecure = \PHPMailer\PHPMailer\PHPMailer::ENCRYPTION_SMTPS;
					$phpmailer->SMTPAutoTLS = false;
					break;
				case 'none':
					$phpmailer->SMTPSecure = '';
					$phpmailer->SMTPAutoTLS = false;
					break;
				default: // 'tls'
					$phpmailer->SMTPSecure = \PHPMailer\PHPMailer\PHPMailer::ENCRYPTION_STARTTLS;
					$auto_tls = ! isset( $config['auto_tls'] ) || (bool) $config['auto_tls'];
					$phpmailer->SMTPAutoTLS = $auto_tls;
					break;
			}

			$use_auth = ! isset( $config['auth'] ) || (bool) $config['auth'];
			if ( $use_auth ) {
				$phpmailer->SMTPAuth = true;
				$phpmailer->Username = isset( $config['username'] ) && is_string( $config['username'] )
					? $config['username'] : '';
				$phpmailer->Password = $secret;
			}

			// -- From -----------------------------------------------------------
			// $mail['from'] may itself carry the "Display Name <addr>" form (a
			// wp_mail_from filter or a named connection's from_address can both
			// return it that way). Parse it so PHPMailer::setFrom() always
			// receives a bare address, with any embedded name used only when
			// $mail['from_name'] did not already supply one.
			$from_entry   = AddressParser::parse_one( (string) ( $mail['from'] ?? '' ) );
			$from_address = $from_entry !== null ? $from_entry['address'] : (string) ( $mail['from'] ?? '' );
			$from_name    = (string) ( $mail['from_name'] ?? '' );
			if ( $from_name === '' && $from_entry !== null && $from_entry['name'] !== '' ) {
				$from_name = $from_entry['name'];
			}
			$phpmailer->setFrom( $from_address, $from_name );

			if ( ! empty( $mail['return_path'] ) ) {
				$phpmailer->Sender = $from_address;
			}

			// -- Recipients -------------------------------------------------------
			// Every to/cc/bcc/reply_to entry is a raw wp_mail() header value: a
			// bare address, "Display Name <addr>", or one entry that itself packs
			// a comma-separated list (a single "Cc:" header carrying two
			// addresses). AddressParser splits and parses all of that into
			// {address, name} pairs. Each parsed address is added to PHPMailer
			// individually, in its OWN try/catch, so one malformed address is
			// dropped rather than throwing out of the whole send (PHPMailer is
			// constructed with exceptions enabled above). Losing one recipient
			// is recoverable; aborting the entire message is not.
			$invalid_addresses     = array();
			$added_recipient_count = 0;

			$added_recipient_count += $this->add_addresses( $phpmailer, 'addAddress', $mail['to'] ?? array(), $invalid_addresses );
			$added_recipient_count += $this->add_addresses( $phpmailer, 'addCC', $mail['cc'] ?? array(), $invalid_addresses );
			$added_recipient_count += $this->add_addresses( $phpmailer, 'addBCC', $mail['bcc'] ?? array(), $invalid_addresses );
			$this->add_addresses( $phpmailer, 'addReplyTo', $mail['reply_to'] ?? array(), $invalid_addresses );

			// PHPMailer requires at least one To/CC/BCC recipient. Fail clearly
			// (naming what was rejected) rather than letting send() throw its own
			// generic "You must provide at least one recipient" exception.
			if ( $added_recipient_count === 0 ) {
				return $this->failure(
					'no valid recipient address' . ( $invalid_addresses !== array() ? ' (rejected: ' . implode( ', ', $invalid_addresses ) . ')' : '' )
				);
			}

			// -- Subject + body -----------------------------------------------
			$phpmailer->Subject = (string) ( $mail['subject'] ?? '' );

			$body_html = (string) ( $mail['body_html'] ?? '' );
			$body_text = (string) ( $mail['body_text'] ?? '' );

			if ( $body_html !== '' ) {
				$phpmailer->isHTML( true );
				$phpmailer->Body    = $body_html;
				$phpmailer->AltBody = $body_text;
			} else {
				$phpmailer->isHTML( false );
				$phpmailer->Body = $body_text;
			}

			// -- Correlation header -------------------------------------------
			$site_id = (string) ( $mail['x_site_id'] ?? '' );
			if ( $site_id !== '' ) {
				$phpmailer->addCustomHeader( 'X-WPMgr-Site', $site_id );
			}

			// -- Attachments --------------------------------------------------
			foreach ( (array) ( $mail['attachments'] ?? array() ) as $att ) {
				if ( ! is_array( $att ) || empty( $att['path'] ) ) {
					continue;
				}
				$phpmailer->addAttachment(
					(string) $att['path'],
					(string) ( $att['name'] ?? '' ),
					'base64',
					(string) ( $att['mime'] ?? 'application/octet-stream' )
				);
			}

			$phpmailer->send();

			$message_id = $phpmailer->getLastMessageID();

			// Report (but do not fail on) any address dropped along the way, so
			// a malformed Cc/Reply-To is visible in the email log instead of
			// silently vanishing.
			$provider_response = 'SMTP send OK';
			if ( $invalid_addresses !== array() ) {
				$provider_response .= '; skipped invalid address(es): ' . implode( ', ', $invalid_addresses );
			}

			return array(
				'ok'                => true,
				'message_id'        => (string) $message_id,
				'error'             => '',
				'provider_response' => $provider_response,
			);
		} catch ( \Throwable $e ) {
			return $this->failure( $e->getMessage() );
		}
	}

	/**
	 * Add one address field (to/cc/bcc/reply_to) to PHPMailer.
	 *
	 * Each entry is added in its OWN try/catch, so one malformed address costs
	 * that recipient rather than throwing out of the whole send (PHPMailer is
	 * constructed with exceptions enabled). Losing one recipient is
	 * recoverable; aborting the entire message is what GH #312 reported.
	 *
	 * ENTRIES THE PARSER REFUSED ARE STILL OFFERED TO PHPMAILER RAW, and only
	 * recorded as invalid once PHPMailer itself refuses them. That passthrough
	 * is load bearing, not defensive: our validator is FILTER_VALIDATE_EMAIL,
	 * which rejects an internationalised domain, while PHPMailer detects an
	 * 8-bit domain BEFORE it validates and punycodes it on send. Dropping
	 * unparsed entries here would silently stop delivering to every IDN
	 * recipient that works today. The passthrough cannot reintroduce the bug
	 * this fix closes, because a well-formed "Name <addr>" always parses.
	 *
	 * @param \PHPMailer\PHPMailer\PHPMailer $phpmailer Live mailer.
	 * @param string                         $method    addAddress|addCC|addBCC|addReplyTo.
	 * @param mixed                          $raw       Raw field from the mail payload.
	 * @param list<string>                   $invalid   Collects entries nothing would accept.
	 * @return int How many addresses were accepted.
	 */
	private function add_addresses( $phpmailer, string $method, $raw, array &$invalid ): int {
		$added   = 0;
		$entries = AddressParser::parse_list_verbose( $raw );

		foreach ( $entries['valid'] as $entry ) {
			try {
				$phpmailer->{$method}( $entry['address'], $entry['name'] );
				++$added;
			} catch ( \Throwable ) {
				$invalid[] = $entry['address'];
			}
		}

		foreach ( $entries['invalid'] as $entry ) {
			$raw_entry = trim( (string) $entry );
			// Only retry something at least shaped like an address. An entry
			// with no "@" is not one our validator merely failed to recognise.
			if ( $raw_entry === '' || strpos( $raw_entry, '@' ) === false ) {
				$invalid[] = (string) $entry;
				continue;
			}
			try {
				$phpmailer->{$method}( $raw_entry );
				++$added;
			} catch ( \Throwable ) {
				$invalid[] = $raw_entry;
			}
		}

		return $added;
	}

	/**
	 * Build a structured failure result.
	 *
	 * @param string $error Error message.
	 * @return array{ok:bool,message_id:string,error:string,provider_response:string}
	 */
	private function failure( string $error ): array {
		return array(
			'ok'                => false,
			'message_id'        => '',
			'error'             => $error,
			'provider_response' => $error,
		);
	}
}
