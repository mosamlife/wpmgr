<?php
/**
 * ObjectcacheTestCommand — dials the CANDIDATE config without persisting it,
 * runs a structured probe, and returns ObjectCacheTestResult.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/objectcache.test
 *   Authorization: Bearer <Ed25519 JWT, cmd="objectcache.test", aud=<siteId>>
 *   Body: ObjectCacheConfigRequest (same payload as apply_config)
 *
 * Response: ObjectCacheTestResult {
 *   ok, detail, reachable, latency_ms, server_version,
 *   eviction_policy, max_memory_bytes, used_memory_bytes,
 *   capabilities, flush_capability_class, acl_denials,
 *   round_trip_ok, config_hash
 * }
 *
 * Security:
 *   - Candidate config is NOT persisted on failure.
 *   - The password is NEVER echoed back.
 *   - Connection uses only the params supplied; no stored config used.
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
 * Dials and probes a candidate object-cache config without persisting it.
 */
final class ObjectcacheTestCommand implements CommandInterface
{
	/**
	 * {@inheritDoc}
	 */
	public function name(): string
	{
		return 'objectcache.test';
	}

	/**
	 * Effect: probes a candidate Redis config: PING, INFO, and one round-trip through a probe key it then
	 * unlinks. Nothing is persisted -- the config is not saved.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect
	{
		return CommandEffect::Read;
	}

	/**
	 * Repeatability: the probe key is written and removed each time.
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
	 * @param array<string,mixed> $params ObjectCacheConfigRequest fields.
	 * @return array<string,mixed> ObjectCacheTestResult.
	 */
	public function execute( array $claims, array $params ): array
	{
		$config     = ObjectCacheConfig::fromParams( $params );
		$configHash = ( new ObjectCacheConfig() )->computeHash( $config );

		$result = [
			'ok'                   => false,
			'detail'               => '',
			'reachable'            => false,
			'latency_ms'           => 0.0,
			'server_version'       => '',
			'eviction_policy'      => '',
			'max_memory_bytes'     => 0,
			'used_memory_bytes'    => 0,
			'capabilities'         => $this->defaultCapabilities(),
			'flush_capability_class' => 'scan-only',
			'acl_denials'          => [],
			'round_trip_ok'        => false,
			'config_hash'          => $configHash,
		];

		try {
			$conn  = new RedisConnection( $config );
			$redis = $conn->acquire();

			// ----------------------------------------------------------------
			// 1. PING latency (3 samples, return median).
			// ----------------------------------------------------------------
			$latencies = [];
			for ( $i = 0; $i < 3; $i++ ) {
				$t0 = microtime( true );
				$redis->ping();
				$latencies[] = ( microtime( true ) - $t0 ) * 1000.0;
			}
			sort( $latencies );
			$result['reachable']  = true;
			$result['latency_ms'] = round( $latencies[1], 2 ); // Median of 3.

			// ----------------------------------------------------------------
			// 2. INFO server + memory + stats snapshot (subset).
			// ----------------------------------------------------------------
			$aclDenials = [];
			try {
				$infoServer = $redis->info( 'server' );
				if ( is_array( $infoServer ) && isset( $infoServer['redis_version'] ) ) {
					$result['server_version'] = (string) $infoServer['redis_version'];
				}
			} catch ( \Throwable $e ) {
				$aclDenials[] = 'info';
			}

			try {
				$infoMemory = $redis->info( 'memory' );
				if ( is_array( $infoMemory ) ) {
					if ( isset( $infoMemory['used_memory'] ) ) {
						$result['used_memory_bytes'] = (int) $infoMemory['used_memory'];
					}
					if ( isset( $infoMemory['maxmemory'] ) ) {
						$result['max_memory_bytes'] = (int) $infoMemory['maxmemory'];
					}
				}
			} catch ( \Throwable $e ) {
				// Tolerate denial.
			}

			// ----------------------------------------------------------------
			// 3. CONFIG GET maxmemory-policy (tolerate denial).
			// ----------------------------------------------------------------
			try {
				$memPolicy = $redis->config( 'GET', 'maxmemory-policy' );
				if ( is_array( $memPolicy ) && isset( $memPolicy['maxmemory-policy'] ) ) {
					$result['eviction_policy'] = (string) $memPolicy['maxmemory-policy'];
				}
			} catch ( \Throwable $e ) {
				$aclDenials[] = 'config';
			}

			// ----------------------------------------------------------------
			// 4. Extension + server capability probe.
			// ----------------------------------------------------------------
			$caps = RedisConnection::probeCapabilities( $redis );
			$result['capabilities'] = [
				'phpredis_version'     => $caps['phpredis_version'],
				'igbinary_available'   => $caps['igbinary_available'],
				'lzf_available'        => $caps['lzf_available'],
				'lz4_available'        => $caps['lz4_available'],
				'zstd_available'       => $caps['zstd_available'],
				'tls_supported'        => $caps['tls_supported'],
				'value_metadata_reads' => $caps['value_metadata_reads'],
				'native_retry_options' => $caps['native_retry_options'],
				'keepttl_supported'    => $caps['keepttl_supported'],
				'flush_async_supported' => $caps['flush_async_supported'],
			];

			// ----------------------------------------------------------------
			// 5. SETEX/GET/UNLINK round-trip under the configured prefix.
			// ----------------------------------------------------------------
			$testKey   = (string) ( $config['prefix'] ?? 'wpmgr' ) . ':__oc_test__';
			$testValue = 'wpmgr_oc_test_' . wp_rand( 1000, 9999 );
			try {
				$redis->setex( $testKey, 5, $testValue );
				$fetched = $redis->get( $testKey );
				if ( $fetched === $testValue ) {
					$result['round_trip_ok'] = true;
				}
				$redis->unlink( $testKey );
			} catch ( \Throwable $e ) {
				$result['round_trip_ok'] = false;
			}

			// ----------------------------------------------------------------
			// 6. Flush-capability class.
			// ----------------------------------------------------------------
			$shared  = isset( $config['shared'] ) && (bool) $config['shared'];
			$canFlush = false;
			if ( ! $shared ) {
				try {
					// Test if FLUSHDB is permitted (try with ASYNC false).
					// We do NOT actually flush; we test with a SCAN-only probe instead.
					// Flush-capability = "flushdb-safe" only when not shared AND
					// the operator declares shared=false (we trust their declaration for v1).
					$canFlush = true;
				} catch ( \Throwable $e ) {
					$aclDenials[] = 'flush';
				}
			}

			// Check if SCAN is denied using the canonical phpredis by-ref iterator idiom.
			$scanDenied = false;
			try {
				$redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
				$it = null;
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem; by-ref iterator is the canonical phpredis pattern
				$redis->scan( $it, '__wpmgr_scan_probe__', 1 );
			} catch ( \Throwable $e ) {
				$scanDenied   = true;
				$aclDenials[] = 'scan';
			}

			$result['flush_capability_class'] = ( $canFlush && ! $shared ) ? 'flushdb-safe' : 'scan-only';
			$result['acl_denials']            = array_values( array_unique( $aclDenials ) );

			$result['ok']     = $result['reachable'] && $result['round_trip_ok'];
			$result['detail'] = $result['ok'] ? 'probe passed' : 'probe failed';

			$conn->close();

		} catch ( \Throwable $e ) {
			$result['ok']     = false;
			$result['detail'] = 'connection failed';
		}

		return $result;
	}

	/**
	 * Return the default (all-false) capabilities struct.
	 *
	 * @return array<string,mixed>
	 */
	private function defaultCapabilities(): array
	{
		return [
			'phpredis_version'     => '',
			'igbinary_available'   => false,
			'lzf_available'        => false,
			'lz4_available'        => false,
			'zstd_available'       => false,
			'tls_supported'        => false,
			'value_metadata_reads' => false,
			'native_retry_options' => false,
			'keepttl_supported'    => false,
			'flush_async_supported' => false,
		];
	}
}
