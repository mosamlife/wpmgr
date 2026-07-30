<?php
/**
 * SHARED GOLDEN VECTOR for the agent self-update beat 1 (ARM) wire contract.
 *
 * This is the PHP half. The Go half lives in apps/api/internal/agentcmd and
 * pins the SAME literals. Both halves are deliberately redundant: the defects
 * this file was written for were three status/cron_mode strings that one side
 * believed in and the other never emitted, and they were invisible to both
 * suites because each suite only ever tested its own half's fiction. A golden
 * vector that each side pins independently is the only thing that turns a
 * one-sided edit into a failing suite on the side that made it.
 *
 * THE CONTRACT, authoritative.
 *
 * status, exactly five strings:
 *   "scheduled"          verified and staged, cron event spawned. Carries
 *                        to_version and expires_at.
 *   "up_to_date"         the verified manifest offers nothing newer than what
 *                        is installed.
 *   "not_eligible"       this build cannot self-update (wp.org build) or the
 *                        site is not enrolled.
 *   "already_scheduled"  a staged record from a previous arm is still live.
 *                        Carries to_version and expires_at. NOT a failure.
 *   "error"              the arm failed. Carries a human-readable detail.
 *
 * cron_mode, exactly two strings:
 *   "loopback"   WordPress loopback cron is available.
 *   "external"   loopback cron is unavailable (DISABLE_WP_CRON or equivalent);
 *                the site relies on system cron.
 *
 * "failed", "disabled" and "alternate" are NOT part of this contract. The agent
 * has never emitted any of them from beat 1. ("failed" remains a legitimate
 * value of the separate BEAT 3 apply-result record stored in
 * AgentSelfUpdateCommand::OPTION_RESULT, which is a different field on a
 * different wire; the assertions below scope themselves to beat 1.)
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Support\UpdateChecker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AgentSelfUpdateCommand
 * @covers \WPMgr\Agent\Support\UpdateChecker
 */
final class AgentSelfUpdateWireContractTest extends TestCase
{
    /**
     * THE golden status vector. Sorted, so set comparisons below read as
     * equality rather than as a subset test.
     *
     * @var list<string>
     */
    private const GOLDEN_STATUSES = [
        'already_scheduled',
        'error',
        'not_eligible',
        'scheduled',
        'up_to_date',
    ];

    /**
     * THE golden cron_mode vector.
     *
     * @var list<string>
     */
    private const GOLDEN_CRON_MODES = [
        'external',
        'loopback',
    ];

    /**
     * Strings a previous version of the control-plane contract believed in and
     * the agent never emitted. Pinned here so re-introducing one on this side
     * fails loudly instead of quietly resurrecting the mismatch.
     *
     * @var list<string>
     */
    private const NEVER_EMITTED = ['failed', 'disabled', 'alternate'];

    /**
     * The exact response shape. Beat 1 answers exactly these keys, always,
     * whichever status it carries.
     *
     * @var list<string>
     */
    private const RESPONSE_KEYS = [
        'status',
        'ok',
        'from_version',
        'to_version',
        'detail',
        'cron_mode',
        'expires_at',
    ];

    // -------------------------------------------------------------------------
    // The vector itself
    // -------------------------------------------------------------------------

    /**
     * Pins the literals. If a future edit widens or narrows the contract, this
     * is the assertion that has to be changed deliberately, in the same commit
     * as its Go counterpart.
     */
    public function test_golden_vector_is_exactly_five_statuses_and_two_cron_modes(): void
    {
        $this->assertCount(5, self::GOLDEN_STATUSES);
        $this->assertSame(
            ['already_scheduled', 'error', 'not_eligible', 'scheduled', 'up_to_date'],
            self::GOLDEN_STATUSES
        );

        $this->assertCount(2, self::GOLDEN_CRON_MODES);
        $this->assertSame(['external', 'loopback'], self::GOLDEN_CRON_MODES);

        foreach (self::NEVER_EMITTED as $ghost) {
            $this->assertNotContains(
                $ghost,
                self::GOLDEN_STATUSES,
                "'{$ghost}' is not a beat 1 status; it was removed from the contract because the agent never emitted it"
            );
            $this->assertNotContains($ghost, self::GOLDEN_CRON_MODES);
        }
    }

    // -------------------------------------------------------------------------
    // What the sources actually emit
    // -------------------------------------------------------------------------

    /**
     * Every status literal the two beat 1 answer builders can produce, taken
     * from the source rather than from a doc block, must be exactly the golden
     * set. Not a subset: adding a sixth status fails here, and so does dropping
     * one, which is what keeps the two halves honest.
     */
    public function test_the_arm_sources_emit_exactly_the_golden_statuses(): void
    {
        $emitted = array_merge(
            self::firstStringArguments(self::checkerSource(), ['stageAnswer']),
            self::firstStringArguments(self::commandSource(), ['answer'])
        );

        $emitted = array_values(array_unique($emitted));
        sort($emitted);

        $this->assertSame(
            self::GOLDEN_STATUSES,
            $emitted,
            'the beat 1 status set drifted from the wire contract. Statuses found in the agent source: '
            . implode(', ', $emitted)
        );
    }

    /**
     * The same for cron_mode, read out of the `'cron_mode' =>` expression in
     * each answer builder. Both builders must offer the same two values, so a
     * site never reports one vocabulary from the command shell and another from
     * the self-updater.
     */
    public function test_both_answer_builders_emit_exactly_the_golden_cron_modes(): void
    {
        foreach (['class-update-checker.php' => self::checkerSource(), 'class-agent-self-update-command.php' => self::commandSource()] as $label => $source) {
            $modes = self::cronModeLiterals($source);

            $this->assertSame(
                self::GOLDEN_CRON_MODES,
                $modes,
                "{$label} emits a cron_mode set that is not the wire contract's two values. Found: "
                . (implode(', ', $modes) ?: '(none, so the expression was refactored and this vector needs re-pinning)')
            );
        }
    }

    // -------------------------------------------------------------------------
    // Timing, the other half of the contract
    // -------------------------------------------------------------------------

    /**
     * The staged record MUST outlive the control plane's patience.
     *
     * The CP waits for beat 3 for a window chosen by the cron_mode this agent
     * reported in beat 1: 20 minutes for "loopback", 90 minutes for "external"
     * (agentConfirmDeadline and agentConfirmDeadlineExternalCron in
     * apps/api/internal/update/agent_worker.go). A staged TTL shorter than the
     * longer of the two is a defect, not a tuning choice: a site whose system
     * cron runs hourly expires its own staged record before the tick that
     * would have applied it, so the canary false-fails and the rollout halts
     * with nothing wrong with the build.
     */
    public function test_staged_ttl_outlives_the_control_planes_longest_confirm_deadline(): void
    {
        $cpLoopbackDeadline = 20 * 60;
        $cpExternalDeadline = 90 * 60;

        // The control plane declares this floor itself, as
        // SelfUpdateStagedTTLFloor in
        // apps/api/internal/agentcmd/agent_self_update_contract.go. Assert the
        // SAME number rather than a locally derived "deadline plus headroom":
        // a weaker local sum let this constant be lowered to a value that kept
        // both suites green while violating the floor the other half publishes,
        // which is the exact class of two-halves-disagree defect this whole
        // contract test exists to catch.
        $cpDeclaredFloor = 2 * 60 * 60;

        $this->assertGreaterThan(
            $cpLoopbackDeadline,
            UpdateChecker::STAGED_TTL_SECONDS,
            'a stage that expires before the loopback confirm window cannot be applied in time'
        );

        $this->assertGreaterThanOrEqual(
            $cpDeclaredFloor,
            UpdateChecker::STAGED_TTL_SECONDS,
            'STAGED_TTL_SECONDS must be at least the control plane\'s declared floor ('
            . $cpDeclaredFloor . 's, SelfUpdateStagedTTLFloor), which already carries headroom over its longest'
            . ' confirm deadline (' . $cpExternalDeadline . 's, external cron). Raise the TTL first, then the CP window.'
        );
    }

    // -------------------------------------------------------------------------
    // Runtime: the shape and the locally reachable statuses
    // -------------------------------------------------------------------------

    /**
     * The command shell's locally generated answer, exercised for real. This is
     * the path that does not need the self-updater, so it runs without a
     * WordPress fixture: a null self-updater is "not_eligible".
     *
     * The body is irrelevant to that answer. This command takes no parameters,
     * so an unexpected one is IGNORED rather than turned into an "error", and
     * both bodies must produce the identical golden envelope. Rejecting a body
     * once halted a production rollout wave over a difference the command had
     * no reason to care about.
     */
    public function test_command_local_paths_answer_only_golden_values(): void
    {
        $command = new AgentSelfUpdateCommand(null);

        $withBody = $command->execute([], ['unexpected' => 1]);
        $this->assertSame('not_eligible', $withBody['status'], 'an unexpected body is ignored, never an error');

        $ineligible = $command->execute([], []);
        $this->assertSame('not_eligible', $ineligible['status']);
        $this->assertTrue($ineligible['ok'], '"not_eligible" is informational, never a failure');
        $this->assertSame($ineligible, $withBody, 'the body must not change the answer');

        foreach ([$withBody, $ineligible] as $answer) {
            $this->assertSame(self::RESPONSE_KEYS, array_keys($answer));
            $this->assertContains($answer['status'], self::GOLDEN_STATUSES);
            $this->assertContains($answer['cron_mode'], self::GOLDEN_CRON_MODES);
            $this->assertIsString($answer['from_version']);
            $this->assertSame('', $answer['to_version'], 'only scheduled/already_scheduled carry a target version');
            $this->assertIsString($answer['detail']);
            $this->assertSame(0, $answer['expires_at'], 'expires_at is 0 unless a record was staged');
        }
    }

    // -------------------------------------------------------------------------
    // Source helpers
    // -------------------------------------------------------------------------

    /** @return string Source of the self-updater. */
    private static function checkerSource(): string
    {
        return (string) file_get_contents(dirname(__DIR__) . '/includes/support/class-update-checker.php');
    }

    /** @return string Source of the command shell. */
    private static function commandSource(): string
    {
        return (string) file_get_contents(dirname(__DIR__) . '/includes/commands/class-agent-self-update-command.php');
    }

    /**
     * Collect the first argument of every call to one of $methods, when that
     * argument is a plain quoted string. Method DECLARATIONS are skipped for
     * free: their first token after `(` is a type, not a string literal.
     *
     * @param string       $source  PHP source.
     * @param list<string> $methods Method names to collect from.
     * @return list<string> Unquoted literals, in source order.
     */
    private static function firstStringArguments(string $source, array $methods): array
    {
        $tokens = token_get_all($source);
        $count  = count($tokens);
        $found  = [];

        for ($i = 0; $i < $count; $i++) {
            $token = $tokens[$i];
            if (!is_array($token) || $token[0] !== T_STRING || !in_array($token[1], $methods, true)) {
                continue;
            }

            $open = self::nextSignificant($tokens, $i);
            if ($open === -1 || $tokens[$open] !== '(') {
                continue;
            }

            $arg = self::nextSignificant($tokens, $open);
            if ($arg === -1 || !is_array($tokens[$arg]) || $tokens[$arg][0] !== T_CONSTANT_ENCAPSED_STRING) {
                continue;
            }

            $found[] = self::unquote($tokens[$arg][1]);
        }

        return $found;
    }

    /**
     * Collect the string literals of the `'cron_mode' => ...` array entry, at
     * the entry's own nesting level only. Literals nested inside a call (the
     * DISABLE_WP_CRON constant name) are not part of the emitted vocabulary and
     * are skipped.
     *
     * @param string $source PHP source.
     * @return list<string> Sorted, de-duplicated literals.
     */
    private static function cronModeLiterals(string $source): array
    {
        $tokens = token_get_all($source);
        $count  = count($tokens);
        $found  = [];

        for ($i = 0; $i < $count; $i++) {
            $token = $tokens[$i];
            if (!is_array($token) || $token[0] !== T_CONSTANT_ENCAPSED_STRING) {
                continue;
            }
            if (self::unquote($token[1]) !== 'cron_mode') {
                continue;
            }

            $arrow = self::nextSignificant($tokens, $i);
            if ($arrow === -1 || !is_array($tokens[$arrow]) || $tokens[$arrow][0] !== T_DOUBLE_ARROW) {
                continue;
            }

            $depth = 0;
            for ($j = $arrow + 1; $j < $count; $j++) {
                $inner = $tokens[$j];
                if ($inner === '(' || $inner === '[') {
                    $depth++;
                    continue;
                }
                if ($inner === ')' || $inner === ']') {
                    if ($depth === 0) {
                        break;
                    }
                    $depth--;
                    continue;
                }
                if ($inner === ',' && $depth === 0) {
                    break;
                }
                if ($depth === 0 && is_array($inner) && $inner[0] === T_CONSTANT_ENCAPSED_STRING) {
                    $found[] = self::unquote($inner[1]);
                }
            }
        }

        $found = array_values(array_unique($found));
        sort($found);

        return $found;
    }

    /**
     * @param string $literal Quoted PHP string literal.
     * @return string Its value, for the simple single-quoted case this file needs.
     */
    private static function unquote(string $literal): string
    {
        return trim($literal, "'\"");
    }

    /**
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $from   Index to search after.
     * @return int Index of the next significant token, or -1.
     */
    private static function nextSignificant(array $tokens, int $from): int
    {
        $count = count($tokens);
        for ($i = $from + 1; $i < $count; $i++) {
            $token = $tokens[$i];
            if (is_array($token) && in_array($token[0], [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true)) {
                continue;
            }

            return $i;
        }

        return -1;
    }
}
