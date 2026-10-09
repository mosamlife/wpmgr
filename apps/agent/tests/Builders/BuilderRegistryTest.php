<?php
/**
 * BuilderRegistry and AdapterStatus: which builder a page-create editor value
 * may reach, and the tested version range that data can only narrow.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\AdapterStatus;
use WPMgr\Agent\Abilities\Builders\BuilderAdapter;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\BuilderRegistry;
use WPMgr\Agent\Abilities\Builders\DocumentDescriptor;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\NativeDocument;
use WPMgr\Agent\Abilities\Builders\Projection;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderRegistry
 * @covers \WPMgr\Agent\Abilities\Builders\AdapterStatus
 */
final class BuilderRegistryTest extends TestCase
{
    private const NOT_COMPILED = ['code' => 'builder_not_available', 'detail' => 'not_compiled'];

    private const UNVERIFIED = ['code' => 'builder_not_available', 'detail' => 'version_unverified'];

    public function test_unknown_builder_id_refused_even_when_enabled(): void
    {
        // Listed in the entry and present in the compiled set: still refused.
        $limits = self::limits('{"builders_enabled":["elementor","evil","Elementor"]}');
        $seam   = ['evil' => self::adapter('evil'), 'elementor' => self::adapter('elementor')];

        $editors = [
            'builder:evil',
            'builder:Elementor',
            "builder:elementor\n",
            'builder:elementor ',
            ' builder:elementor',
            'builder:elementor:x',
            'builder:elem-entor',
            'builder:',
            'builder',
            'elementor',
            'wordpress_blocks',
            '',
        ];
        foreach ($editors as $editor) {
            $result = BuilderRegistry::resolve($editor, $limits, $seam);
            $this->assertSame('bad_input', $result['code'] ?? null, var_export($editor, true));
            $this->assertArrayNotHasKey('adapter', $result);
            $this->assertStringNotContainsString('evil', (string) ($result['detail'] ?? ''));
        }
    }

    public function test_known_id_not_compiled_refused(): void
    {
        $everyId = self::limits((string) json_encode(['builders_enabled' => BuilderRegistry::IDS]));

        // Only elementor may become compiled into the agent in this slice.
        foreach (array_diff(BuilderRegistry::IDS, ['elementor']) as $id) {
            $this->assertSame(self::NOT_COMPILED, BuilderRegistry::resolve('builder:' . $id, $everyId), $id);
        }

        // With the compiled set standing in: an id outside it is refused ...
        $seam = ['elementor' => self::adapter('elementor')];
        $this->assertSame(self::NOT_COMPILED, BuilderRegistry::resolve('builder:bricks', $everyId, $seam));
        $this->assertSame(self::NOT_COMPILED, BuilderRegistry::resolve('builder:elementor', $everyId, []));

        // ... and so is an adapter filed under an id it does not answer to.
        $this->assertSame(
            self::NOT_COMPILED,
            BuilderRegistry::resolve('builder:bricks', $everyId, ['bricks' => self::adapter('elementor')])
        );
    }

    public function test_builders_enabled_required_and_must_name_the_id(): void
    {
        $seam = ['elementor' => self::adapter('elementor')];

        $nonList                   = new \stdClass();
        $nonList->builders_enabled = [1 => 'elementor'];

        $cases = [
            'limits null'           => null,
            'limits a list'         => [],
            'limits an array'       => ['builders_enabled' => ['elementor']],
            'limits a string'       => '{"builders_enabled":["elementor"]}',
            'key missing'           => self::limits('{}'),
            'key null'              => self::limits('{"builders_enabled":null}'),
            'key a string'          => self::limits('{"builders_enabled":"elementor"}'),
            'key an object'         => self::limits('{"builders_enabled":{"0":"elementor"}}'),
            'key an object by name' => self::limits('{"builders_enabled":{"elementor":true}}'),
            'key not a list'        => $nonList,
            'empty list'            => self::limits('{"builders_enabled":[]}'),
            'other ids'             => self::limits('{"builders_enabled":["beaver","bricks"]}'),
            'other case'            => self::limits('{"builders_enabled":["Elementor"]}'),
            'padded'                => self::limits('{"builders_enabled":["elementor "," elementor"]}'),
            'nested'                => self::limits('{"builders_enabled":[["elementor"]]}'),
            'a non-string member'   => self::limits('{"builders_enabled":["elementor",7]}'),
        ];
        foreach ($cases as $name => $limits) {
            $result = BuilderRegistry::resolve('builder:elementor', $limits, $seam);
            $this->assertSame('builder_not_enabled', $result['code'] ?? null, $name);
            $this->assertArrayNotHasKey('adapter', $result, $name);
        }

        $result = BuilderRegistry::resolve('builder:elementor', self::limits('{"builders_enabled":["bricks","elementor"]}'), $seam);
        $this->assertSame($seam['elementor'], $result['adapter'] ?? null);
    }

    public function test_compiled_and_enabled_resolves(): void
    {
        $seam = [];
        foreach (BuilderRegistry::IDS as $id) {
            $seam[$id] = self::adapter($id);
        }
        $limits = self::limits((string) json_encode(['builders_enabled' => BuilderRegistry::IDS]));

        foreach (BuilderRegistry::IDS as $id) {
            $this->assertSame(['adapter' => $seam[$id]], BuilderRegistry::resolve('builder:' . $id, $limits, $seam), $id);
        }
        $this->assertSame(
            ['elementor', 'beaver', 'wpbakery', 'divi5', 'bricks', 'breakdance', 'oxygen6'],
            BuilderRegistry::IDS
        );
    }

    public function test_elementor_compiled_and_enabled_resolves(): void
    {
        // No seam: the adapter compiled into the agent.
        $result  = BuilderRegistry::resolve('builder:elementor', self::limits('{"builders_enabled":["elementor"]}'));
        $adapter = $result['adapter'] ?? null;
        $this->assertInstanceOf(ElementorAdapter::class, $adapter);
        $this->assertSame('elementor', $adapter->id());
        $this->assertSame(['adapter'], array_keys($result));

        // Compiled, but the entry still has to enable it.
        $this->assertSame('builder_not_enabled', BuilderRegistry::resolve('builder:elementor', self::limits('{"builders_enabled":["bricks"]}'))['code'] ?? null);
        $this->assertSame('builder_not_enabled', BuilderRegistry::resolve('builder:elementor', self::limits('{}'))['code'] ?? null);

        // The adapter declares every page-edit operation and node kind, and the mapper's allowlist and leaf rules.
        $this->assertSame(BuilderContract::OPS, $adapter->capabilities()['operations']);
        $this->assertSame(BuilderContract::KINDS, $adapter->capabilities()['node_kinds']);
        $this->assertSame(ElementorClassicMapper::ALLOWED_KEYS, $adapter->allowedKeys());
        $this->assertSame(['refuse_braces' => false, 'forbidden' => ['[elementor-tag']], $adapter->leafRules());
        $this->assertSame(ElementorDocument::DESCRIPTOR_KEYS, $adapter->descriptor()->exactKeys);
    }

    public function test_entry_range_can_only_narrow(): void
    {
        $limits  = self::limits('{"builders_enabled":["elementor"]}');
        $tested  = new AdapterStatus(true, '4.3.4', '3.20.0', '4.3.99');
        $adapter = BuilderRegistry::resolve('builder:elementor', $limits, ['elementor' => self::adapter('elementor', $tested)])['adapter'] ?? null;
        $this->assertInstanceOf(BuilderAdapter::class, $adapter);
        $status = $adapter->status();
        $this->assertNull($status->refusal());

        // An entry range wider than the tested one changes nothing.
        $wide = $status->narrowed('1.0.0', '9.9.9');
        $this->assertSame(['3.20.0', '4.3.99'], [$wide->testedMin, $wide->testedMax]);

        // A version past either end stays refused whatever the entry admits.
        $newer = new AdapterStatus(true, '4.4.0', '3.20.0', '4.3.99');
        $this->assertSame(self::UNVERIFIED, $newer->refusal());
        $this->assertSame(self::UNVERIFIED, $newer->narrowed(null, '4.9.0')->refusal());
        $this->assertSame(self::UNVERIFIED, $newer->narrowed('3.0.0', '5.0.0')->refusal());
        $older = new AdapterStatus(true, '3.19.9', '3.20.0', '4.3.99');
        $this->assertSame(self::UNVERIFIED, $older->narrowed('3.0.0', null)->refusal());

        // A narrower entry range does take effect, from either end.
        $narrow = $status->narrowed('3.30.0', '4.3.0');
        $this->assertSame(['3.30.0', '4.3.0'], [$narrow->testedMin, $narrow->testedMax]);
        $this->assertSame(self::UNVERIFIED, $narrow->refusal());
        $this->assertSame(self::UNVERIFIED, $status->narrowed('4.3.5', null)->refusal());
        $this->assertNull($status->narrowed('4.0.0', '4.3.10')->refusal());
        $this->assertSame(self::UNVERIFIED, $status->narrowed('4.3.50', '4.3.40')->refusal(), 'an empty range admits nothing');

        // Ordering is VersionCompare's: 3.100.0 is past 3.99.0.
        $this->assertSame(self::UNVERIFIED, (new AdapterStatus(true, '3.100.0', '3.20.0', '3.99.0'))->refusal());

        // Narrowing returns a new status; the tested one is unchanged.
        $this->assertSame(['3.20.0', '4.3.99'], [$status->testedMin, $status->testedMax]);
    }

    public function test_status_refusal_names_the_first_problem(): void
    {
        $this->assertSame(
            ['code' => 'builder_not_available', 'detail' => 'elementor_missing'],
            (new AdapterStatus(false, null, '3.20.0', '4.3.99', ['elementor_missing']))->refusal()
        );
        $this->assertSame(
            ['code' => 'builder_not_available', 'detail' => 'builder_missing'],
            (new AdapterStatus(false, '4.3.4', '3.20.0', '4.3.99'))->refusal()
        );
        $this->assertSame(self::UNVERIFIED, (new AdapterStatus(true, null, '3.20.0', '4.3.99'))->refusal());
        $this->assertSame(self::UNVERIFIED, (new AdapterStatus(true, '', '3.20.0', '4.3.99'))->refusal());
        $this->assertSame(
            ['code' => 'builder_not_available', 'detail' => 'role_excluded'],
            (new AdapterStatus(true, '4.3.4', '3.20.0', '4.3.99', ['role_excluded', 'post_type_not_supported']))->refusal()
        );
        $this->assertNull((new AdapterStatus(true, '3.20.0', '3.20.0', '4.3.99'))->refusal());
        $this->assertNull((new AdapterStatus(true, '4.3.99', '3.20.0', '4.3.99'))->refusal());

        // A reason is a token from WPMgr's vocabulary, never free text.
        $this->expectException(\InvalidArgumentException::class);
        new AdapterStatus(true, '4.3.4', '3.20.0', '4.3.99', ['Elementor <b>Pro</b> is missing']);
    }

    /**
     * Entry limits decoded the way the agent decodes a catalogue entry.
     *
     * @param string $json Limits JSON.
     * @return mixed
     */
    private static function limits(string $json): mixed
    {
        return json_decode($json, false, 32, JSON_THROW_ON_ERROR);
    }

    /**
     * A stand-in adapter that answers to $id.
     *
     * @param string             $id     Adapter id.
     * @param AdapterStatus|null $status Status, default active and in range.
     * @return BuilderAdapter
     */
    private static function adapter(string $id, ?AdapterStatus $status = null): BuilderAdapter
    {
        return new class ($id, $status ?? new AdapterStatus(true, '1.0.0', '1.0.0', '1.9.9')) implements BuilderAdapter {
            public function __construct(private string $adapterId, private AdapterStatus $adapterStatus)
            {
            }

            public function id(): string
            {
                return $this->adapterId;
            }

            public function status(): AdapterStatus
            {
                return $this->adapterStatus;
            }

            public function owns(int $postId): ?bool
            {
                return null;
            }

            public function descriptor(): DocumentDescriptor
            {
                return new DocumentDescriptor(['_stand_in_data']);
            }

            public function capabilities(): array
            {
                return ['operations' => [], 'node_kinds' => []];
            }

            public function allowedKeys(): array
            {
                return [];
            }

            public function leafRules(): array
            {
                return ['refuse_braces' => true, 'forbidden' => []];
            }

            public function buildCreate(array $spec, IdSeed $ids, array $mediaById): array
            {
                return ['code' => 'node_not_supported_by_builder', 'detail' => 'outline[0]'];
            }

            public function project(array $tree): Projection
            {
                return new Projection([Projection::FALLBACK_LABEL => 'Element']);
            }

            public function planEdit(int $postId, array $ops, IdSeed $ids, array $mediaById): array
            {
                return ['code' => 'op_not_supported_by_builder', 'detail' => 'set_text', 'op_index' => 0];
            }

            public function write(int $postId, NativeDocument $doc): array
            {
                return ['ok' => false, 'code' => 'builder_save_refused'];
            }

            public function verifyCreated(int $postId, NativeDocument $doc, int $principal, string $requestId): ?string
            {
                return 'not_built';
            }

            public function afterRestore(int $postId): void
            {
            }
        };
    }
}
