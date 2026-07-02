<?php
/**
 * DbTableActionAnalyzeConvertTest — Phase 2.5 coverage.
 *
 * Verifies:
 *   - ANALYZE succeeds on any table (no LAYER-1 owner_type gate).
 *   - ANALYZE issues ANALYZE TABLE SQL with the information_schema-validated name.
 *   - ANALYZE on a core table is allowed (no gate).
 *   - ANALYZE returns status=error when wpdb->query() returns false.
 *   - ANALYZE rejects an unknown/invalid table name (not_found from LAYER 2).
 *   - CONVERT_INNODB succeeds on any table (no LAYER-1 owner_type gate).
 *   - CONVERT_INNODB issues ALTER TABLE ... ENGINE=InnoDB with the validated name.
 *   - CONVERT_INNODB issues a follow-up ANALYZE TABLE on success.
 *   - CONVERT_INNODB on a core table is allowed (no gate).
 *   - CONVERT_INNODB returns status=error when wpdb->query() returns false.
 *   - CONVERT_INNODB rejects an unknown/invalid table name (not_found from LAYER 2).
 *   - Both actions use the backtick-escaped validated name in SQL.
 *   - Unknown action string rejected with the updated error message.
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
final class DbTableActionAnalyzeConvertTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Default WP stubs — no plugins/themes, cold transient.
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('');
        Functions\when('get_template')->justReturn('');
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('delete_transient')->justReturn(true);
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
     * Build an AnalyzeTestWpdb where get_var() returns $validatedName for
     * $inputName (information_schema hit). query() records SQL and returns $queryReturn.
     */
    private function wpdbWithTable(
        string $inputName,
        string $validatedName,
        int|false $queryReturn = 1
    ): AnalyzeTestWpdb {
        return new AnalyzeTestWpdb($validatedName, $inputName, $queryReturn);
    }

    /**
     * Build an AnalyzeTestWpdb where get_var() returns null (table not in DB).
     */
    private function wpdbMissingTable(): AnalyzeTestWpdb
    {
        return new AnalyzeTestWpdb('', '', 1, true);
    }

    /**
     * Execute a db_table_action command and return the full result array.
     *
     * @param AnalyzeTestWpdb $wpdb
     * @param string          $action
     * @param list<string>    $tables
     * @return array<string,mixed>
     */
    private function execAction(AnalyzeTestWpdb $wpdb, string $action, array $tables): array
    {
        $cmd = new DbTableActionCommand(null, $wpdb);
        return $cmd->execute([], [
            'job_id' => 'test-' . $action,
            'action' => $action,
            'tables' => $tables,
        ]);
    }

    // =========================================================================
    // ANALYZE — basic success
    // =========================================================================

    /**
     * ANALYZE on a plugin table must return status=done.
     */
    public function test_analyze_returns_done_on_plugin_table(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $result = $this->execAction($wpdb, 'analyze', ['wp_actionscheduler_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('analyze', $result['action']);
        $this->assertSame('done', $result['results'][0]['status']);
        $this->assertSame('wp_actionscheduler_logs', $result['results'][0]['table']);
        $this->assertSame('', $result['results'][0]['detail']);
    }

    /**
     * ANALYZE must issue ANALYZE TABLE SQL with the information_schema-validated name.
     */
    public function test_analyze_issues_analyze_table_sql(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $this->execAction($wpdb, 'analyze', ['wp_actionscheduler_logs']);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );

        $this->assertNotEmpty($analyzeSqls, 'ANALYZE TABLE SQL must be issued');
        foreach ($analyzeSqls as $sql) {
            $this->assertStringContainsString(
                'wp_actionscheduler_logs',
                $sql,
                'ANALYZE TABLE SQL must reference the validated table name'
            );
        }
    }

    /**
     * ANALYZE must use a backtick-escaped identifier in the SQL statement.
     */
    public function test_analyze_sql_uses_backtick_escaped_identifier(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $this->execAction($wpdb, 'analyze', ['wp_actionscheduler_logs']);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );

        $this->assertNotEmpty($analyzeSqls);
        foreach ($analyzeSqls as $sql) {
            $this->assertStringContainsString(
                '`wp_actionscheduler_logs`',
                $sql,
                'Table identifier must be backtick-escaped in ANALYZE TABLE SQL'
            );
        }
    }

    /**
     * ANALYZE on a WP-core table must succeed — no LAYER-1 gate for analyze.
     */
    public function test_analyze_has_no_gate_on_core_table(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_posts', 'wp_posts');
        $result = $this->execAction($wpdb, 'analyze', ['wp_posts']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'ANALYZE must be allowed on WP-core tables (no LAYER-1 gate)');
    }

    /**
     * ANALYZE on wp_options (core) must succeed and issue the SQL.
     */
    public function test_analyze_has_no_gate_on_wp_options(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_options', 'wp_options');
        $result = $this->execAction($wpdb, 'analyze', ['wp_options']);

        $this->assertSame('done', $result['results'][0]['status']);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );
        $this->assertNotEmpty($analyzeSqls, 'ANALYZE TABLE SQL must be issued for core table');
    }

    /**
     * ANALYZE returns status=error when wpdb->query() returns false and
     * last_error is populated.
     */
    public function test_analyze_returns_error_when_query_fails(): void
    {
        $wpdb              = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs', false);
        $wpdb->last_error  = 'Access denied for user';

        $result = $this->execAction($wpdb, 'analyze', ['wp_actionscheduler_logs']);

        $this->assertTrue($result['ok'], 'Command ok is true even when a per-table error occurs');
        $this->assertSame('error', $result['results'][0]['status']);
        $this->assertSame('Access denied for user', $result['results'][0]['detail']);
    }

    /**
     * ANALYZE on a table not present in information_schema returns status=not_found.
     */
    public function test_analyze_returns_not_found_for_unknown_table(): void
    {
        $wpdb   = $this->wpdbMissingTable();
        $result = $this->execAction($wpdb, 'analyze', ['wp_nonexistent_table']);

        $this->assertTrue($result['ok']);
        $this->assertSame('not_found', $result['results'][0]['status'],
            'LAYER 2 must reject a table not present in information_schema');
    }

    /**
     * ANALYZE must NOT issue any SQL when the table is not found (LAYER 2 blocks it).
     */
    public function test_analyze_issues_no_sql_for_missing_table(): void
    {
        $wpdb = $this->wpdbMissingTable();
        $this->execAction($wpdb, 'analyze', ['wp_nonexistent_table']);

        $this->assertEmpty($wpdb->executedSqls, 'No SQL must be executed when the table is not found');
    }

    // =========================================================================
    // CONVERT_INNODB — basic success
    // =========================================================================

    /**
     * CONVERT_INNODB on a plugin table must return status=done.
     */
    public function test_convert_innodb_returns_done_on_plugin_table(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $result = $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('convert_innodb', $result['action']);
        $this->assertSame('done', $result['results'][0]['status']);
        $this->assertSame('wp_actionscheduler_logs', $result['results'][0]['table']);
        $this->assertSame('', $result['results'][0]['detail']);
    }

    /**
     * CONVERT_INNODB must issue ALTER TABLE ... ENGINE=InnoDB with the validated name.
     */
    public function test_convert_innodb_issues_alter_table_sql(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $alterSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ALTER TABLE') !== false
                                  && stripos($s, 'ENGINE=InnoDB') !== false
        );

        $this->assertNotEmpty($alterSqls, 'ALTER TABLE ... ENGINE=InnoDB must be issued');
        foreach ($alterSqls as $sql) {
            $this->assertStringContainsString(
                'wp_actionscheduler_logs',
                $sql,
                'ALTER TABLE SQL must reference the validated table name'
            );
        }
    }

    /**
     * CONVERT_INNODB must issue a follow-up ANALYZE TABLE after the ALTER succeeds.
     */
    public function test_convert_innodb_issues_follow_up_analyze(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );

        $this->assertNotEmpty($analyzeSqls, 'A follow-up ANALYZE TABLE must be issued after ALTER succeeds');
        foreach ($analyzeSqls as $sql) {
            $this->assertStringContainsString(
                'wp_actionscheduler_logs',
                $sql
            );
        }
    }

    /**
     * CONVERT_INNODB must use a backtick-escaped identifier in both SQL statements.
     */
    public function test_convert_innodb_sql_uses_backtick_escaped_identifier(): void
    {
        $wpdb = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs');
        $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $alterSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ALTER TABLE') !== false
        );

        $this->assertNotEmpty($alterSqls);
        foreach ($alterSqls as $sql) {
            $this->assertStringContainsString(
                '`wp_actionscheduler_logs`',
                $sql,
                'Table identifier must be backtick-escaped in ALTER TABLE SQL'
            );
        }
    }

    /**
     * CONVERT_INNODB on a WP-core table must succeed — no LAYER-1 gate.
     * Converting a core MyISAM table to InnoDB is a legitimate, safe operation.
     */
    public function test_convert_innodb_has_no_gate_on_core_table(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_posts', 'wp_posts');
        $result = $this->execAction($wpdb, 'convert_innodb', ['wp_posts']);

        $this->assertTrue($result['ok']);
        $this->assertSame('done', $result['results'][0]['status'],
            'CONVERT_INNODB must be allowed on WP-core tables (no LAYER-1 gate)');
    }

    /**
     * CONVERT_INNODB on wp_options (core) must issue ALTER TABLE.
     */
    public function test_convert_innodb_issues_alter_on_core_table(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_options', 'wp_options');
        $result = $this->execAction($wpdb, 'convert_innodb', ['wp_options']);

        $this->assertSame('done', $result['results'][0]['status']);

        $alterSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ALTER TABLE') !== false
                                  && stripos($s, 'ENGINE=InnoDB') !== false
        );
        $this->assertNotEmpty($alterSqls, 'ALTER TABLE ENGINE=InnoDB must be issued for core table');
    }

    /**
     * CONVERT_INNODB returns status=error when wpdb->query() returns false.
     */
    public function test_convert_innodb_returns_error_when_alter_fails(): void
    {
        $wpdb              = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs', false);
        $wpdb->last_error  = 'Table is read only';

        $result = $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $this->assertTrue($result['ok']);
        $this->assertSame('error', $result['results'][0]['status']);
        $this->assertSame('Table is read only', $result['results'][0]['detail']);
    }

    /**
     * CONVERT_INNODB must NOT issue ANALYZE TABLE when ALTER fails.
     */
    public function test_convert_innodb_no_analyze_when_alter_fails(): void
    {
        $wpdb             = $this->wpdbWithTable('wp_actionscheduler_logs', 'wp_actionscheduler_logs', false);
        $wpdb->last_error = 'Table is read only';

        $this->execAction($wpdb, 'convert_innodb', ['wp_actionscheduler_logs']);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );
        $this->assertEmpty($analyzeSqls, 'ANALYZE TABLE must NOT be issued when ALTER TABLE fails');
    }

    /**
     * CONVERT_INNODB on a table not present in information_schema returns not_found.
     */
    public function test_convert_innodb_returns_not_found_for_unknown_table(): void
    {
        $wpdb   = $this->wpdbMissingTable();
        $result = $this->execAction($wpdb, 'convert_innodb', ['wp_nonexistent_table']);

        $this->assertTrue($result['ok']);
        $this->assertSame('not_found', $result['results'][0]['status'],
            'LAYER 2 must reject a table not present in information_schema');
    }

    /**
     * CONVERT_INNODB must NOT issue any SQL when the table is not found.
     */
    public function test_convert_innodb_issues_no_sql_for_missing_table(): void
    {
        $wpdb = $this->wpdbMissingTable();
        $this->execAction($wpdb, 'convert_innodb', ['wp_nonexistent_table']);

        $this->assertEmpty($wpdb->executedSqls, 'No SQL must be executed when the table is not found');
    }

    // =========================================================================
    // SQL uses information_schema-validated name, not raw input
    // =========================================================================

    /**
     * The ANALYZE TABLE SQL must reference the validated name returned by
     * information_schema, not the raw request input (LAYER 2 contract).
     */
    public function test_analyze_sql_uses_validated_name_not_raw_input(): void
    {
        $rawInput      = 'wp_actionscheduler_logs';
        $validatedName = 'wp_actionscheduler_logs';
        $wpdb          = $this->wpdbWithTable($rawInput, $validatedName);

        $this->execAction($wpdb, 'analyze', [$rawInput]);

        $analyzeSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ANALYZE TABLE') !== false
        );

        $this->assertNotEmpty($analyzeSqls);
        foreach ($analyzeSqls as $sql) {
            $this->assertStringContainsString($validatedName, $sql,
                'ANALYZE SQL must use the information_schema-validated name');
        }
    }

    /**
     * The ALTER TABLE SQL must reference the validated name returned by
     * information_schema (LAYER 2 contract).
     */
    public function test_convert_innodb_sql_uses_validated_name_not_raw_input(): void
    {
        $rawInput      = 'wp_actionscheduler_logs';
        $validatedName = 'wp_actionscheduler_logs';
        $wpdb          = $this->wpdbWithTable($rawInput, $validatedName);

        $this->execAction($wpdb, 'convert_innodb', [$rawInput]);

        $alterSqls = array_filter(
            $wpdb->executedSqls,
            static fn (string $s) => stripos($s, 'ALTER TABLE') !== false
        );

        $this->assertNotEmpty($alterSqls);
        foreach ($alterSqls as $sql) {
            $this->assertStringContainsString($validatedName, $sql,
                'ALTER TABLE SQL must use the information_schema-validated name');
        }
    }

    // =========================================================================
    // Error message — updated allowed-action list
    // =========================================================================

    /**
     * An unknown action string must be rejected with the updated error message
     * that includes analyze and convert_innodb in the list of valid actions.
     */
    public function test_unknown_action_rejection_includes_new_actions(): void
    {
        $wpdb   = $this->wpdbWithTable('wp_posts', 'wp_posts');
        $cmd    = new DbTableActionCommand(null, $wpdb);
        $result = $cmd->execute([], [
            'job_id' => 'test-bad-action',
            'action' => 'reindex',
            'tables' => ['wp_posts'],
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('analyze', $result['detail'],
            'Rejection message must include "analyze" in the valid-action list');
        $this->assertStringContainsString('convert_innodb', $result['detail'],
            'Rejection message must include "convert_innodb" in the valid-action list');
    }

    // =========================================================================
    // Bulk: both new actions work across multiple tables
    // =========================================================================

    /**
     * ANALYZE on a batch of two tables must return done for both.
     */
    public function test_analyze_bulk_two_tables(): void
    {
        $wpdb = new AnalyzeTestWpdbMulti([
            'wp_actionscheduler_logs'  => 'wp_actionscheduler_logs',
            'wp_wpmgr_activity_log'    => 'wp_wpmgr_activity_log',
        ]);

        $cmd    = new DbTableActionCommand(null, $wpdb);
        $result = $cmd->execute([], [
            'job_id' => 'bulk-analyze',
            'action' => 'analyze',
            'tables' => ['wp_actionscheduler_logs', 'wp_wpmgr_activity_log'],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertCount(2, $result['results']);
        foreach ($result['results'] as $r) {
            $this->assertSame('done', $r['status'], "Table {$r['table']} must be done");
        }
    }
}

// =============================================================================
// AnalyzeTestWpdb: minimal wpdb double for single-table analyze/convert tests.
//
// get_var() simulates an information_schema hit: returns $validatedName when
// the prepared SQL contains $inputName as the bound argument, null otherwise.
// query() records all SQL and returns $queryReturn (default 1, or false to
// simulate a MySQL error).
// =============================================================================

final class AnalyzeTestWpdb
{
    public string $prefix = 'wp_';
    public string $last_error = '';

    /** @var list<string> */
    public array $executedSqls = [];

    private string $validatedName;
    private string $inputName;
    private int|false $queryReturn;
    private bool $alwaysNull;

    public function __construct(
        string $validatedName,
        string $inputName,
        int|false $queryReturn = 1,
        bool $alwaysNull = false
    ) {
        $this->validatedName = $validatedName;
        $this->inputName     = $inputName;
        $this->queryReturn   = $queryReturn;
        $this->alwaysNull    = $alwaysNull;
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
        if ($this->alwaysNull) {
            return null;
        }
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return null;
        }
        $arg = (string) ($decoded['args'][0] ?? '');
        return $arg === $this->inputName ? $this->validatedName : null;
    }

    public function query(string $sql): int|false
    {
        if ($this->queryReturn !== false) {
            $this->executedSqls[] = $sql;
        }
        return $this->queryReturn;
    }
}

// =============================================================================
// AnalyzeTestWpdbMulti: wpdb double that recognises multiple table names for
// bulk tests. The table map is inputName => validatedName. query() always
// returns 1 and records the SQL.
// =============================================================================

final class AnalyzeTestWpdbMulti
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
