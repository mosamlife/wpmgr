<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The patterns of a designed page, as Elementor containers.
 *
 * A pattern is a child of a section: one labelled container, an inner one, that
 * holds the pattern's content. Its Navigator label (LeafPolicy::LABEL_KEY) is
 * label(): LeafPolicy::LABEL_PREFIX, the pattern's name and its variant, so the
 * label alone says which pattern and variant the subtree is, and fromLabel()
 * reads it back.
 *
 *   hero background  a group (the full-width column) holding the hero's blocks,
 *   hero centered    centred: the heading, the lead, the buttons and a picture
 *   cta band         the same, for a call to action
 *   hero split       a columns row, copy in the 55 percent column on the left
 *                    and the picture in the 45 percent column on the right;
 *                    the row stacks on a phone. A split hero with no picture is
 *                    the stacked group
 *
 * The blocks of a hero or a cta arrive built, by the leaf compiler in the tone
 * of the section and in the alignment align() names for the variant: the
 * pattern decides how its blocks are laid out, so the section's own alignment
 * does not reach them. A centred pattern also centres each row of buttons.
 *
 * The group, the row and its columns are the section compiler's, so they are
 * the settings the round-trip probes store; this class adds the label after
 * the last key, where Elementor returns it.
 *
 * Every argument comes from OutlineV3, which has judged it; one outside what the
 * grammar accepts is a programming error and throws \InvalidArgumentException.
 * Node ids come from the IdSeed, keyed by the pattern's outline path and, for a
 * container the outline never names, by that path and a "#" suffix.
 *
 * Pure: no WordPress call.
 */
final class ElementorPatternCompiler
{
    /** The Navigator name of each pattern, as written after LeafPolicy::LABEL_PREFIX. */
    private const NAMES = [
        'hero'         => 'Hero',
        'cta'          => 'CTA',
        'features'     => 'Features',
        'testimonials' => 'Testimonials',
        'pricing'      => 'Pricing',
        'faq'          => 'FAQ',
        'logos'        => 'Logos',
        'stats'        => 'Stats',
    ];

    /** The patterns that hold blocks the leaf compiler builds. */
    public const COMPOSED = ['hero', 'cta'];

    /** The alignment the blocks of a composed pattern are built in, by pattern and variant. */
    private const ALIGN = [
        'hero' => ['background' => 'center', 'split' => 'start', 'centered' => 'center'],
        'cta'  => ['band' => 'center'],
    ];

    /** The width of the copy column and of the picture column of a split hero, in percent. */
    public const SPLIT_COPY_PERCENT = 55;

    public const SPLIT_MEDIA_PERCENT = 45;

    /**
     * Every settings key this compiler writes beyond the keys of the section
     * compiler and the leaf styles, by element type, with its pin.
     *
     * @var array<string,array<string,string>>
     */
    public const KEYS = [];

    private DesignTokens $tokens;

    private ElementorSectionCompiler $sections;

    /** @var array<mixed> */
    private array $media;

    /**
     * @param DesignTokens             $tokens   The design's tokens.
     * @param ElementorSectionCompiler $sections The page's section compiler: its groups, rows and columns are the patterns' containers.
     * @param array<mixed>             $media    Resolved image facts by attachment id, as the page-create precheck hands them on.
     */
    public function __construct(DesignTokens $tokens, ElementorSectionCompiler $sections, array $media = [])
    {
        $this->tokens   = $tokens;
        $this->sections = $sections;
        $this->media    = $media;
    }

    /**
     * The Navigator label of a pattern's container.
     *
     * @param string $pattern One of OutlineV3Schema::PATTERNS.
     * @param string $variant One of that pattern's OutlineV3Schema::VARIANTS.
     * @return string
     * @throws \InvalidArgumentException When the pattern or the variant is one the grammar does not have.
     */
    public static function label(string $pattern, string $variant): string
    {
        $name = self::NAMES[$pattern] ?? null;
        if ($name === null || !in_array($variant, OutlineV3Schema::VARIANTS[$pattern] ?? [], true)) {
            throw new \InvalidArgumentException('a pattern is one of the patterns, with one of its variants');
        }

        return LeafPolicy::LABEL_PREFIX . $name . ' ' . $variant;
    }

    /**
     * The pattern and variant a label names; null for any other label.
     *
     * @param string $label A container's Navigator label.
     * @return array{pattern:string,variant:string}|null
     */
    public static function fromLabel(string $label): ?array
    {
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                if ($label === self::label($pattern, $variant)) {
                    return ['pattern' => $pattern, 'variant' => $variant];
                }
            }
        }

        return null;
    }

    /**
     * The alignment the blocks of a composed pattern are built in: centre for a
     * hero over a picture, a centred hero and a call to action, start for a split
     * hero. Null for a pattern that holds no blocks and for a name the grammar
     * does not have.
     *
     * @param string $pattern A pattern.
     * @param string $variant Its variant.
     * @return string|null One of OutlineV3Schema::ALIGNS.
     */
    public static function align(string $pattern, string $variant): ?string
    {
        return self::ALIGN[$pattern][$variant] ?? null;
    }

    /**
     * The allowed keys of a page that holds patterns: the given allowlist with
     * this compiler's keys added. A key the allowlist already lists keeps its
     * pin, so adding these never narrows one.
     *
     * @param array<string,array<string,string>> $allowed Element type, then settings path, then pin.
     * @return array<string,array<string,string>>
     */
    public static function allowedKeys(array $allowed): array
    {
        foreach (self::KEYS as $type => $paths) {
            $allowed[$type] = ($allowed[$type] ?? []) + $paths;
        }

        return $allowed;
    }

    /**
     * A pattern as its container.
     *
     * @param IdSeed                    $ids     Node ids for this request.
     * @param string                    $path    The pattern's outline path, e.g. "outline[0].children[0]".
     * @param array<string,mixed>       $pattern The pattern node.
     * @param string                    $tone    The tone of the section it stands in.
     * @param list<array<string,mixed>> $blocks  The built blocks of a hero or a cta; none for any other pattern.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the pattern is not one this compiler builds, or holds a value the grammar does not accept.
     */
    public function compile(IdSeed $ids, string $path, array $pattern, string $tone, array $blocks = []): array
    {
        return match ($pattern['pattern'] ?? null) {
            'hero'  => $this->hero($ids, $path, $pattern, $blocks),
            'cta'   => $this->cta($ids, $path, $pattern, $blocks),
            default => throw new \InvalidArgumentException('this pattern is not one the compiler builds'),
        };
    }

    /**
     * A hero around its built blocks.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The hero's outline path.
     * @param array<string,mixed>       $pattern  The hero node.
     * @param list<array<string,mixed>> $elements Its blocks, built in align()'s alignment.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a hero with a variant of the grammar, or has no blocks.
     */
    public function hero(IdSeed $ids, string $path, array $pattern, array $elements): array
    {
        $variant = self::variantOf($pattern, 'hero');
        $label   = self::label('hero', $variant);
        if ($elements === []) {
            throw new \InvalidArgumentException('a hero holds at least one block');
        }
        $id = $ids->next($path);
        if ($variant === 'split') {
            return $this->split($ids, $path, $id, $label, $elements);
        }

        return $this->stack($id, $label, self::centred($elements));
    }

    /**
     * A call to action around its built blocks.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The cta's outline path.
     * @param array<string,mixed>       $pattern  The cta node.
     * @param list<array<string,mixed>> $elements Its blocks, built in align()'s alignment.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a cta with a variant of the grammar, or has no blocks.
     */
    public function cta(IdSeed $ids, string $path, array $pattern, array $elements): array
    {
        $label = self::label('cta', self::variantOf($pattern, 'cta'));
        if ($elements === []) {
            throw new \InvalidArgumentException('a call to action holds at least one block');
        }

        return $this->stack($ids->next($path), $label, self::centred($elements));
    }

    /**
     * The variant of a pattern node of the given name.
     *
     * @param array<string,mixed> $pattern The pattern node.
     * @param string              $name    The pattern it must be.
     * @return string
     * @throws \InvalidArgumentException When it is another pattern or has no variant of the grammar.
     */
    private static function variantOf(array $pattern, string $name): string
    {
        $variant = $pattern['variant'] ?? null;
        if (($pattern['pattern'] ?? null) !== $name || !is_string($variant) || !in_array($variant, OutlineV3Schema::VARIANTS[$name], true)) {
            throw new \InvalidArgumentException('a pattern node names its pattern and one of that pattern\'s variants');
        }

        return $variant;
    }

    /**
     * The blocks in one full-width column, labelled.
     *
     * @param string                    $id       The container's id.
     * @param string                    $label    Its Navigator label.
     * @param list<array<string,mixed>> $elements Its blocks.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function stack(string $id, string $label, array $elements): array
    {
        return self::labelled($this->sections->group($id, $elements), $label);
    }

    /**
     * A split hero: the copy on the left and the pictures on the right, in a
     * labelled row. Without a picture, or without copy, it is the stacked group.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The hero's outline path.
     * @param string                    $id       The container's id.
     * @param string                    $label    Its Navigator label.
     * @param list<array<string,mixed>> $elements Its blocks.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function split(IdSeed $ids, string $path, string $id, string $label, array $elements): array
    {
        $copy  = [];
        $media = [];
        foreach ($elements as $element) {
            if (($element['widgetType'] ?? null) === 'image') {
                $media[] = $element;
            } else {
                $copy[] = $element;
            }
        }
        if ($copy === [] || $media === []) {
            return $this->stack($id, $label, $elements);
        }
        $columns = [
            $this->sections->column($ids->next($path . '#copy'), self::SPLIT_COPY_PERCENT, $copy),
            $this->sections->column($ids->next($path . '#media'), self::SPLIT_MEDIA_PERCENT, $media),
        ];

        return self::labelled($this->sections->row($id, $columns), $label);
    }

    /**
     * Each row of buttons among the elements centred: the key follows the row
     * keys the leaf compiler wrote, where it writes it for a centred row.
     *
     * @param list<array<string,mixed>> $elements Built blocks.
     * @return list<array<string,mixed>>
     */
    private static function centred(array $elements): array
    {
        foreach ($elements as $i => $element) {
            $children = $element['elements'] ?? [];
            if (($element['elType'] ?? null) !== 'container' || $children === [] || isset($element['settings']['flex_justify_content'])) {
                continue;
            }
            if (array_column($children, 'widgetType') === array_fill(0, count($children), 'button')) {
                $elements[$i]['settings']['flex_justify_content'] = 'center';
            }
        }

        return $elements;
    }

    /**
     * A container with its Navigator label after the last key.
     *
     * @param array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool} $container A container.
     * @param string                                                                                                       $label     Its label.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private static function labelled(array $container, string $label): array
    {
        $container['settings'][LeafPolicy::LABEL_KEY] = $label;

        return $container;
    }
}
