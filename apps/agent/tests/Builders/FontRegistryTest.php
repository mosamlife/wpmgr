<?php
/**
 * The design's fonts against Elementor's font registry: font_unknown with up
 * to five close names, `system` sets no family, and the closed family pattern
 * is applied before the registry is read and before a name can reach a style.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\ElementorRuntime;
use WPMgr\Agent\Abilities\Builders\FontRegistry;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * FontRegistry and ElementorRuntime::fontFamilies().
 */
final class FontRegistryTest extends TestCase
{
    /** The registry the shared case table's font row gives the site. */
    private const SITE = ['Manrope', 'Inter', 'Lora', 'Roboto', 'Source Sans 3'];

    /** How many times the registry was read. */
    private int $reads = 0;

    protected function set_up(): void
    {
        parent::set_up();
        $this->reads                         = 0;
        FontRegistryPluginStandIn::$instance = null;
        FontRegistryFontsStandIn::$fonts     = null;
    }

    protected function tear_down(): void
    {
        FontRegistryPluginStandIn::$instance = null;
        FontRegistryFontsStandIn::$fonts     = null;
        parent::tear_down();
    }

    public function testRegistryFamiliesAndSystemPass(): void
    {
        $this->assertNull(FontRegistry::refusal(['heading' => 'Manrope', 'body' => 'Source Sans 3'], $this->registry(self::SITE)));
        $this->assertSame(1, $this->reads, 'the registry is read once for both roles');

        $this->reads = 0;
        $this->assertNull(FontRegistry::refusal(['heading' => 'Lora', 'body' => 'system'], $this->registry(self::SITE)));
        $this->assertNull(FontRegistry::refusal(['heading' => 'system', 'body' => 'Inter'], $this->registry(self::SITE)));
        $this->assertSame(2, $this->reads);

        // system sets no family: with no family named the registry is never
        // read, so a site whose registry cannot be read still passes.
        $this->reads = 0;
        $this->assertNull(FontRegistry::refusal(['heading' => 'system', 'body' => 'system'], $this->registry(null)));
        $this->assertSame(0, $this->reads);

        // Entries that are not strings are skipped; the rest still count.
        $this->assertNull(FontRegistry::refusal(['heading' => 'Manrope', 'body' => 'Manrope'], $this->registry([7, null, ['Manrope'], 'Manrope'])));

        // The literal is system exactly; any other spelling is a family name.
        $refusal = FontRegistry::refusal(['heading' => 'System', 'body' => 'system'], $this->registry(self::SITE));
        $this->assertSame(FontRegistry::CODE_UNKNOWN, $refusal['code'] ?? null);
    }

    public function testUnknownFamilyIsFontUnknownWithCloseNames(): void
    {
        $refusal = FontRegistry::refusal(['heading' => 'Zzyzx Display', 'body' => 'Source Sans 3'], $this->registry(self::SITE));
        $this->assertNotNull($refusal);
        $this->assertSame('font_unknown', $refusal['code']);
        $this->assertSame([['path' => 'design.fonts', 'key' => 'heading', 'reason' => FontRegistry::REASON_UNKNOWN]], $refusal['failures'], 'no close name, so no allowed list');
        $this->assertStringContainsString('design.fonts.heading: ' . FontRegistry::REASON_UNKNOWN, $refusal['detail']);
        $this->assertStringNotContainsString('Zzyzx', $refusal['detail'], 'the message never echoes the input');

        // Both roles are judged, in role order.
        $refusal = FontRegistry::refusal(['heading' => 'Zzyzx Display', 'body' => 'Qqq'], $this->registry(self::SITE));
        $this->assertSame(['heading', 'body'], array_column($refusal['failures'] ?? [], 'key'));

        // Close names: a case slip, a missing word, a typo, a family inside
        // the name asked for.
        $close = [
            ['manrope', ['Manrope']],
            ['Source Sans', ['Source Sans 3']],
            ['Robotto', ['Roboto']],
            ['Intre', ['Inter']],
            ['Lora Serif', ['Lora']],
        ];
        foreach ($close as [$asked, $expected]) {
            $refusal = FontRegistry::refusal(['heading' => 'Manrope', 'body' => $asked], $this->registry(self::SITE));
            $this->assertSame(
                [['path' => 'design.fonts', 'key' => 'body', 'reason' => FontRegistry::REASON_UNKNOWN, 'allowed' => $expected]],
                $refusal['failures'] ?? null,
                $asked
            );
            $this->assertStringContainsString('(allowed: ' . implode(', ', $expected) . ')', $refusal['detail']);
        }

        // At most five, closest first and then by name. A name the closed
        // pattern refuses, or one too long to list whole, is never offered.
        $registry = [
            'Noto Sans TC', 'Noto Sans SC', 'Noto Sans KR', 'Noto Sans JP', 'Noto Sans HK', 'Noto Sans', 'Noto Sans',
            'Noto Sans Xi', 'Noto Serif', 'Noto-Sans X', 'Noto Sans X;', 'Noto Sans X Display Extra Condensed Rounded',
        ];
        $this->assertSame(
            ['Noto Sans Xi', 'Noto Sans', 'Noto Sans HK', 'Noto Sans JP', 'Noto Sans KR'],
            FontRegistry::closeFamilies('Noto Sans X', $registry)
        );
        $this->assertSame([], FontRegistry::closeFamilies('Zzyzx Display', self::SITE));
        $this->assertSame(5, FontRegistry::MAX_CLOSE);
    }

    public function testClosedPatternRunsBeforeTheRegistry(): void
    {
        $hostile = ['', 'Inter;', 'Inter"', "Inter'", 'Inter\\', "Inter\n", 'Inter</style>', 'Inter}body{color:red', "\u{00CF}nter", str_repeat('A', 61)];
        foreach (array_merge($hostile, [7, null, true, ['Inter']]) as $name) {
            // The registry even holds the exact string: it is never asked.
            $this->reads = 0;
            $registry    = array_merge(self::SITE, array_filter($hostile, 'is_string'));
            $refusal     = FontRegistry::refusal(['heading' => $name, 'body' => 'Inter'], $this->registry($registry));
            $this->assertSame('bad_input', $refusal['code'] ?? null, var_export($name, true));
            $this->assertSame([['path' => 'design.fonts', 'key' => 'heading', 'reason' => FontRegistry::REASON_PATTERN]], $refusal['failures']);
            $this->assertSame(0, $this->reads, var_export($name, true));
        }

        // Sixty characters is the longest family name.
        $this->assertNull(FontRegistry::refusal(['heading' => str_repeat('A', 60), 'body' => 'system'], $this->registry([str_repeat('A', 60)])));

        // A fonts object that is missing, or misses a role.
        $this->reads = 0;
        foreach (['Manrope', null, 7] as $fonts) {
            $refusal = FontRegistry::refusal($fonts, $this->registry(self::SITE));
            $this->assertSame('bad_input', $refusal['code'] ?? null);
            $this->assertSame([['path' => 'design', 'key' => 'fonts', 'reason' => FontRegistry::REASON_OBJECT]], $refusal['failures']);
        }
        $refusal = FontRegistry::refusal(['heading' => 'Manrope'], $this->registry(self::SITE));
        $this->assertSame([['path' => 'design.fonts', 'key' => 'body', 'reason' => FontRegistry::REASON_PATTERN]], $refusal['failures'] ?? null);
        $refusal = FontRegistry::refusal(['Manrope', 'Inter'], $this->registry(self::SITE));
        $this->assertSame(['heading', 'body'], array_column($refusal['failures'] ?? [], 'key'));
        $this->assertSame(0, $this->reads);
    }

    public function testRegistryThatCannotBeReadRefuses(): void
    {
        $refusal = FontRegistry::refusal(['heading' => 'Manrope', 'body' => 'system'], $this->registry(null));
        $this->assertSame(['code' => 'builder_not_available', 'detail' => 'font_registry_unavailable', 'failures' => []], $refusal);
        $this->assertSame(1, $this->reads);
    }

    public function testRuntimeReadsElementorsFontRegistry(): void
    {
        $runtime = new ElementorRuntime(FontRegistryPluginStandIn::class, 'Wpmgr_Test_No_Such_Utils', 'WPMGR_TEST_NO_SUCH_VERSION', FontRegistryFontsStandIn::class);
        FontRegistryFontsStandIn::$fonts = ['Arial' => 'system', 'Manrope' => 'googlefonts', 'Source Sans 3' => 'googlefonts', 1942 => 'googlefonts', '' => 'system'];

        // Elementor is not loaded: its registry is not asked.
        $this->assertNull($runtime->fontFamilies());

        FontRegistryPluginStandIn::$instance = new FontRegistryPluginStandIn();
        $this->assertSame(['Arial', 'Manrope', 'Source Sans 3', '1942'], $runtime->fontFamilies(), 'the family names, as Elementor keys them');

        $unreadable = [
            'Manrope',
            null,
            static fn () => throw new \RuntimeException('fonts failed'),
            static fn () => throw new \TypeError('fonts failed'),
            array_fill_keys(array_map(static fn (int $i): string => 'Font ' . $i, range(1, ElementorRuntime::MAX_FONT_FAMILIES + 1)), 'googlefonts'),
        ];
        foreach ($unreadable as $i => $answer) {
            FontRegistryFontsStandIn::$fonts = $answer;
            $this->assertNull($runtime->fontFamilies(), (string) $i);
        }

        // A registry of exactly the cap is read whole.
        FontRegistryFontsStandIn::$fonts = array_fill_keys(array_map(static fn (int $i): string => 'Font ' . $i, range(1, ElementorRuntime::MAX_FONT_FAMILIES)), 'googlefonts');
        $this->assertCount(ElementorRuntime::MAX_FONT_FAMILIES, $runtime->fontFamilies() ?? []);

        // No fonts class: null.
        $missing = new ElementorRuntime(FontRegistryPluginStandIn::class, 'Wpmgr_Test_No_Such_Utils', 'WPMGR_TEST_NO_SUCH_VERSION', 'Wpmgr_Test_No_Such_Fonts');
        $this->assertNull($missing->fontFamilies());
        $this->assertSame('Elementor\\Fonts', ElementorRuntime::FONTS_CLASS);

        // The test double answers what a test sets, and nothing when unloaded.
        $fake = new FakeElementorApi();
        $this->assertSame(self::SITE, $fake->fontFamilies());
        $fake->loaded = false;
        $this->assertNull($fake->fontFamilies());
    }

    /**
     * A registry reader that counts its reads.
     *
     * @param list<mixed>|null $families What the registry answers.
     * @return \Closure(): (list<mixed>|null)
     */
    private function registry(?array $families): \Closure
    {
        return function () use ($families): ?array {
            $this->reads++;

            return $families;
        };
    }
}

/**
 * Stands in for Elementor's main plugin class: only its instance matters here.
 */
final class FontRegistryPluginStandIn
{
    /** @var object|null */
    public static ?object $instance = null;
}

/**
 * Stands in for Elementor's fonts class: get_fonts() answers $fonts, or what
 * $fonts answers when it is a closure.
 */
final class FontRegistryFontsStandIn
{
    /** @var mixed */
    public static mixed $fonts = null;

    /**
     * @return mixed
     */
    public static function get_fonts()
    {
        return self::$fonts instanceof \Closure ? (self::$fonts)() : self::$fonts;
    }
}
