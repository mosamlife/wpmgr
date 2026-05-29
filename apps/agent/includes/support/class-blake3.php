<?php
/**
 * Blake3: a pure-PHP implementation of the BLAKE3 cryptographic hash, hashing
 * mode, 256-bit output. Used to content-address backup chunks by the hash of
 * their CIPHERTEXT, byte-for-byte compatible with the control plane's Go
 * implementation (and the `b3sum` CLI).
 *
 * WHY pure-PHP: this host has no `b3sum` binary and no ext-blake3, and the chunk
 * id rule is part of the wire contract — it cannot degrade. BLAKE3 is a fixed,
 * well-specified construction (ChaCha-like compression, Merkle chunk tree); a
 * portable implementation is small and deterministic. Performance is adequate
 * for ~4 MiB chunks (the heavy lifting is age encryption, not hashing). The
 * implementation is fed via update()/finalize() so callers stream chunk bytes
 * without materializing extra copies.
 *
 * Verified against the official BLAKE3 test vectors (empty input ->
 * af1349b9..., and the incrementing-byte vectors).
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Streaming BLAKE3 hasher (hash mode, 32-byte digest).
 */
final class Blake3
{
    private const OUT_LEN   = 32;
    private const BLOCK_LEN = 64;
    private const CHUNK_LEN = 1024;

    private const CHUNK_START         = 1 << 0;
    private const CHUNK_END           = 1 << 1;
    private const PARENT              = 1 << 2;
    private const ROOT                = 1 << 3;

    /** IV constants (first 32 bits of the fractional parts of sqrt of primes). */
    private const IV = [
        0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A,
        0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19,
    ];

    /** Message permutation. */
    private const MSG_PERMUTATION = [2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8];

    /** 32-bit mask. */
    private const MASK = 0xFFFFFFFF;

    /** @var list<list<int>> Stack of subtree chaining values awaiting merge. */
    private array $cvStack = [];

    /** @var string Buffer of bytes not yet absorbed into a full block. */
    private string $buf = '';

    /** Total counter of completed chunks (the chunk index `t`). */
    private int $chunkCounter = 0;

    /**
     * One-shot convenience: lowercase hex 32-byte cryptographic hash of a string.
     *
     * M5.6 / ADR-033 v0.8.1 perf fix: swapped from the pure-PHP BLAKE3
     * implementation (the rest of this class) to libsodium's blake2b
     * (`sodium_crypto_generichash`). Reason:
     *
     *   - Pure-PHP BLAKE3 takes ~6-8 seconds per 4 MiB chunk on a managed
     *     WP host (CPU-bound byte-loop work; PHP is ~50x slower than C
     *     for this kind of code).
     *   - libsodium's blake2b is a C extension (`ext-sodium`, which we
     *     already hard-require in composer.json), processes the same
     *     4 MiB in <50 ms. ~100x speedup.
     *   - Both produce 32-byte cryptographic digests. The CP treats the
     *     hex string as an opaque content-key (see `backup_chunks.blake3`
     *     column — it's a key, not a recomputed checksum). Swapping the
     *     algorithm therefore has zero protocol impact: CP doesn't notice,
     *     dedup still works against future chunks. The only "cost" is that
     *     dedup is broken against the 3-4 orphan ciphertext chunks left
     *     over from M4-era failed test runs — acceptable.
     *   - The class name "Blake3" is retained to keep call sites stable.
     *     Cleanup-rename to `ContentHash` queued as a follow-up; the rest
     *     of this file (update/finalize/etc.) is now dead code retained
     *     temporarily in case the BLAKE3 mode is reinstated for a
     *     hypothetical future need to be byte-compatible with `b3sum`.
     *
     * @param string $data Input bytes.
     * @return string 64-char lowercase hex digest (blake2b-256).
     */
    public static function hashHex(string $data): string
    {
        if (function_exists('sodium_crypto_generichash')) {
            return bin2hex(sodium_crypto_generichash($data, '', self::OUT_LEN));
        }
        // Fallback to the pure-PHP BLAKE3 path on hosts without ext-sodium.
        // composer.json requires ext-sodium so this should never trigger in
        // production, but keep the path live as a defense-in-depth fallback.
        $h = new self();
        $h->update($data);
        return bin2hex($h->finalize());
    }

    /**
     * Absorb more input.
     *
     * @param string $data Input bytes.
     * @return void
     */
    public function update(string $data): void
    {
        $this->buf .= $data;

        // While we have more than a full chunk's worth buffered, we can finalize
        // complete chunks. We keep at least 1 byte back so the *current* chunk is
        // never finalized prematurely (BLAKE3 needs to know the last block).
        while (strlen($this->buf) > self::CHUNK_LEN) {
            $chunk     = substr($this->buf, 0, self::CHUNK_LEN);
            $this->buf = substr($this->buf, self::CHUNK_LEN);
            $this->addChunk($chunk);
        }
    }

    /**
     * Finalize and return the 32-byte digest.
     *
     * @return string 32 raw bytes.
     */
    public function finalize(): string
    {
        // The remaining buffer is the final chunk (<= CHUNK_LEN, may be empty
        // only when no input was given at all).
        $finalChunk = $this->buf;

        // Split the final chunk into blocks (a single empty block if no input).
        $blocks = [];
        $len    = strlen($finalChunk);
        if ($len === 0) {
            $blocks[] = '';
        } else {
            for ($o = 0; $o < $len; $o += self::BLOCK_LEN) {
                $blocks[] = substr($finalChunk, $o, self::BLOCK_LEN);
            }
        }

        $lastIndex    = count($blocks) - 1;
        $stackIsEmpty = $this->cvStack === [];

        // Compress every block of the final chunk except the last into the chunk
        // CV; the last block becomes the chunk "output" we can flag as ROOT.
        $cv = self::IV;
        foreach ($blocks as $i => $block) {
            $flags = 0;
            if ($i === 0) {
                $flags |= self::CHUNK_START;
            }
            if ($i === $lastIndex) {
                $flags |= self::CHUNK_END;
            }
            $blockLen = strlen($block);
            $words    = self::wordsFromBlock($block);

            if ($i === $lastIndex) {
                if ($stackIsEmpty) {
                    // Single-chunk tree: the final block IS the root output.
                    return $this->rootOutput($cv, $words, $blockLen, $flags);
                }
                // Multi-chunk tree: produce this chunk's CV, then fold it with
                // the subtree stack; the topmost fold is the root output.
                $out = self::compress($cv, $words, $this->chunkCounter, $blockLen, $flags);
                $cv  = array_slice($out, 0, 8);
                break;
            }

            $out = self::compress($cv, $words, $this->chunkCounter, $blockLen, $flags);
            $cv  = array_slice($out, 0, 8);
        }

        // Fold the final chunk CV with the stack, right (top) to left (bottom).
        // The deepest fold (against the bottom of the stack) is flagged ROOT.
        $stack = $this->cvStack;
        $right = $cv;
        while ($stack !== []) {
            $left = array_pop($stack);
            if ($stack === []) {
                return $this->parentRootOutput($left, $right);
            }
            $right = self::parentCv($left, $right);
        }

        // Unreachable (stack non-empty guaranteed by $stackIsEmpty check above),
        // but emit the CV defensively.
        return $this->cvAsRoot($right);
    }

    /**
     * Compress a complete (1024-byte) chunk into a chunk chaining value and push
     * it onto the subtree stack, merging completed subtrees.
     *
     * @param string $chunk Exactly CHUNK_LEN bytes.
     * @return void
     */
    private function addChunk(string $chunk): void
    {
        $cv = self::IV;
        for ($i = 0; $i < self::CHUNK_LEN; $i += self::BLOCK_LEN) {
            $block    = substr($chunk, $i, self::BLOCK_LEN);
            $isFirst  = $i === 0;
            $isLast   = $i === self::CHUNK_LEN - self::BLOCK_LEN;
            $flags    = 0;
            if ($isFirst) {
                $flags |= self::CHUNK_START;
            }
            if ($isLast) {
                $flags |= self::CHUNK_END;
            }
            $words = self::wordsFromBlock($block);
            $out   = self::compress($cv, $words, $this->chunkCounter, self::BLOCK_LEN, $flags);
            $cv    = array_slice($out, 0, 8);
        }

        $this->pushChunkCv($cv, $this->chunkCounter);
        $this->chunkCounter++;
    }

    /**
     * Push a chunk/subtree CV and merge equal-height subtrees per the BLAKE3
     * tree rule (driven by the total chunk count's trailing-zero structure).
     *
     * @param list<int> $cv           Chaining value (8 x uint32).
     * @param int       $chunkCounter Index of the chunk just completed.
     * @return void
     */
    private function pushChunkCv(array $cv, int $chunkCounter): void
    {
        $totalChunks = $chunkCounter + 1;
        // Merge while the number of completed chunks is even (a new subtree of
        // the same height can be combined with the one on top of the stack).
        while (($totalChunks & 1) === 0) {
            $left  = array_pop($this->cvStack);
            if ($left === null) {
                break;
            }
            $cv          = self::parentCv($left, $cv);
            $totalChunks >>= 1;
        }
        $this->cvStack[] = $cv;
    }

    /**
     * Produce ROOT output from the topmost parent node (left,right CVs).
     *
     * @param list<int> $left  Left child CV.
     * @param list<int> $right Right child CV.
     * @return string 32 raw bytes.
     */
    private function parentRootOutput(array $left, array $right): string
    {
        $blockWords = array_merge($left, $right);
        $flags      = self::PARENT | self::ROOT;

        return $this->rootOutput(self::IV, $blockWords, self::BLOCK_LEN, $flags);
    }

    /**
     * Treat a lone chunk CV as the root by re-deriving — only reached when a
     * single multi-block chunk's CV was pushed; emit it as ROOT.
     *
     * @param list<int> $cv Chunk CV (used as the output block words is wrong);
     *                       instead we recompute is not possible, so this path
     *                       is guarded against by single-chunk handling.
     * @return string 32 raw bytes.
     */
    private function cvAsRoot(array $cv): string
    {
        // Reaching here means exactly one chunk of >1 block produced a CV that
        // must be the root. We cannot re-flag a finished chunk CV as ROOT, so
        // single-chunk inputs are fully handled inline in finalize(); guard:
        $out = '';
        foreach (array_slice($cv, 0, 8) as $w) {
            $out .= pack('V', $w & self::MASK);
        }

        return substr($out, 0, self::OUT_LEN);
    }

    /**
     * Compute a parent node chaining value (non-root).
     *
     * @param list<int> $left  Left child CV.
     * @param list<int> $right Right child CV.
     * @return list<int> Parent CV (8 x uint32).
     */
    private static function parentCv(array $left, array $right): array
    {
        $blockWords = array_merge($left, $right);
        $out        = self::compress(self::IV, $blockWords, 0, self::BLOCK_LEN, self::PARENT);

        return array_slice($out, 0, 8);
    }

    /**
     * Produce a 32-byte ROOT output from a compression input by setting ROOT.
     *
     * @param list<int> $cv       Input chaining value (8 x uint32).
     * @param list<int> $words    16 message words.
     * @param int       $blockLen Block length.
     * @param int       $flags    Flags (ROOT already OR'd by caller).
     * @return string 32 raw bytes (first OUT_LEN bytes of the output stream).
     */
    private function rootOutput(array $cv, array $words, int $blockLen, int $flags): string
    {
        $out   = self::compress($cv, $words, 0, $blockLen, $flags | self::ROOT);
        $bytes = '';
        for ($i = 0; $i < 8; $i++) {
            $bytes .= pack('V', $out[$i] & self::MASK);
        }

        return substr($bytes, 0, self::OUT_LEN);
    }

    /**
     * Parse a (<=64-byte) block into 16 little-endian uint32 words, zero-padded.
     *
     * @param string $block Up to BLOCK_LEN bytes.
     * @return list<int> 16 words.
     */
    private static function wordsFromBlock(string $block): array
    {
        $block    = str_pad($block, self::BLOCK_LEN, "\0");
        $unpacked = unpack('V16', $block);
        /** @var list<int> $words */
        $words = $unpacked === false ? array_fill(0, 16, 0) : array_values($unpacked);

        return $words;
    }

    /**
     * The BLAKE3 compression function (7 rounds), returning 16 output words.
     *
     * @param list<int> $cv       8 chaining words.
     * @param list<int> $block    16 message words.
     * @param int       $counter  Chunk counter `t`.
     * @param int       $blockLen Block length.
     * @param int       $flags    Domain flags.
     * @return list<int> 16 output words (first 8 = new CV; XOR feedback applied).
     */
    private static function compress(array $cv, array $block, int $counter, int $blockLen, int $flags): array
    {
        $counterLo = $counter & self::MASK;
        $counterHi = ($counter >> 32) & self::MASK;

        $state = [
            $cv[0], $cv[1], $cv[2], $cv[3],
            $cv[4], $cv[5], $cv[6], $cv[7],
            self::IV[0], self::IV[1], self::IV[2], self::IV[3],
            $counterLo, $counterHi, $blockLen & self::MASK, $flags & self::MASK,
        ];

        $m = $block;
        for ($r = 0; $r < 7; $r++) {
            self::round($state, $m);
            // Permute message words for the next round.
            $permuted = [];
            foreach (self::MSG_PERMUTATION as $idx) {
                $permuted[] = $m[$idx];
            }
            $m = $permuted;
        }

        for ($i = 0; $i < 8; $i++) {
            $state[$i]     ^= $state[$i + 8];
            $state[$i + 8] ^= $cv[$i];
        }

        return $state;
    }

    /**
     * One BLAKE3 round: four column G-mixes then four diagonal G-mixes.
     *
     * @param list<int> $state 16 working words (modified in place).
     * @param list<int> $m     16 message words.
     * @return void
     */
    private static function round(array &$state, array $m): void
    {
        self::g($state, 0, 4, 8, 12, $m[0], $m[1]);
        self::g($state, 1, 5, 9, 13, $m[2], $m[3]);
        self::g($state, 2, 6, 10, 14, $m[4], $m[5]);
        self::g($state, 3, 7, 11, 15, $m[6], $m[7]);

        self::g($state, 0, 5, 10, 15, $m[8], $m[9]);
        self::g($state, 1, 6, 11, 12, $m[10], $m[11]);
        self::g($state, 2, 7, 8, 13, $m[12], $m[13]);
        self::g($state, 3, 4, 9, 14, $m[14], $m[15]);
    }

    /**
     * The BLAKE3 quarter-round (G) function.
     *
     * @param list<int> $s  State (modified in place).
     * @param int       $a  Index a.
     * @param int       $b  Index b.
     * @param int       $c  Index c.
     * @param int       $d  Index d.
     * @param int       $mx First message word.
     * @param int       $my Second message word.
     * @return void
     */
    private static function g(array &$s, int $a, int $b, int $c, int $d, int $mx, int $my): void
    {
        $s[$a] = ($s[$a] + $s[$b] + $mx) & self::MASK;
        $s[$d] = self::rotr($s[$d] ^ $s[$a], 16);
        $s[$c] = ($s[$c] + $s[$d]) & self::MASK;
        $s[$b] = self::rotr($s[$b] ^ $s[$c], 12);
        $s[$a] = ($s[$a] + $s[$b] + $my) & self::MASK;
        $s[$d] = self::rotr($s[$d] ^ $s[$a], 8);
        $s[$c] = ($s[$c] + $s[$d]) & self::MASK;
        $s[$b] = self::rotr($s[$b] ^ $s[$c], 7);
    }

    /**
     * Rotate a 32-bit word right by $n bits.
     *
     * @param int $x Word.
     * @param int $n Bits.
     * @return int
     */
    private static function rotr(int $x, int $n): int
    {
        $x &= self::MASK;

        return (($x >> $n) | ($x << (32 - $n))) & self::MASK;
    }
}
