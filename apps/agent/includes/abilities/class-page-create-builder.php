<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-create: validate the outline, and render it deterministically.
 *
 * Input (exact JSON text, an object):
 *   post_type  "page" | "post"
 *   editor     "wordpress_blocks" | "wordpress_classic"
 *   title      plain text, 1..200 characters, no newline
 *   outline    1..200 nodes, each one of:
 *                {"type":"heading","level":2|3|4,"text":"..."}
 *                {"type":"paragraph","text":"..."}
 *                {"type":"list","ordered":bool,"items":["...", ...]}  (1..50 items)
 *
 * Text only. Every text value is plain characters: no markup, no shortcode
 * brackets, no template syntax, no control or bidi-override characters.
 * Anything outside the grammar is refused, never stripped. The rendered bytes
 * are then retokenised and every tag must be one this builder emits.
 */
final class PageCreateBuilder
{
    public const EDITOR_BLOCKS  = 'wordpress_blocks';
    public const EDITOR_CLASSIC = 'wordpress_classic';

    private const MAX_NODES = 200;

    private const MAX_ITEMS = 50;

    private const MAX_TITLE_CHARS = 200;

    private const MAX_TEXT_CHARS = 5000;

    private const MAX_TOTAL_CHARS = 60000;

    /** Sequences refused anywhere in text (Sec-F6). */
    private const FORBIDDEN_SEQUENCES = ['<', '>', '[', ']', '{{', '}}', '{%', '%}', '<!--', '-->', '`'];

    /** Tags the renderer may emit; the retokenisation pass allows only these. */
    private const ALLOWED_TAGS = [
        '<p>', '</p>', '<h2>', '</h2>', '<h3>', '</h3>', '<h4>', '</h4>',
        '<h2 class="wp-block-heading">', '<h3 class="wp-block-heading">', '<h4 class="wp-block-heading">',
        '<ul>', '</ul>', '<ol>', '</ol>', '<ul class="wp-block-list">', '<ol class="wp-block-list">',
        '<li>', '</li>',
    ];

    /** Block comment delimiters the renderer may emit. */
    private const ALLOWED_COMMENTS = [
        '<!-- wp:paragraph -->', '<!-- /wp:paragraph -->',
        '<!-- wp:heading -->', '<!-- wp:heading {"level":3} -->', '<!-- wp:heading {"level":4} -->', '<!-- /wp:heading -->',
        '<!-- wp:list -->', '<!-- wp:list {"ordered":true} -->', '<!-- /wp:list -->',
        '<!-- wp:list-item -->', '<!-- /wp:list-item -->',
    ];

    /**
     * Validate the input. Returns the normalised spec or a refusal.
     *
     * @param object $input Decoded input.
     * @return array{spec?:array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>},code?:string,detail?:string}
     */
    public static function validate(object $input): array
    {
        $vars    = get_object_vars($input);
        $allowed = ['post_type', 'editor', 'title', 'outline'];
        foreach (array_keys($vars) as $key) {
            if (!in_array((string) $key, $allowed, true)) {
                return self::bad('bad_input', 'unknown input field');
            }
        }
        $type = $vars['post_type'] ?? null;
        if (!is_string($type) || !in_array($type, ['page', 'post'], true)) {
            return self::bad('bad_input', 'post_type must be page or post');
        }
        $editor = $vars['editor'] ?? null;
        if (!is_string($editor) || !in_array($editor, [self::EDITOR_BLOCKS, self::EDITOR_CLASSIC], true)) {
            return self::bad('bad_input', 'editor must be wordpress_blocks or wordpress_classic');
        }
        $title = $vars['title'] ?? null;
        if (!is_string($title)) {
            return self::bad('bad_input', 'title must be a string');
        }
        $why = self::textProblem($title, self::MAX_TITLE_CHARS);
        if ($why !== null) {
            return self::bad('create_content_invalid', 'title: ' . $why);
        }

        $outline = $vars['outline'] ?? null;
        if (!is_array($outline) || $outline === [] || count($outline) > self::MAX_NODES) {
            return self::bad('bad_input', 'outline must be a list of 1 to 200 nodes');
        }
        $nodes = [];
        $total = self::chars($title);
        foreach ($outline as $i => $node) {
            if (!is_object($node)) {
                return self::bad('bad_input', 'outline[' . $i . '] must be an object');
            }
            $n    = get_object_vars($node);
            $kind = $n['type'] ?? null;
            switch ($kind) {
                case 'heading':
                    if (array_diff(array_keys($n), ['type', 'level', 'text']) !== []) {
                        return self::bad('bad_input', 'outline[' . $i . '] has an unknown field');
                    }
                    $level = $n['level'] ?? null;
                    if (!is_int($level) || $level < 2 || $level > 4) {
                        return self::bad('bad_input', 'outline[' . $i . '].level must be 2, 3 or 4');
                    }
                    $text = $n['text'] ?? null;
                    $why  = is_string($text) ? self::textProblem($text, self::MAX_TEXT_CHARS) : 'must be a string';
                    if ($why !== null) {
                        return self::bad('create_content_invalid', 'outline[' . $i . '].text: ' . $why);
                    }
                    $total  += self::chars((string) $text);
                    $nodes[] = ['type' => 'heading', 'level' => $level, 'text' => (string) $text];
                    break;
                case 'paragraph':
                    if (array_diff(array_keys($n), ['type', 'text']) !== []) {
                        return self::bad('bad_input', 'outline[' . $i . '] has an unknown field');
                    }
                    $text = $n['text'] ?? null;
                    $why  = is_string($text) ? self::textProblem($text, self::MAX_TEXT_CHARS) : 'must be a string';
                    if ($why !== null) {
                        return self::bad('create_content_invalid', 'outline[' . $i . '].text: ' . $why);
                    }
                    $total  += self::chars((string) $text);
                    $nodes[] = ['type' => 'paragraph', 'text' => (string) $text];
                    break;
                case 'list':
                    if (array_diff(array_keys($n), ['type', 'ordered', 'items']) !== []) {
                        return self::bad('bad_input', 'outline[' . $i . '] has an unknown field');
                    }
                    $ordered = $n['ordered'] ?? null;
                    $items   = $n['items'] ?? null;
                    if (!is_bool($ordered)) {
                        return self::bad('bad_input', 'outline[' . $i . '].ordered must be a boolean');
                    }
                    if (!is_array($items) || $items === [] || count($items) > self::MAX_ITEMS) {
                        return self::bad('bad_input', 'outline[' . $i . '].items must be 1 to 50 strings');
                    }
                    $clean = [];
                    foreach ($items as $j => $item) {
                        $why = is_string($item) ? self::textProblem($item, self::MAX_TEXT_CHARS) : 'must be a string';
                        if ($why !== null) {
                            return self::bad('create_content_invalid', 'outline[' . $i . '].items[' . $j . ']: ' . $why);
                        }
                        $total  += self::chars((string) $item);
                        $clean[] = (string) $item;
                    }
                    $nodes[] = ['type' => 'list', 'ordered' => $ordered, 'items' => $clean];
                    break;
                default:
                    return self::bad('create_content_invalid', 'outline[' . $i . '] is not a heading, paragraph or list');
            }
        }
        if ($total > self::MAX_TOTAL_CHARS) {
            return self::bad('create_content_invalid', 'the content is longer than 60000 characters');
        }

        return ['spec' => ['post_type' => $type, 'editor' => $editor, 'title' => $title, 'outline' => $nodes]];
    }

    /**
     * Render the outline. Deterministic: the same spec gives the same bytes.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec Spec.
     * @return string
     */
    public static function render(array $spec): string
    {
        $blocks = $spec['editor'] === self::EDITOR_BLOCKS;
        $parts  = [];
        foreach ($spec['outline'] as $node) {
            switch ($node['type']) {
                case 'heading':
                    $level = (int) $node['level'];
                    $text  = self::text((string) $node['text']);
                    if ($blocks) {
                        $attrs   = $level === 2 ? '' : ' {"level":' . $level . '}';
                        $parts[] = '<!-- wp:heading' . $attrs . " -->\n<h" . $level . ' class="wp-block-heading">' . $text . '</h' . $level . ">\n<!-- /wp:heading -->";
                    } else {
                        $parts[] = '<h' . $level . '>' . $text . '</h' . $level . '>';
                    }
                    break;
                case 'paragraph':
                    $text    = self::text((string) $node['text']);
                    $parts[] = $blocks
                        ? "<!-- wp:paragraph -->\n<p>" . $text . "</p>\n<!-- /wp:paragraph -->"
                        : '<p>' . $text . '</p>';
                    break;
                case 'list':
                    $tag   = $node['ordered'] ? 'ol' : 'ul';
                    $items = '';
                    foreach ((array) $node['items'] as $item) {
                        $li     = '<li>' . self::text((string) $item) . '</li>';
                        $items .= $blocks ? "<!-- wp:list-item -->\n" . $li . "\n<!-- /wp:list-item -->" : "\n" . $li;
                    }
                    if ($blocks) {
                        $attrs   = $node['ordered'] ? ' {"ordered":true}' : '';
                        $parts[] = '<!-- wp:list' . $attrs . " -->\n<" . $tag . ' class="wp-block-list">' . $items . '</' . $tag . ">\n<!-- /wp:list -->";
                    } else {
                        $parts[] = '<' . $tag . '>' . $items . "\n</" . $tag . '>';
                    }
                    break;
            }
        }

        return implode("\n\n", $parts);
    }

    /**
     * Retokenise the rendered bytes: every tag and comment must be one the
     * renderer emits, and no shortcode or template syntax may appear.
     *
     * @param string $html Rendered bytes.
     * @return string|null Problem, or null.
     */
    public static function retokenise(string $html): ?string
    {
        if (preg_match_all('/<!--.*?-->|<[^>]*>/s', $html, $m) === false) {
            return 'the content could not be tokenised';
        }
        foreach ($m[0] as $token) {
            if (strncmp($token, '<!--', 4) === 0) {
                if (!in_array($token, self::ALLOWED_COMMENTS, true)) {
                    return 'an unexpected comment appeared';
                }
                continue;
            }
            if (!in_array($token, self::ALLOWED_TAGS, true)) {
                return 'an unexpected tag appeared';
            }
        }
        $outside = (string) preg_replace('/<!--.*?-->|<[^>]*>/s', '', $html);
        foreach (['<', '>', '[', ']', '{{', '{%'] as $seq) {
            if (strpos($outside, $seq) !== false) {
                return 'markup or template syntax appeared in text';
            }
        }

        return null;
    }

    /**
     * The preview digest: exactly what will be created.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec    Spec.
     * @param string                                                                              $content Rendered bytes.
     * @return string
     */
    public static function previewDigest(array $spec, string $content): string
    {
        return hash('sha256', (string) json_encode([$spec['editor'], $spec['post_type'], 'draft', $spec['title'], $content]));
    }

    /**
     * The base fingerprint for a new object: the created-object key.
     *
     * @param string $postType Post type.
     * @return string
     */
    public static function baseFingerprint(string $postType): string
    {
        return hash('sha256', (string) json_encode(['new_post', $postType]));
    }

    /**
     * document fingerprint of a stored post, for the person-undo guard.
     *
     * @param object $post Post.
     * @return string
     */
    public static function documentFingerprint(object $post): string
    {
        return hash('sha256', (string) json_encode([
            (string) ($post->post_type ?? ''),
            (string) ($post->post_status ?? ''),
            (string) ($post->post_title ?? ''),
            (string) ($post->post_content ?? ''),
            (string) ($post->post_excerpt ?? ''),
            (string) ($post->post_name ?? ''),
            (string) ($post->post_password ?? ''),
            (string) ($post->post_modified_gmt ?? ''),
        ]));
    }

    /**
     * Why a text value is not acceptable, or null.
     *
     * @param string $text  Text.
     * @param int    $max   Character limit.
     * @return string|null
     */
    private static function textProblem(string $text, int $max): ?string
    {
        if (preg_match('//u', $text) !== 1) {
            return 'not valid UTF-8';
        }
        if (trim($text) === '') {
            return 'empty';
        }
        if (self::chars($text) > $max) {
            return 'too long';
        }
        // Control characters (newlines included), bidi overrides and isolates, BOM, noncharacters.
        if (preg_match('/[\x{0000}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]/u', $text) === 1) {
            return 'contains a control or invisible formatting character';
        }
        foreach (self::FORBIDDEN_SEQUENCES as $seq) {
            if (strpos($text, $seq) !== false) {
                return 'contains a forbidden sequence';
            }
        }
        if (preg_match('/&(#[0-9]+|#x[0-9a-f]+|[a-z][a-z0-9]*);/i', $text) === 1) {
            return 'contains an HTML entity';
        }

        return null;
    }

    /**
     * Escape text for an HTML text node. Only `&` can need escaping, since
     * `<` and `>` are refused before this runs.
     *
     * @param string $text Text.
     * @return string
     */
    private static function text(string $text): string
    {
        return htmlspecialchars($text, ENT_NOQUOTES | ENT_SUBSTITUTE, 'UTF-8');
    }

    /**
     * @param string $text Text.
     * @return int
     */
    private static function chars(string $text): int
    {
        return function_exists('mb_strlen') ? mb_strlen($text, 'UTF-8') : strlen($text);
    }

    /**
     * @param string $code   Code.
     * @param string $detail Detail.
     * @return array{code:string,detail:string}
     */
    private static function bad(string $code, string $detail): array
    {
        return ['code' => $code, 'detail' => $detail];
    }
}
