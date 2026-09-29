<?php
/**
 * Merge gate for the reach table.
 *
 * Every host and edge integration declares SLUG, REACH and REACH_NOTE. A row
 * that is not 'shared', or that overrides purgeUrlsExact(), must carry a note
 * of the form "Verified YYYY-MM-DD: <what was checked>". The control plane
 * keeps a mirror of this table; the slugs and reach values here are the
 * source it is checked against.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Integrations\Integration;
use WPMgr\Agent\Integrations\Integrations;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/** Fixture: 'install' with no dated note. */
final class ReachGateInstallUnnoted extends Integration
{
    public const SLUG = 'gate_install_unnoted';

    protected const REACH = self::REACH_INSTALL;

    protected const REACH_NOTE = 'Checked with the host, reaches only this install.';

    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
    }
}

/** Fixture: 'install' with a dated note. */
final class ReachGateInstallNoted extends Integration
{
    public const SLUG = 'gate_install_noted';

    protected const REACH = self::REACH_INSTALL;

    protected const REACH_NOTE = 'Verified 2026-09-29: the host purge names only this install.';

    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
    }
}

/** Fixture: an exact-URL override with no dated note. */
final class ReachGateExactUnnoted extends Integration
{
    public const SLUG = 'gate_exact_unnoted';

    protected const REACH_NOTE = 'Shared: candidate exact purge.';

    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
    }

    protected function purgeUrlsExact(array $urls): void
    {
    }
}

/** Fixture: a reach value outside the two allowed. */
final class ReachGateUnknownReach extends Integration
{
    public const SLUG = 'gate_unknown_reach';

    protected const REACH = 'site';

    protected const REACH_NOTE = 'Verified 2026-09-29: fixture.';

    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
    }
}

/** Fixture: no SLUG and no note. */
final class ReachGateUndeclared extends Integration
{
    protected function detect(): bool
    {
        return false;
    }

    protected function purgeAll(): void
    {
    }
}

/**
 * @covers \WPMgr\Agent\Integrations\Integration
 * @covers \WPMgr\Agent\Integrations\Integrations
 */
final class IntegrationReachGateTest extends TestCase
{
    /** The reach table as it must stand at merge: every row shared. */
    private const EXPECTED = [
        'cloudpanel' => 'shared',
        'varnish'    => 'shared',
        'cloudflare' => 'shared',
        'kinsta'     => 'shared',
        'siteground' => 'shared',
        'wpengine'   => 'shared',
        'cloudways'  => 'shared',
        'runcloud'   => 'shared',
        'gridpane'   => 'shared',
        'spinupwp'   => 'shared',
        'rocketnet'  => 'shared',
        'wpcloud'    => 'shared',
    ];

    public function test_every_shipped_integration_passes_the_gate(): void
    {
        $classes = Integrations::classes();
        $this->assertNotSame([], $classes, 'the gate must have integrations to check');

        foreach ($classes as $class) {
            $this->assertTrue(class_exists($class), $class . ' must load');
            $this->assertSame([], $class::reachProblems(), $class . ' fails the reach gate');
        }
    }

    public function test_reach_table_matches_the_mirror(): void
    {
        $table = [];
        foreach (Integrations::reachTable() as $row) {
            $this->assertArrayNotHasKey($row['slug'], $table, 'slugs are unique');
            $table[$row['slug']] = $row['reach'];
            $this->assertFalse($row['urls_exact'], $row['slug'] . ' enables an exact-URL purge');
            $this->assertNotSame('', trim($row['note']), $row['slug'] . ' has no note');
        }

        $this->assertSame(self::EXPECTED, $table);
        $this->assertCount(count(Integrations::classes()), $table, 'every booted integration has a row');
    }

    public function test_slugs_are_lowercase_identifiers(): void
    {
        foreach (Integrations::reachTable() as $row) {
            $this->assertMatchesRegularExpression('/^[a-z][a-z0-9_]*$/', $row['slug']);
        }
    }

    /**
     * @dataProvider plantedRows
     *
     * @param class-string<Integration> $class
     */
    public function test_gate_refuses_a_planted_row(string $class, string $expected): void
    {
        $problems = $class::reachProblems();

        $this->assertNotSame([], $problems);
        $this->assertStringContainsString($expected, implode("\n", $problems));
    }

    /** @return array<string,array{0:class-string,1:string}> */
    public static function plantedRows(): array
    {
        return [
            'install without a dated note'   => [ReachGateInstallUnnoted::class, 'no dated Verified note'],
            'exact override without a note'  => [ReachGateExactUnnoted::class, 'purgeUrlsExact() without a dated Verified note'],
            'unknown reach value'            => [ReachGateUnknownReach::class, 'unknown REACH'],
            'no slug'                        => [ReachGateUndeclared::class, 'declares no SLUG'],
            'no note'                        => [ReachGateUndeclared::class, 'declares no REACH_NOTE'],
        ];
    }

    public function test_gate_accepts_an_install_row_with_a_dated_note(): void
    {
        $this->assertSame([], ReachGateInstallNoted::reachProblems());
    }

    /**
     * @dataProvider notes
     */
    public function test_verified_note_pattern(string $note, bool $verified): void
    {
        $this->assertSame($verified, Integration::isVerifiedNote($note));
    }

    /** @return array<string,array{0:string,1:bool}> */
    public static function notes(): array
    {
        return [
            'dated'               => ['Verified 2026-09-29: purge names this domain.', true],
            'no date'             => ['Verified: purge names this domain.', false],
            'no text after colon' => ['Verified 2026-09-29: ', false],
            'not at start'        => ['Note. Verified 2026-09-29: x', false],
            'lower case'          => ['verified 2026-09-29: x', false],
            'short date'          => ['Verified 26-9-29: x', false],
            'empty'               => ['', false],
        ];
    }
}
