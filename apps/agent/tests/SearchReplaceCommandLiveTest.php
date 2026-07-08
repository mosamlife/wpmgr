<?php
/**
 * P1 outcome test (GH #170 Wave 5): `SearchReplaceCommand::execute()` must
 * actually WALK the real database tables and issue real UPDATE statements —
 * never a stub that merely reports a plausible-looking summary.
 *
 * `SearchReplaceCommand::runReplace()` opens its own dedicated `\mysqli`
 * connection directly from the WordPress `DB_HOST`/`DB_USER`/`DB_PASSWORD`/
 * `DB_NAME` constants (by design — see the class docblock: it must not share
 * $wpdb's connection, which buffers the full result set and risks OOM on a
 * large table walk). There is no dependency-injection seam to fake it
 * through, so — mirroring `DbRestorerLiveTest`'s identical constraint and
 * its `WPMGR_TEST_MYSQL_*`-gated live-MySQL pattern — a real scratch
 * database is the only way to prove:
 *
 *   1. the walk actually UPDATEs exactly the rows that contain the search
 *      term, with the correctly rewritten value (never "all rows" and never
 *      "no rows"),
 *   2. `UrlRewriter::DENYLIST_TABLES` is honoured — a denylisted table (e.g.
 *      `wfHits`) is never scanned or written to even though it contains a
 *      matching value,
 *   3. `posts.guid` is never rewritten (WordPress Codex rule) even when a
 *      matching row's `post_content` IS rewritten in the same pass,
 *   4. the caller-supplied `tables` allowlist actually restricts the walk —
 *      a table left off the allowlist is never touched even though it
 *      contains a match,
 *   5. `rows_changed`/`tables_scanned` reflect the REAL count of rows
 *      actually written, not a hard-coded or matched-row count.
 *
 * Skipped unless `WPMGR_TEST_MYSQL_*` env vars are present — keeps CI green
 * on hosts without a database (identical gating to DbDumperTest /
 * DbRestorerLiveTest).
 *
 * @group integration
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Commands\SearchReplaceCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\SearchReplaceCommand
 */
final class SearchReplaceCommandLiveTest extends TestCase
{
    private ?\mysqli $mysqli = null;
    private string $prefix = '';

    /** @var list<string> Tables created by this test, dropped in tear_down(). */
    private array $createdTables = [];

    protected function set_up(): void
    {
        parent::set_up();

        $host = getenv('WPMGR_TEST_MYSQL_HOST');
        $user = getenv('WPMGR_TEST_MYSQL_USER');
        $pass = getenv('WPMGR_TEST_MYSQL_PASSWORD');
        $name = getenv('WPMGR_TEST_MYSQL_DATABASE');

        if ($host === false || $user === false || $name === false) {
            $this->markTestSkipped('WPMGR_TEST_MYSQL_* env vars not set.');
        }

        // SearchReplaceCommand::openMysqli() reads the DB credentials from the
        // global WordPress DB_* constants (not env vars, not injectable) — this
        // is the ONLY place in the whole test suite that defines them, so a
        // fresh `define()` here cannot collide with any other test file.
        [$connHost, $connPort] = $this->splitHostPort((string) $host);
        if (!defined('DB_HOST')) {
            define('DB_HOST', $connPort !== null ? $connHost . ':' . $connPort : $connHost);
        }
        if (!defined('DB_USER')) {
            define('DB_USER', (string) $user);
        }
        if (!defined('DB_PASSWORD')) {
            define('DB_PASSWORD', $pass === false ? '' : (string) $pass);
        }
        if (!defined('DB_NAME')) {
            define('DB_NAME', (string) $name);
        }

        $this->mysqli = @new \mysqli($connHost, (string) $user, $pass === false ? '' : (string) $pass, (string) $name, $connPort ?? 3306); // phpcs:ignore WordPress.DB.RestrictedClasses.mysql__mysqli -- test-side verification connection, mirrors DbRestorerLiveTest's live-MySQL smoke setup
        if ($this->mysqli->connect_errno) {
            $this->markTestSkipped('Cannot connect to scratch MySQL: ' . $this->mysqli->connect_error);
        }

        $suffix       = bin2hex(random_bytes(4));
        $this->prefix = 'srtest' . $suffix . '_';

        $this->createFixtureTables();
    }

    protected function tear_down(): void
    {
        if ($this->mysqli instanceof \mysqli) {
            foreach ($this->createdTables as $table) {
                @$this->mysqli->query('DROP TABLE IF EXISTS `' . $table . '`');
            }
            @$this->mysqli->close();
        }
        unset($GLOBALS['wpdb']);
        parent::tear_down();
    }

    /**
     * @return array{0:string,1:int|null}
     */
    private function splitHostPort(string $host): array
    {
        if (strpos($host, ':') !== false) {
            [$h, $p] = explode(':', $host, 2);
            return [$h, ctype_digit($p) ? (int) $p : null];
        }
        return [$host, null];
    }

    /**
     * Build three tables under the test-unique prefix:
     *   - {prefix}posts:   1 row whose post_content contains the search term
     *                      AND whose guid ALSO contains it (guid must survive
     *                      untouched — WP Codex rule).
     *   - {prefix}options: 1 matching row + 1 non-matching row.
     *   - {prefix}wfHits:  a UrlRewriter::DENYLIST_TABLES entry — contains a
     *                      matching value that must NEVER be touched.
     */
    private function createFixtureTables(): void
    {
        $postsTable   = $this->prefix . 'posts';
        $optionsTable = $this->prefix . 'options';
        $denyTable    = $this->prefix . 'wfHits';
        $this->createdTables = [$postsTable, $optionsTable, $denyTable];

        $this->mustQuery('DROP TABLE IF EXISTS `' . $postsTable . '`');
        $this->mustQuery(
            'CREATE TABLE `' . $postsTable . '` (' .
            '`ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,' .
            '`post_title` text NOT NULL,' .
            '`post_content` longtext NOT NULL,' .
            '`guid` varchar(255) NOT NULL DEFAULT \'\',' .
            'PRIMARY KEY (`ID`)' .
            ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4'
        );
        $this->mustQuery(
            'INSERT INTO `' . $postsTable . '` (post_title, post_content, guid) VALUES ' .
            '(\'Hello\', \'Welcome to https://staging.example.com/shop\', \'https://staging.example.com/?p=1\')'
        );

        $this->mustQuery('DROP TABLE IF EXISTS `' . $optionsTable . '`');
        $this->mustQuery(
            'CREATE TABLE `' . $optionsTable . '` (' .
            '`option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,' .
            '`option_name` varchar(191) NOT NULL DEFAULT \'\',' .
            '`option_value` longtext NOT NULL,' .
            'PRIMARY KEY (`option_id`),' .
            'UNIQUE KEY `option_name` (`option_name`)' .
            ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4'
        );
        $this->mustQuery(
            'INSERT INTO `' . $optionsTable . '` (option_name, option_value) VALUES ' .
            '(\'siteurl\', \'https://staging.example.com\'), ' .
            '(\'blogname\', \'A site with no URL in it\')'
        );

        $this->mustQuery('DROP TABLE IF EXISTS `' . $denyTable . '`');
        $this->mustQuery(
            'CREATE TABLE `' . $denyTable . '` (' .
            '`id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,' .
            '`ip` varchar(64) NOT NULL DEFAULT \'\',' .
            '`note` text NOT NULL,' .
            'PRIMARY KEY (`id`)' .
            ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4'
        );
        $this->mustQuery(
            'INSERT INTO `' . $denyTable . '` (ip, note) VALUES ' .
            '(\'203.0.113.5\', \'blocked hit from https://staging.example.com/wp-login.php\')'
        );
    }

    private function mustQuery(string $sql): void
    {
        $ok = $this->mysqli->query($sql);
        $this->assertNotFalse($ok, 'fixture setup query failed: ' . $sql . ' -- ' . $this->mysqli->error);
    }

    /**
     * A minimal $wpdb double — SearchReplaceCommand::runReplace() only ever
     * checks isset()/is_object() on it and reads ->prefix; the real table
     * walk goes through its own dedicated \mysqli connection (see class
     * docblock), so a full wpdb fake is unnecessary here.
     */
    private function installFakeWpdb(): void
    {
        $GLOBALS['wpdb'] = (object) ['prefix' => $this->prefix];
    }

    private function readValue(string $table, string $pkCol, string $pkVal, string $col): ?string
    {
        $escapedPk = $this->mysqli->real_escape_string($pkVal);
        $res       = $this->mysqli->query(
            'SELECT `' . $col . '` FROM `' . $table . '` WHERE `' . $pkCol . '` = \'' . $escapedPk . '\''
        );
        $this->assertNotFalse($res, 'verification SELECT failed against ' . $table);
        $row = $res->fetch_assoc();
        $res->free();
        return is_array($row) ? (string) ($row[$col] ?? '') : null;
    }

    // -------------------------------------------------------------------------
    // 1. Full walk (no tables allowlist): exact-row UPDATE, guid preserved,
    //    denylisted table skipped, counts reflect the real writes.
    // -------------------------------------------------------------------------

    public function test_run_replace_updates_matching_rows_preserves_guid_and_skips_denylisted_table(): void
    {
        $this->installFakeWpdb();
        $cmd = new SearchReplaceCommand();

        $result = $cmd->execute([], [
            'job_id'  => 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
            'search'  => 'staging.example.com',
            'replace' => 'production.example.com',
            'dry_run' => false,
        ]);

        $this->assertTrue($result['ok'], 'execute() must succeed against a live scratch DB: ' . ($result['detail'] ?? ''));

        // tables_scanned: posts + options (2) — wfHits is denylisted and must
        // never even be counted as scanned.
        $this->assertSame(2, $result['tables_scanned'], 'wfHits (denylisted) must not be counted as scanned');

        // rows_matched: 1 posts row (post_content match) + 1 options row
        // (siteurl match) = 2. The non-matching 'blogname' row must NOT count.
        $this->assertSame(2, $result['rows_matched']);

        // rows_changed must reflect the REAL number of rows actually written —
        // a stub that reports a hard-coded or matched-row-derived count without
        // really writing would still need to pass this, but the DB-read
        // assertions below are what actually catch that class of stub.
        $this->assertSame(2, $result['rows_changed']);

        // --- Verify via the SEPARATE verification connection, never trusting
        //     the command's own return value alone. ---
        $postsTable   = $this->prefix . 'posts';
        $optionsTable = $this->prefix . 'options';
        $denyTable    = $this->prefix . 'wfHits';

        $this->assertSame(
            'Welcome to https://production.example.com/shop',
            $this->readValue($postsTable, 'ID', '1', 'post_content'),
            'post_content must have been rewritten to the new domain'
        );
        $this->assertSame(
            'https://staging.example.com/?p=1',
            $this->readValue($postsTable, 'ID', '1', 'guid'),
            'posts.guid must NEVER be rewritten even though it contains a match'
        );

        $this->assertSame(
            'https://production.example.com',
            $this->readValue($optionsTable, 'option_name', 'siteurl', 'option_value'),
            'the matching options row must have been rewritten'
        );
        $this->assertSame(
            'A site with no URL in it',
            $this->readValue($optionsTable, 'option_name', 'blogname', 'option_value'),
            'the non-matching options row must be left byte-for-byte unchanged'
        );

        $this->assertSame(
            'blocked hit from https://staging.example.com/wp-login.php',
            $this->readValue($denyTable, 'id', '1', 'note'),
            'a UrlRewriter::DENYLIST_TABLES table must never be written to, even though it contains a match'
        );
    }

    // -------------------------------------------------------------------------
    // 2. Caller-supplied `tables` allowlist actually restricts the walk.
    // -------------------------------------------------------------------------

    public function test_run_replace_honors_caller_supplied_tables_allowlist(): void
    {
        $this->installFakeWpdb();
        $cmd = new SearchReplaceCommand();

        $optionsTable = $this->prefix . 'options';
        $postsTable   = $this->prefix . 'posts';

        $result = $cmd->execute([], [
            'job_id'  => 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
            'search'  => 'staging.example.com',
            'replace' => 'production.example.com',
            'dry_run' => false,
            'tables'  => [$optionsTable],
        ]);

        $this->assertTrue($result['ok'], 'execute() must succeed: ' . ($result['detail'] ?? ''));
        $this->assertSame(1, $result['tables_scanned'], 'only the allowlisted table must be scanned');
        $this->assertSame(1, $result['rows_matched']);
        $this->assertSame(1, $result['rows_changed']);

        $this->assertSame(
            'https://production.example.com',
            $this->readValue($optionsTable, 'option_name', 'siteurl', 'option_value'),
            'the allowlisted table must have been rewritten'
        );
        $this->assertSame(
            'Welcome to https://staging.example.com/shop',
            $this->readValue($postsTable, 'ID', '1', 'post_content'),
            'a table left OFF the allowlist must be completely untouched even though it contains a match'
        );
    }

    // -------------------------------------------------------------------------
    // 3. dry_run=true counts matches but writes nothing.
    // -------------------------------------------------------------------------

    public function test_run_replace_dry_run_counts_matches_without_writing(): void
    {
        $this->installFakeWpdb();
        $cmd = new SearchReplaceCommand();

        $result = $cmd->execute([], [
            'job_id'  => 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
            'search'  => 'staging.example.com',
            'replace' => 'production.example.com',
            'dry_run' => true,
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame(2, $result['rows_matched'], 'dry_run must still count the real matches');
        $this->assertSame(0, $result['rows_changed'], 'dry_run must report zero writes');

        $optionsTable = $this->prefix . 'options';
        $this->assertSame(
            'https://staging.example.com',
            $this->readValue($optionsTable, 'option_name', 'siteurl', 'option_value'),
            'dry_run must not have written anything to the database'
        );
    }
}
