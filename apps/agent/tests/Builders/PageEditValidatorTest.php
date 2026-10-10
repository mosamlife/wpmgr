<?php
/**
 * PageEditValidator: the wpmgr/page-edit input rules, replayed from the
 * shared ops cases and pinned one rule at a time.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\PageEditValidator
 */
final class PageEditValidatorTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-edit-ops-cases.json';

    private const FP = '7787514f2667f5577151c00759c33e4553edc1d75f96e0dc1ddcdc23989a5ea7';

    // -------------------------------------------------------------------------
    // The shared cases
    // -------------------------------------------------------------------------

    public function test_replays_every_shared_case(): void
    {
        $doc = self::opsCases();
        $this->assertNotEmpty($doc['cases']);
        foreach ($doc['cases'] as $case) {
            $builder = $case['builder'] ?? $doc['page']['builder'];
            $this->assertSame($case['agent'], self::agentAnswer(self::inputText($case), $doc['page']['nodes'], $builder), $case['name']);
        }
    }

    public function test_parse_refuses_exactly_the_cases_the_control_plane_refuses(): void
    {
        foreach (self::opsCases()['cases'] as $case) {
            $r = PageEditValidator::parse(self::inputText($case));
            if ($case['go'] === 'refuse') {
                $this->assertArrayHasKey('code', $r, $case['name'] . ': a rule that needs no page belongs to parse()');
                $this->assertSame($case['agent'], $r['code'], $case['name']);
            } else {
                $this->assertArrayHasKey('input', $r, $case['name'] . ': parse() refused ' . ($r['detail'] ?? ''));
            }
        }
    }

    // -------------------------------------------------------------------------
    // Rules that need the page
    // -------------------------------------------------------------------------

    public function test_refs_must_exist_before_the_call(): void
    {
        $this->assertRefused('node_not_found', 'ref_not_on_page', 0, [self::setText('0f0f0f0', 'text', 'Hello')]);
        $this->assertRefused('node_not_found', 'ref_not_on_page', 0, [self::insertAt('after', 'root', [self::para('Hello')])]);
        $this->assertRefused('node_not_found', 'ref_not_on_page', 1, [
            self::insertAt('after', '3c4d5e6', [self::para('Hello')]),
            self::insertAt('before', '0f0f0f0', [self::para('Hello')]),
        ]);
        $this->assertNull(self::check([self::setText('3c4d5e6', 'text', 'Autumn sale')]));
    }

    public function test_ref_removed_earlier_is_ops_invalid(): void
    {
        $this->assertRefused('ops_invalid', 'ref_gone', 1, [
            ['op' => 'remove', 'ref' => '2b3c4d5'],
            self::setText('3c4d5e6', 'text', 'Autumn sale'),
        ]);
        $this->assertRefused('ops_invalid', 'ref_gone', 1, [
            ['op' => 'replace', 'ref' => '1a2b3c4', 'outline' => [self::para('Opening hours')]],
            self::insertAt('into', '2b3c4d5', [self::para('Hello')]),
        ]);
        // A node that moved into a subtree removed later is gone with it (on
        // the page without its locked node, which the removed section holds).
        $this->assertRefused('ops_invalid', 'ref_gone', 2, [
            ['op' => 'move', 'ref' => '2b3c4d5', 'after' => '8b9c0d1'],
            ['op' => 'remove', 'ref' => '7a8b9c0'],
            self::setText('3c4d5e6', 'text', 'Autumn sale'),
        ], self::pageWithoutLocked());
        // A node that moved out of a subtree removed later is still on the page.
        $this->assertNull(self::check([
            ['op' => 'move', 'ref' => '2b3c4d5', 'after' => '8b9c0d1'],
            ['op' => 'remove', 'ref' => '1a2b3c4'],
            self::setText('3c4d5e6', 'text', 'Autumn sale'),
        ]));
        // Naming a node before an operation removes its subtree is fine.
        $this->assertNull(self::check([
            self::setText('3c4d5e6', 'text', 'Autumn sale'),
            ['op' => 'remove', 'ref' => '2b3c4d5'],
        ]));
    }

    public function test_insert_into_leaf_refused(): void
    {
        foreach (['4d5e6f7', '3c4d5e6', '5e6f7a8', 'b1e2f3a', 'd3a4b5c'] as $leaf) {
            $this->assertRefused('node_not_editable', 'takes_no_children', 0, [self::insertAt('into', $leaf, [self::para('Hello')])]);
        }
        $this->assertNull(self::check([self::insertAt('into', '2b3c4d5', [self::para('Hello')])]));
        $this->assertNull(self::check([self::insertAt('into', '1a2b3c4', [self::para('Hello')], 'first')]));

        $layout = [
            ['ref' => 'g1', 'parent' => 'root', 'kind' => 'group', 'editable' => []],
            ['ref' => 'r1', 'parent' => 'root', 'kind' => 'columns', 'editable' => []],
            ['ref' => 'c1', 'parent' => 'r1', 'kind' => 'column', 'editable' => []],
        ];
        foreach (['g1', 'r1', 'c1'] as $container) {
            $this->assertNull(self::check([self::insertAt('into', $container, [self::para('Hello')], 'last')], $layout), $container);
        }
    }

    public function test_move_into_own_subtree_refused(): void
    {
        $this->assertRefused('node_not_editable', 'into_own_subtree', 0, [['op' => 'move', 'ref' => '7a8b9c0', 'after' => '9c0d1e2']]);
        $this->assertRefused('node_not_editable', 'into_own_subtree', 0, [['op' => 'move', 'ref' => '8b9c0d1', 'before' => '9c0d1e2']]);
        $this->assertNull(self::check([['op' => 'move', 'ref' => '9c0d1e2', 'after' => 'a0d1e2f']]));

        // The subtree is the one the earlier operations left.
        $this->assertRefused('node_not_editable', 'into_own_subtree', 1, [
            ['op' => 'move', 'ref' => '8b9c0d1', 'before' => '2b3c4d5'],
            ['op' => 'move', 'ref' => '1a2b3c4', 'after' => '9c0d1e2'],
        ]);
        $this->assertNull(self::check([
            ['op' => 'move', 'ref' => '2b3c4d5', 'after' => '8b9c0d1'],
            ['op' => 'move', 'ref' => '1a2b3c4', 'after' => '3c4d5e6'],
        ]));
    }

    public function test_locked_node_never_targeted(): void
    {
        $targeted = [
            self::setText('b1e2f3a', 'text', 'Hello'),
            ['op' => 'replace', 'ref' => 'b1e2f3a', 'outline' => [self::para('Hello')]],
            ['op' => 'remove', 'ref' => 'b1e2f3a'],
            ['op' => 'move', 'ref' => 'b1e2f3a', 'after' => '9c0d1e2'],
            ['op' => 'move', 'ref' => 'b1e2f3a', 'before' => '3c4d5e6'],
        ];
        foreach ($targeted as $op) {
            $this->assertRefused('node_not_editable', 'locked', 0, [$op]);
        }

        // A locked node stays where it is, so it may be an anchor.
        $this->assertNull(self::check([self::insertAt('after', 'b1e2f3a', [self::para('Hello')])]));
        $this->assertNull(self::check([self::insertAt('before', 'b1e2f3a', [self::para('Hello')])]));
        $this->assertNull(self::check([['op' => 'move', 'ref' => '9c0d1e2', 'after' => 'b1e2f3a']]));
    }

    public function test_locked_node_never_goes_with_its_holder(): void
    {
        // Section 7a8b9c0 holds column 8b9c0d1, which holds the locked b1e2f3a.
        foreach (['7a8b9c0', '8b9c0d1'] as $holder) {
            $this->assertRefused('node_not_editable', 'holds_locked', 0, [['op' => 'remove', 'ref' => $holder]]);
            $this->assertRefused('node_not_editable', 'holds_locked', 0, [['op' => 'replace', 'ref' => $holder, 'outline' => [self::para('Hello')]]]);
        }
        $this->assertRefused('node_not_editable', 'holds_locked', 1, [
            self::setText('9c0d1e2', 'text', 'Autumn sale'),
            ['op' => 'remove', 'ref' => '7a8b9c0'],
        ]);

        // The holder is judged on the page as the earlier operations leave it.
        $this->assertRefused('node_not_editable', 'holds_locked', 1, [
            ['op' => 'move', 'ref' => '8b9c0d1', 'before' => '2b3c4d5'],
            ['op' => 'remove', 'ref' => '1a2b3c4'],
        ]);
        $this->assertNull(self::check([
            ['op' => 'move', 'ref' => '8b9c0d1', 'before' => '2b3c4d5'],
            ['op' => 'remove', 'ref' => '7a8b9c0'],
        ]), 'a section whose locked node moved out of it may go');

        // A node holding no locked node goes, and a holder may still move.
        $this->assertNull(self::check([['op' => 'remove', 'ref' => '1a2b3c4']]));
        $this->assertNull(self::check([['op' => 'replace', 'ref' => '2b3c4d5', 'outline' => [self::para('Hello')]]]));
        $this->assertNull(self::check([['op' => 'move', 'ref' => '7a8b9c0', 'before' => '1a2b3c4']]));
        $this->assertNull(self::check([['op' => 'remove', 'ref' => '7a8b9c0']], self::pageWithoutLocked()), 'the same section without its locked node');
    }

    public function test_set_text_names_a_field_the_node_offers(): void
    {
        $this->assertRefused('node_not_editable', 'field_not_offered', 0, [self::setText('3c4d5e6', 'caption', 'Hello')]);
        $this->assertRefused('node_not_editable', 'field_not_offered', 0, [self::setText('1a2b3c4', 'text', 'Hello')]);
        $this->assertRefused('node_not_editable', 'field_not_offered', 0, [self::setText('c2f3a4b', 'alt', 'Our team')]);
        $this->assertNull(self::check([self::setText('c2f3a4b', 'caption', 'Our team')]));
        $this->assertNull(self::check([self::setText('5e6f7a8', 'alt', 'Our team')]));
        $this->assertNull(self::check([self::setText('d3a4b5c', 'url', 'https://example.com/sale')]));
    }

    public function test_button_text_keeps_the_button_rule(): void
    {
        $this->assertNull(self::check([self::setText('d3a4b5c', 'text', str_repeat('é', 80))]));
        $r = self::check([self::setText('d3a4b5c', 'text', str_repeat('é', 81))]);
        $this->assertNotNull($r);
        $this->assertSame('bad_input', $r['code']);
        $this->assertSame(0, $r['op_index']);
        $this->assertStringStartsWith('operations[0].text: ', $r['detail']);
        // A heading takes the paragraph limit.
        $this->assertNull(self::check([self::setText('3c4d5e6', 'text', str_repeat('é', 81))]));
    }

    public function test_op_declared_by_the_adapter(): void
    {
        $noMove = ['operations' => ['set_text', 'insert', 'replace', 'remove'], 'node_kinds' => BuilderContract::KINDS];
        $this->assertRefused('op_not_supported_by_builder', 'move', 1, [
            self::setText('3c4d5e6', 'text', 'Autumn sale'),
            ['op' => 'move', 'ref' => '9c0d1e2', 'after' => 'a0d1e2f'],
        ], null, $noMove);
        $this->assertRefused('op_not_supported_by_builder', 'set_text', 0, [self::setText('3c4d5e6', 'text', 'Autumn sale')], null, ['operations' => []]);
        $this->assertNull(self::check([self::setText('3c4d5e6', 'text', 'Autumn sale')], null, $noMove));
    }

    public function test_new_nodes_counted_the_way_page_create_counts_blocks(): void
    {
        $outline = [
            self::para('One'),
            ['type' => 'group', 'children' => [self::para('A'), self::para('B'), self::para('C')]],
            ['type' => 'columns', 'columns' => [['children' => [self::para('L1'), self::para('L2')]], ['children' => [self::para('R1'), self::para('R2')]]]],
            ['type' => 'buttons', 'buttons' => [
                ['text' => 'One', 'url' => '/one'],
                ['text' => 'Two', 'url' => '/two'],
                ['text' => 'Three', 'url' => 'https://example.com/three'],
            ]],
        ];
        $r = PageEditValidator::parse(self::input([['op' => 'insert', 'after' => '1a2b3c4', 'outline' => $outline]]));
        $this->assertArrayHasKey('input', $r, $r['detail'] ?? '');
        // 1 + (1 + 3) + (1 + 2 x (1 + 2)) + (1 + 3)
        $this->assertSame(16, PageEditValidator::countNodes($r['input']['operations'][0]['outline']));

        $fifty = array_fill(0, 50, self::para('New'));
        $ops   = [];
        foreach (['3c4d5e6', '4d5e6f7', '5e6f7a8', '6f7a8b9', '9c0d1e2', 'a0d1e2f', 'c2f3a4b', 'd3a4b5c'] as $anchor) {
            $ops[] = self::insertAt('after', $anchor, $fifty);
        }
        $this->assertNull(self::check($ops), 'exactly 400 new nodes');
        $ops[] = ['op' => 'replace', 'ref' => '2b3c4d5', 'outline' => [self::para('One more')]];
        $this->assertRefused('page_too_large', 'new_nodes', 8, $ops);
    }

    public function test_nodes_after_the_call(): void
    {
        // 792 nodes in the first section, 8 in the second: 800.
        $nodes = [
            ['ref' => 's1', 'parent' => 'root', 'kind' => 'section', 'editable' => []],
            ['ref' => 'c1', 'parent' => 's1', 'kind' => 'column', 'editable' => []],
        ];
        for ($i = 0; $i < 790; ++$i) {
            $nodes[] = ['ref' => 'p' . $i, 'parent' => 'c1', 'kind' => 'paragraph', 'editable' => ['text']];
        }
        $nodes[] = ['ref' => 's2', 'parent' => 'root', 'kind' => 'section', 'editable' => []];
        $nodes[] = ['ref' => 'c2', 'parent' => 's2', 'kind' => 'column', 'editable' => []];
        for ($i = 0; $i < 6; ++$i) {
            $nodes[] = ['ref' => 'q' . $i, 'parent' => 'c2', 'kind' => 'paragraph', 'editable' => ['text']];
        }

        $this->assertNull(self::check([self::setText('p0', 'text', 'Hello')], $nodes));
        $this->assertRefused('page_too_large', 'nodes_after', null, [self::insertAt('after', 'p0', [self::para('One more')])], $nodes);
        $this->assertNull(self::check([['op' => 'remove', 'ref' => 'p1'], self::insertAt('after', 'p0', [self::para('One more')])], $nodes));
        // New nodes inside a subtree removed later leave with it: 800 + 10 - (8 + 10).
        $this->assertNull(self::check([
            self::insertAt('into', 'c2', array_fill(0, 10, self::para('New'))),
            ['op' => 'remove', 'ref' => 's2'],
        ], $nodes));
        // A replace counts its new nodes in and the replaced subtree out: 800 - 8 + 9.
        $this->assertRefused('page_too_large', 'nodes_after', null, [
            ['op' => 'replace', 'ref' => 's2', 'outline' => array_fill(0, 9, self::para('New'))],
        ], $nodes);
        $this->assertNull(self::check([
            ['op' => 'replace', 'ref' => 's2', 'outline' => array_fill(0, 8, self::para('New'))],
        ], $nodes));
    }

    public function test_malformed_projection_is_refused_loudly(): void
    {
        $parsed = PageEditValidator::parse(self::input([self::setText('a1', 'text', 'Hello')]));
        $this->assertArrayHasKey('input', $parsed);
        $bad = [
            'repeated ref'     => [['ref' => 'a1', 'parent' => 'root', 'kind' => 'heading'], ['ref' => 'a1', 'parent' => 'root', 'kind' => 'heading']],
            'child first'      => [['ref' => 'a1', 'parent' => 'b1', 'kind' => 'heading'], ['ref' => 'b1', 'parent' => 'root', 'kind' => 'section']],
            'no kind'          => [['ref' => 'a1', 'parent' => 'root']],
            'the root as node' => [['ref' => 'root', 'parent' => 'root', 'kind' => 'section']],
        ];
        foreach ($bad as $why => $nodes) {
            try {
                PageEditValidator::againstPage($parsed['input'], $nodes, ['operations' => BuilderContract::OPS]);
                $this->fail($why . ': accepted');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $why);
            }
        }
    }

    // -------------------------------------------------------------------------
    // Rules that need no page
    // -------------------------------------------------------------------------

    public function test_size_measured_on_bytes_not_characters(): void
    {
        $base = self::input([self::setText('3c4d5e6', 'text', str_repeat('é', 4000))]);
        $this->assertGreaterThan(mb_strlen($base, 'UTF-8'), strlen($base));

        $atLimit = $base . str_repeat(' ', BuilderContract::MAX_INPUT_BYTES - strlen($base));
        $this->assertSame(BuilderContract::MAX_INPUT_BYTES, strlen($atLimit));
        $this->assertArrayHasKey('input', PageEditValidator::parse($atLimit));

        $over = $atLimit . ' ';
        $this->assertSame(BuilderContract::MAX_INPUT_BYTES + 1, strlen($over));
        $this->assertLessThan(BuilderContract::MAX_INPUT_BYTES, mb_strlen($over, 'UTF-8'), 'fewer characters than the byte limit');
        $this->assertSame('bad_input', PageEditValidator::parse($over)['code'] ?? null);

        // A set_text text is measured in characters, as page-create measures it.
        $this->assertArrayHasKey('input', PageEditValidator::parse(self::input([self::setText('3c4d5e6', 'text', str_repeat('é', 5000))])));
        $this->assertSame('bad_input', PageEditValidator::parse(self::input([self::setText('3c4d5e6', 'text', str_repeat('a', 5001))]))['code'] ?? null);
    }

    public function test_object_not_array(): void
    {
        $op    = '{"op":"remove","ref":"6f7a8b9"}';
        $texts = [
            '[]',
            '{}',
            '',
            'null',
            '"text"',
            '418',
            'not json',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":{"0":' . $op . '}}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[[]]}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[{"op":"insert","after":"4d5e6f7","outline":{"0":{"type":"paragraph","text":"Hi"}}}]}',
            '{"post_id":418.0,"base_fingerprint":"' . self::FP . '","operations":[' . $op . ']}',
            '{"post_id":"418","base_fingerprint":"' . self::FP . '","operations":[' . $op . ']}',
            '{"post_id":0,"base_fingerprint":"' . self::FP . '","operations":[' . $op . ']}',
            '{"post_id":418,"base_fingerprint":"' . strtoupper(self::FP) . '","operations":[' . $op . ']}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[' . $op . '],"editor":"builder:elementor"}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[{"op":"remove","ref":"6f7a8b9","at":"x"}]}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[{"op":"move","ref":"6f7a8b9","after":"3c4d5e6","before":"4d5e6f7"}]}',
            '{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[{"op":"move","ref":"6f7a8b9","into":"2b3c4d5"}]}',
        ];
        foreach ($texts as $text) {
            $this->assertSame('bad_input', PageEditValidator::parse($text)['code'] ?? null, $text);
        }
        $this->assertArrayHasKey('input', PageEditValidator::parse('{"post_id":418,"base_fingerprint":"' . self::FP . '","operations":[' . $op . ']}'));
    }

    public function test_a_call_names_each_node_once(): void
    {
        $twice = [
            [['op' => 'move', 'ref' => '9c0d1e2', 'after' => '9c0d1e2']],
            [self::insertAt('after', '4d5e6f7', [self::para('Hi')]), ['op' => 'remove', 'ref' => '4d5e6f7']],
            [['op' => 'move', 'ref' => '9c0d1e2', 'after' => 'a0d1e2f'], ['op' => 'move', 'ref' => '6f7a8b9', 'before' => 'a0d1e2f']],
        ];
        foreach ($twice as $ops) {
            $this->assertSame('ops_invalid', PageEditValidator::parse(self::input($ops))['code'] ?? null);
        }
    }

    public function test_set_text_follows_page_create_rules_for_its_field(): void
    {
        $refused = [
            ['text', '<b>Autumn sale</b>'],
            ['text', 'a [b] c'],
            ['text', ''],
            ['text', '   '],
            ['text', "two\nlines"],
            ['text', '{{ price }}'],
            ['alt', 'Our team [1]'],
            ['alt', 'Fish &amp; chips'],
            ['alt', str_repeat('a', 301)],
            ['caption', ''],
            ['caption', str_repeat('a', 501)],
            ['url', 'javascript:alert(1)'],
            ['url', 'http://example.com/'],
            ['url', '//example.com/'],
            ['url', '/a:b'],
            ['url', ''],
            ['title', 'Hello'],
        ];
        foreach ($refused as [$field, $text]) {
            $r = PageEditValidator::parse(self::input([self::setText('3c4d5e6', $field, $text)]));
            $this->assertSame('bad_input', $r['code'] ?? null, $field . ' ' . $text);
        }
        $accepted = [
            ['text', 'Autumn sale [1]'],
            ['alt', ''],
            ['alt', str_repeat('a', 300)],
            ['caption', str_repeat('é', 500)],
            ['url', 'https://example.com/sale?a=1&b=2'],
            ['url', '/about'],
        ];
        foreach ($accepted as [$field, $text]) {
            $r = PageEditValidator::parse(self::input([self::setText('3c4d5e6', $field, $text)]));
            $this->assertArrayHasKey('input', $r, $field . ' ' . $text . ': ' . ($r['detail'] ?? ''));
            $this->assertSame($text, $r['input']['operations'][0]['text']);
        }
    }

    public function test_outline_goes_through_page_create(): void
    {
        $bad = [
            [['type' => 'heading', 'level' => 5, 'text' => 'Hi'], 'operations[0].outline[0].level'],
            [self::para('<b>Hi</b>'), 'operations[0].outline[0].text: '],
            [['type' => 'video', 'url' => 'https://example.com/'], 'operations[0].outline[0] is not a known block type'],
        ];
        foreach ($bad as [$node, $detail]) {
            $r = PageEditValidator::parse(self::input([self::insertAt('after', '4d5e6f7', [$node])]));
            $this->assertSame('bad_input', $r['code'] ?? null, $detail);
            $this->assertStringStartsWith($detail, $r['detail'] ?? '');
        }
        $images = array_fill(0, 21, ['type' => 'image', 'attachment_id' => 7, 'alt' => 'A']);
        $r      = PageEditValidator::parse(self::input([self::insertAt('after', '4d5e6f7', $images)]));
        $this->assertSame('bad_input', $r['code'] ?? null);
        $this->assertSame('operations[0].outline: the page has more than 20 images', $r['detail'] ?? null);

        $r = PageEditValidator::parse(self::input([
            ['op' => 'replace', 'ref' => '5e6f7a8', 'outline' => [['type' => 'image', 'attachment_id' => 7, 'alt' => 'A']]],
            self::insertAt('into', '2b3c4d5', [self::para('Hi')], 'first'),
        ]));
        $this->assertArrayHasKey('input', $r, $r['detail'] ?? '');
        $this->assertSame(418, $r['input']['post_id']);
        $this->assertSame(self::FP, $r['input']['base_fingerprint']);
        $this->assertSame([
            ['op' => 'replace', 'ref' => '5e6f7a8', 'outline' => [['type' => 'image', 'attachment_id' => 7, 'alt' => 'A', 'caption' => null, 'align' => 'none']]],
            ['op' => 'insert', 'into' => '2b3c4d5', 'position' => 'first', 'outline' => [['type' => 'paragraph', 'text' => 'Hi']]],
        ], $r['input']['operations'], 'outlines come back in page-create\'s normalised form');
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * What the agent answers for an input text: parse(), then againstPage().
     *
     * @param list<array<string,mixed>> $nodes Page nodes.
     */
    private static function agentAnswer(string $text, array $nodes, string $builder): string
    {
        $r = PageEditValidator::parse($text);
        if (!isset($r['input'])) {
            return (string) ($r['code'] ?? '');
        }
        $refused = PageEditValidator::againstPage($r['input'], $nodes, self::capabilities($builder));

        return $refused === null ? 'ok' : $refused['code'];
    }

    /**
     * The operations a builder declares: Elementor all five, any other no move.
     *
     * @return array{operations: list<string>, node_kinds: list<string>}
     */
    private static function capabilities(string $builder): array
    {
        $ops = BuilderContract::OPS;
        if ($builder !== 'elementor') {
            $ops = array_values(array_diff($ops, ['move']));
        }

        return ['operations' => $ops, 'node_kinds' => BuilderContract::KINDS];
    }

    /**
     * parse() must accept; then againstPage() on the fixture page (or $nodes).
     *
     * @param list<array<string,mixed>>      $ops          Operations.
     * @param list<array<string,mixed>>|null $nodes        Page nodes; the fixture page when null.
     * @param array<string,mixed>|null       $capabilities Capabilities; Elementor's when null.
     * @return array{code: string, detail: string, op_index: int|null}|null
     */
    private static function check(array $ops, ?array $nodes = null, ?array $capabilities = null): ?array
    {
        $r = PageEditValidator::parse(self::input($ops));
        self::assertArrayHasKey('input', $r, 'parse() refused: ' . ($r['detail'] ?? ''));

        return PageEditValidator::againstPage($r['input'], $nodes ?? self::opsCases()['page']['nodes'], $capabilities ?? self::capabilities('elementor'));
    }

    /**
     * The fixture page without its one locked node.
     *
     * @return list<array<string,mixed>>
     */
    private static function pageWithoutLocked(): array
    {
        return array_values(array_filter(self::opsCases()['page']['nodes'], static fn (array $n): bool => $n['kind'] !== 'locked'));
    }

    /**
     * @param list<array<string,mixed>>      $ops          Operations.
     * @param list<array<string,mixed>>|null $nodes        Page nodes.
     * @param array<string,mixed>|null       $capabilities Capabilities.
     */
    private function assertRefused(string $code, string $detail, ?int $opIndex, array $ops, ?array $nodes = null, ?array $capabilities = null): void
    {
        $this->assertSame(['code' => $code, 'detail' => $detail, 'op_index' => $opIndex], self::check($ops, $nodes, $capabilities));
    }

    /**
     * @param list<array<string,mixed>> $ops Operations.
     */
    private static function input(array $ops): string
    {
        return json_encode(['post_id' => 418, 'base_fingerprint' => self::FP, 'operations' => $ops], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    }

    /**
     * @return array<string,string>
     */
    private static function setText(string $ref, string $field, string $text): array
    {
        return ['op' => 'set_text', 'ref' => $ref, 'field' => $field, 'text' => $text];
    }

    /**
     * @param list<array<string,mixed>> $outline Outline.
     * @return array<string,mixed>
     */
    private static function insertAt(string $how, string $ref, array $outline, ?string $position = null): array
    {
        $op = ['op' => 'insert', $how => $ref];
        if ($position !== null) {
            $op['position'] = $position;
        }
        $op['outline'] = $outline;

        return $op;
    }

    /**
     * @return array<string,string>
     */
    private static function para(string $text): array
    {
        return ['type' => 'paragraph', 'text' => $text];
    }

    /**
     * The exact input text of a case, generated when the case says how.
     *
     * @param array<string,mixed> $case Case.
     */
    private static function inputText(array $case): string
    {
        $text = (string) $case['input'];
        if (!isset($case['generate'])) {
            return $text;
        }

        return $text . str_repeat((string) $case['generate']['append'], (int) $case['generate']['to_bytes'] - strlen($text));
    }

    /**
     * @return array<string,mixed>
     */
    private static function opsCases(): array
    {
        $raw = file_get_contents(self::FIXTURE);
        self::assertIsString($raw, 'page-edit-ops-cases.json is missing');
        $doc = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        self::assertIsArray($doc);

        return $doc;
    }
}
