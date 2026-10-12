<?php
/**
 * page-edit on a designed page: the input's design, the facts the page's own
 * tree gives the validator, set_text on a pattern path, and inserted and
 * replaced sections compiled with the design.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LayoutOps;
use WPMgr\Agent\Abilities\Builders\OutlineV3;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\PageEditValidator
 * @covers \WPMgr\Agent\Abilities\Builders\LayoutOps
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 */
final class PageEditDesignedTest extends TestCase
{
    private const FP = '7787514f2667f5577151c00759c33e4553edc1d75f96e0dc1ddcdc23989a5ea7';

    private const PAGE = '[{"type":"section","anchor":"top","children":[{"type":"paragraph","text":"Welcome."},{"type":"buttons","buttons":[{"text":"See plans","url":"#plans"}]}]},'
        . '{"type":"section","anchor":"plans","tone":"surface","children":[{"type":"pattern","pattern":"pricing","variant":"three-plans","plans":['
        . '{"name":"Starter","price":"$19","period":"per month","features":["One site","Email support"],"cta":{"text":"Choose Starter","url":"https://example.com/start"}},'
        . '{"name":"Growth","price":"$29","period":"per month","features":["Five sites","Chat support"],"cta":{"text":"Choose Growth","url":"https://example.com/grow"}},'
        . '{"name":"Scale","price":"$99","period":"per month","features":["Fifty sites","Phone support"],"cta":{"text":"Choose Scale","url":"https://example.com/scale"}}]}]}]';

    private const NEW_SECTION = '[{"type":"section","anchor":"press","children":[{"type":"paragraph","text":"Where we were mentioned."}]}]';

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

    // -------------------------------------------------------------------------
    // parse(): the design member
    // -------------------------------------------------------------------------

    public function test_parse_takes_the_design_last_and_judges_it(): void
    {
        $ops   = [['op' => 'remove', 'ref' => 'abc1234']];
        $plain = PageEditValidator::parse(self::input($ops));
        $this->assertArrayNotHasKey('design', $plain['input'] ?? ['design' => 1], 'no design member, no design in the parsed input');

        $r = PageEditValidator::parse(self::input($ops, self::design()));
        $this->assertArrayHasKey('input', $r, $r['detail'] ?? '');
        $this->assertSame(['post_id', 'base_fingerprint', 'operations', 'design'], array_keys($r['input']));
        $this->assertSame('Manrope', $r['input']['design']['fonts']['heading']);

        $missing = self::design();
        unset($missing['radius']);
        $r = PageEditValidator::parse(self::input($ops, $missing));
        $this->assertSame('design_not_ready', $r['code'] ?? null, 'a design with a field left out is the not-ready refusal');

        $r = PageEditValidator::parse(self::input($ops, 'not an object'));
        $this->assertArrayHasKey('code', $r);
        $this->assertArrayNotHasKey('input', $r);
    }

    // -------------------------------------------------------------------------
    // The page's own tree gives the validator its facts
    // -------------------------------------------------------------------------

    public function test_projection_gives_the_checks_anchors_links_and_pattern_names_and_the_answer_none(): void
    {
        $tree  = self::page();
        $nodes = ElementorClassicMapper::project($tree)->nodes();
        $kinds = array_column($nodes, 'kind');
        $this->assertSame('section', $kinds[0], 'a designed section is a section');
        $this->assertSame('top', $nodes[0]['anchor']);
        $button = self::first($nodes, 'buttons');
        $this->assertSame(['#plans'], $button['links']);
        $pattern = self::first($nodes, 'pattern');
        $this->assertSame('pricing', $pattern['pattern']);

        $answer = ElementorClassicMapper::project($tree)->toArray(500)['nodes'];
        foreach ($answer as $node) {
            $this->assertSame([], array_values(array_intersect(array_keys($node), ['anchor', 'links', 'pattern'])), 'the answer to the AI keeps its shape');
        }
    }

    public function test_the_validator_judges_a_real_page_by_what_its_tree_says(): void
    {
        $nodes   = ElementorClassicMapper::project(self::page())->nodes();
        $plans   = self::sectionOf($nodes, 'plans');
        $button  = self::first($nodes, 'buttons');
        $caps    = ['operations' => ['set_text', 'insert', 'replace', 'remove', 'move']];
        $removed = PageEditValidator::parse(self::input([['op' => 'remove', 'ref' => $plans['ref']]]));
        $refused = PageEditValidator::againstPage($removed['input'], $nodes, $caps);
        $this->assertSame('anchor_in_use', $refused['code'] ?? null);
        $this->assertSame([$button['ref']], $refused['refs'] ?? null, 'the refusal names the button that links to the section');

        $both = PageEditValidator::parse(self::input([
            ['op' => 'remove', 'ref' => $plans['ref']],
            ['op' => 'remove', 'ref' => $button['ref']],
        ]));
        $this->assertNull(PageEditValidator::againstPage($both['input'], $nodes, $caps), 'taking the link away with the section is fine');

        $into = PageEditValidator::parse(self::input([['op' => 'insert', 'into' => $plans['ref'], 'outline' => self::fragment(self::NEW_SECTION)]]));
        $this->assertSame('section_at_top_only', PageEditValidator::againstPage($into['input'], $nodes, $caps)['detail'] ?? null);
    }

    // -------------------------------------------------------------------------
    // set_text on a pattern path, and inserted sections compiled with the design
    // -------------------------------------------------------------------------

    public function test_set_text_on_a_pattern_path_changes_that_text_and_nothing_else(): void
    {
        $tree    = self::page();
        $pattern = self::first(ElementorClassicMapper::project($tree)->nodes(), 'pattern');
        $this->assertContains('plans.1.price', $pattern['editable']);

        $r = LayoutOps::apply($tree, [['op' => 'set_text', 'ref' => $pattern['ref'], 'field' => 'plans.1.price', 'text' => '$59']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000011'), [], true, self::compiler());
        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $this->assertSame(
            str_replace('"$29"', '"$59"', (string) json_encode($tree)),
            json_encode($r['tree']),
            'only the price of the second plan differs'
        );
        $this->assertSame(['$29'], array_values((array) $r['changes'][0]['before']));

        $notOffered = LayoutOps::apply($tree, [['op' => 'set_text', 'ref' => $pattern['ref'], 'field' => 'plans.3.price', 'text' => '$59']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000012'), [], true, self::compiler());
        $this->assertSame('node_not_editable', $notOffered['code'] ?? null);

        $markup = LayoutOps::apply($tree, [['op' => 'set_text', 'ref' => $pattern['ref'], 'field' => 'plans.1.cta.url', 'text' => 'https://example.com/a&b']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000013'), [], true, self::compiler());
        $this->assertSame('link_invalid', $markup['code'] ?? null, 'a link on an Elementor page holds no "&"');

        $anchor = LayoutOps::apply($tree, [['op' => 'set_text', 'ref' => $pattern['ref'], 'field' => 'plans.1.cta.url', 'text' => '#top']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000014'), [], true, self::compiler());
        $this->assertArrayHasKey('tree', $anchor, 'a link to a section of the page is a link: ' . (string) ($anchor['detail'] ?? ''));
    }

    public function test_an_inserted_section_is_compiled_with_the_design_and_only_with_one(): void
    {
        $tree  = self::page();
        $nodes = ElementorClassicMapper::project($tree)->nodes();
        $ops   = [['op' => 'insert', 'after' => $nodes[0]['ref'], 'outline' => self::fragment(self::NEW_SECTION)]];

        $r = LayoutOps::apply($tree, $ops, new IdSeed('b7e2a4c1-0000-4000-8000-000000000015'), [], true, self::compiler());
        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $this->assertCount(3, $r['tree']);
        $made = $r['tree'][1];
        $this->assertSame('press', $made['settings']['_element_id']);
        $this->assertSame('WPMgr · Section', $made['settings']['_title']);
        $this->assertSame('Where we were mentioned.', strip_tags($made['elements'][0]['settings']['editor']));

        $without = LayoutOps::apply($tree, $ops, new IdSeed('b7e2a4c1-0000-4000-8000-000000000015'), [], true, null);
        $this->assertArrayHasKey('code', $without, 'a section is not built without the design');
    }

    public function test_a_paragraph_inserted_into_a_designed_section_takes_the_sections_tone(): void
    {
        $tree  = self::page();
        $nodes = ElementorClassicMapper::project($tree)->nodes();
        $outer = self::fragment('[{"type":"paragraph","text":"A note."}]');
        $r     = LayoutOps::apply($tree, [['op' => 'insert', 'into' => $nodes[0]['ref'], 'position' => 'last', 'outline' => $outer]], new IdSeed('b7e2a4c1-0000-4000-8000-000000000016'), [], true, self::compiler());
        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $kids = $r['tree'][0]['elements'];
        $last = end($kids);
        $this->assertSame('text-editor', $last['widgetType']);
        $first = $kids[0];
        $this->assertSame($first['settings']['text_color'] ?? null, $last['settings']['text_color'] ?? null, 'the new paragraph is coloured as the section\'s own');
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @return array<string,mixed>
     */
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
            'fonts'   => ['heading' => 'Manrope', 'body' => 'Work Sans'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ];
    }

    private static function compiler(): ElementorSectionCompiler
    {
        return new ElementorSectionCompiler(DesignTokens::fromDesign(self::design()), ['mobile', 'tablet'], []);
    }

    /**
     * The designed page, compiled as page-create compiles it.
     *
     * @return list<array<string,mixed>>
     */
    private static function page(): array
    {
        $outline = OutlineV3::validate(json_decode(self::PAGE, false));
        self::assertArrayHasKey('outline', $outline, (string) ($outline['detail'] ?? ''));
        $mapped = ElementorClassicMapper::map(['outline' => $outline['outline']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000001'), [], true, self::compiler());
        self::assertArrayHasKey('tree', $mapped, (string) ($mapped['detail'] ?? ''));

        return $mapped['tree'];
    }

    /**
     * @return list<array<string,mixed>>
     */
    private static function fragment(string $json): array
    {
        $r = OutlineV3::fragment(json_decode($json, false));
        self::assertArrayHasKey('outline', $r, (string) ($r['detail'] ?? ''));

        return $r['outline'];
    }

    /**
     * @param list<array<string,mixed>> $nodes  Projection nodes.
     * @param string                    $anchor The section's anchor.
     * @return array<string,mixed>
     */
    private static function sectionOf(array $nodes, string $anchor): array
    {
        foreach ($nodes as $node) {
            if (($node['anchor'] ?? null) === $anchor) {
                return $node;
            }
        }
        self::fail('no section ' . $anchor);
    }

    /**
     * @param list<array<string,mixed>> $nodes Projection nodes.
     * @return array<string,mixed>
     */
    private static function first(array $nodes, string $kind): array
    {
        foreach ($nodes as $node) {
            if ($node['kind'] === $kind) {
                return $node;
            }
        }
        self::fail('no ' . $kind . ' node');
    }

    /**
     * @param list<array<string,mixed>> $ops    Operations.
     * @param mixed                     $design The design member; left out when null.
     */
    private static function input(array $ops, $design = null): string
    {
        $input = ['post_id' => 418, 'base_fingerprint' => self::FP, 'operations' => $ops];
        if ($design !== null) {
            $input['design'] = $design;
        }

        return json_encode($input, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    }
}
