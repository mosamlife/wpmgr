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
use PHPUnit\Framework\Attributes\DataProvider;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
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

    /** Every pin, as the refusal of an unknown one lists them. */
    private const PIN_OUTSIDE = 'a pin outside html, text, url, int, enum, slider, dimensions, gaps, colour, switcher, image, link';

    /** A pin carrying an argument its pin does not take. */
    private const PIN_ARGUMENT = 'a pin argument its pin does not take';

    private const NOT_SLIDER = 'not a slider value: exactly unit and size, unit one of px, em, vh, %, size a decimal number written as a string';

    private const NOT_DIMENSIONS = 'not a dimensions value: exactly unit "px", top, right, bottom and left as decimal numbers of 0 or more written as strings, and isLinked true or false';

    private const NOT_GAPS = 'not a gaps value: exactly column and row as decimal numbers of 0 or more written as strings, isLinked true or false, and unit "px"';

    private const NOT_COLOUR = 'not a colour: an upper-case #RRGGBB, transparent, or rgba(r,g,b,a) with no spaces';

    private const NOT_SWITCHER = 'not a switcher value: "yes" or ""';

    private const NOT_IMAGE = 'not an image value: exactly id and url, or id, url and source "library", with id a whole number above 0 written as a string';

    private const NOT_LINK = 'not a link value: exactly url, is_external and nofollow, with is_external and nofollow each "on" or ""';

    private const NOT_LISTED = 'not one of the values this key takes';

    /** A WPMgr section container's label, written out here rather than read from the class. */
    private const SECTION = "WPMgr \u{00B7} Section";

    /** A WPMgr pattern container's label. */
    private const FEATURES = "WPMgr \u{00B7} Features";

    /** The refusal of an anchor anywhere but the top of a section container's settings. */
    private const OFF_SECTION = 'key _element_id is written only at the top of a WPMgr section container\'s settings';

    /** The refusal of an anchor value outside the SP-09 form. */
    private const NOT_ANCHOR = 'not an anchor of a lower-case letter, then 1 to 39 lower-case letters, digits or hyphens';

    /** The refusal of a label outside the WPMgr form. */
    private const NOT_LABEL = 'not a label of the WPMgr prefix, then a name of letters and digits with single spaces or hyphens between words, up to 40 characters';

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
            ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a settings key not on the adapter allowlist'],
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

    public function test_anchor_written_only_on_a_wpmgr_section_container(): void
    {
        $keys = $this->ruledKeys();
        $page = [
            $this->container('s1', ['_title' => self::SECTION, '_element_id' => 'pricing', 'content_width' => 'boxed'], [
                $this->container('p1', ['_title' => self::FEATURES], [$this->widget('h1', 'heading', ['title' => 'Plans'])]),
            ]),
            // The anchor may come before the label.
            $this->container('s2', ['_element_id' => 'ab', '_title' => self::SECTION], []),
            $this->container('s3', ['_title' => self::SECTION, '_element_id' => 'a' . str_repeat('-9', 19) . 'z'], []),
            $this->container('s4', ['_title' => self::SECTION], []),
        ];
        $this->assertNull(LeafPolicy::checkTree($page, $keys, self::RULES), 'anchors on WPMgr section containers');
        $this->assertSame(
            ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a settings key not on the adapter allowlist'],
            LeafPolicy::checkTree([$page[0]], self::KEYS, self::RULES),
            'the adapter allowlist still has to name the key'
        );

        $deepButton = [$this->container('s1', ['_title' => self::SECTION], [
            $this->container('c1', [], [$this->widget('b1', 'button', ['text' => 'Go', '_element_id' => 'pricing'])]),
        ])];
        $offSection = [
            'nested container with the section label' => [
                [$this->container('s1', ['_title' => self::SECTION], [$this->container('s2', ['_element_id' => 'pricing', '_title' => self::SECTION], [])])],
                'elements[0].elements[0].settings',
            ],
            'top-level container with no label'       => [[$this->container('s1', ['_element_id' => 'pricing'], [])], 'elements[0].settings'],
            'top-level container with a pattern label' => [[$this->container('s1', ['_element_id' => 'pricing', '_title' => self::FEATURES], [])], 'elements[0].settings'],
            'widget inside a section'                 => [$deepButton, 'elements[0].elements[0].elements[0].settings'],
            'top-level widget'                        => [[$this->widget('b1', 'button', ['_element_id' => 'pricing'])], 'elements[0].settings'],
            'inside a settings object'                => [[$this->container('s1', ['_title' => self::SECTION, 'width' => ['unit' => '%', '_element_id' => 'pricing']], [])], 'elements[0].settings'],
        ];
        foreach ($offSection as $name => [$tree, $at]) {
            $this->assertSame(
                ['code' => 'adapter_key_not_allowed', 'detail' => $at . ': ' . self::OFF_SECTION],
                LeafPolicy::checkTree($tree, $keys, self::RULES),
                $name
            );
        }
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings: key _element_id is written only in lower case'],
            LeafPolicy::checkTree([$this->container('s1', ['_title' => self::SECTION, '_Element_ID' => 'pricing'], [])], $this->ruledKeys(['container' => ['_Element_ID' => 'text']]), self::RULES)
        );

        $badValues = [
            'upper case'       => 'Pricing',
            'one character'    => 'a',
            '41 characters'    => 'a' . str_repeat('-9', 19) . 'za',
            'digit first'      => '1pricing',
            'hyphen first'     => '-pricing',
            'trailing newline' => "pricing\n",
            'space'            => 'pri cing',
            'underscore'       => 'pricing_2',
            'empty'            => '',
            'not ASCII'        => "pr\u{00ED}cing",
            'integer'          => 5,
            'list'             => ['pricing'],
            'empty list'       => [],
        ];
        foreach ($badValues as $name => $value) {
            $this->assertSame(
                ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings._element_id: ' . self::NOT_ANCHOR],
                LeafPolicy::checkTree([$this->container('s1', ['_title' => self::SECTION, '_element_id' => $value], [])], $keys, self::RULES),
                $name
            );
        }
        foreach (['content', 'main', 'primary', 'page', 'masthead', 'colophon', 'site-header', 'site-footer', 'wpadminbar'] as $reserved) {
            $this->assertSame(
                ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings._element_id: an anchor the theme or WordPress already uses'],
                LeafPolicy::checkTree([$this->container('s1', ['_title' => self::SECTION, '_element_id' => $reserved], [])], $keys, self::RULES),
                $reserved
            );
        }
        $this->assertSame(
            ['code' => 'leaf_unsafe', 'detail' => 'elements[2].settings._element_id: an anchor an earlier element already uses'],
            LeafPolicy::checkTree([
                $this->container('s1', ['_title' => self::SECTION, '_element_id' => 'pricing'], []),
                $this->container('s2', ['_title' => self::SECTION, '_element_id' => 'faq'], []),
                $this->container('s3', ['_title' => self::SECTION, '_element_id' => 'pricing'], []),
            ], $keys, self::RULES)
        );
    }

    public function test_label_written_only_with_the_wpmgr_prefix(): void
    {
        $keys   = $this->ruledKeys();
        $labels = [self::SECTION, self::FEATURES, "WPMgr \u{00B7} Pricing card", "WPMgr \u{00B7} Call-to-action", "WPMgr \u{00B7} FAQ 2", "WPMgr \u{00B7} " . str_repeat('ab', 20)];
        foreach ($labels as $label) {
            $tree = [$this->container('s1', ['_title' => $label], [$this->container('c1', ['_title' => $label], [])])];
            $this->assertNull(LeafPolicy::checkTree($tree, $keys, self::RULES), $label);
        }

        $badValues = [
            'no prefix'           => 'Section',
            'prefix lower case'   => "wpmgr \u{00B7} Section",
            'prefix upper case'   => "WPMGR \u{00B7} Section",
            'hyphen for the dot'  => 'WPMgr - Section',
            'no spaces'           => "WPMgr\u{00B7}Section",
            'no-break space'      => "WPMgr\u{00A0}\u{00B7} Section",
            'character reference' => 'WPMgr &#183; Section',
            'prefix only'         => "WPMgr \u{00B7} ",
            'two spaces'          => "WPMgr \u{00B7}  Section",
            'trailing space'      => "WPMgr \u{00B7} Section ",
            'trailing newline'    => "WPMgr \u{00B7} Section\n",
            'digit first'         => "WPMgr \u{00B7} 2nd",
            'markup'              => "WPMgr \u{00B7} <b>Section</b>",
            'quote'               => "WPMgr \u{00B7} Section\"",
            '41 characters'       => "WPMgr \u{00B7} " . str_repeat('ab', 20) . 'c',
            'integer'             => 7,
            'list'                => [self::SECTION],
            'empty list'          => [],
        ];
        foreach ($badValues as $name => $value) {
            $this->assertSame(
                ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings._title: ' . self::NOT_LABEL],
                LeafPolicy::checkTree([$this->container('s1', ['_title' => $value], [])], $keys, self::RULES),
                $name
            );
        }

        $offContainer = 'key _title is written only at the top of a container\'s settings';
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].elements[0].settings: ' . $offContainer],
            LeafPolicy::checkTree([$this->container('s1', [], [$this->widget('b1', 'button', ['text' => 'Go', '_title' => self::SECTION])])], $keys, self::RULES),
            'a widget'
        );
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings: ' . $offContainer],
            LeafPolicy::checkTree([$this->container('s1', ['width' => ['unit' => '%', '_title' => self::SECTION]], [])], $keys, self::RULES),
            'inside a settings object'
        );
        $this->assertSame(
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings: key _title is written only in lower case'],
            LeafPolicy::checkTree([$this->container('s1', ['_TITLE' => self::SECTION], [])], $this->ruledKeys(['container' => ['_TITLE' => 'text']]), self::RULES)
        );
    }

    public function test_globals_stays_denied(): void
    {
        $this->assertContains('__globals__', LeafPolicy::DENIED_KEYS);

        // An allowlist that names a binding is overruled, even on a section container that is otherwise accepted.
        $keys    = $this->ruledKeys();
        $section = ['_title' => self::SECTION, '_element_id' => 'pricing'];
        $this->assertNull(LeafPolicy::checkTree([$this->container('s1', $section, [])], $keys, self::RULES));
        $cases = [
            'binding'     => [[$this->container('s1', $section + ['__globals__' => ['background_color' => 'globals/colors?id=primary']], [])], 'elements[0].settings'],
            'empty map'   => [[$this->container('s1', $section + ['__globals__' => []], [])], 'elements[0].settings'],
            'case folded' => [[$this->container('s1', $section + ['__GLOBALS__' => ['background_color' => 'globals/colors?id=primary']], [])], 'elements[0].settings'],
            'nested'      => [[$this->container('s1', $section + ['width' => ['unit' => '%', '__globals__' => []]], [])], 'elements[0].settings'],
            'on a widget' => [[$this->container('s1', $section, [$this->widget('h1', 'heading', ['title' => 'Hi', '__globals__' => ['title_color' => 'globals/colors?id=primary']])])], 'elements[0].elements[0].settings'],
        ];
        foreach ($cases as $name => [$tree, $at]) {
            $this->assertSame(
                ['code' => 'adapter_key_not_allowed', 'detail' => $at . ': key __globals__ is never written'],
                LeafPolicy::checkTree($tree, $keys, self::RULES),
                $name
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
            ['code' => 'adapter_key_not_allowed', 'detail' => 'elements[0].settings.title: ' . self::PIN_OUTSIDE],
            LeafPolicy::checkTree([$this->widget('h1', 'heading', ['title' => 'Hi'])], $odd, self::RULES)
        );
    }

    /**
     * A shape pin takes the whole value at its path in exactly the measured
     * form and nothing near it: numbers as decimal strings, flags as booleans,
     * no missing, extra or misspelt member, no unit or colour form beyond the
     * measured ones, and an enum with a list takes only a listed value.
     *
     * @param string                       $pin     Pin of the path "k".
     * @param mixed                        $value   Value at "k".
     * @param array{0: string, 1: string}|null $refusal Code and reason, or null when the value is taken.
     */
    #[DataProvider('shapeRows')]
    public function test_shape_pins_take_only_the_measured_forms(string $pin, $value, ?array $refusal): void
    {
        $got = LeafPolicy::checkTree([$this->container('c1', ['k' => $value], [])], ['container' => ['k' => $pin]], self::RULES);

        $this->assertSame($refusal === null ? null : ['code' => $refusal[0], 'detail' => 'elements[0].settings.k: ' . $refusal[1]], $got);
    }

    /**
     * Taken rows are values of the frozen recipe trees; refused rows are the
     * near misses Elementor would store and then ignore or misrender.
     *
     * @return array<string, array{0: string, 1: mixed, 2: array{0: string, 1: string}|null}>
     */
    public static function shapeRows(): array
    {
        $u   = 'leaf_unsafe';
        $k   = 'adapter_key_not_allowed';
        $pad = ['unit' => 'px', 'top' => '96', 'right' => '24', 'bottom' => '96', 'left' => '24', 'isLinked' => false];
        $gap = ['column' => '32', 'row' => '32', 'isLinked' => true, 'unit' => 'px'];
        $img = ['url' => 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png', 'id' => '5', 'source' => 'library'];
        $lnk = ['url' => 'https://example.com/partner', 'is_external' => 'on', 'nofollow' => 'on'];

        return [
            'slider px'                       => ['slider', ['unit' => 'px', 'size' => '15'], null],
            'slider em negative'              => ['slider', ['unit' => 'em', 'size' => '-0.02'], null],
            'slider em fraction'              => ['slider', ['unit' => 'em', 'size' => '1.05'], null],
            'slider vh'                       => ['slider', ['unit' => 'vh', 'size' => '80'], null],
            'slider vh zero'                  => ['slider', ['unit' => 'vh', 'size' => '0'], null],
            'slider percent'                  => ['slider', ['unit' => '%', 'size' => '55'], null],
            'slider opacity fraction'         => ['slider', ['unit' => 'px', 'size' => '0.45'], null],
            'slider members in any order'     => ['slider', ['size' => '18', 'unit' => 'px'], null],
            'slider narrowed unit'            => ['slider:vh', ['unit' => 'vh', 'size' => '80'], null],
            'slider two units'                => ['slider:px|em', ['unit' => 'em', 'size' => '1'], null],
            'slider integer size'             => ['slider', ['unit' => 'px', 'size' => 24], [$u, self::NOT_SLIDER]],
            'slider float size'               => ['slider', ['unit' => 'px', 'size' => 0.45], [$u, self::NOT_SLIDER]],
            'slider with sizes'               => ['slider', ['unit' => 'px', 'size' => '24', 'sizes' => []], [$u, self::NOT_SLIDER]],
            'slider without size'             => ['slider', ['unit' => 'px'], [$u, self::NOT_SLIDER]],
            'slider rem'                      => ['slider', ['unit' => 'rem', 'size' => '1'], [$u, self::NOT_SLIDER]],
            'slider custom unit'              => ['slider', ['unit' => 'custom', 'size' => 'clamp(1rem, 2vw, 3rem)'], [$u, self::NOT_SLIDER]],
            'slider size with unit'           => ['slider', ['unit' => 'px', 'size' => '24px'], [$u, self::NOT_SLIDER]],
            'slider size newline'             => ['slider', ['unit' => 'px', 'size' => "24\n"], [$u, self::NOT_SLIDER]],
            'slider size exponent'            => ['slider', ['unit' => 'px', 'size' => '1e3'], [$u, self::NOT_SLIDER]],
            'slider size empty'               => ['slider', ['unit' => 'px', 'size' => ''], [$u, self::NOT_SLIDER]],
            'slider size leading zero'        => ['slider', ['unit' => 'px', 'size' => '024'], [$u, self::NOT_SLIDER]],
            'slider as a string'              => ['slider', '24', [$u, self::NOT_SLIDER]],
            'slider empty'                    => ['slider', [], [$u, self::NOT_SLIDER]],
            'slider unit outside the pin'     => ['slider:vh', ['unit' => 'px', 'size' => '80'], [$u, 'not a slider value: exactly unit and size, unit one of vh, size a decimal number written as a string']],
            'dimensions padding'              => ['dimensions', $pad, null],
            'dimensions linked fraction'      => ['dimensions', ['unit' => 'px', 'top' => '1.5', 'right' => '1.5', 'bottom' => '1.5', 'left' => '1.5', 'isLinked' => true], null],
            'dimensions number member'        => ['dimensions', ['top' => 96] + $pad, [$u, self::NOT_DIMENSIONS]],
            'dimensions flag as empty string' => ['dimensions', ['isLinked' => ''] + $pad, [$u, self::NOT_DIMENSIONS]],
            'dimensions flag as "1"'          => ['dimensions', ['isLinked' => '1'] + $pad, [$u, self::NOT_DIMENSIONS]],
            'dimensions em'                   => ['dimensions', ['unit' => 'em'] + $pad, [$u, self::NOT_DIMENSIONS]],
            'dimensions without left'         => ['dimensions', array_diff_key($pad, ['left' => true]), [$u, self::NOT_DIMENSIONS]],
            'dimensions misspelt member'      => ['dimensions', ['Top' => '96'] + array_diff_key($pad, ['top' => true]), [$u, self::NOT_DIMENSIONS]],
            'dimensions extra member'         => ['dimensions', $pad + ['sizes' => []], [$u, self::NOT_DIMENSIONS]],
            'dimensions negative member'      => ['dimensions', ['left' => '-8'] + $pad, [$u, self::NOT_DIMENSIONS]],
            'gaps'                            => ['gaps', $gap, null],
            'gaps as a slider'                => ['gaps', ['unit' => 'px', 'size' => '32'], [$u, self::NOT_GAPS]],
            'gaps number member'              => ['gaps', ['column' => 32] + $gap, [$u, self::NOT_GAPS]],
            'gaps flag as a string'           => ['gaps', ['isLinked' => 'yes'] + $gap, [$u, self::NOT_GAPS]],
            'colour hex'                      => ['colour', '#F6F5F2', null],
            'colour white'                    => ['colour', '#FFFFFF', null],
            'colour transparent'              => ['colour', 'transparent', null],
            'colour rgba'                     => ['colour', 'rgba(246,245,242,0.8)', null],
            'colour rgba opaque'              => ['colour', 'rgba(21,23,28,1)', null],
            'colour lower-case hex'           => ['colour', '#f6f5f2', [$u, self::NOT_COLOUR]],
            'colour short hex'                => ['colour', '#FFF', [$u, self::NOT_COLOUR]],
            'colour hex with alpha'           => ['colour', '#F6F5F2FF', [$u, self::NOT_COLOUR]],
            'colour rgba with spaces'         => ['colour', 'rgba(246, 245, 242, 0.8)', [$u, self::NOT_COLOUR]],
            'colour channel above 255'        => ['colour', 'rgba(256,0,0,1)', [$u, self::NOT_COLOUR]],
            'colour alpha above 1'            => ['colour', 'rgba(1,2,3,1.5)', [$u, self::NOT_COLOUR]],
            'colour name'                     => ['colour', 'red', [$u, self::NOT_COLOUR]],
            'colour variable'                 => ['colour', 'var(--e-global-color-primary)', [$u, self::NOT_COLOUR]],
            'colour with a declaration'       => ['colour', '#F6F5F2;background:url(x)', [$u, self::NOT_COLOUR]],
            'colour newline'                  => ['colour', "#F6F5F2\n", [$u, self::NOT_COLOUR]],
            'colour empty list'               => ['colour', [], [$u, self::NOT_COLOUR]],
            'switcher on'                     => ['switcher', 'yes', null],
            'switcher off'                    => ['switcher', '', null],
            'switcher "on"'                   => ['switcher', 'on', [$u, self::NOT_SWITCHER]],
            'switcher boolean'                => ['switcher', true, [$u, self::NOT_SWITCHER]],
            'switcher upper case'             => ['switcher', 'YES', [$u, self::NOT_SWITCHER]],
            'image background'                => ['image', ['id' => '5', 'url' => 'https://example.com/wp-content/uploads/2026/10/hero-1600x900.jpg'], null],
            'image library'                   => ['image', $img, null],
            'image number id'                 => ['image', ['id' => 5] + $img, [$u, self::NOT_IMAGE]],
            'image zero id'                   => ['image', ['id' => '0'] + $img, [$u, self::NOT_IMAGE]],
            'image leading zero id'           => ['image', ['id' => '05'] + $img, [$u, self::NOT_IMAGE]],
            'image other source'              => ['image', ['source' => 'url'] + $img, [$u, self::NOT_IMAGE]],
            'image without id'                => ['image', array_diff_key($img, ['id' => true]), [$u, self::NOT_IMAGE]],
            'image extra member'              => ['image', $img + ['alt' => 'Van'], [$u, self::NOT_IMAGE]],
            'image script address'            => ['image', ['url' => 'javascript:alert(1)'] + $img, [$u, 'not an address the page-create image rule accepts']],
            'image path address'              => ['image', ['url' => '/wp-content/uploads/van.png'] + $img, [$u, 'not an address the page-create image rule accepts']],
            'link external nofollow'          => ['link', $lnk, null],
            'link section anchor'             => ['link', ['url' => '#signup', 'is_external' => '', 'nofollow' => ''], null],
            'link site path'                  => ['link', ['url' => '/about', 'is_external' => '', 'nofollow' => ''], null],
            'link without nofollow'           => ['link', array_diff_key($lnk, ['nofollow' => true]), [$u, self::NOT_LINK]],
            'link nofollow "yes"'             => ['link', ['nofollow' => 'yes'] + $lnk, [$u, self::NOT_LINK]],
            'link boolean flag'               => ['link', ['is_external' => true] + $lnk, [$u, self::NOT_LINK]],
            'link custom attributes'          => ['link', $lnk + ['custom_attributes' => 'data-x|1'], [$k, 'key custom_attributes is never written']],
            'link dynamic member'             => ['link', $lnk + ['__dynamic__' => ['url' => 'x']], [$k, 'key __dynamic__ is never written']],
            'link script address'             => ['link', ['url' => 'javascript:alert(1)'] + $lnk, [$u, 'not an address the page-create link rule accepts']],
            'link anchor in upper case'       => ['link', ['url' => '#Signup'] + $lnk, [$u, 'not an address the page-create link rule accepts']],
            'link with a user'                => ['link', ['url' => 'https://user@example.com/'] + $lnk, [$u, 'not an address the page-create link rule accepts']],
            'enum listed'                     => ['enum:boxed|full', 'full', null],
            'enum only empty'                 => ['enum:', '', null],
            'enum with a space'               => ['enum:center center', 'center center', null],
            'enum number string'              => ['enum:400|600|700', '600', null],
            'enum other case'                 => ['enum:boxed|full', 'Boxed', [$u, self::NOT_LISTED]],
            'enum unlisted'                   => ['enum:boxed|full', 'wide', [$u, self::NOT_LISTED]],
            'enum boolean'                    => ['enum:boxed|full', true, [$u, self::NOT_LISTED]],
            'enum empty list'                 => ['enum:boxed|full', [], [$u, self::NOT_LISTED]],
            'enum empty string unlisted'      => ['enum:boxed|full', '', [$u, self::NOT_LISTED]],
            'argument on colour'              => ['colour:upper', '#FFFFFF', [$k, self::PIN_ARGUMENT]],
            'argument on switcher'            => ['switcher:yes', 'yes', [$k, self::PIN_ARGUMENT]],
            'argument on int'                 => ['int:5', 5, [$k, self::PIN_ARGUMENT]],
            'slider unit not measured'        => ['slider:rem', ['unit' => 'rem', 'size' => '1'], [$k, self::PIN_ARGUMENT]],
            'slider empty unit list'          => ['slider:', ['unit' => 'px', 'size' => '1'], [$k, self::PIN_ARGUMENT]],
            'unknown pin with argument'       => ['raw:x', 'a', [$k, self::PIN_OUTSIDE]],
        ];
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

    public function testUnknownStyleKeyRefused(): void
    {
        $unlisted = ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a settings key not on the adapter allowlist'];
        $keys     = ElementorClassicMapper::ALLOWED_KEYS;

        // A style key E1 did not prove, a misspelt one, and unproven members of a proven group.
        foreach (['title_shadow' => '#15171C', 'title_colour' => '#15171C', 'typography_font_style' => 'italic', 'typography_word_spacing' => ['unit' => 'em', 'size' => '1']] as $key => $value) {
            $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'typography_typography' => 'custom', $key => $value])];
            $this->assertSame($unlisted, LeafPolicy::checkTree($tree, $keys, self::RULES), $key);
        }
        // A key listed for another element is not listed for this one.
        $this->assertSame($unlisted, LeafPolicy::checkTree([$this->widget('t1', 'text-editor', ['editor' => '<p>Hi</p>', 'title_color' => '#15171C'])], $keys, self::RULES));
        // An unproven group stays unlisted: a box shadow on a section.
        $section = ['_title' => self::SECTION, 'box_shadow_box_shadow_type' => 'yes'];
        $this->assertSame($unlisted, LeafPolicy::checkTree([$this->container('s1', $section, [])], $keys, self::RULES));
        // A path inside a value that a dotted allowlist does not list.
        $this->assertSame($unlisted, LeafPolicy::checkTree([$this->container('s1', ['width' => ['unit' => '%', 'size' => '5', 'sizes' => [], 'extra' => 'x']], [])], $keys, self::RULES));
        // The refusal never echoes the key read from the tree.
        $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'zz_unique_key' => '#15171C'])];
        $this->assertStringNotContainsString('zz_unique_key', (string) (LeafPolicy::checkTree($tree, $keys, self::RULES)['detail'] ?? ''));

        // Every listed style key in its measured form is accepted.
        $tree = [$this->widget('h1', 'heading', [
            'title'                     => 'Hi',
            'title_color'               => '#15171C',
            'typography_typography'     => 'custom',
            'typography_font_family'    => 'Work Sans',
            'typography_font_size'      => ['unit' => 'px', 'size' => '49'],
            'typography_font_weight'    => '700',
            'typography_line_height'    => ['unit' => 'em', 'size' => '1.05'],
            'typography_letter_spacing' => ['unit' => 'em', 'size' => '-0.02'],
        ])];
        $this->assertNull(LeafPolicy::checkTree($tree, $keys, self::RULES));

        // A listed font family key holds a family name, which reaches the post CSS.
        foreach (['Work Sans", serif; color: red', '', str_repeat('A', 61), "Work\nSans", "Work Sans\n"] as $bad) {
            $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'typography_typography' => 'custom', 'typography_font_family' => $bad])];
            $this->assertSame(
                ['code' => 'leaf_unsafe', 'detail' => 'elements[0].settings.typography_font_family: not a font family name of 1 to 60 letters, digits and spaces'],
                LeafPolicy::checkTree($tree, $keys, self::RULES),
                var_export($bad, true)
            );
        }
    }

    public function testLaptopSuffixRefused(): void
    {
        // Even an allowlist that lists the override and its control cannot have it written.
        foreach (LeafPolicy::BREAKPOINTS_NOT_WRITTEN as $breakpoint) {
            $key  = 'typography_font_size_' . $breakpoint;
            $keys = $this->ruledKeys(['heading' => ['typography_typography' => 'enum:custom', 'typography_font_size' => 'slider:px', $key => 'slider:px']]);
            $tree = [$this->widget('h1', 'heading', ['title' => 'Hi', 'typography_typography' => 'custom', $key => ['unit' => 'px', 'size' => '40']])];
            $this->assertSame(
                ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a ' . $breakpoint . ' override, which WPMgr does not write; it writes only tablet and mobile overrides'],
                LeafPolicy::checkTree($tree, $keys, self::RULES),
                $breakpoint
            );
        }
        $tree = [$this->container('s1', ['_title' => self::SECTION, 'padding_laptop' => ['unit' => 'px', 'top' => '9', 'right' => '9', 'bottom' => '9', 'left' => '9', 'isLinked' => true]], [])];
        $this->assertSame('style_key_not_allowed', LeafPolicy::checkTree($tree, ElementorClassicMapper::ALLOWED_KEYS, self::RULES)['code'] ?? null);

        // A key that only looks like an override, or names no control, is judged by the allowlist alone.
        $keys = $this->ruledKeys(['heading' => ['_laptop' => 'text', 'laptop' => 'text']]);
        $this->assertNull(LeafPolicy::checkTree([$this->widget('h1', 'heading', ['title' => 'Hi', '_laptop' => 'x', 'laptop' => 'y'])], $keys, self::RULES));
    }

    public function test_overrides_follow_the_sites_active_breakpoints(): void
    {
        $sides   = static fn (string $v): array => ['unit' => 'px', 'top' => $v, 'right' => '24', 'bottom' => $v, 'left' => '24', 'isLinked' => false];
        $section = ['_title' => self::SECTION, 'padding' => $sides('96'), 'padding_tablet' => $sides('72'), 'padding_mobile' => $sides('56')];
        $tree    = [$this->container('s1', $section, [])];
        $check   = static fn (?array $active): ?array => LeafPolicy::checkTree($tree, LeafPolicy::forBreakpoints(ElementorClassicMapper::ALLOWED_KEYS, $active), self::RULES);

        $this->assertNull($check(['mobile', 'tablet']));
        $this->assertNull($check(['mobile', 'tablet', 'laptop']));
        $tablet = ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a tablet override WPMgr writes only for a control it lists as responsive on this element, on a site with the tablet breakpoint active'];
        $mobile = ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a mobile override WPMgr writes only for a control it lists as responsive on this element, on a site with the mobile breakpoint active'];
        $this->assertSame($tablet, $check(['mobile']));
        $this->assertSame($tablet, $check(['laptop']));
        $this->assertSame($tablet, $check([]));
        $this->assertSame($tablet, $check(null), 'no known breakpoint means none is active');
        $this->assertSame($mobile, $check(['tablet']));

        // Without the overrides the same section passes on a site with none active.
        unset($section['padding_tablet'], $section['padding_mobile']);
        $this->assertNull(LeafPolicy::checkTree([$this->container('s1', $section, [])], LeafPolicy::forBreakpoints(ElementorClassicMapper::ALLOWED_KEYS, null), self::RULES));

        // forBreakpoints() drops exactly the overrides of inactive breakpoints, dotted paths too.
        $site = LeafPolicy::forBreakpoints(['container' => ['flex_gap.size' => 'enum', 'flex_gap_mobile.size' => 'enum', 'padding_tablet' => 'dimensions', 'mobile' => 'text']], ['tablet']);
        $this->assertSame(['container' => ['flex_gap.size' => 'enum', 'padding_tablet' => 'dimensions', 'mobile' => 'text']], $site);

        // A text editor's size is not proven responsive: its override is refused on every site.
        $text = ['editor' => '<p>Hi</p>', 'typography_typography' => 'custom', 'typography_font_size' => ['unit' => 'px', 'size' => '16'], 'typography_font_size_mobile' => ['unit' => 'px', 'size' => '15']];
        $this->assertSame($mobile, LeafPolicy::checkTree([$this->widget('t1', 'text-editor', $text)], LeafPolicy::forBreakpoints(ElementorClassicMapper::ALLOWED_KEYS, ['mobile', 'tablet']), self::RULES));

        // An allowlist that lists an override without its control cannot have it written.
        $keys = $this->ruledKeys(['heading' => ['title_color_tablet' => 'colour']]);
        $this->assertSame(
            ['code' => 'style_key_not_allowed', 'detail' => 'elements[0].settings: a tablet override of a control the allowlist does not list for this element'],
            LeafPolicy::checkTree([$this->widget('h1', 'heading', ['title' => 'Hi', 'title_color_tablet' => '#15171C'])], $keys, self::RULES)
        );
    }

    /**
     * The test allowlist with the anchor, the label and a binding listed on
     * types and paths where the policy must still refuse them, so a refusal
     * proves the policy and not the allowlist.
     *
     * @param array<string, array<string, string>> $more Extra paths by type.
     * @return array<string, array<string, string>>
     */
    private function ruledKeys(array $more = []): array
    {
        $keys               = self::KEYS;
        $keys['container'] += [
            '_element_id'                  => 'text',
            '_title'                       => 'text',
            'width._element_id'            => 'text',
            'width._title'                 => 'text',
            'width.__globals__'            => 'text',
            '__globals__.background_color' => 'text',
            '__GLOBALS__.background_color' => 'text',
        ];
        $keys['button']    += ['_element_id' => 'text', '_title' => 'text'];
        $keys['heading']   += ['__globals__.title_color' => 'text'];
        foreach ($more as $type => $paths) {
            $keys[$type] = ($keys[$type] ?? []) + $paths;
        }

        return $keys;
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
