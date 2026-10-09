<?php
/**
 * VersionCompare replays the case file the control plane's Go compare and SQL
 * compare are tested against, so all three order versions identically.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Abilities\VersionCompare;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\VersionCompare
 */
final class VersionCompareTest extends TestCase
{
    public function test_every_shared_case_matches_the_control_plane(): void
    {
        $cases = self::cases();
        $wrong = [];
        foreach ($cases as [$a, $b, $want]) {
            if (VersionCompare::compare($a, $b) !== $want) {
                $wrong[] = sprintf('compare(%s, %s) = %d, want %d', json_encode($a), json_encode($b), VersionCompare::compare($a, $b), $want);
            }
            if (VersionCompare::compare($b, $a) !== -$want) {
                $wrong[] = sprintf('compare(%s, %s) = %d, want %d (reversed)', json_encode($b), json_encode($a), VersionCompare::compare($b, $a), -$want);
            }
        }
        $this->assertSame([], $wrong);
    }

    public function test_the_case_file_is_not_php_version_compare(): void
    {
        $differ = 0;
        foreach (self::cases() as [$a, $b, $want]) {
            if (version_compare($a, $b) !== $want) {
                $differ++;
            }
        }
        $this->assertGreaterThan(0, $differ, 'the cases must include inputs where PHP orders differently, or they prove nothing about the port');
    }

    public function test_in_range_is_inclusive_at_both_ends(): void
    {
        $this->assertTrue(VersionCompare::inRange('2.4.1', '2.4.1', '2.4.1'));
        $this->assertTrue(VersionCompare::inRange('2.4', '2.4.0', '2.5'));
        $this->assertFalse(VersionCompare::inRange('2.4.2', '2.4', '2.4.1'));
        $this->assertFalse(VersionCompare::inRange('2.3.9', '2.4', '2.4.1'));
    }

    /**
     * The case file, read by its repo-relative path. A missing or malformed
     * file fails the test rather than passing it vacuously.
     *
     * @return list<array{0:string,1:string,2:int}>
     */
    private static function cases(): array
    {
        $path = dirname(__DIR__, 2) . '/api/db/testdata/wpmgr_version_cmp_cases.json';
        self::assertFileExists($path, 'the shared version case file is missing, so this test proves nothing');
        $raw = json_decode((string) file_get_contents($path), true);
        self::assertIsArray($raw, 'the shared version case file is not JSON');
        $cases = $raw['cases'] ?? null;
        self::assertIsArray($cases);
        self::assertNotEmpty($cases, 'the shared version case file has no cases');
        $out = [];
        foreach ($cases as $c) {
            self::assertTrue(is_array($c) && count($c) === 3 && is_string($c[0]) && is_string($c[1]) && is_int($c[2]), 'malformed case: ' . json_encode($c));
            $out[] = [$c[0], $c[1], $c[2]];
        }

        return $out;
    }
}
