<?php
/**
 * FileDeleteCommand: delete a file or directory within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_delete
 *   Authorization: Bearer <Ed25519 JWT cmd="file_delete">
 *   Body: {
 *     "path":      <site-relative path, forward slashes>,
 *     "recursive": <bool — default false; must be true to delete a non-empty directory>
 *   }
 *
 * Response (200 OK):
 *   { "path": <string>, "deleted": <bool> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, protected_root,
 *          is_directory (non-empty without recursive flag), base_unresolved, write_failed.
 *
 * Security:
 *   - Path jail via FileListCommand::jailPath().
 *   - T13: Protected-root guard refuses deletion of wp-admin, wp-includes,
 *     WordPress core files, and active theme/plugin roots.
 *   - Non-empty directories require recursive=true; this prevents accidental
 *     bulk-delete without explicit intent.
 *   - T3: jail root resolved before any FS mutation.
 *   - Recursive delete is implemented as a controlled walk that NEVER follows
 *     symlinks (symlinks are unlinked, not recursed into).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Deletes a file or directory within the agent file jail.
 */
final class FileDeleteCommand implements CommandInterface {

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_delete';
	}

	/**
	 * Effect: unlinks a file, or recursively removes a directory tree, with no backup staged. Nothing is
	 * retained.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect {
		return CommandEffect::Destructive;
	}

	/**
	 * Repeatability: the path is fixed, what lives at it is not. A blind retry deletes a file recreated at
	 * that path since the first run, which is destruction the first run did not perform. The first revision
	 * called this idempotent because the end state -- path absent -- converges; that reads the outcome and
	 * ignores the victim.
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
		if ( ! array_key_exists( 'path', $params ) || ! is_string( $params['path'] ) || $params['path'] === '' ) {
			return $this->error( 'invalid_path', 'path is required' );
		}

		$relPath   = str_replace( '\\', '/', (string) $params['path'] );
		$recursive = ! empty( $params['recursive'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3 guard).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no deletion performed' );
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
		// 4. Protected-root guard (T13) — checked BEFORE file-exists so that
		//    attempting to delete a protected path always returns protected_root
		//    regardless of whether the directory actually exists in this sandbox.
		//    This prevents information leakage about whether protected paths exist
		//    and ensures the guard fires even when the test environment does not
		//    have a real wp-admin directory on disk.
		// ------------------------------------------------------------------
		if ( FileGuards::isProtectedRoot( $resolvedRel ) ) {
			return $this->error(
				'protected_root',
				'deletion refused: path is a protected WordPress root; deletion would brick the site'
			);
		}

		// ------------------------------------------------------------------
		// 5. Path must exist.
		// ------------------------------------------------------------------
		if ( ! file_exists( $absPath ) && ! is_link( $absPath ) ) {
			return $this->error( 'not_found', 'path not found: ' . $resolvedRel );
		}

		// ------------------------------------------------------------------
		// 6. Delete the path.
		// ------------------------------------------------------------------
		if ( is_link( $absPath ) ) {
			// Symlinks are always unlinked directly — never recursed.
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; direct unlink is the only option for symlink removal
			if ( ! @unlink( $absPath ) ) {
				return $this->error( 'write_failed', 'could not remove symlink' );
			}
			return [ 'path' => $resolvedRel, 'deleted' => true ];
		}

		if ( is_dir( $absPath ) ) {
			$handle = @opendir( $absPath );
			$isEmpty = true;
			if ( $handle !== false ) {
				while ( true ) {
					$entry = readdir( $handle );
					if ( $entry === false ) {
						break;
					}
					if ( $entry !== '.' && $entry !== '..' ) {
						$isEmpty = false;
						break;
					}
				}
				closedir( $handle );
			}

			if ( ! $isEmpty && ! $recursive ) {
				return $this->error(
					'is_directory',
					'path is a non-empty directory; set recursive=true to delete recursively'
				);
			}

			if ( $recursive ) {
				$ok = $this->deleteRecursive( $absPath, $jailRoot );
				if ( ! $ok ) {
					return $this->error( 'write_failed', 'recursive delete failed (partial deletion may have occurred)' );
				}
			} else {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- headless agent; WP_Filesystem never initialized; rmdir is the only option for empty-dir removal
				if ( ! @rmdir( $absPath ) ) {
					return $this->error( 'write_failed', 'could not remove empty directory' );
				}
			}

			return [ 'path' => $resolvedRel, 'deleted' => true ];
		}

		// Regular file.
		// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- WP_Filesystem is never initialized in the headless agent (prompts for FTP creds); wp_delete_file() wraps unlink() but requires WP_Filesystem for some paths; direct unlink is appropriate here
		if ( ! @unlink( $absPath ) ) {
			return $this->error( 'write_failed', 'could not delete file' );
		}

		return [ 'path' => $resolvedRel, 'deleted' => true ];
	}

	// ------------------------------------------------------------------
	// Recursive delete helper.
	// Never follows symlinks — unlinks them as-is.
	// ------------------------------------------------------------------

	/**
	 * Recursively delete a directory tree. Returns true when fully deleted.
	 *
	 * This is a controlled walk, NOT a glob-then-exec — it uses opendir/readdir
	 * and re-jails each child against $jailRoot as a defence-in-depth measure
	 * (the directory tree itself should be within the jail, but we re-check the
	 * realpath of each child before acting on it).
	 *
	 * Symlinks inside the tree are unlinked, NOT recursed into, so a symlink
	 * pointing outside the jail cannot cause a delete outside the jail.
	 *
	 * @param string $absDir  Absolute path of the directory to delete.
	 * @param string $jailRoot Absolute jail root (no trailing slash).
	 * @return bool True if the directory was fully deleted.
	 */
	private function deleteRecursive( string $absDir, string $jailRoot ): bool {
		$handle = @opendir( $absDir );
		if ( $handle === false ) {
			return false;
		}

		$ok = true;
		while ( true ) {
			$entry = readdir( $handle );
			if ( $entry === false ) {
				break;
			}
			if ( $entry === '.' || $entry === '..' ) {
				continue;
			}

			$child = $absDir . '/' . $entry;

			// Re-verify containment for each child (defence-in-depth).
			$real = realpath( $child );
			if ( $real !== false ) {
				if (
					strncmp( $real, $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
					&& $real !== $jailRoot
				) {
					// Child resolves outside jail — skip it, do NOT delete.
					continue;
				}
			}

			if ( is_link( $child ) ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; direct unlink required for symlink removal in recursive walk
				if ( ! @unlink( $child ) ) {
					$ok = false;
				}
			} elseif ( is_dir( $child ) ) {
				if ( ! $this->deleteRecursive( $child, $jailRoot ) ) {
					$ok = false;
				}
			} else {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; direct unlink required in recursive delete walk
				if ( ! @unlink( $child ) ) {
					$ok = false;
				}
			}
		}
		closedir( $handle );

		if ( $ok ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- headless agent; WP_Filesystem never initialized; rmdir is the only option in a recursive directory delete walk
			if ( ! @rmdir( $absDir ) ) {
				$ok = false;
			}
		}

		return $ok;
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
