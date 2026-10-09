<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * What a builder document WPMgr stores may hold.
 *
 * Four layers, each checked on its own; one never stands in for another:
 *
 * - L1 grammar: raw text obeys the page-create TEXT, CELL and ALT rules,
 *   re-checked here before any escaping.
 * - L2 stored string: every string stored in a node equals what the HTML
 *   sanitiser makes of it with inline styles allowed nowhere, and holds none
 *   of the forbidden sequences, matched without regard to case, in its stored
 *   form and in every form reached by decoding character references (at most
 *   three rounds, which must reach a fixed point).
 * - L3 keys: every node type and every settings path is on the adapter's
 *   allowlist, and the hard-denied types and keys are refused at any depth
 *   even when an allowlist names them.
 * - L4 pins: the pin of a settings path picks the extra check for its value.
 *
 * Nothing is ever stripped or rewritten: a failure refuses the whole call,
 * with leaf_unsafe for a value and adapter_key_not_allowed for a type or a
 * key. Refusal details are WPMgr's own words and node positions, never text
 * read from the tree. This class holds no builder's allowlist; each adapter
 * passes its own.
 */
final class LeafPolicy
{
    /** A stored value that is not acceptable. */
    public const CODE_UNSAFE = 'leaf_unsafe';

    /** A node type or settings key that may not be written. */
    public const CODE_KEY = 'adapter_key_not_allowed';

    /** Settings keys refused anywhere, compared without regard to case. */
    public const DENIED_KEYS = [
        '__dynamic__',
        '__globals__',
        'custom_css',
        '_attributes',
        'custom_attributes',
        '_element_id',
        '_css_classes',
    ];

    /** Settings key prefixes refused anywhere, compared without regard to case. */
    public const DENIED_KEY_PREFIXES = ['motion_fx_', 'sticky'];

    /** Node types refused anywhere, compared without regard to case. */
    public const DENIED_TYPES = [
        'html',
        'shortcode',
        'text-path',
        'menu-anchor',
        'sidebar',
        'template',
        'e-svg',
        'e-component',
        'e-self-hosted-video',
        'e-youtube',
    ];

    /** Node type prefixes refused anywhere, compared without regard to case. */
    public const DENIED_TYPE_PREFIXES = ['wp-widget-', 'e-form'];

    /** Sequences no stored string may hold in any decoded form, compared without regard to case. */
    public const FORBIDDEN = ['<script', '<iframe', '<object', '<embed', '<style', '<?', '{echo:'];

    /** URL schemes no stored string may name; ASCII whitespace and controls inside them are ignored. */
    public const FORBIDDEN_SCHEMES = ['javascript:', 'vbscript:', 'data:text/html'];

    /** Rounds of character-reference decoding before a stored string must be at a fixed point. */
    public const DECODE_ROUNDS = 3;

    /** The only keys an element node may carry. */
    private const NODE_KEYS = ['id', 'elType', 'widgetType', 'isInner', 'settings', 'elements'];

    /** Deepest element nesting walked. */
    private const MAX_NODE_DEPTH = 32;

    /** Deepest settings nesting walked. */
    private const MAX_SETTINGS_DEPTH = 8;

    /** Sequences refused anywhere in raw text: the page-create rule. */
    private const RAW_FORBIDDEN = ['<', '>', '{{', '}}', '{%', '%}', '<!--', '-->', '`'];

    /** The only bracketed raw text allowed: a number of one to four digits. */
    private const BRACKETED_NUMBER = '/\[[0-9]{1,4}\]/';

    /** An ampersand that starts a character reference. */
    private const CHARACTER_REFERENCE = '/&(?:[A-Za-z0-9]+|#[0-9]+|#[xX][0-9A-Fa-f]+);/';

    /** An ampersand that does not start a character reference. */
    private const BARE_AMPERSAND = '/&(?!(?:[A-Za-z][A-Za-z0-9]*|#[0-9]+|#[xX][0-9A-Fa-f]+);)/';

    /** An event-handler attribute name followed by "=". */
    private const EVENT_HANDLER = '/\bon[a-z]+\s*=/i';

    /** A shortcode opener. */
    private const SHORTCODE_OPENER = '/\[[a-z]/i';

    /**
     * Escape plain text for a field its builder prints as HTML: every
     * character special to HTML text is escaped, so an entity-like input shows
     * literally, and square brackets become the three-digit numeric references
     * the core entity normaliser keeps unchanged, so no shortcode can form.
     * The same bytes PageCreateBuilder stores for a title.
     *
     * @param string $text Plain text, already checked by textProblem().
     * @return string
     */
    public static function htmlText(string $text): string
    {
        return str_replace(['[', ']'], ['&#091;', '&#093;'], htmlspecialchars($text, ENT_NOQUOTES | ENT_SUBSTITUTE, 'UTF-8'));
    }

    /**
     * L1: why raw text breaks the page-create rule it is held to, or null.
     *
     * Rules: 'text' (not empty, up to PageCreateBuilder::MAX_TEXT_CHARS),
     * 'cell' (may be empty, up to MAX_CELL_CHARS) and 'alt' (may be empty, up
     * to MAX_ALT_CHARS, no square bracket and no character reference).
     * Narrower per-field limits are the validator's; this re-check guards the
     * content rules. An unknown rule is a problem.
     *
     * @param string $raw  Raw text.
     * @param string $rule 'text', 'cell' or 'alt'.
     * @return string|null
     */
    public static function textProblem(string $raw, string $rule): ?string
    {
        $limits = [
            'text' => PageCreateBuilder::MAX_TEXT_CHARS,
            'cell' => PageCreateBuilder::MAX_CELL_CHARS,
            'alt'  => PageCreateBuilder::MAX_ALT_CHARS,
        ];
        if (!isset($limits[$rule])) {
            return 'unknown text rule';
        }
        if (preg_match('//u', $raw) !== 1) {
            return 'not valid UTF-8';
        }
        if ($raw === '' && $rule !== 'text') {
            return null;
        }
        if (trim($raw) === '') {
            return 'empty';
        }
        if (self::chars($raw) > $limits[$rule]) {
            return 'too long';
        }
        // Control characters (newlines included), bidi overrides and isolates, BOM, noncharacters.
        if (preg_match('/[\x{0000}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]/u', $raw) === 1) {
            return 'contains a control or invisible formatting character';
        }
        foreach (self::RAW_FORBIDDEN as $seq) {
            if (strpos($raw, $seq) !== false) {
                return 'contains "' . $seq . '"; write plain text with no markup, comments or template syntax';
            }
        }
        if ($rule === 'alt') {
            if (strpos($raw, '[') !== false || strpos($raw, ']') !== false) {
                return 'contains a square bracket; alt text cannot hold one, so use parentheses instead';
            }
            if (preg_match(self::CHARACTER_REFERENCE, $raw) === 1) {
                return 'contains "&" then a name or number and ";", which reads as a character reference; write the character itself, or put a space after the "&"';
            }

            return null;
        }
        $unbracketed = (string) preg_replace(self::BRACKETED_NUMBER, '', $raw);
        if (strpos($unbracketed, '[') !== false || strpos($unbracketed, ']') !== false) {
            return 'contains a square bracket; only a bracketed number such as [1] is allowed, so use parentheses instead';
        }

        return null;
    }

    /**
     * L2: why a string as it would be stored is not acceptable, or null.
     *
     * In order: valid UTF-8; unchanged by wp_kses_post() while a
     * safe_style_css filter returning no properties is added at PHP_INT_MAX
     * (removed again before returning, also when the sanitiser throws); its
     * character references decode to a fixed point within DECODE_ROUNDS
     * rounds; and neither the stored form nor any decoded form holds a
     * FORBIDDEN sequence, a FORBIDDEN_SCHEMES scheme, an event-handler
     * attribute, a shortcode opener, a brace when $rules refuse braces, or one
     * of the adapter's forbidden substrings.
     *
     * @param string                                             $stored Stored string.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}    $rules  The adapter's leafRules().
     * @return string|null
     */
    public static function storedProblem(string $stored, array $rules): ?string
    {
        $forbidden = self::adapterForbidden($rules);
        if ($forbidden === null) {
            return 'the adapter leaf rules are malformed';
        }
        if (preg_match('//u', $stored) !== 1) {
            return 'not valid UTF-8';
        }
        if (!self::ksesFixedPoint($stored)) {
            return 'changed by the HTML sanitiser';
        }

        $forms   = [$stored];
        $current = $stored;
        for ($round = 0; $round < self::DECODE_ROUNDS; $round++) {
            $next = html_entity_decode($current, ENT_QUOTES | ENT_HTML5, 'UTF-8');
            if ($next === $current) {
                break;
            }
            $forms[] = $next;
            $current = $next;
        }
        if (html_entity_decode($current, ENT_QUOTES | ENT_HTML5, 'UTF-8') !== $current) {
            return 'character references nested deeper than ' . self::DECODE_ROUNDS . ' levels';
        }

        foreach ($forms as $form) {
            $why = self::sequenceProblem($form, $rules['refuse_braces'] === true, $forbidden);
            if ($why !== null) {
                return $why;
            }
        }

        return null;
    }

    /**
     * L3 and L4 over an element tree: every node, depth first in document
     * order. Returns the first refusal, or null when the whole tree may be
     * stored.
     *
     * A node is an object with only the keys id (string), elType (string),
     * widgetType (string, exactly when elType is "widget"), isInner (bool),
     * settings (object or empty list) and elements (list of nodes). Its type
     * is widgetType for a widget and elType otherwise.
     *
     * @param array<mixed>                                     $nodes       Top-level nodes.
     * @param array<string, array<string, string>>             $allowedKeys Type, then settings path, then pin.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules       The adapter's leafRules().
     * @return array{code: string, detail: string}|null
     */
    public static function checkTree(array $nodes, array $allowedKeys, array $rules): ?array
    {
        if (self::adapterForbidden($rules) === null) {
            return self::refuse(self::CODE_UNSAFE, 'the adapter leaf rules are malformed');
        }
        if (!array_is_list($nodes)) {
            return self::refuse(self::CODE_KEY, 'elements: not a list of nodes');
        }

        return self::checkNodes($nodes, 'elements', 1, $allowedKeys, $rules);
    }

    /**
     * @param list<mixed>                                      $nodes       Nodes.
     * @param string                                           $where       Position of the list.
     * @param int                                              $depth       Nesting depth of the list.
     * @param array<string, array<string, string>>             $allowedKeys Allowlist.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules       Leaf rules.
     * @return array{code: string, detail: string}|null
     */
    private static function checkNodes(array $nodes, string $where, int $depth, array $allowedKeys, array $rules): ?array
    {
        if ($depth > self::MAX_NODE_DEPTH) {
            return self::refuse(self::CODE_KEY, $where . ': elements nested deeper than ' . self::MAX_NODE_DEPTH . ' levels');
        }
        foreach ($nodes as $i => $node) {
            $at = $where . '[' . $i . ']';
            if (!is_array($node) || ($node !== [] && array_is_list($node))) {
                return self::refuse(self::CODE_KEY, $at . ': not a node');
            }
            $refusal = self::checkNode($node, $at, $depth, $allowedKeys, $rules);
            if ($refusal !== null) {
                return $refusal;
            }
        }

        return null;
    }

    /**
     * @param array<mixed>                                     $node        Node.
     * @param string                                           $at          Position of the node.
     * @param int                                              $depth       Nesting depth.
     * @param array<string, array<string, string>>             $allowedKeys Allowlist.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules       Leaf rules.
     * @return array{code: string, detail: string}|null
     */
    private static function checkNode(array $node, string $at, int $depth, array $allowedKeys, array $rules): ?array
    {
        foreach (array_keys($node) as $key) {
            $denied = self::deniedKey((string) $key);
            if ($denied !== null) {
                return self::refuse(self::CODE_KEY, $at . ': key ' . $denied . ' is never written');
            }
            if (!in_array($key, self::NODE_KEYS, true)) {
                return self::refuse(self::CODE_KEY, $at . ': a node key outside id, elType, widgetType, isInner, settings and elements');
            }
        }

        $elType = $node['elType'] ?? null;
        if (!is_string($elType) || $elType === '') {
            return self::refuse(self::CODE_KEY, $at . ': elType missing or not a string');
        }
        if ($elType === 'widget') {
            $type = $node['widgetType'] ?? null;
            if (!is_string($type) || $type === '') {
                return self::refuse(self::CODE_KEY, $at . ': a widget without a widgetType');
            }
        } else {
            if (array_key_exists('widgetType', $node)) {
                return self::refuse(self::CODE_KEY, $at . ': widgetType on an element that is not a widget');
            }
            $type = $elType;
        }
        foreach ([$elType, $type] as $name) {
            $denied = self::deniedType($name);
            if ($denied !== null) {
                return self::refuse(self::CODE_KEY, $at . ': element type ' . $denied . ' is never written');
            }
        }
        if (!isset($allowedKeys[$type]) || !is_array($allowedKeys[$type])) {
            return self::refuse(self::CODE_KEY, $at . ': an element type not on the adapter allowlist');
        }

        $id = $node['id'] ?? null;
        if (!is_string($id) || $id === '') {
            return self::refuse(self::CODE_KEY, $at . ': id missing or not a string');
        }
        foreach (['id' => $id, 'elType' => $elType, 'widgetType' => $node['widgetType'] ?? null] as $field => $value) {
            if (!is_string($value)) {
                continue;
            }
            $why = self::storedProblem($value, $rules);
            if ($why !== null) {
                return self::refuse(self::CODE_UNSAFE, $at . '.' . $field . ': ' . $why);
            }
        }
        if (array_key_exists('isInner', $node) && !is_bool($node['isInner'])) {
            return self::refuse(self::CODE_KEY, $at . ': isInner not a boolean');
        }

        $settings = $node['settings'] ?? [];
        if (!is_array($settings)) {
            return self::refuse(self::CODE_KEY, $at . ': settings not an object');
        }
        $refusal = self::checkSettings($settings, '', $at . '.settings', 1, $allowedKeys[$type], $rules);
        if ($refusal !== null) {
            return $refusal;
        }

        $children = $node['elements'] ?? [];
        if (!is_array($children) || !array_is_list($children)) {
            return self::refuse(self::CODE_KEY, $at . ': elements not a list of nodes');
        }

        return self::checkNodes($children, $at . '.elements', $depth + 1, $allowedKeys, $rules);
    }

    /**
     * @param array<mixed>                                     $settings Settings at this level.
     * @param string                                           $prefix   Dotted path of this level ('' at the top).
     * @param string                                           $at       Position, for details.
     * @param int                                              $depth    Settings nesting depth.
     * @param array<string, string>                            $paths    The type's settings paths and pins.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules    Leaf rules.
     * @return array{code: string, detail: string}|null
     */
    private static function checkSettings(array $settings, string $prefix, string $at, int $depth, array $paths, array $rules): ?array
    {
        if ($depth > self::MAX_SETTINGS_DEPTH) {
            return self::refuse(self::CODE_KEY, $at . ': settings nested deeper than ' . self::MAX_SETTINGS_DEPTH . ' levels');
        }
        foreach ($settings as $key => $value) {
            $key    = (string) $key;
            $denied = self::deniedKey($key);
            if ($denied !== null) {
                return self::refuse(self::CODE_KEY, $at . ': key ' . $denied . ' is never written');
            }
            $path   = $prefix === '' ? $key : $prefix . '.' . $key;
            $listed = array_key_exists($path, $paths);
            if (is_array($value) && $value !== []) {
                if (!self::isAncestor($path, $paths)) {
                    return self::refuse(self::CODE_KEY, $at . ': a settings key not on the adapter allowlist');
                }
                $refusal = self::checkSettings($value, $path, $at, $depth + 1, $paths, $rules);
                if ($refusal !== null) {
                    return $refusal;
                }
                continue;
            }
            if (!$listed && !($value === [] && self::isAncestor($path, $paths))) {
                return self::refuse(self::CODE_KEY, $at . ': a settings key not on the adapter allowlist');
            }
            if ($value === []) {
                // An empty list holds no value to check.
                continue;
            }
            $why = self::pinProblem($value, (string) $paths[$path], $rules);
            if ($why !== null) {
                return self::refuse($why[0], $at . '.' . $path . ': ' . $why[1]);
            }
        }

        return null;
    }

    /**
     * L4, then L2 for every string: the check a value's pin selects.
     *
     * @param mixed                                            $value Leaf value.
     * @param string                                           $pin   One of BuilderAdapter::PINS.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules Leaf rules.
     * @return array{0: string, 1: string}|null Code and reason.
     */
    private static function pinProblem($value, string $pin, array $rules): ?array
    {
        switch ($pin) {
            case 'int':
                return is_int($value) ? null : [self::CODE_UNSAFE, 'an int value that is not an integer'];
            case 'html':
            case 'text':
            case 'url':
            case 'enum':
                break;
            default:
                return [self::CODE_KEY, 'a pin outside ' . implode(', ', BuilderAdapter::PINS)];
        }
        if (!is_string($value)) {
            return [self::CODE_UNSAFE, 'a ' . $pin . ' value that is not a string'];
        }
        if ($pin === 'html' && preg_match(self::BARE_AMPERSAND, $value) === 1) {
            return [self::CODE_UNSAFE, 'an "&" that does not start a character reference'];
        }
        if ($pin === 'url' && PageCreateBuilder::linkProblem($value) !== null && PageCreateBuilder::imageUrlProblem($value) !== null) {
            return [self::CODE_UNSAFE, 'not an address the page-create link or image rule accepts'];
        }
        $why = self::storedProblem($value, $rules);

        return $why === null ? null : [self::CODE_UNSAFE, $why];
    }

    /**
     * Whether the sanitiser leaves the string unchanged with every inline
     * style property refused. The filter is in place only for this call.
     *
     * @param string $stored Stored string.
     * @return bool
     */
    private static function ksesFixedPoint(string $stored): bool
    {
        if (!function_exists('wp_kses_post') || !function_exists('add_filter') || !function_exists('remove_filter')) {
            return false;
        }
        $noStyles = static function (): array {
            return [];
        };
        add_filter('safe_style_css', $noStyles, PHP_INT_MAX);
        try {
            $sanitised = wp_kses_post($stored);
        } finally {
            remove_filter('safe_style_css', $noStyles, PHP_INT_MAX);
        }

        return $sanitised === $stored;
    }

    /**
     * @param string       $form          One form of a stored string.
     * @param bool         $refuseBraces  Whether a brace refuses it.
     * @param list<string> $forbidden     The adapter's forbidden substrings.
     * @return string|null
     */
    private static function sequenceProblem(string $form, bool $refuseBraces, array $forbidden): ?string
    {
        foreach (self::FORBIDDEN as $seq) {
            if (stripos($form, $seq) !== false) {
                return 'holds "' . $seq . '"';
            }
        }
        $compact = (string) preg_replace('/[\x00-\x20]+/', '', $form);
        foreach (self::FORBIDDEN_SCHEMES as $scheme) {
            if (stripos($compact, $scheme) !== false) {
                return 'holds "' . $scheme . '"';
            }
        }
        if (preg_match(self::EVENT_HANDLER, $form) === 1) {
            return 'holds an event-handler attribute';
        }
        if (preg_match(self::SHORTCODE_OPENER, $form) === 1) {
            return 'holds a shortcode opener';
        }
        if ($refuseBraces && (strpos($form, '{') !== false || strpos($form, '}') !== false)) {
            return 'holds a brace, which this builder reads as dynamic data';
        }
        foreach ($forbidden as $seq) {
            if (stripos($form, $seq) !== false) {
                return 'holds "' . $seq . '", which this builder refuses';
            }
        }

        return null;
    }

    /**
     * The adapter's forbidden substrings, or null when the rules are
     * malformed: refuse_braces must be a boolean and forbidden a list of
     * non-empty strings.
     *
     * @param array<mixed> $rules Leaf rules.
     * @return list<string>|null
     */
    private static function adapterForbidden(array $rules): ?array
    {
        if (!isset($rules['refuse_braces']) || !is_bool($rules['refuse_braces'])) {
            return null;
        }
        $forbidden = $rules['forbidden'] ?? null;
        if (!is_array($forbidden) || !array_is_list($forbidden)) {
            return null;
        }
        $out = [];
        foreach ($forbidden as $seq) {
            if (!is_string($seq) || $seq === '') {
                return null;
            }
            $out[] = $seq;
        }

        return $out;
    }

    /**
     * The denylist entry a key matches, or null.
     *
     * @param string $key Key.
     * @return string|null
     */
    private static function deniedKey(string $key): ?string
    {
        $lower = strtolower($key);
        if (in_array($lower, self::DENIED_KEYS, true)) {
            return $lower;
        }
        foreach (self::DENIED_KEY_PREFIXES as $prefix) {
            if (strncmp($lower, $prefix, strlen($prefix)) === 0) {
                return $prefix . '*';
            }
        }

        return null;
    }

    /**
     * The denylist entry a node type matches, or null.
     *
     * @param string $type Type.
     * @return string|null
     */
    private static function deniedType(string $type): ?string
    {
        $lower = strtolower($type);
        if (in_array($lower, self::DENIED_TYPES, true)) {
            return $lower;
        }
        foreach (self::DENIED_TYPE_PREFIXES as $prefix) {
            if (strncmp($lower, $prefix, strlen($prefix)) === 0) {
                return $prefix . '*';
            }
        }

        return null;
    }

    /**
     * Whether a listed settings path lies under $path.
     *
     * @param string                $path  Dotted path.
     * @param array<string, string> $paths Listed paths.
     * @return bool
     */
    private static function isAncestor(string $path, array $paths): bool
    {
        $prefix = $path . '.';
        foreach (array_keys($paths) as $listed) {
            if (strncmp((string) $listed, $prefix, strlen($prefix)) === 0) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param string $code   Code.
     * @param string $detail Detail.
     * @return array{code: string, detail: string}
     */
    private static function refuse(string $code, string $detail): array
    {
        return ['code' => $code, 'detail' => $detail];
    }

    /**
     * @param string $text Text.
     * @return int
     */
    private static function chars(string $text): int
    {
        return function_exists('mb_strlen') ? mb_strlen($text, 'UTF-8') : strlen($text);
    }
}
