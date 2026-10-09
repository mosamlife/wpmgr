<?php
/**
 * IdSeed: deterministic node ids, so a precheck and its write build the same
 * bytes.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\IdSeed;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\IdSeed
 */
final class IdSeedTest extends TestCase
{
    private const REQUEST = '0b6f2c1e-6a43-4c39-9b1d-2f6f0d6a7e15';

    private const PATHS = ['outline[0]', 'outline[1]', 'outline[1].children[0]', 'outline[1].children[1]', 'outline[0]'];

    public function test_ids_are_seven_lowercase_hex(): void
    {
        $seed = new IdSeed(self::REQUEST);
        $ids  = [];
        for ($i = 0; $i < 300; $i++) {
            $id = $seed->next('outline[' . $i . ']');
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}$/D', $id);
            $ids[] = $id;
        }
        $this->assertCount(300, array_unique($ids), 'one seed never issues the same id twice');

        $long = new IdSeed(self::REQUEST, 13);
        $this->assertMatchesRegularExpression('/^[0-9a-f]{13}$/D', $long->next('outline[0]'));

        foreach ([0, 65, -1] as $bad) {
            try {
                new IdSeed(self::REQUEST, $bad);
                $this->fail('length ' . $bad . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
        $this->expectException(\InvalidArgumentException::class);
        new IdSeed('');
    }

    public function test_same_request_and_paths_give_same_ids(): void
    {
        $a = new IdSeed(self::REQUEST);
        $b = new IdSeed(self::REQUEST);
        $a->reserve(['abc1234', 7]);
        $b->reserve(['abc1234', 7]);

        $fromA = array_map([$a, 'next'], self::PATHS);
        $fromB = array_map([$b, 'next'], self::PATHS);

        $this->assertSame($fromA, $fromB);
        $this->assertCount(count(self::PATHS), array_unique($fromA), 'a repeated path still gets a fresh id');
    }

    public function test_other_request_gives_other_ids(): void
    {
        $a = new IdSeed(self::REQUEST);
        $b = new IdSeed('0b6f2c1e-6a43-4c39-9b1d-2f6f0d6a7e16');

        foreach (self::PATHS as $path) {
            $this->assertNotSame($a->next($path), $b->next($path), $path);
        }
    }

    public function test_collision_bumps_counter_deterministically(): void
    {
        $path = 'outline[1].children[0]';

        // Already on the page: the counter-0 id is skipped for counter 1.
        $seed = new IdSeed(self::REQUEST);
        $seed->reserve([self::derive($path, 0)]);
        $this->assertSame(self::derive($path, 1), $seed->next($path));

        // Two reserved: counter 2. Values that are not ids are ignored.
        $seed = new IdSeed(self::REQUEST);
        $seed->reserve([self::derive($path, 0), null, ['x'], 1.5, self::derive($path, 1)]);
        $this->assertSame(self::derive($path, 2), $seed->next($path));

        // Issued earlier by the same seed: the same path asked again moves on.
        $seed = new IdSeed(self::REQUEST);
        $this->assertSame(self::derive($path, 0), $seed->next($path));
        $this->assertSame(self::derive($path, 1), $seed->next($path));
        $this->assertSame(self::derive($path, 2), $seed->next($path));

        // A one-character id space runs out instead of looping for ever.
        $tiny = new IdSeed(self::REQUEST, 1);
        for ($i = 0; $i < 16; $i++) {
            $tiny->next('n');
        }
        $this->expectException(\RuntimeException::class);
        $tiny->next('n');
    }

    public function test_pinned_vector(): void
    {
        // Derived once with php -r straight from the formula, not through IdSeed.
        $seed = new IdSeed(self::REQUEST);
        $this->assertSame(
            ['7a8b223', '3fe9336', 'cf57111', '3af8513', 'd890b1e'],
            array_map([$seed, 'next'], self::PATHS)
        );
        $this->assertSame('wpmgr.builder.id.v1', IdSeed::DOMAIN);
    }

    /**
     * The formula, written out independently of the class.
     *
     * @param string $path    Node path.
     * @param int    $counter Counter.
     * @return string
     */
    private static function derive(string $path, int $counter): string
    {
        return substr(hash('sha256', (string) json_encode(['wpmgr.builder.id.v1', self::REQUEST, $path, $counter])), 0, 7);
    }
}
