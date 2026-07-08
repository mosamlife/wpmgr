<?php
/**
 * UpdateRunnerCompletenessTest — S6 (GitHub issue #131 adversarial review)
 * and the BLOCKER fix (issue #131 final-hardening review) for
 * UpdateRunner::isComplete()'s plugin path.
 *
 * BLOCKER: isComplete() no longer contains ANY vendor/autoload.php
 * sub-check (it was deleted entirely — see UpdateRunner::isComplete()'s
 * class doc for why: it could not distinguish a defensive
 * `if (file_exists(...)) require ...` idiom, or a bare comment, from an
 * unconditional require, and false-reverted a legitimately good update that
 * simply shipped without a `vendor/` directory). The tests below prove
 * isComplete() is now governed purely by validate_plugin()/get_plugins(),
 * regardless of what a plugin's main-file source references or what exists
 * on disk under `vendor/`.
 *
 * S6: a plugin whose main file was renamed by an update must still resolve
 * by FOLDER via isComplete()'s fallback — this needs no filesystem I/O at
 * all (get_plugins()/validate_plugin() are Brain-Monkey-stubbed), so it is
 * fully order-independent regardless of WP_PLUGIN_DIR's global state.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 */
final class UpdateRunnerCompletenessTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-completeness-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0755, true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    // =========================================================================
    // BLOCKER — the vendor/autoload.php sub-check is gone; a defensive
    // reference to it (or its absence on disk) must never revert a good update
    // =========================================================================

    public function test_isComplete_true_for_a_plugin_that_defensively_references_vendor_autoload_but_ships_without_it(): void
    {
        // The exact false-positive scenario the removed S3 check produced: a
        // main file that references vendor/autoload.php only defensively
        // (`if (file_exists(...)) { require ...; }`) and genuinely ships
        // WITHOUT a vendor/ directory at all (a dev-only Composer
        // dependency, a "lite" build, or a graceful-degradation pattern) —
        // this is a perfectly GOOD, complete apply and must be reported
        // complete, not reverted.
        $dir = $this->root . '/defensive-vendor-plugin';
        mkdir($dir, 0755, true);
        $mainFile = $dir . '/main.php';
        file_put_contents(
            $mainFile,
            "<?php\n"
            . "// Defensive Composer autoload — this plugin ships without vendor/ in production.\n"
            . "if ( file_exists( __DIR__ . '/vendor/autoload.php' ) ) {\n"
            . "    require __DIR__ . '/vendor/autoload.php';\n"
            . "}\n"
        );
        // vendor/autoload.php deliberately NOT created — this must NOT be
        // treated as a half-write; the sub-check that used to flag this no
        // longer exists at all.
        $this->assertFalse(is_dir($dir . '/vendor'), 'precondition: no vendor/ directory exists');

        Functions\when('get_plugins')->justReturn([
            'defensive-vendor-plugin/main.php' => ['Name' => 'Defensive Vendor Plugin', 'Version' => '2.0'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertTrue(
            $runner->isComplete('plugin', 'defensive-vendor-plugin/main.php'),
            'BLOCKER: a plugin that defensively references vendor/autoload.php but ships without it must be reported COMPLETE, never auto-reverted'
        );
    }

    public function test_isComplete_governed_purely_by_validate_plugin_regardless_of_vendor_autoload_references(): void
    {
        // Order-independent control: even a main file that UNCONDITIONALLY
        // requires vendor/autoload.php, with the path genuinely missing,
        // must now be judged solely by validate_plugin()/get_plugins() — the
        // vendor sub-check is not merely narrowed, it is GONE.
        Functions\when('get_plugins')->justReturn([
            'demo/demo.php' => ['Name' => 'Demo', 'Version' => '2.0'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertTrue($runner->isComplete('plugin', 'demo/demo.php'));
    }

    // =========================================================================
    // S6 — renamed-main-file plugin resolves by folder
    // =========================================================================

    public function test_isComplete_resolves_a_renamed_main_file_by_folder_and_does_not_false_fail(): void
    {
        // The CP sends the FULL basename slug the site had installed BEFORE
        // the update ("folder/oldmain.php"). The update renamed the
        // bootstrap file to "folder/newmain.php" — a completely normal,
        // GOOD outcome — so get_plugins() no longer has an "folder/oldmain.php"
        // key at all.
        Functions\when('get_plugins')->justReturn([
            'kadence-blocks/kadence-blocks-main.php' => ['Name' => 'Kadence Blocks', 'Version' => '3.3.0'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertTrue(
            $runner->isComplete('plugin', 'kadence-blocks/kadence-blocks.php'),
            'S6: a plugin whose main file was renamed by a GOOD update must resolve by folder, not be false-rolled-back'
        );
    }

    public function test_isInstalled_also_resolves_a_renamed_main_file_by_folder(): void
    {
        // Same folder-derivation bug existed in isInstalled()'s identical
        // fallback loop — fixed alongside isComplete() for consistency.
        Functions\when('get_plugins')->justReturn([
            'kadence-blocks/kadence-blocks-main.php' => ['Name' => 'Kadence Blocks', 'Version' => '3.3.0'],
        ]);

        $runner = new UpdateRunner();

        $this->assertTrue($runner->isInstalled('plugin', 'kadence-blocks/kadence-blocks.php'));
    }

    public function test_currentVersion_also_resolves_a_renamed_main_file_by_folder(): void
    {
        // Same fallback loop, third call site.
        Functions\when('get_plugins')->justReturn([
            'kadence-blocks/kadence-blocks-main.php' => ['Name' => 'Kadence Blocks', 'Version' => '3.3.0'],
        ]);

        $runner = new UpdateRunner();

        $this->assertSame('3.3.0', $runner->currentVersion('plugin', 'kadence-blocks/kadence-blocks.php'));
    }

    public function test_isComplete_false_when_the_slug_resolves_to_no_folder_at_all(): void
    {
        // The genuine half-write symptom must still be caught: no installed
        // plugin under ANY folder matches.
        Functions\when('get_plugins')->justReturn([
            'unrelated/unrelated.php' => ['Name' => 'Unrelated', 'Version' => '1.0'],
        ]);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertFalse($runner->isComplete('plugin', 'gone-plugin/gone-plugin.php'));
        // Agent-only, 0.61.19 (GitHub issue #182's sibling visibility fix):
        // the same reason DebugLog already recorded must now also be readable
        // via lastIncompleteReason(), so UpdateCommand can surface it in the
        // CP-visible item log without requiring WPMGR_DEBUG.
        $this->assertStringContainsString(
            'basename-unresolved',
            $runner->lastIncompleteReason(),
            'lastIncompleteReason() must report the basename-unresolved reason'
        );
    }

    public function test_isComplete_true_for_an_exact_unchanged_basename_match(): void
    {
        // Control: the common case (main file NOT renamed) must be
        // unaffected by the S6 fallback-folder change.
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '5.3'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertTrue($runner->isComplete('plugin', 'akismet/akismet.php'));
    }

    // =========================================================================
    // Agent-only regression fix (0.61.18) — stale post-apply stat/realpath
    // cache false-negative on open_basedir/RunCloud-style hosts. See
    // UpdateRunner::isComplete()'s class doc for the full symptom/fix.
    // =========================================================================

    /**
     * PROOF: a GOOD apply must no longer be reported incomplete just because
     * the OS-level stat/realpath cache still holds the pre-apply directory
     * listing. validate_plugin() is faked to return a WP_Error (the exact
     * "stale cache reads the just-swapped main file as absent" symptom)
     * UNTIL clearstatcache() has actually run, proving both the outcome
     * (isComplete() === true) and the ORDER (clearstatcache() must run
     * BEFORE the validate_plugin() re-read, not merely somewhere in the
     * method) — a fix that busted the cache only AFTER this read would still
     * observe the stale WP_Error here and fail this assertion.
     */
    public function test_stale_stat_cache_post_apply_still_reports_complete(): void
    {
        // Shared mutable flag. $cacheState is an object, so every closure
        // below that captures it by VALUE still shares the same underlying
        // instance (PHP object handles), no by-reference capture needed.
        $cacheState = new class {
            public bool $cleared = false;
        };

        Functions\when('clearstatcache')->alias(function () use ($cacheState) {
            $cacheState->cleared = true;
        });
        Functions\when('get_plugins')->justReturn([
            'demo/demo.php' => ['Name' => 'Demo', 'Version' => '2.0'],
        ]);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);
        Functions\when('validate_plugin')->alias(function () use ($cacheState) {
            if (!$cacheState->cleared) {
                return new \WP_Error('plugin_not_found', 'stale stat cache: main file not found.');
            }

            return 0;
        });

        $runner = new UpdateRunner();

        $this->assertTrue(
            $runner->isComplete('plugin', 'demo/demo.php'),
            'clearstatcache() must run BEFORE validate_plugin() so a stale post-apply stat cache never produces a false incomplete verdict'
        );
        $this->assertTrue(
            $cacheState->cleared,
            'precondition: clearstatcache() must actually run as part of isComplete()'
        );
    }

    /**
     * ADVERSARIAL GUARD: the clearstatcache() fix must NOT weaken half-write
     * detection into ignoring a real partial write. Unlike the stale-cache
     * test above, validate_plugin() genuinely fails on EVERY call — cache-bust
     * or not — the real symptom this check exists to catch.
     */
    public function test_genuinely_missing_main_file_still_reports_incomplete(): void
    {
        Functions\when('clearstatcache')->justReturn(null);
        Functions\when('get_plugins')->justReturn([
            'demo/demo.php' => ['Name' => 'Demo', 'Version' => '2.0'],
        ]);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);
        Functions\when('validate_plugin')->justReturn(
            new \WP_Error('plugin_not_found', 'main file genuinely missing on disk.')
        );

        $runner = new UpdateRunner();

        $this->assertFalse(
            $runner->isComplete('plugin', 'demo/demo.php'),
            'ADVERSARIAL: clearstatcache() must not mask a GENUINE half-write — validate_plugin() failing even after the cache-bust must still report incomplete'
        );
        // Agent-only, 0.61.19: the validate_plugin() WP_Error message must be
        // reflected in lastIncompleteReason() — the concrete "why" a #144-style
        // "Failed + rollback" item needs, without WPMGR_DEBUG.
        $this->assertStringContainsString(
            'validate_plugin',
            $runner->lastIncompleteReason(),
            'lastIncompleteReason() must report the validate_plugin() error'
        );
        $this->assertStringContainsString(
            'main file genuinely missing on disk.',
            $runner->lastIncompleteReason(),
            'lastIncompleteReason() must include the underlying WP_Error message'
        );
    }

    // =========================================================================
    // lastIncompleteReason() lifecycle — agent-only, 0.61.19
    // =========================================================================

    public function test_lastIncompleteReason_is_empty_after_a_true_verdict(): void
    {
        Functions\when('get_plugins')->justReturn([
            'demo/demo.php' => ['Name' => 'Demo', 'Version' => '2.0'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertTrue($runner->isComplete('plugin', 'demo/demo.php'));
        $this->assertSame('', $runner->lastIncompleteReason(), 'a TRUE verdict must never leave a stale reason behind');
    }

    public function test_lastIncompleteReason_never_leaks_across_two_isComplete_calls_on_the_same_runner(): void
    {
        // A single UpdateCommand::execute() batch reuses ONE UpdateRunner
        // across every item — if isComplete() didn't reset the reason at the
        // START of every call, a SECOND item whose apply already failed
        // (never reaching isComplete() at all) could read the FIRST item's
        // stale reason. This proves the reset happens unconditionally.
        Functions\when('get_plugins')->justReturn([
            'unrelated/unrelated.php' => ['Name' => 'Unrelated', 'Version' => '1.0'],
        ]);
        Functions\when('wp_clean_plugins_cache')->justReturn(null);

        $runner = new UpdateRunner();

        $this->assertFalse($runner->isComplete('plugin', 'gone-plugin/gone-plugin.php'));
        $this->assertNotSame('', $runner->lastIncompleteReason(), 'precondition: the first call must have set a reason');

        Functions\when('get_plugins')->justReturn([
            'demo/demo.php' => ['Name' => 'Demo', 'Version' => '2.0'],
        ]);
        Functions\when('validate_plugin')->justReturn(0);

        $this->assertTrue($runner->isComplete('plugin', 'demo/demo.php'));
        $this->assertSame(
            '',
            $runner->lastIncompleteReason(),
            'a subsequent TRUE verdict must clear out the previous call\'s reason, never leak it'
        );
    }

    // =========================================================================
    // Theme parity — the same clearstatcache() fix, theme-side.
    // =========================================================================

    public function test_theme_truncated_style_css_still_reports_incomplete_after_cache_bust(): void
    {
        Functions\when('clearstatcache')->justReturn(null);
        Functions\when('wp_clean_themes_cache')->justReturn(null);

        // A genuinely truncated style.css (corrupted mid-copy) reads back
        // with an empty Name even after the cache-bust — must still be
        // reported incomplete, not masked.
        $theme = new class {
            public function get(string $key): string
            {
                return '';
            }
        };
        Functions\when('wp_get_theme')->justReturn($theme);

        $runner = new UpdateRunner();

        $this->assertFalse(
            $runner->isComplete('theme', 'broken-theme'),
            'ADVERSARIAL: a genuinely truncated style.css must still report incomplete after the cache-bust'
        );
    }

    public function test_theme_good_update_reports_complete_after_cache_bust(): void
    {
        Functions\when('clearstatcache')->justReturn(null);
        Functions\when('wp_clean_themes_cache')->justReturn(null);

        $theme = new class {
            public function get(string $key): string
            {
                return $key === 'Name' ? 'Good Theme' : '2.0';
            }
        };
        Functions\when('wp_get_theme')->justReturn($theme);

        $runner = new UpdateRunner();

        $this->assertTrue($runner->isComplete('theme', 'good-theme'));
    }

    /** Theme-side parity to test_stale_stat_cache_post_apply_still_reports_complete(). */
    public function test_theme_stale_stat_cache_post_apply_still_reports_complete(): void
    {
        $cacheState = new class {
            public bool $cleared = false;
        };

        Functions\when('clearstatcache')->alias(function () use ($cacheState) {
            $cacheState->cleared = true;
        });
        Functions\when('wp_clean_themes_cache')->justReturn(null);

        $theme = new class ($cacheState) {
            public function __construct(private object $cacheState)
            {
            }

            public function get(string $key): string
            {
                if (!$this->cacheState->cleared) {
                    // The stale-cache symptom: an empty Name as if the
                    // header hadn't landed yet.
                    return '';
                }

                return $key === 'Name' ? 'Good Theme' : '2.0';
            }
        };
        Functions\when('wp_get_theme')->justReturn($theme);

        $runner = new UpdateRunner();

        $this->assertTrue(
            $runner->isComplete('theme', 'good-theme'),
            'theme parity: clearstatcache() must run BEFORE wp_get_theme()\'s re-read too'
        );
        $this->assertTrue($cacheState->cleared);
    }
}
