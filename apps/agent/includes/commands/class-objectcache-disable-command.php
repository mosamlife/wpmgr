<?php
/**
 * ObjectcacheDisableCommand — removes the object-cache.php drop-in and
 * optionally flushes via a standalone connection.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/objectcache.disable
 *   Authorization: Bearer <Ed25519 JWT, cmd="objectcache.disable", aud=<siteId>>
 *   Body: ObjectCacheDisableRequest { flush: bool }
 *
 * Response: ObjectCacheDisableResult { ok, detail, dropin_removed, flushed }
 *
 * The flush uses a fresh standalone connection (so disable works even when the
 * live cache is broken). The config file is NOT removed (operator may re-enable).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\ObjectCache\ObjectCacheConfig;
use WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller;
use WPMgr\Agent\ObjectCache\RedisConnection;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Removes the object-cache.php drop-in and optionally flushes.
 */
final class ObjectcacheDisableCommand implements CommandInterface
{
	/**
	 * {@inheritDoc}
	 */
	public function name(): string
	{
		return 'objectcache.disable';
	}

	/**
	 * Effect: removes the object-cache drop-in and, by default, flushes first. Both are derived state. The
	 * flush takes the same guarded path as objectcache.flush: FLUSHDB only when the stored config declares the
	 * database is not shared, prefix-scoped SCAN + UNLINK otherwise.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect
	{
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: disabling an already-disabled object cache converges.
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
	 * @param array<string,mixed> $params ObjectCacheDisableRequest fields.
	 * @return array{ok:bool,detail:string,dropin_removed:bool,flushed:bool}
	 */
	public function execute( array $claims, array $params ): array
	{
		$doFlush = ! isset( $params['flush'] ) || (bool) $params['flush'];

		$flushed       = false;
		$dropinRemoved = false;

		try {
			// 1. Flush via a standalone connection BEFORE removing the drop-in.
			if ( $doFlush ) {
				$flushed = $this->standaloneFlush();
			}

			// 2. Remove the drop-in.
			$installer     = new ObjectCacheDropinInstaller();
			$dropinRemoved = $installer->uninstall();

			return [
				'ok'             => true,
				'detail'         => 'disabled',
				'dropin_removed' => $dropinRemoved,
				'flushed'        => $flushed,
			];

		} catch ( \Throwable $e ) {
			return [
				'ok'             => false,
				'detail'         => 'objectcache.disable failed: ' . get_class( $e ),
				'dropin_removed' => $dropinRemoved,
				'flushed'        => $flushed,
			];
		}
	}

	/**
	 * Open a fresh standalone connection and flush the prefix-scoped keyspace.
	 * Does NOT use the live engine; works even when the engine is broken.
	 *
	 * @return bool True when flush succeeded or no config is stored.
	 */
	private function standaloneFlush(): bool
	{
		$loader = new ObjectCacheConfig();
		$config = $loader->load();

		if ( $config === [] ) {
			return true; // No config; nothing to flush.
		}

		try {
			$conn   = new RedisConnection( $config );
			$redis  = $conn->acquire();
			$prefix = isset( $config['prefix'] ) && is_string( $config['prefix'] )
				? (string) $config['prefix'] : 'wpmgr';
			$shared = ! isset( $config['shared'] ) || (bool) $config['shared'];
			$async  = isset( $config['async_flush'] ) && (bool) $config['async_flush'];
			$strategy = isset( $config['flush_strategy'] ) && is_string( $config['flush_strategy'] )
				? (string) $config['flush_strategy'] : 'auto';

			$useFlushDb = ( $strategy === 'flushdb' || $strategy === 'auto' ) && ! $shared;

			if ( $useFlushDb ) {
				$redis->flushDB( $async );
			} else {
				// SCAN+MATCH+UNLINK prefix-scoped using canonical phpredis idiom:
				// by-ref integer iterator, SCAN_RETRY, flat key array return.
				$redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
				$it      = null;
				$pattern = $prefix . ':*';
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem; by-ref iterator is the canonical phpredis pattern
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
			return false; // Best-effort flush.
		}
	}
}
