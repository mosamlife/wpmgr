<?php
// File: class-file-version-restore-command.php — FileVersionRestoreCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\StoragePaths;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileVersionRestoreCommand: restore a staged backup version over a jailed file.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_version_restore
 *   Authorization: Bearer <Ed25519 JWT cmd="file_version_restore">
 *   Body: {
 *     "path":              <site-relative path, forward slashes>,
 *     "version_id":        <opaque version identifier from file_versions_list>,
 *     "confirm_sensitive": <bool — default false; CP owner-gates this>
 *   }
 *
 * Response (200 OK):
 *   { "path": <string>, "size": <int>, "mtime": <int> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, no_such_version,
 *          write_failed, base_unresolved, sensitive_denied, bad_backup.
 *
 * F3: Restoring a sensitive file (wp-config.php, .env, key files, etc.) requires
 * confirm_sensitive=true (CP owner-gates it). This prevents a read/write-role
 * operator from silently downgrading credentials by restoring a stale backup.
 *
 * SECURITY:
 *   - version_id is validated to belong to the exact requested path:
 *     we derive the backup dir for the path and check that the .bak file exists
 *     there. A version_id for a different path cannot be used to restore this path
 *     because the backup dir derivation is path-bound.
 *   - The restore is atomic: the current file content is FIRST backed up as a new
 *     version (making the restore itself reversible), then the staged .bak is
 *     copied to a temp file in the same directory and rename()'d atomically over
 *     the target.
 *   - The restored path is re-jailed (realpath + strncmp) after staging the backup.
 *   - Symlinks at the destination are rejected (same guard as file_write).
 *   - The parent directory is re-jailed before rename (TOCTOU guard).
 *   - Only Keystore AES-256-GCM envelopes are accepted (NEW-1): a .bak that does
 *     not base64-decode to ≥ 28 bytes or whose GCM tag fails is rejected with
 *     bad_backup. No plaintext fallback exists on 0.60+ installs.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileVersionRestoreCommand implements CommandInterface {

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_version_restore';
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
		// 1. Validate parameters.
		// ------------------------------------------------------------------
		if ( ! array_key_exists( 'path', $params ) || ! is_string( $params['path'] ) || $params['path'] === '' ) {
			return $this->error( 'invalid_path', 'path is required' );
		}

		if ( ! array_key_exists( 'version_id', $params ) || ! is_string( $params['version_id'] ) || $params['version_id'] === '' ) {
			return $this->error( 'invalid_path', 'version_id is required' );
		}

		$relPath          = str_replace( '\\', '/', (string) $params['path'] );
		$versionId        = sanitize_file_name( (string) $params['version_id'] );
		$confirmSensitive = ! empty( $params['confirm_sensitive'] );

		// version_id must be a plain filename (no slashes, no .., no NUL, must end in .bak).
		if ( $versionId === '' || strpos( $versionId, '/' ) !== false || strpos( $versionId, "\0" ) !== false ) {
			return $this->error( 'invalid_path', 'version_id is invalid' );
		}

		if ( ! str_ends_with( $versionId, '.bak' ) ) {
			return $this->error( 'no_such_version', 'version_id is not a valid version identifier' );
		}

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3 guard: must be non-empty before any write).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the target path.
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( (string) $jailResult['code'], (string) $jailResult['message'] );
		}

		$absPath     = (string) $jailResult['abs'];
		$resolvedRel = (string) $jailResult['rel'];

		if ( is_dir( $absPath ) ) {
			return $this->error( 'invalid_path', 'path is a directory, not a file' );
		}

		// ------------------------------------------------------------------
		// F3: Sensitive-file gate. Restoring a backup of a sensitive file
		// (wp-config.php, .env, key files, etc.) requires confirm_sensitive=true
		// from the CP. Without this gate, a write-role operator could silently
		// downgrade credentials by restoring a stale backup without owner approval.
		// ------------------------------------------------------------------
		if ( ! $confirmSensitive && FileReadCommand::isSensitive( $resolvedRel, basename( $resolvedRel ) ) ) {
			return $this->error(
				'sensitive_denied',
				'restoring a backup of a sensitive file requires confirm_sensitive=true (owner permission required)'
			);
		}

		// ------------------------------------------------------------------
		// 4. Resolve the backup file and validate it belongs to this path.
		// ------------------------------------------------------------------
		$stagingBase = StoragePaths::dataBase( 'file-backups' );
		if ( $stagingBase === '' ) {
			return $this->error( 'no_such_version', 'staging area is not available' );
		}

		$backupDir  = FileVersionsListCommand::backupDirForPath( $stagingBase, $resolvedRel );
		$backupFile = $backupDir . '/' . $versionId;

		// Validate containment: backupFile must be inside backupDir.
		$realBackupDir = realpath( $backupDir );
		if ( $realBackupDir === false ) {
			return $this->error( 'no_such_version', 'version not found' );
		}

		$realBackupFile = $realBackupDir . '/' . $versionId;

		// Double-check: the resolved path must be inside the per-file backup dir.
		if ( strncmp( $realBackupFile, $realBackupDir . '/', strlen( $realBackupDir ) + 1 ) !== 0 ) {
			return $this->error( 'no_such_version', 'version_id resolves outside the per-file backup directory' );
		}

		if ( ! file_exists( $realBackupFile ) || ! is_file( $realBackupFile ) || is_link( $realBackupFile ) ) {
			return $this->error( 'no_such_version', 'version not found: ' . $versionId );
		}

		// ------------------------------------------------------------------
		// 5. Pre-restore backup: stage the CURRENT content as a new version.
		//    This makes the restore itself reversible (restore-of-restore works).
		//    We call the same stageBackup logic used by FileWriteCommand.
		// ------------------------------------------------------------------
		if ( file_exists( $absPath ) && ! is_dir( $absPath ) && ! is_link( $absPath ) ) {
			$this->stageCurrentFile( $absPath, $resolvedRel, $stagingBase );
		}

		// ------------------------------------------------------------------
		// 6. Atomic restore: decrypt backup → write to temp file → rename atomically.
		//
		// NEW-1: Strict envelope enforcement — only AES-256-GCM Keystore envelopes
		// are accepted. The plaintext fallback that existed in earlier code has been
		// removed. On a fresh 0.60+ install no pre-encryption .bak file can
		// legitimately exist (every .bak is written encrypted by stageBackup /
		// stageCurrentFile), so the fallback was dead weight that could have allowed
		// a planted plaintext .bak to be restored.
		//
		// Implementation: the Keystore envelope is base64(iv[12]||tag[16]||ciphertext),
		// so the minimum raw length after base64-decode is 28 bytes. Any .bak that
		// does not base64-decode to ≥ 28 bytes is structurally not an envelope and is
		// rejected before attempting decryption. A valid-looking envelope whose GCM
		// tag does not verify is also rejected (Keystore::decrypt() throws). In both
		// cases the error code is 'bad_backup' so the CP can surface a clear message.
		//
		// Trade-off: pre-0.60 plaintext .bak files are not restorable via this
		// command. This is acceptable: .bak files are transient version history (not
		// long-term archives), and a fresh 0.60+ install never has any plaintext .bak.
		// ------------------------------------------------------------------

		// Re-verify the target is not a symlink before write.
		if ( is_link( $absPath ) ) {
			return $this->error( 'outside_root', 'write denied: destination is a symbolic link' );
		}

		// Ensure parent directory is still inside the jail (TOCTOU guard).
		$parentDir  = dirname( $absPath );
		$parentReal = realpath( $parentDir );
		if ( $parentReal === false
			|| ( strncmp( str_replace( '\\', '/', $parentReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $parentReal ) !== $jailRoot )
		) {
			return $this->error( 'outside_root', 'write denied: parent directory is outside the jail root' );
		}

		// Read the backup file — it must be a Keystore AES-256-GCM envelope (NEW-1).
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_get_contents -- headless agent; WP_Filesystem never initialized; reading encrypted backup file for restore
		$backupBytes = @file_get_contents( $realBackupFile );
		if ( $backupBytes === false ) {
			return $this->error( 'write_failed', 'could not read backup file for restore' );
		}

		// NEW-1: Structural pre-check — base64-decode and verify minimum envelope length
		// (iv[12] + tag[16] = 28 bytes minimum) before handing to Keystore::decrypt().
		// This rejects plaintext files and truncated data with a clear error before
		// any decryption attempt.
		$rawCheck = base64_decode( $backupBytes, true );
		if ( $rawCheck === false || strlen( $rawCheck ) < 28 ) {
			return $this->error( 'bad_backup', 'backup file is not a valid encrypted envelope; only encrypted backups are restorable' );
		}

		// Decrypt the Keystore envelope. Throws on GCM tag mismatch (tampered / wrong key).
		try {
			$keystore       = new Keystore();
			$restoreContent = $keystore->decrypt( $backupBytes );
		} catch ( \Throwable $e ) {
			return $this->error( 'bad_backup', 'backup decryption failed; the file may be tampered or the key has changed' );
		}

		$tmpPath = $parentReal . '/.wpmgr-tmp-' . bin2hex( random_bytes( 6 ) );

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; writing decrypted backup content to temp file for atomic restore
		if ( @file_put_contents( $tmpPath, $restoreContent, LOCK_EX ) === false ) {
			return $this->error( 'write_failed', 'could not stage backup for restore' );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- WP_Filesystem::move() is non-atomic (copy+delete); native rename() is the only atomic option on POSIX; headless agent never initialises WP_Filesystem
		if ( ! @rename( $tmpPath, $absPath ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup of temp file on rename failure
			@unlink( $tmpPath );
			return $this->error( 'write_failed', 'atomic rename failed during restore' );
		}

		// ------------------------------------------------------------------
		// 7. Build response.
		// ------------------------------------------------------------------
		$lstat = @lstat( $absPath );
		$size  = $lstat !== false ? (int) ( $lstat['size'] ?? 0 ) : 0;
		$mtime = $lstat !== false ? (int) ( $lstat['mtime'] ?? 0 ) : 0;

		return [
			'path'  => $resolvedRel,
			'size'  => $size,
			'mtime' => $mtime,
		];
	}

	// ------------------------------------------------------------------
	// Pre-restore backup helper
	// ------------------------------------------------------------------

	/**
	 * Copy the current file to the staging area before overwriting it,
	 * AES-256-GCM-encrypting the backup content at rest (F4).
	 *
	 * This mirrors FileWriteCommand::stageBackup() exactly so that file_versions_list
	 * can see the pre-restore version alongside the pre-write versions.
	 *
	 * Errors are silenced — failure here does NOT block the restore.
	 *
	 * @param string $absPath     Absolute path to the current target file.
	 * @param string $resolvedRel Site-relative path of the target.
	 * @param string $stagingBase Absolute staging base directory.
	 * @return void
	 */
	private function stageCurrentFile( string $absPath, string $resolvedRel, string $stagingBase ): void {
		$backupDir = FileVersionsListCommand::backupDirForPath( $stagingBase, $resolvedRel );

		if ( ! is_dir( $backupDir ) ) {
			wp_mkdir_p( $backupDir );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_get_contents -- headless agent; WP_Filesystem never initialized; reading source file for encrypted pre-restore backup
		$plaintext = @file_get_contents( $absPath );
		if ( $plaintext === false ) {
			return; // Unreadable source — skip silently.
		}

		// F4: Encrypt backup with the existing Keystore AES-256-GCM cipher.
		try {
			$keystore   = new Keystore();
			$ciphertext = $keystore->encrypt( $plaintext );
		} catch ( \Throwable $e ) {
			return; // Encryption failed — skip backup rather than writing plaintext.
		}

		$backupFile = $backupDir . '/' . time() . '-' . bin2hex( random_bytes( 4 ) ) . '.bak';
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; writing AES-256-GCM-encrypted pre-restore backup
		@file_put_contents( $backupFile, $ciphertext, LOCK_EX );
	}

	// ------------------------------------------------------------------
	// Response helpers
	// ------------------------------------------------------------------

	/**
	 * @param string $code    Error code.
	 * @param string $message Human-readable message (no absolute host paths).
	 * @return array{error:array{code:string,message:string}}
	 */
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
