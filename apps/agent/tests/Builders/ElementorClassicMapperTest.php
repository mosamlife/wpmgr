<?php
/**
 * ElementorClassicMapper: the golden trees for both layouts, the closed
 * allowlist, the Elementor node rules, escaping, deterministic ids and the
 * projection of a stored tree.
 *
 * Every outline goes through PageCreateBuilder::validate() first, as in
 * production. The HTML sanitiser is a double: it drops event-handler
 * attributes, drops tags outside a short list (keeping their text) and writes
 * a bare "&" as "&amp;". The real sanitiser is proven against a real
 * WordPress elsewhere.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\Projection;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 */
final class ElementorClassicMapperTest extends TestCase
{
    private const FIXTURES = __DIR__ . '/../fixtures/ability-run/';

    private const REQUEST = '11111111-2222-4333-8444-777777777777';

    /** The case names the goldens must carry: one per outline node kind, then layouts. */
    private const GOLDEN_CASES = [
        'heading',
        'text',
        'list',
        'quote',
        'button',
        'image',
        'spacer',
        'divider',
        'columns',
        'group-with-columns-and-widths',
        'mixed-top-level',
    ];

    /** Tags the sanitiser double keeps. */
    private const KEPT_TAGS = ['p', 'h2', 'h3', 'h4', 'ul', 'ol', 'li', 'blockquote', 'cite', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'strong', 'b', 'a', 'img'];

    /** Media facts used by the hand-written cases. */
    private const MEDIA = [
        5 => ['url' => 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png', 'alt' => 'Library alt text'],
        7 => ['url' => 'https://example.com/wp-content/uploads/2026/10/team-1024x683.jpg', 'alt' => ''],
    ];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('wp_kses_post')->alias(static fn ($s) => self::kses((string) $s));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_containers_goldens(): void
    {
        $this->assertGoldens('elementor-classic-containers.json', true);
    }

    public function test_sections_goldens(): void
    {
        $this->assertGoldens('elementor-classic-sections.json', false);
    }

    public function test_every_emitted_key_is_allowlisted(): void
    {
        $emitted = [];
        foreach (['elementor-classic-containers.json', 'elementor-classic-sections.json'] as $file) {
            foreach ($this->golden($file)['cases'] as $case) {
                $this->walk($case['tree'], function (array $node, string $at) use (&$emitted): void {
                    $type = $node['elType'] === 'widget' ? $node['widgetType'] : $node['elType'];
                    $this->assertArrayHasKey($type, ElementorClassicMapper::ALLOWED_KEYS, $at . ': type not allowlisted');
                    $emitted[$type] = $emitted[$type] ?? [];
                    $settings       = $node['settings'];
                    $this->assertIsArray($settings);
                    foreach (self::leaves($settings, '') as $path => $value) {
                        $this->assertArrayHasKey($path, ElementorClassicMapper::ALLOWED_KEYS[$type], $at . '.' . $path . ': path not allowlisted');
                        $this->assertTrue(is_string($value) || $value === [], $at . '.' . $path . ': a setting holds something other than a string');
                        $emitted[$type][$path] = true;
                    }
                    $expected = $node['elType'] === 'widget'
                        ? ['id', 'elType', 'settings', 'elements', 'widgetType']
                        : ['id', 'elType', 'settings', 'elements', 'isInner'];
                    $this->assertSame($expected, array_keys($node), $at . ': key order');
                });
                $this->assertNull(LeafPolicy::checkTree($case['tree'], ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES), $file . ' ' . $case['name']);
            }
        }

        // The allowlist is exactly what the goldens write: no type or path is listed that no case emits.
        $this->assertSame(array_keys(ElementorClassicMapper::ALLOWED_KEYS), array_values(array_intersect(array_keys(ElementorClassicMapper::ALLOWED_KEYS), array_keys($emitted))));
        foreach (ElementorClassicMapper::ALLOWED_KEYS as $type => $paths) {
            $this->assertSame(array_keys($paths), array_keys(array_intersect_key($paths, $emitted[$type])), $type . ': an allowlisted path no golden writes');
        }
    }

    public function test_outline_button_refused_with_node_path(): void
    {
        $outline = '[{"type":"group","children":[{"type":"heading","level":2,"text":"Book"},{"type":"columns","columns":['
            . '{"children":[{"type":"paragraph","text":"Left"}]},'
            . '{"children":[{"type":"buttons","buttons":[{"text":"Call","url":"/call"},{"text":"Mail","url":"/mail","style":"outline"}]}]}]}]}]';

        foreach ([true, false] as $containers) {
            $r = $this->mapOutline($outline, $containers);
            $this->assertSame('node_not_supported_by_builder', $r['code'] ?? null);
            $this->assertStringStartsWith('outline[0].children[1].columns[1].children[0].buttons[1].style: ', (string) $r['detail']);
            $this->assertStringEndsWith('the button styles it builds are: fill', (string) $r['detail']);
            $this->assertArrayNotHasKey('tree', $r);
        }

        $fill = str_replace(',"style":"outline"', ',"style":"fill"', $outline);
        $this->assertArrayHasKey('tree', $this->mapOutline($fill, true));
    }

    public function test_url_only_paragraph_refused(): void
    {
        foreach (['https://example.com/video', 'http://example.com', '  HTTPS://example.com/a?b=1  '] as $text) {
            $r = $this->mapOutline('[{"type":"paragraph","text":' . json_encode($text) . '}]', true);
            $this->assertSame('create_content_invalid', $r['code'] ?? null, $text);
            $this->assertStringStartsWith('outline[0].text: ', (string) $r['detail']);
        }

        $quote = $this->mapOutline('[{"type":"quote","paragraphs":["Watch this","https://example.com/v"]}]', false);
        $this->assertSame('create_content_invalid', $quote['code'] ?? null);
        $this->assertStringStartsWith('outline[0].paragraphs[1]: ', (string) $quote['detail']);

        foreach (['See https://example.com/video', 'https://example.com and more', 'example.com', 'ftp://example.com'] as $text) {
            $this->assertArrayHasKey('tree', $this->mapOutline('[{"type":"paragraph","text":' . json_encode($text) . '}]', true), $text);
        }
    }

    public function test_alt_must_equal_library_alt(): void
    {
        foreach (['Different alt', 'Library alt text ', 'library alt text', ''] as $alt) {
            $r = $this->mapOutline('[{"type":"image","attachment_id":5,"alt":' . json_encode($alt) . '}]', true);
            $this->assertSame('image_alt_from_library', $r['code'] ?? null, $alt);
            $this->assertStringStartsWith('outline[0].alt: ', (string) $r['detail']);
            $this->assertStringNotContainsString('Library alt text', (string) $r['detail'], 'the detail never echoes site text');
        }

        $this->assertArrayHasKey('tree', $this->mapOutline('[{"type":"image","attachment_id":5,"alt":"Library alt text"}]', true));
        $this->assertArrayHasKey('tree', $this->mapOutline('[{"type":"image","attachment_id":7,"alt":""}]', false));
        $r = $this->mapOutline('[{"type":"image","attachment_id":7,"alt":"Team"}]', false);
        $this->assertSame('image_alt_from_library', $r['code'] ?? null);
    }

    public function test_text_escaped_in_every_html_leaf(): void
    {
        $raw     = 'Tom & Jerry [12] "café" &amp; &lt;b&gt; \'q\'';
        $escaped = 'Tom &amp; Jerry &#091;12&#093; "café" &amp;amp; &amp;lt;b&amp;gt; \'q\'';
        $this->assertSame($escaped, PageCreateBuilder::storedTitle($raw));
        $t       = json_encode($raw);
        $outline = '[{"type":"heading","level":2,"text":' . $t . '},'
            . '{"type":"paragraph","text":' . $t . '},'
            . '{"type":"list","ordered":true,"items":[' . $t . ']},'
            . '{"type":"quote","paragraphs":[' . $t . '],"citation":' . $t . '},'
            . '{"type":"table","header":[' . $t . '],"rows":[[' . $t . ']]},'
            . '{"type":"buttons","buttons":[{"text":' . $t . ',"url":"/a"}]},'
            . '{"type":"image","attachment_id":5,"alt":"Library alt text","caption":' . $t . '}]';

        $r = $this->mapOutline($outline, true);
        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $widgets = $r['tree'][0]['elements'];

        $this->assertSame($escaped, $widgets[0]['settings']['title']);
        $this->assertSame('<p>' . $escaped . '</p>', $widgets[1]['settings']['editor']);
        $this->assertSame('<ol><li>' . $escaped . '</li></ol>', $widgets[2]['settings']['editor']);
        $this->assertSame('<blockquote><p>' . $escaped . '</p><cite>' . $escaped . '</cite></blockquote>', $widgets[3]['settings']['editor']);
        $this->assertSame('<table><thead><tr><th>' . $escaped . '</th></tr></thead><tbody><tr><td>' . $escaped . '</td></tr></tbody></table>', $widgets[4]['settings']['editor']);
        $this->assertSame($escaped, $widgets[5]['settings']['text']);
        $this->assertSame($escaped, $widgets[6]['settings']['caption']);

        $htmlLeaves = 0;
        $this->walk($r['tree'], function (array $node) use (&$htmlLeaves): void {
            $type = $node['elType'] === 'widget' ? $node['widgetType'] : $node['elType'];
            foreach (self::leaves($node['settings'], '') as $path => $value) {
                if ((ElementorClassicMapper::ALLOWED_KEYS[$type][$path] ?? '') !== 'html') {
                    continue;
                }
                ++$htmlLeaves;
                $this->assertStringNotContainsString('[', (string) $value, $type . '.' . $path);
                $this->assertStringNotContainsString(']', (string) $value, $type . '.' . $path);
                $this->assertSame(0, preg_match('/&(?!(?:amp|lt|gt|#091|#093);)/', (string) $value), $type . '.' . $path . ': a bare or unexpected "&"');
            }
        });
        $this->assertSame(7, $htmlLeaves);
    }

    public function test_ids_unique_and_deterministic(): void
    {
        $case          = $this->goldenCase('elementor-classic-containers.json', 'mixed-top-level');
        $case['input'] = json_decode((string) json_encode($case['input']), false, 512, JSON_THROW_ON_ERROR);
        $a             = $this->mapCase($case['input'], true, self::REQUEST);
        $b             = $this->mapCase($case['input'], true, self::REQUEST);
        $this->assertArrayHasKey('tree', $a);
        $this->assertSame(json_encode($a['tree']), json_encode($b['tree']));

        foreach ([true, false] as $containers) {
            $tree = $this->mapCase($case['input'], $containers, self::REQUEST)['tree'];
            $ids  = [];
            $this->walk($tree, static function (array $node) use (&$ids): void {
                $ids[] = $node['id'];
            });
            $this->assertSame($ids, array_values(array_unique($ids)));
            foreach ($ids as $id) {
                $this->assertMatchesRegularExpression('/^[0-9a-f]{7}$/D', $id);
            }
            $other    = $this->mapCase($case['input'], $containers, 'aaaaaaaa-2222-4333-8444-777777777777')['tree'];
            $otherIds = [];
            $this->walk($other, static function (array $node) use (&$otherIds): void {
                $otherIds[] = $node['id'];
            });
            $this->assertSame([], array_values(array_intersect($ids, $otherIds)));
        }

        // Paths, in document order: a wrapper is keyed by its first node's path plus "#wrapper".
        $seed = new IdSeed(self::REQUEST);
        $tree = $this->mapCase($case['input'], false, self::REQUEST)['tree'];
        $this->assertSame($seed->next('outline[0]#wrapper'), $tree[0]['id']);
        $this->assertSame($seed->next('outline[0]#wrapper#column'), $tree[0]['elements'][0]['id']);
        $this->assertSame($seed->next('outline[0]'), $tree[0]['elements'][0]['elements'][0]['id']);
        $this->assertSame($seed->next('outline[1]'), $tree[0]['elements'][0]['elements'][1]['id']);
        $this->assertSame($seed->next('outline[2]'), $tree[1]['id']);
        $this->assertSame($seed->next('outline[2].columns[0]'), $tree[1]['elements'][0]['id']);
    }

    public function test_projection_kinds_round_trip(): void
    {
        $expected = [
            'containers' => [
                'heading'                       => 'group heading heading',
                'text'                          => 'group paragraph',
                'list'                          => 'group list',
                'quote'                         => 'group quote',
                'button'                        => 'group buttons',
                'image'                         => 'group image',
                'spacer'                        => 'group spacer',
                'divider'                       => 'group separator',
                'columns'                       => 'columns column heading column paragraph',
                'group-with-columns-and-widths' => 'group heading columns column list column table paragraph',
                'mixed-top-level'               => 'group heading image columns column paragraph column paragraph column spacer quote group buttons buttons spacer separator table',
            ],
            'sections'   => [
                'heading'                       => 'section column heading heading',
                'text'                          => 'section column paragraph',
                'list'                          => 'section column list',
                'quote'                         => 'section column quote',
                'button'                        => 'section column buttons',
                'image'                         => 'section column image',
                'spacer'                        => 'section column spacer',
                'divider'                       => 'section column separator',
                'columns'                       => 'section column heading column paragraph',
                'group-with-columns-and-widths' => 'section column heading section column list column table paragraph',
                'mixed-top-level'               => 'section column heading image section column paragraph column paragraph column spacer quote section column buttons buttons spacer separator table',
            ],
        ];

        foreach ($expected as $mode => $kinds) {
            $golden = $this->golden('elementor-classic-' . $mode . '.json');
            $this->assertSame(self::GOLDEN_CASES, array_keys($kinds));
            foreach ($golden['cases'] as $case) {
                $answer = ElementorClassicMapper::project($case['tree'])->toArray(BuilderContract::MAX_STRUCTURE_NODES);
                $this->assertFalse($answer['truncated']);
                $this->assertSame($kinds[$case['name']], implode(' ', array_column($answer['nodes'], 'kind')), $mode . ' ' . $case['name']);
                $this->assertSame(self::countNodes($case['tree']), $answer['node_count']);
            }
        }

        // Text fields read back as the AI wrote them.
        $tree  = $this->goldenCase('elementor-classic-containers.json', 'heading')['tree'];
        $nodes = ElementorClassicMapper::project($tree)->toArray(10)['nodes'];
        $this->assertSame(2, $nodes[1]['level']);
        $this->assertSame(['text'], $nodes[1]['editable']);
        $this->assertSame('Fish & Chips [1] — “Ça va”', $nodes[1]['from_the_site']->text);
        $this->assertSame(3, $nodes[2]['level']);

        $tree  = $this->goldenCase('elementor-classic-sections.json', 'button')['tree'];
        $nodes = ElementorClassicMapper::project($tree)->toArray(10)['nodes'];
        $this->assertSame(['text', 'url'], $nodes[2]['editable']);
        $this->assertEquals((object) ['text' => 'Book now & save', 'url' => 'https://example.com/book?ref=home'], $nodes[2]['from_the_site']);

        $tree  = $this->goldenCase('elementor-classic-containers.json', 'image')['tree'];
        $nodes = ElementorClassicMapper::project($tree)->toArray(10)['nodes'];
        $this->assertSame(['caption'], $nodes[1]['editable']);
        $this->assertSame('Our van & crew', $nodes[1]['from_the_site']->caption);

        $tree  = $this->goldenCase('elementor-classic-containers.json', 'text')['tree'];
        $nodes = ElementorClassicMapper::project($tree)->toArray(10)['nodes'];
        $this->assertSame('We fix leaking taps fast & cheap [1].', $nodes[1]['from_the_site']->text);
        $this->assertSame([], $nodes[0]['editable']);
    }

    public function test_unknown_widget_projects_locked_with_wpmgr_label(): void
    {
        $tree = [[
            'id'       => 'a000001',
            'elType'   => 'container',
            'settings' => ['content_width' => 'boxed', 'flex_direction' => 'column'],
            'elements' => [
                self::storedWidget('a000002', 'heading', ['title' => 'Kept', 'header_size' => 'h5', 'title_color' => '#123']),
                self::storedWidget('a000003', 'site-secret-widget<b>', ['title' => 'Site words']),
                self::storedWidget('a000004', 'html', ['html' => '<script>x</script>']),
                self::storedWidget('a000005', 'heading', ['title' => 'Live', '__dynamic__' => ['title' => '[elementor-tag id="1"]']]),
                self::storedWidget('a000006', 'text-editor', ['editor' => '<p><strong>Styled</strong> words</p>']),
                self::storedWidget('a000007', 'wp-widget-site-calendar', []),
                self::storedWidget('a000008', 'button', ['text' => 'Go', 'link' => ['url' => '/go', 'custom_attributes' => 'onclick|x']]),
                ['id' => 'a000009', 'elType' => 'e-flexbox', 'settings' => [], 'elements' => [self::storedWidget('a00000a', 'heading', ['title' => 'Inside'])]],
                ['id' => 'a00000b', 'elType' => 'widget', 'settings' => [], 'elements' => []],
            ],
            'isInner'  => false,
        ]];

        $answer = ElementorClassicMapper::project($tree)->toArray(50);
        $byRef  = array_column($answer['nodes'], null, 'ref');

        $this->assertSame('group', $byRef['a000001']['kind']);
        $this->assertSame('heading', $byRef['a000002']['kind']);
        $this->assertSame(5, $byRef['a000002']['level']);
        $labels = ElementorClassicMapper::LOCKED_LABELS;
        $this->assertSame(['kind' => 'locked', 'label' => $labels[Projection::FALLBACK_LABEL]], array_intersect_key($byRef['a000003'], ['kind' => 1, 'label' => 1]));
        $this->assertSame($labels['html'], $byRef['a000004']['label']);
        $this->assertSame($labels[ElementorClassicMapper::LABEL_DYNAMIC], $byRef['a000005']['label']);
        $this->assertSame($labels[ElementorClassicMapper::LABEL_CUSTOM_TEXT], $byRef['a000006']['label']);
        $this->assertSame($labels[ElementorClassicMapper::LABEL_WP_WIDGET], $byRef['a000007']['label']);
        $this->assertSame($labels[ElementorClassicMapper::LABEL_DYNAMIC], $byRef['a000008']['label']);
        $this->assertSame($labels[ElementorClassicMapper::LABEL_ATOMIC], $byRef['a000009']['label']);
        $this->assertSame($labels[Projection::FALLBACK_LABEL], $byRef['a00000b']['label']);
        $this->assertArrayNotHasKey('a00000a', $byRef, 'a locked element is not walked');
        foreach (['a000003', 'a000004', 'a000005', 'a000006', 'a000007', 'a000008', 'a000009', 'a00000b'] as $ref) {
            $this->assertSame('locked', $byRef[$ref]['kind'], $ref);
            $this->assertArrayNotHasKey('from_the_site', $byRef[$ref], $ref);
        }

        $json = (string) json_encode($answer, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
        foreach (['site-secret-widget', '<b>', 'Site words', 'calendar', 'script', 'Styled', 'elementor-tag', 'Live', 'onclick', 'Inside'] as $siteText) {
            $this->assertStringNotContainsString($siteText, $json, $siteText);
        }

        $this->expectException(\InvalidArgumentException::class);
        ElementorClassicMapper::project([['elType' => 'widget', 'widgetType' => 'heading', 'settings' => []]]);
    }

    public function test_link_with_ampersand_refused(): void
    {
        $r = $this->mapOutline('[{"type":"buttons","buttons":[{"text":"Sign up","url":"https://example.com/signup?plan=pro&ref=home"}]}]', true);
        $this->assertSame('link_invalid', $r['code'] ?? null);
        $this->assertStringStartsWith('outline[0].buttons[0].url: ', (string) $r['detail']);
    }

    public function test_wide_or_full_image_refused(): void
    {
        foreach (['wide', 'full'] as $align) {
            $r = $this->mapOutline('[{"type":"image","attachment_id":5,"alt":"Library alt text","align":"' . $align . '"}]', true);
            $this->assertSame('node_not_supported_by_builder', $r['code'] ?? null, $align);
            $this->assertStringStartsWith('outline[0].align: ', (string) $r['detail']);
            $this->assertStringEndsWith('none, center', (string) $r['detail']);
        }
        $tree = $this->mapOutline('[{"type":"image","attachment_id":5,"alt":"Library alt text","align":"none"}]', true)['tree'];
        $this->assertArrayNotHasKey('align', $tree[0]['elements'][0]['settings']);
    }

    public function test_media_facts_are_required_and_checked(): void
    {
        $r = $this->mapOutline('[{"type":"image","attachment_id":9,"alt":""}]', true);
        $this->assertSame('image_not_available', $r['code'] ?? null);

        $r = $this->mapOutline('[{"type":"image","attachment_id":5,"alt":"Library alt text"}]', true, [5 => ['url' => 'javascript:alert(1)', 'alt' => 'Library alt text']]);
        $this->assertSame('image_url_unusable', $r['code'] ?? null);

        // The leaf policy backstop: an address the sanitiser would rewrite is refused, never stored.
        $r = $this->mapOutline('[{"type":"image","attachment_id":5,"alt":"Library alt text"}]', true, [5 => ['url' => 'https://example.com/a.png?x=1&y=2', 'alt' => 'Library alt text']]);
        $this->assertSame(LeafPolicy::CODE_UNSAFE, $r['code'] ?? null);
    }

    // ---------------------------------------------------------------------

    /**
     * @param string $file       Fixture file.
     * @param bool   $containers Layout.
     */
    private function assertGoldens(string $file, bool $containers): void
    {
        $golden = $this->golden($file);
        $this->assertSame(self::REQUEST, $golden['request_id']);
        $this->assertSame(['3.20.4', '3.35.9', '4.3.4'], $golden['elementor_versions']);
        $this->assertSame(self::GOLDEN_CASES, array_column($golden['cases'], 'name'));

        $objects = json_decode((string) file_get_contents(self::FIXTURES . $file), false, 512, JSON_THROW_ON_ERROR);
        foreach ($golden['cases'] as $i => $case) {
            $r = $this->mapCase($objects->cases[$i]->input, $containers, $golden['request_id'], $golden['media']);
            $this->assertArrayHasKey('tree', $r, $case['name'] . ': ' . ($r['code'] ?? '') . ' ' . ($r['detail'] ?? ''));
            $this->assertSame($case['tree'], $r['tree'], $case['name']);
            $this->assertSame(json_encode($case['tree']), json_encode($r['tree']), $case['name']);
        }
    }

    /**
     * @param mixed             $outline    Decoded outline (objects).
     * @param bool              $containers Layout.
     * @param string            $request    Request id.
     * @param array<mixed>|null $media      Media facts.
     * @return array<string, mixed>
     */
    private function mapCase($outline, bool $containers, string $request, ?array $media = null): array
    {
        $valid = PageCreateBuilder::validate((object) [
            'post_type' => 'page',
            // The block editor's rules accept every layout node; the mapper reads only the outline.
            'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
            'title'     => 'Golden',
            'outline'   => $outline,
        ]);
        $this->assertArrayHasKey('spec', $valid, (string) ($valid['detail'] ?? ''));

        return ElementorClassicMapper::map($valid['spec'], new IdSeed($request), $media ?? self::MEDIA, $containers);
    }

    /**
     * @param string            $json       Outline JSON text.
     * @param bool              $containers Layout.
     * @param array<mixed>|null $media      Media facts.
     * @return array<string, mixed>
     */
    private function mapOutline(string $json, bool $containers, ?array $media = null): array
    {
        return $this->mapCase(json_decode($json, false, 512, JSON_THROW_ON_ERROR), $containers, self::REQUEST, $media);
    }

    /**
     * @param string $file Fixture file.
     * @return array<string, mixed>
     */
    private function golden(string $file): array
    {
        $data = json_decode((string) file_get_contents(self::FIXTURES . $file), true, 512, JSON_THROW_ON_ERROR);
        $this->assertIsArray($data);

        return $data;
    }

    /**
     * @param string $file Fixture file.
     * @param string $name Case name.
     * @return array<string, mixed>
     */
    private function goldenCase(string $file, string $name): array
    {
        foreach ($this->golden($file)['cases'] as $case) {
            if ($case['name'] === $name) {
                return $case;
            }
        }
        $this->fail('no golden case ' . $name);
    }

    /**
     * Depth-first, parent first.
     *
     * @param array<mixed> $nodes Nodes.
     * @param callable     $fn    Called with (node, position).
     * @param string       $at    Position of the list.
     */
    private function walk(array $nodes, callable $fn, string $at = 'tree'): void
    {
        foreach ($nodes as $i => $node) {
            $fn($node, $at . '[' . $i . ']');
            $this->walk($node['elements'], $fn, $at . '[' . $i . '].elements');
        }
    }

    /**
     * Settings leaves by dotted path; an empty list is a leaf.
     *
     * @param array<mixed> $settings Settings.
     * @param string       $prefix   Path so far.
     * @return array<string, mixed>
     */
    private static function leaves(array $settings, string $prefix): array
    {
        $out = [];
        foreach ($settings as $key => $value) {
            $path = $prefix === '' ? (string) $key : $prefix . '.' . $key;
            if (is_array($value) && $value !== []) {
                $out += self::leaves($value, $path);
                continue;
            }
            $out[$path] = $value;
        }

        return $out;
    }

    /**
     * @param array<mixed> $nodes Nodes.
     * @return int
     */
    private static function countNodes(array $nodes): int
    {
        $n = 0;
        foreach ($nodes as $node) {
            $n += 1 + self::countNodes($node['elements']);
        }

        return $n;
    }

    /**
     * @param string       $id       Id.
     * @param string       $type     Widget type.
     * @param array<mixed> $settings Settings.
     * @return array<string, mixed>
     */
    private static function storedWidget(string $id, string $type, array $settings): array
    {
        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => $type];
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
