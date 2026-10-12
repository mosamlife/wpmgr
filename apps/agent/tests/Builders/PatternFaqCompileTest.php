<?php
/**
 * The faq pattern of a designed page: one classic accordion in a labelled
 * group, its questions as the accordion's tabs, FAQ schema on. The accordion
 * is compiled as the frozen round-trip probe stores it, the tabs with the
 * seven-hex repeater id the IdSeed gives them.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorLeafStyle;
use WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler
 */
final class PatternFaqCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000015';

    private const AT = 'outline[3].children[0]';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('wp_kses_post')->returnArg();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private static function tokens(): DesignTokens
    {
        return DesignTokens::fromDesign([
            'colors'  => [
                'ground'    => '#FFFFFF',
                'surface'   => '#F6F5F2',
                'ink'       => '#15171C',
                'muted'     => '#5A606C',
                'accent'    => '#0D7A63',
                'on_accent' => '#FAFAF7',
            ],
            'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ]);
    }

    private static function patterns(): ElementorPatternCompiler
    {
        return new ElementorPatternCompiler(self::tokens(), new ElementorSectionCompiler(self::tokens(), self::STOCK_BREAKPOINTS));
    }

    /** @return list<array<string,mixed>> The first $count items; the first two are the accordion probe's. */
    private static function items(int $count): array
    {
        $items = [
            ['question' => 'Do you ship?', 'answer' => 'Yes, worldwide.'],
            ['question' => 'Is it fresh?', 'answer' => 'Roasted weekly.'],
        ];
        for ($i = 2; $i < $count; $i++) {
            $items[] = ['question' => 'Question number ' . $i . '?', 'answer' => 'Answer number ' . $i . '.'];
        }

        return array_slice($items, 0, $count);
    }

    /** @param list<array<string,mixed>> $items Faq items. */
    private static function node(array $items): array
    {
        return ['type' => 'pattern', 'pattern' => 'faq', 'variant' => 'accordion', 'faq' => $items];
    }

    /** @param array<mixed> $value A value. */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    private static function allowed(): array
    {
        $keys = ElementorClassicMapper::ALLOWED_KEYS;
        foreach (ElementorLeafStyle::KEYS as $type => $paths) {
            $keys[$type] = $type === 'container' ? $keys[$type] + $paths : array_merge($keys[$type], $paths);
        }

        return ElementorPatternCompiler::allowedKeys($keys);
    }

    private function assertAllowed(array $tree): void
    {
        $answer = LeafPolicy::checkTree($tree, self::allowed(), ElementorClassicMapper::LEAF_RULES);
        $this->assertNull($answer, 'the tree is refused: ' . ($answer['detail'] ?? ''));
    }

    // -------------------------------------------------------------------------
    // pattern-faq
    // -------------------------------------------------------------------------

    public function test_pattern_faq(): void
    {
        $tree  = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(2)), 'ground');
        $probe = RoundTripProbes::tree('pattern-accordion-faq')[0]['elements'][0];
        $tone  = self::tokens()->tone('ground');

        // One labelled group, holding the accordion.
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($tree));
        $this->assertSame(['content_width', 'flex_direction', '_title'], array_keys($tree['settings']));
        $this->assertSame('full', $tree['settings']['content_width']);
        $this->assertSame('column', $tree['settings']['flex_direction']);
        $this->assertSame("WPMgr \u{00B7} FAQ accordion", $tree['settings']['_title']);
        $this->assertTrue($tree['isInner']);
        $this->assertCount(1, $tree['elements']);

        // The accordion is the probe's widget: its key order, its tabs, FAQ schema on, the tone's colours.
        $accordion = $tree['elements'][0];
        $this->assertSame(array_keys($probe), array_keys($accordion));
        $this->assertSame('widget', $accordion['elType']);
        $this->assertSame('accordion', $accordion['widgetType']);
        $this->assertSame([], $accordion['elements']);
        $this->assertSame(array_keys($probe['settings']), array_keys($accordion['settings']));

        $expected = $probe['settings'];
        foreach (array_keys($expected['tabs']) as $k) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $accordion['settings']['tabs'][$k]['_id']);
            $expected['tabs'][$k]['_id'] = $accordion['settings']['tabs'][$k]['_id'];
        }
        $expected['title_color']      = $tone['heading'];
        $expected['tab_active_color'] = $tone['accent'];
        $expected['content_color']    = $tone['body'];
        $this->assertSame(self::bytes($expected), self::bytes($accordion['settings']));
        $this->assertSame('yes', $accordion['settings']['faq_schema']);

        $this->assertAllowed([$tree]);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function tones(): array
    {
        return ['ground' => ['ground'], 'surface' => ['surface'], 'inverse' => ['inverse'], 'accent' => ['accent']];
    }

    /** @dataProvider tones */
    public function test_an_accordion_is_drawn_in_the_colours_of_its_tone(string $tone): void
    {
        $tree    = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(3)), $tone);
        $colours = self::tokens()->tone($tone);

        $settings = $tree['elements'][0]['settings'];
        $this->assertSame($colours['heading'], $settings['title_color']);
        $this->assertSame($colours['accent'], $settings['tab_active_color']);
        $this->assertSame($colours['body'], $settings['content_color']);
        $this->assertAllowed([$tree]);
    }

    public function test_the_tab_ids_are_seven_hex_stable_for_a_request_and_shared_with_no_node(): void
    {
        $first  = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(12)), 'ground');
        $again  = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(12)), 'ground');
        $other  = self::patterns()->faq(new IdSeed(self::REQUEST . '-other'), self::AT, self::node(self::items(12)), 'ground');

        $this->assertSame(self::bytes($first), self::bytes($again), 'a precheck and its write build the same bytes');
        $ids = array_column($first['elements'][0]['settings']['tabs'], '_id');
        $this->assertNotSame($ids, array_column($other['elements'][0]['settings']['tabs'], '_id'), 'another request gives other ids');
        $all = array_merge($ids, [$first['id'], $first['elements'][0]['id']]);
        $this->assertCount(14, array_unique($all));
        foreach ($all as $id) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $id);
        }
    }

    public function test_the_questions_and_answers_are_escaped_and_an_answer_is_a_paragraph(): void
    {
        $tree = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node([
            ['question' => 'Cats & <dogs> [1]?', 'answer' => 'Fish & chips [2] <b>only</b>.'],
        ]), 'ground');

        $tab = $tree['elements'][0]['settings']['tabs'][0];
        $this->assertSame('Cats &amp; &lt;dogs&gt; &#091;1&#093;?', $tab['tab_title']);
        $this->assertSame('<p>Fish &amp; chips &#091;2&#093; &lt;b&gt;only&lt;/b&gt;.</p>', $tab['tab_content']);
        $this->assertAllowed([$tree]);
    }

    public function test_a_faq_holds_up_to_twelve_questions_and_the_policy_lists_exactly_those_tabs(): void
    {
        $tree = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(12)), 'ground');
        $this->assertCount(12, $tree['elements'][0]['settings']['tabs']);
        $this->assertAllowed([$tree]);

        $tree['elements'][0]['settings']['tabs'][] = ['tab_title' => 'One too many', 'tab_content' => '<p>No.</p>', '_id' => 'abcdef1'];
        $answer = LeafPolicy::checkTree([$tree], self::allowed(), ElementorClassicMapper::LEAF_RULES);
        $this->assertNotNull($answer, 'a thirteenth tab is not on the allowlist');
        $this->assertSame(LeafPolicy::CODE_STYLE_KEY, $answer['code']);
    }

    public function test_a_faq_pattern_outside_one_to_twelve_items_is_not_compiled(): void
    {
        foreach ([0, 13] as $count) {
            try {
                self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node($count === 0 ? [] : self::items($count)), 'ground');
                $this->fail($count . ' items were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
        $one = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(1)), 'ground');
        $this->assertCount(1, $one['elements'][0]['settings']['tabs']);
        $this->assertAllowed([$one]);
    }

    public function test_a_question_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $bad = [
            'no question'    => ['answer' => 'x'],
            'empty question' => ['question' => '', 'answer' => 'x'],
            'question list'  => ['question' => ['x'], 'answer' => 'x'],
            'no answer'      => ['question' => 'x'],
            'empty answer'   => ['question' => 'x', 'answer' => ''],
            'answer number'  => ['question' => 'x', 'answer' => 7],
        ];
        $fine = ['question' => 'Fine?', 'answer' => 'Fine.'];
        foreach ($bad as $name => $item) {
            foreach ([[$item, $fine], [$fine, $item]] as $position => $items) {
                try {
                    self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node($items), 'ground');
                    $this->fail($name . ' was compiled at ' . $position);
                } catch (\InvalidArgumentException $e) {
                    $this->assertNotSame('', $e->getMessage(), $name);
                }
            }
        }
    }

    public function test_the_compile_entry_builds_a_faq(): void
    {
        $patterns = self::patterns();
        $node     = self::node(self::items(5));

        $this->assertSame(
            self::bytes($patterns->faq(new IdSeed(self::REQUEST), self::AT, $node, 'surface')),
            self::bytes($patterns->compile(new IdSeed(self::REQUEST), self::AT, $node, 'surface'))
        );
    }

    public function test_every_accordion_key_this_compiler_lists_is_one_a_faq_writes(): void
    {
        $tree      = self::patterns()->faq(new IdSeed(self::REQUEST), self::AT, self::node(self::items(12)), 'ground');
        $settings  = $tree['elements'][0]['settings'];

        $this->assertArrayHasKey('accordion', ElementorPatternCompiler::KEYS);
        foreach (array_keys(ElementorPatternCompiler::KEYS['accordion']) as $key) {
            $this->assertArrayHasKey($key, $settings, $key . ' is listed and no faq writes it');
        }

        // The tabs are a list: every position up to the grammar's most has its own listed members, and none beyond.
        $most    = OutlineV3Schema::ITEM_CAPS['faq'][1];
        $listed  = ElementorPatternCompiler::allowedKeys([])['accordion'];
        $members = [];
        foreach ($settings['tabs'] as $position => $tab) {
            foreach (array_keys($tab) as $member) {
                $this->assertArrayHasKey('tabs.' . $position . '.' . $member, $listed);
                $members[] = 'tabs.' . $position . '.' . $member;
            }
        }
        $this->assertCount($most * 3, $members);
        $this->assertSame($members, array_values(array_filter(array_keys($listed), static fn (string $path): bool => str_starts_with($path, 'tabs.'))));
        $this->assertArrayNotHasKey('tabs.' . $most . '.tab_title', $listed);
    }

    public function test_the_allowed_keys_for_a_list_never_narrow_a_pin_already_in_the_allowlist(): void
    {
        $allowed = ElementorPatternCompiler::allowedKeys(['accordion' => ['faq_schema' => 'enum', 'tabs.0.tab_title' => 'enum']]);

        $this->assertSame('enum', $allowed['accordion']['faq_schema']);
        $this->assertSame('enum', $allowed['accordion']['tabs.0.tab_title']);
        $this->assertSame('html', $allowed['accordion']['tabs.0.tab_content']);
        $this->assertSame('colour', $allowed['accordion']['title_color']);
    }
}
