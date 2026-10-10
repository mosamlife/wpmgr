<?php
/**
 * The builder contract fixtures shared with the control plane.
 *
 * Fixtures:
 *   page-edit-schema.json       generated; regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit
 *   page-structure-schema.json  generated; regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit
 *   page-edit-ops-cases.json    hand-authored accept/refuse table (agent code + Go verdict); a case with
 *                               a "note" says how its input was generated
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderContract
 */
final class BuilderContractFixturesTest extends TestCase
{
    private const FIXTURE_DIR = __DIR__ . '/../fixtures/ability-run/';

    /**
     * Every rule the ops cases must exercise, with the verdicts of the case
     * that shows it: the agent's answer and the control plane's.
     */
    private const RULES = [
        'operations_at_least_one'             => ['bad_input', 'refuse'],
        'operations_at_most_25'               => ['bad_input', 'refuse'],
        'op_known'                            => ['bad_input', 'refuse'],
        'ref_charset'                         => ['bad_input', 'refuse'],
        'ref_at_most_32'                      => ['bad_input', 'refuse'],
        'ref_at_most_once'                    => ['ops_invalid', 'refuse'],
        'insert_one_anchor'                   => ['bad_input', 'refuse'],
        'position_only_with_into'             => ['bad_input', 'refuse'],
        'outline_at_most_50'                  => ['bad_input', 'refuse'],
        'field_in_editable'                   => ['node_not_editable', 'accept'],
        'ref_on_page'                         => ['node_not_found', 'accept'],
        'locked_not_set_text'                 => ['node_not_editable', 'accept'],
        'locked_not_replaced'                 => ['node_not_editable', 'accept'],
        'locked_not_removed'                  => ['node_not_editable', 'accept'],
        'locked_not_moved'                    => ['node_not_editable', 'accept'],
        'holder_of_locked_not_removed'        => ['node_not_editable', 'accept'],
        'holder_of_locked_not_replaced'       => ['node_not_editable', 'accept'],
        'op_declared_by_builder'            => ['op_not_supported_by_builder', 'accept'],
        'input_at_most_64_kib'                => ['bad_input', 'refuse'],
        'valid_batch'                         => ['ok', 'accept'],
        'ref_not_removed_earlier'             => ['ops_invalid', 'accept'],
        'anchor_not_replaced_earlier'         => ['ops_invalid', 'accept'],
        'insert_into_takes_children'          => ['node_not_editable', 'accept'],
        'move_not_into_own_subtree'           => ['node_not_editable', 'accept'],
        'new_nodes_at_most_400'               => ['page_too_large', 'accept'],
        'set_text_url_rule'                   => ['bad_input', 'refuse'],
        'set_text_text_rule'                  => ['bad_input', 'refuse'],
        'elementor_image_offers_caption_only' => ['node_not_editable', 'accept'],
        'set_text_alt_rule'                   => ['bad_input', 'refuse'],
        'set_text_caption_rule'               => ['bad_input', 'refuse'],
        'set_text_button_text_rule'           => ['bad_input', 'accept'],
    ];

    /** Rules whose cases must also show the accepted side of the boundary. */
    private const BOUNDARY_RULES = ['input_at_most_64_kib', 'set_text_caption_rule', 'set_text_button_text_rule'];

    private const AGENT_VERDICTS = ['ok', 'bad_input', 'ops_invalid', 'node_not_found', 'node_not_editable', 'op_not_supported_by_builder', 'page_too_large'];

    private const GO_VERDICTS = ['accept', 'refuse'];

    /** The builder ids an editor may name. */
    private const BUILDERS = ['elementor', 'beaver', 'wpbakery', 'divi5', 'bricks', 'breakdance', 'oxygen6'];

    private const CASE_KEYS = ['name', 'rule', 'note', 'builder', 'input', 'generate', 'agent', 'go'];

    // -------------------------------------------------------------------------
    // Schemas
    // -------------------------------------------------------------------------

    public function test_page_edit_schema_is_the_fixture(): void
    {
        $this->assertFixture('page-edit-schema.json', self::encodeSchema(BuilderContract::pageEditInputSchema()));
    }

    public function test_page_structure_schema_is_the_fixture(): void
    {
        $this->assertFixture('page-structure-schema.json', self::encodeSchema(BuilderContract::pageStructureInputSchema()));
    }

    public function test_page_edit_schema_pins_the_limits(): void
    {
        $this->assertSame('/^[A-Za-z0-9_-]{1,32}$/D', BuilderContract::RE_REF);
        $this->assertSame(25, BuilderContract::MAX_OPS);
        $this->assertSame(50, BuilderContract::MAX_OUTLINE_PER_OP);
        $this->assertSame(400, BuilderContract::MAX_NEW_NODES);
        $this->assertSame(800, BuilderContract::MAX_NODES_AFTER);
        $this->assertSame(65536, BuilderContract::MAX_INPUT_BYTES);
        $this->assertSame(PageCreateBuilder::MAX_INPUT_BYTES, BuilderContract::MAX_INPUT_BYTES);
        $this->assertSame(1048576, BuilderContract::MAX_DOCUMENT_BYTES);
        $this->assertSame(4194304, BuilderContract::MAX_SNAPSHOT_BYTES);
        $this->assertSame(500, BuilderContract::MAX_STRUCTURE_NODES);
        $this->assertSame(65536, BuilderContract::MAX_STRUCTURE_BYTES);
        $this->assertSame(['set_text', 'insert', 'replace', 'remove', 'move'], BuilderContract::OPS);

        $edit = BuilderContract::pageEditInputSchema();
        $this->assertSame(['post_id', 'base_fingerprint', 'operations'], $edit['required']);
        $this->assertFalse($edit['additionalProperties']);
        $this->assertSame(['type' => 'integer', 'minimum' => 1], $edit['properties']['post_id']);
        $this->assertSame('^[0-9a-f]{64}$', $edit['properties']['base_fingerprint']['pattern']);
        $ops = $edit['properties']['operations'];
        $this->assertSame(1, $ops['minItems']);
        $this->assertSame(BuilderContract::MAX_OPS, $ops['maxItems']);

        $outlineItems = PageCreateBuilder::inputSchema()['properties']['outline']['items'];
        $variants     = [];
        foreach ($ops['items']['oneOf'] as $variant) {
            $this->assertFalse($variant['additionalProperties']);
            $props = $variant['properties'];
            $name  = $props['op']['const'];
            unset($props['op']);
            $keys = array_keys($props);
            sort($keys);
            $variants[] = $name . '(' . implode(',', $keys) . ')';
            foreach (['ref', 'after', 'before', 'into'] as $refField) {
                if (isset($props[$refField])) {
                    $this->assertSame(['type' => 'string', 'pattern' => '^[A-Za-z0-9_-]{1,32}$'], $props[$refField], $name . '.' . $refField);
                    $this->assertSame(BuilderContract::RE_REF, '/' . $props[$refField]['pattern'] . '/D');
                }
            }
            if (isset($props['outline'])) {
                $this->assertSame(1, $props['outline']['minItems'], $name);
                $this->assertSame(BuilderContract::MAX_OUTLINE_PER_OP, $props['outline']['maxItems'], $name);
                $this->assertSame($outlineItems, $props['outline']['items'], $name . ': the page-create outline grammar, unchanged');
            }
            if (isset($props['position'])) {
                $this->assertSame(['first', 'last'], $props['position']['enum']);
            }
        }
        sort($variants);
        $this->assertSame([
            'insert(after,outline)',
            'insert(before,outline)',
            'insert(into,outline,position)',
            'move(after,ref)',
            'move(before,ref)',
            'remove(ref)',
            'replace(outline,ref)',
            'set_text(field,ref,text)',
        ], $variants, 'one variant per operation shape: an insert names one anchor, only into takes a position');

        $structure = BuilderContract::pageStructureInputSchema();
        $this->assertSame(['post_id'], $structure['required']);
        $this->assertFalse($structure['additionalProperties']);
        $this->assertSame(['type' => 'integer', 'minimum' => 1, 'maximum' => 500], $structure['properties']['max_nodes']);
        $this->assertSame(['type' => 'string', 'pattern' => '^[A-Za-z0-9_-]{1,32}$'], $structure['properties']['node']);
    }

    public function test_kinds_are_the_outline_kinds_and_the_builder_kinds(): void
    {
        $outline = [];
        self::collectConsts(PageCreateBuilder::inputSchema()['properties']['outline']['items'], $outline);
        $outline = array_values(array_unique($outline));
        $this->assertNotEmpty($outline);

        $builderOnly = array_values(array_diff(BuilderContract::KINDS, $outline));
        $this->assertSame(['section', 'column', 'locked'], $builderOnly);
        $this->assertSame([], array_values(array_diff($outline, BuilderContract::KINDS)), 'every outline kind is a page kind');
        $this->assertContains('group', BuilderContract::KINDS);
        $this->assertSame(BuilderContract::KINDS, array_values(array_unique(BuilderContract::KINDS)));
    }

    // -------------------------------------------------------------------------
    // Ops cases
    // -------------------------------------------------------------------------

    public function test_ops_cases_are_well_formed(): void
    {
        $doc = self::opsCases();
        $this->assertSame(['note', 'page', 'cases'], array_keys($doc));
        $this->assertIsString($doc['note']);
        $this->assertNotSame('', $doc['note']);

        $page = $doc['page'];
        $this->assertIsInt($page['post_id']);
        $this->assertGreaterThanOrEqual(1, $page['post_id']);
        $this->assertContains($page['builder'], self::BUILDERS);
        $this->assertMatchesRegularExpression('/^[0-9a-f]{64}$/D', $page['base_fingerprint']);
        $refs   = [];
        $locked = 0;
        foreach ($page['nodes'] as $node) {
            $this->assertMatchesRegularExpression(BuilderContract::RE_REF, $node['ref']);
            $this->assertNotContains($node['ref'], $refs, 'page refs are unique');
            $this->assertTrue($node['parent'] === 'root' || in_array($node['parent'], $refs, true), $node['ref'] . ': parent precedes the node');
            $this->assertContains($node['kind'], BuilderContract::KINDS);
            if ($node['kind'] === 'locked') {
                ++$locked;
                $this->assertArrayNotHasKey('editable', $node, 'a locked node offers no field');
                $this->assertIsString($node['label']);
                $this->assertNotSame('', $node['label']);
            } else {
                $this->assertIsArray($node['editable']);
                $this->assertSame([], array_values(array_diff($node['editable'], BuilderContract::FIELDS)), $node['ref']);
            }
            $refs[] = $node['ref'];
        }
        $this->assertSame(1, $locked, 'the page has exactly one locked node');

        $names = [];
        foreach ($doc['cases'] as $case) {
            $name = (string) ($case['name'] ?? '');
            $this->assertNotSame('', $name);
            $this->assertNotContains($name, $names, 'case names are unique');
            $names[] = $name;
            $this->assertSame([], array_values(array_diff(array_keys($case), self::CASE_KEYS)), $name . ': unknown case field');
            $this->assertArrayHasKey($case['rule'], self::RULES, $name . ': unknown rule');
            $this->assertContains($case['agent'], self::AGENT_VERDICTS, $name);
            $this->assertContains($case['go'], self::GO_VERDICTS, $name);
            if (isset($case['note'])) {
                $this->assertIsString($case['note'], $name);
                $this->assertNotSame('', $case['note'], $name . ': a note says how the case was made');
            }
            if ($case['agent'] === 'ok') {
                $this->assertSame('accept', $case['go'], $name . ': what the agent accepts, the control plane accepts');
            }
            if (isset($case['builder'])) {
                $this->assertContains($case['builder'], self::BUILDERS, $name);
                $this->assertNotSame($page['builder'], $case['builder'], $name . ': builder is only for another builder');
            }

            $text = self::inputText($case);
            if (isset($case['generate'])) {
                $this->assertSame($case['generate']['to_bytes'], strlen($text), $name);
            }
            $input = json_decode($text, false, 32);
            $this->assertIsObject($input, $name . ': input is a JSON object');

            if ($case['go'] === 'accept') {
                $this->assertAcceptShape($name, $text, $input, $page);
            }
        }
    }

    public function test_ops_cases_cover_every_rule(): void
    {
        $seen = [];
        foreach (self::opsCases()['cases'] as $case) {
            $this->assertArrayHasKey($case['rule'], self::RULES, $case['name'] . ': unknown rule');
            $seen[$case['rule']][] = [$case['agent'], $case['go']];
        }
        foreach (self::RULES as $rule => $verdicts) {
            $this->assertArrayHasKey($rule, $seen, 'no case exercises ' . $rule);
            $this->assertContains($verdicts, $seen[$rule], $rule . ' has no case answering ' . implode('/', $verdicts));
        }
        foreach (self::BOUNDARY_RULES as $rule) {
            $this->assertContains(['ok', 'accept'], $seen[$rule], $rule . ' has no case at the limit');
        }
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * The input-only rules a case the control plane accepts must already meet.
     *
     * @param array<string,mixed> $page The page projection.
     */
    private function assertAcceptShape(string $name, string $text, object $input, array $page): void
    {
        $this->assertLessThanOrEqual(BuilderContract::MAX_INPUT_BYTES, strlen($text), $name);
        $this->assertSame(['post_id', 'base_fingerprint', 'operations'], array_keys(get_object_vars($input)), $name);
        $this->assertSame($page['post_id'], $input->post_id, $name);
        $this->assertSame($page['base_fingerprint'], $input->base_fingerprint, $name);
        $this->assertIsArray($input->operations, $name);
        $this->assertGreaterThanOrEqual(1, count($input->operations), $name);
        $this->assertLessThanOrEqual(BuilderContract::MAX_OPS, count($input->operations), $name);
        $targets = [];
        foreach ($input->operations as $op) {
            $this->assertContains($op->op, BuilderContract::OPS, $name);
            foreach (['ref', 'after', 'before', 'into'] as $field) {
                if (isset($op->{$field})) {
                    $this->assertMatchesRegularExpression(BuilderContract::RE_REF, $op->{$field}, $name);
                }
            }
            if (isset($op->ref)) {
                $this->assertNotContains($op->ref, $targets, $name . ': a ref is the target of one operation at most');
                $targets[] = $op->ref;
            }
            if (isset($op->outline)) {
                $this->assertGreaterThanOrEqual(1, count($op->outline), $name);
                $this->assertLessThanOrEqual(BuilderContract::MAX_OUTLINE_PER_OP, count($op->outline), $name);
            }
            if ($op->op === 'set_text') {
                $this->assertContains($op->field, BuilderContract::FIELDS, $name);
            }
        }
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
        $append = $case['generate']['append'];
        $to     = $case['generate']['to_bytes'];
        self::assertSame(' ', $append, 'generate appends JSON whitespace only');
        self::assertIsInt($to);
        self::assertGreaterThan(strlen($text), $to);

        return $text . str_repeat($append, $to - strlen($text));
    }

    /**
     * @return array<string,mixed>
     */
    private static function opsCases(): array
    {
        $raw = file_get_contents(self::FIXTURE_DIR . 'page-edit-ops-cases.json');
        self::assertIsString($raw, 'page-edit-ops-cases.json is missing');
        $doc = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        self::assertIsArray($doc);

        return $doc;
    }

    /**
     * Every "const" in a schema, depth first.
     *
     * @param mixed        $schema Schema fragment.
     * @param list<string> $out    Collected values.
     */
    private static function collectConsts($schema, array &$out): void
    {
        if (!is_array($schema)) {
            return;
        }
        if (isset($schema['const']) && is_string($schema['const'])) {
            $out[] = $schema['const'];
        }
        foreach ($schema as $child) {
            self::collectConsts($child, $out);
        }
    }

    /**
     * @param array<string,mixed> $schema Schema.
     */
    private static function encodeSchema(array $schema): string
    {
        return json_encode($schema, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
    }

    private function assertFixture(string $name, string $want): void
    {
        $path = self::FIXTURE_DIR . $name;
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents($path, $want), 'could not write ' . $name);
        }
        $got = file_get_contents($path);
        $this->assertIsString($got, $name . ' is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame($want, $got, $name . ' differs from what the agent produces now; regenerate with WPMGR_WRITE_FIXTURES=1');
    }
}
