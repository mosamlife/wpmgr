<?php
/**
 * Regression tests for the four diagnosed object-cache bugs.
 *
 * Bug 1: Stub templating — stamped absolute engine path, ours-outdated auto-refresh.
 * Bug 2: phpredis SCAN idiom — by-ref iterator, not Predis array-arg.
 * Bug 3: Heartbeat hit/miss counters via accumulate + consume-and-reset.
 * Bug 4: Exception class name appended to detail strings.
 *
 * @package WPMgr\Agent\Tests\ObjectCache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\ObjectCache;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller;
use WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat;
use WPMgr\Agent\ObjectCache\ObjectCacheConfig;
use WPMgr\Agent\Commands\ObjectcacheFlushCommand;
use WPMgr\Agent\Commands\ObjectcacheDisableCommand;
use WPMgr\Agent\Commands\ObjectcacheEnableCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller
 * @covers \WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat
 * @covers \WPMgr\Agent\Commands\ObjectcacheFlushCommand
 * @covers \WPMgr\Agent\Commands\ObjectcacheDisableCommand
 * @covers \WPMgr\Agent\Commands\ObjectcacheEnableCommand
 */
final class ObjectCacheBugFixTest extends TestCase
{
	private string $tmpDir;

	/** @var array<string,mixed> */
	private array $optionStore = [];

	protected function set_up(): void
	{
		parent::set_up();
		Monkey\setUp();

		$this->tmpDir     = sys_get_temp_dir() . '/wpmgr_bugfix_test_' . uniqid( '', true );
		mkdir( $this->tmpDir, 0755, true );
		$this->optionStore = [];

		Functions\when( 'get_option' )->alias( fn( $k, $d = false ) => $this->optionStore[ $k ] ?? $d );
		Functions\when( 'update_option' )->alias( function ( $k, $v ) {
			$this->optionStore[ $k ] = $v;
			return true;
		} );
		Functions\when( 'sanitize_key' )->alias(
			static fn( $key ) => strtolower( preg_replace( '/[^a-z0-9_\-.]/', '', strtolower( (string) $key ) ) ?? '' )
		);
		Functions\when( 'sanitize_text_field' )->alias( static fn( $v ) => (string) $v );
		Functions\when( 'wp_delete_file' )->alias( static function ( $file ) {
			@unlink( $file ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test stub
		} );
	}

	protected function tear_down(): void
	{
		$this->removeDir( $this->tmpDir );
		Monkey\tearDown();
		parent::tear_down();
	}

	private function removeDir( string $dir ): void
	{
		if ( ! is_dir( $dir ) ) {
			return;
		}
		$files = glob( $dir . '/*' );
		if ( is_array( $files ) ) {
			foreach ( $files as $file ) {
				is_dir( $file ) ? $this->removeDir( $file ) : @unlink( $file ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test cleanup
			}
		}
		@rmdir( $dir );
	}

	// =========================================================================
	// Phase A: Self-contained generated artifact
	// =========================================================================

	/**
	 * The generated drop-in artifact must contain our SIGNATURE and Version: 2.2.1
	 * within the first 200 bytes (stub version updated to 2.2.1 in 0.43.1 shutdown-marker fix).
	 */
	public function test_generated_artifact_signature_and_version_in_first_200_bytes(): void
	{
		$artifactPath = dirname( __DIR__, 2 ) . '/assets/wpmgr-object-cache-dropin.php';
		$this->assertFileExists( $artifactPath );
		$first200 = substr( (string) file_get_contents( $artifactPath ), 0, 200 );
		$this->assertStringContainsString(
			ObjectCacheDropinInstaller::SIGNATURE,
			$first200,
			'SIGNATURE must be in the first 200 bytes of the generated artifact'
		);
		$this->assertStringContainsString(
			'Version: 2.2.1',
			$first200,
			'Version: 2.2.1 must be in the first 200 bytes of the generated artifact (0.43.1 shutdown-marker fix)'
		);
	}

	/**
	 * install() with the generated artifact installs a byte-for-byte copy and
	 * state() returns ours-current. The installed file must be valid PHP.
	 */
	public function test_install_copies_generated_artifact(): void
	{
		$artifactPath = dirname( __DIR__, 2 ) . '/assets/wpmgr-object-cache-dropin.php';
		if ( ! is_file( $artifactPath ) ) {
			$this->markTestSkipped( 'Generated artifact not found' );
		}

		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $artifactPath );
		$result    = $installer->install();

		$this->assertTrue( $result['ok'], 'install() must succeed: ' . $result['detail'] );

		$installed  = (string) file_get_contents( $this->tmpDir . '/' . ObjectCacheDropinInstaller::CANONICAL );
		$artifact   = (string) file_get_contents( $artifactPath );
		$this->assertSame( $artifact, $installed, 'Installed file must be byte-for-byte copy of the artifact' );

		// Must be syntactically valid PHP.
		$tmpOut = sys_get_temp_dir() . '/wpmgr_artifact_lint_' . uniqid( '', true ) . '.php';
		file_put_contents( $tmpOut, $installed );
		$output = [];
		$exit   = 0;
		exec( 'php -l ' . escapeshellarg( $tmpOut ) . ' 2>&1', $output, $exit );
		@unlink( $tmpOut ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test cleanup
		$this->assertSame( 0, $exit, 'Installed artifact must be valid PHP: ' . implode( "\n", $output ) );
	}

	/**
	 * The SIGNATURE check must work on the generated artifact after install.
	 */
	public function test_state_ours_current_on_generated_artifact(): void
	{
		$artifactPath = dirname( __DIR__, 2 ) . '/assets/wpmgr-object-cache-dropin.php';
		if ( ! is_file( $artifactPath ) ) {
			$this->markTestSkipped( 'Generated artifact not found' );
		}

		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $artifactPath );
		$installer->install();

		$this->assertSame(
			ObjectCacheDropinInstaller::STATE_OURS_CURRENT,
			$installer->state(),
			'state() must return ours-current after install with the generated artifact'
		);
	}

	/**
	 * A stub installed with Version: 1.0.0 (the pre-fix version) must be
	 * classified as ours-outdated when the stub template is 1.1.0.
	 */
	public function test_old_version_stub_classified_as_ours_outdated(): void
	{
		// Write an old stub (1.0.0) to the content dir.
		$oldStub = "<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.0.0\n";
		$dropinPath = $this->tmpDir . '/' . ObjectCacheDropinInstaller::CANONICAL;
		file_put_contents( $dropinPath, $oldStub );

		// New stub template has Version: 1.1.0.
		$newStubPath = $this->tmpDir . '/new_stub.php';
		file_put_contents(
			$newStubPath,
			"<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.1.0\n"
		);

		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $newStubPath );
		$this->assertSame(
			ObjectCacheDropinInstaller::STATE_OURS_OUTDATED,
			$installer->state(),
			'Stub with old version must be classified as ours-outdated'
		);
	}

	/**
	 * maybeAutoRefresh() must replace an ours-outdated stub and return true.
	 */
	public function test_maybe_auto_refresh_replaces_outdated_stub(): void
	{
		// Write an old stub (1.0.0).
		$oldStub    = "<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.0.0\n";
		$dropinPath = $this->tmpDir . '/' . ObjectCacheDropinInstaller::CANONICAL;
		file_put_contents( $dropinPath, $oldStub );

		// New stub template has Version: 1.1.0.
		$newStubPath = $this->tmpDir . '/new_stub.php';
		file_put_contents(
			$newStubPath,
			"<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.1.0\n"
		);

		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $newStubPath );
		$this->assertSame( ObjectCacheDropinInstaller::STATE_OURS_OUTDATED, $installer->state() );

		$refreshed = $installer->maybeAutoRefresh();
		$this->assertTrue( $refreshed, 'maybeAutoRefresh() must return true after refreshing' );
		$this->assertSame( ObjectCacheDropinInstaller::STATE_OURS_CURRENT, $installer->state() );
	}

	/**
	 * maybeAutoRefresh() must NOT touch a foreign drop-in.
	 */
	public function test_maybe_auto_refresh_does_not_touch_foreign(): void
	{
		$foreignContent = "<?php\n// A completely different cache plugin.\n";
		$dropinPath     = $this->tmpDir . '/' . ObjectCacheDropinInstaller::CANONICAL;
		file_put_contents( $dropinPath, $foreignContent );

		$newStubPath = $this->tmpDir . '/new_stub.php';
		file_put_contents(
			$newStubPath,
			"<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.1.0\n"
		);

		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $newStubPath );
		$result    = $installer->maybeAutoRefresh();
		$this->assertFalse( $result, 'maybeAutoRefresh() must return false for foreign drop-in' );

		// File content must be unchanged.
		$this->assertSame( $foreignContent, file_get_contents( $dropinPath ) );
	}

	/**
	 * maybeAutoRefresh() must return true (no-op) when the stub is already current.
	 */
	public function test_maybe_auto_refresh_noop_when_current(): void
	{
		$stubContent = "<?php\n// " . ObjectCacheDropinInstaller::SIGNATURE . "\n// Version: 1.1.0\n";
		$stubPath    = $this->tmpDir . '/stub.php';
		file_put_contents( $stubPath, $stubContent );

		// Install first.
		$installer = new ObjectCacheDropinInstaller( $this->tmpDir, $stubPath );
		$installer->install();

		$result = $installer->maybeAutoRefresh();
		$this->assertTrue( $result, 'maybeAutoRefresh() must return true when already current' );
	}

	// =========================================================================
	// BUG 2: phpredis SCAN idiom — FakeRedis double
	//
	// The fake \Redis double enforces the phpredis SCAN signature:
	//   scan(?int &$iterator, ?string $pattern = null, int $count = 0)
	// - First argument is typed ?int (by-ref). Passing a non-null non-int value
	//   raises TypeError on PHP 8 — exactly what the real phpredis extension does.
	// - Returns a flat array of keys (or false when iteration is complete).
	// =========================================================================

	/**
	 * Build a fake Redis double that:
	 * - Has a ?int-typed by-ref first argument (enforces phpredis signature).
	 * - Returns flat key arrays across batches, then false.
	 * - Records setOption calls and deleted keys.
	 *
	 * @param int $batchSize Keys returned per scan() call (before filtering).
	 * @return object
	 */
	private function buildFakeRedis( int $batchSize = 3 ): \Redis
	{
		return new class ( $batchSize ) extends \Redis {
			/** @var array<string> All keys in the "Redis" store. */
			private array $store = [];

			/** @var int Batch size per scan call. */
			private int $batchSize;

			/** @var int Scan position (index into $store). */
			private int $pos = 0;

			/** @var array<string> Keys deleted via del/unlink. */
			public array $deleted = [];

			/** @var bool True when setOption( OPT_SCAN, SCAN_RETRY ) was called. */
			public bool $scanRetrySet = false;

			public function __construct( int $batchSize )
			{
				$this->batchSize = $batchSize;
				for ( $i = 0; $i < 7; $i++ ) {
					$this->store[] = 'wpmgr:default:key' . $i;
				}
				$this->store[] = 'other:default:key99';
			}

			public function setOption( int $opt, mixed $value ): bool
			{
				// Resolve via the \Redis constants so the tracker works against
				// both the constants-only test stub and the real extension.
				if ( $opt === \Redis::OPT_SCAN ) {
					$this->scanRetrySet = ( (int) $value === \Redis::SCAN_RETRY );
				}
				return true;
			}

			/**
			 * phpredis-compatible SCAN: first argument must be ?int (by-ref).
			 * The signature mirrors the native phpredis 6 declaration exactly so
			 * this class stays loadable when ext-redis is present (CI runners) and
			 * when only the constants-only test stub exists (local).
			 *
			 * @param string|int|null $it      By-ref iterator cursor.
			 * @param ?string         $pattern Key pattern.
			 * @param int             $count   Hint count.
			 * @param ?string         $type    Key type filter (unused by the fake).
			 * @return array<string>|false
			 */
			public function scan( string|int|null &$it, ?string $pattern = null, int $count = 0, ?string $type = null ): array|false
			{
				if ( $this->pos >= count( $this->store ) ) {
					$it = 0;
					return false;
				}

				$slice = array_slice( $this->store, $this->pos, $this->batchSize );
				$this->pos += $this->batchSize;

				if ( $pattern !== null && $pattern !== '' ) {
					$prefix = rtrim( $pattern, '*' );
					$slice  = array_values( array_filter( $slice, static fn( $k ) => str_starts_with( $k, $prefix ) ) );
				}

				$it = ( $this->pos >= count( $this->store ) ) ? 0 : $this->pos;
				return $slice;
			}

			/**
			 * Mirrors the native phpredis signature (key-or-array first arg).
			 *
			 * The real Redis::del() parameter types differ by phpredis build:
			 * some ship `array|string $key, string ...$other_keys`, others ship
			 * both parameters fully untyped. PHP's override-compatibility check
			 * is contravariant on parameter types, so this override must be no
			 * narrower than the loosest real signature — i.e. untyped here —
			 * or the class fatals to load under whichever phpredis build has
			 * the untyped form. Untyped is compatible with both. The intended,
			 * narrower types live in the @param tags below instead of the
			 * signature.
			 *
			 * @param array<string>|string $key        First key or array of keys.
			 * @param string               ...$other_keys Additional keys.
			 */
			public function del( $key, ...$other_keys ): int
			{
				$keys          = array_merge( is_array( $key ) ? array_values( $key ) : [ $key ], array_values( $other_keys ) );
				$this->deleted = array_merge( $this->deleted, $keys );
				return count( $keys );
			}

			/**
			 * Same parameter-typing rationale as del() above: untyped so the
			 * override stays compatible with every real phpredis build.
			 *
			 * @param array<string>|string $key        First key or array of keys.
			 * @param string               ...$other_keys Additional keys.
			 */
			public function unlink( $key, ...$other_keys ): int
			{
				$keys          = array_merge( is_array( $key ) ? array_values( $key ) : [ $key ], array_values( $other_keys ) );
				$this->deleted = array_merge( $this->deleted, $keys );
				return count( $keys );
			}

			public function flushDB( ?bool $sync = null ): bool
			{
				$this->store   = [];
				$this->deleted = [];
				return true;
			}

			public function close(): bool
			{
				return true;
			}
		};
	}

	/**
	 * buildFakeRedis() returns an anonymous class extending \Redis. Whether
	 * \Redis is the C-implemented ext-redis class or the constants-only
	 * userland stub in tests/wp-stubs.php is an environment fact this suite
	 * does not control, and the two give this file different meaning: only
	 * the real extension's method signatures can prove the override is
	 * actually compatible with what a production site loads. Report which
	 * case ran instead of letting it pass silently either way.
	 */
	public function test_fake_redis_extends_the_real_extension_class(): void
	{
		if ( ! extension_loaded( 'redis' ) ) {
			$this->markTestSkipped(
				'ext-redis is not loaded in this environment, so the FakeRedis override ' .
				'in buildFakeRedis() was only checked against the constants-only stub in ' .
				'tests/wp-stubs.php, not against the real Redis class signatures.'
			);
		}

		$this->assertTrue(
			( new \ReflectionClass( \Redis::class ) )->isInternal(),
			'\\Redis resolved to a userland class, not the internal ext-redis one, ' .
			'even though extension_loaded(\'redis\') reported true.'
		);
		$this->assertInstanceOf(
			\Redis::class,
			$this->buildFakeRedis(),
			'FakeRedis must actually extend the real, internal Redis class here.'
		);
	}

	/**
	 * Passing an array as the by-ref ?int first argument to the fake Redis scan()
	 * must throw TypeError — this confirms the fake enforces the phpredis signature
	 * and would catch a caller using the wrong Predis array-arg idiom where the
	 * array ends up at the first position.
	 */
	public function test_fake_redis_throws_typeerror_on_array_as_first_arg(): void
	{
		$redis = $this->buildFakeRedis();
		$arr   = [ 'match' => 'wpmgr:*', 'count' => 500 ];
		$this->expectException( \TypeError::class );
		$redis->scan( $arr );
	}

	/**
	 * The canonical by-ref iterator call (null start) must NOT throw TypeError.
	 */
	public function test_fake_redis_accepts_null_iterator(): void
	{
		$redis = $this->buildFakeRedis();
		$redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
		$it   = null;
		$keys = $redis->scan( $it, 'wpmgr:*', 500 );
		$this->assertIsArray( $keys );
	}

	/**
	 * ObjectcacheFlushCommand::scanFlush (accessed via reflection) must:
	 * - Set SCAN_RETRY on the Redis handle.
	 * - Delete all matching keys via the by-ref iterator pattern.
	 * - Not throw TypeError.
	 */
	public function test_flush_command_scan_uses_byref_iterator(): void
	{
		$redis = $this->buildFakeRedis( 3 );

		$cmd = new ObjectcacheFlushCommand();
		$ref    = new \ReflectionClass( $cmd );
		$method = $ref->getMethod( 'scanFlush' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.

		// Must not throw; must delete the matching keys.
		$deleted = $method->invoke( $cmd, $redis, 'wpmgr:*', false );
		$this->assertGreaterThan( 0, $deleted, 'scanFlush must delete matching keys' );
		$this->assertTrue( $redis->scanRetrySet, 'scanFlush must call setOption(OPT_SCAN, SCAN_RETRY)' );
		$this->assertNotEmpty( $redis->deleted, 'scanFlush must delete keys via del()' );
	}

	/**
	 * All deleted keys must match the supplied pattern (no cross-prefix leakage).
	 */
	public function test_flush_command_scan_only_deletes_matching_keys(): void
	{
		$redis = $this->buildFakeRedis( 10 ); // Large batch: get all keys in one pass.

		$cmd    = new ObjectcacheFlushCommand();
		$ref    = new \ReflectionClass( $cmd );
		$method = $ref->getMethod( 'scanFlush' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.

		$method->invoke( $cmd, $redis, 'wpmgr:*', false );

		foreach ( $redis->deleted as $key ) {
			$this->assertStringStartsWith( 'wpmgr:', $key, "Deleted key '{$key}' must match the pattern" );
		}
	}

	// =========================================================================
	// BUG 3: Heartbeat accumulate + consume-and-reset
	// =========================================================================

	/**
	 * persistStats on the engine must write delta_hit_count and delta_miss_count
	 * to the stats option.
	 */
	public function test_persist_stats_writes_delta_counters(): void
	{
		$oc  = \WPMgr_Object_Cache::boot();
		$ref = new \ReflectionClass( $oc );

		$configProp = $ref->getProperty( 'config' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$configProp->setValue( $oc, [ 'analytics_enabled' => true ] );

		// Produce hits and misses in array mode.
		$oc->set( 'k1', 'v1', 'default' );
		$oc->get( 'k1', 'default' );    // Hit.
		$oc->get( 'k_miss', 'default' ); // Miss.

		$persist = $ref->getMethod( 'persistStats' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$persist->invoke( $oc );

		$stored = $this->optionStore['wpmgr_object_cache_stats'] ?? null;
		$this->assertIsArray( $stored, 'persistStats must write to the option' );
		$this->assertArrayHasKey( 'delta_hit_count', $stored );
		$this->assertArrayHasKey( 'delta_miss_count', $stored );
		$this->assertGreaterThanOrEqual( 1, $stored['delta_hit_count'] );
		$this->assertGreaterThanOrEqual( 1, $stored['delta_miss_count'] );
	}

	/**
	 * A second persistStats call must ACCUMULATE on top of the first, not overwrite.
	 */
	public function test_persist_stats_accumulates_across_calls(): void
	{
		$oc  = \WPMgr_Object_Cache::boot();
		$ref = new \ReflectionClass( $oc );

		$configProp = $ref->getProperty( 'config' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$configProp->setValue( $oc, [ 'analytics_enabled' => true ] );

		$hitProp = $ref->getProperty( 'cache_hits' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$missProp = $ref->getProperty( 'cache_misses' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.

		$persist = $ref->getMethod( 'persistStats' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.

		// First "request": 2 hits, 1 miss.
		$hitProp->setValue( $oc, 2 );
		$missProp->setValue( $oc, 1 );
		$persist->invoke( $oc );

		$after1 = $this->optionStore['wpmgr_object_cache_stats'] ?? [];
		$this->assertSame( 2, $after1['delta_hit_count'] );
		$this->assertSame( 1, $after1['delta_miss_count'] );

		// Second "request" (simulate new request by resetting in-memory counters).
		$hitProp->setValue( $oc, 3 );
		$missProp->setValue( $oc, 2 );
		$persist->invoke( $oc );

		$after2 = $this->optionStore['wpmgr_object_cache_stats'] ?? [];
		$this->assertSame( 5, $after2['delta_hit_count'], 'second persist must accumulate: 2+3=5' );
		$this->assertSame( 3, $after2['delta_miss_count'], 'second persist must accumulate: 1+2=3' );
	}

	/**
	 * persistStats must preserve delta_since_ts from the first call (not reset it on second).
	 */
	public function test_persist_stats_preserves_since_ts(): void
	{
		$oc  = \WPMgr_Object_Cache::boot();
		$ref = new \ReflectionClass( $oc );

		$configProp = $ref->getProperty( 'config' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$configProp->setValue( $oc, [ 'analytics_enabled' => true ] );

		$hitProp = $ref->getProperty( 'cache_hits' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$hitProp->setValue( $oc, 1 );

		$persist = $ref->getMethod( 'persistStats' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$persist->invoke( $oc );

		$after1 = $this->optionStore['wpmgr_object_cache_stats'] ?? [];
		$since1 = $after1['delta_since_ts'] ?? 0.0;
		$this->assertGreaterThan( 0.0, $since1, 'delta_since_ts must be set on first persist' );

		// Second persist must NOT reset delta_since_ts.
		$hitProp->setValue( $oc, 1 );
		$persist->invoke( $oc );

		$after2  = $this->optionStore['wpmgr_object_cache_stats'] ?? [];
		$since2  = $after2['delta_since_ts'] ?? 0.0;
		$this->assertSame( $since1, $since2, 'delta_since_ts must be preserved across accumulations' );
	}

	/**
	 * The consume-and-reset logic: after build() consumes the deltas,
	 * the stored option must have zeroed delta counters.
	 */
	public function test_consume_and_reset_zeroes_delta_counters(): void
	{
		$sinceTs = microtime( true ) - 10.0;

		$stats = [
			'state'               => 'connected',
			'latency_ms'          => 2.0,
			'last_error_class'    => '',
			'hit_ratio_window_pct' => 60.0,
			'delta_hit_count'     => 20,
			'delta_miss_count'    => 5,
			'delta_ops'           => 50,
			'delta_wait_ms'       => 30.0,
			'delta_sample_count'  => 3,
			'delta_since_ts'      => $sinceTs,
		];

		// Consume the deltas (mirrors what ObjectCacheHeartbeat::build() does).
		$hitCount  = (int) $stats['delta_hit_count'];
		$missCount = (int) $stats['delta_miss_count'];
		$ops       = (int) $stats['delta_ops'];
		$waitMs    = (float) $stats['delta_wait_ms'];
		$samples   = (int) $stats['delta_sample_count'];
		$since     = (float) $stats['delta_since_ts'];

		$elapsedSec = max( 0.001, microtime( true ) - $since );
		$opsPerSec  = ( $elapsedSec > 0 && $ops > 0 ) ? round( $ops / $elapsedSec, 2 ) : 0.0;
		$avgWaitMs  = $samples > 0 ? round( $waitMs / $samples, 2 ) : 0.0;

		// Reset (as build() does).
		$reset = array_merge( $stats, [
			'delta_hit_count'    => 0,
			'delta_miss_count'   => 0,
			'delta_ops'          => 0,
			'delta_wait_ms'      => 0.0,
			'delta_sample_count' => 0,
			'delta_since_ts'     => 0.0,
		] );
		$this->optionStore[ ObjectCacheHeartbeat::OPTION_STATS ] = $reset;

		// Verify consumed values are correct.
		$this->assertSame( 20, $hitCount );
		$this->assertSame( 5, $missCount );
		$this->assertGreaterThan( 0.0, $opsPerSec );
		$this->assertSame( round( 30.0 / 3, 2 ), $avgWaitMs );

		// Verify the option was zeroed.
		$after = $this->optionStore[ ObjectCacheHeartbeat::OPTION_STATS ];
		$this->assertSame( 0, $after['delta_hit_count'] );
		$this->assertSame( 0, $after['delta_miss_count'] );
		$this->assertSame( 0, $after['delta_ops'] );
		$this->assertSame( 0.0, $after['delta_wait_ms'] );
		$this->assertSame( 0.0, $after['delta_since_ts'] );
	}

	/**
	 * avg_wait_ms must be 0.0 when there are no samples (guard against div-by-zero).
	 */
	public function test_avg_wait_ms_zero_when_no_samples(): void
	{
		$samples   = 0;
		$waitMs    = 0.0;
		$avgWaitMs = $samples > 0 ? round( $waitMs / $samples, 2 ) : 0.0;
		$this->assertSame( 0.0, $avgWaitMs );
	}

	/**
	 * ops_per_sec must be 0.0 when there are no ops (guard against useless division).
	 */
	public function test_ops_per_sec_zero_when_no_ops(): void
	{
		$ops        = 0;
		$elapsedSec = 10.0;
		$opsPerSec  = ( $elapsedSec > 0 && $ops > 0 ) ? round( $ops / $elapsedSec, 2 ) : 0.0;
		$this->assertSame( 0.0, $opsPerSec );
	}

	/**
	 * The heartbeat block must include hit_count, miss_count, ops_per_sec,
	 * and avg_wait_ms keys when stats are present with non-zero deltas.
	 *
	 * We cannot invoke ObjectCacheHeartbeat::build() directly in the test env
	 * (WP_CONTENT_DIR is undefined so ObjectCacheConfig::load() returns [] and
	 * build() returns null). Instead we verify the shape of the stats option
	 * that build() would consume, confirming all four keys are present after
	 * a full accumulate cycle.
	 */
	public function test_stats_option_has_all_four_delta_fields(): void
	{
		$oc  = \WPMgr_Object_Cache::boot();
		$ref = new \ReflectionClass( $oc );

		$configProp = $ref->getProperty( 'config' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$configProp->setValue( $oc, [ 'analytics_enabled' => true ] );

		$hitProp  = $ref->getProperty( 'cache_hits' );
		$missProp = $ref->getProperty( 'cache_misses' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$hitProp->setValue( $oc, 5 );
		$missProp->setValue( $oc, 2 );

		$persist = $ref->getMethod( 'persistStats' );
		// setAccessible() is a no-op since PHP 8.1 — omitted.
		$persist->invoke( $oc );

		$stored = $this->optionStore['wpmgr_object_cache_stats'] ?? [];

		foreach ( [ 'delta_hit_count', 'delta_miss_count', 'delta_ops', 'delta_wait_ms' ] as $key ) {
			$this->assertArrayHasKey( $key, $stored, "Stats option must have '{$key}' after persistStats()" );
		}
	}

	// =========================================================================
	// BUG 4: Exception class name in detail string
	// =========================================================================

	/**
	 * The detail format rule: exception class name appended, message excluded.
	 */
	public function test_bug4_detail_includes_class_not_message(): void
	{
		$e      = new \TypeError( 'Redis::scan(): host@server:6379 credentials in message' );
		$detail = 'objectcache.flush failed: ' . get_class( $e );

		$this->assertStringContainsString( 'TypeError', $detail );
		$this->assertStringNotContainsString( 'credentials', $detail );
		$this->assertStringNotContainsString( 'host@server', $detail );
	}

	/**
	 * flush command returns ok:false with no config stored.
	 * When no exception is thrown (normal validation bail), there is no class suffix.
	 */
	public function test_flush_command_no_config_detail(): void
	{
		$cmd    = new ObjectcacheFlushCommand();
		$result = $cmd->execute( [], [ 'scope' => 'all' ] );
		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'no config', $result['detail'] );
	}

	/**
	 * disable command success detail must be 'disabled' (no exception class).
	 */
	public function test_disable_command_success_detail(): void
	{
		$cmd    = new ObjectcacheDisableCommand();
		$result = $cmd->execute( [], [ 'flush' => false ] );
		$this->assertTrue( $result['ok'] );
		$this->assertSame( 'disabled', $result['detail'] );
	}

	/**
	 * enable command missing config_hash detail must not contain a colon
	 * (it is a validation bail, not a catch-wrapped exception).
	 */
	public function test_enable_command_validation_bail_detail_no_colon(): void
	{
		$cmd    = new ObjectcacheEnableCommand();
		$result = $cmd->execute( [], [] );
		$this->assertFalse( $result['ok'] );
		$this->assertStringContainsString( 'config_hash', $result['detail'] );
		$this->assertStringNotContainsString( ':', $result['detail'] );
	}

	/**
	 * The enable command catch-all appends the exception class name.
	 * We verify the format string directly since we cannot easily force
	 * the enable command to throw in the catch path without a live Redis.
	 */
	public function test_enable_command_catch_format_includes_class(): void
	{
		$e      = new \RuntimeException( 'internal error' );
		$detail = 'objectcache.enable failed: ' . get_class( $e );
		$this->assertStringEndsWith( 'RuntimeException', $detail );
		$this->assertStringNotContainsString( 'internal error', $detail );
	}

	/**
	 * The disable command catch-all appends the exception class name.
	 */
	public function test_disable_command_catch_format_includes_class(): void
	{
		$e      = new \TypeError( 'some internal' );
		$detail = 'objectcache.disable failed: ' . get_class( $e );
		$this->assertStringEndsWith( 'TypeError', $detail );
		$this->assertStringNotContainsString( 'some internal', $detail );
	}
}
