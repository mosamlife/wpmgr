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
