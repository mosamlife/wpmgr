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
 * A SECOND `pre_wp_mail` filter is registered at PHP_INT_MAX (see
 * reassert_failed_short_circuit()). Filters on this hook run in priority
 * order and each one receives the PREVIOUS filter's return value, so a
 * later filter can turn our honest `false` back into `null` -- an ordinary
 * idiom like `if ( ! $return ) { return null; }` does exactly that, since
 * `false` and `null` are both falsy. `null` is the ONE value that makes
 * wp_mail() fall through to its own PHPMailer, so that ordinary idiom would
 * make WordPress send a message WPMgr already attempted and knows failed --
 * a silent duplicate delivery. The low-priority filter re-asserts `false`
 * only when it can prove WPMgr itself attempted and failed THIS dispatch
 * and the value has been reset to exactly `null`; any other non-null value
 * means some other plugin has legitimately taken over, and is left alone.
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
	 * LIFO stack, one entry pushed per `pre_wp_mail` dispatch that reaches
	 * intercept() (i.e. every call: intercept() pushes unconditionally on
	 * entry), true only when THIS dispatch attempted a send and it failed.
	 * reassert_failed_short_circuit() pops exactly one entry per dispatch.
	 * A stack (not a single flag) is required because a `wp_mail_failed`
	 * listener that itself calls wp_mail() re-enters intercept() BEFORE the
	 * outer dispatch's own reassert filter has run; nested pre_wp_mail
	 * dispatches always fully resolve -- both filters -- before an outer one
	 * resumes, so push/pop stay correctly paired (LIFO) across recursion.
	 *
	 * @var array<int,bool>
	 */
	private array $attemptFailed = array();

	/**
	 * True for the duration of OUR OWN do_action( 'wp_mail_failed', ... )
	 * call inside intercept()'s failure branch. Guards against unbounded
	 * recursion: a listener on that hook which itself calls wp_mail() (a
	 * typical failure-alerting plugin) would otherwise re-enter intercept()
	 * forever whenever the alert send also fails. See intercept().
	 *
	 * @var bool
	 */
	private bool $firingFailureNotice = false;

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

		// Second filter, at the lowest possible priority so it runs LAST: a
		// later filter at ordinary priority can legitimately (if accidentally)
		// reset our `false` back to `null` -- see the class docblock. This one
		// re-asserts `false` only when WPMgr itself attempted THIS dispatch and
		// it failed, and only when the value it now sees is exactly `null`.
		add_filter( 'pre_wp_mail', array( $this, 'reassert_failed_short_circuit' ), PHP_INT_MAX, 2 );
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
		// Push a placeholder for THIS dispatch before anything else, so
		// reassert_failed_short_circuit() -- which always runs after this
		// method for the same dispatch, even across a recursive wp_mail()
		// call triggered below -- can pop the matching entry. See the
		// $attemptFailed docblock for why a stack, not a single flag.
		$this->attemptFailed[] = false;
		$stack_index           = array_key_last( $this->attemptFailed );

		// If another filter already short-circuited us, honour it.
		if ( $return !== null ) {
			return $return;
		}

		// REGRESSION E2 guard: refuse a nested send while we are already
		// inside firing OUR OWN wp_mail_failed for an earlier failure in
		// this call stack. Without this, a wp_mail_failed listener that
		// itself calls wp_mail() (a typical failure-alerting plugin)
		// recurses without bound whenever the alert send also fails --
		// reproduced by the adversarial review to depth 2001. Reporting an
		// immediate, un-attempted `false` here is deliberate: it is the only
		// response that terminates the cycle, because it neither hits the
		// provider again nor re-fires the hook that caused the recursion.
		if ( $this->firingFailureNotice ) {
			$this->attemptFailed[ $stack_index ] = true;
			// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- gated diagnostic channel (DebugLog only writes under WP_DEBUG_LOG or WPMGR_DEBUG); never fires wp_mail_failed again so this branch cannot itself recurse
			\WPMgr\Agent\Support\DebugLog::write( 'WPMgr Mail: refused a re-entrant wp_mail() call received while already handling a failed send, to avoid unbounded recursion.' );
			return false;
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
		// the failure message, and the mail args) so existing listeners on
		// that hook keep working.
		$this->attemptFailed[ $stack_index ] = true;

		$error_message = $result['detail'] !== '' ? $result['detail'] : 'WPMgr: mail send failed.';

		$error_data = array(
			'to'          => $atts['to'] ?? array(),
			'subject'     => (string) ( $atts['subject'] ?? '' ),
			// Same raw headers a listener would see from core's own native
			// wp_mail_failed, except the VALUE of a Bcc or an auth/token/
			// key/secret/password-shaped header NAME is redacted -- see
			// redact_sensitive_headers(). FINDING C: this hook now fires on
			// every WPMgr-routed failure, which it never did before GH #439
			// made a failed send honest, so a header the site set for its
			// own delivery (a Bcc archive address, a bearer token some site
			// code adds) would otherwise reach every OTHER plugin subscribed
			// to this hook on a site where nothing reached them before.
			'headers'     => $this->redact_sensitive_headers( $mail['headers'] ),
			'attachments' => $atts['attachments'] ?? array(),
			// Provider-side failure detail. Never the message body -- $mail
			// carries body_text/body_html but neither is read here -- and
			// never a raw credential: this is a provider-defined error
			// string, not header or body content.
			'wpmgr_provider_detail' => $error_message,
		);

		// REGRESSION E1 guard: a listener on wp_mail_failed that throws must
		// not stop US from producing our own honest return value. Core's own
		// wp_mail() does not catch anything from this hook, so an uncaught
		// exception here would otherwise escape wp_mail() itself and fatal
		// whatever called it (a checkout, a password reset). The finally
		// block resets the re-entrancy guard even when a listener throws --
		// without it, one thrown exception would wedge the guard "on" for
		// the rest of the request and silently break every later send.
		$this->firingFailureNotice = true;
		try {
			// phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing core's own wp_mail_failed hook, not registering a global
			do_action( 'wp_mail_failed', new \WP_Error( 'wp_mail_failed', $error_message, $error_data ) );
		} catch ( \Throwable $e ) {
			// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- gated diagnostic channel; only writes under WP_DEBUG_LOG or WPMGR_DEBUG
			\WPMgr\Agent\Support\DebugLog::write( 'WPMgr Mail: a wp_mail_failed listener threw and was caught so the send path still returns: ' . $e->getMessage() );
		} finally {
			$this->firingFailureNotice = false;
		}

		return false;
	}

	/**
	 * Second `pre_wp_mail` filter, registered at PHP_INT_MAX (see
	 * register_hooks()) so it runs after every other registered filter.
	 *
	 * Corrects ONLY the case where WPMgr itself attempted THIS dispatch, it
	 * failed, and a later filter has reset the value to exactly `null` --
	 * see the class docblock for why that specific reset is dangerous (it is
	 * the one value that makes wp_mail() fall through to core's own
	 * PHPMailer, duplicating a send WPMgr already made). Any other non-null
	 * value means some other plugin has legitimately taken over and is left
	 * untouched.
	 *
	 * @param mixed               $return Filter chain's current value.
	 * @param array<string,mixed> $atts   wp_mail() arguments array (unused; required by the pre_wp_mail signature).
	 * @return mixed
	 */
	public function reassert_failed_short_circuit( $return, array $atts ) {
		unset( $atts );

		$failed = array_pop( $this->attemptFailed ) ?? false;

		if ( $failed && $return === null ) {
			return false;
		}

		return $return;
	}

	/**
	 * Redact the VALUE of a known-sensitive header before it reaches a
	 * third-party wp_mail_failed listener. The header NAME (and every other
	 * header) is left exactly as-is, so a diagnostic listener can still see
	 * what kind of header was present.
	 *
	 * "Sensitive" is a closed, name-based check: the literal header `Bcc`
	 * (its entire purpose is to stay hidden from other recipients, so
	 * surfacing it to every OTHER plugin on the site is itself a leak), plus
	 * any header name that looks credential-shaped (auth, token, key,
	 * secret, or password, case-insensitive) -- e.g. `Authorization` or a
	 * site-specific `X-Auth-Token`.
	 *
	 * @param array<int,string> $header_lines Normalised "Name: value" header lines.
	 * @return array<int,string>
	 */
	private function redact_sensitive_headers( array $header_lines ): array {
		return array_map(
			static function ( $line ) {
				$line = (string) $line;
				if ( strpos( $line, ':' ) === false ) {
					return $line;
				}
				list( $name, $value ) = explode( ':', $line, 2 );
				unset( $value );
				$bare_name = strtolower( trim( $name ) );
				$is_sensitive = $bare_name === 'bcc' || preg_match( '/auth|token|key|secret|password/i', $bare_name ) === 1;
				if ( ! $is_sensitive ) {
					return $line;
				}
				return trim( $name ) . ': [redacted]';
			},
			$header_lines
		);
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
