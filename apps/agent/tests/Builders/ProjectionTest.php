<?php
/**
 * Projection: the builder-neutral page structure, with kinds closed to the
 * contract, locked labels from WPMgr's map only, and the node and byte caps.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorPatternReader;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\Builders\Projection;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\Projection
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternReader
 */
final class ProjectionTest extends TestCase
{
    private const LABELS = [
        'form'                    => 'Form',
        'html'                    => 'Custom code',
        Projection::FALLBACK_LABEL => 'Element WPMgr does not edit',
    ];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000042';

    private const AT = 'outline[1].children[0]';

    private const PROBE_URL = 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png';

    /** WPMgr's words for a pattern container that was restructured by hand. */
    private const EDITED_WORDS = 'WPMgr pattern changed in Elementor';

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

    public function test_kinds_are_closed(): void
    {
        $p     = new Projection(self::LABELS);
        $kinds = array_values(array_diff(BuilderContract::KINDS, ['locked']));
        foreach ($kinds as $i => $kind) {
            if ($kind === 'pattern') {
                $p->addPattern('n' . $i, Projection::ROOT, 'three-plans', []);
                continue;
            }
            if ($kind === 'native') {
                $p->addNative('n' . $i, Projection::ROOT, 'counter', 'A counter');
                continue;
            }
            $p->add('n' . $i, Projection::ROOT, $kind, $kind === 'heading' ? 2 : null, [], []);
        }
        $p->addLocked('x1', Projection::ROOT, 'form');

        $out = $p->toArray(BuilderContract::MAX_STRUCTURE_NODES);
        $this->assertSame(array_merge($kinds, ['locked']), array_column($out['nodes'], 'kind'));

        $refused = ['locked', 'pattern', 'native', 'widget', 'Heading', 'heading ', 'text-editor', 'container', ''];
        foreach ($refused as $kind) {
            try {
                $p->add('r' . bin2hex($kind), Projection::ROOT, $kind, null, [], []);
                $this->fail('kind ' . var_export($kind, true) . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
        $this->assertSame(count($kinds) + 1, $p->toArray(500)['node_count'], 'a refused node adds nothing');
    }

    public function test_locked_label_comes_from_wpmgr_map(): void
    {
        $p = new Projection(self::LABELS);
        $p->addLocked('a1', Projection::ROOT, 'form');
        $p->addLocked('a2', Projection::ROOT, 'Spring sale <script>alert(1)</script>');
        $p->addLocked('a3', 'a1', 'testimonial');
        $p->addLocked('a4', Projection::ROOT, '');

        $out = $p->toArray(500);
        $this->assertSame(
            [
                ['ref' => 'a1', 'parent' => 'root', 'kind' => 'locked', 'label' => 'Form'],
                ['ref' => 'a2', 'parent' => 'root', 'kind' => 'locked', 'label' => 'Element WPMgr does not edit'],
                ['ref' => 'a3', 'parent' => 'a1', 'kind' => 'locked', 'label' => 'Element WPMgr does not edit'],
                ['ref' => 'a4', 'parent' => 'root', 'kind' => 'locked', 'label' => 'Element WPMgr does not edit'],
            ],
            $out['nodes']
        );
        $json = (string) json_encode($out);
        $this->assertStringNotContainsString('Spring', $json);
        $this->assertStringNotContainsString('testimonial', $json);

        foreach ([['form' => 'Form'], [Projection::FALLBACK_LABEL => ''], [Projection::FALLBACK_LABEL => 'x', 'form' => 7]] as $map) {
            try {
                new Projection($map);
                $this->fail('label map ' . (string) json_encode($map) . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_truncates_at_max_nodes_and_cap(): void
    {
        $small = new Projection(self::LABELS);
        for ($i = 0; $i < 10; $i++) {
            $small->add('s' . $i, Projection::ROOT, 'paragraph', null, ['text'], ['text' => 'Line ' . $i]);
        }
        $three = $small->toArray(3);
        $this->assertSame(10, $three['node_count']);
        $this->assertTrue($three['truncated']);
        $this->assertSame(['s0', 's1', 's2'], array_column($three['nodes'], 'ref'));
        $all = $small->toArray(10);
        $this->assertFalse($all['truncated']);
        $this->assertCount(10, $all['nodes']);
        $this->assertSame(['node_count' => 10, 'truncated' => true, 'nodes' => []], $small->toArray(0));
        $this->assertCount(10, $small->toArray(100000)['nodes']);

        // The node cap: never more than MAX_STRUCTURE_NODES, whatever is asked.
        $many = new Projection(self::LABELS);
        for ($i = 0; $i < 600; $i++) {
            $many->addLocked('m' . $i, Projection::ROOT, 'form');
        }
        $capped = $many->toArray(100000);
        $this->assertCount(BuilderContract::MAX_STRUCTURE_NODES, $capped['nodes']);
        $this->assertSame(600, $capped['node_count']);
        $this->assertTrue($capped['truncated']);

        // The byte cap: the encoded answer stays within 64 KiB, cut after a
        // whole node, and the next node would not have fitted.
        $big  = new Projection(self::LABELS);
        $text = str_repeat('é/<', 400);
        for ($i = 0; $i < 80; $i++) {
            $parent = $i === 0 ? Projection::ROOT : 'b0';
            $big->add('b' . $i, $parent, 'heading', 2, ['text'], ['text' => $text]);
        }
        $out     = $big->toArray(500);
        $encoded = (string) json_encode($out);
        $this->assertLessThanOrEqual(BuilderContract::MAX_STRUCTURE_BYTES, strlen($encoded));
        $this->assertTrue($out['truncated']);
        $this->assertSame(80, $out['node_count']);
        $kept = count($out['nodes']);
        $this->assertGreaterThan(0, $kept);
        $this->assertLessThan(80, $kept);
        $this->assertSame(array_map(static fn (int $i): string => 'b' . $i, range(0, $kept - 1)), array_column($out['nodes'], 'ref'));
        $next = (string) json_encode($big->toArray(500, PHP_INT_MAX)['nodes'][$kept]);
        $this->assertGreaterThan(BuilderContract::MAX_STRUCTURE_BYTES, strlen($encoded) + 1 + strlen($next));

        // A smaller budget, for a caller that wraps the answer, is honoured.
        $this->assertLessThanOrEqual(30000, strlen((string) json_encode($big->toArray(500, 30000))));
    }

    public function test_refs_and_parents_form_a_tree(): void
    {
        $p = new Projection(self::LABELS);
        $p->add('a1b2c3d', Projection::ROOT, 'section', null, [], []);
        $p->add('e4f5a6b', 'a1b2c3d', 'heading', 3, ['text'], ['text' => "Spring \xC3 sale"]);

        $nodes = $p->toArray(500)['nodes'];
        $this->assertSame(
            '{"ref":"a1b2c3d","parent":"root","kind":"section","editable":[],"from_the_site":{}}',
            json_encode($nodes[0])
        );
        $this->assertSame(['text'], $nodes[1]['editable']);
        $this->assertSame(3, $nodes[1]['level']);
        $this->assertSame("Spring \u{FFFD} sale", $nodes[1]['from_the_site']->text, 'invalid UTF-8 is replaced, never dropped');

        $refused = [
            'duplicate ref'           => static fn () => $p->add('a1b2c3d', Projection::ROOT, 'paragraph', null, [], []),
            'ref is the root'         => static fn () => $p->add('root', Projection::ROOT, 'paragraph', null, [], []),
            'ref too long'            => static fn () => $p->add(str_repeat('a', 33), Projection::ROOT, 'paragraph', null, [], []),
            'ref with a space'        => static fn () => $p->addLocked('a b', Projection::ROOT, 'form'),
            'ref with a newline'      => static fn () => $p->addLocked("ab\n", Projection::ROOT, 'form'),
            'parent not added yet'    => static fn () => $p->add('c1', 'later', 'paragraph', null, [], []),
            'level 7'                 => static fn () => $p->add('c2', Projection::ROOT, 'heading', 7, [], []),
            'level 0'                 => static fn () => $p->add('c3', Projection::ROOT, 'heading', 0, [], []),
            'unknown editable field'  => static fn () => $p->add('c4', Projection::ROOT, 'paragraph', null, ['html'], []),
            'repeated editable field' => static fn () => $p->add('c5', Projection::ROOT, 'paragraph', null, ['text', 'text'], []),
            'site text off contract'  => static fn () => $p->add('c6', Projection::ROOT, 'paragraph', null, [], ['label' => 'x']),
            'site text not a string'  => static fn () => $p->add('c7', Projection::ROOT, 'paragraph', null, [], ['text' => ['x']]),
        ];
        foreach ($refused as $name => $call) {
            try {
                $call();
                $this->fail($name . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
        $this->assertSame(2, $p->toArray(500)['node_count']);
    }

    // ---------------------------------------------------------------------
    // Patterns of a designed page, read back from compiled trees
    // ---------------------------------------------------------------------

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
            'fonts'   => ['heading' => 'Manrope', 'body' => 'Work Sans'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ]);
    }

    /** @return array<int,array<string,mixed>> Library images 5 to 14, by id. */
    private static function media(): array
    {
        $media = [];
        foreach (range(5, 14) as $id) {
            $media[$id] = [
                'id'           => $id,
                'url'          => $id === 5 ? self::PROBE_URL : 'https://example.com/wp-content/uploads/2026/10/logo-' . $id . '-300x120.png',
                'filename'     => 'logo-' . $id . '.png',
                'mime'         => 'image/png',
                'width'        => 300,
                'height'       => 120,
                'modified_gmt' => '2026-10-01 09:00:00',
                'alt'          => 'Logo ' . $id,
            ];
        }

        return $media;
    }

    /** @return list<array<string,mixed>> The outline items of an item pattern. */
    private static function items(string $pattern, int $count): array
    {
        $plans = [
            ['name' => 'Starter', 'price' => '$19', 'period' => 'per month', 'features' => ['1 site', 'Email & chat'], 'cta' => ['text' => 'Start', 'url' => '#signup'], 'highlighted' => false],
            ['name' => 'Growth', 'price' => '$49', 'period' => 'per month', 'features' => ['10 sites', 'Priority support', 'Daily backups'], 'cta' => ['text' => 'Choose Growth', 'url' => '#signup'], 'highlighted' => true],
            ['name' => 'Scale', 'price' => '$99', 'features' => ['Unlimited sites', 'Phone support'], 'cta' => ['text' => 'Talk to us', 'url' => 'https://example.com/contact']],
            ['name' => 'Agency', 'price' => '$199', 'period' => 'per month', 'features' => ['Everything', 'White label', 'Service level', 'Training'], 'cta' => ['text' => 'Contact sales', 'url' => '#contact']],
        ];
        if ($pattern === 'pricing') {
            return array_slice($plans, 0, $count);
        }
        $items = [];
        for ($k = 0; $k < $count; $k++) {
            $items[] = match ($pattern) {
                'features'     => ['icon' => 'bolt', 'title' => 'Feature ' . $k, 'text' => 'Fast & safe [' . $k . ']'],
                'testimonials' => $k === 1 ? ['quote' => 'Quote ' . $k, 'name' => 'Name ' . $k] : ['quote' => 'Quote ' . $k, 'name' => 'Name ' . $k, 'role' => 'Role ' . $k],
                'faq'          => ['question' => 'Question ' . $k . '?', 'answer' => 'Answer <' . $k . '>.'],
                'stats'        => $k % 2 === 0 ? ['value' => 10 * ($k + 1), 'suffix' => '+', 'label' => 'Label ' . $k] : ['value' => 7 + $k, 'label' => 'Label ' . $k],
                'logos'        => ['attachment_id' => 5 + $k, 'alt' => 'Logo ' . (5 + $k)],
            };
        }

        return $items;
    }

    /**
     * What the projection must offer for these items: path to the text the
     * outline gave, in document order.
     *
     * @param list<array<string,mixed>> $items The outline items.
     * @return array<string,string>
     */
    private static function expected(string $pattern, array $items): array
    {
        $out = [];
        foreach ($items as $k => $item) {
            switch ($pattern) {
                case 'features':
                    $out['features.' . $k . '.title'] = $item['title'];
                    $out['features.' . $k . '.text']  = $item['text'];
                    break;
                case 'testimonials':
                    $out['testimonials.' . $k . '.quote'] = $item['quote'];
                    $out['testimonials.' . $k . '.name']  = $item['name'];
                    if (isset($item['role'])) {
                        $out['testimonials.' . $k . '.role'] = $item['role'];
                    }
                    break;
                case 'pricing':
                    $out['plans.' . $k . '.name']  = $item['name'];
                    $out['plans.' . $k . '.price'] = $item['price'];
                    if (isset($item['period'])) {
                        $out['plans.' . $k . '.period'] = $item['period'];
                    }
                    foreach ($item['features'] as $j => $line) {
                        $out['plans.' . $k . '.features.' . $j] = $line;
                    }
                    $out['plans.' . $k . '.cta.text'] = $item['cta']['text'];
                    $out['plans.' . $k . '.cta.url']  = $item['cta']['url'];
                    break;
                case 'faq':
                    $out['faq.' . $k . '.question'] = $item['question'];
                    $out['faq.' . $k . '.answer']   = $item['answer'];
                    break;
                case 'stats':
                    $out['stats.' . $k . '.value'] = (string) $item['value'];
                    if (isset($item['suffix'])) {
                        $out['stats.' . $k . '.suffix'] = $item['suffix'];
                    }
                    $out['stats.' . $k . '.label'] = $item['label'];
                    break;
            }
        }

        return $out;
    }

    /** @return list<array<string,mixed>> The built blocks of a hero or a call to action. */
    private static function blocks(bool $picture): array
    {
        $blocks = [
            ['id' => 'b100001', 'elType' => 'widget', 'widgetType' => 'heading', 'settings' => ['title' => 'Big idea', 'header_size' => 'h1'], 'elements' => []],
            ['id' => 'b100002', 'elType' => 'widget', 'widgetType' => 'text-editor', 'settings' => ['editor' => '<p>Lead text</p>'], 'elements' => []],
        ];
        if ($picture) {
            $blocks[] = ['id' => 'b100003', 'elType' => 'widget', 'widgetType' => 'image', 'settings' => ['image' => ['url' => self::PROBE_URL, 'id' => '5']], 'elements' => []];
        }

        return $blocks;
    }

    /**
     * A designed page: one section holding one pattern, compiled as page-create compiles it.
     *
     * @param list<string>|null $breakpoints The site's active breakpoints.
     * @return array{0: list<array<string,mixed>>, 1: list<array<string,mixed>>} The page, and the outline items.
     */
    private static function page(string $pattern, string $variant, int $count, ?array $breakpoints = ['mobile', 'tablet'], bool $picture = true): array
    {
        $media    = self::media();
        $sections = new ElementorSectionCompiler(self::tokens(), $breakpoints, $media);
        $patterns = new ElementorPatternCompiler(self::tokens(), $sections, $media);
        $ids      = new IdSeed(self::REQUEST);
        $items    = in_array($pattern, ElementorPatternCompiler::COMPOSED, true) ? [] : self::items($pattern, $count);
        $node     = ['type' => 'pattern', 'pattern' => $pattern, 'variant' => $variant];
        if ($items !== []) {
            $node[OutlineV3Schema::PATTERN_ARRAY[$pattern]] = $items;
        }
        $blocks   = in_array($pattern, ElementorPatternCompiler::COMPOSED, true) ? self::blocks($picture) : [];
        $compiled = $patterns->compile($ids, self::AT, $node, 'ground', $blocks);
        $section  = $sections->container($ids->next('outline[1]'), ['type' => 'section', 'anchor' => 'plans'], [$compiled]);

        return [[$section], $items];
    }

    /**
     * @param list<array<string,mixed>> $page A page from page().
     * @return list<array<string,mixed>>
     */
    private static function projected(array $page): array
    {
        return ElementorClassicMapper::project($page)->nodes();
    }

    public function testPatternPaths(): void
    {
        $sets = [
            ['features', 'split', range(3, 8)],
            ['features', 'bento', range(3, 8)],
            ['features', 'grid', range(3, 8)],
            ['testimonials', 'featured', range(1, 6)],
            ['pricing', 'three-plans', range(1, 4)],
            ['faq', 'accordion', range(1, 12)],
            ['stats', 'counters', range(2, 4)],
            ['logos', 'row', range(3, 10)],
        ];
        $seen = 0;
        foreach ($sets as [$pattern, $variant, $counts]) {
            foreach ($counts as $count) {
                foreach ([['mobile', 'tablet'], null] as $breakpoints) {
                    $what = $pattern . ' ' . $variant . ' x' . $count . ($breakpoints === null ? ' without breakpoints' : '');
                    [$page, $items] = self::page($pattern, $variant, $count, $breakpoints);
                    $nodes    = self::projected($page);
                    $expected = self::expected($pattern, $items);

                    $this->assertCount(2, $nodes, $what . ': the section and the pattern, and no node inside the pattern');
                    $node = $nodes[1];
                    $this->assertSame($page[0]['elements'][0]['id'], $node['ref'], $what);
                    $this->assertSame($page[0]['id'], $node['parent'], $what);
                    $this->assertSame('pattern', $node['kind'], $what);
                    $this->assertSame($variant, $node['variant'], $what);
                    $this->assertSame(array_keys($expected), $node['editable'], $what . ': the paths, in document order');
                    $this->assertSame($expected, (array) $node['from_the_site'], $what . ': the outline\'s own words, decoded');
                    ++$seen;
                }
            }
        }
        $this->assertSame(2 * (18 + 6 + 4 + 12 + 3 + 8), $seen, 'every item count of every pattern was read');

        // The k-th item is the k-th item of the document: a second plan is plans.1.
        [$page] = self::page('pricing', 'three-plans', 3);
        $node = self::projected($page)[1];
        $this->assertSame('$49', ((array) $node['from_the_site'])['plans.1.price']);
        $this->assertSame('Choose Growth', ((array) $node['from_the_site'])['plans.1.cta.text']);

        // The answer is the contract's: a pattern path is a path set_text names.
        foreach ($node['editable'] as $path) {
            $this->assertSame(1, preg_match('/' . OutlineV3Schema::EDIT_PATH_PATTERN . '/D', $path), $path);
        }
        $json = (string) json_encode(ElementorClassicMapper::project($page)->toArray(500)['nodes'][1]);
        $this->assertStringStartsWith('{"ref":"', $json);
        $this->assertStringContainsString('"kind":"pattern","variant":"three-plans","editable":["plans.0.name"', $json);
    }

    public function testComposedPatternChildren(): void
    {
        // A hero over a picture or centred, and a call to action, hold blocks that project as ever.
        foreach ([['hero', 'background'], ['hero', 'centered'], ['cta', 'band']] as [$pattern, $variant]) {
            [$page] = self::page($pattern, $variant, 0, ['mobile', 'tablet'], false);
            $nodes  = self::projected($page);
            $this->assertSame(['section', 'pattern', 'heading', 'paragraph'], array_column($nodes, 'kind'), $pattern . ' ' . $variant);
            $this->assertSame($variant, $nodes[1]['variant']);
            $this->assertSame([], $nodes[1]['editable']);
            $this->assertSame(['text'], $nodes[2]['editable']);
            $this->assertSame('Big idea', $nodes[2]['from_the_site']->text);
            $this->assertSame($nodes[1]['ref'], $nodes[2]['parent']);
        }

        // A split hero with a picture is a row of two columns; without one it is the stacked group.
        [$page] = self::page('hero', 'split', 0, ['mobile', 'tablet'], true);
        $nodes  = self::projected($page);
        $this->assertSame(['section', 'pattern', 'column', 'heading', 'paragraph', 'column', 'image'], array_column($nodes, 'kind'));
        $this->assertSame('split', $nodes[1]['variant']);
        [$page] = self::page('hero', 'split', 0, ['mobile', 'tablet'], false);
        $this->assertSame(['section', 'pattern', 'heading', 'paragraph'], array_column(self::projected($page), 'kind'));

        // A block holding dynamic content is locked on its own; the hero stays a pattern.
        [$page] = self::page('hero', 'centered', 0, ['mobile', 'tablet'], false);
        $page[0]['elements'][0]['elements'][0]['settings']['__dynamic__'] = ['title' => '[elementor-tag id="x"]'];
        $nodes = self::projected($page);
        $this->assertSame(['section', 'pattern', 'locked', 'paragraph'], array_column($nodes, 'kind'));
        $this->assertSame('Elementor element with dynamic content or custom code', $nodes[2]['label']);
    }

    /** @return array<string,array{0:string,1:string,2:int,3:callable}> */
    public static function restructures(): array
    {
        $widget = static fn (string $type): array => ['id' => 'z' . substr(md5($type), 0, 6), 'elType' => 'widget', 'widgetType' => $type, 'settings' => [], 'elements' => []];

        return [
            'pricing: a card gains a stray widget'          => ['pricing', 'three-plans', 3, static function (array $c) use ($widget): array {
                $c['elements'][1]['elements'][] = $widget('html');
                return $c;
            }],
            'pricing: a price is wrapped in a container'    => ['pricing', 'three-plans', 3, static function (array $c): array {
                $c['elements'][0]['elements'][1] = ['id' => 'z000001', 'elType' => 'container', 'settings' => [], 'elements' => [$c['elements'][0]['elements'][1]], 'isInner' => true];
                return $c;
            }],
            'pricing: the cards stand in a column'           => ['pricing', 'three-plans', 3, static function (array $c): array {
                $c['settings']['flex_direction'] = 'column';
                return $c;
            }],
            'pricing: no card is left'                       => ['pricing', 'three-plans', 1, static function (array $c): array {
                $c['elements'] = [];
                return $c;
            }],
            'pricing: a card has no button'                  => ['pricing', 'three-plans', 2, static function (array $c): array {
                array_pop($c['elements'][0]['elements']);
                return $c;
            }],
            'pricing: a ninth feature line'                  => ['pricing', 'three-plans', 1, static function (array $c): array {
                $c['elements'][0]['elements'][3]['settings']['icon_list'] = array_fill(0, 9, ['text' => 'Line', 'selected_icon' => ['value' => 'fas fa-check', 'library' => 'fa-solid'], '_id' => 'abcdef0']);
                return $c;
            }],
            'pricing: no feature line'                       => ['pricing', 'three-plans', 1, static function (array $c): array {
                $c['elements'][0]['elements'][3]['settings']['icon_list'] = [];
                return $c;
            }],
            'faq: the elements are keyed by name'           => ['faq', 'accordion', 3, static function (array $c): array {
                $c['elements'] = ['first' => $c['elements'][0]];
                return $c;
            }],
            'stats: the columns are keyed by name'          => ['stats', 'counters', 2, static function (array $c): array {
                $c['elements'] = ['a' => $c['elements'][0], 'b' => $c['elements'][1]];
                return $c;
            }],
            'features split: a third column'                => ['features', 'split', 4, static function (array $c): array {
                $c['elements'][] = ['id' => 'z000002', 'elType' => 'container', 'settings' => [], 'elements' => [], 'isInner' => true];
                return $c;
            }],
            'features split: a box moves to the other side' => ['features', 'split', 4, static function (array $c): array {
                $c['elements'][0]['elements'][] = array_pop($c['elements'][1]['elements']);
                return $c;
            }],
            'features grid: a card holds two boxes'         => ['features', 'grid', 6, static function (array $c): array {
                $c['elements'][0]['elements'][0]['elements'][] = $c['elements'][0]['elements'][1]['elements'][0];
                return $c;
            }],
            'features bento: a box stands outside a card'   => ['features', 'bento', 5, static function (array $c): array {
                $c['elements'][0] = $c['elements'][0]['elements'][0]['elements'][0];
                return $c;
            }],
            'features: a box is a heading now'               => ['features', 'split', 4, static function (array $c) use ($widget): array {
                $c['elements'][0]['elements'][0] = $widget('heading');
                return $c;
            }],
            'testimonials: the featured one is in a row'    => ['testimonials', 'featured', 4, static function (array $c): array {
                $c['elements'][0] = ['id' => 'z000003', 'elType' => 'container', 'settings' => ['flex_direction' => 'row'], 'elements' => [$c['elements'][0]], 'isInner' => true];
                return $c;
            }],
            'testimonials: a row of three'                  => ['testimonials', 'featured', 4, static function (array $c): array {
                $c['elements'][1]['elements'][] = $c['elements'][2]['elements'][0];
                $c['elements'][1]['elements'][] = $c['elements'][2]['elements'][0];
                return $c;
            }],
            'faq: a heading beside the accordion'           => ['faq', 'accordion', 3, static function (array $c) use ($widget): array {
                array_unshift($c['elements'], $widget('heading'));
                return $c;
            }],
            'faq: two accordions'                           => ['faq', 'accordion', 3, static function (array $c): array {
                $c['elements'][] = $c['elements'][0];
                return $c;
            }],
            'stats: a counter is a heading now'             => ['stats', 'counters', 3, static function (array $c) use ($widget): array {
                $c['elements'][1]['elements'][0] = $widget('heading');
                return $c;
            }],
            'stats: one counter is left'                    => ['stats', 'counters', 2, static function (array $c): array {
                array_pop($c['elements']);
                return $c;
            }],
            'logos: a text among the pictures'              => ['logos', 'row', 4, static function (array $c) use ($widget): array {
                $c['elements'][1] = $widget('text-editor');
                return $c;
            }],
            'logos: the pictures are nested'                => ['logos', 'row', 4, static function (array $c): array {
                $c['elements'] = [['id' => 'z000004', 'elType' => 'container', 'settings' => [], 'elements' => $c['elements'], 'isInner' => true]];
                return $c;
            }],
            'hero split: a third column'                    => ['hero', 'split', 0, static function (array $c): array {
                $c['elements'][] = ['id' => 'z000005', 'elType' => 'container', 'settings' => [], 'elements' => [], 'isInner' => true];
                return $c;
            }],
            'hero background: the blocks stand in a row'    => ['hero', 'background', 0, static function (array $c): array {
                $c['settings']['flex_direction'] = 'row';
                return $c;
            }],
            'cta: no block is left'                         => ['cta', 'band', 0, static function (array $c): array {
                $c['elements'] = [];
                return $c;
            }],
        ];
    }

    /**
     * @dataProvider restructures
     * @param callable(array<string,mixed>):array<string,mixed> $change Changes the pattern container the way a person can in Elementor.
     */
    public function testRestructuredLocked(string $pattern, string $variant, int $count, callable $change): void
    {
        [$page] = self::page($pattern, $variant, $count);
        // Before the change it reads as the pattern it is.
        $this->assertSame('pattern', self::projected($page)[1]['kind']);

        $page[0]['elements'][0] = $change($page[0]['elements'][0]);
        $nodes                  = self::projected($page);

        $this->assertCount(2, $nodes, 'the section and the locked pattern: nothing inside it is walked');
        $this->assertSame(
            ['ref' => $page[0]['elements'][0]['id'], 'parent' => $page[0]['id'], 'kind' => 'locked', 'label' => self::EDITED_WORDS],
            $nodes[1]
        );
    }

    public function testOnlyAnExactPatternLabelIsAPattern(): void
    {
        [$page] = self::page('hero', 'background', 0, ['mobile', 'tablet'], false);
        $exact  = LeafPolicy::LABEL_PREFIX . 'Hero background';
        $this->assertSame($exact, $page[0]['elements'][0]['settings'][LeafPolicy::LABEL_KEY]);
        $this->assertSame('pattern', self::projected($page)[1]['kind']);

        $others = [
            'an unknown variant'   => LeafPolicy::LABEL_PREFIX . 'Hero bogus',
            'a trailing space'     => $exact . ' ',
            'a longer name'        => $exact . ' two',
            'lower case'           => strtolower($exact),
            'the section label'    => LeafPolicy::SECTION_LABEL,
            'a plain bullet'       => 'WPMgr - Hero background',
            'no prefix'            => 'Hero background',
            'another pattern name' => LeafPolicy::LABEL_PREFIX . 'Pricing background',
        ];
        foreach ($others as $what => $label) {
            $copy = $page;
            $copy[0]['elements'][0]['settings'][LeafPolicy::LABEL_KEY] = $label;
            $this->assertSame(['section', 'group', 'heading', 'paragraph'], array_column(self::projected($copy), 'kind'), $what . ': a container like any other');
        }

        // A label on a widget does not make a pattern of it.
        $copy = $page;
        $copy[0]['elements'][0]['elements'][0]['settings'][LeafPolicy::LABEL_KEY] = $exact;
        $this->assertSame(['section', 'pattern', 'heading', 'paragraph'], array_column(self::projected($copy), 'kind'));

        // The same shape with no label is not a pattern either.
        [$page] = self::page('pricing', 'three-plans', 3);
        unset($page[0]['elements'][0]['settings'][LeafPolicy::LABEL_KEY]);
        $this->assertNotContains('pattern', array_column(self::projected($page), 'kind'));
    }

    public function testDynamicContentInsideAPatternLocksIt(): void
    {
        foreach ([['testimonials', 'featured', 2, [0]], ['faq', 'accordion', 3, [0]], ['pricing', 'three-plans', 2, [1, 2]]] as [$pattern, $variant, $count, $route]) {
            [$page] = self::page($pattern, $variant, $count);
            $at = &$page[0]['elements'][0];
            foreach ($route as $step) {
                $at = &$at['elements'][$step];
            }
            $at['settings']['__dynamic__'] = ['title' => '[elementor-tag id="x" name="post-title"]'];
            unset($at);
            $nodes = self::projected($page);
            $this->assertCount(2, $nodes, $pattern);
            $this->assertSame('locked', $nodes[1]['kind'], $pattern);
            $this->assertSame('Elementor element with dynamic content or custom code', $nodes[1]['label'], $pattern);
        }
    }

    public function testPatternTargetsNameWhereEachPathIsStored(): void
    {
        [$page] = self::page('pricing', 'three-plans', 2);
        $card   = $page[0]['elements'][0]['elements'][1];
        $found  = ElementorPatternReader::fields($page[0]['elements'][0], 'pricing', 'three-plans');
        $this->assertNotNull($found);
        $this->assertSame($card['elements'][1]['id'], $found['plans.1.price']['id']);
        $this->assertSame(['title'], $found['plans.1.price']['at']);
        $this->assertSame('text', $found['plans.1.price']['form']);
        $this->assertSame($card['elements'][3]['id'], $found['plans.1.features.0']['id']);
        $this->assertSame(['icon_list', 0, 'text'], $found['plans.1.features.0']['at']);
        $this->assertSame(['link', 'url'], $found['plans.1.cta.url']['at']);
        $this->assertSame('url', $found['plans.1.cta.url']['form']);
        $this->assertSame($card['elements'][4]['elements'][0]['id'], $found['plans.1.cta.text']['id']);
        $this->assertSame(['text'], $found['plans.1.cta.text']['at']);
        $this->assertSame('per month', $found['plans.1.period']['text']);
        $this->assertSame(['editor'], $found['plans.1.period']['at']);
        $this->assertSame('paragraph', $found['plans.1.period']['form']);

        [$page] = self::page('faq', 'accordion', 3);
        $found  = ElementorPatternReader::fields($page[0]['elements'][0], 'faq', 'accordion');
        $this->assertSame(['tabs', 2, 'tab_content'], $found['faq.2.answer']['at']);
        $this->assertSame('paragraph', $found['faq.2.answer']['form']);
        $this->assertSame($page[0]['elements'][0]['elements'][0]['id'], $found['faq.2.question']['id']);

        [$page] = self::page('stats', 'counters', 3);
        $found  = ElementorPatternReader::fields($page[0]['elements'][0], 'stats', 'counters');
        $this->assertSame(['ending_number'], $found['stats.1.value']['at']);
        $this->assertSame('number', $found['stats.1.value']['form']);
        $this->assertSame('8', $found['stats.1.value']['text']);
        $this->assertArrayNotHasKey('stats.1.suffix', $found, 'a counter with no suffix offers none');

        // A shape the compiler does not build is not read.
        $page[0]['elements'][0]['elements'][0]['elements'][] = ['id' => 'z000009', 'elType' => 'widget', 'widgetType' => 'html', 'settings' => [], 'elements' => []];
        $this->assertNull(ElementorPatternReader::fields($page[0]['elements'][0], 'stats', 'counters'));
    }

    public function testPatternNodeRules(): void
    {
        $p = new Projection(self::LABELS);
        $p->addPattern('p1', Projection::ROOT, 'three-plans', ['plans.0.name' => 'Starter', 'plans.0.features.1' => "Email \xC3 chat"]);
        $node = $p->nodes()[0];
        $this->assertSame(['ref' => 'p1', 'parent' => 'root', 'kind' => 'pattern', 'variant' => 'three-plans'], array_slice($node, 0, 4));
        $this->assertSame(['plans.0.name', 'plans.0.features.1'], $node['editable']);
        $this->assertSame("Email \u{FFFD} chat", $node['from_the_site']->{'plans.0.features.1'}, 'invalid UTF-8 is replaced, never dropped');
        $this->assertSame([], $p->readOnly()->nodes()[0]['editable'], 'a read-only page offers no path');
        $this->assertSame('Starter', $p->readOnly()->nodes()[0]['from_the_site']->{'plans.0.name'});

        $refused = [
            'a plain kind through add'      => static fn () => $p->add('q1', Projection::ROOT, 'pattern', null, [], []),
            'a path off the grammar'        => static fn () => $p->addPattern('q2', Projection::ROOT, 'three-plans', ['pricing.0.name' => 'x']),
            'a path with a trailing break'  => static fn () => $p->addPattern('q3', Projection::ROOT, 'three-plans', ["plans.0.name\n" => 'x']),
            'a plain field on a pattern'    => static fn () => $p->addPattern('q4', Projection::ROOT, 'three-plans', ['text' => 'x']),
            'text that is not a string'     => static fn () => $p->addPattern('q5', Projection::ROOT, 'three-plans', ['plans.0.name' => 7]),
            'a variant with a capital'      => static fn () => $p->addPattern('q6', Projection::ROOT, 'Three-plans', []),
            'an empty variant'              => static fn () => $p->addPattern('q7', Projection::ROOT, '', []),
            'a variant with a newline'      => static fn () => $p->addPattern('q8', Projection::ROOT, "background\n", []),
            'a ref taken'                   => static fn () => $p->addPattern('p1', Projection::ROOT, 'row', []),
            'a parent not added'            => static fn () => $p->addPattern('q9', 'later', 'row', []),
        ];
        foreach ($refused as $what => $call) {
            try {
                $call();
                $this->fail($what . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
        $this->assertCount(1, $p->nodes(), 'a refused node adds nothing');
    }
}
