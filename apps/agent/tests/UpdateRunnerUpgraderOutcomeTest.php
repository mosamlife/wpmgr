<?php
/**
 * UpdateRunnerUpgraderOutcomeTest — agent-only visibility fix (0.61.21,
 * GitHub issue #131 second follow-up, Part 1).
 *
 * Before this fix, UpdateRunner::upgraderOutcome() reduced any false/null
 * upgrade() return to the bare literal "Update failed." — accurate but
 * useless for diagnosing WHY (a download failure, a corrupt archive, the
 * temp-dir collision this same release fixes, ...). This is exactly the
 * symptom the real production report showed: "Update failed." with no
 * further detail, no downstream isComplete() "Reason:" suffix, meaning the
 * WP upgrader itself failed before any file copy ever started.
 *
 * upgraderOutcome() now also captures the WP_Upgrader's own skin's collected
 * progress/error messages (Automatic_Upgrader_Skin::get_upgrade_messages(),
 * inherited by the WP_Ajax_Upgrader_Skin used throughout this class) — the
 * exact strings WordPress's own admin UI would have shown — and folds them
 * into the CP-visible log for BOTH a bare false/null result and a WP_Error
 * result, so the next occurrence of this class of failure is diagnosable
 * from the control plane's "View logs" without WPMGR_DEBUG.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 */
final class UpdateRunnerUpgraderOutcomeTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Invoke the private upgraderOutcome() via reflection. */
    private function outcome(UpdateRunner $runner, mixed $result, ?object $upgrader = null): array
    {
        $method = new \ReflectionMethod(UpdateRunner::class, 'upgraderOutcome');

        /** @var array{ok:bool,log:string} $out */
        $out = $method->invoke($runner, $result, $upgrader);

        return $out;
    }

    /** Build a fake WP_Upgrader-shaped object carrying a fake skin. */
    private function fakeUpgrader(array $messages): object
    {
        $skin = new class ($messages) {
            /** @var array<int,string> */
            private array $messages;

            /**
             * @param array<int,string> $messages Collected upgrade messages.
             */
            public function __construct(array $messages)
            {
                $this->messages = $messages;
            }

            /** @return array<int,string> */
            public function get_upgrade_messages(): array
            {
                return $this->messages;
            }
        };

        $upgrader = new \stdClass();
        $upgrader->skin = $skin; // phpcs:ignore WordPress.NamingConventions.ValidVariableName.UsedPropertyNotSnakeCase -- mirrors WP_Upgrader's own public $skin property name exactly

        return $upgrader;
    }

    /**
     * A WP_Error result surfaces its code + message, unchanged in spirit from
     * before this fix (now also code-prefixed).
     */
    public function test_upgrader_failure_captures_wp_error_message(): void
    {
        $runner = new UpdateRunner();
        $result = new \WP_Error('destination_already_exists', 'The plugin already exists.');

        $out = $this->outcome($runner, $result);

        $this->assertFalse($out['ok']);
        $this->assertStringContainsString('destination_already_exists', $out['log']);
        $this->assertStringContainsString('The plugin already exists.', $out['log']);
    }

    /**
     * THE FIX — a bare false/null result (the exact shape the real
     * production report showed: "Update failed." with nothing else) must now
     * include the WP upgrader skin's own collected messages rather than stay
     * a dead-end literal. Proves the fix: run this against the pre-fix code
     * (upgraderOutcome($result) with no $upgrader parameter at all) and the
     * log is just "Update failed." — no skin detail could ever reach it.
     */
    public function test_bare_false_result_includes_skin_messages(): void
    {
        $runner   = new UpdateRunner();
        $upgrader = $this->fakeUpgrader([
            'Downloading update from https://downloads.wordpress.org/plugin/woocommerce.9.4.0.zip…',
            'Unpacking the update…',
            'The package could not be installed. No valid plugins were found.',
        ]);

        $out = $this->outcome($runner, false, $upgrader);

        $this->assertFalse($out['ok']);
        $this->assertStringContainsString(
            'Update failed:',
            $out['log'],
            'REGRESSION: a bare false result with a real skin attached must never collapse back to the bare '
            . '"Update failed." literal — that is exactly the unhelpful message the real production report showed'
        );
        $this->assertStringContainsString('The package could not be installed. No valid plugins were found.', $out['log']);
        $this->assertStringContainsString('Unpacking the update', $out['log']);
    }

    /** A null result is treated identically to false. */
    public function test_null_result_includes_skin_messages(): void
    {
        $runner   = new UpdateRunner();
        $upgrader = $this->fakeUpgrader(['Downloading update from https://example.com/plugin.zip…']);

        $out = $this->outcome($runner, null, $upgrader);

        $this->assertFalse($out['ok']);
        $this->assertStringContainsString('Downloading update from', $out['log']);
    }

    /** No $upgrader supplied at all degrades gracefully to the bare message. */
    public function test_bare_false_result_without_upgrader_stays_bare(): void
    {
        $runner = new UpdateRunner();

        $out = $this->outcome($runner, false, null);

        $this->assertFalse($out['ok']);
        $this->assertSame('Update failed.', $out['log']);
    }

    /** HTML markup the skin allows (a/br/em/strong) is stripped for the log. */
    public function test_skin_messages_are_stripped_of_html(): void
    {
        $runner   = new UpdateRunner();
        $upgrader = $this->fakeUpgrader(['<strong>Unpacking</strong> the update&hellip;']);

        $out = $this->outcome($runner, false, $upgrader);

        $this->assertStringNotContainsString('<strong>', $out['log']);
        $this->assertStringContainsString('Unpacking the update', $out['log']);
    }

    /** A skin with no get_upgrade_messages() method degrades gracefully. */
    public function test_upgrader_without_get_upgrade_messages_method_degrades_gracefully(): void
    {
        $runner   = new UpdateRunner();
        $upgrader = new \stdClass();
        $upgrader->skin = new \stdClass(); // phpcs:ignore WordPress.NamingConventions.ValidVariableName.UsedPropertyNotSnakeCase -- mirrors WP_Upgrader's own public $skin property name exactly

        $out = $this->outcome($runner, false, $upgrader);

        $this->assertFalse($out['ok']);
        $this->assertSame('Update failed.', $out['log']);
    }

    /** A truthy, non-error, non-false result is still reported as success. */
    public function test_truthy_result_is_ok(): void
    {
        $runner = new UpdateRunner();

        $out = $this->outcome($runner, true, null);

        $this->assertTrue($out['ok']);
    }
}
