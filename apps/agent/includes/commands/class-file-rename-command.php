<?php
/**
 * FileRenameCommand: rename or move a file/directory within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_rename
 *   Authorization: Bearer <Ed25519 JWT cmd="file_rename">
 *   Body: {
 *     "src":                     <site-relative source path, forward slashes>,
 *     "dst":                     <site-relative destination path, forward slashes>,
 *     "confirm_executable_write": <bool — default false>,
 *     "confirm_sensitive":        <bool — default false>
 *   }
 *
 * Response (200 OK):
 *   { "src": <string>, "dst": <string> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, exists, is_directory,
 *          sensitive_denied, executable_write_denied, base_unresolved, write_failed.
 *
 * Security:
 *   - Containment guard (jailPath) applied to BOTH src AND dst independently.
 *   - If the rename makes the destination basename executable or targets a
 *     sensitive path, the same guards as file_write apply.
 *   - T3: jail root resolved before any FS mutation.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Renames / moves a path within the agent file jail.
 */
final class FileRenameCommand implements CommandInterface {

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_rename';
	}

	/**
	 * Effect: moves a path. The destination is refused when it already exists, so nothing is overwritten and
	 * no content is lost.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect {
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: after a successful run the source is gone, so a blind retry either errors or, if
	 * something new has since appeared at the source path, moves the wrong file.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability {
		return CommandRepeatability::Unsafe;
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
		if ( ! array_key_exists( 'src', $params ) || ! is_string( $params['src'] ) || $params['src'] === '' ) {
			return $this->error( 'invalid_path', 'src is required' );
		}
		if ( ! array_key_exists( 'dst', $params ) || ! is_string( $params['dst'] ) || $params['dst'] === '' ) {
			return $this->error( 'invalid_path', 'dst is required' );
		}

		$srcRel = str_replace( '\\', '/', (string) $params['src'] );
		$dstRel = str_replace( '\\', '/', (string) $params['dst'] );

		$confirmExecutableWrite = ! empty( $params['confirm_executable_write'] );
		$confirmSensitive       = ! empty( $params['confirm_sensitive'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3 guard).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail BOTH src AND dst independently.
		// ------------------------------------------------------------------
		$srcJail = FileListCommand::jailPath( $jailRoot, $srcRel );
		if ( ! $srcJail['ok'] ) {
			return $this->error( $srcJail['code'], $srcJail['message'] );
		}
		$srcAbs     = $srcJail['abs'];
		$srcResolved = $srcJail['rel'];

		$dstJail = FileListCommand::jailPath( $jailRoot, $dstRel );
		if ( ! $dstJail['ok'] ) {
			return $this->error( $dstJail['code'], $dstJail['message'] );
		}
		$dstAbs     = $dstJail['abs'];
		$dstResolved = $dstJail['rel'];

		// ------------------------------------------------------------------
		// 4. Source must exist.
		// ------------------------------------------------------------------
		if ( ! file_exists( $srcAbs ) ) {
			return $this->error( 'not_found', 'source path not found: ' . $srcResolved );
		}

		// ------------------------------------------------------------------
		// 5. Destination must not already exist.
		// ------------------------------------------------------------------
		if ( file_exists( $dstAbs ) ) {
			return $this->error( 'exists', 'destination already exists: ' . $dstResolved );
		}

		// ------------------------------------------------------------------
		// 6. Sensitive-file deny on BOTH src and dst.
		//    Renaming a sensitive file out of its guarded name is allowed with
		//    confirm_sensitive; renaming a file TO a sensitive name is also guarded.
		// ------------------------------------------------------------------
		$srcBasename = basename( $srcResolved );
		$dstBasename = basename( $dstResolved );

		if ( ! $confirmSensitive ) {
			if ( FileReadCommand::isSensitive( $srcResolved, $srcBasename ) ) {
				return $this->error( 'sensitive_denied', 'source matches the sensitive-file deny-list; set confirm_sensitive=true to override' );
			}
			if ( FileReadCommand::isSensitive( $dstResolved, $dstBasename ) ) {
				return $this->error( 'sensitive_denied', 'destination matches the sensitive-file deny-list; set confirm_sensitive=true to override' );
			}
		}

		// ------------------------------------------------------------------
		// 7. Executable-write check on the destination basename (T1).
		//    If the rename would make the destination executable (e.g. a.txt→a.php),
		//    apply the same guard as file_write. Content sniff is not applicable
		//    here since we are not writing new content — only checking the name.
		//    We pass an empty content string so the content sniff never fires.
		// ------------------------------------------------------------------
		if ( ! $confirmExecutableWrite && FileGuards::isExecutableWrite( $dstAbs, $dstResolved, '' ) ) {
			return $this->error(
				'executable_write_denied',
				'rename denied: destination extension indicates an executable file; set confirm_executable_write=true to override'
			);
		}

		// ------------------------------------------------------------------
		// 8. Source extension check — prevent silently moving a shell.php.
		// ------------------------------------------------------------------
		if ( ! is_dir( $srcAbs ) && ! $confirmExecutableWrite && FileGuards::isExecutableWrite( $srcAbs, $srcResolved, '' ) ) {
			return $this->error(
				'executable_write_denied',
				'rename denied: source file is an executable type; set confirm_executable_write=true to override'
			);
		}

		// ------------------------------------------------------------------
		// 8b. F5: Source content sniff — if the source content sniffs as PHP
		//     but its name is extension-clean (e.g. a .txt that contained a PHP
		//     short open tag), deny the rename unless confirm_executable_write=true.
		//     We read only a head window (first 8 KiB): PHP open tags always appear
		//     near the start of a PHP file. The full-file sniff was already applied
		//     on the original upload/write; this head scan catches renames of files
		//     that may have bypassed an older, narrower sniff.
		// ------------------------------------------------------------------
		if ( ! is_dir( $srcAbs ) && ! $confirmExecutableWrite ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- head-window source content sniff for rename; WP_Filesystem has no streaming API; headless agent never initializes WP_Filesystem
			$srcFh = @fopen( $srcAbs, 'rb' );
			if ( $srcFh !== false ) {
				// phpcs:disable WordPress.WP.AlternativeFunctions.file_system_operations_fread,WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming source content sniff; WP_Filesystem has no streaming/fread API and is never initialized in the headless agent
				$srcHead = fread( $srcFh, 8192 );
				@fclose( $srcFh );
				// phpcs:enable WordPress.WP.AlternativeFunctions.file_system_operations_fread,WordPress.WP.AlternativeFunctions.file_system_operations_fclose
				if ( is_string( $srcHead ) && FileGuards::sniffsAsPhp( $srcHead ) ) {
					return $this->error(
						'executable_write_denied',
						'rename denied: source file content contains a PHP open tag; set confirm_executable_write=true to override'
					);
				}
			}
		}

		// ------------------------------------------------------------------
		// 9. TOCTOU symlink guard on destination before the final rename (F3).
		//    rename() follows symlinks at the destination and would write outside
		//    the jail if one was planted between jailPath() and here.
		// ------------------------------------------------------------------
		if ( is_link( $dstAbs ) ) {
			return $this->error( 'outside_root', 'rename denied: destination is a symbolic link' );
		}

		// Re-verify the destination's parent directory is still jailed.
		$dstParent     = dirname( $dstAbs );
		$dstParentReal = realpath( $dstParent );
		if ( $dstParentReal === false
			|| ( strncmp( str_replace( '\\', '/', $dstParentReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $dstParentReal ) !== $jailRoot )
		) {
			return $this->error( 'outside_root', 'rename denied: destination parent is outside the jail root' );
		}

		// ------------------------------------------------------------------
		// 10. Atomic rename.
		// ------------------------------------------------------------------
		// phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- WP_Filesystem::move() is non-atomic (copy+delete) and WP_Filesystem is never initialized in the headless agent; native rename() is the only atomic option
		if ( ! @rename( $srcAbs, $dstAbs ) ) {
			return $this->error( 'write_failed', 'rename failed' );
		}

		return [
			'src' => $srcResolved,
			'dst' => $dstResolved,
		];
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
