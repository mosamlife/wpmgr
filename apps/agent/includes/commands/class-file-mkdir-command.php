<?php
/**
 * FileMkdirCommand: create a directory within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_mkdir
 *   Authorization: Bearer <Ed25519 JWT cmd="file_mkdir">
 *   Body: { "path": <site-relative path, forward slashes> }
 *
 * Response (200 OK):
 *   { "path": <string> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, exists, base_unresolved, write_failed.
 *
 * Security:
 *   - Path jail via FileListCommand::jailPath() (segment checks + realpath containment).
 *   - F3: nearest existing ancestor re-jailed via realpath before wp_mkdir_p()
 *     (jailPath() is lexical for not-yet-existing targets; a pre-existing
 *     in-jail symlink parent must not redirect the mkdir outside the jail).
 *   - T3: jail root resolved or error before any FS write.
 *   - Directory is hardened via StoragePaths::ensureHardenedPath() (.htaccess + index.php guard).
 *   - 'exists' error if the path is already present (any type).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\StoragePaths;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Creates a hardened directory within the agent file jail.
 */
final class FileMkdirCommand implements CommandInterface {

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_mkdir';
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

		$relPath = str_replace( '\\', '/', (string) $params['path'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3: throw-before-write guard).
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
		// 4. Check for pre-existing entry.
		//    file_exists() follows symlinks, so a dangling symlink at the
		//    target slips past it — the is_link() check below catches it.
		// ------------------------------------------------------------------
		if ( file_exists( $absPath ) ) {
			return $this->error( 'exists', 'path already exists: ' . $resolvedRel );
		}

		// ------------------------------------------------------------------
		// 5. F3: Re-jail the nearest existing ancestor before creating.
		//    jailPath() returns a LEXICAL path when the target does not exist
		//    yet, so a pre-existing in-jail symlink in a parent segment would
		//    let wp_mkdir_p() (which follows symlinks) create the directory
		//    outside the jail. Walk up to the deepest ancestor that exists (or
		//    is a dangling symlink) and require its realpath to stay inside
		//    the jail root — the same parent re-jail the other write commands
		//    perform before touching the filesystem.
		// ------------------------------------------------------------------
		if ( is_link( $absPath ) ) {
			return $this->error( 'outside_root', 'mkdir denied: destination is a symbolic link' );
		}

		$ancestor = dirname( $absPath );
		while ( ! file_exists( $ancestor ) && ! is_link( $ancestor ) ) {
			$parent = dirname( $ancestor );
			if ( $parent === $ancestor ) {
				break;
			}
			$ancestor = $parent;
		}

		$ancestorReal = realpath( $ancestor );
		if ( $ancestorReal === false
			|| ( strncmp( str_replace( '\\', '/', $ancestorReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $ancestorReal ) !== $jailRoot )
		) {
			return $this->error( 'outside_root', 'mkdir denied: parent directory resolves outside the jail root' );
		}

		// ------------------------------------------------------------------
		// 6. Create and harden the directory.
		//    StoragePaths::ensureHardenedPath() creates the directory with
		//    wp_mkdir_p() and drops a deny-all .htaccess + index.php guard.
		// ------------------------------------------------------------------
		$result = StoragePaths::ensureHardenedPath( $absPath );
		if ( $result === '' || ! is_dir( $absPath ) ) {
			return $this->error( 'write_failed', 'could not create directory' );
		}

		return [ 'path' => $resolvedRel ];
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
