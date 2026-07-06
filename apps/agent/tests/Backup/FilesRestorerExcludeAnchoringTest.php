<?php
/**
 * GH #147 regression: FilesRestorer::isExcluded() must use ANCHORED matching
 * (exact path / exact segment / anchored prefix), never an unanchored
 * substring match. The prior substring-based implementation matched
 * `strpos($entryName, 'db.php') !== false`, so a nested file like
 * `includes/modules/redirections/class-db.php` was silently dropped at
 * extract time — before a single byte reached staging, so the file was gone
 * after the swap with no error, no log line, and no way to notice until the
 * plugin broke in production.
 *
 * This test exercises `isExcluded()` directly via reflection — the same
 * pattern `ObjectCacheConfigRestoreExclusionTest` uses — because the bug and
 * the fix both live entirely inside that one method: `stage()` calls
 * `isExcluded()` BEFORE extraction (class-files-restorer.php), so whatever
 * `isExcluded()` returns is exactly what would/would not land in staging and
 * therefore exactly what would/would not survive the atomic swap.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use ReflectionClass;
use ReflectionMethod;
use WPMgr\Agent\Backup\FilesRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class FilesRestorerExcludeAnchoringTest extends TestCase
{
    private static ReflectionMethod $isExcluded;

    public static function set_up_before_class(): void
    {
        parent::set_up_before_class();
        $rc               = new ReflectionClass(FilesRestorer::class);
        self::$isExcluded = $rc->getMethod('isExcluded');
        // setAccessible() is a no-op since PHP 8.1 and deprecated in 8.5.
    }

    private static function isExcluded(string $entryName): bool
    {
        return (bool) self::$isExcluded->invoke(null, $entryName);
    }

    /**
     * Real-world nested `*db.php` plugin files (the exact GH #147 report:
     * Rank Math SEO ships five files matching `class-db.php` under different
     * module directories). None of these are the wp-content-root `db.php`
     * drop-in — every one of them MUST be extracted/restored.
     *
     * @return array<string,array{0:string}>
     */
    public static function nestedDbPhpProvider(): array
    {
        return [
            'rank-math helpers'             => ['plugins/seo-by-rank-math/includes/helpers/class-db.php'],
            'rank-math 404-monitor module'  => ['plugins/seo-by-rank-math/includes/modules/404-monitor/class-db.php'],
            'rank-math analytics module'    => ['plugins/seo-by-rank-math/includes/modules/analytics/class-db.php'],
            'rank-math redirections module' => ['plugins/seo-by-rank-math/includes/modules/redirections/class-db.php'],
            'rank-math schema module'       => ['plugins/seo-by-rank-math/includes/modules/schema/class-db.php'],
            'instawp sync db class'         => ['plugins/instawp-connect/includes/sync/class-instawp-sync-db.php'],
        ];
    }

    /**
     * @dataProvider nestedDbPhpProvider
     */
    public function test_nested_db_php_variants_are_not_excluded(string $entryName): void
    {
        $this->assertFalse(
            self::isExcluded($entryName),
            "FilesRestorer::isExcluded must NOT exclude nested plugin file: {$entryName}"
        );
    }

    /**
     * A bundled `object-cache.php` TEMPLATE shipped inside a plugin's own
     * directory (e.g. redis-cache ships one under includes/) is plugin
     * content, not the live wp-content-root drop-in — it must be restored.
     */
    public function test_bundled_object_cache_template_is_not_excluded(): void
    {
        $this->assertFalse(
            self::isExcluded('plugins/redis-cache/includes/object-cache.php'),
            'FilesRestorer::isExcluded must not exclude a plugin-bundled object-cache.php template'
        );
    }

    /**
     * A nested `.htaccess` (e.g. the one WordPress core writes under
     * uploads/ to block PHP execution) is content, not the ABSPATH-root
     * config file — it must be restored.
     */
    public function test_nested_htaccess_is_not_excluded(): void
    {
        $this->assertFalse(
            self::isExcluded('uploads/2026/.htaccess'),
            'FilesRestorer::isExcluded must not exclude a nested uploads/.htaccess'
        );
    }

    /**
     * The wp-content-ROOT drop-ins/config MUST still be excluded — only when
     * they appear with no path prefix (i.e. actually at the root of the
     * archive).
     *
     * @return array<string,array{0:string}>
     */
    public static function rootDropInProvider(): array
    {
        return [
            'db.php'              => ['db.php'],
            'object-cache.php'    => ['object-cache.php'],
            'advanced-cache.php'  => ['advanced-cache.php'],
            'wp-config.php'       => ['wp-config.php'],
            '.htaccess'           => ['.htaccess'],
            '.user.ini'           => ['.user.ini'],
        ];
    }

    /**
     * @dataProvider rootDropInProvider
     */
    public function test_root_drop_ins_are_still_excluded(string $entryName): void
    {
        $this->assertTrue(
            self::isExcluded($entryName),
            "FilesRestorer::isExcluded must still exclude the wp-content-root drop-in/config: {$entryName}"
        );
    }

    /**
     * WPMgr secret/state files must still be excluded, at any depth —
     * unchanged behavior, guarded here so a future refactor can't regress it
     * alongside the anchoring fix.
     */
    public function test_wpmgr_secret_files_still_excluded_at_any_depth(): void
    {
        $this->assertTrue(
            self::isExcluded('wpmgr-object-cache-config.php'),
            'wpmgr-object-cache-config.php must be excluded at the wp-content root'
        );
        $this->assertTrue(
            self::isExcluded('some/subdir/wpmgr-object-cache-config.php'),
            'wpmgr-object-cache-config.php must be excluded when nested'
        );
        $this->assertTrue(
            self::isExcluded('.wpmgr-oc-state.json'),
            '.wpmgr-oc-state.json must be excluded'
        );
        $this->assertTrue(
            self::isExcluded('wpmgr-agent/restores/abc123/database.sql.gz'),
            'the wp-content-root wpmgr-agent scratch dir must still be excluded'
        );
        $this->assertTrue(
            self::isExcluded('plugins/wpmgr-agent/wpmgr-agent.php'),
            'the wpmgr-agent plugin tree must still be excluded'
        );
        $this->assertTrue(
            self::isExcluded('uploads/wpmgr-snapshots/db/dump.sql'),
            'wpmgr-snapshots must still be excluded even when nested under uploads/'
        );
    }

    /**
     * Ordinary plugin files must never be excluded (no regression on the
     * happy path).
     */
    public function test_normal_plugin_file_is_not_excluded(): void
    {
        $this->assertFalse(
            self::isExcluded('plugins/my-plugin/main.php'),
            'FilesRestorer::isExcluded must not exclude normal plugin files'
        );
    }

    /**
     * Near-miss segments must NOT be excluded: the WPMgr segment/prefix guards
     * match on an exact path COMPONENT, never a substring — so a third-party
     * plugin whose name merely starts with or contains "wpmgr-agent" is restored.
     * Locks in the anchoring so a future refactor to substring matching regresses
     * loudly (GH #147 hardening).
     */
    public function test_near_miss_wpmgr_segments_are_not_excluded(): void
    {
        $this->assertFalse(
            self::isExcluded('plugins/my-wpmgr-agent-helper/x.php'),
            'a plugin dir CONTAINING wpmgr-agent as a substring must be restored'
        );
        $this->assertFalse(
            self::isExcluded('plugins/wpmgr-agentx/x.php'),
            'a plugin dir starting with wpmgr-agent but a different segment must be restored'
        );
        $this->assertFalse(
            self::isExcluded('plugins/foo/wpmgr-snapshots-README.md'),
            'a file whose name merely contains wpmgr-snapshots must be restored'
        );
    }

    /**
     * Path normalization must not let a drop-in slip through (or a good file be
     * dropped): backslashes, leading slash, and a trailing-slash directory entry
     * are normalized before anchored matching.
     */
    public function test_path_normalization_before_anchored_match(): void
    {
        // Backslash-separated + leading slash still resolves the root drop-in.
        $this->assertTrue(
            self::isExcluded('\\db.php'),
            'a backslash/leading-slash root db.php drop-in must still be excluded'
        );
        $this->assertTrue(
            self::isExcluded('/object-cache.php'),
            'a leading-slash root object-cache.php drop-in must still be excluded'
        );
        // Backslash-separated nested plugin file must still be restored.
        $this->assertFalse(
            self::isExcluded('plugins\\seo-by-rank-math\\includes\\modules\\redirections\\class-db.php'),
            'a backslash-separated nested class-db.php must be restored'
        );
    }
}
