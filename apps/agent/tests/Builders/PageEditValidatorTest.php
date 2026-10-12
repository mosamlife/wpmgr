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
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\PageEditValidator
 */
final class PageEditValidatorTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-edit-ops-cases.json';

    private const FP = '7787514f2667f5577151c00759c33e4553edc1d75f96e0dc1ddcdc23989a5ea7';

    /**
     * Every rule the version 3 rows exercise, with the [agent, go] verdicts
     * the rows must show. A rule with a refusing verdict shows its accepted
     * side as well when [ok, accept] is listed.
     */
    private const V3_RULES = [
        'pattern_path_set_text'           => [['ok', 'accept']],
        'pattern_path_in_editable'        => [['node_not_editable', 'accept']],
        'pattern_path_syntax'             => [['bad_input', 'refuse']],
        'pattern_path_text_rule'          => [['bad_input', 'refuse']],
        'pattern_path_button_text_rule'   => [['ok', 'accept'], ['bad_input', 'refuse']],
        'pattern_path_link_rule'          => [['bad_input', 'refuse']],
        'link_anchor_syntax'              => [['bad_input', 'refuse']],
        'link_anchor_resolves_after_call' => [['ok', 'accept'], ['link_anchor_missing', 'accept']],
        'anchor_in_use'                   => [['ok', 'accept'], ['anchor_in_use', 'accept']],
        'anchor_unique_after_call'        => [['ok', 'accept'], ['anchor_duplicate', 'accept']],
        'v3_insert_valid'                 => [['ok', 'accept']],
        'section_at_top_only'             => [['node_not_editable', 'accept']],
        'hero_first_on_the_page'          => [['hero_misplaced', 'accept'], ['ok', 'accept']],
        'h1_once_on_the_page'             => [['layout_invalid', 'accept'], ['ok', 'accept']],
        'v3_grammar_in_outline'           => [['bad_input', 'refuse']],
    ];

    /** The rules judged on the page as the whole call leaves it. */
    private const V3_AFTER_THE_CALL = ['anchor_in_use', 'anchor_unique_after_call', 'link_anchor_resolves_after_call'];

    private const V3_AGENT_VERDICTS = [
        'ok', 'bad_input', 'ops_invalid', 'node_not_found', 'node_not_editable', 'op_not_supported_by_builder',
        'page_too_large', 'anchor_in_use', 'link_anchor_missing', 'anchor_duplicate', 'layout_invalid', 'hero_misplaced',
    ];

    private const V3_CASE_KEYS = ['name', 'rule', 'note', 'input', 'agent', 'go'];

    // -------------------------------------------------------------------------
    // The shared cases
    // -------------------------------------------------------------------------

    public function test_replays_every_shared_case(): void
    {
        $doc = self::opsCases();
        $this->assertNotEmpty($doc['cases']);
        $this->assertNotEmpty($doc['v3_cases']);
        foreach (self::allCases() as [$page, $case]) {
            $builder = $case['builder'] ?? $page['builder'];
            $this->assertSame($case['agent'], self::agentAnswer(self::inputText($case), $page['nodes'], $builder), $case['name']);
        }
    }

    public function test_parse_refuses_exactly_the_cases_the_control_plane_refuses(): void
    {
        foreach (self::allCases() as [, $case]) {
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

    public function test_a_designed_insert_with_an_ampersand_in_a_link_is_bad_input(): void
    {
        $plans = static fn (string $url): array => ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => [
            ['name' => 'Team', 'price' => '49', 'features' => ['5 sites'], 'cta' => ['text' => 'Choose', 'url' => $url]],
        ]];
        $hero  = static fn (string $url): array => ['type' => 'section', 'anchor' => 'top', 'children' => [
            ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => [
                ['type' => 'heading', 'level' => 1, 'text' => 'Title'],
                ['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => $url]]],
            ]],
        ]];

        foreach (['the button of a plan' => $plans, 'the button of a hero' => $hero] as $what => $make) {
            $r = PageEditValidator::parse(self::input([self::insertAt('after', '4d5e6f7', [$make('/signup?plan=pro&seats=5')])]));
            $this->assertSame('bad_input', $r['code'] ?? null, $what);
            $this->assertStringStartsWith('operations[0].outline', $r['detail'] ?? '', $what);
            $this->assertStringContainsString('"&"', $r['detail'] ?? '', $what . ': the refusal names the character');
            $this->assertStringNotContainsString('seats', $r['detail'] ?? '', $what . ': the refusal never echoes the link');

            $ok = PageEditValidator::parse(self::input([self::insertAt('after', '4d5e6f7', [$make('/signup?plan=pro')])]));
            $this->assertArrayHasKey('input', $ok, $what . ': ' . ($ok['detail'] ?? ''));
        }
    }

    // -------------------------------------------------------------------------
    // Version 3 rows (v3_page, v3_cases)
    // -------------------------------------------------------------------------

    public function test_v3_cases_are_well_formed(): void
    {
        $doc = self::opsCases();
        $this->assertArrayHasKey('v3_page', $doc, 'the version 3 rows are missing');
        $this->assertArrayHasKey('v3_cases', $doc, 'the version 3 rows are missing');
        $page = $doc['v3_page'];
        $this->assertIsInt($page['post_id']);
        $this->assertSame('elementor', $page['builder']);
        $this->assertMatchesRegularExpression('/^[0-9a-f]{64}$/D', $page['base_fingerprint']);

        $refs    = [];
        $anchors = [];
        $links   = [];
        foreach ($page['nodes'] as $node) {
            $ref = $node['ref'];
            $this->assertMatchesRegularExpression(BuilderContract::RE_REF, $ref);
            $this->assertNotContains($ref, $refs, 'page refs are unique');
            $this->assertTrue($node['parent'] === 'root' || in_array($node['parent'], $refs, true), $ref . ': parent precedes the node');
            $this->assertContains($node['kind'], BuilderContract::KINDS);
            $this->assertNotSame('locked', $node['kind'], 'the designed page has no locked node');
            foreach ($node['editable'] as $field) {
                if ($node['kind'] === 'pattern') {
                    $this->assertMatchesRegularExpression('/' . OutlineV3Schema::EDIT_PATH_PATTERN . '/D', $field, $ref);
                } else {
                    $this->assertContains($field, BuilderContract::FIELDS, $ref);
                }
            }
            if (isset($node['anchor'])) {
                $this->assertSame('section', $node['kind'], $ref . ': only a section has an anchor');
                $this->assertSame('root', $node['parent'], $ref . ': a section is at the top level');
                $this->assertMatchesRegularExpression('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', $node['anchor']);
                $this->assertNotContains($node['anchor'], OutlineV3Schema::RESERVED_ANCHORS);
                $this->assertNotContains($node['anchor'], $anchors, 'page anchors are unique');
                $anchors[] = $node['anchor'];
            }
            foreach ($node['links'] ?? [] as $link) {
                $this->assertMatchesRegularExpression('/^#[a-z][a-z0-9-]{1,39}$/D', $link, $ref);
                $links[] = substr($link, 1);
            }
            $refs[] = $ref;
        }
        $this->assertSame([], array_values(array_diff($links, $anchors)), 'the designed page has no dangling link');

        $names = array_column($doc['cases'], 'name');
        $seen  = [];
        $this->assertNotEmpty($doc['v3_cases']);
        foreach ($doc['v3_cases'] as $case) {
            $name = (string) ($case['name'] ?? '');
            $this->assertNotSame('', $name);
            $this->assertNotContains($name, $names, 'case names are unique');
            $names[] = $name;
            $this->assertSame([], array_values(array_diff(array_keys($case), self::V3_CASE_KEYS)), $name . ': unknown case field');
            $this->assertArrayHasKey($case['rule'], self::V3_RULES, $name . ': unknown rule');
            $this->assertContains($case['agent'], self::V3_AGENT_VERDICTS, $name);
            $this->assertContains($case['go'], ['accept', 'refuse'], $name);
            if (isset($case['note'])) {
                $this->assertNotSame('', $case['note'], $name . ': a note says what the case shows');
            }
            if ($case['agent'] === 'ok') {
                $this->assertSame('accept', $case['go'], $name . ': what the agent accepts, the control plane accepts');
            }
            $seen[$case['rule']][] = [$case['agent'], $case['go']];

            $text = (string) $case['input'];
            $this->assertLessThanOrEqual(BuilderContract::MAX_INPUT_BYTES, strlen($text), $name);
            $input = json_decode($text, false, 32);
            $this->assertIsObject($input, $name . ': input is a JSON object');
            $this->assertSame(['post_id', 'base_fingerprint', 'operations'], array_keys(get_object_vars($input)), $name);
            $this->assertSame($page['post_id'], $input->post_id, $name);
            $this->assertSame($page['base_fingerprint'], $input->base_fingerprint, $name);
            $this->assertIsArray($input->operations, $name);
            $this->assertGreaterThanOrEqual(1, count($input->operations), $name);
            $this->assertLessThanOrEqual(BuilderContract::MAX_OPS, count($input->operations), $name);
            $targets = [];
            foreach ($input->operations as $op) {
                $this->assertContains($op->op, BuilderContract::OPS, $name);
                foreach (['ref', 'after', 'before', 'into'] as $field) {
                    if (isset($op->{$field})) {
                        $this->assertContains($op->{$field}, $refs, $name . ': a case names nodes the page has');
                        $this->assertNotContains($op->{$field}, $targets, $name . ': a call names each node once');
                        $targets[] = $op->{$field};
                    }
                }
            }
        }
        foreach (self::V3_RULES as $rule => $verdicts) {
            $this->assertArrayHasKey($rule, $seen, 'no case exercises ' . $rule);
            foreach ($verdicts as $verdict) {
                $this->assertContains($verdict, $seen[$rule], $rule . ' has no case answering ' . implode('/', $verdict));
            }
        }
        foreach (self::V3_AFTER_THE_CALL as $rule) {
            $this->assertGreaterThanOrEqual(
                2,
                count(array_filter($doc['v3_cases'], static fn (array $c): bool => $c['rule'] === $rule && count(json_decode((string) $c['input'], true)['operations']) > 1)),
                $rule . ' needs rows of more than one operation, to show the call is judged as a whole'
            );
        }
    }

    /**
     * Every row of v3_cases, with what the agent answers: pattern paths,
     * version 3 nodes and the anchor rules, all together.
     */
    public function test_replays_every_v3_case(): void
    {
        $v3    = self::v3Cases();
        $wrong = [];
        foreach ($v3['cases'] as $case) {
            $got = self::agentAnswer((string) $case['input'], $v3['page']['nodes'], $v3['page']['builder']);
            if ($got !== $case['agent']) {
                $wrong[$case['name']] = 'wants ' . $case['agent'] . ', answered ' . ($got === '' ? 'nothing' : $got);
            }
        }
        $this->assertSame([], $wrong, count($wrong) . ' of ' . count($v3['cases']) . ' version 3 rows are not answered as the table says');
    }

    /**
     * The rows the control plane refuses are refused by parse() with the
     * agent's code, and every other row of v3_cases passes parse().
     */
    public function test_parse_refuses_exactly_the_v3_cases_the_control_plane_refuses(): void
    {
        $wrong = [];
        foreach (self::v3Cases()['cases'] as $case) {
            $r = PageEditValidator::parse((string) $case['input']);
            if ($case['go'] === 'refuse') {
                if (($r['code'] ?? null) !== $case['agent']) {
                    $wrong[$case['name']] = 'wants ' . $case['agent'] . ' from parse(), got ' . ($r['code'] ?? 'an input');
                }
            } elseif (!isset($r['input'])) {
                $wrong[$case['name']] = 'parse() refused ' . ($r['detail'] ?? '');
            }
        }
        $this->assertSame([], $wrong);
    }

    /**
     * The anchor rules are judged on the page the whole call leaves: the order
     * of the operations never changes the answer.
     */
    public function test_anchor_and_link_rules_do_not_depend_on_operation_order(): void
    {
        $v3    = self::v3Cases();
        $wrong = [];
        $tried = 0;
        foreach ($v3['cases'] as $case) {
            if (!in_array($case['rule'], self::V3_AFTER_THE_CALL, true)) {
                continue;
            }
            $input = json_decode((string) $case['input'], true, 32);
            if (count($input['operations']) < 2) {
                continue;
            }
            ++$tried;
            $input['operations'] = array_reverse($input['operations']);
            $text                = json_encode($input, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
            $got                 = self::agentAnswer($text, $v3['page']['nodes'], $v3['page']['builder']);
            if ($got !== $case['agent']) {
                $wrong[$case['name']] = 'in reverse order wants ' . $case['agent'] . ', answered ' . ($got === '' ? 'nothing' : $got);
            }
        }
        $this->assertGreaterThanOrEqual(6, $tried, 'the rows of more than one operation');
        $this->assertSame([], $wrong);
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
     * The designed page's projection and the rows that run against it.
     *
     * @return array{page: array<string,mixed>, cases: list<array<string,mixed>>}
     */
    private static function v3Cases(): array
    {
        $doc = self::opsCases();
        self::assertArrayHasKey('v3_page', $doc, 'the version 3 rows are missing');
        self::assertArrayHasKey('v3_cases', $doc, 'the version 3 rows are missing');

        return ['page' => $doc['v3_page'], 'cases' => $doc['v3_cases']];
    }

    /**
     * Every row of both tables with the page it runs against.
     *
     * @return list<array{0: array<string,mixed>, 1: array<string,mixed>}>
     */
    private static function allCases(): array
    {
        $doc  = self::opsCases();
        $rows = [];
        foreach ($doc['cases'] as $case) {
            $rows[] = [$doc['page'], $case];
        }
        foreach ($doc['v3_cases'] as $case) {
            $rows[] = [$doc['v3_page'], $case];
        }

        return $rows;
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
