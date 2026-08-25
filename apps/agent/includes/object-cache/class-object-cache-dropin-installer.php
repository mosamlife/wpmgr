<?php
/**
 * ObjectCacheDropinInstaller — manages the object-cache.php drop-in lifecycle
 * in wp-content.
 *
 * Installs the self-contained generated artifact
 * (assets/wpmgr-object-cache-dropin.php) as wp-content/object-cache.php.
 * The generated file inlines all engine classes so no path resolution can
 * fail after installation.
 *
 * Security constraints:
 *   - Foreign object-cache.php (not ours) is never overwritten without force.
 *   - Writability is proven by a real temp-file write probe before any action.
 *   - DISALLOW_FILE_MODS is honored.
 *
 * @package WPMgr\Agent\ObjectCache
 */

declare(strict_types=1);

namespace WPMgr\Agent\ObjectCache;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Installs and removes the object-cache.php drop-in.
 */
final class ObjectCacheDropinInstaller
{
	/** WordPress canonical object-cache drop-in filename. */
	public const CANONICAL = 'object-cache.php';

	/** Signature line proving a drop-in on disk is ours. */
	public const SIGNATURE = 'WPMgr Object Cache drop-in';

	/** Version header string prefix in the artifact. */
	public const VERSION_PREFIX = 'Version: ';

	/** Drop-in state: ours and current. */
	public const STATE_OURS_CURRENT = 'ours-current';

	/** Drop-in state: ours but outdated. */
	public const STATE_OURS_OUTDATED = 'ours-outdated';

	/** Drop-in state: another plugin owns this file. */
	public const STATE_FOREIGN = 'foreign';

	/** Drop-in state: file absent. */
	public const STATE_MISSING = 'missing';

	/** Absolute path to the wp-content directory. */
	private string $contentDir;

	/** Absolute path to the generated drop-in artifact. */
	private string $stubPath;

	/**
	 * @param string|null $contentDir wp-content path override (for tests).
	 * @param string|null $stubPath   Generated artifact path override (for tests).
	 */
	public function __construct( ?string $contentDir = null, ?string $stubPath = null )
	{
		if ( $contentDir !== null ) {
			$this->contentDir = rtrim( $contentDir, '/\\' );
		} elseif ( defined( 'WP_CONTENT_DIR' ) ) {
			$this->contentDir = rtrim( (string) constant( 'WP_CONTENT_DIR' ), '/\\' );
		} else {
			$this->contentDir = '';
		}

		if ( $stubPath !== null ) {
			$this->stubPath = $stubPath;
		} elseif ( defined( 'WPMGR_AGENT_DIR' ) ) {
			$this->stubPath = rtrim( (string) constant( 'WPMGR_AGENT_DIR' ), '/\\' )
				. '/assets/wpmgr-object-cache-dropin.php';
		} else {
			$this->stubPath = '';
		}
	}

	/**
	 * Absolute path where the drop-in would be installed.
	 *
	 * @return string
	 */
	public function dropinPath(): string
	{
		return $this->contentDir !== '' ? $this->contentDir . '/' . self::CANONICAL : '';
	}

	/**
	 * Inspect the current state of the drop-in.
	 *
	 * @return string One of the STATE_* constants.
	 */
	public function state(): string
	{
		$path = $this->dropinPath();
		if ( $path === '' || ! @is_file( $path ) ) {
			return self::STATE_MISSING;
		}
		$content = @file_get_contents( $path );
		if ( $content === false ) {
			return self::STATE_FOREIGN;
		}
		if ( strpos( $content, self::SIGNATURE ) === false ) {
			return self::STATE_FOREIGN;
		}
		$installedVersion = $this->extractVersion( $content );
		$artifactVersion  = $this->artifactVersion();
		if ( $artifactVersion !== '' && $installedVersion !== '' && $installedVersion !== $artifactVersion ) {
			return self::STATE_OURS_OUTDATED;
		}
		return self::STATE_OURS_CURRENT;
	}

	/**
	 * Whether the drop-in is installed and ours (current or outdated).
	 *
	 * @return bool
	 */
	public function isInstalled(): bool
	{
		$s = $this->state();
		return $s === self::STATE_OURS_CURRENT || $s === self::STATE_OURS_OUTDATED;
	}

	/**
	 * Whether wp-content is writable (via a real temp-file probe).
	 *
	 * @return bool
	 */
	public function isWritable(): bool
	{
		if ( $this->contentDir === '' ) {
			return false;
		}
		if ( defined( 'DISALLOW_FILE_MODS' ) && constant( 'DISALLOW_FILE_MODS' ) ) {
			return false;
		}
		$tmp = $this->contentDir . '/.wpmgr_oc_probe_' . wp_rand( 100000, 999999 );
		$ok  = @file_put_contents( $tmp, '1' ) !== false;
		if ( $ok ) {
			@unlink( $tmp ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- probe temp file; not an attachment
		}
		return $ok;
	}

	/**
	 * Install the object-cache.php drop-in. Idempotent.
	 *
	 * Copies the self-contained generated artifact directly; no stamping.
	 * Refuses to overwrite a foreign drop-in unless $force is true.
	 *
	 * @param bool $force Overwrite a foreign drop-in.
	 * @return array{ok:bool,detail:string,foreign_dropin:bool,opcache_invalidate_ok:bool}
	 */
	public function install( bool $force = false ): array
	{
		$defaultResult = [
			'ok'                   => false,
			'detail'               => '',
			'foreign_dropin'       => false,
			'opcache_invalidate_ok' => false,
		];

		$path = $this->dropinPath();
		if ( $path === '' ) {
			$defaultResult['detail'] = 'wp-content path unavailable';
			return $defaultResult;
		}

		if ( $this->stubPath === '' || ! @is_file( $this->stubPath ) ) {
			$defaultResult['detail'] = 'drop-in artifact not found';
			return $defaultResult;
		}

		if ( defined( 'DISALLOW_FILE_MODS' ) && constant( 'DISALLOW_FILE_MODS' ) ) {
			$defaultResult['detail'] = 'DISALLOW_FILE_MODS is set';
			return $defaultResult;
		}

		// Foreign drop-in check. Treat an unreadable existing file as foreign.
		if ( @is_file( $path ) ) {
			$existing  = @file_get_contents( $path );
			$isForeign = $existing === false
				|| ( strpos( $existing, self::SIGNATURE ) === false && trim( $existing ) !== '' );
			if ( $isForeign && ! $force ) {
				return array_merge( $defaultResult, [
					'detail'        => 'another object-cache drop-in is installed; use force to replace',
					'foreign_dropin' => true,
				] );
			}
		}

		// Writability check.
		if ( ! $this->isWritable() ) {
			$defaultResult['detail'] = 'wp-content is not writable';
			return $defaultResult;
		}

		$artifact = @file_get_contents( $this->stubPath );
		if ( $artifact === false ) {
			$defaultResult['detail'] = 'could not read drop-in artifact';
			return $defaultResult;
		}

		// Idempotent: byte-identical content already installed.
		if ( @is_file( $path ) ) {
			$current = @file_get_contents( $path );
			if ( $current === $artifact ) {
				return array_merge( $defaultResult, [
					'ok'                    => true,
					'detail'                => 'already current',
					'opcache_invalidate_ok' => true,
				] );
			}
		}

		$result = @file_put_contents( $path, $artifact, LOCK_EX );
		if ( $result === false ) {
			$defaultResult['detail'] = 'write failed';
			return $defaultResult;
		}

		// Opcache invalidation for the installed drop-in.
		$invalidateOk = false;
		if ( function_exists( 'opcache_invalidate' ) ) {
			$invalidateOk = opcache_invalidate( $path, true );
		}

		// Append opcache.restrict_api info when detectable.
		$opcacheRestrictApi = '';
		if ( function_exists( 'opcache_get_status' ) && function_exists( 'ini_get' ) ) {
			$restrictApi = ini_get( 'opcache.restrict_api' ); // phpcs:ignore WordPress.PHP.IniSet.Risky -- ini_get is read-only; no value is set
			if ( is_string( $restrictApi ) && $restrictApi !== '' ) {
				$opcacheRestrictApi = $restrictApi;
			}
		}

		$ret = [
			'ok'                    => true,
			'detail'                => 'installed',
			'foreign_dropin'        => false,
			'opcache_invalidate_ok' => $invalidateOk,
		];
		if ( $opcacheRestrictApi !== '' ) {
			$ret['opcache_restrict_api'] = $opcacheRestrictApi;
		}
		return $ret;
	}

	/**
	 * Remove the drop-in if (and only if) it is ours. Idempotent.
	 *
	 * @return bool True when the drop-in is absent or successfully removed.
	 */
	public function uninstall(): bool
	{
		$path = $this->dropinPath();
		if ( $path === '' || ! @is_file( $path ) ) {
			return true;
		}
		$content = @file_get_contents( $path );
		if ( $content === false ) {
			return true;
		}
		if ( strpos( $content, self::SIGNATURE ) === false ) {
			return true; // Foreign: leave it alone.
		}
		wp_delete_file( $path );
		if ( function_exists( 'opcache_invalidate' ) ) {
			opcache_invalidate( $path, true );
		}
		return ! @file_exists( $path );
	}

	/**
	 * Purge transients from the DB after the object cache is enabled, so
	 * they migrate to Redis on next write.
	 *
	 * @return int Number of transient rows deleted.
	 */
	public function purgeTransients(): int
	{
		global $wpdb;
		if ( ! isset( $wpdb ) || ! is_object( $wpdb ) ) {
			return 0;
		}
		/** @var \wpdb $wpdb */
		$count = 0;

		// Identifier defense in depth: $wpdb->options is a trusted core property,
		// but escape it anyway so static analysis can verify the query.
		$optionsTable = esc_sql( $wpdb->options ?? '' );
		if ( $optionsTable !== '' ) {
			// Build the two LIKE patterns through esc_like so special characters
			// in the prefix are safe; the patterns themselves are not user input.
			$likeTransient     = $wpdb->esc_like( '_transient_' ) . '%';
			$likeSiteTransient = $wpdb->esc_like( '_site_transient_' ) . '%';
			// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- no WP API for bulk transient delete; anti-replay / transient purge; caching would defeat the purpose
			$count += (int) $wpdb->query(
				$wpdb->prepare(
					// phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- only the table identifier is interpolated ($wpdb->options, a trusted WP core property, not user input); the LIKE values are bound as %s placeholders
					"DELETE FROM `{$optionsTable}` WHERE option_name LIKE %s OR option_name LIKE %s",
					$likeTransient,
					$likeSiteTransient
				)
			);
		}

		return $count;
	}

	/**
	 * Auto-refresh the drop-in when it is ours-outdated and wp-content is writable.
	 *
	 * Called from the agent's periodic work path (PerfReporter / heartbeat).
	 * Never touches a foreign drop-in.
	 * Returns true when the installed drop-in is now current.
	 *
	 * @return bool True when the installed drop-in is current after this call.
	 */
	public function maybeAutoRefresh(): bool
	{
		$state = $this->state();

		if ( $state === self::STATE_OURS_CURRENT ) {
			return true;
		}

		if ( $state !== self::STATE_OURS_OUTDATED ) {
			return false;
		}

		$result = $this->install();
		return (bool) $result['ok'];
	}

	// -------------------------------------------------------------------------
	// Public helpers (also called from Plugin::maybeInvalidateEngineOpcache)
	// -------------------------------------------------------------------------

	/**
	 * Opcache-invalidate the installed drop-in and the in-plugin generated
	 * artifact. Called on agent version change and after install.
	 *
	 * In the self-contained model the engine source files inside the plugin are
	 * only meaningful to the build tool, not to the runtime. However, invalidating
	 * them on version change ensures any transient bytecode from the old version
	 * is cleared immediately.
	 *
	 * @return void
	 */
	public function invalidateEngineFiles(): void
	{
		if ( ! function_exists( 'opcache_invalidate' ) ) {
			return;
		}

		// Invalidate the installed drop-in in wp-content.
		$installed = $this->dropinPath();
		if ( $installed !== '' && @is_file( $installed ) ) {
			opcache_invalidate( $installed, true );
		}

		// Invalidate the in-plugin generated artifact.
		if ( $this->stubPath !== '' && @is_file( $this->stubPath ) ) {
			opcache_invalidate( $this->stubPath, true );
		}

		// Invalidate the engine source files (used by build tool; old bytecode
		// is irrelevant at runtime but harmless to clear on version change).
		$this->invalidateEngineSourceFiles();
	}

	// -------------------------------------------------------------------------
	// Private helpers
	// -------------------------------------------------------------------------

	/**
	 * Opcache-invalidate the three engine source files inside the plugin.
	 *
	 * @return void
	 */
	private function invalidateEngineSourceFiles(): void
	{
		if ( ! function_exists( 'opcache_invalidate' ) ) {
			return;
		}

		$engineDir = '';

		if ( $this->stubPath !== '' ) {
			$pluginRoot = dirname( dirname( $this->stubPath ) );
			$candidate  = $pluginRoot . '/includes/object-cache';
			if ( @is_dir( $candidate ) ) {
				$engineDir = $candidate;
			}
		}

		if ( $engineDir === '' && defined( 'WPMGR_AGENT_DIR' ) ) {
			$candidate = rtrim( (string) constant( 'WPMGR_AGENT_DIR' ), '/\\' )
				. '/includes/object-cache';
			if ( @is_dir( $candidate ) ) {
				$engineDir = $candidate;
			}
		}

		if ( $engineDir === '' ) {
			return;
		}

		$files = [
			$engineDir . '/class-object-cache-engine.php',
			$engineDir . '/class-object-cache-config.php',
			$engineDir . '/class-redis-connection.php',
		];

		foreach ( $files as $file ) {
			if ( @is_file( $file ) ) {
				opcache_invalidate( $file, true );
			}
		}
	}

	/**
	 * Extract the Version header value from a drop-in file's content.
	 *
	 * @param string $content File content.
	 * @return string Version string or '' if not found.
	 */
	private function extractVersion( string $content ): string
	{
		$pos = strpos( $content, self::VERSION_PREFIX );
		if ( $pos === false ) {
			return '';
		}
		$start   = $pos + strlen( self::VERSION_PREFIX );
		$end     = strpos( $content, "\n", $start );
		$version = $end !== false ? substr( $content, $start, $end - $start ) : substr( $content, $start );
		return trim( $version );
	}

	/**
	 * Extract the version from the generated artifact (reads first 2048 bytes).
	 *
	 * @return string
	 */
	private function artifactVersion(): string
	{
		if ( $this->stubPath === '' || ! @is_file( $this->stubPath ) ) {
			return '';
		}
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- headless agent; WP_Filesystem not initialized; streaming read of plugin-controlled artifact file only
		$handle = @fopen( $this->stubPath, 'r' );
		if ( $handle === false ) {
			return '';
		}
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- same justification as fopen above
		$header = (string) fread( $handle, 2048 );
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- same justification as fopen above
		fclose( $handle );
		return $this->extractVersion( $header );
	}
}
