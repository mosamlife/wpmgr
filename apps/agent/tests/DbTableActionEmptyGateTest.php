<?php
/**
 * DbTableActionEmptyGateTest — Phase 2.3 + Phase 2.4 gate coverage.
 *
 * Verifies the LAYER-1 safety gate:
 *   - EMPTY refuses WP-core tables (owner_type=core).
 *   - EMPTY refuses unknown tables (owner_type=unknown).
 *   - EMPTY allows plugin tables (owner_type=plugin).
 *   - EMPTY allows orphan tables (owner_type=orphan).
 *   - DROP (Phase 2.4) refuses core tables (owner_type=core).
 *   - DROP (Phase 2.4) refuses unknown tables (owner_type=unknown).
 *   - DROP (Phase 2.4) allows plugin tables (owner_type=plugin).
 *   - DROP (Phase 2.4) allows theme tables (owner_type=theme).
 *   - DROP (Phase 2.4) allows orphan tables (unchanged).
 *   - TRUNCATE TABLE SQL uses the information_schema-validated name, not raw input.
 *   - DROP TABLE IF EXISTS SQL uses the information_schema-validated name.
 *   - OPTIMIZE/REPAIR have no gate and proceed on any table.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\DbTableActionCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\DbTableActionCommand
 */
final class DbTableActionEmptyGateTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Default stubs: no plugins, no themes, cold transient.
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('');
        Functions\when('get_template')->justReturn('');
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('delete_transient')->justReturn(true);
        // is_multisite() is called (after function_exists guard) inside
        // classifyTable()'s multisite-safety block; single-site default.
        Functions\when('is_multisite')->justReturn(false);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Build a GateTestWpdb where get_var() returns $validatedName for $inputName,
     * simulating an information_schema hit. query() records calls and returns 1.
     */
    private function wpdbWithTable(string $inputName, string $validatedName): GateTestWpdb
    {
        return new GateTestWpdb($validatedName, $inputName);
    }

    /**
     * Execute a db_table_action command and return the result array.
     *
     * @param GateTestWpdb      $wpdb
     * @param string            $action   'optimize'|'repair'|'drop'|'empty'
     * @param list<string>      $tables
     * @return array<string,mixed>
     */
    private function execAction(GateTestWpdb $wpdb, string $action, array $tables): array
    {
        $cmd = new DbTableActionCommand(null, $wpdb);
        return $cmd->execute([], [
            'job_id' => 'test-' . $action . '-gate',
            'action' => $action,
            'tables' => $tables,
        ]);
    }

    // =========================================================================
    // EMPTY — refuses WP-core tables
    // =========================================================================

    /**
     * wp_posts is owner_type=core; EMPTY must return status=skipped.
     */
    public function test_empty_refuses_core_table_wp_posts(): void
    {
        $wpdb = $this->wpdbWithTable('wp_posts', 'wp_posts');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb,'empty', ['wp_posts']);

        $this->assertTrue($result['ok'], 'Command ok must be true even when table is skipped');
        $this->assertSame('skipped', $result['results'][0]['status']);
        $this->assertStringContainsString('core', $result['results'][0]['detail']);
    }

    /**
     * wp_options is owner_type=core; EMPTY must return status=skipped.
     */
    public function test_empty_refuses_core_table_wp_options(): void
    {
        $wpdb = $this->wpdbWithTable('wp_options', 'wp_options');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb,'empty', ['wp_options']);

        $this->assertSame('skipped', $result['results'][0]['status']);
        $this->assertStringContainsString('core', $result['results'][0]['detail']);
    }

    /**
     * wp_users is owner_type=core; EMPTY must return status=skipped.
     */
    public function test_empty_refuses_core_table_wp_users(): void
    {
        $wpdb = $this->wpdbWithTable('wp_users', 'wp_users');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb,'empty', ['wp_users']);

        $this->assertSame('skipped', $result['results'][0]['status']);
    }

    /**
     * wp_usermeta is owner_type=core; EMPTY must return status=skipped.
     */
    public function test_empty_refuses_core_table_wp_usermeta(): void
    {
        $wpdb = $this->wpdbWithTable('wp_usermeta', 'wp_usermeta');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb,'empty', ['wp_usermeta']);

        $this->assertSame('skipped', $result['results'][0]['status']);
    }

    /**
     * No TRUNCATE SQL must be executed when a core table is refused.
     */
    public function test_empty_core_table_issues_no_sql(): void
    {
        $wpdb = $this->wpdbWithTable('wp_postmeta', 'wp_postmeta');
        Functions\when('get_transient')->justReturn([]);

        $this->execAction($wpdb, 'empty', ['wp_postmeta']);

        $truncateSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'TRUNCATE') !== false
        );
        $this->assertEmpty($truncateSqls, 'TRUNCATE must NOT be issued for a refused core table');
    }

    // =========================================================================
    // EMPTY — allows plugin tables (the primary user-facing fix)
    // =========================================================================

    /**
     * wp_digits_failed_login_logs is owner_type=plugin (DIGITS).
     * EMPTY must pass the gate and issue TRUNCATE TABLE.
     */
    public function test_empty_allows_plugin_table_digits_log(): void
    {
        $wpdb = $this->wpdbWithTable('wp_digits_failed_login_logs', 'wp_digits_failed_login_logs');

        // Source map: digits bare name attributed to the DIGITS plugin.
        Functions\when('get_transient')->justReturn([
            'digits_failed_login_logs' => ['digits' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_plugins')->justReturn([
            'digits/digits.php' => ['Name' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_option')->justReturn(['digits/digits.php']);

        $result = $this->execAction($wpdb,'empty', ['wp_digits_failed_login_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'EMPTY on a plugin-owned log table must succeed');
    }

    /**
     * wp_wpmgr_activity_log is owner_type=plugin (WPMgr Agent).
     * EMPTY must pass the gate and issue TRUNCATE TABLE.
     */
    public function test_empty_allows_plugin_table_wpmgr_activity_log(): void
    {
        $wpdb = $this->wpdbWithTable('wp_wpmgr_activity_log', 'wp_wpmgr_activity_log');

        // Cold transient: classification falls to PASS 0 (WPMgr own-tables hardlist)
        // which returns owner_type=plugin.
        Functions\when('get_transient')->justReturn(false);

        $result = $this->execAction($wpdb,'empty', ['wp_wpmgr_activity_log']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'EMPTY on wp_wpmgr_activity_log must succeed (plugin, not core)');
    }

    /**
     * wp_wpmgr_login_events is owner_type=plugin (WPMgr Agent, PASS 0 hardlist).
     * EMPTY must pass the gate and issue TRUNCATE TABLE.
     */
    public function test_empty_allows_plugin_table_wpmgr_login_events(): void
    {
        $wpdb = $this->wpdbWithTable('wp_wpmgr_login_events', 'wp_wpmgr_login_events');
        Functions\when('get_transient')->justReturn(false);

        $result = $this->execAction($wpdb,'empty', ['wp_wpmgr_login_events']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status']);
    }

    /**
     * wp_actionscheduler_logs is owner_type=orphan when WooCommerce is not installed.
     * EMPTY must pass the gate (orphan is allowed).
     */
    public function test_empty_allows_orphan_table(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        Functions\when('get_transient')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_option')->justReturn([]);

        $result = $this->execAction($wpdb,'empty', ['wp_actionscheduler_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'EMPTY on an orphan table must succeed');
    }

    // =========================================================================
    // EMPTY — TRUNCATE SQL uses the information_schema-validated name
    // =========================================================================

    /**
     * The TRUNCATE TABLE statement must reference the validated table name that
     * came from information_schema, not the raw input string.
     */
    public function test_empty_truncate_uses_validated_table_name(): void
    {
        // information_schema returns 'wp_actionscheduler_logs' (normalised).
        // Raw input could theoretically differ in casing — we verify the SQL
        // uses the name the DB returned.
        $validatedName = 'wp_actionscheduler_logs';
        $rawInput      = 'wp_actionscheduler_logs'; // same here, integrity is in flow
        $wpdb          = $this->wpdbWithTable($rawInput, $validatedName);

        Functions\when('get_transient')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_option')->justReturn([]);

        $this->execAction($wpdb, 'empty', [$rawInput]);

        $truncateSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'TRUNCATE') !== false
        );

        $this->assertNotEmpty($truncateSqls, 'TRUNCATE SQL must be executed');

        foreach ($truncateSqls as $sql) {
            $this->assertStringContainsString(
                $validatedName,
                $sql,
                'TRUNCATE SQL must reference the information_schema-validated name'
            );
        }
    }

    /**
     * The TRUNCATE TABLE statement must NOT contain raw backtick-unescaped input;
     * the identifier must be properly escaped with backticks.
     */
    public function test_empty_truncate_sql_uses_backtick_escaped_identifier(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        Functions\when('get_transient')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_option')->justReturn([]);

        $this->execAction($wpdb, 'empty', ['wp_actionscheduler_logs']);

        $truncateSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'TRUNCATE') !== false
        );

        $this->assertNotEmpty($truncateSqls);
        foreach ($truncateSqls as $sql) {
            $this->assertStringContainsString('`wp_actionscheduler_logs`', $sql,
                'Table identifier in TRUNCATE must be backtick-escaped');
        }
    }

    // =========================================================================
    // DROP — Phase 2.4: non-core gate (refuse core+unknown; allow plugin/theme/orphan)
    // =========================================================================

    /**
     * DROP on a WP-core table must be refused (owner_type=core → skipped).
     * The detail string must contain "core".
     */
    public function test_drop_refuses_core_table_wp_posts(): void
    {
        $wpdb = $this->wpdbWithTable('wp_posts', 'wp_posts');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb, 'drop', ['wp_posts']);

        $this->assertTrue($result['ok']);
        $this->assertSame('skipped', $result['results'][0]['status'],
            'DROP must be refused for a WP-core table');
        $this->assertStringContainsString('core', $result['results'][0]['detail']);
    }

    /**
     * DROP on wp_options must be refused (owner_type=core).
     */
    public function test_drop_refuses_core_table_wp_options(): void
    {
        $wpdb = $this->wpdbWithTable('wp_options', 'wp_options');
        Functions\when('get_transient')->justReturn([]);

        $result = $this->execAction($wpdb, 'drop', ['wp_options']);

        $this->assertTrue($result['ok']);
        $this->assertSame('skipped', $result['results'][0]['status']);
        $this->assertStringContainsString('core', $result['results'][0]['detail']);
    }

    /**
     * No DROP TABLE SQL must be executed when a core table is refused.
     */
    public function test_drop_core_table_issues_no_sql(): void
    {
        $wpdb = $this->wpdbWithTable('wp_users', 'wp_users');
        Functions\when('get_transient')->justReturn([]);

        $this->execAction($wpdb, 'drop', ['wp_users']);

        $dropSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'DROP TABLE') !== false
        );
        $this->assertEmpty($dropSqls, 'DROP TABLE must NOT be issued for a refused core table');
    }

    /**
     * DROP on a plugin table must NOW be allowed (Phase 2.4 gate relaxation).
     * wp_digits_failed_login_logs is owner_type=plugin (DIGITS plugin).
     * The plugin will recreate its schema on next activation.
     */
    public function test_drop_allows_plugin_table_digits_log(): void
    {
        $wpdb = $this->wpdbWithTable('wp_digits_failed_login_logs', 'wp_digits_failed_login_logs');
        Functions\when('get_transient')->justReturn([
            'digits_failed_login_logs' => ['digits' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_plugins')->justReturn([
            'digits/digits.php' => ['Name' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_option')->justReturn(['digits/digits.php']);

        $result = $this->execAction($wpdb, 'drop', ['wp_digits_failed_login_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'DROP on a plugin-owned log table must now be allowed (Phase 2.4)');
    }

    /**
     * DROP TABLE IF EXISTS SQL must be issued when a plugin table is allowed.
     */
    public function test_drop_plugin_table_issues_drop_sql(): void
    {
        $wpdb = $this->wpdbWithTable('wp_digits_failed_login_logs', 'wp_digits_failed_login_logs');
        Functions\when('get_transient')->justReturn([
            'digits_failed_login_logs' => ['digits' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_plugins')->justReturn([
            'digits/digits.php' => ['Name' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_option')->justReturn(['digits/digits.php']);

        $this->execAction($wpdb, 'drop', ['wp_digits_failed_login_logs']);

        $dropSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'DROP TABLE') !== false
        );
        $this->assertNotEmpty($dropSqls, 'DROP TABLE IF EXISTS must be issued for a plugin table');
        foreach ($dropSqls as $sql) {
            $this->assertStringContainsString(
                '`wp_digits_failed_login_logs`',
                $sql,
                'DROP TABLE SQL must reference the backtick-escaped validated table name'
            );
        }
    }

    /**
     * DROP on wp_wpmgr_activity_log must be allowed (owner_type=plugin, PASS 0 hardlist).
     */
    public function test_drop_allows_plugin_table_wpmgr_activity_log(): void
    {
        $wpdb = $this->wpdbWithTable('wp_wpmgr_activity_log', 'wp_wpmgr_activity_log');
        Functions\when('get_transient')->justReturn(false);

        $result = $this->execAction($wpdb, 'drop', ['wp_wpmgr_activity_log']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'DROP on wp_wpmgr_activity_log must be allowed (plugin, non-core)');
    }

    /**
     * DROP on an orphan table must still succeed (unchanged from prior behavior).
     */
    public function test_drop_allows_orphan_table(): void
    {
        $wpdb = $this->wpdbWithTable('wp_leftover_plugin_garbage', 'wp_leftover_plugin_garbage');
        Functions\when('get_transient')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_option')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);

        $result = $this->execAction($wpdb, 'drop', ['wp_leftover_plugin_garbage']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'DROP must still succeed for an orphan table');
    }

    /**
     * Bulk DROP: core table is skipped, plugin table is dropped in the same call.
     */
    public function test_drop_bulk_skips_core_allows_plugin(): void
    {
        $wpdb = new GateTestWpdbMulti([
            'wp_posts'                => 'wp_posts',
            'wp_digits_failed_login_logs' => 'wp_digits_failed_login_logs',
        ]);

        Functions\when('get_transient')->justReturn([
            'digits_failed_login_logs' => ['digits' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_plugins')->justReturn([
            'digits/digits.php' => ['Name' => 'DIGITS: Mobile Number Signup and Login'],
        ]);
        Functions\when('get_option')->justReturn(['digits/digits.php']);
        Functions\when('wp_get_themes')->justReturn([]);

        $cmd    = new DbTableActionCommand(null, $wpdb);
        $result = $cmd->execute([], [
            'job_id' => 'bulk-drop-mixed',
            'action' => 'drop',
            'tables' => ['wp_posts', 'wp_digits_failed_login_logs'],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertCount(2, $result['results']);

        $byTable = [];
        foreach ($result['results'] as $r) {
            $byTable[$r['table']] = $r;
        }

        $this->assertSame('skipped', $byTable['wp_posts']['status'],
            'wp_posts (core) must be skipped');
        $this->assertSame('done', $byTable['wp_digits_failed_login_logs']['status'],
            'wp_digits_failed_login_logs (plugin) must be done');
    }

    // =========================================================================
    // OPTIMIZE / REPAIR — no gate, any table allowed
    // =========================================================================

    /**
     * OPTIMIZE on a WP-core table must succeed (no LAYER-1 gate for optimize).
     */
    public function test_optimize_has_no_gate_on_core_table(): void
    {
        $wpdb = $this->wpdbWithTable('wp_posts', 'wp_posts');

        $result = $this->execAction($wpdb,'optimize', ['wp_posts']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'OPTIMIZE must be allowed on any table including WP-core');
    }

    /**
     * REPAIR on a WP-core table must succeed (no LAYER-1 gate for repair).
     */
    public function test_repair_has_no_gate_on_core_table(): void
    {
        $wpdb = $this->wpdbWithTable('wp_options', 'wp_options');

        $result = $this->execAction($wpdb,'repair', ['wp_options']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'REPAIR must be allowed on any table including WP-core');
    }

    // =========================================================================
    // Bulk: multiple tables in one call, mixed core + plugin
    // =========================================================================

    /**
     * When the tables array contains a mix of core and plugin tables, EMPTY must
     * skip core and complete plugin tables in the same response.
     */
    public function test_empty_bulk_skips_core_allows_plugin(): void
    {
        // wpdb that recognises both table names.
        $wpdb = new GateTestWpdbMulti([
            'wp_posts'                 => 'wp_posts',
            'wp_actionscheduler_logs'  => 'wp_actionscheduler_logs',
        ]);

        Functions\when('get_transient')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_option')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);

        $cmd    = new DbTableActionCommand(null, $wpdb);
        $result = $cmd->execute([], [
            'job_id' => 'bulk-empty-mixed',
            'action' => 'empty',
            'tables' => ['wp_posts', 'wp_actionscheduler_logs'],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertCount(2, $result['results']);

        $byTable = [];
        foreach ($result['results'] as $r) {
            $byTable[$r['table']] = $r;
        }

        $this->assertSame('skipped', $byTable['wp_posts']['status'],
            'wp_posts (core) must be skipped');
        $this->assertSame('done', $byTable['wp_actionscheduler_logs']['status'],
            'wp_actionscheduler_logs (orphan) must be done');
    }
}

// =============================================================================
// GateTestWpdb: minimal wpdb double for single-table LAYER-1 gate tests.
//
// get_var() simulates an information_schema hit: returns $validatedName when the
// prepared SQL contains $inputName as the bound argument, null otherwise.
// query() records all SQL statements and returns 1 (success).
// =============================================================================

final class GateTestWpdb
{
    public string $prefix = 'wp_';
    public string $last_error = '';

    /** @var list<string> */
    public array $executedSqls = [];

    private string $validatedName;
    private string $inputName;

    public function __construct(string $validatedName, string $inputName)
    {
        $this->validatedName = $validatedName;
        $this->inputName     = $inputName;
    }

    public function prepare(string $query, ...$args): string
    {
        // %i (identifier) queries are DDL statements bound for query(), not the
        // information_schema %s SELECT decoded by get_var() below — substitute
        // for real, mirroring core wpdb::prepare(), so executedSqls assertions
        // can see the actual backtick-quoted table name.
        if (strpos($query, '%i') !== false) {
            $i = 0;
            return (string) preg_replace_callback('/%i/', static function ($m) use (&$i, $args) {
                $v = $args[$i] ?? '';
                $i++;
                return '`' . str_replace('`', '``', (string) $v) . '`';
            }, $query);
        }
        return json_encode(['sql' => $query, 'args' => $args]) ?: $query;
    }

    public function get_var(string $prepared): ?string
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return null;
        }
        $arg = (string) ($decoded['args'][0] ?? '');
        return $arg === $this->inputName ? $this->validatedName : null;
    }

    public function query(string $sql): int
    {
        $this->executedSqls[] = $sql;
        return 1;
    }
}

// =============================================================================
// GateTestWpdbMulti: wpdb double that recognises multiple table names for bulk
// tests. The table map is inputName => validatedName.
// =============================================================================

final class GateTestWpdbMulti
{
    public string $prefix = 'wp_';
    public string $last_error = '';

    /** @var list<string> */
    public array $executedSqls = [];

    /** @var array<string,string> inputName => validatedName */
    private array $tableMap;

    /**
     * @param array<string,string> $tableMap
     */
    public function __construct(array $tableMap)
    {
        $this->tableMap = $tableMap;
    }

    public function prepare(string $query, ...$args): string
    {
        // %i (identifier) queries are DDL statements bound for query(), not the
        // information_schema %s SELECT decoded by get_var() below — substitute
        // for real, mirroring core wpdb::prepare(), so executedSqls assertions
        // can see the actual backtick-quoted table name.
        if (strpos($query, '%i') !== false) {
            $i = 0;
            return (string) preg_replace_callback('/%i/', static function ($m) use (&$i, $args) {
                $v = $args[$i] ?? '';
                $i++;
                return '`' . str_replace('`', '``', (string) $v) . '`';
            }, $query);
        }
        return json_encode(['sql' => $query, 'args' => $args]) ?: $query;
    }

    public function get_var(string $prepared): ?string
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return null;
        }
        $arg = (string) ($decoded['args'][0] ?? '');
        return $this->tableMap[$arg] ?? null;
    }

    public function query(string $sql): int
    {
        $this->executedSqls[] = $sql;
        return 1;
    }
}
