<?php
/**
 * LayoutOps: page-edit operations on a stored Elementor classic tree, in
 * containers and in sections, and the adapter's planEdit over the stored row.
 *
 * Pages are built by ElementorClassicMapper::map() as page-create builds them,
 * then given styling a person set and a widget WPMgr does not edit. Every
 * operation goes through PageEditValidator::parse() first, as in production.
 * The HTML sanitiser is the same double as in ElementorClassicMapperTest.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   elementor-edit-cases.json  before tree, operations, after tree and changes
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LayoutOps;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\LayoutOps
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorAdapter
 */
final class LayoutOpsTest extends TestCase
{
    private const FIXTURES = __DIR__ . '/../fixtures/ability-run/';

    private const EDIT_CASES = self::FIXTURES . 'elementor-edit-cases.json';

    /** The request that created the page. */
    private const PAGE_REQUEST = '11111111-2222-4333-8444-777777777777';

    /** The page-edit request. */
    private const EDIT = '22222222-3333-4444-8555-888888888888';

    private const POST_ID = 418;

    /** The page every case starts from, as page-create's outline. */
    private const OUTLINE = '[{"type":"heading","level":2,"text":"Spring sale"},{"type":"paragraph","text":"Open every day."},'
        . '{"type":"columns","columns":[{"children":[{"type":"paragraph","text":"One"}]},{"children":[{"type":"buttons","buttons":[{"text":"Call us","url":"/contact"}]}]}]},'
        . '{"type":"group","children":[{"type":"heading","level":3,"text":"Our team"},{"type":"image","attachment_id":7,"alt":"","caption":"Team photo"}]}]';

    /** Styling a person set on the first heading, outside WPMgr's keys. */
    private const PERSON_COLOR = '#c0392b';

    /** Tags the sanitiser double keeps. */
    private const KEPT_TAGS = ['p', 'h2', 'h3', 'h4', 'ul', 'ol', 'li', 'blockquote', 'cite', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'strong', 'b', 'a', 'img'];

    private const MEDIA = [
        5 => ['url' => 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png', 'alt' => 'Library alt text'],
        7 => ['url' => 'https://example.com/wp-content/uploads/2026/10/team-1024x683.jpg', 'alt' => ''],
    ];

    private bool $hadWpdb = false;

    private mixed $savedWpdb = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('wp_kses_post')->alias(static fn ($s) => self::kses((string) $s));
        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // set_text
    // -------------------------------------------------------------------------

    public function test_set_text_keeps_every_other_setting(): void
    {
        foreach ([true, false] as $containers) {
            $tree = self::page($containers);
            $ref  = self::refs($tree, $containers);
            $r    = self::applied($tree, [
                self::setText($ref['h2'], 'text', 'Summer sale'),
                self::setText($ref['btn'], 'url', 'https://example.com/book'),
                self::setText($ref['img'], 'caption', 'The whole team'),
            ], $containers);

            $after = self::byId($r['tree']);
            $this->assertSame(['title' => 'Summer sale', 'header_size' => 'h2', 'title_color' => self::PERSON_COLOR], $after[$ref['h2']]['settings'], 'the person\'s colour stays, in its place');
            $this->assertSame(['url' => 'https://example.com/book', 'is_external' => '', 'nofollow' => ''], $after[$ref['btn']]['settings']['link'], 'only link.url changes');
            $this->assertSame('Call us', $after[$ref['btn']]['settings']['text']);
            $this->assertSame('custom', $after[$ref['img']]['settings']['caption_source']);
            $this->assertSame('The whole team', $after[$ref['img']]['settings']['caption']);
            $this->assertSame(self::page($containers)[0]['id'], $r['tree'][0]['id']);
            $this->assertSame([$ref['h2'], $ref['btn'], $ref['img']], $r['touched']);
            $this->assertSame(0, $r['new_count']);
            $this->assertSame(['op' => 'set_text', 'ref' => $ref['h2'], 'kind' => 'heading', 'level' => 2, 'before' => ['text' => 'Spring sale'], 'after' => ['text' => 'Summer sale']], $r['changes'][0]);
            $this->assertSame(['url' => '/contact'], $r['changes'][1]['before']);
        }
    }

    public function test_set_text_escapes_like_create(): void
    {
        $text = 'Fish & chips [1] café &lt;b&gt;';
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $r    = self::applied($tree, [
            self::setText($ref['h2'], 'text', $text),
            self::setText($ref['p1'], 'text', $text),
            self::setText($ref['btn'], 'text', $text),
            self::setText($ref['img'], 'caption', $text),
        ], true);
        $after = self::byId($r['tree']);

        $created = self::created('[{"type":"heading","level":2,"text":' . json_encode($text) . '},{"type":"paragraph","text":' . json_encode($text) . '},'
            . '{"type":"buttons","buttons":[{"text":' . json_encode($text) . ',"url":"/"}]},{"type":"image","attachment_id":7,"alt":"","caption":' . json_encode($text) . '}]');
        $widgets = $created[0]['elements'];
        $this->assertSame('Fish &amp; chips &#091;1&#093; café &amp;lt;b&amp;gt;', $widgets[0]['settings']['title']);
        $this->assertSame($widgets[0]['settings']['title'], $after[$ref['h2']]['settings']['title']);
        $this->assertSame($widgets[1]['settings']['editor'], $after[$ref['p1']]['settings']['editor']);
        $this->assertSame($widgets[2]['settings']['text'], $after[$ref['btn']]['settings']['text']);
        $this->assertSame($widgets[3]['settings']['caption'], $after[$ref['img']]['settings']['caption']);
        $this->assertSame(['text' => $text], $r['changes'][0]['after'], 'the change shows the text as the AI wrote it');
    }

    public function test_set_text_keeps_create_rules(): void
    {
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $this->assertRefused('link_invalid', 0, $tree, [self::setText($ref['btn'], 'url', '/shop?a=1&b=2')], true);
        $this->assertRefused('create_content_invalid', 0, $tree, [self::setText($ref['p1'], 'text', 'https://example.com/video')], true);
        $this->assertRefused('leaf_unsafe', 0, $tree, [self::setText($ref['h2'], 'text', 'Go javascript:alert(1)')], true);
        $this->assertRefused('node_not_editable', 0, $tree, [self::setText($ref['img'], 'alt', 'Team')], true, 'field_not_offered');
        $this->assertRefused('node_not_editable', 1, $tree, [self::setText($ref['h2'], 'text', 'Fine'), self::setText($ref['lock'], 'text', 'Hi')], true, 'locked');

        // Refused at its own place: nothing is half-applied.
        $direct = ElementorClassicMapper::setText(self::byId($tree)[$ref['btn']], 'buttons', 'url', 'javascript:alert(1)', 'operations[0]');
        $this->assertSame('bad_input', $direct['code']);
    }

    // -------------------------------------------------------------------------
    // insert, replace, remove, move
    // -------------------------------------------------------------------------

    public function test_insert_after_before_into_first_last(): void
    {
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $r    = self::applied($tree, [
            self::insert(['after' => $ref['h2']], [self::para('A')]),
            self::insert(['before' => $ref['row']], [self::para('B')]),
            self::insert(['into' => $ref['top2'], 'position' => 'first'], [self::para('C')]),
            self::insert(['into' => $ref['col1']], [self::para('D')]),
        ], true);
        $t = $r['tree'];

        $this->assertSame([$ref['h2'], $r['changes'][0]['new_refs'][0], $ref['p1']], array_column($t[0]['elements'], 'id'), 'after the heading, inside its group: a widget, no wrapper');
        $this->assertCount(4, $t, 'before a top-level row: one new top-level container');
        $this->assertSame($ref['row'], $t[2]['id']);
        $this->assertSame(['content_width' => 'boxed', 'flex_direction' => 'column'], $t[1]['settings']);
        $this->assertSame('<p>B</p>', $t[1]['elements'][0]['settings']['editor']);
        $this->assertSame([$r['changes'][2]['new_refs'][0], $ref['h3'], $ref['img'], $ref['lock']], array_column($t[3]['elements'], 'id'), 'into a group, first');
        $this->assertSame([$ref['one'], $r['changes'][3]['new_refs'][0]], array_column($t[2]['elements'][0]['elements'], 'id'), 'into a column, last when no position is given');
        $this->assertSame(['ref' => $ref['col1'], 'how' => 'into', 'position' => 'last', 'kind' => 'column'], $r['changes'][3]['anchor']);
        $this->assertSame(['ref' => $ref['row'], 'how' => 'before', 'kind' => 'columns'], $r['changes'][1]['anchor']);
        $this->assertSame(5, $r['new_count']);

        // Sections: before a top-level section is a new section and column.
        $tree = self::page(false);
        $ref  = self::refs($tree, false);
        $r    = self::applied($tree, [self::insert(['before' => $ref['row']], [self::para('B')])], false);
        $this->assertSame(['section', 'column', 'widget'], [$r['tree'][1]['elType'], $r['tree'][1]['elements'][0]['elType'], $r['tree'][1]['elements'][0]['elements'][0]['elType']]);
        $this->assertSame(3, $r['new_count']);
    }

    public function test_placement_follows_the_page(): void
    {
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $this->assertRefused('layout_invalid', 0, $tree, [self::insert(['into' => $ref['row']], [self::para('X')])], true);
        $this->assertRefused('layout_invalid', 0, $tree, [self::insert(['into' => $ref['top0']], [['type' => 'group', 'children' => [self::para('X')]]])], true);
        $this->assertRefused('layout_invalid', 0, $tree, [self::insert(['into' => $ref['col1']], [self::cols()])], true);
        $this->assertRefused('layout_invalid', 0, $tree, [['op' => 'replace', 'ref' => $ref['col1'], 'outline' => [self::para('X')]]], true);

        // Columns inside a top-level group: an inner row of column containers.
        $r   = self::applied($tree, [self::insert(['into' => $ref['top0']], [self::cols()])], true);
        $row = $r['tree'][0]['elements'][2];
        $this->assertSame(['container', true, 'full', 'row'], [$row['elType'], $row['isInner'], $row['settings']['content_width'], $row['settings']['flex_direction']]);

        $tree = self::page(false);
        $ref  = self::refs($tree, false);
        $this->assertRefused('layout_invalid', 0, $tree, [self::insert(['into' => $ref['top0']], [self::para('X')])], false);
        // Columns inside a top-level section's column: an inner section, even on a site with containers on.
        $r     = self::applied($tree, [self::insert(['into' => $ref['box0']], [self::cols()])], true);
        $inner = $r['tree'][0]['elements'][0]['elements'][2];
        $this->assertSame(['section', true, '20'], [$inner['elType'], $inner['isInner'], $inner['settings']['structure']]);
        $this->assertSame([true, true], array_column($inner['elements'], 'isInner'));
        $this->assertSame('section', self::applied($tree, [self::insert(['into' => $ref['col1']], [self::cols()])], false)['tree'][1]['elements'][0]['elements'][1]['elType'], 'any top-level section\'s column takes an inner section');
        // An inner section's column takes leaves only.
        $innerColumn = $inner['elements'][0]['id'];
        $this->assertRefused('layout_invalid', 0, $r['tree'], [self::insert(['into' => $innerColumn], [self::cols()])], false, null, self::PAGE_REQUEST);
        $this->assertSame('widget', self::applied($r['tree'], [self::insert(['into' => $innerColumn], [self::para('X')])], false, self::PAGE_REQUEST)['tree'][0]['elements'][0]['elements'][2]['elements'][0]['elements'][1]['elType']);
    }

    public function test_replace_keeps_position_and_takes_new_ids(): void
    {
        foreach ([true, false] as $containers) {
            $tree    = self::page($containers);
            $ref     = self::refs($tree, $containers);
            $pageIds = self::ids($tree);
            $r       = self::applied($tree, [
                ['op' => 'replace', 'ref' => $ref['p1'], 'outline' => [['type' => 'list', 'ordered' => false, 'items' => ['Tea', 'Cake']]]],
                ['op' => 'replace', 'ref' => $ref['row'], 'outline' => [self::para('Row gone')]],
            ], $containers);

            $box = self::byId($r['tree'])[$ref['box0']];
            $this->assertSame($ref['h2'], $box['elements'][0]['id']);
            $this->assertSame('<ul><li>Tea</li><li>Cake</li></ul>', $box['elements'][1]['settings']['editor'], 'the list stands where the paragraph stood');
            $this->assertCount(2, $box['elements']);
            $this->assertSame([$ref['top0'], $r['changes'][1]['new_refs'][0], $ref['top2']], array_column($r['tree'], 'id'), 'the new top-level element stands where the row stood');
            $this->assertArrayNotHasKey($ref['p1'], self::byId($r['tree']));
            foreach ($r['changes'] as $change) {
                foreach ($change['new_refs'] as $id) {
                    $this->assertNotContains($id, $pageIds, 'a new id is never one on the page');
                }
            }
            $this->assertSame(['text' => 'Open every day.'], $r['changes'][0]['before']);
            $this->assertArrayNotHasKey('before', $r['changes'][1], 'a row holds no text of its own');
        }
    }

    public function test_remove_drops_the_subtree(): void
    {
        $tree   = self::page(true);
        $ref    = self::refs($tree, true);
        $before = self::elementCount($tree);
        $r      = self::applied($tree, [['op' => 'remove', 'ref' => $ref['row']], ['op' => 'remove', 'ref' => $ref['img']]], true);

        $this->assertSame($before - 6, self::elementCount($r['tree']), 'the row, its two columns and their two widgets, and the image');
        foreach (['row', 'col1', 'one', 'col2', 'btn', 'img'] as $name) {
            $this->assertArrayNotHasKey($ref[$name], self::byId($r['tree']), $name);
        }
        $this->assertSame(['op' => 'remove', 'ref' => $ref['img'], 'kind' => 'image', 'before' => ['caption' => 'Team photo']], $r['changes'][1]);
        $this->assertSame([], $r['touched']);
    }

    public function test_locked_node_never_goes_with_its_holder(): void
    {
        foreach ([true, false] as $containers) {
            $tree = self::page($containers);
            $ref  = self::refs($tree, $containers);
            // top2 holds the locked widget: directly as a container, or
            // through its column box2 as a section.
            foreach (array_unique([$ref['top2'], $ref['box2']]) as $holder) {
                $this->assertRefused('node_not_editable', 0, $tree, [['op' => 'remove', 'ref' => $holder]], $containers, 'holds_locked');
                $this->assertRefused('node_not_editable', 0, $tree, [['op' => 'replace', 'ref' => $holder, 'outline' => [self::para('Gone')]]], $containers, 'holds_locked');
            }
            $this->assertRefused('node_not_editable', 1, $tree, [self::setText($ref['h3'], 'text', 'Fine'), ['op' => 'remove', 'ref' => $ref['top2']]], $containers, 'holds_locked');

            // A holder may still move, and the lock goes with it unchanged.
            $r = self::applied($tree, [['op' => 'move', 'ref' => $ref['top2'], 'before' => $ref['top0']]], $containers);
            $this->assertSame(json_encode(self::byId($tree)[$ref['lock']]), json_encode(self::byId($r['tree'])[$ref['lock']]));
        }

        // Judged on the tree the earlier operations left: the column holding
        // the locked widget moves into the row, so the row now holds it.
        $tree = self::page(false);
        $ref  = self::refs($tree, false);
        $move = ['op' => 'move', 'ref' => $ref['box2'], 'before' => $ref['col1']];
        $this->assertRefused('node_not_editable', 1, $tree, [$move, ['op' => 'remove', 'ref' => $ref['row']]], false, 'holds_locked');
        $r = self::applied($tree, [$move, ['op' => 'remove', 'ref' => $ref['top2']]], false);
        $this->assertArrayHasKey($ref['lock'], self::byId($r['tree']), 'the section the lock left may go; the lock stays');
        $this->assertArrayNotHasKey($ref['top2'], self::byId($r['tree']));
    }

    public function test_move_keeps_node_bytes(): void
    {
        foreach ([true, false] as $containers) {
            $tree = self::page($containers);
            $ref  = self::refs($tree, $containers);
            $was  = self::byId($tree);
            $r    = self::applied($tree, [
                ['op' => 'move', 'ref' => $ref['top2'], 'before' => $ref['top0']],
                ['op' => 'move', 'ref' => $ref['btn'], 'after' => $ref['h2']],
                ['op' => 'move', 'ref' => $ref['col2'], 'before' => $ref['col1']],
            ], $containers);
            $now = self::byId($r['tree']);

            $this->assertSame([$ref['top2'], $ref['top0'], $ref['row']], array_column($r['tree'], 'id'));
            $this->assertSame(json_encode($was[$ref['top2']]), json_encode($now[$ref['top2']]), 'a moved node keeps every byte, its insides too');
            $this->assertSame(json_encode($was[$ref['btn']]), json_encode($now[$ref['btn']]));
            $this->assertSame([$ref['h2'], $ref['btn'], $ref['p1']], array_column($now[$ref['box0']]['elements'], 'id'));
            $this->assertSame([$ref['col2'], $ref['col1']], array_column($now[$ref['row']]['elements'], 'id'), 'columns reorder within their row');
            $this->assertSame(['ref' => $ref['h2'], 'how' => 'after', 'kind' => 'heading', 'level' => 2], $r['changes'][1]['anchor']);
            $this->assertSame(0, $r['new_count']);
        }
    }

    public function test_move_only_among_the_same_place(): void
    {
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $this->assertRefused('layout_invalid', 0, $tree, [['op' => 'move', 'ref' => $ref['h2'], 'before' => $ref['row']]], true);
        $this->assertRefused('layout_invalid', 0, $tree, [['op' => 'move', 'ref' => $ref['col1'], 'after' => $ref['h3']]], true);
        $this->assertRefused('layout_invalid', 0, $tree, [['op' => 'move', 'ref' => $ref['top2'], 'after' => $ref['h2']]], true);
        $this->assertRefused('node_not_editable', 0, $tree, [['op' => 'move', 'ref' => $ref['top2'], 'before' => $ref['h3']]], true, 'into_own_subtree');
        $this->assertRefused('node_not_editable', 0, $tree, [['op' => 'move', 'ref' => $ref['lock'], 'before' => $ref['h3']]], true, 'locked');

        // An inner row moves only where a new one may go.
        $nested = self::applied($tree, [self::insert(['into' => $ref['top0']], [self::cols()])], true)['tree'];
        $inner  = $nested[0]['elements'][2]['id'];
        $this->assertRefused('layout_invalid', 0, $nested, [['op' => 'move', 'ref' => $inner, 'after' => $ref['one']]], true, null, self::PAGE_REQUEST);
        $moved = self::applied($nested, [['op' => 'move', 'ref' => $inner, 'after' => $ref['h3']]], true, self::PAGE_REQUEST);
        $this->assertSame($inner, $moved['tree'][2]['elements'][1]['id']);

        // A locked node may anchor a move.
        $r = self::applied($tree, [['op' => 'move', 'ref' => $ref['h3'], 'after' => $ref['lock']]], true);
        $this->assertSame([$ref['img'], $ref['lock'], $ref['h3']], array_column($r['tree'][2]['elements'], 'id'));
        $this->assertSame(['ref' => $ref['lock'], 'how' => 'after', 'kind' => 'locked', 'label' => 'Elementor testimonial'], $r['changes'][0]['anchor']);
    }

    public function test_refs_resolve_only_among_projected_nodes(): void
    {
        $tree   = self::page(true);
        $ref    = self::refs($tree, true);
        $hidden = ['id' => 'c0ffee1', 'elType' => 'container', 'settings' => ['custom_css' => 'x'], 'elements' => [
            ['id' => 'c0ffee2', 'elType' => 'widget', 'settings' => ['title' => 'Inside'], 'elements' => [], 'widgetType' => 'heading'],
        ], 'isInner' => false];
        $tree[] = $hidden;

        $this->assertRefused('node_not_found', 0, $tree, [self::setText('0f0f0f0', 'text', 'Hi')], true, 'ref_not_on_page');
        $this->assertRefused('node_not_found', 0, $tree, [self::setText('c0ffee2', 'text', 'Hi')], true, 'ref_not_on_page');
        $this->assertRefused('ops_invalid', 1, $tree, [['op' => 'remove', 'ref' => $ref['top0']], self::setText($ref['h2'], 'text', 'Hi')], true, 'ref_gone');
        $this->assertRefused('node_not_editable', 0, $tree, [self::insert(['into' => $ref['h2']], [self::para('X')])], true, 'takes_no_children');
        $this->assertRefused('data_unreadable', null, [['id' => 'a', 'elements' => 'x']], [self::setText($ref['h2'], 'text', 'Hi')], true);
        $this->assertRefused('data_unreadable', null, [['id' => 'abc1234', 'elType' => 'widget', 'widgetType' => 'heading', 'settings' => ['title' => 'A'], 'elements' => []], ['id' => 'abc1234', 'elType' => 'widget', 'widgetType' => 'heading', 'settings' => ['title' => 'B'], 'elements' => []]], [self::setText('abc1234', 'text', 'Hi')], true);
    }

    // -------------------------------------------------------------------------
    // Bytes, ids, counts
    // -------------------------------------------------------------------------

    public function test_untouched_nodes_byte_identical(): void
    {
        foreach ([true, false] as $containers) {
            $tree = self::page($containers);
            $ref  = self::refs($tree, $containers);
            $r    = self::applied($tree, [
                self::setText($ref['h2'], 'text', 'Summer sale'),
                self::insert(['into' => $ref['box2']], [self::para('New')]),
                ['op' => 'remove', 'ref' => $ref['one']],
                ['op' => 'move', 'ref' => $ref['p1'], 'after' => $ref['h3']],
            ], $containers);

            $before = self::byId($tree);
            $after  = self::byId($r['tree']);
            $checked = 0;
            foreach ($before as $id => $node) {
                if (!isset($after[$id])) {
                    continue;
                }
                if ((string) $id !== $ref['h2']) {
                    $this->assertSame(json_encode(self::own($node)), json_encode(self::own($after[$id])), $id . ': its own keys and values');
                }
                if (self::ids([$node]) === self::ids([$after[$id]]) && !in_array((string) $id, [$ref['h2'], $ref['top0'], $ref['box0']], true)) {
                    $this->assertSame(json_encode($node), json_encode($after[$id]), $id . ': untouched, with everything inside it');
                    ++$checked;
                }
            }
            $this->assertGreaterThanOrEqual(6, $checked, 'the untouched nodes were compared');
            $this->assertSame(json_encode($before[$ref['lock']]), json_encode($after[$ref['lock']]), 'the locked widget keeps its own key order and values');
        }
    }

    public function test_new_ids_never_collide_with_page_ids(): void
    {
        $probe    = new IdSeed(self::EDIT);
        $wouldBe  = $probe->next('operations[0].outline[0]');
        $nextOne  = $probe->next('operations[1].outline[0]');
        $tree     = self::page(true);
        $ref      = self::refs($tree, true);
        // The ids the edit would take first: one hidden inside a locked element, one on a node the AI can see.
        $tree[]   = ['id' => 'c0ffee1', 'elType' => 'container', 'settings' => ['custom_css' => 'x'], 'elements' => [
            ['id' => $wouldBe, 'elType' => 'widget', 'settings' => ['title' => 'Inside'], 'elements' => [], 'widgetType' => 'heading'],
        ], 'isInner' => false];
        $tree[0]['elements'][1]['id'] = $nextOne;

        $r   = self::applied($tree, [self::insert(['into' => $ref['top2']], [self::para('A')]), self::insert(['into' => $ref['col1']], [self::para('B')])], true);
        $all = self::ids($r['tree']);
        $this->assertNotSame($wouldBe, $r['changes'][0]['new_refs'][0]);
        $this->assertNotSame($nextOne, $r['changes'][1]['new_refs'][0]);
        $this->assertSame(count($all), count(array_unique($all)), 'every id on the page after the edit is unique');
        foreach ($r['changes'] as $change) {
            $this->assertSame(7, strlen($change['new_refs'][0]));
        }
    }

    public function test_same_request_same_tree(): void
    {
        $tree = self::page(true);
        $ref  = self::refs($tree, true);
        $ops  = [
            self::insert(['after' => $ref['top0']], [self::para('A'), self::cols()]),
            ['op' => 'replace', 'ref' => $ref['p1'], 'outline' => [self::para('B')]],
        ];
        $first  = self::applied($tree, $ops, true);
        $second = self::applied($tree, $ops, true);
        $other  = self::applied($tree, $ops, true, '33333333-4444-4555-8666-999999999999');

        $this->assertSame(json_encode($first), json_encode($second), 'the same request, page and operations make the same bytes');
        $this->assertNotSame($first['changes'][0]['new_refs'], $other['changes'][0]['new_refs'], 'another request makes other ids');
        $this->assertSame(array_map('strlen', $first['changes'][0]['new_refs']), array_map('strlen', $other['changes'][0]['new_refs']));
    }

    public function test_create_goldens_unchanged(): void
    {
        foreach (['elementor-classic-containers.json' => true, 'elementor-classic-sections.json' => false] as $file => $containers) {
            $golden  = json_decode((string) file_get_contents(self::FIXTURES . $file), true, 512, JSON_THROW_ON_ERROR);
            $objects = json_decode((string) file_get_contents(self::FIXTURES . $file), false, 512, JSON_THROW_ON_ERROR);
            $this->assertNotEmpty($golden['cases']);
            foreach ($golden['cases'] as $i => $case) {
                $valid = PageCreateBuilder::validate((object) ['post_type' => 'page', 'editor' => PageCreateBuilder::EDITOR_BLOCKS, 'title' => 'Golden', 'outline' => $objects->cases[$i]->input]);
                $this->assertArrayHasKey('spec', $valid, (string) ($valid['detail'] ?? ''));
                $r = ElementorClassicMapper::map($valid['spec'], new IdSeed($golden['request_id']), $golden['media'], $containers);
                $this->assertSame(json_encode($case['tree']), json_encode($r['tree'] ?? null), $file . ' ' . $case['name']);
            }
        }
    }

    public function test_counts_and_size_limits(): void
    {
        // New nodes are counted on the tree: fifty separators at the top are fifty-one elements.
        $tree = [];
        for ($k = 0; $k < 8; $k++) {
            $tree[] = ['id' => 'b00000' . $k, 'elType' => 'container', 'settings' => ['content_width' => 'boxed', 'flex_direction' => 'column'], 'elements' => [], 'isInner' => false];
        }
        $fifty = array_fill(0, 50, ['type' => 'separator']);
        $ops   = [];
        for ($k = 0; $k < 8; $k++) {
            $ops[] = self::insert(['after' => 'b00000' . $k], $fifty);
        }
        $this->assertNull(PageEditValidator::againstPage(['operations' => self::parsed($ops)], ElementorClassicMapper::project($tree)->nodes(), ['operations' => BuilderContract::OPS]), 'the outline count is 400');
        $this->assertRefused('page_too_large', 7, $tree, $ops, true, 'new_nodes');
        $this->assertSame(357, self::applied($tree, array_slice($ops, 0, 7), true)['new_count']);

        // Nodes after: every element, the insides of locked elements included.
        $full = [];
        for ($k = 0; $k < 8; $k++) {
            $kids = [];
            for ($m = 0; $m < 99; $m++) {
                $kids[] = ['id' => sprintf('e%02d%04d', $k, $m), 'elType' => 'widget', 'settings' => ['title' => 'T'], 'elements' => [], 'widgetType' => 'heading'];
            }
            $full[] = ['id' => 'b00000' . $k, 'elType' => 'container', 'settings' => ['content_width' => 'boxed', 'flex_direction' => 'column'], 'elements' => $kids, 'isInner' => false];
        }
        $this->assertSame(BuilderContract::MAX_NODES_AFTER, self::elementCount($full));
        $this->assertSame(0, self::applied($full, [self::setText('e000000', 'text', 'Hi')], true)['new_count']);
        $this->assertRefused('page_too_large', null, $full, [self::insert(['into' => 'b000000'], [self::para('One more')])], true, 'nodes_after');

        // The stored document at most 1 MiB.
        $big   = self::page(true);
        $ref   = self::refs($big, true);
        $big[] = ['id' => 'f1f1f1f', 'elType' => 'widget', 'settings' => ['testimonial_content' => str_repeat('a', BuilderContract::MAX_DOCUMENT_BYTES)], 'elements' => [], 'widgetType' => 'testimonial'];
        $this->assertRefused('page_too_large', null, $big, [self::setText($ref['h2'], 'text', 'Hi')], true, 'document_bytes');
    }

    // -------------------------------------------------------------------------
    // The adapter
    // -------------------------------------------------------------------------

    public function test_plan_edit_reads_the_one_stored_row(): void
    {
        $tree = self::page(false);
        $ref  = self::refs($tree, false);
        $ops  = self::parsed([self::insert(['after' => $ref['top2']], [self::para('Last')]), self::setText($ref['h3'], 'text', 'Our people')]);

        foreach ([true => 'container', false => 'section'] as $containers => $topType) {
            $db              = new FakeBuilderWpdb();
            $GLOBALS['wpdb'] = $db;
            $db->addPost(self::POST_ID, ['post_type' => 'page', 'post_status' => 'draft', 'post_title' => 'Spring', 'post_content' => '', 'post_modified_gmt' => '2026-10-09 08:00:00']);
            $db->addMeta(9, self::POST_ID, '_elementor_data', (string) json_encode($tree));
            $api                           = new FakeElementorApi();
            $api->experiments['container'] = (bool) $containers;

            $planned = (new ElementorAdapter($api, 7))->planEdit(self::POST_ID, $ops, new IdSeed(self::EDIT), self::MEDIA);
            $direct  = LayoutOps::apply($tree, $ops, new IdSeed(self::EDIT), self::MEDIA, (bool) $containers);
            $this->assertArrayHasKey('doc', $planned, json_encode($planned));
            $this->assertSame($direct['tree'], $planned['doc']->tree);
            $this->assertSame(json_encode($direct['tree']), $planned['doc']->canonicalBytes);
            $this->assertSame([], $planned['doc']->meta, 'an edit stores only the tree');
            $this->assertSame([$direct['changes'], $direct['touched'], $direct['new_count']], [$planned['changes'], $planned['touched'], $planned['new_count']]);
            $this->assertSame($topType, $planned['doc']->tree[3]['elType'], 'a new top-level element takes the site\'s layout');
        }

        $GLOBALS['wpdb'] = new FakeBuilderWpdb();
        $GLOBALS['wpdb']->addPost(self::POST_ID, ['post_type' => 'page', 'post_status' => 'draft', 'post_title' => 'Spring', 'post_content' => '', 'post_modified_gmt' => '2026-10-09 08:00:00']);
        $adapter = new ElementorAdapter(new FakeElementorApi(), 7);
        $this->assertSame('data_unreadable', $adapter->planEdit(self::POST_ID, $ops, new IdSeed(self::EDIT), self::MEDIA)['code'] ?? null, 'no row');
        $GLOBALS['wpdb']->addMeta(10, self::POST_ID, '_elementor_data', '{"id":"a"}');
        $this->assertSame('data_unreadable', $adapter->planEdit(self::POST_ID, $ops, new IdSeed(self::EDIT), self::MEDIA)['code'] ?? null, 'not a list');
        $GLOBALS['wpdb']->addMeta(11, self::POST_ID, '_elementor_data', '[]');
        $this->assertSame('data_unreadable', $adapter->planEdit(self::POST_ID, $ops, new IdSeed(self::EDIT), self::MEDIA)['code'] ?? null, 'two rows');
    }

    // -------------------------------------------------------------------------
    // Shared fixture
    // -------------------------------------------------------------------------

    public function test_replays_edit_cases_fixture(): void
    {
        $want = self::editCasesJson();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::EDIT_CASES, $want), 'could not write elementor-edit-cases.json');
        }
        $got = file_get_contents(self::EDIT_CASES);
        $this->assertIsString($got, 'elementor-edit-cases.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame($want, $got, 'elementor-edit-cases.json differs from what the agent produces now; regenerate with WPMGR_WRITE_FIXTURES=1');

        $doc   = json_decode($got, true, 512, JSON_THROW_ON_ERROR);
        $names = array_column($doc['cases'], 'name');
        $this->assertSame(['containers', 'sections'], array_values(array_unique(array_column($doc['cases'], 'layout'))));
        $this->assertCount(8, $names);
        foreach ($doc['cases'] as $case) {
            $ops = self::parsed($case['ops']);
            $this->assertNull(PageEditValidator::againstPage(['operations' => $ops], ElementorClassicMapper::project($case['before_tree'])->nodes(), ['operations' => BuilderContract::OPS]), $case['name']);
            $r = LayoutOps::apply($case['before_tree'], $ops, new IdSeed($case['request_id']), $doc['media'], $case['layout'] === 'containers');
            $this->assertSame(json_encode($case['after_tree']), json_encode($r['tree'] ?? null), $case['name']);
            $this->assertSame($case['changes'], $r['changes'], $case['name']);
            $this->assertNull(\WPMgr\Agent\Abilities\Builders\LeafPolicy::checkTree(self::madeNodes($r['tree'], $r['touched'], $r['changes']), ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES), $case['name'] . ': new nodes pass L3');
        }
    }

    /**
     * The fixture text.
     *
     * @return string
     */
    private static function editCasesJson(): string
    {
        $cases = [];
        foreach (['containers' => true, 'sections' => false] as $layout => $containers) {
            $tree = self::page($containers);
            $ref  = self::refs($tree, $containers);
            $sets = [
                'set-text-every-field' => ['Changes heading and paragraph text, a button link and an image caption; the heading keeps the colour a person set.', [
                    self::setText($ref['h2'], 'text', 'Summer sale & more'),
                    self::setText($ref['p1'], 'text', 'Open every day [1].'),
                    self::setText($ref['btn'], 'url', 'https://example.com/book'),
                    self::setText($ref['img'], 'caption', 'The whole team'),
                ]],
                'insert-every-place'   => ['Inserts at the top (wrapped), into a group first, into a column last, an inner row into a group, and before a locked widget.', [
                    self::insert(['after' => $ref['row']], [['type' => 'heading', 'level' => 2, 'text' => 'Find us'], self::para('Main Street 1')]),
                    self::insert(['into' => $ref['box0'], 'position' => 'first'], [self::para('New this week')]),
                    self::insert(['into' => $ref['col2']], [['type' => 'buttons', 'buttons' => [['text' => 'Email', 'url' => 'https://example.com/mail']]]]),
                    self::insert(['into' => $ref['box2']], [self::cols()]),
                    self::insert(['before' => $ref['lock']], [['type' => 'image', 'attachment_id' => 5, 'alt' => 'Library alt text']]),
                ]],
                'replace-remove-move'  => ['Replaces a paragraph with a list, removes the image, moves the group to the top and a button beside the heading.', [
                    ['op' => 'replace', 'ref' => $ref['p1'], 'outline' => [['type' => 'list', 'ordered' => true, 'items' => ['Tea', 'Cake']]]],
                    ['op' => 'remove', 'ref' => $ref['img']],
                    ['op' => 'move', 'ref' => $ref['top2'], 'before' => $ref['top0']],
                    ['op' => 'move', 'ref' => $ref['btn'], 'after' => $ref['h2']],
                ]],
                'change-then-remove'   => ['Changes a paragraph, moves a heading beside another and inserts into a column and before a paragraph, then removes the first block and replaces the row, so none of those changes is on the page after the call.', [
                    self::setText($ref['p1'], 'text', 'Open every day [1].'),
                    ['op' => 'move', 'ref' => $ref['h3'], 'after' => $ref['h2']],
                    self::insert(['into' => $ref['col2']], [['type' => 'buttons', 'buttons' => [['text' => 'Email', 'url' => 'https://example.com/mail']]]]),
                    self::insert(['before' => $ref['one']], [self::para('Before one')]),
                    ['op' => 'remove', 'ref' => $ref['top0']],
                    ['op' => 'replace', 'ref' => $ref['row'], 'outline' => [['type' => 'heading', 'level' => 2, 'text' => 'Find us']]],
                ]],
            ];
            foreach ($sets as $name => [$note, $operations]) {
                $r = self::applied($tree, $operations, $containers);
                $cases[] = [
                    'name'        => $name . '-' . $layout,
                    'note'        => $note,
                    'layout'      => $layout,
                    'request_id'  => self::EDIT,
                    'before_tree' => $tree,
                    'ops'         => $operations,
                    'after_tree'  => $r['tree'],
                    'changes'     => $r['changes'],
                    'touched'     => $r['touched'],
                    'new_count'   => $r['new_count'],
                ];
            }
        }
        $doc = [
            'note'  => 'Generated by the agent (tests/Builders/LayoutOpsTest.php). Regenerate with WPMGR_WRITE_FIXTURES=1. '
                . 'Each case is a stored Elementor classic tree (before_tree, made by page-create with request ' . self::PAGE_REQUEST . ', then given a colour a person set on the first heading and a testimonial widget WPMgr does not edit), '
                . 'the page-edit operations as the AI sends them (ops), and what LayoutOps makes of them for request_id: after_tree, changes (one per operation), touched (refs whose text changed, then refs made) and new_count (elements made). '
                . 'layout is the site\'s layout for new top-level elements; media holds the attachment facts new images read.',
            'media' => self::MEDIA,
            'cases' => $cases,
        ];

        return json_encode($doc, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * The page every case starts from.
     *
     * @param bool $containers Layout.
     * @return list<array<string, mixed>>
     */
    private static function page(bool $containers): array
    {
        $tree = self::created(self::OUTLINE, $containers);
        if ($containers) {
            $tree[0]['elements'][0]['settings']['title_color'] = self::PERSON_COLOR;
            $tree[2]['elements'][]                             = self::lockedWidget();
        } else {
            $tree[0]['elements'][0]['elements'][0]['settings']['title_color'] = self::PERSON_COLOR;
            $tree[2]['elements'][0]['elements'][]                             = self::lockedWidget();
        }

        return $tree;
    }

    /**
     * A widget WPMgr does not edit, its keys in an order of its own.
     *
     * @return array<string, mixed>
     */
    private static function lockedWidget(): array
    {
        return [
            'id'         => 'f0f0f0f',
            'settings'   => ['testimonial_content' => 'Great service', 'testimonial_name' => 'Ann', 'name_text_color' => '#333333'],
            'elements'   => [],
            'isInner'    => false,
            'widgetType' => 'testimonial',
            'elType'     => 'widget',
        ];
    }

    /**
     * A page as page-create builds it.
     *
     * @param string $outline    Outline JSON.
     * @param bool   $containers Layout.
     * @return list<array<string, mixed>>
     */
    private static function created(string $outline, bool $containers = true): array
    {
        $valid = PageCreateBuilder::validate((object) [
            'post_type' => 'page',
            'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
            'title'     => 'Spring',
            'outline'   => json_decode($outline, false, 512, JSON_THROW_ON_ERROR),
        ]);
        if (!isset($valid['spec'])) {
            throw new \RuntimeException('outline: ' . ($valid['detail'] ?? ''));
        }
        $mapped = ElementorClassicMapper::map($valid['spec'], new IdSeed(self::PAGE_REQUEST), self::MEDIA, $containers);
        if (!isset($mapped['tree'])) {
            throw new \RuntimeException('map: ' . ($mapped['detail'] ?? ''));
        }

        return $mapped['tree'];
    }

    /**
     * Names for the page's refs. box0 and box2 are the elements that hold the
     * first and last block's widgets: the container itself, or its section's
     * column.
     *
     * @param list<array<string, mixed>> $tree       The page.
     * @param bool                       $containers Layout.
     * @return array<string, string>
     */
    private static function refs(array $tree, bool $containers): array
    {
        $at = static function (int ...$path) use ($tree): string {
            $node = $tree[$path[0]];
            foreach (array_slice($path, 1) as $k) {
                $node = $node['elements'][$k];
            }

            return $node['id'];
        };
        if ($containers) {
            return [
                'top0' => $at(0), 'box0' => $at(0), 'h2' => $at(0, 0), 'p1' => $at(0, 1),
                'row' => $at(1), 'col1' => $at(1, 0), 'one' => $at(1, 0, 0), 'col2' => $at(1, 1), 'btn' => $at(1, 1, 0),
                'top2' => $at(2), 'box2' => $at(2), 'h3' => $at(2, 0), 'img' => $at(2, 1), 'lock' => $at(2, 2),
            ];
        }

        return [
            'top0' => $at(0), 'box0' => $at(0, 0), 'h2' => $at(0, 0, 0), 'p1' => $at(0, 0, 1),
            'row' => $at(1), 'col1' => $at(1, 0), 'one' => $at(1, 0, 0), 'col2' => $at(1, 1), 'btn' => $at(1, 1, 0),
            'top2' => $at(2), 'box2' => $at(2, 0), 'h3' => $at(2, 0, 0), 'img' => $at(2, 0, 1), 'lock' => $at(2, 0, 2),
        ];
    }

    /**
     * Operations as PageEditValidator::parse() normalises them.
     *
     * @param list<array<string, mixed>> $operations Operations as the AI sends them.
     * @return list<array<string, mixed>>
     */
    private static function parsed(array $operations): array
    {
        $r = PageEditValidator::parse((string) json_encode(['post_id' => self::POST_ID, 'base_fingerprint' => str_repeat('a', 64), 'operations' => $operations]));
        if (!isset($r['input'])) {
            throw new \RuntimeException('parse: ' . ($r['code'] ?? '') . ' ' . ($r['detail'] ?? ''));
        }

        return $r['input']['operations'];
    }

    /**
     * LayoutOps on the page; the call must succeed.
     *
     * @param list<array<string, mixed>> $tree       The page.
     * @param list<array<string, mixed>> $operations Operations as the AI sends them.
     * @param bool                       $containers The site's layout.
     * @param string                     $request    Request id.
     * @return array<string, mixed>
     */
    private static function applied(array $tree, array $operations, bool $containers, string $request = self::EDIT): array
    {
        $r = LayoutOps::apply($tree, self::parsed($operations), new IdSeed($request), self::MEDIA, $containers);
        if (!isset($r['tree'])) {
            throw new \RuntimeException('apply: ' . ($r['code'] ?? '') . ' ' . ($r['detail'] ?? ''));
        }

        return $r;
    }

    /**
     * @param string                     $code       Expected code.
     * @param int|null                   $opIndex    Expected operation.
     * @param list<array<string, mixed>> $tree       The page.
     * @param list<array<string, mixed>> $operations Operations.
     * @param bool                       $containers The site's layout.
     * @param string|null                $detail     Expected detail, when it is a fixed word.
     * @param string                     $request    Request id.
     */
    private function assertRefused(string $code, ?int $opIndex, array $tree, array $operations, bool $containers, ?string $detail = null, string $request = self::EDIT): void
    {
        $r = LayoutOps::apply($tree, self::parsed($operations), new IdSeed($request), self::MEDIA, $containers);
        $this->assertArrayNotHasKey('tree', $r, 'expected ' . $code);
        $this->assertSame($code, $r['code'] ?? null, (string) ($r['detail'] ?? ''));
        $this->assertSame($opIndex, $r['op_index']);
        if ($detail !== null) {
            $this->assertSame($detail, $r['detail']);
        }
    }

    /**
     * @param string $ref   Ref.
     * @param string $field Field.
     * @param string $text  Text.
     * @return array<string, string>
     */
    private static function setText(string $ref, string $field, string $text): array
    {
        return ['op' => 'set_text', 'ref' => $ref, 'field' => $field, 'text' => $text];
    }

    /**
     * @param array<string, string>      $anchor  Anchor fields.
     * @param list<array<string, mixed>> $outline Outline.
     * @return array<string, mixed>
     */
    private static function insert(array $anchor, array $outline): array
    {
        return ['op' => 'insert'] + $anchor + ['outline' => $outline];
    }

    /**
     * @param string $text Text.
     * @return array<string, string>
     */
    private static function para(string $text): array
    {
        return ['type' => 'paragraph', 'text' => $text];
    }

    /**
     * Two columns of one paragraph each.
     *
     * @return array<string, mixed>
     */
    private static function cols(): array
    {
        return ['type' => 'columns', 'columns' => [['children' => [self::para('Left')]], ['children' => [self::para('Right')]]]];
    }

    /**
     * Every element by id.
     *
     * @param array<mixed> $list Elements.
     * @return array<string, array<string, mixed>>
     */
    private static function byId(array $list): array
    {
        $out = [];
        foreach ($list as $node) {
            $out[$node['id']] = $node;
            $out += self::byId($node['elements'] ?? []);
        }

        return $out;
    }

    /**
     * Every id, parent first.
     *
     * @param array<mixed> $list Elements.
     * @return list<string>
     */
    private static function ids(array $list): array
    {
        return array_map('strval', array_keys(self::byId($list)));
    }

    /**
     * @param array<mixed> $list Elements.
     * @return int
     */
    private static function elementCount(array $list): int
    {
        return count(self::byId($list));
    }

    /**
     * A node without its elements.
     *
     * @param array<string, mixed> $node Node.
     * @return array<string, mixed>
     */
    private static function own(array $node): array
    {
        unset($node['elements']);

        return $node;
    }

    /**
     * The top-most nodes made by the call, for the whole-tree check.
     *
     * @param list<array<string, mixed>> $tree    Tree after the call.
     * @param list<string>               $touched Touched refs.
     * @param list<array<string, mixed>> $changes Changes.
     * @return list<array<string, mixed>>
     */
    private static function madeNodes(array $tree, array $touched, array $changes): array
    {
        $made = [];
        foreach ($changes as $change) {
            foreach ($change['new_refs'] ?? [] as $id) {
                $made[$id] = true;
            }
        }
        $out = [];
        $walk = static function (array $list, bool $inside) use (&$walk, &$out, $made): void {
            foreach ($list as $node) {
                $isNew = isset($made[$node['id']]);
                if ($isNew && !$inside) {
                    $out[] = $node;
                }
                $walk($node['elements'] ?? [], $inside || $isNew);
            }
        };
        $walk($tree, false);

        return $out;
    }

    /**
     * The sanitiser double.
     *
     * @param string $s Input.
     * @return string
     */
    private static function kses(string $s): string
    {
        $out = (string) preg_replace('/\s+on[a-z]+\s*=\s*(?:"[^"]*"|\'[^\']*\'|[^\s>]*)/i', '', $s);
        $out = (string) preg_replace_callback(
            '/<\/?([a-z][a-z0-9]*)\b[^>]*>/i',
            static fn (array $m): string => in_array(strtolower($m[1]), self::KEPT_TAGS, true) ? $m[0] : '',
            $out
        );

        return (string) preg_replace('/&(?!(?:[A-Za-z][A-Za-z0-9]*|#[0-9]+|#[xX][0-9A-Fa-f]+);)/', '&amp;', $out);
    }
}
