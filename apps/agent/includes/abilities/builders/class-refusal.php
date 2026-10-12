<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The refusal envelope: a refusal that teaches the fix.
 *
 * Every refusal carries
 *   code      the refusal code, unchanged from the code the check answers;
 *   detail    one line of text that starts with the summary and inlines the
 *             first three concrete failures, so a client that shows only the
 *             message still gets the fix;
 *   failures  up to MAX_FAILURES of {ref?, path?, key, reason, allowed?};
 *   pattern   and item_schema, for a refusal about a pattern that holds items:
 *             the schema of one item, as the input schema publishes it.
 *
 * A failure names where it is by a node `ref` (an edit) or an input `path`
 * (a create), and the offending `key`; the dotted address of the field is the
 * path and the key together. `reason` is WPMgr's own words for the fix.
 * `allowed` lists the accepted values when the set is closed. Neither holds
 * text that came from the page or the input.
 *
 * Every string is one line and bounded, so a refusal cannot grow with its
 * input. A malformed failure is a programming error and throws.
 *
 * Pure: no WordPress call.
 */
final class Refusal
{
    /** Failures spelled out in the message. */
    public const INLINE_FAILURES = 3;

    /** Failures kept in failures[]. */
    public const MAX_FAILURES = 20;

    /** Allowed values kept per failure, and spelled out in the message. */
    public const MAX_ALLOWED    = 12;
    public const INLINE_ALLOWED = 6;

    /** Characters of the message and of each field. */
    public const MAX_DETAIL_CHARS  = 1200;
    public const MAX_REASON_CHARS  = 200;
    public const MAX_PATH_CHARS    = 120;
    public const MAX_KEY_CHARS     = 60;
    public const MAX_REF_CHARS     = 32;
    public const MAX_ALLOWED_CHARS = 40;

    /** A refusal code, as the checks spell it. */
    private const RE_CODE = '/^[a-z][a-z0-9_]{2,63}$/D';

    /** The fields of a failure, in the order they are written. */
    private const FAILURE_KEYS = ['ref', 'path', 'key', 'reason', 'allowed'];

    /**
     * A refusal.
     *
     * @param string                      $code     Refusal code.
     * @param string                      $summary  What was refused, in a sentence.
     * @param list<array<string,mixed>>   $failures {ref?, path?, key, reason, allowed?}, most useful first.
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}
     * @throws \InvalidArgumentException When the code or a failure is malformed.
     */
    public static function make(string $code, string $summary, array $failures): array
    {
        if (preg_match(self::RE_CODE, $code) !== 1) {
            throw new \InvalidArgumentException('a refusal code is 3 to 64 characters of a-z, 0-9 and _, and starts with a letter');
        }
        $listed = [];
        foreach ($failures as $failure) {
            if (count($listed) === self::MAX_FAILURES) {
                break;
            }
            $listed[] = self::failure($failure);
        }

        return [
            'code'     => $code,
            'detail'   => self::message($summary, $listed, count($failures) > count($listed)),
            'failures' => $listed,
        ];
    }

    /**
     * A refusal about a pattern: it also carries the schema of one of the
     * pattern's items, when the pattern holds items.
     *
     * @param string                    $code     Refusal code.
     * @param string                    $summary  What was refused, in a sentence.
     * @param string                    $pattern  Pattern name.
     * @param list<array<string,mixed>> $failures Failures, as make() takes them.
     * @return array<string,mixed>
     */
    public static function forPattern(string $code, string $summary, string $pattern, array $failures): array
    {
        $refusal = self::make($code, $summary, $failures);
        $item    = OutlineV3Schema::itemSchema($pattern);
        if ($item !== null) {
            $refusal['pattern']     = $pattern;
            $refusal['item_schema'] = $item;
        }

        return $refusal;
    }

    /**
     * The refusal in the command's shape: the fields every refusal has, then
     * the envelope's.
     *
     * @param array<string,mixed> $refusal A refusal from make() or forPattern().
     * @return array<string,mixed>
     */
    public static function command(array $refusal): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'refused',
            'code'      => $refusal['code'],
            'detail'    => $refusal['detail'],
            'retryable' => false,
        ] + array_diff_key($refusal, ['code' => 1, 'detail' => 1]);
    }

    // ---------------------------------------------------------------------
    // Failures
    // ---------------------------------------------------------------------

    /**
     * @param mixed $failure One failure as given.
     * @return array<string,mixed>
     * @throws \InvalidArgumentException When it is not {ref?, path?, key, reason, allowed?}.
     */
    private static function failure($failure): array
    {
        if (!is_array($failure)) {
            throw new \InvalidArgumentException('a failure is a map of ref or path, key, reason and allowed');
        }
        $unknown = array_diff(array_keys($failure), self::FAILURE_KEYS);
        if ($unknown !== []) {
            throw new \InvalidArgumentException('a failure has no field ' . implode(', ', array_map('strval', $unknown)));
        }
        $out = [];
        foreach (['ref' => self::MAX_REF_CHARS, 'path' => self::MAX_PATH_CHARS, 'key' => self::MAX_KEY_CHARS, 'reason' => self::MAX_REASON_CHARS] as $name => $max) {
            if (!array_key_exists($name, $failure)) {
                continue;
            }
            if (!is_string($failure[$name])) {
                throw new \InvalidArgumentException('failure ' . $name . ' must be a string');
            }
            $text = self::line($failure[$name], $max);
            if ($text !== '') {
                $out[$name] = $text;
            }
        }
        if (!isset($out['ref']) && !isset($out['path'])) {
            throw new \InvalidArgumentException('a failure names a ref or a path');
        }
        foreach (['key', 'reason'] as $name) {
            if (!isset($out[$name])) {
                throw new \InvalidArgumentException('a failure needs a ' . $name);
            }
        }
        if (array_key_exists('allowed', $failure)) {
            $allowed = self::allowed($failure['allowed']);
            if ($allowed !== []) {
                $out['allowed'] = $allowed;
            }
        }

        return self::ordered($out);
    }

    /**
     * @param mixed $allowed The allowed values as given.
     * @return list<string>
     * @throws \InvalidArgumentException When it is not a list of strings.
     */
    private static function allowed($allowed): array
    {
        if (!is_array($allowed) || array_values($allowed) !== $allowed) {
            throw new \InvalidArgumentException('failure allowed must be a list of strings');
        }
        $out = [];
        foreach ($allowed as $value) {
            if (!is_string($value)) {
                throw new \InvalidArgumentException('failure allowed must be a list of strings');
            }
            $text = self::line($value, self::MAX_ALLOWED_CHARS);
            if ($text !== '') {
                $out[] = $text;
            }
            if (count($out) === self::MAX_ALLOWED) {
                break;
            }
        }

        return $out;
    }

    /**
     * A failure with its fields in the written order.
     *
     * @param array<string,mixed> $failure Failure.
     * @return array<string,mixed>
     */
    private static function ordered(array $failure): array
    {
        $out = [];
        foreach (self::FAILURE_KEYS as $name) {
            if (isset($failure[$name])) {
                $out[$name] = $failure[$name];
            }
        }

        return $out;
    }

    // ---------------------------------------------------------------------
    // Message
    // ---------------------------------------------------------------------

    /**
     * @param string                    $summary Summary.
     * @param list<array<string,mixed>> $listed  The failures kept.
     * @param bool                      $cut     Whether more failures were found than kept.
     */
    private static function message(string $summary, array $listed, bool $cut): string
    {
        $text  = self::line($summary, self::MAX_DETAIL_CHARS);
        $parts = [];
        foreach (array_slice($listed, 0, self::INLINE_FAILURES) as $i => $f) {
            $part = ($i + 1) . '. ' . self::where($f) . ': ' . $f['reason'];
            if (isset($f['allowed'])) {
                $shown = array_slice($f['allowed'], 0, self::INLINE_ALLOWED);
                $more  = count($f['allowed']) > count($shown) ? ', ...' : '';
                $part .= ' (allowed: ' . implode(', ', $shown) . $more . ')';
            }
            $parts[] = $part;
        }
        if ($parts !== []) {
            $text = ($text === '' ? '' : $text . ' ') . implode('; ', $parts);
        }
        $rest = count($listed) - count($parts);
        if ($rest > 0) {
            $text .= ' (' . $rest . ($cut ? '+' : '') . ' more in failures)';
        }

        return self::line($text, self::MAX_DETAIL_CHARS);
    }

    /**
     * Where a failure is: a node ref, or the dotted address of the field.
     *
     * @param array<string,mixed> $f Failure.
     */
    private static function where(array $f): string
    {
        $path = $f['path'] ?? '';
        $key  = $f['key'];
        if (isset($f['ref'])) {
            return 'ref ' . $f['ref'] . ($path === '' ? '.' . $key : ' ' . $path . '.' . $key);
        }

        return $path . '.' . $key;
    }

    // ---------------------------------------------------------------------
    // Text
    // ---------------------------------------------------------------------

    /**
     * One line of at most $max characters: control characters and runs of
     * white space become one space, and a longer text ends with "...".
     *
     * @param string $text Text.
     * @param int    $max  Most characters.
     */
    private static function line(string $text, int $max): string
    {
        if (preg_match('//u', $text) !== 1) {
            $text = (string) preg_replace('/[^\x20-\x7E]/', '?', $text);
        }
        $text = trim((string) preg_replace('/[\x00-\x20\x7F]+/u', ' ', $text));
        if (self::chars($text) <= $max) {
            return $text;
        }

        return rtrim(self::head($text, $max - 3)) . '...';
    }

    private static function chars(string $text): int
    {
        return function_exists('mb_strlen') ? mb_strlen($text, 'UTF-8') : (int) preg_match_all('/./us', $text);
    }

    private static function head(string $text, int $n): string
    {
        if (function_exists('mb_substr')) {
            return mb_substr($text, 0, $n, 'UTF-8');
        }

        return preg_match('/^.{0,' . $n . '}/us', $text, $m) === 1 ? $m[0] : '';
    }
}
