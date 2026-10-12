<?php
/**
 * GUT: the refusal envelope every builder refusal can carry. A client that
 * shows only the message still gets the fix, because the message inlines the
 * first three concrete failures; the same failures travel in failures[], and
 * a refusal about a pattern carries that pattern's item schema.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use InvalidArgumentException;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\Builders\Refusal;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\Refusal
 */
final class RefusalEnvelopeTest extends TestCase
{
    /** @return list<array<string,mixed>> */
    private static function five(): array
    {
        return [
            ['path' => 'outline[1]', 'key' => 'anchor', 'reason' => 'must start with a letter and hold only a-z, 0-9 and -'],
            ['path' => 'outline[2]', 'key' => 'anchor', 'reason' => 'is already used by outline[1]'],
            ['path' => 'design.colors', 'key' => 'ink', 'reason' => 'must be #RRGGBB in capitals'],
            ['path' => 'design', 'key' => 'radius', 'reason' => 'must be one of the allowed values', 'allowed' => ['none', 'sm', 'md', 'lg']],
            ['path' => 'outline[4].children[0]', 'key' => 'url', 'reason' => 'points at an anchor the page does not have'],
        ];
    }

    public function testEnvelopeCarriesCodeMessageAndFailures(): void
    {
        $r = Refusal::make('anchor_invalid', 'The outline was refused.', self::five());

        $this->assertSame(['code', 'detail', 'failures'], array_keys($r));
        $this->assertSame('anchor_invalid', $r['code']);
        $this->assertIsString($r['detail']);
        $this->assertCount(5, $r['failures']);
        $this->assertSame(
            ['path' => 'outline[1]', 'key' => 'anchor', 'reason' => 'must start with a letter and hold only a-z, 0-9 and -'],
            $r['failures'][0],
            'a failure is {path, key, reason}, in that order, and has no allowed list unless it was given one'
        );
        $this->assertSame(
            ['path' => 'design', 'key' => 'radius', 'reason' => 'must be one of the allowed values', 'allowed' => ['none', 'sm', 'md', 'lg']],
            $r['failures'][3]
        );
    }

    public function testMessageInlinesTheFirstThreeFailures(): void
    {
        $r = Refusal::make('anchor_invalid', 'The outline was refused.', self::five());

        $this->assertSame(
            'The outline was refused. '
            . '1. outline[1].anchor: must start with a letter and hold only a-z, 0-9 and -; '
            . '2. outline[2].anchor: is already used by outline[1]; '
            . '3. design.colors.ink: must be #RRGGBB in capitals (2 more in failures)',
            $r['detail']
        );
        $this->assertStringNotContainsString('radius', $r['detail'], 'the fourth failure is in failures[], not in the message');
    }

    public function testFewerThanThreeFailuresAreAllInlined(): void
    {
        $one = Refusal::make('layout_invalid', 'The page was refused.', [
            ['path' => 'outline[0]', 'key' => 'children', 'reason' => 'needs at least one child'],
        ]);
        $this->assertSame('The page was refused. 1. outline[0].children: needs at least one child', $one['detail']);

        $none = Refusal::make('bad_input', 'The page was refused.', []);
        $this->assertSame('The page was refused.', $none['detail']);
        $this->assertSame([], $none['failures']);
    }

    public function testAllowedValuesAreInlinedAndBounded(): void
    {
        $r = Refusal::make('bad_input', 'Refused.', [
            ['path' => 'design', 'key' => 'radius', 'reason' => 'must be one of the allowed values', 'allowed' => ['none', 'sm', 'md', 'lg']],
        ]);
        $this->assertSame('Refused. 1. design.radius: must be one of the allowed values (allowed: none, sm, md, lg)', $r['detail']);

        $many = [];
        for ($i = 1; $i <= 40; ++$i) {
            $many[] = 'icon-' . $i;
        }
        $r = Refusal::make('icon_unknown', 'Refused.', [
            ['path' => 'outline[0].children[0].features[1]', 'key' => 'icon', 'reason' => 'is not in the icon list', 'allowed' => $many],
        ]);
        $this->assertCount(Refusal::MAX_ALLOWED, $r['failures'][0]['allowed'], 'allowed[] is bounded');
        $this->assertStringContainsString('icon-1,', $r['detail']);
        $this->assertStringNotContainsString('icon-40', $r['detail'], 'the message inlines only the first few allowed values');
        $this->assertStringContainsString('...', $r['detail']);
    }

    public function testAFailureMayNameANodeRef(): void
    {
        $r = Refusal::make('node_not_editable', 'The edit was refused.', [
            ['ref' => '3c4d5e6', 'key' => 'field', 'reason' => 'is not offered by this node', 'allowed' => ['text']],
        ]);
        $this->assertSame(
            ['ref' => '3c4d5e6', 'key' => 'field', 'reason' => 'is not offered by this node', 'allowed' => ['text']],
            $r['failures'][0]
        );
        $this->assertSame('The edit was refused. 1. ref 3c4d5e6.field: is not offered by this node (allowed: text)', $r['detail']);
    }

    public function testAPatternRefusalCarriesTheItemSchema(): void
    {
        $r = Refusal::forPattern('pattern_items_out_of_range', 'The pattern was refused.', 'pricing', [
            ['path' => 'outline[3].children[1]', 'key' => 'plans', 'reason' => 'takes 1 to 4 plans; this one has 5'],
        ]);

        $this->assertSame(['code', 'detail', 'failures', 'pattern', 'item_schema'], array_keys($r));
        $this->assertSame('pricing', $r['pattern']);
        $plans = null;
        foreach (OutlineV3Schema::patterns([]) as $node) {
            if ($node['properties']['pattern']['enum'] === ['pricing']) {
                $plans = $node['properties']['plans']['items'];
            }
        }
        $this->assertNotNull($plans);
        $this->assertSame($plans, $r['item_schema'], 'the item schema is the one the input schema publishes');

        foreach (['features', 'testimonials', 'faq', 'logos', 'stats'] as $pattern) {
            $this->assertIsArray(OutlineV3Schema::itemSchema($pattern), $pattern . ' has an item schema');
        }
        foreach (['hero', 'cta', 'carousel'] as $pattern) {
            $this->assertNull(OutlineV3Schema::itemSchema($pattern), $pattern . ' holds no items');
        }
        $hero = Refusal::forPattern('hero_misplaced', 'Refused.', 'hero', [['path' => 'outline[1]', 'key' => 'pattern', 'reason' => 'the hero opens the first section']]);
        $this->assertSame(['code', 'detail', 'failures'], array_keys($hero), 'a pattern with no items carries no item schema');
    }

    public function testCodesAreNeverRenamed(): void
    {
        $codes = [
            'bad_input', 'create_content_invalid', 'layout_invalid', 'link_invalid', 'layout_needs_block_editor',
            'node_not_editable', 'node_not_found', 'ops_invalid', 'op_not_supported_by_builder', 'page_too_large', 'conflict',
            'containers_required', 'anchor_invalid', 'anchor_duplicate', 'anchor_reserved', 'hero_misplaced', 'h1_outside_hero',
            'background_not_image', 'pattern_items_out_of_range', 'icon_unknown', 'link_anchor_missing', 'anchor_in_use',
            'font_unknown', 'design_contrast_low', 'design_not_ready', 'group_activator_missing', 'style_key_not_allowed',
        ];
        foreach ($codes as $code) {
            $this->assertSame($code, Refusal::make($code, 'Refused.', [])['code']);
        }
        foreach (['', 'Bad_Input', 'bad input', 'a', '1code', str_repeat('a', 65)] as $bad) {
            try {
                Refusal::make($bad, 'Refused.', []);
                $this->fail('"' . $bad . '" is not a refusal code');
            } catch (InvalidArgumentException $e) {
                $this->assertStringContainsString('code', $e->getMessage());
            }
        }
    }

    public function testTheCommandShapeKeepsTheOldFieldsAndAddsTheNew(): void
    {
        $env = Refusal::make('anchor_duplicate', 'Refused.', [
            ['path' => 'outline[1]', 'key' => 'anchor', 'reason' => 'is already used by outline[0]'],
        ]);
        $cmd = Refusal::command($env);

        $this->assertSame(['ok', 'outcome', 'code', 'detail', 'retryable', 'failures'], array_keys($cmd));
        $this->assertFalse($cmd['ok']);
        $this->assertSame('refused', $cmd['outcome']);
        $this->assertFalse($cmd['retryable']);
        $this->assertSame($env['code'], $cmd['code']);
        $this->assertSame($env['detail'], $cmd['detail']);
        $this->assertSame($env['failures'], $cmd['failures']);

        $pattern = Refusal::command(Refusal::forPattern('pattern_items_out_of_range', 'Refused.', 'faq', [
            ['path' => 'outline[1].children[0]', 'key' => 'faq', 'reason' => 'takes 1 to 12 items'],
        ]));
        $this->assertSame(['ok', 'outcome', 'code', 'detail', 'retryable', 'failures', 'pattern', 'item_schema'], array_keys($pattern));
    }

    public function testStringsAreOneLineAndBounded(): void
    {
        $long = str_repeat('word ', 200);
        $r    = Refusal::make('bad_input', "Refused\nnow.\t", [
            ['path' => "outline[0]\n", 'key' => "anchor\r", 'reason' => $long],
        ]);

        $this->assertDoesNotMatchRegularExpression('/[\x00-\x1F\x7F]/', $r['detail']);
        $this->assertLessThanOrEqual(Refusal::MAX_DETAIL_CHARS, strlen($r['detail']));
        $this->assertSame('outline[0]', $r['failures'][0]['path']);
        $this->assertSame('anchor', $r['failures'][0]['key']);
        $this->assertLessThanOrEqual(Refusal::MAX_REASON_CHARS, strlen($r['failures'][0]['reason']));
        $this->assertStringEndsWith('...', $r['failures'][0]['reason']);

        $many = [];
        for ($i = 0; $i < 60; ++$i) {
            $many[] = ['path' => 'outline[' . $i . ']', 'key' => 'anchor', 'reason' => 'is not valid'];
        }
        $r = Refusal::make('anchor_invalid', 'Refused.', $many);
        $this->assertCount(Refusal::MAX_FAILURES, $r['failures'], 'failures[] is bounded');
        $this->assertStringContainsString('(' . (Refusal::MAX_FAILURES - 3) . '+ more in failures)', $r['detail'], 'a cut list says it was cut');
    }

    public function testAMalformedFailureIsAProgrammingError(): void
    {
        $bad = [
            'no reason'               => [['path' => 'outline[0]', 'key' => 'anchor']],
            'empty reason'            => [['path' => 'outline[0]', 'key' => 'anchor', 'reason' => '']],
            'neither ref nor path'    => [['key' => 'anchor', 'reason' => 'is wrong']],
            'unknown field'           => [['path' => 'outline[0]', 'key' => 'anchor', 'reason' => 'is wrong', 'value' => '<b>']],
            'allowed not a list'      => [['path' => 'outline[0]', 'key' => 'anchor', 'reason' => 'is wrong', 'allowed' => 'a']],
            'allowed holds an int'    => [['path' => 'outline[0]', 'key' => 'anchor', 'reason' => 'is wrong', 'allowed' => [1]]],
            'path is not a string'    => [['path' => 3, 'key' => 'anchor', 'reason' => 'is wrong']],
            'a failure is not a map'  => ['outline[0]'],
        ];
        foreach ($bad as $name => $failures) {
            try {
                Refusal::make('bad_input', 'Refused.', $failures);
                $this->fail($name . ' should be refused');
            } catch (InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
    }
}
