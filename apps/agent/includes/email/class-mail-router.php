<?php
/**
 * MailRouter: hooks into WordPress's mail pipeline to route outgoing mail
 * through the configured WPMgr provider handler.
 *
 * Hook strategy (non-destructive):
 *   - Primary:  `pre_wp_mail` (WP 5.7+) short-circuits wp_mail() before PHPMailer
 *     is even instantiated. When no email config is set, returns null so WP's
 *     default mail path is UNTOUCHED.
 *   - Fallback: If `pre_wp_mail` is unavailable (WP < 5.7), hooks
 *     `wp_mail` (the pluggable function) via a wpmgr_mail() shim registered
 *     only when that hook exists, but in practice WP 5.7+ is the baseline
 *     (Requires at least: 6.2 in the plugin header), so the primary path
 *     always fires.
 *
 * Force-from and Return-Path are applied here (before the provider handler
 * sees the resolved From address) so every handler gets consistent mail data.
 *
 * The X-WPMgr-Site correlation header is stamped on every outgoing message for
 * the Phase-4 CP webhook fan-out.
 *
 * wp_mail()'s return value is honest: a provider failure makes wp_mail()
 * return false (never a lie of true) and fires `wp_mail_failed` with a
 * WP_Error, same as core would on its own send failure. See intercept().
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

use WPMgr\Agent\Settings;

/**
 * Intercepts wp_mail() and routes mail through WPMgr's provider pipeline.
 */
final class MailRouter {

	private ProviderRouter $router;

	private Settings $settings;

	/**
	 * @param ProviderRouter $router   Resolved provider dispatcher.
	 * @param Settings       $settings Agent settings (site_id for correlation header).
	 */
	public function __construct( ProviderRouter $router, Settings $settings ) {
		$this->router   = $router;
		$this->settings = $settings;
	}

	/**
	 * Register hooks. Called from Plugin::registerHooks().
	 *
	 * @return void
	 */
	public function register_hooks(): void {
		// pre_wp_mail is the primary interception point (WP 5.7+, our baseline).
		// Per wp-includes/pluggable.php, wp_mail() only lets WP's own PHPMailer
		// path run when this filter returns exactly `null`; ANY other value --
		// including `false` -- short-circuits and becomes wp_mail()'s own return
		// value verbatim. So we return the result array on success, false on
		// failure (still short-circuits, so WP never re-attempts a send we
		// already made -- but wp_mail() now honestly reports the failure instead
		// of claiming success), and null when email is not configured (leaves
		// WP untouched).
		add_filter( 'pre_wp_mail', array( $this, 'intercept' ), 10, 2 );
	}

	/**
	 * `pre_wp_mail` filter handler.
	 *
	 * @param mixed               $return    Existing filter return (null by default).
	 * @param array<string,mixed> $atts      wp_mail() arguments array:
	 *                                       {to, subject, message, headers, attachments}.
	 * @return mixed Null to let WP handle it; a value to short-circuit.
	 */
	public function intercept( $return, array $atts ) {
		// If another filter already short-circuited us, honour it.
		if ( $return !== null ) {
			return $return;
		}

		$cfg = EmailConfig::load();
		if ( ! $cfg->is_configured() ) {
			// No email config, leave WP's default mail path untouched.
			return null;
		}

		$mail = $this->build_mail_payload( $atts, $cfg );

		$result = $this->router->send( $mail, $cfg );

		if ( $result['ok'] ) {
			// Success: unchanged from before -- the provider result array is a
			// non-null, truthy value, so it short-circuits wp_mail() and becomes
			// its return value.
			return $result;
		}

		// Failure: `false` still short-circuits wp_mail() (see register_hooks()
		// above) so WP does NOT fall through to its own PHPMailer for a message
		// we already attempted -- but wp_mail()'s return value is now the
		// honest `false` instead of a lie. Fire wp_mail_failed ourselves since
		// short-circuiting skips WP's own call to it entirely, matching the
		// WP_Error shape core's own failure paths use (code 'wp_mail_failed',
		// the failure message, and the mail args minus body/secrets) so
		// existing listeners on that hook keep working.
		$error_message = $result['detail'] !== '' ? $result['detail'] : 'WPMgr: mail send failed.';

		$error_data = array(
			'to'                    => $atts['to'] ?? array(),
			'subject'               => (string) ( $atts['subject'] ?? '' ),
			'headers'               => $atts['headers'] ?? array(),
			'attachments'           => $atts['attachments'] ?? array(),
			// Provider-side failure detail; never credentials or the message body.
			'wpmgr_provider_detail' => $error_message,
		);

		// phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing core's own wp_mail_failed hook, not registering a global
		do_action( 'wp_mail_failed', new \WP_Error( 'wp_mail_failed', $error_message, $error_data ) );

		return false;
	}

	/**
	 * Build the normalised mail payload from raw wp_mail() arguments.
	 *
	 * Applies force-from-email, force-from-name, Return-Path, and stamps the
	 * X-WPMgr-Site correlation header.
	 *
	 * @param array<string,mixed> $atts wp_mail() argument array.
	 * @param EmailConfig         $cfg  Current email config.
	 * @return array<string,mixed> Normalised payload for ProviderHandlerInterface::send().
	 */
	public function build_mail_payload( array $atts, EmailConfig $cfg ): array {
		// -- Recipients -------------------------------------------------------
		// AddressParser::split_list() is quote-aware, so a bare comma-separated
		// $to string with a quoted display name that itself contains a comma
		// (e.g. '"Rossi, Andrea" <a@x.com>, b@y.com') splits into the correct
		// two entries rather than shredding the name in half.
		$to_raw = $atts['to'] ?? '';
		$to     = is_array( $to_raw ) ? $to_raw : AddressParser::split_list( (string) $to_raw );

		// -- Headers ----------------------------------------------------------
		$raw_headers = $atts['headers'] ?? '';
		$header_lines = is_array( $raw_headers )
			? $raw_headers
			: array_filter( array_map( 'trim', explode( "\n", str_replace( "\r\n", "\n", (string) $raw_headers ) ) ) );

		$cc       = array();
		$bcc      = array();
		$reply_to = array();
		$content_type = 'text/plain';
		$charset      = 'UTF-8';

		foreach ( $header_lines as $line ) {
			$line = (string) $line;
			if ( strpos( $line, ':' ) === false ) {
				continue;
			}
			list( $name, $value ) = explode( ':', $line, 2 );
			$name  = strtolower( trim( $name ) );
			$value = trim( $value );
			switch ( $name ) {
				case 'cc':
					$cc[] = $value;
					break;
				case 'bcc':
					$bcc[] = $value;
					break;
				case 'reply-to':
					$reply_to[] = $value;
					break;
				case 'content-type':
					// e.g. "text/html; charset=UTF-8"
					$parts        = explode( ';', $value, 2 );
					$content_type = trim( $parts[0] );
					if ( isset( $parts[1] ) && strpos( $parts[1], 'charset=' ) !== false ) {
						$cs = explode( '=', $parts[1], 2 );
						if ( isset( $cs[1] ) ) {
							$charset = trim( $cs[1] );
						}
					}
					break;
			}
		}

		// -- From address + name ----------------------------------------------
		// WP's default From filters: wp_mail_from / wp_mail_from_name.
		$wp_from      = function_exists( 'apply_filters' )
			// phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- calling core filter wp_mail_from, not registering a global
			? (string) apply_filters( 'wp_mail_from', 'wordpress@' . ( function_exists( 'wp_parse_url' ) ? (string) ( wp_parse_url( home_url(), PHP_URL_HOST ) ?? 'localhost' ) : 'localhost' ) )
			: '';
		$wp_from_name = function_exists( 'apply_filters' )
			// phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- calling core filter wp_mail_from_name, not registering a global
			? (string) apply_filters( 'wp_mail_from_name', 'WordPress' )
			: 'WordPress';

		$from      = ( $cfg->force_from_email && $cfg->from_address !== '' ) ? $cfg->from_address : $wp_from;
		$from_name = ( $cfg->force_from_name && $cfg->from_name !== '' ) ? $cfg->from_name : $wp_from_name;

		// -- Message body -----------------------------------------------------
		$raw_message = (string) ( $atts['message'] ?? '' );
		$body_html   = '';
		$body_text   = '';

		if ( strpos( strtolower( $content_type ), 'text/html' ) !== false ) {
			$body_html = $raw_message;
		} else {
			$body_text = $raw_message;
		}

		// -- Attachments -------------------------------------------------------
		// Capture basename, path, mime, and size_bytes (cap 50, filesize guarded).
		$raw_attachments = isset( $atts['attachments'] ) && is_array( $atts['attachments'] )
			? $atts['attachments'] : array();

		$attachments = array();
		foreach ( $raw_attachments as $path ) {
			$path = (string) $path;
			if ( $path === '' || ! @is_file( $path ) ) {
				continue;
			}
			if ( count( $attachments ) >= 50 ) {
				break;
			}
			$mime = function_exists( 'mime_content_type' )
				? (string) ( mime_content_type( $path ) ?: 'application/octet-stream' )
				: 'application/octet-stream';
			// filesize() returns int|false; treat false (unreadable) as 0 (unknown).
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_filesize -- headless agent; WP_Filesystem has no filesize() equivalent for local disk
			$raw_size   = @filesize( $path );
			$size_bytes = is_int( $raw_size ) && $raw_size >= 0 ? $raw_size : 0;
			$attachments[] = array(
				'name'       => basename( $path ),
				'path'       => $path,
				'mime'       => $mime,
				'size_bytes' => $size_bytes,
			);
		}

		// -- Site/tenant correlation headers (for Phase-4a CP webhook fan-out) --
		$site_id   = $this->settings->siteId();
		$tenant_id = $this->settings->tenantId();

		return array(
			'to'          => array_values( $to ),
			'cc'          => array_values( $cc ),
			'bcc'         => array_values( $bcc ),
			'reply_to'    => array_values( $reply_to ),
			'from'        => $from,
			'from_name'   => $from_name,
			'subject'     => (string) ( $atts['subject'] ?? '' ),
			'body_text'   => $body_text,
			'body_html'   => $body_html,
			'charset'     => $charset,
			'headers'     => array_values( $header_lines ),
			'attachments' => $attachments,
			'return_path' => $cfg->return_path,
			'x_site_id'   => $site_id !== '' ? $site_id : 'unknown',
			'x_tenant_id' => $tenant_id,
		);
	}
}
