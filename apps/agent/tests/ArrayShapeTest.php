<?php
/**
 * ArrayShape::isList must agree with PHP's own list test on every shape.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Support\ArrayShape;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\ArrayShape
 */
final class ArrayShapeTest extends TestCase
{
    /**
     * @return array<string,array{0:array<mixed>}>
     */
    public static function shapes(): array
    {
        $unset = [1, 2, 3];
        unset($unset[1]);
        $popped = [1, 2, 3];
        array_pop($popped);
        $reordered = [1 => 'b', 0 => 'a'];
        $sparse    = [];
        $sparse[5] = 'x';

        return [
            'empty'                  => [[]],
            'one'                    => [['a']],
            'three'                  => [[1, 2, 3]],
            'nested values'          => [[[1], ['k' => 2], null]],
            'explicit 0..n-1'        => [[0 => 'a', 1 => 'b', 2 => 'c']],
            'starts at 1'            => [[1 => 'a', 2 => 'b']],
            'out of order'           => [$reordered],
            'gap from unset'         => [$unset],
            'after pop'              => [$popped],
            'sparse'                 => [$sparse],
            'string keys'            => [['a' => 1, 'b' => 2]],
            'numeric string key'     => [['0' => 'a', '1' => 'b']],
            'non-numeric string key' => [['x' => 1]],
            'mixed keys'             => [[0 => 'a', 'k' => 'b']],
            'negative key'           => [[-1 => 'a']],
            'list then string'       => [[0 => 'a', 1 => 'b', 'c' => 'c']],
            'only key 1'             => [[1 => 'a']],
        ];
    }

    /**
     * @dataProvider shapes
     * @param array<mixed> $value Array under test.
     */
    public function test_agrees_with_php(array $value): void
    {
        $this->assertSame(\array_is_list($value), ArrayShape::isList($value));
    }

    public function test_both_answers_are_exercised(): void
    {
        $answers = [];
        foreach (self::shapes() as [$value]) {
            $answers[ArrayShape::isList($value) ? 'list' : 'map'] = true;
        }
        $this->assertSame(['list' => true, 'map' => true], $answers + ['list' => false, 'map' => false]);
    }
}
