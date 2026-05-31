<?php
/**
 * Tests for Schema::ensureCurrent — the WP "plugin upgrade routine" that
 * heals missing tables on re-uploads/same-version installs where
 * register_activation_hook does NOT fire.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Connector;
use WPMgr\Agent\ReplayCache;
use WPMgr\Agent\Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Schema
 */
final class SchemaTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var array<int,string> Captured dbDelta() SQL invocations. */
    private array $dbDeltaCalls = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options      = [];
        $this->dbDeltaCalls = [];

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);
            return true;
        });

        // Provide a tiny $wpdb double so Schema::definitions() returns a map
        // with a real prefix and (empty) charset string.
        $GLOBALS['wpdb'] = new FakeSchemaWpdb();

        // Stand-in dbDelta capture. Because Schema::ensureCurrent attempts to
        // require_once ABSPATH . 'wp-admin/includes/upgrade.php' and the test
        // sandbox has neither ABSPATH nor that file, function_exists('dbDelta')
        // returns true only because we declare a real function here at the
        // class-test-file level via the helper below.
        TestDbDeltaCapture::reset();
        if (!function_exists('dbDelta')) {
            eval('function dbDelta(string $sql): array { \WPMgr\Agent\Tests\TestDbDeltaCapture::record($sql); return []; }');
        }
        TestDbDeltaCapture::$onRecord = function (string $sql): void {
            $this->dbDeltaCalls[] = $sql;
        };
    }

    protected function tear_down(): void
    {
        TestDbDeltaCapture::$onRecord = null;
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_ensure_current_upgrades_from_zero_and_calls_dbdelta_for_each_table(): void
    {
        // No stored version => the migration runs and stamps the option.
        $this->assertArrayNotHasKey(Schema::OPTION_DB_VERSION, $this->options);

        Schema::ensureCurrent();

        $this->assertSame(Schema::CURRENT_VERSION, $this->options[Schema::OPTION_DB_VERSION]);

        // One dbDelta call per declared table.
        $this->assertCount(count(Schema::definitions()), $this->dbDeltaCalls);

        // Both expected tables appear in the captured SQL.
        $joined = implode("\n", $this->dbDeltaCalls);
        $this->assertStringContainsString('wp_' . Connector::JTI_TABLE, $joined);
        $this->assertStringContainsString('wp_' . ReplayCache::TABLE, $joined);
    }

    public function test_ensure_current_short_circuits_when_version_matches(): void
    {
        $this->options[Schema::OPTION_DB_VERSION] = Schema::CURRENT_VERSION;

        Schema::ensureCurrent();

        $this->assertSame([], $this->dbDeltaCalls, 'dbDelta must NOT run when schema is already current.');
    }

    public function test_ensure_current_force_runs_even_when_version_matches(): void
    {
        // Same-version BUT force=true (the autologin self-heal path).
        $this->options[Schema::OPTION_DB_VERSION] = Schema::CURRENT_VERSION;

        Schema::ensureCurrent(true);

        $this->assertCount(count(Schema::definitions()), $this->dbDeltaCalls);
        // Option remains current (idempotent).
        $this->assertSame(Schema::CURRENT_VERSION, $this->options[Schema::OPTION_DB_VERSION]);
    }

    public function test_definitions_returns_create_table_sql_for_every_agent_table(): void
    {
        $defs = Schema::definitions();

        $this->assertArrayHasKey(Connector::JTI_TABLE, $defs);
        $this->assertArrayHasKey(ReplayCache::TABLE, $defs);

        foreach ($defs as $sql) {
            $this->assertStringContainsString('CREATE TABLE', $sql);
        }
    }
}

/**
 * Tiny $wpdb double for Schema unit tests: only needs prefix +
 * get_charset_collate() because Schema::definitions() touches no more.
 */
final class FakeSchemaWpdb
{
    public string $prefix = 'wp_';

    public function get_charset_collate(): string
    {
        return '';
    }
}
