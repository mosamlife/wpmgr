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
 *   outline    1..200 items; an item is a block, a group or columns
 *
 * Blocks (allowed anywhere a node is allowed):
 *   {"type":"heading","level":2|3|4,"text":TEXT}
 *   {"type":"paragraph","text":TEXT}
 *   {"type":"list","ordered":bool,"items":[TEXT x1..50]}
 *   {"type":"image","attachment_id":INT,"alt":ALT,"caption"?:TEXT,"align"?:"none"|"center"|"wide"|"full"}
 *   {"type":"buttons","align"?:"left"|"center","buttons":[{"text":TEXT,"url":URL,"style"?:"fill"|"outline"} x1..3]}
 *   {"type":"quote","paragraphs":[TEXT x1..10],"citation"?:TEXT}
 *   {"type":"separator"}
 *   {"type":"spacer","size":"small"|"medium"|"large"}
 *   {"type":"table","header"?:[CELL x1..6],"rows":[[CELL x1..6] x1..50]}
 * Containers (not recursive):
 *   {"type":"group","children":[block | columns x1..50]}       top level only
 *   {"type":"columns","widths"?:[INT 10..90 x n],"columns":[{"children":[block x1..50]} x n]}, n = 2..4
 *
 * Text is plain characters: no markup, no template syntax, no control or
 * bidi-override characters, and no square bracket except a bracketed number
 * such as [1]. Alt text holds no square bracket at all. Alt text, links and
 * image addresses hold no "&" that starts a character reference ("&" then a
 * name or number and ";"). A link is an https:// address or a path on this
 * site with no colon and no "&#". An image is an attachment already in the
 * media library, named by id; its address comes from the site, never from
 * the input. Anything outside the grammar is refused, never stripped.
 *
 * Text is stored escaped: `&` as `&amp;` and brackets as numeric references,
 * so entity-like text renders literally and no bracket byte reaches the
 * stored post. Attribute values carry only `&amp;`, `&quot;` and `&apos;`
 * escapes. No input string ever enters a block comment. The rendered bytes are
 * then retokenised and every tag and comment must match a shape this builder
 * emits. An outline of headings, paragraphs and lists renders exactly as it
 * did before layout blocks existed.
 *
 * The builder never calls WordPress: the command resolves images and passes
 * their facts in.
 */
final class PageCreateBuilder
{
    public const EDITOR_BLOCKS  = 'wordpress_blocks';
    public const EDITOR_CLASSIC = 'wordpress_classic';

    public const MAX_TOP_LEVEL_NODES = 200;

    /** Every node object: blocks, groups, columns, each column, each button. */
    public const MAX_NODES = 400;

    public const MAX_ITEMS = 50;

    public const MIN_COLUMNS = 2;

    public const MAX_COLUMNS = 4;

    public const MAX_CHILDREN = 50;

    public const MAX_IMAGES = 20;

    public const MAX_BUTTONS = 12;

    public const MAX_BUTTONS_PER_BLOCK = 3;

    public const MAX_TABLES = 10;

    public const MAX_TABLE_ROWS = 50;

    public const MAX_TABLE_COLUMNS = 6;

    public const MAX_QUOTE_PARAGRAPHS = 10;

    public const MAX_TITLE_CHARS = 200;

    public const MAX_TEXT_CHARS = 5000;

    public const MAX_CAPTION_CHARS = 500;

    public const MAX_CITATION_CHARS = 200;

    public const MAX_BUTTON_TEXT_CHARS = 80;

    public const MAX_CELL_CHARS = 500;

    public const MAX_ALT_CHARS = 300;

    public const MAX_URL_BYTES = 2048;

    public const MAX_TOTAL_CHARS = 60000;

    public const MAX_INPUT_BYTES = 65536;

    public const MAX_ATTACHMENT_ID = 2147483647;

    public const MAX_FILENAME_CHARS = 255;

    public const MAX_IMAGE_DIMENSION = 100000;

    /** Image types an image block may show. */
    public const IMAGE_MIMES = ['image/jpeg', 'image/png', 'image/gif', 'image/webp', 'image/avif'];

    /** Spacer heights in px. */
    private const SPACER_PX = ['small' => 24, 'medium' => 48, 'large' => 96];

    /** Where a node sits: an outline item, a group child, a column child. */
    private const AT_TOP    = 0;
    private const AT_GROUP  = 1;
    private const AT_COLUMN = 2;

    /** Sequences refused anywhere in text (Sec-F6). */
    private const FORBIDDEN_SEQUENCES = ['<', '>', '{{', '}}', '{%', '%}', '<!--', '-->', '`'];

    /** The only bracketed text allowed: a number of one to four digits. */
    private const BRACKETED_NUMBER = '/\[[0-9]{1,4}\]/';

    /** Every character a link or image address may hold (ASCII only). */
    private const URL_CHARSET = '/^[A-Za-z0-9\-._~:\/?#!$&()*+,;=%@]+$/D';

    /**
     * An ampersand that starts a character reference: a name, a decimal
     * number or a hex number, ended by ";". The block editor writes such an
     * ampersand bare in an attribute, so an attribute value holding one
     * cannot keep the bytes this builder writes; alt text, links and image
     * addresses refuse it.
     */
    private const CHARACTER_REFERENCE = '/&(?:[A-Za-z0-9]+|#[0-9]+|#[xX][0-9A-Fa-f]+);/';

    /** URL attribute value as written: an http(s) address or a site path. */
    private const RE_URL_VALUE = '(?:https?:\/\/|\/(?![\/\\\\]))[A-Za-z0-9\-._~:\/?#!$&()*+,;=%@]*';

    /** Alt attribute value as written. */
    private const RE_ALT_VALUE = '[^"\'<>\[\]]*';

    /** Tag shapes the renderer emits; retokenise allows only these. */
    private const TAG_PATTERNS = [
        '/^<(?:p|h[234]|ul|ol|li|blockquote|cite|table|thead|tbody|tr|th|td)>$/D',
        '/^<\/(?:p|h[234]|ul|ol|li|blockquote|cite|table|thead|tbody|tr|th|td|figure|figcaption|div|a)>$/D',
        '/^<h[234] class="wp-block-heading">$/D',
        '/^<(?:ul|ol) class="wp-block-list">$/D',
        '/^<figure class="wp-block-image(?: align(?:center|wide|full))? size-large">$/D',
        '/^<img src="URL" alt="ALT" class="wp-image-[1-9][0-9]{0,9}" \/>$/D',
        '/^<img src="URL" alt="ALT" class="(?:align(?:center|wide|full) )?wp-image-[1-9][0-9]{0,9} size-large" \/>$/D',
        '/^<figcaption class="wp-element-caption">$/D',
        '/^<div class="wp-block-(?:columns|column|group|buttons|button|button is-style-outline)">$/D',
        '/^<div class="wp-block-column" style="flex-basis:[1-9][0-9]%">$/D',
        '/^<div style="height:(?:24|48|96)px" aria-hidden="true" class="wp-block-spacer">$/D',
        '/^<a class="wp-block-button__link wp-element-button" href="URL">$/D',
        '/^<blockquote class="wp-block-quote">$/D',
        '/^<hr class="wp-block-separator has-alpha-channel-opacity" \/>$/D',
        '/^<hr \/>$/D',
        '/^<figure class="wp-block-table">$/D',
        '/^<table class="has-fixed-layout">$/D',
    ];

    /** Block comment delimiters the renderer emits. */
    private const COMMENT_PATTERNS = [
        '/^<!-- \/wp:(?:paragraph|heading|list|list-item|image|columns|column|group|buttons|button|quote|separator|spacer|table) -->$/D',
        '/^<!-- wp:(?:paragraph|heading|list|list-item|columns|column|buttons|button|quote|separator) -->$/D',
        '/^<!-- wp:heading \{"level":[34]\} -->$/D',
        '/^<!-- wp:list \{"ordered":true\} -->$/D',
        '/^<!-- wp:image \{"id":[1-9][0-9]{0,9},"sizeSlug":"large","linkDestination":"none"(?:,"align":"(?:center|wide|full)")?\} -->$/D',
        '/^<!-- wp:column \{"width":"[1-9][0-9]%"\} -->$/D',
        '/^<!-- wp:group \{"layout":\{"type":"constrained"\}\} -->$/D',
        '/^<!-- wp:buttons \{"layout":\{"type":"flex","justifyContent":"center"\}\} -->$/D',
        '/^<!-- wp:button \{"className":"is-style-outline"\} -->$/D',
        '/^<!-- wp:spacer \{"height":"(?:24|48|96)px"\} -->$/D',
        '/^<!-- wp:table \{"hasFixedLayout":true\} -->$/D',
    ];

    /**
     * Validate the input. Returns the normalised spec or a refusal.
     *
     * Codes: bad_input (unknown key, wrong type, value outside an enum),
     * create_content_invalid (a text rule), layout_invalid (placement, counts,
     * widths, table shape), link_invalid (a button link),
     * layout_needs_block_editor (a block-editor-only node on the classic
     * editor). Nothing is rewritten.
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
        if (!is_array($outline) || $outline === [] || count($outline) > self::MAX_TOP_LEVEL_NODES) {
            return self::bad('bad_input', 'outline must be a list of 1 to 200 nodes');
        }
        $ctx = [
            'total'      => self::chars($title),
            'nodes'      => 0,
            'images'     => 0,
            'buttons'    => 0,
            'tables'     => 0,
            'block_only' => '',
        ];
        $nodes = [];
        foreach ($outline as $i => $node) {
            $r = self::node($node, 'outline[' . $i . ']', self::AT_TOP, $ctx);
            if (!isset($r['node'])) {
                return $r;
            }
            $nodes[] = $r['node'];
        }
        if ($ctx['total'] > self::MAX_TOTAL_CHARS) {
            return self::bad('create_content_invalid', 'the content is longer than 60000 characters');
        }
        if ($editor === self::EDITOR_CLASSIC && $ctx['block_only'] !== '') {
            return self::bad('layout_needs_block_editor', $ctx['block_only'] . ' needs the block editor');
        }

        return ['spec' => ['post_type' => $type, 'editor' => $editor, 'title' => $title, 'outline' => $nodes]];
    }

    /**
     * Distinct attachment ids, in first-appearance (document) order.
     *
     * @param array{outline:list<array<string,mixed>>} $spec Spec.
     * @return list<int>
     */
    public static function mediaIds(array $spec): array
    {
        $ids = [];
        self::collectIds($spec['outline'], $ids);

        return array_map('intval', array_keys($ids));
    }

    /**
     * Render the outline. Deterministic: the same spec and media give the
     * same bytes. $media maps each attachment id to its resolved facts; only
     * `url` is used here.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec  Spec.
     * @param array<int,array<string,mixed>>                                                      $media Resolved images.
     * @return string
     * @throws \UnexpectedValueException When an image has no resolved address.
     */
    public static function render(array $spec, array $media = []): string
    {
        $blocks = $spec['editor'] === self::EDITOR_BLOCKS;
        $parts  = [];
        foreach ($spec['outline'] as $node) {
            $parts[] = self::renderNode($node, $blocks, $media);
        }

        return implode("\n\n", $parts);
    }

    /**
     * Retokenise the rendered bytes: every tag and comment must match a shape
     * the renderer emits, and no markup, shortcode or template syntax may
     * appear in text.
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
                if (!self::matchesAny($token, self::COMMENT_PATTERNS)) {
                    return 'an unexpected comment appeared';
                }
                continue;
            }
            if (!self::matchesAny($token, self::TAG_PATTERNS)) {
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
     * The exact post_title bytes stored for a plain-text title: the same
     * escaping as body text, which every save filter leaves byte-identical.
     *
     * @param string $title Plain-text title, already validated.
     * @return string
     */
    public static function storedTitle(string $title): string
    {
        return self::text($title);
    }

    /**
     * The base fingerprint for a new object. Without images it is the
     * created-object key; with images it also binds every fact of every
     * image, in mediaIds() order, so a replaced, deleted, re-parented or
     * re-dated attachment changes it. json_encode with default flags.
     *
     * @param string                          $postType   Post type.
     * @param list<array<string,mixed>>       $mediaFacts Facts, as mediaFacts() rows.
     * @return string
     * @throws \JsonException When a fact cannot be encoded (never for checked facts).
     */
    public static function baseFingerprint(string $postType, array $mediaFacts = []): string
    {
        if ($mediaFacts === []) {
            return hash('sha256', (string) json_encode(['new_post', $postType]));
        }
        $rows = [];
        foreach ($mediaFacts as $f) {
            $rows[] = [
                (int) $f['id'],
                (string) $f['url'],
                (string) $f['filename'],
                (string) $f['mime'],
                (int) $f['width'],
                (int) $f['height'],
                (string) $f['modified_gmt'],
            ];
        }

        return hash('sha256', json_encode(['new_post', $postType, $rows], JSON_THROW_ON_ERROR));
    }

    /**
     * Why a resolved image fact row cannot be used, or null. The same rules
     * the control plane applies to the precheck answer.
     *
     * @param array<string,mixed> $fact {id,url,filename,mime,width,height,modified_gmt}.
     * @return string|null
     */
    public static function mediaFactProblem(array $fact): ?string
    {
        $keys = ['id', 'url', 'filename', 'mime', 'width', 'height', 'modified_gmt'];
        if (array_keys($fact) !== $keys) {
            return 'the image facts are incomplete';
        }
        if (!is_int($fact['id']) || $fact['id'] < 1 || $fact['id'] > self::MAX_ATTACHMENT_ID) {
            return 'the image id is not usable';
        }
        if (!is_string($fact['url']) || self::imageUrlProblem($fact['url']) !== null) {
            return 'the image address is not usable';
        }
        $name = $fact['filename'];
        if (!is_string($name) || $name === '' || preg_match('//u', $name) !== 1 || self::chars($name) > self::MAX_FILENAME_CHARS) {
            return 'the image file name is not usable';
        }
        if (!is_string($fact['mime']) || !in_array($fact['mime'], self::IMAGE_MIMES, true)) {
            return 'the image type is not allowed';
        }
        foreach (['width', 'height'] as $dim) {
            if (!is_int($fact[$dim]) || $fact[$dim] < 0 || $fact[$dim] > self::MAX_IMAGE_DIMENSION) {
                return 'the image size is not usable';
            }
        }
        if (!is_string($fact['modified_gmt']) || preg_match('/^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$/D', $fact['modified_gmt']) !== 1) {
            return 'the image date is not usable';
        }

        return null;
    }

    /**
     * Why an address the site gave for an image is unusable, or null:
     * http:// or https:// (lowercase), a host with no user name, ASCII from
     * the link character set, every % followed by two hex digits, no "&"
     * that starts a character reference, at most 2048 bytes.
     *
     * @param string $url Address.
     * @return string|null
     */
    public static function imageUrlProblem(string $url): ?string
    {
        $shape = self::urlShapeProblem($url);
        if ($shape !== null) {
            return $shape;
        }
        if (strncmp($url, 'https://', 8) === 0) {
            $rest = substr($url, 8);
        } elseif (strncmp($url, 'http://', 7) === 0) {
            $rest = substr($url, 7);
        } else {
            return 'is not an http or https address';
        }
        $authority = substr($rest, 0, strcspn($rest, '/?#'));
        if (preg_match('/^[A-Za-z0-9.-]+(?::[0-9]{1,5})?$/D', $authority) !== 1) {
            return 'has no plain host name';
        }

        return null;
    }

    /**
     * Why a button link is not acceptable, or null. Either an absolute
     * https:// address (lowercase scheme, DNS host with at least one dot,
     * optional port 1..65535, no user name or password) or a path on this
     * site (starts with one /, and holds no colon and no "&#"). ASCII from the
     * link character set only, every % followed by two hex digits, no "&"
     * that starts a character reference, at most 2048 bytes.
     *
     * @param string $url Link.
     * @return string|null
     */
    public static function linkProblem(string $url): ?string
    {
        $shape = self::urlShapeProblem($url);
        if ($shape !== null) {
            return $shape;
        }
        if ($url[0] === '/') {
            if (strlen($url) > 1 && ($url[1] === '/' || $url[1] === '\\')) {
                return 'must not start with two slashes';
            }
            // WordPress reads the text before a colon in a link, in any
            // spelling, as its protocol and drops it on save. A path holds no
            // colon and no "&#" anywhere; %3A is a colon it keeps.
            if (strpos($url, ':') !== false || strpos($url, '&#') !== false) {
                return 'a path on this site cannot hold a colon or "&#"; write a colon as %3A';
            }

            return null;
        }
        if (strncmp($url, 'https://', 8) !== 0) {
            return 'must be an https:// address or a path on this site that starts with /';
        }
        $rest      = substr($url, 8);
        $authority = substr($rest, 0, strcspn($rest, '/?#'));
        if (strpos($authority, '@') !== false) {
            return 'must not carry a user name or password';
        }
        if (preg_match('/^((?:[A-Za-z0-9-]+\.)+[A-Za-z0-9-]+)(?::([0-9]{1,5}))?$/D', $authority, $m) !== 1) {
            return 'must name a host such as example.com';
        }
        // PCRE omits an unmatched trailing group, so $m[2] is set only for a port.
        if (isset($m[2]) && ((int) $m[2] < 1 || (int) $m[2] > 65535)) {
            return 'has a port out of range';
        }

        return null;
    }

    /**
     * The limits, as published to the AI in the catalogue entry.
     *
     * @return array<string,int>
     */
    public static function limits(): array
    {
        return [
            'max_top_level_nodes' => self::MAX_TOP_LEVEL_NODES,
            'max_nodes'           => self::MAX_NODES,
            'max_columns'         => self::MAX_COLUMNS,
            'max_children'        => self::MAX_CHILDREN,
            'max_images'          => self::MAX_IMAGES,
            'max_buttons'         => self::MAX_BUTTONS,
            'max_tables'          => self::MAX_TABLES,
            'max_table_rows'      => self::MAX_TABLE_ROWS,
            'max_table_columns'   => self::MAX_TABLE_COLUMNS,
            'max_title_chars'     => self::MAX_TITLE_CHARS,
            'max_text_chars'      => self::MAX_TEXT_CHARS,
            'max_total_chars'     => self::MAX_TOTAL_CHARS,
            'max_input_bytes'     => self::MAX_INPUT_BYTES,
        ];
    }

    /**
     * The JSON schema of the input. The block variants are written out in
     * full at each place they may appear (no $ref), with a group allowing
     * columns and a column allowing blocks only.
     *
     * @return array<string,mixed>
     */
    public static function inputSchema(): array
    {
        $leaves  = self::leafSchemas();
        $columns = self::nodeSchema('columns', [
            'widths'  => [
                'type'     => 'array',
                'minItems' => self::MIN_COLUMNS,
                'maxItems' => self::MAX_COLUMNS,
                'items'    => ['type' => 'integer', 'minimum' => 10, 'maximum' => 90],
            ],
            'columns' => [
                'type'     => 'array',
                'minItems' => self::MIN_COLUMNS,
                'maxItems' => self::MAX_COLUMNS,
                'items'    => [
                    'type'                 => 'object',
                    'properties'           => [
                        'children' => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_CHILDREN, 'items' => ['oneOf' => $leaves]],
                    ],
                    'required'             => ['children'],
                    'additionalProperties' => false,
                ],
            ],
        ], ['columns']);
        $group = self::nodeSchema('group', [
            'children' => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_CHILDREN, 'items' => ['oneOf' => array_merge($leaves, [$columns])]],
        ], ['children']);

        return [
            'type'                 => 'object',
            'properties'           => [
                'post_type' => ['type' => 'string', 'enum' => ['page', 'post']],
                'editor'    => ['type' => 'string', 'enum' => [self::EDITOR_BLOCKS, self::EDITOR_CLASSIC]],
                'title'     => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_TITLE_CHARS],
                'outline'   => [
                    'type'     => 'array',
                    'minItems' => 1,
                    'maxItems' => self::MAX_TOP_LEVEL_NODES,
                    'items'    => ['oneOf' => array_merge($leaves, [$group, $columns])],
                ],
            ],
            'required'             => ['post_type', 'editor', 'title', 'outline'],
            'additionalProperties' => false,
        ];
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
            (int) ($post->post_parent ?? 0),
            (int) ($post->menu_order ?? 0),
        ]));
    }

    // ---------------------------------------------------------------------
    // Grammar
    // ---------------------------------------------------------------------

    /**
     * One node at a placement.
     *
     * @param mixed                $node Decoded node.
     * @param string               $at   Path, for the detail text.
     * @param int                  $pos  AT_TOP, AT_GROUP or AT_COLUMN.
     * @param array<string,mixed>  $ctx  Page-wide counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function node($node, string $at, int $pos, array &$ctx): array
    {
        if (!is_object($node)) {
            return self::bad('bad_input', $at . ' must be an object');
        }
        if (++$ctx['nodes'] > self::MAX_NODES) {
            return self::bad('layout_invalid', 'the page has more than 400 blocks');
        }
        $n    = get_object_vars($node);
        $kind = $n['type'] ?? null;
        switch ($kind) {
            case 'heading':
                if (array_diff(array_keys($n), ['type', 'level', 'text']) !== []) {
                    return self::bad('bad_input', $at . ' has an unknown field');
                }
                $level = $n['level'] ?? null;
                if (!is_int($level) || $level < 2 || $level > 4) {
                    return self::bad('bad_input', $at . '.level must be 2, 3 or 4');
                }
                $text = $n['text'] ?? null;
                $why  = is_string($text) ? self::textProblem($text, self::MAX_TEXT_CHARS) : 'must be a string';
                if ($why !== null) {
                    return self::bad('create_content_invalid', $at . '.text: ' . $why);
                }
                $ctx['total'] += self::chars((string) $text);

                return ['node' => ['type' => 'heading', 'level' => $level, 'text' => (string) $text]];
            case 'paragraph':
                if (array_diff(array_keys($n), ['type', 'text']) !== []) {
                    return self::bad('bad_input', $at . ' has an unknown field');
                }
                $text = $n['text'] ?? null;
                $why  = is_string($text) ? self::textProblem($text, self::MAX_TEXT_CHARS) : 'must be a string';
                if ($why !== null) {
                    return self::bad('create_content_invalid', $at . '.text: ' . $why);
                }
                $ctx['total'] += self::chars((string) $text);

                return ['node' => ['type' => 'paragraph', 'text' => (string) $text]];
            case 'list':
                if (array_diff(array_keys($n), ['type', 'ordered', 'items']) !== []) {
                    return self::bad('bad_input', $at . ' has an unknown field');
                }
                $ordered = $n['ordered'] ?? null;
                $items   = $n['items'] ?? null;
                if (!is_bool($ordered)) {
                    return self::bad('bad_input', $at . '.ordered must be a boolean');
                }
                if (!is_array($items) || $items === [] || count($items) > self::MAX_ITEMS) {
                    return self::bad('bad_input', $at . '.items must be 1 to 50 strings');
                }
                $clean = [];
                foreach ($items as $j => $item) {
                    $why = is_string($item) ? self::textProblem($item, self::MAX_TEXT_CHARS) : 'must be a string';
                    if ($why !== null) {
                        return self::bad('create_content_invalid', $at . '.items[' . $j . ']: ' . $why);
                    }
                    $ctx['total'] += self::chars((string) $item);
                    $clean[]       = (string) $item;
                }

                return ['node' => ['type' => 'list', 'ordered' => $ordered, 'items' => $clean]];
            case 'image':
                return self::image($n, $at, $ctx);
            case 'buttons':
                return self::buttons($n, $at, $ctx);
            case 'quote':
                return self::quote($n, $at, $ctx);
            case 'separator':
                $bad = self::keys($n, ['type'], ['type'], $at);
                if ($bad !== null) {
                    return $bad;
                }

                return ['node' => ['type' => 'separator']];
            case 'spacer':
                $bad = self::keys($n, ['type', 'size'], ['type', 'size'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $size = $n['size'];
                if (!is_string($size) || !isset(self::SPACER_PX[$size])) {
                    return self::bad('bad_input', $at . '.size must be small, medium or large');
                }
                self::blockOnly($ctx, $at);

                return ['node' => ['type' => 'spacer', 'size' => $size]];
            case 'table':
                return self::table($n, $at, $ctx);
            case 'group':
                if ($pos !== self::AT_TOP) {
                    return self::bad('layout_invalid', $at . ': a group can only be an outline item');
                }

                return self::group($n, $at, $ctx);
            case 'columns':
                if ($pos === self::AT_COLUMN) {
                    return self::bad('layout_invalid', $at . ': columns cannot be inside a column');
                }

                return self::columns($n, $at, $ctx);
            default:
                return self::bad('create_content_invalid', $at . ' is not a known block type');
        }
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function image(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'attachment_id', 'alt', 'caption', 'align'], ['type', 'attachment_id', 'alt'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $id = $n['attachment_id'];
        if (!is_int($id) || $id < 1 || $id > self::MAX_ATTACHMENT_ID) {
            return self::bad('bad_input', $at . '.attachment_id must be a whole number from 1 to 2147483647');
        }
        $alt = self::plain($n['alt'], $at . '.alt', self::MAX_ALT_CHARS, 'alt', $ctx);
        if (is_array($alt)) {
            return $alt;
        }
        $caption = null;
        if (array_key_exists('caption', $n)) {
            $caption = self::plain($n['caption'], $at . '.caption', self::MAX_CAPTION_CHARS, 'text', $ctx);
            if (is_array($caption)) {
                return $caption;
            }
            self::blockOnly($ctx, $at . '.caption');
        }
        $align = 'none';
        if (array_key_exists('align', $n)) {
            if (!is_string($n['align']) || !in_array($n['align'], ['none', 'center', 'wide', 'full'], true)) {
                return self::bad('bad_input', $at . '.align must be none, center, wide or full');
            }
            $align = $n['align'];
        }
        if (++$ctx['images'] > self::MAX_IMAGES) {
            return self::bad('layout_invalid', 'the page has more than 20 images');
        }

        return ['node' => ['type' => 'image', 'attachment_id' => $id, 'alt' => $alt, 'caption' => $caption, 'align' => $align]];
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function buttons(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'align', 'buttons'], ['type', 'buttons'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $align = 'left';
        if (array_key_exists('align', $n)) {
            if (!is_string($n['align']) || !in_array($n['align'], ['left', 'center'], true)) {
                return self::bad('bad_input', $at . '.align must be left or center');
            }
            $align = $n['align'];
        }
        $list = $n['buttons'];
        if (!is_array($list)) {
            return self::bad('bad_input', $at . '.buttons must be a list');
        }
        if ($list === [] || count($list) > self::MAX_BUTTONS_PER_BLOCK) {
            return self::bad('layout_invalid', $at . '.buttons must hold 1 to 3 buttons');
        }
        $clean = [];
        foreach ($list as $j => $button) {
            $where = $at . '.buttons[' . $j . ']';
            if (!is_object($button)) {
                return self::bad('bad_input', $where . ' must be an object');
            }
            if (++$ctx['nodes'] > self::MAX_NODES) {
                return self::bad('layout_invalid', 'the page has more than 400 blocks');
            }
            $b   = get_object_vars($button);
            $bad = self::keys($b, ['text', 'url', 'style'], ['text', 'url'], $where);
            if ($bad !== null) {
                return $bad;
            }
            $text = self::plain($b['text'], $where . '.text', self::MAX_BUTTON_TEXT_CHARS, 'text', $ctx);
            if (is_array($text)) {
                return $text;
            }
            $url = $b['url'];
            if (!is_string($url)) {
                return self::bad('bad_input', $where . '.url must be a string');
            }
            $why = self::linkProblem($url);
            if ($why !== null) {
                return self::bad('link_invalid', $where . '.url ' . $why);
            }
            $ctx['total'] += strlen($url);
            $style         = 'fill';
            if (array_key_exists('style', $b)) {
                if (!is_string($b['style']) || !in_array($b['style'], ['fill', 'outline'], true)) {
                    return self::bad('bad_input', $where . '.style must be fill or outline');
                }
                $style = $b['style'];
            }
            if (++$ctx['buttons'] > self::MAX_BUTTONS) {
                return self::bad('layout_invalid', 'the page has more than 12 buttons');
            }
            $clean[] = ['text' => $text, 'url' => $url, 'style' => $style];
        }
        self::blockOnly($ctx, $at);

        return ['node' => ['type' => 'buttons', 'align' => $align, 'buttons' => $clean]];
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function quote(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'paragraphs', 'citation'], ['type', 'paragraphs'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $paras = $n['paragraphs'];
        if (!is_array($paras)) {
            return self::bad('bad_input', $at . '.paragraphs must be a list');
        }
        if ($paras === [] || count($paras) > self::MAX_QUOTE_PARAGRAPHS) {
            return self::bad('layout_invalid', $at . '.paragraphs must hold 1 to 10 paragraphs');
        }
        $clean = [];
        foreach ($paras as $j => $para) {
            $text = self::plain($para, $at . '.paragraphs[' . $j . ']', self::MAX_TEXT_CHARS, 'text', $ctx);
            if (is_array($text)) {
                return $text;
            }
            $clean[] = $text;
        }
        $citation = null;
        if (array_key_exists('citation', $n)) {
            $citation = self::plain($n['citation'], $at . '.citation', self::MAX_CITATION_CHARS, 'text', $ctx);
            if (is_array($citation)) {
                return $citation;
            }
        }

        return ['node' => ['type' => 'quote', 'paragraphs' => $clean, 'citation' => $citation]];
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function table(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'header', 'rows'], ['type', 'rows'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $width  = null;
        $header = null;
        if (array_key_exists('header', $n)) {
            $cells = self::cells($n['header'], $at . '.header', $ctx);
            if (!isset($cells['cells'])) {
                return $cells;
            }
            $header = $cells['cells'];
            $width  = count($header);
        }
        $rows = $n['rows'];
        if (!is_array($rows)) {
            return self::bad('bad_input', $at . '.rows must be a list');
        }
        if ($rows === [] || count($rows) > self::MAX_TABLE_ROWS) {
            return self::bad('layout_invalid', $at . '.rows must hold 1 to 50 rows');
        }
        $clean = [];
        foreach ($rows as $j => $row) {
            $cells = self::cells($row, $at . '.rows[' . $j . ']', $ctx);
            if (!isset($cells['cells'])) {
                return $cells;
            }
            if ($width !== null && count($cells['cells']) !== $width) {
                return self::bad('layout_invalid', $at . ': every row and the header must have the same number of cells');
            }
            $width   = count($cells['cells']);
            $clean[] = $cells['cells'];
        }
        if (++$ctx['tables'] > self::MAX_TABLES) {
            return self::bad('layout_invalid', 'the page has more than 10 tables');
        }

        return ['node' => ['type' => 'table', 'header' => $header, 'rows' => $clean]];
    }

    /**
     * One table row (or the header): 1..6 cells.
     *
     * @param mixed               $row   Decoded row.
     * @param string              $where Path.
     * @param array<string,mixed> $ctx   Counters.
     * @return array{cells?:list<string>,code?:string,detail?:string}
     */
    private static function cells($row, string $where, array &$ctx): array
    {
        if (!is_array($row)) {
            return self::bad('bad_input', $where . ' must be a list of cells');
        }
        if ($row === [] || count($row) > self::MAX_TABLE_COLUMNS) {
            return self::bad('layout_invalid', $where . ' must hold 1 to 6 cells');
        }
        $clean = [];
        foreach ($row as $k => $cell) {
            $text = self::plain($cell, $where . '[' . $k . ']', self::MAX_CELL_CHARS, 'cell', $ctx);
            if (is_array($text)) {
                return $text;
            }
            $clean[] = $text;
        }

        return ['cells' => $clean];
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function group(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'children'], ['type', 'children'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $children = self::children($n['children'], $at . '.children', self::AT_GROUP, $ctx);
        if (!isset($children['children'])) {
            return $children;
        }
        self::blockOnly($ctx, $at);

        return ['node' => ['type' => 'group', 'children' => $children['children']]];
    }

    /**
     * @param array<string,mixed> $n   Node fields.
     * @param string              $at  Path.
     * @param array<string,mixed> $ctx Counters.
     * @return array{node?:array<string,mixed>,code?:string,detail?:string}
     */
    private static function columns(array $n, string $at, array &$ctx): array
    {
        $bad = self::keys($n, ['type', 'widths', 'columns'], ['type', 'columns'], $at);
        if ($bad !== null) {
            return $bad;
        }
        $cols = $n['columns'];
        if (!is_array($cols)) {
            return self::bad('bad_input', $at . '.columns must be a list');
        }
        if (count($cols) < self::MIN_COLUMNS || count($cols) > self::MAX_COLUMNS) {
            return self::bad('layout_invalid', $at . '.columns must hold 2 to 4 columns');
        }
        $widths = null;
        if (array_key_exists('widths', $n)) {
            $widths = $n['widths'];
            if (!is_array($widths)) {
                return self::bad('bad_input', $at . '.widths must be a list of whole numbers');
            }
            foreach ($widths as $w) {
                if (!is_int($w)) {
                    return self::bad('bad_input', $at . '.widths must be a list of whole numbers');
                }
            }
            if (count($widths) !== count($cols)) {
                return self::bad('layout_invalid', $at . '.widths must give one width per column');
            }
            foreach ($widths as $w) {
                if ($w < 10 || $w > 90) {
                    return self::bad('layout_invalid', $at . '.widths must each be 10 to 90');
                }
            }
            if (array_sum($widths) !== 100) {
                return self::bad('layout_invalid', $at . '.widths must add up to 100');
            }
            $widths = array_values($widths);
        }
        $clean = [];
        foreach ($cols as $j => $col) {
            $where = $at . '.columns[' . $j . ']';
            if (!is_object($col)) {
                return self::bad('bad_input', $where . ' must be an object');
            }
            if (++$ctx['nodes'] > self::MAX_NODES) {
                return self::bad('layout_invalid', 'the page has more than 400 blocks');
            }
            $c   = get_object_vars($col);
            $bad = self::keys($c, ['children'], ['children'], $where);
            if ($bad !== null) {
                return $bad;
            }
            $children = self::children($c['children'], $where . '.children', self::AT_COLUMN, $ctx);
            if (!isset($children['children'])) {
                return $children;
            }
            $clean[] = $children['children'];
        }
        self::blockOnly($ctx, $at);

        return ['node' => ['type' => 'columns', 'widths' => $widths, 'columns' => $clean]];
    }

    /**
     * A container's children: 1..50 nodes at one placement.
     *
     * @param mixed               $list  Decoded list.
     * @param string              $where Path.
     * @param int                 $pos   Placement of the children.
     * @param array<string,mixed> $ctx   Counters.
     * @return array{children?:list<array<string,mixed>>,code?:string,detail?:string}
     */
    private static function children($list, string $where, int $pos, array &$ctx): array
    {
        if (!is_array($list)) {
            return self::bad('bad_input', $where . ' must be a list');
        }
        if ($list === [] || count($list) > self::MAX_CHILDREN) {
            return self::bad('layout_invalid', $where . ' must hold 1 to 50 blocks');
        }
        $clean = [];
        foreach ($list as $j => $child) {
            $r = self::node($child, $where . '[' . $j . ']', $pos, $ctx);
            if (!isset($r['node'])) {
                return $r;
            }
            $clean[] = $r['node'];
        }

        return ['children' => $clean];
    }

    /**
     * Exactly the allowed keys, and every required key present.
     *
     * @param array<string,mixed> $n        Fields.
     * @param list<string>        $allowed  Allowed keys.
     * @param list<string>        $required Required keys.
     * @param string              $at       Path.
     * @return array{code:string,detail:string}|null
     */
    private static function keys(array $n, array $allowed, array $required, string $at): ?array
    {
        foreach (array_keys($n) as $key) {
            if (!in_array((string) $key, $allowed, true)) {
                return self::bad('bad_input', $at . ' has an unknown field');
            }
        }
        foreach ($required as $key) {
            if (!array_key_exists($key, $n)) {
                return self::bad('bad_input', $at . ' is missing ' . $key);
            }
        }

        return null;
    }

    /**
     * A text value under a rule: 'text' (not empty), 'cell' (may be empty) or
     * 'alt' (may be empty, no square bracket at all). Counts toward the total.
     *
     * @param mixed               $value Decoded value.
     * @param string              $where Path.
     * @param int                 $max   Character limit.
     * @param string              $rule  Rule.
     * @param array<string,mixed> $ctx   Counters.
     * @return string|array{code:string,detail:string}
     */
    private static function plain($value, string $where, int $max, string $rule, array &$ctx)
    {
        if (!is_string($value)) {
            return self::bad('bad_input', $where . ' must be a string');
        }
        $why = self::textProblem($value, $max, $rule);
        if ($why !== null) {
            return self::bad('create_content_invalid', $where . ': ' . $why);
        }
        $ctx['total'] += self::chars($value);

        return $value;
    }

    /**
     * Record the first node the classic editor cannot hold.
     *
     * @param array<string,mixed> $ctx Counters.
     * @param string              $at  Path.
     * @return void
     */
    private static function blockOnly(array &$ctx, string $at): void
    {
        if ($ctx['block_only'] === '') {
            $ctx['block_only'] = $at;
        }
    }

    /**
     * @param list<array<string,mixed>> $nodes Nodes.
     * @param array<int,bool>           $ids   Seen ids, in order.
     * @return void
     */
    private static function collectIds(array $nodes, array &$ids): void
    {
        foreach ($nodes as $node) {
            switch ($node['type']) {
                case 'image':
                    $ids[(int) $node['attachment_id']] = true;
                    break;
                case 'group':
                    self::collectIds($node['children'], $ids);
                    break;
                case 'columns':
                    foreach ($node['columns'] as $children) {
                        self::collectIds($children, $ids);
                    }
                    break;
            }
        }
    }

    // ---------------------------------------------------------------------
    // Rendering
    // ---------------------------------------------------------------------

    /**
     * @param array<string,mixed>            $node   Node.
     * @param bool                           $blocks Block editor.
     * @param array<int,array<string,mixed>> $media  Resolved images.
     * @return string
     * @throws \UnexpectedValueException When a node cannot be rendered.
     */
    private static function renderNode(array $node, bool $blocks, array $media): string
    {
        switch ($node['type']) {
            case 'heading':
                $level = (int) $node['level'];
                $text  = self::text((string) $node['text']);
                if ($blocks) {
                    $attrs = $level === 2 ? '' : ' {"level":' . $level . '}';

                    return '<!-- wp:heading' . $attrs . " -->\n<h" . $level . ' class="wp-block-heading">' . $text . '</h' . $level . ">\n<!-- /wp:heading -->";
                }

                return '<h' . $level . '>' . $text . '</h' . $level . '>';
            case 'paragraph':
                return self::paragraph((string) $node['text'], $blocks);
            case 'list':
                $tag   = $node['ordered'] ? 'ol' : 'ul';
                $items = '';
                foreach ((array) $node['items'] as $item) {
                    $li     = '<li>' . self::text((string) $item) . '</li>';
                    $items .= $blocks ? "<!-- wp:list-item -->\n" . $li . "\n<!-- /wp:list-item -->" : "\n" . $li;
                }
                if ($blocks) {
                    $attrs = $node['ordered'] ? ' {"ordered":true}' : '';

                    return '<!-- wp:list' . $attrs . " -->\n<" . $tag . ' class="wp-block-list">' . $items . '</' . $tag . ">\n<!-- /wp:list -->";
                }

                return '<' . $tag . '>' . $items . "\n</" . $tag . '>';
            case 'image':
                return self::renderImage($node, $blocks, $media);
            case 'quote':
                $cite = $node['citation'] === null ? '' : '<cite>' . self::text((string) $node['citation']) . '</cite>';
                $body = [];
                foreach ((array) $node['paragraphs'] as $para) {
                    $body[] = self::paragraph((string) $para, $blocks);
                }
                if ($blocks) {
                    return "<!-- wp:quote -->\n<blockquote class=\"wp-block-quote\">" . implode("\n\n", $body) . $cite . "</blockquote>\n<!-- /wp:quote -->";
                }

                return '<blockquote>' . implode('', $body) . $cite . '</blockquote>';
            case 'separator':
                return $blocks
                    ? "<!-- wp:separator -->\n<hr class=\"wp-block-separator has-alpha-channel-opacity\" />\n<!-- /wp:separator -->"
                    : '<hr />';
            case 'table':
                $inner = '';
                if ($node['header'] !== null) {
                    $inner .= '<thead><tr>';
                    foreach ((array) $node['header'] as $cell) {
                        $inner .= '<th>' . self::text((string) $cell) . '</th>';
                    }
                    $inner .= '</tr></thead>';
                }
                $inner .= '<tbody>';
                foreach ((array) $node['rows'] as $row) {
                    $inner .= '<tr>';
                    foreach ((array) $row as $cell) {
                        $inner .= '<td>' . self::text((string) $cell) . '</td>';
                    }
                    $inner .= '</tr>';
                }
                $inner .= '</tbody>';
                if ($blocks) {
                    return "<!-- wp:table {\"hasFixedLayout\":true} -->\n<figure class=\"wp-block-table\"><table class=\"has-fixed-layout\">" . $inner . "</table></figure>\n<!-- /wp:table -->";
                }

                return '<table>' . $inner . '</table>';
        }
        // The rest exist in the block editor only; the classic editor refuses
        // them in validate(), and they are never flattened.
        if (!$blocks) {
            throw new \UnexpectedValueException('a block-editor layout node cannot be rendered for the classic editor');
        }
        switch ($node['type']) {
            case 'buttons':
                $attrs = $node['align'] === 'center' ? ' {"layout":{"type":"flex","justifyContent":"center"}}' : '';
                $each  = [];
                foreach ((array) $node['buttons'] as $b) {
                    $outline = $b['style'] === 'outline';
                    $each[]  = '<!-- wp:button' . ($outline ? ' {"className":"is-style-outline"}' : '') . " -->\n"
                        . '<div class="wp-block-button' . ($outline ? ' is-style-outline' : '') . '">'
                        . '<a class="wp-block-button__link wp-element-button" href="' . self::urlAttr((string) $b['url']) . '">' . self::text((string) $b['text']) . '</a>'
                        . "</div>\n<!-- /wp:button -->";
                }

                return '<!-- wp:buttons' . $attrs . " -->\n<div class=\"wp-block-buttons\">" . implode("\n\n", $each) . "</div>\n<!-- /wp:buttons -->";
            case 'spacer':
                $px = self::SPACER_PX[(string) $node['size']];

                return '<!-- wp:spacer {"height":"' . $px . "px\"} -->\n"
                    . '<div style="height:' . $px . 'px" aria-hidden="true" class="wp-block-spacer"></div>'
                    . "\n<!-- /wp:spacer -->";
            case 'group':
                $kids = [];
                foreach ((array) $node['children'] as $child) {
                    $kids[] = self::renderNode($child, true, $media);
                }

                return "<!-- wp:group {\"layout\":{\"type\":\"constrained\"}} -->\n<div class=\"wp-block-group\">" . implode("\n\n", $kids) . "</div>\n<!-- /wp:group -->";
            case 'columns':
                $cols = [];
                foreach ((array) $node['columns'] as $i => $children) {
                    $w    = is_array($node['widths']) ? (int) $node['widths'][$i] : null;
                    $open = $w === null
                        ? "<!-- wp:column -->\n<div class=\"wp-block-column\">"
                        : '<!-- wp:column {"width":"' . $w . "%\"} -->\n<div class=\"wp-block-column\" style=\"flex-basis:" . $w . '%">';
                    $kids = [];
                    foreach ((array) $children as $child) {
                        $kids[] = self::renderNode($child, true, $media);
                    }
                    $cols[] = $open . implode("\n\n", $kids) . "</div>\n<!-- /wp:column -->";
                }

                return "<!-- wp:columns -->\n<div class=\"wp-block-columns\">" . implode("\n\n", $cols) . "</div>\n<!-- /wp:columns -->";
        }

        throw new \UnexpectedValueException('an unknown node cannot be rendered');
    }

    /**
     * @param string $text   Text.
     * @param bool   $blocks Block editor.
     * @return string
     */
    private static function paragraph(string $text, bool $blocks): string
    {
        $text = self::text($text);

        return $blocks ? "<!-- wp:paragraph -->\n<p>" . $text . "</p>\n<!-- /wp:paragraph -->" : '<p>' . $text . '</p>';
    }

    /**
     * @param array<string,mixed>            $node   Image node.
     * @param bool                           $blocks Block editor.
     * @param array<int,array<string,mixed>> $media  Resolved images.
     * @return string
     * @throws \UnexpectedValueException When the image has no usable resolved address.
     */
    private static function renderImage(array $node, bool $blocks, array $media): string
    {
        $id  = (int) $node['attachment_id'];
        $url = $media[$id]['url'] ?? null;
        if (!is_string($url) || self::imageUrlProblem($url) !== null) {
            throw new \UnexpectedValueException('an image has no usable resolved address');
        }
        $align = (string) $node['align'];
        $src   = self::urlAttr($url);
        $alt   = self::attr((string) $node['alt']);
        if (!$blocks) {
            $class = ($align !== 'none' ? 'align' . $align . ' ' : '') . 'wp-image-' . $id . ' size-large';

            return '<img src="' . $src . '" alt="' . $alt . '" class="' . $class . '" />';
        }
        $attrs   = '{"id":' . $id . ',"sizeSlug":"large","linkDestination":"none"' . ($align !== 'none' ? ',"align":"' . $align . '"' : '') . '}';
        $figure  = 'wp-block-image' . ($align !== 'none' ? ' align' . $align : '') . ' size-large';
        $caption = $node['caption'] === null ? '' : '<figcaption class="wp-element-caption">' . self::text((string) $node['caption']) . '</figcaption>';

        return '<!-- wp:image ' . $attrs . " -->\n<figure class=\"" . $figure . '"><img src="' . $src . '" alt="' . $alt . '" class="wp-image-' . $id . '" />' . $caption . "</figure>\n<!-- /wp:image -->";
    }

    // ---------------------------------------------------------------------
    // Schema
    // ---------------------------------------------------------------------

    /**
     * The nine block variants.
     *
     * @return list<array<string,mixed>>
     */
    private static function leafSchemas(): array
    {
        $text = ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_TEXT_CHARS];
        $cell = ['type' => 'string', 'maxLength' => self::MAX_CELL_CHARS];
        $row  = ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_TABLE_COLUMNS, 'items' => $cell];

        return [
            self::nodeSchema('heading', [
                'level' => ['type' => 'integer', 'minimum' => 2, 'maximum' => 4],
                'text'  => $text,
            ], ['level', 'text']),
            self::nodeSchema('paragraph', ['text' => $text], ['text']),
            self::nodeSchema('list', [
                'ordered' => ['type' => 'boolean'],
                'items'   => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_ITEMS, 'items' => $text],
            ], ['ordered', 'items']),
            self::nodeSchema('image', [
                'attachment_id' => ['type' => 'integer', 'minimum' => 1, 'maximum' => self::MAX_ATTACHMENT_ID],
                'alt'           => ['type' => 'string', 'maxLength' => self::MAX_ALT_CHARS],
                'caption'       => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_CAPTION_CHARS],
                'align'         => ['type' => 'string', 'enum' => ['none', 'center', 'wide', 'full']],
            ], ['attachment_id', 'alt']),
            self::nodeSchema('buttons', [
                'align'   => ['type' => 'string', 'enum' => ['left', 'center']],
                'buttons' => [
                    'type'     => 'array',
                    'minItems' => 1,
                    'maxItems' => self::MAX_BUTTONS_PER_BLOCK,
                    'items'    => [
                        'type'                 => 'object',
                        'properties'           => [
                            'text'  => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_BUTTON_TEXT_CHARS],
                            'url'   => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_URL_BYTES],
                            'style' => ['type' => 'string', 'enum' => ['fill', 'outline']],
                        ],
                        'required'             => ['text', 'url'],
                        'additionalProperties' => false,
                    ],
                ],
            ], ['buttons']),
            self::nodeSchema('quote', [
                'paragraphs' => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_QUOTE_PARAGRAPHS, 'items' => $text],
                'citation'   => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_CITATION_CHARS],
            ], ['paragraphs']),
            self::nodeSchema('separator', [], []),
            self::nodeSchema('spacer', [
                'size' => ['type' => 'string', 'enum' => ['small', 'medium', 'large']],
            ], ['size']),
            self::nodeSchema('table', [
                'header' => $row,
                'rows'   => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_TABLE_ROWS, 'items' => $row],
            ], ['rows']),
        ];
    }

    /**
     * @param string                $type     Node type.
     * @param array<string,mixed>   $props    Properties other than type.
     * @param list<string>          $required Required properties other than type.
     * @return array<string,mixed>
     */
    private static function nodeSchema(string $type, array $props, array $required): array
    {
        return [
            'type'                 => 'object',
            'properties'           => ['type' => ['const' => $type]] + $props,
            'required'             => array_merge(['type'], $required),
            'additionalProperties' => false,
        ];
    }

    // ---------------------------------------------------------------------
    // Text and escaping
    // ---------------------------------------------------------------------

    /**
     * Why a text value is not acceptable, or null.
     *
     * @param string $text Text.
     * @param int    $max  Character limit.
     * @param string $rule 'text', 'cell' or 'alt'.
     * @return string|null
     */
    private static function textProblem(string $text, int $max, string $rule = 'text'): ?string
    {
        if (preg_match('//u', $text) !== 1) {
            return 'not valid UTF-8';
        }
        if ($text === '' && $rule !== 'text') {
            return null;
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
                return 'contains "' . $seq . '"; write plain text with no markup, comments or template syntax';
            }
        }
        if ($rule === 'alt') {
            if (strpos($text, '[') !== false || strpos($text, ']') !== false) {
                return 'contains a square bracket; alt text cannot hold one, so use parentheses instead';
            }
            if (preg_match(self::CHARACTER_REFERENCE, $text) === 1) {
                return 'contains "&" then a name or number and ";", which reads as a character reference; write the character itself, or put a space after the "&"';
            }

            return null;
        }
        $unbracketed = (string) preg_replace(self::BRACKETED_NUMBER, '', $text);
        if (strpos($unbracketed, '[') !== false || strpos($unbracketed, ']') !== false) {
            return 'contains a square bracket; only a bracketed number such as [1] is allowed, so use parentheses instead';
        }

        return null;
    }

    /**
     * The shared shape of a link or image address: length, character set and
     * percent escapes.
     *
     * @param string $url Address.
     * @return string|null
     */
    private static function urlShapeProblem(string $url): ?string
    {
        $len = strlen($url);
        if ($len < 1 || $len > self::MAX_URL_BYTES) {
            return 'must be 1 to 2048 characters';
        }
        if (preg_match(self::URL_CHARSET, $url) !== 1) {
            return 'contains a character a link cannot hold';
        }
        if (preg_match('/%(?![0-9A-Fa-f]{2})/', $url) === 1) {
            return 'has a % that is not followed by two hex digits';
        }
        if (preg_match(self::CHARACTER_REFERENCE, $url) === 1) {
            return 'contains "&" then a name or number and ";", which reads as a character reference';
        }

        return null;
    }

    /**
     * Escape text for an HTML text node. `<` and `>` are refused before this
     * runs. `&` is always escaped, so an entity-like input renders literally.
     * Brackets become the numeric references core's entity normaliser keeps
     * unchanged (three-digit form), so the stored bytes hold no `[` or `]`
     * and the shortcode parser never sees an opening bracket.
     *
     * @param string $text Text.
     * @return string
     */
    private static function text(string $text): string
    {
        return str_replace(['[', ']'], ['&#091;', '&#093;'], htmlspecialchars($text, ENT_NOQUOTES | ENT_SUBSTITUTE, 'UTF-8'));
    }

    /**
     * Escape alt text for a double-quoted attribute: `&`, `"` and `'` only,
     * the three forms every supported core keeps unchanged on save. `<`,
     * `>` and brackets are refused before this runs.
     *
     * @param string $text Alt text.
     * @return string
     */
    private static function attr(string $text): string
    {
        return str_replace(['&', '"', "'"], ['&amp;', '&quot;', '&apos;'], $text);
    }

    /**
     * Escape a checked address for a double-quoted attribute. The character
     * set already excludes quotes, angle brackets, square brackets,
     * backslashes and spaces, so only `&` needs escaping.
     *
     * @param string $url Address.
     * @return string
     */
    private static function urlAttr(string $url): string
    {
        return str_replace('&', '&amp;', $url);
    }

    /**
     * @param string       $token    Token.
     * @param list<string> $patterns Patterns; URL and ALT are placeholders.
     * @return bool
     */
    private static function matchesAny(string $token, array $patterns): bool
    {
        foreach ($patterns as $pattern) {
            $re = str_replace(['URL', 'ALT'], [self::RE_URL_VALUE, self::RE_ALT_VALUE], $pattern);
            if (preg_match($re, $token) === 1) {
                return true;
            }
        }

        return false;
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
