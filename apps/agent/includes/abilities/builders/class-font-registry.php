<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The design's fonts against the site's Elementor font registry.
 *
 * A design names a heading and a body font. Each is the literal `system`,
 * which sets no family so the theme's own applies, or a family name. A family
 * name must match the closed pattern (1 to 60 letters, digits and spaces) and
 * must then be a family in Elementor's font registry on this site, spelled
 * exactly as Elementor spells it.
 *
 * The pattern is applied to both roles before the registry is read, so a name
 * outside it is refused without reaching the registry or any style. It is the
 * only family text a style may carry; DesignTokens applies the same pattern
 * again where the families become tokens.
 *
 * A family the registry lacks is refused with font_unknown and up to
 * MAX_CLOSE close names from the registry, each one a name the pattern admits
 * and short enough to be listed whole. No refusal text is taken from the
 * input.
 *
 * Pure: the registry is read through the closure the caller passes, at most
 * once, and only when a role names a family.
 */
final class FontRegistry
{
    /** The refusal for a family the site's Elementor does not have. */
    public const CODE_UNKNOWN = 'font_unknown';

    /** The design's font roles, in the order they are judged. */
    public const ROLES = ['heading', 'body'];

    /** The name that sets no family. */
    public const SYSTEM = 'system';

    /** Most close names offered with font_unknown. */
    public const MAX_CLOSE = 5;

    /** The builder_not_available detail when the registry cannot be read. */
    public const UNAVAILABLE = 'font_registry_unavailable';

    /** Why a family the registry lacks is refused. */
    public const REASON_UNKNOWN = 'Elementor on this site has no font family by this name: name one it has, or system to set no family';

    /** Why a name outside the closed pattern is refused. */
    public const REASON_PATTERN = 'a font family is 1 to 60 letters, digits and spaces, or system to set no family';

    /** Why a design without a fonts object is refused. */
    public const REASON_OBJECT = 'the design names a heading and a body font';

    /** The closed family pattern, as the input schema states it. */
    private const RE_FAMILY = '/' . OutlineV3Schema::FONT_PATTERN . '/D';

    /** The input path of the design's fonts. */
    private const PATH = 'design.fonts';

    /** The shortest name that counts as close by being inside another. */
    private const MIN_CONTAINED = 3;

    /**
     * Whether the design's fonts can be written on this site, as a refusal.
     *
     * Null when each role is `system` or a registry family. Otherwise, in
     * this order: bad_input when $fonts is not an object or a role is not a
     * name the closed pattern admits (the registry is not read);
     * builder_not_available with UNAVAILABLE when a role names a family and
     * the registry cannot be read; font_unknown naming each role whose family
     * the registry lacks, with its close names as `allowed`.
     *
     * @param mixed                            $fonts    The design's `fonts` object.
     * @param \Closure(): (array<mixed>|null) $registry Reads the registry's family names; null when it cannot be read.
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}|null
     */
    public static function refusal(mixed $fonts, \Closure $registry): ?array
    {
        if (!is_array($fonts)) {
            return Refusal::make('bad_input', 'the design has no fonts object.', [['path' => 'design', 'key' => 'fonts', 'reason' => self::REASON_OBJECT]]);
        }
        $families = [];
        $failures = [];
        foreach (self::ROLES as $role) {
            $name = $fonts[$role] ?? null;
            if (!is_string($name) || preg_match(self::RE_FAMILY, $name) !== 1) {
                $failures[] = ['path' => self::PATH, 'key' => $role, 'reason' => self::REASON_PATTERN];
                continue;
            }
            if ($name !== self::SYSTEM) {
                $families[$role] = $name;
            }
        }
        if ($failures !== []) {
            return Refusal::make('bad_input', 'a design font is not a family name.', $failures);
        }
        if ($families === []) {
            return null;
        }

        $registered = $registry();
        if (!is_array($registered)) {
            return Refusal::make(AdapterStatus::CODE, self::UNAVAILABLE, []);
        }
        $known = [];
        foreach ($registered as $name) {
            if (is_string($name)) {
                $known[$name] = true;
            }
        }
        foreach ($families as $role => $name) {
            if (isset($known[$name])) {
                continue;
            }
            $failure = ['path' => self::PATH, 'key' => $role, 'reason' => self::REASON_UNKNOWN];
            $close   = self::closeFamilies($name, $registered);
            if ($close !== []) {
                $failure['allowed'] = $close;
            }
            $failures[] = $failure;
        }

        return $failures === [] ? null : Refusal::make(self::CODE_UNKNOWN, 'a design font is not in the font registry of Elementor on this site.', $failures);
    }

    /**
     * Up to MAX_CLOSE registry families close to $family, closest first and
     * then by name.
     *
     * Close means, ignoring case, at most max(2, a third of the length of
     * $family) single-character edits away, or one name inside the other
     * when the shorter has at least MIN_CONTAINED characters. Only names the
     * closed pattern admits and that fit Refusal::MAX_ALLOWED_CHARS are
     * offered, each once.
     *
     * @param string       $family   A family name the registry lacks.
     * @param array<mixed> $registry The registry's family names.
     * @return list<string>
     */
    public static function closeFamilies(string $family, array $registry): array
    {
        $want  = strtolower($family);
        $limit = max(2, intdiv(strlen($want), 3));
        $close = [];
        foreach ($registry as $name) {
            if (!is_string($name) || isset($close[$name]) || strlen($name) > Refusal::MAX_ALLOWED_CHARS || preg_match(self::RE_FAMILY, $name) !== 1) {
                continue;
            }
            $have     = strtolower($name);
            $distance = levenshtein($want, $have);
            if ($distance > $limit && !self::inside($want, $have)) {
                continue;
            }
            $close[$name] = $distance;
        }
        $names = array_keys($close);
        usort(
            $names,
            static fn ($a, $b): int => [$close[$a], (string) $a] <=> [$close[$b], (string) $b]
        );

        return array_map('strval', array_slice($names, 0, self::MAX_CLOSE));
    }

    /**
     * Whether one lowercase name holds the other, the shorter having at
     * least MIN_CONTAINED characters.
     *
     * @param string $a A lowercase name.
     * @param string $b A lowercase name.
     * @return bool
     */
    private static function inside(string $a, string $b): bool
    {
        return min(strlen($a), strlen($b)) >= self::MIN_CONTAINED && (str_contains($a, $b) || str_contains($b, $a));
    }
}
