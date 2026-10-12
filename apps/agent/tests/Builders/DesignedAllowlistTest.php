<?php
/**
 * The one allowlist of a designed page, ElementorClassicMapper::designedKeys():
 * how it merges the mapper's keys with the leaf style's and the patterns',
 * the designed trees it admits on each set of active breakpoints, what it
 * still refuses, and the phone direction of a plain row.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\DataProvider;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorLeafStyle;
use WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class DesignedAllowlistTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000400';

    private const FACE_URL = 'https://example.com/wp-content/uploads/2026/10/face-5-150x150.png';

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

    /** @return array<int,array<string,mixed>> Resolved facts of the one picture the testimonials use. */
    private static function media(): array
    {
        return [
            5 => [
                'id'           => 5,
                'url'          => self::FACE_URL,
                'filename'     => 'face-5.png',
                'mime'         => 'image/png',
                'width'        => 150,
                'height'       => 150,
                'modified_gmt' => '2026-10-01 09:00:00',
                'alt'          => '',
            ],
        ];
    }

    /**
     * @param list<string>|null $breakpoints Active breakpoints.
     * @return ElementorSectionCompiler
     */
    private static function sections(?array $breakpoints): ElementorSectionCompiler
    {
        return new ElementorSectionCompiler(self::tokens(), $breakpoints, self::media());
    }

    /**
     * @param string $id   Widget id.
     * @param string $text Its title.
     * @return array<string,mixed> A plain heading widget.
     */
    private static function heading(string $id, string $text): array
    {
        return ['id' => $id, 'elType' => 'widget', 'settings' => ['title' => $text, 'header_size' => 'h2'], 'elements' => [], 'widgetType' => 'heading'];
    }

    /**
     * The designed trees of one site: a columns row, a group, a testimonials
     * pattern (pictured and not) and a faq pattern.
     *
     * @param list<string>|null $breakpoints Active breakpoints.
     * @return array<string,array<string,mixed>>
     */
    private static function designedTrees(?array $breakpoints): array
    {
        $sections = self::sections($breakpoints);
        $patterns = new ElementorPatternCompiler(self::tokens(), $sections, self::media());
        $ids      = new IdSeed(self::REQUEST);
        $columns  = [];
        foreach ([55, 45] as $k => $width) {
            $columns[] = $sections->column($ids->next('row.columns[' . $k . ']'), $width, [self::heading($ids->next('row.columns[' . $k . '].h'), 'Column ' . $k)]);
        }
        $testimonials = [
            ['quote' => 'Best flat white in town.', 'name' => 'Asha Rao', 'role' => 'Regular', 'attachment_id' => 5],
            ['quote' => 'Fast, warm and never rushed.', 'name' => 'Joao Silva', 'role' => 'Courier'],
            ['quote' => 'A proper local roaster.', 'name' => 'Sam Okoye'],
        ];
        $faq = [
            ['question' => 'Do you ship?', 'answer' => 'Yes, worldwide.'],
            ['question' => 'Is it fresh?', 'answer' => 'Roasted weekly.'],
        ];

        return [
            'row'          => $sections->row($ids->next('row'), $columns),
            'group'        => $sections->group($ids->next('group'), [self::heading($ids->next('group.h'), 'Grouped')]),
            'testimonials' => $patterns->testimonials($ids, 'outline[0].children[0]', ['type' => 'pattern', 'pattern' => 'testimonials', 'variant' => 'featured', 'testimonials' => $testimonials], 'ground'),
            'faq'          => $patterns->faq($ids, 'outline[1].children[0]', ['type' => 'pattern', 'pattern' => 'faq', 'variant' => 'accordion', 'faq' => $faq], 'surface'),
            'pricing'      => $patterns->pricing($ids, 'outline[2].children[0]', ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => [
                ['name' => 'Starter', 'price' => '$19', 'features' => ['Email support'], 'cta' => ['text' => 'Start', 'url' => 'https://example.com/start']],
            ]], 'ground'),
            'stats'        => $patterns->stats($ids, 'outline[3].children[0]', ['type' => 'pattern', 'pattern' => 'stats', 'variant' => 'counters', 'stats' => [
                ['value' => 0, 'label' => 'Starting'],
                ['value' => 999999999, 'label' => 'Maximum'],
            ]], 'ground'),
        ];
    }

    /**
     * @param array<string,mixed> $tree       One element.
     * @param list<string>|null   $breakpoints The site's active breakpoints.
     * @return array{code:string,detail:string}|null
     */
    private static function check(array $tree, ?array $breakpoints): ?array
    {
        return LeafPolicy::checkTree([$tree], LeafPolicy::forBreakpoints(ElementorClassicMapper::designedKeys(), $breakpoints), ElementorClassicMapper::LEAF_RULES);
    }

    /** @return array<string,mixed> A normalised columns node of two headings. */
    private static function columnsNode(): array
    {
        return [
            'type'    => 'columns',
            'widths'  => [55, 45],
            'columns' => [
                [['type' => 'heading', 'level' => 2, 'text' => 'Copy column']],
                [['type' => 'heading', 'level' => 2, 'text' => 'Image column']],
            ],
        ];
    }

    /**
     * @param array<mixed> $value A value.
     * @return string Its bytes.
     */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    // -------------------------------------------------------------------------
    // The merge rule
    // -------------------------------------------------------------------------

    public function test_a_leaf_style_widens_a_leaf_pin_and_a_container_keeps_its_base_pin(): void
    {
        $keys = ElementorClassicMapper::designedKeys();

        // A heading of a designed section is centred as well as left.
        $this->assertSame('enum:left|center', $keys['heading']['align']);
        $this->assertSame('enum:center', $keys['text-editor']['align']);
        $this->assertSame('link', $keys['button']['link']);
        // A section's own width and direction stay as open as the mapper's.
        $this->assertSame('enum', $keys['container']['content_width']);
        $this->assertSame('enum', $keys['container']['flex_direction']);
        $this->assertSame('enum', $keys['container']['flex_direction_mobile']);
        foreach (['flex_gap' => 'gaps', 'flex_gap_tablet' => 'gaps', 'flex_gap_mobile' => 'gaps', 'width' => 'slider:%', 'width_mobile' => 'slider:%', 'flex_wrap' => 'enum:wrap'] as $path => $pin) {
            $this->assertSame($pin, $keys['container'][$path] ?? null, $path);
        }
        $this->assertSame('image', $keys['testimonial']['testimonial_image']);
        $this->assertSame('html', $keys['accordion']['tabs.0.tab_title']);
        // Every path the mapper lists stays listed, and every path a leaf style writes is listed.
        foreach ([ElementorClassicMapper::ALLOWED_KEYS, ElementorLeafStyle::KEYS] as $source) {
            foreach ($source as $type => $paths) {
                foreach (array_keys($paths) as $path) {
                    $this->assertArrayHasKey($path, $keys[$type], $type . ' ' . $path);
                }
            }
        }
    }

    public function test_no_value_a_listed_pin_takes_is_lost_in_the_merge(): void
    {
        $keys = ElementorClassicMapper::designedKeys();

        foreach ([ElementorClassicMapper::ALLOWED_KEYS, ElementorLeafStyle::KEYS] as $source) {
            foreach ($source as $type => $paths) {
                foreach ($paths as $path => $pin) {
                    $from   = explode(':', $pin, 2);
                    $merged = explode(':', $keys[$type][$path], 2);
                    if ($from[0] !== $merged[0] || !isset($from[1], $merged[1])) {
                        continue;
                    }
                    foreach (explode('|', $from[1]) as $value) {
                        $this->assertContains($value, explode('|', $merged[1]), $type . ' ' . $path);
                    }
                }
            }
        }
    }

    /** @return array<string,array{0:string,1:string,2:string}> A listed pin, a leaf style's pin, the merged pin. */
    public static function widenings(): array
    {
        return [
            'two lists join'          => ['enum:left', 'enum:center', 'enum:left|center'],
            'a shared value once'     => ['enum:left', 'enum:left|center', 'enum:left|center'],
            'units join'              => ['slider:px', 'slider:em|px', 'slider:px|em'],
            'the open pin stays open' => ['enum', 'enum:center', 'enum'],
            'the open style pin'      => ['enum:md', 'enum', 'enum'],
            'another name'            => ['text', 'enum', 'enum'],
            'the same pin'            => ['colour', 'colour', 'colour'],
        ];
    }

    /**
     * @dataProvider widenings
     * @param string $listed The mapper's pin.
     * @param string $style  The leaf style's pin.
     * @param string $merged The pin of the designed allowlist.
     */
    public function test_a_leaf_style_pin_widens_a_listed_one(string $listed, string $style, string $merged): void
    {
        $widen = new \ReflectionMethod(ElementorClassicMapper::class, 'widen');

        $this->assertSame($merged, $widen->invoke(null, $listed, $style));
    }

    public function test_a_pattern_never_changes_a_pin_the_mapper_or_a_leaf_style_lists(): void
    {
        $keys = ElementorClassicMapper::designedKeys();

        foreach (ElementorPatternCompiler::KEYS as $type => $paths) {
            foreach ($paths as $path => $pin) {
                $base = ElementorClassicMapper::ALLOWED_KEYS[$type][$path] ?? null;
                $this->assertSame($base ?? $pin, $keys[$type][$path], $type . ' ' . $path);
            }
        }
    }

    // -------------------------------------------------------------------------
    // Designed trees on each set of active breakpoints
    // -------------------------------------------------------------------------

    /** @return array<string,array{0:list<string>|null}> */
    public static function breakpointSets(): array
    {
        return [
            'stock'       => [self::STOCK_BREAKPOINTS],
            'tablet only' => [['tablet']],
            'mobile only' => [['mobile']],
            'none'        => [null],
        ];
    }

    /**
     * @dataProvider breakpointSets
     * @param list<string>|null $active The site's active breakpoints.
     */
    public function test_every_designed_tree_passes_on_its_own_site(?array $active): void
    {
        foreach (self::designedTrees($active) as $name => $tree) {
            $answer = self::check($tree, $active);
            $this->assertNull($answer, $name . ': ' . ($answer['detail'] ?? ''));
        }
    }

    public function test_an_override_of_a_breakpoint_the_site_lacks_is_refused(): void
    {
        $stock = self::designedTrees(self::STOCK_BREAKPOINTS);

        // The phone direction, the phone gap and a column's phone width each need the mobile breakpoint.
        $this->assertSame(LeafPolicy::CODE_STYLE_KEY, self::check($stock['row'], ['tablet'])['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_STYLE_KEY, self::check($stock['row']['elements'][0], ['tablet'])['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_STYLE_KEY, self::check($stock['testimonials'], ['mobile'])['code'] ?? null);
        $this->assertNull(self::check($stock['row'], self::STOCK_BREAKPOINTS));
    }

    public function test_the_designed_allowlist_holds_a_gap_and_a_width_to_their_measured_shapes(): void
    {
        $row = self::designedTrees(self::STOCK_BREAKPOINTS)['row'];

        $gap                                  = $row;
        $gap['settings']['flex_gap']['size']  = '24';
        $width                                = $row;
        $width['elements'][1]['settings']['width']['sizes'] = [];
        $unit                                 = $row;
        $unit['elements'][0]['settings']['width_mobile']['unit'] = 'px';
        $wrap                                 = $row;
        $wrap['settings']['flex_wrap']        = 'nowrap';
        $stranger                             = $row;
        $stranger['settings']['flex_align_content'] = 'center';

        $this->assertSame(LeafPolicy::CODE_UNSAFE, self::check($gap, self::STOCK_BREAKPOINTS)['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_UNSAFE, self::check($width, self::STOCK_BREAKPOINTS)['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_UNSAFE, self::check($unit, self::STOCK_BREAKPOINTS)['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_UNSAFE, self::check($wrap, self::STOCK_BREAKPOINTS)['code'] ?? null);
        $this->assertSame(LeafPolicy::CODE_STYLE_KEY, self::check($stranger, self::STOCK_BREAKPOINTS)['code'] ?? null);
    }

    /** @return array<string,array{0:string,1:string,2:mixed,3:bool}> Settings leaves and whether their shapes are accepted. */
    public static function numericAndIdLeaves(): array
    {
        $cases  = [];
        $common = ['0;}body{display:none}.x{', '', ' 0', "0\n", 0, 0.0, true, null, [], ['value' => '0']];
        $groups = [
            'shadow' => [
                'paths' => ['container' => array_map(static fn ($key) => 'box_shadow_box_shadow.' . $key, ['horizontal', 'vertical', 'blur', 'spread'])],
                'valid' => ['0', '8', '24', '-8', '999', '-999', '000'],
                'bad'   => ['1000', '-1000', '+8', '8.5', '8px', '1e2', '--8'],
            ],
            'id' => [
                'paths' => [
                    'accordion' => array_map(static fn ($i) => 'tabs.' . $i . '._id', range(0, 11)),
                    'icon-list' => array_map(static fn ($i) => 'icon_list.' . $i . '._id', range(0, 7)),
                ],
                'valid' => [(new IdSeed(self::REQUEST))->item('outline[0]', 'tabs', 0), 'abcdef0', '0000000', '1234567'],
                'bad'   => ['abcdef', 'abcdef01', 'ABCDEF0', 'abcdefg', "abcdef0\n", ' abcdef0'],
            ],
            'counter' => [
                'paths' => ['counter' => ['ending_number']],
                'valid' => ['0', '24', '999999999', '000000000'],
                'bad'   => ['1000000000', '-1', '+1', '1.5', '1e2', '1px'],
            ],
        ];
        foreach ($groups as $group) {
            foreach ($group['paths'] as $type => $paths) {
                foreach ($paths as $path) {
                    foreach ([true => $group['valid'], false => array_merge($common, $group['bad'])] as $accepted => $values) {
                        foreach ($values as $i => $value) {
                            $cases[$type . ' ' . $path . ' ' . ($accepted ? 'valid' : 'invalid') . ' ' . $i] = [$type, $path, $value, (bool) $accepted];
                        }
                    }
                }
            }
        }

        return $cases;
    }

    /**
     * @param string $type     Element type.
     * @param string $path     Settings path.
     * @param mixed  $value    Settings value.
     * @param bool   $accepted Whether the shape is accepted.
     */
    #[DataProvider('numericAndIdLeaves')]
    public function test_designed_numeric_and_id_pins(string $type, string $path, $value, bool $accepted): void
    {
        $tree = ['id' => 'abcdef0', 'elType' => $type === 'container' ? 'container' : 'widget', 'settings' => [], 'elements' => []];
        if ($type !== 'container') {
            $tree['widgetType'] = $type;
        } else {
            $tree['settings']['box_shadow_box_shadow_type'] = 'yes';
        }
        $leaf =& $tree['settings'];
        foreach (explode('.', $path) as $key) {
            $leaf =& $leaf[$key];
        }
        $leaf = $value;
        unset($leaf);
        $answer = self::check($tree, self::STOCK_BREAKPOINTS);
        if ($accepted) {
            $this->assertNull($answer, $answer['detail'] ?? '');
        } else {
            $this->assertSame(LeafPolicy::CODE_UNSAFE, $answer['code'] ?? null);
        }
    }

    // -------------------------------------------------------------------------
    // Through the mapper
    // -------------------------------------------------------------------------

    public function test_the_mapper_checks_a_designed_page_against_the_designed_allowlist_of_its_site(): void
    {
        foreach (self::breakpointSets() as $name => [$active]) {
            $r = ElementorClassicMapper::map(['outline' => [['type' => 'group', 'children' => [self::columnsNode()]]]], new IdSeed(self::REQUEST), [], true, self::sections($active));
            $this->assertArrayHasKey('tree', $r, $name . ': ' . ($r['detail'] ?? ''));
            $this->assertNull(self::check($r['tree'][0], $active), $name);
        }

        // A designed page is checked for its compiler's breakpoints, whatever the plain ones say.
        $r = ElementorClassicMapper::map(['outline' => [self::columnsNode()]], new IdSeed(self::REQUEST), [], true, self::sections(self::STOCK_BREAKPOINTS), ['tablet']);
        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $this->assertSame('column', $r['tree'][0]['settings']['flex_direction_mobile'] ?? null);
    }

    public function test_a_plain_row_writes_its_phone_direction_only_where_the_site_has_the_mobile_breakpoint(): void
    {
        $spec   = ['outline' => [self::columnsNode()]];
        $before = ElementorClassicMapper::map($spec, new IdSeed(self::REQUEST), [], true);
        $stock  = ElementorClassicMapper::map($spec, new IdSeed(self::REQUEST), [], true, null, self::STOCK_BREAKPOINTS);

        $this->assertArrayHasKey('tree', $before);
        $this->assertSame(self::bytes($before), self::bytes($stock));
        $this->assertSame(['content_width', 'flex_direction', 'flex_direction_mobile', 'flex_gap'], array_keys($before['tree'][0]['settings']));
        foreach ([['tablet'], null] as $active) {
            $r = ElementorClassicMapper::map($spec, new IdSeed(self::REQUEST), [], true, null, $active);
            $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
            $this->assertSame(['content_width', 'flex_direction', 'flex_gap'], array_keys($r['tree'][0]['settings']));
            $fragment = ElementorClassicMapper::mapFragment([self::columnsNode()], new IdSeed(self::REQUEST), [], true, ElementorClassicMapper::CONTEXT_TOP, 'operations[0].', $active);
            $this->assertArrayHasKey('tree', $fragment, (string) ($fragment['detail'] ?? ''));
            $this->assertArrayNotHasKey('flex_direction_mobile', $fragment['tree'][0]['settings']);
        }
        $fragment = ElementorClassicMapper::mapFragment([self::columnsNode()], new IdSeed(self::REQUEST), [], true, ElementorClassicMapper::CONTEXT_TOP, 'operations[0].');
        $this->assertSame('column', $fragment['tree'][0]['settings']['flex_direction_mobile'] ?? null);
    }
}
