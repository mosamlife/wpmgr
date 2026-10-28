<?php
/**
 * QrEncoder: pure-PHP QR code encoder producing inline SVG output.
 *
 * Scope: the otpauth:// URI shown on the TOTP enrollment screen.
 *   - Byte mode, error correction level M, version 1-40 (smallest that fits).
 *   - Output: inline SVG, one path of dark modules on a white background,
 *     with the 4-module quiet zone the standard requires.
 *
 * Implements ISO/IEC 18004 (QR Code): Reed-Solomon error correction over
 * GF(256), block interleaving, function patterns, format and version
 * information, data placement and masking with the standard penalty rules.
 * No Composer dependency: this file is the sole dependency for TOTP setup.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Pure-PHP QR code encoder, inline SVG output.
 */
final class QrEncoder
{
    /** Highest symbol version (177 x 177 modules). */
    private const MAX_VERSION = 40;

    /** Format information error correction level indicator for M (L=01, M=00, Q=11, H=10). */
    private const ECC_LEVEL_M = 0b00;

    /**
     * Error correction codewords per block, level M, indexed by version (ISO/IEC 18004 Table 9).
     *
     * @var array<int,int>
     */
    private const ECC_PER_BLOCK_M = [
        1 => 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26,
        26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28,
    ];

    /**
     * Number of error correction blocks, level M, indexed by version (ISO/IEC 18004 Table 9).
     *
     * @var array<int,int>
     */
    private const BLOCKS_M = [
        1 => 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14,
        16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49,
    ];

    /** Quiet zone width in modules. */
    private const QUIET_ZONE = 4;

    /** Smallest rendered module, in pixels; larger symbols grow instead of shrinking below it. */
    private const MIN_MODULE_PX = 4;

    /**
     * GF(256) antilog table: $gfExp[i] = alpha^i, for i in 0..254.
     *
     * @var array<int,int>
     */
    private static array $gfExp = [];

    /**
     * GF(256) log table: $gfLog[x] = i where alpha^i = x, for x in 1..255.
     * The zero element has no logarithm and no entry.
     *
     * @var array<int,int>
     */
    private static array $gfLog = [];

    // -------------------------------------------------------------------------
    // Public API
    // -------------------------------------------------------------------------

    /**
     * Encode $data and return an inline SVG string at the given pixel size.
     *
     * @param string $data   Data to encode (bytes, encoded in byte mode).
     * @param int    $pixels Requested width and height in pixels, raised to
     *                       MIN_MODULE_PX per module (quiet zone included) when
     *                       the symbol needs more.
     * @return string Inline SVG markup, or '' when $data exceeds the version 40-M capacity.
     */
    public static function toSvg(string $data, int $pixels = 256): string
    {
        $matrix = self::encode($data);
        if ($matrix === []) {
            return '';
        }
        return self::renderSvg($matrix, $pixels);
    }

    // -------------------------------------------------------------------------
    // Symbol assembly
    // -------------------------------------------------------------------------

    /**
     * Encode $data into the final module matrix (1 = dark), or [] when it does not fit.
     *
     * @param string $data
     * @return array<int, array<int, int>>
     */
    private static function encode(string $data): array
    {
        $version = self::chooseVersion(strlen($data));
        if ($version === 0) {
            return [];
        }

        $codewords = self::interleave(self::dataCodewords($data, $version), $version);

        [$modules, $reserved] = self::functionPatterns($version);
        self::placeData($modules, $reserved, $codewords);

        $best        = [];
        $bestPenalty = PHP_INT_MAX;
        for ($mask = 0; $mask < 8; $mask++) {
            $candidate = self::applyMask($modules, $reserved, $mask);
            self::drawFormatBits($candidate, self::formatBits(self::ECC_LEVEL_M, $mask));
            $penalty = self::penalty($candidate);
            if ($penalty < $bestPenalty) {
                $bestPenalty = $penalty;
                $best        = $candidate;
            }
        }

        return $best;
    }

    /**
     * Smallest version whose level-M capacity holds $byteLen bytes in byte mode, or 0.
     *
     * @param int $byteLen
     * @return int
     */
    private static function chooseVersion(int $byteLen): int
    {
        for ($version = 1; $version <= self::MAX_VERSION; $version++) {
            $bits = 4 + self::countBits($version) + 8 * $byteLen;
            if ($bits <= 8 * self::dataCodewordCount($version)) {
                return $version;
            }
        }
        return 0;
    }

    /**
     * Character count indicator length for byte mode.
     *
     * @param int $version
     * @return int
     */
    private static function countBits(int $version): int
    {
        return $version <= 9 ? 8 : 16;
    }

    /**
     * Number of modules available for codewords (data plus ECC, plus remainder bits).
     *
     * @param int $version
     * @return int
     */
    private static function rawDataModules(int $version): int
    {
        $size   = 17 + 4 * $version;
        $result = $size * $size;
        $result -= 3 * 64;                 // Finder patterns with their separators.
        $result -= 2 * ($size - 16);       // Timing patterns between the separators.
        $result -= 31;                     // Format information (2 x 15) and the dark module.
        if ($version >= 2) {
            $count   = intdiv($version, 7) + 2;
            $result -= 25 * ($count * $count - 3);     // Alignment patterns, minus the 3 under finders.
            $result += 10 * ($count - 2);              // Alignment modules already counted as timing.
        }
        if ($version >= 7) {
            $result -= 36;                 // Version information (2 x 18).
        }
        return $result;
    }

    /**
     * Total codewords (data plus ECC) in a symbol of this version.
     *
     * @param int $version
     * @return int
     */
    private static function totalCodewords(int $version): int
    {
        return intdiv(self::rawDataModules($version), 8);
    }

    /**
     * Level-M data codewords in a symbol of this version.
     *
     * @param int $version
     * @return int
     */
    private static function dataCodewordCount(int $version): int
    {
        return self::totalCodewords($version) - self::ECC_PER_BLOCK_M[$version] * self::BLOCKS_M[$version];
    }

    /**
     * Level-M block structure: one [data codewords, ECC codewords] pair per block, in order.
     *
     * @param int $version
     * @return list<array{int,int}>
     */
    private static function blockLayout(int $version): array
    {
        $blocks      = self::BLOCKS_M[$version];
        $ecc         = self::ECC_PER_BLOCK_M[$version];
        $total       = self::totalCodewords($version);
        $shortBlocks = $blocks - $total % $blocks;
        $shortData   = intdiv($total, $blocks) - $ecc;

        $layout = [];
        for ($b = 0; $b < $blocks; $b++) {
            $layout[] = [$b < $shortBlocks ? $shortData : $shortData + 1, $ecc];
        }
        return $layout;
    }

    // -------------------------------------------------------------------------
    // Data codewords
    // -------------------------------------------------------------------------

    /**
     * Byte-mode segment, terminator and padding, as data codewords.
     *
     * @param string $data
     * @param int    $version
     * @return list<int>
     */
    private static function dataCodewords(string $data, int $version): array
    {
        $capacityBits = 8 * self::dataCodewordCount($version);
        $byteLen      = strlen($data);

        /** @var list<int> $bits */
        $bits = [];
        self::appendBits($bits, 0b0100, 4);                       // Byte mode indicator.
        self::appendBits($bits, $byteLen, self::countBits($version));
        for ($i = 0; $i < $byteLen; $i++) {
            self::appendBits($bits, ord($data[$i]), 8);
        }

        // Terminator: up to four zero bits, never past capacity.
        self::appendBits($bits, 0, min(4, $capacityBits - count($bits)));
        // Zero bits to the next codeword boundary.
        self::appendBits($bits, 0, (8 - count($bits) % 8) % 8);

        $codewords = [];
        $bitCount  = count($bits);
        for ($i = 0; $i < $bitCount; $i += 8) {
            $byte = 0;
            for ($j = 0; $j < 8; $j++) {
                $byte = ($byte << 1) | $bits[$i + $j];
            }
            $codewords[] = $byte;
        }

        // Pad codewords 11101100 and 00010001, alternating, to capacity.
        $capacity = intdiv($capacityBits, 8);
        for ($pad = 0xEC; count($codewords) < $capacity; $pad ^= 0xEC ^ 0x11) {
            $codewords[] = $pad;
        }

        return $codewords;
    }

    /**
     * Append the low $len bits of $value, most significant first.
     *
     * @param list<int> $bits
     * @param int       $value
     * @param int       $len
     * @return void
     */
    private static function appendBits(array &$bits, int $value, int $len): void
    {
        for ($i = $len - 1; $i >= 0; $i--) {
            $bits[] = ($value >> $i) & 1;
        }
    }

    /**
     * Split data codewords into blocks, append each block's ECC, and interleave.
     *
     * @param list<int> $data
     * @param int       $version
     * @return list<int>
     */
    private static function interleave(array $data, int $version): array
    {
        $dataBlocks = [];
        $eccBlocks  = [];
        $offset     = 0;
        foreach (self::blockLayout($version) as [$dataLen, $eccLen]) {
            $block        = array_slice($data, $offset, $dataLen);
            $offset      += $dataLen;
            $dataBlocks[] = $block;
            $eccBlocks[]  = self::rsEncode($block, $eccLen);
        }

        $out     = [];
        $longest = max(array_map('count', $dataBlocks));
        for ($i = 0; $i < $longest; $i++) {
            foreach ($dataBlocks as $block) {
                if ($i < count($block)) {
                    $out[] = $block[$i];
                }
            }
        }
        $eccLen = self::ECC_PER_BLOCK_M[$version];
        for ($i = 0; $i < $eccLen; $i++) {
            foreach ($eccBlocks as $block) {
                $out[] = $block[$i];
            }
        }

        return $out;
    }

    // -------------------------------------------------------------------------
    // Reed-Solomon error correction over GF(256), primitive polynomial 0x11D
    // -------------------------------------------------------------------------

    /**
     * The $n error correction codewords for one block: the remainder of
     * data(x) * x^n divided by the generator polynomial of degree $n.
     *
     * @param list<int> $data
     * @param int       $n
     * @return list<int>
     */
    private static function rsEncode(array $data, int $n): array
    {
        $generator = self::rsGenerator($n);
        $remainder = array_fill(0, $n, 0);

        foreach ($data as $codeword) {
            $factor = $codeword ^ $remainder[0];
            array_shift($remainder);
            $remainder[] = 0;
            for ($j = 0; $j < $n; $j++) {
                $remainder[$j] ^= self::gfMul($generator[$j + 1], $factor);
            }
        }

        return array_values($remainder);
    }

    /**
     * Generator polynomial (x - alpha^0)(x - alpha^1)...(x - alpha^(n-1)),
     * as field elements, highest degree first (the leading coefficient is 1).
     *
     * @param int $n
     * @return list<int>
     */
    private static function rsGenerator(int $n): array
    {
        self::initGf();

        $poly = [1];
        for ($i = 0; $i < $n; $i++) {
            $root = self::$gfExp[$i];
            $next = array_fill(0, count($poly) + 1, 0);
            foreach ($poly as $j => $coeff) {
                $next[$j]     ^= $coeff;                      // x * poly
                $next[$j + 1] ^= self::gfMul($coeff, $root);  // alpha^i * poly
            }
            $poly = $next;
        }

        return array_values($poly);
    }

    /**
     * Multiply two GF(256) field elements.
     *
     * @param int $a
     * @param int $b
     * @return int
     */
    private static function gfMul(int $a, int $b): int
    {
        // Zero is the one element without a logarithm; its product is zero.
        if ($a === 0 || $b === 0) {
            return 0;
        }
        self::initGf();
        return self::$gfExp[(self::$gfLog[$a] + self::$gfLog[$b]) % 255];
    }

    /**
     * Build the GF(256) antilog and log tables once.
     *
     * @return void
     */
    private static function initGf(): void
    {
        if (self::$gfExp !== []) {
            return;
        }
        $x = 1;
        for ($i = 0; $i < 255; $i++) {
            self::$gfExp[$i] = $x;
            self::$gfLog[$x] = $i;
            $x <<= 1;
            if ($x > 0xFF) {
                $x ^= 0x11D; // x^8 + x^4 + x^3 + x^2 + 1.
            }
        }
    }

    // -------------------------------------------------------------------------
    // Format and version information (BCH codes)
    // -------------------------------------------------------------------------

    /**
     * 15-bit format information: level indicator and mask, BCH(15,5), XOR 101010000010010.
     *
     * @param int $eccLevel 2-bit level indicator (L=01, M=00, Q=11, H=10).
     * @param int $mask     Mask pattern 0-7.
     * @return int
     */
    private static function formatBits(int $eccLevel, int $mask): int
    {
        $data = (($eccLevel & 0b11) << 3) | ($mask & 0b111);
        $rem  = $data << 10;
        for ($i = 14; $i >= 10; $i--) {
            if ((($rem >> $i) & 1) === 1) {
                $rem ^= 0x537 << ($i - 10); // x^10 + x^8 + x^5 + x^4 + x^2 + x + 1.
            }
        }
        return (($data << 10) | $rem) ^ 0x5412;
    }

    /**
     * 18-bit version information: 6-bit version, BCH(18,6). Versions 7 and up only.
     *
     * @param int $version
     * @return int
     */
    private static function versionBits(int $version): int
    {
        $rem = $version << 12;
        for ($i = 17; $i >= 12; $i--) {
            if ((($rem >> $i) & 1) === 1) {
                $rem ^= 0x1F25 << ($i - 12); // x^12 + x^11 + x^10 + x^9 + x^8 + x^5 + x^2 + 1.
            }
        }
        return ($version << 12) | $rem;
    }

    /**
     * Module coordinates [row, col] of format information bit i (0 = least
     * significant), for the copy around the top-left finder and the copy
     * split between the top-right and bottom-left finders.
     *
     * @param int $size
     * @return array{list<array{int,int}>, list<array{int,int}>}
     */
    private static function formatPositions(int $size): array
    {
        $first  = [];
        $second = [];
        for ($i = 0; $i < 15; $i++) {
            if ($i <= 5) {
                $first[] = [$i, 8];
            } elseif ($i === 6) {
                $first[] = [7, 8];
            } elseif ($i === 7) {
                $first[] = [8, 8];
            } elseif ($i === 8) {
                $first[] = [8, 7];
            } else {
                $first[] = [8, 14 - $i];
            }
            $second[] = $i < 8 ? [8, $size - 1 - $i] : [$size - 15 + $i, 8];
        }
        return [$first, $second];
    }

    /**
     * Write both copies of the format information.
     *
     * @param array<int, array<int, int>> $modules
     * @param int             $bits
     * @return void
     */
    private static function drawFormatBits(array &$modules, int $bits): void
    {
        [$first, $second] = self::formatPositions(count($modules));
        for ($i = 0; $i < 15; $i++) {
            $bit = ($bits >> $i) & 1;
            $modules[$first[$i][0]][$first[$i][1]]   = $bit;
            $modules[$second[$i][0]][$second[$i][1]] = $bit;
        }
    }

    // -------------------------------------------------------------------------
    // Function patterns
    // -------------------------------------------------------------------------

    /**
     * Draw every function pattern and reserve every function module.
     *
     * Returns the module values and, separately, which modules are function
     * modules: data placement and masking touch only the unreserved ones.
     *
     * @param int $version
     * @return array{array<int, array<int, int>>, array<int, array<int, bool>>}
     */
    private static function functionPatterns(int $version): array
    {
        $size     = 17 + 4 * $version;
        $modules  = array_fill(0, $size, array_fill(0, $size, 0));
        $reserved = array_fill(0, $size, array_fill(0, $size, false));

        // Timing patterns: row 6 and column 6, dark on even indexes.
        for ($i = 0; $i < $size; $i++) {
            self::setFunction($modules, $reserved, 6, $i, $i % 2 === 0 ? 1 : 0);
            self::setFunction($modules, $reserved, $i, 6, $i % 2 === 0 ? 1 : 0);
        }

        // Finder patterns with their light separators.
        foreach ([[0, 0], [0, $size - 7], [$size - 7, 0]] as [$top, $left]) {
            for ($dr = -1; $dr <= 7; $dr++) {
                for ($dc = -1; $dc <= 7; $dc++) {
                    $row = $top + $dr;
                    $col = $left + $dc;
                    if ($row < 0 || $col < 0 || $row >= $size || $col >= $size) {
                        continue;
                    }
                    $ring = max(abs($dr - 3), abs($dc - 3));
                    self::setFunction($modules, $reserved, $row, $col, ($ring === 2 || $ring === 4) ? 0 : 1);
                }
            }
        }

        // Alignment patterns, except the three that would overlap finders.
        $centres = self::alignmentCentres($version);
        $last    = count($centres) - 1;
        foreach ($centres as $i => $row) {
            foreach ($centres as $j => $col) {
                if (($i === 0 && $j === 0) || ($i === 0 && $j === $last) || ($i === $last && $j === 0)) {
                    continue;
                }
                for ($dr = -2; $dr <= 2; $dr++) {
                    for ($dc = -2; $dc <= 2; $dc++) {
                        $dark = max(abs($dr), abs($dc)) === 1 ? 0 : 1;
                        self::setFunction($modules, $reserved, $row + $dr, $col + $dc, $dark);
                    }
                }
            }
        }

        // Format information areas (written per mask later), then the dark module.
        [$first, $second] = self::formatPositions($size);
        foreach (array_merge($first, $second) as [$row, $col]) {
            self::setFunction($modules, $reserved, $row, $col, 0);
        }
        self::setFunction($modules, $reserved, $size - 8, 8, 1);

        // Version information, versions 7 and up: two 6 x 3 blocks.
        if ($version >= 7) {
            $bits = self::versionBits($version);
            for ($i = 0; $i < 18; $i++) {
                $bit = ($bits >> $i) & 1;
                $a   = $size - 11 + $i % 3;
                $b   = intdiv($i, 3);
                self::setFunction($modules, $reserved, $b, $a, $bit);
                self::setFunction($modules, $reserved, $a, $b, $bit);
            }
        }

        return [$modules, $reserved];
    }

    /**
     * Set a function module and mark it reserved.
     *
     * @param array<int, array<int, int>>  $modules
     * @param array<int, array<int, bool>> $reserved
     * @param int              $row
     * @param int              $col
     * @param int              $dark
     * @return void
     */
    private static function setFunction(array &$modules, array &$reserved, int $row, int $col, int $dark): void
    {
        $modules[$row][$col]  = $dark;
        $reserved[$row][$col] = true;
    }

    /**
     * Alignment pattern centre coordinates (both axes) for the version.
     *
     * @param int $version
     * @return list<int>
     */
    private static function alignmentCentres(int $version): array
    {
        if ($version < 2) {
            return [];
        }
        // The first centre is always 6 and the last is 7 modules in from the
        // far edge; the ones between are evenly spaced by an even step.
        $count = intdiv($version, 7) + 2;
        $step  = $version === 32 ? 26 : intdiv($version * 4 + $count * 2 + 1, $count * 2 - 2) * 2;
        $last  = 17 + 4 * $version - 7;

        $centres = [6];
        for ($k = $count - 2; $k >= 0; $k--) {
            $centres[] = $last - $k * $step;
        }
        return $centres;
    }

    // -------------------------------------------------------------------------
    // Data placement and masking
    // -------------------------------------------------------------------------

    /**
     * Place codeword bits in the zigzag order: two-column strips from the
     * right edge, alternating upward and downward, skipping the vertical
     * timing column and every reserved module. Leftover modules stay light
     * (the remainder bits).
     *
     * @param array<int, array<int, int>>  $modules
     * @param array<int, array<int, bool>> $reserved
     * @param list<int>        $codewords
     * @return void
     */
    private static function placeData(array &$modules, array $reserved, array $codewords): void
    {
        $size      = count($modules);
        $totalBits = count($codewords) * 8;
        $bit       = 0;

        for ($right = $size - 1; $right >= 1; $right -= 2) {
            if ($right === 6) {
                $right = 5;
            }
            $upward = (intdiv($right + 1, 2) % 2) === 0;
            for ($step = 0; $step < $size; $step++) {
                $row = $upward ? $size - 1 - $step : $step;
                for ($k = 0; $k < 2; $k++) {
                    $col = $right - $k;
                    if ($reserved[$row][$col]) {
                        continue;
                    }
                    if ($bit < $totalBits) {
                        $modules[$row][$col] = ($codewords[$bit >> 3] >> (7 - ($bit & 7))) & 1;
                        $bit++;
                    }
                }
            }
        }
    }

    /**
     * Return a copy with mask pattern $mask applied to every non-reserved module.
     *
     * @param array<int, array<int, int>>  $modules
     * @param array<int, array<int, bool>> $reserved
     * @param int              $mask
     * @return array<int, array<int, int>>
     */
    private static function applyMask(array $modules, array $reserved, int $mask): array
    {
        $size = count($modules);
        for ($row = 0; $row < $size; $row++) {
            for ($col = 0; $col < $size; $col++) {
                if (!$reserved[$row][$col] && self::maskCondition($mask, $row, $col)) {
                    $modules[$row][$col] ^= 1;
                }
            }
        }
        return $modules;
    }

    /**
     * Mask pattern condition at (row, col): true means the module is inverted.
     *
     * @param int $mask
     * @param int $row
     * @param int $col
     * @return bool
     */
    private static function maskCondition(int $mask, int $row, int $col): bool
    {
        return match ($mask) {
            0 => ($row + $col) % 2 === 0,
            1 => $row % 2 === 0,
            2 => $col % 3 === 0,
            3 => ($row + $col) % 3 === 0,
            4 => (intdiv($row, 2) + intdiv($col, 3)) % 2 === 0,
            5 => ($row * $col) % 2 + ($row * $col) % 3 === 0,
            6 => (($row * $col) % 2 + ($row * $col) % 3) % 2 === 0,
            7 => (($row + $col) % 2 + ($row * $col) % 3) % 2 === 0,
            default => false,
        };
    }

    /**
     * Mask evaluation penalty: the four scoring rules of ISO/IEC 18004; lower is better.
     *
     * @param array<int, array<int, int>> $m
     * @return int
     */
    private static function penalty(array $m): int
    {
        $size    = count($m);
        $penalty = 0;
        $dark    = 0;

        for ($i = 0; $i < $size; $i++) {
            $dark += array_sum($m[$i]);
            foreach ([implode('', $m[$i]), implode('', array_column($m, $i))] as $line) {
                // Rule 1: each run of five or more same-colour modules scores 3 + (length - 5).
                if (preg_match_all('/0{5,}|1{5,}/', $line, $runs) > 0) {
                    foreach ($runs[0] as $run) {
                        $penalty += strlen($run) - 2;
                    }
                }
                // Rule 3: 1:1:3:1:1 finder-like pattern with four light modules on one side.
                // Neither pattern can overlap itself, so substr_count() finds every one.
                $penalty += 40 * (substr_count($line, '10111010000') + substr_count($line, '00001011101'));
            }
        }

        // Rule 2: 2 x 2 blocks of one colour.
        for ($r = 0; $r < $size - 1; $r++) {
            for ($c = 0; $c < $size - 1; $c++) {
                $v = $m[$r][$c];
                if ($v === $m[$r][$c + 1] && $v === $m[$r + 1][$c] && $v === $m[$r + 1][$c + 1]) {
                    $penalty += 3;
                }
            }
        }

        // Rule 4: deviation of the dark proportion from 50 %, per 5 % step.
        $total    = $size * $size;
        $penalty += 10 * intdiv(abs($dark * 20 - $total * 10), $total);

        return $penalty;
    }

    // -------------------------------------------------------------------------
    // SVG rendering
    // -------------------------------------------------------------------------

    /**
     * Render the matrix as an inline SVG: a white background and one path of dark modules.
     *
     * One path with crispEdges leaves no anti-aliased seams between modules,
     * and the fixed width and height keep the symbol square.
     *
     * @param array<int, array<int, int>> $matrix
     * @param int             $pixels Rendered width and height in pixels.
     * @return string
     */
    private static function renderSvg(array $matrix, int $pixels): string
    {
        $size   = count($matrix);
        $total  = $size + 2 * self::QUIET_ZONE;
        $pixels = max($pixels, self::MIN_MODULE_PX * $total);

        $path = '';
        for ($row = 0; $row < $size; $row++) {
            $col = 0;
            while ($col < $size) {
                if ($matrix[$row][$col] !== 1) {
                    $col++;
                    continue;
                }
                $start = $col;
                while ($col < $size && $matrix[$row][$col] === 1) {
                    $col++;
                }
                $run   = $col - $start;
                $path .= 'M' . ($start + self::QUIET_ZONE) . ',' . ($row + self::QUIET_ZONE) . 'h' . $run . 'v1h-' . $run . 'z';
            }
        }

        $svg  = '<svg xmlns="http://www.w3.org/2000/svg"';
        $svg .= ' width="' . esc_attr((string) $pixels) . '"';
        $svg .= ' height="' . esc_attr((string) $pixels) . '"';
        $svg .= ' viewBox="0 0 ' . esc_attr((string) $total) . ' ' . esc_attr((string) $total) . '"';
        $svg .= ' shape-rendering="crispEdges"';
        $svg .= ' role="img" aria-label="' . esc_attr__('QR code for two-factor setup', 'wpmgr-agent') . '">';
        $svg .= '<rect width="' . esc_attr((string) $total) . '" height="' . esc_attr((string) $total) . '" fill="#ffffff"/>';
        $svg .= '<path d="' . esc_attr($path) . '" fill="#000000"/>';
        $svg .= '</svg>';
        return $svg;
    }
}
