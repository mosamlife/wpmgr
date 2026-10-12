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
 * A validated page-create outline as an Elementor classic (v3) element tree,
 * and a stored classic tree as the builder-neutral projection.
 *
 * Layout. With containers on, every top-level node is a container; with
 * containers off, a section holding columns.
 *
 *   outline leaves in a row   one boxed column container (containers) or one
 *                             section "10" with one column "100" (sections)
 *                             holding those leaves in order
 *   group                     the same wrapper, holding the group's children
 *   columns (n = 2..4)        a row container whose n child containers have
 *                             content_width "full" and a percentage width
 *                             (the outline's widths, else 100/n rounded down),
 *                             or a section "n0" whose n columns carry
 *                             _column_size 100/n and, with widths,
 *                             _inline_size; inside a group the row container
 *                             or section is an inner one
 *
 * Leaves. heading: heading widget, title and h2|h3|h4. paragraph, list,
 * quote, table: one text-editor widget holding a fixed tag template with no
 * attributes. buttons: one button widget per button. image: image widget
 * with the attachment's library address, size "large", and the caption when
 * there is one. separator: divider. spacer: 24, 48 or 96 px.
 *
 * Bytes. Every text an AI wrote is stored through LeafPolicy::htmlText().
 * Every number is stored as a string and no setting holds a boolean or null,
 * so the stored tree equals the built one on every supported version.
 * Element keys follow Elementor's own order: id, elType, settings, elements,
 * then isInner on a layout element and widgetType on a widget. Node ids come
 * from the request's IdSeed in document order, parent before children, keyed
 * by the node's outline path; a wrapper WPMgr adds is keyed by the path of
 * its first node plus a "#" suffix the outline never produces.
 *
 * Refused, never rewritten: an outline button on a plain outline
 * (node_not_supported_by_builder),
 * an image aligned wide or full (node_not_supported_by_builder), a paragraph
 * whose whole text is one http(s) address (create_content_invalid), alt text
 * that differs from the attachment's library alt (image_alt_from_library),
 * and a button link holding "&" (link_invalid). The finished tree then passes
 * LeafPolicy::checkTree() with ALLOWED_KEYS (designedKeys() for a designed
 * page), without the overrides of a breakpoint the site does not have
 * active, and LEAF_RULES, or the call is refused with that check's answer.
 *
 * A page edit builds its new nodes with mapFragment(), under the same rules,
 * and changes one text with setText(), which writes the same bytes create
 * writes for that text.
 *
 * Design-carrying nodes (DESIGN_NODES: a section and a pattern) are built from
 * containers. A site that builds sections and columns refuses each with
 * CODE_CONTAINERS_REQUIRED, naming the node's path, and a plain outline is
 * built there as it always was.
 *
 * The leaves of a designed section (mapDesignedLeaves) take the design's look
 * from ElementorLeafStyle: a level 1 heading is allowed, the first paragraph
 * after it is the lead, a list, quote and table are body text, and buttons
 * are a fill, an outline or text, standing in one row. All of it is in the
 * section's tone and alignment.
 *
 * Pure: no WordPress write. The only WordPress function reached is the HTML
 * sanitiser, through LeafPolicy.
 */
final class ElementorClassicMapper
{
    /**
     * Every element type and settings path the mapper and the compilers
     * write, with its pin. Numbers are stored as strings: a plain number is
     * pinned "enum", and a style value is pinned by its whole shape. A style
     * key is listed only once an E1 round-trip probe has stored and rendered
     * it; a tablet or mobile override is listed only for a control a probe
     * showed responsive, and LeafPolicy::forBreakpoints() keeps it only on a
     * site with that breakpoint active.
     */
    public const ALLOWED_KEYS = [
        'container'   => [
            'content_width'         => 'enum',
            'flex_direction'        => 'enum',
            'flex_direction_mobile' => 'enum',
            'flex_gap.unit'         => 'enum',
            'flex_gap.size'         => 'enum',
            'flex_gap.column'       => 'enum',
            'flex_gap.row'          => 'enum',
            'width.unit'            => 'enum',
            'width.size'            => 'enum',
            'width.sizes'           => 'enum',
            // A designed section: its tag, its anchor and its Navigator label.
            // LeafPolicy judges the anchor and the label by their own rules, and
            // only for keys listed here.
            'html_tag'              => 'enum',
            '_element_id'           => 'enum',
            '_title'                => 'enum',
            // Style keys from here on: each one an E1 round-trip probe stored
            // and rendered, judged whole by its shape pin in the measured form.
            'padding'                       => 'dimensions',
            'padding_tablet'                => 'dimensions',
            'padding_mobile'                => 'dimensions',
            'background_background'         => 'enum:classic',
            'background_color'              => 'colour',
            'background_image'              => 'image',
            'background_position'           => 'enum:center center|top center|bottom center',
            'background_size'               => 'enum:cover',
            'background_repeat'             => 'enum:no-repeat',
            'background_overlay_background' => 'enum:classic',
            'background_overlay_color'      => 'colour',
            'background_overlay_opacity'    => 'slider:px',
            'min_height'                    => 'slider:vh',
            'min_height_mobile'             => 'slider:vh',
            'flex_justify_content'          => 'enum:center',
        ],
        'section'     => [
            'structure' => 'enum',
        ],
        'column'      => [
            '_column_size' => 'enum',
            '_inline_size' => 'enum',
        ],
        'heading'     => [
            'title'                       => 'html',
            'header_size'                 => 'enum',
            'title_color'                 => 'colour',
            'align'                       => 'enum:left',
            'typography_typography'       => 'enum:custom',
            'typography_font_family'      => 'text',
            'typography_font_size'        => 'slider:px',
            'typography_font_size_tablet' => 'slider:px',
            'typography_font_size_mobile' => 'slider:px',
            'typography_font_weight'      => 'enum:400|600|700',
            'typography_line_height'      => 'slider:em',
            'typography_letter_spacing'   => 'slider:em',
        ],
        'text-editor' => [
            'editor'                    => 'html',
            'text_color'                => 'colour',
            'typography_typography'     => 'enum:custom',
            'typography_font_family'    => 'text',
            'typography_font_size'      => 'slider:px',
            'typography_font_weight'    => 'enum:400|600|700',
            'typography_line_height'    => 'slider:em',
            'typography_letter_spacing' => 'slider:em',
            'typography_text_transform' => 'enum:uppercase',
        ],
        'button'      => [
            'text'             => 'html',
            'link.url'         => 'url',
            'link.is_external' => 'enum',
            'link.nofollow'    => 'enum',
            'align'            => 'enum',
            // "enum:" takes one value, the empty string.
            'size'                               => 'enum:md',
            'button_type'                        => 'enum:',
            'background_background'              => 'enum:classic',
            'background_color'                   => 'colour',
            'button_text_color'                  => 'colour',
            'button_background_hover_background' => 'enum:classic',
            'button_background_hover_color'      => 'colour',
            'hover_color'                        => 'colour',
            'border_radius'                      => 'dimensions',
            'border_border'                      => 'enum:solid',
            'border_width'                       => 'dimensions',
            'border_color'                       => 'colour',
            'typography_typography'              => 'enum:custom',
            'typography_font_family'             => 'text',
            'typography_font_size'               => 'slider:px',
            'typography_font_weight'             => 'enum:400|600|700',
            'typography_line_height'             => 'slider:em',
            'typography_text_decoration'         => 'enum:underline',
        ],
        'image'       => [
            'image.url'      => 'url',
            'image.id'       => 'enum',
            'image.source'   => 'enum',
            'image_size'     => 'enum',
            'align'          => 'enum',
            'caption_source' => 'enum',
            'caption'        => 'html',
            'width'          => 'slider:px',
        ],
        'divider'     => [],
        'spacer'      => [
            'space.unit'  => 'enum',
            'space.size'  => 'enum',
            'space.sizes' => 'enum',
        ],
    ];

    /** Elementor's leaf rules: braces are plain text; its dynamic-tag marker is refused. */
    public const LEAF_RULES = [
        'refuse_braces' => false,
        'forbidden'     => ['[elementor-tag'],
    ];

    /**
     * The breakpoints Elementor has active out of the box. A plain outline
     * mapped without the site's own breakpoints is built for these.
     */
    public const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    /** The widget types whose pins a leaf style widens in designedKeys(). */
    private const STYLE_WIDENS = ['heading', 'text-editor', 'button'];

    /** The refusal of a design-carrying node on a site that builds sections and columns. */
    public const CODE_CONTAINERS_REQUIRED = 'containers_required';

    /** The outline node types that carry a design, which only containers can hold. */
    public const DESIGN_NODES = ['section', 'pattern'];

    /** Label key of an element holding dynamic content, code or attributes. */
    public const LABEL_DYNAMIC = 'wpmgr:dynamic';

    /** Label key of a text editor whose HTML is not one of WPMgr's templates. */
    public const LABEL_CUSTOM_TEXT = 'wpmgr:custom-text';

    /** Label key of an Elementor v4 (atomic) element. */
    public const LABEL_ATOMIC = 'wpmgr:atomic';

    /** Label key of a WordPress widget placed in Elementor. */
    public const LABEL_WP_WIDGET = 'wpmgr:wp-widget';

    /** Label key of a WPMgr pattern whose subtree a person restructured in Elementor. */
    public const LABEL_PATTERN_EDITED = 'wpmgr:pattern-edited';

    /** New nodes at the top of the page: leaves are wrapped, groups and columns map as on a new page. */
    public const CONTEXT_TOP = 'top';

    /** New nodes inside a top-level element that holds widgets: leaves, and columns as an inner row. */
    public const CONTEXT_GROUP = 'group';

    /** New nodes inside an inner element, such as a column of a row: leaves only. */
    public const CONTEXT_INNER = 'inner';

    /**
     * WPMgr's words for a node it does not edit, by Elementor type or by one
     * of the label keys above. The site's own type names never reach the
     * answer: a type not listed gets the fallback text.
     */
    public const LOCKED_LABELS = [
        Projection::FALLBACK_LABEL => 'Elementor element WPMgr does not edit',
        self::LABEL_DYNAMIC        => 'Elementor element with dynamic content or custom code',
        self::LABEL_CUSTOM_TEXT    => 'Elementor text with formatting WPMgr does not edit',
        self::LABEL_ATOMIC         => 'Elementor v4 element',
        self::LABEL_WP_WIDGET      => 'WordPress widget',
        self::LABEL_PATTERN_EDITED => 'WPMgr pattern changed in Elementor',
        'html'                     => 'Elementor HTML code',
        'shortcode'                => 'Elementor shortcode',
        'template'                 => 'Elementor saved template',
        'text-path'                => 'Elementor text path',
        'menu-anchor'              => 'Elementor menu anchor',
        'sidebar'                  => 'Elementor sidebar',
        'video'                    => 'Elementor video',
        'audio'                    => 'Elementor audio',
        'icon'                     => 'Elementor icon',
        'icon-box'                 => 'Elementor icon box',
        'icon-list'                => 'Elementor icon list',
        'image-box'                => 'Elementor image box',
        'image-gallery'            => 'Elementor image gallery',
        'image-carousel'           => 'Elementor image carousel',
        'counter'                  => 'Elementor counter',
        'progress'                 => 'Elementor progress bar',
        'testimonial'              => 'Elementor testimonial',
        'tabs'                     => 'Elementor tabs',
        'nested-tabs'              => 'Elementor tabs',
        'accordion'                => 'Elementor accordion',
        'nested-accordion'         => 'Elementor accordion',
        'toggle'                   => 'Elementor toggle',
        'social-icons'             => 'Elementor social icons',
        'alert'                    => 'Elementor alert',
        'google_maps'              => 'Elementor map',
        'star-rating'              => 'Elementor star rating',
        'rating'                   => 'Elementor rating',
        'form'                     => 'Elementor form',
        'posts'                    => 'Elementor posts',
        'nav-menu'                 => 'Elementor menu',
        'slides'                   => 'Elementor slides',
        'price-table'              => 'Elementor price table',
        'call-to-action'           => 'Elementor call to action',
        'countdown'                => 'Elementor countdown',
    ];

    /** Spacer heights in px, as stored. */
    private const SPACER_PX = ['small' => '24', 'medium' => '48', 'large' => '96'];

    /** Gap between the columns of a row container, in px, as stored. */
    private const GAP_PX = '24';

    /** Heading levels the outline allows, as Elementor's header size. */
    private const HEADER_SIZES = [2 => 'h2', 3 => 'h3', 4 => 'h4'];

    /** Section structure by column count. */
    private const STRUCTURES = [1 => '10', 2 => '20', 3 => '30', 4 => '40'];

    /** A column's preset size by column count. */
    private const COLUMN_SIZES = [1 => '100', 2 => '50', 3 => '33', 4 => '25'];

    /** Button alignments the outline allows, as Elementor's. */
    private const BUTTON_ALIGN = ['left' => 'left', 'center' => 'center'];

    /** Image alignments Elementor's classic image widget can show. */
    private const IMAGE_ALIGN = ['none', 'center'];

    /** A paragraph whose whole text is one http(s) address. */
    private const OWN_PARAGRAPH_ADDRESS = '#^\s*https?://[^\s<>"]+\s*$#i';

    /** The text-editor HTML each template classifies as. */
    private const TEMPLATES = [
        'paragraph' => '#^<p>[^<>]*</p>$#D',
        'list'      => '#^(?:<ul>(?:<li>[^<>]*</li>)+</ul>|<ol>(?:<li>[^<>]*</li>)+</ol>)$#D',
        'quote'     => '#^<blockquote>(?:<p>[^<>]*</p>)+(?:<cite>[^<>]*</cite>)?</blockquote>$#D',
        'table'     => '#^<table>(?:<thead><tr>(?:<th>[^<>]*</th>)+</tr></thead>)?<tbody>(?:<tr>(?:<td>[^<>]*</td>)+</tr>)+</tbody></table>$#D',
    ];

    /** Settings keys that make a stored element dynamic content or code, without regard to case. */
    private const DYNAMIC_KEYS = ['__dynamic__', 'custom_css', '_attributes', 'custom_attributes'];

    /**
     * Where set_text writes, by projection kind and field: the widget type
     * and the settings path of the field's own value.
     */
    private const TEXT_TARGETS = [
        'heading'   => ['text' => ['heading', ['title']]],
        'paragraph' => ['text' => ['text-editor', ['editor']]],
        'buttons'   => ['text' => ['button', ['text']], 'url' => ['button', ['link', 'url']]],
        'image'     => ['caption' => ['image', ['caption']]],
    ];

    private IdSeed $ids;

    /** @var array<mixed> */
    private array $media;

    private bool $containers;

    /** The compiler of a designed page's containers; null for a plain outline. */
    private ?ElementorSectionCompiler $design;

    /**
     * The site's active breakpoints: the design compiler's on a designed
     * page, else the ones the caller named.
     *
     * @var list<string>
     */
    private array $active;

    /** @var array{code: string, detail: string}|null */
    private ?array $refusal = null;

    /** The designed look of the leaves; null builds the plain look of an outline without a design. */
    private ?ElementorLeafStyle $look = null;

    /** The tone of the section whose leaves are being built. */
    private string $tone = ElementorSectionCompiler::DEFAULT_TONE;

    /** The alignment of that section. */
    private string $align = 'start';

    /** Whether the next paragraph is the lead: no paragraph has followed the level 1 heading yet. */
    private bool $leadNext = false;

    /**
     * @param IdSeed                        $ids        Node ids for this request.
     * @param array<mixed>                  $media      Media facts by attachment id.
     * @param bool                          $containers Whether the site has Elementor containers on.
     * @param ElementorSectionCompiler|null $design     The compiler of a designed page; null for a plain outline.
     */
    private function __construct(IdSeed $ids, array $media, bool $containers, ?ElementorSectionCompiler $design = null, ?array $breakpoints = self::STOCK_BREAKPOINTS)
    {
        $this->ids        = $ids;
        $this->media      = $media;
        $this->containers = $containers;
        $this->design     = $design;
        $this->active     = $design !== null ? $design->breakpoints() : ($breakpoints ?? []);
    }

    /**
     * Whether a stored tree holds a designed section: a top-level container
     * labelled LeafPolicy::SECTION_LABEL, as the projection reads one. A page
     * that does is a designed page (outline version 3); any other is a plain
     * one.
     *
     * @param array<mixed> $tree Top-level elements.
     * @return bool
     */
    public static function holdsDesignedSection(array $tree): bool
    {
        foreach ($tree as $node) {
            if (is_array($node) && ($node['elType'] ?? null) === 'container' && is_array($node['settings'] ?? null)
                && ($node['settings'][LeafPolicy::LABEL_KEY] ?? null) === LeafPolicy::SECTION_LABEL) {
                return true;
            }
        }

        return false;
    }

    /**
     * The allowlist of a designed page: ALLOWED_KEYS, the keys the leaf styles
     * write (ElementorLeafStyle::KEYS) and the keys the patterns write
     * (ElementorPatternCompiler::allowedKeys()), as one list.
     *
     * A leaf style widens the pins of a heading, a text editor and a button:
     * a path both list takes every value either pin takes, so a heading is
     * aligned center as well as left. A container keeps every pin
     * ALLOWED_KEYS gives it, so a section's content width and direction are
     * never narrowed to a button row's. A pattern adds the paths it writes
     * and changes no pin already listed. A tree compiled for a site is
     * checked against LeafPolicy::forBreakpoints() of this list, so an
     * override of a breakpoint the site does not have active is refused.
     *
     * @return array<string, array<string, string>> Element type, then settings path, then pin.
     */
    public static function designedKeys(): array
    {
        $keys = self::ALLOWED_KEYS;
        foreach (ElementorLeafStyle::KEYS as $type => $paths) {
            $listed = $keys[$type] ?? [];
            if (!in_array($type, self::STYLE_WIDENS, true)) {
                $keys[$type] = $listed + $paths;
                continue;
            }
            foreach ($paths as $path => $pin) {
                $listed[$path] = isset($listed[$path]) ? self::widen($listed[$path], $pin) : $pin;
            }
            $keys[$type] = $listed;
        }

        return ElementorPatternCompiler::allowedKeys($keys);
    }

    /**
     * One pin for a path two lists pin: the values of two lists of the same
     * pin joined, the pin without a list when one of the two has none (it
     * takes every value), and the leaf style's pin when the names differ.
     *
     * @param string $listed The pin ALLOWED_KEYS gives the path.
     * @param string $style  The pin ElementorLeafStyle::KEYS gives it.
     * @return string
     */
    private static function widen(string $listed, string $style): string
    {
        $a = explode(':', $listed, 2);
        $b = explode(':', $style, 2);
        if ($a[0] !== $b[0]) {
            return $style;
        }
        if (!isset($a[1], $b[1])) {
            return $a[0];
        }

        return $a[0] . ':' . implode('|', array_values(array_unique(array_merge(explode('|', $a[1]), explode('|', $b[1])))));
    }

    /**
     * The allowlist this mapper's tree is checked against: designedKeys() for
     * a designed page, ALLOWED_KEYS for a plain one, each without the
     * overrides of a breakpoint the site does not have active.
     *
     * @return array<string, array<string, string>>
     */
    private function allowlist(): array
    {
        return LeafPolicy::forBreakpoints($this->design !== null ? self::designedKeys() : self::ALLOWED_KEYS, $this->active);
    }

    /**
     * Map a validated page-create spec to a classic element tree.
     *
     * $mediaById maps each attachment id in the outline to its facts; this
     * uses "url" (the large-size address, checked as an image address) and
     * "alt" (the attachment's library alt text, "" when it has none).
     *
     * With $design, a designed page's groups and columns rows on a site with
     * containers are the design's (ElementorSectionCompiler::group(), row()
     * and column()); without it, a plain outline is built as it always was.
     *
     * The tree of a designed page passes LeafPolicy::checkTree() with
     * designedKeys() for the design compiler's breakpoints, and a plain
     * outline's with ALLOWED_KEYS for $breakpoints; a plain row stacks on a
     * phone only when the mobile breakpoint is among them.
     *
     * @param array<string, mixed>          $spec        Validated page-create spec (its outline).
     * @param IdSeed                        $ids         Node ids for this request.
     * @param array<mixed>                  $mediaById   Media facts by attachment id.
     * @param bool                          $containers  Whether the site has Elementor containers on.
     * @param ElementorSectionCompiler|null $design      The compiler of a designed page (outline version 3); null for a plain outline.
     * @param list<string>|null             $breakpoints A plain outline's site's active breakpoints (ElementorFacts active_breakpoints), null when none is; a designed page takes its compiler's.
     * @return array{tree?: list<array<string, mixed>>, code?: string, detail?: string}
     */
    public static function map(array $spec, IdSeed $ids, array $mediaById, bool $containers, ?ElementorSectionCompiler $design = null, ?array $breakpoints = self::STOCK_BREAKPOINTS): array
    {
        $outline = $spec['outline'] ?? null;
        if (!is_array($outline) || $outline === [] || !ArrayShape::isList($outline)) {
            return ['code' => 'bad_input', 'detail' => 'outline: not a list of nodes'];
        }
        $mapper = new self($ids, $mediaById, $containers, $design, $breakpoints);
        $tree   = $mapper->outline($outline);
        if ($tree === null) {
            return $mapper->refusal ?? ['code' => 'bad_input', 'detail' => 'outline: could not be mapped'];
        }
        $check = LeafPolicy::checkTree($tree, $mapper->allowlist(), self::LEAF_RULES);
        if ($check !== null) {
            return $check;
        }

        return ['tree' => $tree];
    }

    /**
     * The builder-neutral projection of a stored classic tree.
     *
     * A node is classified when its type is one this mapper writes, it holds
     * no dynamic content, custom code or attributes, and (for a text editor)
     * its HTML is exactly one of WPMgr's templates. Everything else is locked,
     * labelled from LOCKED_LABELS, and its children are not walked. Only the
     * fields set_text may change are offered as editable, and only those
     * fields' current text appears under from_the_site, with character
     * references decoded.
     *
     * A container whose Navigator label names a WPMgr pattern is a pattern
     * node while its subtree is the shape the compiler builds
     * (ElementorPatternReader): it carries its variant and offers its items'
     * text by path, and an item pattern's children are not walked. When the
     * subtree is any other shape the node is locked (LABEL_PATTERN_EDITED).
     *
     * @param array<mixed> $tree Decoded stored tree.
     * @return Projection
     * @throws \InvalidArgumentException When an element is not an object or has no usable, unique id.
     */
    public static function project(array $tree): Projection
    {
        $projection = new Projection(self::LOCKED_LABELS);
        self::projectNodes($tree, Projection::ROOT, '', $projection);

        return $projection;
    }

    /**
     * Map a validated outline to the new nodes of one page-edit insert or
     * replace, for the place they go:
     *
     *   CONTEXT_TOP    the top of the page: as map() does for a new page;
     *                  a run of leaves is wrapped, a group and a columns node
     *                  are top-level elements
     *   CONTEXT_GROUP  inside a top-level element that holds widgets (a
     *                  group container, or a column of a top-level section):
     *                  each leaf is its widgets with no wrapper, a columns
     *                  node is an inner row, and a group is refused
     *                  (layout_invalid)
     *   CONTEXT_INNER  inside an inner element, such as a column of a row:
     *                  leaves only (layout_invalid for a group or columns)
     *
     * So an edit nests no deeper than a created page: group, then columns,
     * then column, then leaf. $containers is the layout of the new nodes:
     * containers, or sections and columns. Each node path, which keys the
     * node's id and names it in a refusal, is $pathPrefix followed by
     * "outline[i]…", so the operations of one request never share a path.
     * Every rule of map() holds, and the new nodes pass
     * LeafPolicy::checkTree() with ALLOWED_KEYS for $breakpoints and
     * LEAF_RULES.
     *
     * @param array<mixed>      $outline     Validated outline nodes, in page-create's normalised form.
     * @param IdSeed            $ids         Node ids for this request.
     * @param array<mixed>      $mediaById   Media facts by attachment id.
     * @param bool              $containers  Whether the new nodes are containers.
     * @param string            $context     One of the CONTEXT_ values.
     * @param string            $pathPrefix  Prefix of every node path, e.g. "operations[2].".
     * @param list<string>|null $breakpoints The site's active breakpoints (ElementorFacts active_breakpoints), null when none is.
     * @param ElementorSectionCompiler|null $design The compiler of a designed page, when the edit carries the page's design: sections, patterns and groups compile in its look.
     * @param array{tone:string,align:string}|null $host The tone and alignment of the section the new nodes go into, for CONTEXT_GROUP with a design.
     * @return array{tree?: list<array<string, mixed>>, code?: string, detail?: string}
     */
    public static function mapFragment(array $outline, IdSeed $ids, array $mediaById, bool $containers, string $context, string $pathPrefix, ?array $breakpoints = self::STOCK_BREAKPOINTS, ?ElementorSectionCompiler $design = null, ?array $host = null): array
    {
        $listPath = $pathPrefix . 'outline';
        if ($outline === [] || !ArrayShape::isList($outline)) {
            return ['code' => 'bad_input', 'detail' => $listPath . ': not a list of nodes'];
        }
        $mapper = new self($ids, $mediaById, $containers, $design, $breakpoints);
        switch ($context) {
            case self::CONTEXT_TOP:
                $tree = $mapper->outline($outline, $listPath);
                break;
            case self::CONTEXT_GROUP:
                $tree = $design !== null && $host !== null
                    ? $mapper->designedChildren($outline, $listPath, $host['tone'], $host['align'])
                    : $mapper->groupChildren($outline, $listPath);
                break;
            case self::CONTEXT_INNER:
                $run = [];
                foreach ($outline as $i => $node) {
                    $run[] = [$listPath . '[' . $i . ']', $node];
                }
                $tree = $mapper->leafList($run);
                break;
            default:
                return ['code' => 'bad_input', 'detail' => $listPath . ': not a place new nodes can go'];
        }
        if ($tree === null) {
            return $mapper->refusal ?? ['code' => 'bad_input', 'detail' => $listPath . ': could not be mapped'];
        }
        $check = LeafPolicy::checkTree($tree, $mapper->allowlist(), self::LEAF_RULES);
        if ($check !== null) {
            return $check;
        }

        return ['tree' => $tree];
    }

    /**
     * The leaves of one designed section, each as its widgets, in the design's
     * look.
     *
     * A heading, paragraph, list, quote and table take the section's tone and
     * alignment from $look; a level 1 heading is allowed, and the first
     * paragraph after it is the lead. Everything else about a leaf is as in
     * map(): the same text rules, the same refusals, the same ids from $ids
     * keyed by the leaf's path ($listPath, then "[i]"). Leaves only: a group
     * or columns is layout_invalid.
     *
     * The widgets pass LeafPolicy::checkTree() with designedKeys(), the one
     * allowlist of a designed page.
     *
     * @param array<mixed>      $leaves    The section's leaves, in page-create's normalised form.
     * @param IdSeed            $ids       Node ids for this request.
     * @param array<mixed>      $mediaById Media facts by attachment id.
     * @param ElementorLeafStyle $look     The design's look.
     * @param string            $tone      One of OutlineV3Schema::TONES.
     * @param string            $align     One of OutlineV3Schema::ALIGNS.
     * @param string            $listPath  Path of the list of leaves, e.g. "outline[2].children".
     * @return array{tree?: list<array<string, mixed>>, code?: string, detail?: string}
     */
    public static function mapDesignedLeaves(array $leaves, IdSeed $ids, array $mediaById, ElementorLeafStyle $look, string $tone, string $align, string $listPath): array
    {
        if ($leaves === [] || !ArrayShape::isList($leaves)) {
            return ['code' => 'bad_input', 'detail' => $listPath . ': not a list of nodes'];
        }
        if (!in_array($tone, OutlineV3Schema::TONES, true) || !in_array($align, OutlineV3Schema::ALIGNS, true)) {
            return ['code' => 'bad_input', 'detail' => $listPath . ': a section tone is ground, surface, inverse or accent, and its alignment start or center'];
        }
        $mapper        = new self($ids, $mediaById, true, null);
        $mapper->look  = $look;
        $mapper->tone  = $tone;
        $mapper->align = $align;
        $run           = [];
        foreach ($leaves as $i => $node) {
            $run[] = [$listPath . '[' . $i . ']', $node];
        }
        $tree = $mapper->leafList($run);
        if ($tree === null) {
            return $mapper->refusal ?? ['code' => 'bad_input', 'detail' => $listPath . ': could not be mapped'];
        }
        $check = LeafPolicy::checkTree($tree, self::designedKeys(), self::LEAF_RULES);
        if ($check !== null) {
            return $check;
        }

        return ['tree' => $tree];
    }

    /**
     * A stored node with one field's text changed, for a page-edit set_text.
     *
     * Only the field's own settings path is written, with the bytes create
     * writes for that text; every other key of the node keeps its value and
     * its place, so styling a person set stays:
     *
     *   heading    text     settings.title      LeafPolicy::htmlText(text)
     *   paragraph  text     settings.editor     "<p>" . htmlText(text) . "</p>"
     *   buttons    text     settings.text       htmlText(text)
     *   buttons    url      settings.link.url   the link as given; the other link members stay
     *   image      caption  settings.caption    htmlText(text)
     *
     * Before the write: a text passes the page-create text rule (L1), a link
     * the page-create link rule (bad_input), and create's Elementor rules
     * hold: a paragraph that is only a web address (create_content_invalid)
     * and a link holding "&" (link_invalid) are refused. The value written
     * passes its own pin and the stored-string rules (L4 and L2) on its
     * settings path alone. The rest of the node is not held to ALLOWED_KEYS,
     * because it carries what a person set.
     *
     * On a designed page ($designed, see holdsDesignedSection()) a link may
     * also be "#" and a section anchor, as the grammar of a designed page lets
     * a button's address be. The designed allowlist's link pin then judges the
     * whole link, so the link is checked with the members the node keeps; on
     * any other page the link rule and the allowlist are what they always were.
     *
     * @param array<mixed> $node     The stored node.
     * @param string       $kind     Its projection kind.
     * @param string       $field    The field, one the node offers.
     * @param string       $text     The new text, or the new link for "url".
     * @param string       $at       The operation's position, for refusal details.
     * @param bool         $designed Whether the page the node is on is a designed page.
     * @return array{node?: array<mixed>, code?: string, detail?: string}
     */
    public static function setText(array $node, string $kind, string $field, string $text, string $at, bool $designed = false): array
    {
        $target   = self::TEXT_TARGETS[$kind][$field] ?? null;
        $type     = ($node['elType'] ?? null) === 'widget' ? ($node['widgetType'] ?? null) : null;
        $settings = $node['settings'] ?? null;
        if ($target === null || $type !== $target[0] || !is_array($settings)) {
            return ['code' => 'node_not_editable', 'detail' => $at . ': this node does not offer ' . $field];
        }
        [$key, $member] = [$target[1][0], $target[1][1] ?? null];
        $holder         = $member === null ? $settings : ($settings[$key] ?? null);
        if (!is_array($holder) || !is_string($holder[$member ?? $key] ?? null)) {
            return ['code' => 'node_not_editable', 'detail' => $at . ': this node does not offer ' . $field];
        }

        if ($field === 'url') {
            $why = PageCreateBuilder::linkProblem($text, $designed);
            if ($why !== null) {
                return ['code' => 'bad_input', 'detail' => $at . '.text: ' . $why];
            }
            if (strpos($text, '&') !== false) {
                return ['code' => 'link_invalid', 'detail' => $at . '.text: a link on an Elementor page cannot hold "&"; use a link without one'];
            }
            $value = $text;
        } else {
            $why = LeafPolicy::textProblem($text, 'text');
            if ($why !== null) {
                return ['code' => 'bad_input', 'detail' => $at . '.text: ' . $why];
            }
            if ($kind === 'paragraph' && preg_match(self::OWN_PARAGRAPH_ADDRESS, $text) === 1) {
                return ['code' => 'create_content_invalid', 'detail' => $at . '.text: a paragraph that is only a web address becomes an embedded player on an Elementor page; add words around the address or use a button'];
            }
            $value = $kind === 'paragraph' ? '<p>' . LeafPolicy::htmlText($text) . '</p>' : LeafPolicy::htmlText($text);
        }

        // L4 and L2 on the value at its own path: a node holding only that value.
        $allowed = self::ALLOWED_KEYS;
        $only    = $member === null ? [$key => $value] : [$key => [$member => $value]];
        if ($designed && $field === 'url') {
            // A designed page's link, whole: the designed allowlist's link pin takes a section anchor and judges the members the node keeps.
            $link          = $holder;
            $link[$member] = $value;
            $allowed       = self::designedKeys();
            $only          = [$key => $link];
        }
        $check = LeafPolicy::checkTree([['id' => 'set', 'elType' => 'widget', 'settings' => $only, 'elements' => [], 'widgetType' => $type]], $allowed, self::LEAF_RULES);
        if ($check !== null) {
            $reason = strpos($check['detail'], ': ');

            return ['code' => $check['code'], 'detail' => $at . '.text: ' . ($reason === false ? $check['detail'] : substr($check['detail'], $reason + 2))];
        }

        if ($member === null) {
            $settings[$key] = $value;
        } else {
            $holder[$member] = $value;
            $settings[$key]  = $holder;
        }
        $node['settings'] = $settings;

        return ['node' => $node];
    }

    /**
     * A stored pattern node with one piece of its text changed, for a
     * page-edit set_text on a pattern path such as plans.1.price.
     *
     * The path is one ElementorPatternReader offers for the node as it is
     * stored; only that widget's own settings path is written, in the form the
     * compiler writes it, and every other key of the node keeps its value and
     * its place. A text passes page-create's text rule, a link its link rule
     * (a link to a section of the page, "#" and its anchor, included), a number
     * is a whole number of at most nine digits, and the value written passes
     * its own pin and the stored-string rules on its settings path alone.
     *
     * @param array<mixed> $node The stored pattern container.
     * @param string       $path The pattern path.
     * @param string       $text The new text, or the new link.
     * @param string       $at   The operation's position, for refusal details.
     * @return array{node?: array<mixed>, code?: string, detail?: string}
     */
    public static function setPatternText(array $node, string $path, string $text, string $at): array
    {
        $label  = is_array($node['settings'] ?? null) ? ($node['settings'][LeafPolicy::LABEL_KEY] ?? null) : null;
        $named  = is_string($label) ? ElementorPatternCompiler::fromLabel($label) : null;
        $fields = $named === null ? null : ElementorPatternReader::fields($node, $named['pattern'], $named['variant']);
        $field  = $fields[$path] ?? null;
        if ($field === null) {
            return ['code' => 'node_not_editable', 'detail' => $at . ': this node does not offer ' . $path];
        }
        $form = $field['form'];
        if ($form === 'url') {
            $why = PageCreateBuilder::linkProblem($text, true);
            if ($why !== null) {
                return ['code' => 'bad_input', 'detail' => $at . '.text: ' . $why];
            }
            if (strpos($text, '&') !== false) {
                return ['code' => 'link_invalid', 'detail' => $at . '.text: a link on an Elementor page cannot hold "&"; use a link without one'];
            }
            $value = $text;
        } elseif ($form === 'number') {
            if (preg_match('/^[0-9]{1,9}$/D', $text) !== 1) {
                return ['code' => 'bad_input', 'detail' => $at . '.text: this is a whole number of at most 9 digits'];
            }
            $value = $text;
        } else {
            $why = LeafPolicy::textProblem($text, 'text');
            if ($why !== null) {
                return ['code' => 'bad_input', 'detail' => $at . '.text: ' . $why];
            }
            $value = $form === 'paragraph' ? '<p>' . LeafPolicy::htmlText($text) . '</p>' : LeafPolicy::htmlText($text);
        }

        $type    = null;
        $after   = [];
        $written = self::rewrite($node, $field['id'], static function (array $widget) use ($field, $value, &$type, &$after): ?array {
            $type     = $widget['widgetType'] ?? null;
            $settings = $widget['settings'] ?? null;
            if (!is_array($settings)) {
                return null;
            }
            $widget['settings'] = self::putAt($settings, $field['at'], $value);
            $after              = $widget['settings'];

            return $widget;
        });
        if ($written === null || !is_string($type)) {
            return ['code' => 'node_not_editable', 'detail' => $at . ': this node does not offer ' . $path];
        }

        // The value with the rest of its setting (a link keeps its other members): a widget holding only that setting.
        $top   = $field['at'][0];
        $only  = [$top => $after[$top] ?? null];
        $check = LeafPolicy::checkTree([['id' => 'set', 'elType' => 'widget', 'settings' => $only, 'elements' => [], 'widgetType' => $type]], self::designedKeys(), self::LEAF_RULES);
        if ($check !== null) {
            $reason = strpos($check['detail'], ': ');

            return ['code' => $check['code'], 'detail' => $at . '.text: ' . ($reason === false ? $check['detail'] : substr($check['detail'], $reason + 2))];
        }

        return ['node' => $written];
    }

    /**
     * The element with an id, rewritten by $change, inside a stored subtree.
     *
     * @param array<mixed>                        $node   The subtree.
     * @param string                              $id     The element's id.
     * @param callable(array<mixed>): (array<mixed>|null) $change The rewrite of the element; null refuses.
     * @return array<mixed>|null The subtree, or null when the element is not there or $change refused.
     */
    private static function rewrite(array $node, string $id, callable $change): ?array
    {
        if (($node['id'] ?? null) === $id) {
            return $change($node);
        }
        $children = $node['elements'] ?? null;
        if (!is_array($children)) {
            return null;
        }
        foreach ($children as $k => $child) {
            if (!is_array($child)) {
                continue;
            }
            $done = self::rewrite($child, $id, $change);
            if ($done !== null) {
                $children[$k]     = $done;
                $node['elements'] = $children;

                return $node;
            }
        }

        return null;
    }

    /**
     * $settings with $value at the settings path $at.
     *
     * @param array<mixed>      $settings The settings.
     * @param list<int|string>  $at       The path.
     * @param string            $value    The value.
     * @return array<mixed>
     */
    private static function putAt(array $settings, array $at, string $value): array
    {
        $step = array_shift($at);
        if ($step === null) {
            return $settings;
        }
        if ($at === []) {
            $settings[$step] = $value;

            return $settings;
        }
        $below            = is_array($settings[$step] ?? null) ? $settings[$step] : [];
        $settings[$step] = self::putAt($below, $at, $value);

        return $settings;
    }

    // ---------------------------------------------------------------------
    // Mapping
    // ---------------------------------------------------------------------

    /**
     * Top-level nodes, as on a new page.
     *
     * @param list<mixed> $outline  Outline nodes.
     * @param string      $listPath Path of the outline; a node's path is this plus "[i]".
     * @return list<array<string, mixed>>|null
     */
    private function outline(array $outline, string $listPath = 'outline'): ?array
    {
        // An outline of sections is a designed page; an outline of blocks, groups and
        // columns takes the design's containers for its groups and rows, if it has one.
        if ($this->design !== null && self::holdsSection($outline)) {
            return $this->designedPage($outline, $listPath);
        }
        $tree  = [];
        $run   = [];
        $start = '';
        foreach ($outline as $i => $node) {
            $path = $listPath . '[' . $i . ']';
            if (!is_array($node)) {
                return $this->fail('bad_input', $path . ': not a node');
            }
            $type = $node['type'] ?? null;
            if ($type === 'group' || $type === 'columns') {
                if ($run !== []) {
                    $wrapper = $this->wrapper($run, $start);
                    if ($wrapper === null) {
                        return null;
                    }
                    $tree[] = $wrapper;
                    $run    = [];
                }
                $built = $type === 'group' ? $this->group($node, $path) : $this->columns($node, $path, false);
                if ($built === null) {
                    return null;
                }
                $tree[] = $built;
                continue;
            }
            if ($run === []) {
                $start = $path;
            }
            $run[] = [$path, $node];
        }
        if ($run !== []) {
            $wrapper = $this->wrapper($run, $start);
            if ($wrapper === null) {
                return null;
            }
            $tree[] = $wrapper;
        }

        return $tree;
    }

    /**
     * Whether any top-level node of the outline is a section.
     *
     * @param list<mixed> $outline Outline nodes.
     * @return bool
     */
    private static function holdsSection(array $outline): bool
    {
        foreach ($outline as $node) {
            if (is_array($node) && ($node['type'] ?? null) === 'section') {
                return true;
            }
        }

        return false;
    }

    /**
     * The sections of a designed page, each as its container.
     *
     * @param list<mixed> $outline  Outline version 3: sections.
     * @param string      $listPath Path of the outline; a section's path is this plus "[i]".
     * @return list<array<string, mixed>>|null
     */
    private function designedPage(array $outline, string $listPath): ?array
    {
        if (!$this->designAllowed($listPath, 'a designed page')) {
            return null;
        }
        $tree = [];
        foreach ($outline as $i => $section) {
            $path = $listPath . '[' . $i . ']';
            if (!is_array($section) || ($section['type'] ?? null) !== 'section') {
                return $this->fail('layout_invalid', $path . ': every top-level item of a designed page is a section');
            }
            $built = $this->designedSection($section, $path);
            if ($built === null) {
                return null;
            }
            $tree[] = $built;
        }

        return $tree;
    }

    /**
     * One section as its container: its leaves as widgets in the design's
     * look and the section's tone and alignment, its groups and columns as
     * the design's containers, and its patterns as the pattern compiler's.
     * A hero and a call to action hold blocks, which are built as the
     * section's leaves are, in the alignment the pattern's variant asks for.
     *
     * @param array<mixed> $section A section of the validated outline.
     * @param string       $path    Its outline path.
     * @return array<string, mixed>|null
     */
    private function designedSection(array $section, string $path): ?array
    {
        $design   = $this->design;
        $children = $section['children'] ?? null;
        if ($design === null || !is_array($children) || $children === [] || !ArrayShape::isList($children)) {
            return $this->fail('bad_input', $path . '.children: not a list of nodes');
        }
        $align = $section['align'] ?? 'start';
        if (!is_string($align) || !in_array($align, OutlineV3Schema::ALIGNS, true)) {
            return $this->fail('bad_input', $path . '.align: a section alignment is start or center');
        }
        try {
            $tone = $design->tone($section);
        } catch (\InvalidArgumentException $e) {
            return $this->fail('bad_input', $path . '.tone: a section tone is ground, surface, inverse or accent');
        }
        $elements = $this->designedChildren($children, $path . '.children', $tone, $align);
        if ($elements === null) {
            return null;
        }

        try {
            return $design->container($this->ids->next($path), $section, $elements);
        } catch (\InvalidArgumentException $e) {
            return $this->fail('bad_input', $path . ': the section could not be built');
        }
    }

    /**
     * The children of a designed section, or the nodes an edit puts into one:
     * leaves as widgets in the design's look and the section's tone and
     * alignment, groups and columns as the design's containers, patterns as the
     * pattern compiler's.
     *
     * @param list<mixed> $children The validated nodes.
     * @param string      $listPath Path of the list; a node's path is this plus "[i]".
     * @param string      $tone     One of OutlineV3Schema::TONES.
     * @param string      $align    One of OutlineV3Schema::ALIGNS.
     * @return list<array<string, mixed>>|null
     */
    private function designedChildren(array $children, string $listPath, string $tone, string $align): ?array
    {
        $design = $this->design;
        if ($design === null || $children === [] || !ArrayShape::isList($children)) {
            return $this->fail('bad_input', $listPath . ': not a list of nodes');
        }
        if (!in_array($tone, OutlineV3Schema::TONES, true) || !in_array($align, OutlineV3Schema::ALIGNS, true)) {
            return $this->fail('bad_input', $listPath . ': a section tone is ground, surface, inverse or accent, and its alignment start or center');
        }
        $this->look     = new ElementorLeafStyle($design->tokens());
        $this->tone     = $tone;
        $this->align    = $align;
        $this->leadNext = false;
        $patterns       = new ElementorPatternCompiler($design->tokens(), $design, $this->media);

        $elements = [];
        $run      = [];
        foreach ($children as $j => $child) {
            $at   = $listPath . '[' . $j . ']';
            $type = is_array($child) ? ($child['type'] ?? null) : null;
            if ($type !== 'group' && $type !== 'columns' && $type !== 'pattern') {
                $run[] = [$at, $child];
                continue;
            }
            if ($run !== []) {
                $widgets = $this->leafList($run);
                if ($widgets === null) {
                    return null;
                }
                array_push($elements, ...$widgets);
                $run = [];
            }
            $this->leadNext = false;
            $built          = match ($type) {
                'group'   => $this->group($child, $at),
                'columns' => $this->columns($child, $at, false),
                default   => $this->designedPattern($patterns, $child, $at, $tone),
            };
            if ($built === null) {
                return null;
            }
            $elements[] = $built;
        }
        if ($run !== []) {
            $widgets = $this->leafList($run);
            if ($widgets === null) {
                return null;
            }
            array_push($elements, ...$widgets);
        }


        return $elements;
    }

    /**
     * One pattern as its container. The blocks of a hero or a call to action
     * are built first, as a section's leaves are, in the alignment of the
     * pattern's variant.
     *
     * @param ElementorPatternCompiler $patterns The page's pattern compiler.
     * @param array<mixed>             $pattern  A pattern node of the validated outline.
     * @param string                   $path     Its outline path.
     * @param string                   $tone     The tone of the section it stands in.
     * @return array<string, mixed>|null
     */
    private function designedPattern(ElementorPatternCompiler $patterns, array $pattern, string $path, string $tone): ?array
    {
        $name   = $pattern['pattern'] ?? null;
        $blocks = [];
        if ($name === 'hero' || $name === 'cta') {
            $variant = $pattern['variant'] ?? null;
            $align   = is_string($name) && is_string($variant) ? ElementorPatternCompiler::align($name, $variant) : null;
            $leaves  = $pattern['children'] ?? null;
            if ($align === null || $this->look === null || !is_array($leaves)) {
                return $this->fail('bad_input', $path . ': a ' . (string) $name . ' holds blocks and has a variant of its own');
            }
            $r = self::mapDesignedLeaves($leaves, $this->ids, $this->media, $this->look, $tone, $align, $path . '.children');
            if (!isset($r['tree'])) {
                return $this->fail((string) ($r['code'] ?? 'bad_input'), (string) ($r['detail'] ?? $path . '.children: could not be mapped'));
            }
            $blocks = $r['tree'];
        }
        try {
            return $patterns->compile($this->ids, $path, $pattern, $tone, $blocks);
        } catch (\InvalidArgumentException $e) {
            return $this->fail('bad_input', $path . ': the pattern could not be built');
        }
    }

    /**
     * A run of top-level leaves in one wrapper.
     *
     * @param list<array{0: string, 1: array<mixed>}> $run   Leaves with their paths.
     * @param string                                  $start Path of the first leaf.
     * @return array<string, mixed>|null
     */
    private function wrapper(array $run, string $start): ?array
    {
        $key = $start . '#wrapper';
        $id  = $this->ids->next($key);
        if (!$this->containers) {
            $columnId = $this->ids->next($key . '#column');
            $elements = $this->leafList($run);
            if ($elements === null) {
                return null;
            }

            return self::layout($id, 'section', ['structure' => self::STRUCTURES[1]], [
                self::layout($columnId, 'column', ['_column_size' => self::COLUMN_SIZES[1]], $elements, false),
            ], false);
        }
        $elements = $this->leafList($run);
        if ($elements === null) {
            return null;
        }

        return self::layout($id, 'container', ['content_width' => 'boxed', 'flex_direction' => 'column'], $elements, false);
    }

    /**
     * @param array<mixed> $node Group node.
     * @param string       $path Its outline path.
     * @return array<string, mixed>|null
     */
    private function group(array $node, string $path): ?array
    {
        $children = $node['children'] ?? null;
        if (!is_array($children) || $children === [] || !ArrayShape::isList($children)) {
            return $this->fail('bad_input', $path . '.children: not a list of nodes');
        }
        $id       = $this->ids->next($path);
        $columnId = $this->containers ? null : $this->ids->next($path . '#column');
        $elements = $this->groupChildren($children, $path . '.children');
        if ($elements === null) {
            return null;
        }
        if ($columnId === null) {
            return $this->design !== null
                ? $this->design->group($id, $elements)
                : self::layout($id, 'container', ['content_width' => 'boxed', 'flex_direction' => 'column'], $elements, false);
        }

        return self::layout($id, 'section', ['structure' => self::STRUCTURES[1]], [
            self::layout($columnId, 'column', ['_column_size' => self::COLUMN_SIZES[1]], $elements, false),
        ], false);
    }

    /**
     * What a group holds: each leaf as its widgets, and a columns node as an
     * inner row. A group inside it is refused.
     *
     * @param list<mixed> $children Child nodes.
     * @param string      $listPath Path of the list; a child's path is this plus "[j]".
     * @return list<array<string, mixed>>|null
     */
    private function groupChildren(array $children, string $listPath): ?array
    {
        $elements = [];
        foreach ($children as $j => $child) {
            $at = $listPath . '[' . $j . ']';
            if (!is_array($child)) {
                return $this->fail('bad_input', $at . ': not a node');
            }
            $type = $child['type'] ?? null;
            if ($type === 'group') {
                return $this->fail('layout_invalid', $at . ': a group cannot hold a group');
            }
            if ($type === 'columns') {
                $built = $this->columns($child, $at, true);
                if ($built === null) {
                    return null;
                }
                $elements[] = $built;
                continue;
            }
            $widgets = $this->leaf($child, $at);
            if ($widgets === null) {
                return null;
            }
            array_push($elements, ...$widgets);
        }

        return $elements;
    }

    /**
     * @param array<mixed> $node   Columns node.
     * @param string       $path   Its outline path.
     * @param bool         $nested Whether it sits inside a group.
     * @return array<string, mixed>|null
     */
    private function columns(array $node, string $path, bool $nested): ?array
    {
        $columns = $node['columns'] ?? null;
        if (!is_array($columns) || !ArrayShape::isList($columns) || count($columns) < 2 || count($columns) > 4) {
            return $this->fail('bad_input', $path . '.columns: not a list of 2 to 4 columns');
        }
        $count  = count($columns);
        $widths = $node['widths'] ?? null;
        if ($widths !== null) {
            if (!is_array($widths) || !ArrayShape::isList($widths) || count($widths) !== $count) {
                return $this->fail('bad_input', $path . '.widths: not one width per column');
            }
            foreach ($widths as $w) {
                if (!is_int($w) || $w < 10 || $w > 90) {
                    return $this->fail('bad_input', $path . '.widths: each must be a whole number from 10 to 90');
                }
            }
        }

        $id    = $this->ids->next($path);
        $built = [];
        foreach ($columns as $k => $children) {
            $at = $path . '.columns[' . $k . ']';
            if (!is_array($children) || $children === [] || !ArrayShape::isList($children)) {
                return $this->fail('bad_input', $at . ': not a list of nodes');
            }
            $columnId = $this->ids->next($at);
            $run      = [];
            foreach ($children as $m => $child) {
                $run[] = [$at . '.children[' . $m . ']', $child];
            }
            $elements = $this->leafList($run);
            if ($elements === null) {
                return null;
            }
            if ($this->containers && $this->design !== null) {
                $built[] = $this->design->column($columnId, $widths === null ? intdiv(100, $count) : (int) $widths[$k], $elements);
                continue;
            }
            if ($this->containers) {
                $size    = $widths === null ? intdiv(100, $count) : (int) $widths[$k];
                $built[] = self::layout($columnId, 'container', [
                    'content_width'  => 'full',
                    'flex_direction' => 'column',
                    'width'          => ['unit' => '%', 'size' => (string) $size, 'sizes' => []],
                ], $elements, true);
                continue;
            }
            $settings = ['_column_size' => self::COLUMN_SIZES[$count]];
            if ($widths !== null) {
                $settings['_inline_size'] = (string) (int) $widths[$k];
            }
            $built[] = self::layout($columnId, 'column', $settings, $elements, $nested);
        }

        if ($this->containers && $this->design !== null) {
            return $this->design->row($id, $built);
        }
        if ($this->containers) {
            $settings = ['content_width' => $nested ? 'full' : 'boxed', 'flex_direction' => 'row'];
            // The row stacks on a phone only where the site has the mobile breakpoint active.
            if (in_array('mobile', $this->active, true)) {
                $settings['flex_direction_mobile'] = 'column';
            }
            $settings['flex_gap'] = ['unit' => 'px', 'size' => self::GAP_PX, 'column' => self::GAP_PX, 'row' => self::GAP_PX];

            return self::layout($id, 'container', $settings, $built, $nested);
        }

        return self::layout($id, 'section', ['structure' => self::STRUCTURES[$count]], $built, $nested);
    }

    /**
     * Leaves in order, each as one or more widgets.
     *
     * @param list<array{0: string, 1: mixed}> $run Leaves with their paths.
     * @return list<array<string, mixed>>|null
     */
    private function leafList(array $run): ?array
    {
        $elements = [];
        foreach ($run as [$at, $child]) {
            if (!is_array($child)) {
                return $this->fail('bad_input', $at . ': not a node');
            }
            $widgets = $this->leaf($child, $at);
            if ($widgets === null) {
                return null;
            }
            array_push($elements, ...$widgets);
        }

        return $elements;
    }

    /**
     * One outline leaf as widgets (a buttons node is one widget per button).
     *
     * @param array<mixed> $node Leaf node.
     * @param string       $path Its outline path.
     * @return list<array<string, mixed>>|null
     */
    private function leaf(array $node, string $path): ?array
    {
        $type = $node['type'] ?? null;
        if (in_array($type, self::DESIGN_NODES, true) && !$this->designAllowed($path, 'a ' . $type)) {
            return null;
        }
        switch ($type) {
            case 'heading':
                $level = $node['level'] ?? null;
                $text  = $node['text'] ?? null;
                // A designed page has one level 1 heading, in its hero.
                $sizes = $this->look === null ? self::HEADER_SIZES : [1 => 'h1'] + self::HEADER_SIZES;
                if (!is_int($level) || !isset($sizes[$level]) || !is_string($text)) {
                    return $this->fail('bad_input', $path . ($this->look === null ? ': a heading needs a level of 2, 3 or 4 and a text' : ': a heading needs a level of 1, 2, 3 or 4 and a text'));
                }
                $settings = ['title' => LeafPolicy::htmlText($text), 'header_size' => $sizes[$level]];
                if ($this->look !== null) {
                    $settings       = array_merge($settings, $this->look->headingStyle($level, $this->tone, $this->align));
                    $this->leadNext = $this->leadNext || $level === 1;
                }

                return [$this->widget($path, 'heading', $settings)];
            case 'paragraph':
                $text = $node['text'] ?? null;
                if (!is_string($text)) {
                    return $this->fail('bad_input', $path . '.text: not a string');
                }
                if (preg_match(self::OWN_PARAGRAPH_ADDRESS, $text) === 1) {
                    return $this->fail('create_content_invalid', $path . '.text: a paragraph that is only a web address becomes an embedded player on an Elementor page; add words around the address or use a button');
                }
                // The first paragraph after the title is the lead; every other is body text.
                $style          = $this->leadNext ? 'lead' : 'body';
                $this->leadNext = false;

                return [$this->textEditor($path, '<p>' . LeafPolicy::htmlText($text) . '</p>', $style)];
            case 'list':
                $items   = $node['items'] ?? null;
                $ordered = $node['ordered'] ?? null;
                if (!is_bool($ordered) || !self::isTextList($items, false)) {
                    return $this->fail('bad_input', $path . ': a list needs ordered and its items');
                }
                $tag  = $ordered ? 'ol' : 'ul';
                $html = '<' . $tag . '>';
                foreach ($items as $item) {
                    $html .= '<li>' . LeafPolicy::htmlText($item) . '</li>';
                }

                return [$this->textEditor($path, $html . '</' . $tag . '>')];
            case 'quote':
                return $this->quote($node, $path);
            case 'table':
                return $this->table($node, $path);
            case 'separator':
                return [$this->widget($path, 'divider', [])];
            case 'spacer':
                $size = $node['size'] ?? null;
                if (!is_string($size) || !isset(self::SPACER_PX[$size])) {
                    return $this->fail('bad_input', $path . '.size: must be small, medium or large');
                }

                return [$this->widget($path, 'spacer', [
                    'space' => ['unit' => 'px', 'size' => self::SPACER_PX[$size], 'sizes' => []],
                ])];
            case 'image':
                return $this->image($node, $path);
            case 'buttons':
                return $this->buttons($node, $path);
            case 'group':
            case 'columns':
                return $this->fail('layout_invalid', $path . ': a ' . $type . ' cannot be placed here');
        }

        return $this->fail('bad_input', $path . ': not a known node type');
    }

    /**
     * Whether a design-carrying node may be built here. A site that builds
     * sections and columns may not, and the refusal is recorded.
     *
     * @param string $path Its outline path.
     * @param string $what What it is, for the refusal.
     * @return bool
     */
    private function designAllowed(string $path, string $what): bool
    {
        if ($this->containers) {
            return true;
        }
        $this->fail(self::CODE_CONTAINERS_REQUIRED, $path . ': ' . $what . ' needs Elementor containers, and this site builds sections and columns');

        return false;
    }

    /**
     * @param array<mixed> $node Quote node.
     * @param string       $path Its outline path.
     * @return list<array<string, mixed>>|null
     */
    private function quote(array $node, string $path): ?array
    {
        $paragraphs = $node['paragraphs'] ?? null;
        $citation   = $node['citation'] ?? null;
        if (!self::isTextList($paragraphs, false) || ($citation !== null && !is_string($citation))) {
            return $this->fail('bad_input', $path . ': a quote needs its paragraphs and an optional citation');
        }
        $html = '<blockquote>';
        foreach ($paragraphs as $j => $paragraph) {
            if (preg_match(self::OWN_PARAGRAPH_ADDRESS, $paragraph) === 1) {
                return $this->fail('create_content_invalid', $path . '.paragraphs[' . $j . ']: a paragraph that is only a web address becomes an embedded player on an Elementor page; add words around the address');
            }
            $html .= '<p>' . LeafPolicy::htmlText($paragraph) . '</p>';
        }
        if (is_string($citation)) {
            $html .= '<cite>' . LeafPolicy::htmlText($citation) . '</cite>';
        }

        return [$this->textEditor($path, $html . '</blockquote>')];
    }

    /**
     * @param array<mixed> $node Table node.
     * @param string       $path Its outline path.
     * @return list<array<string, mixed>>|null
     */
    private function table(array $node, string $path): ?array
    {
        $header = $node['header'] ?? null;
        $rows   = $node['rows'] ?? null;
        if (($header !== null && !self::isTextList($header, true)) || !is_array($rows) || $rows === [] || !ArrayShape::isList($rows)) {
            return $this->fail('bad_input', $path . ': a table needs its rows and an optional header');
        }
        $html = '<table>';
        if (is_array($header)) {
            $html .= '<thead><tr>';
            foreach ($header as $cell) {
                $html .= '<th>' . LeafPolicy::htmlText($cell) . '</th>';
            }
            $html .= '</tr></thead>';
        }
        $html .= '<tbody>';
        foreach ($rows as $row) {
            if (!self::isTextList($row, true)) {
                return $this->fail('bad_input', $path . '.rows: each row must be a list of cells');
            }
            $html .= '<tr>';
            foreach ($row as $cell) {
                $html .= '<td>' . LeafPolicy::htmlText($cell) . '</td>';
            }
            $html .= '</tr>';
        }

        return [$this->textEditor($path, $html . '</tbody></table>')];
    }

    /**
     * @param array<mixed> $node Image node.
     * @param string       $path Its outline path.
     * @return list<array<string, mixed>>|null
     */
    private function image(array $node, string $path): ?array
    {
        $id      = $node['attachment_id'] ?? null;
        $alt     = $node['alt'] ?? null;
        $caption = $node['caption'] ?? null;
        $align   = $node['align'] ?? 'none';
        if (!is_int($id) || $id < 1 || $id > PageCreateBuilder::MAX_ATTACHMENT_ID || !is_string($alt) || ($caption !== null && !is_string($caption)) || !is_string($align)) {
            return $this->fail('bad_input', $path . ': an image needs an attachment id, alt text and optional caption and align');
        }
        if (!in_array($align, self::IMAGE_ALIGN, true)) {
            return $this->fail('node_not_supported_by_builder', $path . '.align: this alignment is not available in Elementor\'s image widget; the image alignments it builds are: ' . implode(', ', self::IMAGE_ALIGN));
        }
        $fact = $this->media[$id] ?? null;
        if (!is_array($fact) || !is_string($fact['url'] ?? null) || !is_string($fact['alt'] ?? null)) {
            return $this->fail('image_not_available', $path . '.attachment_id: the image facts are incomplete');
        }
        if (PageCreateBuilder::imageUrlProblem($fact['url']) !== null) {
            return $this->fail('image_url_unusable', $path . '.attachment_id: the site gave an image address that cannot be stored');
        }
        if ($alt !== $fact['alt']) {
            return $this->fail('image_alt_from_library', $path . '.alt: an Elementor image shows the alt text saved with the image in the media library; set alt to that text exactly, or change it in the media library first');
        }

        $settings = [
            'image'      => ['url' => $fact['url'], 'id' => (string) $id, 'source' => 'library'],
            'image_size' => 'large',
        ];
        if ($align !== 'none') {
            $settings['align'] = $align;
        }
        if (is_string($caption)) {
            $settings['caption_source'] = 'custom';
            $settings['caption']        = LeafPolicy::htmlText($caption);
        }

        return [$this->widget($path, 'image', $settings)];
    }

    /**
     * @param array<mixed> $node Buttons node.
     * @param string       $path Its outline path.
     * @return list<array<string, mixed>>|null
     */
    private function buttons(array $node, string $path): ?array
    {
        $align   = $node['align'] ?? 'left';
        $buttons = $node['buttons'] ?? null;
        if (!is_string($align) || !isset(self::BUTTON_ALIGN[$align]) || !is_array($buttons) || $buttons === [] || !ArrayShape::isList($buttons)) {
            return $this->fail('bad_input', $path . ': buttons need an align of left or center and a list of buttons');
        }
        // A designed section draws a button as a fill, an outline or text; the buttons stand in one row.
        $rowId   = $this->look === null ? null : $this->ids->next($path);
        $widgets = [];
        foreach ($buttons as $j => $button) {
            $at    = $path . '.buttons[' . $j . ']';
            $text  = is_array($button) ? ($button['text'] ?? null) : null;
            $url   = is_array($button) ? ($button['url'] ?? null) : null;
            $style = is_array($button) ? ($button['style'] ?? 'fill') : null;
            if (!is_string($text) || !is_string($url) || !is_string($style)) {
                return $this->fail('bad_input', $at . ': a button needs a text, a link and an optional style');
            }
            if ($this->look === null && $style !== 'fill') {
                return $this->fail('node_not_supported_by_builder', $at . '.style: an outline button is not available in Elementor; the button styles it builds are: fill');
            }
            if ($this->look !== null && !in_array($style, ElementorLeafStyle::BUTTON_STYLES, true)) {
                return $this->fail('bad_input', $at . '.style: a button style is fill, outline or text');
            }
            if (strpos($url, '&') !== false) {
                return $this->fail('link_invalid', $at . '.url: a link on an Elementor page cannot hold "&"; use a link without one');
            }
            $settings = ['text' => LeafPolicy::htmlText($text), 'link' => ['url' => $url, 'is_external' => '', 'nofollow' => '']];
            $settings = $this->look === null
                ? array_merge($settings, ['align' => self::BUTTON_ALIGN[$align]])
                : array_merge($settings, $this->look->buttonStyle($style, $this->tone));
            $widgets[] = $this->widget($at, 'button', $settings);
        }
        if ($this->look !== null && $rowId !== null) {
            return [$this->look->buttonRow($rowId, $widgets, self::BUTTON_ALIGN[$align])];
        }

        return $widgets;
    }

    /**
     * @param string $path  Outline path.
     * @param string $html  Template HTML.
     * @param string $style Text style of a designed page: body, lead or eyebrow.
     * @return array<string, mixed>
     */
    private function textEditor(string $path, string $html, string $style = 'body'): array
    {
        $settings = ['editor' => $html];
        if ($this->look !== null) {
            $settings = array_merge($settings, $this->look->textStyle($style, $this->tone, $this->align));
        }

        return $this->widget($path, 'text-editor', $settings);
    }

    /**
     * @param string       $path     Outline path, for the id.
     * @param string       $type     Widget type.
     * @param array<mixed> $settings Settings.
     * @return array<string, mixed>
     */
    private function widget(string $path, string $type, array $settings): array
    {
        return [
            'id'         => $this->ids->next($path),
            'elType'     => 'widget',
            'settings'   => $settings,
            'elements'   => [],
            'widgetType' => $type,
        ];
    }

    /**
     * @param string                     $id       Node id.
     * @param string                     $elType   container, section or column.
     * @param array<string, mixed>       $settings Settings.
     * @param list<array<string, mixed>> $elements Children.
     * @param bool                       $isInner  Whether it is an inner element.
     * @return array<string, mixed>
     */
    private static function layout(string $id, string $elType, array $settings, array $elements, bool $isInner): array
    {
        return [
            'id'       => $id,
            'elType'   => $elType,
            'settings' => $settings,
            'elements' => $elements,
            'isInner'  => $isInner,
        ];
    }

    /**
     * @param mixed $value      Value.
     * @param bool  $emptyCells Whether "" is a member.
     * @return bool
     * @phpstan-assert-if-true list<string> $value
     */
    private static function isTextList($value, bool $emptyCells): bool
    {
        if (!is_array($value) || $value === [] || !ArrayShape::isList($value)) {
            return false;
        }
        foreach ($value as $item) {
            if (!is_string($item) || ($item === '' && !$emptyCells)) {
                return false;
            }
        }

        return true;
    }

    /**
     * Record the first refusal.
     *
     * @param string $code   Code.
     * @param string $detail Detail: WPMgr's words and outline positions only.
     * @return null
     */
    private function fail(string $code, string $detail)
    {
        if ($this->refusal === null) {
            $this->refusal = ['code' => $code, 'detail' => $detail];
        }

        return null;
    }

    // ---------------------------------------------------------------------
    // Projection
    // ---------------------------------------------------------------------

    /**
     * @param array<mixed> $nodes      Stored elements.
     * @param string       $parent     Parent ref.
     * @param string       $parentKind Kind of the parent, or "" at the top.
     * @param Projection   $projection Projection being built.
     * @return void
     * @throws \InvalidArgumentException When an element is not an object or has no usable, unique id.
     */
    private static function projectNodes(array $nodes, string $parent, string $parentKind, Projection $projection): void
    {
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                throw new \InvalidArgumentException('a stored element is not an object');
            }
            $id = $node['id'] ?? null;
            if (!is_string($id)) {
                throw new \InvalidArgumentException('a stored element has no id');
            }
            $elType   = $node['elType'] ?? null;
            $type     = $elType === 'widget' ? ($node['widgetType'] ?? null) : $elType;
            $settings = $node['settings'] ?? [];
            if (!is_string($type) || !is_array($settings)) {
                $projection->addLocked($id, $parent, Projection::FALLBACK_LABEL);
                continue;
            }
            $known = isset(self::ALLOWED_KEYS[$type]) && ($elType === 'widget') === !in_array($type, ['container', 'section', 'column'], true);
            if (!$known) {
                $projection->addLocked($id, $parent, self::labelKey($type));
                continue;
            }
            if (self::holdsDynamicKey($settings, 1)) {
                $projection->addLocked($id, $parent, self::LABEL_DYNAMIC);
                continue;
            }
            $named = $type === 'container' ? self::patternOf($settings) : null;
            if ($named !== null) {
                self::projectPattern($node, $id, $parent, $named, $projection);
                continue;
            }

            // A designed section: a top-level container with WPMgr's section label and an anchor it could have been given.
            if ($type === 'container' && $parentKind === '' && ($settings[LeafPolicy::LABEL_KEY] ?? null) === LeafPolicy::SECTION_LABEL) {
                $anchor = $settings[LeafPolicy::ANCHOR_KEY] ?? null;
                if (is_string($anchor) && preg_match('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', $anchor) === 1 && !in_array($anchor, OutlineV3Schema::RESERVED_ANCHORS, true)) {
                    $projection->add($id, $parent, 'section', null, [], []);
                    $projection->note($id, ['anchor' => $anchor]);
                    $below = $node['elements'] ?? [];
                    if (is_array($below)) {
                        self::projectNodes($below, $id, 'section', $projection);
                    }
                    continue;
                }
            }

            $kind = self::classify($type, $settings, $parentKind);
            if ($kind === null) {
                $projection->addLocked($id, $parent, $type === 'text-editor' ? self::LABEL_CUSTOM_TEXT : self::labelKey($type));
                continue;
            }
            [$kindName, $level, $editable, $text] = $kind;
            $projection->add($id, $parent, $kindName, $level, $editable, $text);
            if ($kindName === 'buttons' && isset($text['url']) && strncmp($text['url'], '#', 1) === 0) {
                $projection->note($id, ['links' => [$text['url']]]);
            }

            if (in_array($type, ['container', 'section', 'column'], true)) {
                $children = $node['elements'] ?? [];
                if (is_array($children)) {
                    self::projectNodes($children, $id, $kindName, $projection);
                }
            }
        }
    }

    /**
     * The pattern and variant a container's Navigator label names.
     *
     * @param array<mixed> $settings The container's settings.
     * @return array{pattern:string,variant:string}|null
     */
    private static function patternOf(array $settings): ?array
    {
        $label = $settings[LeafPolicy::LABEL_KEY] ?? null;

        return is_string($label) ? ElementorPatternCompiler::fromLabel($label) : null;
    }

    /**
     * A container labelled as a WPMgr pattern: a pattern node when its subtree
     * is the shape the compiler builds (ElementorPatternReader), else locked,
     * its children not walked. An item pattern is one node that offers its
     * items by path; a hero or a call to action holds ordinary blocks, which
     * are walked and projected as any other.
     *
     * @param array<mixed>                       $node       The stored container.
     * @param string                             $id         Its id.
     * @param string                             $parent     Parent ref.
     * @param array{pattern:string,variant:string} $named      The pattern and variant of its label.
     * @param Projection                         $projection Projection being built.
     * @return void
     * @throws \InvalidArgumentException When an element below a hero or a call to action is not an object or has no usable, unique id.
     */
    private static function projectPattern(array $node, string $id, string $parent, array $named, Projection $projection): void
    {
        $fields = ElementorPatternReader::fields($node, $named['pattern'], $named['variant']);
        if ($fields === null) {
            $projection->addLocked($id, $parent, self::LABEL_PATTERN_EDITED);

            return;
        }
        $composed = in_array($named['pattern'], ElementorPatternCompiler::COMPOSED, true);
        if (!$composed && self::subtreeHoldsDynamicKey($node, 0)) {
            $projection->addLocked($id, $parent, self::LABEL_DYNAMIC);

            return;
        }
        $projection->addPattern($id, $parent, $named['variant'], array_map(static fn (array $field): string => $field['text'], $fields));
        $links = [];
        foreach ($fields as $field) {
            if ($field['form'] === 'url' && strncmp($field['text'], '#', 1) === 0) {
                $links[] = $field['text'];
            }
        }
        $projection->note($id, ['pattern' => $named['pattern']] + ($links === [] ? [] : ['links' => $links]));
        if ($composed && is_array($node['elements'] ?? null)) {
            // The two columns of a split hero are columns; the blocks of a stacked one are the pattern's own.
            self::projectNodes($node['elements'], $id, ($node['settings']['flex_direction'] ?? null) === 'row' ? 'columns' : 'pattern', $projection);
        }
    }

    /**
     * Whether any element below a node holds a key that makes it dynamic
     * content or code, or stands deeper than the projection follows.
     *
     * @param array<mixed> $node  A stored element.
     * @param int          $depth How deep it stands.
     * @return bool
     */
    private static function subtreeHoldsDynamicKey(array $node, int $depth): bool
    {
        $children = $node['elements'] ?? [];
        foreach (is_array($children) ? $children : [] as $child) {
            if (!is_array($child)) {
                continue;
            }
            $settings = $child['settings'] ?? [];
            if ($depth > 8 || (is_array($settings) && self::holdsDynamicKey($settings, 1)) || self::subtreeHoldsDynamicKey($child, $depth + 1)) {
                return true;
            }
        }

        return false;
    }

    /**
     * Kind, heading level, editable fields and their current text; null when
     * the element is not shaped as WPMgr writes it.
     *
     * @param string       $type       Element type.
     * @param array<mixed> $settings   Its settings.
     * @param string       $parentKind Kind of its parent.
     * @return array{0: string, 1: int|null, 2: list<string>, 3: array<string, string>}|null
     */
    private static function classify(string $type, array $settings, string $parentKind): ?array
    {
        switch ($type) {
            case 'container':
                if ($parentKind === 'columns') {
                    return ['column', null, [], []];
                }

                return [($settings['flex_direction'] ?? null) === 'row' ? 'columns' : 'group', null, [], []];
            case 'section':
                return ['section', null, [], []];
            case 'column':
                return ['column', null, [], []];
            case 'heading':
                $title = $settings['title'] ?? null;
                $size  = $settings['header_size'] ?? 'h2';
                if (!is_string($title) || !is_string($size)) {
                    return null;
                }
                $level = preg_match('/^h([1-6])$/D', $size, $m) === 1 ? (int) $m[1] : null;

                return ['heading', $level, ['text'], ['text' => self::decode($title)]];
            case 'text-editor':
                $html = $settings['editor'] ?? null;
                if (!is_string($html)) {
                    return null;
                }
                foreach (self::TEMPLATES as $kind => $pattern) {
                    if (preg_match($pattern, $html) !== 1) {
                        continue;
                    }
                    if ($kind === 'paragraph') {
                        return ['paragraph', null, ['text'], ['text' => self::decode(substr($html, 3, -4))]];
                    }

                    return [$kind, null, [], []];
                }

                return null;
            case 'button':
                $text = $settings['text'] ?? null;
                $url  = $settings['link']['url'] ?? null;
                if (!is_string($text) || !is_string($url)) {
                    return null;
                }

                return ['buttons', null, ['text', 'url'], ['text' => self::decode($text), 'url' => self::decode($url)]];
            case 'image':
                $caption = $settings['caption'] ?? null;
                if (($settings['caption_source'] ?? null) === 'custom' && is_string($caption)) {
                    return ['image', null, ['caption'], ['caption' => self::decode($caption)]];
                }

                return ['image', null, [], []];
            case 'divider':
                return ['separator', null, [], []];
            case 'spacer':
                return ['spacer', null, [], []];
        }

        return null;
    }

    /**
     * Whether settings hold a key that makes an element dynamic content or
     * code, at any depth.
     *
     * @param array<mixed> $settings Settings.
     * @param int          $depth    Nesting depth.
     * @return bool
     */
    private static function holdsDynamicKey(array $settings, int $depth): bool
    {
        if ($depth > 8) {
            return true;
        }
        foreach ($settings as $key => $value) {
            if (in_array(strtolower((string) $key), self::DYNAMIC_KEYS, true)) {
                return true;
            }
            if (is_array($value) && self::holdsDynamicKey($value, $depth + 1)) {
                return true;
            }
        }

        return false;
    }

    /**
     * The LOCKED_LABELS key for an element type the mapper does not write.
     *
     * @param string $type Element type.
     * @return string
     */
    private static function labelKey(string $type): string
    {
        if (isset(self::LOCKED_LABELS[$type])) {
            return $type;
        }
        if (strncmp($type, 'e-', 2) === 0) {
            return self::LABEL_ATOMIC;
        }
        if (strncmp($type, 'wp-widget-', 10) === 0) {
            return self::LABEL_WP_WIDGET;
        }

        return Projection::FALLBACK_LABEL;
    }

    /**
     * Stored text with its character references decoded.
     *
     * @param string $stored Stored text.
     * @return string
     */
    public static function decode(string $stored): string
    {
        return html_entity_decode($stored, ENT_QUOTES | ENT_HTML5, 'UTF-8');
    }

    /**
     * The text of a text editor holding exactly one paragraph of WPMgr's
     * template, with its character references decoded; null for other HTML.
     *
     * @param string $html Stored text editor HTML.
     * @return string|null
     */
    public static function paragraphText(string $html): ?string
    {
        return preg_match(self::TEMPLATES['paragraph'], $html) === 1 ? self::decode(substr($html, 3, -4)) : null;
    }
}
