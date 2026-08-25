<?php
/**
 * DbRestorerCommentTailTest — locks the fail-closed contract of
 * `DbRestorer::looksLikeCommentOnly()`.
 *
 * At EOF `DbRestorer::restore()` holds whatever bytes were left in the read
 * buffer after the last `;` — the dump's trailing fragment. It asks
 * `looksLikeCommentOnly()` whether that fragment is nothing but whitespace and
 * SQL comments, and DISCARDS it when the answer is `true`. So a spurious
 * `true` does not merely mis-parse: it drops the dump's final SQL statement
 * and lets the restore commit and report success. That is silent data loss on
 * the recovery path.
 *
 * The method reaches its verdict through `preg_split()`. `preg_split()`
 * returns `false` when PCRE gives up — a backtrack/recursion limit exhausted
 * on a host with tight `pcre.*` ini values, for instance. The failure used to
 * be coerced to an empty list, which made the classification loop iterate zero
 * times and fall through to `return true`: PCRE failing was read as "only
 * comments here". A regex failure is not evidence that a fragment is
 * comment-only, it is evidence that we cannot tell, and "cannot tell" must
 * never resolve to "safe to discard".
 *
 * The failure is forced here by setting `pcre.backtrack_limit` to 0, which is
 * how the real defect reproduces: PCRE aborts the match and `preg_split()`
 * returns `false`. Note that the subject MUST contain a newline. PCRE's
 * start-of-match optimisation finds the required `\n` code unit with a plain
 * scan and returns NOMATCH without ever entering the match engine, so a
 * single-line subject never touches the backtrack limit and never fails. Real
 * dumps put multi-line statements in the tail routinely (extended INSERTs,
 * CREATE TABLE bodies), so the multi-line subject is the realistic case, not a
 * contrived one.
 *
 * `pcre.backtrack_limit` is process-global, so every test that lowers it
 * restores it in a `finally` — a leaked limit of 0 would break every later
 * test in the suite.
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
final class DbRestorerCommentTailTest extends TestCase
{
    /**
     * A two-line INSERT with no trailing `;` — exactly what lands in the tail
     * when a dump's final statement is unterminated.
     */
    private const REAL_SQL_TAIL = "INSERT INTO `wp_options` (option_name, option_value)\nVALUES ('siteurl', 'https://example.test')";

    /**
     * Invoke the private classifier. No setAccessible() call — since PHP 8.1
     * ReflectionMethod::invoke() reaches private methods on its own, and
     * setAccessible() is deprecated as of 8.5 (which would trip
     * phpunit.xml.dist's failOnWarning).
     */
    private function classify(string $tail): bool
    {
        $method = new \ReflectionMethod(DbRestorer::class, 'looksLikeCommentOnly');

        return (bool) $method->invoke(null, $tail);
    }

    /**
     * Run $fn with pcre.backtrack_limit forced to 0, restoring it afterwards
     * whatever happens.
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
     * The mechanism this whole test class depends on: with the backtrack limit
     * exhausted, preg_split() really does return false on a multi-line
     * subject. If this ever stops being true the tests below would pass
     * vacuously, so assert it explicitly rather than assume it.
     */
    public function test_preg_split_really_fails_when_the_backtrack_limit_is_exhausted(): void
    {
        $result = $this->withBrokenPcre(static fn () => preg_split('/\r?\n/', "first\nsecond"));

        $this->assertFalse(
            $result,
            'preg_split() must return false under an exhausted backtrack limit — '
            . 'without that, every other test here proves nothing'
        );
    }

    /**
     * THE REGRESSION LOCK. A preg_split() failure must abort, never be read as
     * "this tail is only comments". Before the fix this returned true, and the
     * caller then skipped the dump's final statement and committed the restore
     * as a success.
     */
    public function test_preg_split_failure_aborts_instead_of_discarding_the_tail(): void
    {
        $this->expectException(\RuntimeException::class);
        $this->expectExceptionMessageMatches('/cannot classify the trailing SQL fragment/');

        $this->withBrokenPcre(fn () => $this->classify(self::REAL_SQL_TAIL));
    }

    /**
     * The abort message has to tell an operator what happened and what to do,
     * not just fail. It names the restore, the reason, and the ini knobs.
     */
    public function test_abort_message_is_actionable(): void
    {
        try {
            $this->withBrokenPcre(fn () => $this->classify(self::REAL_SQL_TAIL));
            $this->fail('expected a RuntimeException');
        } catch (\RuntimeException $e) {
            $message = $e->getMessage();
            $this->assertStringContainsString('DbRestorer', $message);
            $this->assertStringContainsString('preg_split failed', $message);
            $this->assertStringContainsString('Aborting the restore', $message);
            $this->assertStringContainsString('pcre.backtrack_limit', $message);
        }
    }

    /**
     * OVER-FIRE GUARD. A genuinely comment-only tail must still be recognised
     * as comment-only when PCRE is healthy. A fix that aborts real restores is
     * worse than the bug it fixes.
     *
     * @dataProvider provide_comment_only_tails
     */
    public function test_genuine_comment_only_tails_are_still_discarded(string $tail, string $why): void
    {
        $this->assertTrue($this->classify($tail), $why);
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_comment_only_tails(): array
    {
        return [
            'empty string'          => ['', 'an empty tail is nothing to flush'],
            'whitespace only'       => ["  \n\t\n  ", 'whitespace is not a statement'],
            'double-dash comment'   => ['-- End of dump', 'the canonical dump trailer'],
            'hash comment'          => ['# MySQL-style line comment', 'MySQL accepts # line comments'],
            'multiple line comments' => ["-- one\n-- two\n\n", 'several comment lines plus blanks'],
            'block comment'         => ["/* multi\n   line\n   block */", 'block comments are stripped first'],
            'block then line'       => ["/* banner */\n-- trailer", 'mixed comment styles'],
        ];
    }

    /**
     * OVER-FIRE GUARD, the other direction. Real SQL in the tail must be
     * reported as NOT comment-only so the caller flushes it, and that must
     * happen without throwing when PCRE is healthy.
     *
     * @dataProvider provide_real_sql_tails
     */
    public function test_real_sql_tails_are_never_treated_as_comments(string $tail, string $why): void
    {
        $this->assertFalse($this->classify($tail), $why);
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_real_sql_tails(): array
    {
        return [
            'single-line insert' => ["INSERT INTO `wp_options` VALUES (1, 'a')", 'a bare unterminated INSERT'],
            'multi-line insert'  => [self::REAL_SQL_TAIL, 'the realistic extended-INSERT tail'],
            'sql after comment'  => ["-- trailing note\nINSERT INTO `wp_posts` VALUES (1)", 'a comment must not shadow the SQL under it'],
            'sql after block'    => ["/* banner */\nUPDATE `wp_options` SET option_value = '1'", 'a block comment must not shadow the SQL under it'],
        ];
    }
}
