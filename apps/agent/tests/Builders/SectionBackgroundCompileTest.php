<?php
/**
 * A section's background image: the library image placed at its focal point,
 * the ink overlay at the strength the section names, the inverse tone it
 * forces, and background_not_image for an attachment that is not an image.
 * Compiled byte for byte as the frozen hero probe stores it.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler
 */
final class SectionBackgroundCompileTest extends TestCase
{
    /** The attachment the hero probe shows, and the address its tree carries. */
    private const IMAGE_ID  = 5;
    private const IMAGE_URL = 'https://example.com/wp-content/uploads/2026/10/hero-1600x900.jpg';

    /** The keys of the hero probe that belong to its height, not to its background. */
    private const HEIGHT_KEYS = ['min_height', 'min_height_mobile', 'flex_justify_content'];

    /** Where the shared outline v3 case table lives, from this file. */
    private const CASES = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

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

    /** @return array<string,mixed> A valid design whose ink is the hero probe's colour. */
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
            'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ];
    }

    /**
     * A resolved image fact row, as the page-create precheck hands it on.
     *
     * @param array<string,mixed> $override Keys to replace.
     * @return array<string,mixed>
     */
    private static function fact(array $override = []): array
    {
        return array_replace([
            'id'           => self::IMAGE_ID,
            'url'          => self::IMAGE_URL,
            'filename'     => 'hero-1600x900.jpg',
            'mime'         => 'image/jpeg',
            'width'        => 1600,
            'height'       => 900,
            'modified_gmt' => '2026-10-01 09:00:00',
            'alt'          => '',
        ], $override);
    }

    /**
     * @param array<mixed> $media Resolved images by attachment id.
     * @return ElementorSectionCompiler
     */
    private static function compiler(array $media = [self::IMAGE_ID => null]): ElementorSectionCompiler
    {
        if (array_key_exists(self::IMAGE_ID, $media) && $media[self::IMAGE_ID] === null) {
            $media[self::IMAGE_ID] = self::fact();
        }

        return new ElementorSectionCompiler(DesignTokens::fromDesign(self::design()), ['mobile', 'tablet'], $media);
    }

    /**
     * The hero section of the layout-hero-background probe, without its height.
     *
     * @param array<string,mixed> $background The background.
     * @param array<string,mixed> $override   Section keys to replace.
     * @return array<string,mixed>
     */
    private static function section(array $background, array $override = []): array
    {
        return array_replace([
            'type'       => 'section',
            'anchor'     => 'hero',
            'width'      => 'full',
            'spacing'    => 'xl',
            'background' => $background,
            'children'   => [['type' => 'heading', 'level' => 1, 'text' => 'Launch week']],
        ], $override);
    }

    /**
     * The hero probe's settings without its height keys, with the overlay
     * opacity given.
     *
     * @param string $opacity The overlay opacity size.
     * @return array<string,mixed>
     */
    private static function probe(string $opacity): array
    {
        $settings = array_diff_key(RoundTripProbes::settings('layout-hero-background'), array_flip(self::HEIGHT_KEYS));
        $settings['background_overlay_opacity']['size'] = $opacity;

        return $settings;
    }

    /**
     * @param array<string,mixed> $settings Settings.
     * @return string
     */
    private static function bytes(array $settings): string
    {
        return json_encode($settings, JSON_THROW_ON_ERROR);
    }

    // -------------------------------------------------------------------------
    // The frozen hero probe
    // -------------------------------------------------------------------------

    public function test_a_soft_overlay_is_the_hero_probes_background(): void
    {
        $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID, 'overlay' => 'soft', 'focal' => 'center']));

        $this->assertSame(self::bytes(self::probe('0.45')), self::bytes($settings));
    }

    public function test_section_background_strong(): void
    {
        $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID, 'overlay' => 'strong', 'focal' => 'center']));

        $this->assertSame(self::bytes(self::probe('0.65')), self::bytes($settings));
        $this->assertSame(['unit' => 'px', 'size' => '0.65'], $settings['background_overlay_opacity']);
    }

    public function test_a_background_that_names_no_overlay_or_focal_is_strong_and_centred(): void
    {
        $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID]));

        $this->assertSame(self::bytes(self::probe('0.65')), self::bytes($settings));
        $this->assertSame('strong', ElementorSectionCompiler::DEFAULT_OVERLAY);
        $this->assertSame('center', ElementorSectionCompiler::DEFAULT_FOCAL);
    }

    public function test_no_overlay_writes_the_image_and_no_overlay_key(): void
    {
        $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID, 'overlay' => 'none']));

        $expected = array_diff_key(self::probe('0.65'), array_flip(['background_overlay_background', 'background_overlay_color', 'background_overlay_opacity']));
        $this->assertSame(self::bytes($expected), self::bytes($settings));
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function focals(): array
    {
        return [
            'center' => ['center', 'center center'],
            'top'    => ['top', 'top center'],
            'bottom' => ['bottom', 'bottom center'],
        ];
    }

    /**
     * @dataProvider focals
     */
    public function test_the_focal_point_is_the_background_position(string $focal, string $position): void
    {
        $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID, 'focal' => $focal]));

        $this->assertSame($position, $settings['background_position']);
    }

    public function test_every_focal_point_the_grammar_accepts_has_a_position(): void
    {
        foreach (OutlineV3Schema::FOCALS as $focal) {
            $settings = self::compiler()->settings(self::section(['attachment_id' => self::IMAGE_ID, 'focal' => $focal]));
            $this->assertMatchesRegularExpression('/^(center|top|bottom) center\z/', $settings['background_position'], $focal);
        }
    }

    public function test_the_image_is_the_resolved_attachment_with_its_id_as_a_numeric_string(): void
    {
        $url      = 'https://example.com/wp-content/uploads/2026/10/other-1024x576.webp';
        $media    = [42 => self::fact(['id' => 42, 'url' => $url, 'mime' => 'image/webp'])];
        $settings = self::compiler($media)->settings(self::section(['attachment_id' => 42]));

        $this->assertSame(['id' => '42', 'url' => $url], $settings['background_image']);
    }

    // -------------------------------------------------------------------------
    // The inverse tone a background forces
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string}>
     */
    public static function tones(): array
    {
        $out = [];
        foreach (OutlineV3Schema::TONES as $tone) {
            $out[$tone] = [$tone];
        }

        return $out;
    }

    /**
     * @dataProvider tones
     */
    public function test_a_background_forces_the_inverse_tone(string $tone): void
    {
        $compiler = self::compiler();
        $section  = self::section(['attachment_id' => self::IMAGE_ID], ['tone' => $tone]);
        $inverse  = DesignTokens::fromDesign(self::design())->tone('inverse');

        $this->assertSame('inverse', $compiler->tone($section));
        $this->assertSame($inverse['background'], $compiler->settings($section)['background_color']);
        $this->assertSame('#15171C', $inverse['background']);
    }

    /**
     * @dataProvider tones
     */
    public function test_a_section_without_a_background_keeps_its_tone(string $tone): void
    {
        $section = self::section([], ['tone' => $tone]);
        unset($section['background']);

        $this->assertSame($tone, self::compiler()->tone($section));
        $this->assertArrayNotHasKey('background_image', self::compiler()->settings($section));
    }

    public function test_a_section_that_names_no_tone_and_no_background_is_ground(): void
    {
        $this->assertSame('ground', self::compiler()->tone(['type' => 'section', 'anchor' => 'story', 'children' => []]));
    }

    // -------------------------------------------------------------------------
    // background_not_image
    // -------------------------------------------------------------------------

    /**
     * The shared case row a site with a PDF at the background's attachment id
     * refuses, judged here with the site's own answer.
     */
    public function testBackgroundNotImage(): void
    {
        $row = self::caseRow('background-not-an-image');
        $this->assertSame('background_not_image', $row['agent']);
        $mimes  = $row['site']['attachment_mimes'];
        $mimeOf = static fn (int $id): ?string => $mimes[(string) $id] ?? null;
        $input  = json_decode($row['input'], true, 512, JSON_THROW_ON_ERROR);

        $refusal = ElementorSectionCompiler::backgroundRefusal($input['outline'], $mimeOf);

        $this->assertNotNull($refusal);
        $this->assertSame($row['agent'], $refusal['code']);
        $this->assertSame(ElementorSectionCompiler::CODE_BACKGROUND_NOT_IMAGE, $refusal['code']);
        $this->assertCount(1, $refusal['failures']);
        $this->assertSame('outline[0].background', $refusal['failures'][0]['path']);
        $this->assertSame('attachment_id', $refusal['failures'][0]['key']);
        $this->assertStringContainsString('image', $refusal['failures'][0]['reason']);
    }

    public function test_every_background_that_is_not_an_image_is_named(): void
    {
        $outline = [
            self::section(['attachment_id' => 7]),
            ['type' => 'section', 'anchor' => 'plain', 'children' => []],
            self::section(['attachment_id' => 8], ['anchor' => 'story']),
            self::section(['attachment_id' => 9], ['anchor' => 'proof']),
        ];
        $mimes   = [7 => 'application/pdf', 8 => 'image/png', 9 => 'video/mp4'];
        $refusal = ElementorSectionCompiler::backgroundRefusal($outline, static fn (int $id): ?string => $mimes[$id] ?? null);

        $this->assertNotNull($refusal);
        $this->assertSame(['outline[0].background', 'outline[3].background'], array_column($refusal['failures'], 'path'));
    }

    /**
     * @return array<string,array{0:string|null}>
     */
    public static function judgedElsewhere(): array
    {
        $out = [];
        foreach (['image/jpeg', 'image/png', 'image/gif', 'image/webp', 'image/avif'] as $mime) {
            $out[$mime] = [$mime];
        }
        // No readable attachment: the image resolution answers it, as for every image.
        $out['no attachment'] = [null];
        $out['no type']       = [''];

        return $out;
    }

    /**
     * @dataProvider judgedElsewhere
     */
    public function test_an_image_or_an_id_the_site_cannot_show_is_not_background_not_image(?string $mime): void
    {
        $outline = [self::section(['attachment_id' => 43])];

        $this->assertNull(ElementorSectionCompiler::backgroundRefusal($outline, static fn (int $id): ?string => $mime));
    }

    public function test_an_outline_without_a_background_never_asks_the_site(): void
    {
        $asked   = 0;
        $outline = [['type' => 'section', 'anchor' => 'story', 'children' => []]];

        $this->assertNull(ElementorSectionCompiler::backgroundRefusal($outline, static function (int $id) use (&$asked): ?string {
            ++$asked;

            return 'application/pdf';
        }));
        $this->assertSame(0, $asked);
    }

    /**
     * @return array<string,array{0:array<mixed>}>
     */
    public static function unresolved(): array
    {
        return [
            'not resolved'      => [[]],
            'not an image type' => [[self::IMAGE_ID => self::fact(['mime' => 'application/pdf'])]],
            'no type'           => [[self::IMAGE_ID => array_diff_key(self::fact(), ['mime' => true])]],
            'another id'        => [[self::IMAGE_ID => self::fact(['id' => 6])]],
            'unusable address'  => [[self::IMAGE_ID => self::fact(['url' => 'javascript:alert(1)'])]],
            'quote in address'  => [[self::IMAGE_ID => self::fact(['url' => 'https://example.com/a".jpg'])]],
        ];
    }

    /**
     * @dataProvider unresolved
     * @param array<mixed> $media Resolved images by attachment id.
     */
    public function test_a_background_the_site_did_not_resolve_as_an_image_is_not_compiled(array $media): void
    {
        $this->expectException(\InvalidArgumentException::class);

        self::compiler($media)->settings(self::section(['attachment_id' => self::IMAGE_ID]));
    }

    /**
     * @return array<string,array{0:mixed}>
     */
    public static function refusedBackgrounds(): array
    {
        return [
            'not an object'   => ['hero.jpg'],
            'no id'           => [['overlay' => 'soft']],
            'id as text'      => [['attachment_id' => '5']],
            'id zero'         => [['attachment_id' => 0]],
            'unknown overlay' => [['attachment_id' => self::IMAGE_ID, 'overlay' => 'heavy']],
            'unknown focal'   => [['attachment_id' => self::IMAGE_ID, 'focal' => 'left']],
            'unknown key'     => [['attachment_id' => self::IMAGE_ID, 'opacity' => 0.9]],
        ];
    }

    /**
     * @dataProvider refusedBackgrounds
     * @param mixed $background What the section gets as its background.
     */
    public function test_a_background_the_grammar_would_have_refused_is_not_compiled($background): void
    {
        $this->expectException(\InvalidArgumentException::class);

        self::compiler()->settings(self::section([], ['background' => $background]));
    }

    /**
     * One row of the shared case table.
     *
     * @param string $name Row name.
     * @return array<string,mixed>
     */
    private static function caseRow(string $name): array
    {
        $doc = json_decode((string) file_get_contents(self::CASES), true, 512, JSON_THROW_ON_ERROR);
        foreach ($doc['cases'] as $row) {
            if ($row['name'] === $name) {
                return $row;
            }
        }
        throw new \RuntimeException('no case row named ' . $name);
    }
}
