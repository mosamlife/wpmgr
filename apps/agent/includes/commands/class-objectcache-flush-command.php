<?php
/**
 * ObjectcacheFlushCommand — flushes the Redis cache with the strategy selected
 * per the plan (FLUSHDB on confirmed dedicated DB, SCAN+MATCH+UNLINK on shared).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/objectcache.flush
 *   Authorization: Bearer <Ed25519 JWT, cmd="objectcache.flush", aud=<siteId>>
 *   Body: ObjectCacheFlushRequest { scope: "all"|"site"|"group", group?: string, reason?: string }
 *
 * Response: ObjectCacheFlushResult { ok, detail, strategy, keys_deleted }
 *
 * FLUSHALL is never issued. The strategy used is reported in the response.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\ObjectCache\ObjectCacheConfig;
use WPMgr\Agent\ObjectCache\RedisConnection;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Flushes the Redis object cache.
 */
final class ObjectcacheFlushCommand implements CommandInterface
{
	/**
	 * {@inheritDoc}
	 */
	public function name(): string
	{
		return 'objectcache.flush';
	}

	/**
	 * Effect: empties the object cache. Cache content is derived and the site rebuilds it. An earlier revision
	 * of this comment warned that FLUSHDB can clear keys another application put in the same Redis database;
	 * re-reading the code, that is already guarded -- $useFlushDb is ( $strategy === 'flushdb' || $strategy
	 * === 'auto' ) && ! $shared, and $shared defaults to TRUE, so a database not explicitly declared exclusive
	 * gets a prefix-scoped SCAN + UNLINK instead. The indiscriminate path is reachable only when an operator
	 * has declared the database is not shared.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect
	{
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: flushing an already-empty cache is a no-op.
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
	 * @param array<string,mixed> $params ObjectCacheFlushRequest fields.
	 * @return array{ok:bool,detail:string,strategy:string,keys_deleted:int}
	 */
	public function execute( array $claims, array $params ): array
	{
		$scope  = isset( $params['scope'] ) && is_string( $params['scope'] )
			? sanitize_key( $params['scope'] ) : 'all';

		// M15: reject scope 'site' — flushBlog does not exist yet; a flush button
		// must not silently nuke the entire network keyspace.
		if ( $scope === 'site' ) {
			return [
				'ok'           => false,
				'detail'       => "scope 'site' is not supported; use 'all' or 'group'",
				'strategy'     => '',
				'keys_deleted' => 0,
			];
		}

		if ( ! in_array( $scope, [ 'all', 'group' ], true ) ) {
			$scope = 'all';
		}

		$group = isset( $params['group'] ) && is_string( $params['group'] )
			? sanitize_text_field( $params['group'] ) : '';

		try {
			$loader = new ObjectCacheConfig();
			$config = $loader->load();

			if ( $config === [] ) {
				return [
					'ok'          => false,
					'detail'      => 'no config stored',
					'strategy'    => '',
					'keys_deleted' => 0,
				];
			}

			$conn   = new RedisConnection( $config );
			$redis  = $conn->acquire();
			$prefix = isset( $config['prefix'] ) && is_string( $config['prefix'] )
				? (string) $config['prefix'] : 'wpmgr';
			$shared = ! isset( $config['shared'] ) || (bool) $config['shared'];
			$async  = isset( $config['async_flush'] ) && (bool) $config['async_flush'];
			$strategy = isset( $config['flush_strategy'] ) && is_string( $config['flush_strategy'] )
				? (string) $config['flush_strategy'] : 'auto';

			$usedStrategy  = 'scan';
			$keysDeleted   = 0;

			if ( $scope === 'group' ) {
				if ( $group === '' ) {
					$conn->close();
					return [
						'ok'          => false,
						'detail'      => 'group scope requires a group name',
						'strategy'    => '',
						'keys_deleted' => 0,
					];
				}
				$keysDeleted = $this->scanFlush( $redis, $prefix . ':*:' . $group . ':*', $async );
				$keysDeleted += $this->scanFlush( $redis, $prefix . ':' . $group . ':*', $async );
				$usedStrategy = 'scan';

			} elseif ( ( $strategy === 'flushdb' || $strategy === 'auto' ) && ! $shared ) {
				$redis->flushDB( $async );
				$usedStrategy = 'flushdb';

			} else {
				// Shared or scan: SCAN+MATCH+UNLINK prefix-scoped.
				$keysDeleted = $this->scanFlush( $redis, $prefix . ':*', $async );
				$usedStrategy = 'scan';
			}

			$conn->close();

			return [
				'ok'          => true,
				'detail'      => 'flushed',
				'strategy'    => $usedStrategy,
				'keys_deleted' => $keysDeleted,
			];

		} catch ( \Throwable $e ) {
			return [
				'ok'          => false,
				'detail'      => 'objectcache.flush failed: ' . get_class( $e ),
				'strategy'    => '',
				'keys_deleted' => 0,
			];
		}
	}

	/**
	 * SCAN+MATCH+UNLINK flush for a given key pattern.
	 * Returns the approximate number of keys deleted.
	 *
	 * Uses the canonical phpredis SCAN idiom: by-ref integer iterator,
	 * SCAN_RETRY option so phpredis handles empty-batch re-scanning internally,
	 * and a flat key array return (not the [cursor, keys] tuple used by Predis).
	 *
	 * @param \Redis $redis   Active phpredis handle.
	 * @param string $pattern Key pattern (e.g. "prefix:*").
	 * @param bool   $async   Use UNLINK (async) vs DEL.
	 * @return int Approximate keys deleted.
	 */
	private function scanFlush( \Redis $redis, string $pattern, bool $async ): int
	{
		$redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
		$it    = null;
		$total = 0;

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem; by-ref iterator is the canonical phpredis pattern
		while ( ( $keys = $redis->scan( $it, $pattern, 500 ) ) !== false ) {
			if ( ! empty( $keys ) ) {
				$total += count( $keys );
				if ( $async ) {
					$redis->unlink( ...$keys );
				} else {
					$redis->del( ...$keys );
				}
				usleep( 500 ); // 0.5ms inter-batch sleep.
			}
			if ( $it === 0 ) {
				break;
			}
		}

		return $total;
	}
}
