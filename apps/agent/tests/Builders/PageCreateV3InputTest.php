<?php
/**
 * wpmgr/page-create with outline_version 3, judged where the whole input is
 * read (PageCreateBuilder::validate()): the version, the editor and format, the
 * design, the page frame, and the images a designed page names.
 *
 * Fixture:
 *   page-create-v3-cases.json  the accept/refuse table the control plane and the
 *                              dashboard read as well; a row's `agent` is the
 *                              answer of the agent
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\PageCreateBuilder
 * @covers \WPMgr\Agent\Abilities\Builders\DesignInput
 */
final class PageCreateV3InputTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    /** The rules of the table the input alone decides, with the two pages it accepts. */
    private const JUDGED = [
        'minimal_v3_page',
        'full_landing_page',
        'outline_version_3',
        'v3_needs_elementor_classic',
        'v3_fields_need_version',
        'section_needs_version',
        'page_frame_enum',
        'design_complete',
        'design_values_closed',
        'design_contrast',
    ];

    /** A floor that keeps the run from passing by judging next to nothing. */
    private const MIN_ROWS_RUN = 16;

    /**
     * @return array<string,array{0:string}>
     */
    public static function rows(): array
    {
        $rows = [];
        foreach (self::cases() as $case) {
            if (in_array($case['rule'], self::JUDGED, true)) {
                $rows[$case['name']] = [$case['name']];
            }
        }

        return $rows;
    }

    public function testTheRunJudgesTheRowsItShould(): void
    {
        $this->assertGreaterThanOrEqual(self::MIN_ROWS_RUN, count(self::rows()));
        $seen = [];
        foreach (self::cases() as $case) {
            $seen[$case['rule']] = true;
        }
        foreach (self::JUDGED as $rule) {
            $this->assertArrayHasKey($rule, $seen, $rule . ' names no row of the table');
        }
    }

    /**
     * @dataProvider rows
     */
    public function testTheInputIsJudgedAsTheTableSays(string $name): void
    {
        $case   = self::byName($name);
        $result = PageCreateBuilder::validate(self::decode($case['input']));

        $this->assertSame(
            $case['agent'],
            isset($result['spec']) ? 'ok' : ($result['code'] ?? 'no code'),
            $name . ' (' . $case['rule'] . '): ' . (string) ($result['detail'] ?? 'accepted')
        );
    }

    /**
     * @dataProvider rows
     */
    public function testARefusedInputCarriesItsFailures(string $name): void
    {
        $case = self::byName($name);
        if ($case['agent'] === 'ok') {
            $this->assertArrayHasKey('spec', PageCreateBuilder::validate(self::decode($case['input'])));

            return;
        }
        $result = PageCreateBuilder::validate(self::decode($case['input']));
        $this->assertSame($case['agent'], $result['code'] ?? null, $name);
        $this->assertIsString($result['detail'] ?? null, $name);
        $this->assertDoesNotMatchRegularExpression('/[\x00-\x1F\x7F]/', $result['detail'], $name . ': one line');
        if (str_starts_with($case['rule'], 'design_')) {
            $this->assertNotEmpty($result['failures'] ?? [], $name . ': a design refusal names its failures');
            $first = $result['failures'][0] ?? [];
            $this->assertStringContainsString('design', ($first['path'] ?? '') . '.' . ($first['key'] ?? ''), $name . ': the failure names the design');
        }
    }

    public function testAV3SpecKeepsTheVersionTheDesignAndTheFrame(): void
    {
        $input = self::decode(self::byName('v3-full-landing-page')['input']);
        $spec  = PageCreateBuilder::validate($input)['spec'] ?? null;

        $this->assertIsArray($spec);
        $this->assertSame(3, $spec['outline_version']);
        $this->assertSame('full_width', $spec['page_frame']);
        $this->assertSame('classic', $spec['elementor_format']);
        $this->assertSame(['colors', 'fonts', 'scale', 'radius', 'density'], array_keys($spec['design']));
        $this->assertSame('#0D7A63', $spec['design']['colors']['accent']);
        $this->assertSame(['base_px' => 17, 'ratio' => 'major_third'], $spec['design']['scale']);
        $this->assertCount(8, $spec['outline']);
        $this->assertSame('section', $spec['outline'][0]['type']);
        $this->assertIsArray($spec['outline'][0]['children'][0], 'the outline is arrays, not objects');

        unset($input->page_frame);
        $without = PageCreateBuilder::validate($input)['spec'] ?? [];
        $this->assertArrayNotHasKey('page_frame', $without, 'a frame not sent is the site\'s to resolve');
    }

    public function testAPlainSpecIsUnchanged(): void
    {
        $spec = PageCreateBuilder::validate((object) [
            'post_type'        => 'page',
            'editor'           => 'builder:elementor',
            'title'            => 'T',
            'elementor_format' => 'classic',
            'outline'          => [(object) ['type' => 'paragraph', 'text' => 'x']],
        ])['spec'] ?? [];

        $this->assertSame(['post_type', 'editor', 'title', 'outline', 'elementor_format'], array_keys($spec));
    }

    public function testADesignHoldsNoUnknownKeyAndNoSecondColourSpelling(): void
    {
        $base = self::decode(self::byName('v3-minimal-page')['input']);

        $extra = clone $base;
        $extra->design = clone $base->design;
        $extra->design->extra = 1;
        $this->assertSame('bad_input', PageCreateBuilder::validate($extra)['code'] ?? null, 'a key the design does not have');

        $lower = clone $base;
        $lower->design = clone $base->design;
        $lower->design->colors = clone $base->design->colors;
        $lower->design->colors->accent = '#0d7a63';
        $this->assertSame('bad_input', PageCreateBuilder::validate($lower)['code'] ?? null, 'the colour is upper-case #RRGGBB');

        $font = clone $base;
        $font->design = clone $base->design;
        $font->design->fonts = (object) ['heading' => 'Manrope;}', 'body' => 'system'];
        $this->assertSame('bad_input', PageCreateBuilder::validate($font)['code'] ?? null, 'a font family is letters, digits and spaces');

        $this->assertArrayHasKey('spec', PageCreateBuilder::validate($base), 'the untouched design is accepted');
    }

    public function testTheImagesOfADesignedPageInDocumentOrder(): void
    {
        $full = PageCreateBuilder::validate(self::decode(self::byName('v3-full-landing-page')['input']))['spec'] ?? [];
        $this->assertSame([42, 101, 102, 103, 104, 105, 106, 201, 202, 203], PageCreateBuilder::mediaIds($full));

        $spec = ['outline' => [
            ['type' => 'section', 'anchor' => 'a', 'background' => ['attachment_id' => 9], 'children' => [
                ['type' => 'image', 'attachment_id' => 5, 'alt' => ''],
                ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'split', 'children' => [['type' => 'image', 'attachment_id' => 7, 'alt' => '']]],
                ['type' => 'group', 'children' => [['type' => 'image', 'attachment_id' => 5, 'alt' => '']]],
                ['type' => 'columns', 'columns' => [[['type' => 'image', 'attachment_id' => 8, 'alt' => '']], [['type' => 'paragraph', 'text' => 'x']]]],
                ['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => [['attachment_id' => 11, 'alt' => 'x'], ['attachment_id' => 9, 'alt' => 'y']]],
                ['type' => 'pattern', 'pattern' => 'testimonials', 'variant' => 'featured', 'testimonials' => [
                    ['quote' => 'q', 'name' => 'n', 'attachment_id' => 13],
                    ['quote' => 'q', 'name' => 'n'],
                ]],
            ]],
        ]];
        $this->assertSame([9, 5, 7, 8, 11, 13], PageCreateBuilder::mediaIds($spec), 'distinct, first appearance');
    }

    // ---- fixture -----------------------------------------------------------

    /**
     * @return list<array<string,mixed>>
     */
    private static function cases(): array
    {
        $doc = json_decode((string) file_get_contents(self::FIXTURE), true, 512, JSON_THROW_ON_ERROR);
        self::assertIsArray($doc['cases'] ?? null);

        return $doc['cases'];
    }

    /**
     * @return array<string,mixed>
     */
    private static function byName(string $name): array
    {
        foreach (self::cases() as $case) {
            if ($case['name'] === $name) {
                return $case;
            }
        }
        throw new \LogicException('no such row: ' . $name);
    }

    private static function decode(string $input): object
    {
        $object = json_decode($input, false, 32);
        self::assertIsObject($object);

        return $object;
    }
}
