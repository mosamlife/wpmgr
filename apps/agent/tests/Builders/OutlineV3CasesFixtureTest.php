<?php
/**
 * The outline v3 case table shared with the control plane and the dashboard.
 *
 * Fixture:
 *   page-create-v3-cases.json  hand-authored accept/refuse table for the
 *                              wpmgr/page-create input with outline_version 3
 *
 * This test judges the table, not the grammar. It checks that the table is
 * well formed, that every code outline v3 introduces and every pattern has a
 * case, and that each case does what its row says: an accepted case breaks no
 * rule, and a refused case breaks exactly the rule it names. The rules are
 * restated here as small probes over the decoded input so a typo in the table
 * cannot pass for a contract. The agent's grammar reads the same table in its
 * own test.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @coversNothing
 */
final class OutlineV3CasesFixtureTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    private const CASE_KEYS = ['name', 'rule', 'note', 'site', 'generate', 'input', 'agent', 'go'];

    /** The wpmgr/page-create input limit in bytes. */
    private const MAX_INPUT_BYTES = 65536;

    /** Sections on one page, the images a page may show, and the heroes. */
    private const MAX_SECTIONS = 20;
    private const MAX_IMAGES   = 20;
    private const MAX_HEROES   = 1;

    /** The type-scale ratios a design may name. The input never carries the number. */
    private const RATIOS = ['major_second', 'minor_third', 'major_third', 'perfect_fourth'];

    /** Children of one section. */
    private const MAX_CHILDREN = 50;

    /** The anchor grammar and the ids a theme already owns. */
    private const RE_ANCHOR = '/^[a-z][a-z0-9-]{1,39}$/D';
    private const RESERVED  = ['content', 'main', 'primary', 'page', 'masthead', 'colophon', 'site-header', 'site-footer', 'wpadminbar'];

    /** Pattern arrays and their item counts: [min, max]. */
    private const ITEM_CAPS = [
        'features'     => [3, 8],
        'testimonials' => [1, 6],
        'plans'        => [1, 4],
        'faq'          => [1, 12],
        'logos'        => [3, 10],
        'stats'        => [2, 4],
    ];

    /** The array each item pattern keeps its items in. */
    private const PATTERN_ARRAY = [
        'features'     => 'features',
        'testimonials' => 'testimonials',
        'pricing'      => 'plans',
        'faq'          => 'faq',
        'logos'        => 'logos',
        'stats'        => 'stats',
    ];

    /** Every pattern node, composed ones included. */
    private const PATTERNS = ['hero', 'features', 'testimonials', 'pricing', 'faq', 'cta', 'logos', 'stats'];

    /**
     * The codes outline v3 introduces for the page-create input, each with
     * the control plane's verdict for the case that shows it. A rule that
     * depends on the site is the agent's alone, so its verdict is accept.
     */
    private const NEW_CODES = [
        'containers_required'        => 'accept',
        'anchor_invalid'             => 'refuse',
        'anchor_duplicate'           => 'refuse',
        'anchor_reserved'            => 'refuse',
        'hero_misplaced'             => 'refuse',
        'h1_outside_hero'            => 'refuse',
        'background_not_image'       => 'accept',
        'pattern_items_out_of_range' => 'refuse',
        'icon_unknown'               => 'refuse',
        'link_anchor_missing'        => 'refuse',
        'font_unknown'               => 'accept',
        'design_contrast_low'        => 'refuse',
        'design_not_ready'           => 'refuse',
        'page_frame_unavailable'     => 'accept',
    ];

    /** The codes a case may answer: the grammar's own, then the new ones. */
    private const OLD_CODES = ['bad_input', 'create_content_invalid', 'layout_invalid', 'link_invalid', 'layout_needs_block_editor'];

    /** The site fact each site-dependent code needs in its case. */
    private const SITE_FACT = [
        'containers_required'    => 'elementor_containers',
        'background_not_image'   => 'attachment_mimes',
        'font_unknown'           => 'fonts',
        'page_frame_unavailable' => 'block_theme',
    ];

    /**
     * Rules whose accepted rows depend on the site too: the site decides what
     * is built, so an accepted row states the site it was judged for.
     */
    private const SITE_RULES = ['page_frame_fits_theme'];

    // -------------------------------------------------------------------------
    // The table
    // -------------------------------------------------------------------------

    public function test_cases_are_well_formed(): void
    {
        $doc = self::doc();
        $this->assertSame(['note', 'icons', 'cases'], array_keys($doc));
        $this->assertIsString($doc['note']);
        $this->assertNotSame('', $doc['note']);

        $this->assertIsArray($doc['icons']);
        $this->assertGreaterThanOrEqual(36, count($doc['icons']), 'about 40 neutral icon names');
        $this->assertLessThanOrEqual(44, count($doc['icons']));
        $this->assertSame($doc['icons'], array_values(array_unique($doc['icons'])), 'icon names are unique');
        foreach ($doc['icons'] as $icon) {
            $this->assertMatchesRegularExpression('/^[a-z][a-z-]{1,23}$/D', $icon);
        }

        $names = [];
        foreach ($doc['cases'] as $case) {
            $name = (string) ($case['name'] ?? '');
            $this->assertMatchesRegularExpression('/^[a-z0-9][a-z0-9-]*$/D', $name, 'a case name is kebab-case');
            $this->assertNotContains($name, $names, 'case names are unique');
            $names[] = $name;
            $this->assertSame([], array_values(array_diff(array_keys($case), self::CASE_KEYS)), $name . ': unknown case field');
            foreach (['rule', 'input', 'agent', 'go'] as $key) {
                $this->assertArrayHasKey($key, $case, $name . ': ' . $key . ' is required');
            }
            $this->assertMatchesRegularExpression('/^[a-z][a-z0-9_]*$/D', $case['rule'], $name);
            $this->assertContains($case['go'], ['accept', 'refuse'], $name);
            $this->assertTrue($case['agent'] === 'ok' || in_array($case['agent'], self::allCodes(), true), $name . ': unknown agent verdict ' . $case['agent']);
            if (isset($case['note'])) {
                $this->assertIsString($case['note'], $name);
                $this->assertNotSame('', $case['note'], $name . ': a note says something');
            }
            if ($case['agent'] === 'ok') {
                $this->assertSame('accept', $case['go'], $name . ': what the agent accepts, the control plane accepts');
            }
            if (isset($case['site'])) {
                $this->assertSame('accept', $case['go'], $name . ': a rule that needs the site is the agent\'s alone');
                if ($case['agent'] === 'ok') {
                    $this->assertContains($case['rule'], self::SITE_RULES, $name . ': an accepted row carries a site only for a rule the site decides');
                    $this->assertNotSame([], $case['site'], $name);
                    foreach (array_keys($case['site']) as $fact) {
                        $this->assertContains($fact, self::SITE_FACT, $name . ': unknown site fact ' . $fact);
                    }
                } else {
                    $this->assertContains($case['agent'], array_keys(self::SITE_FACT), $name . ': only a site code carries a site');
                    $this->assertArrayHasKey(self::SITE_FACT[$case['agent']], $case['site'], $name);
                }
            } else {
                $this->assertNotContains($case['agent'], array_keys(self::SITE_FACT), $name . ': a site code names the site facts it needs');
                if ($case['agent'] !== 'ok' && $case['agent'] !== 'create_content_invalid') {
                    $this->assertSame('refuse', $case['go'], $name . ': both halves refuse a rule that does not need the site');
                }
            }

            $text = self::inputText($case);
            if (isset($case['generate'])) {
                $this->assertSame($case['generate']['to_bytes'], strlen($text), $name);
            }
            $this->assertIsObject(json_decode($text, false, 32), $name . ': input is a JSON object');
        }
    }

    public function test_the_probes_and_the_published_schema_state_the_same_numbers(): void
    {
        $this->assertSame(OutlineV3Schema::ITEM_CAPS, self::ITEM_CAPS);
        $this->assertSame(OutlineV3Schema::PATTERN_ARRAY, self::PATTERN_ARRAY);
        $this->assertSame(OutlineV3Schema::RESERVED_ANCHORS, self::RESERVED);
        $this->assertSame(OutlineV3Schema::MAX_SECTIONS, self::MAX_SECTIONS);
        $this->assertSame(OutlineV3Schema::MAX_HEROES, self::MAX_HEROES);
        $this->assertSame(OutlineV3Schema::SCALE_RATIOS, self::RATIOS);
        $this->assertSame(OutlineV3Schema::MAX_SECTION_CHILDREN, self::MAX_CHILDREN);
        $this->assertSame('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', self::RE_ANCHOR);
        $this->assertSame(OutlineV3Schema::ICONS, self::doc()['icons']);
        $patterns = self::PATTERNS;
        $schema   = OutlineV3Schema::PATTERNS;
        sort($patterns);
        sort($schema);
        $this->assertSame($schema, $patterns);
        $this->assertSame(array_keys(OutlineV3Schema::VARIANTS), OutlineV3Schema::PATTERNS);
    }

    public function test_every_new_code_has_a_case(): void
    {
        $seen = [];
        foreach (self::doc()['cases'] as $case) {
            $seen[$case['agent']][] = $case['go'];
        }
        foreach (self::NEW_CODES as $code => $go) {
            $this->assertArrayHasKey($code, $seen, 'no case answers ' . $code);
            $this->assertContains($go, $seen[$code], $code . ' has no case with control plane verdict ' . $go);
        }
        foreach (array_keys(self::NEW_CODES) as $code) {
            $this->assertContains($code, self::allCodes());
        }
    }

    public function test_every_pattern_has_an_honest_case(): void
    {
        $shown = [];
        foreach (self::doc()['cases'] as $case) {
            if ($case['agent'] !== 'ok') {
                continue;
            }
            $facts = self::facts(self::decode($case));
            foreach ($facts['patterns'] as $p) {
                $shown[$p['pattern']][] = $case['name'];
            }
        }
        foreach (self::PATTERNS as $pattern) {
            $this->assertArrayHasKey($pattern, $shown, 'no accepted case shows the ' . $pattern . ' pattern');
        }
        foreach (self::PATTERN_ARRAY as $pattern => $array) {
            $this->assertNotEmpty(self::itemCountsShown($array), 'an accepted case shows the ' . $array . ' items');
        }
    }

    public function test_item_caps_are_shown_at_both_ends(): void
    {
        foreach (self::ITEM_CAPS as $array => [$min, $max]) {
            $counts = self::itemCountsShown($array);
            $this->assertContains($min, $counts, $array . ': no accepted case at the smallest count');
            $this->assertContains($max, $counts, $array . ': no accepted case at the largest count');
        }
    }

    public function test_accepted_cases_break_no_rule(): void
    {
        foreach (self::doc()['cases'] as $case) {
            if ($case['agent'] !== 'ok') {
                continue;
            }
            $this->assertSame([], self::violations($case), $case['name'] . ': an accepted case breaks a rule');
        }
    }

    public function test_refused_cases_break_exactly_the_rule_they_name(): void
    {
        $probed = 0;
        foreach (self::doc()['cases'] as $case) {
            if ($case['agent'] === 'ok' || !array_key_exists($case['rule'], self::probes())) {
                continue;
            }
            ++$probed;
            $this->assertSame([$case['rule']], self::violations($case), $case['name'] . ': a refused case breaks one rule, the one it names');
        }
        $this->assertGreaterThanOrEqual(25, $probed, 'most refused cases are probed');
    }

    public function test_each_probe_is_shown_firing_and_quiet(): void
    {
        $fired = [];
        $quiet = [];
        foreach (self::doc()['cases'] as $case) {
            $violations = self::violations($case);
            foreach ($violations as $rule) {
                $fired[$rule] = true;
            }
            if ($case['agent'] === 'ok') {
                foreach (array_keys(self::probes()) as $rule) {
                    $quiet[$rule] = true;
                }
            }
        }
        foreach (array_keys(self::probes()) as $rule) {
            $this->assertArrayHasKey($rule, $fired, 'no case breaks ' . $rule . '; its probe was never seen to fire');
            $this->assertArrayHasKey($rule, $quiet, $rule);
        }
    }

    public function test_the_ratio_probe_takes_each_name_and_nothing_else(): void
    {
        // A positive control for the design probes: the same design with only the ratio changed.
        $design = self::decode(self::caseNamed('v3-full-landing-page'))['design'];
        $with   = static function ($ratio) use ($design): array {
            $design['scale']['ratio'] = $ratio;

            return $design;
        };
        foreach (self::RATIOS as $name) {
            $this->assertTrue(self::designValuesValid($with($name)), $name . ' is a ratio');
        }
        foreach ([1.25, 1, '1.25', 'golden', 'Major_Third', "major_third\n", '', null, true] as $bad) {
            $this->assertFalse(self::designValuesValid($with($bad)), var_export($bad, true) . ' is not a ratio');
        }
    }

    public function test_the_hero_probes_split_the_count_from_the_place(): void
    {
        // One hero in place breaks neither; a lone hero out of place breaks the place; two heroes break the count only.
        $one = self::decode(self::caseNamed('hero-first-with-the-h1'));
        $this->assertSame(1, self::heroCount($one));
        $this->assertFalse(self::heroMisplaced($one));

        $lone = self::decode(self::caseNamed('hero-in-the-second-section'));
        $this->assertSame(1, self::heroCount($lone));
        $this->assertTrue(self::heroMisplaced($lone));

        foreach (['two-heroes', 'two-heroes-the-first-out-of-place'] as $name) {
            $two = self::decode(self::caseNamed($name));
            $this->assertSame(2, self::heroCount($two), $name);
            $this->assertFalse(self::heroMisplaced($two), $name . ': the place of a hero is not judged on a page with two');
        }
    }

    /**
     * @param string $name Case name.
     * @return array<string,mixed>
     */
    private static function caseNamed(string $name): array
    {
        foreach (self::doc()['cases'] as $case) {
            if ($case['name'] === $name) {
                return $case;
            }
        }
        self::fail('no case named ' . $name);
    }

    public function test_the_size_cases_sit_on_the_input_limit(): void
    {
        $at   = null;
        $over = null;
        foreach (self::doc()['cases'] as $case) {
            if (!isset($case['generate'])) {
                continue;
            }
            $this->assertSame(' ', $case['generate']['append'], 'generate appends JSON whitespace only');
            if ($case['generate']['to_bytes'] === self::MAX_INPUT_BYTES) {
                $at = $case;
            }
            if ($case['generate']['to_bytes'] === self::MAX_INPUT_BYTES + 1) {
                $over = $case;
            }
        }
        $this->assertNotNull($at, 'a case at 65536 bytes');
        $this->assertNotNull($over, 'a case at 65537 bytes');
        $this->assertSame('ok', $at['agent']);
        $this->assertSame('accept', $at['go']);
        $this->assertSame('bad_input', $over['agent']);
        $this->assertSame('refuse', $over['go']);
        $this->assertSame($at['input'], $over['input'], 'the two size cases differ only in padding');
        $facts = self::facts(self::decode($at));
        $this->assertGreaterThanOrEqual(6, count($facts['patterns']), 'the size cases are built on a full landing page');
    }

    // -------------------------------------------------------------------------
    // Probes: the rules, restated over the decoded input
    // -------------------------------------------------------------------------

    /**
     * Every probe, keyed by rule name. A probe answers true when its rule is
     * broken. A case row names the rule it exercises, so a refused case must
     * break that rule and no other.
     *
     * @return array<string,callable(array<string,mixed>):bool>
     */
    private static function probes(): array
    {
        return [
            'outline_version_3'         => static fn (array $in): bool => array_key_exists('outline_version', $in) && $in['outline_version'] !== 3,
            'v3_needs_elementor_classic' => static fn (array $in): bool => ($in['outline_version'] ?? null) === 3
                && (($in['editor'] ?? null) !== 'builder:elementor' || ($in['elementor_format'] ?? null) !== 'classic'),
            'v3_fields_need_version'    => static fn (array $in): bool => !array_key_exists('outline_version', $in)
                && (array_key_exists('design', $in) || array_key_exists('page_frame', $in)),
            'section_needs_version'     => static fn (array $in): bool => !array_key_exists('outline_version', $in) && self::topTypes($in) !== [] && in_array('section', self::topTypes($in), true),
            'page_frame_enum'           => static fn (array $in): bool => array_key_exists('page_frame', $in) && !in_array($in['page_frame'], ['theme', 'full_width', 'canvas'], true),
            'design_complete'           => static fn (array $in): bool => ($in['outline_version'] ?? null) === 3 && !self::designComplete($in['design'] ?? null),
            'design_values_closed'      => static fn (array $in): bool => is_array($in['design'] ?? null) && self::designComplete($in['design']) && !self::designValuesValid($in['design']),
            'design_contrast'           => static fn (array $in): bool => is_array($in['design'] ?? null) && self::designComplete($in['design']) && self::designValuesValid($in['design']) && !self::designContrastOk($in['design']),
            'outline_all_sections'      => static fn (array $in): bool => ($in['outline_version'] ?? null) === 3 && array_diff(self::topTypes($in), ['section']) !== [],
            'sections_at_most_20'       => static fn (array $in): bool => count(self::facts($in)['sections']) > self::MAX_SECTIONS,
            'section_only_at_top'       => static fn (array $in): bool => self::facts($in)['nested_section'],
            'section_children_1_to_50'  => static fn (array $in): bool => self::facts($in)['bad_child_count'],
            'anchor_required'           => static fn (array $in): bool => self::facts($in)['anchor_missing'],
            'anchor_regex'              => static fn (array $in): bool => self::anyAnchor($in, static fn (string $a): bool => preg_match(self::RE_ANCHOR, $a) !== 1),
            'anchor_unique'             => static fn (array $in): bool => count(self::facts($in)['anchors']) !== count(array_unique(self::facts($in)['anchors'])),
            'anchor_not_reserved'       => static fn (array $in): bool => self::anyAnchor($in, static fn (string $a): bool => in_array($a, self::RESERVED, true)),
            'link_anchor_resolves'      => static fn (array $in): bool => self::anyLink($in, static fn (string $u, array $anchors): bool => preg_match('/^#[a-z][a-z0-9-]{1,39}$/D', $u) === 1 && !in_array(substr($u, 1), $anchors, true)),
            'link_anchor_syntax'        => static fn (array $in): bool => self::anyLink($in, static fn (string $u, array $anchors): bool => $u !== '' && $u[0] === '#' && preg_match('/^#[a-z][a-z0-9-]{1,39}$/D', $u) !== 1),
            'hero_at_most_once'         => static fn (array $in): bool => self::heroCount($in) > self::MAX_HEROES,
            'hero_first'                => static fn (array $in): bool => self::heroMisplaced($in),
            'h1_inside_hero_only'       => static fn (array $in): bool => self::facts($in)['h1_outside'],
            'h1_at_most_once'           => static fn (array $in): bool => self::facts($in)['h1_count'] > 1,
            'pattern_item_caps'         => static fn (array $in): bool => self::facts($in)['items_out_of_range'],
            'icon_in_list'              => static fn (array $in): bool => self::facts($in)['icon_unknown'],
            'images_at_most_20'         => static fn (array $in): bool => self::facts($in)['images'] > self::MAX_IMAGES,
        ];
    }

    /**
     * The rules a case breaks, by probe name.
     *
     * @param array<string,mixed> $case Case.
     * @return list<string>
     */
    private static function violations(array $case): array
    {
        $in  = self::decode($case);
        $out = [];
        foreach (self::probes() as $rule => $probe) {
            if ($probe($in)) {
                $out[] = $rule;
            }
        }
        if (strlen(self::inputText($case)) > self::MAX_INPUT_BYTES) {
            $out[] = 'input_at_most_64_kib';
        }

        return $out;
    }

    /**
     * One walk over the outline.
     *
     * @param array<string,mixed> $in Decoded input.
     * @return array<string,mixed>
     */
    private static function facts(array $in): array
    {
        static $memo = [];
        $key = md5((string) json_encode($in));
        if (isset($memo[$key])) {
            return $memo[$key];
        }
        $icons = self::doc()['icons'];
        $f     = [
            'sections'           => [],
            'anchors'            => [],
            'anchor_missing'     => false,
            'nested_section'     => false,
            'bad_child_count'    => false,
            'patterns'           => [],
            'h1_count'           => 0,
            'h1_outside'         => false,
            'items_out_of_range' => false,
            'icon_unknown'       => false,
            'images'             => 0,
            'links'              => [],
            'hero_at'            => [],
        ];
        $outline = is_array($in['outline'] ?? null) ? $in['outline'] : [];
        $walk    = null;
        $walk    = static function (array $nodes, int $section, bool $inHero, bool $direct) use (&$walk, &$f, $icons): void {
            foreach (array_values($nodes) as $j => $n) {
                if (!is_array($n)) {
                    continue;
                }
                $type = $n['type'] ?? null;
                if ($type === 'section') {
                    $f['nested_section'] = true;
                    continue;
                }
                if ($type === 'heading' && ($n['level'] ?? null) === 1) {
                    ++$f['h1_count'];
                    if (!$inHero) {
                        $f['h1_outside'] = true;
                    }
                }
                if ($type === 'image') {
                    ++$f['images'];
                }
                if ($type === 'buttons') {
                    foreach ($n['buttons'] ?? [] as $b) {
                        $f['links'][] = (string) ($b['url'] ?? '');
                    }
                }
                if ($type === 'group') {
                    $walk($n['children'] ?? [], $section, $inHero, false);
                }
                if ($type === 'columns') {
                    foreach ($n['columns'] ?? [] as $col) {
                        $walk($col['children'] ?? [], $section, $inHero, false);
                    }
                }
                if ($type !== 'pattern') {
                    continue;
                }
                $pattern = (string) ($n['pattern'] ?? '');
                $f['patterns'][] = ['pattern' => $pattern, 'section' => $section, 'index' => $j, 'direct' => $direct];
                if ($pattern === 'hero') {
                    $f['hero_at'][] = [$section, $j, $direct];
                }
                if ($pattern === 'hero' || $pattern === 'cta') {
                    $walk($n['children'] ?? [], $section, $pattern === 'hero', false);
                    continue;
                }
                $array = self::PATTERN_ARRAY[$pattern] ?? null;
                if ($array === null) {
                    continue;
                }
                $items = $n[$array] ?? [];
                [$min, $max] = self::ITEM_CAPS[$array];
                if (!is_array($items) || count($items) < $min || count($items) > $max) {
                    $f['items_out_of_range'] = true;
                }
                foreach (is_array($items) ? $items : [] as $item) {
                    if (isset($item['icon']) && !in_array($item['icon'], $icons, true)) {
                        $f['icon_unknown'] = true;
                    }
                    if (isset($item['attachment_id'])) {
                        ++$f['images'];
                    }
                    if (isset($item['cta']['url'])) {
                        $f['links'][] = (string) $item['cta']['url'];
                    }
                }
            }
        };
        foreach (array_values($outline) as $i => $s) {
            if (!is_array($s) || ($s['type'] ?? null) !== 'section') {
                continue;
            }
            $f['sections'][] = $s;
            if (!isset($s['anchor'])) {
                $f['anchor_missing'] = true;
            } else {
                $f['anchors'][] = (string) $s['anchor'];
            }
            $children = $s['children'] ?? null;
            if (!is_array($children) || $children === [] || count($children) > self::MAX_CHILDREN) {
                $f['bad_child_count'] = true;
            }
            if (isset($s['background']['attachment_id'])) {
                ++$f['images'];
            }
            $walk(is_array($children) ? $children : [], $i, false, true);
        }

        return $memo[$key] = $f;
    }

    /**
     * The heroes of a page, counted by shape alone as the grammar counts them:
     * the hero patterns that are direct children of a section.
     *
     * @param array<string,mixed> $in Input.
     */
    private static function heroCount(array $in): int
    {
        $count = 0;
        foreach (self::facts($in)['hero_at'] as [, , $direct]) {
            if ($direct) {
                ++$count;
            }
        }

        return $count;
    }

    /**
     * Whether the hero is out of place. The place is judged only on a page
     * with at most MAX_HEROES heroes: a second hero is counted first, so it
     * breaks the count rule and not this one.
     *
     * @param array<string,mixed> $in Input.
     */
    private static function heroMisplaced(array $in): bool
    {
        if (self::heroCount($in) > self::MAX_HEROES) {
            return false;
        }
        foreach (self::facts($in)['hero_at'] as [$section, $index, $direct]) {
            if ($section !== 0 || $index !== 0 || !$direct) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param array<string,mixed>      $in Input.
     * @param callable(string):bool    $bad Predicate on one anchor.
     */
    private static function anyAnchor(array $in, callable $bad): bool
    {
        foreach (self::facts($in)['anchors'] as $a) {
            if ($bad($a)) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param array<string,mixed>                      $in  Input.
     * @param callable(string,list<string>):bool       $bad Predicate on one link and the page anchors.
     */
    private static function anyLink(array $in, callable $bad): bool
    {
        $f = self::facts($in);
        foreach ($f['links'] as $u) {
            if ($bad($u, $f['anchors'])) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param array<string,mixed> $in Input.
     * @return list<string>
     */
    private static function topTypes(array $in): array
    {
        $types = [];
        foreach (is_array($in['outline'] ?? null) ? $in['outline'] : [] as $n) {
            $types[] = is_array($n) ? (string) ($n['type'] ?? '') : '';
        }

        return $types;
    }

    /** @param mixed $design Design. */
    private static function designComplete($design): bool
    {
        if (!is_array($design)) {
            return false;
        }
        $roles = ['ground', 'surface', 'ink', 'muted', 'accent', 'on_accent'];
        if (!is_array($design['colors'] ?? null) || array_diff($roles, array_keys($design['colors'])) !== []) {
            return false;
        }
        if (!is_array($design['fonts'] ?? null) || !isset($design['fonts']['heading'], $design['fonts']['body'])) {
            return false;
        }
        if (!is_array($design['scale'] ?? null) || !isset($design['scale']['base_px'], $design['scale']['ratio'])) {
            return false;
        }

        return isset($design['radius'], $design['density']);
    }

    /** @param array<string,mixed> $design Complete design. */
    private static function designValuesValid(array $design): bool
    {
        $roles = ['ground', 'surface', 'ink', 'muted', 'accent', 'on_accent'];
        if (array_keys($design) !== array_values(array_intersect(array_keys($design), ['colors', 'fonts', 'scale', 'radius', 'density']))) {
            return false;
        }
        if (array_diff(array_keys($design['colors']), $roles) !== []) {
            return false;
        }
        foreach ($design['colors'] as $hex) {
            if (!is_string($hex) || preg_match('/^#[0-9A-F]{6}$/D', $hex) !== 1 || $hex === '#000000') {
                return false;
            }
        }
        foreach ($design['fonts'] as $family) {
            if (!is_string($family) || preg_match('/^[A-Za-z0-9 ]{1,60}$/D', $family) !== 1) {
                return false;
            }
        }
        $base  = $design['scale']['base_px'];
        $ratio = $design['scale']['ratio'];
        if (!is_int($base) || $base < 15 || $base > 20 || !in_array($ratio, self::RATIOS, true)) {
            return false;
        }

        return in_array($design['radius'], ['none', 'sm', 'md', 'lg'], true) && in_array($design['density'], ['compact', 'normal', 'airy'], true);
    }

    /** @param array<string,mixed> $design Valid design. */
    private static function designContrastOk(array $design): bool
    {
        $c = $design['colors'];

        return self::contrast($c['ink'], $c['ground']) >= 4.5
            && self::contrast($c['ink'], $c['surface']) >= 4.5
            && self::contrast($c['on_accent'], $c['accent']) >= 4.5;
    }

    /** WCAG contrast ratio of two #RRGGBB colours. */
    private static function contrast(string $a, string $b): float
    {
        $la = self::luminance($a);
        $lb = self::luminance($b);

        return (max($la, $lb) + 0.05) / (min($la, $lb) + 0.05);
    }

    private static function luminance(string $hex): float
    {
        $lin = [];
        foreach ([1, 3, 5] as $at) {
            $v     = hexdec(substr($hex, $at, 2)) / 255;
            $lin[] = $v <= 0.03928 ? $v / 12.92 : (($v + 0.055) / 1.055) ** 2.4;
        }

        return 0.2126 * $lin[0] + 0.7152 * $lin[1] + 0.0722 * $lin[2];
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Item counts the accepted cases show for one pattern array.
     *
     * @return list<int>
     */
    private static function itemCountsShown(string $array): array
    {
        $counts = [];
        foreach (self::doc()['cases'] as $case) {
            if ($case['agent'] !== 'ok') {
                continue;
            }
            $in = self::decode($case);
            foreach (is_array($in['outline'] ?? null) ? $in['outline'] : [] as $s) {
                foreach (is_array($s['children'] ?? null) ? $s['children'] : [] as $n) {
                    if (is_array($n) && ($n['type'] ?? null) === 'pattern' && isset($n[$array]) && is_array($n[$array])) {
                        $counts[] = count($n[$array]);
                    }
                }
            }
        }

        return array_values(array_unique($counts));
    }

    /** @return list<string> */
    private static function allCodes(): array
    {
        return array_merge(self::OLD_CODES, array_keys(self::NEW_CODES));
    }

    /**
     * The decoded input of a case, padding included.
     *
     * @param array<string,mixed> $case Case.
     * @return array<string,mixed>
     */
    private static function decode(array $case): array
    {
        $in = json_decode(self::inputText($case), true, 32);
        self::assertIsArray($in, (string) $case['name']);

        return $in;
    }

    /**
     * The exact input text of a case, generated when the case says how.
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
        $to = $case['generate']['to_bytes'];
        self::assertIsInt($to);
        self::assertGreaterThan(strlen($text), $to);

        return $text . str_repeat((string) $case['generate']['append'], $to - strlen($text));
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
