<?php
/**
 * The hero and call-to-action patterns of a designed page: each a labelled
 * container around the blocks the page's leaf compiler built, in the
 * alignment the variant asks for. Compiled from the frozen round-trip probes'
 * own settings (the group, the row and its columns, the hero section), the
 * hero in a section and through the leaf compiler as a page builds it.
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
final class PatternHeroCtaCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000010';

    private const IMAGE_URL = 'https://example.com/wp-content/uploads/2026/10/hero-1600x900.jpg';

    private const HERO_AT = 'outline[0].children[0]';

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

    /** @return array<string,mixed> */
    private static function design(): array
    {
        return [
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
        ];
    }

    private static function tokens(): DesignTokens
    {
        return DesignTokens::fromDesign(self::design());
    }

    /** @return array<int,array<string,mixed>> Resolved image facts by attachment id. */
    private static function media(): array
    {
        return [5 => [
            'id'           => 5,
            'url'          => self::IMAGE_URL,
            'filename'     => 'hero-1600x900.jpg',
            'mime'         => 'image/jpeg',
            'width'        => 1600,
            'height'       => 900,
            'modified_gmt' => '2026-10-01 09:00:00',
            'alt'          => '',
        ]];
    }

    /** @param list<string>|null $breakpoints Active breakpoints; null when none is. */
    private static function sections(?array $breakpoints = self::STOCK_BREAKPOINTS): ElementorSectionCompiler
    {
        return new ElementorSectionCompiler(self::tokens(), $breakpoints, self::media());
    }

    /** @param list<string>|null $breakpoints Active breakpoints; null when none is. */
    private static function patterns(?array $breakpoints = self::STOCK_BREAKPOINTS): ElementorPatternCompiler
    {
        return new ElementorPatternCompiler(self::tokens(), self::sections($breakpoints), self::media());
    }

    /** @return array<string,mixed> The hero section the layout-hero-background probe compiles from. */
    private static function heroSection(): array
    {
        return [
            'type'       => 'section',
            'anchor'     => 'hero',
            'width'      => 'full',
            'spacing'    => 'xl',
            'background' => ['attachment_id' => 5, 'overlay' => 'soft', 'focal' => 'center'],
            'min_height' => 'screen',
            'children'   => [],
        ];
    }

    /** @return list<array<string,mixed>> The blocks of a hero. */
    private static function heroBlocks(bool $withImage = false): array
    {
        $blocks = [
            ['type' => 'heading', 'level' => 1, 'text' => 'Launch week'],
            ['type' => 'paragraph', 'text' => 'Everything you need to ship the first page.'],
            ['type' => 'buttons', 'buttons' => [
                ['text' => 'See pricing', 'url' => '#pricing', 'style' => 'fill'],
                ['text' => 'Talk to us', 'url' => '/contact', 'style' => 'outline'],
            ]],
        ];
        if ($withImage) {
            $blocks[] = ['type' => 'image', 'attachment_id' => 5, 'alt' => ''];
        }

        return $blocks;
    }

    /**
     * The blocks of a pattern as the page's leaf compiler builds them, in the
     * alignment the pattern asks for.
     *
     * @param array<string,mixed> $pattern A hero or cta node.
     * @param string              $tone    The tone of the section.
     * @return list<array<string,mixed>>
     */
    private static function built(array $pattern, IdSeed $ids, string $tone): array
    {
        $align = ElementorPatternCompiler::align($pattern['pattern'], $pattern['variant']);
        $r     = ElementorClassicMapper::mapDesignedLeaves($pattern['children'], $ids, self::media(), new ElementorLeafStyle(self::tokens()), $tone, (string) $align, self::HERO_AT . '.children');
        self::assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));

        return $r['tree'];
    }

    /** @param array<mixed> $value A value. */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    /**
     * A tree with every element id checked for IdSeed's shape and replaced by
     * its place, so two trees compare by everything but their ids.
     *
     * @param list<array<string,mixed>> $tree Elements.
     * @param string                    $at   Place of the list.
     * @return list<array<string,mixed>>
     */
    private function shape(array $tree, string $at = 'e'): array
    {
        $out = [];
        foreach ($tree as $i => $node) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $node['id'], $at . $i . ': id shape');
            $node['id']       = $at . $i;
            $node['elements'] = $this->shape($node['elements'], $at . $i . '.');
            $out[]            = $node;
        }

        return $out;
    }


    /**
     * Asserts the tree passes LeafPolicy with every key set a page uses; a failure
     * names the first element whose settings do not.
     *
     * @param list<array<string,mixed>> $tree Elements.
     */
    private function assertAllowed(array $tree): void
    {
        $keys   = ElementorPatternCompiler::allowedKeys(self::withLeafStyle(ElementorClassicMapper::ALLOWED_KEYS));
        $answer = LeafPolicy::checkTree($tree, $keys, ElementorClassicMapper::LEAF_RULES);
        if ($answer !== null) {
            $this->fail('the tree is refused: ' . $answer['detail'] . ' ' . self::offender($tree, $keys));
        }
        $this->assertNull($answer);
    }

    /**
     * The mapper's allowed keys with the leaf styles' added: a leaf's style may
     * widen a pin (a centred heading), a container's never narrows a section's.
     *
     * @param array<string,array<string,string>> $keys Allowed keys.
     * @return array<string,array<string,string>>
     */
    private static function withLeafStyle(array $keys): array
    {
        foreach (ElementorLeafStyle::KEYS as $type => $paths) {
            // A leaf's style widens its pins (a centred heading); a container's must not narrow a section's.
            $keys[$type] = $type === 'container' ? ($keys[$type] ?? []) + $paths : array_merge($keys[$type] ?? [], $paths);
        }

        return $keys;
    }

    /**
     * @param list<array<string,mixed>>          $tree Elements.
     * @param array<string,array<string,string>> $keys Allowed keys.
     */
    private static function offender(array $tree, array $keys): string
    {
        foreach ($tree as $element) {
            $alone = $element;
            $alone['elements'] = [];
            if (LeafPolicy::checkTree([$alone], $keys, ElementorClassicMapper::LEAF_RULES) !== null) {
                return 'at ' . ($element['widgetType'] ?? $element['elType']) . ' with ' . implode(',', array_keys($element['settings']));
            }
            $deeper = self::offender($element['elements'], $keys);
            if ($deeper !== '') {
                return $deeper;
            }
        }

        return '';
    }

    /** @return array<string,mixed> The settings of the layout-group probe's inner container. */
    private static function groupSettings(): array
    {
        return RoundTripProbes::tree('layout-group')[0]['elements'][0]['settings'];
    }

    // -------------------------------------------------------------------------
    // Labels
    // -------------------------------------------------------------------------

    public function test_a_label_is_the_prefix_the_pattern_and_the_variant(): void
    {
        $this->assertSame("WPMgr \u{00B7} Hero background", ElementorPatternCompiler::label('hero', 'background'));
        $this->assertSame("WPMgr \u{00B7} CTA band", ElementorPatternCompiler::label('cta', 'band'));
        $this->assertSame("WPMgr \u{00B7} Pricing three-plans", ElementorPatternCompiler::label('pricing', 'three-plans'));
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                $label = ElementorPatternCompiler::label($pattern, $variant);
                $this->assertStringStartsWith(LeafPolicy::LABEL_PREFIX, $label);
                $tree = [['id' => 'abcdef1', 'elType' => 'container', 'settings' => ['_title' => $label], 'elements' => [], 'isInner' => false]];
                $this->assertNull(LeafPolicy::checkTree($tree, ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES), $label . ' is a label LeafPolicy accepts');
            }
        }
    }

    public function test_a_label_reads_back_to_its_pattern_and_variant_and_nothing_else_does(): void
    {
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                $this->assertSame(['pattern' => $pattern, 'variant' => $variant], ElementorPatternCompiler::fromLabel(ElementorPatternCompiler::label($pattern, $variant)));
            }
        }
        foreach ([LeafPolicy::SECTION_LABEL, LeafPolicy::LABEL_PREFIX . 'Hero', LeafPolicy::LABEL_PREFIX . 'Hero other', 'Hero background', '', LeafPolicy::LABEL_PREFIX . 'Hero background '] as $label) {
            $this->assertNull(ElementorPatternCompiler::fromLabel($label), $label);
        }
    }

    public function test_a_pattern_or_variant_the_grammar_does_not_have_has_no_label(): void
    {
        foreach ([['hero', 'band'], ['carousel', 'row'], ['cta', '']] as [$pattern, $variant]) {
            try {
                ElementorPatternCompiler::label($pattern, $variant);
                $this->fail($pattern . ' ' . $variant . ' was given a label');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_the_alignment_each_variant_builds_its_blocks_in(): void
    {
        $this->assertSame('center', ElementorPatternCompiler::align('hero', 'background'));
        $this->assertSame('center', ElementorPatternCompiler::align('hero', 'centered'));
        $this->assertSame('start', ElementorPatternCompiler::align('hero', 'split'));
        $this->assertSame('center', ElementorPatternCompiler::align('cta', 'band'));
        $this->assertNull(ElementorPatternCompiler::align('stats', 'counters'));
        $this->assertNull(ElementorPatternCompiler::align('hero', 'unknown'));
    }

    // -------------------------------------------------------------------------
    // pattern-hero-background
    // -------------------------------------------------------------------------

    public function test_pattern_hero_background(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $section = self::heroSection();
        $pattern = ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'background', 'children' => self::heroBlocks()];
        $tone    = self::sections()->tone($section);
        $leaves  = self::built($pattern, $ids, $tone);
        $hero    = self::patterns()->hero($ids, self::HERO_AT, $pattern, $leaves);
        $tree    = [self::sections()->container($ids->next('outline[0]'), $section, [$hero])];

        // The section is the hero probe's, the hero the group probe's with the label.
        $this->assertSame('inverse', $tone);
        $this->assertSame(self::bytes(RoundTripProbes::settings('layout-hero-background')), self::bytes($tree[0]['settings']));
        $this->assertSame(self::bytes(self::groupSettings() + ['_title' => "WPMgr \u{00B7} Hero background"]), self::bytes($hero['settings']));
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($hero));
        $this->assertTrue($hero['isInner']);

        // Its blocks stand in it as the leaf compiler built them, centred, in the inverse tone.
        $centred = $leaves;
        $centred[2]['settings']['flex_justify_content'] = 'center';
        $this->assertSame(self::bytes($centred), self::bytes($hero['elements']));
        $heading = $hero['elements'][0];
        $this->assertSame('h1', $heading['settings']['header_size']);
        $this->assertSame('center', $heading['settings']['align']);
        $this->assertSame(self::tokens()->tone('inverse')['heading'], $heading['settings']['title_color']);
        $lead = $hero['elements'][1];
        $this->assertSame('center', $lead['settings']['align']);
        $row = $hero['elements'][2];
        $this->assertSame('center', $row['settings']['flex_justify_content']);
        $this->assertSame(['button', 'button'], array_column($row['elements'], 'widgetType'));

        $this->assertAllowed($tree);
        $this->shape($tree);
    }

    public function test_a_centred_hero_centres_a_row_of_buttons_the_outline_left_aligned(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $blocks  = self::heroBlocks();
        $blocks[2]['align'] = 'left';
        $pattern = ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => $blocks];
        $hero    = self::patterns()->hero($ids, self::HERO_AT, $pattern, self::built($pattern, $ids, 'inverse'));

        $this->assertSame("WPMgr \u{00B7} Hero centered", $hero['settings']['_title']);
        $this->assertSame('center', $hero['elements'][2]['settings']['flex_justify_content']);
        $keys = array_keys($hero['elements'][2]['settings']);
        $this->assertSame('flex_justify_content', end($keys), 'after the row keys the leaf compiler wrote');
    }

    // -------------------------------------------------------------------------
    // The split hero
    // -------------------------------------------------------------------------

    public function test_a_split_hero_is_the_columns_row_probe_with_the_label(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $pattern = ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'split', 'children' => self::heroBlocks(true)];
        $leaves  = self::built($pattern, $ids, 'ground');
        $hero    = self::patterns()->hero($ids, self::HERO_AT, $pattern, $leaves);

        $probe = RoundTripProbes::tree('layout-columns-row')[0]['elements'][0];
        $this->assertSame(self::bytes($probe['settings'] + ['_title' => "WPMgr \u{00B7} Hero split"]), self::bytes($hero['settings']));
        $this->assertSame(['container', 'container'], array_column($hero['elements'], 'elType'));
        foreach ([0, 1] as $k) {
            $this->assertSame(self::bytes($probe['elements'][$k]['settings']), self::bytes($hero['elements'][$k]['settings']), 'column ' . $k);
        }
        // Copy on the left at 55, the picture on the right at 45; copy keeps the start alignment.
        $this->assertSame(['heading', 'text-editor'], array_column(array_slice($hero['elements'][0]['elements'], 0, 2), 'widgetType'));
        $this->assertSame('container', $hero['elements'][0]['elements'][2]['elType']);
        $this->assertSame(['image'], array_column($hero['elements'][1]['elements'], 'widgetType'));
        $this->assertSame('left', $hero['elements'][0]['elements'][0]['settings']['align']);
        $this->assertArrayNotHasKey('flex_justify_content', $hero['elements'][0]['elements'][2]['settings']);

        $this->assertAllowed([$hero]);
        $this->shape([$hero]);
    }

    public function test_a_split_hero_with_no_picture_is_one_stacked_group(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $pattern = ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'split', 'children' => self::heroBlocks()];
        $hero    = self::patterns()->hero($ids, self::HERO_AT, $pattern, self::built($pattern, $ids, 'ground'));

        $this->assertSame(self::bytes(self::groupSettings() + ['_title' => "WPMgr \u{00B7} Hero split"]), self::bytes($hero['settings']));
        $this->assertCount(3, $hero['elements']);
    }

    public function test_a_split_hero_on_a_site_without_breakpoints_writes_the_desktop_row_only(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $pattern = ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'split', 'children' => self::heroBlocks(true)];
        $hero    = self::patterns(null)->hero($ids, self::HERO_AT, $pattern, self::built($pattern, $ids, 'ground'));

        $this->assertSame(['flex_direction', 'flex_gap', '_title'], array_keys($hero['settings']));
        $this->assertSame(['content_width', 'width'], array_keys($hero['elements'][0]['settings']));
    }

    // -------------------------------------------------------------------------
    // pattern-cta-band
    // -------------------------------------------------------------------------

    public function test_pattern_cta_band(): void
    {
        $ids     = new IdSeed(self::REQUEST);
        $section = ['type' => 'section', 'anchor' => 'signup', 'tone' => 'accent', 'children' => []];
        $pattern = ['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [
            ['type' => 'heading', 'level' => 2, 'text' => 'Ready when you are'],
            ['type' => 'paragraph', 'text' => 'Start with one page.'],
            ['type' => 'buttons', 'buttons' => [['text' => 'Get started', 'url' => '#signup', 'style' => 'fill'], ['text' => 'Ask a question', 'url' => '/contact', 'style' => 'outline']]],
        ]];
        $tone   = self::sections()->tone($section);
        $leaves = self::built($pattern, $ids, $tone);
        $cta    = self::patterns()->cta($ids, 'outline[5].children[0]', $pattern, $leaves);
        $tree   = [self::sections()->container($ids->next('outline[5]'), $section, [$cta])];

        $this->assertSame('accent', $tone);
        $this->assertSame(self::bytes(self::groupSettings() + ['_title' => "WPMgr \u{00B7} CTA band"]), self::bytes($cta['settings']));
        $this->assertSame(self::design()['colors']['accent'], $tree[0]['settings']['background_color']);

        // The heading and the buttons are drawn in on_accent, centred.
        $onAccent = self::design()['colors']['on_accent'];
        $this->assertSame($onAccent, $cta['elements'][0]['settings']['title_color']);
        $this->assertSame('center', $cta['elements'][0]['settings']['align']);
        $row = $cta['elements'][2];
        $this->assertSame('center', $row['settings']['flex_justify_content']);
        $this->assertSame($onAccent, $row['elements'][0]['settings']['background_color']);
        $this->assertSame($onAccent, $row['elements'][1]['settings']['border_color']);
        $this->assertSame(self::design()['colors']['accent'], $row['elements'][0]['settings']['button_text_color']);

        $this->assertAllowed($tree);
    }

    // -------------------------------------------------------------------------
    // What is not compiled
    // -------------------------------------------------------------------------

    public function test_a_hero_of_another_pattern_or_variant_or_without_blocks_is_not_compiled(): void
    {
        $ids   = new IdSeed(self::REQUEST);
        $block = [['id' => 'abcdef1', 'elType' => 'widget', 'settings' => [], 'elements' => [], 'widgetType' => 'heading']];
        $cases = [
            'cta given to hero'   => [['pattern' => 'cta', 'variant' => 'band'], $block],
            'variant of another'  => [['pattern' => 'hero', 'variant' => 'band'], $block],
            'no variant'          => [['pattern' => 'hero'], $block],
            'no blocks'           => [['pattern' => 'hero', 'variant' => 'background'], []],
        ];
        foreach ($cases as $name => [$pattern, $elements]) {
            try {
                self::patterns()->hero($ids, self::HERO_AT, $pattern, $elements);
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
    }

    public function test_the_compile_entry_builds_what_the_named_method_builds(): void
    {
        $pattern = ['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [['type' => 'heading', 'level' => 2, 'text' => 'Ready']]];
        $one     = new IdSeed(self::REQUEST);
        $two     = new IdSeed(self::REQUEST);
        $leaves  = self::built($pattern, $one, 'accent');
        $this->assertSame(
            self::bytes(self::patterns()->cta($one, 'outline[5].children[0]', $pattern, $leaves)),
            self::bytes(self::patterns()->compile($two, 'outline[5].children[0]', $pattern, 'accent', self::built($pattern, $two, 'accent')))
        );
    }
}
