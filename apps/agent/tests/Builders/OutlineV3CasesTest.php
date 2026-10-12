<?php
/**
 * The outline version 3 grammar against the shared case table.
 *
 * Fixture:
 *   page-create-v3-cases.json  the accept/refuse table the control plane and the
 *                              dashboard read as well
 *
 * A row's `agent` is the grammar's answer for the row's outline: "ok" or the
 * refusal code. The grammar judges the outline. A row whose rule belongs to the
 * whole input (version, format, design, page frame, site facts, input size) is
 * judged where that input is read, so it is not run here, and a test makes sure
 * no rule of the table is left without a judge.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\OutlineV3;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\OutlineV3
 */
final class OutlineV3CasesTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    /** Rules about sections and what they hold: every row of them is run. */
    private const SECTION_RULES = [
        'minimal_v3_page',
        'full_landing_page',
        'sections_at_most_20',
        'section_only_at_top',
        'outline_all_sections',
        'section_children_1_to_50',
        'section_fields_closed',
        'section_options',
        'anchor_required',
        'images_at_most_20',
    ];

    /** Rules about patterns, their items, caps and icons. */
    private const PATTERN_RULES = [
        'pattern_item_caps',
        'icon_in_list',
        'pattern_text_rules',
    ];

    /** Rules about a block's own links and styles. */
    private const BLOCK_RULES = [
        'link_anchor_syntax',
        'button_style_enum',
    ];

    /**
     * Rules that look across the page: anchors, the links that point at them,
     * the number of heroes, the hero's place and heading level 1. Every row of
     * them is run.
     */
    private const PAGE_RULES = [
        'anchor_regex',
        'anchor_unique',
        'anchor_not_reserved',
        'link_anchor_resolves',
        'hero_at_most_once',
        'hero_first',
        'h1_inside_hero_only',
        'h1_at_most_once',
    ];

    /** Rules of the whole input and of the site, judged where the input is read. */
    private const INPUT_RULES = [
        'outline_version_3',
        'v3_needs_elementor_classic',
        'v3_fields_need_version',
        'section_needs_version',
        'page_frame_enum',
        'design_complete',
        'design_values_closed',
        'design_contrast',
        'font_in_registry',
        'background_is_image',
        'containers_required',
        'input_at_most_64_kib',
    ];

    /** Floors that keep the run from passing by judging next to nothing. */
    private const MIN_ROWS_RUN         = 37;
    private const MIN_REFUSED_ROWS_RUN = 26;

    // -------------------------------------------------------------------------
    // The rows
    // -------------------------------------------------------------------------

    /**
     * Every row the outline grammar answers.
     *
     * @return array<string,array{0:string}>
     */
    public static function judgedRows(): array
    {
        $rows = [];
        foreach (self::doc()['cases'] as $case) {
            if (self::judged($case)) {
                $rows[$case['name']] = [$case['name']];
            }
        }

        return $rows;
    }

    /**
     * @dataProvider judgedRows
     */
    public function testTheGrammarAnswersTheRow(string $name): void
    {
        $case   = self::byName($name);
        $result = OutlineV3::validate(self::outlineOf($case));

        $this->assertSame(
            $case['agent'],
            self::verdict($result),
            $name . ' (' . $case['rule'] . '): ' . (string) ($result['detail'] ?? 'accepted')
        );
    }

    /**
     * @dataProvider judgedRows
     */
    public function testARefusalCarriesTheEnvelope(string $name): void
    {
        $case = self::byName($name);
        if ($case['agent'] === 'ok') {
            $this->assertArrayHasKey('outline', OutlineV3::validate(self::outlineOf($case)));

            return;
        }
        $result = OutlineV3::validate(self::outlineOf($case));

        $this->assertSame($case['agent'], $result['code'] ?? null, $name);
        $this->assertIsString($result['detail'] ?? null, $name);
        $this->assertDoesNotMatchRegularExpression('/[\x00-\x1F\x7F]/', $result['detail'], $name . ': the message is one line');
        $this->assertLessThanOrEqual(1200, strlen($result['detail']), $name);
        $this->assertNotEmpty($result['failures'] ?? [], $name . ': a refusal names its failures');
        foreach ($result['failures'] as $failure) {
            $this->assertArrayHasKey('path', $failure, $name);
            $this->assertArrayHasKey('key', $failure, $name);
            $this->assertArrayHasKey('reason', $failure, $name);
        }
        $first = $result['failures'][0];
        $this->assertStringContainsString($first['reason'], $result['detail'], $name . ': the message inlines the first failure');
        $this->assertStringContainsString($first['path'] . '.' . $first['key'], $result['detail'], $name . ': the message says where');
    }

    public function testTheJudgedSetIsWhatTheTableHolds(): void
    {
        $rows = self::judgedRows();
        $this->assertGreaterThanOrEqual(self::MIN_ROWS_RUN, count($rows), 'most rows of the table are run');

        $refused = 0;
        foreach (array_keys($rows) as $name) {
            if (self::byName($name)['agent'] !== 'ok') {
                ++$refused;
            }
        }
        $this->assertGreaterThanOrEqual(self::MIN_REFUSED_ROWS_RUN, $refused, 'the refused rows are run, so the run cannot pass by accepting everything');

        foreach (['v3-minimal-page', 'v3-full-landing-page', 'sections-21', 'features-2-items', 'icon-not-in-the-list', 'pattern-text-with-markup', 'link-anchor-malformed', 'buttons-in-three-styles', 'hero-first-with-the-h1'] as $name) {
            $this->assertArrayHasKey($name, $rows, $name . ' is run');
        }
        foreach (['anchor-uppercase', 'anchor-41-characters', 'anchor-duplicate', 'anchor-reserved', 'link-anchor-missing', 'hero-in-the-second-section', 'hero-not-the-first-child', 'two-heroes', 'two-heroes-the-first-out-of-place', 'h1-without-a-hero', 'h1-in-a-later-section', 'h1-twice-in-the-hero'] as $name) {
            $this->assertArrayHasKey($name, $rows, $name . ' is run: the page-wide rules are judged here');
        }
        foreach (['design-missing', 'font-not-in-the-registry', 'outline-version-4', 'landing-page-over-the-input-limit'] as $name) {
            $this->assertArrayNotHasKey($name, $rows, $name . ' is judged elsewhere');
        }
    }

    public function testEveryRuleOfTheTableHasAJudgeAndNoRuleHasTwo(): void
    {
        $lists = [
            'sections' => self::SECTION_RULES,
            'patterns' => self::PATTERN_RULES,
            'blocks'   => self::BLOCK_RULES,
            'page'     => self::PAGE_RULES,
            'input'    => self::INPUT_RULES,
        ];
        $seen = [];
        foreach ($lists as $list => $rules) {
            foreach ($rules as $rule) {
                $this->assertArrayNotHasKey($rule, $seen, $rule . ' is in two lists');
                $seen[$rule] = $list;
            }
        }
        $inTable = [];
        foreach (self::doc()['cases'] as $case) {
            $inTable[$case['rule']] = true;
            $this->assertArrayHasKey($case['rule'], $seen, $case['name'] . ': rule ' . $case['rule'] . ' has no judge');
        }
        foreach (array_keys($seen) as $rule) {
            $this->assertArrayHasKey($rule, $inTable, $rule . ' is listed but no row has it');
        }
    }

    public function testAnAcceptedRowStaysAcceptedWhenOneFieldIsBroken(): void
    {
        // A positive control for the run itself: break the accepted full page
        // in one place and the verdict changes with it.
        $case = self::byName('v3-full-landing-page');
        $good = self::outlineOf($case);
        $this->assertSame('ok', self::verdict(OutlineV3::validate($good)));

        $broken = self::outlineOf($case);
        $broken[2]->children[1]->features[0]->icon = 'unicorn';
        $this->assertSame('icon_unknown', self::verdict(OutlineV3::validate($broken)));

        $broken = self::outlineOf($case);
        array_pop($broken[3]->children[0]->stats);
        array_pop($broken[3]->children[0]->stats);
        $this->assertSame('pattern_items_out_of_range', self::verdict(OutlineV3::validate($broken)));
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Whether the grammar answers this row: every row of the outline rules is
     * run, refused or accepted. The rules of the whole input are not.
     *
     * @param array<string,mixed> $case Case.
     */
    private static function judged(array $case): bool
    {
        $rule = (string) $case['rule'];

        return in_array($rule, self::SECTION_RULES, true)
            || in_array($rule, self::PATTERN_RULES, true)
            || in_array($rule, self::BLOCK_RULES, true)
            || in_array($rule, self::PAGE_RULES, true);
    }

    /**
     * @param array{outline?:mixed,code?:string} $result A validate() answer.
     */
    private static function verdict(array $result): string
    {
        return isset($result['outline']) ? 'ok' : (string) ($result['code'] ?? 'no code');
    }

    /**
     * The decoded outline of a case, with objects as the command decodes them.
     *
     * @param array<string,mixed> $case Case.
     * @return mixed
     */
    private static function outlineOf(array $case)
    {
        $input = json_decode(self::inputText($case), false, 64);
        self::assertIsObject($input, (string) $case['name']);

        return $input->outline ?? null;
    }

    /**
     * The exact input text of a case, padded when the case says how.
     *
     * @param array<string,mixed> $case Case.
     */
    private static function inputText(array $case): string
    {
        $text = $case['input'];
        self::assertIsString($text);
        if (!isset($case['generate'])) {
            return $text;
        }

        return $text . str_repeat((string) $case['generate']['append'], (int) $case['generate']['to_bytes'] - strlen($text));
    }

    /**
     * @return array<string,mixed>
     */
    private static function byName(string $name): array
    {
        foreach (self::doc()['cases'] as $case) {
            if ($case['name'] === $name) {
                return $case;
            }
        }
        self::fail('no case named ' . $name);
    }

    /**
     * @return array<string,mixed>
     */
    private static function doc(): array
    {
        static $doc = null;
        if ($doc === null) {
            $raw = file_get_contents(self::FIXTURE);
            self::assertIsString($raw, 'page-create-v3-cases.json is missing');
            $doc = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
            self::assertIsArray($doc);
        }

        return $doc;
    }
}
