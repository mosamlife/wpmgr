<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The control plane's version ordering, reproduced exactly.
 *
 * A catalogue entry's version range is checked here against the live owner
 * version, and the control plane checks the same range with its own compare.
 * The two must agree on every input, so this is a port of that algorithm and
 * deliberately not PHP's version_compare(), which orders some inputs
 * differently. A shared case file pins the agreement in both test suites.
 *
 * Rules: "_", "-" and "+" are separators like "."; each part splits again at
 * digit/non-digit boundaries; an empty part is a marker ranked between
 * release candidates and releases; a missing trailing part counts as "0".
 * Numeric parts compare as integers of any length. Other parts rank
 * dev < alpha = a < beta = b < rc < (marker) < anything unrecognised
 * (a release) < pl = p. A "*" compares as the empty string.
 */
final class VersionCompare
{
    /** Rank of each recognised non-numeric part; anything else ranks 5. */
    private const RANK = [
        'dev'   => 0,
        'alpha' => 1,
        'a'     => 1,
        'beta'  => 2,
        'b'     => 2,
        'rc'    => 3,
        '#'     => 4,
        'pl'    => 6,
        'p'     => 6,
    ];

    private const RELEASE_RANK = 5;

    /**
     * Compare two version strings.
     *
     * @param string $a Left.
     * @param string $b Right.
     * @return int -1, 0 or 1.
     */
    public static function compare(string $a, string $b): int
    {
        if ($a === '*') {
            $a = '';
        }
        if ($b === '*') {
            $b = '';
        }
        $ta  = self::tokenize($a);
        $tb  = self::tokenize($b);
        $max = max(count($ta), count($tb));
        for ($i = 0; $i < $max; $i++) {
            $c = self::compareToken($ta[$i] ?? '0', $tb[$i] ?? '0');
            if ($c !== 0) {
                return $c;
            }
        }

        return 0;
    }

    /**
     * Is $version within [$min, $max], both bounds inclusive?
     *
     * @param string $version Live version.
     * @param string $min     Lowest admitted version.
     * @param string $max     Highest tested version.
     * @return bool
     */
    public static function inRange(string $version, string $min, string $max): bool
    {
        return self::compare($version, $min) >= 0 && self::compare($version, $max) <= 0;
    }

    /**
     * @param string $v Version.
     * @return list<string>
     */
    private static function tokenize(string $v): array
    {
        if ($v === '') {
            return ['#'];
        }
        $v      = strtr($v, ['_' => '.', '-' => '.', '+' => '.']);
        $tokens = [];
        foreach (explode('.', $v) as $part) {
            if ($part === '') {
                $tokens[] = '#';
                continue;
            }
            foreach (self::splitBoundary($part) as $t) {
                $tokens[] = $t;
            }
        }

        return $tokens;
    }

    /**
     * Split at each change between an ASCII digit byte and any other byte.
     *
     * @param string $s Non-empty part.
     * @return list<string>
     */
    private static function splitBoundary(string $s): array
    {
        $parts   = [];
        $start   = 0;
        $len     = strlen($s);
        $isDigit = self::digitByte($s[0]);
        for ($i = 1; $i < $len; $i++) {
            $d = self::digitByte($s[$i]);
            if ($d !== $isDigit) {
                $parts[] = substr($s, $start, $i - $start);
                $start   = $i;
                $isDigit = $d;
            }
        }
        $parts[] = substr($s, $start);

        return $parts;
    }

    /**
     * @param string $byte One byte.
     * @return bool
     */
    private static function digitByte(string $byte): bool
    {
        $o = ord($byte);

        return $o >= 48 && $o <= 57;
    }

    /**
     * @param string $a Left token.
     * @param string $b Right token.
     * @return int
     */
    private static function compareToken(string $a, string $b): int
    {
        $aNum = self::isNumeric($a);
        $bNum = self::isNumeric($b);
        if ($aNum && $bNum) {
            return self::compareNumeric($a, $b);
        }
        $aRank = $aNum ? self::RELEASE_RANK : self::rankOf($a);
        $bRank = $bNum ? self::RELEASE_RANK : self::rankOf($b);

        return $aRank <=> $bRank;
    }

    /**
     * @param string $t Token.
     * @return int
     */
    private static function rankOf(string $t): int
    {
        return self::RANK[strtolower($t)] ?? self::RELEASE_RANK;
    }

    /**
     * Every character a decimal digit (any script), over valid UTF-8 only.
     *
     * @param string $s Token.
     * @return bool
     */
    private static function isNumeric(string $s): bool
    {
        return $s !== '' && preg_match('/^\p{Nd}+$/u', $s) === 1;
    }

    /**
     * Integer comparison of digit strings of any length.
     *
     * @param string $a Left.
     * @param string $b Right.
     * @return int
     */
    private static function compareNumeric(string $a, string $b): int
    {
        $a = ltrim($a, '0');
        $b = ltrim($b, '0');
        if (strlen($a) !== strlen($b)) {
            return strlen($a) <=> strlen($b);
        }

        // Byte order, never PHP's numeric-string comparison (floats lose
        // precision past 2^53).
        return strcmp($a, $b) <=> 0;
    }
}
