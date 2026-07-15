<?php
/**
 * WPMgr Object Cache engine — implements the full WordPress wp_cache_* API
 * backed by phpredis with graceful degradation to a pure in-memory array cache.
 *
 * This file is included from the object-cache.php drop-in stub. It:
 *   1. Loads the supporting classes (autoloader may not be available this early).
 *   2. Builds the config from the 0600 config file.
 *   3. Attempts to connect; on any boot Throwable, falls back to a pure-array
 *      cache so the site never errors.
 *   4. Instantiates the global $wp_object_cache and registers the shutdown hook.
 *
 * @package WPMgr\Agent\ObjectCache
 */

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

// ---------------------------------------------------------------------------
// Load supporting classes. Neither the Composer autoloader nor any plugin
// constant exists at drop-in load time (wp-settings.php includes
// object-cache.php before plugins), so resolve the siblings from this file's
// own directory: they always live next to the engine.
// ---------------------------------------------------------------------------

foreach (
	[
		'class-object-cache-config.php',
		'class-redis-connection.php',
	] as $wpmgr_oc_dep
) {
	$wpmgr_oc_dep_path = __DIR__ . '/' . $wpmgr_oc_dep;
	if ( @is_file( $wpmgr_oc_dep_path ) ) {
		require_once $wpmgr_oc_dep_path; // phpcs:ignore WordPressVIPMinimum.Files.IncludingFile.NotAbsolutePath -- __DIR__-anchored sibling, always absolute
	}
}

unset( $wpmgr_oc_dep, $wpmgr_oc_dep_path );

// ---------------------------------------------------------------------------
// Boot the cache: try Redis, fall back to pure array on any Throwable.
// ---------------------------------------------------------------------------

/**
 * Returns the global WP Object Cache instance, booting it if necessary.
 *
 * FD-1: If boot() is currently in progress (detected via the static guard),
 * the shared array-mode fallback is returned without re-entering boot().
 * This prevents get_option -> wp_cache_get -> here -> boot() recursion.
 *
 * FD-2: After a lazy boot (global was unset when called), runPostBootTasks()
 * is called so the H5 marker check still runs for lazily booted instances.
 *
 * @return \WPMgr_Object_Cache
 */
function wpmgr_get_object_cache(): \WPMgr_Object_Cache
{
	global $wp_object_cache;
	if ( ! ( $wp_object_cache instanceof \WPMgr_Object_Cache ) ) {
		// phpcs:ignore WordPress.WP.GlobalVariablesOverride.Prohibited -- object-cache drop-ins MUST assign $wp_object_cache; this is the required WP pattern
		$wp_object_cache = \WPMgr_Object_Cache::boot();
		// FD-2: run post-boot tasks after global is assigned so the H5 marker
		// check can call get_option -> wp_cache_get without recursive boot.
		$wp_object_cache->runPostBootTasks();
	}
	return $wp_object_cache;
}

// Boot now and install the shutdown hook for stats persist + close.
global $wp_object_cache;
// phpcs:ignore WordPress.WP.GlobalVariablesOverride.Prohibited -- object-cache drop-ins MUST assign $wp_object_cache; this is the required WP pattern
$wp_object_cache = \WPMgr_Object_Cache::boot();
// FD-2: run post-boot tasks AFTER the global is assigned. This is the primary
// call site. runPostBootTasks() is idempotent; the once-flag ensures H5 runs
// exactly once even if wpmgr_get_object_cache() is called before this line
// in some exotic load order.
$wp_object_cache->runPostBootTasks();

register_shutdown_function(
	static function (): void {
		global $wp_object_cache;
		if ( $wp_object_cache instanceof \WPMgr_Object_Cache ) {
			$wp_object_cache->shutdown();
		}
	}
);

// Register the per-request debug header emitter via the send_headers action.
// send_headers fires only on front-end main requests — not on REST API,
// admin-ajax, WP-Cron, wp-admin, or wp-login.php. This is intentional.
// Two-prong gate: (a) debug_header_enabled config key (operator opt-in, default
// false), OR (b) the authenticated user has manage_options capability (admins
// always see it on front-end probes; logged-in requests bypass the page cache,
// so this is reliable).
if ( function_exists( 'add_action' ) ) {
	add_action(
		'send_headers',
		static function (): void {
			global $wp_object_cache;
			if ( ! ( $wp_object_cache instanceof \WPMgr_Object_Cache ) ) {
				return;
			}
			if ( headers_sent() ) {
				return;
			}
			// Gating: flag OR manage_options capability.
			$config   = $wp_object_cache->wpmgr_get_config();
			$flagOn   = ! empty( $config['debug_header_enabled'] );
			$capOn    = false;
			if ( ! $flagOn ) {
				try {
					$capOn = function_exists( 'current_user_can' ) && current_user_can( 'manage_options' );
				} catch ( \Throwable $_ ) {
					// Never fatal.
				}
			}
			if ( ! $flagOn && ! $capOn ) {
				return;
			}
			header( 'x-wpmgr-object-cache: ' . $wp_object_cache->buildDebugHeaderValue() );
		}
	);
}

// ---------------------------------------------------------------------------
// WordPress wp_cache_* function bridge.
// WordPress defines these functions in wp-includes/cache.php ONLY when an
// object-cache drop-in is NOT present. Since we ARE the drop-in we must
// define them all here. All names are mandated by the WordPress cache API;
// they cannot carry a plugin prefix — PrefixAllGlobals is disabled for
// this bridge section only.
// phpcs:disable WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedFunctionFound -- required WordPress object-cache drop-in API; function names are not ours to choose
// ---------------------------------------------------------------------------

/**
 * Adds data to the cache if the key doesn't already exist.
 *
 * @param int|string $key    Cache key.
 * @param mixed      $data   Data to store.
 * @param mixed      $group  Cache group (any scalar; cast to string internally).
 * @param mixed      $expire TTL in seconds (any numeric; cast to int internally).
 * @return bool
 */
function wp_cache_add( $key, $data, $group = '', $expire = 0 ): bool
{
	try {
		return wpmgr_get_object_cache()->add( $key, $data, $group, $expire );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Adds multiple cache entries.
 *
 * @param array<int|string,mixed> $data   Map of key => value.
 * @param mixed                   $group  Cache group (any scalar; cast to string internally).
 * @param mixed                   $expire TTL in seconds (any numeric; cast to int internally).
 * @return array<int|string,bool>
 */
function wp_cache_add_multiple( array $data, $group = '', $expire = 0 ): array
{
	try {
		return wpmgr_get_object_cache()->add_multiple( $data, $group, $expire );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return array_fill_keys( array_keys( $data ), false );
	}
}

/**
 * Replaces the cached data only when it already exists.
 *
 * @param int|string $key    Cache key.
 * @param mixed      $data   New data.
 * @param mixed      $group  Cache group (any scalar; cast to string internally).
 * @param mixed      $expire TTL in seconds (any numeric; cast to int internally).
 * @return bool
 */
function wp_cache_replace( $key, $data, $group = '', $expire = 0 ): bool
{
	try {
		return wpmgr_get_object_cache()->replace( $key, $data, $group, $expire );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Saves data to the cache.
 *
 * WP core signature: wp_cache_set( $key, $data, $group = '', $expire = 0 ).
 * The $group parameter is the THIRD argument; $expire is FOURTH.
 * Callers that pass an int as $group (e.g. wp_cache_set($k, $v, 3600)) are
 * treated by WP core as setting group='3600' with expire=0. We match that
 * semantic via scalar normalization in the engine.
 *
 * @param int|string $key    Cache key.
 * @param mixed      $data   Data to store.
 * @param mixed      $group  Cache group (any scalar; cast to string internally).
 * @param mixed      $expire TTL in seconds (any numeric; cast to int internally).
 * @return bool
 */
function wp_cache_set( $key, $data, $group = '', $expire = 0 ): bool
{
	try {
		return wpmgr_get_object_cache()->set( $key, $data, $group, $expire );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Sets multiple cache entries.
 *
 * @param array<int|string,mixed> $data   Map of key => value.
 * @param mixed                   $group  Cache group (any scalar; cast to string internally).
 * @param mixed                   $expire TTL in seconds (any numeric; cast to int internally).
 * @return array<int|string,bool>
 */
function wp_cache_set_multiple( array $data, $group = '', $expire = 0 ): array
{
	try {
		return wpmgr_get_object_cache()->set_multiple( $data, $group, $expire );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return array_fill_keys( array_keys( $data ), false );
	}
}

/**
 * Retrieves cached data.
 *
 * @param int|string $key   Cache key.
 * @param mixed      $group Cache group (any scalar; cast to string internally).
 * @param bool       $force Force a fresh fetch from the backend.
 * @param bool|null  $found Output: whether the key was found.
 * @return mixed False when not found.
 */
function wp_cache_get( $key, $group = '', $force = false, &$found = null )
{
	try {
		return wpmgr_get_object_cache()->get( $key, $group, $force, $found );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		$found = false;
		return false;
	}
}

/**
 * Retrieves multiple cached values.
 *
 * @param array<int|string>  $keys  Cache keys.
 * @param mixed              $group Cache group (any scalar; cast to string internally).
 * @param bool               $force Force fetch.
 * @return array<int|string,mixed>
 */
function wp_cache_get_multiple( $keys, $group = '', $force = false ): array
{
	try {
		return wpmgr_get_object_cache()->get_multiple( $keys, $group, $force );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return array_fill_keys( (array) $keys, false );
	}
}

/**
 * Deletes cached data.
 *
 * @param int|string $key   Cache key.
 * @param mixed      $group Cache group (any scalar; cast to string internally).
 * @return bool
 */
function wp_cache_delete( $key, $group = '' ): bool
{
	try {
		return wpmgr_get_object_cache()->delete( $key, $group );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Deletes multiple cached entries.
 *
 * @param array<int|string> $keys  Cache keys.
 * @param mixed             $group Cache group (any scalar; cast to string internally).
 * @return array<int|string,bool>
 */
function wp_cache_delete_multiple( array $keys, $group = '' ): array
{
	try {
		return wpmgr_get_object_cache()->delete_multiple( $keys, $group );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return array_fill_keys( $keys, false );
	}
}

/**
 * Increments a numeric cache item.
 *
 * @param int|string $key    Cache key.
 * @param int        $offset Amount to increment.
 * @param mixed      $group  Cache group (any scalar; cast to string internally).
 * @return int|false New value or false on failure.
 */
function wp_cache_incr( $key, $offset = 1, $group = '' )
{
	try {
		return wpmgr_get_object_cache()->incr( $key, $offset, $group );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Decrements a numeric cache item.
 *
 * @param int|string $key    Cache key.
 * @param int        $offset Amount to decrement.
 * @param mixed      $group  Cache group (any scalar; cast to string internally).
 * @return int|false New value or false on failure.
 */
function wp_cache_decr( $key, $offset = 1, $group = '' )
{
	try {
		return wpmgr_get_object_cache()->decr( $key, $offset, $group );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Flushes the entire object cache.
 *
 * @return bool
 */
function wp_cache_flush(): bool
{
	try {
		return wpmgr_get_object_cache()->flush();
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Flushes only the in-memory runtime cache (not the persistent backend).
 *
 * @return bool
 */
function wp_cache_flush_runtime(): bool
{
	try {
		return wpmgr_get_object_cache()->flush_runtime();
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Flushes all entries in a specific cache group.
 *
 * @param mixed $group Cache group (any scalar; cast to string internally).
 * @return bool
 */
function wp_cache_flush_group( $group ): bool
{
	try {
		return wpmgr_get_object_cache()->flush_group( $group );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Initialises the cache. Called by WordPress on init.
 *
 * @return void
 */
function wp_cache_init(): void
{
	try {
		wpmgr_get_object_cache()->init();
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
	}
}

/**
 * Closes the cache connection. Called at shutdown.
 *
 * @return bool
 */
function wp_cache_close(): bool
{
	try {
		return wpmgr_get_object_cache()->close();
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * Switches the blog context in multisite.
 *
 * @param int $blog_id Blog ID to switch to.
 * @return void
 */
function wp_cache_switch_to_blog( $blog_id ): void
{
	try {
		wpmgr_get_object_cache()->switch_to_blog( (int) $blog_id );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
	}
}

/**
 * Adds a list of groups that should share a global namespace.
 *
 * @param array<string>|string $groups Groups to add.
 * @return void
 */
function wp_cache_add_global_groups( $groups ): void
{
	try {
		wpmgr_get_object_cache()->add_global_groups( (array) $groups );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
	}
}

/**
 * Adds a list of groups that should not be backed by the persistent cache.
 *
 * @param array<string>|string $groups Groups to add.
 * @return void
 */
function wp_cache_add_non_persistent_groups( $groups ): void
{
	try {
		wpmgr_get_object_cache()->add_non_persistent_groups( (array) $groups );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
	}
}

/**
 * Registers groups that should not be prefetched (v2 stub).
 *
 * @param array<string>|string $groups Groups to register.
 * @return void
 */
function wp_cache_add_non_prefetchable_groups( $groups ): void
{
	try {
		wpmgr_get_object_cache()->add_non_prefetchable_groups( (array) $groups );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
	}
}

/**
 * Reports whether a specific feature is supported.
 *
 * @param string $feature Feature name.
 * @return bool
 */
function wp_cache_supports( $feature ): bool
{
	try {
		return wpmgr_get_object_cache()->supports( (string) $feature );
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * LOW bridge: wp_cache_remember — get or compute-and-set.
 *
 * @param int|string $key      Cache key.
 * @param mixed      $group    Cache group.
 * @param int        $expire   TTL in seconds.
 * @param callable   $callback Callback to compute the value on miss.
 * @return mixed
 */
function wp_cache_remember( $key, $group, int $expire, callable $callback )
{
	try {
		$found = false;
		$value = wpmgr_get_object_cache()->get( $key, $group, false, $found );
		if ( $found ) {
			return $value;
		}
		$value = $callback();
		wpmgr_get_object_cache()->set( $key, $value, $group, $expire );
		return $value;
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * LOW bridge: wp_cache_sear — get, or set+return if missing (never false).
 *
 * @param int|string $key      Cache key.
 * @param mixed      $group    Cache group.
 * @param int        $expire   TTL in seconds.
 * @param callable   $callback Callback to compute the value on miss.
 * @return mixed
 */
function wp_cache_sear( $key, $group, int $expire, callable $callback )
{
	try {
		$found = false;
		$value = wpmgr_get_object_cache()->get( $key, $group, false, $found );
		if ( $found ) {
			return $value;
		}
		$value = $callback();
		if ( $value !== false ) {
			wpmgr_get_object_cache()->set( $key, $value, $group, $expire );
		}
		return $value;
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

/**
 * LOW bridge: wp_cache_supports_group_flush — reports group-flush support.
 *
 * @return bool
 */
function wp_cache_supports_group_flush(): bool
{
	return wpmgr_get_object_cache()->supports( 'flush_group' );
}

/**
 * LOW bridge: wp_cache_reset — resets the in-memory L1 cache (deprecated core alias).
 *
 * @return bool
 */
function wp_cache_reset(): bool
{
	try {
		return wpmgr_get_object_cache()->flush_runtime();
	} catch ( \Throwable $e ) {
		wpmgr_get_object_cache()->wpmgr_journal_wrapper_error( get_class( $e ) );
		return false;
	}
}

// phpcs:enable WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedFunctionFound

// ---------------------------------------------------------------------------
// WPMgr_Object_Cache class definition.
// This class is in the global namespace as WordPress expects it.
// ---------------------------------------------------------------------------

/**
 * WPMgr persistent object cache backed by phpredis.
 *
 * Implements the full WordPress wp_cache_* API with:
 *   - L1 per-request runtime array cache (clone-on-store/read).
 *   - Group semantics: global groups, non-persistent groups, wildcard matching.
 *   - Key shape: prefix:[blogId:]group:key.
 *   - Graceful degradation: boot failure or runtime errors => array-only mode.
 *   - maxttl ceiling on every write (D6, default 7d).
 *   - NX/XX conditional writes, KEEPTTL incr/decr with old-server fallback.
 *   - MGET + pipelines for multi ops.
 *   - UNLINK for async deletes when configured.
 *   - Flush strategies: FLUSHDB (dedicated) or SCAN+MATCH+UNLINK (shared).
 *   - Metadata integrity: JSON metadata key, flush on risky-option change.
 *   - Per-request error journal for CP diagnostics.
 *   - In-process stats counters for heartbeat block.
 */
class WPMgr_Object_Cache
{
	// -------------------------------------------------------------------------
	// Engine version — visible on the heartbeat wire so operators can confirm
	// which code is actually executing after an agent update.
	// -------------------------------------------------------------------------

	/** Version of this engine class. Included in every heartbeat block. */
	public const ENGINE_VERSION = '0.45.0';

	// -------------------------------------------------------------------------
	// Feature advertisement (wp_cache_supports).
	// -------------------------------------------------------------------------

	/** @var array<string> Supported features. */
	private const SUPPORTS = [
		'add_multiple',
		'set_multiple',
		'get_multiple',
		'delete_multiple',
		'flush_runtime',
		'flush_group',
	];

	// -------------------------------------------------------------------------
	// Global group and non-persistent group defaults.
	// -------------------------------------------------------------------------

	/** @var array<string> Groups that share a global (site-agnostic) namespace. */
	private const DEFAULT_GLOBAL_GROUPS = [
		'blog-details',
		'blog-id-cache',
		'blog-lookup',
		'global-posts',
		'networks',
		'rss',
		'site-details',
		'site-lookup',
		'site-options',
		'site-transient',
		'users',
		'useremail',
		'userlogins',
		'usermeta',
		'user_meta',
		'userslugs',
	];

	/** @var array<string> Groups whose values are never stored in Redis. */
	private const DEFAULT_NON_PERSISTENT = [
		'counts',
		'plugins',
	];

	// -------------------------------------------------------------------------
	// Instance state.
	// -------------------------------------------------------------------------

	/** wp-option name for the persisted outage epoch marker (H5). */
	public const FAILBACK_MARKER_OPTION = 'wpmgr_oc_outage_marker';

	/** Redis key suffix for the failback-flush NX lock (H5). */
	private const FAILBACK_LOCK_SUFFIX = '__wpmgr_oc_failback_lock__';

	/**
	 * FD-1: Set to true while boot() is on the call stack.
	 * Prevents recursive boot() calls from creating new Redis connections.
	 */
	private static bool $bootInProgress = false;

	/**
	 * FD-1: Shared array-mode fallback instance returned during a recursive boot.
	 * Never connects, cheap to construct; persists for the request lifetime.
	 */
	private static ?self $bootFallback = null;

	/**
	 * FD-2: Once-flag so runPostBootTasks() is idempotent across multiple callers.
	 */
	private bool $postBootTasksRan = false;

	/** @var \WPMgr\Agent\ObjectCache\RedisConnection|null Redis connection (null in array-only mode). */
	private ?\WPMgr\Agent\ObjectCache\RedisConnection $connection = null;

	/** @var \Redis|null Active phpredis handle (null in array-only mode). */
	private ?\Redis $redis = null;

	/** @var bool True when boot failed and we are running as a pure-array cache. */
	private bool $arrayMode = false;

	/** @var bool True when a reconnect this request has already been attempted. */
	private bool $reconnectAttempted = false;

	/** @var array<string,array<string,mixed>> L1 runtime cache: keyed by buildKey() output. */
	private array $cache = [];

	/** @var array<string> Global group registry. */
	private array $globalGroups = [];

	/** @var array<string> Non-persistent group registry. */
	private array $nonPersistentGroups = [];

	/** @var array<string> Non-prefetchable group registry (v2, stored for later). */
	private array $nonPrefetchableGroups = [];

	/** @var array<string,bool> Memoized wildcard group-match results. */
	private array $wildcardMemo = [];

	/** @var array<string,string> Memoized per-group key prefix (H1: buildKey prefix without the key segment). */
	private array $keyPrefixMemo = [];

	/** @var string Prefix applied to all keys. */
	private string $prefix = 'wpmgr';

	/** @var int Current blog ID for key namespacing in multisite. */
	private int $blogId = 1;

	/** @var bool Whether is_multisite() returned true at boot (H1). */
	private bool $isMultisite = false;

	/** @var int Max TTL in seconds (D6, default 7d). */
	private int $maxttl = 604800;

	/** @var int Query-group TTL in seconds (default 24h). */
	private int $queryttl = 86400;

	/** @var bool Whether to use UNLINK for deletes. */
	private bool $asyncFlush = false;

	/** @var string Flush strategy: 'auto' | 'flushdb' | 'scan'. */
	private string $flushStrategy = 'auto';

	/** @var bool Whether this is a shared Redis instance. */
	private bool $shared = true;

	/** @var bool Whether to flush on failback after an outage. */
	private bool $flushOnFailback = true;

	/** @var bool Whether we already ran the failback flush this boot. */
	private bool $failbackFlushed = false;

	/**
	 * True once the PHP-shutdown path has entered shutdown().
	 * A Redis failure that occurs during persistStats() at shutdown is not
	 * an actionable outage — the process is dying. When this flag is set,
	 * redisOp()'s error handler skips both markDegraded-side
	 * persistOutageMarker() and the marker write, while still degrading
	 * in-memory so the fallback path executes gracefully.
	 */
	private bool $inShutdown = false;

	/** @var array<string,mixed> Loaded config array. */
	private array $config = [];

	/** @var int Hit counter for current request. */
	public int $cache_hits = 0;

	/** @var int Miss counter for current request. */
	public int $cache_misses = 0;

	/** Legacy aliases for plugins that poke internals. */
	public int $hits = 0;

	/** Legacy alias. */
	public int $misses = 0;

	/** @var array<string> Per-request error journal (last N errors). */
	private array $errorJournal = [];

	/** Maximum entries in the error journal. */
	private const MAX_JOURNAL = 20;

	/** @var float Total wait time (ms) for Redis commands this request. */
	private float $totalWaitMs = 0.0;

	/** @var int Total Redis reads this request. */
	private int $redisReads = 0;

	/** @var int Total Redis writes this request. */
	private int $redisWrites = 0;

	/** @var bool Whether KEEPTTL is supported (probed at connect). */
	private bool $keepttlSupported = false;

	// -------------------------------------------------------------------------
	// Factory + boot
	// -------------------------------------------------------------------------

	/**
	 * Boot the cache: load config, connect, return the instance.
	 * On any Throwable during boot, return an array-mode instance.
	 *
	 * Post-boot invariant: this method makes ZERO WordPress function calls.
	 * The H5 outage-marker check (maybeFailbackFlushOnBoot / runPostBootTasks)
	 * is deferred to the include footer AFTER the global assignment so that
	 * get_option -> wp_cache_get -> wpmgr_get_object_cache -> boot() recursion
	 * cannot occur (FD-2 root fix).
	 *
	 * @return self
	 */
	public static function boot(): self
	{
		// FD-1: structural non-reentrance guard.
		// If boot() is called again while already on the call stack (e.g. via
		// get_option -> wp_cache_get -> wpmgr_get_object_cache -> boot()), return
		// the shared array-mode fallback without connecting or assigning the global.
		if ( self::$bootInProgress ) {
			if ( self::$bootFallback === null ) {
				self::$bootFallback = new self();
				self::$bootFallback->globalGroups        = array_flip( self::DEFAULT_GLOBAL_GROUPS );
				self::$bootFallback->nonPersistentGroups = array_flip( self::DEFAULT_NON_PERSISTENT );
				self::$bootFallback->bootArrayMode( 'boot_reentrance' );
			}
			// LOW-2: reset the in-memory L1 cache and request counters each time
			// the fallback is returned to a reentrant caller. The static instance
			// persists across requests in long-lived workers; resetting here ensures
			// one request's L1 entries can never bleed into another request's reads
			// should a future boot-time WP call reintroduce this code path.
			self::$bootFallback->cache        = [];
			self::$bootFallback->cache_hits   = 0;
			self::$bootFallback->cache_misses = 0;
			self::$bootFallback->hits         = 0;
			self::$bootFallback->misses       = 0;
			return self::$bootFallback;
		}

		self::$bootInProgress = true;
		try {
			$instance = new self();
			$instance->globalGroups         = array_flip( self::DEFAULT_GLOBAL_GROUPS );
			$instance->nonPersistentGroups  = array_flip( self::DEFAULT_NON_PERSISTENT );

			// H1: Capture multisite state at boot. switch_to_blog() becomes a no-op on single-site.
			$instance->isMultisite = function_exists( 'is_multisite' ) && is_multisite();

			// Set the current blog ID from WordPress globals when available.
			if ( isset( $GLOBALS['blog_id'] ) ) {
				$instance->blogId = (int) $GLOBALS['blog_id'];
			}

			try {
				// Load config from the 0600 file.
				if ( ! class_exists( 'WPMgr\Agent\ObjectCache\ObjectCacheConfig' ) ) {
					// Supporting classes not loaded (e.g. engine loaded standalone).
					$instance->bootArrayMode( 'classes_missing' );
					return $instance;
				}

				$configLoader = new \WPMgr\Agent\ObjectCache\ObjectCacheConfig();
				[ $config, $reason ] = $configLoader->loadWithReason();

				if ( $config === [] ) {
					// H7: distinguish config_unreadable from config_empty.
					$instance->bootArrayMode( $reason !== '' ? $reason : 'config_empty' );
					return $instance;
				}

				$instance->config     = $config;
				$instance->prefix     = isset( $config['prefix'] ) && is_string( $config['prefix'] )
					? $instance->sanitizePrefix( (string) $config['prefix'] )
					: 'wpmgr';
				$instance->maxttl     = isset( $config['maxttl_seconds'] ) ? (int) $config['maxttl_seconds'] : 604800;
				$instance->queryttl   = isset( $config['queryttl_seconds'] ) ? (int) $config['queryttl_seconds'] : 86400;
				$instance->asyncFlush = isset( $config['async_flush'] ) && (bool) $config['async_flush'];
				$instance->flushStrategy = isset( $config['flush_strategy'] ) && is_string( $config['flush_strategy'] )
					? (string) $config['flush_strategy'] : 'auto';
				$instance->shared        = ! isset( $config['shared'] ) || (bool) $config['shared'];
				$instance->flushOnFailback = ! isset( $config['flush_on_failback'] ) || (bool) $config['flush_on_failback'];

				// FD-6: check persisted reconnect cool-down before dialing.
				$cooldownResult = $instance->checkCooldown( $config );
				if ( $cooldownResult !== null ) {
					// Inside the backoff window: skip connect entirely, land in array mode.
					$instance->bootArrayMode( $cooldownResult );
					return $instance;
				}

				// Connect.
				$instance->connection = new \WPMgr\Agent\ObjectCache\RedisConnection( $config );
				$instance->redis      = $instance->connection->acquire();

				// FD-6: successful connect clears the failure state.
				$instance->clearCooldownState( $config );

				// Probe KEEPTTL support.
				$caps = \WPMgr\Agent\ObjectCache\RedisConnection::probeCapabilities( $instance->redis );
				$instance->keepttlSupported = (bool) ( $caps['keepttl_supported'] ?? false );

				// M8: phpredis 6 FLUSHDB flag gate — already handled in executeFlush.

				// FD-4: Metadata integrity uses EFFECTIVE codec values from the connection.
				$instance->checkMetadataIntegrity( $config );

				// H5: maybeFailbackFlushOnBoot() is intentionally NOT called here.
				// It is deferred to runPostBootTasks() which is called by the include
				// footer AFTER $wp_object_cache is assigned. Calling it inside boot()
				// caused get_option -> wp_cache_get -> wpmgr_get_object_cache -> boot()
				// infinite recursion with FD accumulation (FD-2 root fix).

			} catch ( \Throwable $e ) {
				// FD-6: persist cool-down state on boot connect failure.
				// LOW-1: wrap in a best-effort try/catch — writeCooldownState's
				// random_int() can throw on a CSPRNG-less platform, and any
				// exception here must not escape into the unwrapped include-footer
				// call before WP finishes loading.
				if ( isset( $config ) && $config !== [] ) {
					try {
						$instance->recordCooldownFailure( $config );
					} catch ( \Throwable $_ ) {
						// Best-effort: cool-down write failed; swallow and proceed
						// to array-mode so WP still loads.
					}
				}
				$instance->bootArrayMode( $e->getMessage() );
			}

			return $instance;
		} finally {
			self::$bootInProgress = false;
		}
	}

	/**
	 * FD-2: Run post-boot tasks that require the global $wp_object_cache to be
	 * assigned first. Idempotent: the once-flag ensures it runs exactly once per
	 * instance even if called from both the include footer and the lazy bridge.
	 *
	 * Currently performs the H5 outage-marker check (maybeFailbackFlushOnBoot).
	 * Safe to call after $wp_object_cache = \WPMgr_Object_Cache::boot() because
	 * by that point the global is set, so get_option -> wp_cache_get ->
	 * wpmgr_get_object_cache() finds the instance and returns it immediately
	 * (instanceof check passes) without triggering boot() again.
	 *
	 * @return void
	 */
	public function runPostBootTasks(): void
	{
		if ( $this->postBootTasksRan ) {
			return;
		}
		$this->postBootTasksRan = true;
		// H5: check for outage marker and attempt NX-lock failback flush.
		$this->maybeFailbackFlushOnBoot();
	}

	/**
	 * Enter array-only mode (graceful degradation).
	 *
	 * FD-3d: Close any stranded connection/redis handle before nulling them so
	 * no FD is left in the per-process pool in an unusable state.
	 *
	 * @param string $reason Reason for the fallback (logged when WP_DEBUG is on).
	 * @return void
	 */
	private function bootArrayMode( string $reason ): void
	{
		// FD-3d: close the connection through RedisConnection.close() so the FD is
		// returned to the pool rather than leaked. Keep shutdown() no-op for HEALTHY
		// handles (pooling design unchanged); this close only fires on the failed path.
		if ( $this->connection !== null ) {
			$this->connection->close();
		}

		$this->arrayMode  = true;
		$this->redis      = null;
		$this->connection = null;

		if ( $reason !== '' ) {
			$this->journalError( 'boot_failure', $reason );
			if ( defined( 'WP_DEBUG' ) && WP_DEBUG ) {
				// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- WP_DEBUG-gated diagnostic
				error_log( 'WPMgr Object Cache: degraded to array-only mode. Reason: ' . $reason );
			}
		}
	}

	// -------------------------------------------------------------------------
	// wp_cache_* API implementation
	// -------------------------------------------------------------------------

	/**
	 * Adds data to the cache if the key does not already exist.
	 *
	 * Accepts any scalar for $group and $expire to match WP core's loose-typed
	 * cache.php API. Non-string/falsy $group is cast to string (empty => 'default').
	 * Non-int $expire is cast to int.
	 *
	 * @param int|string $key    Cache key.
	 * @param mixed      $data   Data to store.
	 * @param mixed      $group  Cache group (any scalar).
	 * @param mixed      $expire TTL in seconds (any numeric).
	 * @return bool
	 */
	public function add( $key, $data, $group = '', $expire = 0 ): bool
	{
		$group  = $this->castGroup( $group );
		$expire = (int) $expire;
		if ( function_exists( 'wp_suspend_cache_addition' ) && wp_suspend_cache_addition() ) {
			return false;
		}
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group  = $this->normalizeGroup( $group );
		$keyStr = (string) $key;

		// H1: L1 keyed by the fully-qualified Redis key so blog-switched L1 can never collide.
		$redisKey = $this->buildKey( $keyStr, $group );

		// L1 hit: key already exists.
		if ( array_key_exists( $redisKey, $this->cache ) ) {
			return false;
		}

		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			$this->storeL1( $redisKey, $data );
			return true;
		}

		$ttl = $this->clampTtl( $expire, $group );

		return $this->redisOp(
			function () use ( $redisKey, $data, $ttl ): bool {
				$this->redisWrites++;
				if ( $ttl > 0 ) {
					$result = $this->redis->set( $redisKey, $data, [ 'nx', 'ex' => $ttl ] );
				} else {
					$result = $this->redis->set( $redisKey, $data, [ 'nx' ] );
				}
				return $result === true;
			},
			static function (): bool {
				return false;
			}
		) && $this->storeL1( $redisKey, $data );
	}

	/**
	 * Adds multiple cache entries.
	 *
	 * @param array<int|string,mixed> $data   Map of key => value.
	 * @param mixed                   $group  Cache group (any scalar).
	 * @param mixed                   $expire TTL in seconds (any numeric).
	 * @return array<int|string,bool>
	 */
	public function add_multiple( array $data, $group = '', $expire = 0 ): array
	{
		$group  = $this->castGroup( $group );
		$expire = (int) $expire;
		$results = [];
		foreach ( $data as $key => $value ) {
			$results[ $key ] = $this->add( $key, $value, $group, $expire );
		}
		return $results;
	}

	/**
	 * Replaces cached data only when the key already exists.
	 *
	 * @param int|string $key    Cache key.
	 * @param mixed      $data   New data.
	 * @param mixed      $group  Cache group (any scalar).
	 * @param mixed      $expire TTL in seconds (any numeric).
	 * @return bool
	 */
	public function replace( $key, $data, $group = '', $expire = 0 ): bool
	{
		$group  = $this->castGroup( $group );
		$expire = (int) $expire;
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		// M4: for non-persistent / array mode use hasInMemory check only (no pre-get round-trip).
		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			if ( ! array_key_exists( $redisKey, $this->cache ) ) {
				return false;
			}
			$this->storeL1( $redisKey, $data );
			return true;
		}

		// M4: rely on SET XX (no pre-get); Redis will reject if key is absent.
		$ttl = $this->clampTtl( $expire, $group );

		return $this->redisOp(
			function () use ( $redisKey, $data, $ttl ): bool {
				$this->redisWrites++;
				if ( $ttl > 0 ) {
					$result = $this->redis->set( $redisKey, $data, [ 'xx', 'ex' => $ttl ] );
				} else {
					$result = $this->redis->set( $redisKey, $data, [ 'xx' ] );
				}
				return $result === true;
			},
			static function (): bool {
				return false;
			}
		) && $this->storeL1( $redisKey, $data );
	}

	/**
	 * Saves data to the cache.
	 *
	 * @param int|string $key    Cache key.
	 * @param mixed      $data   Data to store.
	 * @param mixed      $group  Cache group (any scalar).
	 * @param mixed      $expire TTL in seconds (any numeric; 0 = use maxttl).
	 * @return bool
	 */
	public function set( $key, $data, $group = '', $expire = 0 ): bool
	{
		$group  = $this->castGroup( $group );
		$expire = (int) $expire;
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			$this->storeL1( $redisKey, $data );
			return true;
		}

		$ttl = $this->clampTtl( $expire, $group );

		// M3: storeL1 only on Redis success.
		$ok = $this->redisOp(
			function () use ( $redisKey, $data, $ttl ): bool {
				$this->redisWrites++;
				if ( $ttl > 0 ) {
					return $this->redis->setex( $redisKey, $ttl, $data ) === true;
				}
				return $this->redis->set( $redisKey, $data ) !== false;
			},
			static function (): bool {
				return false;
			}
		);

		if ( $ok ) {
			$this->storeL1( $redisKey, $data );
		}
		return $ok;
	}

	/**
	 * Sets multiple cache entries using a pipeline.
	 *
	 * @param array<int|string,mixed> $data   Map of key => value.
	 * @param mixed                   $group  Cache group (any scalar).
	 * @param mixed                   $expire TTL in seconds (any numeric).
	 * @return array<int|string,bool>
	 */
	public function set_multiple( array $data, $group = '', $expire = 0 ): array
	{
		$group   = $this->castGroup( $group );
		$expire  = (int) $expire;
		$group   = $this->normalizeGroup( $group );
		$results = [];

		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			foreach ( $data as $key => $value ) {
				if ( $this->validateKey( $key ) ) {
					$redisKey = $this->buildKey( (string) $key, $group );
					$this->storeL1( $redisKey, $value );
					$results[ $key ] = true;
				} else {
					$results[ $key ] = false;
				}
			}
			return $results;
		}

		// Validate and batch.
		$valid    = []; // redisKey => [origKey, value]
		foreach ( $data as $key => $value ) {
			if ( $this->validateKey( $key ) ) {
				$redisKey         = $this->buildKey( (string) $key, $group );
				$valid[ $redisKey ] = [ 'orig' => $key, 'val' => $value ];
				$results[ $key ]  = false; // Pre-fill; overwritten on success (M3).
			} else {
				$results[ $key ] = false;
			}
		}

		if ( $valid === [] ) {
			return $results;
		}

		$ttl = $this->clampTtl( $expire, $group );

		$this->redisOp(
			function () use ( $valid, $ttl, &$results ): bool {
				$this->redisWrites += count( $valid );
				$pipe = $this->redis->pipeline();
				foreach ( $valid as $redisKey => $entry ) {
					if ( $ttl > 0 ) {
						$pipe->setex( $redisKey, $ttl, $entry['val'] );
					} else {
						$pipe->set( $redisKey, $entry['val'] );
					}
				}
				$pipeResults = $pipe->exec();
				if ( is_array( $pipeResults ) ) {
					$rKeys = array_keys( $valid );
					foreach ( $pipeResults as $i => $res ) {
						if ( isset( $rKeys[ $i ] ) ) {
							$rk = $rKeys[ $i ];
							$ok = ( $res === true || $res === 'OK' );
							$results[ $valid[ $rk ]['orig'] ] = $ok;
							// M3: storeL1 per-key only on Redis success.
							if ( $ok ) {
								$this->storeL1( $rk, $valid[ $rk ]['val'] );
							}
						}
					}
				}
				return true;
			},
			static function () use ( &$results ): bool {
				// On failure all remain false.
				return false;
			}
		);

		return $results;
	}

	/**
	 * Retrieves cached data.
	 *
	 * @param int|string $key   Cache key.
	 * @param mixed      $group Cache group (any scalar).
	 * @param bool       $force Bypass L1.
	 * @param bool|null  $found Output: whether the key was found.
	 * @return mixed False when not found.
	 */
	public function get( $key, $group = '', bool $force = false, ?bool &$found = null )
	{
		$group = $this->castGroup( $group );
		if ( ! $this->validateKey( $key ) ) {
			$found = false;
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		// M2: non-persistent / array mode always serves L1 (force flag ignored for these groups).
		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			if ( array_key_exists( $redisKey, $this->cache ) ) {
				$found = true;
				$this->cache_hits++;
				$this->hits++;
				return $this->cloneValue( $this->cache[ $redisKey ] );
			}
			$found = false;
			$this->cache_misses++;
			$this->misses++;
			return false;
		}

		// H1: L1 hit (unless forced) — keyed by fully-qualified redisKey.
		if ( ! $force && array_key_exists( $redisKey, $this->cache ) ) {
			$found = true;
			$this->cache_hits++;
			$this->hits++;
			return $this->cloneValue( $this->cache[ $redisKey ] );
		}

		$value = $this->redisOp(
			function () use ( $redisKey ): mixed {
				$this->redisReads++;
				$val = $this->redis->get( $redisKey );
				return $val;
			},
			static function (): mixed {
				return false;
			},
			true // idempotent read: retry-once on timeout
		);

		if ( $value === false ) {
			$found = false;
			$this->cache_misses++;
			$this->misses++;
			return false;
		}

		$found = true;
		$this->cache_hits++;
		$this->hits++;
		$this->storeL1( $redisKey, $value );
		return $this->cloneValue( $value );
	}

	/**
	 * Retrieves multiple cached values using MGET.
	 *
	 * @param array<int|string> $keys  Cache keys.
	 * @param mixed             $group Cache group (any scalar).
	 * @param bool              $force Bypass L1.
	 * @return array<int|string,mixed>
	 */
	public function get_multiple( array $keys, $group = '', bool $force = false ): array
	{
		$group   = $this->castGroup( $group );
		$group   = $this->normalizeGroup( $group );
		// M6: pre-fill results in input order (invalid keys get false, valid start as false).
		$results = [];
		foreach ( $keys as $key ) {
			$results[ $key ] = false;
		}

		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			foreach ( $keys as $key ) {
				if ( $this->validateKey( $key ) ) {
					$redisKey = $this->buildKey( (string) $key, $group );
					if ( array_key_exists( $redisKey, $this->cache ) ) {
						$results[ $key ] = $this->cloneValue( $this->cache[ $redisKey ] );
					}
				}
				// invalid keys stay false (M6 keeps them in order too)
			}
			return $results;
		}

		// Partition: L1 hits vs Redis misses.
		$redisKeys   = [];
		$redisKeyMap = []; // redisKey => original key

		foreach ( $keys as $key ) {
			if ( ! $this->validateKey( $key ) ) {
				continue; // stays false in $results (pre-filled)
			}
			$keyStr   = (string) $key;
			$redisKey = $this->buildKey( $keyStr, $group );
			if ( ! $force && array_key_exists( $redisKey, $this->cache ) ) {
				$results[ $key ] = $this->cloneValue( $this->cache[ $redisKey ] );
				$this->cache_hits++;
				$this->hits++;
			} else {
				$redisKeys[]              = $redisKey;
				$redisKeyMap[ $redisKey ] = $key;
			}
		}

		if ( $redisKeys === [] ) {
			return $results;
		}

		$this->redisOp(
			function () use ( $redisKeys, $redisKeyMap, &$results ): bool {
				$this->redisReads += count( $redisKeys );
				$fetched = $this->redis->mget( $redisKeys );
				if ( ! is_array( $fetched ) ) {
					return false;
				}
				foreach ( $redisKeys as $i => $redisKey ) {
					if ( ! isset( $fetched[ $i ] ) ) {
						continue;
					}
					$val     = $fetched[ $i ];
					$origKey = $redisKeyMap[ $redisKey ] ?? null;
					if ( $origKey === null ) {
						continue;
					}
					if ( $val === false ) {
						$this->cache_misses++;
						$this->misses++;
					} else {
						$this->cache_hits++;
						$this->hits++;
						$this->storeL1( $redisKey, $val );
						$results[ $origKey ] = $this->cloneValue( $val );
					}
				}
				return true;
			},
			function () use ( $redisKeyMap ): bool {
				foreach ( $redisKeyMap as $origKey ) {
					$this->cache_misses++;
					$this->misses++;
				}
				return false;
			},
			true
		);

		return $results;
	}

	/**
	 * Deletes cached data.
	 *
	 * @param int|string $key   Cache key.
	 * @param mixed      $group Cache group (any scalar).
	 * @return bool
	 */
	public function delete( $key, $group = '' ): bool
	{
		$group = $this->castGroup( $group );
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		$inL1 = array_key_exists( $redisKey, $this->cache );
		unset( $this->cache[ $redisKey ] );

		// M1: non-persistent / array mode returns false on missing keys.
		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			return $inL1;
		}

		return $this->redisOp(
			function () use ( $redisKey, $inL1 ): bool {
				$this->redisWrites++;
				if ( $this->asyncFlush ) {
					$deleted = $this->redis->unlink( $redisKey );
				} else {
					$deleted = $this->redis->del( $redisKey );
				}
				// M1: > 0 means a key was actually deleted.
				return ( $deleted > 0 ) || $inL1;
			},
			static function (): bool {
				return false;
			}
		);
	}

	/**
	 * Deletes multiple cached entries.
	 *
	 * @param array<int|string> $keys  Cache keys.
	 * @param mixed             $group Cache group (any scalar).
	 * @return array<int|string,bool>
	 */
	public function delete_multiple( array $keys, $group = '' ): array
	{
		$group   = $this->castGroup( $group );
		$group   = $this->normalizeGroup( $group );
		$results = [];

		foreach ( $keys as $key ) {
			$results[ $key ] = $this->delete( $key, $group );
		}
		return $results;
	}

	/**
	 * Increments a numeric cache item, preserving TTL via KEEPTTL where supported.
	 *
	 * @param int|string $key    Cache key.
	 * @param int        $offset Amount to increment.
	 * @param mixed      $group  Cache group (any scalar).
	 * @return int|false New value or false on failure.
	 */
	public function incr( $key, int $offset = 1, $group = '' )
	{
		$group = $this->castGroup( $group );
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		// H2: non-persistent / array mode: return false on missing key.
		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			if ( ! array_key_exists( $redisKey, $this->cache ) ) {
				$this->journalError( 'incr_missing', 'incr on non-existent key: ' . $keyStr );
				return false;
			}
			$new = max( 0, (int) $this->cache[ $redisKey ] + $offset );
			$this->storeL1( $redisKey, $new );
			return $new;
		}

		$result = $this->redisOp(
			function () use ( $redisKey, $offset ): int|false {
				$this->redisWrites++;
				// H2: GET first — if key missing, return false (no create).
				$current = $this->redis->get( $redisKey );
				if ( $current === false ) {
					return false;
				}
				$newVal = max( 0, (int) $current + $offset ); // OURS-BETTER: (int) numeric-string coercion.
				// H2: GET+SET fallback so values stay serializer-encoded.
				if ( $this->keepttlSupported ) {
					$ttl  = $this->redis->ttl( $redisKey );
					$opts = $ttl > 0 ? [ 'ex' => $ttl ] : [ 'keepttl' ];
					// If key expired between GET and TTL probe, treat as missing.
					if ( $ttl === -2 ) {
						return false;
					}
					$this->redis->set( $redisKey, $newVal, $opts );
				} else {
					// No KEEPTTL: GET the TTL, then SET with ex when TTL > 0.
					$ttl = $this->redis->ttl( $redisKey );
					if ( $ttl === -2 ) {
						return false;
					}
					if ( $ttl > 0 ) {
						$this->redis->set( $redisKey, $newVal, [ 'ex' => $ttl ] );
					} else {
						$this->redis->set( $redisKey, $newVal );
					}
				}
				return $newVal;
			},
			static function (): false {
				return false;
			}
		);

		if ( $result !== false ) {
			$this->storeL1( $redisKey, $result );
		} else {
			unset( $this->cache[ $redisKey ] );
		}
		return $result;
	}

	/**
	 * Decrements a numeric cache item, preserving TTL via KEEPTTL where supported.
	 *
	 * @param int|string $key    Cache key.
	 * @param int        $offset Amount to decrement.
	 * @param mixed      $group  Cache group (any scalar).
	 * @return int|false New value or false on failure.
	 */
	public function decr( $key, int $offset = 1, $group = '' )
	{
		$group = $this->castGroup( $group );
		if ( ! $this->validateKey( $key ) ) {
			return false;
		}
		$group    = $this->normalizeGroup( $group );
		$keyStr   = (string) $key;
		$redisKey = $this->buildKey( $keyStr, $group );

		// H2: non-persistent / array mode: return false on missing key.
		if ( $this->isNonPersistent( $group ) || $this->arrayMode ) {
			if ( ! array_key_exists( $redisKey, $this->cache ) ) {
				$this->journalError( 'incr_missing', 'decr on non-existent key: ' . $keyStr );
				return false;
			}
			$new = max( 0, (int) $this->cache[ $redisKey ] - $offset );
			$this->storeL1( $redisKey, $new );
			return $new;
		}

		$result = $this->redisOp(
			function () use ( $redisKey, $offset ): int|false {
				$this->redisWrites++;
				// H2: GET first — if key missing, return false (no create).
				$current = $this->redis->get( $redisKey );
				if ( $current === false ) {
					return false;
				}
				$newVal = max( 0, (int) $current - $offset ); // OURS-BETTER: (int) numeric-string coercion.
				// H2: GET+SET fallback so values stay serializer-encoded.
				if ( $this->keepttlSupported ) {
					$ttl  = $this->redis->ttl( $redisKey );
					$opts = $ttl > 0 ? [ 'ex' => $ttl ] : [ 'keepttl' ];
					if ( $ttl === -2 ) {
						return false;
					}
					$this->redis->set( $redisKey, $newVal, $opts );
				} else {
					$ttl = $this->redis->ttl( $redisKey );
					if ( $ttl === -2 ) {
						return false;
					}
					if ( $ttl > 0 ) {
						$this->redis->set( $redisKey, $newVal, [ 'ex' => $ttl ] );
					} else {
						$this->redis->set( $redisKey, $newVal );
					}
				}
				return $newVal;
			},
			static function (): false {
				return false;
			}
		);

		if ( $result !== false ) {
			$this->storeL1( $redisKey, $result );
		} else {
			unset( $this->cache[ $redisKey ] );
		}
		return $result;
	}

	/**
	 * Flushes the entire cache. Strategy: FLUSHDB on dedicated, SCAN+UNLINK on shared.
	 *
	 * @return bool
	 */
	public function flush(): bool
	{
		$this->cache = [];

		if ( $this->arrayMode ) {
			// H7: if config file exists but is unreadable (cross-uid CLI scenario),
			// return false so WP-CLI surfaces an error instead of falsely reporting success.
			if ( ! empty( $this->config ) ) {
				// We had a config at some point — this should not happen; return true (normal array mode).
				return true;
			}
			// Check whether a config file exists; if so, this flush is a no-op against Redis.
			if (
				class_exists( 'WPMgr\Agent\ObjectCache\ObjectCacheConfig' )
				&& ( new \WPMgr\Agent\ObjectCache\ObjectCacheConfig() )->exists()
			) {
				// Config file exists but engine could not read it (unreadable) — be honest.
				// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- H7: WP-CLI flush-honesty diagnostic
				error_log( 'WPMgr Object Cache: flush() called but engine is in array mode; config file exists but may be unreadable. Redis keyspace NOT flushed.' );
				return false;
			}
			return true;
		}

		return $this->redisOp(
			function (): bool {
				return $this->executeFlush( 'all' );
			},
			static function (): bool {
				return false;
			}
		);
	}

	/**
	 * Flushes only the in-memory runtime cache.
	 *
	 * @return bool
	 */
	public function flush_runtime(): bool
	{
		$this->cache        = [];
		$this->keyPrefixMemo = [];
		return true;
	}

	/**
	 * Flushes all entries in a specific group.
	 *
	 * @param mixed $group Cache group (any scalar).
	 * @return bool
	 */
	public function flush_group( $group ): bool
	{
		$group = $this->castGroup( $group );
		$group = $this->normalizeGroup( $group );

		// H1: L1 is flat keyed by Redis key — evict all keys whose group segment matches.
		// The key shape is prefix:[blogId:]group:key (colon-delimited).
		// We do a prefix-match against the memoized group key prefix to avoid scanning all keys.
		$groupPrefixGlobal = $this->prefix . ':' . $group . ':';
		$groupPrefixBlog   = $this->prefix . ':' . $this->blogId . ':' . $group . ':';
		foreach ( array_keys( $this->cache ) as $rk ) {
			if ( strncmp( $rk, $groupPrefixGlobal, strlen( $groupPrefixGlobal ) ) === 0
				|| strncmp( $rk, $groupPrefixBlog, strlen( $groupPrefixBlog ) ) === 0
			) {
				unset( $this->cache[ $rk ] );
			}
		}

		if ( $this->arrayMode || $this->isNonPersistent( $group ) ) {
			return true;
		}

		return $this->redisOp(
			function () use ( $group ): bool {
				return $this->executeGroupFlush( $group );
			},
			static function (): bool {
				return false;
			}
		);
	}

	/**
	 * Initialises the cache (called from WordPress init hook).
	 *
	 * @return void
	 */
	public function init(): void
	{
		if ( isset( $GLOBALS['blog_id'] ) ) {
			$this->blogId = (int) $GLOBALS['blog_id'];
		}
	}

	/**
	 * Closes the connection. Work is deferred to shutdown(); this is a no-op.
	 *
	 * @return bool
	 */
	public function close(): bool
	{
		return true;
	}

	/**
	 * Shutdown hook: persist stats (for heartbeat), close connection.
	 *
	 * @return void
	 */
	public function shutdown(): void
	{
		// Mark the shutdown path so that any Redis failure inside persistStats()
		// does not write a spurious outage marker (the process is already dying).
		$this->inShutdown = true;
		try {
			$this->persistStats();
		} catch ( \Throwable $e ) {
			// Best-effort.
		}
		if ( $this->connection !== null ) {
			// pconnect handles stay pooled in the FPM worker; close is a no-op.
		}
	}

	/**
	 * Switches the blog context (multisite).
	 *
	 * @param int $blogId Blog ID to switch to.
	 * @return void
	 */
	public function switch_to_blog( int $blogId ): void
	{
		// H1: single-site installations ignore blog switches entirely.
		if ( ! $this->isMultisite ) {
			return;
		}
		if ( $this->blogId === $blogId ) {
			return;
		}
		$this->blogId       = $blogId;
		$this->wildcardMemo = []; // Invalidate memos when blog changes.
		$this->keyPrefixMemo = []; // Invalidate memoized key prefixes (H1).

		// Belt-and-braces: clear L1 entries that belong to the old blog (non-global).
		// Since L1 is now keyed by the fully-qualified Redis key (which includes the
		// blog-id segment), old-blog non-global keys will simply never match new-blog
		// buildKey() output — so there is no correctness issue. We do NOT wipe global
		// group L1 entries because they are shared across blogs by design.
	}

	/**
	 * Registers global groups.
	 *
	 * @param array<string> $groups Groups to add.
	 * @return void
	 */
	public function add_global_groups( array $groups ): void
	{
		foreach ( $groups as $group ) {
			if ( is_string( $group ) && $group !== '' ) {
				$this->globalGroups[ $group ] = true;
				// Memo invalidation: a late registration may change routing.
				unset( $this->wildcardMemo[ $group ] );
				// H1: invalidate keyPrefixMemo for this group since global flag changed.
				unset( $this->keyPrefixMemo[ $group . ':g' ] );
				unset( $this->keyPrefixMemo[ $group . ':' . $this->blogId ] );
			}
		}
	}

	/**
	 * Registers non-persistent groups.
	 *
	 * @param array<string> $groups Groups to add.
	 * @return void
	 */
	public function add_non_persistent_groups( array $groups ): void
	{
		foreach ( $groups as $group ) {
			if ( is_string( $group ) && $group !== '' ) {
				$this->nonPersistentGroups[ $group ] = true;
				unset( $this->wildcardMemo[ $group ] );
			}
		}
	}

	/**
	 * Registers non-prefetchable groups (v2 stub; stored for future prefetch).
	 *
	 * @param array<string> $groups Groups to add.
	 * @return void
	 */
	public function add_non_prefetchable_groups( array $groups ): void
	{
		foreach ( $groups as $group ) {
			if ( is_string( $group ) ) {
				$this->nonPrefetchableGroups[] = $group;
			}
		}
	}

	/**
	 * Reports whether a specific feature is supported.
	 *
	 * @param string $feature Feature name.
	 * @return bool
	 */
	public function supports( string $feature ): bool
	{
		return in_array( $feature, self::SUPPORTS, true );
	}

	/**
	 * M7: __get magic back-compat bridge for plugins that read internal properties.
	 * Exposes cache, global_groups, non_persistent_groups, multisite, blog_prefix.
	 * Triggers E_USER_WARNING on unknown property names.
	 *
	 * @param string $name Property name.
	 * @return mixed
	 */
	public function __get( string $name ): mixed
	{
		switch ( $name ) {
			case 'cache':
				// Return a group-indexed array copy for back-compat.
				$grouped = [];
				foreach ( $this->cache as $rk => $val ) {
					$parts = explode( ':', $rk, 4 );
					$group = count( $parts ) >= 3 ? $parts[ count( $parts ) - 2 ] : 'default';
					$key   = $parts[ count( $parts ) - 1 ] ?? $rk;
					$grouped[ $group ][ $key ] = $val;
				}
				return $grouped;
			case 'global_groups':
				return array_keys( $this->globalGroups );
			case 'non_persistent_groups':
				return array_keys( $this->nonPersistentGroups );
			case 'multisite':
				return $this->isMultisite;
			case 'blog_prefix':
				return $this->prefix . ':' . $this->blogId . ':';
			default:
				trigger_error( // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_trigger_error -- M7: back-compat bridge; E_USER_WARNING on unknown property
					'WPMgr_Object_Cache: Undefined property: ' . $name, // phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- trigger_error string is a PHP diagnostic, not HTML output
					E_USER_WARNING
				);
				return null;
		}
	}

	/**
	 * M7: __isset magic bridge — returns true for the back-compat properties.
	 *
	 * @param string $name Property name.
	 * @return bool
	 */
	public function __isset( string $name ): bool
	{
		return in_array( $name, [ 'cache', 'global_groups', 'non_persistent_groups', 'multisite', 'blog_prefix' ], true );
	}

	/**
	 * M7: Core-compatible stats() stub.
	 * Some plugins call $wp_object_cache->stats() for diagnostics output.
	 *
	 * @return void
	 */
	public function stats(): void
	{
		echo '<p><strong>WPMgr Object Cache</strong> (engine ' . esc_html( self::ENGINE_VERSION ) . ')</p>';
		echo '<ul>';
		echo '<li>Cache hits: ' . (int) $this->cache_hits . '</li>';
		echo '<li>Cache misses: ' . (int) $this->cache_misses . '</li>';
		echo '<li>Mode: ' . ( $this->arrayMode ? 'array (degraded)' : 'redis' ) . '</li>';
		echo '</ul>';
	}

	/**
	 * Whether the cache is in array-only (degraded) mode.
	 *
	 * @return bool
	 */
	public function isArrayMode(): bool
	{
		return $this->arrayMode;
	}

	/**
	 * Return the per-request error journal (for diagnostics/heartbeat).
	 *
	 * @return array<string>
	 */
	public function getErrorJournal(): array
	{
		return $this->errorJournal;
	}

	/**
	 * Return the loaded config array (for the debug-header emitter and diagnostics).
	 * Does not expose the password; callers must not log or emit this array verbatim.
	 *
	 * @return array<string,mixed>
	 */
	public function wpmgr_get_config(): array
	{
		return $this->config;
	}

	/**
	 * Record a Throwable class name caught in a wp_cache_* wrapper.
	 * Called from the global bridge functions so unexpected engine errors never
	 * escape to user-visible PHP fatals.
	 *
	 * @param string $class Throwable class name (from get_class($e)).
	 * @return void
	 */
	public function wpmgr_journal_wrapper_error( string $class ): void
	{
		$this->journalError( 'wrapper_catch', $class );
	}

	/**
	 * Return stats suitable for the heartbeat block.
	 *
	 * @return array<string,mixed>
	 */
	public function getHeartbeatStats(): array
	{
		$state = 'disabled';
		if ( $this->arrayMode && count( $this->errorJournal ) > 0 ) {
			$state = 'down';
		} elseif ( $this->arrayMode ) {
			$state = 'disabled';
		} elseif ( $this->connection !== null && $this->connection->isDegraded() ) {
			$state = 'degraded';
		} elseif ( $this->redis !== null ) {
			$state = 'connected';
		}

		$totalOps = $this->cache_hits + $this->cache_misses;
		$hitRatio = $totalOps > 0 ? round( $this->cache_hits / $totalOps * 100, 1 ) : 0.0;
		$latencyMs = $this->redisReads + $this->redisWrites > 0
			? round( $this->totalWaitMs / ( $this->redisReads + $this->redisWrites ), 2 )
			: 0.0;

		$lastError = $this->errorJournal !== [] ? $this->errorJournal[ count( $this->errorJournal ) - 1 ] : '';

		$stats = [
			'state'              => $state,
			'latency_ms'         => $latencyMs,
			'last_error_class'   => $lastError,
			'hit_ratio_window_pct' => $hitRatio,
			'engine_version'     => self::ENGINE_VERSION,
		];

		// FD-4: surface effective codec values and fallback cause for diagnostics.
		if ( $this->connection !== null ) {
			$stats['serializer_effective']   = $this->connection->effectiveSerializer();
			$stats['compression_effective']  = $this->connection->effectiveCompression();
			$codecFallback = $this->connection->codecFallbackCause();
			if ( $codecFallback !== '' ) {
				$stats['codec_fallback'] = $codecFallback;
			}
		}

		// used_memory_bytes: attempt a live INFO query (best-effort, no extra cost
		// if INFO is denied or throws).
		if ( $this->redis !== null && ! $this->arrayMode ) {
			try {
				$info = @$this->redis->info( 'memory' );
				if ( is_array( $info ) && isset( $info['used_memory'] ) ) {
					$stats['used_memory_bytes'] = (int) $info['used_memory'];
				}
			} catch ( \Throwable $e ) {
				// Best-effort; omit the field on denial.
			}
		}

		return $stats;
	}

	/**
	 * Build the value for the x-wpmgr-object-cache debug response header.
	 *
	 * Emits only safe, non-sensitive per-request counters. The value never
	 * contains Redis host, port, socket path, prefix, username, database index,
	 * key names, or engine version — only aggregate counters derived from the
	 * current request's activity.
	 *
	 * The header is emitted via the send_headers action, which WordPress fires
	 * only on front-end main requests. It does not fire on REST API, admin-ajax,
	 * WP-Cron, wp-admin, or wp-login.php requests; this is intentional and
	 * consistent with the page-cache header family.
	 *
	 * State ladder matches getHeartbeatStats(): connected | degraded | down | disabled.
	 *
	 * @return string Header value string.
	 */
	public function buildDebugHeaderValue(): string
	{
		$state = 'disabled';
		if ( $this->arrayMode && count( $this->errorJournal ) > 0 ) {
			$state = 'down';
		} elseif ( $this->arrayMode ) {
			$state = 'disabled';
		} elseif ( $this->connection !== null && $this->connection->isDegraded() ) {
			$state = 'degraded';
		} elseif ( $this->redis !== null ) {
			$state = 'connected';
		}

		$ms = ( $this->redisReads + $this->redisWrites ) > 0
			? round( $this->totalWaitMs / ( $this->redisReads + $this->redisWrites ), 2 )
			: 0.0;

		return sprintf(
			'state=%s; hits=%d; misses=%d; reads=%d; writes=%d; ms=%.2f',
			$state,
			$this->cache_hits,
			$this->cache_misses,
			$this->redisReads,
			$this->redisWrites,
			$ms
		);
	}

	// -------------------------------------------------------------------------
	// Internal: key building, group classification, TTL, L1
	// -------------------------------------------------------------------------

	/**
	 * Build a fully-qualified Redis key.
	 * Shape: prefix:[blogId:]group:key
	 *
	 * @param string $key   Cache key (already validated as string).
	 * @param string $group Normalized group name.
	 * @return string
	 */
	private function buildKey( string $key, string $group ): string
	{
		// H1: memoize the per-group prefix (prefix + optional blog segment + group + colon).
		// The memo is invalidated by switch_to_blog() (blogId changes) and
		// add_global_groups() (global flag changes). O(1) per call on warm path.
		$memoKey = $group . ':' . ( isset( $this->globalGroups[ $group ] ) ? 'g' : $this->blogId );
		if ( ! isset( $this->keyPrefixMemo[ $memoKey ] ) ) {
			if ( isset( $this->globalGroups[ $group ] ) ) {
				$this->keyPrefixMemo[ $memoKey ] = $this->prefix . ':' . $group . ':';
			} else {
				$this->keyPrefixMemo[ $memoKey ] = $this->prefix . ':' . $this->blogId . ':' . $group . ':';
			}
		}
		return $this->keyPrefixMemo[ $memoKey ] . $key;
	}

	/**
	 * Cast any scalar $group to string, matching WP core's loose-typed cache API.
	 *
	 * WP core cache.php declares $group as untyped with a string default. Callers
	 * may legally pass any scalar (int, float, bool, null). We cast to string so
	 * normalizeGroup always receives a string. Empty/falsy values become '' which
	 * normalizeGroup then maps to 'default'.
	 *
	 * @param mixed $group Raw group value from the caller.
	 * @return string
	 */
	private function castGroup( $group ): string
	{
		if ( is_string( $group ) ) {
			return $group;
		}
		// null, false, 0 => '' (normalizeGroup will convert to 'default').
		if ( $group === null || $group === false || $group === 0 || $group === 0.0 ) {
			return '';
		}
		return (string) $group;
	}

	/**
	 * Normalize a group string: trim + default to 'default'.
	 *
	 * @param string $group Raw group.
	 * @return string
	 */
	private function normalizeGroup( string $group ): string
	{
		$group = trim( $group );
		// LOW: '0' and '' both map to 'default' (mirrors core behaviour).
		return ( $group !== '' && $group !== '0' ) ? $group : 'default';
	}

	/**
	 * Sanitize and truncate the prefix to 32 characters, replacing unsafe chars.
	 *
	 * @param string $prefix Raw prefix.
	 * @return string
	 */
	private function sanitizePrefix( string $prefix ): string
	{
		$prefix = preg_replace( '/[^a-zA-Z0-9_-]/', '_', $prefix ) ?? 'wpmgr';
		$prefix = substr( $prefix, 0, 32 );
		// An empty prefix after sanitization defeats shared-Redis namespacing
		// and makes SCAN `:*` flush cross site boundaries. Fall back to 'wpmgr'.
		return $prefix !== '' ? $prefix : 'wpmgr';
	}

	/**
	 * Whether a group is non-persistent (runtime-only).
	 * Supports fnmatch wildcards in registered group names; results are memoized.
	 *
	 * @param string $group Normalized group.
	 * @return bool
	 */
	private function isNonPersistent( string $group ): bool
	{
		if ( isset( $this->nonPersistentGroups[ $group ] ) ) {
			return true;
		}
		// Wildcard match (memoized).
		if ( array_key_exists( 'np_' . $group, $this->wildcardMemo ) ) {
			return $this->wildcardMemo[ 'np_' . $group ];
		}
		foreach ( array_keys( $this->nonPersistentGroups ) as $pattern ) {
			if ( strpos( $pattern, '*' ) !== false && fnmatch( $pattern, $group ) ) {
				$this->wildcardMemo[ 'np_' . $group ] = true;
				return true;
			}
		}
		$this->wildcardMemo[ 'np_' . $group ] = false;
		return false;
	}

	/**
	 * Clamp a TTL: negative => 0 (delete), 0 or > maxttl => maxttl.
	 * Query groups get min(queryttl, maxttl).
	 *
	 * @param int    $ttl   Requested TTL.
	 * @param string $group Normalized group.
	 * @return int
	 */
	private function clampTtl( int $ttl, string $group ): int
	{
		// LOW: negative expire → 0 (use maxttl / delete-on-miss, not immediate expiry).
		if ( $ttl < 0 ) {
			$ttl = 0;
		}

		// LOW: query groups — suffix-match only (not substring anywhere in name).
		if ( substr( $group, -8 ) === '-queries' ) {
			$limit = min( $this->queryttl, $this->maxttl );
			if ( $ttl === 0 || $ttl > $limit ) {
				return $limit;
			}
			return $ttl;
		}

		if ( $ttl === 0 || $ttl > $this->maxttl ) {
			return $this->maxttl;
		}
		return $ttl;
	}

	/**
	 * Store a value in the L1 array cache with clone-on-store.
	 * H1: the flat key is the fully-qualified Redis key (buildKey() output).
	 *
	 * @param string $redisKey Fully-qualified Redis key from buildKey().
	 * @param mixed  $value    Value to store.
	 * @return bool Always true (for fluent chaining).
	 */
	private function storeL1( string $redisKey, mixed $value ): bool
	{
		$this->cache[ $redisKey ] = $this->cloneValue( $value );
		return true;
	}

	/**
	 * Clone an object or return a scalar/array as-is. Clone-on-read/store
	 * prevents by-reference mutation leaks.
	 *
	 * @param mixed $value Value to clone.
	 * @return mixed
	 */
	private function cloneValue( mixed $value ): mixed
	{
		return is_object( $value ) ? clone $value : $value;
	}

	/**
	 * Validate that a key is a string or integer. Non-valid keys are journaled.
	 *
	 * @param mixed $key Raw key.
	 * @return bool
	 */
	private function validateKey( mixed $key ): bool
	{
		if ( is_int( $key ) ) {
			return true; // Integers are always valid (M5: ints exempt from empty check).
		}
		if ( ! is_string( $key ) ) {
			$this->journalError( 'invalid_key', 'key must be int or string; got ' . gettype( $key ) );
			return false;
		}
		// M5: reject empty or whitespace-only string keys.
		if ( trim( $key ) === '' ) {
			$this->journalError( 'invalid_key', 'key must not be empty or whitespace-only' );
			return false;
		}
		return true;
	}

	// -------------------------------------------------------------------------
	// Internal: Redis operation wrapper with degradation
	// -------------------------------------------------------------------------

	/**
	 * Execute a Redis operation with per-op try/catch degradation.
	 *
	 * On failure: journal the error, try reconnect-once (only for idempotent
	 * reads), then fall back to the $onError result for the remainder of the
	 * request. The site never errors.
	 *
	 * @template T
	 * @param callable(): T $op          Redis operation.
	 * @param callable(): T $onError     Fallback when degraded.
	 * @param bool          $idempotent  Whether a read-timeout retry is safe.
	 * @return T
	 */
	private function redisOp( callable $op, callable $onError, bool $idempotent = false ): mixed
	{
		if ( $this->arrayMode || $this->redis === null || $this->connection === null ) {
			return $onError();
		}

		$t0 = microtime( true );

		try {
			$result = $op();
			$this->totalWaitMs += ( microtime( true ) - $t0 ) * 1000.0;
			$this->connection->recordSuccess();
			// H5: mid-request failback trigger REMOVED. The failback flush is now
			// handled exclusively at healthy boot via maybeFailbackFlushOnBoot().
			return $result;

		} catch ( \Throwable $e ) {
			$this->totalWaitMs += ( microtime( true ) - $t0 ) * 1000.0;
			$this->journalError( get_class( $e ), $e->getMessage() );

			// H5: persist the outage marker immediately on first degradation.
			// Exception: skip the marker write during PHP shutdown (process is
			// dying; a Redis failure in persistStats is not an actionable outage).
			if ( $this->connection !== null && ! $this->connection->isDegraded() ) {
				$this->connection->markDegraded();
				if ( ! $this->inShutdown ) {
					$this->persistOutageMarker();
				}
			} elseif ( $this->connection !== null ) {
				$this->connection->markDegraded();
			}

			// Attempt reconnect-once per request for idempotent reads.
			if ( $idempotent && ! $this->reconnectAttempted && $this->connection !== null ) {
				$this->reconnectAttempted = true;
				try {
					$this->redis = $this->connection->acquire();
					$t1 = microtime( true );
					$result = $op();
					$this->totalWaitMs += ( microtime( true ) - $t1 ) * 1000.0;
					return $result;
				} catch ( \Throwable $e2 ) {
					$this->journalError( 'reconnect_failed', $e2->getMessage() );
				}
			}

			return $onError();
		}
	}

	// -------------------------------------------------------------------------
	// Internal: flush strategies
	// -------------------------------------------------------------------------

	/**
	 * Execute the flush strategy for a full or site-scoped flush.
	 * FLUSHALL is never issued.
	 *
	 * @param string $scope 'all' | 'site' | 'group' (group handled separately).
	 * @return bool
	 */
	private function executeFlush( string $scope ): bool
	{
		$useFlushDb = false;

		if ( $this->flushStrategy === 'flushdb' && ! $this->shared ) {
			$useFlushDb = true;
		} elseif ( $this->flushStrategy === 'auto' && ! $this->shared ) {
			$useFlushDb = true;
		}

		if ( $useFlushDb && $this->redis !== null ) {
			// M8: phpredis 6 changed flushDB() — the boolean async flag is inverted.
			// phpredis < 6: true = async; phpredis >= 6: pass Redis::ASYNC_FLUSHDB (1) or none.
			// M9: save/restore read-timeout around sync FLUSHDB (H4-style try/finally).
			$savedTimeout = null;
			try {
				if ( ! $this->asyncFlush ) {
					$savedTimeout = $this->redis->getOption( \Redis::OPT_READ_TIMEOUT );
					$this->redis->setOption( \Redis::OPT_READ_TIMEOUT, (string) -1 );
				}
				// M8: version_compare gate for the flag argument.
				$phpredisVer = phpversion( 'redis' );
				if ( $phpredisVer !== false && version_compare( (string) $phpredisVer, '6.0.0', '>=' ) ) {
					// phpredis >= 6: pass no argument for sync flush (async is via ASYNC constant).
					if ( $this->asyncFlush && defined( 'Redis::FLUSHDB_ASYNC' ) ) {
						$this->redis->flushDB( (bool) constant( 'Redis::FLUSHDB_ASYNC' ) );
					} else {
						$this->redis->flushDB();
					}
				} else {
					$this->redis->flushDB( $this->asyncFlush );
				}
			} finally {
				// M9: always restore read timeout.
				if ( $savedTimeout !== null && $this->redis !== null ) {
					$this->redis->setOption( \Redis::OPT_READ_TIMEOUT, (string) $savedTimeout );
				}
			}
			return true;
		}

		// Shared or scan-only: SCAN+MATCH+UNLINK prefix-scoped.
		return $this->executeScanFlush( $this->prefix . ':' );
	}

	/**
	 * Execute a SCAN+MATCH+UNLINK flush scoped to the given pattern prefix.
	 * COUNT 500, inter-batch sleep (0.5ms) to bound instance impact.
	 *
	 * Uses the canonical phpredis SCAN idiom: by-ref integer iterator and
	 * SCAN_RETRY so phpredis handles empty-batch re-scanning internally,
	 * returning a flat key array (not the [cursor, keys] tuple used by Predis).
	 *
	 * @param string $prefixPattern Key prefix to match (e.g. "wpmgr:").
	 * @return bool
	 */
	private function executeScanFlush( string $prefixPattern ): bool
	{
		if ( $this->redis === null ) {
			return false;
		}

		$this->redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
		$it      = null;
		$pattern = $prefixPattern . '*';

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem; by-ref iterator is the canonical phpredis pattern
		while ( ( $keys = $this->redis->scan( $it, $pattern, 500 ) ) !== false ) {
			if ( ! empty( $keys ) ) {
				if ( $this->asyncFlush ) {
					$this->redis->unlink( ...$keys );
				} else {
					$this->redis->del( ...$keys );
				}
				usleep( 500 ); // 0.5ms inter-batch sleep to reduce instance impact.
			}
			if ( $it === 0 ) {
				break;
			}
		}

		return true;
	}

	/**
	 * Flush all keys for a specific group via SCAN+MATCH+UNLINK.
	 *
	 * The SCAN globs use '*' to span blog-ID and key segments, but '*' in Redis
	 * glob spans ':' — so the pattern `prefix:*:post:*` also matches a key like
	 * `prefix:1:postmeta:key` if the group substring appears as an interior
	 * token. Post-filter each SCAN batch: only UNLINK keys whose colon-delimited
	 * segments contain the exact group token at the correct position.
	 *
	 * Key shapes:
	 *   Global:   prefix:group:key
	 *   Per-blog: prefix:blogId:group:key
	 *
	 * @param string $group Normalized group.
	 * @return bool
	 */
	private function executeGroupFlush( string $group ): bool
	{
		// Match both global (no blog segment) and per-blog variants.
		$globalPattern = $this->prefix . ':' . $group . ':';
		$blogPattern   = $this->prefix . ':*:' . $group . ':';

		$this->executeScanFlushWithGroupFilter( $globalPattern, $group, false );
		$this->executeScanFlushWithGroupFilter( $blogPattern, $group, true );

		return true;
	}

	/**
	 * SCAN+MATCH+UNLINK with exact group-segment post-filter.
	 *
	 * After each SCAN batch the keys are filtered to those where the group
	 * token sits at the exact colon-segment position:
	 *   $hasBlogSegment=false: prefix:group:key     => segment index 1
	 *   $hasBlogSegment=true:  prefix:blogId:group:key => segment index 2
	 *
	 * @param string $prefixPattern SCAN MATCH pattern.
	 * @param string $group         Exact group name to confirm.
	 * @param bool   $hasBlogSegment Whether the pattern includes a blog-ID wildcard.
	 * @return void
	 */
	private function executeScanFlushWithGroupFilter( string $prefixPattern, string $group, bool $hasBlogSegment ): void
	{
		if ( $this->redis === null ) {
			return;
		}

		$pattern = $prefixPattern . '*';
		// Group segment index in the colon-delimited key:
		// Global key:   0=prefix, 1=group, 2+=key
		// Per-blog key: 0=prefix, 1=blogId, 2=group, 3+=key
		$groupSegmentIndex = $hasBlogSegment ? 2 : 1;

		// Canonical phpredis SCAN idiom: by-ref integer iterator, SCAN_RETRY,
		// flat key array return (not the [cursor, keys] tuple used by Predis).
		$this->redis->setOption( \Redis::OPT_SCAN, \Redis::SCAN_RETRY );
		$it = null;

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_scan -- phpredis SCAN command, not filesystem; by-ref iterator is the canonical phpredis pattern
		while ( ( $keys = $this->redis->scan( $it, $pattern, 500 ) ) !== false ) {
			if ( ! empty( $keys ) ) {
				// Post-filter: confirm the key's group segment is an exact match.
				$confirmed = [];
				foreach ( $keys as $k ) {
					$parts = explode( ':', (string) $k );
					if ( isset( $parts[ $groupSegmentIndex ] ) && $parts[ $groupSegmentIndex ] === $group ) {
						$confirmed[] = $k;
					}
				}
				if ( $confirmed !== [] ) {
					if ( $this->asyncFlush ) {
						$this->redis->unlink( ...$confirmed );
					} else {
						$this->redis->del( ...$confirmed );
					}
				}
				usleep( 500 ); // 0.5ms inter-batch sleep to reduce instance impact.
			}
			if ( $it === 0 ) {
				break;
			}
		}
	}

	// -------------------------------------------------------------------------
	// FD-6: Reconnect cool-down side channel
	// -------------------------------------------------------------------------

	/**
	 * FD-6: Derive the absolute path to the state file.
	 * Lives next to the config file in wp-content.
	 *
	 * @param array<string,mixed> $config Loaded config (unused directly; reserved for future scoping).
	 * @return string Absolute path, or '' when WP_CONTENT_DIR is unavailable.
	 */
	private function cooldownStatePath( array $config ): string
	{
		if ( ! class_exists( 'WPMgr\Agent\ObjectCache\ObjectCacheConfig' ) ) {
			return '';
		}
		$configLoader = new \WPMgr\Agent\ObjectCache\ObjectCacheConfig();
		$configDir    = dirname( $configLoader->filePath() );
		if ( $configDir === '' || $configDir === '.' ) {
			return '';
		}
		return $configDir . '/' . \WPMgr\Agent\ObjectCache\ObjectCacheConfig::STATE_FILENAME;
	}

	/**
	 * FD-6: Whether APCu is actually usable in this SAPI. function_exists alone
	 * is wrong: on the command line the extension is commonly loaded with
	 * apc.enable_cli off, so the functions exist but the store is inert and
	 * the state file fallback must be used instead.
	 *
	 * @return bool
	 */
	private function apcuUsable(): bool
	{
		return function_exists( 'apcu_enabled' ) && apcu_enabled();
	}

	/**
	 * FD-6: Read the cool-down state (APCu or state file).
	 * Returns array{last_failure_ts:float,consecutive_failures:int} or null when absent.
	 *
	 * @return array<string,mixed>|null
	 */
	private function readCooldownState(): ?array
	{
		// APCu is preferred: no disk I/O on the hot path.
		if ( $this->apcuUsable() ) {
			$val = apcu_fetch( 'wpmgr_oc_cooldown' );
			if ( is_array( $val ) ) {
				return $val;
			}
			return null;
		}
		// Fallback: state file.
		// Security: _state_path_override is only honored in test mode (WPMGR_OC_TEST_STATE_OVERRIDE).
		// In production the path is always derived from WP_CONTENT_DIR.
		$overrideActive = defined( 'WPMGR_OC_TEST_STATE_OVERRIDE' ) && WPMGR_OC_TEST_STATE_OVERRIDE === true;
		$path = ( $overrideActive && isset( $this->config['_state_path_override'] ) )
			? (string) $this->config['_state_path_override']
			: $this->cooldownStatePath( $this->config );
		if ( $path === '' || ! @is_file( $path ) ) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- silent is_file on optional state file; failure => no cooldown
			return null;
		}
		$raw = @file_get_contents( $path ); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents -- state file; WP_Filesystem not available at drop-in time
		if ( $raw === false ) {
			return null;
		}
		$decoded = json_decode( $raw, true );
		return is_array( $decoded ) ? $decoded : null;
	}

	/**
	 * FD-6: Write the cool-down state atomically.
	 *
	 * @param array<string,mixed> $state State to persist.
	 * @return void
	 */
	private function writeCooldownState( array $state ): void
	{
		if ( $this->apcuUsable() ) {
			apcu_store( 'wpmgr_oc_cooldown', $state, 600 );
			return;
		}
		// Security: _state_path_override is only honored in test mode (WPMGR_OC_TEST_STATE_OVERRIDE).
		$overrideActive = defined( 'WPMGR_OC_TEST_STATE_OVERRIDE' ) && WPMGR_OC_TEST_STATE_OVERRIDE === true;
		$path = ( $overrideActive && isset( $this->config['_state_path_override'] ) )
			? (string) $this->config['_state_path_override']
			: $this->cooldownStatePath( $this->config );
		if ( $path === '' ) {
			return;
		}
		$json = (string) json_encode( $state );
		// Atomic write: tmp file + rename with random suffix to avoid collisions.
		$tmp = $path . '.tmp.' . random_int( 1000000, 9999999 );
		$prev = umask( 0077 );
		$written = @file_put_contents( $tmp, $json ); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- state file; WP_Filesystem not available at drop-in time; atomic via tmp+rename
		umask( $prev );
		if ( $written !== false ) {
			@rename( $tmp, $path ); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic move; WP_Filesystem::move() is non-atomic

			// Ownership alignment: a privileged writer (command line provisioning)
			// leaves the state file unreadable by the web server user. Target the
			// owner of the WordPress core entry file the web server serves; the
			// chown attempt only succeeds for privileged processes and fails
			// silently otherwise. Never blocks the state write.
			try {
				$refFile = defined( 'ABSPATH' ) ? constant( 'ABSPATH' ) . 'index.php' : '';
				$ref     = ( $refFile !== '' && @is_file( $refFile ) ) ? $refFile : dirname( $path ); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort reference probe
				$owner   = @fileowner( $ref ); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort; failure skips chown
				$group   = @filegroup( $ref ); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort; failure skips chgrp
				$current = @fileowner( $path ); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort current-owner probe
				if ( $owner !== false && $owner !== 0 && $owner !== $current ) {
					// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chown,WordPress.PHP.NoSilencedErrors.Discouraged -- headless agent; WP_Filesystem not initialised; best-effort ownership alignment; never fatal
					@chown( $path, $owner );
					if ( $group !== false ) {
						// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chgrp,WordPress.PHP.NoSilencedErrors.Discouraged -- headless agent; WP_Filesystem not initialised; best-effort group alignment; never fatal
						@chgrp( $path, $group );
					}
				}
			} catch ( \Throwable $_ ) {
				// Best-effort: chown/chgrp failed; state file still written.
			}
		} else {
			@unlink( $tmp ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- cleanup tmp file on failure
		}
	}

	/**
	 * FD-6: Check whether the reconnect cool-down is active.
	 * Returns a cause string ('cooldown') when inside the backoff window,
	 * null when the boot should proceed normally.
	 *
	 * @param array<string,mixed> $config Loaded config.
	 * @return string|null 'cooldown' when inside the window, null otherwise.
	 */
	private function checkCooldown( array $config ): ?string
	{
		$state = $this->readCooldownState();
		if ( $state === null ) {
			return null;
		}
		$lastFailure  = (float) ( $state['last_failure_ts'] ?? 0.0 );
		$consecutive  = (int) ( $state['consecutive_failures'] ?? 0 );
		if ( $lastFailure <= 0.0 || $consecutive <= 0 ) {
			return null;
		}
		// 15s doubling per consecutive failure, capped at 300s.
		$backoff = min( 15 * (int) pow( 2, $consecutive - 1 ), 300 );
		// Use time() directly: boot runs before WP; current_time() is unavailable.
		$elapsed = time() - (int) $lastFailure;
		// MEDIUM-1: fail OPEN on implausible elapsed — a large-negative value signals
		// a future timestamp (backward NTP step, VM snapshot restore, or tampered
		// state file). Treat as no cool-down so we never enter permanent silent
		// array-mode. A non-positive last_failure_ts is already rejected above.
		if ( $elapsed < 0 ) {
			return null;
		}
		if ( $elapsed < $backoff ) {
			return 'cooldown';
		}
		return null;
	}

	/**
	 * FD-6: Record a boot connect failure into the cool-down state.
	 *
	 * @param array<string,mixed> $config Loaded config.
	 * @return void
	 */
	private function recordCooldownFailure( array $config ): void
	{
		$existing    = $this->readCooldownState();
		$consecutive = isset( $existing['consecutive_failures'] ) ? (int) $existing['consecutive_failures'] + 1 : 1;
		$this->writeCooldownState( [
			'last_failure_ts'    => (float) time(),
			'consecutive_failures' => $consecutive,
		] );
	}

	/**
	 * FD-6: Clear the cool-down state after a successful connect.
	 *
	 * @param array<string,mixed> $config Loaded config.
	 * @return void
	 */
	private function clearCooldownState( array $config ): void
	{
		if ( $this->apcuUsable() ) {
			apcu_delete( 'wpmgr_oc_cooldown' );
			return;
		}
		// Security: _state_path_override is only honored in test mode (WPMGR_OC_TEST_STATE_OVERRIDE).
		$overrideActive = defined( 'WPMGR_OC_TEST_STATE_OVERRIDE' ) && WPMGR_OC_TEST_STATE_OVERRIDE === true;
		$path = ( $overrideActive && isset( $this->config['_state_path_override'] ) )
			? (string) $this->config['_state_path_override']
			: $this->cooldownStatePath( $this->config );
		if ( $path !== '' && @is_file( $path ) ) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- silent check on optional file
			@unlink( $path ); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- state file cleanup
		}
	}

	/**
	 * H5: Persist an outage marker to wp-options immediately on first degradation.
	 * This ensures the next healthy boot knows a flush is needed, even if the
	 * current request's shutdown hook never runs.
	 *
	 * @return void
	 */
	private function persistOutageMarker(): void
	{
		// Skip during PHP shutdown: the process is dying and this is not an
		// actionable outage. The guard in redisOp() prevents reaching here in
		// the normal code path; this inner guard makes the contract testable
		// even when the method is called directly (e.g. from reflection-based
		// unit tests that set $inShutdown = true).
		if ( $this->inShutdown ) {
			return;
		}
		if ( ! function_exists( 'update_option' ) ) {
			return;
		}
		// Best-effort; do not let a DB error surface.
		try {
			update_option( self::FAILBACK_MARKER_OPTION, (string) microtime( true ), false );
		} catch ( \Throwable $e ) {
			// Best-effort.
		}
	}

	/**
	 * H5: On healthy boot, check for a persisted outage marker.
	 * If found, attempt a SET NX EX 300 lock — only the winner flushes.
	 * Clears the marker after a successful flush.
	 *
	 * @return void
	 */
	private function maybeFailbackFlushOnBoot(): void
	{
		if ( ! $this->flushOnFailback ) {
			return;
		}
		if ( ! function_exists( 'get_option' ) || ! function_exists( 'delete_option' ) ) {
			return;
		}
		if ( $this->redis === null || $this->arrayMode ) {
			return;
		}

		$marker = get_option( self::FAILBACK_MARKER_OPTION, false );
		if ( $marker === false ) {
			return; // No outage recorded.
		}

		// Attempt NX lock: only one request flushes.
		$lockKey = $this->prefix . ':' . self::FAILBACK_LOCK_SUFFIX;
		try {
			$won = $this->redis->set( $lockKey, '1', [ 'nx', 'ex' => 300 ] );
		} catch ( \Throwable $e ) {
			return; // Redis still down; skip.
		}

		if ( $won !== true ) {
			return; // Another request is already flushing.
		}

		$this->failbackFlushed = true;
		try {
			// H5 FIX: delete the marker BEFORE flushing. FLUSHDB wipes the NX
			// lock key written above; if the marker delete happened after the
			// flush it would be racing against the now-absent lock, allowing a
			// second healthy-boot request to see the marker and flush again.
			delete_option( self::FAILBACK_MARKER_OPTION );
			$this->executeFlush( 'all' );
			// WP_DEBUG forensics: log when a failback flush actually fires so the
			// E2E container log reveals whether and why a flush occurred.
			if ( defined( 'WP_DEBUG' ) && WP_DEBUG ) {
				// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- WP_DEBUG-gated diagnostic journal; not shipped to end-users
				error_log( sprintf(
					'wpmgr-object-cache: failback flush executed (marker_ts=%s)',
					is_string( $marker ) ? $marker : 'unknown'
				) );
			}
		} catch ( \Throwable $e ) {
			$this->journalError( 'failback_flush_failed', $e->getMessage() );
		}
	}

	// -------------------------------------------------------------------------
	// Internal: metadata integrity
	// -------------------------------------------------------------------------

	/**
	 * Metadata integrity key. Written raw (no serializer/compression) so it
	 * survives serializer changes. maxttl-exempt.
	 *
	 * @return string
	 */
	private function metadataKey(): string
	{
		return $this->prefix . ':__wpmgr_oc_meta__';
	}

	/**
	 * Check the metadata integrity key. If risky options changed, flush and
	 * rewrite the metadata key.
	 *
	 * @param array<string,mixed> $config Current config.
	 * @return void
	 */
	private function checkMetadataIntegrity( array $config ): void
	{
		if ( $this->redis === null ) {
			return;
		}

		$metaKey = $this->metadataKey();

		// FD-4: use EFFECTIVE codec values post-fallback from the connection, not
		// the configured (requested) values. This ensures checkMetadataIntegrity
		// detects igbinary->php or zstd->none transitions correctly and performs
		// a one-time flush on the transition instead of serving unreadable blobs.
		$effectiveSerializer  = ( $this->connection !== null )
			? $this->connection->effectiveSerializer()
			: ( $config['serializer'] ?? 'php' );
		$effectiveCompression = ( $this->connection !== null )
			? $this->connection->effectiveCompression()
			: ( $config['compression'] ?? 'none' );

		// H4: always use try/finally to restore OPT_SERIALIZER.
		$savedSerializer = $this->redis->getOption( \Redis::OPT_SERIALIZER );

		try {
			// Switch to no-serializer for the raw read.
			$this->redis->setOption( \Redis::OPT_SERIALIZER, (string) \Redis::SERIALIZER_NONE );

			// H4: 2 short retries on the metadata GET.
			$stored = false;
			for ( $attempt = 0; $attempt < 2; $attempt++ ) {
				try {
					$stored = $this->redis->get( $metaKey );
					break;
				} catch ( \Throwable $e ) {
					if ( $attempt === 1 ) {
						throw $e; // Re-throw after second failure.
					}
				}
			}

			if ( $stored !== false && is_string( $stored ) ) {
				$meta = json_decode( $stored, true );
				if ( is_array( $meta ) ) {
					$riskyChanged = false;
					// Treat absent old fields as unchanged (no false integrity flush on upgrade).
					if ( isset( $meta['serializer'] ) && $meta['serializer'] !== $effectiveSerializer ) {
						$riskyChanged = true;
					}
					if ( isset( $meta['compression'] ) && $meta['compression'] !== $effectiveCompression ) {
						$riskyChanged = true;
					}
					if ( isset( $meta['database'] ) && (int) $meta['database'] !== (int) ( $config['database'] ?? 0 ) ) {
						$riskyChanged = true;
					}
					// M10: wp_version change triggers integrity flush.
					$currentWpVer = isset( $GLOBALS['wp_version'] ) ? (string) $GLOBALS['wp_version'] : '';
					if ( isset( $meta['wp_version'] ) && $currentWpVer !== '' && $meta['wp_version'] !== $currentWpVer ) {
						$riskyChanged = true;
					}
					if ( $riskyChanged ) {
						// Restore serializer before flush (which may call Redis ops).
						$this->redis->setOption( \Redis::OPT_SERIALIZER, (string) $savedSerializer );
						$this->executeFlush( 'all' );
						// Switch back to NONE for the write below.
						$this->redis->setOption( \Redis::OPT_SERIALIZER, (string) \Redis::SERIALIZER_NONE );
						if ( defined( 'WP_DEBUG' ) && WP_DEBUG ) {
							// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- WP_DEBUG-gated diagnostic
							error_log( 'WPMgr Object Cache: integrity flush triggered by config change.' );
						}
					}
				}
			}

			// Write/rewrite the metadata key (raw bytes, no TTL).
			// H4: second NONE window — also protected by the outer try/finally.
			$newMeta = (string) wp_json_encode( [
				'database'             => (int) ( $config['database'] ?? 0 ),
				'prefix'               => $this->prefix,
				'serializer'           => $effectiveSerializer,
				'effective_serializer' => $effectiveSerializer,
				'compression'          => $effectiveCompression,
				'effective_compression' => $effectiveCompression,
				'wp_version'           => isset( $GLOBALS['wp_version'] ) ? (string) $GLOBALS['wp_version'] : '',
			] );
			$this->redis->set( $metaKey, $newMeta ); // maxttl-exempt: no TTL.

		} catch ( \Throwable $e ) {
			// Tolerate metadata key failures gracefully.
			$this->journalError( 'metadata_integrity_failed', $e->getMessage() );
		} finally {
			// H4: always restore OPT_SERIALIZER.
			if ( $this->redis !== null ) {
				$this->redis->setOption( \Redis::OPT_SERIALIZER, (string) $savedSerializer );
			}
		}
	}

	// -------------------------------------------------------------------------
	// Internal: stats persistence
	// -------------------------------------------------------------------------

	/**
	 * Persist aggregated stats for the heartbeat block.
	 *
	 * ACCUMULATES this request's counters into the wp-option so that the
	 * heartbeat can consume window-delta values (hit_count, miss_count,
	 * ops, wait_ms) across multiple requests between heartbeat pushes.
	 * The heartbeat consumer reads the accumulated deltas and resets them
	 * (consume-and-reset pattern).
	 *
	 * The STATE SNAPSHOT fields (state, latency_ms, last_error_class,
	 * hit_ratio_window_pct, engine_version) are persisted UNCONDITIONALLY so
	 * the heartbeat always has a fresh snapshot to report, even when analytics
	 * is disabled. The delta accumulation fields are gated on analytics_enabled.
	 * Missing analytics_enabled is treated as ON (matching the m68 default).
	 *
	 * @return void
	 */
	private function persistStats(): void
	{
		if ( ! function_exists( 'update_option' ) || ! function_exists( 'get_option' ) ) {
			return;
		}

		// Compute per-request snapshot fields (state, latency, last error).
		// These are written unconditionally so the heartbeat can always read
		// a fresh live snapshot, independent of the analytics setting.
		$snapshot = $this->getHeartbeatStats();

		// Read the existing accumulated option (default empty array).
		$existing = get_option( 'wpmgr_object_cache_stats', [] );
		if ( ! is_array( $existing ) ) {
			$existing = [];
		}

		// Analytics-gated: accumulate delta counters only when analytics is on.
		// Missing analytics_enabled is treated as ON (the default).
		$analyticsOn = ! isset( $this->config['analytics_enabled'] ) || (bool) $this->config['analytics_enabled'];

		if ( $analyticsOn ) {
			// Accumulate cumulative delta counters into the stored option.
			// These are consumed-and-reset by ObjectCacheHeartbeat::build().
			$totalOps = $this->redisReads + $this->redisWrites;

			$merged = array_merge( $snapshot, [
				// Carry forward any unconsumed deltas from prior requests.
				'delta_hit_count'   => ( isset( $existing['delta_hit_count'] ) ? (int) $existing['delta_hit_count'] : 0 )
					+ $this->cache_hits,
				'delta_miss_count'  => ( isset( $existing['delta_miss_count'] ) ? (int) $existing['delta_miss_count'] : 0 )
					+ $this->cache_misses,
				'delta_ops'         => ( isset( $existing['delta_ops'] ) ? (int) $existing['delta_ops'] : 0 )
					+ $totalOps,
				'delta_wait_ms'     => ( isset( $existing['delta_wait_ms'] ) ? (float) $existing['delta_wait_ms'] : 0.0 )
					+ $this->totalWaitMs,
				'delta_sample_count' => ( isset( $existing['delta_sample_count'] ) ? (int) $existing['delta_sample_count'] : 0 )
					+ ( $totalOps > 0 ? 1 : 0 ),
				// Timestamp of the first un-consumed delta (for ops_per_sec calculation).
				'delta_since_ts'    => isset( $existing['delta_since_ts'] ) && (float) $existing['delta_since_ts'] > 0
					? (float) $existing['delta_since_ts']
					: microtime( true ),
			] );
		} else {
			// Analytics off: persist only the state snapshot; preserve any
			// existing unconsumed delta fields so they are not silently lost.
			$merged = array_merge( $existing, $snapshot );
		}

		update_option( 'wpmgr_object_cache_stats', $merged, false );
	}

	// -------------------------------------------------------------------------
	// Internal: error journal
	// -------------------------------------------------------------------------

	/**
	 * Add an entry to the per-request error journal.
	 *
	 * @param string $class   Error class name.
	 * @param string $message Error message.
	 * @return void
	 */
	private function journalError( string $class, string $message ): void
	{
		if ( count( $this->errorJournal ) >= self::MAX_JOURNAL ) {
			array_shift( $this->errorJournal );
		}
		$this->errorJournal[] = $class;

		// LOW: append to $GLOBALS['wpmgr_object_cache_errors'], a plugin-owned
		// diagnostic journal (not a WP core global).
		if ( ! isset( $GLOBALS['wpmgr_object_cache_errors'] ) || ! is_array( $GLOBALS['wpmgr_object_cache_errors'] ) ) {
			$GLOBALS['wpmgr_object_cache_errors'] = [];
		}
		$GLOBALS['wpmgr_object_cache_errors'][] = '[' . $class . '] ' . $message;

		if ( defined( 'WP_DEBUG' ) && WP_DEBUG ) {
			// phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- WP_DEBUG-gated diagnostic
			error_log( 'WPMgr Object Cache error [' . $class . ']: ' . $message );
		}
	}
}
