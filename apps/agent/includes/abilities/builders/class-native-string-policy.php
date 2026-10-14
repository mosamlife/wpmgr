<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;

if (!defined('ABSPATH')) {
    exit;
}

/** Parsed, closed string grammar for AI-written native settings. Pure PHP. */
final class NativeStringPolicy
{
    /** Rich text is a property of an owned widget/key pair, never a site-reported type. */
    public const RICH_TEXT_KEYS = ['text-editor' => ['editor']];

    /** Units remain closed even when a site's control offers additional units. */
    public const UNITS = ['px', 'em', 'rem', '%', 'vh', 'vw', 'vmin', 'vmax', 'ch', 'ex'];

    private const TAGS = ['p', 'br', 'strong', 'em', 'b', 'i', 'u', 's', 'ul', 'ol', 'li', 'blockquote', 'a'];
    private const LINK_ATTRIBUTES = ['href', 'title', 'target', 'rel'];
    private const NUMERIC_FIELDS = ['size', 'top', 'right', 'bottom', 'left', 'horizontal', 'vertical', 'blur', 'spread',
        'font_size', 'font_weight', 'line_height', 'letter_spacing', 'word_spacing', 'z_index'];

    public static function richKey(string $widget, string $key, string $path): bool
    {
        return $path === 'settings' && in_array($key, self::RICH_TEXT_KEYS[$widget] ?? [], true);
    }

    /**
     * Every scalar in an AI-written control, including object and list leaves.
     * Repeaters call this on their individual fields so failures name the row.
     *
     * @param mixed $value Control value.
     * @param string $type Site-reported control type, used only to narrow.
     * @param bool $rich Whether the owned key permits parsed rich text.
     * @param string $field Object member name, or empty for the whole control.
     * @param int $depth Value nesting depth.
     */
    public static function leafProblem($value, string $type, bool $rich = false, string $field = '', int $depth = 0): ?string
    {
        if ($depth > 8) {
            return 'written values are nested too deeply';
        }
        if (is_array($value)) {
            foreach ($value as $key => $member) {
                $why = self::leafProblem($member, $type, false, is_string($key) ? $key : '', $depth + 1);
                if ($why !== null) {
                    return $why;
                }
            }
            return null;
        }
        if (!is_string($value)) {
            return 'every AI-written scalar must be a string';
        }
        if ($field === 'url' || $field === 'href') {
            return PageCreateBuilder::linkProblem($value, true);
        }
        if ($field === 'unit') {
            return in_array($value, self::UNITS, true) ? null : 'use a closed CSS unit';
        }
        if (in_array($field, self::NUMERIC_FIELDS, true) || $type === 'number') {
            return LeafPolicy::shapeMatches(['unit' => 'px', 'size' => $value], 'slider', ['px'])
                ? null : 'use a closed decimal numeric string';
        }
        if ($field === 'isLinked') {
            return LeafPolicy::shapeMatches($value, 'switcher') ? null : 'write yes or the empty string for a flag';
        }
        $why = self::problem($value, $rich);
        if ($why !== null) {
            return $why;
        }
        if (in_array($type, ['select', 'select2', 'choose'], true)
            && preg_match('/^[A-Za-z0-9 _%\-]*$/D', $value) !== 1) {
            return 'option values must also be closed inert tokens';
        }

        return null;
    }

    /** Closed text grammar and the decoding/sequence checks of LeafPolicy. */
    public static function problem(string $text, bool $rich = false): ?string
    {
        if (strlen($text) > 20000 || preg_match('//u', $text) !== 1
            || preg_match_all('/./us', $text) > PageCreateBuilder::MAX_TEXT_CHARS) {
            return 'write bounded valid UTF-8 text';
        }
        $why = LeafPolicy::decodedProblem($text, ['refuse_braces' => true, 'forbidden' => []]);
        if ($why !== null) {
            return $why;
        }
        if (preg_match('/[\x{0000}-\x{0008}\x{000B}\x{000C}\x{000E}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]/u', $text) === 1) {
            return 'text cannot contain control or invisible formatting characters';
        }
        if (!$rich) {
            return self::plain($text);
        }

        return self::markup($text);
    }

    /** Plain text permits valid character entities and no literal markup or brackets. */
    private static function plain(string $text): ?string
    {
        if (preg_match('/[<>\[\]\x00-\x1F\x7F]/', $text) === 1) {
            return 'plain text cannot contain markup, brackets or control characters';
        }
        if (preg_match('/&(?![A-Za-z][A-Za-z0-9]*;|#[0-9]+;|#[xX][0-9A-Fa-f]+;)/', $text) === 1) {
            return 'write an ampersand as a character entity';
        }
        preg_match_all('/&(?:[A-Za-z][A-Za-z0-9]*|#[0-9]+|#[xX][0-9A-Fa-f]+);/', $text, $entities);
        foreach ($entities[0] as $entity) {
            if (html_entity_decode($entity, ENT_QUOTES | ENT_HTML5, 'UTF-8') === $entity) {
                return 'use a recognised character entity';
            }
        }
        $form = $text;
        for ($round = 0; $round < LeafPolicy::DECODE_ROUNDS; $round++) {
            $form = html_entity_decode($form, ENT_QUOTES | ENT_HTML5, 'UTF-8');
            if (preg_match('/[<>\[\]\x{0000}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]/u', $form) === 1) {
                return 'decoded plain text cannot contain markup, brackets or control characters';
            }
        }

        return null;
    }

    /**
     * A strict tag/attribute parser: balanced tags, quoted attributes and no
     * declarations, comments, duplicate attributes or unrecognised tokens.
     * It never repairs, sanitises or rewrites the input.
     */
    private static function markup(string $text): ?string
    {
        $stack = [];
        $offset = 0;
        while ($offset < strlen($text)) {
            if ($text[$offset] !== '<') {
                $next = strpos($text, '<', $offset);
                $next = $next === false ? strlen($text) : $next;
                $chunk = substr($text, $offset, $next - $offset);
                $why = self::plain(str_replace(["\n", "\r", "\t"], ' ', $chunk));
                if ($why !== null) {
                    return $why;
                }
                $offset = $next;
                continue;
            }
            if (preg_match('/\G<(\/?)([A-Za-z][A-Za-z0-9]*)([^<>]*)>/', $text, $match, 0, $offset) !== 1) {
                return 'rich text must use the closed tag grammar';
            }
            $tag = strtolower($match[2]);
            if (!in_array($tag, self::TAGS, true)) {
                return 'rich text uses an unlisted tag';
            }
            $tail = $match[3];
            if ($match[1] === '/') {
                if (trim($tail) !== '' || array_pop($stack) !== $tag) {
                    return 'rich text tags must be balanced';
                }
            } else {
                if ($tag === 'br' && preg_match('/^\s*\/?\s*$/D', $tail) === 1) {
                    $tail = '';
                }
                $why = self::attributes($tag, $tail);
                if ($why !== null) {
                    return $why;
                }
                if ($tag !== 'br') {
                    if (count($stack) >= 8 || ($tag === 'a' && in_array('a', $stack, true))) {
                        return 'rich text nesting must stay within its closed grammar';
                    }
                    $stack[] = $tag;
                }
            }
            $offset += strlen($match[0]);
        }

        return $stack === [] ? null : 'rich text tags must be balanced';
    }

    private static function attributes(string $tag, string $tail): ?string
    {
        $seen = [];
        while (trim($tail) !== '') {
            if (preg_match('/^\s+([A-Za-z][A-Za-z0-9_-]*)\s*=\s*("[^"<>]*"|\x27[^\x27<>]*\x27)/', $tail, $match) !== 1) {
                return 'rich text attributes must use the closed quoted grammar';
            }
            $key = strtolower($match[1]);
            if ($tag !== 'a' || !in_array($key, self::LINK_ATTRIBUTES, true) || isset($seen[$key])) {
                return 'rich text uses a forbidden or repeated attribute';
            }
            $seen[$key] = true;
            $raw = substr($match[2], 1, -1);
            $why = self::plain($raw);
            if ($why !== null) {
                return $why;
            }
            $value = html_entity_decode($raw, ENT_QUOTES | ENT_HTML5, 'UTF-8');
            if (($key === 'href' && PageCreateBuilder::linkProblem($value, true) !== null)
                || ($key === 'target' && !in_array($value, ['_blank', '_self'], true))
                || ($key === 'rel' && preg_match('/^(?:nofollow|noopener|noreferrer)(?: (?:nofollow|noopener|noreferrer))*$/D', $value) !== 1)) {
                return 'rich text link attributes must use the closed link rules';
            }
            $tail = substr($tail, strlen($match[0]));
        }

        return null;
    }
}
