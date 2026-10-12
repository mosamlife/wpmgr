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
 *   even when an allowlist names them. Two keys are written only where their
 *   own rule allows, also when an allowlist names them elsewhere: the anchor
 *   (ANCHOR_KEY) only at the top of a WPMgr section container's settings,
 *   holding an anchor of the outline's form that is not reserved and not used
 *   by an earlier element of the tree; and the Navigator label (LABEL_KEY)
 *   only at the top of a container's settings, holding LABEL_PREFIX and a
 *   name. A WPMgr section container is a container at the top of the tree
 *   whose label is SECTION_LABEL. A settings path the type's own allowlist
 *   does not list is style_key_not_allowed. A key that overrides a
 *   breakpoint ("<control>_<breakpoint>") is written only for tablet and
 *   mobile, only when the allowlist lists the override and the control
 *   itself; forBreakpoints() drops the overrides of a breakpoint the site
 *   does not have active, so a tree checked against its answer holds none.
 * - L4 pins: the pin of a settings path picks the extra check for its value.
 *   A shape pin (SHAPE_PINS, or an enum with a list of values) checks the
 *   whole value at its path in exactly one written form and is never walked
 *   into; a hard-denied key anywhere inside it is still refused as a key.
 *   A key ending in FONT_FAMILY_SUFFIX holds a font family name of the
 *   outline's form.
 * - L5 groups: a member of a group control (groupOf()) is written only
 *   beside its group's activator holding the value GROUP_ACTIVATORS names,
 *   or the node is refused group_activator_missing.
 *
 * Nothing is ever stripped or rewritten: a failure refuses the whole call,
 * with leaf_unsafe for a value, adapter_key_not_allowed for a type or a
 * key outside any allowlist's reach, style_key_not_allowed for a settings
 * key an element's allowlist does not take, and group_activator_missing for
 * an orphan group member. Refusal details are WPMgr's own words and node
 * positions, never text read from the tree. This class holds no builder's
 * allowlist; each adapter passes its own.
 */
final class LeafPolicy
{
    /** A stored value that is not acceptable. */
    public const CODE_UNSAFE = 'leaf_unsafe';

    /** A node type or settings key that may not be written. */
    public const CODE_KEY = 'adapter_key_not_allowed';

    /** A settings key the element's own allowlist does not list, or a breakpoint override WPMgr does not write. */
    public const CODE_STYLE_KEY = 'style_key_not_allowed';

    /** A member of a group control written without its group's activator. */
    public const CODE_ACTIVATOR = 'group_activator_missing';

    /** The breakpoints WPMgr writes an override for: a key "<control>_tablet" or "<control>_mobile". */
    public const BREAKPOINTS_WRITTEN = ['tablet', 'mobile'];

    /** The other breakpoints a builder may define; an override for one is never written. */
    public const BREAKPOINTS_NOT_WRITTEN = ['mobile_extra', 'tablet_extra', 'laptop', 'widescreen'];

    /**
     * Group controls by group name: the activator key and the one value it
     * holds. A key "<group>_<field>" other than the activator is a member of
     * the longest group whose name it starts with, and a member is written
     * only beside its activator holding that value.
     */
    public const GROUP_ACTIVATORS = [
        'typography'              => ['typography_typography', 'custom'],
        'title_typography'        => ['title_typography_typography', 'custom'],
        'background'              => ['background_background', 'classic'],
        'background_overlay'      => ['background_overlay_background', 'classic'],
        'button_background_hover' => ['button_background_hover_background', 'classic'],
        'border'                  => ['border_border', 'solid'],
        'box_shadow'              => ['box_shadow_box_shadow_type', 'yes'],
    ];

    /** Controls that start with a group's name but are not members of it, with or without a breakpoint override. */
    public const GROUP_STANDALONE = ['border_radius'];

    /** What a settings key holding a font family name ends with. */
    public const FONT_FAMILY_SUFFIX = 'font_family';

    /** Settings keys refused anywhere, compared without regard to case. */
    public const DENIED_KEYS = [
        '__dynamic__',
        '__globals__',
        'custom_css',
        '_attributes',
        'custom_attributes',
        '_css_classes',
    ];

    /** The settings key of an element's HTML id, which is its in-page anchor. */
    public const ANCHOR_KEY = '_element_id';

    /** The settings key of an element's Navigator label. */
    public const LABEL_KEY = '_title';

    /** What every Navigator label WPMgr writes starts with. */
    public const LABEL_PREFIX = "WPMgr \u{00B7} ";

    /** The Navigator label of a WPMgr section container. */
    public const SECTION_LABEL = self::LABEL_PREFIX . 'Section';

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

    /** Pins that check the whole value at their path; an enum with a list does too. */
    public const SHAPE_PINS = ['slider', 'dimensions', 'gaps', 'colour', 'switcher', 'image', 'link', 'shadow-offset', 'repeater-id', 'counter-number'];

    /** The units a slider takes; a "slider:" argument names a subset. */
    public const SLIDER_UNITS = ['px', 'em', 'vh', '%'];

    /** The only keys an element node may carry. */
    private const NODE_KEYS = ['id', 'elType', 'widgetType', 'isInner', 'settings', 'elements'];

    /** A decimal number of 0 or more: no sign, no leading zero, up to six whole and four fraction digits. */
    private const DECIMAL = '(?:0|[1-9][0-9]{0,5})(?:\.[0-9]{1,4})?';

    /** An attachment id: a whole number above 0. */
    private const ATTACHMENT_ID = '/^[1-9][0-9]{0,18}\z/';

    /** A colour: upper-case #RRGGBB, transparent, or rgba() of three channels 0 to 255 and an alpha 0 to 1, with no spaces. */
    private const COLOUR = '/^(?:#[0-9A-F]{6}|transparent|rgba\((?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9]),){3}(?:0|1|0\.[0-9]{1,2})\))\z/';

    /** Why a value is not its shape pin's form, by pin; the slider reason names its units. */
    private const SHAPE_REASONS = [
        'slider'     => 'not a slider value: exactly unit and size, unit one of %s, size a decimal number written as a string',
        'dimensions' => 'not a dimensions value: exactly unit "px", top, right, bottom and left as decimal numbers of 0 or more written as strings, and isLinked true or false',
        'gaps'       => 'not a gaps value: exactly column and row as decimal numbers of 0 or more written as strings, isLinked true or false, and unit "px"',
        'colour'     => 'not a colour: an upper-case #RRGGBB, transparent, or rgba(r,g,b,a) with no spaces',
        'switcher'   => 'not a switcher value: "yes" or ""',
        'image'      => 'not an image value: exactly id and url, or id, url and source "library", with id a whole number above 0 written as a string',
        'link'       => 'not a link value: exactly url, is_external and nofollow, with is_external and nofollow each "on" or ""',
        'enum'       => 'not one of the values this key takes',
        'shadow-offset'  => 'not a whole number of one to three digits with an optional leading minus, written as a string',
        'repeater-id'    => 'not exactly seven lower-case hex characters',
        'counter-number' => 'not one to nine digits written as a string',
    ];

    /** Deepest element nesting walked. */
    private const MAX_NODE_DEPTH = 32;

    /** Deepest settings nesting walked. */
    private const MAX_SETTINGS_DEPTH = 8;

    /** The name after LABEL_PREFIX: words of ASCII letters and digits, the first starting with a letter, one space or hyphen between words. */
    private const LABEL_NAME = '/^[A-Za-z][A-Za-z0-9]*(?:[ -][A-Za-z0-9]+)*$/D';

    /** Longest name after LABEL_PREFIX, in characters. */
    private const MAX_LABEL_NAME_CHARS = 40;

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

        $refuseBraces = ($rules['refuse_braces'] ?? null) === true;
        foreach ($forms as $form) {
            $why = self::sequenceProblem($form, $refuseBraces, $forbidden);
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
        if (!ArrayShape::isList($nodes)) {
            return self::refuse(self::CODE_KEY, 'elements: not a list of nodes');
        }

        $anchors = [];

        return self::checkNodes($nodes, 'elements', 1, $allowedKeys, $rules, $anchors);
    }

    /**
     * @param list<mixed>                                      $nodes       Nodes.
     * @param string                                           $where       Position of the list.
     * @param int                                              $depth       Nesting depth of the list.
     * @param array<string, array<string, string>>             $allowedKeys Allowlist.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules       Leaf rules.
     * @param array<string, true>                              $anchors     Anchors already written in this tree.
     * @return array{code: string, detail: string}|null
     */
    private static function checkNodes(array $nodes, string $where, int $depth, array $allowedKeys, array $rules, array &$anchors): ?array
    {
        if ($depth > self::MAX_NODE_DEPTH) {
            return self::refuse(self::CODE_KEY, $where . ': elements nested deeper than ' . self::MAX_NODE_DEPTH . ' levels');
        }
        foreach ($nodes as $i => $node) {
            $at = $where . '[' . $i . ']';
            if (!is_array($node) || ($node !== [] && ArrayShape::isList($node))) {
                return self::refuse(self::CODE_KEY, $at . ': not a node');
            }
            $refusal = self::checkNode($node, $at, $depth, $allowedKeys, $rules, $anchors);
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
     * @param array<string, true>                              $anchors     Anchors already written in this tree.
     * @return array{code: string, detail: string}|null
     */
    private static function checkNode(array $node, string $at, int $depth, array $allowedKeys, array $rules, array &$anchors): ?array
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
        $container = $elType === 'container';
        $placed    = [
            self::ANCHOR_KEY => $container && $depth === 1 && ($settings[self::LABEL_KEY] ?? null) === self::SECTION_LABEL,
            self::LABEL_KEY  => $container,
        ];
        $refusal   = self::checkSettings($settings, '', $at . '.settings', 1, $allowedKeys[$type], $rules, $placed, $anchors);
        if ($refusal !== null) {
            return $refusal;
        }

        $children = $node['elements'] ?? [];
        if (!is_array($children) || !ArrayShape::isList($children)) {
            return self::refuse(self::CODE_KEY, $at . ': elements not a list of nodes');
        }

        return self::checkNodes($children, $at . '.elements', $depth + 1, $allowedKeys, $rules, $anchors);
    }

    /**
     * @param array<mixed>                                     $settings Settings at this level.
     * @param string                                           $prefix   Dotted path of this level ('' at the top).
     * @param string                                           $at       Position, for details.
     * @param int                                              $depth    Settings nesting depth.
     * @param array<string, string>                            $paths    The type's settings paths and pins.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules    Leaf rules.
     * @param array<string, bool>                              $placed   Whether this node may carry each ruled key at the top of its settings.
     * @param array<string, true>                              $anchors  Anchors already written in this tree.
     * @return array{code: string, detail: string}|null
     */
    private static function checkSettings(array $settings, string $prefix, string $at, int $depth, array $paths, array $rules, array $placed, array &$anchors): ?array
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
            $ruled  = self::ruledProblem($key, $value, $prefix === '' ? $placed : [], $anchors);
            if ($ruled !== null) {
                return self::refuse($ruled[0], ($ruled[0] === self::CODE_KEY ? $at : $at . '.' . $path) . ': ' . $ruled[1]);
            }
            if ($prefix === '') {
                $override = self::overrideProblem($key, $paths);
                if ($override !== null) {
                    return self::refuse(self::CODE_STYLE_KEY, $at . ': ' . $override);
                }
            }
            $listed = array_key_exists($path, $paths);
            if ($listed && $prefix === '' && self::endsWith($key, self::FONT_FAMILY_SUFFIX) && !self::isFontFamily($value)) {
                return self::refuse(self::CODE_UNSAFE, $at . '.' . $path . ': not a font family name of 1 to 60 letters, digits and spaces');
            }
            if ($listed && self::isShapePin((string) $paths[$path])) {
                // The whole value, object or string, is the pin's to check; it is never walked into.
                $why = self::pinProblem($value, (string) $paths[$path], $rules);
                if ($why !== null) {
                    return self::refuse($why[0], $at . '.' . $path . ': ' . $why[1]);
                }
                continue;
            }
            if (is_array($value) && $value !== []) {
                if (!self::isAncestor($path, $paths)) {
                    return self::refuse(self::CODE_STYLE_KEY, $at . ': a settings key not on the adapter allowlist');
                }
                $refusal = self::checkSettings($value, $path, $at, $depth + 1, $paths, $rules, [], $anchors);
                if ($refusal !== null) {
                    return $refusal;
                }
                continue;
            }
            if (!$listed && !($value === [] && self::isAncestor($path, $paths))) {
                return self::refuse(self::CODE_STYLE_KEY, $at . ': a settings key not on the adapter allowlist');
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
        if ($prefix === '') {
            $orphan = self::activatorProblem($settings);
            if ($orphan !== null) {
                return self::refuse(self::CODE_ACTIVATOR, $at . '.' . $orphan);
            }
        }

        return null;
    }

    /**
     * The rule of the anchor and label keys, checked before the allowlist:
     * null for any other key, and null for a ruled key that is spelled in
     * lower case, allowed in this place and holds an acceptable value. An
     * accepted anchor is recorded in $anchors, so a later element of the
     * same tree cannot write it again.
     *
     * @param string              $key     Settings key as given.
     * @param mixed               $value   Its value.
     * @param array<string, bool> $placed  Whether each ruled key may be written here.
     * @param array<string, true> $anchors Anchors already written in this tree.
     * @return array{0: string, 1: string}|null Code and reason.
     */
    private static function ruledProblem(string $key, $value, array $placed, array &$anchors): ?array
    {
        $ruled = strtolower($key);
        if ($ruled !== self::ANCHOR_KEY && $ruled !== self::LABEL_KEY) {
            return null;
        }
        if ($key !== $ruled) {
            return [self::CODE_KEY, 'key ' . $ruled . ' is written only in lower case'];
        }
        if (($placed[$ruled] ?? false) !== true) {
            return [self::CODE_KEY, $ruled === self::ANCHOR_KEY
                ? 'key ' . self::ANCHOR_KEY . ' is written only at the top of a WPMgr section container\'s settings'
                : 'key ' . self::LABEL_KEY . ' is written only at the top of a container\'s settings'];
        }
        if ($ruled === self::LABEL_KEY) {
            return self::isLabel($value) ? null : [self::CODE_UNSAFE, 'not a label of the WPMgr prefix, then a name of letters and digits with single spaces or hyphens between words, up to ' . self::MAX_LABEL_NAME_CHARS . ' characters'];
        }
        if (!is_string($value) || preg_match('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', $value) !== 1) {
            return [self::CODE_UNSAFE, 'not an anchor of a lower-case letter, then 1 to 39 lower-case letters, digits or hyphens'];
        }
        if (in_array($value, OutlineV3Schema::RESERVED_ANCHORS, true)) {
            return [self::CODE_UNSAFE, 'an anchor the theme or WordPress already uses'];
        }
        if (isset($anchors[$value])) {
            return [self::CODE_UNSAFE, 'an anchor an earlier element already uses'];
        }
        $anchors[$value] = true;

        return null;
    }

    /**
     * The allowlist for one site: the given one without every path whose
     * settings key overrides a breakpoint the site does not have active.
     * $breakpoints is the site's active breakpoint names (ElementorFacts
     * active_breakpoints); null means none is active, so no override is
     * kept. A caller that compiles for a site checks its tree against this.
     *
     * @param array<string, array<string, string>> $allowedKeys Type, then settings path, then pin.
     * @param list<string>|null                    $breakpoints Active breakpoint names, or null.
     * @return array<string, array<string, string>>
     */
    public static function forBreakpoints(array $allowedKeys, ?array $breakpoints): array
    {
        $active = $breakpoints ?? [];
        $site   = [];
        foreach ($allowedKeys as $type => $paths) {
            $kept = [];
            foreach ($paths as $path => $pin) {
                $breakpoint = self::breakpointOf(explode('.', (string) $path, 2)[0]);
                if ($breakpoint === null || in_array($breakpoint, $active, true)) {
                    $kept[$path] = $pin;
                }
            }
            $site[$type] = $kept;
        }

        return $site;
    }

    /**
     * The group control a settings key is a member of: the longest name in
     * GROUP_ACTIVATORS the key starts with followed by "_", or null when no
     * name matches, when the key is that group's activator, or when the key
     * without a breakpoint override is in GROUP_STANDALONE.
     *
     * @param string $key Settings key at the top of a node's settings.
     * @return string|null
     */
    public static function groupOf(string $key): ?string
    {
        $group = null;
        foreach (array_keys(self::GROUP_ACTIVATORS) as $name) {
            if (strncmp($key, $name . '_', strlen($name) + 1) === 0 && ($group === null || strlen($name) > strlen($group))) {
                $group = $name;
            }
        }
        if ($group === null || $key === self::GROUP_ACTIVATORS[$group][0]) {
            return null;
        }
        $breakpoint = self::breakpointOf($key);
        $control    = $breakpoint === null ? $key : substr($key, 0, -strlen('_' . $breakpoint));

        return in_array($control, self::GROUP_STANDALONE, true) ? null : $group;
    }

    /**
     * The breakpoint a settings key overrides: the name in
     * BREAKPOINTS_WRITTEN or BREAKPOINTS_NOT_WRITTEN that ends the key after
     * a "_" and a control name of at least one character, or null.
     *
     * @param string $key Settings key.
     * @return string|null
     */
    private static function breakpointOf(string $key): ?string
    {
        $found = null;
        foreach (array_merge(self::BREAKPOINTS_NOT_WRITTEN, self::BREAKPOINTS_WRITTEN) as $name) {
            if (strlen($key) > strlen($name) + 1 && self::endsWith($key, '_' . $name) && ($found === null || strlen($name) > strlen($found))) {
                $found = $name;
            }
        }

        return $found;
    }

    /**
     * The breakpoint rule of a key at the top of a node's settings: null for
     * a key that overrides no breakpoint, and for a tablet or mobile
     * override the type's allowlist lists while it also lists the control
     * itself. An override of any other breakpoint is refused even when an
     * allowlist names it. Refusal reasons name the breakpoint from WPMgr's
     * own lists, never the key.
     *
     * @param string                $key   Settings key as given.
     * @param array<string, string> $paths The type's settings paths and pins.
     * @return string|null Reason.
     */
    private static function overrideProblem(string $key, array $paths): ?string
    {
        $breakpoint = self::breakpointOf($key);
        if ($breakpoint === null) {
            return null;
        }
        if (!in_array($breakpoint, self::BREAKPOINTS_WRITTEN, true)) {
            return 'a ' . $breakpoint . ' override, which WPMgr does not write; it writes only tablet and mobile overrides';
        }
        if (!array_key_exists($key, $paths) && !self::isAncestor($key, $paths)) {
            return 'a ' . $breakpoint . ' override WPMgr writes only for a control it lists as responsive on this element, on a site with the ' . $breakpoint . ' breakpoint active';
        }
        $control = substr($key, 0, -strlen('_' . $breakpoint));
        if (!array_key_exists($control, $paths) && !self::isAncestor($control, $paths)) {
            return 'a ' . $breakpoint . ' override of a control the allowlist does not list for this element';
        }

        return null;
    }

    /**
     * The group rule over the top of a node's settings: the first key that is
     * a member of a group (groupOf()) while that group's activator is missing
     * or holds another value, as "<key>: <reason>", or null. Every key has
     * passed the allowlist before this runs, so the key named is one the
     * allowlist lists.
     *
     * @param array<mixed> $settings Settings at the top of a node.
     * @return string|null
     */
    private static function activatorProblem(array $settings): ?string
    {
        foreach (array_keys($settings) as $key) {
            $key   = (string) $key;
            $group = self::groupOf($key);
            if ($group === null) {
                continue;
            }
            [$activator, $on] = self::GROUP_ACTIVATORS[$group];
            if (($settings[$activator] ?? null) !== $on) {
                return $key . ': a member of the ' . $group . ' group, written only beside ' . $activator . ' set to "' . $on . '"';
            }
        }

        return null;
    }

    /**
     * Whether a value is a font family name the outline accepts
     * (OutlineV3Schema::FONT_PATTERN).
     *
     * @param mixed $value Value.
     * @return bool
     */
    private static function isFontFamily($value): bool
    {
        return is_string($value) && preg_match('/' . OutlineV3Schema::FONT_PATTERN . '/D', $value) === 1;
    }

    /**
     * Whether a string ends with another.
     *
     * @param string $text   Text.
     * @param string $suffix Suffix.
     * @return bool
     */
    private static function endsWith(string $text, string $suffix): bool
    {
        return $suffix !== '' && substr($text, -strlen($suffix)) === $suffix;
    }

    /**
     * Whether a value is LABEL_PREFIX followed by a name LABEL_NAME accepts,
     * of at most MAX_LABEL_NAME_CHARS characters.
     *
     * @param mixed $value Value.
     * @return bool
     */
    private static function isLabel($value): bool
    {
        if (!is_string($value) || strncmp($value, self::LABEL_PREFIX, strlen(self::LABEL_PREFIX)) !== 0) {
            return false;
        }
        $name = substr($value, strlen(self::LABEL_PREFIX));

        return strlen($name) <= self::MAX_LABEL_NAME_CHARS && preg_match(self::LABEL_NAME, $name) === 1;
    }

    /**
     * L4, then L2 for every string: the check a value's pin selects.
     *
     * A pin is one of BuilderAdapter::PINS, optionally followed by a colon
     * and an argument of values separated by "|"; only enum (the values it
     * takes) and slider (a subset of SLIDER_UNITS) take one.
     *
     * @param mixed                                            $value Leaf value, or the whole value of a shape pin.
     * @param string                                           $pin   Pin, with its argument if any.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules Leaf rules.
     * @return array{0: string, 1: string}|null Code and reason.
     */
    private static function pinProblem($value, string $pin, array $rules): ?array
    {
        $parts   = explode(':', $pin, 2);
        $name    = $parts[0];
        $choices = isset($parts[1]) ? explode('|', $parts[1]) : null;
        if (!in_array($name, BuilderAdapter::PINS, true)) {
            return [self::CODE_KEY, 'a pin outside ' . implode(', ', BuilderAdapter::PINS)];
        }
        if ($choices !== null && !self::takesArgument($name, $choices)) {
            return [self::CODE_KEY, 'a pin argument its pin does not take'];
        }
        if (in_array($name, self::SHAPE_PINS, true) || $choices !== null) {
            return self::shapeProblem($value, $name, $choices, $rules);
        }
        if ($name === 'int') {
            return is_int($value) ? null : [self::CODE_UNSAFE, 'int value that is not an integer'];
        }
        if (!is_string($value)) {
            return [self::CODE_UNSAFE, $pin . ' value that is not a string'];
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
     * Whether the allowlist pins a settings path with a shape pin: the pin
     * checks the whole value there, as checkTree() takes it, and no part of it.
     *
     * @param array<string, string> $paths One element type's settings paths and pins.
     * @param string                $path  A dotted settings path.
     * @return bool
     */
    public static function judgesWhole(array $paths, string $path): bool
    {
        return array_key_exists($path, $paths) && self::isShapePin((string) $paths[$path]);
    }

    /**
     * Whether a pin checks the whole value at its path: a shape pin, with or
     * without an argument, or an enum with a list of values.
     *
     * @param string $pin Pin, with its argument if any.
     * @return bool
     */
    private static function isShapePin(string $pin): bool
    {
        $parts = explode(':', $pin, 2);

        return in_array($parts[0], self::SHAPE_PINS, true) || ($parts[0] === 'enum' && isset($parts[1]));
    }

    /**
     * Whether a pin takes the argument it carries: enum takes any list of
     * values, slider a list of units from SLIDER_UNITS, every other pin none.
     *
     * @param string       $name    Pin name.
     * @param list<string> $choices The argument's values.
     * @return bool
     */
    private static function takesArgument(string $name, array $choices): bool
    {
        if ($name === 'enum') {
            return true;
        }
        if ($name !== 'slider') {
            return false;
        }
        foreach ($choices as $unit) {
            if (!in_array($unit, self::SLIDER_UNITS, true)) {
                return false;
            }
        }

        return true;
    }

    /**
     * The check of a shape pin or an enum with a list, over the whole value:
     * a hard-denied key anywhere inside refuses it as a key, then the value
     * must be the pin's form exactly, an image or link address must pass the
     * page-create rule, and every string in it must pass L2.
     *
     * @param mixed                                            $value   Whole value.
     * @param string                                           $name    Pin name.
     * @param list<string>|null                                $choices The pin's argument: enum values or slider units.
     * @param array{refuse_braces?: mixed, forbidden?: mixed}  $rules   Leaf rules.
     * @return array{0: string, 1: string}|null Code and reason.
     */
    private static function shapeProblem($value, string $name, ?array $choices, array $rules): ?array
    {
        if (is_array($value)) {
            $denied = self::deniedWithin($value, 1);
            if ($denied !== null) {
                return [self::CODE_KEY, 'key ' . $denied . ' is never written'];
            }
        }
        if (!self::hasShape($value, $name, $choices)) {
            $units = $name === 'slider' ? implode(', ', $choices ?? self::SLIDER_UNITS) : '';

            return [self::CODE_UNSAFE, sprintf(self::SHAPE_REASONS[$name], $units)];
        }
        if ($name === 'image' || $name === 'link') {
            // hasShape() has made the address a string; a link may also be "#" and a section anchor.
            $url = is_array($value) ? ($value['url'] ?? null) : null;
            if (!is_string($url) || ($name === 'image' ? PageCreateBuilder::imageUrlProblem($url) : PageCreateBuilder::linkProblem($url, true)) !== null) {
                return [self::CODE_UNSAFE, 'not an address the page-create ' . $name . ' rule accepts'];
            }
        }
        foreach (is_array($value) ? $value : [$value] as $member) {
            if (!is_string($member)) {
                continue;
            }
            $why = self::storedProblem($member, $rules);
            if ($why !== null) {
                return [self::CODE_UNSAFE, $why];
            }
        }

        return null;
    }

    /**
     * Whether a value is exactly its pin's form. Members may come in any
     * order; none may be missing, extra or spelled differently.
     *
     * @param mixed             $value   Whole value.
     * @param string            $name    Pin name.
     * @param list<string>|null $choices The pin's argument: enum values or slider units.
     * @return bool
     */
    private static function hasShape($value, string $name, ?array $choices): bool
    {
        switch ($name) {
            case 'enum':
                return is_string($value) && in_array($value, $choices ?? [], true);
            case 'shadow-offset':
                return is_string($value) && preg_match('/^-?[0-9]{1,3}\z/', $value) === 1;
            case 'repeater-id':
                return is_string($value) && preg_match('/^[0-9a-f]{7}\z/', $value) === 1;
            case 'counter-number':
                return is_string($value) && preg_match('/^[0-9]{1,9}\z/', $value) === 1;
            case 'colour':
                return is_string($value) && preg_match(self::COLOUR, $value) === 1;
            case 'switcher':
                return $value === 'yes' || $value === '';
            case 'slider':
                return self::hasMembers($value, ['unit', 'size'])
                    && in_array($value['unit'], $choices ?? self::SLIDER_UNITS, true)
                    && is_string($value['size'])
                    && preg_match('/^-?' . self::DECIMAL . '\z/', $value['size']) === 1;
            case 'dimensions':
                return self::hasMembers($value, ['unit', 'top', 'right', 'bottom', 'left', 'isLinked'])
                    && $value['unit'] === 'px'
                    && is_bool($value['isLinked'])
                    && self::isDecimal($value['top'])
                    && self::isDecimal($value['right'])
                    && self::isDecimal($value['bottom'])
                    && self::isDecimal($value['left']);
            case 'gaps':
                return self::hasMembers($value, ['column', 'row', 'isLinked', 'unit'])
                    && $value['unit'] === 'px'
                    && is_bool($value['isLinked'])
                    && self::isDecimal($value['column'])
                    && self::isDecimal($value['row']);
            case 'image':
                $sourced = is_array($value) && array_key_exists('source', $value);

                return self::hasMembers($value, $sourced ? ['id', 'url', 'source'] : ['id', 'url'])
                    && (!$sourced || $value['source'] === 'library')
                    && is_string($value['id'])
                    && preg_match(self::ATTACHMENT_ID, $value['id']) === 1
                    && is_string($value['url']);
            case 'link':
                return self::hasMembers($value, ['url', 'is_external', 'nofollow'])
                    && is_string($value['url'])
                    && in_array($value['is_external'], ['on', ''], true)
                    && in_array($value['nofollow'], ['on', ''], true);
        }

        return false;
    }

    /**
     * Whether a value is an object with exactly these members.
     *
     * @param mixed        $value   Value.
     * @param list<string> $members Member names.
     * @return bool
     */
    private static function hasMembers($value, array $members): bool
    {
        if (!is_array($value) || count($value) !== count($members)) {
            return false;
        }
        foreach ($members as $member) {
            if (!array_key_exists($member, $value)) {
                return false;
            }
        }

        return true;
    }

    /**
     * Whether a value is a decimal number of 0 or more written as a string.
     *
     * @param mixed $value Value.
     * @return bool
     */
    private static function isDecimal($value): bool
    {
        return is_string($value) && preg_match('/^' . self::DECIMAL . '\z/', $value) === 1;
    }

    /**
     * The denylist entry a key anywhere inside a value matches, or null.
     * Nesting past MAX_SETTINGS_DEPTH is not walked; no shape holds any.
     *
     * @param array<mixed> $value Value.
     * @param int          $depth Nesting depth.
     * @return string|null
     */
    private static function deniedWithin(array $value, int $depth): ?string
    {
        foreach ($value as $key => $member) {
            $denied = self::deniedKey((string) $key);
            if ($denied !== null) {
                return $denied;
            }
            if (is_array($member) && $depth < self::MAX_SETTINGS_DEPTH) {
                $inner = self::deniedWithin($member, $depth + 1);
                if ($inner !== null) {
                    return $inner;
                }
            }
        }

        return null;
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
        if (!is_array($forbidden) || !ArrayShape::isList($forbidden)) {
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
