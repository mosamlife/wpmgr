<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

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
 *   _title                LeafPolicy::SECTION_LABEL
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

    /** The padding key of each breakpoint a section overrides, in the order they are written. */
    private const PADDING_OVERRIDES = ['tablet' => 'padding_tablet', 'mobile' => 'padding_mobile'];

    private DesignTokens $tokens;

    /** @var list<string> */
    private array $active;

    /**
     * @param DesignTokens      $tokens      The design's tokens.
     * @param list<string>|null $breakpoints The site's active breakpoints (ElementorFacts active_breakpoints); null when none is.
     */
    public function __construct(DesignTokens $tokens, ?array $breakpoints)
    {
        $this->tokens = $tokens;
        $this->active = $breakpoints ?? [];
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
        $width   = self::option($section, 'width', OutlineV3Schema::WIDTHS, self::DEFAULT_WIDTH);
        $tone    = self::option($section, 'tone', OutlineV3Schema::TONES, self::DEFAULT_TONE);
        $spacing = self::option($section, 'spacing', OutlineV3Schema::SPACINGS, self::DEFAULT_SPACING);

        return array_merge(
            [
                'content_width'  => $width,
                'html_tag'       => 'section',
                'flex_direction' => 'column',
                '_element_id'    => self::anchor($section),
            ],
            $this->padding($spacing),
            $this->background($tone),
            [LeafPolicy::LABEL_KEY => LeafPolicy::SECTION_LABEL]
        );
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
     * @return string
     * @throws \InvalidArgumentException When the option is present and not one of $allowed.
     */
    private static function option(array $section, string $key, array $allowed, string $fallback): string
    {
        if (!array_key_exists($key, $section)) {
            return $fallback;
        }
        $value = $section[$key];
        if (!is_string($value) || !in_array($value, $allowed, true)) {
            throw new \InvalidArgumentException('a section ' . $key . ' is ' . implode(', ', $allowed));
        }

        return $value;
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
