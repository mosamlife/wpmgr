<?php
/**
 * A site that builds sections and columns refuses the nodes that carry a
 * design (containers_required), and builds a plain outline as it always did.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageCreate
 */
final class SectionsSiteRefusalTest extends TestCase
{
    private const REQUEST = 'a3d4c1b2-0000-4000-8000-000000000001';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        // The sanitiser leaves a text with no markup unchanged.
        Functions\when('wp_kses_post')->returnArg();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /** @return array<string,mixed> */
    private static function section(): array
    {
        return [
            'type'     => 'section',
            'anchor'   => 'intro',
            'children' => [['type' => 'paragraph', 'text' => 'Welcome to the studio.']],
        ];
    }

    /** @return array<string,mixed> */
    private static function pattern(): array
    {
        return ['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [['type' => 'heading', 'level' => 2, 'text' => 'Ready?']]];
    }

    /**
     * @param list<array<string,mixed>> $outline Outline nodes.
     * @return array<string,mixed>
     */
    private static function map(array $outline, bool $containers): array
    {
        return ElementorClassicMapper::map(['outline' => $outline], new IdSeed(self::REQUEST), [], $containers);
    }

    /**
     * The site facts of a page-create precheck, with the container feature as given.
     *
     * @param bool|null $containers Whether the site builds containers; null when it cannot say.
     * @return array<string,mixed>
     */
    private static function facts(?bool $containers): array
    {
        return [
            'active'             => true,
            'version'            => '3.35.9',
            'classic_in_range'   => true,
            'containers'         => $containers,
            'atomic'             => false,
            'role_excluded'      => false,
            'post_types'         => ['page', 'post'],
            'active_kit_id'      => 4,
            'pro_active'         => false,
            'active_breakpoints' => ['mobile', 'tablet'],
        ];
    }

    /**
     * @param bool|null $containers The container feature.
     * @return array<string,mixed>
     */
    private static function precheck(?bool $containers): array
    {
        $api     = new FakeElementorApi();
        $adapter = new ElementorAdapter($api, 2);
        $spec    = [
            'post_type'        => 'page',
            'editor'           => 'builder:elementor',
            'elementor_format' => 'classic',
            'title'            => 'Launch page',
            'outline_version'  => 3,
            'outline'          => [self::section()],
        ];

        return BuilderPageCreate::precheck($spec, $adapter, self::facts($containers), [], self::REQUEST);
    }

    public function testSectionsSiteRefusesDesignNodes(): void
    {
        // A section at the top of the outline.
        $refused = self::map([self::section()], false);
        $this->assertSame('containers_required', $refused['code'] ?? null);
        $this->assertStringStartsWith('outline[0]: ', $refused['detail'] ?? '');
        $this->assertArrayNotHasKey('tree', $refused);

        // A pattern inside a group, further down the outline: the path names it.
        $refused = self::map([
            ['type' => 'paragraph', 'text' => 'Plain.'],
            ['type' => 'group', 'children' => [['type' => 'paragraph', 'text' => 'Plain.'], self::pattern()]],
        ], false);
        $this->assertSame('containers_required', $refused['code'] ?? null);
        $this->assertStringStartsWith('outline[1].children[1]: ', $refused['detail'] ?? '');

        // The nodes a page edit adds are judged the same way, in every place.
        foreach ([ElementorClassicMapper::CONTEXT_TOP, ElementorClassicMapper::CONTEXT_GROUP, ElementorClassicMapper::CONTEXT_INNER] as $context) {
            $fragment = ElementorClassicMapper::mapFragment([self::pattern()], new IdSeed(self::REQUEST), [], false, $context, 'operations[2].');
            $this->assertSame('containers_required', $fragment['code'] ?? null, $context);
            $this->assertStringStartsWith('operations[2].outline[0]: ', $fragment['detail'] ?? '', $context);
        }

        // The page-create check refuses before anything is built, with the envelope's failures.
        foreach ([false, null] as $containers) {
            $answer = self::precheck($containers);
            $this->assertSame('containers_required', $answer['refusal']['code'] ?? null);
            $this->assertSame(false, $answer['refusal']['ok'] ?? null);
            $this->assertSame('refused', $answer['refusal']['outcome'] ?? null);
            $this->assertSame('outline_version', $answer['refusal']['failures'][0]['key'] ?? null);
            $this->assertNotSame([], $answer['refusal']['failures'] ?? []);
            $this->assertSame([], $answer['tree']);
            $this->assertSame('', $answer['preview_digest']);
        }
    }

    public function testAPlainOutlineStillBuildsOnASectionsSite(): void
    {
        $plain = [
            ['type' => 'heading', 'level' => 2, 'text' => 'Our services'],
            ['type' => 'paragraph', 'text' => 'Welcome.'],
            ['type' => 'group', 'children' => [['type' => 'paragraph', 'text' => 'Inside.']]],
            ['type' => 'columns', 'columns' => [[['type' => 'paragraph', 'text' => 'One.']], [['type' => 'paragraph', 'text' => 'Two.']]]],
        ];

        $built = self::map($plain, false);

        $this->assertArrayNotHasKey('code', $built);
        $this->assertSame(['section', 'section', 'section'], array_column($built['tree'] ?? [], 'elType'));
    }

    public function testASiteWithContainersIsNotRefusedForThem(): void
    {
        $this->assertNotSame('containers_required', self::map([self::section()], true)['code'] ?? null);

        $answer = self::precheck(true);
        $this->assertNotSame('containers_required', $answer['refusal']['code'] ?? null);
    }
}
