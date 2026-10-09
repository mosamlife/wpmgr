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

    /**
     * Plugin Check refuses a call to array_is_list() against the plugin's
     * declared minimum WordPress version, so no file the plugin ships calls it.
     */
    public function test_no_shipped_file_calls_array_is_list(): void
    {
        $root  = dirname(__DIR__);
        $files = [$root . '/wpmgr-agent.php'];
        foreach (['includes', 'mu-plugin-loader'] as $dir) {
            $iterator = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator($root . '/' . $dir, \FilesystemIterator::SKIP_DOTS)
            );
            foreach ($iterator as $file) {
                if ($file instanceof \SplFileInfo && $file->isFile() && $file->getExtension() === 'php') {
                    $files[] = $file->getPathname();
                }
            }
        }
        // The scan must have found the plugin's sources, or it proves nothing.
        $this->assertGreaterThan(100, count($files));
        $this->assertContains($root . '/includes/support/class-array-shape.php', $files);

        $found = [];
        foreach ($files as $path) {
            $source = file_get_contents($path);
            $this->assertIsString($source, $path);
            foreach (self::arrayIsListCallLines($source) as $line) {
                $found[] = substr($path, strlen($root) + 1) . ':' . $line;
            }
        }
        $this->assertSame([], $found, 'use ArrayShape::isList() instead of array_is_list()');
    }

    public function test_the_scan_sees_calls_and_ignores_everything_else(): void
    {
        $calls = [
            'plain'            => '<?php if (array_is_list($x)) {}',
            'spaced'           => '<?php $a = array_is_list ( $x );',
            'fully qualified'  => '<?php $a = \array_is_list($x);',
            'callback string'  => '<?php $a = array_filter($x, \'array_is_list\');',
            'upper case'       => '<?php $a = ARRAY_IS_LIST($x);',
        ];
        foreach ($calls as $label => $source) {
            $this->assertSame([1], self::arrayIsListCallLines($source), $label);
        }

        $not_calls = [
            'comment'         => "<?php\n// array_is_list(\$x) is not used\n\$a = 1;",
            'doc comment'     => "<?php\n/** array_is_list(\$x) */\n\$a = 1;",
            'string'          => '<?php $a = "call array_is_list(\$x) here";',
            'method'          => '<?php $a = $this->array_is_list($x);',
            'static method'   => '<?php $a = Shape::array_is_list($x);',
            'declaration'     => '<?php function array_is_list($x) { return true; }',
            'own helper'      => '<?php $a = ArrayShape::isList($x);',
            'longer name'     => '<?php $a = my_array_is_list($x);',
        ];
        foreach ($not_calls as $label => $source) {
            $this->assertSame([], self::arrayIsListCallLines($source), $label);
        }

        $this->assertSame([3], self::arrayIsListCallLines("<?php\n\n\$a = array_is_list(\$x);"), 'the line of the call');
    }

    /**
     * The lines of $source that call array_is_list() or name it as a callback
     * string. Comments, other strings, methods and declarations do not count.
     *
     * @param string $source PHP source.
     * @return list<int>
     */
    private static function arrayIsListCallLines(string $source): array
    {
        $tokens = token_get_all($source);
        $lines  = [];
        $prev   = null;
        $count  = count($tokens);
        for ($i = 0; $i < $count; $i++) {
            $token = $tokens[$i];
            if (is_array($token) && in_array($token[0], [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true)) {
                continue;
            }
            if (is_array($token)) {
                $name = strtolower(ltrim($token[1], '\\'));
                if (
                    in_array($token[0], [T_STRING, T_NAME_FULLY_QUALIFIED], true)
                    && $name === 'array_is_list'
                    && !in_array($prev, [T_OBJECT_OPERATOR, T_NULLSAFE_OBJECT_OPERATOR, T_DOUBLE_COLON, T_FUNCTION, T_CONST, T_NEW], true)
                ) {
                    for ($j = $i + 1; $j < $count; $j++) {
                        $next = $tokens[$j];
                        if (is_array($next) && $next[0] === T_WHITESPACE) {
                            continue;
                        }
                        if ($next === '(') {
                            $lines[] = $token[2];
                        }
                        break;
                    }
                } elseif ($token[0] === T_CONSTANT_ENCAPSED_STRING && trim($token[1], '\'"') === 'array_is_list') {
                    $lines[] = $token[2];
                }
                $prev = $token[0];
            } else {
                $prev = $token;
            }
        }

        return $lines;
    }
}
