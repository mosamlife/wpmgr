<?php
/**
 * Origin-only purge: the integrations whose reach is not confirmed to be this
 * install never run, the result reports what each detected integration did,
 * and without the option nothing changes.
 *
 * The purge actions run through a small in-test dispatcher with WordPress's
 * accepted_args semantics, so the options reach each handler exactly as they
 * would on a real site.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\Purge;
use WPMgr\Agent\Commands\CachePurgeCommand;
use WPMgr\Agent\Integrations\Integration;
use WPMgr\Agent\Integrations\Kinsta;
use WPMgr\Agent\Integrations\Varnish;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * Records every purge call a fixture integration makes.
 */
final class OriginFixtureLog
{
    /** @var list<array{0:string,1:string,2:list<string>}> slug, method, urls */
    public static array $calls = [];
}

/**
 * An integration that declares no reach at all: the base default applies.
 */
final class OriginFixtureUnclassified extends Integration
{
    public const SLUG = 'fixture_unclassified';

    protected function detect(): bool
    {
        return true;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }

    protected function purgeUrls(array $urls): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeUrls', $urls];
    }
}

/**
 * An 'install' integration with a dated verification note.
 */
final class OriginFixtureInstallNoted extends Integration
{
    public const SLUG = 'fixture_install_noted';

    protected const REACH = self::REACH_INSTALL;

    protected const REACH_NOTE = 'Verified 2026-09-29: test fixture, reaches only this install.';

    protected function detect(): bool
    {
        return true;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }

    protected function purgeUrls(array $urls): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeUrls', $urls];
    }
}

/**
 * An 'install' integration whose note is not a dated verification note.
 */
final class OriginFixtureInstallUnnoted extends Integration
{
    public const SLUG = 'fixture_install_unnoted';

    protected const REACH = self::REACH_INSTALL;

    protected const REACH_NOTE = 'Believed to reach only this install.';

    protected function detect(): bool
    {
        return true;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }
}

/**
 * A 'shared' integration with a noted exact-URL purge.
 */
final class OriginFixtureExactNoted extends Integration
{
    public const SLUG = 'fixture_exact_noted';

    protected const REACH_NOTE = 'Verified 2026-09-29: test fixture, the exact purge names one URL.';

    protected function detect(): bool
    {
        return true;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }

    protected function purgeUrls(array $urls): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeUrls', $urls];
    }

    protected function purgeUrlsExact(array $urls): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeUrlsExact', $urls];
    }
}

/**
 * A 'shared' integration with an exact-URL purge but no dated note.
 */
final class OriginFixtureExactUnnoted extends Integration
{
    public const SLUG = 'fixture_exact_unnoted';

    protected const REACH_NOTE = 'Candidate: not yet checked.';

    protected function detect(): bool
    {
        return true;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }

    protected function purgeUrlsExact(array $urls): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeUrlsExact', $urls];
    }
}

/**
 * An 'install' integration whose host is not present on the site.
 */
final class OriginFixtureUndetected extends Integration
{
    public const SLUG = 'fixture_undetected';

    protected const REACH = self::REACH_INSTALL;

    protected const REACH_NOTE = 'Verified 2026-09-29: test fixture.';

    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
        OriginFixtureLog::$calls[] = [self::SLUG, 'purgeAll', []];
    }
}

/**
 * @covers \WPMgr\Agent\Integrations\Integration
 * @covers \WPMgr\Agent\Cache\CacheManager
 * @covers \WPMgr\Agent\Cache\Purge
 * @covers \WPMgr\Agent\Commands\CachePurgeCommand
 */
final class CacheOriginOnlyPurgeTest extends TestCase
{
    /** @var array<string,list<array{0:callable,1:int}>> hook => [callback, accepted_args] */
    private array $hooks = [];

    /** @var list<array{0:string,1:array<mixed>}> do_action calls: hook, args */
    private array $fired = [];

    /** @var list<string> Calls made on the recording Kinsta purger. */
    private array $kinstaCalls = [];

    /** @var list<array{0:string,1:string}> Outbound HTTP: url, method. */
    private array $http = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->hooks       = [];
        $this->fired       = [];
        $this->kinstaCalls = [];
        $this->http        = [];
        OriginFixtureLog::$calls = [];
        Integration::endReport();

        Functions\when('add_action')->alias(function (string $hook, $cb, $prio = 10, $args = 1) {
            $this->hooks[$hook][] = [$cb, (int) $args];
            return true;
        });
        // WordPress semantics: with no arguments the callbacks receive a single
        // '', and each callback receives at most its accepted_args.
        Functions\when('do_action')->alias(function (string $hook, ...$args) {
            $this->fired[] = [$hook, $args];
            if ($args === []) {
                $args = [''];
            }
            foreach ($this->hooks[$hook] ?? [] as [$cb, $accepted]) {
                $cb(...array_slice($args, 0, $accepted));
            }
        });
        Functions\when('update_option')->justReturn(true);
        Functions\when('get_option')->alias(static fn ($k, $d = false) => $d);
        Functions\when('wp_remote_request')->alias(function ($url, $args = []) {
            $this->http[] = [(string) $url, (string) ($args['method'] ?? 'GET')];
            return [];
        });
        Functions\when('wp_unslash')->alias(static fn ($v) => is_string($v) ? stripslashes($v) : $v);
        Functions\when('sanitize_text_field')->alias(static fn ($v) => trim((string) $v));

        // Purge boots the production integrations once per process. Re-arm it so
        // they register on this test's dispatcher.
        $booted = new \ReflectionProperty(Purge::class, 'integrationsBooted');
        $booted->setValue(null, false);

        unset($GLOBALS['kinsta_cache'], $_SERVER['HTTP_X_VARNISH'], $_SERVER['HTTP_X_APPLICATION'], $_SERVER['HTTP_HOST']);
    }

    protected function tear_down(): void
    {
        Integration::endReport();
        $booted = new \ReflectionProperty(Purge::class, 'integrationsBooted');
        $booted->setValue(null, false);
        unset($GLOBALS['kinsta_cache'], $_SERVER['HTTP_X_VARNISH'], $_SERVER['HTTP_HOST']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Make two production integrations detectable, each recording what it does. */
    private function presentKinstaAndVarnish(): void
    {
        $calls  = &$this->kinstaCalls;
        $purger = new class ($calls) {
            /** @var list<string> */
            private array $calls;

            /** @param list<string> $calls */
            public function __construct(array &$calls)
            {
                $this->calls = &$calls;
            }

            public function purge_complete_caches(): void
            {
                $this->calls[] = 'purge_complete_caches';
            }

            /** @param list<string> $urls */
            public function purge_caches_urls(array $urls): void
            {
                $this->calls[] = 'purge_caches_urls';
            }
        };
        $GLOBALS['kinsta_cache'] = (object) ['kinsta_cache_purge' => $purger];

        $_SERVER['HTTP_X_VARNISH'] = '1';
        $_SERVER['HTTP_HOST']      = 'shop.test';
    }

    /** Register every fixture integration on the dispatcher. */
    private function registerFixtures(): void
    {
        new OriginFixtureUnclassified();
        new OriginFixtureInstallNoted();
        new OriginFixtureInstallUnnoted();
        new OriginFixtureExactNoted();
        new OriginFixtureExactUnnoted();
        new OriginFixtureUndetected();
    }

    /**
     * The purge engine, booted so the production integrations and the fixtures
     * are all on the dispatcher.
     */
    private function engine(): Purge
    {
        $purge = new Purge(sys_get_temp_dir() . '/wpmgr-origin-' . uniqid('', true) . '/cache/wpmgr');
        $this->registerFixtures();
        return $purge;
    }

    /** @return array<string,string> slug => action */
    private static function bySlug(array $report): array
    {
        $out = [];
        foreach ($report as $row) {
            $out[$row['slug']] = $row['action'];
        }
        return $out;
    }

    /** @return list<string> "slug:method" for every recorded fixture call */
    private static function fixtureCalls(): array
    {
        return array_map(static fn (array $c): string => $c[0] . ':' . $c[1], OriginFixtureLog::$calls);
    }

    // ----------------------------------------------------------------- hooks

    public function test_hooks_register_with_the_accepted_args_that_carry_the_options(): void
    {
        new OriginFixtureUnclassified();

        $this->assertSame(2, $this->hooks['wpmgr_purge_urls:before'][0][1]);
        $this->assertSame(1, $this->hooks['wpmgr_purge_everything:before'][0][1]);
        $this->assertSame(0, $this->hooks['wpmgr_purge_pages:before'][0][1]);
    }

    public function test_purge_actions_carry_the_options_only_when_there_are_any(): void
    {
        $purge = $this->engine();

        $purge->purgeUrl('https://shop.test/a/', ['origin_only' => true]);
        $purge->purgeEverything(['origin_only' => true]);
        $purge->purgeUrl('https://shop.test/b/');
        $purge->purgeEverything();

        $fired = array_values(array_filter($this->fired, static fn (array $f): bool => str_ends_with($f[0], ':before')));
        $this->assertSame(
            [
                ['wpmgr_purge_urls:before', [['https://shop.test/a/'], ['origin_only' => true]]],
                ['wpmgr_purge_everything:before', [['origin_only' => true]]],
                ['wpmgr_purge_urls:before', [['https://shop.test/b/']]],
                ['wpmgr_purge_everything:before', []],
            ],
            $fired
        );
    }

    // ------------------------------------------------------ origin_only, all

    public function test_origin_only_all_runs_only_noted_install_integrations(): void
    {
        $this->presentKinstaAndVarnish();
        $purge = $this->engine();

        Integration::beginReport();
        $purge->purgeEverything(['origin_only' => true]);
        $report = Integration::endReport();

        $this->assertSame(['fixture_install_noted:purgeAll'], self::fixtureCalls());
        $this->assertSame([], $this->kinstaCalls, 'a shared production integration must not purge');
        $this->assertSame([], $this->http, 'a shared production integration must not call out');

        $this->assertSame(
            [
                'varnish'                 => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'kinsta'                  => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_unclassified'    => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_install_noted'   => Integration::ACTION_PURGED_ALL,
                'fixture_install_unnoted' => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_exact_noted'     => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_exact_unnoted'   => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
            ],
            self::bySlug($report)
        );
        $this->assertArrayNotHasKey('fixture_undetected', self::bySlug($report), 'undetected integrations are not listed');
    }

    // ------------------------------------------------------ origin_only, url

    public function test_origin_only_url_runs_no_purge_urls_and_only_the_noted_exact_purge(): void
    {
        $this->presentKinstaAndVarnish();
        $purge = $this->engine();

        Integration::beginReport();
        $purge->purgeUrl('https://shop.test/sale/', ['origin_only' => true]);
        $report = Integration::endReport();

        $this->assertSame(
            ['fixture_install_noted:purgeUrls', 'fixture_exact_noted:purgeUrlsExact'],
            self::fixtureCalls()
        );
        $exact = array_values(array_filter(OriginFixtureLog::$calls, static fn (array $c): bool => $c[1] === 'purgeUrlsExact'));
        $this->assertSame(['https://shop.test/sale/'], $exact[0][2], 'the exact purge receives that URL only');
        $this->assertSame([], $this->kinstaCalls);
        $this->assertSame([], $this->http);

        $this->assertSame(
            [
                'varnish'                 => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'kinsta'                  => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_unclassified'    => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_install_noted'   => Integration::ACTION_PURGED_URLS,
                'fixture_install_unnoted' => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
                'fixture_exact_noted'     => Integration::ACTION_PURGED_URLS_EXACT,
                'fixture_exact_unnoted'   => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED,
            ],
            self::bySlug($report)
        );
    }

    // ------------------------------------------------------- option absent

    public function test_without_the_option_every_detected_integration_purges_as_before(): void
    {
        $this->presentKinstaAndVarnish();
        $purge = $this->engine();

        $purge->purgeUrl('https://shop.test/sale/');
        $purge->purgeEverything();

        $this->assertSame(['purge_caches_urls', 'purge_complete_caches'], $this->kinstaCalls);
        $this->assertSame(
            [['http://127.0.0.1/sale/', 'PURGE'], ['http://127.0.0.1/.*', 'BAN']],
            $this->http
        );
        $this->assertSame(
            [
                'fixture_unclassified:purgeUrls',
                'fixture_install_noted:purgeUrls',
                'fixture_install_unnoted:purgeAll',
                'fixture_exact_noted:purgeUrls',
                'fixture_exact_unnoted:purgeAll',
                'fixture_unclassified:purgeAll',
                'fixture_install_noted:purgeAll',
                'fixture_install_unnoted:purgeAll',
                'fixture_exact_noted:purgeAll',
                'fixture_exact_unnoted:purgeAll',
            ],
            self::fixtureCalls()
        );
    }

    public function test_an_options_value_other_than_true_is_not_origin_only(): void
    {
        $purge = $this->engine();

        $purge->purgeEverything(['origin_only' => 'yes']);

        $this->assertContains('fixture_unclassified:purgeAll', self::fixtureCalls());
    }

    // ----------------------------------------------------- the result fields

    public function test_manager_result_carries_the_report_only_under_origin_only(): void
    {
        $this->registerFixtures();
        $mgr = new CacheManager();

        $plain = $mgr->purge('https://shop.test/x/');
        $this->assertSame(['ok', 'detail', 'removed'], array_keys($plain));

        $origin = $mgr->purge('https://shop.test/x/', ['origin_only' => true]);
        $this->assertTrue($origin['origin_only_honoured']);
        $this->assertIsArray($origin['integrations']);
        $this->assertContains(
            ['slug' => 'fixture_exact_noted', 'action' => Integration::ACTION_PURGED_URLS_EXACT],
            $origin['integrations']
        );

        $all = $mgr->purge('all', ['origin_only' => true]);
        $this->assertTrue($all['origin_only_honoured']);
        $this->assertContains(
            ['slug' => 'fixture_exact_noted', 'action' => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED],
            $all['integrations']
        );
    }

    public function test_manager_result_lists_an_empty_report_when_nothing_is_detected(): void
    {
        $mgr    = new CacheManager();
        $result = $mgr->purge('all', ['origin_only' => true]);

        $this->assertTrue($result['origin_only_honoured']);
        $this->assertSame([], $result['integrations']);
    }

    public function test_command_reads_origin_only_and_returns_the_report(): void
    {
        $this->registerFixtures();
        Functions\when('get_option')->justReturn(false);
        $cmd = new CachePurgeCommand(new CacheManager());

        $res = $cmd->execute([], ['scope' => 'url', 'url' => 'https://shop.test/x/', 'origin_only' => true]);
        $this->assertTrue($res['origin_only_honoured']);
        $this->assertIsArray($res['integrations']);
        $this->assertNotContains('fixture_unclassified:purgeUrls', self::fixtureCalls());

        OriginFixtureLog::$calls = [];
        $plain = $cmd->execute([], ['scope' => 'all', 'origin_only' => false]);
        $this->assertArrayNotHasKey('origin_only_honoured', $plain);
        $this->assertArrayNotHasKey('integrations', $plain);
        $this->assertContains('fixture_unclassified:purgeAll', self::fixtureCalls());
    }

    // -------------------------------------------------------- nested purges

    /**
     * Start a nested origin_only URL purge from inside the outer purge's hook,
     * after the production integrations have run and before the fixtures do.
     *
     * @param array<string,mixed> $inner Receives the inner purge result.
     */
    private function nestOnce(array &$inner): void
    {
        $done = false;
        $this->hooks['wpmgr_purge_everything:before'][] = [
            function () use (&$inner, &$done): void {
                if ($done) {
                    return;
                }
                $done  = true;
                $inner = (new CacheManager())->purge('https://shop.test/x/', ['origin_only' => true]);
            },
            0,
        ];
    }

    public function test_a_nested_origin_only_purge_keeps_each_report_separate(): void
    {
        $this->presentKinstaAndVarnish();
        $mgr = new CacheManager();
        $mgr->purge('https://shop.test/warm/'); // boot the production integrations first
        $inner = [];
        $this->nestOnce($inner);
        $this->registerFixtures();

        $outer   = $mgr->purge('all', ['origin_only' => true]);
        $outerBy = self::bySlug($outer['integrations']);
        $innerBy = self::bySlug($inner['integrations']);

        // Outer: ran before, at and after the nesting point.
        $this->assertSame(Integration::ACTION_SKIPPED_REACH_UNCONFIRMED, $outerBy['varnish'] ?? null);
        $this->assertSame(Integration::ACTION_PURGED_ALL, $outerBy['fixture_install_noted'] ?? null);
        $this->assertSame(Integration::ACTION_SKIPPED_REACH_UNCONFIRMED, $outerBy['fixture_exact_noted'] ?? null);
        // Inner: the URL purge, where the exact-noted integration acts.
        $this->assertSame(Integration::ACTION_PURGED_URLS_EXACT, $innerBy['fixture_exact_noted'] ?? null);
        // Each purge lists each integration once: no cross-contamination.
        $this->assertCount(count($outer['integrations']), $outerBy);
        $this->assertCount(count($inner['integrations']), $innerBy);
    }

    public function test_an_exception_mid_purge_leaves_no_open_report_frame(): void
    {
        $this->registerFixtures();
        $this->hooks['wpmgr_purge_everything:before'][] = [
            static function (): void {
                throw new \RuntimeException('hook failed');
            },
            0,
        ];

        try {
            (new CacheManager())->purge('all', ['origin_only' => true]);
            $this->fail('the purge must propagate the hook exception');
        } catch (\RuntimeException $e) {
            $this->assertSame('hook failed', $e->getMessage());
        }

        $frames = new \ReflectionProperty(Integration::class, 'reportFrames');
        $this->assertSame([], $frames->getValue(null), 'no frame may be left open');
    }

    public function test_an_exception_in_a_nested_purge_closes_both_frames(): void
    {
        // Contract: a purge closes its own report frame however it ends, so an
        // exception from a nested purge leaves neither the nested frame nor the
        // outer one open.
        $frames = new \ReflectionProperty(Integration::class, 'reportFrames');
        $this->assertSame([], $frames->getValue(null), 'the test starts with no frame open');

        $this->presentKinstaAndVarnish();
        $mgr = new CacheManager();
        $mgr->purge('https://shop.test/warm/'); // boot the production integrations first
        $inner = [];
        $this->nestOnce($inner);
        $this->registerFixtures();

        // The nested purge throws after every integration has acted for it. The
        // frames are read at that moment: both purges are open and each holds
        // its own entries.
        $open = null;
        $this->hooks['wpmgr_purge_urls:before'][] = [
            static function () use ($frames, &$open): void {
                $open = $frames->getValue(null);
                throw new \RuntimeException('nested purge failed');
            },
            0,
        ];

        $thrown = null;
        try {
            $mgr->purge('all', ['origin_only' => true]);
        } catch (\RuntimeException $e) {
            $thrown = $e;
        }

        $this->assertNotNull($thrown, 'the nested exception must propagate out of the outer purge');
        $this->assertSame('nested purge failed', $thrown->getMessage());

        $this->assertIsArray($open, 'the nested purge must have started');
        $this->assertCount(2, $open, 'the outer and the nested purge were both open when it threw');
        $this->assertSame(
            [
                ['slug' => 'varnish', 'action' => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED],
                ['slug' => 'kinsta', 'action' => Integration::ACTION_SKIPPED_REACH_UNCONFIRMED],
            ],
            $open[0],
            'the outer frame holds what the outer purge recorded before the nested one started, and nothing of the nested purge'
        );
        $innerBy = self::bySlug($open[1]);
        $this->assertSame(Integration::ACTION_PURGED_URLS_EXACT, $innerBy['fixture_exact_noted'] ?? null);
        $this->assertCount(count($open[1]), $innerBy, 'the nested frame lists each integration once');

        $this->assertSame([], $frames->getValue(null), 'neither the outer nor the nested frame may be left open');
    }

    /**
     * @dataProvider nonBooleanOriginOnly
     */
    public function test_command_refuses_a_non_boolean_origin_only(mixed $value): void
    {
        $this->registerFixtures();
        $cmd = new CachePurgeCommand(new CacheManager());

        $res = $cmd->execute([], ['scope' => 'all', 'origin_only' => $value]);

        $this->assertFalse($res['ok']);
        $this->assertSame('origin_only must be a boolean', $res['detail']);
        $this->assertSame([], OriginFixtureLog::$calls, 'a refused purge runs nothing');
    }

    /** @return array<string,array{0:mixed}> */
    public static function nonBooleanOriginOnly(): array
    {
        return [
            'string true' => ['true'],
            'int 1'       => [1],
            'null'        => [null],
            'array'       => [['x']],
        ];
    }

    public function test_report_is_closed_even_when_an_integration_throws(): void
    {
        // One integration records into the report, then the next one throws.
        Functions\when('do_action')->alias(static function (string $hook, ...$args) {
            if ($hook === 'wpmgr_purge_everything:before') {
                (new OriginFixtureInstallNoted())->onPurgeEverything(['origin_only' => true]);
                throw new \RuntimeException('host purge failed');
            }
        });
        $mgr = new CacheManager();

        try {
            $mgr->purge('all', ['origin_only' => true]);
            $this->fail('the exception must propagate');
        } catch (\RuntimeException $e) {
            $this->assertSame('host purge failed', $e->getMessage());
        }

        // A report left open would be merged into the next purge's report.
        $this->assertSame([], Integration::endReport());
    }
}
