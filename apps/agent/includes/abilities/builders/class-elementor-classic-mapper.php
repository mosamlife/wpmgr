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
 * Refused, never rewritten: an outline button (node_not_supported_by_builder),
 * an image aligned wide or full (node_not_supported_by_builder), a paragraph
 * whose whole text is one http(s) address (create_content_invalid), alt text
 * that differs from the attachment's library alt (image_alt_from_library),
 * and a button link holding "&" (link_invalid). The finished tree then passes
 * LeafPolicy::checkTree() with ALLOWED_KEYS and LEAF_RULES, or the call is
 * refused with that check's answer.
 *
 * Pure: no WordPress write. The only WordPress function reached is the HTML
 * sanitiser, through LeafPolicy.
 */
final class ElementorClassicMapper
{
    /**
     * Every element type and settings path the mapper writes, with its pin.
     * Numbers are stored as strings, so they are pinned "enum".
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
        ],
        'section'     => [
            'structure' => 'enum',
        ],
        'column'      => [
            '_column_size' => 'enum',
            '_inline_size' => 'enum',
        ],
        'heading'     => [
            'title'       => 'html',
            'header_size' => 'enum',
        ],
        'text-editor' => [
            'editor' => 'html',
        ],
        'button'      => [
            'text'             => 'html',
            'link.url'         => 'url',
            'link.is_external' => 'enum',
            'link.nofollow'    => 'enum',
            'align'            => 'enum',
        ],
        'image'       => [
            'image.url'      => 'url',
            'image.id'       => 'enum',
            'image.source'   => 'enum',
            'image_size'     => 'enum',
            'align'          => 'enum',
            'caption_source' => 'enum',
            'caption'        => 'html',
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

    /** Label key of an element holding dynamic content, code or attributes. */
    public const LABEL_DYNAMIC = 'wpmgr:dynamic';

    /** Label key of a text editor whose HTML is not one of WPMgr's templates. */
    public const LABEL_CUSTOM_TEXT = 'wpmgr:custom-text';

    /** Label key of an Elementor v4 (atomic) element. */
    public const LABEL_ATOMIC = 'wpmgr:atomic';

    /** Label key of a WordPress widget placed in Elementor. */
    public const LABEL_WP_WIDGET = 'wpmgr:wp-widget';

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

    private IdSeed $ids;

    /** @var array<mixed> */
    private array $media;

    private bool $containers;

    /** @var array{code: string, detail: string}|null */
    private ?array $refusal = null;

    /**
     * @param IdSeed       $ids        Node ids for this request.
     * @param array<mixed> $media      Media facts by attachment id.
     * @param bool         $containers Whether the site has Elementor containers on.
     */
    private function __construct(IdSeed $ids, array $media, bool $containers)
    {
        $this->ids        = $ids;
        $this->media      = $media;
        $this->containers = $containers;
    }

    /**
     * Map a validated page-create spec to a classic element tree.
     *
     * $mediaById maps each attachment id in the outline to its facts; this
     * uses "url" (the large-size address, checked as an image address) and
     * "alt" (the attachment's library alt text, "" when it has none).
     *
     * @param array<string, mixed> $spec       Validated page-create spec (its outline).
     * @param IdSeed               $ids        Node ids for this request.
     * @param array<mixed>         $mediaById  Media facts by attachment id.
     * @param bool                 $containers Whether the site has Elementor containers on.
     * @return array{tree?: list<array<string, mixed>>, code?: string, detail?: string}
     */
    public static function map(array $spec, IdSeed $ids, array $mediaById, bool $containers): array
    {
        $outline = $spec['outline'] ?? null;
        if (!is_array($outline) || $outline === [] || !ArrayShape::isList($outline)) {
            return ['code' => 'bad_input', 'detail' => 'outline: not a list of nodes'];
        }
        $mapper = new self($ids, $mediaById, $containers);
        $tree   = $mapper->outline($outline);
        if ($tree === null) {
            return $mapper->refusal ?? ['code' => 'bad_input', 'detail' => 'outline: could not be mapped'];
        }
        $check = LeafPolicy::checkTree($tree, self::ALLOWED_KEYS, self::LEAF_RULES);
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

    // ---------------------------------------------------------------------
    // Mapping
    // ---------------------------------------------------------------------

    /**
     * @param list<mixed> $outline Outline nodes.
     * @return list<array<string, mixed>>|null
     */
    private function outline(array $outline): ?array
    {
        $tree  = [];
        $run   = [];
        $start = '';
        foreach ($outline as $i => $node) {
            $path = 'outline[' . $i . ']';
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
        $elements = [];
        foreach ($children as $j => $child) {
            $at = $path . '.children[' . $j . ']';
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
        if ($columnId === null) {
            return self::layout($id, 'container', ['content_width' => 'boxed', 'flex_direction' => 'column'], $elements, false);
        }

        return self::layout($id, 'section', ['structure' => self::STRUCTURES[1]], [
            self::layout($columnId, 'column', ['_column_size' => self::COLUMN_SIZES[1]], $elements, false),
        ], false);
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

        if ($this->containers) {
            return self::layout($id, 'container', [
                'content_width'         => $nested ? 'full' : 'boxed',
                'flex_direction'        => 'row',
                'flex_direction_mobile' => 'column',
                'flex_gap'              => ['unit' => 'px', 'size' => self::GAP_PX, 'column' => self::GAP_PX, 'row' => self::GAP_PX],
            ], $built, $nested);
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
        switch ($type) {
            case 'heading':
                $level = $node['level'] ?? null;
                $text  = $node['text'] ?? null;
                if (!is_int($level) || !isset(self::HEADER_SIZES[$level]) || !is_string($text)) {
                    return $this->fail('bad_input', $path . ': a heading needs a level of 2, 3 or 4 and a text');
                }

                return [$this->widget($path, 'heading', [
                    'title'       => LeafPolicy::htmlText($text),
                    'header_size' => self::HEADER_SIZES[$level],
                ])];
            case 'paragraph':
                $text = $node['text'] ?? null;
                if (!is_string($text)) {
                    return $this->fail('bad_input', $path . '.text: not a string');
                }
                if (preg_match(self::OWN_PARAGRAPH_ADDRESS, $text) === 1) {
                    return $this->fail('create_content_invalid', $path . '.text: a paragraph that is only a web address becomes an embedded player on an Elementor page; add words around the address or use a button');
                }

                return [$this->textEditor($path, '<p>' . LeafPolicy::htmlText($text) . '</p>')];
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
        $widgets = [];
        foreach ($buttons as $j => $button) {
            $at    = $path . '.buttons[' . $j . ']';
            $text  = is_array($button) ? ($button['text'] ?? null) : null;
            $url   = is_array($button) ? ($button['url'] ?? null) : null;
            $style = is_array($button) ? ($button['style'] ?? 'fill') : null;
            if (!is_string($text) || !is_string($url) || !is_string($style)) {
                return $this->fail('bad_input', $at . ': a button needs a text, a link and an optional style');
            }
            if ($style !== 'fill') {
                return $this->fail('node_not_supported_by_builder', $at . '.style: an outline button is not available in Elementor; the button styles it builds are: fill');
            }
            if (strpos($url, '&') !== false) {
                return $this->fail('link_invalid', $at . '.url: a link on an Elementor page cannot hold "&"; use a link without one');
            }
            $widgets[] = $this->widget($at, 'button', [
                'text'  => LeafPolicy::htmlText($text),
                'link'  => ['url' => $url, 'is_external' => '', 'nofollow' => ''],
                'align' => self::BUTTON_ALIGN[$align],
            ]);
        }

        return $widgets;
    }

    /**
     * @param string $path Outline path.
     * @param string $html Template HTML.
     * @return array<string, mixed>
     */
    private function textEditor(string $path, string $html): array
    {
        return $this->widget($path, 'text-editor', ['editor' => $html]);
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

            $kind = self::classify($type, $settings, $parentKind);
            if ($kind === null) {
                $projection->addLocked($id, $parent, $type === 'text-editor' ? self::LABEL_CUSTOM_TEXT : self::labelKey($type));
                continue;
            }
            [$kindName, $level, $editable, $text] = $kind;
            $projection->add($id, $parent, $kindName, $level, $editable, $text);

            if (in_array($type, ['container', 'section', 'column'], true)) {
                $children = $node['elements'] ?? [];
                if (is_array($children)) {
                    self::projectNodes($children, $id, $kindName, $projection);
                }
            }
        }
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
    private static function decode(string $stored): string
    {
        return html_entity_decode($stored, ENT_QUOTES | ENT_HTML5, 'UTF-8');
    }
}
