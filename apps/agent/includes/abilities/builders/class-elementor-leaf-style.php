<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The look a designed page gives the leaves of a section, as the settings
 * Elementor stores after a leaf's own content keys.
 *
 * Headings and text take the design's type ramp and the colours of the
 * section's tone:
 *
 *   heading level 1     Display          title colour of the tone
 *   heading level 2     H2               title colour of the tone
 *   heading level 3, 4  H3               title colour of the tone
 *   paragraph           Body             body colour of the tone
 *   lead                Lead             body colour of the tone
 *   eyebrow             Eyebrow          accent colour of the tone
 *
 * Every typography group is written whole, as literals, behind its activator
 * `typography_typography`: family (left out for the design font `system`),
 * size in px, weight, line height in em, text transform, letter spacing in em.
 * Sizes are the desktop sizes. Every number is a numeric string, so the stored
 * tree equals the built one on every supported version.
 *
 * A heading always carries its alignment. Text carries one only when the
 * section is centred, because left is what Elementor gives text without one.
 *
 * A button is a fill, an outline or text, in the accent of the tone: size md,
 * the design's radius, the Button typography, and a hover that is a computed
 * literal. Buttons stand in a row container that wraps.
 *
 * The keys come back in the order Elementor returns them, ready to follow the
 * content keys (`title` and `header_size`, `editor`). KEYS lists every key
 * written, with the pin LeafPolicy holds its value to.
 *
 * Every argument comes from OutlineV3, which has judged it; one outside what
 * the grammar accepts is a programming error and throws
 * \InvalidArgumentException.
 *
 * Pure: no WordPress call, no I/O, and no float reaches a string except
 * through a fixed format, so the same design gives the same bytes on any host.
 */
final class ElementorLeafStyle
{
    /** The text styles a paragraph or an eyebrow can take. */
    public const TEXT_STYLES = ['body', 'lead', 'eyebrow'];

    /** The styles of a button. */
    public const BUTTON_STYLES = ['fill', 'outline', 'text'];

    /** How a row of buttons is aligned, as the outline names it. */
    public const ROW_ALIGNS = ['left', 'center'];

    /** The gap between the buttons of a row, in px. */
    private const BUTTON_GAP_PX = 16;

    /** The width of an outline button's border, in px as stored. */
    private const BORDER_WIDTH = '1.5';

    /** The alignments of a section's content, as the outline names them, and the value Elementor stores for each. */
    private const ALIGN = ['start' => 'left', 'center' => 'center'];

    /** The style of the ramp each heading level takes. */
    private const HEADING_STYLE = [1 => 'display', 2 => 'h2', 3 => 'h3', 4 => 'h3'];

    /** The font weights the ramp writes. */
    private const WEIGHTS = '400|600|700';

    /**
     * Every settings key the leaf styles write, by widget type, with its pin.
     *
     * @var array<string,array<string,string>>
     */
    public const KEYS = [
        'heading'     => [
            'align'                     => 'enum:left|center',
            'title_color'               => 'colour',
            'typography_typography'     => 'enum:custom',
            'typography_font_family'    => 'enum',
            'typography_font_size'      => 'slider:px',
            'typography_font_weight'    => 'enum:' . self::WEIGHTS,
            'typography_line_height'    => 'slider:em',
            'typography_letter_spacing' => 'slider:em',
        ],
        'text-editor' => [
            'align'                     => 'enum:center',
            'text_color'                => 'colour',
            'typography_typography'     => 'enum:custom',
            'typography_font_family'    => 'enum',
            'typography_font_size'      => 'slider:px',
            'typography_font_weight'    => 'enum:' . self::WEIGHTS,
            'typography_line_height'    => 'slider:em',
            'typography_text_transform' => 'enum:uppercase',
            'typography_letter_spacing' => 'slider:em',
        ],
        'button'      => [
            // The whole link, which may be a section anchor such as #pricing.
            'link'                               => 'link',
            'size'                               => 'enum:md',
            'button_type'                        => 'enum:',
            'background_background'              => 'enum:classic',
            'background_color'                   => 'colour',
            'button_text_color'                  => 'colour',
            'border_border'                      => 'enum:solid',
            'border_width'                       => 'dimensions',
            'border_color'                       => 'colour',
            'button_background_hover_background' => 'enum:classic',
            'button_background_hover_color'      => 'colour',
            'hover_color'                        => 'colour',
            'border_radius'                      => 'dimensions',
            'typography_typography'              => 'enum:custom',
            'typography_font_family'             => 'enum',
            'typography_font_size'               => 'slider:px',
            'typography_font_weight'             => 'enum:' . self::WEIGHTS,
            'typography_line_height'             => 'slider:em',
            'typography_text_decoration'         => 'enum:underline',
        ],
        'container'   => [
            'content_width'        => 'enum:full',
            'flex_direction'       => 'enum:row',
            'flex_wrap'            => 'enum:wrap',
            'flex_gap'             => 'gaps',
            'flex_justify_content' => 'enum:center',
        ],
    ];

    private DesignTokens $tokens;

    /**
     * @param DesignTokens $tokens The design's tokens.
     */
    public function __construct(DesignTokens $tokens)
    {
        $this->tokens = $tokens;
    }

    /**
     * The style keys of a heading.
     *
     * @param int    $level 1 to 4.
     * @param string $tone  One of OutlineV3Schema::TONES: the tone of the section the heading stands in.
     * @param string $align One of OutlineV3Schema::ALIGNS: the alignment of that section.
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When an argument is outside what the grammar accepts.
     */
    public function headingStyle(int $level, string $tone, string $align): array
    {
        $style  = self::HEADING_STYLE[$level] ?? throw new \InvalidArgumentException('a heading level is 1 to 4');
        $colour = $this->tokens->tone($tone)['heading'];
        $side   = self::ALIGN[$align] ?? throw new \InvalidArgumentException('a section alignment is start or center');

        return array_merge(
            ['align' => $side, 'title_color' => $colour],
            $this->typography($style)
        );
    }

    /**
     * The style keys of a text-editor widget.
     *
     * @param string $style One of TEXT_STYLES.
     * @param string $tone  One of OutlineV3Schema::TONES: the tone of the section the text stands in.
     * @param string $align One of OutlineV3Schema::ALIGNS: the alignment of that section.
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When an argument is outside what the grammar accepts.
     */
    public function textStyle(string $style, string $tone, string $align): array
    {
        if (!in_array($style, self::TEXT_STYLES, true)) {
            throw new \InvalidArgumentException('a text style is body, lead or eyebrow');
        }
        $colours = $this->tokens->tone($tone);
        $side    = self::ALIGN[$align] ?? throw new \InvalidArgumentException('a section alignment is start or center');

        $keys = $side === 'center' ? ['align' => $side] : [];
        $keys['text_color'] = $style === 'eyebrow' ? $colours['accent'] : $colours['body'];

        return array_merge($keys, $this->typography($style));
    }

    /**
     * The style keys of a button, to follow its `text` and `link`.
     *
     *   fill     the accent as background, on_accent text; hover is the accent
     *            8 points darker, a literal
     *   outline  transparent background, the accent as text and as a 1.5 px
     *            border; hover fills with the accent under on_accent text
     *   text     no background and no border, the accent as text, underlined
     *            at rest (the button has no hover typography); hover darkens
     *            the text
     *
     * Every button has size md, the design's radius and the Button
     * typography. On the accent tone the section is the accent, so a button is
     * drawn in on_accent and the text on a fill is the accent.
     *
     * @param string $style One of BUTTON_STYLES.
     * @param string $tone  One of OutlineV3Schema::TONES: the tone of the section the button stands in.
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When an argument is outside what the grammar accepts.
     */
    public function buttonStyle(string $style, string $tone): array
    {
        if (!in_array($style, self::BUTTON_STYLES, true)) {
            throw new \InvalidArgumentException('a button style is fill, outline or text');
        }
        $onAccent = $this->tokens->tone('accent');
        $accent   = $this->tokens->tone($tone)['accent'];
        // The colour of the button, and the colour that sits on it.
        $main = $accent;
        $on   = $tone === 'accent' ? $onAccent['background'] : $onAccent['heading'];

        $keys = ['size' => 'md', 'button_type' => '', 'background_background' => 'classic'];
        if ($style === 'fill') {
            $keys['background_color']                   = $main;
            $keys['button_text_color']                  = $on;
            $keys['button_background_hover_background'] = 'classic';
            $keys['button_background_hover_color']      = DesignTokens::hover($main);
            $keys['hover_color']                        = $on;
        } elseif ($style === 'outline') {
            $keys['background_color']                   = 'transparent';
            $keys['button_text_color']                  = $main;
            $keys['border_border']                      = 'solid';
            $keys['border_width']                       = self::sides(self::BORDER_WIDTH);
            $keys['border_color']                       = $main;
            $keys['button_background_hover_background'] = 'classic';
            $keys['button_background_hover_color']      = $main;
            $keys['hover_color']                        = $on;
        } else {
            $keys['background_color']                   = 'transparent';
            $keys['button_text_color']                  = $main;
            $keys['button_background_hover_background'] = 'classic';
            $keys['button_background_hover_color']      = 'transparent';
            $keys['hover_color']                        = DesignTokens::hover($main);
        }
        $keys['border_radius'] = self::sides((string) $this->tokens->radius());
        $keys                  = array_merge($keys, $this->typography('button'));
        if ($style === 'text') {
            $keys['typography_text_decoration'] = 'underline';
        }

        return $keys;
    }

    /**
     * A row of buttons: an inner container that holds them side by side and
     * wraps them on a narrow screen.
     *
     * @param string                     $id      The row's id, from IdSeed.
     * @param list<array<string,mixed>>  $buttons The button widgets, in order.
     * @param string                     $align   One of ROW_ALIGNS.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the alignment is not one of ROW_ALIGNS.
     */
    public function buttonRow(string $id, array $buttons, string $align): array
    {
        if (!in_array($align, self::ROW_ALIGNS, true)) {
            throw new \InvalidArgumentException('a row of buttons is aligned left or center');
        }
        $gap      = (string) self::BUTTON_GAP_PX;
        $settings = [
            'content_width'  => 'full',
            'flex_direction' => 'row',
            'flex_wrap'      => 'wrap',
            'flex_gap'       => ['column' => $gap, 'row' => $gap, 'isLinked' => true, 'unit' => 'px'],
        ];
        if ($align === 'center') {
            $settings['flex_justify_content'] = 'center';
        }

        return [
            'id'       => $id,
            'elType'   => 'container',
            'settings' => $settings,
            'elements' => $buttons,
            'isInner'  => true,
        ];
    }

    /**
     * A dimensions value with the same length on every side.
     *
     * @param string $length Length in px, as stored.
     * @return array<string,mixed>
     */
    private static function sides(string $length): array
    {
        return ['unit' => 'px', 'top' => $length, 'right' => $length, 'bottom' => $length, 'left' => $length, 'isLinked' => true];
    }

    /**
     * A typography group as literals, behind its activator.
     *
     * @param string $style One of DesignTokens::STYLES.
     * @return array<string,mixed>
     */
    private function typography(string $style): array
    {
        $type = $this->tokens->typography($style);
        $keys = ['typography_typography' => 'custom'];
        if ($type['family'] !== null) {
            $keys['typography_font_family'] = $type['family'];
        }
        $keys['typography_font_size']   = ['unit' => 'px', 'size' => (string) $type['size']];
        $keys['typography_font_weight'] = (string) $type['weight'];
        if ($type['line_height'] !== null) {
            $keys['typography_line_height'] = ['unit' => 'em', 'size' => self::decimal($type['line_height'])];
        }
        if ($type['transform'] !== null) {
            $keys['typography_text_transform'] = $type['transform'];
        }
        if ($type['letter_spacing'] !== null) {
            $keys['typography_letter_spacing'] = ['unit' => 'em', 'size' => self::decimal($type['letter_spacing'])];
        }

        return $keys;
    }

    /**
     * A number as text: a point for the decimal mark whatever the host's
     * locale, at most four decimals, none of them a trailing zero.
     *
     * @param float $value The number.
     * @return string
     */
    private static function decimal(float $value): string
    {
        return rtrim(rtrim(number_format($value, 4, '.', ''), '0'), '.');
    }
}
