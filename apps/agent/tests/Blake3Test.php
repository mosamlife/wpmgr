<?php
/**
 * Tests for the pure-PHP BLAKE3 implementation against the official test
 * vectors and streaming equivalence (the chunk-addressing primitive must match
 * the control plane byte-for-byte).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Support\Blake3;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\Blake3
 */
final class Blake3Test extends TestCase
{
    /**
     * Official BLAKE3 input: repeating 0..250 byte pattern.
     *
     * @param int $n Length.
     * @return string
     */
    private function input(int $n): string
    {
        $s = '';
        for ($i = 0; $i < $n; $i++) {
            $s .= chr($i % 251);
        }

        return $s;
    }

    /**
     * @return array<string,array{int,string}>
     */
    public static function vectors(): array
    {
        return [
            'empty'         => [0, 'af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262'],
            'one'           => [1, '2d3adedff11b61f14c886e35afa036736dcd87a74d27b5c1510225d0f592e213'],
            'block-1'       => [1023, '10108970eeda3eb932baac1428c7a2163b0e924c9a9e25b35bba72b28f70bd11'],
            'one-chunk'     => [1024, '42214739f095a406f3fc83deb889744ac00df831c10daa55189b5d121c855af7'],
            'two-chunks'    => [1025, 'd00278ae47eb27b34faecf67b4fe263f82d5412916c1ffd97c8cb7fb814b8444'],
            'two-full'      => [2048, 'e776b6028c7cd22a4d0ba182a8bf62205d2ef576467e838ed6f2529b85fba24a'],
            'three'         => [3072, 'b98cb0ff3623be03326b373de6b9095218513e64f1ee2edd2525c7ad1e5cffd2'],
            'four'          => [4096, '015094013f57a5277b59d8475c0501042c0b642e531b0a1c8f58d2163229e969'],
            'six'           => [6144, '3e2e5b74e048f3add6d21faab3f83aa44d3b2278afb83b80b3c35164ebeca205'],
        ];
    }

    /**
     * One-shot BLAKE3 over the streaming primitive (update()/finalize()).
     *
     * NOTE: the official vectors are asserted against the pure-PHP BLAKE3
     * implementation (the update()/finalize() streaming path), NOT against the
     * static Blake3::hashHex() convenience. Per ADR-033 (M5.6, v0.8.1)
     * hashHex() was deliberately switched to libsodium's blake2b-256 for a
     * ~100x speedup; the CP treats the chunk id as an opaque content-key, so
     * the algorithm behind hashHex() is a perf detail, not a wire contract.
     * The BLAKE3 construction itself still lives in update()/finalize() and is
     * what these official vectors exercise.
     *
     * @param string $data Input bytes.
     * @return string 64-char lowercase hex digest (BLAKE3, hash mode).
     */
    private function blake3Hex(string $data): string
    {
        $h = new Blake3();
        $h->update($data);

        return bin2hex($h->finalize());
    }

    /**
     * @dataProvider vectors
     * @param int    $n        Input length.
     * @param string $expected Expected hex digest.
     */
    public function test_official_vectors(int $n, string $expected): void
    {
        $this->assertSame($expected, $this->blake3Hex($this->input($n)));
    }

    public function test_streaming_equals_one_shot(): void
    {
        foreach ([0, 1, 63, 64, 65, 1023, 1024, 1025, 4096, 5000, 100000] as $n) {
            $data    = $this->input($n);
            $oneShot = $this->blake3Hex($data);

            $h      = new Blake3();
            $offset = 0;
            while ($offset < strlen($data)) {
                $take = ($offset % 97) + 1;
                $h->update(substr($data, $offset, $take));
                $offset += $take;
            }
            $this->assertSame($oneShot, bin2hex($h->finalize()), "stream mismatch at n=$n");
        }
    }

    /**
     * Distinct inputs hash to distinct 32-byte digests via the production
     * content-addressing primitive (Blake3::hashHex(), blake2b-256 per ADR-033).
     */
    public function test_distinct_inputs_distinct_hashes(): void
    {
        $a = Blake3::hashHex('alpha');
        $b = Blake3::hashHex('beta');
        $this->assertNotSame($a, $b);
        $this->assertSame(64, strlen($a));
    }

    /**
     * Guard the production content-addressing primitive: Blake3::hashHex() is
     * intentionally blake2b-256 (ADR-033), deterministic, and 32 bytes wide.
     */
    public function test_hash_hex_is_deterministic_blake2b(): void
    {
        $data = $this->input(4096);
        $this->assertSame(Blake3::hashHex($data), Blake3::hashHex($data));
        $this->assertSame(64, strlen(Blake3::hashHex($data)));

        if (function_exists('sodium_crypto_generichash')) {
            $this->assertSame(
                bin2hex(sodium_crypto_generichash($data, '', 32)),
                Blake3::hashHex($data),
                'hashHex() must match libsodium blake2b-256 (ADR-033 content-key).'
            );
        }
    }
}
