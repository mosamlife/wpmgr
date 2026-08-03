<?php
/**
 * UpdateOutcomeClassifierTest - one case per pre-install error code, plus the
 * three rules that make the classifier safe (GitHub issue #328).
 *
 * The tables are an ALLOWLIST audited against WordPress 7.0.2, not a proof.
 * These tests exist so that editing one is a deliberate act: the per-code
 * providers are generated FROM the constants, so adding a code without
 * thinking about which side of the boundary it belongs on is impossible, and
 * the count assertions fail the moment a table changes size.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Support\UpdateOutcome;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateOutcome
 */
final class UpdateOutcomeClassifierTest extends TestCase
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

    /**
     * Build a WP_Upgrader-shaped object whose skin reports the given error
     * codes, the way WP_Ajax_Upgrader_Skin::get_errors() does. Self-contained
     * anonymous classes, because Patchwork redefinitions leak across tests.
     *
     * @param array<string,string> $codes Code => error data.
     * @return object
     */
    private function upgraderWithSkinErrors(array $codes): object
    {
        $errors = new class ($codes) {
            /** @var array<string,string> */
            private array $codes;

            /** @param array<string,string> $codes Code => data. */
            public function __construct(array $codes)
            {
                $this->codes = $codes;
            }

            /** @return array<int,string> */
            public function get_error_codes(): array
            {
                return array_keys($this->codes);
            }

            /**
             * @param string $code Error code.
             * @return string
             */
            public function get_error_data(string $code = ''): string
            {
                return $this->codes[$code] ?? '';
            }
        };

        $skin = new class ($errors) {
            private object $errors;

            /** @param object $errors WP_Error-shaped double. */
            public function __construct(object $errors)
            {
                $this->errors = $errors;
            }

            /** @return object */
            public function get_errors(): object
            {
                return $this->errors;
            }
        };

        $upgrader = new \stdClass();
        $upgrader->skin = $skin; // phpcs:ignore WordPress.NamingConventions.ValidVariableName.UsedPropertyNotSnakeCase -- mirrors WP_Upgrader's own public $skin property name exactly

        return $upgrader;
    }

    /** @return array<string,array{string}> */
    public static function untouchedCodeProvider(): array
    {
        $out = [];
        foreach (UpdateOutcome::UNTOUCHED_CODES as $code) {
            $out[$code] = [$code];
        }

        return $out;
    }

    /** @return array<string,array{string}> */
    public static function touchedCodeProvider(): array
    {
        $out = [];
        foreach (UpdateOutcome::TOUCHED_CODES as $code) {
            $out[$code] = [$code];
        }

        return $out;
    }

    /**
     * @dataProvider untouchedCodeProvider
     *
     * @param string $code Allowlisted pre-boundary code.
     */
    public function test_every_untouched_code_classifies_as_untouched(string $code): void
    {
        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([$code => '']));

        $this->assertFalse(
            $out['destination_touched'],
            $code . ' is allowlisted as returned above class-wp-upgrader.php:608 and must classify as untouched'
        );
        $this->assertSame($code, $out['code']);
        $this->assertNotSame('unknown', $out['stage'], $code . ' must carry an operator-facing stage label');
    }

    /**
     * @dataProvider touchedCodeProvider
     *
     * @param string $code Post-boundary code.
     */
    public function test_every_touched_code_classifies_as_touched(string $code): void
    {
        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([$code => '']));

        $this->assertTrue(
            $out['destination_touched'],
            $code . ' is reachable at or after class-wp-upgrader.php:608 and must classify as touched'
        );
        $this->assertSame($code, $out['code']);
    }

    /** A code in both tables would make the verdict order-dependent. */
    public function test_no_code_appears_in_both_tables(): void
    {
        $this->assertSame(
            [],
            array_values(array_intersect(UpdateOutcome::UNTOUCHED_CODES, UpdateOutcome::TOUCHED_CODES))
        );
    }

    /**
     * Editing either table without revisiting these tests must fail loudly.
     * Update the counts deliberately, after re-reading install_package().
     */
    public function test_table_sizes_are_pinned(): void
    {
        $this->assertCount(27, UpdateOutcome::UNTOUCHED_CODES);
        $this->assertCount(14, UpdateOutcome::TOUCHED_CODES);
    }

    /**
     * THE TEST THAT STOPS A REFACTOR DERIVING ONE TABLE FROM THE OTHER. Both
     * temp-backup codes are stage `install`, but move_to_temp_backup_dir()
     * returns fs_temp_backup_mkdir at class-wp-upgrader.php:1156, BEFORE its
     * own move_dir() at :1170, and fs_temp_backup_move at :1172, after it.
     * Stage and destination_touched are SEPARATE functions of the code.
     */
    public function test_the_two_temp_backup_codes_share_a_stage_and_disagree_on_touched(): void
    {
        $mkdir = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors(['fs_temp_backup_mkdir' => '']));
        $move  = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors(['fs_temp_backup_move' => '']));

        $this->assertSame('install', $mkdir['stage']);
        $this->assertSame('install', $move['stage']);
        $this->assertFalse($mkdir['destination_touched']);
        $this->assertTrue($move['destination_touched']);
    }

    /**
     * bad_request is emitted on BOTH sides of the boundary
     * (class-wp-upgrader.php:541 and class-plugin-upgrader.php:575 above it,
     * class-plugin-upgrader.php:684 below it), so it is unclassifiable by code
     * alone and must resolve to null, which restores.
     */
    public function test_bad_request_is_in_neither_table_and_resolves_to_null(): void
    {
        $this->assertNotContains('bad_request', UpdateOutcome::UNTOUCHED_CODES);
        $this->assertNotContains('bad_request', UpdateOutcome::TOUCHED_CODES);

        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors(['bad_request' => '']));
        $this->assertNull($out['destination_touched']);
        $this->assertSame('unknown', $out['stage']);
    }

    /**
     * `up_to_date` never reaches us as a skin CODE: class-plugin-upgrader.php:203
     * passes the STRING, and WP_Ajax_Upgrader_Skin::error() rewrites string
     * errors to unknown_upgrade_error_N. That path is FACT 1's job.
     */
    public function test_up_to_date_is_not_a_table_entry(): void
    {
        $this->assertNotContains('up_to_date', UpdateOutcome::UNTOUCHED_CODES);
        $this->assertNotContains('up_to_date', UpdateOutcome::TOUCHED_CODES);
    }

    /** One touched code condemns the whole set, even beside allowlisted ones. */
    public function test_one_touched_code_beats_every_untouched_one(): void
    {
        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([
            'download_failed'   => '',
            'copy_failed_copy_dir' => '',
        ]));

        $this->assertTrue($out['destination_touched']);
    }

    /** One unrecognised code withholds the clean bill of health. */
    public function test_one_unrecognised_code_withholds_the_verdict(): void
    {
        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([
            'download_failed'         => '',
            'some_third_party_error'  => '',
        ]));

        $this->assertNull($out['destination_touched']);
    }

    // ---- FACT 1, the false identity -------------------------------------

    /**
     * Plugin_Upgrader::upgrade() returns literal false at
     * class-plugin-upgrader.php:205 BEFORE run() at :224 when WordPress has no
     * update entry for the target. Nothing ran, so nothing was touched. Without
     * this rule, a benign race (the control plane says pending, WordPress's own
     * transient disagrees because a sibling just cleared it) triggers a
     * SPURIOUS restore.
     */
    public function test_false_identity_classifies_as_untouched(): void
    {
        $out = UpdateOutcome::classify(false, null);

        $this->assertFalse($out['destination_touched']);
        $this->assertSame('preflight', $out['stage']);
        $this->assertSame(UpdateOutcome::CODE_NO_UPDATE_OFFERED, $out['code']);
        $this->assertFalse($out['may_have_deactivated']);
    }

    /** The identity rule must not consult the skin at all. */
    public function test_false_identity_ignores_skin_errors_entirely(): void
    {
        $out = UpdateOutcome::classify(false, $this->upgraderWithSkinErrors(['copy_failed_copy_dir' => '']));

        $this->assertFalse($out['destination_touched']);
        $this->assertSame(UpdateOutcome::CODE_NO_UPDATE_OFFERED, $out['code']);
    }

    /** @return array<string,array{mixed}> */
    public static function notFalseProvider(): array
    {
        return [
            'zero'         => [0],
            'empty string' => [''],
            'empty array'  => [[]],
            'null'         => [null],
        ];
    }

    /**
     * @dataProvider notFalseProvider
     *
     * IDENTITY, not truthiness. A null result is the shape a real pre-install
     * failure takes (Plugin_Upgrader::$result is shadowed uninitialised at
     * class-plugin-upgrader.php:31, so upgrade() returns null at :250 to :251),
     * and it must fall through to the skin, never take FACT 1's shortcut.
     *
     * @param mixed $result Falsy-but-not-false upgrader result.
     */
    public function test_falsy_results_that_are_not_false_do_not_take_the_identity_shortcut(mixed $result): void
    {
        $out = UpdateOutcome::classify($result, null);

        $this->assertNull($out['destination_touched']);
        $this->assertNotSame(UpdateOutcome::CODE_NO_UPDATE_OFFERED, $out['code']);
    }

    // ---- FACT 2, a WP_Error result --------------------------------------

    public function test_a_wp_error_result_uses_its_own_code_and_data(): void
    {
        // Self-contained WP_Error-shaped double rather than the suite's shared
        // one, whose constructor narrows $data to array while real WP_Error
        // (and core's own download_failed at class-wp-upgrader.php:340) passes
        // a string.
        $error = new class {
            /** @return string */
            public function get_error_code(): string
            {
                return 'download_failed';
            }

            /**
             * @param string $code Error code.
             * @return string
             */
            public function get_error_data(string $code = ''): string
            {
                return 'https://example.test/x.zip';
            }

            /** @return string */
            public function get_error_message(): string
            {
                return 'nope';
            }
        };

        $out = UpdateOutcome::classify($error, null);

        $this->assertSame('download_failed', $out['code']);
        $this->assertSame('https://example.test/x.zip', $out['data']);
        $this->assertSame('download', $out['stage']);
        $this->assertFalse($out['destination_touched']);
    }

    // ---- THE REPORTED FAILURE -------------------------------------------

    /**
     * THE REGRESSION TEST FOR GitHub issue #328's own report: an unzip that
     * fails with "Could not copy file." while extracting into
     * wp-content/upgrade/. That is a pre-boundary failure and must never
     * trigger a restore over an untouched plugin directory.
     */
    public function test_the_reported_failure_classifies_as_untouched_at_the_unpack_stage(): void
    {
        $out = UpdateOutcome::classify(
            null,
            $this->upgraderWithSkinErrors([
                'copy_failed_ziparchive' => 'wp-seopress/vendor/psr/log/Psr/Log/Test/DummyTest.php',
            ])
        );

        $this->assertFalse($out['destination_touched']);
        $this->assertSame('unpack', $out['stage']);
        $this->assertSame('copy_failed_ziparchive', $out['code']);
        $this->assertSame('wp-seopress/vendor/psr/log/Psr/Log/Test/DummyTest.php', $out['data']);
        $this->assertFalse(
            $out['may_have_deactivated'],
            'an unpack failure returns at class-wp-upgrader.php:888 to :894, before install_package() is called at '
            . ':898, so core never reached the filter that deactivates the plugin'
        );
    }

    // ---- may_have_deactivated -------------------------------------------

    public function test_codes_at_or_after_the_pre_install_filter_report_possible_deactivation(): void
    {
        foreach (['source_read_failed', 'fs_temp_backup_mkdir', 'copy_failed_copy_dir'] as $code) {
            $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([$code => '']));
            $this->assertTrue($out['may_have_deactivated'], $code . ' arrives after upgrader_pre_install');
        }
    }

    public function test_download_and_unpack_codes_never_report_possible_deactivation(): void
    {
        foreach (['no_package', 'download_failed', 'incompatible_archive', 'copy_failed_ziparchive'] as $code) {
            $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors([$code => '']));
            $this->assertFalse($out['may_have_deactivated'], $code . ' arrives before install_package() is called');
        }
    }

    // ---- sanitisation ----------------------------------------------------

    public function test_a_hostile_code_is_replaced_rather_than_echoed(): void
    {
        $out = UpdateOutcome::classify(null, $this->upgraderWithSkinErrors(['DROP TABLE wp_options' => '']));

        $this->assertSame(UpdateOutcome::CODE_UNRECOGNIZED, $out['code']);
        $this->assertNull($out['destination_touched']);
    }

    public function test_error_data_is_bounded_and_stripped_of_control_characters(): void
    {
        $out = UpdateOutcome::classify(
            null,
            $this->upgraderWithSkinErrors(['copy_failed_ziparchive' => "a\x00b\x1F" . str_repeat('x', 400)])
        );

        $this->assertLessThanOrEqual(203, strlen($out['data']));
        $this->assertStringNotContainsString("\x00", $out['data']);
        $this->assertStringNotContainsString("\x1F", $out['data']);
    }

    // ---- shapes we cannot read ------------------------------------------

    public function test_a_skin_without_get_errors_resolves_to_null(): void
    {
        $upgrader = new \stdClass();
        $upgrader->skin = new \stdClass(); // phpcs:ignore WordPress.NamingConventions.ValidVariableName.UsedPropertyNotSnakeCase -- mirrors WP_Upgrader's own public $skin property name exactly

        $out = UpdateOutcome::classify(null, $upgrader);
        $this->assertNull($out['destination_touched']);
        $this->assertSame('', $out['code']);
    }

    public function test_a_throwing_skin_resolves_to_null_rather_than_propagating(): void
    {
        $skin = new class {
            /** @return object */
            public function get_errors(): object
            {
                throw new \RuntimeException('skin exploded');
            }
        };
        $upgrader = new \stdClass();
        $upgrader->skin = $skin; // phpcs:ignore WordPress.NamingConventions.ValidVariableName.UsedPropertyNotSnakeCase -- mirrors WP_Upgrader's own public $skin property name exactly

        $out = UpdateOutcome::classify(null, $upgrader);
        $this->assertNull($out['destination_touched']);
    }

    public function test_a_successful_result_is_never_classified_untouched(): void
    {
        $out = UpdateOutcome::classify(['destination' => '/x'], null);

        $this->assertNull($out['destination_touched'], 'classify() only speaks about failures; the caller decides');
    }

    // ---- the standing prohibition ---------------------------------------

    /**
     * NOTHING on this path may set wp_doing_cron(). Pretending to be cron
     * inside a REST request re-enables WordPress's own background updater
     * (wp-includes/update.php:331 and :885 to :891) inside the very window we
     * are holding the site's update lock for. The single most likely wrong
     * future edit, because it would make core enable maintenance mode for us
     * for free and would look like a simplification.
     */
    public function test_no_restore_skip_source_file_sets_wp_doing_cron(): void
    {
        $files = [
            'includes/support/class-update-outcome.php',
            'includes/support/class-destination-verifier.php',
            'includes/support/class-site-update-lock.php',
            'includes/commands/class-update-command.php',
        ];

        foreach ($files as $relative) {
            $source = (string) file_get_contents(dirname(__DIR__) . '/' . $relative);
            $source = (string) preg_replace('#/\*.*?\*/#s', '', $source);
            $source = (string) preg_replace('#//[^\n]*#', '', $source);

            $this->assertStringNotContainsString('DOING_CRON', $source, $relative . ' must not define DOING_CRON');
            $this->assertStringNotContainsString(
                'wp_doing_cron',
                $source,
                $relative . ' must not touch wp_doing_cron()'
            );
        }
    }
}
