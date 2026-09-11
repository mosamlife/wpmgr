<?php
/**
 * FileChmodCommand: set permissions on a file or directory within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_chmod
 *   Authorization: Bearer <Ed25519 JWT cmd="file_chmod">
 *   Body: {
 *     "path": <site-relative path, forward slashes>,
 *     "mode": <octal string e.g. "0644">
 *   }
 *
 * Response (200 OK):
 *   { "path": <string>, "mode": <string octal> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, mode_denied, base_unresolved, write_failed.
 *
 * Mode allowlist:
 *   Files (non-directory): 0600, 0640, 0644
 *   Directories:           0700, 0750, 0755
 *
 *   Denied categories:
 *     - Any mode with setuid (04000), setgid (02000), or sticky (01000) bits.
 *     - Any mode with world-write (0o002) bit: 0666, 0777, etc.
 *     - Any mode that falls outside the above allowlist.
 *
 * The Go CP must validate the mode before dispatching the command and mirror
 * this allowlist. The agent is the authoritative enforcement point.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Changes file/directory permissions within the agent file jail.
 */
final class FileChmodCommand implements CommandInterface {

	/**
	 * Permitted modes for regular files (non-directory).
	 * No setuid/setgid/sticky; no world-write.
	 *
	 * @var list<int>
	 */
	private const ALLOWED_FILE_MODES = [ 0600, 0640, 0644 ];

	/**
	 * Permitted modes for directories.
	 * No setuid/setgid/sticky; no world-write.
	 *
	 * @var list<int>
	 */
	private const ALLOWED_DIR_MODES = [ 0700, 0750, 0755 ];

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_chmod';
	}

	/**
	 * Effect: changes a file's permission bits from a safe-mode allowlist. No file content changes.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect {
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: setting the same mode twice converges.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability {
		return CommandRepeatability::Idempotent;
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params Decoded JSON body from the CP.
	 * @return array<string,mixed>
	 */
	public function execute( array $claims, array $params ): array {
		// ------------------------------------------------------------------
		// 1. Extract and validate parameters.
		// ------------------------------------------------------------------
		if ( ! array_key_exists( 'path', $params ) || ! is_string( $params['path'] ) || $params['path'] === '' ) {
			return $this->error( 'invalid_path', 'path is required' );
		}
		if ( ! array_key_exists( 'mode', $params ) || ! is_string( $params['mode'] ) || $params['mode'] === '' ) {
			return $this->error( 'invalid_path', 'mode is required (octal string, e.g. "0644")' );
		}

		$relPath = str_replace( '\\', '/', (string) $params['path'] );
		$modeStr = trim( (string) $params['mode'] );

		// Parse the mode string as octal. Accept '644' or '0644' both as valid.
		$modeInt = self::parseOctalMode( $modeStr );
		if ( $modeInt === null ) {
			return $this->error( 'mode_denied', 'mode must be a valid octal string (e.g. "0644", "0755")' );
		}

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3 guard).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the path.
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( $jailResult['code'], $jailResult['message'] );
		}
		$absPath     = $jailResult['abs'];
		$resolvedRel = $jailResult['rel'];

		// ------------------------------------------------------------------
		// 4. Path must exist.
		// ------------------------------------------------------------------
		if ( ! file_exists( $absPath ) && ! is_link( $absPath ) ) {
			return $this->error( 'not_found', 'path not found: ' . $resolvedRel );
		}

		// ------------------------------------------------------------------
		// 5. Validate mode against the allowlist.
		//    Separate allowlists for files and directories.
		// ------------------------------------------------------------------
		$isDir   = is_dir( $absPath );
		$allowed = $isDir ? self::ALLOWED_DIR_MODES : self::ALLOWED_FILE_MODES;

		if ( ! in_array( $modeInt, $allowed, true ) ) {
			$allowedHex = array_map( static fn( int $m ): string => sprintf( '0%o', $m ), $allowed );
			return $this->error(
				'mode_denied',
				'mode ' . $modeStr . ' is not in the safe allowlist for ' . ( $isDir ? 'directories' : 'files' )
				. '; allowed: ' . implode( ', ', $allowedHex )
			);
		}

		// ------------------------------------------------------------------
		// 6. Apply the chmod.
		// ------------------------------------------------------------------
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- headless agent; WP_Filesystem never initialized; chmod is applied to a validated path within an allowlist
		if ( ! @chmod( $absPath, $modeInt ) ) {
			return $this->error( 'write_failed', 'chmod failed' );
		}

		// Re-read the mode from lstat so the response reflects what the OS set.
		$lstat       = @lstat( $absPath );
		$actualMode  = $lstat !== false ? (int) ( $lstat['mode'] ?? $modeInt ) : $modeInt;

		return [
			'path' => $resolvedRel,
			'mode' => sprintf( '%04o', $actualMode & 07777 ),
		];
	}

	// ------------------------------------------------------------------
	// Helpers
	// ------------------------------------------------------------------

	/**
	 * Parse a mode string as an octal integer.
	 * Accepts '644', '0644', '0o644' (Python-style), '755', '0755', etc.
	 *
	 * @param string $modeStr Mode string from the request body.
	 * @return int|null Parsed integer, or null on failure.
	 */
	private static function parseOctalMode( string $modeStr ): ?int {
		// Strip leading '0o' (Python/Go octal literal prefix).
		$s = ltrim( $modeStr, '0' );
		if ( $s === '' ) {
			$s = '0';
		}
		// Reconstruct with a leading 0 so octdec() / intval() recognise it.
		$octalStr = '0' . $s;

		if ( ! ctype_digit( str_replace( 'o', '', strtolower( $modeStr ) ) ) ) {
			// Contains non-digit characters other than the allowed 'o' prefix: reject.
			// Allow only characters 0-7.
			if ( ! preg_match( '/^0?[0-7]{1,4}$/', $modeStr ) ) {
				return null;
			}
		}

		if ( ! preg_match( '/^0?[0-7]{1,4}$/', $modeStr ) ) {
			return null;
		}

		// Use octdec on the digits portion to get the integer value.
		$digits = ltrim( $modeStr, '0' );
		if ( $digits === '' ) {
			$digits = '0';
		}
		$value = octdec( $digits );
		if ( ! is_int( $value ) && ! is_float( $value ) ) {
			return null;
		}

		return (int) $value;
	}

	// ------------------------------------------------------------------
	// Response helpers
	// ------------------------------------------------------------------

	/**
	 * @param string $code    Structured error code.
	 * @param string $message Human-readable message (no absolute host paths).
	 * @return array{error:array{code:string,message:string}}
	 */
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
