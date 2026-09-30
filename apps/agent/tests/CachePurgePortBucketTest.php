<?php
/**
 * A per-URL purge clears the bucket the cache writer filled.
 *
 * The writer buckets a page by the request's Host header (CacheKey::
 * normalizeHost), which carries a port only when it is not the scheme's
 * default. Each row seeds a page under that bucket and purges it through
 * Purge::purgeUrl, CacheManager::purge and AutoPurge.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\AutoPurge;
use WPMgr\Agent\Cache\CacheKey;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\Preload;
use WPMgr\Agent\Cache\Purge;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\Purge
 * @covers \WPMgr\Agent\Cache\CacheManager
 * @covers \WPMgr\Agent\Cache\AutoPurge
 */
final class CachePurgePortBucketTest extends TestCase
{
    /** Hosts this test seeds; cleared before and after each test. */
    private const HOSTS = ['shop.test', 'shop.test:8443', 'shop.test:443', 'shop.test:80'];

    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('do_action')->justReturn(null);
        Functions\when('update_option')->justReturn(true);
        Functions\when('get_option')->alias(static fn ($k, $d = false) => $d);
        $this->root = '';
    }

    protected function tear_down(): void
    {
        $this->clearHosts();
        if ($this->root !== '' && str_contains($this->root, '/wpmgr-port-')) {
            $this->rrmdir(dirname(dirname($this->root)));
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * @return array<string,array{0:string,1:string}> purge URL, Host the page was cached under
     */
    public static function rows(): array
    {
        return [
            'https with :443'          => ['https://shop.test:443/x/', 'shop.test'],
            'http with :80'            => ['http://shop.test:80/x/', 'shop.test'],
            'https with :8443'         => ['https://shop.test:8443/x/', 'shop.test:8443'],
            'http with :443'           => ['http://shop.test:443/x/', 'shop.test:443'],
            'no port'                  => ['https://shop.test/x/', 'shop.test'],
            'upper-case scheme, :443'  => ['HTTPS://Shop.Test:443/x/', 'shop.test'],
            'https with :80'           => ['https://shop.test:80/x/', 'shop.test:80'],
        ];
    }

    /** Seed one cached page for /x/ under the bucket for $host. Returns its path. */
    private function seedPage(string $host): string
    {
        $bucket = CacheKey::normalizeHost($host);
        $this->assertNotSame('', $bucket, 'fixture host must be cacheable');
        $dir = $this->root . '/' . $bucket . '/x';
        if (!is_dir($dir)) {
            mkdir($dir, 0o777, true);
        }
        $file = $dir . '/index' . CacheKey::EXTENSION;
        file_put_contents($file, 'x');
        return $file;
    }

    /** Seed a decoy page in every other bucket; returns the decoy paths. */
    private function seedDecoys(string $host): array
    {
        $decoys = [];
        foreach (self::HOSTS as $other) {
            if (CacheKey::normalizeHost($other) !== CacheKey::normalizeHost($host)) {
                $decoys[] = $this->seedPage($other);
            }
        }
        return $decoys;
    }

    private function clearHosts(): void
    {
        if ($this->root === '') {
            return;
        }
        foreach (self::HOSTS as $host) {
            $this->rrmdir($this->root . '/' . CacheKey::normalizeHost($host));
        }
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $e) {
            if ($e === '.' || $e === '..') {
                continue;
            }
            $p = $dir . '/' . $e;
            is_dir($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    private function useTempRoot(): void
    {
        $this->root = sys_get_temp_dir() . '/wpmgr-port-' . uniqid('', true) . '/cache/wpmgr';
    }

    /** @param list<string> $decoys */
    private function assertOnlyTargetCleared(string $page, array $decoys): void
    {
        $this->assertFileDoesNotExist($page, 'the page cached under this bucket is deleted');
        foreach ($decoys as $decoy) {
            $this->assertFileExists($decoy, 'a page in another bucket survives');
        }
    }

    /**
     * @dataProvider rows
     */
    public function test_purge_url_clears_the_writers_bucket(string $url, string $cachedHost): void
    {
        $this->useTempRoot();
        $page   = $this->seedPage($cachedHost);
        $decoys = $this->seedDecoys($cachedHost);

        $removed = (new Purge($this->root))->purgeUrl($url);

        $this->assertSame(1, $removed);
        $this->assertOnlyTargetCleared($page, $decoys);
    }

    /**
     * @dataProvider rows
     */
    public function test_cache_manager_purge_clears_the_writers_bucket(string $url, string $cachedHost): void
    {
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        $mgr        = new CacheManager();
        $this->root = $mgr->cacheRoot();
        $this->assertNotSame('', $this->root);
        $this->clearHosts();

        $page   = $this->seedPage($cachedHost);
        $decoys = $this->seedDecoys($cachedHost);

        $result = $mgr->purge($url);

        $this->assertSame(['ok' => true, 'detail' => 'purged url', 'removed' => 1], $result);
        $this->assertOnlyTargetCleared($page, $decoys);
    }

    /**
     * Auto-purge is fed by home_url() and get_permalink(), which spell the
     * site's address as configured, including a default port when the site
     * URL writes one.
     *
     * @dataProvider rows
     */
    public function test_auto_purge_clears_the_writers_bucket(string $url, string $cachedHost): void
    {
        $this->useTempRoot();
        $page   = $this->seedPage($cachedHost);
        $decoys = $this->seedDecoys($cachedHost);

        $GLOBALS['wpdb'] = new FakePreloadQueueWpdb();
        Functions\when('home_url')->alias(static fn ($path = '/') => $url);
        Functions\when('get_permalink')->justReturn('');
        Functions\when('get_post_type')->justReturn('page');
        Functions\when('get_post_type_archive_link')->justReturn('');
        Functions\when('get_post_field')->justReturn(0);
        Functions\when('get_author_posts_url')->justReturn('');
        Functions\when('get_object_taxonomies')->justReturn([]);
        Functions\when('apply_filters')->returnArg(2);
        Functions\when('wp_next_scheduled')->justReturn(false);
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('add_action')->justReturn(true);
        Functions\when('add_filter')->justReturn(true);

        $auto = new AutoPurge(new Purge($this->root), new Preload(false));
        $auto->onPostUpdated(7, (object) ['post_status' => 'publish', 'post_type' => 'page'], null);

        $this->assertOnlyTargetCleared($page, $decoys);
    }

    public function test_path_only_url_keeps_todays_behaviour(): void
    {
        $this->useTempRoot();
        // A path-only URL has no host, so it resolves against the root itself.
        $dir = $this->root . '/x';
        mkdir($dir, 0o777, true);
        file_put_contents($dir . '/index' . CacheKey::EXTENSION, 'x');
        $page = $this->seedPage('shop.test');

        $removed = (new Purge($this->root))->purgeUrl('/x/');

        $this->assertSame(1, $removed);
        $this->assertFileDoesNotExist($dir . '/index' . CacheKey::EXTENSION);
        $this->assertFileExists($page);
    }
}
