<?php
/**
 * LeafPolicy: escaping parity with page-create, the stored-string checks
 * (sanitiser fixed point with inline styles refused, forbidden sequences in
 * every decoded form, braces), and the element tree's types, keys and pins.
 *
 * The HTML sanitiser here is a double: it drops event-handler attributes,
 * drops tags outside a short list (keeping their text), and drops a style
 * attribute whose properties the safe_style_css filters no longer allow. The
 * real sanitiser is proven against a real WordPress elsewhere.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\LeafPolicy
 */
final class LeafPolicyTest extends TestCase
{
    /** Leaf rules of a builder that keeps braces. */
    private const RULES = ['refuse_braces' => false, 'forbidden' => ['[elementor-tag']];

    /** Leaf rules of a builder that reads braces as dynamic data. */
    private const BRACE_RULES = ['refuse_braces' => true, 'forbidden' => []];

    /** A test allowlist; an adapter's real one comes with its mapper. */
    private const KEYS = [
        'container'   => [
            'content_width'  => 'enum',
            'flex_direction' => 'enum',
            'width.unit'     => 'enum',
            'width.size'     => 'int',
            'width.sizes'    => 'enum',
        ],
        'heading'     => ['title' => 'html', 'header_size' => 'enum'],
        'text-editor' => ['editor' => 'html'],
        'button'      => ['text' => 'html', 'link.url' => 'url', 'link.is_external' => 'enum', 'link.nofollow' => 'enum'],
        'image'       => ['image.id' => 'int', 'image.url' => 'url'],
        'spacer'      => ['space.unit' => 'enum', 'space.size' => 'int', 'space.sizes' => 'enum'],
    ];

    /** Tags the sanitiser double keeps. */
    private const KEPT_TAGS = ['p', 'h2', 'h3', 'h4', 'ul', 'ol', 'li', 'blockquote', 'cite', 'table', 'tr', 'td', 'b', 'img', 'a'];

    /** Style properties the double allows before any safe_style_css filter runs. */
    private const DEFAULT_STYLES = ['color', 'background', 'position', 'width'];

    /** @var array<string, array<int, list<callable>>> */
    private array $filters = [];

    /** @var list<array{input: string, styles: array<mixed>, priorities: list<int>}> */
    private array $ksesCalls = [];

    private bool $ksesThrows = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->filters    = [];
        $this->ksesCalls  = [];
        $this->ksesThrows = false;

        Functions\when('add_filter')->alias(function ($hook, $callback, $priority = 10) {
            $this->filters[$hook][$priority][] = $callback;

            return true;
        });
        Functions\when('remove_filter')->alias(function ($hook, $callback, $priority = 10) {
            foreach ($this->filters[$hook][$priority] ?? [] as $i => $known) {
                if ($known === $callback) {
                    unset($this->filters[$hook][$priority][$i]);
                    // Rebuilt rather than unset in place, so no emptied PHP_INT_MAX slot
                    // stays behind as the array's next free index.
                    $this->filters[$hook] = array_filter(
                        array_map('array_values', $this->filters[$hook]),
                        static fn (array $list): bool => $list !== []
                    );
                    if ($this->filters[$hook] === []) {
                        unset($this->filters[$hook]);
                    }

                    return true;
                }
            }

            return false;
        });
        Functions\when('wp_kses_post')->alias(fn ($s) => $this->kses((string) $s));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_html_text_matches_gut_stored_title(): void
    {
        $corpus = [
            '&',
            '&amp;',
            '[1]',
            'é',
            '"double" and \'single\' quotes',
            'Tom & Jerry [12] café',
            'a < b > c',
            '&#091;',
            'x]y[',
            "bad \xff byte",
            '{site_title}',
        ];
        foreach ($corpus as $text) {
            $this->assertSame(PageCreateBuilder::storedTitle($text), LeafPolicy::htmlText($text), 'parity for ' . var_export($text, true));
        }

        $this->assertSame('&amp;', LeafPolicy::htmlText('&'));
        $this->assertSame('&amp;amp;', LeafPolicy::htmlText('&amp;'));
        $this->assertSame('&#091;1&#093;', LeafPolicy::htmlText('[1]'));
        $this->assertSame('é', LeafPolicy::htmlText('é'));
        $this->assertSame('"a" \'b\'', LeafPolicy::htmlText('"a" \'b\''), 'quotes stay: a text node needs no quote escaping');
        $this->assertSame("bad \u{FFFD} byte", LeafPolicy::htmlText("bad \xff byte"), 'an invalid byte becomes U+FFFD');
    }

    public function test_bold_markup_text_stays_literal(): void
    {
        $this->assertSame('&lt;b&gt;bold&lt;/b&gt;', LeafPolicy::htmlText('<b>bold</b>'));
        $this->assertSame('&amp;lt;b&amp;gt;', LeafPolicy::htmlText('&lt;b&gt;'), 'typed entities show literally');

        $this->assertNull(LeafPolicy::storedProblem(LeafPolicy::htmlText('<b>bold</b>'), self::RULES));
        $this->assertNull(LeafPolicy::storedProblem(LeafPolicy::htmlText('&lt;b&gt;'), self::RULES));
    }

    public function test_brackets_become_numeric_references(): void
    {
        $stored = LeafPolicy::htmlText('[gallery]');
        $this->assertSame('&#091;gallery&#093;', $stored);
        $this->assertStringNotContainsString('[', $stored);
        $this->assertStringNotContainsString(']', $stored);

        $this->assertSame('See &#091;1&#093;', LeafPolicy::htmlText('See [1]'));
        $this->assertNull(LeafPolicy::storedProblem(LeafPolicy::htmlText('See [1]'), self::RULES), 'a bracketed number is stored');
    }

    public function test_l2_refuses_planted_onerror_image(): void
    {
        // A tag the sanitiser drops and no sequence names: only the fixed point sees it.
        $form = '<p>Hi</p><form action="https://example.com/x"><button>Send</button></form>';
        $this->assertSame('changed by the HTML sanitiser', LeafPolicy::storedProblem($form, self::RULES));

        $planted = '<p>Hello</p><img src="https://example.com/a.png" onerror="alert(1)">';
        $this->assertSame('changed by the HTML sanitiser', LeafPolicy::storedProblem($planted, self::RULES));

        $tree = [$this->widget('a1', 'text-editor', ['editor' => $planted])];
        $this->assertSame(
            ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings.editor: changed by the HTML sanitiser'],
            LeafPolicy::checkTree($tree, self::KEYS, self::RULES)
        );

        $this->assertNull(LeafPolicy::storedProblem('<p>Hello</p><img src="https://example.com/a.png">', self::RULES));
    }

    public function test_l2_decodes_entities_before_matching(): void
    {
        $this->assertSame('holds "<script"', LeafPolicy::storedProblem('&lt;script&gt;alert(1)&lt;/script&gt;', self::RULES));
        $this->assertSame('holds "<script"', LeafPolicy::storedProblem('&amp;lt;SCRIPT&amp;gt;', self::RULES), 'two rounds, any case');
        $this->assertSame('holds "<iframe"', LeafPolicy::storedProblem('&#x3C;iframe src=x&#62;', self::RULES));
        $this->assertSame('holds "javascript:"', LeafPolicy::storedProblem('java&#9;script&colon;alert(1)', self::RULES));
        $this->assertSame('holds an event-handler attribute', LeafPolicy::storedProblem('x&#32;onload&#61;go()', self::RULES));
        $this->assertSame(
            'character references nested deeper than 3 levels',
            LeafPolicy::storedProblem('&amp;amp;amp;amp;lt;b', self::RULES)
        );

        $this->assertNull(LeafPolicy::storedProblem('Tom &amp; Jerry &#091;1&#093; caf&eacute;', self::RULES));
        $this->assertNull(LeafPolicy::storedProblem('&amp;amp;amp;', self::RULES), 'three rounds reach a fixed point');
    }

    public function test_shortcode_opener_refused(): void
    {
        foreach (['[gallery ids="1,2"]', 'Read [Contact-Form id=3]', '&#091;gallery&#093;', '&amp;#091;embed&amp;#093;'] as $s) {
            $this->assertSame('holds a shortcode opener', LeafPolicy::storedProblem($s, self::RULES), var_export($s, true));
        }

        $rules = ['refuse_braces' => false, 'forbidden' => ['%%x%%']];
        $this->assertSame('holds "%%x%%", which this builder refuses', LeafPolicy::storedProblem('a %%X%% b', $rules));
        $this->assertSame('holds "{echo:"', LeafPolicy::storedProblem('{ECHO:get_option}', self::RULES));

        $this->assertNull(LeafPolicy::storedProblem('&#091;12&#093; and [3] and [/x]', self::RULES), 'numbers and a closer are not openers');
    }

    public function test_sc10_brace_builder_refuses_site_title_token(): void
    {
        $title = LeafPolicy::htmlText('{site_title}');
        $this->assertSame('{site_title}', $title);

        $brace = 'holds a brace, which this builder reads as dynamic data';
        $this->assertSame($brace, LeafPolicy::storedProblem($title, self::BRACE_RULES));
        $this->assertSame($brace, LeafPolicy::storedProblem('&#123;site_title&#125;', self::BRACE_RULES), 'decoded braces count');
        $this->assertSame($brace, LeafPolicy::storedProblem('price }', self::BRACE_RULES));

        $tree = [$this->widget('h1', 'heading', ['title' => $title, 'header_size' => 'h2'])];
        $this->assertSame(
            ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings.title: ' . $brace],
            LeafPolicy::checkTree($tree, self::KEYS, self::BRACE_RULES)
        );

        $this->assertNull(LeafPolicy::storedProblem($title, self::RULES), 'a builder that keeps braces stores it');
        $this->assertNull(LeafPolicy::checkTree($tree, self::KEYS, self::RULES));
    }

    public function test_keys_outside_allowlist_refused(): void
    {
        $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'title_color' => '#fff'])];
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings: a settings key not on the adapter allowlist'],
            LeafPolicy::checkTree($tree, self::KEYS, self::RULES)
        );

        // An allowlist that names custom_attributes is overruled.
        $faulty                                 = self::KEYS;
        $faulty['heading']['custom_attributes'] = 'text';
        $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'custom_attributes' => 'data-track|1'])];
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings: key custom_attributes is never written'],
            LeafPolicy::checkTree($tree, $faulty, self::RULES)
        );

        $tree = [$this->widget('w1', 'icon-box', ['title_text' => 'Hi'])];
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0]: an element type not on the adapter allowlist'],
            LeafPolicy::checkTree($tree, self::KEYS, self::RULES)
        );

        $node           = $this->widget('h1', 'heading', ['title' => 'Hi']);
        $node['extras'] = true;
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0]: a node key outside id, elType, widgetType, isInner, settings and elements'],
            LeafPolicy::checkTree([$node], self::KEYS, self::RULES)
        );

        $node               = $this->container('c1', [], []);
        $node['widgetType'] = 'heading';
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0]: widgetType on an element that is not a widget'],
            LeafPolicy::checkTree([$node], self::KEYS, self::RULES)
        );
    }

    public function test_denied_key_refused_at_any_depth(): void
    {
        $deep = static function (array $settings): array {
            return [
                [
                    'id'       => 'c1',
                    'elType'   => 'container',
                    'settings' => [],
                    'elements' => [
                        [
                            'id'       => 'c2',
                            'elType'   => 'container',
                            'settings' => [],
                            'elements' => [
                                ['id' => 'b1', 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'button'],
                            ],
                            'isInner'  => true,
                        ],
                    ],
                    'isInner'  => false,
                ],
            ];
        };
        $at = 'elements[0].elements[0].elements[0]';

        $cases = [
            '__dynamic__'          => [['text' => 'Go', '__dynamic__' => ['text' => '[elementor-tag id="1"]']], '__dynamic__'],
            'nested __dynamic__'   => [['text' => 'Go', 'link' => ['url' => '/a', '__dynamic__' => []]], '__dynamic__'],
            'motion_fx prefix'     => [['text' => 'Go', 'motion_fx_motion_fx_scrolling' => 'yes'], 'motion_fx_*'],
            'sticky prefix'        => [['text' => 'Go', 'sticky_on' => ['desktop']], 'sticky*'],
            'case folded'          => [['text' => 'Go', 'Custom_CSS' => 'a{}'], 'custom_css'],
            '_element_id'          => [['text' => 'Go', '_element_id' => 'x'], '_element_id'],
        ];
        $faulty                  = self::KEYS;
        $faulty['button']        = $faulty['button'] + ['__dynamic__.text' => 'text', 'motion_fx_motion_fx_scrolling' => 'enum', '_element_id' => 'enum'];
        foreach ($cases as $name => [$settings, $entry]) {
            $this->assertSame(
                ['code' => 'adapter_key_not_allowed', 'detail' => $at . '.settings: key ' . $entry . ' is never written'],
                LeafPolicy::checkTree($deep($settings), $faulty, self::RULES),
                $name
            );
        }

        $node                = $this->widget('h1', 'heading', ['title' => 'Hi']);
        $node['__globals__'] = [];
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0]: key __globals__ is never written'],
            LeafPolicy::checkTree([$node], self::KEYS, self::RULES)
        );
    }

    public function test_denied_widget_type_refused(): void
    {
        $faulty = self::KEYS + [
            'html'               => ['html' => 'html'],
            'shortcode'          => ['shortcode' => 'text'],
            'wp-widget-archives' => [],
            'e-form-submit'      => [],
            'template'           => ['template_id' => 'int'],
        ];
        $cases  = [
            'html'               => [['html' => '<p>Hi</p>'], 'html'],
            'shortcode'          => [['shortcode' => 'Hi'], 'shortcode'],
            'wp-widget-archives' => [[], 'wp-widget-*'],
            'e-form-submit'      => [[], 'e-form*'],
            'template'           => [['template_id' => 7], 'template'],
        ];
        foreach ($cases as $type => [$settings, $entry]) {
            $tree = [$this->container('c1', [], [$this->widget('w1', $type, $settings)])];
            $this->assertSame(
                ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].elements[0]: element type ' . $entry . ' is never written'],
                LeafPolicy::checkTree($tree, $faulty, self::RULES),
                $type
            );
        }
    }

    public function test_safe_style_css_emptied_only_during_check(): void
    {
        $styled = '<p style="color:red">Hi</p>';
        $this->assertSame($styled, \wp_kses_post($styled), 'outside the check the site default keeps an allowed style');
        $this->ksesCalls = [];

        $this->assertSame('changed by the HTML sanitiser', LeafPolicy::storedProblem($styled, self::RULES));
        $this->assertCount(1, $this->ksesCalls);
        $this->assertSame([], $this->ksesCalls[0]['styles'], 'every style property refused during the check');
        $this->assertSame([PHP_INT_MAX], $this->ksesCalls[0]['priorities']);
        $this->assertArrayNotHasKey('safe_style_css', $this->filters, 'the filter is gone after the check');

        $this->assertNull(LeafPolicy::storedProblem('<p>Hi</p>', self::RULES));
        $this->assertArrayNotHasKey('safe_style_css', $this->filters);

        $this->ksesThrows = true;
        try {
            LeafPolicy::storedProblem('<p>Hi</p>', self::RULES);
            $this->fail('the sanitiser failure must reach the caller');
        } catch (\RuntimeException $e) {
            $this->assertSame('sanitiser failed', $e->getMessage());
        }
        $this->assertArrayNotHasKey('safe_style_css', $this->filters, 'removed even when the sanitiser throws');
        $this->ksesThrows = false;

        $site = static function (array $styles): array {
            return array_merge($styles, ['float']);
        };
        \add_filter('safe_style_css', $site, 10);
        $this->ksesCalls = [];
        $this->assertSame('changed by the HTML sanitiser', LeafPolicy::storedProblem($styled, self::RULES));
        $this->assertSame([10, PHP_INT_MAX], $this->ksesCalls[0]['priorities'], 'ours runs after the site filter');
        $this->assertSame([], $this->ksesCalls[0]['styles']);
        $this->assertSame(['safe_style_css' => [10 => [$site]]], $this->filters, 'the site filter stays');
    }

    public function test_l1_matches_gut_text_rules(): void
    {
        $gut    = new \ReflectionMethod(PageCreateBuilder::class, 'textProblem');
        $limits = [
            'text' => PageCreateBuilder::MAX_TEXT_CHARS,
            'cell' => PageCreateBuilder::MAX_CELL_CHARS,
            'alt'  => PageCreateBuilder::MAX_ALT_CHARS,
        ];
        $corpus = [
            '', ' ', 'Hello', 'a<b', 'a>b', '{{x}}', 'x}}', '{%', '%}', '<!--', '`', "a\nb", "\u{202E}x", "x\u{200B}",
            '[1]', '[12345]', '[gallery]', 'x]', '&amp;', 'Tom & Jerry', "\xff", 'café [3] done', '&#91;',
            str_repeat('a', 301), str_repeat('a', 501), str_repeat('a', 5001),
        ];
        foreach ($limits as $rule => $max) {
            foreach ($corpus as $text) {
                $this->assertSame(
                    $gut->invoke(null, $text, $max, $rule),
                    LeafPolicy::textProblem($text, $rule),
                    $rule . ' parity for ' . var_export(substr($text, 0, 20), true)
                );
            }
        }
        $this->assertSame('unknown text rule', LeafPolicy::textProblem('Hello', 'title'));
    }

    public function test_pins_select_the_extra_check(): void
    {
        $check = function (string $type, array $settings): ?array {
            return LeafPolicy::checkTree([$this->widget('w1', $type, $settings)], self::KEYS, self::RULES);
        };
        $unsafe = static function (string $path, string $why): array {
            return ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings.' . $path . ': ' . $why];
        };

        $this->assertSame($unsafe('title', 'an "&" that does not start a character reference'), $check('heading', ['title' => 'Tom & Jerry']));
        $this->assertNull($check('heading', ['title' => 'Tom &amp; Jerry', 'header_size' => 'h2']));

        $badUrl = 'not an address the page-create link or image rule accepts';
        $this->assertSame($unsafe('link.url', $badUrl), $check('button', ['text' => 'Go', 'link' => ['url' => 'javascript:alert(1)']]));
        $this->assertSame($unsafe('link.url', $badUrl), $check('button', ['text' => 'Go', 'link' => ['url' => 'https://user@example.com/']]));
        $this->assertNull($check('button', ['text' => 'Go', 'link' => ['url' => '/about', 'is_external' => '', 'nofollow' => '']]));
        $this->assertNull($check('image', ['image' => ['id' => 12, 'url' => 'http://example.com/a.png']]));

        $this->assertSame($unsafe('image.id', 'int value that is not an integer'), $check('image', ['image' => ['id' => '12']]));
        $this->assertSame($unsafe('header_size', 'enum value that is not a string'), $check('heading', ['title' => 'Hi', 'header_size' => true]));
        $this->assertSame($unsafe('title', 'html value that is not a string'), $check('heading', ['title' => null]));

        $odd = self::KEYS;
        $odd['heading']['title'] = 'raw';
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings.title: a pin outside html, text, url, int, enum'],
            LeafPolicy::checkTree([$this->widget('h1', 'heading', ['title' => 'Hi'])], $odd, self::RULES)
        );
    }

    public function test_clean_tree_passes(): void
    {
        $tree = [
            $this->container('c1', ['content_width' => 'boxed', 'flex_direction' => 'row'], [
                $this->container('c2', ['width' => ['unit' => '%', 'size' => 50, 'sizes' => []]], [
                    $this->widget('h1', 'heading', ['title' => LeafPolicy::htmlText('Fish & chips [1]'), 'header_size' => 'h2']),
                    $this->widget('t1', 'text-editor', ['editor' => '<p>' . LeafPolicy::htmlText('Open <daily>') . '</p>']),
                ]),
                $this->container('c3', [], [
                    $this->widget('b1', 'button', ['text' => 'Book', 'link' => ['url' => 'https://example.com/book?a=1', 'is_external' => '', 'nofollow' => '']]),
                    $this->widget('s1', 'spacer', ['space' => ['unit' => 'px', 'size' => 24, 'sizes' => []]]),
                    $this->widget('d1', 'spacer', []),
                ]),
            ]),
        ];
        $this->assertNull(LeafPolicy::checkTree($tree, self::KEYS, self::RULES));
        $this->assertNull(LeafPolicy::checkTree([], self::KEYS, self::RULES), 'an empty page');

        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements: not a list of nodes'],
            LeafPolicy::checkTree(['a' => $tree[0]], self::KEYS, self::RULES)
        );
        $this->assertSame(
            ['code' => 'leaf_unsafe', 'detail' => 'the adapter leaf rules are malformed'],
            LeafPolicy::checkTree($tree, self::KEYS, ['refuse_braces' => 'yes', 'forbidden' => []])
        );
        $this->assertSame('the adapter leaf rules are malformed', LeafPolicy::storedProblem('Hi', ['refuse_braces' => false, 'forbidden' => ['']]));
    }

    /**
     * @param array<string, mixed> $settings Settings.
     * @return array<string, mixed>
     */
    private function widget(string $id, string $type, array $settings): array
    {
        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => $type];
    }

    /**
     * @param array<string, mixed> $settings Settings.
     * @param list<mixed>          $children Child nodes.
     * @return array<string, mixed>
     */
    private function container(string $id, array $settings, array $children): array
    {
        return ['id' => $id, 'elType' => 'container', 'settings' => $settings, 'elements' => $children, 'isInner' => false];
    }

    /**
     * The sanitiser double.
     *
     * @param string $s Input.
     * @return string
     */
    private function kses(string $s): string
    {
        $styles = self::DEFAULT_STYLES;
        $hooks  = $this->filters['safe_style_css'] ?? [];
        ksort($hooks);
        foreach ($hooks as $callbacks) {
            foreach ($callbacks as $callback) {
                $styles = $callback($styles);
            }
        }
        $this->ksesCalls[] = ['input' => $s, 'styles' => $styles, 'priorities' => array_keys($hooks)];
        if ($this->ksesThrows) {
            throw new \RuntimeException('sanitiser failed');
        }

        $out = (string) preg_replace('/\s+on[a-z]+\s*=\s*(?:"[^"]*"|\'[^\']*\'|[^\s>]*)/i', '', $s);
        $out = (string) preg_replace_callback(
            '/<\/?([a-z][a-z0-9]*)\b[^>]*>/i',
            static fn (array $m): string => in_array(strtolower($m[1]), self::KEPT_TAGS, true) ? $m[0] : '',
            $out
        );

        return (string) preg_replace_callback(
            '/\s+style\s*=\s*"([^"]*)"/i',
            static function (array $m) use ($styles): string {
                $kept = [];
                foreach (explode(';', $m[1]) as $declaration) {
                    $property = strtolower(trim(explode(':', $declaration, 2)[0]));
                    if ($property !== '' && in_array($property, $styles, true)) {
                        $kept[] = trim($declaration);
                    }
                }

                return $kept === [] ? '' : ' style="' . implode(';', $kept) . '"';
            },
            $out
        );
    }
}
