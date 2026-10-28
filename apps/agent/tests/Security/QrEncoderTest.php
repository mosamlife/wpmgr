<?php
/**
 * QrEncoderTest: ISO/IEC 18004 conformance of the 2FA setup QR code (#963).
 *
 * Three layers, each independent of the encoder's own tables:
 *
 *  1. Known-answer tests against published values: Reed-Solomon ECC for the
 *     1-M "HELLO WORLD" and "01234567" data codewords, the 32 format
 *     information BCH codes, the 34 version information BCH codes, the
 *     level-M block table (version 13 in particular).
 *  2. Structural checks on the shipped SVG output: finder patterns,
 *     separators, timing patterns, every alignment pattern, the dark module,
 *     both format information copies and both version information blocks
 *     must be exactly where the standard puts them, whatever mask was chosen.
 *  3. A round trip through an independent decoder (zbarimg): every encoded
 *     otpauth:// URI must decode back byte for byte. When zbarimg is not
 *     installed the round trip is marked skipped, never passed.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\QrEncoder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\QrEncoder
 */
final class QrEncoderTest extends TestCase
{
    /**
     * Level-M data codeword capacity per version (ISO/IEC 18004 Table 7).
     */
    private const DATA_CODEWORDS_M = [
        1 => 16, 28, 44, 64, 86, 108, 124, 154, 182, 216,
        254, 290, 334, 365, 415, 453, 507, 563, 627, 669,
        714, 782, 860, 914, 1000, 1062, 1128, 1193, 1267, 1373,
        1455, 1541, 1631, 1725, 1812, 1914, 1992, 2102, 2216, 2334,
    ];

    /**
     * Alignment pattern centre coordinates per version (ISO/IEC 18004 Annex E).
     */
    private const ALIGNMENT_CENTRES = [
        2 => [6, 18], 3 => [6, 22], 4 => [6, 26], 5 => [6, 30], 6 => [6, 34],
        7 => [6, 22, 38], 8 => [6, 24, 42], 9 => [6, 26, 46], 10 => [6, 28, 50],
        11 => [6, 30, 54], 12 => [6, 32, 58], 13 => [6, 34, 62],
        14 => [6, 26, 46, 66], 15 => [6, 26, 48, 70], 16 => [6, 26, 50, 74],
        17 => [6, 30, 54, 78], 18 => [6, 30, 56, 82], 19 => [6, 30, 58, 86],
        20 => [6, 34, 62, 90], 21 => [6, 28, 50, 72, 94], 22 => [6, 26, 50, 74, 98],
        23 => [6, 30, 54, 78, 102], 24 => [6, 28, 54, 80, 106], 25 => [6, 32, 58, 84, 110],
        26 => [6, 30, 58, 86, 114], 27 => [6, 34, 62, 90, 118],
        28 => [6, 26, 50, 74, 98, 122], 29 => [6, 30, 54, 78, 102, 126],
        30 => [6, 26, 52, 78, 104, 130], 31 => [6, 30, 56, 82, 108, 134],
        32 => [6, 34, 60, 86, 112, 138], 33 => [6, 30, 58, 86, 114, 142],
        34 => [6, 34, 62, 90, 118, 146], 35 => [6, 30, 54, 78, 102, 126, 150],
        36 => [6, 24, 50, 76, 102, 128, 154], 37 => [6, 28, 54, 80, 106, 132, 158],
        38 => [6, 32, 58, 84, 110, 136, 162], 39 => [6, 26, 54, 82, 110, 138, 166],
        40 => [6, 30, 58, 86, 114, 142, 170],
    ];

    /**
     * Format information after the 101010000010010 mask (ISO/IEC 18004 Annex C),
     * keyed by the level indicator (L=01, M=00, Q=11, H=10) then the mask.
     */
    private const FORMAT_CODES = [
        0b01 => [0x77C4, 0x72F3, 0x7DAA, 0x789D, 0x662F, 0x6318, 0x6C41, 0x6976],
        0b00 => [0x5412, 0x5125, 0x5E7C, 0x5B4B, 0x45F9, 0x40CE, 0x4F97, 0x4AA0],
        0b11 => [0x355F, 0x3068, 0x3F31, 0x3A06, 0x24B4, 0x2183, 0x2EDA, 0x2BED],
        0b10 => [0x1689, 0x13BE, 0x1CE7, 0x19D0, 0x0762, 0x0255, 0x0D0C, 0x083B],
    ];

    /**
     * Version information, versions 7 to 40 (ISO/IEC 18004 Annex D, Table D.1).
     */
    private const VERSION_CODES = [
        7 => 0x07C94, 0x085BC, 0x09A99, 0x0A4D3, 0x0BBF6, 0x0C762, 0x0D847, 0x0E60D,
        0x0F928, 0x10B78, 0x1145D, 0x12A17, 0x13532, 0x149A6, 0x15683, 0x168C9,
        0x177EC, 0x18EC4, 0x191E1, 0x1AFAB, 0x1B08E, 0x1CC1A, 0x1D33F, 0x1ED75,
        0x1F250, 0x209D5, 0x216F0, 0x228BA, 0x2379F, 0x24B0B, 0x2542E, 0x26A64,
        0x27541, 0x28C69,
    ];

    /** Quiet zone the SVG carries around the symbol, in modules. */
    private const QUIET = 4;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('esc_attr')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr__')->alias(fn ($t, $d = '') => $t);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // 1. Known-answer tests
    // -------------------------------------------------------------------------

    public function test_reed_solomon_ecc_matches_the_published_hello_world_1m_vector(): void
    {
        // "HELLO WORLD", alphanumeric mode, version 1-M: 16 data codewords.
        $data = [32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17];

        $this->assertSame(
            [196, 35, 39, 119, 235, 215, 231, 226, 93, 23],
            self::invoke('rsEncode', $data, 10)
        );
    }

    public function test_reed_solomon_ecc_matches_the_published_numeric_01234567_1m_vector(): void
    {
        // "01234567", numeric mode, version 1-M (the standard's worked example).
        $data = [16, 32, 12, 86, 97, 128, 236, 17, 236, 17, 236, 17, 236, 17, 236, 17];

        $this->assertSame(
            [165, 36, 212, 193, 237, 54, 199, 135, 44, 85],
            self::invoke('rsEncode', $data, 10)
        );
    }

    public function test_reed_solomon_ecc_of_zero_codewords_is_zero(): void
    {
        // A Reed-Solomon code is linear: the zero message has the zero remainder.
        // Confusing the field element 0 with alpha^0 breaks exactly this.
        foreach ([7, 10, 16, 18, 22, 24, 26, 28, 30] as $n) {
            $this->assertSame(
                array_fill(0, $n, 0),
                self::invoke('rsEncode', array_fill(0, 20, 0), $n),
                "zero block, {$n} ECC codewords"
            );
        }

        // A zero codeword in the middle of real data must contribute nothing:
        // the ECC of [x, 0, 0] equals the ECC of [x] shifted by two zero terms.
        $withZeros = self::invoke('rsEncode', [32, 0, 0, 91, 0, 11], 10);
        $this->assertCount(10, $withZeros);
        $this->assertSame($withZeros, self::invoke('rsEncode', [0, 0, 32, 0, 0, 91, 0, 11], 10));
    }

    public function test_format_information_bch_codes_for_every_level_and_mask(): void
    {
        foreach (self::FORMAT_CODES as $level => $byMask) {
            foreach ($byMask as $mask => $expected) {
                $this->assertSame(
                    $expected,
                    self::invoke('formatBits', $level, $mask),
                    sprintf('format bits for level indicator %02b, mask %d', $level, $mask)
                );
            }
        }
    }

    public function test_version_information_bch_codes_for_versions_7_to_40(): void
    {
        $this->assertCount(34, self::VERSION_CODES);
        foreach (self::VERSION_CODES as $version => $expected) {
            $this->assertSame($expected, self::invoke('versionBits', $version), "version information for v{$version}");
        }
    }

    public function test_version_13_m_has_nine_blocks(): void
    {
        // ISO/IEC 18004 Table 9, 13-M: 8 blocks of (59, 37) and 1 block of (60, 38).
        $expected = array_merge(array_fill(0, 8, [37, 22]), [[38, 22]]);

        $this->assertSame($expected, self::invoke('blockLayout', 13));
    }

    public function test_block_layout_matches_the_level_m_capacity_table_for_every_version(): void
    {
        foreach (self::DATA_CODEWORDS_M as $version => $dataCodewords) {
            /** @var list<array{int,int}> $layout */
            $layout = self::invoke('blockLayout', $version);
            $data   = array_sum(array_column($layout, 0));
            $this->assertSame($dataCodewords, $data, "data codewords, v{$version}-M");

            $lengths = array_unique(array_column($layout, 0));
            $this->assertLessThanOrEqual(2, count($lengths), "at most two block lengths, v{$version}");
            $this->assertLessThanOrEqual(1, max($lengths) - min($lengths), "block lengths differ by at most one, v{$version}");
        }

        // Spot-check the published group structure.
        $this->assertSame(array_merge(array_fill(0, 2, [38, 22]), array_fill(0, 2, [39, 22])), self::invoke('blockLayout', 8));
        $this->assertSame(array_merge([[50, 30]], array_fill(0, 4, [51, 30])), self::invoke('blockLayout', 11));
        $this->assertSame([[16, 10]], self::invoke('blockLayout', 1));
    }

    // -------------------------------------------------------------------------
    // 2. Structure of the shipped SVG output
    // -------------------------------------------------------------------------

    public function test_function_patterns_survive_masking_in_the_shipped_svg(): void
    {
        $masksSeen = [];
        foreach (self::corpus() as $uri) {
            $matrix  = self::matrixFromSvg(QrEncoder::toSvg($uri, 256));
            $size    = count($matrix);
            $version = intdiv($size - 17, 4);
            $this->assertSame(17 + 4 * $version, $size, 'symbol size is 17 + 4v');

            self::assertFunctionPatterns($matrix, $version);
            $masksSeen[self::readFormat($matrix)[0] & 0b111] = true;
        }

        // The corpus must exercise several masks, or a masked function module
        // could hide behind a mask that happens to leave it alone.
        $this->assertGreaterThanOrEqual(4, count($masksSeen), 'masks exercised: ' . implode(',', array_keys($masksSeen)));
    }

    public function test_format_information_is_level_m_and_both_copies_agree(): void
    {
        foreach (self::corpus() as $uri) {
            $matrix = self::matrixFromSvg(QrEncoder::toSvg($uri, 256));
            [$first, $second] = self::readFormat($matrix);

            $this->assertSame($first, $second, 'the two format information copies must agree');
            $this->assertContains(
                $first,
                self::FORMAT_CODES[0b00],
                sprintf('format information %015b is not a level-M code', $first)
            );
        }
    }

    public function test_version_information_is_present_from_version_7(): void
    {
        $checked = 0;
        foreach (self::versionSweep() as $version => $data) {
            $matrix = self::matrixFromSvg(QrEncoder::toSvg($data, 256));
            $this->assertSame(17 + 4 * $version, count($matrix), "v{$version} sweep input selects v{$version}");
            if ($version < 7) {
                continue;
            }
            $size = count($matrix);
            $topRight   = 0;
            $bottomLeft = 0;
            for ($i = 0; $i < 18; $i++) {
                $topRight   |= $matrix[intdiv($i, 3)][$size - 11 + $i % 3] << $i;
                $bottomLeft |= $matrix[$size - 11 + $i % 3][intdiv($i, 3)] << $i;
            }
            $this->assertSame(self::VERSION_CODES[$version], $topRight, "v{$version} top-right version block");
            $this->assertSame(self::VERSION_CODES[$version], $bottomLeft, "v{$version} bottom-left version block");
            $checked++;
        }
        $this->assertSame(34, $checked, 'every version from 7 to 40 checked');
    }

    public function test_svg_is_a_single_crisp_path_with_fixed_dimensions(): void
    {
        $svg = QrEncoder::toSvg('otpauth://totp/Site%3Aalice?secret=JBSWY3DPEHPK3PXP&issuer=Site', 256);

        $this->assertSame(1, substr_count($svg, '<path '), 'all dark modules are one path');
        $this->assertSame(1, substr_count($svg, '<rect '), 'only the background is a rect');
        $this->assertStringContainsString('shape-rendering="crispEdges"', $svg);
        $this->assertStringContainsString('width="256"', $svg);
        $this->assertStringContainsString('height="256"', $svg);
    }

    public function test_data_too_long_for_version_40_returns_empty_string(): void
    {
        // 2331 bytes is the 40-M byte-mode capacity; one more cannot be encoded.
        $this->assertNotSame('', QrEncoder::toSvg(str_repeat('A', 2331), 256));
        $this->assertSame('', QrEncoder::toSvg(str_repeat('A', 2332), 256));
    }

    // -------------------------------------------------------------------------
    // 3. Round trip through an independent decoder
    // -------------------------------------------------------------------------

    public function test_otpauth_uris_round_trip_through_zbarimg(): void
    {
        $zbarimg = self::zbarimg();
        $corpus  = self::corpus();
        $this->assertGreaterThanOrEqual(30, count($corpus));

        $versions = [];
        $failures = [];
        $decoded  = 0;
        foreach ($corpus as $uri) {
            $this->assertGreaterThanOrEqual(60, strlen($uri));
            $this->assertLessThanOrEqual(300, strlen($uri));
            $svg    = QrEncoder::toSvg($uri, 256);
            $matrix = self::matrixFromSvg($svg);
            $versions[intdiv(count($matrix) - 17, 4)] = true;

            $out = self::decode($zbarimg, $svg);
            if ($out === $uri) {
                $decoded++;
            } else {
                $failures[] = sprintf('%d bytes: got %s', strlen($uri), var_export($out, true));
            }
        }

        ksort($versions);
        $this->assertSame(count($corpus), $decoded, "decoded {$decoded}/" . count($corpus) . ":\n" . implode("\n", $failures));
        $this->assertArrayHasKey(4, $versions, 'corpus reaches version 4');
        $this->assertArrayHasKey(11, $versions, 'corpus reaches version 11');
    }

    public function test_every_version_from_1_to_40_round_trips_through_zbarimg(): void
    {
        $zbarimg  = self::zbarimg();
        $failures = [];
        foreach (self::versionSweep() as $version => $data) {
            $out = self::decode($zbarimg, QrEncoder::toSvg($data, 256));
            if ($out !== $data) {
                $failures[] = "v{$version} (" . strlen($data) . ' bytes)';
            }
        }
        $this->assertSame([], $failures, 'versions that did not decode byte for byte');
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Call a private static helper of the encoder.
     *
     * @param mixed ...$args
     * @return mixed
     */
    private static function invoke(string $method, ...$args)
    {
        $ref = new \ReflectionMethod(QrEncoder::class, $method);
        return $ref->invoke(null, ...$args);
    }

    /**
     * At least 30 otpauth:// URIs between 60 and 300 bytes.
     *
     * @return list<string>
     */
    private static function corpus(): array
    {
        $issuers  = ['Acme', 'Jayso Labs Staging', 'My%20Shop%20%E2%80%94%20Store', 'Example%20Blog', 'Northwind%20Traders%20Intranet', 'Caf%C3%A9%20Bistro'];
        $accounts = ['alice', 'bob%40example.com', 'site-admin', 'a.very.long.administrator.name%40subdomain.example.org', 'editor_42', 'ops%2Bbackup%40mail.example.net'];
        $secrets  = ['JBSWY3DPEHPK3PXP', 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 'MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43UOV3HO6DZPI'];

        // The shortest realistic URIs (60-62 bytes) land on version 4.
        $uris = [
            'otpauth://totp/Ab%3Aalice2?secret=JBSWY3DPEHPK3PXP&issuer=Ab',
            'otpauth://totp/Abc%3Aalice?secret=JBSWY3DPEHPK3PXP&issuer=Abc',
            'otpauth://totp/Abc%3Aalice1?secret=JBSWY3DPEHPK3PXP&issuer=Abc',
        ];
        $i = 0;
        foreach ($issuers as $issuer) {
            foreach ($accounts as $account) {
                $secret = $secrets[$i % 3];
                $uri    = 'otpauth://totp/' . $issuer . '%3A' . $account . '?secret=' . $secret . '&issuer=' . $issuer;
                if ($i % 2 === 1) {
                    $uri .= '&algorithm=SHA1&digits=6&period=30';
                }
                // Stretch some labels so the corpus spans the full 60-300 byte range.
                if ($i % 5 === 4) {
                    $uri .= '&image=' . str_repeat('x', 40 + 7 * $i);
                }
                $i++;
                if (strlen($uri) >= 60 && strlen($uri) <= 300) {
                    $uris[] = $uri;
                }
            }
        }

        // Version 11 (214-251 bytes), including both of its boundaries.
        $base = 'otpauth://totp/Long%20Issuer%20Name%3Aadministrator%40example.org?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=Long%20Issuer%20Name&image=';
        foreach ([214, 230, 251] as $len) {
            $uris[] = str_pad($base, $len, 'https%3A%2F%2Fcdn.example.org%2Flogo.png');
        }
        return $uris;
    }

    /**
     * One input per version 1-40, each exactly filling that version's byte capacity.
     *
     * @return array<int,string>
     */
    private static function versionSweep(): array
    {
        $out = [];
        foreach (self::DATA_CODEWORDS_M as $version => $dataCodewords) {
            $countBits = $version <= 9 ? 8 : 16;
            $capacity  = intdiv($dataCodewords * 8 - 4 - $countBits, 8);
            $seed      = 'otpauth://totp/V' . $version . '%3Auser?secret=JBSWY3DPEHPK3PXP&issuer=V' . $version . '&pad=';
            $out[$version] = substr(str_pad($seed, $capacity, 'abcdefghijklmnopqrstuvwxyz0123456789'), 0, $capacity);
        }
        return $out;
    }

    /**
     * Rasterise the encoder's SVG back into a module matrix (1 = dark), quiet zone removed.
     *
     * Reads the rendering the setup screen actually ships: one <path> of
     * horizontal runs, or (older output) one <rect> per module.
     *
     * @return list<list<int>>
     */
    private static function matrixFromSvg(string $svg): array
    {
        if (preg_match('/viewBox="0 0 (\d+) (\d+)"/', $svg, $vb) !== 1) {
            throw new \RuntimeException('SVG has no viewBox: ' . substr($svg, 0, 200));
        }
        $total = (int) $vb[1];
        $grid  = array_fill(0, $total, array_fill(0, $total, 0));
        $dark  = 0;

        if (preg_match('/<path d="([^"]*)" fill="#000000"/', $svg, $path) === 1) {
            preg_match_all('/M(\d+),(\d+)h(\d+)v1h-(\d+)z/', $path[1], $runs, PREG_SET_ORDER);
            foreach ($runs as $run) {
                for ($c = 0; $c < (int) $run[3]; $c++) {
                    $grid[(int) $run[2]][(int) $run[1] + $c] = 1;
                    $dark++;
                }
            }
        }
        preg_match_all('/<rect x="(\d+)" y="(\d+)" width="1" height="1" fill="#000000"/', $svg, $rects, PREG_SET_ORDER);
        foreach ($rects as $rect) {
            $grid[(int) $rect[2]][(int) $rect[1]] = 1;
            $dark++;
        }
        if ($dark < 50) {
            throw new \RuntimeException("SVG parse found only {$dark} dark modules");
        }

        $size   = $total - 2 * self::QUIET;
        $matrix = [];
        for ($r = 0; $r < $total; $r++) {
            for ($c = 0; $c < $total; $c++) {
                $inside = $r >= self::QUIET && $r < self::QUIET + $size && $c >= self::QUIET && $c < self::QUIET + $size;
                if (!$inside && $grid[$r][$c] === 1) {
                    throw new \RuntimeException("dark module in the quiet zone at ({$r},{$c})");
                }
            }
        }
        for ($r = 0; $r < $size; $r++) {
            $matrix[] = array_slice($grid[$r + self::QUIET], self::QUIET, $size);
        }
        return $matrix;
    }

    /**
     * Assert every function pattern the standard fixes, independent of the mask.
     *
     * @param list<list<int>> $m
     */
    private static function assertFunctionPatterns(array $m, int $version): void
    {
        $size = count($m);

        foreach ([[0, 0], [0, $size - 7], [$size - 7, 0]] as [$r0, $c0]) {
            for ($dr = -1; $dr <= 7; $dr++) {
                for ($dc = -1; $dc <= 7; $dc++) {
                    $r = $r0 + $dr;
                    $c = $c0 + $dc;
                    if ($r < 0 || $c < 0 || $r >= $size || $c >= $size) {
                        continue;
                    }
                    $ring = max(abs($dr - 3), abs($dc - 3));
                    $want = ($ring === 2 || $ring === 4) ? 0 : 1; // ring 4 is the separator
                    self::assertSame($want, $m[$r][$c], "v{$version} finder/separator module ({$r},{$c})");
                }
            }
        }

        for ($i = 8; $i <= $size - 9; $i++) {
            $want = $i % 2 === 0 ? 1 : 0;
            self::assertSame($want, $m[6][$i], "v{$version} horizontal timing module (6,{$i})");
            self::assertSame($want, $m[$i][6], "v{$version} vertical timing module ({$i},6)");
        }

        self::assertSame(1, $m[4 * $version + 9][8], "v{$version} dark module");

        $centres = self::ALIGNMENT_CENTRES[$version] ?? [];
        $last    = count($centres) - 1;
        foreach ($centres as $i => $row) {
            foreach ($centres as $j => $col) {
                if (($i === 0 && $j === 0) || ($i === 0 && $j === $last) || ($i === $last && $j === 0)) {
                    continue;
                }
                for ($dr = -2; $dr <= 2; $dr++) {
                    for ($dc = -2; $dc <= 2; $dc++) {
                        $want = max(abs($dr), abs($dc)) === 1 ? 0 : 1;
                        self::assertSame($want, $m[$row + $dr][$col + $dc], "v{$version} alignment pattern at ({$row},{$col}) offset ({$dr},{$dc})");
                    }
                }
            }
        }
    }

    /**
     * Read both 15-bit format information copies (bit 0 = least significant).
     *
     * @param list<list<int>> $m
     * @return array{int,int}
     */
    private static function readFormat(array $m): array
    {
        $size  = count($m);
        $first = [];
        for ($i = 0; $i <= 5; $i++) {
            $first[$i] = $m[$i][8];
        }
        $first[6] = $m[7][8];
        $first[7] = $m[8][8];
        $first[8] = $m[8][7];
        for ($i = 9; $i < 15; $i++) {
            $first[$i] = $m[8][14 - $i];
        }

        $second = [];
        for ($i = 0; $i < 8; $i++) {
            $second[$i] = $m[8][$size - 1 - $i];
        }
        for ($i = 8; $i < 15; $i++) {
            $second[$i] = $m[$size - 15 + $i][8];
        }

        $a = 0;
        $b = 0;
        for ($i = 0; $i < 15; $i++) {
            $a |= $first[$i] << $i;
            $b |= $second[$i] << $i;
        }
        return [$a, $b];
    }

    /**
     * Locate zbarimg, or mark the calling test skipped with the reason.
     * WPMGR_TEST_ZBARIMG names the binary explicitly (and lets the skip path be exercised).
     */
    private static function zbarimg(): string
    {
        $override   = getenv('WPMGR_TEST_ZBARIMG');
        $candidates = is_string($override) && $override !== ''
            ? [$override]
            : ['/opt/homebrew/bin/zbarimg', '/usr/local/bin/zbarimg', '/usr/bin/zbarimg'];
        foreach ($candidates as $candidate) {
            if (is_executable($candidate)) {
                return $candidate;
            }
        }
        self::markTestSkipped('zbarimg is not installed, so the QR round trip cannot run (brew install zbar / apt-get install zbar-tools).');
    }

    /** Rasterise the SVG to a PBM and return what zbarimg decodes from it, or null. */
    private static function decode(string $zbarimg, string $svg): ?string
    {
        if (preg_match('/viewBox="0 0 (\d+) (\d+)"/', $svg, $vb) !== 1) {
            return null;
        }
        $matrix = self::matrixFromSvg($svg);
        $size   = count($matrix);
        $total  = (int) $vb[1];
        $scale  = 4;
        $px     = $total * $scale;

        $rows = [];
        for ($y = 0; $y < $px; $y++) {
            $r    = intdiv($y, $scale) - self::QUIET;
            $line = '';
            for ($x = 0; $x < $px; $x++) {
                $c     = intdiv($x, $scale) - self::QUIET;
                $line .= ($r >= 0 && $c >= 0 && $r < $size && $c < $size && $matrix[$r][$c] === 1) ? '1' : '0';
            }
            $rows[] = $line;
        }

        $file = tempnam(sys_get_temp_dir(), 'wpmgr-qr-') . '.pbm';
        file_put_contents($file, "P1\n{$px} {$px}\n" . implode("\n", $rows) . "\n");
        $out  = [];
        $code = 0;
        exec(escapeshellarg($zbarimg) . ' --raw -q -Sdisable -Sqrcode.enable ' . escapeshellarg($file) . ' 2>/dev/null', $out, $code);
        unlink($file);
        unlink(substr($file, 0, -4));

        if ($code !== 0 || $out === []) {
            return null;
        }
        return implode("\n", $out);
    }
}
