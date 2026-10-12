<?php
/**
 * The outline version 3 grammar: what a section holds, what each pattern
 * holds, the caps, and the refusal each break answers with. The shared case
 * table is run in OutlineV3CasesTest; this class pins the edges the table
 * does not name.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\OutlineV3;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\OutlineV3
 * @covers \WPMgr\Agent\Abilities\PageCreateBuilder
 */
final class OutlineV3GrammarTest extends TestCase
{
    /** A smallest valid item of each item array. */
    private const ITEM = [
        'features'     => ['icon' => 'bolt', 'title' => 'Fast setup', 'text' => 'Go live in an afternoon.'],
        'testimonials' => ['quote' => 'It paid for itself.', 'name' => 'Dana Whitfield'],
        'plans'        => ['name' => 'Team', 'price' => '49', 'features' => ['5 sites'], 'cta' => ['text' => 'Choose Team', 'url' => '/signup']],
        'faq'          => ['question' => 'How long does it take?', 'answer' => 'About a week.'],
        'logos'        => ['attachment_id' => 11, 'alt' => 'Acme logo'],
        'stats'        => ['value' => 5, 'label' => 'sites launched'],
    ];

    /** The pattern and variant each item array belongs to. */
    private const PATTERN_OF = [
        'features'     => ['features', 'split'],
        'testimonials' => ['testimonials', 'featured'],
        'plans'        => ['pricing', 'three-plans'],
        'faq'          => ['faq', 'accordion'],
        'logos'        => ['logos', 'row'],
        'stats'        => ['stats', 'counters'],
    ];

    // -------------------------------------------------------------------------
    // The outline and its sections
    // -------------------------------------------------------------------------

    public function testAnAcceptedOutlineComesBackAsNormalisedArrays(): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Welcome.']]));

        $this->assertSame(
            ['outline' => [['type' => 'section', 'anchor' => 'intro', 'children' => [['type' => 'paragraph', 'text' => 'Welcome.']]]]],
            $r
        );
    }

    public function testASectionKeepsItsOptionsInTheSchemaOrder(): void
    {
        $r = self::check([[
            'children'   => [['type' => 'paragraph', 'text' => 'Hello.']],
            'min_height' => 'screen',
            'align'      => 'center',
            'spacing'    => 'xl',
            'background' => ['focal' => 'top', 'overlay' => 'strong', 'attachment_id' => 42],
            'tone'       => 'inverse',
            'width'      => 'full',
            'anchor'     => 'story',
            'type'       => 'section',
        ]]);

        $this->assertArrayHasKey('outline', $r);
        $section = $r['outline'][0];
        $this->assertSame(['type', 'anchor', 'width', 'tone', 'background', 'spacing', 'align', 'min_height', 'children'], array_keys($section));
        $this->assertSame(['attachment_id' => 42, 'overlay' => 'strong', 'focal' => 'top'], $section['background']);
    }

    public function testEveryItemPatternComesBackAsItWasGiven(): void
    {
        $doc = json_decode((string) file_get_contents(__DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json'), true);
        $in  = null;
        foreach ($doc['cases'] as $case) {
            if ($case['name'] === 'v3-full-landing-page') {
                $in = json_decode($case['input'], true);
            }
        }
        $this->assertIsArray($in);
        $r = self::check($in['outline']);
        $this->assertArrayHasKey('outline', $r);

        $seen = [];
        foreach ($in['outline'] as $i => $section) {
            foreach ($section['children'] as $j => $child) {
                if ($child['type'] === 'pattern' && isset(OutlineV3Schema::PATTERN_ARRAY[$child['pattern']])) {
                    $seen[] = $child['pattern'];
                    $this->assertSame($child, $r['outline'][$i]['children'][$j], $child['pattern']);
                }
            }
        }
        $this->assertSame(array_keys(OutlineV3Schema::PATTERN_ARRAY), $this->sorted($seen, array_keys(OutlineV3Schema::PATTERN_ARRAY)));
    }

    /**
     * @return array<string,array{0:mixed}>
     */
    public static function notAListOfSections(): array
    {
        return [
            'a string'           => ['outline'],
            'an object'          => [new \stdClass()],
            'an empty list'      => [[]],
            'a keyed array'      => [['a' => ['type' => 'section']]],
            'null'               => [null],
            'a section as a map' => [(object) ['type' => 'section', 'anchor' => 'intro', 'children' => []]],
        ];
    }

    /**
     * @dataProvider notAListOfSections
     * @param mixed $outline Outline as given.
     */
    public function testTheOutlineIsAListOfSections($outline): void
    {
        $r = OutlineV3::validate($outline);
        $this->assertSame('bad_input', $r['code'] ?? 'accepted');
    }

    public function testASectionIsAnObject(): void
    {
        $r = OutlineV3::validate([7]);
        $this->assertSame('bad_input', $r['code'] ?? 'accepted');
        $this->assertSame('outline[0]', $r['failures'][0]['path']);
    }

    public function testTwentySectionsAreTheMostAndTheCountIsALayoutRule(): void
    {
        $sections = [];
        for ($i = 1; $i <= 21; ++$i) {
            $sections[] = ['type' => 'section', 'anchor' => 's' . str_pad((string) $i, 2, '0', STR_PAD_LEFT), 'children' => [['type' => 'paragraph', 'text' => 'Section.']]];
        }
        $this->assertArrayHasKey('outline', self::check(array_slice($sections, 0, OutlineV3Schema::MAX_SECTIONS)));
        $r = self::check($sections);
        $this->assertSame('layout_invalid', $r['code']);
    }

    public function testASectionTakesItsFieldsOnlyAndNamesThem(): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Hi.']], ['padding' => 48]));

        $this->assertSame('bad_input', $r['code']);
        $this->assertSame('outline[0]', $r['failures'][0]['path']);
        $this->assertSame(['type', 'anchor', 'width', 'tone', 'background', 'spacing', 'align', 'min_height', 'children'], $r['failures'][0]['allowed'] ?? null);
        $this->assertStringNotContainsString('padding', (string) json_encode($r), 'the refusal does not echo the field it refused');
    }

    /**
     * @return array<string,array{0:string,1:mixed}>
     */
    public static function badSectionOptions(): array
    {
        return [
            'width'      => ['width', 'wide'],
            'tone'       => ['tone', 'dark'],
            'spacing'    => ['spacing', 'sm'],
            'align'      => ['align', 'left'],
            'min_height' => ['min_height', 'half'],
            'a number'   => ['width', 1],
            'null'       => ['tone', null],
        ];
    }

    /**
     * @dataProvider badSectionOptions
     * @param mixed $value Value as given.
     */
    public function testASectionOptionIsAClosedSet(string $key, $value): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Hi.']], [$key => $value]));

        $this->assertSame('bad_input', $r['code']);
        $this->assertSame($key, $r['failures'][0]['key']);
        $this->assertNotEmpty($r['failures'][0]['allowed'], 'the failure lists the accepted values');
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function badBackgrounds(): array
    {
        return [
            'not an object'     => ['photo'],
            'no attachment'     => [['overlay' => 'soft']],
            'attachment zero'   => [['attachment_id' => 0]],
            'attachment string' => [['attachment_id' => '42']],
            'attachment huge'   => [['attachment_id' => 2147483648]],
            'overlay unknown'   => [['attachment_id' => 42, 'overlay' => 'heavy']],
            'focal unknown'     => [['attachment_id' => 42, 'focal' => 'left']],
            'raw colour'        => [['attachment_id' => 42, 'color' => '#FF0000']],
        ];
    }

    /**
     * @dataProvider badBackgrounds
     * @param mixed $background Background as given.
     */
    public function testABackgroundIsAnImageAndTwoOptions($background): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Hi.']], ['background' => $background]));

        $this->assertSame('bad_input', $r['code']);
        $this->assertStringStartsWith('outline[0]', $r['failures'][0]['path']);
    }

    public function testASectionNeedsAnAnchorAndChildren(): void
    {
        $noAnchor = self::check([['type' => 'section', 'children' => [['type' => 'paragraph', 'text' => 'Hi.']]]]);
        $this->assertSame('bad_input', $noAnchor['code']);
        $this->assertSame('anchor', $noAnchor['failures'][0]['key']);

        $numberAnchor = self::check([['type' => 'section', 'anchor' => 7, 'children' => [['type' => 'paragraph', 'text' => 'Hi.']]]]);
        $this->assertSame('bad_input', $numberAnchor['code']);

        $noChildren = self::check([['type' => 'section', 'anchor' => 'intro']]);
        $this->assertSame('bad_input', $noChildren['code']);
        $this->assertSame('children', $noChildren['failures'][0]['key']);

        $stringChildren = self::check([['type' => 'section', 'anchor' => 'intro', 'children' => 'text']]);
        $this->assertSame('bad_input', $stringChildren['code']);
    }

    public function testFiftyChildrenAreTheMostAndTheCountIsALayoutRule(): void
    {
        $fifty = array_fill(0, 50, ['type' => 'separator']);
        $this->assertArrayHasKey('outline', self::check(self::page($fifty)));

        $r = self::check(self::page(array_merge($fifty, [['type' => 'separator']])));
        $this->assertSame('layout_invalid', $r['code']);
        $this->assertSame('children', $r['failures'][0]['key']);
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function nestedSections(): array
    {
        $inner = ['type' => 'section', 'anchor' => 'inner', 'children' => [['type' => 'paragraph', 'text' => 'Inner.']]];

        return [
            'in a section'  => [$inner],
            'in a group'    => [['type' => 'group', 'children' => [$inner]]],
            'in a column'   => [['type' => 'columns', 'columns' => [['children' => [['type' => 'paragraph', 'text' => 'A.']]], ['children' => [$inner]]]]],
            'in a hero'     => [['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => [$inner]]],
            'a pattern in a group'  => [['type' => 'group', 'children' => [['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [['type' => 'paragraph', 'text' => 'Go.']]]]]],
            'a pattern in a column' => [['type' => 'columns', 'columns' => [['children' => [['type' => 'paragraph', 'text' => 'A.']]], ['children' => [['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [['type' => 'paragraph', 'text' => 'Go.']]]]]]]],
            'a pattern in a hero'   => [['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => [['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [['type' => 'paragraph', 'text' => 'Go.']]]]]],
        ];
    }

    /**
     * @dataProvider nestedSections
     * @param array<string,mixed> $child The block under test.
     */
    public function testASectionAndAPatternStandOnlyWhereTheOutlinePutsThem(array $child): void
    {
        $r = self::check(self::page([$child]));

        $this->assertSame('layout_invalid', $r['code']);
    }

    public function testGroupsAndColumnsHoldVersionThreeLeaves(): void
    {
        $r = self::check(self::page([
            ['type' => 'group', 'children' => [['type' => 'heading', 'level' => 2, 'text' => 'Title'], ['type' => 'paragraph', 'text' => 'Body.']]],
            ['type' => 'columns', 'widths' => [50, 50], 'columns' => [
                ['children' => [['type' => 'paragraph', 'text' => 'Left.']]],
                ['children' => [['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go', 'style' => 'text']]]]],
            ]],
        ]));

        $this->assertArrayHasKey('outline', $r);
        $this->assertSame('text', $r['outline'][0]['children'][1]['columns'][1][0]['buttons'][0]['style']);
    }

    // -------------------------------------------------------------------------
    // Patterns
    // -------------------------------------------------------------------------

    public function testAPatternNameAndVariantAreClosedSets(): void
    {
        $name = self::check(self::page([['type' => 'pattern', 'pattern' => 'carousel', 'variant' => 'row']]));
        $this->assertSame('bad_input', $name['code']);
        $this->assertSame('pattern', $name['failures'][0]['key']);
        $this->assertContains('hero', $name['failures'][0]['allowed']);

        $variant = self::check(self::page([self::pattern('features', 'wall', ['features' => self::items('features', 3)])]));
        $this->assertSame('bad_input', $variant['code']);
        $this->assertSame('variant', $variant['failures'][0]['key']);
        $this->assertSame(OutlineV3Schema::VARIANTS['features'], $variant['failures'][0]['allowed']);

        $noVariant = self::check(self::page([['type' => 'pattern', 'pattern' => 'features', 'features' => self::items('features', 3)]]));
        $this->assertSame('bad_input', $noVariant['code']);

        $numberName = self::check(self::page([['type' => 'pattern', 'pattern' => 7, 'variant' => 'row']]));
        $this->assertSame('bad_input', $numberName['code']);
    }

    public function testEveryVariantOfEveryPatternIsAccepted(): void
    {
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                $array = OutlineV3Schema::PATTERN_ARRAY[$pattern] ?? null;
                $body  = $array === null
                    ? ['children' => [['type' => 'paragraph', 'text' => 'Words.']]]
                    : [$array => self::items($array, OutlineV3Schema::ITEM_CAPS[$array][0])];
                $r = self::check(self::page([self::pattern($pattern, $variant, $body)]));
                $this->assertArrayHasKey('outline', $r, $pattern . '/' . $variant . ': ' . (string) ($r['detail'] ?? ''));
            }
        }
    }

    public function testAPatternTakesItsOwnFieldOnly(): void
    {
        $itemsAndChildren = self::check(self::page([self::pattern('features', 'split', ['features' => self::items('features', 3), 'children' => [['type' => 'paragraph', 'text' => 'x']]])]));
        $this->assertSame('bad_input', $itemsAndChildren['code']);

        $heroWithItems = self::check(self::page([self::pattern('hero', 'centered', ['children' => [['type' => 'paragraph', 'text' => 'x']], 'features' => self::items('features', 3)])]));
        $this->assertSame('bad_input', $heroWithItems['code']);

        $wrongArray = self::check(self::page([self::pattern('pricing', 'three-plans', ['features' => self::items('features', 3)])]));
        $this->assertSame('bad_input', $wrongArray['code']);

        $notAList = self::check(self::page([self::pattern('faq', 'accordion', ['faq' => 'questions'])]));
        $this->assertSame('bad_input', $notAList['code']);

        $keyedList = self::check(self::page([self::pattern('faq', 'accordion', ['faq' => ['a' => self::ITEM['faq']]])]));
        $this->assertSame('bad_input', $keyedList['code']);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function itemArrays(): array
    {
        $out = [];
        foreach (OutlineV3Schema::ITEM_CAPS as $array => $caps) {
            $out[$array] = [$array];
        }

        return $out;
    }

    /**
     * @dataProvider itemArrays
     */
    public function testItemCountsAreInclusiveAtBothEnds(string $array): void
    {
        [$min, $max] = OutlineV3Schema::ITEM_CAPS[$array];
        [$pattern, $variant] = self::PATTERN_OF[$array];

        foreach ([$min, $max] as $count) {
            $r = self::check(self::page([self::pattern($pattern, $variant, [$array => self::items($array, $count)])]));
            $this->assertArrayHasKey('outline', $r, $array . ' x ' . $count . ': ' . (string) ($r['detail'] ?? ''));
        }
        foreach (array_filter([$min - 1, $max + 1], static fn (int $n): bool => $n >= 0) as $count) {
            $r = self::check(self::page([self::pattern($pattern, $variant, [$array => self::items($array, $count)])]));
            $this->assertSame('pattern_items_out_of_range', $r['code'] ?? 'accepted', $array . ' x ' . $count);
            $this->assertSame($array, $r['failures'][0]['key']);
            $this->assertSame('outline[0].children[0]', $r['failures'][0]['path']);
            $this->assertSame($pattern, $r['pattern'] ?? null, 'a refusal about a pattern names it');
            $this->assertIsArray($r['item_schema'] ?? null, 'and carries the schema of one item');
        }
    }

    public function testAPlanHoldsOneToEightFeatures(): void
    {
        foreach ([1, OutlineV3Schema::MAX_PLAN_FEATURES] as $count) {
            $plan = self::ITEM['plans'];
            $plan['features'] = array_fill(0, $count, 'A feature');
            $r = self::check(self::page([self::pattern('pricing', 'three-plans', ['plans' => [$plan]])]));
            $this->assertArrayHasKey('outline', $r, 'features x ' . $count);
        }
        foreach ([0, OutlineV3Schema::MAX_PLAN_FEATURES + 1] as $count) {
            $plan = self::ITEM['plans'];
            $plan['features'] = array_fill(0, $count, 'A feature');
            $r = self::check(self::page([self::pattern('pricing', 'three-plans', ['plans' => [$plan]])]));
            $this->assertSame('pattern_items_out_of_range', $r['code'] ?? 'accepted', 'features x ' . $count);
            $this->assertSame('outline[0].children[0].plans[0]', $r['failures'][0]['path']);
            $this->assertSame('features', $r['failures'][0]['key']);
        }
    }

    public function testEveryUnknownIconIsListedOnce(): void
    {
        $items         = self::items('features', 4);
        $items[1]['icon'] = 'unicorn';
        $items[3]['icon'] = 'Bolt';
        $r = self::check(self::page([self::pattern('features', 'grid', ['features' => $items])]));

        $this->assertSame('icon_unknown', $r['code']);
        $this->assertCount(2, $r['failures']);
        $this->assertSame('outline[0].children[0].features[1]', $r['failures'][0]['path']);
        $this->assertSame('outline[0].children[0].features[3]', $r['failures'][1]['path']);
        $this->assertSame('icon', $r['failures'][0]['key']);
        $this->assertSame(array_slice(OutlineV3Schema::ICONS, 0, 12), $r['failures'][0]['allowed']);
        $this->assertSame(OutlineV3Schema::ICONS, $r['item_schema']['properties']['icon']['enum'], 'the item schema lists every icon');
    }

    /**
     * Each row: label, item array, the item as given, the verdict.
     *
     * @return array<string,array{0:string,1:array<string,mixed>,2:string}>
     */
    public static function itemFields(): array
    {
        $long = str_repeat('a', 5001);

        return [
            'features: icon missing'          => ['features', ['title' => 'T', 'text' => 'B'], 'bad_input'],
            'features: icon a number'         => ['features', ['icon' => 4, 'title' => 'T', 'text' => 'B'], 'bad_input'],
            'features: title empty'           => ['features', ['icon' => 'bolt', 'title' => '', 'text' => 'B'], 'create_content_invalid'],
            'features: title with markup'     => ['features', ['icon' => 'bolt', 'title' => 'A <b>', 'text' => 'B'], 'create_content_invalid'],
            'features: text a number'         => ['features', ['icon' => 'bolt', 'title' => 'T', 'text' => 3], 'bad_input'],
            'features: text too long'         => ['features', ['icon' => 'bolt', 'title' => 'T', 'text' => $long], 'create_content_invalid'],
            'features: extra field'           => ['features', ['icon' => 'bolt', 'title' => 'T', 'text' => 'B', 'url' => '/x'], 'bad_input'],
            'testimonials: name missing'      => ['testimonials', ['quote' => 'Q'], 'bad_input'],
            'testimonials: role a number'     => ['testimonials', ['quote' => 'Q', 'name' => 'N', 'role' => 5], 'bad_input'],
            'testimonials: picture zero'      => ['testimonials', ['quote' => 'Q', 'name' => 'N', 'attachment_id' => 0], 'bad_input'],
            'testimonials: picture a string'  => ['testimonials', ['quote' => 'Q', 'name' => 'N', 'attachment_id' => '7'], 'bad_input'],
            'testimonials: quote in braces'   => ['testimonials', ['quote' => 'Q {{ x }}', 'name' => 'N'], 'create_content_invalid'],
            'plans: price missing'            => ['plans', ['name' => 'N', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => '/x']], 'bad_input'],
            'plans: price empty'              => ['plans', ['name' => 'N', 'price' => '', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => '/x']], 'create_content_invalid'],
            'plans: features a string'        => ['plans', ['name' => 'N', 'price' => '1', 'features' => 'a', 'cta' => ['text' => 'T', 'url' => '/x']], 'bad_input'],
            'plans: a feature a number'       => ['plans', ['name' => 'N', 'price' => '1', 'features' => [3], 'cta' => ['text' => 'T', 'url' => '/x']], 'bad_input'],
            'plans: a feature with markup'    => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['<i>a</i>'], 'cta' => ['text' => 'T', 'url' => '/x']], 'create_content_invalid'],
            'plans: cta missing'              => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a']], 'bad_input'],
            'plans: cta url missing'          => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T']], 'bad_input'],
            'plans: cta extra field'          => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => '/x', 'style' => 'fill']], 'bad_input'],
            'plans: cta text 81 characters'   => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => str_repeat('b', 81), 'url' => '/x']], 'create_content_invalid'],
            'plans: cta url not https'        => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => 'ftp://example.com/x']], 'link_invalid'],
            'plans: cta url a script'         => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => 'javascript:alert(1)']], 'link_invalid'],
            'plans: cta url a bad anchor'     => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => '#Bad']], 'link_invalid'],
            'plans: cta url a number'         => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => 5]], 'bad_input'],
            'plans: highlighted a string'     => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => '/x'], 'highlighted' => 'yes'], 'bad_input'],
            'faq: question a number'          => ['faq', ['question' => 1, 'answer' => 'A'], 'bad_input'],
            'faq: answer in braces'           => ['faq', ['question' => 'Q', 'answer' => 'A {{ b }}'], 'create_content_invalid'],
            'faq: answer missing'             => ['faq', ['question' => 'Q'], 'bad_input'],
            'logos: alt missing'              => ['logos', ['attachment_id' => 3], 'bad_input'],
            'logos: alt with a bracket'       => ['logos', ['attachment_id' => 3, 'alt' => 'Acme [logo]'], 'create_content_invalid'],
            'logos: picture missing'          => ['logos', ['alt' => 'Acme'], 'bad_input'],
            'logos: picture too large'        => ['logos', ['attachment_id' => 2147483648, 'alt' => 'Acme'], 'bad_input'],
            'stats: value negative'           => ['stats', ['value' => -1, 'label' => 'L'], 'bad_input'],
            'stats: value over the largest'   => ['stats', ['value' => 1000000000, 'label' => 'L'], 'bad_input'],
            'stats: value a fraction'         => ['stats', ['value' => 1.5, 'label' => 'L'], 'bad_input'],
            'stats: value a string'           => ['stats', ['value' => '12', 'label' => 'L'], 'bad_input'],
            'stats: label missing'            => ['stats', ['value' => 12], 'bad_input'],
            'stats: suffix 7 characters'      => ['stats', ['value' => 12, 'suffix' => 'abcdefg', 'label' => 'L'], 'create_content_invalid'],
            'stats: suffix empty'             => ['stats', ['value' => 12, 'suffix' => '', 'label' => 'L'], 'create_content_invalid'],
            'features: all fields fine'       => ['features', ['icon' => 'bolt', 'title' => 'T', 'text' => 'B'], 'ok'],
            'testimonials: with picture, role' => ['testimonials', ['quote' => 'Q', 'name' => 'N', 'role' => 'Owner', 'attachment_id' => 7], 'ok'],
            'plans: every field'              => ['plans', ['name' => 'N', 'price' => '1', 'period' => 'month', 'features' => ['a', 'b'], 'cta' => ['text' => 'T', 'url' => '#signup'], 'highlighted' => true], 'ok'],
            'plans: an https link'            => ['plans', ['name' => 'N', 'price' => '1', 'features' => ['a'], 'cta' => ['text' => 'T', 'url' => 'https://example.com/buy']], 'ok'],
            'logos: alt empty'                => ['logos', ['attachment_id' => 3, 'alt' => ''], 'ok'],
            'stats: value zero and a suffix'  => ['stats', ['value' => 0, 'suffix' => '%', 'label' => 'L'], 'ok'],
            'stats: value the largest'        => ['stats', ['value' => 999999999, 'suffix' => '+', 'label' => 'L'], 'ok'],
        ];
    }

    /**
     * @dataProvider itemFields
     * @param array<string,mixed> $item Item as given.
     */
    public function testAnItemIsJudgedFieldByField(string $array, array $item, string $verdict): void
    {
        [$pattern, $variant] = self::PATTERN_OF[$array];
        $items = self::items($array, OutlineV3Schema::ITEM_CAPS[$array][0]);
        $items[0] = $item;
        // The second section carries the anchor a '#signup' link points at.
        $outline = self::page([self::pattern($pattern, $variant, [$array => $items])]);
        $outline[] = ['type' => 'section', 'anchor' => 'signup', 'children' => [['type' => 'paragraph', 'text' => 'Sign up.']]];
        $r = self::check($outline);

        $this->assertSame($verdict, isset($r['outline']) ? 'ok' : $r['code'], (string) ($r['detail'] ?? ''));
        if ($verdict !== 'ok') {
            $this->assertStringStartsWith('outline[0].children[0]', $r['failures'][0]['path']);
        }
    }

    // -------------------------------------------------------------------------
    // The hero and the call to action
    // -------------------------------------------------------------------------

    public function testAHeroHoldsOneToSixBlocksAndACtaOneToFour(): void
    {
        foreach ([['hero', 'centered', OutlineV3Schema::MAX_HERO_CHILDREN], ['cta', 'band', OutlineV3Schema::MAX_CTA_CHILDREN]] as [$name, $variant, $max]) {
            $ok = self::check(self::page([self::pattern($name, $variant, ['children' => self::paragraphs($max)])]));
            $this->assertArrayHasKey('outline', $ok, $name . ' x ' . $max);

            foreach ([0, $max + 1] as $count) {
                $r = self::check(self::page([self::pattern($name, $variant, ['children' => self::paragraphs($count)])]));
                $this->assertSame('layout_invalid', $r['code'] ?? 'accepted', $name . ' x ' . $count);
                $this->assertSame('children', $r['failures'][0]['key']);
                $this->assertArrayNotHasKey('item_schema', $r, 'a pattern of blocks has no item schema');
            }
        }
        $notAList = self::check(self::page([self::pattern('hero', 'split', ['children' => 'text'])]));
        $this->assertSame('bad_input', $notAList['code']);
    }

    /**
     * @return array<string,array{0:string,1:array<string,mixed>}>
     */
    public static function foreignBlocks(): array
    {
        return [
            'hero: a list'      => ['hero', ['type' => 'list', 'ordered' => false, 'items' => ['a']]],
            'hero: a quote'     => ['hero', ['type' => 'quote', 'paragraphs' => ['a']]],
            'hero: a spacer'    => ['hero', ['type' => 'spacer', 'size' => 'small']],
            'hero: a group'     => ['hero', ['type' => 'group', 'children' => [['type' => 'paragraph', 'text' => 'a']]]],
            'cta: an image'     => ['cta', ['type' => 'image', 'attachment_id' => 5, 'alt' => 'a']],
            'cta: a table'      => ['cta', ['type' => 'table', 'rows' => [['a']]]],
        ];
    }

    /**
     * @dataProvider foreignBlocks
     * @param array<string,mixed> $block Block as given.
     */
    public function testAHeroAndACtaHoldTheirOwnBlocksOnly(string $name, array $block): void
    {
        $variant = $name === 'hero' ? 'centered' : 'band';
        $r       = self::check(self::page([self::pattern($name, $variant, ['children' => [['type' => 'paragraph', 'text' => 'Lead.'], $block]])]));

        $this->assertSame('layout_invalid', $r['code']);
        $this->assertSame('outline[0].children[0].children[1]', $r['failures'][0]['path']);
        $this->assertSame('type', $r['failures'][0]['key']);
    }

    public function testAHeroHoldsAHeadingAnImageAndButtons(): void
    {
        $r = self::check(self::page([self::pattern('hero', 'background', ['children' => [
            ['type' => 'heading', 'level' => 1, 'text' => 'Websites that earn their keep'],
            ['type' => 'paragraph', 'text' => 'We build sites.'],
            ['type' => 'image', 'attachment_id' => 9, 'alt' => 'The team'],
            ['type' => 'buttons', 'buttons' => [['text' => 'See pricing', 'url' => '#intro', 'style' => 'fill'], ['text' => 'Talk', 'url' => '/contact', 'style' => 'text']]],
        ]])]));

        $this->assertArrayHasKey('outline', $r, (string) ($r['detail'] ?? ''));
        $this->assertSame(1, $r['outline'][0]['children'][0]['children'][0]['level']);
    }

    public function testUnknownAndMalformedBlocksInAHeroAreTheLeafGrammarsToRefuse(): void
    {
        $unknown = self::check(self::page([self::pattern('hero', 'centered', ['children' => [['type' => 'banner']]])]));
        $this->assertSame('create_content_invalid', $unknown['code']);

        $string = self::check(self::page([self::pattern('hero', 'centered', ['children' => ['Words']])]));
        $this->assertSame('bad_input', $string['code']);
    }

    /**
     * Each row: the outline, and the path of the hero that is out of place.
     *
     * @return array<string,array{0:list<array<string,mixed>>,1:string}>
     */
    public static function misplacedHeroes(): array
    {
        $hero = self::pattern('hero', 'centered', ['children' => [['type' => 'paragraph', 'text' => 'Lead.']]]);
        $cta  = self::pattern('cta', 'band', ['children' => [['type' => 'paragraph', 'text' => 'Join.']]]);

        return [
            'second block of the first section' => [[self::section('top', [['type' => 'paragraph', 'text' => 'Before.'], $hero])], 'outline[0].children[1]'],
            'first block of the second section' => [[self::section('top'), self::section('more', [$hero])], 'outline[1].children[0]'],
            'after a call to action'            => [[self::section('top', [$cta, $hero])], 'outline[0].children[1]'],
            'a call to action is not a hero'    => [[self::section('top', [$cta, $cta, $hero])], 'outline[0].children[2]'],
            'the last of fifty blocks'          => [[self::section('top', array_merge(array_fill(0, 49, ['type' => 'separator']), [$hero]))], 'outline[0].children[49]'],
        ];
    }

    /**
     * @dataProvider misplacedHeroes
     * @param list<array<string,mixed>> $outline Outline.
     * @param string                    $path    Path of the hero that is out of place.
     */
    public function testAHeroIsTheFirstBlockOfTheFirstSection(array $outline, string $path): void
    {
        $r = self::check($outline);

        $this->assertSame('hero_misplaced', $r['code'] ?? 'accepted');
        $this->assertSame($path, $r['failures'][0]['path']);
        $this->assertSame('pattern', $r['failures'][0]['key']);
        $this->assertStringContainsString('first block of the first section', $r['failures'][0]['reason'], 'the refusal says where the hero goes');
    }

    /**
     * Each row: the outline, and the path of the second hero, the first one
     * over the cap. The count comes before any hero's place is judged, so the
     * answer is the same whether or not the first hero stands in place.
     *
     * @return array<string,array{0:list<array<string,mixed>>,1:string}>
     */
    public static function secondHeroes(): array
    {
        $hero  = self::pattern('hero', 'centered', ['children' => [['type' => 'paragraph', 'text' => 'Lead.']]]);
        $lead  = ['type' => 'paragraph', 'text' => 'Before.'];
        $cta   = self::pattern('cta', 'band', ['children' => [['type' => 'paragraph', 'text' => 'Join.']]]);
        $group = ['type' => 'group', 'children' => [['type' => 'paragraph', 'text' => 'Inside.']]];

        return [
            'beside the first'                         => [[self::section('top', [$hero, $hero])], 'outline[0].children[1]'],
            'in a later section'                       => [[self::section('top', [$hero]), self::section('more', [$hero])], 'outline[1].children[0]'],
            'the first out of place'                   => [[self::section('top', [$lead, $hero]), self::section('more', [$hero])], 'outline[1].children[0]'],
            'both out of place, in one section'        => [[self::section('top', [$lead, $hero, $hero])], 'outline[0].children[2]'],
            'both in later sections'                   => [[self::section('top'), self::section('more', [$hero]), self::section('last', [$hero])], 'outline[2].children[0]'],
            'three heroes name the second'             => [[self::section('top', [$hero, $hero, $hero])], 'outline[0].children[1]'],
            'a call to action between the two'         => [[self::section('top', [$hero, $cta, $hero])], 'outline[0].children[2]'],
            'a group beside two heroes'                => [[self::section('top', [$group, $hero, $hero])], 'outline[0].children[2]'],
        ];
    }

    /**
     * @dataProvider secondHeroes
     * @param list<array<string,mixed>> $outline Outline.
     * @param string                    $path    Path of the second hero.
     */
    public function testASecondHeroIsALayoutRuleCountedBeforeThePlace(array $outline, string $path): void
    {
        $r = self::check($outline);

        $this->assertSame('layout_invalid', $r['code'] ?? 'accepted');
        $this->assertCount(1, $r['failures']);
        $this->assertSame($path, $r['failures'][0]['path']);
        $this->assertSame('pattern', $r['failures'][0]['key']);
        $this->assertStringContainsString('one hero', $r['failures'][0]['reason'], 'the refusal says a page has one hero');
        $this->assertStringContainsString($path . '.pattern', $r['detail'], 'the message says where');
    }

    public function testTheHeroCountIsReadByShapeAndComesBeforeEveryOtherJudgement(): void
    {
        $hero = self::pattern('hero', 'centered', ['children' => [['type' => 'paragraph', 'text' => 'Lead.']]]);

        // A second hero wins over a fault the walk would have found first.
        $r = self::check([array_merge(self::section('top', [$hero, $hero]), ['colour' => 'red'])]);
        $this->assertSame('layout_invalid', $r['code'] ?? 'accepted', 'the count is taken before the sections are read');

        // Only a hero pattern that is a direct child of a section counts: other patterns, other nodes and
        // junk do not, and a lone hero out of place is still the place rule's.
        $odd = self::check([
            self::section('top', [$hero]),
            self::section('more', [self::pattern('cta', 'band', ['children' => [['type' => 'paragraph', 'text' => 'Join.']]]), ['type' => 'paragraph', 'text' => 'x']]),
        ]);
        $this->assertArrayHasKey('outline', $odd, (string) ($odd['detail'] ?? ''));

        $lone = self::check([self::section('top', [['type' => 'paragraph', 'text' => 'x'], $hero])]);
        $this->assertSame('hero_misplaced', $lone['code'] ?? 'accepted');

        // Nodes that are not objects are the grammar's to refuse, not the count's.
        $junk = OutlineV3::validate(json_decode((string) json_encode([self::section('top', [$hero]), 'text', null, 7, [self::section('more')]]), false, 64));
        $this->assertNotSame('layout_invalid', $junk['code'] ?? '', 'junk beside one hero is not a second hero');
        $this->assertSame('bad_input', $junk['code'] ?? 'accepted');
    }

    public function testTheHeroMayHaveBlocksAfterItAndSectionsAfterItsSection(): void
    {
        $hero = self::pattern('hero', 'centered', ['children' => [['type' => 'paragraph', 'text' => 'Lead.']]]);
        $r    = self::check([
            self::section('top', [$hero, ['type' => 'paragraph', 'text' => 'After.'], self::pattern('cta', 'band', ['children' => [['type' => 'paragraph', 'text' => 'Join.']]])]),
            self::section('more'),
        ]);

        $this->assertArrayHasKey('outline', $r, (string) ($r['detail'] ?? ''));
    }

    public function testAPageWithoutAHeroIsAccepted(): void
    {
        $this->assertArrayHasKey('outline', self::check([self::section('top'), self::section('more')]));
    }

    // -------------------------------------------------------------------------
    // The forms of a block inside a section
    // -------------------------------------------------------------------------

    public function testHeadingLevelsAndButtonStylesAreTheVersionThreeSets(): void
    {
        $level5 = self::check(self::page([['type' => 'heading', 'level' => 5, 'text' => 'Title']]));
        $this->assertSame('bad_input', $level5['code']);
        $this->assertSame('level', $level5['failures'][0]['key']);
        $this->assertSame('outline[0].children[0]', $level5['failures'][0]['path']);

        $level0 = self::check(self::page([['type' => 'heading', 'level' => 0, 'text' => 'Title']]));
        $this->assertSame('bad_input', $level0['code']);

        $ghost = self::check(self::page([['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go', 'style' => 'ghost']]]]));
        $this->assertSame('bad_input', $ghost['code']);

        foreach (['fill', 'outline', 'text'] as $style) {
            $ok = self::check(self::page([['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go', 'style' => $style]]]]));
            $this->assertArrayHasKey('outline', $ok, $style);
        }
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function anchorLinks(): array
    {
        return [
            'a section anchor'      => ['#intro', 'ok'],
            'two characters'        => ['#ab', 'ok'],
            'forty characters'      => ['#' . str_repeat('a', 40), 'ok'],
            'capitals'              => ['#Intro', 'link_invalid'],
            'a bare hash'           => ['#', 'link_invalid'],
            'one character'         => ['#a', 'link_invalid'],
            'forty-one characters'  => ['#' . str_repeat('a', 41), 'link_invalid'],
            'a leading digit'       => ['#1abc', 'link_invalid'],
            'an underscore'         => ['#in_tro', 'link_invalid'],
            'a space'               => ['#in tro', 'link_invalid'],
            'a trailing slash'      => ['#intro/', 'link_invalid'],
            'a second hash'         => ['#intro#two', 'link_invalid'],
            'a query'               => ['#intro?x=1', 'link_invalid'],
        ];
    }

    /**
     * @dataProvider anchorLinks
     */
    public function testAnInPageLinkIsAHashAndAnAnchor(string $url, string $verdict): void
    {
        $outline = [self::section('start', [['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => $url]]]])];
        if ($verdict === 'ok') {
            // The link resolves: a section further down holds the anchor it names.
            $outline[] = self::section(substr($url, 1));
        }
        $r = self::check($outline);

        $this->assertSame($verdict, isset($r['outline']) ? 'ok' : $r['code'], $url);
    }

    public function testAnInPageLinkEndingInANewlineIsRefused(): void
    {
        $r = OutlineV3::validate(json_decode((string) json_encode(self::page([['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => "#intro\n"]]]])), false));

        $this->assertSame('link_invalid', $r['code'] ?? 'accepted');
    }

    /**
     * @return array<string,array{0:mixed}>
     */
    public static function badButtonStyles(): array
    {
        return [
            'ghost'      => ['ghost'],
            'upper case' => ['Fill'],
            'empty'      => [''],
            'a number'   => [1],
            'null'       => [null],
            'a list'     => [['fill']],
        ];
    }

    /**
     * @dataProvider badButtonStyles
     * @param mixed $style Style as given.
     */
    public function testAButtonStyleIsFillOutlineOrText($style): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Hi.'], ['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go', 'style' => $style]]]]));

        $this->assertSame('bad_input', $r['code'] ?? 'accepted');
        $this->assertSame('outline[0].children[1]', $r['failures'][0]['path']);
        $this->assertSame('buttons[0].style', $r['failures'][0]['key']);
        $this->assertStringContainsString('fill, outline or text', $r['failures'][0]['reason']);
    }

    public function testAButtonWithoutAStyleIsAFillButton(): void
    {
        $r = self::check(self::page([['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go']]]]));

        $this->assertSame('fill', $r['outline'][0]['children'][0]['buttons'][0]['style']);
    }

    // -------------------------------------------------------------------------
    // Anchors
    // -------------------------------------------------------------------------

    /**
     * Each row: the anchor as given, and the verdict.
     *
     * @return array<string,array{0:string,1:string}>
     */
    public static function anchors(): array
    {
        return [
            'two characters'          => ['ab', 'ok'],
            'forty characters'        => ['a' . str_repeat('b', 39), 'ok'],
            'digits and hyphens'      => ['plan-2-b', 'ok'],
            'a hyphen at the end'     => ['plans-', 'ok'],
            'a near miss of a reserved id' => ['main-menu', 'ok'],
            'one character'           => ['a', 'anchor_invalid'],
            'forty-one characters'    => ['a' . str_repeat('b', 40), 'anchor_invalid'],
            'capitals'                => ['Pricing', 'anchor_invalid'],
            'a leading digit'         => ['1abc', 'anchor_invalid'],
            'a leading hyphen'        => ['-abc', 'anchor_invalid'],
            'an underscore'           => ['my_plans', 'anchor_invalid'],
            'a space'                 => ['my plans', 'anchor_invalid'],
            'a dot'                   => ['my.plans', 'anchor_invalid'],
            'a hash'                  => ['#plans', 'anchor_invalid'],
            'empty'                   => ['', 'anchor_invalid'],
            'a trailing newline'      => ["plans\n", 'anchor_invalid'],
            'a letter with an accent' => ["pl\u{00E4}n", 'anchor_invalid'],
        ];
    }

    /**
     * @dataProvider anchors
     */
    public function testAnAnchorIsTwoToFortyOfLowerCaseLettersDigitsAndHyphens(string $anchor, string $verdict): void
    {
        $r = self::check([self::section($anchor)]);

        $this->assertSame($verdict, isset($r['outline']) ? 'ok' : $r['code'], (string) json_encode($anchor));
        if ($verdict !== 'ok') {
            $this->assertSame('outline[0]', $r['failures'][0]['path']);
            $this->assertSame('anchor', $r['failures'][0]['key']);
        }
    }

    public function testAnAnchorTheThemeOwnsIsRefusedAndTheRefusalListsThem(): void
    {
        $this->assertSame(['content', 'main', 'primary', 'page', 'masthead', 'colophon', 'site-header', 'site-footer', 'wpadminbar'], OutlineV3Schema::RESERVED_ANCHORS);

        foreach (OutlineV3Schema::RESERVED_ANCHORS as $id) {
            $r = self::check([self::section('top'), self::section($id)]);

            $this->assertSame('anchor_reserved', $r['code'] ?? 'accepted', $id);
            $this->assertSame('outline[1]', $r['failures'][0]['path'], $id);
            $this->assertSame('anchor', $r['failures'][0]['key'], $id);
            $this->assertStringContainsString('wpadminbar', $r['failures'][0]['reason'], 'the reason names the ids the theme owns');
        }
    }

    public function testAnAnchorBelongsToOneSection(): void
    {
        $r = self::check([self::section('intro'), self::section('pricing'), self::section('faq'), self::section('pricing')]);

        $this->assertSame('anchor_duplicate', $r['code'] ?? 'accepted');
        $this->assertSame('outline[3]', $r['failures'][0]['path']);
        $this->assertSame('anchor', $r['failures'][0]['key']);
        $this->assertStringContainsString('outline[1]', $r['failures'][0]['reason'], 'the reason names the section that has the anchor');
    }

    public function testTwentySectionsWithTwentyAnchorsAreAccepted(): void
    {
        $sections = [];
        for ($i = 0; $i < OutlineV3Schema::MAX_SECTIONS; ++$i) {
            $sections[] = self::section('part-' . $i);
        }

        $this->assertArrayHasKey('outline', self::check($sections));
    }

    public function testARefusalAboutAnAnchorDoesNotEchoTheAnchor(): void
    {
        $invalid = self::check([self::section('ZQXANCHOR')]);
        $this->assertSame('anchor_invalid', $invalid['code'] ?? 'accepted');
        $this->assertStringNotContainsString('ZQXANCHOR', (string) json_encode($invalid));

        $duplicate = self::check([self::section('zqxdup'), self::section('zqxdup')]);
        $this->assertSame('anchor_duplicate', $duplicate['code'] ?? 'accepted');
        $this->assertStringNotContainsString('zqxdup', (string) json_encode($duplicate));
    }

    // -------------------------------------------------------------------------
    // Links to a section of the page
    // -------------------------------------------------------------------------

    /**
     * Each row: the blocks of the first section that make a link to the url they
     * are given, and the path of the object that holds the url.
     *
     * @return array<string,array{0:callable,1:string}>
     */
    public static function linkPlaces(): array
    {
        $text   = ['type' => 'paragraph', 'text' => 'Words.'];
        $button = static fn (string $url): array => ['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => $url]]];

        return [
            'a button of a section' => [
                static fn (string $url): array => [$text, $button($url)],
                'outline[0].children[1].buttons[0]',
            ],
            'a button in a group' => [
                static fn (string $url): array => [['type' => 'group', 'children' => [$text, $button($url)]]],
                'outline[0].children[0].children[1].buttons[0]',
            ],
            'a button in a column' => [
                static fn (string $url): array => [['type' => 'columns', 'columns' => [['children' => [$text]], ['children' => [$button($url)]]]]],
                'outline[0].children[0].columns[1].children[0].buttons[0]',
            ],
            'a button in the hero' => [
                static fn (string $url): array => [self::pattern('hero', 'centered', ['children' => [$text, $button($url)]])],
                'outline[0].children[0].children[1].buttons[0]',
            ],
            'a button in a call to action' => [
                static fn (string $url): array => [self::pattern('cta', 'band', ['children' => [$button($url)]])],
                'outline[0].children[0].children[0].buttons[0]',
            ],
            'the button of a plan' => [
                static fn (string $url): array => [self::pattern('pricing', 'three-plans', ['plans' => [array_merge(self::ITEM['plans'], ['cta' => ['text' => 'Choose', 'url' => $url]])]])],
                'outline[0].children[0].plans[0].cta',
            ],
        ];
    }

    /**
     * @dataProvider linkPlaces
     * @param callable $make Makes the blocks of the first section from a url.
     * @param string   $path Path of the object that holds the url.
     */
    public function testALinkToASectionFurtherDownOrToItsOwnSectionResolves(callable $make, string $path): void
    {
        $forward = self::check([self::section('top', $make('#plans')), self::section('plans')]);
        $this->assertArrayHasKey('outline', $forward, $path . ': ' . (string) ($forward['detail'] ?? ''));

        $own = self::check([self::section('top', $make('#top'))]);
        $this->assertArrayHasKey('outline', $own, $path . ': ' . (string) ($own['detail'] ?? ''));
    }

    /**
     * @dataProvider linkPlaces
     * @param callable $make Makes the blocks of the first section from a url.
     * @param string   $path Path of the object that holds the url.
     */
    public function testALinkToAnAnchorNoSectionHasIsRefusedWithThePageAnchors(callable $make, string $path): void
    {
        $r = self::check([self::section('top', $make('#zqxsection')), self::section('faq')]);

        $this->assertSame('link_anchor_missing', $r['code'] ?? 'accepted', $path);
        $this->assertCount(1, $r['failures']);
        $this->assertSame($path, $r['failures'][0]['path']);
        $this->assertSame('url', $r['failures'][0]['key']);
        $this->assertSame(['top', 'faq'], $r['failures'][0]['allowed'], 'the refusal lists the anchors the page has');
        $this->assertStringNotContainsString('zqxsection', (string) json_encode($r), 'and does not repeat the one it was given');
    }

    public function testEveryLinkToAMissingAnchorIsListedInTheOrderOfThePage(): void
    {
        $r = self::check([
            self::section('top', [['type' => 'buttons', 'buttons' => [
                ['text' => 'A', 'url' => '#nowhere'],
                ['text' => 'B', 'url' => '#top'],
                ['text' => 'C', 'url' => '#elsewhere'],
            ]]]),
            self::section('more', [['type' => 'buttons', 'buttons' => [['text' => 'D', 'url' => '#nowhere']]]]),
        ]);

        $this->assertSame('link_anchor_missing', $r['code'] ?? 'accepted');
        $this->assertSame(
            ['outline[0].children[0].buttons[0]', 'outline[0].children[0].buttons[2]', 'outline[1].children[0].buttons[0]'],
            array_column($r['failures'], 'path')
        );
    }

    public function testLinksAreJudgedAfterTheWholeOutline(): void
    {
        $r = self::check([
            self::section('top', [['type' => 'buttons', 'buttons' => [['text' => 'A', 'url' => '#nowhere']]]]),
            self::section('more', [['type' => 'paragraph', 'text' => 'Has <b>markup</b>.']]),
        ]);

        $this->assertSame('create_content_invalid', $r['code'] ?? 'accepted', 'a fault that is read on the way is told before a link that could not resolve');
    }

    public function testAnExternalLinkAndASitePathNeedNoSection(): void
    {
        $r = self::check([self::section('top', [['type' => 'buttons', 'buttons' => [
            ['text' => 'A', 'url' => 'https://example.com/#nowhere'],
            ['text' => 'B', 'url' => '/pricing/#nowhere'],
        ]]])]);

        $this->assertArrayHasKey('outline', $r, (string) ($r['detail'] ?? ''));
    }

    // -------------------------------------------------------------------------
    // The level 1 heading
    // -------------------------------------------------------------------------

    /**
     * Each row: the outline, and the path of the level 1 heading outside the hero.
     *
     * @return array<string,array{0:list<array<string,mixed>>,1:string}>
     */
    public static function titlesOutsideTheHero(): array
    {
        $h1   = ['type' => 'heading', 'level' => 1, 'text' => 'Title'];
        $text = ['type' => 'paragraph', 'text' => 'Words.'];
        $hero = self::pattern('hero', 'centered', ['children' => [['type' => 'heading', 'level' => 1, 'text' => 'Websites that earn their keep']]]);

        return [
            'a section'                => [[self::section('top', [$h1])], 'outline[0].children[0]'],
            'a group'                  => [[self::section('top', [['type' => 'group', 'children' => [$text, $h1]]])], 'outline[0].children[0].children[1]'],
            'a column'                 => [[self::section('top', [['type' => 'columns', 'columns' => [['children' => [$text]], ['children' => [$h1]]]]])], 'outline[0].children[0].columns[1].children[0]'],
            'a call to action'         => [[self::section('top', [self::pattern('cta', 'band', ['children' => [$h1]])])], 'outline[0].children[0].children[0]'],
            'a section after the hero' => [[self::section('top', [$hero]), self::section('more', [$h1])], 'outline[1].children[0]'],
            'a group beside the hero'  => [[self::section('top', [$hero, ['type' => 'group', 'children' => [$h1]]])], 'outline[0].children[1].children[0]'],
        ];
    }

    /**
     * @dataProvider titlesOutsideTheHero
     * @param list<array<string,mixed>> $outline Outline.
     * @param string                    $path    Path of the heading outside the hero.
     */
    public function testALevelOneHeadingLivesInTheHero(array $outline, string $path): void
    {
        $r = self::check($outline);

        $this->assertSame('h1_outside_hero', $r['code'] ?? 'accepted');
        $this->assertSame($path, $r['failures'][0]['path']);
        $this->assertSame('level', $r['failures'][0]['key']);
    }

    public function testTheHeroMayHoldTheOneLevelOneHeadingOfThePage(): void
    {
        $r = self::check([
            self::section('top', [self::pattern('hero', 'centered', ['children' => [
                ['type' => 'heading', 'level' => 1, 'text' => 'Websites that earn their keep'],
                ['type' => 'paragraph', 'text' => 'Lead.'],
            ]])]),
            self::section('more', [['type' => 'heading', 'level' => 2, 'text' => 'More'], ['type' => 'heading', 'level' => 3, 'text' => 'And more']]),
        ]);

        $this->assertArrayHasKey('outline', $r, (string) ($r['detail'] ?? ''));
    }

    public function testASecondLevelOneHeadingIsALayoutFault(): void
    {
        $h1 = ['type' => 'heading', 'level' => 1, 'text' => 'Title'];
        $r  = self::check([self::section('top', [self::pattern('hero', 'centered', ['children' => [$h1, ['type' => 'paragraph', 'text' => 'Lead.'], $h1]])])]);

        $this->assertSame('layout_invalid', $r['code'] ?? 'accepted');
        $this->assertSame('outline[0].children[0].children[2]', $r['failures'][0]['path']);
        $this->assertSame('level', $r['failures'][0]['key']);

        $elsewhere = self::check([
            self::section('top', [self::pattern('hero', 'centered', ['children' => [$h1]])]),
            self::section('more', [$h1]),
        ]);
        $this->assertSame('h1_outside_hero', $elsewhere['code'] ?? 'accepted', 'where the heading stands is told before how many there are');
    }

    public function testVersionTwoIsUnchanged(): void
    {
        $v2 = static function (array $node): array {
            return PageCreateBuilder::validate(json_decode((string) json_encode([
                'post_type' => 'page',
                'editor'    => 'wordpress_blocks',
                'title'     => 'Page',
                'outline'   => [$node],
            ]), false));
        };

        $this->assertSame('bad_input', $v2(['type' => 'heading', 'level' => 1, 'text' => 'Title'])['code']);
        $this->assertSame('bad_input', $v2(['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '/go', 'style' => 'text']]])['code']);
        $this->assertSame('link_invalid', $v2(['type' => 'buttons', 'buttons' => [['text' => 'Go', 'url' => '#intro']]])['code']);
        $this->assertSame('create_content_invalid', $v2(['type' => 'section', 'anchor' => 'intro', 'children' => [['type' => 'paragraph', 'text' => 'Hi.']]])['code']);
        $this->assertSame('create_content_invalid', $v2(['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => []])['code']);
        $this->assertNotNull(PageCreateBuilder::linkProblem('#intro'), 'a version 2 link is never an in-page link');
    }

    // -------------------------------------------------------------------------
    // Page-wide caps
    // -------------------------------------------------------------------------

    public function testBackgroundsAndPatternPicturesCountTowardTheImageCap(): void
    {
        $sections = [];
        $logos    = self::pattern('logos', 'row', ['logos' => self::items('logos', 10)]);
        $stories  = self::pattern('testimonials', 'featured', ['testimonials' => array_map(
            static fn (int $i): array => self::ITEM['testimonials'] + ['attachment_id' => 100 + $i],
            range(0, 5)
        )]);
        $sections[] = ['type' => 'section', 'anchor' => 'proof', 'children' => [$logos, $stories]];
        for ($i = 1; $i <= 4; ++$i) {
            $sections[] = ['type' => 'section', 'anchor' => 'bg' . $i, 'background' => ['attachment_id' => 500 + $i], 'children' => [['type' => 'paragraph', 'text' => 'Words.']]];
        }
        $this->assertArrayHasKey('outline', self::check($sections), 'sixteen pictures in patterns and four backgrounds make twenty');

        $sections[] = ['type' => 'section', 'anchor' => 'bg5', 'background' => ['attachment_id' => 505], 'children' => [['type' => 'paragraph', 'text' => 'Words.']]];
        $r = self::check($sections);
        $this->assertSame('layout_invalid', $r['code']);
        $this->assertStringContainsString('20', $r['failures'][0]['reason']);

        $viaBlock = self::check([
            ['type' => 'section', 'anchor' => 'proof', 'children' => [$logos, $stories, ['type' => 'image', 'attachment_id' => 900, 'alt' => 'A'], ['type' => 'image', 'attachment_id' => 901, 'alt' => 'B'], ['type' => 'image', 'attachment_id' => 902, 'alt' => 'C'], ['type' => 'image', 'attachment_id' => 903, 'alt' => 'D'], ['type' => 'image', 'attachment_id' => 904, 'alt' => 'E']]],
        ]);
        $this->assertSame('layout_invalid', $viaBlock['code'], 'twenty-one pictures across patterns and blocks');
    }

    public function testButtonsAreCountedAcrossThePage(): void
    {
        $block = ['type' => 'buttons', 'buttons' => [['text' => 'A', 'url' => '/a'], ['text' => 'B', 'url' => '/b'], ['text' => 'C', 'url' => '/c']]];
        $this->assertArrayHasKey('outline', self::check(self::page(array_fill(0, 4, $block))), 'twelve buttons');
        $r = self::check(self::page(array_merge(array_fill(0, 4, $block), [['type' => 'buttons', 'buttons' => [['text' => 'D', 'url' => '/d']]]])));
        $this->assertSame('layout_invalid', $r['code']);
    }

    public function testBlocksAreCountedAcrossThePage(): void
    {
        $fifty    = array_fill(0, 50, ['type' => 'separator']);
        $sections = [];
        for ($i = 1; $i <= 9; ++$i) {
            $sections[] = ['type' => 'section', 'anchor' => 's' . $i . 'x', 'children' => $fifty];
        }
        $r = self::check($sections);

        $this->assertSame('layout_invalid', $r['code']);
        $this->assertStringContainsString('400', $r['failures'][0]['reason']);
    }

    public function testTheTextOfAPageIsCountedAcrossPatternsAndBlocks(): void
    {
        $paragraphs = array_fill(0, 12, ['type' => 'paragraph', 'text' => str_repeat('a', PageCreateBuilder::MAX_TEXT_CHARS)]);
        $this->assertArrayHasKey('outline', self::check(self::page($paragraphs)), 'sixty thousand characters');

        $over = self::check(self::page(array_merge($paragraphs, [self::pattern('faq', 'accordion', ['faq' => [['question' => 'Q', 'answer' => 'A']]])])));
        $this->assertSame('create_content_invalid', $over['code'], 'the text of pattern items counts too');
    }

    // -------------------------------------------------------------------------
    // The refusal
    // -------------------------------------------------------------------------

    public function testARefusalPointsAtTheNodeAndTheKey(): void
    {
        $outline = [
            ['type' => 'section', 'anchor' => 'one', 'children' => [['type' => 'paragraph', 'text' => 'Fine.']]],
            ['type' => 'section', 'anchor' => 'two', 'children' => [
                ['type' => 'paragraph', 'text' => 'Fine.'],
                ['type' => 'paragraph', 'text' => 'Fine.'],
                self::pattern('features', 'split', ['features' => self::items('features', 2)]),
            ]],
        ];
        $r = self::check($outline);

        $this->assertSame('pattern_items_out_of_range', $r['code']);
        $this->assertSame('outline[1].children[2]', $r['failures'][0]['path']);
        $this->assertSame('features', $r['failures'][0]['key']);
        $this->assertStringContainsString('outline[1].children[2].features', $r['detail']);
        $this->assertStringContainsString('3 to 8', $r['failures'][0]['reason']);
    }

    public function testALeafRefusalNamesTheBlockAndTheKey(): void
    {
        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Fine.'], ['type' => 'paragraph', 'text' => 'Has <b>markup</b>.']]));

        $this->assertSame('create_content_invalid', $r['code']);
        $this->assertSame('outline[0].children[1]', $r['failures'][0]['path']);
        $this->assertSame('text', $r['failures'][0]['key']);
    }

    public function testARefusalNeverEchoesWhatCameInWithTheInput(): void
    {
        $r = self::check(self::page([
            ['type' => 'paragraph', 'text' => 'Marker ZQXMARK <b>here</b>.'],
        ], ['zqxfield' => 1]));
        $this->assertStringNotContainsString('zqxfield', (string) json_encode($r));

        $r = self::check(self::page([['type' => 'paragraph', 'text' => 'Marker ZQXMARK <b>here</b>.']]));
        $this->assertSame('create_content_invalid', $r['code']);
        $this->assertStringNotContainsString('ZQXMARK', (string) json_encode($r));

        $r = self::check(self::page([self::pattern('faq', 'accordion', ['faq' => [['question' => 'Q', 'answer' => 'A ZQXMARK {{x}}']]])]));
        $this->assertSame('create_content_invalid', $r['code']);
        $this->assertStringNotContainsString('ZQXMARK', (string) json_encode($r));
    }

    public function testARefusalAboutAnItemPatternCarriesItsItemSchema(): void
    {
        $r = self::check(self::page([self::pattern('pricing', 'three-plans', ['plans' => [array_merge(self::ITEM['plans'], ['price' => ''])]])]));

        $this->assertSame('create_content_invalid', $r['code']);
        $this->assertSame('pricing', $r['pattern'] ?? null);
        $this->assertSame(OutlineV3Schema::itemSchema('pricing'), $r['item_schema'] ?? null);
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Run the grammar over an outline written as PHP arrays: the arrays become
     * the objects the command decodes.
     *
     * @param mixed $outline Outline.
     * @return array<string,mixed>
     */
    private static function check($outline): array
    {
        return OutlineV3::validate(json_decode((string) json_encode($outline), false, 64));
    }

    /**
     * One section whose anchor is "intro".
     *
     * @param list<array<string,mixed>> $children Children.
     * @param array<string,mixed>       $extra    More section fields.
     * @return list<array<string,mixed>>
     */
    private static function page(array $children, array $extra = []): array
    {
        return [array_merge(['type' => 'section', 'anchor' => 'intro', 'children' => $children], $extra)];
    }

    /**
     * A section with the given anchor, holding one paragraph unless it is given blocks.
     *
     * @param string                         $anchor   Anchor.
     * @param list<array<string,mixed>>|null $children Blocks of the section.
     * @return array<string,mixed>
     */
    private static function section(string $anchor, ?array $children = null): array
    {
        return ['type' => 'section', 'anchor' => $anchor, 'children' => $children ?? [['type' => 'paragraph', 'text' => 'Words.']]];
    }

    /**
     * @param string               $name    Pattern name.
     * @param string               $variant Variant.
     * @param array<string,mixed>  $body    The pattern's own field.
     * @return array<string,mixed>
     */
    private static function pattern(string $name, string $variant, array $body): array
    {
        return ['type' => 'pattern', 'pattern' => $name, 'variant' => $variant] + $body;
    }

    /**
     * @param string $array Item array.
     * @param int    $count How many.
     * @return list<array<string,mixed>>
     */
    private static function items(string $array, int $count): array
    {
        $out = [];
        for ($i = 0; $i < $count; ++$i) {
            $item = self::ITEM[$array];
            if (isset($item['attachment_id'])) {
                $item['attachment_id'] += $i;
            }
            $out[] = $item;
        }

        return $out;
    }

    /**
     * @param int $count How many.
     * @return list<array<string,mixed>>
     */
    private static function paragraphs(int $count): array
    {
        return $count === 0 ? [] : array_fill(0, $count, ['type' => 'paragraph', 'text' => 'Words.']);
    }

    /**
     * The seen names in the order of a reference list, once each.
     *
     * @param list<string> $seen      Names seen.
     * @param list<string> $reference Order.
     * @return list<string>
     */
    private function sorted(array $seen, array $reference): array
    {
        return array_values(array_intersect($reference, $seen));
    }
}
