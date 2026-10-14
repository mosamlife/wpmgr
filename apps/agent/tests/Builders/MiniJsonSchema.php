<?php
/**
 * A small JSON-schema checker for the schemas the agent publishes.
 *
 * It knows exactly the keywords those schemas use. A keyword it does not
 * know is an error, never a silent pass, so a schema that grows a keyword
 * cannot be waved through by a checker that ignores it. Values are decoded
 * with json_decode(..., false): a JSON object is an object, a JSON array is
 * an array.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

final class MiniJsonSchema
{
    private const KEYWORDS = [
        'type', 'properties', 'required', 'additionalProperties', 'items', 'oneOf', 'anyOf',
        'description', 'not', 'uniqueItems', 'enum', 'const', 'minimum', 'maximum', 'minLength', 'maxLength', 'minItems', 'maxItems', 'pattern',
    ];

    /**
     * What is wrong with $value under $schema, as dotted paths. Empty when it fits.
     *
     * @param mixed               $value  Decoded value.
     * @param array<string,mixed> $schema Schema.
     * @return list<string>
     */
    public static function errors($value, array $schema, string $path = '$'): array
    {
        foreach (array_keys($schema) as $keyword) {
            if (!in_array($keyword, self::KEYWORDS, true)) {
                return [$path . ': the checker does not know the keyword ' . $keyword];
            }
        }
        $out = [];
        if (isset($schema['not']) && self::errors($value, $schema['not'], $path) === []) {
            $out[] = $path . ': fits the forbidden schema';
        }
        if (isset($schema['type']) && !self::typeFits($value, (string) $schema['type'])) {
            return [$path . ': not a ' . $schema['type']];
        }
        if (array_key_exists('const', $schema) && $value !== $schema['const']) {
            $out[] = $path . ': not the constant';
        }
        if (isset($schema['enum']) && !in_array($value, $schema['enum'], true)) {
            $out[] = $path . ': not in the enum';
        }
        if (is_string($value)) {
            $len = mb_strlen($value, 'UTF-8');
            if (isset($schema['minLength']) && $len < $schema['minLength']) {
                $out[] = $path . ': too short';
            }
            if (isset($schema['maxLength']) && $len > $schema['maxLength']) {
                $out[] = $path . ': too long';
            }
            if (isset($schema['pattern']) && preg_match('~' . str_replace('~', '\~', (string) $schema['pattern']) . '~u', $value) !== 1) {
                $out[] = $path . ': does not match the pattern';
            }
        }
        if ((is_int($value) || is_float($value)) && !is_bool($value)) {
            if (isset($schema['minimum']) && $value < $schema['minimum']) {
                $out[] = $path . ': below the minimum';
            }
            if (isset($schema['maximum']) && $value > $schema['maximum']) {
                $out[] = $path . ': above the maximum';
            }
        }
        if (is_array($value)) {
            if (($schema['uniqueItems'] ?? false) === true) {
                $seen = [];
                foreach ($value as $item) {
                    if (in_array($item, $seen, true)) {
                        $out[] = $path . ': repeated item';
                        break;
                    }
                    $seen[] = $item;
                }
            }
            if (isset($schema['minItems']) && count($value) < $schema['minItems']) {
                $out[] = $path . ': too few items';
            }
            if (isset($schema['maxItems']) && count($value) > $schema['maxItems']) {
                $out[] = $path . ': too many items';
            }
            if (isset($schema['items'])) {
                foreach ($value as $i => $item) {
                    $out = array_merge($out, self::errors($item, $schema['items'], $path . '[' . $i . ']'));
                }
            }
        }
        if (is_object($value)) {
            $vars = get_object_vars($value);
            foreach ($schema['required'] ?? [] as $key) {
                if (!array_key_exists($key, $vars)) {
                    $out[] = $path . ': missing ' . $key;
                }
            }
            $props = $schema['properties'] ?? [];
            foreach ($vars as $key => $child) {
                if (isset($props[$key])) {
                    $out = array_merge($out, self::errors($child, $props[$key], $path . '.' . $key));
                } elseif (($schema['additionalProperties'] ?? true) === false) {
                    $out[] = $path . ': unknown field ' . $key;
                }
            }
        }
        if (isset($schema['anyOf'])) {
            $fits = 0;
            foreach ($schema['anyOf'] as $branch) {
                if (self::errors($value, $branch, $path) === []) {
                    ++$fits;
                }
            }
            if ($fits < 1) {
                $out[] = $path . ': fits none of anyOf';
            }
        }
        if (isset($schema['oneOf'])) {
            $fits = 0;
            foreach ($schema['oneOf'] as $branch) {
                if (self::errors($value, $branch, $path) === []) {
                    ++$fits;
                }
            }
            if ($fits !== 1) {
                $out[] = $path . ': fits ' . $fits . ' of oneOf, not exactly one';
            }
        }

        return $out;
    }

    /** @param mixed $value Decoded value. */
    private static function typeFits($value, string $type): bool
    {
        switch ($type) {
            case 'object':
                return is_object($value);
            case 'array':
                return is_array($value);
            case 'string':
                return is_string($value);
            case 'boolean':
                return is_bool($value);
            case 'integer':
                return is_int($value);
            case 'number':
                return is_int($value) || is_float($value);
            default:
                return false;
        }
    }
}
