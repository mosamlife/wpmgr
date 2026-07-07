<?php
/**
 * P0 outcome test (GH #170 Wave 2): `DbRestorer::restore()` must actually
 * REPLAY a real SQL dump into live tables — never a stub that merely reports
 * `ok=true`. Also exercises the URL-rewrite spot check
 * (`DbRestorer::rewriteAllTables()`) and the final atomic per-table
 * `swap()`, against a live scratch MySQL/MariaDB instance.
 *
 * Skipped unless `WPMGR_TEST_MYSQL_*` env vars are present — mirrors
 * `DbDumperTest::test_dump_against_live_mysql_produces_sql_gz`'s identical
 * live-MySQL smoke pattern, so CI stays green on hosts without a database
 * while giving a real, non-fakeable proof that:
 *
 *   1. the dump was actually replayed (row VALUES match, not just "some
 *      table got created"),
 *   2. the URL rewrite actually rewrote the columns it should
 *      (siteurl/home/post_content) and skipped the one column WordPress
 *      says must never be rewritten (`posts.guid`), and
 *   3. the atomic per-table swap() actually promotes the tmp tables to the
 *      target prefix.
 *
 * `DbRestorer::restore()` opens its own `\mysqli` connection directly (by
 * design — see the class docblock, it must not share $wpdb's connection for
 * a long-running replay), so there is no dependency-injection seam to fake
 * it through; a live scratch database is the only way to prove the SQL
 * actually executed rather than a stub that returns a plausible-looking
 * result.
 *
 * @group integration
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\DbRestorer;
use WPMgr\Agent\Backup\UrlRewriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\DbRestorer
 */
final class DbRestorerLiveTest extends TestCase
{
    private ?\mysqli $mysqli = null;
    private string $tmpPrefix = '';
    private string $targetPrefix = '';
    private string $dumpPath = '';

    /** @var array{host:string,user:string,password:string,name:string,prefix:string} */
    private array $db = [];

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

        $suffix             = bin2hex(random_bytes(4));
        $this->tmpPrefix    = 'tmp' . $suffix . '_';
        $this->targetPrefix = 'wptest' . $suffix . '_';

        $this->db = [
            'host'     => (string) $host,
            'user'     => (string) $user,
            'password' => $pass === false ? '' : (string) $pass,
            'name'     => (string) $name,
            'prefix'   => $this->targetPrefix,
        ];

        [$connHost, $connPort] = $this->splitHostPort((string) $host);
        $this->mysqli = @new \mysqli($connHost, (string) $user, $pass === false ? '' : (string) $pass, (string) $name, $connPort ?? 3306); // phpcs:ignore WordPress.DB.RestrictedClasses.mysql__mysqli -- test-side verification connection, mirrors DbDumperTest's live-MySQL smoke setup
        if ($this->mysqli->connect_errno) {
            $this->markTestSkipped('Cannot connect to scratch MySQL: ' . $this->mysqli->connect_error);
        }

        $this->dumpPath = sys_get_temp_dir() . '/wpmgr-dbrestorer-test-' . $suffix . '.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode($this->buildFixtureDump()));
    }

    protected function tear_down(): void
    {
        if ($this->mysqli instanceof \mysqli) {
            @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->tmpPrefix . 'options`');
            @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->tmpPrefix . 'posts`');
            @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->targetPrefix . 'options`');
            @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->targetPrefix . 'posts`');
            @$this->mysqli->close();
        }
        if ($this->dumpPath !== '' && is_file($this->dumpPath)) {
            @unlink($this->dumpPath);
        }
        parent::tear_down();
    }

    /**
     * End-to-end: replay -> URL-rewrite spot check -> atomic swap, all
     * against the real scratch database, asserting on rows read back via a
     * SEPARATE verification connection (never trusting DbRestorer's own
     * return value alone).
     */
    public function test_restore_replays_dump_rewrites_urls_and_swaps_to_target_prefix(): void
    {
        $restorer = new DbRestorer($this->db);
        $progress = [];

        $tmpTables = $restorer->restore(
            $this->dumpPath,
            $this->tmpPrefix,
            'wp_',
            function (string $phase, array $detail) use (&$progress): void {
                $progress[] = [$phase, $detail];
            }
        );

        $this->assertNotEmpty($progress, 'progress hook should have fired');
        sort($tmpTables);
        $this->assertSame(
            [$this->tmpPrefix . 'options', $this->tmpPrefix . 'posts'],
            $tmpTables,
            'restore() must report the tmp tables it actually populated'
        );

        // --- The dump was ACTUALLY replayed: assert real row values via a
        //     separate verification connection, not DbRestorer's own say-so. ---
        $this->assertSame(
            'https://old-site.example',
            $this->readOptionValue($this->tmpPrefix . 'options', 'siteurl'),
            'siteurl must have been replayed from the dump into the tmp table'
        );
        $this->assertSame(
            'WPMgr Fixture Site',
            $this->readOptionValue($this->tmpPrefix . 'options', 'blogname')
        );
        $postRow = $this->readPostRow($this->tmpPrefix . 'posts', 1);
        $this->assertSame('Hello World', $postRow['post_title']);
        $this->assertStringContainsString('https://old-site.example/', $postRow['post_content']);
        $this->assertSame(
            'https://old-site.example/?p=1',
            $postRow['guid'],
            'guid must still hold the OLD URL prior to the rewrite pass below'
        );

        // --- URL-rewrite spot check: rewrite old-site.example -> new-site.example
        //     across the tmp tables, then assert the rewrite actually ran. ---
        $replacements = UrlRewriter::build_replacements(
            'https://old-site.example',
            'https://new-site.example',
            'https://old-site.example',
            'https://new-site.example',
            '',
            '',
            '',
            ''
        );
        $rewriteResult = $restorer->rewriteAllTables(
            $this->tmpPrefix,
            'wp_',
            $replacements,
            [],
            static function (array $newSubState): void {
            },
            static function (string $phase, array $detail): void {
            }
        );
        $this->assertTrue($rewriteResult['finished'] ?? false);
        $this->assertGreaterThan(0, $rewriteResult['total_updates'] ?? 0, 'the URL rewrite must have actually updated rows');

        $this->assertSame(
            'https://new-site.example',
            $this->readOptionValue($this->tmpPrefix . 'options', 'siteurl'),
            'siteurl must have been rewritten to the new URL'
        );
        $postRowAfterRewrite = $this->readPostRow($this->tmpPrefix . 'posts', 1);
        $this->assertStringContainsString(
            'https://new-site.example/',
            $postRowAfterRewrite['post_content'],
            'post_content must have been rewritten to the new URL'
        );
        $this->assertSame(
            'https://old-site.example/?p=1',
            $postRowAfterRewrite['guid'],
            'posts.guid must NEVER be rewritten — WordPress Codex rule, enforced by UrlRewriter::rewriteTable'
        );

        // --- Atomic swap: tmp tables promote to the target prefix. -----------
        $swapProgress = [];
        $restorer->swap(
            $this->tmpPrefix,
            $this->targetPrefix,
            $tmpTables,
            function (string $phase, array $detail) use (&$swapProgress): void {
                $swapProgress[] = [$phase, $detail];
            }
        );
        $this->assertNotEmpty($swapProgress);

        $this->assertSame(
            'https://new-site.example',
            $this->readOptionValue($this->targetPrefix . 'options', 'siteurl'),
            'after swap(), the REWRITTEN value must be readable under the target prefix'
        );
        $this->assertFalse(
            $this->tableExists($this->tmpPrefix . 'options'),
            'swap() must RENAME the tmp table away, not leave a duplicate behind'
        );
    }

    /**
     * A minimal-but-real mysqldump-shaped fixture. The source prefix is
     * always literal `wp_` — a live install's actual table prefix is
     * whatever `$this->targetPrefix` is; DbRestorer's job is exactly to
     * rewrite `wp_X` identifiers in the DUMP to the tmp prefix regardless of
     * what the live install's prefix happens to be, then later swap the tmp
     * tables to that live prefix. Using a fixed 'wp_' source prefix mirrors
     * what a REAL backup snapshot looks like.
     */
    private function buildFixtureDump(): string
    {
        return <<<'SQL'
DROP TABLE IF EXISTS `wp_options`;
CREATE TABLE `wp_options` (
  `option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `option_name` varchar(191) NOT NULL DEFAULT '',
  `option_value` longtext NOT NULL,
  PRIMARY KEY (`option_id`),
  UNIQUE KEY `option_name` (`option_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO `wp_options` VALUES (1,'siteurl','https://old-site.example'),(2,'home','https://old-site.example'),(3,'blogname','WPMgr Fixture Site');
DROP TABLE IF EXISTS `wp_posts`;
CREATE TABLE `wp_posts` (
  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `post_title` text NOT NULL,
  `post_content` longtext NOT NULL,
  `guid` varchar(255) NOT NULL DEFAULT '',
  PRIMARY KEY (`ID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO `wp_posts` VALUES (1,'Hello World','Welcome to https://old-site.example/ — enjoy your stay.','https://old-site.example/?p=1');
SQL;
    }

    private function readOptionValue(string $table, string $optionName): ?string
    {
        $escaped = $this->mysqli->real_escape_string($optionName);
        $res     = $this->mysqli->query('SELECT option_value FROM `' . $table . "` WHERE option_name = '" . $escaped . "'");
        if ($res === false) {
            return null;
        }
        $row = $res->fetch_assoc();
        $res->free();
        return is_array($row) ? (string) ($row['option_value'] ?? '') : null;
    }

    /**
     * @return array{post_title:string,post_content:string,guid:string}
     */
    private function readPostRow(string $table, int $id): array
    {
        $res = $this->mysqli->query('SELECT post_title, post_content, guid FROM `' . $table . '` WHERE ID = ' . $id);
        $this->assertNotFalse($res, 'SELECT against ' . $table . ' failed: ' . $this->mysqli->error);
        $row = $res->fetch_assoc();
        $res->free();
        $this->assertIsArray($row, 'expected a row in ' . $table);
        return [
            'post_title'   => (string) $row['post_title'],
            'post_content' => (string) $row['post_content'],
            'guid'         => (string) $row['guid'],
        ];
    }

    private function tableExists(string $table): bool
    {
        $escaped = $this->mysqli->real_escape_string($table);
        $res     = $this->mysqli->query("SHOW TABLES LIKE '" . $escaped . "'");
        if ($res === false) {
            return false;
        }
        $found = $res->fetch_row() !== null;
        $res->free();
        return $found;
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
}
