<?php
/**
 * Projection: the builder-neutral page structure, with kinds closed to the
 * contract, locked labels from WPMgr's map only, and the node and byte caps.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\Projection;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\Projection
 */
final class ProjectionTest extends TestCase
{
    private const LABELS = [
        'form'                    => 'Form',
        'html'                    => 'Custom code',
        Projection::FALLBACK_LABEL => 'Element WPMgr does not edit',
    ];

    public function test_kinds_are_closed(): void
    {
        $p     = new Projection(self::LABELS);
        $kinds = array_values(array_diff(BuilderContract::KINDS, ['locked']));
        foreach ($kinds as $i => $kind) {
            $p->add('n' . $i, Projection::ROOT, $kind, $kind === 'heading' ? 2 : null, [], []);
        }
        $p->addLocked('x1', Projection::ROOT, 'form');

        $out = $p->toArray(BuilderContract::MAX_STRUCTURE_NODES);
        $this->assertSame(array_merge($kinds, ['locked']), array_column($out['nodes'], 'kind'));

        $refused = ['locked', 'widget', 'Heading', 'heading ', 'text-editor', 'container', ''];
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
}
