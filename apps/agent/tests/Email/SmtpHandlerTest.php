<?php
/**
 * SmtpHandlerTest: first coverage of SmtpHandler::send(), the exact call
 * site that produced the GH #312 report:
 *
 *   {"summary": "Invalid address:  (Reply-To): Andrea Somigli <salesianalibri@gmail.com>"}
 *
 * Uses the self-contained \PHPMailer\PHPMailer\PHPMailer double declared in
 * fake-phpmailer.php (see that file's docblock for why it lives separately).
 *
 * @package WPMgr\Agent\Tests\Email
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
	public function send( array $mail, array $config, string$secret ): array {
		// Ensure PHPMailer dependencies are loaded correctly.
		$this->ensure_phpmailer_loaded();

		if ( ! class_exists( 'PHPMailer\\PHPMailer\\PHPMailer' ) ) {
			return $this->failure( 'PHPMailer class not available' );
		}

		try {
			$phpmailer = new \PHPMailer\PHPMailer\PHPMailer( true );$phpmailer->isSMTP();
			
			// Prevent infinite hangs on background/cron executions.
			$phpmailer->Timeout = 15;

			$phpmailer->CharSet = isset( $mail['charset'] ) ? (string)$mail['charset'] : 'UTF-8';

			// -- SMTP connection settings ------------------------------------
			$host = isset($config['host'] ) && is_string( $config['host'] ) ? trim( $config['host'] ) : '';
			if ( $host === '' ) {
				return $this->failure( 'SMTP host not configured' );
			}
			$phpmailer->Host =$host;
			$phpmailer->Port = isset( $config['port'] ) ? (int)$config['port'] : 587;

			$encryption = isset( $config['encryption'] ) && is_string($config['encryption'] )
				? strtolower( $config['encryption'] ) : 'tls';

			switch ( $encryption ) {
				case 'ssl':
					$phpmailer->SMTPSecure = \PHPMailer\PHPMailer\PHPMailer::ENCRYPTION_SMTPS;
					$phpmailer->SMTPAutoTLS = false;
					break;
				case 'none':
					$phpmailer->SMTPSecure = '';$phpmailer->SMTPAutoTLS = false;
					break;
				default: // 'tls'
					$phpmailer->SMTPSecure = \PHPMailer\PHPMailer\PHPMailer::ENCRYPTION_STARTTLS;
					$auto_tls = ! isset( $config['auto_tls'] ) \vert{}\vert{} (bool)$config['auto_tls'];
					$phpmailer->SMTPAutoTLS =$auto_tls;
					break;
			}

			// -- SMTP Authentication -----------------------------------------
			$use_auth = ! isset( $config['auth'] ) \vert{}\vert{} (bool)$config['auth'];
			if ( $use_auth ) {$username = isset( $config['username'] ) && is_string($config['username'] )
					? trim( $config['username'] ) : '';
				$password = trim( str_replace( array( "\r", "\n" ), '', $secret ) );

				if ( $username === '' \vert{}\vert{}$password === '' ) {
					return $this->failure( 'SMTP authentication credentials missing or empty' );
				}

				$phpmailer->SMTPAuth = true;
				$phpmailer->Username =$username;
				$phpmailer->Password =$password;
			}

			// -- From -----------------------------------------------------------
			$from_entry   = AddressParser::parse_one( (string) ($mail['from'] ?? '' ) );
			$from_address =$from_entry !== null ? $from_entry['address'] : (string) ($mail['from'] ?? '' );
			$from_name    = (string) ($mail['from_name'] ?? '' );
			if ( $from_name === '' && $from_entry !== null &&$from_entry['name'] !== '' ) {
				$from_name =$from_entry['name'];
			}
			$phpmailer->setFrom( $from_address,$from_name );

			if ( ! empty( $mail['return_path'] ) ) {
				$phpmailer->Sender =$from_address;
			}

			// -- Recipients -------------------------------------------------------
			$invalid_addresses     = array();$added_recipient_count = 0;

			$added_recipient_count += $this->add_addresses($phpmailer, 'addAddress', $mail['to'] ?? array(), $invalid_addresses );
			$added_recipient_count +=$this->add_addresses( $phpmailer, 'addCC',$mail['cc'] ?? array(), $invalid_addresses );$added_recipient_count += $this->add_addresses($phpmailer, 'addBCC', $mail['bcc'] ?? array(), $invalid_addresses );
			$this->add_addresses($phpmailer, 'addReplyTo', $mail['reply_to'] ?? array(), $invalid_addresses );

			if ( $added_recipient_count === 0 ) {
				return $this->failure(
					'no valid recipient address' . ( $invalid_addresses !== array() ? ' (rejected: ' . implode( ', ', $invalid_addresses ) . ')' : '' )
				);
			}

			// -- Subject + body -----------------------------------------------
			$phpmailer->Subject = (string) ($mail['subject'] ?? '' );

			$body_html = (string) ($mail['body_html'] ?? '' );
			$body_text = (string) ($mail['body_text'] ?? '' );

			if ( $body_html !== '' ) {$phpmailer->isHTML( true );
				$phpmailer->Body    =$body_html;
				$phpmailer->AltBody =$body_text;
			} else {
				$phpmailer->isHTML( false );
				$phpmailer->Body =$body_text;
			}

			// -- Correlation header -------------------------------------------
			$site_id = (string) ($mail['x_site_id'] ?? '' );
			if ( $site_id !== '' ) {
				$phpmailer->addCustomHeader( 'X-WPMgr-Site',$site_id );
			}

			// -- Attachments --------------------------------------------------
			foreach ( (array) ( $mail['attachments'] ?? array() ) as $att ) {
				if ( ! is_array( $att ) || empty( $att['path'] ) ) { 					continue; 				}$phpmailer->addAttachment(
					(string) $att['path'],
					(string) ( $att['name'] ?? '' ),
					'base64',
					(string) ( $att['mime'] ?? 'application/octet-stream' )
				);
			}

			$phpmailer->send();

			$message_id =$phpmailer->getLastMessageID();

			$provider_response = 'SMTP send OK';
			if ( $invalid_addresses !== array() ) {$provider_response .= '; skipped invalid address(es): ' . implode( ', ', $invalid_addresses );
			}

			return array(
				'ok'                => true,
				'message_id'        => (string) $message_id,
				'error'             => '',
				'provider_response' => $provider_response,
			);
		} catch ( \Throwable $e ) {
			return $this->failure($e->getMessage() );
		}
	}

	/**
	 * Ensure PHPMailer and its required components are included.
	 */
	private function ensure_phpmailer_loaded(): void {
		if ( class_exists( 'PHPMailer\\PHPMailer\\PHPMailer' ) ) {
			return;
		}

		if ( defined( 'ABSPATH' ) ) {
			$base = ABSPATH . 'wp-includes/PHPMailer/';
			if ( is_file( $base . 'PHPMailer.php' ) ) {
				require_once $base . 'Exception.php';
				require_once $base . 'PHPMailer.php';
				require_once $base . 'SMTP.php';
			}
		}
	}

	/**
	 * Add addresses to PHPMailer safely.
	 */
	private function add_addresses( $phpmailer, string $method,$raw, array &$invalid ): int {$added   = 0;
		$entries = AddressParser::parse_list_verbose($raw );

		foreach ( $entries['valid'] as$entry ) {
			try {
				$phpmailer->{$method}( $entry['address'],$entry['name'] );
				++$added;
			} catch ( \Throwable ) {
				$invalid[] =$entry['address'];
			}
		}

		foreach ( $entries['invalid'] as$entry ) {
			$raw_entry = trim( (string)$entry );
			if ( $raw_entry === '' \vert{}\vert{} strpos($raw_entry, '@' ) === false ) {
				$invalid[] = (string)$entry;
				continue;
			}
			try {
				$phpmailer->{$method}($raw_entry );
				++$added;
			} catch ( \Throwable ) {
				$invalid[] =$raw_entry;
			}
		}

		return $added;
	}

	/**
	 * Build a structured failure result.
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
