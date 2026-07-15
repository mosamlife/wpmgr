<?php
/**
 * Engine core contract tests for 0.42.0.
 *
 * Covers M1–M16 items and LOW one-liners that can be verified in array mode
 * (no Redis needed). For items that need a Redis spy the test uses a
 * minimal in-process substitute that tracks calls.
 *
 * @package WPMgr\Agent\Tests\ObjectCache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\ObjectCache;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr_Object_Cache
 */
final class EngineCoreContractTest extends TestCase
{
	/** @var \WPMgr_Object_Cache */
	private \WPMgr_Object_Cache $oc;

	protected function set_up(): void
	{
		parent::set_up();
		$this->oc = \WPMgr_Object_Cache::boot();
	}

	// -------------------------------------------------------------------------
	// M1 — delete/delete_multiple false-on-missing
	// -------------------------------------------------------------------------

	public function test_delete_missing_returns_false(): void
	{
		$result = $this->oc->delete( 'never_set_key_xyz' );
		$this->assertFalse( $result, 'delete() on a missing key must return false' );
	}

	public function test_delete_existing_returns_true(): void
	{
		$this->oc->set( 'del_existing', 'v' );
		$result = $this->oc->delete( 'del_existing' );
		$this->assertTrue( $result, 'delete() on an existing key must return true' );
	}

	public function test_delete_multiple_missing_returns_all_false(): void
	{
		$result = $this->oc->delete_multiple( [ 'nx1', 'nx2' ], 'default' );
		$this->assertSame(
			[ 'nx1' => false, 'nx2' => false ],
			$result,
			'delete_multiple() on missing keys must return false for each'
		);
	}

	// -------------------------------------------------------------------------
	// M2 — get($force) on non-persistent / array-mode serves L1
	// -------------------------------------------------------------------------

	public function test_get_force_still_hits_l1_in_array_mode(): void
	{
		$this->oc->set( 'force_key', 'v_l1', 'default', 60 );
		$found  = null;
		// In array mode, $force=true must not skip L1 (there is no Redis to re-read from).
		$result = $this->oc->get( 'force_key', 'default', true, $found );
		$this->assertTrue( $found, 'get($force=true) in array mode must return L1 hit' );
		$this->assertSame( 'v_l1', $result );
	}

	public function test_get_force_non_persistent_group_hits_l1(): void
	{
		$this->oc->set( 'npkey', 'np_val', 'counts', 60 );
		$found  = null;
		$result = $this->oc->get( 'npkey', 'counts', true, $found );
		$this->assertTrue( $found, 'get($force=true) on non-persistent group must return L1 hit' );
		$this->assertSame( 'np_val', $result );
	}

	// -------------------------------------------------------------------------
	// M3 — set/set_multiple L1 only on success (array mode always succeeds)
	// -------------------------------------------------------------------------

	public function test_set_stores_l1_in_array_mode(): void
	{
		$ok = $this->oc->set( 'm3_key', 'val', 'default', 60 );
		$this->assertTrue( $ok );
		$found  = null;
		$result = $this->oc->get( 'm3_key', 'default', false, $found );
		$this->assertTrue( $found );
		$this->assertSame( 'val', $result );
	}

	// -------------------------------------------------------------------------
	// M4 — replace() stored-false fixture
	// -------------------------------------------------------------------------

	public function test_replace_with_stored_false_value(): void
	{
		$this->oc->set( 'm4_key', false, 'default', 60 );
		// Key exists with value false — replace must succeed.
		$ok = $this->oc->replace( 'm4_key', 'replaced', 'default', 60 );
		$this->assertTrue( $ok, 'replace() must succeed when key exists with a false value' );
		$result = $this->oc->get( 'm4_key', 'default' );
		$this->assertSame( 'replaced', $result );
	}

	public function test_replace_missing_key_returns_false(): void
	{
		$result = $this->oc->replace( 'absent_m4', 'v' );
		$this->assertFalse( $result, 'replace() must return false when key is absent' );
	}

	// -------------------------------------------------------------------------
	// M5 — empty/whitespace key rejected
	// -------------------------------------------------------------------------

	public function test_set_empty_string_key_returns_false(): void
	{
		$result = $this->oc->set( '', 'value' );
		$this->assertFalse( $result, 'set() with empty string key must return false' );
	}

	public function test_set_whitespace_only_key_returns_false(): void
	{
		$result = $this->oc->set( '   ', 'value' );
		$this->assertFalse( $result, 'set() with whitespace-only key must return false' );
	}

	public function test_set_int_key_is_allowed(): void
	{
		// Integer keys are exempt from the empty/whitespace check.
		$result = $this->oc->set( 0, 'int_key_val' );
		$this->assertTrue( $result, 'set() with int key 0 must be allowed (int exempt from empty check)' );
	}

	// -------------------------------------------------------------------------
	// M6 — get_multiple input-order preservation
	// -------------------------------------------------------------------------

	public function test_get_multiple_preserves_input_order(): void
	{
		$this->oc->set( 'gm_c', 'C', 'default', 60 );
		$this->oc->set( 'gm_a', 'A', 'default', 60 );
		// Key 'gm_b' is absent.
		$keys   = [ 'gm_c', 'gm_b', 'gm_a' ];
		$result = $this->oc->get_multiple( $keys, 'default', false );
		$this->assertSame( $keys, array_keys( $result ), 'get_multiple must preserve input key order' );
		$this->assertSame( 'C', $result['gm_c'] );
		$this->assertFalse( $result['gm_b'] );
		$this->assertSame( 'A', $result['gm_a'] );
	}

	// -------------------------------------------------------------------------
	// M7 — __get/__isset back-compat bridge
	// -------------------------------------------------------------------------

	public function test_magic_get_cache_does_not_fatal(): void
	{
		// Reading ->cache must not throw; returns array.
		$cache = $this->oc->cache;
		$this->assertIsArray( $cache );
	}

	public function test_magic_get_global_groups_does_not_fatal(): void
	{
		$groups = $this->oc->global_groups;
		$this->assertIsArray( $groups );
	}

	public function test_magic_isset_multisite(): void
	{
		$result = isset( $this->oc->multisite );
		$this->assertIsBool( $result );
	}

	// -------------------------------------------------------------------------
	// LOW — group '0' normalized to 'default'
	// -------------------------------------------------------------------------

	public function test_group_zero_normalized_to_default(): void
	{
		$this->oc->set( 'k_grp0', 'val', '0', 60 );
		$found  = null;
		$result = $this->oc->get( 'k_grp0', '0', false, $found );
		$this->assertTrue( $found, "group '0' should normalize to 'default' and be retrievable" );
		$this->assertSame( 'val', $result );
	}

	// -------------------------------------------------------------------------
	// LOW — negative expire clamped to 0
	// -------------------------------------------------------------------------

	public function test_negative_expire_is_accepted(): void
	{
		// Negative TTL must not crash; it is clamped to 0 (meaning: no expiry in array mode).
		$ok = $this->oc->set( 'neg_ttl', 'v', 'default', -1 );
		$this->assertTrue( $ok );
	}

	// -------------------------------------------------------------------------
	// LOW — 'comment' and 'themes' removed from DEFAULT_NON_PERSISTENT
	// -------------------------------------------------------------------------

	public function test_comment_not_in_non_persistent(): void
	{
		$this->oc->set( 'c1', 'val', 'comment', 60 );
		$found  = null;
		// 'comment' is no longer non-persistent; the value must survive (in array mode it is always L1).
		$this->oc->get( 'c1', 'comment', false, $found );
		$this->assertTrue( $found, "'comment' group must not be non-persistent in 0.42.0" );
	}

	// -------------------------------------------------------------------------
	// LOW — wpmgr_object_cache_errors journal appended in journalError
	// -------------------------------------------------------------------------

	public function test_invalid_key_appends_to_global_errors(): void
	{
		// Clear the global errors array.
		$GLOBALS['wpmgr_object_cache_errors'] = [];
		// Trigger a validation failure.
		$this->oc->set( '', 'val' );
		// The global errors array must have been appended to.
		$errors = $GLOBALS['wpmgr_object_cache_errors'] ?? [];
		$this->assertNotEmpty( $errors, 'journalError must append to $GLOBALS[wpmgr_object_cache_errors]' );
	}

	// -------------------------------------------------------------------------
	// LOW — bridge functions exist
	// -------------------------------------------------------------------------

	public function test_wp_cache_remember_defined(): void
	{
		$this->assertTrue( function_exists( 'wp_cache_remember' ), 'wp_cache_remember() must be defined' );
	}

	public function test_wp_cache_sear_defined(): void
	{
		$this->assertTrue( function_exists( 'wp_cache_sear' ), 'wp_cache_sear() must be defined' );
	}

	public function test_wp_cache_supports_group_flush_defined(): void
	{
		$this->assertTrue( function_exists( 'wp_cache_supports_group_flush' ), 'wp_cache_supports_group_flush() must be defined' );
	}

	public function test_wp_cache_reset_defined(): void
	{
		$this->assertTrue( function_exists( 'wp_cache_reset' ), 'wp_cache_reset() must be defined' );
	}

	public function test_wp_cache_remember_returns_callback_result_on_miss(): void
	{
		if ( ! function_exists( 'wp_cache_remember' ) ) {
			$this->markTestSkipped( 'wp_cache_remember not defined' );
		}
		$result = wp_cache_remember( 'rem_miss', 'default', 60, static fn() => 'computed' );
		$this->assertSame( 'computed', $result );
	}

	public function test_wp_cache_remember_returns_cached_on_hit(): void
	{
		if ( ! function_exists( 'wp_cache_remember' ) ) {
			$this->markTestSkipped( 'wp_cache_remember not defined' );
		}
		wp_cache_set( 'rem_hit', 'cached', 'default' );
		$result = wp_cache_remember( 'rem_hit', 'default', 60, static fn() => 'computed' );
		$this->assertSame( 'cached', $result );
	}

	public function test_wp_cache_supports_group_flush_returns_bool(): void
	{
		if ( ! function_exists( 'wp_cache_supports_group_flush' ) ) {
			$this->markTestSkipped( 'wp_cache_supports_group_flush not defined' );
		}
		$this->assertIsBool( wp_cache_supports_group_flush() );
	}

	// -------------------------------------------------------------------------
	// M14 — Performance Lab filter (unit: only verifies filter is registered when
	//        the state is ours-current; in test env the state is unknown so we
	//        just verify the constant is known to the installer)
	// -------------------------------------------------------------------------

	public function test_dropin_installer_state_constant_exists(): void
	{
		$this->assertTrue(
			defined( '\WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller::STATE_OURS_CURRENT' ),
			'STATE_OURS_CURRENT constant must be defined on ObjectCacheDropinInstaller'
		);
	}

	// -------------------------------------------------------------------------
	// M16 — bridge de-typing: wp_cache_get signature has no strict types
	//        (verified in ObjectCacheDropinBuildTest; here we test the behavior)
	// -------------------------------------------------------------------------

	public function test_int_group_accepted_without_type_error(): void
	{
		// int group — must not throw TypeError in loose-typed WP caller scenario.
		$ok = $this->oc->set( 'as_k', true, 3600, 300 );
		$this->assertTrue( $ok );
		$found  = null;
		$result = $this->oc->get( 'as_k', 3600, false, $found );
		$this->assertTrue( $found );
		$this->assertTrue( $result );
	}
}
