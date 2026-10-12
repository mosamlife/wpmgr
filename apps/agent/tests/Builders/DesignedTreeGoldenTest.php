<?php
/**
 * Designed-tree golden: the tree the page-create precheck builds for a designed
 * page (outline version 3), recorded for a spread of inputs and sites in one
 * shared fixture and asserted equal here. A twin of the compile in another
 * language replays the same file.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   elementor-designed-containers.json  inputs, the site facts each was
 *                                       compiled with, and what the precheck
 *                                       answered
 *
 * Every case runs through BuilderPageCreate::precheck(), the code the
 * wpmgr/page-create command's precheck runs after it has validated the input
 * and resolved the images, over the real ElementorAdapter and a stand-in for
 * Elementor's runtime.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\NativeDocument;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageCreate
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorAdapter
 */
final class DesignedTreeGoldenTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-designed-containers.json';

    /** The shared accept/refuse table; its full landing page is one of the golden's inputs. */
    private const SHARED_CASES = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    private const PRINCIPAL = 2;

    /** The request id of most cases, and a second one. */
    private const REQ_A = '11111111-2222-4333-8444-777777777777';

    private const REQ_B = '99999999-2222-4333-8444-555555555555';

    private const BOTH = ['mobile', 'tablet'];

    /** The palettes of the designs, in the order of OutlineV3Schema::COLOUR_ROLES. */
    private const PALETTES = [
        'studio' => ['#F6F5F2', '#FFFFFF', '#15171C', '#5A606C', '#0D7A63', '#FFFFFF'],
        'dark'   => ['#0F1115', '#171A21', '#F3F4F6', '#9CA3AF', '#22D3EE', '#0F1115'],
        'warm'   => ['#FBF7F0', '#F1E8DA', '#2B1D14', '#6B5646', '#B4532A', '#FFFFFF'],
        'slate'  => ['#F8FAFC', '#EEF2F7', '#0F172A', '#475569', '#2563EB', '#FFFFFF'],
    ];

    /** The alt text each image has in the media library, by attachment id; an id not listed has none. */
    private const LIBRARY_ALT = [
        42  => 'Our team at work',
        43  => 'A quiet street',
        44  => 'Product close-up',
        101 => 'Acme logo', 102 => 'Birchwood logo', 103 => 'Cobalt logo', 104 => 'Dunmore logo', 105 => 'Everly logo',
        106 => 'Fernhill logo', 107 => 'Glenn logo', 108 => 'Harbour logo', 109 => 'Ivy logo', 110 => 'Juniper logo',
    ];

    /** The images whose file is not a JPEG. */
    private const MIME_OF = [44 => 'image/png', 105 => 'image/png', 106 => 'image/webp', 109 => 'image/gif', 110 => 'image/avif'];

    private const EXTENSION_OF = ['image/jpeg' => 'jpg', 'image/png' => 'png', 'image/webp' => 'webp', 'image/gif' => 'gif', 'image/avif' => 'avif'];

    private const WIDGETS = ['heading', 'text-editor', 'button', 'image', 'divider', 'spacer', 'icon-box', 'counter', 'testimonial', 'accordion', 'icon-list', 'image-box', 'tabs'];

    private FakeElementorApi $api;

    private bool $blockTheme = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $theme = new class () {
            /** @return array<string,string> */
            public function get_page_templates($post = null, $post_type = 'page'): array
            {
                return ['elementor_header_footer' => 'Elementor Full Width', 'elementor_canvas' => 'Elementor Canvas'];
            }
        };
        $slash = static function ($value) use (&$slash) {
            if (is_array($value)) {
                return array_map($slash, $value);
            }

            return is_string($value) ? addslashes($value) : $value;
        };
        Functions\when('get_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('get_userdata')->justReturn(false);
        Functions\when('wp_slash')->alias($slash);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wp_get_theme')->justReturn($theme);
        Functions\when('wp_is_block_theme')->alias(fn (): bool => $this->blockTheme);
        Functions\when('get_post_meta')->alias(static fn ($id, $key = '', $single = false) => $key === BuilderPageCreate::ALT_META_KEY ? (self::LIBRARY_ALT[(int) $id] ?? '') : '');

        $this->api               = new FakeElementorApi();
        $this->api->widgetTypes  = self::WIDGETS;
        $this->api->fontFamilies = ['Manrope', 'Inter', 'Lora', 'Roboto', 'Source Sans 3', 'Playfair Display'];
        $this->blockTheme        = false;
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The fixture
    // -------------------------------------------------------------------------

    public function test_fixture_is_what_the_code_produces(): void
    {
        $want = $this->fixtureText();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::FIXTURE, $want), 'could not write elementor-designed-containers.json');
        }
        $this->assertFileExists(self::FIXTURE, 'elementor-designed-containers.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $got = file_get_contents(self::FIXTURE);
        $this->assertIsString($got);
        $this->assertSame($want, $got, 'elementor-designed-containers.json differs from what the precheck builds now; regenerate with WPMGR_WRITE_FIXTURES=1');
    }

    /**
     * Each case replays from what it records alone: the input text and the
     * facts, not the generator's memory of them.
     */
    public function test_every_case_replays_from_its_input_and_facts(): void
    {
        $doc = $this->fixture();
        $this->assertNotEmpty($doc['cases'], 'the golden holds no case');
        // A precheck writes nothing: no post, no meta.
        Functions\expect('wp_insert_post')->never();
        Functions\expect('update_post_meta')->never();
        Functions\expect('add_post_meta')->never();
        foreach ($doc['cases'] as $case) {
            $answer = $this->precheck((string) $case['input'], $case['facts']);
            $this->assertArrayNotHasKey('refusal', $answer, $case['name'] . ': ' . json_encode($answer['refusal'] ?? null));
            $this->assertSame($case['tree'], $answer['tree'], $case['name'] . ' tree');
            $this->assertSame($case['builder_facts'], $answer['builder_facts'], $case['name'] . ' builder_facts');
            $this->assertSame($case['preview_digest'], $answer['preview_digest'], $case['name'] . ' preview_digest');
            $this->assertSame($case['facts']['layout'], $answer['preview']['layout'], $case['name'] . ' layout');
            $this->assertSame($case['facts']['elementor_version'], $answer['preview']['elementor_version'], $case['name'] . ' version');
            $this->assertSame($case['facts']['media'], $answer['preview']['media'] ?? [], $case['name'] . ' media');
        }
    }

    /**
     * The tree in the file, encoded as the digest binds it, is the tree the
     * digest was taken over: a twin that matches the tree matches the digest.
     */
    public function test_the_digest_binds_the_tree_in_the_file(): void
    {
        $doc = $this->fixture();
        foreach ($doc['cases'] as $case) {
            $decoded = json_decode((string) $case['input'], true, 512, JSON_THROW_ON_ERROR);
            $bytes   = (new NativeDocument($case['tree']))->canonicalBytes;
            $digest  = BuilderPageCreate::previewDigest('builder:elementor', 'classic', $case['facts']['elementor_version'], $decoded['post_type'], $decoded['title'], $bytes);
            $this->assertSame($case['preview_digest'], $digest, $case['name']);
        }
    }

    public function test_the_golden_holds_no_float(): void
    {
        $doc = $this->fixture();
        foreach ($doc['cases'] as $case) {
            $this->assertSame([], self::floatPaths($case['tree']), $case['name'] . ': a number written as a float depends on the host float settings');
        }
    }

    // -------------------------------------------------------------------------
    // What the golden has to hold
    // -------------------------------------------------------------------------

    public function test_the_golden_covers_every_pattern_variant_and_design_option(): void
    {
        $seen = ['pattern' => [], 'density' => [], 'ratio' => [], 'radius' => [], 'tone' => [], 'style' => [], 'overlay' => [], 'focal' => [], 'icon' => [], 'sets' => [], 'bases' => []];
        foreach ($this->fixture()['cases'] as $case) {
            $page = json_decode((string) $case['input'], true, 512, JSON_THROW_ON_ERROR);
            $seen['density'][$page['design']['density']]       = true;
            $seen['ratio'][$page['design']['scale']['ratio']]  = true;
            $seen['radius'][$page['design']['radius']]         = true;
            $seen['bases'][$page['design']['scale']['base_px']] = true;
            $seen['sets'][json_encode($case['facts']['active_breakpoints'])] = true;
            self::walk($page['outline'], static function (array $node) use (&$seen): void {
                if (($node['type'] ?? null) === 'section') {
                    $seen['tone'][$node['tone'] ?? 'ground'] = true;
                    $background = $node['background'] ?? null;
                    if (is_array($background)) {
                        $seen['overlay'][$background['overlay'] ?? 'omitted'] = true;
                        $seen['focal'][$background['focal'] ?? 'omitted']     = true;
                    }
                }
                if (($node['type'] ?? null) === 'pattern') {
                    $seen['pattern'][$node['pattern'] . '/' . $node['variant']] = true;
                    foreach ($node['features'] ?? [] as $feature) {
                        $seen['icon'][$feature['icon']] = true;
                    }
                }
                foreach ($node['buttons'] ?? [] as $button) {
                    if (is_array($button) && isset($button['style'])) {
                        $seen['style'][$button['style']] = true;
                    }
                }
                foreach ($node['plans'] ?? [] as $plan) {
                    $seen['style']['plan'] = true;
                }
            });
        }

        $every = [];
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                $every[] = $pattern . '/' . $variant;
            }
        }
        $this->assertSame(self::sorted($every), self::sorted(array_keys($seen['pattern'])), 'every pattern in every variant');
        $this->assertSame(self::sorted(OutlineV3Schema::DENSITIES), self::sorted(array_keys($seen['density'])), 'every density');
        $this->assertSame(self::sorted(DesignTokens::RATIOS ? array_keys(DesignTokens::RATIOS) : []), self::sorted(array_keys($seen['ratio'])), 'every ratio name');
        $this->assertSame(self::sorted(OutlineV3Schema::RADII), self::sorted(array_keys($seen['radius'])), 'every radius');
        $this->assertSame(self::sorted(OutlineV3Schema::TONES), self::sorted(array_keys($seen['tone'])), 'every tone');
        $this->assertSame(['fill', 'outline', 'plan', 'text'], self::sorted(array_keys($seen['style'])), 'every button style, and the pricing plans');
        $this->assertSame(self::sorted(array_merge(OutlineV3Schema::OVERLAYS, ['omitted'])), self::sorted(array_keys($seen['overlay'])), 'every overlay strength, and none given');
        $this->assertSame(self::sorted(array_merge(OutlineV3Schema::FOCALS, ['omitted'])), self::sorted(array_keys($seen['focal'])), 'every focal point, and none given');
        $this->assertSame(self::sorted(OutlineV3Schema::ICONS), self::sorted(array_keys($seen['icon'])), 'every icon');
        foreach ([json_encode(self::BOTH), json_encode(['mobile']), json_encode(['tablet']), json_encode([]), 'null'] as $set) {
            $this->assertArrayHasKey($set, $seen['sets'], 'a case for the active breakpoints ' . $set);
        }
        $this->assertGreaterThan(1, count($seen['bases']), 'more than one base size');
    }

    /**
     * Counts that change the layout of a pattern: the smallest and the largest
     * the grammar allows, and one between.
     */
    public function test_the_golden_covers_the_counts_of_every_item_pattern(): void
    {
        $counts = [];
        foreach ($this->fixture()['cases'] as $case) {
            $page = json_decode((string) $case['input'], true, 512, JSON_THROW_ON_ERROR);
            self::walk($page['outline'], static function (array $node) use (&$counts): void {
                if (($node['type'] ?? null) !== 'pattern' || !isset(OutlineV3Schema::PATTERN_ARRAY[$node['pattern']])) {
                    return;
                }
                $array                                                              = OutlineV3Schema::PATTERN_ARRAY[$node['pattern']];
                $counts[$node['pattern'] . '/' . $node['variant']][count($node[$array])] = true;
            });
        }
        foreach (OutlineV3Schema::PATTERN_ARRAY as $pattern => $array) {
            [$min, $max] = OutlineV3Schema::ITEM_CAPS[$array];
            $variant     = OutlineV3Schema::VARIANTS[$pattern][0];
            $have        = array_keys($counts[$pattern . '/' . $variant] ?? []);
            $this->assertContains($min, $have, $pattern . ' at its smallest');
            $this->assertContains($max, $have, $pattern . ' at its largest');
        }
        foreach (OutlineV3Schema::VARIANTS['features'] as $variant) {
            $have = array_keys($counts['features/' . $variant]);
            $this->assertGreaterThanOrEqual(3, count($have), 'features/' . $variant . ' at three counts');
        }
    }

    public function test_builder_facts_answer_the_breakpoints_the_compile_read(): void
    {
        $rows = 0;
        foreach ($this->fixture()['cases'] as $case) {
            $this->assertSame(
                ['block_theme', 'page_frame', 'active_breakpoints'],
                array_keys($case['builder_facts']),
                $case['name'] . ' builder_facts members'
            );
            $this->assertSame($case['facts']['active_breakpoints'], $case['builder_facts']['active_breakpoints'], $case['name'] . ' the breakpoints the tree was compiled for');
            $this->assertSame($case['facts']['block_theme'], $case['builder_facts']['block_theme'], $case['name'] . ' theme');
            $rows++;
        }
        $this->assertGreaterThan(0, $rows);
    }

    /**
     * A responsive override is written only for a breakpoint the site has
     * active, and takes nothing else with it: the tree under both breakpoints,
     * less those keys, is the tree under none.
     */
    public function test_breakpoints_add_only_their_own_keys(): void
    {
        $byName = [];
        foreach ($this->fixture()['cases'] as $case) {
            $byName[$case['name']] = $case;
        }
        $none = $byName['responsive-probe-no-breakpoints']['tree'];
        $this->assertSame([], self::responsiveKeys($none, ['mobile', 'tablet']), 'no breakpoint active: no override of one');
        $this->assertSame([], self::responsiveKeys($byName['responsive-probe-no-breakpoints-reported']['tree'], ['mobile', 'tablet']), 'a site that cannot say has none active');
        $this->assertSame($none, $byName['responsive-probe-no-breakpoints-reported']['tree'], 'none active and not reported are the same tree');

        foreach ([['mobile', 'tablet'], ['mobile'], ['tablet']] as $active) {
            $name = 'responsive-probe-' . implode('-and-', $active);
            $tree = $byName[$name]['tree'];
            $this->assertNotSame([], self::responsiveKeys($tree, $active), $name . ' writes overrides');
            $other = array_values(array_diff(['mobile', 'tablet'], $active));
            $this->assertSame([], self::responsiveKeys($tree, $other), $name . ' writes none for a breakpoint that is not active');
            $this->assertSame($none, self::withoutResponsiveKeys($tree, $active), $name . ' differs from none by its overrides only');
        }
        // Breakpoints Elementor offers beyond these two change nothing.
        $this->assertSame($byName['responsive-probe-mobile-and-tablet']['tree'], $byName['responsive-probe-with-extra-breakpoints']['tree']);
    }

    /**
     * The compile has no branch on the Elementor release: the faq is the
     * classic accordion at every one, so the tree is the same.
     */
    public function test_the_tree_does_not_depend_on_the_elementor_version(): void
    {
        $byName = [];
        foreach ($this->fixture()['cases'] as $case) {
            $byName[$case['name']] = $case;
        }
        $base = $byName['faq-at-elementor-3-35-9'];
        foreach (['faq-at-elementor-3-20-4' => '3.20.4', 'faq-at-elementor-4-3-4' => '4.3.4'] as $name => $version) {
            $this->assertSame($version, $byName[$name]['facts']['elementor_version']);
            $this->assertSame($base['input'], $byName[$name]['input']);
            $this->assertSame($base['tree'], $byName[$name]['tree'], $name);
            $this->assertNotSame($base['preview_digest'], $byName[$name]['preview_digest'], 'the release is bound by the digest');
        }
        $this->assertSame('accordion', self::firstWidget($base['tree'], 'accordion')['widgetType'] ?? null);
    }

    /** The frame the page is stored in never changes the tree. */
    public function test_the_page_frame_never_changes_the_tree(): void
    {
        $byName = [];
        foreach ($this->fixture()['cases'] as $case) {
            $byName[$case['name']] = $case;
        }
        $base = $byName['hero-centered'];
        foreach (['hero-centered-full-width', 'hero-centered-on-a-block-theme'] as $name) {
            $this->assertSame($base['tree'], $byName[$name]['tree'], $name);
            $this->assertNotSame($base['builder_facts'], $byName[$name]['builder_facts'], $name . ' answers its own frame');
        }
        $this->assertSame(['block_theme' => true, 'page_frame' => 'canvas', 'active_breakpoints' => self::BOTH], $byName['hero-centered-on-a-block-theme']['builder_facts']);
    }

    /** Node ids are a function of the request: another request, other ids, the same settings. */
    public function test_another_request_changes_the_ids_and_nothing_else(): void
    {
        $byName = [];
        foreach ($this->fixture()['cases'] as $case) {
            $byName[$case['name']] = $case;
        }
        $a = $byName['hero-centered']['tree'];
        $b = $byName['hero-centered-another-request']['tree'];
        $this->assertNotSame(self::idsOf($a), self::idsOf($b));
        $this->assertSame(self::withoutIds($a), self::withoutIds($b));
        $this->assertSame(self::REQ_A, $byName['hero-centered']['facts']['request_id']);
        $this->assertSame(self::REQ_B, $byName['hero-centered-another-request']['facts']['request_id']);
    }

    public function test_every_tree_is_on_the_designed_allowlist_of_its_breakpoints(): void
    {
        foreach ($this->fixture()['cases'] as $case) {
            $allowed = LeafPolicy::forBreakpoints(ElementorClassicMapper::designedKeys(), $case['facts']['active_breakpoints'] ?? []);
            $this->assertNull(LeafPolicy::checkTree($case['tree'], $allowed, ElementorClassicMapper::LEAF_RULES), $case['name']);
        }
    }

    // -------------------------------------------------------------------------
    // The generator
    // -------------------------------------------------------------------------

    /**
     * The cases of the golden, as inputs and the site each is compiled on.
     *
     * @return list<array{name:string,page:array<string,mixed>,facts:array<string,mixed>}>
     */
    private static function inputs(): array
    {
        $cases = [];
        $add   = static function (string $name, array $outline, array $design = [], array $facts = [], string $frame = 'theme') use (&$cases): void {
            $cases[] = ['name' => $name, 'page' => self::page($outline, $design, $frame), 'facts' => $facts];
        };

        // ---- the design: ratio, density, radius, palette, fonts, base size ----
        $spacings = static fn (): array => [
            self::section('first', [self::h(2, 'A heading of the second level'), self::h(3, 'A heading of the third level'), self::h(4, 'A heading of the fourth level'), self::p('Body text under the headings.')], ['spacing' => 'md']),
            self::section('second', [self::h(2, 'Second section'), self::p('More body text.'), self::buttons([['Learn more', '#third', 'fill']])], ['spacing' => 'lg', 'tone' => 'surface']),
            self::section('third', [self::h(2, 'Third section'), self::p('Closing text.'), self::buttons([['Contact', '/contact', 'outline'], ['Read on', 'https://example.com/read', 'text']], 'center')], ['spacing' => 'xl', 'tone' => 'inverse', 'align' => 'center']),
        ];
        $add('design-compact-major-second', $spacings(), ['palette' => 'slate', 'fonts' => ['system', 'system'], 'base' => 15, 'ratio' => 'major_second', 'radius' => 'none', 'density' => 'compact']);
        $add('design-normal-minor-third', $spacings(), ['palette' => 'studio', 'base' => 16, 'ratio' => 'minor_third', 'radius' => 'sm', 'density' => 'normal']);
        $add('design-airy-major-third', $spacings(), ['palette' => 'warm', 'fonts' => ['Playfair Display', 'Inter'], 'base' => 18, 'ratio' => 'major_third', 'radius' => 'md', 'density' => 'airy']);
        $add('design-normal-perfect-fourth', $spacings(), ['palette' => 'dark', 'fonts' => ['Inter', 'Lora'], 'base' => 20, 'ratio' => 'perfect_fourth', 'radius' => 'lg', 'density' => 'normal']);

        // ---- tones and button styles ----
        $toned = [];
        foreach (OutlineV3Schema::TONES as $tone) {
            $toned[] = self::section('tone-' . $tone, [
                self::h(2, 'A section in the ' . $tone . ' tone'),
                self::p('Text in the ' . $tone . ' tone.'),
                self::buttons([['Fill button', '#tone-ground', 'fill'], ['Outline button', '/outline', 'outline'], ['Text button', 'https://example.com/text', 'text']]),
            ], ['tone' => $tone]);
        }
        $add('tones-and-button-styles', $toned);

        // ---- the section options ----
        $add('section-defaults', [self::section('plain', [self::h(2, 'Only the required members'), self::p('Boxed, ground tone, normal spacing, start aligned.')])]);
        $add('section-every-option', [self::section('story', [self::h(2, 'Our story'), self::p('Started in a garage, still run by the same two people.')], [
            'width' => 'full', 'tone' => 'inverse', 'background' => ['attachment_id' => 42, 'overlay' => 'strong', 'focal' => 'top'], 'spacing' => 'xl', 'align' => 'center', 'min_height' => 'screen',
        ])]);
        $backgrounds = [];
        foreach ([
            ['soft', 'center', 'boxed', 42], ['strong', 'top', 'full', 43], ['none', 'bottom', 'boxed', 42], [null, null, 'full', 43], ['soft', null, 'full', 44], [null, 'bottom', 'boxed', 44],
        ] as $i => [$overlay, $focal, $width, $image]) {
            $background = ['attachment_id' => $image];
            if ($overlay !== null) {
                $background['overlay'] = $overlay;
            }
            if ($focal !== null) {
                $background['focal'] = $focal;
            }
            $backgrounds[] = self::section('picture-' . $i, [self::h(2, 'Over a picture ' . $i), self::p('Text over the picture.')], ['width' => $width, 'tone' => $i % 2 === 0 ? 'inverse' : 'ground', 'background' => $background, 'min_height' => $i % 3 === 0 ? 'screen' : 'auto']);
        }
        $add('section-backgrounds', $backgrounds);

        // ---- the leaves ----
        $add('leaves-of-every-kind', [
            self::section('words', [
                self::h(2, 'Headings and text'), self::h(3, 'A third level'), self::h(4, 'A fourth level'),
                self::p('A paragraph with a link-free sentence.'),
                ['type' => 'list', 'ordered' => false, 'items' => ['First point', 'Second point', 'Third point']],
                ['type' => 'list', 'ordered' => true, 'items' => ['Step one', 'Step two']],
                ['type' => 'quote', 'paragraphs' => ['A quoted line.', 'And a second.'], 'citation' => 'A source'],
                ['type' => 'quote', 'paragraphs' => ['A quote without a source.']],
                ['type' => 'table', 'header' => ['Plan', 'Price'], 'rows' => [['Starter', '19'], ['Team', '49']]],
            ]),
            self::section('shapes', [
                ['type' => 'separator'], ['type' => 'spacer', 'size' => 'small'], ['type' => 'spacer', 'size' => 'medium'], ['type' => 'spacer', 'size' => 'large'],
                ['type' => 'image', 'attachment_id' => 42, 'alt' => self::LIBRARY_ALT[42], 'align' => 'none'],
                ['type' => 'image', 'attachment_id' => 43, 'alt' => self::LIBRARY_ALT[43], 'caption' => 'A caption under it', 'align' => 'center'],
                ['type' => 'group', 'children' => [self::h(3, 'Inside a group'), self::p('Grouped text.'), self::buttons([['Go', '/go', 'fill']], 'center')]],
            ], ['tone' => 'surface']),
            self::section('layout', [
                ['type' => 'columns', 'widths' => [60, 40], 'columns' => [['children' => [self::h(3, 'Left'), self::p('Left text.')]], ['children' => [self::p('Right text.')]]]],
                ['type' => 'columns', 'columns' => [['children' => [self::p('One.')]], ['children' => [self::p('Two.')]], ['children' => [self::p('Three.')]]]],
                ['type' => 'columns', 'widths' => [25, 25, 25, 25], 'columns' => [['children' => [self::p('A.')]], ['children' => [self::p('B.')]], ['children' => [self::p('C.')]], ['children' => [self::p('D.')]]]],
            ], ['tone' => 'inverse']),
        ]);

        // ---- the hero ----
        $copy = [self::h(1, 'Websites that earn their keep'), self::p('We design, build and look after sites for small businesses.'), self::buttons([['See pricing', '#more', 'fill'], ['Talk to us', '/contact', 'outline']])];
        $more = self::section('more', [self::h(2, 'More below'), self::p('The rest of the page.')]);
        foreach ([['soft', 'center'], ['strong', 'top'], ['none', 'bottom'], [null, null]] as [$overlay, $focal]) {
            $background = ['attachment_id' => 42];
            if ($overlay !== null) {
                $background['overlay'] = $overlay;
            }
            if ($focal !== null) {
                $background['focal'] = $focal;
            }
            $add('hero-background-' . ($overlay ?? 'default-overlay'), [self::section('top', [self::hero('background', $copy)], [
                'width' => 'full', 'tone' => 'inverse', 'background' => $background, 'spacing' => 'xl', 'align' => 'center', 'min_height' => 'screen',
            ]), $more]);
        }
        $add('hero-background-without-a-picture', [self::section('top', [self::hero('background', $copy)], ['tone' => 'inverse', 'spacing' => 'xl', 'align' => 'center']), $more]);
        $add('hero-split', [self::section('top', [self::hero('split', array_merge($copy, [['type' => 'image', 'attachment_id' => 42, 'alt' => self::LIBRARY_ALT[42], 'align' => 'center']]))], ['spacing' => 'xl']), $more]);
        $add('hero-split-without-a-picture', [self::section('top', [self::hero('split', $copy)]), $more]);
        $centred = [self::section('top', [self::hero('centered', array_merge($copy, [self::buttons([['Start now', '/start', 'fill']], 'center')]))], ['tone' => 'surface', 'align' => 'center', 'spacing' => 'lg']), $more];
        $add('hero-centered', $centred);
        $add('hero-centered-full-width', $centred, [], [], 'full_width');
        $add('hero-centered-on-a-block-theme', $centred, [], ['block_theme' => true], 'canvas');
        $add('hero-centered-another-request', $centred, [], ['request_id' => self::REQ_B]);

        // ---- the call to action ----
        $callToAction = static fn (): array => [self::h(2, 'Ready when you are'), self::p('Start a trial today and keep everything if you stay.'), self::buttons([['Start free', '/signup', 'fill'], ['Book a call', 'https://example.com/call', 'text']])];
        $add('cta-band', [
            self::section('join', [self::pattern('cta', 'band', 'children', $callToAction())], ['tone' => 'accent', 'align' => 'center']),
            self::section('join-dark', [self::pattern('cta', 'band', 'children', $callToAction())], ['tone' => 'inverse', 'align' => 'center', 'spacing' => 'xl']),
            self::section('join-light', [self::pattern('cta', 'band', 'children', [self::h(2, 'A short one')])], ['tone' => 'surface']),
        ]);

        // ---- the features ----
        $icons = OutlineV3Schema::ICONS;
        $items = static function (int $count, int $from = 0) use ($icons): array {
            $out = [];
            for ($i = 0; $i < $count; $i++) {
                $n     = $from + $i;
                $out[] = ['icon' => $icons[$n % count($icons)], 'title' => 'Feature ' . ($n + 1), 'text' => 'What feature ' . ($n + 1) . ' does for you.'];
            }

            return $out;
        };
        // The icons run on from one case to the next, so that the three cases together name every icon of the list.
        $next = 0;
        foreach (['split' => [3, 4, 8], 'bento' => [3, 5, 8], 'grid' => [3, 6, 7]] as $variant => $counts) {
            $sections = [];
            foreach ($counts as $i => $count) {
                $sections[] = self::section($variant . '-' . $count, [self::h(2, ucfirst($variant) . ' with ' . $count), self::pattern('features', $variant, 'features', $items($count, $next))], ['tone' => OutlineV3Schema::TONES[$i % 4]]);
                $next += $count;
            }
            $add('features-' . $variant . '-' . implode('-', $counts), $sections);
        }

        // ---- the stats ----
        $stat   = static fn (int $value, ?string $suffix, string $label): array => $suffix === null ? ['value' => $value, 'label' => $label] : ['value' => $value, 'suffix' => $suffix, 'label' => $label];
        $add('stats-counters-2-3-4', [
            self::section('two', [self::pattern('stats', 'counters', 'stats', [$stat(0, null, 'a zero'), $stat(999999999, '+', 'the largest')])], ['tone' => 'surface']),
            self::section('three', [self::pattern('stats', 'counters', 'stats', [$stat(1200, '+', 'sites launched'), $stat(98, '%', 'would recommend us'), $stat(24, 'h', 'average reply time')])], ['tone' => 'inverse']),
            self::section('four', [self::pattern('stats', 'counters', 'stats', [$stat(5, null, 'years'), $stat(40, 'k', 'visits'), $stat(7, 'x', 'faster'), $stat(100, '%', 'secure')])], ['tone' => 'accent']),
        ]);

        // ---- the logos ----
        $logos = static function (array $ids): array {
            return array_map(static fn (int $id): array => ['attachment_id' => $id, 'alt' => self::LIBRARY_ALT[$id]], $ids);
        };
        $add('logos-row-3-and-10', [
            self::section('few', [self::pattern('logos', 'row', 'logos', $logos([101, 102, 103]))], ['tone' => 'surface']),
            self::section('many', [self::pattern('logos', 'row', 'logos', $logos([101, 102, 103, 104, 105, 106, 107, 108, 109, 110]))], ['tone' => 'ground']),
        ]);

        // ---- the testimonials ----
        $say = static function (int $n, bool $picture, bool $role): array {
            $out = ['quote' => 'Quote number ' . $n . ' about the work.', 'name' => 'Customer ' . $n];
            if ($role) {
                $out['role'] = 'Role ' . $n;
            }
            if ($picture) {
                $out['attachment_id'] = 200 + $n;
            }

            return $out;
        };
        $add('testimonials-featured-1-2-4-6', [
            self::section('one', [self::pattern('testimonials', 'featured', 'testimonials', [$say(1, false, false)])]),
            self::section('two', [self::pattern('testimonials', 'featured', 'testimonials', [$say(1, true, true), $say(2, true, false)])], ['tone' => 'surface']),
            self::section('four', [self::pattern('testimonials', 'featured', 'testimonials', [$say(1, true, true), $say(2, false, true), $say(3, true, false), $say(4, false, false)])], ['tone' => 'inverse']),
            self::section('six', [self::pattern('testimonials', 'featured', 'testimonials', [$say(1, true, true), $say(2, true, true), $say(3, true, true), $say(4, true, true), $say(5, true, true), $say(6, true, true)])], ['tone' => 'accent']),
        ]);

        // ---- the pricing ----
        $plan = static function (string $name, string $price, ?string $period, int $features, bool $highlighted, ?bool $mark = null) : array {
            $out = ['name' => $name, 'price' => $price];
            if ($period !== null) {
                $out['period'] = $period;
            }
            $list = [];
            for ($i = 1; $i <= $features; $i++) {
                $list[] = 'Included thing ' . $i;
            }
            $out['features'] = $list;
            $out['cta']      = ['text' => 'Choose ' . $name, 'url' => '/buy/' . strtolower($name)];
            if ($mark !== null) {
                $out['highlighted'] = $mark;
            } elseif ($highlighted) {
                $out['highlighted'] = true;
            }

            return $out;
        };
        $add('pricing-three-plans-1-3-4', [
            self::section('solo', [self::pattern('pricing', 'three-plans', 'plans', [$plan('Solo', '29', 'month', 1, true)])]),
            self::section('trio', [self::pattern('pricing', 'three-plans', 'plans', [$plan('Starter', '19', 'month', 2, false, false), $plan('Team', '49', 'month', 3, true), $plan('Business', '99', 'year', 8, false)])], ['tone' => 'surface', 'spacing' => 'xl']),
            self::section('quartet', [self::pattern('pricing', 'three-plans', 'plans', [$plan('Free', '0', null, 1, false), $plan('Plus', '12', 'month', 4, false), $plan('Pro', '48', 'month', 5, true), $plan('Max', '120', 'month', 6, false)])], ['tone' => 'inverse']),
        ]);

        // ---- the faq ----
        $questions = static function (int $count): array {
            $out = [];
            for ($i = 1; $i <= $count; $i++) {
                $out[] = ['question' => 'Question number ' . $i . '?', 'answer' => 'The answer to question number ' . $i . '.'];
            }

            return $out;
        };
        $add('faq-accordion-1-and-12', [
            self::section('short', [self::pattern('faq', 'accordion', 'faq', $questions(1))]),
            self::section('long', [self::pattern('faq', 'accordion', 'faq', $questions(12))], ['tone' => 'inverse']),
        ]);
        $faqPage = [self::section('questions', [self::h(2, 'Questions we hear'), self::pattern('faq', 'accordion', 'faq', $questions(3))], ['tone' => 'surface'])];
        $add('faq-at-elementor-3-20-4', $faqPage, [], ['elementor_version' => '3.20.4']);
        $add('faq-at-elementor-3-35-9', $faqPage, [], ['elementor_version' => '3.35.9']);
        $add('faq-at-elementor-4-3-4', $faqPage, [], ['elementor_version' => '4.3.4']);

        // ---- text that needs escaping, in every pattern that holds text ----
        $odd = 'Fish & Chips [1] "quoted" \'single\' \u{2014} caf\u{00E9} \u{201C}Ca va\u{201D} 100% /slash\\';
        $odd = str_replace(['\u{2014}', '\u{00E9}', '\u{201C}', '\u{201D}'], ["\u{2014}", "\u{00E9}", "\u{201C}", "\u{201D}"], $odd);
        $add('escapes-in-every-pattern', [
            self::section('odd', [
                self::h(2, $odd), self::p($odd), self::buttons([[$odd, '/odd', 'fill']]),
                self::pattern('features', 'grid', 'features', [['icon' => 'star', 'title' => $odd, 'text' => $odd], ['icon' => 'heart', 'title' => 'Two', 'text' => $odd], ['icon' => 'leaf', 'title' => 'Three', 'text' => 'Three text']]),
                self::pattern('stats', 'counters', 'stats', [$stat(3, '%', $odd), $stat(4, null, $odd)]),
                self::pattern('testimonials', 'featured', 'testimonials', [['quote' => $odd, 'name' => $odd, 'role' => $odd]]),
                self::pattern('pricing', 'three-plans', 'plans', [['name' => $odd, 'price' => '$9', 'period' => $odd, 'features' => [$odd], 'cta' => ['text' => $odd, 'url' => '/odd']]]),
                self::pattern('faq', 'accordion', 'faq', [['question' => $odd, 'answer' => $odd]]),
            ]),
        ]);

        // ---- one page at several sets of active breakpoints ----
        $probe = [
            self::section('top', [self::hero('split', array_merge($copy, [['type' => 'image', 'attachment_id' => 42, 'alt' => self::LIBRARY_ALT[42], 'align' => 'center']]))], ['spacing' => 'xl']),
            self::section('more', [self::h(2, 'Responsive probe'), self::pattern('features', 'grid', 'features', $items(4)), self::pattern('stats', 'counters', 'stats', [$stat(10, '+', 'one'), $stat(20, '+', 'two'), $stat(30, '+', 'three')])], ['tone' => 'surface']),
            self::section('cols', [['type' => 'columns', 'widths' => [50, 50], 'columns' => [['children' => [self::p('Left.')]], ['children' => [self::p('Right.')]]]], self::pattern('logos', 'row', 'logos', $logos([101, 102, 103]))], ['tone' => 'inverse', 'spacing' => 'lg']),
        ];
        $add('responsive-probe-mobile-and-tablet', $probe, [], ['active_breakpoints' => self::BOTH]);
        $add('responsive-probe-mobile', $probe, [], ['active_breakpoints' => ['mobile']]);
        $add('responsive-probe-tablet', $probe, [], ['active_breakpoints' => ['tablet']]);
        $add('responsive-probe-no-breakpoints', $probe, [], ['active_breakpoints' => []]);
        $add('responsive-probe-no-breakpoints-reported', $probe, [], ['active_breakpoints' => null]);
        $add('responsive-probe-with-extra-breakpoints', $probe, [], ['active_breakpoints' => ['mobile', 'mobile_extra', 'tablet', 'tablet_extra', 'laptop', 'widescreen']]);

        // ---- the full landing page of the shared table, at three sets of breakpoints ----
        $shared = json_decode((string) file_get_contents(self::SHARED_CASES), true, 512, JSON_THROW_ON_ERROR);
        foreach ($shared['cases'] as $row) {
            if ($row['name'] === 'v3-full-landing-page') {
                $landing = json_decode((string) $row['input'], true, 512, JSON_THROW_ON_ERROR);
                foreach ([['full-landing-page-mobile-and-tablet', self::BOTH], ['full-landing-page-mobile', ['mobile']], ['full-landing-page-no-breakpoints', null]] as [$name, $set]) {
                    $cases[] = ['name' => $name, 'page' => $landing, 'facts' => ['active_breakpoints' => $set]];
                }
            }
        }

        return $cases;
    }

    /**
     * One page-create input of outline version 3, in the order the control plane
     * sends it, with the page frame as its last member.
     *
     * @param list<array<string,mixed>> $outline Sections.
     * @param array<string,mixed>       $design  Options of design().
     * @param string                    $frame   The page frame.
     * @return array<string,mixed>
     */
    private static function page(array $outline, array $design, string $frame): array
    {
        $palette = self::PALETTES[$design['palette'] ?? 'studio'];
        $fonts   = $design['fonts'] ?? ['Manrope', 'Source Sans 3'];

        return [
            'post_type'        => 'page',
            'editor'           => 'builder:elementor',
            'elementor_format' => 'classic',
            'title'            => 'Launch page',
            'outline_version'  => 3,
            'design'           => [
                'colors'  => array_combine(OutlineV3Schema::COLOUR_ROLES, $palette),
                'fonts'   => ['heading' => $fonts[0], 'body' => $fonts[1]],
                'scale'   => ['base_px' => $design['base'] ?? 17, 'ratio' => $design['ratio'] ?? 'major_third'],
                'radius'  => $design['radius'] ?? 'md',
                'density' => $design['density'] ?? 'normal',
            ],
            'outline'          => $outline,
            'page_frame'       => $frame,
        ];
    }

    /**
     * @param string                    $anchor   The anchor.
     * @param list<array<string,mixed>> $children Blocks.
     * @param array<string,mixed>       $options  Members of the section other than the anchor and the children.
     * @return array<string,mixed>
     */
    private static function section(string $anchor, array $children, array $options = []): array
    {
        return array_merge(['type' => 'section', 'anchor' => $anchor], $options, ['children' => $children]);
    }

    /**
     * @return array<string,mixed>
     */
    private static function h(int $level, string $text): array
    {
        return ['type' => 'heading', 'level' => $level, 'text' => $text];
    }

    /**
     * @return array<string,mixed>
     */
    private static function p(string $text): array
    {
        return ['type' => 'paragraph', 'text' => $text];
    }

    /**
     * @param list<array{0:string,1:string,2:string}> $buttons Text, address and style of each.
     * @param string|null                             $align   left or center; left out when null.
     * @return array<string,mixed>
     */
    private static function buttons(array $buttons, ?string $align = null): array
    {
        $out = ['type' => 'buttons'];
        if ($align !== null) {
            $out['align'] = $align;
        }
        $out['buttons'] = array_map(static fn (array $b): array => ['text' => $b[0], 'url' => $b[1], 'style' => $b[2]], $buttons);

        return $out;
    }

    /**
     * @param list<array<string,mixed>> $children Blocks.
     * @return array<string,mixed>
     */
    private static function hero(string $variant, array $children): array
    {
        return self::pattern('hero', $variant, 'children', $children);
    }

    /**
     * @param array<mixed> $content The pattern's blocks or items.
     * @return array<string,mixed>
     */
    private static function pattern(string $pattern, string $variant, string $member, array $content): array
    {
        return ['type' => 'pattern', 'pattern' => $pattern, 'variant' => $variant, $member => $content];
    }

    // -------------------------------------------------------------------------
    // The precheck
    // -------------------------------------------------------------------------

    /**
     * The facts of a case: the defaults, the case's own and the resolved
     * images of the page, in the order the command resolves them.
     *
     * @param array<string,mixed> $page  The input.
     * @param array<string,mixed> $given The facts the case sets.
     * @return array<string,mixed>
     */
    private function factsOf(array $page, array $given): array
    {
        $checked = PageCreateBuilder::validate(json_decode((string) json_encode($page, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE), false, 32));
        $this->assertArrayHasKey('spec', $checked, 'the input is valid: ' . json_encode($checked));
        $media = array_map([self::class, 'mediaFact'], PageCreateBuilder::mediaIds($checked['spec']));

        return [
            'elementor_version'  => $given['elementor_version'] ?? '3.35.9',
            'layout'             => 'containers',
            'block_theme'        => $given['block_theme'] ?? false,
            'active_breakpoints' => array_key_exists('active_breakpoints', $given) ? $given['active_breakpoints'] : self::BOTH,
            'media'              => $media,
            'request_id'         => $given['request_id'] ?? self::REQ_A,
        ];
    }

    /**
     * What the site resolves an attachment id to.
     *
     * @return array<string,mixed>
     */
    private static function mediaFact(int $id): array
    {
        $mime = self::MIME_OF[$id] ?? 'image/jpeg';

        return [
            'id'           => $id,
            'url'          => sprintf('https://example.com/wp-content/uploads/2026/10/img-%d.%s', $id, self::EXTENSION_OF[$mime]),
            'filename'     => sprintf('img-%d.%s', $id, self::EXTENSION_OF[$mime]),
            'mime'         => $mime,
            'width'        => 1600,
            'height'       => 900,
            'modified_gmt' => '2026-10-01 09:00:00',
        ];
    }

    /**
     * The precheck of an input on a site with these facts.
     *
     * @param string              $input The input text.
     * @param array<string,mixed> $facts The case's facts.
     * @return array<string,mixed>
     */
    private function precheck(string $input, array $facts): array
    {
        $this->api->version           = $facts['elementor_version'];
        $this->api->activeBreakpoints = $facts['active_breakpoints'];
        $this->blockTheme             = $facts['block_theme'];

        $checked = PageCreateBuilder::validate(json_decode($input, false, 32));
        $this->assertArrayHasKey('spec', $checked, 'the input is valid: ' . json_encode($checked));
        $adapter = new ElementorAdapter($this->api, self::PRINCIPAL);

        return BuilderPageCreate::precheck($checked['spec'], $adapter, $adapter->facts(), $facts['media'], $facts['request_id']);
    }

    /**
     * The whole fixture.
     *
     * @return array<string,mixed>
     */
    private function document(): array
    {
        $cases   = [];
        $refused = [];
        foreach (self::inputs() as $row) {
            $input  = (string) json_encode($row['page'], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
            $facts  = $this->factsOf($row['page'], $row['facts']);
            $answer = $this->precheck($input, $facts);
            if (isset($answer['refusal'])) {
                $refused[$row['name']] = $answer['refusal'];
                continue;
            }
            $cases[] = [
                'name'           => $row['name'],
                'input'          => $input,
                'facts'          => $facts,
                'builder_facts'  => $answer['builder_facts'],
                'preview_digest' => $answer['preview_digest'],
                'tree'           => $answer['tree'],
            ];
        }
        $this->assertSame([], $refused, 'inputs of the golden the precheck refused');

        return [
            'note'        => 'Golden trees of designed pages (outline version 3) from the wpmgr/page-create precheck, regenerated by DesignedTreeGoldenTest with WPMGR_WRITE_FIXTURES=1 and never edited by hand. '
                . 'Each case holds "input", the exact JSON text of the page-create input with the page frame the control plane appended as its last member; '
                . '"facts", what the site and the request gave the compile: the Elementor release, the layout (always containers), whether the theme is a block theme, '
                . 'the breakpoints Elementor has active (a list in Elementor\'s own order, [] for none, null when the site could not say, which the compile reads as none), '
                . 'the resolved images in the order the input names them (backgrounds, logos and avatars included) and the request id the node ids derive from; '
                . '"builder_facts", the precheck answer\'s block of the facts it resolved: the theme fact, the page frame and the active breakpoints the tree was compiled for; '
                . '"preview_digest", the digest the precheck answered, which binds the tree\'s canonical bytes (json_encode with default flags) with the title, the post type and the release; '
                . 'and "tree", the Elementor elements the precheck built. '
                . 'The tree is a function of the input, the design, the active breakpoints, the resolved images and the request id alone: '
                . 'the Elementor release, the theme and the page frame change the digest or builder_facts and never the tree. '
                . '"library_alt" is the alt text each image has in the media library; the precheck refuses a logo or an image whose alt text is another, and no tree holds it. '
                . 'A number is a JSON integer or text, and the file holds no float, so it reads the same under any float setting.',
            'library_alt' => (object) array_map('strval', self::LIBRARY_ALT),
            'cases'       => $cases,
        ];
    }

    /**
     * The fixture as text: the file pretty-printed, each tree as a list with
     * one top-level section to a line, so a change to one section is a change
     * to one line.
     */
    private function fixtureText(): string
    {
        $flags = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
        $doc   = $this->document();
        $lines = [];
        foreach ($doc['cases'] as $i => $case) {
            $lines[$i]             = array_map(static fn (array $section): string => json_encode($section, $flags), $case['tree']);
            $doc['cases'][$i]['tree'] = '@@tree' . $i . '@@';
        }
        $text = json_encode($doc, JSON_PRETTY_PRINT | $flags);
        foreach ($lines as $i => $sections) {
            $text = str_replace('"@@tree' . $i . '@@"', "[\n            " . implode(",\n            ", $sections) . "\n        ]", $text);
        }

        return $text . "\n";
    }

    /**
     * The fixture, read.
     *
     * @return array<string,mixed>
     */
    private function fixture(): array
    {
        $this->assertFileExists(self::FIXTURE, 'elementor-designed-containers.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $raw = file_get_contents(self::FIXTURE);
        $this->assertIsString($raw);

        return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    }

    // -------------------------------------------------------------------------
    // Reading a tree
    // -------------------------------------------------------------------------

    /**
     * Every node of an outline, depth first: sections, their children, the
     * children of groups, columns and patterns.
     *
     * @param array<mixed>      $nodes Nodes.
     * @param callable(array<mixed>):void $visit Called with each.
     */
    private static function walk(array $nodes, callable $visit): void
    {
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                continue;
            }
            $visit($node);
            self::walk($node['children'] ?? [], $visit);
            foreach ($node['columns'] ?? [] as $column) {
                self::walk(is_array($column) ? ($column['children'] ?? []) : [], $visit);
            }
        }
    }

    /**
     * The keys of a tree's settings that override a breakpoint, for these breakpoints.
     *
     * @param array<mixed> $tree   Elements.
     * @param list<string> $names  Breakpoint names.
     * @return list<string> "id:key" of each.
     */
    private static function responsiveKeys(array $tree, array $names): array
    {
        $found = [];
        foreach ($tree as $node) {
            foreach (array_keys($node['settings'] ?? []) as $key) {
                foreach ($names as $name) {
                    if (str_ends_with((string) $key, '_' . $name)) {
                        $found[] = $node['id'] . ':' . $key;
                    }
                }
            }
            $found = array_merge($found, self::responsiveKeys($node['elements'] ?? [], $names));
        }

        return $found;
    }

    /**
     * A tree less the settings that override these breakpoints.
     *
     * @param array<mixed> $tree  Elements.
     * @param list<string> $names Breakpoint names.
     * @return array<mixed>
     */
    private static function withoutResponsiveKeys(array $tree, array $names): array
    {
        foreach ($tree as $i => $node) {
            foreach (array_keys($node['settings'] ?? []) as $key) {
                foreach ($names as $name) {
                    if (str_ends_with((string) $key, '_' . $name)) {
                        unset($tree[$i]['settings'][$key]);
                    }
                }
            }
            $tree[$i]['elements'] = self::withoutResponsiveKeys($node['elements'] ?? [], $names);
        }

        return $tree;
    }

    /**
     * @param array<mixed> $tree Elements.
     * @return list<string>
     */
    private static function idsOf(array $tree): array
    {
        $ids = [];
        foreach ($tree as $node) {
            $ids[] = $node['id'];
            foreach ($node['settings']['tabs'] ?? [] as $tab) {
                $ids[] = $tab['_id'];
            }
            $ids = array_merge($ids, self::idsOf($node['elements'] ?? []));
        }

        return $ids;
    }

    /**
     * A tree with every element id and repeater id blanked.
     *
     * @param array<mixed> $tree Elements.
     * @return array<mixed>
     */
    private static function withoutIds(array $tree): array
    {
        foreach ($tree as $i => $node) {
            $tree[$i]['id'] = '';
            foreach (array_keys($node['settings']['tabs'] ?? []) as $k) {
                $tree[$i]['settings']['tabs'][$k]['_id'] = '';
            }
            $tree[$i]['elements'] = self::withoutIds($node['elements'] ?? []);
        }

        return $tree;
    }

    /**
     * @param array<mixed> $tree Elements.
     * @return array<mixed>|null
     */
    private static function firstWidget(array $tree, string $type): ?array
    {
        foreach ($tree as $node) {
            if (($node['widgetType'] ?? null) === $type) {
                return $node;
            }
            $inner = self::firstWidget($node['elements'] ?? [], $type);
            if ($inner !== null) {
                return $inner;
            }
        }

        return null;
    }

    /**
     * @param array<int,mixed> $values The values.
     * @return array<int,mixed>
     */
    private static function sorted(array $values): array
    {
        sort($values);

        return $values;
    }

    /**
     * The paths of every float in a value.
     *
     * @param mixed  $value The value.
     * @param string $path  The path so far.
     * @return list<string>
     */
    private static function floatPaths(mixed $value, string $path = ''): array
    {
        if (is_float($value)) {
            return [$path];
        }
        $found = [];
        if (is_array($value)) {
            foreach ($value as $key => $item) {
                $found = array_merge($found, self::floatPaths($item, $path . '/' . $key));
            }
        }

        return $found;
    }
}
