<?php
/**
 * GH #577: uninstall leaves nothing in the agent's option namespace.
 *
 * The namespace is a published contract (apps/agent/README.md, "Option
 * namespace contract"): the agent keeps its identity and connection state in
 * options named wpmgr_agent_*, and a transient in the namespace appears as a
 * _transient_wpmgr_agent_* row plus its _transient_timeout_ row, or as the
 * _site_transient_ forms. Operators reset a copied site by removing exactly
 * that namespace, so uninstall must be at least as complete as their sweep.
 *
 * Names are discovered by reflection over every agent class instead of being
 * listed here, so a constant added later is covered the day it lands. A class
 * that cannot be loaded in the test process is skipped; the positive controls
 * fail if discovery as a whole stops finding anything.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Enrollment;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Lifecycle;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Lifecycle
 */
final class LifecycleOwnedOptionsTest extends TestCase
{
    /**
     * The published prefix, pinned as a literal on purpose: it is a contract
     * with operators' scripts, not an implementation detail.
     */
    private const PREFIX = 'wpmgr_agent_';

    /** Row-name forms the namespace takes in the per-site options table. */
    private const OPTIONS_TABLE_FORMS = [
        'wpmgr_agent_',
        '_transient_wpmgr_agent_',
        '_transient_timeout_wpmgr_agent_',
        '_site_transient_wpmgr_agent_',
        '_site_transient_timeout_wpmgr_agent_',
    ];

    /** Row-name forms the namespace takes in the network table on multisite. */
    private const NETWORK_TABLE_FORMS = [
        'wpmgr_agent_',
        '_site_transient_wpmgr_agent_',
        '_site_transient_timeout_wpmgr_agent_',
    ];

    /**
     * Names in the namespace that no constant carries: written from a string
     * literal, by core on the agent's behalf, by a retired release, or by a
     * release that does not exist yet.
     */
    private const NAMES_WITHOUT_A_CONSTANT = [
        'wpmgr_agent_engine_opcache_version',
        'wpmgr_agent_self_update.lock',
        'wpmgr_agent_self_update_staged',
        'wpmgr_agent_name_from_a_future_release',
    ];

    /** @var array<string,mixed> The per-site options table. */
    private array $options = [];

    /** @var array<string,mixed> The network table of network 1 (multisite only). */
    private array $network = [];

    private bool $multisite = false;

    private bool $hadWpdb = false;

    /** @var mixed */
    private $previousWpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options   = [];
        $this->network   = [];
        $this->multisite = false;

        $this->hadWpdb      = array_key_exists('wpdb', $GLOBALS);
        $this->previousWpdb = $GLOBALS['wpdb'] ?? null;
        $GLOBALS['wpdb']    = new FakeOptionsTableWpdb($this->options, $this->network, 1);

        Functions\when('is_multisite')->alias(fn (): bool => $this->multisite);
        Functions\when('get_current_network_id')->justReturn(1);

        Functions\when('get_option')->alias(function ($name, $default = false) {
            return array_key_exists($name, $this->options) ? $this->options[$name] : $default;
        });
        Functions\when('update_option')->alias(function ($name, $value): bool {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('delete_option')->alias(fn ($name): bool => self::deleteRow($this->options, (string) $name));

        // Network options proxy to the options table on single-site, as core does.
        Functions\when('get_site_option')->alias(function ($name, $default = false) {
            $rows = $this->multisite ? $this->network : $this->options;
            return array_key_exists($name, $rows) ? $rows[$name] : $default;
        });
        Functions\when('update_site_option')->alias(function ($name, $value): bool {
            if ($this->multisite) {
                $this->network[$name] = $value;
            } else {
                $this->options[$name] = $value;
            }
            return true;
        });
        Functions\when('delete_site_option')->alias(function ($name): bool {
            return $this->multisite
                ? self::deleteRow($this->network, (string) $name)
                : self::deleteRow($this->options, (string) $name);
        });

        // Database-backed transients, with core's order: the timeout row goes
        // only when the value row did.
        Functions\when('delete_transient')->alias(function ($name): bool {
            $deleted = self::deleteRow($this->options, '_transient_' . $name);
            if ($deleted) {
                self::deleteRow($this->options, '_transient_timeout_' . $name);
            }
            return $deleted;
        });
        Functions\when('delete_site_transient')->alias(function ($name): bool {
            if ($this->multisite) {
                $deleted = self::deleteRow($this->network, '_site_transient_' . $name);
                if ($deleted) {
                    self::deleteRow($this->network, '_site_transient_timeout_' . $name);
                }
                return $deleted;
            }
            $deleted = self::deleteRow($this->options, '_site_transient_' . $name);
            if ($deleted) {
                self::deleteRow($this->options, '_site_transient_timeout_' . $name);
            }
            return $deleted;
        });

        Functions\when('wp_clear_scheduled_hook')->justReturn(0);
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_parse_url')->alias(static fn ($url, $component = -1) => parse_url((string) $url, (int) $component));
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->previousWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Every agent option, and the transient rows of every name in the
     * namespace, is gone from the options table after uninstall.
     */
    public function test_wipe_all_leaves_no_row_in_the_options_table_namespace(): void
    {
        $names = array_merge(array_values(self::namespaceConstants()), self::NAMES_WITHOUT_A_CONSTANT);
        foreach ($names as $name) {
            foreach (self::OPTIONS_TABLE_FORMS as $form) {
                $this->options[$form . substr($name, strlen(self::PREFIX))] = 'seeded';
            }
        }
        $this->options['siteurl'] = 'https://example.test';
        $this->assertContains('_transient_timeout_wpmgr_agent_email_secret', array_keys($this->options), 'precondition: every form was seeded');

        $this->lifecycle()->wipeAll();

        $this->assertSame(
            [],
            self::rowsIn($this->options, self::OPTIONS_TABLE_FORMS),
            'uninstall left rows in the agent namespace'
        );
        $this->assertSame('https://example.test', $this->options['siteurl'] ?? null, 'uninstall must not touch rows outside the namespace');
    }

    /**
     * On multisite the agent keeps its enrollment network-wide; those rows,
     * and network site transients in the namespace, go too.
     */
    public function test_wipe_all_removes_network_rows_on_multisite(): void
    {
        $this->multisite = true;

        // Written through the production Settings API, so the rows land wherever
        // Settings really puts them on multisite.
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://cp.example.test');
        $settings->markActivated(1700000000);
        $settings->setEnrollment('site-copy', 'tenant-copy');
        $settings->setLastHeartbeat(1700000100);
        $settings->setLastMetadata(1700000200);
        $this->network['_site_transient_wpmgr_agent_update_manifest']         = 'seeded';
        $this->network['_site_transient_timeout_wpmgr_agent_update_manifest'] = 'seeded';
        $this->network['wpmgr_agent_name_from_a_future_release']              = 'seeded';
        $this->network['site_name']                                            = 'Network';

        $this->assertArrayHasKey(Settings::OPTION_CP_URL, $this->network, 'precondition: Settings writes network-wide on multisite');
        $this->assertArrayHasKey(Settings::OPTION_ACTIVATED_AT, $this->network, 'precondition: Settings writes network-wide on multisite');

        $this->lifecycle()->wipeAll();

        $this->assertSame(
            [],
            self::rowsIn($this->network, self::NETWORK_TABLE_FORMS),
            'uninstall left network rows in the agent namespace'
        );
        $this->assertSame('Network', $this->network['site_name'] ?? null, 'uninstall must not touch network rows outside the namespace');
    }

    /**
     * Every option constant in the namespace is removed through the options
     * API by name, so uninstall stays complete where the database sweep sees
     * nothing (here: no $wpdb at all).
     */
    public function test_every_named_option_is_removed_without_the_database_sweep(): void
    {
        unset($GLOBALS['wpdb']);
        $this->multisite = true;

        $optionConstants = self::namespaceOptionConstants();
        foreach ($optionConstants as $value) {
            $this->options[$value] = 'seeded';
        }
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://cp.example.test');
        $settings->markActivated(1700000000);
        $settings->setEnrollment('site-copy', 'tenant-copy');

        $this->lifecycle()->wipeAll();

        $left = [];
        foreach ($optionConstants as $constant => $value) {
            if (array_key_exists($value, $this->options)) {
                $left[] = $constant . ' (options table)';
            }
            if (array_key_exists($value, $this->network)) {
                $left[] = $constant . ' (network table)';
            }
        }
        $this->assertSame([], $left, 'uninstall does not remove these by name: ' . implode(', ', $left));
    }

    /**
     * The sweep is anchored, escapes LIKE wildcards, and re-checks the exact
     * prefix: nothing outside the namespace is selected or removed.
     */
    public function test_wipe_all_spares_names_outside_the_namespace(): void
    {
        $this->multisite = true;

        // Names a careless pattern would reach: an unescaped '_' matches any
        // character, an unanchored pattern matches the prefix anywhere.
        $mustNotBeSelected = [
            'wpmgrXagentXlookalike',
            'wpmgr_agentXlookalike',
            'wpmgr_agent',
            'my_wpmgr_agent_setting',
            'xwpmgr_agent_x',
            '_transient_other_wpmgr_agent_x',
            '_transient_wpmgrXagentXx',
            'wpmgr_some_other_feature',
            'siteurl',
            '_transient_doing_cron',
            '_site_transient_theme_roots',
        ];
        // MySQL's default collations compare LIKE case-insensitively, so the
        // query may return this one; the exact prefix check must still spare it.
        $caseVariant = 'WPMGR_AGENT_SHOUTING';

        foreach (array_merge($mustNotBeSelected, [$caseVariant]) as $name) {
            $this->options[$name] = 'keep:' . $name;
            $this->network[$name] = 'keep:' . $name;
        }
        // One row in the namespace per table, so the sweep has work to do.
        $this->options['wpmgr_agent_name_from_a_future_release'] = 'seeded';
        $this->network['wpmgr_agent_name_from_a_future_release'] = 'seeded';

        $wpdb = $GLOBALS['wpdb'];
        $this->assertInstanceOf(FakeOptionsTableWpdb::class, $wpdb);

        $this->lifecycle()->wipeAll();

        $this->assertNotSame([], $wpdb->likePatterns, 'the namespace sweep never queried the database');
        $this->assertArrayNotHasKey('wpmgr_agent_name_from_a_future_release', $this->options);
        $this->assertArrayNotHasKey('wpmgr_agent_name_from_a_future_release', $this->network);

        $this->assertSame(
            [],
            array_values(array_intersect($mustNotBeSelected, $wpdb->selected)),
            'the sweep query selected names outside the namespace'
        );
        foreach (array_merge($mustNotBeSelected, [$caseVariant]) as $name) {
            $this->assertSame('keep:' . $name, $this->options[$name] ?? null, "options row {$name} must survive uninstall");
            $this->assertSame('keep:' . $name, $this->network[$name] ?? null, "network row {$name} must survive uninstall");
        }
    }

    /**
     * The prefix in code and the prefix in the published contract are the
     * same string, and the contract names every form a script must sweep.
     */
    public function test_namespace_prefix_is_the_published_contract(): void
    {
        $this->assertSame(self::PREFIX, Lifecycle::NAME_PREFIX);

        $readme = (string) file_get_contents(dirname(__DIR__) . '/README.md');
        $this->assertStringContainsString('## Option namespace contract', $readme);
        foreach (self::OPTIONS_TABLE_FORMS as $form) {
            $this->assertStringContainsString('`' . $form . '*`', $readme, "the contract must name the {$form}* form");
        }
    }

    /**
     * Uninstall's object graph, as Lifecycle::on_uninstall() builds it.
     *
     * @return Lifecycle
     */
    private function lifecycle(): Lifecycle
    {
        $keystore = new Keystore();
        $settings = new Settings();

        return new Lifecycle(
            $keystore,
            $settings,
            new Enrollment($keystore, $settings, new Signer($keystore), new MetadataCommand())
        );
    }

    /**
     * Every string constant declared on an agent class whose value lies in the
     * namespace, as "Class::NAME" => value.
     *
     * @return array<string,string>
     */
    private static function namespaceConstants(): array
    {
        /** @var array<string,string> $classMap */
        $classMap = require dirname(__DIR__) . '/vendor/composer/autoload_classmap.php';

        $found = [];
        foreach (array_keys($classMap) as $class) {
            if (strpos($class, 'WPMgr\\Agent\\') !== 0 || strpos($class, 'WPMgr\\Agent\\Tests\\') === 0) {
                continue;
            }
            try {
                $reflection = new \ReflectionClass($class);
            } catch (\Throwable $e) {
                continue; // Cannot load in the test process; see the file docblock.
            }
            foreach ($reflection->getReflectionConstants() as $constant) {
                if ($constant->getDeclaringClass()->getName() !== $reflection->getName()) {
                    continue;
                }
                $value = $constant->getValue();
                if (is_string($value) && strpos($value, self::PREFIX) === 0) {
                    $found[$reflection->getName() . '::' . $constant->getName()] = $value;
                }
            }
        }

        // Positive controls: discovery must find the identity rows the contract
        // exists for, or every assertion built on it would pass vacuously.
        foreach (
            [
                Keystore::class . '::OPTION_SITE_KEYPAIR',
                Keystore::class . '::OPTION_AGE_IDENTITY',
                Keystore::class . '::OPTION_EMAIL_SECRET',
                Settings::class . '::OPTION_SITE_ID',
            ] as $control
        ) {
            if (!array_key_exists($control, $found)) {
                throw new \RuntimeException("constant discovery is broken: it did not find {$control}");
            }
        }

        return $found;
    }

    /**
     * The namespace constants that name an option (OPTION or OPTION_*).
     *
     * @return array<string,string>
     */
    private static function namespaceOptionConstants(): array
    {
        return array_filter(
            self::namespaceConstants(),
            static fn (string $key): bool => preg_match('/::OPTION(_[A-Z0-9_]+)?$/', $key) === 1,
            ARRAY_FILTER_USE_KEY
        );
    }

    /**
     * Names in $rows that start, case-sensitively, with one of $forms.
     *
     * @param array<string,mixed> $rows  Table rows keyed by name.
     * @param list<string>        $forms Row-name prefixes.
     * @return list<string>
     */
    private static function rowsIn(array $rows, array $forms): array
    {
        $hits = [];
        foreach (array_keys($rows) as $name) {
            foreach ($forms as $form) {
                if (strpos((string) $name, $form) === 0) {
                    $hits[] = (string) $name;
                    break;
                }
            }
        }
        sort($hits);

        return $hits;
    }

    /**
     * @param array<string,mixed> $rows Table rows keyed by name.
     * @param string              $name Row to delete.
     * @return bool Whether a row was deleted.
     */
    private static function deleteRow(array &$rows, string $name): bool
    {
        if (!array_key_exists($name, $rows)) {
            return false;
        }
        unset($rows[$name]);

        return true;
    }
}
