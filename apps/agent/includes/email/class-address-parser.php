<?php
/**
 * AddressParser: parses RFC 5322 mailbox-list address strings ("Name
 * <addr@example.com>", a bare address, or a comma-separated mixture of the
 * two, including a quoted display name that itself contains a comma) into
 * structured {address, name} entries, and formats them back for header
 * building.
 *
 * Why this exists (GH #312): every outgoing-mail provider handler needs a
 * BARE address at the point it calls PHPMailer::addAddress()/addCC()/
 * addBCC()/addReplyTo() or builds a provider API's `email` field, but
 * MailRouter stores the raw wp_mail() header value verbatim (e.g. the
 * literal string "Andrea Somigli <salesianalibri@gmail.com>"). Handing that
 * whole string to PHPMailer as the address throws
 * "Invalid address:  (Reply-To): Andrea Somigli <salesianalibri@gmail.com>"
 * and, because SmtpHandler wraps the entire send in one try/catch, takes the
 * WHOLE message down with it: not just the one malformed field.
 *
 * This parser is deliberately self-contained (no PHPMailer dependency):
 * PHPMailer::parseAddresses() only splits a quoted-comma display name
 * correctly when the imap extension is loaded (it falls back to a bare
 * explode(',') otherwise), which is neither guaranteed on a live WordPress
 * host nor available in this plugin's own test process. The five provider
 * handlers (SMTP + four HTTP APIs) all need identical, host-independent
 * parsing, so the logic lives here once rather than five times.
 *
 * Never throws. A malformed entry is dropped (parse_one() returns null;
 * parse_list_verbose() surfaces it back to the caller so it can be reported
 * and skipped, one address, instead of failing the whole send).
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

/**
 * Pure, dependency-free RFC 5322 address-list parser and formatter.
 */
final class AddressParser {

	/**
	 * Parse a mixed-shape address list into structured entries, dropping any
	 * entry that fails to resolve to a syntactically valid address.
	 *
	 * @param string|array<int,mixed> $value Raw header value string (which may
	 *                                       itself contain a comma-separated list,
	 *                                       e.g. "a@x.com, b@y.com"), or an array
	 *                                       of such strings (one per header line
	 *                                       MailRouter collected).
	 * @return array<int,array{address:string,name:string}>
	 */
	public static function parse_list( $value ): array {
		return self::parse_list_verbose( $value )['valid'];
	}

	/**
	 * Same as parse_list() but also returns the raw entries that could not be
	 * parsed, so a caller can report and skip them individually rather than
	 * silently losing them.
	 *
	 * @param string|array<int,mixed> $value Raw address list (string or array of strings).
	 * @return array{valid: array<int,array{address:string,name:string}>, invalid: array<int,string>}
	 */
	public static function parse_list_verbose( $value ): array {
		$valid   = array();
		$invalid = array();

		$raw_entries = array();
		if ( is_array( $value ) ) {
			foreach ( $value as $item ) {
				if ( ! is_scalar( $item ) ) {
					continue;
				}
				$raw_entries = array_merge( $raw_entries, self::split_list( (string) $item ) );
			}
		} else {
			$raw_entries = self::split_list( (string) $value );
		}

		foreach ( $raw_entries as $raw_entry ) {
			$parsed = self::parse_one( $raw_entry );
			if ( $parsed === null ) {
				$invalid[] = $raw_entry;
				continue;
			}
			$valid[] = $parsed;
		}

		return array(
			'valid'   => $valid,
			'invalid' => $invalid,
		);
	}

	/**
	 * Parse a SINGLE address entry (already split out of any surrounding
	 * list): a bare address, or the "Display Name <addr@example.com>" form
	 * with an optionally quoted display name. Never throws.
	 *
	 * @param string $entry One address entry.
	 * @return array{address:string,name:string}|null Null when the entry does
	 *         not resolve to a syntactically valid email address.
	 */
	public static function parse_one( string $entry ): ?array {
		$entry = self::strip_crlf( $entry );
		if ( $entry === '' ) {
			return null;
		}

		$name    = '';
		$address = $entry;

		if ( preg_match( '/^(.*)<([^<>]*)>\s*$/', $entry, $matches ) === 1 ) {
			$raw_name = trim( $matches[1] );

			// SECURITY: the pattern above is greedy, so it binds to the LAST
			// angle-addr in the entry. That is correct for a well-formed entry
			// (exactly one angle-addr, at the end) but it means
			// 'Bob <bob@example.com> <evil@example.net>' would resolve to the
			// SECOND address while displaying the first. Header values are
			// routinely built by interpolating a user-supplied name into
			// "{name} <{email}>", so a submitted name carrying its own
			// angle-addr would silently redirect the message.
			//
			// A leftover angle bracket in the display-name part therefore only
			// survives when the whole part is a quoted-string, which is the one
			// shape RFC 5322 allows it in. Anything else is refused here and
			// handled exactly as it was before this parser existed: the raw
			// entry goes to the transport, which rejects it loudly rather than
			// delivering it somewhere the operator did not intend.
			if ( strpbrk( $raw_name, '<>' ) !== false && ! self::is_quoted_string( $raw_name ) ) {
				return null;
			}

			$name    = self::unquote( $raw_name );
			$address = trim( $matches[2] );
		}

		$address = trim( $address );

		if ( $address === '' || ! self::is_valid_address( $address ) ) {
			return null;
		}

		return array(
			'address' => $address,
			'name'    => $name,
		);
	}

	/**
	 * Split a raw address-list string into individual entries on commas or
	 * semicolons that fall OUTSIDE a quoted display name or an angle-bracketed
	 * address, so a quoted name containing a comma (e.g. '"Rossi, Andrea"
	 * <a@x.com>') is never split in half.
	 *
	 * @param string $raw Raw list, e.g. '"Rossi, Andrea" <a@x.com>, b@y.com'.
	 * @return array<int,string> Trimmed, non-empty entries, in order.
	 */
	public static function split_list( string $raw ): array {
		$entries   = array();
		$current   = '';
		$in_quotes = false;
		$in_angle  = false;
		$length    = strlen( $raw );

		for ( $i = 0; $i < $length; $i++ ) {
			$char = $raw[ $i ];

			if ( $in_quotes && $char === '\\' && $i + 1 < $length ) {
				$current .= $char . $raw[ $i + 1 ];
				++$i;
				continue;
			}

			if ( $char === '"' ) {
				$in_quotes = ! $in_quotes;
				$current  .= $char;
				continue;
			}

			if ( ! $in_quotes && $char === '<' ) {
				$in_angle = true;
				$current .= $char;
				continue;
			}

			if ( ! $in_quotes && $char === '>' ) {
				$in_angle = false;
				$current .= $char;
				continue;
			}

			if ( ! $in_quotes && ! $in_angle && ( $char === ',' || $char === ';' ) ) {
				$entries[] = trim( $current );
				$current   = '';
				continue;
			}

			$current .= $char;
		}
		$entries[] = trim( $current );

		return array_values(
			array_filter(
				$entries,
				static function ( string $entry ): bool {
					return $entry !== '';
				}
			)
		);
	}

	/**
	 * Format one parsed entry back into RFC 5322 "Name <addr>" form (or a
	 * bare address when there is no name), quoting/escaping the display name
	 * when needed and stripping CR/LF so the result is always safe to place
	 * directly into a raw header line.
	 *
	 * @param array{address:string,name:string} $entry Parsed entry.
	 * @return string Empty string when the address itself is empty.
	 */
	public static function format( array $entry ): string {
		$address = isset( $entry['address'] ) ? self::strip_crlf( (string) $entry['address'] ) : '';
		if ( $address === '' ) {
			return '';
		}

		$name = isset( $entry['name'] ) ? self::strip_crlf( (string) $entry['name'] ) : '';
		if ( $name === '' ) {
			return $address;
		}

		return self::quote_name( $name ) . ' <' . $address . '>';
	}

	/**
	 * Format a list of parsed entries into a single comma-joined header value.
	 *
	 * @param array<int,array{address:string,name:string}> $entries Parsed entries.
	 * @return string
	 */
	public static function format_list( array $entries ): string {
		$formatted = array();
		foreach ( $entries as $entry ) {
			$one = self::format( $entry );
			if ( $one !== '' ) {
				$formatted[] = $one;
			}
		}
		return implode( ', ', $formatted );
	}

	/**
	 * Whether a string is a syntactically valid RFC 5322 addr-spec.
	 *
	 * Deliberately the same validator PHPMailer itself uses by default
	 * (FILTER_VALIDATE_EMAIL), so an address this parser accepts is never one
	 * that PHPMailer's own addAddress()/addCC()/addBCC()/addReplyTo() would
	 * still reject.
	 *
	 * @param string $address Candidate address.
	 * @return bool
	 */
	private static function is_valid_address( string $address ): bool {
		return filter_var( $address, FILTER_VALIDATE_EMAIL ) !== false;
	}

	/**
	 * Strip a surrounding quoted-string and unescape backslash-escaped
	 * characters from a display name, e.g. '"Rossi, Andrea"' -> 'Rossi, Andrea'.
	 * A name that is not quoted is returned unchanged (aside from trimming).
	 *
	 * @param string $name Raw (possibly quoted) display name.
	 * @return string
	 */
	/**
	 * Whether a display-name part is a complete RFC 5322 quoted-string, i.e.
	 * opens and closes with an unescaped double quote and contains no
	 * unescaped quote in between.
	 *
	 * Used to decide whether an angle bracket inside the display name is
	 * legitimate (quoted, so inert) or a second angle-addr smuggled into an
	 * interpolated header. See parse_one().
	 *
	 * @param string $name Trimmed display-name part.
	 * @return bool
	 */
	private static function is_quoted_string( string $name ): bool {
		$length = strlen( $name );
		if ( $length < 2 || $name[0] !== '"' || $name[ $length - 1 ] !== '"' ) {
			return false;
		}
		// Walk the interior; an unescaped quote would close the string early
		// and leave the rest outside it, which is not a single quoted-string.
		for ( $i = 1; $i < $length - 1; $i++ ) {
			if ( $name[ $i ] === '\\' ) {
				$i++;
				continue;
			}
			if ( $name[ $i ] === '"' ) {
				return false;
			}
		}
		return true;
	}

	private static function unquote( string $name ): string {
		$name   = trim( $name );
		$length = strlen( $name );
		if ( $length >= 2 && $name[0] === '"' && $name[ $length - 1 ] === '"' ) {
			$inner = substr( $name, 1, -1 );
			$inner = preg_replace( '/\\\\(.)/', '$1', $inner );
			return $inner === null ? '' : $inner;
		}
		return $name;
	}

	/**
	 * Quote and backslash-escape a display name for safe reuse inside an
	 * RFC 5322 "Name <addr>" mailbox, when it contains characters (comma,
	 * quote, angle bracket, backslash) that would otherwise change the
	 * meaning of the header.
	 *
	 * @param string $name Display name.
	 * @return string
	 */
	private static function quote_name( string $name ): string {
		if ( preg_match( '/[",<>\\\\]/', $name ) !== 1 ) {
			return $name;
		}
		$escaped = str_replace( array( '\\', '"' ), array( '\\\\', '\\"' ), $name );
		return '"' . $escaped . '"';
	}

	/**
	 * Strip CR/LF from a value before it is used as (part of) a header value.
	 * This is header-injection hardening for values that ultimately originate from
	 * wp_mail() arguments a form plugin may pass through from a site visitor.
	 *
	 * @param string $value Value to clean.
	 * @return string
	 */
	private static function strip_crlf( string $value ): string {
		return trim( str_replace( array( "\r", "\n" ), '', $value ) );
	}

	/**
	 * Redact any RFC 5322-shaped email address embedded inside a free-text
	 * string, replacing each match with a fixed placeholder and leaving the
	 * surrounding text untouched.
	 *
	 * GH #381 phases 1 and 4 (security-reviewer finding): a real
	 * PHPMailer/SMTP failure message routinely embeds the recipient address
	 * verbatim -- e.g. "SMTP Error: The following recipients failed:
	 * a@x.com: 550 5.1.1 User unknown" -- so emptying the `to` column alone
	 * does not stop the address leaving the site through the `error` (or
	 * `response`) column when log_emails is off. A blanket empty-string on
	 * the whole error would also destroy the one part an operator actually
	 * needs to diagnose the failure (the "550 5.1.1 User unknown" part), so
	 * this removes only the address-shaped substring and keeps everything
	 * else.
	 *
	 * Deliberately not anchored to a specific known `to` list: a provider
	 * response can echo back a re-cased/normalised address, or one that was
	 * never in the `to` array (a bounce target, a Cc/Bcc), so this scans for
	 * anything address-shaped rather than trusting a caller-supplied
	 * allow-list of addresses to redact.
	 *
	 * @param string $text Free-text error or provider-response string.
	 * @return string
	 */
	public static function redact_email_addresses( string $text ): string {
		if ( $text === '' ) {
			return $text;
		}
		$redacted = preg_replace( '/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/', '[address removed]', $text );
		return $redacted === null ? $text : $redacted;
	}
}
