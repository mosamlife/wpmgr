<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The container of one section of an outline version 3 page, as Elementor
 * stores it, with the design's literal colours and sizes.
 *
 *   content_width         the section's width: boxed or full (boxed when absent)
 *   html_tag              "section"
 *   flex_direction        "column"
 *   _element_id           the section's anchor
 *   padding               top and bottom from the section's spacing at the
 *                         design's density (md, lg or xl; lg when absent),
 *                         left and right SIDE_PADDING_PX at every density
 *   padding_tablet        the same at the tablet step, written only when the
 *   padding_mobile        site has that breakpoint active, and the mobile step
 *   background_background "classic", the activator of the colour below
 *   background_color      the tone's background (ground, surface, inverse or
 *                         accent; ground when absent)
 *   background_image      a section with a background: {id, url}, the image
 *                         the site resolved, its id as a numeric string and
 *                         its library address
 *   background_position   the focal point: center, top or bottom, centred
 *                         across (center when absent)
 *   background_size       "cover"
 *   background_repeat     "no-repeat"
 *   background_overlay_background  "classic", the activator of the two below
 *   background_overlay_color       the ink colour
 *   background_overlay_opacity     0.45 (soft) or 0.65 (strong; strong when
 *                                  absent), a fraction as a numeric string;
 *                                  an overlay of none writes none of the three
 *   _title                LeafPolicy::SECTION_LABEL
 *
 * A section with a background image is written in the inverse tone whatever
 * tone it names: the ink colour under the image and the inverse text colours
 * over it (tone()). The image must be one the site resolved as an image of an
 * allowed type; backgroundRefusal() answers background_not_image, before the
 * page is compiled, for an attachment the site holds that is not an image.
 *
 * Keys are written in this order, which is the order Elementor returns them
 * in. Every number is a numeric string and a flag is a JSON boolean, so the
 * stored tree equals the built one on every supported version.
 *
 * The section arrives from OutlineV3, which has judged every option; a value
 * outside what the grammar accepts is a programming error and throws
 * \InvalidArgumentException. The compiler issues no ids: the caller gives the
 * container's id and its children.
 *
 * Pure: no WordPress call.
 */
final class ElementorSectionCompiler
{
    /** The width of a section that names none. */
    public const DEFAULT_WIDTH = 'boxed';

    /** The tone of a section that names none. */
    public const DEFAULT_TONE = 'ground';

    /** The spacing of a section that names none. */
    public const DEFAULT_SPACING = 'lg';

    /** Left and right padding of a section, in px, at every density and breakpoint. */
    public const SIDE_PADDING_PX = 24;

    /** The overlay of a background that names none: strong keeps the inverse text legible on any image. */
    public const DEFAULT_OVERLAY = 'strong';

    /** The focal point of a background that names none. */
    public const DEFAULT_FOCAL = 'center';

    /** The tone a section with a background image is written in. */
    public const BACKGROUND_TONE = 'inverse';

    /** The refusal of a section background whose attachment is not an image. */
    public const CODE_BACKGROUND_NOT_IMAGE = 'background_not_image';

    /** The padding key of each breakpoint a section overrides, in the order they are written. */
    private const PADDING_OVERRIDES = ['tablet' => 'padding_tablet', 'mobile' => 'padding_mobile'];

    /** The keys of a section background. */
    private const BACKGROUND_KEYS = ['attachment_id', 'overlay', 'focal'];

    /** Elementor's background position for each focal point. */
    private const FOCAL_POSITIONS = ['center' => 'center center', 'top' => 'top center', 'bottom' => 'bottom center'];

    /** Why a background that is not an image is refused. */
    private const REASON_NOT_IMAGE = 'must name an image in the media library: a JPEG, PNG, GIF, WebP or AVIF file';

    /** An overlay opacity as Elementor writes it into the page's CSS: a fraction above 0 and below 1. */
    private const RE_FRACTION = '/^0\.[0-9]?[1-9]\z/';

    private DesignTokens $tokens;

    /** @var list<string> */
    private array $active;

    /** @var array<mixed> */
    private array $media;

    /**
     * @param DesignTokens      $tokens      The design's tokens.
     * @param list<string>|null $breakpoints The site's active breakpoints (ElementorFacts active_breakpoints); null when none is.
     * @param array<mixed>      $media       Resolved image facts by attachment id, as the page-create precheck hands them on.
     */
    public function __construct(DesignTokens $tokens, ?array $breakpoints, array $media = [])
    {
        $this->tokens = $tokens;
        $this->active = $breakpoints ?? [];
        $this->media  = $media;
    }

    /**
     * A section as a container around its children.
     *
     * @param string                     $id       The container's id, from IdSeed.
     * @param array<string,mixed>        $section  The section node.
     * @param list<array<string,mixed>>  $elements Its compiled children.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the section holds a value the grammar does not accept.
     */
    public function container(string $id, array $section, array $elements): array
    {
        return [
            'id'       => $id,
            'elType'   => 'container',
            'settings' => $this->settings($section),
            'elements' => $elements,
            'isInner'  => false,
        ];
    }

    /**
     * The settings of a section's container.
     *
     * @param array<string,mixed> $section The section node.
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When the section holds a value the grammar does not accept.
     */
    public function settings(array $section): array
    {
        $width   = self::option($section, 'width', OutlineV3Schema::WIDTHS, self::DEFAULT_WIDTH) ?? throw new \InvalidArgumentException('a section width is boxed or full');
        $tone    = self::option($section, 'tone', OutlineV3Schema::TONES, self::DEFAULT_TONE) ?? throw new \InvalidArgumentException('a section tone is ground, surface, inverse or accent');
        $spacing = self::option($section, 'spacing', OutlineV3Schema::SPACINGS, self::DEFAULT_SPACING) ?? throw new \InvalidArgumentException('a section spacing is md, lg or xl');
        $image   = self::backgroundOf($section);

        return array_merge(
            [
                'content_width'  => $width,
                'html_tag'       => 'section',
                'flex_direction' => 'column',
                '_element_id'    => self::anchor($section),
            ],
            $this->padding($spacing),
            $this->background($image === null ? $tone : self::BACKGROUND_TONE),
            $image === null ? [] : $this->image($image),
            [LeafPolicy::LABEL_KEY => LeafPolicy::SECTION_LABEL]
        );
    }

    /**
     * The tone whose colours the section and its children are written in:
     * BACKGROUND_TONE when the section has a background image, else the tone
     * it names (ground when absent).
     *
     * @param array<string,mixed> $section The section node.
     * @return string One of OutlineV3Schema::TONES.
     * @throws \InvalidArgumentException When the section holds a value the grammar does not accept.
     */
    public function tone(array $section): string
    {
        $tone = self::option($section, 'tone', OutlineV3Schema::TONES, self::DEFAULT_TONE) ?? throw new \InvalidArgumentException('a section tone is ground, surface, inverse or accent');

        return self::backgroundOf($section) === null ? $tone : self::BACKGROUND_TONE;
    }

    /**
     * The refusal of the sections whose background names an attachment that
     * is not an image (CODE_BACKGROUND_NOT_IMAGE), each failure naming the
     * section's background; null when none does.
     *
     * $mimeOf answers the type of an attachment the principal may read, and
     * null for an id that names no such attachment: that id is answered by
     * the image resolution, as every image of the page is. A background is an
     * image of a type an image block may show (PageCreateBuilder::IMAGE_MIMES).
     * The site is asked only about the sections that have a background.
     *
     * @param mixed                  $outline The validated outline: a list of sections.
     * @param \Closure(int): ?string $mimeOf  The type of a readable attachment, or null.
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}|null
     */
    public static function backgroundRefusal(mixed $outline, \Closure $mimeOf): ?array
    {
        if (!is_array($outline) || !ArrayShape::isList($outline)) {
            return null;
        }
        $failures = [];
        foreach ($outline as $i => $section) {
            $background = is_array($section) ? ($section['background'] ?? null) : null;
            $id         = is_array($background) ? ($background['attachment_id'] ?? null) : null;
            if (!is_int($id)) {
                continue;
            }
            $mime = $mimeOf($id);
            if (!is_string($mime) || $mime === '' || in_array($mime, PageCreateBuilder::IMAGE_MIMES, true)) {
                continue;
            }
            $failures[] = ['path' => 'outline[' . $i . '].background', 'key' => 'attachment_id', 'reason' => self::REASON_NOT_IMAGE];
        }

        return $failures === [] ? null : Refusal::make(self::CODE_BACKGROUND_NOT_IMAGE, 'A section background is not an image.', $failures);
    }

    /**
     * Padding at the desktop step, then at each breakpoint the site has active.
     *
     * @param string $spacing One of OutlineV3Schema::SPACINGS.
     * @return array<string,array<string,mixed>>
     */
    private function padding(string $spacing): array
    {
        $steps   = $this->tokens->sectionPadding($spacing);
        $padding = ['padding' => self::sides($steps['desktop'])];
        foreach (self::PADDING_OVERRIDES as $breakpoint => $key) {
            if (in_array($breakpoint, $this->active, true)) {
                $padding[$key] = self::sides($steps[$breakpoint]);
            }
        }

        return $padding;
    }

    /**
     * The tone's background colour and the activator Elementor needs to paint it.
     *
     * @param string $tone One of OutlineV3Schema::TONES.
     * @return array<string,string>
     */
    private function background(string $tone): array
    {
        return [
            'background_background' => 'classic',
            'background_color'       => $this->tokens->tone($tone)['background'],
        ];
    }

    /**
     * The background image over the tone's colour: the image the site
     * resolved, placed at the focal point, covering the section without
     * repeating, then the ink overlay at the background's strength.
     *
     * @param array{attachment_id:int,overlay:string,focal:string} $background From backgroundOf().
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When the site did not resolve the attachment as an image.
     */
    private function image(array $background): array
    {
        $id   = $background['attachment_id'];
        $fact = $this->media[$id] ?? null;
        $url  = is_array($fact) ? ($fact['url'] ?? null) : null;
        if (!is_array($fact) || ($fact['id'] ?? null) !== $id || !is_string($url) || PageCreateBuilder::imageUrlProblem($url) !== null
            || !in_array($fact['mime'] ?? null, PageCreateBuilder::IMAGE_MIMES, true)) {
            throw new \InvalidArgumentException('a section background is an image the site resolved before the page is compiled');
        }
        $settings = [
            'background_image'    => ['id' => (string) $id, 'url' => $url],
            'background_position' => self::FOCAL_POSITIONS[$background['focal']],
            'background_size'     => 'cover',
            'background_repeat'   => 'no-repeat',
        ];
        $overlay = $this->tokens->overlay($background['overlay']);
        if ($overlay === null) {
            return $settings;
        }

        return $settings + [
            'background_overlay_background' => 'classic',
            'background_overlay_color'      => $overlay['color'],
            'background_overlay_opacity'    => ['unit' => 'px', 'size' => self::fraction($overlay['opacity'])],
        ];
    }

    /**
     * An overlay opacity as a numeric string Elementor writes verbatim into
     * the page's CSS: a fraction above 0 and below 1 ("0.45"), never a
     * percentage.
     *
     * @param float $opacity The opacity.
     * @return string
     * @throws \InvalidArgumentException When the opacity is not such a fraction.
     */
    private static function fraction(float $opacity): string
    {
        $text = rtrim(rtrim(sprintf('%.2F', $opacity), '0'), '.');
        if (preg_match(self::RE_FRACTION, $text) !== 1) {
            throw new \InvalidArgumentException('an overlay opacity is a fraction above 0 and below 1');
        }

        return $text;
    }

    /**
     * The section's background with its options filled in, or null when it
     * has none.
     *
     * @param array<string,mixed> $section The section node.
     * @return array{attachment_id:int,overlay:string,focal:string}|null
     * @throws \InvalidArgumentException When the background is not one the grammar accepts.
     */
    private static function backgroundOf(array $section): ?array
    {
        if (!array_key_exists('background', $section)) {
            return null;
        }
        $background = $section['background'];
        if (!is_array($background) || array_diff(array_keys($background), self::BACKGROUND_KEYS) !== []) {
            throw new \InvalidArgumentException('a section background is an attachment id, an overlay and a focal point');
        }
        $id = $background['attachment_id'] ?? null;
        if (!is_int($id) || $id < 1 || $id > PageCreateBuilder::MAX_ATTACHMENT_ID) {
            throw new \InvalidArgumentException('a section background names its attachment by a whole number');
        }

        return [
            'attachment_id' => $id,
            'overlay'       => self::option($background, 'overlay', OutlineV3Schema::OVERLAYS, self::DEFAULT_OVERLAY) ?? throw new \InvalidArgumentException('a background overlay is none, soft or strong'),
            'focal'         => self::option($background, 'focal', OutlineV3Schema::FOCALS, self::DEFAULT_FOCAL) ?? throw new \InvalidArgumentException('a background focal point is center, top or bottom'),
        ];
    }

    /**
     * A dimensions value with the same top and bottom and the side gutter.
     *
     * @param int $vertical Top and bottom padding in px.
     * @return array<string,mixed>
     */
    private static function sides(int $vertical): array
    {
        return [
            'unit'     => 'px',
            'top'      => (string) $vertical,
            'right'    => (string) self::SIDE_PADDING_PX,
            'bottom'   => (string) $vertical,
            'left'     => (string) self::SIDE_PADDING_PX,
            'isLinked' => false,
        ];
    }

    /**
     * An option of the section: its value when it names one of $allowed, the
     * default when it names none.
     *
     * @param array<string,mixed> $section  The section node.
     * @param string              $key      The option.
     * @param list<string>        $allowed  The values the grammar accepts.
     * @param string              $fallback The value of a section that names none.
     * @return string|null Null when the option is present and not one of $allowed.
     */
    private static function option(array $section, string $key, array $allowed, string $fallback): ?string
    {
        if (!array_key_exists($key, $section)) {
            return $fallback;
        }
        $value = $section[$key];

        return is_string($value) && in_array($value, $allowed, true) ? $value : null;
    }

    /**
     * The section's anchor, in the form the grammar accepts and not one the
     * theme or WordPress already uses.
     *
     * @param array<string,mixed> $section The section node.
     * @return string
     * @throws \InvalidArgumentException When the anchor is not one the grammar accepts.
     */
    private static function anchor(array $section): string
    {
        $anchor = $section['anchor'] ?? null;
        if (!is_string($anchor) || preg_match('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', $anchor) !== 1) {
            throw new \InvalidArgumentException('a section anchor is a lower-case letter, then 1 to 39 lower-case letters, digits or hyphens');
        }
        if (in_array($anchor, OutlineV3Schema::RESERVED_ANCHORS, true)) {
            throw new \InvalidArgumentException('a section anchor is not one the theme or WordPress already uses');
        }

        return $anchor;
    }
}
