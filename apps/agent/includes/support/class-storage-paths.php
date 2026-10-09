<?php
/**
 * StoragePaths: shared resolver for user-data storage directories.
 *
 * Returns an uploads-first base path for plugin-generated user data, honoring
 * the relocatable upload_path option and multisite per-site subdirectories.
 * Falls back to WP_CONTENT_DIR only when the uploads directory is unavailable.
 *
 * Convention (wp.org Guideline compliance):
 *   - User-generated data (quarantine, snapshots, local backups) -> uploads/wpmgr-<purpose>
 *   - Page cache, optimizer assets, config drop-ins              -> wp-content/cache/wpmgr
 *     (conventional; matches WP Super Cache / W3TC / WP Rocket; kept as-is)
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Provides the canonical storage-path resolver for user-data directories.
 */
final class StoragePaths {

	/**
	 * Resolve the absolute base path for a named user-data purpose.
	 *
	 * The returned path is uploads-first:
	 *   wp_upload_dir()['basedir'] / wpmgr-<purpose>   (preferred)
	 *   WP_CONTENT_DIR / wpmgr-<purpose>               (fallback)
	 *
	 * Returns '' when neither base is available; callers MUST check for '' and
	 * throw/bail before any write.
	 *
	 * @param string $purpose Lowercase slug describing the data purpose
	 *                        (e.g. 'quarantine', 'snapshots', 'backups').
	 * @return string Absolute path WITHOUT a trailing slash, or '' on failure.
	 */
	public static function dataBase( string $purpose ): string {
		if ( function_exists( 'wp_upload_dir' ) ) {
			$upload = wp_upload_dir();
			if ( is_array( $upload )
				&& isset( $upload['basedir'] )
				&& is_string( $upload['basedir'] )
				&& $upload['basedir'] !== ''
			) {
				return rtrim( $upload['basedir'], '/\\' ) . '/wpmgr-' . $purpose;
			}
		}

		// Fallback: wp-content, only when WordPress reports no uploads base directory
		// (wp_upload_dir() is missing or returns an empty basedir). This method does
		// not test writability: a caller that finds the uploads path unwritable has
		// to handle that itself.
		if ( defined( 'WP_CONTENT_DIR' ) && is_string( WP_CONTENT_DIR ) && WP_CONTENT_DIR !== '' ) {
			return rtrim( WP_CONTENT_DIR, '/\\' ) . '/wpmgr-' . $purpose;
		}

		return '';
	}

	/**
	 * Ensure the user-data directory for $purpose exists and is hardened against
	 * direct web access (deny-all .htaccess + empty index.php guard).
	 *
	 * Resolves the path via dataBase(), creates the directory with wp_mkdir_p()
	 * if it does not yet exist, then drops the guard files on first call. Safe
	 * to call on every request — file_exists() short-circuits the writes.
	 *
	 * This is required when the resolved path is under uploads/, which is
	 * web-accessible. Callers that create the directory themselves may call this
	 * method after creation to apply the same hardening.
	 *
	 * @param string $purpose Lowercase slug (same as passed to dataBase()).
	 * @return string The resolved absolute path, or '' when no base is available.
	 */
	public static function ensureHardened( string $purpose ): string {
		$path = self::dataBase( $purpose );
		if ( $path === '' ) {
			return '';
		}
		return self::ensureHardenedPath( $path );
	}

	/**
	 * Harden a SPECIFIC absolute directory against direct web access (deny-all
	 * .htaccess + empty index.php guard), creating it if needed.
	 *
	 * Use this when the caller has already resolved the directory itself (for
	 * example a destination that may fall back to the legacy wp-content path):
	 * harden the path that is actually written to, not a recomputed one, so the
	 * guard files always land in the real data directory.
	 *
	 * @param string $path Absolute directory path (no trailing slash required).
	 * @return string The path, or '' when $path is empty.
	 */
	public static function ensureHardenedPath( string $path ): string {
		$path = rtrim( $path, '/\\' );
		if ( $path === '' ) {
			return '';
		}

		if ( ! is_dir( $path ) ) {
			if ( function_exists( 'wp_mkdir_p' ) ) {
				wp_mkdir_p( $path );
			} else {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- wp_mkdir_p not yet available in this context; headless agent, WP_Filesystem never initialized
				@mkdir( $path, 0755, true );
			}
		}

		// Deny-all .htaccess — blocks Apache/LiteSpeed.
		$htaccess = $path . '/.htaccess';
		if ( ! file_exists( $htaccess ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; direct write of a static guard file
			@file_put_contents(
				$htaccess,
				"# Block direct web access to WPMgr user-data directories.\n"
				. "<IfModule mod_authz_core.c>\n"
				. "    Require all denied\n"
				. "</IfModule>\n"
				. "<IfModule !mod_authz_core.c>\n"
				. "    Deny from all\n"
				. "</IfModule>\n",
				LOCK_EX
			);
		}

		// Empty index.php — silences directory listing on PHP-proxied servers.
		$index = $path . '/index.php';
		if ( ! file_exists( $index ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; direct write of a static guard file
			@file_put_contents( $index, "<?php\n// Silence is golden.\n", LOCK_EX );
		}

		return $path;
	}

	/**
	 * Legacy path for a purpose (wp-content/wpmgr-<purpose>).
	 *
	 * Used by callers that need to read from the old pre-uploads location so
	 * that existing self-hosted installs are not orphaned after the directory
	 * is relocated under uploads/. Returns '' when WP_CONTENT_DIR is absent.
	 *
	 * @param string $purpose Lowercase slug (same value as passed to dataBase()).
	 * @return string Absolute legacy path without trailing slash, or '' on failure.
	 */
	public static function legacyBase( string $purpose ): string {
		if ( defined( 'WP_CONTENT_DIR' ) && is_string( WP_CONTENT_DIR ) && WP_CONTENT_DIR !== '' ) {
			return rtrim( WP_CONTENT_DIR, '/\\' ) . '/wpmgr-' . $purpose;
		}
		return '';
	}
}
