<?php
/**
 * DbRestorerPrefixRewriteFailClosedTest — locks the fail-closed contract of
 * `DbRestorer::rewritePrefix()`, in two tiers.
 *
 *  - UNIT tier, no server, ALWAYS RUNS. `rewritePrefix()` is `static` and the
 *    abort is raised inside it, so the defect and its repair are both a pure
 *    function call away. This tier is what holds the line in CI.
 *  - END-TO-END tier, through the public `DbRestorer::restore()` path against
 *    a live scratch MariaDB/MySQL. It proves the abort reaches `restore()` and
 *    that the live tables survive untouched — the part no unit test can fake,
 *    since DDL is not transactional in MySQL. Each of these calls
 *    `requireLiveMysql()` and skips individually when `WPMGR_TEST_MYSQL_*` is
 *    unset.
 *
 * The tiering is deliberate and is itself a fix. With the scratch-database
 * bootstrap in `set_up()`, the skip covered the whole class, and since
 * `ci.yml` runs `composer test` with no MySQL env, this file reported
 * `Tests: 6, Assertions: 0, Skipped: 6` and exit 0 on every CI run — the
 * regression lock for a data-loss defect announcing success over its own
 * absence, which is the same defect class the code under test commits.
 *
 * `rewritePrefix()` is the single point at which a dump statement naming
 * `wp_posts` becomes a statement naming `tmp<rand>_posts`. That rewrite IS the
 * staging discipline: it is the only reason a restore can fail without
 * damaging the live database.
 *
 * The decision was made with `if (!preg_match($p, $body, $m, ...)) continue;`.
 * `preg_match()` returns 1 on match, 0 on no match, and `false` when PCRE
 * gives up — a backtrack/recursion limit exhausted on a host with tight
 * `pcre.*` ini values. The `!` collapsed `false` into `0`, so PCRE failing was
 * read as "this statement does not name a table". Every pattern failed the
 * same way, the loop fell through, and the statement was returned VERBATIM:
 *
 *   PCRE healthy:  CREATE TABLE `tmp1234_posts` (   currentTable = 'tmp1234_posts'
 *   PCRE failing:  CREATE TABLE `wp_posts` (        currentTable = ''
 *
 * The dump then replayed straight into the LIVE tables. `$currentTable` was
 * never set, so `$touchedTbls` stayed empty, `swap()` took its
 * `$tmpTables === []` branch, emitted `'done' => true` and returned. The live
 * database was overwritten outside the staging discipline, nothing was
 * swapped, and the restore reported Completed.
 *
 * The failure is forced here with `pcre.backtrack_limit = 0`, which is how the
 * real defect reproduces. TWO traps govern that:
 *
 *  1. PCRE's start-of-match optimisation. A subject that cannot match at all
 *     returns NOMATCH from a plain first-byte scan without ever entering the
 *     match engine, so the limit is never consulted and `preg_match()` returns
 *     0, not `false`. Every subject here is therefore one the pattern can
 *     genuinely begin to match, on a realistic multi-line body (a wide CREATE
 *     TABLE, an extended INSERT) rather than a contrived one-liner.
 *  2. A test that never proves PCRE actually failed proves nothing at all, so
 *     `test_pcre_really_fails_...` asserts the mechanism directly against the
 *     production patterns.
 *
 * `pcre.backtrack_limit` is process-global, so every test that lowers it
 * restores it in a `finally` — a leaked limit of 0 would break the rest of the
 * suite.
 *
 * `@group integration` is applied PER METHOD, to the end-to-end tier only.
 * Tagging the class would put the unit tier one `--exclude-group integration`
 * away from silently vanishing again, which is the failure this file just came
 * back from.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\DbRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\DbRestorer
 */
final class DbRestorerPrefixRewriteFailClosedTest extends TestCase
{
    /**
     * The first pattern in rewritePrefix()'s list, copied verbatim. Asserting
     * against the production regex (not a stand-in) is what makes the forced
     * failure evidence about this code rather than about PCRE in general.
     */
    private const CREATE_TABLE_PATTERN = '/^(CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?)`?([A-Za-z0-9_\$]+)`?/i';

    private ?\mysqli $mysqli = null;

    /** The prefix standing in for the LIVE site's tables. */
    private string $livePrefix = '';

    /** The staging prefix the restore is supposed to write to instead. */
    private string $tmpPrefix = '';

    /** A table belonging to some OTHER plugin — must never be touched. */
    private string $foreignTable = '';

    private string $dumpPath = '';

    /** Per-run namespace for every identifier this test creates. */
    private string $suffix = '';

    /** @var array{host:string,user:string,password:string,name:string,prefix:string} */
    private array $db = [];

    /**
     * MySQL-FREE. Everything here works with no server, because most of what
     * this file locks needs none: `rewritePrefix()` is `static`, and the abort
     * is raised inside it, so the defect and its repair are both reachable as
     * a pure function call.
     *
     * The scratch-database bootstrap deliberately does NOT live here. It did
     * once, and the cost was the whole guard: `ci.yml` runs `composer test`
     * and sets no `WPMGR_TEST_MYSQL_*`, so the `markTestSkipped()` in `set_up()`
     * fired for every test in the class and the file reported
     * `Tests: 6, Assertions: 0, Skipped: 6` with exit 0 — the regression lock
     * for a proven data-loss defect announcing success over its own absence,
     * which is precisely the defect class this file exists to guard against.
     *
     * Moving the bootstrap into `requireLiveMysql()`, called by the end-to-end
     * tests only, is what lets the unit-level tests below run unconditionally.
     * Failing hard on absent env instead would have reddened every developer's
     * `composer test`, and a guard that reddens correct work gets switched off.
     */
    protected function set_up(): void
    {
        parent::set_up();

        $suffix = bin2hex(random_bytes(4));
        // Stands in for the live install's `wp_` prefix. Namespaced per run so
        // the test can never collide with a real table in a shared scratch DB,
        // while playing exactly the role `wp_` plays in production: the prefix
        // the dump names and the prefix the live site reads.
        $this->livePrefix   = 'live' . $suffix . '_';
        $this->tmpPrefix    = 'tmp' . $suffix . '_';
        $this->foreignTable = 'other' . $suffix . '_widgets';
        $this->suffix       = $suffix;
    }

    /**
     * Bring up the live scratch database, seed the pre-restore live tables and
     * write the fixture dump — or skip THIS test alone.
     *
     * Called as the first line of the end-to-end tests, never from `set_up()`.
     * The skip is still correct for those: they assert on real DDL against a
     * real server, and there is no honest way to fake that. What is no longer
     * correct is letting the skip reach the tests that need no server.
     */
    private function requireLiveMysql(): void
    {
        $host = getenv('WPMGR_TEST_MYSQL_HOST');
        $user = getenv('WPMGR_TEST_MYSQL_USER');
        $pass = getenv('WPMGR_TEST_MYSQL_PASSWORD');
        $name = getenv('WPMGR_TEST_MYSQL_DATABASE');

        if ($host === false || $user === false || $name === false) {
            $this->markTestSkipped(
                'WPMGR_TEST_MYSQL_* env vars not set — end-to-end restore case skipped. '
                . 'The unit-level fail-closed assertions in this class run regardless.'
            );
        }

        $this->db = [
            'host'     => (string) $host,
            'user'     => (string) $user,
            'password' => $pass === false ? '' : (string) $pass,
            'name'     => (string) $name,
            'prefix'   => $this->livePrefix,
        ];

        [$connHost, $connPort] = $this->splitHostPort((string) $host);
        $this->mysqli = @new \mysqli($connHost, (string) $user, $pass === false ? '' : (string) $pass, (string) $name, $connPort ?? 3306); // phpcs:ignore WordPress.DB.RestrictedClasses.mysql__mysqli -- test-side verification connection, mirrors DbRestorerLiveTest's live-MySQL setup
        if ($this->mysqli->connect_errno) {
            $this->markTestSkipped('Cannot connect to scratch MySQL: ' . $this->mysqli->connect_error);
        }

        $this->seedLiveTables();

        $this->dumpPath = sys_get_temp_dir() . '/wpmgr-dbrestorer-531-' . $this->suffix . '.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode($this->buildFixtureDump()));
    }

    protected function tear_down(): void
    {
        if ($this->mysqli instanceof \mysqli) {
            foreach (['options', 'posts'] as $t) {
                @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->livePrefix . $t . '`');
                @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->tmpPrefix . $t . '`');
            }
            @$this->mysqli->query('DROP TABLE IF EXISTS `' . $this->foreignTable . '`');
            @$this->mysqli->close();
        }
        if ($this->dumpPath !== '' && is_file($this->dumpPath)) {
            @unlink($this->dumpPath);
        }
        parent::tear_down();
    }

    // -----------------------------------------------------------------
    // The mechanism. Without this, every other test here passes vacuously.
    // -----------------------------------------------------------------

    /**
     * With the backtrack limit exhausted, `preg_match()` against
     * rewritePrefix()'s OWN first pattern really does return `false` on a
     * realistic multi-line CREATE TABLE body — and returns plain `0`, not
     * `false`, on a subject the start-of-match optimisation rejects outright.
     * Both halves matter: the first is the defect's trigger, the second is why
     * a one-line harness reports a false pass.
     */
    public function test_pcre_really_fails_when_the_backtrack_limit_is_exhausted(): void
    {
        $matchable = "CREATE TABLE `" . $this->livePrefix . "posts` (\n  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n  `post_title` text NOT NULL,\n  PRIMARY KEY (`ID`)\n) ENGINE=InnoDB";

        $result = $this->withBrokenPcre(function () use ($matchable) {
            return preg_match(self::CREATE_TABLE_PATTERN, $matchable, $m, PREG_OFFSET_CAPTURE);
        });

        $this->assertFalse(
            $result,
            'preg_match() must return false under an exhausted backtrack limit — '
            . 'without that, every other test in this class proves nothing'
        );

        // The trap, asserted rather than described: a subject the pattern
        // cannot start to match never reaches the match engine, so it yields a
        // truthful 0 even with the limit at 0.
        $unmatchable = $this->withBrokenPcre(function () {
            return preg_match(self::CREATE_TABLE_PATTERN, "SET NAMES utf8mb4", $m, PREG_OFFSET_CAPTURE);
        });
        $this->assertSame(
            0,
            $unmatchable,
            "PCRE's start-of-match optimisation should return a genuine NOMATCH here — "
            . 'this is why a single-line or non-matching subject cannot be used to force the failure'
        );
    }

    // -----------------------------------------------------------------
    // RED -> GREEN, unit level. NO SERVER REQUIRED, so this runs in CI.
    //
    // The abort is raised inside the static rewritePrefix(), which is also
    // where the defect lived, so the whole red-to-green transition is
    // observable without MySQL. The end-to-end cases below prove the abort
    // reaches restore() and spares the live tables; THIS one proves the abort
    // happens at all, and it is the assertion that carries the lock in CI.
    // -----------------------------------------------------------------

    /**
     * THE REGRESSION LOCK, unit level.
     *
     * With PCRE unable to answer, `rewritePrefix()` must throw rather than
     * return. The defect was `if (!preg_match(...)) { continue; }`: `false`
     * collapsed into `0`, every pattern "did not match", the loop fell through
     * and the statement came back VERBATIM — still naming the live table, with
     * `$currentTable` never set. Both halves of that are asserted here, so
     * restoring the old line reddens this test with no database in sight.
     */
    public function test_rewrite_prefix_aborts_instead_of_returning_a_live_prefixed_statement(): void
    {
        $stmt = 'CREATE TABLE `' . $this->livePrefix . "posts` (\n"
            . "  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n"
            . "  `post_title` text NOT NULL,\n"
            . "  PRIMARY KEY (`ID`)\n"
            . ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4';

        $currentTable = '';
        $returned     = null;
        $thrown       = null;

        try {
            $returned = $this->withBrokenPcre(function () use ($stmt, &$currentTable) {
                return DbRestorer::rewritePrefix($stmt, $this->livePrefix, $this->tmpPrefix, $currentTable);
            });
        } catch (\RuntimeException $e) {
            $thrown = $e;
        }

        $this->assertNotNull(
            $thrown,
            'rewritePrefix() must ABORT when PCRE cannot tell which table the statement targets. '
            . 'Returning at all is the defect — the returned SQL still names the live table and '
            . 'gets replayed straight into the live database.'
        );

        // The specific shape of the old failure, asserted rather than implied:
        // it did not merely fail to abort, it handed back live-prefixed SQL.
        $this->assertNull(
            $returned,
            'nothing may be returned on a PCRE failure; the verbatim live-prefixed statement is '
            . 'exactly what overwrote the live database'
        );
        $this->assertSame(
            '',
            $currentTable,
            '$currentTable must stay unset — the empty $touchedTbls it produced is what made swap() '
            . "report 'done' => true having swapped nothing"
        );
    }

    /**
     * The abort message must tell an operator what happened and what to do.
     * Asserted against `rewritePrefix()` directly — the message is built there,
     * so this needs no server, and its propagation out through `restore()` is
     * covered by the end-to-end case below.
     */
    public function test_abort_message_is_actionable(): void
    {
        $stmt = 'INSERT INTO `' . $this->livePrefix . "options` VALUES\n"
            . "(1,'siteurl','https://old-site.example'),\n"
            . "(2,'home','https://old-site.example'),\n"
            . "(3,'blogname','WPMgr Fixture Site')";

        $currentTable = '';
        $thrown       = null;

        // Captured, never asserted from inside the catch: PHPUnit's own
        // AssertionFailedError extends RuntimeException, so a `fail()` in the
        // try block lands in the catch and gets re-reported as a nonsense
        // string assertion against the failure message itself.
        try {
            $this->withBrokenPcre(function () use ($stmt, &$currentTable) {
                return DbRestorer::rewritePrefix($stmt, $this->livePrefix, $this->tmpPrefix, $currentTable);
            });
        } catch (\RuntimeException $e) {
            $thrown = $e;
        }

        $this->assertNotNull($thrown, 'rewritePrefix() must throw when preg_match() fails');

        $message = $thrown->getMessage();
        $this->assertStringContainsString('DbRestorer', $message);
        $this->assertStringContainsString('cannot determine which table a dump statement targets', $message);
        $this->assertStringContainsString('preg_match failed', $message);
        $this->assertStringContainsString('Aborting the restore', $message);
        $this->assertStringContainsString('live tables', $message);
        $this->assertStringContainsString('pcre.backtrack_limit', $message);
        $this->assertStringContainsString('pcre.recursion_limit', $message);
    }

    /**
     * OVER-FIRE GUARD, unit level, and NO SERVER REQUIRED. `0` and `false` are
     * now distinct and only `false` aborts, so a statement that legitimately
     * does not match must still be handled exactly as it was before the fix.
     *
     * This pairs with the lock above: together they are what makes the CI-side
     * assertions a real guard rather than a one-directional tripwire.
     */
    public function test_legitimately_unmatched_statements_still_pass_through_untouched(): void
    {
        // Names a table, but not one of OURS: still skipped outright, because
        // replaying it would corrupt an unrelated plugin's data.
        $current = '';
        $this->assertSame(
            '',
            DbRestorer::rewritePrefix(
                'CREATE TABLE `' . $this->foreignTable . "` (\n  `id` bigint(20) unsigned NOT NULL,\n  PRIMARY KEY (`id`)\n)",
                $this->livePrefix,
                $this->tmpPrefix,
                $current
            ),
            'a statement naming a table outside the source prefix must still be skipped entirely'
        );
        $this->assertSame('', $current, 'a foreign table must not become the current table');

        // Names no table at all (SET / START TRANSACTION / COMMIT): still
        // passes through verbatim rather than aborting.
        $current = '';
        $this->assertSame(
            'SET NAMES utf8mb4',
            DbRestorer::rewritePrefix('SET NAMES utf8mb4', $this->livePrefix, $this->tmpPrefix, $current),
            'a prefix-free statement must still pass through verbatim'
        );
        $this->assertSame('', $current, 'a prefix-free statement must not claim a current table');

        // The context-switching statements are still refused.
        $current = '';
        $this->assertSame(
            '',
            DbRestorer::rewritePrefix('USE some_other_database', $this->livePrefix, $this->tmpPrefix, $current),
            'context-switching statements must still be dropped'
        );
    }

    // -----------------------------------------------------------------
    // RED -> GREEN. The end-to-end proof through the public restore() path.
    // These need a live scratch server and skip individually without one.
    // -----------------------------------------------------------------

    /**
     * THE REGRESSION LOCK, end to end.
     *
     * Before the fix this run completed "successfully": the dump's
     * `DROP TABLE IF EXISTS` / `CREATE TABLE` / `INSERT` statements were
     * replayed unrewritten, so the LIVE tables were dropped and repopulated,
     * restore() returned an empty tmp-table list, and swap() reported done
     * over that empty list.
     *
     * After the fix the same bytes abort before a single statement reaches the
     * server, and the live data is exactly as it was. The sentinel row is the
     * proof: DDL is not transactional in MySQL, so had the unrewritten
     * `DROP TABLE IF EXISTS `<live>_options`` executed, no rollback could have
     * brought it back.
     *
     * @group integration
     */
    public function test_pcre_failure_aborts_before_writing_to_the_live_tables(): void
    {
        $this->requireLiveMysql();
        $restorer = new DbRestorer($this->db);

        $thrown = null;
        try {
            $this->withBrokenPcre(function () use ($restorer) {
                return $restorer->restore(
                    $this->dumpPath,
                    $this->tmpPrefix,
                    $this->livePrefix,
                    static function (string $phase, array $detail): void {
                    }
                );
            });
        } catch (\RuntimeException $e) {
            $thrown = $e;
        }

        $this->assertNotNull(
            $thrown,
            'restore() must ABORT when PCRE cannot tell which table a statement targets. '
            . 'Returning normally is the defect: it means the dump was replayed unrewritten.'
        );
        $this->assertMatchesRegularExpression(
            '/cannot determine which table a dump statement targets/',
            $thrown->getMessage()
        );

        // --- The live database is untouched. --------------------------------
        $this->assertTrue(
            $this->tableExists($this->livePrefix . 'options'),
            'the LIVE options table must still exist — an unrewritten DROP TABLE would have removed it'
        );
        $this->assertSame(
            'https://live-site.example',
            $this->readOptionValue($this->livePrefix . 'options', 'siteurl'),
            'the LIVE siteurl row must still hold its pre-restore value — '
            . 'reading the dump value here means the restore wrote to the live table'
        );
        $this->assertSame(
            'sentinel',
            $this->readOptionValue($this->livePrefix . 'options', 'wpmgr_sentinel'),
            'the sentinel row must survive; a DROP+CREATE replay would have erased it'
        );
        $this->assertFalse(
            $this->tableExists($this->livePrefix . 'posts'),
            'the dump\'s CREATE TABLE must not have created a live posts table'
        );
    }

    // -----------------------------------------------------------------
    // OVER-FIRE. A fix that aborts real restores is worse than the bug.
    // -----------------------------------------------------------------

    /**
     * OVER-FIRE GUARD. With PCRE healthy the identical dump restores in full:
     * every statement rewritten to the tmp prefix, every row present, the live
     * tables still untouched, and both tmp tables reported back to swap().
     *
     * @group integration
     */
    public function test_a_normal_restore_still_completes_with_every_row(): void
    {
        $this->requireLiveMysql();
        $restorer = new DbRestorer($this->db);

        $tmpTables = $restorer->restore(
            $this->dumpPath,
            $this->tmpPrefix,
            $this->livePrefix,
            static function (string $phase, array $detail): void {
            }
        );

        sort($tmpTables);
        $this->assertSame(
            [$this->tmpPrefix . 'options', $this->tmpPrefix . 'posts'],
            $tmpTables,
            'restore() must report every tmp table it populated — an empty list is what makes '
            . 'swap() report done having swapped nothing'
        );

        // Every row from the dump landed, in the STAGING tables.
        $this->assertSame(
            'https://old-site.example',
            $this->readOptionValue($this->tmpPrefix . 'options', 'siteurl')
        );
        $this->assertSame(
            'WPMgr Fixture Site',
            $this->readOptionValue($this->tmpPrefix . 'options', 'blogname')
        );
        $this->assertSame(3, $this->countRows($this->tmpPrefix . 'options'), 'all three option rows must be present');
        $this->assertSame(2, $this->countRows($this->tmpPrefix . 'posts'), 'both post rows must be present');

        // And the live tables are still the live tables.
        $this->assertSame(
            'https://live-site.example',
            $this->readOptionValue($this->livePrefix . 'options', 'siteurl'),
            'a healthy restore must stage, never write through to the live tables'
        );
        $this->assertSame('sentinel', $this->readOptionValue($this->livePrefix . 'options', 'wpmgr_sentinel'));
    }

    /**
     * OVER-FIRE GUARD, the other direction, end to end: a foreign table named
     * in the dump is skipped by a real restore rather than created. The
     * unit-level half of this assertion — that `rewritePrefix()` returns `''`
     * for it — is in
     * `test_legitimately_unmatched_statements_still_pass_through_untouched`
     * and runs without a server; this proves `restore()` acts on that `''`.
     *
     * @group integration
     */
    public function test_a_foreign_table_in_the_dump_is_never_created(): void
    {
        $this->requireLiveMysql();
        $restorer = new DbRestorer($this->db);

        $restorer->restore(
            $this->dumpPath,
            $this->tmpPrefix,
            $this->livePrefix,
            static function (string $phase, array $detail): void {
            }
        );

        // The dump contains a CREATE TABLE for a table belonging to another
        // plugin. rewritePrefix() returns '' for it (it names a table, but not
        // one of ours) and restore() skips it — replaying it would corrupt an
        // unrelated plugin's data.
        $this->assertFalse(
            $this->tableExists($this->foreignTable),
            'a statement naming a table outside the source prefix must still be skipped entirely'
        );
    }

    /**
     * OVER-FIRE GUARD, unit level: the rewrite itself is unchanged. This is
     * the exact comparison from the defect report, asserted on the healthy
     * side so a future "fix" that abandons the rewrite cannot pass.
     */
    public function test_healthy_pcre_still_rewrites_to_the_tmp_prefix(): void
    {
        $current = '';
        $out     = DbRestorer::rewritePrefix(
            'CREATE TABLE `' . $this->livePrefix . "posts` (\n  `ID` bigint(20) unsigned NOT NULL,\n  PRIMARY KEY (`ID`)\n)",
            $this->livePrefix,
            $this->tmpPrefix,
            $current
        );

        $this->assertStringStartsWith('CREATE TABLE `' . $this->tmpPrefix . 'posts`', $out);
        $this->assertSame($this->tmpPrefix . 'posts', $current);
        $this->assertStringNotContainsString(
            '`' . $this->livePrefix . 'posts`',
            $out,
            'the live identifier must not survive the rewrite'
        );
    }

    // -----------------------------------------------------------------
    // Harness.
    // -----------------------------------------------------------------

    /**
     * Run $fn with pcre.backtrack_limit forced to 0, restoring it afterwards
     * whatever happens. The limit is process-global; a leaked 0 would break
     * every later test in the suite.
     *
     * @param callable():mixed $fn
     * @return mixed
     */
    private function withBrokenPcre(callable $fn)
    {
        $previous = ini_get('pcre.backtrack_limit');
        ini_set('pcre.backtrack_limit', '0');
        try {
            return $fn();
        } finally {
            ini_set('pcre.backtrack_limit', $previous === false ? '1000000' : $previous);
        }
    }

    /**
     * The pre-restore live database: the tables a real restore is staging
     * AWAY from. The sentinel row exists so "the live table was rewritten" is
     * provable and not merely likely.
     */
    private function seedLiveTables(): void
    {
        $t = $this->livePrefix . 'options';
        $this->mustQuery('DROP TABLE IF EXISTS `' . $t . '`');
        $this->mustQuery(
            'CREATE TABLE `' . $t . '` ('
            . '`option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,'
            . '`option_name` varchar(191) NOT NULL DEFAULT \'\','
            . '`option_value` longtext NOT NULL,'
            . 'PRIMARY KEY (`option_id`), UNIQUE KEY `option_name` (`option_name`)'
            . ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4'
        );
        $this->mustQuery(
            'INSERT INTO `' . $t . '` (option_name, option_value) VALUES '
            . "('siteurl','https://live-site.example'),"
            . "('blogname','The LIVE Site'),"
            . "('wpmgr_sentinel','sentinel')"
        );
    }

    /**
     * A minimal-but-real mysqldump-shaped fixture, prefixed with the LIVE
     * prefix exactly as a real snapshot of this site would be. The bodies are
     * deliberately multi-line: PCRE's start-of-match optimisation means a
     * one-line subject would never reach the match engine and the forced
     * failure would not fire — a false pass. Real dumps are multi-line
     * anyway.
     */
    private function buildFixtureDump(): string
    {
        $live = $this->livePrefix;

        return "-- WPMgr fixture dump\n"
            . "SET NAMES utf8mb4;\n"
            . 'DROP TABLE IF EXISTS `' . $live . "options`;\n"
            . 'CREATE TABLE `' . $live . "options` (\n"
            . "  `option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n"
            . "  `option_name` varchar(191) NOT NULL DEFAULT '',\n"
            . "  `option_value` longtext NOT NULL,\n"
            . "  PRIMARY KEY (`option_id`),\n"
            . "  UNIQUE KEY `option_name` (`option_name`)\n"
            . ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
            . 'INSERT INTO `' . $live . "options` VALUES\n"
            . "(1,'siteurl','https://old-site.example'),\n"
            . "(2,'home','https://old-site.example'),\n"
            . "(3,'blogname','WPMgr Fixture Site');\n"
            . 'DROP TABLE IF EXISTS `' . $live . "posts`;\n"
            . 'CREATE TABLE `' . $live . "posts` (\n"
            . "  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n"
            . "  `post_title` text NOT NULL,\n"
            . "  `post_content` longtext NOT NULL,\n"
            . "  `guid` varchar(255) NOT NULL DEFAULT '',\n"
            . "  PRIMARY KEY (`ID`)\n"
            . ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
            . 'INSERT INTO `' . $live . "posts` VALUES\n"
            . "(1,'Hello World','Welcome to https://old-site.example/ — enjoy your stay.','https://old-site.example/?p=1'),\n"
            . "(2,'Second Post','More content here.','https://old-site.example/?p=2');\n"
            // A table belonging to some other plugin. Names a table, but not
            // one of ours — rewritePrefix() must still skip it outright.
            . 'CREATE TABLE `' . $this->foreignTable . "` (\n"
            . "  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n"
            . "  `payload` longtext NOT NULL,\n"
            . "  PRIMARY KEY (`id`)\n"
            . ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n";
    }

    private function mustQuery(string $sql): void
    {
        $ok = $this->mysqli->query($sql);
        $this->assertNotFalse($ok, 'fixture query failed: ' . $this->mysqli->error);
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

    private function countRows(string $table): int
    {
        $res = $this->mysqli->query('SELECT COUNT(*) AS c FROM `' . $table . '`');
        $this->assertNotFalse($res, 'COUNT against ' . $table . ' failed: ' . $this->mysqli->error);
        $row = $res->fetch_assoc();
        $res->free();
        return is_array($row) ? (int) $row['c'] : -1;
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
