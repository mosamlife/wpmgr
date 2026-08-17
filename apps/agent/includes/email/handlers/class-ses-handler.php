<?php
/**
 * SesHandler — sends via the Amazon SES v1 API using AWS Signature Version 4.
 *
 * Sends a raw MIME message via POST to
 *   https://email.{region}.amazonaws.com/
 * with Action=SendRawEmail, using HTTP form-encoded parameters + SigV4.
 *
 * Config shape (non-secret, from EmailConfig::$config):
 *   access_key  string  AWS Access Key ID.
 *   region      string  AWS region (e.g. us-east-1). Default: us-east-1.
 *
 * Secret (from keystore): AWS Secret Access Key.
 *
 * SigV4 signing is implemented inline in pure PHP: this avoids any third-party
 * AWS SDK dependency. The algorithm follows the published AWS SigV4 spec:
 *   service=email, signed headers=content-type;host;x-amz-date.
 *
 * @package WPMgr\Agent\Email\Handlers
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email\Handlers;

use WPMgr\Agent\Email\ProviderHandlerInterface;

/**
 * Amazon SES provider handler.
 */
final class SesHandler implements ProviderHandlerInterface {

	/** SES endpoint pattern. */
	private const ENDPOINT = 'https://email.%s.amazonaws.com/'; // phpcs:ignore PluginCheck.CodeAnalysis.Offloading.OffloadedContent -- not offloaded front-end content; this is the Amazon SES v1 API endpoint used to send outbound email via the site owner's own AWS SigV4-signed credentials. Flagged only because plugin-check 2.1.0's OffloadingSniff now matches "amazonaws.com" unconditionally in any string literal, not just enqueue/markup contexts.

	/** SES service name for SigV4. */
	private const SERVICE = 'email';

	/** @inheritDoc */
	public function provider(): string {
		return 'ses';
	}

	/** @inheritDoc */
	public function send( array $mail, array $config, string $secret ): array {
		$access_key = isset( $config['access_key'] ) && is_string( $config['access_key'] )
			? trim( $config['access_key'] ) : '';
		$region     = isset( $config['region'] ) && is_string( $config['region'] )
			? trim( $config['region'] ) : 'us-east-1';

		if ( $access_key === '' || $secret === '' ) {
			return $this->failure( 'SES access_key or secret not configured' );
		}

		$raw_mime = $this->build_raw_mime( $mail );

		// SES SendRawEmail — form-encoded POST body.
		// Tags allow the CP webhook fan-out (SNS → Lambda → CP) to resolve
		// the originating site/tenant from the notification metadata.
		$params = array(
			'Action'          => 'SendRawEmail',
			'RawMessage.Data' => base64_encode( $raw_mime ),
		);

		$site_id   = (string) ( $mail['x_site_id'] ?? '' );
		$tenant_id = (string) ( $mail['x_tenant_id'] ?? '' );
		$tag_index = 1;
		if ( $site_id !== '' ) {
			$params[ 'Tags.member.' . $tag_index . '.Name' ]  = 'wpmgr_site';
			$params[ 'Tags.member.' . $tag_index . '.Value' ] = $site_id;
			++$tag_index;
		}
		if ( $tenant_id !== '' ) {
			$params[ 'Tags.member.' . $tag_index . '.Name' ]  = 'wpmgr_tenant';
			$params[ 'Tags.member.' . $tag_index . '.Value' ] = $tenant_id;
		}

		$post_body = http_build_query( $params );

		$endpoint = sprintf( self::ENDPOINT, $region );
		$host     = (string) ( wp_parse_url( $endpoint, PHP_URL_HOST ) ?? '' );

		// Build SigV4 headers.
		$amz_date    = gmdate( 'Ymd\THis\Z' );
		$date_stamp  = gmdate( 'Ymd' );
		$content_type = 'application/x-www-form-urlencoded';

		$headers_to_sign = array(
			'content-type' => $content_type,
			'host'         => $host,
			'x-amz-date'   => $amz_date,
		);

		$signed_headers_str = implode( ';', array_keys( $headers_to_sign ) );

		// Canonical request.
		$canonical_headers = '';
		foreach ( $headers_to_sign as $k => $v ) {
			$canonical_headers .= $k . ':' . trim( $v ) . "\n";
		}

		$canonical_request = implode(
			"\n",
			array(
				'POST',
				'/',
				'',
				$canonical_headers,
				$signed_headers_str,
				hash( 'sha256', $post_body ),
			)
		);

		// String to sign.
		$credential_scope = $date_stamp . '/' . $region . '/' . self::SERVICE . '/aws4_request';
		$string_to_sign   = implode(
			"\n",
			array(
				'AWS4-HMAC-SHA256',
				$amz_date,
				$credential_scope,
				hash( 'sha256', $canonical_request ),
			)
		);

		// Signing key derivation.
		$k_date    = hash_hmac( 'sha256', $date_stamp, 'AWS4' . $secret, true );
		$k_region  = hash_hmac( 'sha256', $region, $k_date, true );
		$k_service = hash_hmac( 'sha256', self::SERVICE, $k_region, true );
		$k_signing = hash_hmac( 'sha256', 'aws4_request', $k_service, true );

		$signature = hash_hmac( 'sha256', $string_to_sign, $k_signing );

		$auth_header = 'AWS4-HMAC-SHA256 Credential=' . $access_key . '/' . $credential_scope
			. ', SignedHeaders=' . $signed_headers_str
			. ', Signature=' . $signature;

		$response = wp_remote_post(
			$endpoint,
			array(
				'timeout' => 30,
				'headers' => array(
					'Content-Type'  => $content_type,
					'X-Amz-Date'    => $amz_date,
					'Authorization' => $auth_header,
				),
				'body'    => $post_body,
			)
		);

		if ( is_wp_error( $response ) ) {
			return $this->failure( $response->get_error_message() );
		}

		$code = (int) wp_remote_retrieve_response_code( $response );
		$body = (string) wp_remote_retrieve_body( $response );

		if ( $code !== 200 ) {
			$msg = $this->parse_error( $body );
			return $this->failure( 'SES error ' . $code . ': ' . $msg );
		}

		// Parse the MessageId from the XML response.
		$message_id = '';
		if ( preg_match( '/<MessageId>([^<]+)<\/MessageId>/', $body, $m ) ) {
			$message_id = $m[1];
		}

		return array(
			'ok'                => true,
			'message_id'        => $message_id,
			'error'             => '',
			'provider_response' => substr( $body, 0, 500 ),
		);
	}

	/**
	 * Build a minimal raw MIME message from the normalised mail payload.
	 *
	 * @param array<string,mixed> $mail Normalised mail payload.
	 * @return string Raw MIME message.
	 */
	private function build_raw_mime( array $mail ): string {
		$boundary = 'wpmgr_' . bin2hex( random_bytes( 8 ) );
		$lines    = array();

		// Headers.
		$from      = (string) ( $mail['from'] ?? '' );
		$from_name = (string) ( $mail['from_name'] ?? '' );
		$from_str  = $from_name !== '' ? $from_name . ' <' . $from . '>' : $from;

		$lines[] = 'From: ' . $from_str;

		$to = (array) ( $mail['to'] ?? array() );
		$lines[] = 'To: ' . implode( ', ', $to );

		$cc = (array) ( $mail['cc'] ?? array() );
		if ( $cc !== array() ) {
			$lines[] = 'Cc: ' . implode( ', ', $cc );
		}

		$bcc = (array) ( $mail['bcc'] ?? array() );
		if ( $bcc !== array() ) {
			$lines[] = 'Bcc: ' . implode( ', ', $bcc );
		}

		$reply_to = (array) ( $mail['reply_to'] ?? array() );
		if ( $reply_to !== array() ) {
			$lines[] = 'Reply-To: ' . implode( ', ', $reply_to );
		}

		$lines[] = 'Subject: ' . (string) ( $mail['subject'] ?? '' );

		$site_id = (string) ( $mail['x_site_id'] ?? '' );
		if ( $site_id !== '' ) {
			$lines[] = 'X-WPMgr-Site: ' . $site_id;
		}

		if ( ! empty( $mail['return_path'] ) && $from !== '' ) {
			$lines[] = 'Return-Path: <' . $from . '>';
		}

		$charset   = (string) ( $mail['charset'] ?? 'UTF-8' );
		$body_html = (string) ( $mail['body_html'] ?? '' );
		$body_text = (string) ( $mail['body_text'] ?? '' );
		$attachments = (array) ( $mail['attachments'] ?? array() );

		if ( $attachments !== array() ) {
			// multipart/mixed wrapping multipart/alternative + attachments.
			$inner_boundary = 'wpmgr_inner_' . bin2hex( random_bytes( 6 ) );
			$lines[] = 'MIME-Version: 1.0';
			$lines[] = 'Content-Type: multipart/mixed; boundary="' . $boundary . '"';
			$lines[] = '';
			$lines[] = '--' . $boundary;
			$lines[] = 'Content-Type: multipart/alternative; boundary="' . $inner_boundary . '"';
			$lines[] = '';

			if ( $body_text !== '' ) {
				$lines[] = '--' . $inner_boundary;
				$lines[] = 'Content-Type: text/plain; charset=' . $charset;
				$lines[] = 'Content-Transfer-Encoding: base64';
				$lines[] = '';
				$lines[] = chunk_split( base64_encode( $body_text ) );
			}
			if ( $body_html !== '' ) {
				$lines[] = '--' . $inner_boundary;
				$lines[] = 'Content-Type: text/html; charset=' . $charset;
				$lines[] = 'Content-Transfer-Encoding: base64';
				$lines[] = '';
				$lines[] = chunk_split( base64_encode( $body_html ) );
			}
			$lines[] = '--' . $inner_boundary . '--';

			foreach ( $attachments as $att ) {
				if ( ! is_array( $att ) || empty( $att['path'] ) ) {
					continue;
				}
				$att_data = @file_get_contents( (string) $att['path'] );
				if ( $att_data === false ) {
					continue;
				}
				$lines[] = '--' . $boundary;
				$lines[] = 'Content-Type: ' . (string) ( $att['mime'] ?? 'application/octet-stream' );
				$lines[] = 'Content-Transfer-Encoding: base64';
				$lines[] = 'Content-Disposition: attachment; filename="' . ( $att['name'] ?? basename( (string) $att['path'] ) ) . '"';
				$lines[] = '';
				$lines[] = chunk_split( base64_encode( $att_data ) );
			}
			$lines[] = '--' . $boundary . '--';
		} elseif ( $body_html !== '' ) {
			$lines[] = 'MIME-Version: 1.0';
			$lines[] = 'Content-Type: multipart/alternative; boundary="' . $boundary . '"';
			$lines[] = '';
			if ( $body_text !== '' ) {
				$lines[] = '--' . $boundary;
				$lines[] = 'Content-Type: text/plain; charset=' . $charset;
				$lines[] = 'Content-Transfer-Encoding: base64';
				$lines[] = '';
				$lines[] = chunk_split( base64_encode( $body_text ) );
			}
			$lines[] = '--' . $boundary;
			$lines[] = 'Content-Type: text/html; charset=' . $charset;
			$lines[] = 'Content-Transfer-Encoding: base64';
			$lines[] = '';
			$lines[] = chunk_split( base64_encode( $body_html ) );
			$lines[] = '--' . $boundary . '--';
		} else {
			$lines[] = 'MIME-Version: 1.0';
			$lines[] = 'Content-Type: text/plain; charset=' . $charset;
			$lines[] = 'Content-Transfer-Encoding: base64';
			$lines[] = '';
			$lines[] = chunk_split( base64_encode( $body_text ) );
		}

		return implode( "\r\n", $lines );
	}

	/**
	 * Extract an error message from a SES XML error response.
	 *
	 * @param string $xml SES error XML body.
	 * @return string Human-readable error.
	 */
	private function parse_error( string $xml ): string {
		if ( preg_match( '/<Message>([^<]+)<\/Message>/', $xml, $m ) ) {
			return $m[1];
		}
		return substr( $xml, 0, 300 );
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
