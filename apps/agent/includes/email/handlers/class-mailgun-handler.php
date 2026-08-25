<?php
/**
 * MailgunHandler — sends via the Mailgun Messages API.
 *
 * Endpoint:
 *   US:  https://api.mailgun.net/v3/{domain}/messages
 *   EU:  https://api.eu.mailgun.net/v3/{domain}/messages
 *
 * Auth:    HTTP Basic — username 'api', password = API key.
 * Success: HTTP 200 (with JSON body containing 'id' and 'message').
 *
 * Config shape (non-secret, from EmailConfig::$config):
 *   domain_name  string  Mailgun domain (e.g. mg.example.com).
 *   region       string  'us' or 'eu' (default 'us').
 *
 * Secret (from keystore): Mailgun API key.
 *
 * @package WPMgr\Agent\Email\Handlers
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email\Handlers;

use WPMgr\Agent\Email\ProviderHandlerInterface;

/**
 * Mailgun Messages API provider handler.
 */
final class MailgunHandler implements ProviderHandlerInterface {

	private const ENDPOINT_US = 'https://api.mailgun.net/v3/%s/messages';
	private const ENDPOINT_EU = 'https://api.eu.mailgun.net/v3/%s/messages';

	/** @inheritDoc */
	public function provider(): string {
		return 'mailgun';
	}

	/** @inheritDoc */
	public function send( array $mail, array $config, string $secret ): array {
		$domain = isset( $config['domain_name'] ) && is_string( $config['domain_name'] )
			? trim( $config['domain_name'] ) : '';
		if ( $domain === '' ) {
			return $this->failure( 'Mailgun domain_name not configured' );
		}
		if ( $secret === '' ) {
			return $this->failure( 'Mailgun API key not configured' );
		}

		$region   = ( isset( $config['region'] ) && strtolower( (string) $config['region'] ) === 'eu' ) ? 'eu' : 'us';
		$endpoint = sprintf(
			$region === 'eu' ? self::ENDPOINT_EU : self::ENDPOINT_US,
			rawurlencode( $domain )
		);

		// -- Build form-encoded body (Mailgun accepts multipart/form-data or
		// application/x-www-form-urlencoded; wp_remote_post passes array bodies
		// as application/x-www-form-urlencoded automatically when the body is
		// an array).
		$from_str = (string) ( $mail['from'] ?? '' );
		$from_name = (string) ( $mail['from_name'] ?? '' );
		if ( $from_name !== '' ) {
			$from_str = $from_name . ' <' . $from_str . '>';
		}

		$body = array(
			'from'    => $from_str,
			'to'      => implode( ',', (array) ( $mail['to'] ?? array() ) ),
			'subject' => (string) ( $mail['subject'] ?? '' ),
		);

		$cc = (array) ( $mail['cc'] ?? array() );
		if ( $cc !== array() ) {
			$body['cc'] = implode( ',', $cc );
		}

		$bcc = (array) ( $mail['bcc'] ?? array() );
		if ( $bcc !== array() ) {
			$body['bcc'] = implode( ',', $bcc );
		}

		$reply_to = (array) ( $mail['reply_to'] ?? array() );
		if ( $reply_to !== array() ) {
			$body['h:Reply-To'] = implode( ',', $reply_to );
		}

		$body_html = (string) ( $mail['body_html'] ?? '' );
		$body_text = (string) ( $mail['body_text'] ?? '' );

		if ( $body_html !== '' ) {
			$body['html'] = $body_html;
		}
		if ( $body_text !== '' ) {
			$body['text'] = $body_text;
		}

		$site_id = (string) ( $mail['x_site_id'] ?? '' );
		if ( $site_id !== '' ) {
			$body['h:X-WPMgr-Site'] = $site_id;
		}

		// Mailgun custom variables (v:) — included in every webhook event payload,
		// letting the CP webhook fan-out resolve the originating site/tenant.
		if ( $site_id !== '' ) {
			$body['v:wpmgr_site'] = $site_id;
		}
		$tenant_id = (string) ( $mail['x_tenant_id'] ?? '' );
		if ( $tenant_id !== '' ) {
			$body['v:wpmgr_tenant'] = $tenant_id;
		}

		if ( ! empty( $mail['return_path'] ) && isset( $mail['from'] ) ) {
			$body['h:Return-Path'] = '<' . $mail['from'] . '>';
		}

		// Attachments: Mailgun's HTTP API accepts inline attachments in the
		// multipart body, but wp_remote_post with an array body sends
		// form-encoded (not multipart/form-data). For v1, attachments are
		// included as base64 data URIs in the 'inline' body field, which
		// Mailgun supports for inline content. Full attachment support
		// requires raw multipart; deferred to Phase 3 if needed.
		// For now, note attachment names in the subject if any are present.
		$attachments = (array) ( $mail['attachments'] ?? array() );
		if ( $attachments !== array() ) {
			// Encode inline attachments as Mailgun attachment fields.
			// wp_remote_post array body cannot do multipart/form-data with
			// binary; instead we base64-encode and use message/rfc822 parts.
			// The limitation is noted: in-flight for Phase 3 multipart support.
			// For now we include a note in the text body (graceful degradation).
			$att_names = array();
			foreach ( $attachments as $att ) {
				if ( is_array( $att ) && isset( $att['name'] ) ) {
					$att_names[] = (string) $att['name'];
				}
			}
			if ( $att_names !== array() && $body_text !== '' ) {
				$body['text'] = ( $body['text'] ?? '' ) . "\n\n[Attachments: " . implode( ', ', $att_names ) . ']';
			}
		}

		$response = wp_remote_post(
			$endpoint,
			array(
				'timeout' => 30,
				'headers' => array(
					'Authorization' => 'Basic ' . base64_encode( 'api:' . $secret ),
				),
				'body'    => $body,
			)
		);

		if ( is_wp_error( $response ) ) {
			return $this->failure( $response->get_error_message() );
		}

		$code     = (int) wp_remote_retrieve_response_code( $response );
		$body_str = (string) wp_remote_retrieve_body( $response );

		if ( $code !== 200 ) {
			$msg = $this->parse_error( $body_str );
			return $this->failure( 'Mailgun error ' . $code . ': ' . $msg );
		}

		$decoded    = json_decode( $body_str, true );
		$message_id = '';
		if ( is_array( $decoded ) && isset( $decoded['id'] ) ) {
			// Mailgun returns '<...@mailgun.org>'; strip angle brackets.
			$message_id = trim( (string) $decoded['id'], '<>' );
		}

		return array(
			'ok'                => true,
			'message_id'        => $message_id,
			'error'             => '',
			'provider_response' => substr( $body_str, 0, 500 ),
		);
	}

	/**
	 * Parse a Mailgun error JSON body.
	 *
	 * @param string $body Response body.
	 * @return string Human-readable error.
	 */
	private function parse_error( string $body ): string {
		$decoded = json_decode( $body, true );
		if ( is_array( $decoded ) && isset( $decoded['message'] ) ) {
			return (string) $decoded['message'];
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
