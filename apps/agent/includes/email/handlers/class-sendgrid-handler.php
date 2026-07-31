<?php
/**
 * SendgridHandler: sends via the SendGrid Web API v3.
 *
 * Endpoint: POST https://api.sendgrid.com/v3/mail/send
 * Auth:     Authorization: Bearer <api_key>
 * Success:  HTTP 202 Accepted (no body; Message-ID from X-Message-Id header).
 *
 * Config shape: (none; the API key is the sole configuration).
 * Secret (from keystore): SendGrid API key.
 *
 * @package WPMgr\Agent\Email\Handlers
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email\Handlers;

use WPMgr\Agent\Email\AddressParser;
use WPMgr\Agent\Email\ProviderHandlerInterface;

/**
 * SendGrid Web API v3 provider handler.
 */
final class SendgridHandler implements ProviderHandlerInterface {

	private const ENDPOINT = 'https://api.sendgrid.com/v3/mail/send';

	/** @inheritDoc */
	public function provider(): string {
		return 'sendgrid';
	}

	/** @inheritDoc */
	public function send( array $mail, array $config, string $secret ): array {
		if ( $secret === '' ) {
			return $this->failure( 'SendGrid API key not configured' );
		}

		$body = $this->build_payload( $mail );

		$json = wp_json_encode( $body );
		if ( $json === false ) {
			return $this->failure( 'Failed to encode SendGrid request body' );
		}

		$response = wp_remote_post(
			self::ENDPOINT,
			array(
				'timeout' => 30,
				'headers' => array(
					'Authorization' => 'Bearer ' . $secret,
					'Content-Type'  => 'application/json',
				),
				'body'    => $json,
			)
		);

		if ( is_wp_error( $response ) ) {
			return $this->failure( $response->get_error_message() );
		}

		$code = (int) wp_remote_retrieve_response_code( $response );

		// SendGrid returns 202 on success with no body.
		if ( $code !== 202 ) {
			$body_str = (string) wp_remote_retrieve_body( $response );
			$msg      = $this->parse_error( $body_str );
			return $this->failure( 'SendGrid error ' . $code . ': ' . $msg );
		}

		// Message-ID is returned in the X-Message-Id header.
		$message_id = (string) wp_remote_retrieve_header( $response, 'x-message-id' );

		return array(
			'ok'                => true,
			'message_id'        => $message_id,
			'error'             => '',
			'provider_response' => '202 Accepted',
		);
	}

	/**
	 * Build the SendGrid Web API v3 JSON payload.
	 *
	 * @param array<string,mixed> $mail Normalised mail payload.
	 * @return array<string,mixed>
	 */
	private function build_payload( array $mail ): array {
		// From object. $mail['from'] may itself carry the "Display Name <addr>"
		// form (a wp_mail_from filter or a named connection's from_address can
		// both return it that way); SendGrid's `email` field must be bare, with
		// any display name in the sibling `name` field, so parse it first.
		$from_entry   = AddressParser::parse_one( (string) ( $mail['from'] ?? '' ) );
		$from_address = $from_entry !== null ? $from_entry['address'] : (string) ( $mail['from'] ?? '' );
		$from_name    = (string) ( $mail['from_name'] ?? '' );
		if ( $from_name === '' && $from_entry !== null && $from_entry['name'] !== '' ) {
			$from_name = $from_entry['name'];
		}
		$from_obj = array( 'email' => $from_address );
		if ( $from_name !== '' ) {
			$from_obj['name'] = $from_name;
		}

		// Personalisation: to / cc / bcc / reply_to. Every entry is a raw
		// wp_mail() header value: a bare address, "Display Name <addr>", or a
		// single entry that itself packs a comma-separated list (one "Cc:"
		// header carrying two addresses), so it is parsed via AddressParser
		// into SendGrid's {email[, name]} object shape before use. A malformed
		// entry that AddressParser cannot resolve is still passed through in
		// its raw form, exactly as it was before this parser existed, so
		// SendGrid stays the judge of anything our own validator does not
		// recognise. FILTER_VALIDATE_EMAIL refuses an internationalised
		// domain, so dropping unparsed entries here would silently stop
		// delivering to every IDN recipient that works today.
		$personalisation = array(
			'to' => $this->address_objects( $mail['to'] ?? array() ),
		);

		$cc = $this->address_objects( $mail['cc'] ?? array() );
		if ( $cc !== array() ) {
			$personalisation['cc'] = $cc;
		}

		$bcc = $this->address_objects( $mail['bcc'] ?? array() );
		if ( $bcc !== array() ) {
			$personalisation['bcc'] = $bcc;
		}

		$payload = array(
			'personalizations' => array( $personalisation ),
			'from'             => $from_obj,
			'subject'          => (string) ( $mail['subject'] ?? '' ),
		);

		// SendGrid's singular `reply_to` field accepts exactly one address;
		// `reply_to_list` is the documented way to carry more than one, so use
		// it whenever a header supplied multiple Reply-To addresses instead of
		// silently keeping only the first (the prior behaviour).
		$reply_to = $this->address_objects( $mail['reply_to'] ?? array() );
		if ( count( $reply_to ) === 1 ) {
			$payload['reply_to'] = $reply_to[0];
		} elseif ( count( $reply_to ) > 1 ) {
			$payload['reply_to_list'] = $reply_to;
		}

		// Content: prefer HTML + plain text; plain text only otherwise.
		$body_html = (string) ( $mail['body_html'] ?? '' );
		$body_text = (string) ( $mail['body_text'] ?? '' );
		$content   = array();

		if ( $body_text !== '' ) {
			$content[] = array( 'type' => 'text/plain', 'value' => $body_text );
		}
		if ( $body_html !== '' ) {
			$content[] = array( 'type' => 'text/html', 'value' => $body_html );
		}
		if ( $content === array() ) {
			$content[] = array( 'type' => 'text/plain', 'value' => '' );
		}
		$payload['content'] = $content;

		// Custom headers (X-WPMgr-Site for SMTP-layer visibility).
		$site_id = (string) ( $mail['x_site_id'] ?? '' );
		if ( $site_id !== '' ) {
			$payload['headers'] = array( 'X-WPMgr-Site' => $site_id );
		}

		// custom_args: stable metadata for CP webhook fan-out (Phase 4a).
		// SendGrid includes these in every event webhook payload, letting the CP
		// resolve which site a bounce/complaint belongs to without parsing headers.
		$custom_args = array();
		if ( $site_id !== '' ) {
			$custom_args['wpmgr_site'] = $site_id;
		}
		$tenant_id = (string) ( $mail['x_tenant_id'] ?? '' );
		if ( $tenant_id !== '' ) {
			$custom_args['wpmgr_tenant'] = $tenant_id;
		}
		if ( $custom_args !== array() ) {
			$personalisation['custom_args'] = $custom_args;
			// Replace the personalisation in the payload with the updated copy.
			$payload['personalizations'] = array( $personalisation );
		}

		// Attachments.
		$attachments = (array) ( $mail['attachments'] ?? array() );
		if ( $attachments !== array() ) {
			$sg_attachments = array();
			foreach ( $attachments as $att ) {
				if ( ! is_array( $att ) || empty( $att['path'] ) ) {
					continue;
				}
				$data = @file_get_contents( (string) $att['path'] );
				if ( $data === false ) {
					continue;
				}
				$sg_attachments[] = array(
					'content'     => base64_encode( $data ),
					'type'        => (string) ( $att['mime'] ?? 'application/octet-stream' ),
					'filename'    => (string) ( $att['name'] ?? basename( (string) $att['path'] ) ),
					'disposition' => 'attachment',
				);
			}
			if ( $sg_attachments !== array() ) {
				$payload['attachments'] = $sg_attachments;
			}
		}

		return $payload;
	}

	/**
	 * Turn one raw address field into SendGrid email objects.
	 *
	 * Parsed entries become {email, name}; entries AddressParser could not
	 * resolve are passed through as {email: raw}, which is byte for byte what
	 * this handler sent before the parser existed. That passthrough exists so
	 * our own validator can never be stricter than SendGrid's: an
	 * internationalised domain fails FILTER_VALIDATE_EMAIL, and dropping it
	 * here would silently stop delivering to a recipient that works today.
	 * It cannot reintroduce GH #312, because a well-formed "Name <addr>"
	 * always parses and so never reaches the passthrough.
	 *
	 * @param mixed $raw Raw address field from the mail payload.
	 * @return list<array{email:string}|array{email:string,name:string}>
	 */
	private function address_objects( $raw ): array {
		$entries = AddressParser::parse_list_verbose( $raw );
		$objects = array();

		foreach ( $entries['valid'] as $entry ) {
			$objects[] = $this->to_sendgrid_email_object( $entry );
		}
		foreach ( $entries['invalid'] as $entry ) {
			$raw_entry = trim( (string) $entry );
			// Only pass through something that is at least shaped like an
			// address. A entry with no "@" is not one our validator merely
			// failed to recognise, it is garbage, and forwarding it would 400
			// the whole personalization and lose every other recipient with it.
			if ( $raw_entry !== '' && strpos( $raw_entry, '@' ) !== false ) {
				$objects[] = array( 'email' => $raw_entry );
			}
		}

		return $objects;
	}

	/**
	 * Convert a parsed {address, name} entry into a SendGrid email object.
	 *
	 * @param array{address:string,name:string} $entry Parsed address entry.
	 * @return array{email:string}|array{email:string,name:string}
	 */
	private function to_sendgrid_email_object( array $entry ): array {
		return $entry['name'] !== ''
			? array( 'email' => $entry['address'], 'name' => $entry['name'] )
			: array( 'email' => $entry['address'] );
	}

	/**
	 * Parse a SendGrid error JSON body to extract the first error message.
	 *
	 * @param string $body Response body.
	 * @return string Human-readable error.
	 */
	private function parse_error( string $body ): string {
		$decoded = json_decode( $body, true );
		if ( is_array( $decoded ) && isset( $decoded['errors'][0]['message'] ) ) {
			return (string) $decoded['errors'][0]['message'];
		}
		return substr( $body, 0, 300 );
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
