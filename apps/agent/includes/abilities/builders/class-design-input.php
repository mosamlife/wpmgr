<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The `design` object of an outline version 3 input, judged where the agent
 * reads the input: the agent builds from this object, so it does not rely on
 * the control plane having judged it.
 *
 *   design  {colors, fonts, scale, radius, density}, all five present
 *   colors  ground, surface, ink, muted, accent, on_accent: upper-case #RRGGBB,
 *           never #000000
 *   fonts   heading, body: a family name of letters, digits and spaces, or system
 *   scale   {base_px, ratio}: a whole number from 15 to 20, and a ratio by name
 *   radius  and density: one of the names the schema lists
 *
 * A field left out is design_not_ready; a field that is there with a value it
 * may not hold, or a key the design does not have, is bad_input; ink on
 * ground, ink on surface and on_accent on accent below MIN_CONTRAST are
 * design_contrast_low. Every failure is collected, in the order above, and the
 * refusal's code is the first failure's. Nothing is rewritten, and a refusal
 * holds WPMgr's own words and the offending keys' names, never text from the
 * input.
 *
 * Pure: no WordPress call.
 */
final class DesignInput
{
    /** Refusal: a part of the design is left out. */
    public const CODE_NOT_READY = 'design_not_ready';

    /** Refusal: a text colour is too close to the colour it sits on. */
    public const CODE_CONTRAST_LOW = 'design_contrast_low';

    /** Refusal: a value the design may not hold. */
    public const CODE_BAD = 'bad_input';

    /** The least WCAG contrast ratio of a pair that carries text. */
    public const MIN_CONTRAST = 4.5;

    /** The pairs a text colour sits on, in report order: the text role, then the role it is set on. */
    private const CONTRAST_PAIRS = [['ink', 'ground'], ['ink', 'surface'], ['on_accent', 'accent']];

    /** The members of the design and of its two nested objects. */
    private const TOP_KEYS   = ['colors', 'fonts', 'scale', 'radius', 'density'];
    private const FONT_KEYS  = ['heading', 'body'];
    private const SCALE_KEYS = ['base_px', 'ratio'];

    /** A design colour and a font family, as the input schema states them. */
    private const COLOUR_REGEX = '/' . OutlineV3Schema::COLOUR_PATTERN . '/D';
    private const FONT_REGEX   = '/' . OutlineV3Schema::FONT_PATTERN . '/D';

    /**
     * Judge a design.
     *
     * @param mixed $design The decoded `design`: an object as json_decode() gives with objects; null when the key is absent.
     * @return array{design?:array<string,mixed>,code?:string,detail?:string,failures?:list<array<string,mixed>>}
     *         The design as arrays, in the schema's order, or a refusal from Refusal::make().
     */
    public static function check($design): array
    {
        if ($design === null) {
            return self::refusal([self::failure(self::CODE_NOT_READY, 'input', 'design', 'missing; send the whole design: colors, fonts, scale, radius and density', self::TOP_KEYS)]);
        }
        $top = self::members($design);
        if ($top === null) {
            return self::refusal([self::failure(self::CODE_BAD, 'input', 'design', 'must be an object holding colors, fonts, scale, radius and density', self::TOP_KEYS)]);
        }

        $failures = [];
        $colors   = self::group($top, 'colors', OutlineV3Schema::COLOUR_ROLES, $failures);
        $colours  = [];
        foreach (OutlineV3Schema::COLOUR_ROLES as $role) {
            if ($colors === null || !array_key_exists($role, $colors)) {
                if ($colors !== null) {
                    $failures[] = self::failure(self::CODE_NOT_READY, 'design.colors', $role, 'missing; set it to an upper-case #RRGGBB colour');
                }
                continue;
            }
            $value = $colors[$role];
            if (!is_string($value) || preg_match(self::COLOUR_REGEX, $value) !== 1 || $value === '#000000') {
                $failures[] = self::failure(self::CODE_BAD, 'design.colors', $role, 'must be an upper-case #RRGGBB colour other than #000000');
                continue;
            }
            $colours[$role] = $value;
        }
        $failures = array_merge($failures, self::unknown($colors, 'design.colors', OutlineV3Schema::COLOUR_ROLES));

        $fonts    = self::group($top, 'fonts', self::FONT_KEYS, $failures);
        $families = [];
        foreach (self::FONT_KEYS as $key) {
            if ($fonts === null) {
                break;
            }
            if (!array_key_exists($key, $fonts)) {
                $failures[] = self::failure(self::CODE_NOT_READY, 'design.fonts', $key, 'missing; set it to a font family name, or system to set no family');
                continue;
            }
            $value = $fonts[$key];
            if (!is_string($value) || preg_match(self::FONT_REGEX, $value) !== 1) {
                $failures[] = self::failure(self::CODE_BAD, 'design.fonts', $key, 'must be a font family name of 1 to 60 letters, digits and spaces, or system to set no family');
                continue;
            }
            $families[$key] = $value;
        }
        $failures = array_merge($failures, self::unknown($fonts, 'design.fonts', self::FONT_KEYS));

        $scale = self::group($top, 'scale', self::SCALE_KEYS, $failures);
        $size  = null;
        $ratio = null;
        if ($scale !== null) {
            if (!array_key_exists('base_px', $scale)) {
                $failures[] = self::failure(self::CODE_NOT_READY, 'design.scale', 'base_px', 'missing; set it to a whole number from ' . OutlineV3Schema::SCALE_BASE_PX[0] . ' to ' . OutlineV3Schema::SCALE_BASE_PX[1]);
            } elseif (!is_int($scale['base_px']) || $scale['base_px'] < OutlineV3Schema::SCALE_BASE_PX[0] || $scale['base_px'] > OutlineV3Schema::SCALE_BASE_PX[1]) {
                $failures[] = self::failure(self::CODE_BAD, 'design.scale', 'base_px', 'must be a whole number from ' . OutlineV3Schema::SCALE_BASE_PX[0] . ' to ' . OutlineV3Schema::SCALE_BASE_PX[1]);
            } else {
                $size = $scale['base_px'];
            }
            $ratios = array_keys(DesignTokens::RATIOS);
            if (!array_key_exists('ratio', $scale)) {
                $failures[] = self::failure(self::CODE_NOT_READY, 'design.scale', 'ratio', 'missing; set it to the name of a ratio', $ratios);
            } elseif (!is_string($scale['ratio']) || !in_array($scale['ratio'], $ratios, true)) {
                $failures[] = self::failure(self::CODE_BAD, 'design.scale', 'ratio', 'must be the name of a ratio, never a number', $ratios);
            } else {
                $ratio = $scale['ratio'];
            }
        }
        $failures = array_merge($failures, self::unknown($scale, 'design.scale', self::SCALE_KEYS));

        $radius  = self::choice($top, 'radius', OutlineV3Schema::RADII, $failures);
        $density = self::choice($top, 'density', OutlineV3Schema::DENSITIES, $failures);
        $failures = array_merge($failures, self::unknown($top, 'design', self::TOP_KEYS));

        if ($failures === []) {
            $failures = self::contrast($colours);
        }
        if ($failures !== []) {
            return self::refusal($failures);
        }

        return ['design' => [
            'colors'  => $colours,
            'fonts'   => $families,
            'scale'   => ['base_px' => $size, 'ratio' => $ratio],
            'radius'  => $radius,
            'density' => $density,
        ]];
    }

    /**
     * The WCAG contrast ratio of two #RRGGBB colours, 1 to 21, the same
     * whichever is passed first.
     *
     * @param string $a A colour.
     * @param string $b Another.
     * @return float
     */
    public static function contrastRatio(string $a, string $b): float
    {
        $la = self::luminance($a);
        $lb = self::luminance($b);
        if ($la < $lb) {
            [$la, $lb] = [$lb, $la];
        }

        return ($la + 0.05) / ($lb + 0.05);
    }

    /**
     * The contrast failures of the colours that passed, in report order.
     *
     * @param array<string,string> $colours The design's colours by role.
     * @return list<array<string,mixed>>
     */
    private static function contrast(array $colours): array
    {
        $failures = [];
        foreach (self::CONTRAST_PAIRS as [$text, $on]) {
            if (!isset($colours[$text], $colours[$on]) || self::contrastRatio($colours[$text], $colours[$on]) >= self::MIN_CONTRAST) {
                continue;
            }
            $failures[] = self::failure(self::CODE_CONTRAST_LOW, 'design.colors', $text, $text . ' on ' . $on . ' has a contrast below ' . self::MIN_CONTRAST . ':1; change ' . $text . ' or ' . $on);
        }

        return $failures;
    }

    /**
     * The WCAG relative luminance of a #RRGGBB colour.
     *
     * @param string $hex A colour that passed COLOUR_REGEX.
     * @return float
     */
    private static function luminance(string $hex): float
    {
        $linear = [];
        foreach ([1, 3, 5] as $at) {
            $channel  = hexdec(substr($hex, $at, 2)) / 255;
            $linear[] = $channel <= 0.04045 ? $channel / 12.92 : (($channel + 0.055) / 1.055) ** 2.4;
        }

        return 0.2126 * $linear[0] + 0.7152 * $linear[1] + 0.0722 * $linear[2];
    }

    /**
     * One of the design's nested objects, or null when it is left out or is
     * not an object (the failure is recorded).
     *
     * @param array<string,mixed>            $top      The design's members.
     * @param string                         $key      colors, fonts or scale.
     * @param list<string>                   $allowed  The members the object has.
     * @param list<array<string,mixed>>      $failures The failures so far.
     * @return array<string,mixed>|null
     */
    private static function group(array $top, string $key, array $allowed, array &$failures): ?array
    {
        if (!array_key_exists($key, $top)) {
            $failures[] = self::failure(self::CODE_NOT_READY, 'design', $key, 'missing; a design needs ' . implode(', ', $allowed), $allowed);

            return null;
        }
        $members = self::members($top[$key]);
        if ($members === null) {
            $failures[] = self::failure(self::CODE_BAD, 'design', $key, 'must be an object holding ' . implode(', ', $allowed), $allowed);
        }

        return $members;
    }

    /**
     * A member of the design that is one of a closed set of names.
     *
     * @param array<string,mixed>       $top      The design's members.
     * @param string                    $key      radius or density.
     * @param list<string>              $allowed  The names.
     * @param list<array<string,mixed>> $failures The failures so far.
     * @return string|null
     */
    private static function choice(array $top, string $key, array $allowed, array &$failures): ?string
    {
        if (!array_key_exists($key, $top)) {
            $failures[] = self::failure(self::CODE_NOT_READY, 'design', $key, 'missing; set it to one of the names allowed', $allowed);

            return null;
        }
        if (!is_string($top[$key]) || !in_array($top[$key], $allowed, true)) {
            $failures[] = self::failure(self::CODE_BAD, 'design', $key, 'must be one of the names allowed', $allowed);

            return null;
        }

        return $top[$key];
    }

    /**
     * The members of an object, or null for anything that is not one.
     *
     * @param mixed $value A decoded value.
     * @return array<string,mixed>|null
     */
    private static function members($value): ?array
    {
        if (!is_object($value)) {
            return null;
        }

        return get_object_vars($value);
    }

    /**
     * The members an object has that the design does not, one failure each.
     *
     * @param array<string,mixed>|null $members The object's members; null when it was not read.
     * @param string                   $path    Where the object is.
     * @param list<string>             $allowed The members it has.
     * @return list<array<string,mixed>>
     */
    private static function unknown(?array $members, string $path, array $allowed): array
    {
        $failures = [];
        foreach (array_keys($members ?? []) as $key) {
            if (!in_array((string) $key, $allowed, true)) {
                $failures[] = self::failure(self::CODE_BAD, $path, 'unknown_key', 'a key the design does not have; the members are ' . implode(', ', $allowed), $allowed);
            }
        }

        return $failures;
    }

    /**
     * @param string            $code    A refusal code.
     * @param string            $path    Where the failure is.
     * @param string            $key     The member.
     * @param string            $reason  WPMgr's words.
     * @param list<string>|null $allowed The values or members allowed.
     * @return array<string,mixed> A failure, with the code that decides the refusal's.
     */
    private static function failure(string $code, string $path, string $key, string $reason, ?array $allowed = null): array
    {
        $failure = ['code' => $code, 'path' => $path, 'key' => $key, 'reason' => $reason];
        if ($allowed !== null) {
            $failure['allowed'] = $allowed;
        }

        return $failure;
    }

    /**
     * The refusal of these failures, coded as the first.
     *
     * @param list<array<string,mixed>> $failures One or more failures from failure().
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}
     */
    private static function refusal(array $failures): array
    {
        $code   = (string) $failures[0]['code'];
        $listed = [];
        foreach ($failures as $failure) {
            unset($failure['code']);
            $listed[] = $failure;
        }

        return Refusal::make($code, 'The design is not one a page can be built from.', $listed);
    }
}
