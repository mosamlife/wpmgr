<?php
/**
 * ObjectcacheEnableCommand — installs the object-cache.php drop-in
 * after verifying the stored config hash matches the tested hash.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/objectcache.enable
 *   Authorization: Bearer <Ed25519 JWT, cmd="objectcache.enable", aud=<siteId>>
 *   Body: ObjectCacheEnableRequest { config_hash: string }
 *
 * Response: ObjectCacheEnableResult {
 *   ok, detail, dropin_installed, foreign_dropin, transients_purged,
 *   opcache_invalidate_ok, active, verify_hint
 * }
 *
 * Handshake gate: the CP only issues this command after a passing objectcache.test
 * result. The agent verifies that the stored config hash matches the hash in the
 * request before proceeding.
 *
 * Phase B verification: after install the enable result includes:
 *   active        => null (unknown — verified by the NEXT heartbeat)
 *   verify_hint   => 'next_heartbeat'
 * This is honest: the current request bootstrapped under the previous (or absent)
 * drop-in; $GLOBALS['wp_object_cache'] in THIS request is not proof. The heartbeat
 * one minute later runs under the newly-installed drop-in and is the real verifier.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\ObjectCache\ObjectCacheConfig;
use WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Installs the object-cache.php drop-in.
 */
final class ObjectcacheEnableCommand implements CommandInterface
{
	/**
	 * {@inheritDoc}
	 */
	public function name(): string
	{
		return 'objectcache.enable';
	}

	/**
	 * Effect: installs the object-cache drop-in, purges transients so they migrate to Redis, and flushes
	 * through the same shared-aware path as objectcache.flush, and is a Write -- turning object caching on
	 * for every request after this one is the whole reason this command exists. All of what it touches is
	 * derived state the site can rebuild; that does not decide the label -- purpose does.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect
	{
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: enabling an already-enabled object cache converges.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability
	{
		return CommandRepeatability::Idempotent;
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims (unused).
	 * @param array<string,mixed> $params ObjectCacheEnableRequest fields.
	 * @return array{ok:bool,detail:string,dropin_installed:bool,foreign_dropin:bool,transients_purged:int,opcache_invalidate_ok:bool,active:null,verify_hint:string,flushed:bool}
	 */
	public function execute( array $claims, array $params ): array
	{
		$defaultResult = [
			'ok'                    => false,
			'detail'                => '',
			'dropin_installed'      => false,
			'foreign_dropin'        => false,
			'transients_purged'     => 0,
			'opcache_invalidate_ok' => false,
			'active'                => null,
			'verify_hint'           => 'next_heartbeat',
			'flushed'               => false,
		];

		try {
			// Validate the config hash.
			$requestedHash = isset( $params['config_hash'] ) && is_string( $params['config_hash'] )
				? sanitize_text_field( $params['config_hash'] ) : '';

			if ( $requestedHash === '' ) {
				$defaultResult['detail'] = 'config_hash is required';
				return $defaultResult;
			}

			$loader = new ObjectCacheConfig();
			$config = $loader->load();

			if ( $config === [] ) {
				$defaultResult['detail'] = 'no config stored; call objectcache.apply_config first';
				return $defaultResult;
			}

			$storedHash = $loader->computeHash( $config );

			// hash_equals: constant-time comparison for config hashes.
			if ( ! hash_equals( $storedHash, $requestedHash ) ) {
				$defaultResult['detail'] = 'config hash mismatch; re-run objectcache.test with the current config';
				return $defaultResult;
			}

			// Install the drop-in (plain copy of the self-contained artifact).
			$installer = new ObjectCacheDropinInstaller();
			$install   = $installer->install();

			if ( $install['foreign_dropin'] ) {
				$defaultResult['detail']         = $install['detail'];
				$defaultResult['foreign_dropin'] = true;
				return $defaultResult;
			}

			if ( ! $install['ok'] ) {
				$defaultResult['detail'] = $install['detail'];
				return $defaultResult;
			}

			// Purge transients so they migrate to Redis.
			$purged = $installer->purgeTransients();

			// M12: flush the Redis prefix-scoped keyspace after install to remove stale data.
			$flushed = false;
			try {
				$flushed = $this->standaloneFlush();
			} catch ( \Throwable $e ) {
				// Best-effort flush; do not block enable.
			}

			// Phase B: active is null (unknown) because THIS request bootstrapped
			// under the old drop-in; $GLOBALS['wp_object_cache'] here is not proof
			// of the new drop-in's activation. The next heartbeat is the verifier.
			return [
				'ok'                    => true,
				'detail'                => $install['detail'] === 'already current' ? 'already current' : 'drop-in installed',
				'dropin_installed'      => true,
				'foreign_dropin'        => false,
				'transients_purged'     => $purged,
				'opcache_invalidate_ok' => (bool) ( $install['opcache_invalidate_ok'] ?? false ),
				'active'                => null,
				'verify_hint'           => 'next_heartbeat',
				'flushed'               => $flushed,
			];

		} catch ( \Throwable $e ) {
			$defaultResult['detail'] = 'objectcache.enable failed: ' . get_class( $e );
			return $defaultResult;
		}
	}

	/**
	 * M12: Open a fresh standalone connection and flush the prefix-scoped keyspace.
	 * Mirrors ObjectcacheDisableCommand::standaloneFlush() but does not remove the drop-in.
	 *
	 * @return bool True when flush succeeded or no config stored.
	 */
	private function standaloneFlush(): bool
	{
		$loader = new ObjectCacheConfig();
		$config = $loader->load();

		if ( $config === [] ) {
			return true;
		}

		try {
			$conn     = new \WPMgr\Agent\ObjectCache\RedisConnection( $config );
			$redis    = $conn->acquire();
			$prefix   = isset( $config['prefix'] ) && is_string( $config['prefix'] ) ? (string) $config['prefix'] : 'wpmgr';
			$shared   = ! isset( $config['shared'] ) || (bool) $config['shared'];
			$async    = isset( $config['async_flush'] ) && (bool) $config['async_flush'];
			$strategy = isset( $config['flush_strategy'] ) && is_string( $config['flush_strategy'] )
				? (string) $config['flush_strategy'] : 'auto';

			$useFlushDb = ( $strategy === 'flushdb' || $strategy === 'auto' ) && ! $shared;

			if ( $useFlushDb ) {
				$redis->flushDB( $async );
			} else {
				$redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
				$it      = null;
				$pattern = $prefix . ':*';
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem
				while ( ( $keys = $redis->scan( $it, $pattern, 500 ) ) !== false ) {
					if ( ! empty( $keys ) ) {
						if ( $async ) {
							$redis->unlink( ...$keys );
						} else {
							$redis->del( ...$keys );
						}
					}
					if ( $it === 0 ) {
						break;
					}
				}
			}

			$conn->close();
			return true;
		} catch ( \Throwable $e ) {
			return false;
		}
	}
}
