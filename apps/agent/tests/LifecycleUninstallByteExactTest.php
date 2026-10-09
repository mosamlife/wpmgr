<?php
/**
 * GH #886: uninstall deletes exactly the rows it owns, byte for byte.
 *
 * Uninstall finds its rows with queries that compare under the column's
 * collation, keeps those whose stored name is exactly one it owns, and deletes
 * each by primary key with its name re-checked. These cases pin the parts a
 * case-insensitive database would otherwise blur:
 *
 *   - The delete picks the row by primary key. In an options table that has
 *     lost its unique key on option_name, the agent's row and another
 *     plugin's copy differing only in case are both stored, and a delete by
 *     name would remove both.
 *   - The name re-check compares bytes. When the id the agent read now carries
 *     a name that differs from the agent's only in case, the row is left.
 *   - The README's example query, which operators run by hand, compares bytes
 *     too.
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
final class LifecycleUninstallByteExactTest extends TestCase
{
    /** The agent's own row in every case below. */
    private const OURS = 'wpmgr_agent_site_id';

    /** Another plugin's row whose name differs from the agent's only in case. */
    private const THEIRS = 'WPMGR_AGENT_SITE_ID';

    /** The namespace forms the README example must sweep, as escaped LIKE patterns. */
    private const README_PATTERNS = [
        'wpmgr\\_agent\\_%',
        '\\_transient\\_wpmgr\\_agent\\_%',
        '\\_transient\\_timeout\\_wpmgr\\_agent\\_%',
        '\\_site\\_transient\\_wpmgr\\_agent\\_%',
        '\\_site\\_transient\\_timeout\\_wpmgr\\_agent\\_%',
    ];

    /** @var array<string,mixed> The per-site options table. */
    private array $options = [];

    /** @var array<string,mixed> The network table of network 1. */
    private array $network = [];

    private bool $multisite = false;

    private FakeOptionsTableWpdb $wpdb;

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
        $this->wpdb         = new FakeOptionsTableWpdb($this->options, $this->network, 1);
        $GLOBALS['wpdb']    = $this->wpdb;

        Functions\when('is_multisite')->alias(fn (): bool => $this->multisite);
        Functions\when('get_current_network_id')->justReturn(1);
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('get_site_option')->alias(fn ($name, $default = false) => $default);
        Functions\when('wp_using_ext_object_cache')->justReturn(false);
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('wp_clear_scheduled_hook')->justReturn(0);
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_parse_url')->alias(static fn ($url, $component = -1) => parse_url((string) $url, (int) $component));

        // Uninstall removes rows with SQL only. A call through the options
        // API would delete by a name the database matches whatever its case.
        foreach (['delete_option', 'delete_site_option', 'delete_transient', 'delete_site_transient'] as $api) {
            Functions\when($api)->alias(static function () use ($api): bool {
                throw new \LogicException("uninstall called {$api}(), which deletes by name");
            });
        }
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
     * The fake refuses, by default, what an options table with its unique key
     * refuses, so the case below has to ask for a table without one.
     */
    public function test_fake_options_table_keeps_its_unique_key_by_default(): void
    {
        $this->options[self::OURS]   = 'ours';
        $this->options[self::THEIRS] = 'theirs';

        $this->expectException(\LogicException::class);
        $this->expectExceptionMessage('unique key');
        $this->wpdb->get_results($this->wpdb->prepare('SELECT option_id AS id, option_name AS name FROM wp_options WHERE option_name = %s', self::OURS), ARRAY_A);
    }

    /**
     * An options table without its unique key on option_name holds the
     * agent's row and another plugin's copy that differs only in case.
     * Uninstall removes the agent's row by primary key and keeps the copy.
     * The network table never has such a key, and the same holds there.
     */
    public function test_uninstall_deletes_by_primary_key_when_a_case_variant_copy_shares_the_table(): void
    {
        $this->multisite              = true;
        $this->wpdb->optionsUniqueKey = false;
        $this->options[self::OURS]    = 'ours';
        $this->options[self::THEIRS]  = 'theirs';
        $this->network[self::OURS]    = 'ours';
        $this->network[self::THEIRS]  = 'theirs';

        $this->lifecycle()->wipeAll();

        $this->assertArrayNotHasKey(self::OURS, $this->options, "uninstall left the agent's own options row");
        $this->assertSame('theirs', $this->options[self::THEIRS] ?? null, 'uninstall deleted the copy in the options table along with the agent row');
        $this->assertArrayNotHasKey(self::OURS, $this->network, "uninstall left the agent's own network row");
        $this->assertSame('theirs', $this->network[self::THEIRS] ?? null, 'uninstall deleted the copy in the network table along with the agent row');
        $this->assertSame(
            ['wp_options:' . self::OURS, 'wp_sitemeta:' . self::OURS],
            $this->wpdb->deleted,
            'uninstall must delete exactly the two agent rows'
        );
    }

    /**
     * Between the read and the delete, the row with the id uninstall read
     * comes to carry a name that differs from the agent's only in case. The
     * name re-check compares bytes, so that row is not the agent's and stays.
     */
    public function test_uninstall_leaves_a_row_whose_id_now_carries_a_case_variant_name(): void
    {
        $this->multisite           = true;
        $this->options[self::OURS] = 'ours';
        $this->network[self::OURS] = 'ours';

        $renamed                  = [];
        $this->wpdb->beforeDelete = function (FakeOptionsTableWpdb $wpdb, string $table) use (&$renamed): void {
            if (!isset($renamed[$table])) {
                $renamed[$table] = true;
                $wpdb->renameRow($table, self::OURS, self::THEIRS);
            }
        };

        $this->lifecycle()->wipeAll();

        $this->assertSame(['wp_options' => true, 'wp_sitemeta' => true], $renamed, 'precondition: each table renamed its row before the delete');
        $this->assertSame('ours', $this->options[self::THEIRS] ?? null, 'uninstall deleted an options row whose name is not, byte for byte, the agent\'s');
        $this->assertSame('ours', $this->network[self::THEIRS] ?? null, 'uninstall deleted a network row whose name is not, byte for byte, the agent\'s');
        $this->assertSame([], $this->wpdb->deleted);
    }

    /**
     * The delete binds the name as the upper-case hex of its bytes, the form
     * HEX() returns on MySQL, MariaDB and SQLite, and the fake compares it
     * exactly. Positive control for the case above: an unrenamed row goes.
     */
    public function test_uninstall_still_deletes_the_agent_row_when_nothing_changed(): void
    {
        $this->options[self::OURS] = 'ours';

        $this->lifecycle()->wipeAll();

        $this->assertSame(['wp_options:' . self::OURS], $this->wpdb->deleted);
        $this->assertArrayNotHasKey(self::OURS, $this->options);
    }

    /**
     * The README's example query compares bytes. Under WordPress's default
     * collations a plain LIKE also returns another plugin's rows that differ
     * from the agent's only in case or accents, and an operator who deletes
     * what it returns removes them.
     */
    public function test_readme_example_query_compares_bytes(): void
    {
        $readme = (string) file_get_contents(dirname(__DIR__) . '/README.md');
        $start  = strpos($readme, '## Option namespace contract');
        $this->assertNotFalse($start, 'README.md has no Option namespace contract section');
        $section = substr($readme, (int) $start);

        $this->assertSame(1, preg_match('/```sql\n(.*?)```/s', $section, $m), 'the contract section has no SQL example');
        $sql = $m[1];

        $this->assertSame(
            count(self::README_PATTERNS),
            preg_match_all('/\bLIKE\b/i', $sql),
            'the example must sweep every namespace form, one LIKE each'
        );
        foreach (self::README_PATTERNS as $pattern) {
            $this->assertStringContainsString(
                "CAST(option_name AS BINARY) LIKE '" . $pattern . "'",
                $sql,
                "the example must match {$pattern} on the stored bytes"
            );
        }
        $this->assertSame(
            0,
            preg_match('/(?<!CAST\()\boption_name\s+(?:LIKE|=)/i', $sql),
            'the example compares option_name under the collation somewhere'
        );
        $this->assertMatchesRegularExpression('/\bcase\b/i', $section, 'the README must say why the example compares bytes');
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
}
