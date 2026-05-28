<?php
/**
 * AgeCrypto: a self-contained, interoperable implementation of the age v1
 * encryption format (https://age-encryption.org/v1) for the X25519 recipient
 * type, built ENTIRELY on libsodium primitives. No ext-age, no external `age`
 * binary, no network.
 *
 * WHY pure-PHP-over-sodium instead of shelling to `age`:
 *   - The control plane stores ONLY ciphertext + the age PUBLIC recipient
 *     ("age1..."). It must never see the identity. age v1 with the X25519
 *     recipient is just X25519 + HKDF-SHA256 + ChaCha20-Poly1305 + a Bech32
 *     key encoding — every one of those is available in ext-sodium + hash().
 *   - Output produced here is byte-for-byte readable by the real `age`/`rage`
 *     binary, and this class reads anything `age` produces for an X25519
 *     identity. So the agent interoperates with the contract's "age (armor
 *     off; binary)" requirement on ANY host with ext-sodium, with NO host
 *     binary requirement and NO graceful-degradation gap.
 *
 * Format implemented (X25519 only; scrypt/SSH recipients are out of scope and
 * rejected on decrypt):
 *   header:  "age-encryption.org/v1\n"
 *            "-> X25519 <b64(ephemeral_share)>\n"
 *            "<b64(wrapped_file_key) wrapped at 64 cols>\n"
 *            "--- <b64(header_mac)>\n"
 *   payload: 16-byte nonce || STREAM(ChaCha20-Poly1305) of 64 KiB chunks.
 *
 * Key derivations (all HKDF-SHA256):
 *   wrap key  : ikm=X25519(eph_sec, recipient), salt=eph_share||recipient,
 *               info="age-encryption.org/v1/X25519"          -> 32 bytes
 *   file-key  : 16 random bytes
 *   stanza body: ChaCha20-Poly1305(wrap_key, file_key), 12x 0x00 nonce
 *   header MAC: key=HKDF(ikm=file_key, salt="", info="header"),
 *               HMAC-SHA256 over the header up to and incl. "---"
 *   payload key: HKDF(ikm=file_key, salt=16-byte payload nonce, info="payload")
 *   STREAM nonce: 11-byte big-endian counter || 1-byte final flag (0x01 last)
 *
 * Base64 in the header is RFC 4648 §4 standard alphabet WITHOUT padding.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * age v1 X25519 encrypt/decrypt over libsodium.
 */
class AgeCrypto
{
    /** age payload STREAM plaintext chunk size (spec-fixed at 64 KiB). */
    public const STREAM_CHUNK = 65536;

    /** ChaCha20-Poly1305 (IETF) tag length. */
    private const TAG = 16;

    /** age file key length (spec-fixed at 16 bytes). */
    private const FILE_KEY_BYTES = 16;

    /** Bech32 HRP for a public recipient. */
    private const HRP_RECIPIENT = 'age';

    /** Bech32 HRP for a secret identity (upper-case on the wire). */
    private const HRP_IDENTITY = 'AGE-SECRET-KEY-';

    /**
     * Generate a fresh X25519 age identity.
     *
     * @return array{identity:string,recipient:string,secret:string}
     *               identity  = "AGE-SECRET-KEY-1..." (Bech32, upper-case),
     *               recipient = "age1..." (Bech32, lower-case),
     *               secret    = raw 32-byte X25519 scalar (caller must zero it).
     */
    public function generateIdentity(): array
    {
        $secret    = random_bytes(SODIUM_CRYPTO_SCALARMULT_BYTES);
        $publicKey = sodium_crypto_scalarmult_base($secret);

        return [
            'identity'  => strtoupper(self::bech32Encode(self::HRP_IDENTITY, $secret)),
            'recipient' => self::bech32Encode(self::HRP_RECIPIENT, $publicKey),
            'secret'    => $secret,
        ];
    }

    /**
     * Derive the public recipient ("age1...") for a raw X25519 secret scalar.
     *
     * @param string $secret Raw 32-byte X25519 scalar.
     * @return string "age1..." recipient.
     * @throws \RuntimeException On an invalid secret length.
     */
    public function recipientForSecret(string $secret): string
    {
        if (strlen($secret) !== SODIUM_CRYPTO_SCALARMULT_BYTES) {
            throw new \RuntimeException('WPMgr Agent: invalid age secret length.');
        }
        $publicKey = sodium_crypto_scalarmult_base($secret);

        return self::bech32Encode(self::HRP_RECIPIENT, $publicKey);
    }

    /**
     * Decode an "AGE-SECRET-KEY-1..." identity string to its raw 32-byte scalar.
     *
     * @param string $identity Bech32 identity (case-insensitive).
     * @return string Raw 32-byte X25519 scalar.
     * @throws \RuntimeException On a malformed identity.
     */
    public function decodeIdentity(string $identity): string
    {
        [$hrp, $data] = self::bech32Decode($identity);
        if (strtoupper($hrp) !== self::HRP_IDENTITY) {
            throw new \RuntimeException('WPMgr Agent: not an age identity.');
        }
        if (strlen($data) !== SODIUM_CRYPTO_SCALARMULT_BYTES) {
            throw new \RuntimeException('WPMgr Agent: bad age identity length.');
        }

        return $data;
    }

    /**
     * Decode an "age1..." recipient string to its raw 32-byte X25519 public key.
     *
     * @param string $recipient Bech32 recipient.
     * @return string Raw 32-byte public key.
     * @throws \RuntimeException On a malformed recipient.
     */
    public function decodeRecipient(string $recipient): string
    {
        [$hrp, $data] = self::bech32Decode($recipient);
        if (strtolower($hrp) !== self::HRP_RECIPIENT) {
            throw new \RuntimeException('WPMgr Agent: not an age recipient.');
        }
        if (strlen($data) !== SODIUM_CRYPTO_SCALARMULT_BYTES) {
            throw new \RuntimeException('WPMgr Agent: bad age recipient length.');
        }

        return $data;
    }

    /**
     * Encrypt a plaintext blob to a single "age1..." recipient (binary, no
     * armor). Memory-bounded by the STREAM chunking (one 64 KiB chunk resident
     * at a time relative to the input), suitable for ~4 MiB backup chunks.
     *
     * @param string $plaintext Bytes to encrypt.
     * @param string $recipient "age1..." recipient string.
     * @return string Binary age ciphertext.
     * @throws \RuntimeException On a malformed recipient.
     */
    public function encrypt(string $plaintext, string $recipient): string
    {
        $recipientKey = $this->decodeRecipient($recipient);

        $fileKey = random_bytes(self::FILE_KEY_BYTES);
        $header  = $this->buildHeader($fileKey, $recipientKey);
        $payload = $this->encryptPayload($fileKey, $plaintext);

        sodium_memzero($fileKey);

        return $header . $payload;
    }

    /**
     * Decrypt a binary age ciphertext with a raw X25519 secret scalar.
     *
     * @param string $ciphertext Binary age file.
     * @param string $secret     Raw 32-byte X25519 scalar.
     * @return string Recovered plaintext.
     * @throws \RuntimeException On any parse/auth failure (no detail leaked).
     */
    public function decrypt(string $ciphertext, string $secret): string
    {
        if (strlen($secret) !== SODIUM_CRYPTO_SCALARMULT_BYTES) {
            throw new \RuntimeException('WPMgr Agent: invalid age secret length.');
        }

        [$fileKey, $payloadOffset] = $this->parseHeaderAndUnwrap($ciphertext, $secret);

        try {
            return $this->decryptPayload($fileKey, substr($ciphertext, $payloadOffset));
        } finally {
            sodium_memzero($fileKey);
        }
    }

    // ------------------------------------------------------------------
    // Header
    // ------------------------------------------------------------------

    /**
     * Build the age header (recipient stanza + MAC line) for a file key.
     *
     * @param string $fileKey      16-byte file key.
     * @param string $recipientKey Raw 32-byte recipient public key.
     * @return string Header bytes (ends with "\n").
     */
    private function buildHeader(string $fileKey, string $recipientKey): string
    {
        $ephSecret = random_bytes(SODIUM_CRYPTO_SCALARMULT_BYTES);
        $ephShare  = sodium_crypto_scalarmult_base($ephSecret);

        $shared  = sodium_crypto_scalarmult($ephSecret, $recipientKey);
        sodium_memzero($ephSecret);

        $salt    = $ephShare . $recipientKey;
        $wrapKey = hash_hkdf('sha256', $shared, 32, 'age-encryption.org/v1/X25519', $salt);
        sodium_memzero($shared);

        // Wrap the file key: ChaCha20-Poly1305-IETF, 12x 0x00 nonce, no AAD.
        $wrapped = sodium_crypto_aead_chacha20poly1305_ietf_encrypt(
            $fileKey,
            '',
            str_repeat("\0", SODIUM_CRYPTO_AEAD_CHACHA20POLY1305_IETF_NPUBBYTES),
            $wrapKey
        );
        sodium_memzero($wrapKey);

        $stanza = '-> X25519 ' . self::b64($ephShare) . "\n"
            . self::wrap64(self::b64($wrapped)) . "\n";

        $macInput = 'age-encryption.org/v1' . "\n" . $stanza . '---';

        $macKey = hash_hkdf('sha256', $fileKey, 32, 'header', '');
        $mac    = hash_hmac('sha256', $macInput, $macKey, true);
        sodium_memzero($macKey);

        return $macInput . ' ' . self::b64($mac) . "\n";
    }

    /**
     * Parse the header, locate the X25519 stanza, unwrap the file key, verify
     * the header MAC, and return [fileKey, payloadOffset].
     *
     * @param string $ciphertext Full age file.
     * @param string $secret     Raw 32-byte X25519 scalar.
     * @return array{0:string,1:int}
     * @throws \RuntimeException On any failure.
     */
    private function parseHeaderAndUnwrap(string $ciphertext, string $secret): array
    {
        $marker = "\n--- ";
        $macPos = strpos($ciphertext, $marker);
        if ($macPos === false) {
            throw new \RuntimeException('WPMgr Agent: malformed age header.');
        }

        // The MAC line: "--- <b64mac>\n". Find the end of that line.
        $macLineStart = $macPos + 1; // points at "---"
        $macEol       = strpos($ciphertext, "\n", $macLineStart);
        if ($macEol === false) {
            throw new \RuntimeException('WPMgr Agent: malformed age MAC line.');
        }

        $macInput = substr($ciphertext, 0, $macPos) . "\n" . '---';
        $macLine  = substr($ciphertext, $macLineStart, $macEol - $macLineStart);
        if (strncmp($macLine, '--- ', 4) !== 0) {
            throw new \RuntimeException('WPMgr Agent: malformed age MAC line.');
        }
        $macB64 = substr($macLine, 4);

        $headerText = substr($ciphertext, 0, $macPos); // "age-...v1\n" + stanzas (no trailing LF)
        $lines      = explode("\n", $headerText);
        if (array_shift($lines) !== 'age-encryption.org/v1') {
            throw new \RuntimeException('WPMgr Agent: unsupported age version.');
        }

        $fileKey = $this->unwrapFromStanzas($lines, $secret);
        if ($fileKey === null) {
            throw new \RuntimeException('WPMgr Agent: no decryptable X25519 stanza.');
        }

        // Verify header MAC with the recovered file key.
        $macKey   = hash_hkdf('sha256', $fileKey, 32, 'header', '');
        $expected = hash_hmac('sha256', $macInput, $macKey, true);
        sodium_memzero($macKey);

        $got = self::b64Decode($macB64);
        if ($got === null || !hash_equals($expected, $got)) {
            sodium_memzero($fileKey);
            throw new \RuntimeException('WPMgr Agent: age header MAC mismatch.');
        }

        return [$fileKey, $macEol + 1];
    }

    /**
     * Walk header stanza lines, and for each "-> X25519 <share>" stanza try to
     * unwrap the file key with our secret. Returns the file key or null.
     *
     * @param list<string> $lines  Header lines after the version line (the last
     *                              element is "" only if headerText ended in \n;
     *                              here it does not).
     * @param string       $secret Raw 32-byte X25519 scalar.
     * @return string|null 16-byte file key, or null if no stanza matched.
     */
    private function unwrapFromStanzas(array $lines, string $secret): ?string
    {
        $count = count($lines);
        for ($i = 0; $i < $count; $i++) {
            $line = $lines[$i];
            if (strncmp($line, '-> ', 3) !== 0) {
                continue;
            }
            $args = explode(' ', substr($line, 3));
            // Body is the following line(s) until a line that does not start a
            // new stanza; age bodies are a single wrapped base64 blob whose last
            // line is < 64 chars. We accept the immediate next line(s).
            $body = '';
            $j    = $i + 1;
            while ($j < $count && strncmp($lines[$j], '-> ', 3) !== 0) {
                $body .= $lines[$j];
                // A full-length 64-char line means the body continues.
                if (strlen($lines[$j]) < 64) {
                    $j++;
                    break;
                }
                $j++;
            }

            if (($args[0] ?? '') !== 'X25519' || !isset($args[1])) {
                continue;
            }

            $ephShare = self::b64Decode($args[1]);
            $wrapped  = self::b64Decode($body);
            if ($ephShare === null || strlen($ephShare) !== SODIUM_CRYPTO_SCALARMULT_BYTES || $wrapped === null) {
                continue;
            }

            $fileKey = $this->tryUnwrap($ephShare, $wrapped, $secret);
            if ($fileKey !== null) {
                return $fileKey;
            }
        }

        return null;
    }

    /**
     * Attempt to unwrap a file key from one X25519 stanza.
     *
     * @param string $ephShare Raw 32-byte ephemeral share.
     * @param string $wrapped  Wrapped file key (32 bytes: ct||tag).
     * @param string $secret   Raw 32-byte X25519 scalar.
     * @return string|null 16-byte file key, or null on auth failure.
     */
    private function tryUnwrap(string $ephShare, string $wrapped, string $secret): ?string
    {
        $publicKey = sodium_crypto_scalarmult_base($secret);
        $shared    = sodium_crypto_scalarmult($secret, $ephShare);
        $salt      = $ephShare . $publicKey;
        $wrapKey   = hash_hkdf('sha256', $shared, 32, 'age-encryption.org/v1/X25519', $salt);
        sodium_memzero($shared);

        $fileKey = sodium_crypto_aead_chacha20poly1305_ietf_decrypt(
            $wrapped,
            '',
            str_repeat("\0", SODIUM_CRYPTO_AEAD_CHACHA20POLY1305_IETF_NPUBBYTES),
            $wrapKey
        );
        sodium_memzero($wrapKey);

        if ($fileKey === false || strlen($fileKey) !== self::FILE_KEY_BYTES) {
            return null;
        }

        return $fileKey;
    }

    // ------------------------------------------------------------------
    // Payload (STREAM)
    // ------------------------------------------------------------------

    /**
     * Encrypt the payload using the age STREAM construction.
     *
     * @param string $fileKey   16-byte file key.
     * @param string $plaintext Plaintext bytes.
     * @return string 16-byte nonce || sequence of ChaCha20-Poly1305 chunks.
     */
    private function encryptPayload(string $fileKey, string $plaintext): string
    {
        $nonce      = random_bytes(16);
        $payloadKey = hash_hkdf('sha256', $fileKey, 32, 'payload', $nonce);

        $out    = $nonce;
        $len    = strlen($plaintext);
        $offset = 0;
        $chunk  = 0;

        // Emit at least one (possibly empty-flagged) chunk; an empty payload
        // produces a single final chunk over zero plaintext bytes.
        do {
            $slice = substr($plaintext, $offset, self::STREAM_CHUNK);
            $offset += strlen($slice);
            $isFinal = $offset >= $len;

            $out .= sodium_crypto_aead_chacha20poly1305_ietf_encrypt(
                $slice,
                '',
                self::streamNonce($chunk, $isFinal),
                $payloadKey
            );
            $chunk++;
        } while ($offset < $len);

        sodium_memzero($payloadKey);

        return $out;
    }

    /**
     * Decrypt an age STREAM payload.
     *
     * @param string $payload 16-byte nonce || chunks.
     * @param string $fileKey 16-byte file key.
     * @return string Recovered plaintext.
     * @throws \RuntimeException On any auth/framing failure.
     */
    private function decryptPayload(string $fileKey, string $payload): string
    {
        if (strlen($payload) < 16) {
            throw new \RuntimeException('WPMgr Agent: truncated age payload.');
        }
        $nonce      = substr($payload, 0, 16);
        $payloadKey = hash_hkdf('sha256', $fileKey, 32, 'payload', $nonce);

        $encChunk = self::STREAM_CHUNK + self::TAG;
        $body     = substr($payload, 16);
        $bodyLen  = strlen($body);

        $out    = '';
        $offset = 0;
        $chunk  = 0;

        try {
            do {
                $remaining = $bodyLen - $offset;
                if ($remaining <= 0) {
                    // Zero-length body is invalid: there is always >=1 chunk.
                    throw new \RuntimeException('WPMgr Agent: empty age payload body.');
                }
                $take    = min($encChunk, $remaining);
                $slice   = substr($body, $offset, $take);
                $offset += $take;
                $isFinal = $offset >= $bodyLen;

                $plain = sodium_crypto_aead_chacha20poly1305_ietf_decrypt(
                    $slice,
                    '',
                    self::streamNonce($chunk, $isFinal),
                    $payloadKey
                );
                if ($plain === false) {
                    throw new \RuntimeException('WPMgr Agent: age payload auth failed.');
                }
                $out .= $plain;
                $chunk++;
            } while ($offset < $bodyLen);
        } finally {
            sodium_memzero($payloadKey);
        }

        return $out;
    }

    /**
     * Build the 12-byte STREAM nonce: 11-byte big-endian counter || final flag.
     *
     * @param int  $counter Chunk index (>= 0).
     * @param bool $isFinal Whether this is the last chunk.
     * @return string 12 bytes.
     */
    private static function streamNonce(int $counter, bool $isFinal): string
    {
        // 11-byte big-endian counter. PHP ints are 64-bit; the high 3 bytes are
        // always zero for any realistic payload, so pad an 8-byte BE pack.
        $packed = pack('J', $counter); // 8 bytes big-endian.
        $nonce  = str_repeat("\0", 3) . $packed;

        return $nonce . ($isFinal ? "\x01" : "\x00");
    }

    // ------------------------------------------------------------------
    // Encoding helpers
    // ------------------------------------------------------------------

    /**
     * RFC 4648 §4 base64 WITHOUT padding (age header encoding).
     *
     * @param string $raw Raw bytes.
     * @return string Unpadded standard base64.
     */
    private static function b64(string $raw): string
    {
        return rtrim(base64_encode($raw), '=');
    }

    /**
     * Decode unpadded (or padded) standard base64; null on failure.
     *
     * @param string $b64 Encoded text.
     * @return string|null Raw bytes, or null.
     */
    private static function b64Decode(string $b64): ?string
    {
        $b64 = trim($b64);
        $pad = strlen($b64) % 4;
        if ($pad !== 0) {
            $b64 .= str_repeat('=', 4 - $pad);
        }
        $raw = base64_decode($b64, true);

        return $raw === false ? null : $raw;
    }

    /**
     * Wrap a base64 string at 64 columns with LF separators (age header style).
     *
     * @param string $b64 Unwrapped base64.
     * @return string Column-wrapped base64 (no trailing LF).
     */
    private static function wrap64(string $b64): string
    {
        return rtrim(chunk_split($b64, 64, "\n"), "\n");
    }

    // ------------------------------------------------------------------
    // Bech32 (BIP-0173) for age key encoding
    // ------------------------------------------------------------------

    /** Bech32 charset. */
    private const BECH32_CHARSET = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l';

    /**
     * Bech32-encode an HRP + 8-bit data payload (converted to 5-bit groups).
     *
     * @param string $hrp  Human-readable part.
     * @param string $data Raw bytes.
     * @return string Bech32 string (lower-case body).
     */
    private static function bech32Encode(string $hrp, string $data): string
    {
        $hrp     = strtolower($hrp);
        $values  = self::convertBits(array_values(unpack('C*', $data) ?: []), 8, 5, true);
        $checksum = self::bech32CreateChecksum($hrp, $values);
        $combined = array_merge($values, $checksum);

        $out = $hrp . '1';
        foreach ($combined as $v) {
            $out .= self::BECH32_CHARSET[$v];
        }

        return $out;
    }

    /**
     * Bech32-decode to [hrp, rawBytes]. Validates the checksum.
     *
     * @param string $bech Bech32 string (any case).
     * @return array{0:string,1:string}
     * @throws \RuntimeException On a malformed/invalid string.
     */
    private static function bech32Decode(string $bech): array
    {
        $lower = strtolower($bech);
        $upper = strtoupper($bech);
        if ($bech !== $lower && $bech !== $upper) {
            throw new \RuntimeException('WPMgr Agent: mixed-case Bech32.');
        }
        $bech = $lower;

        $pos = strrpos($bech, '1');
        if ($pos === false || $pos < 1 || $pos + 7 > strlen($bech)) {
            throw new \RuntimeException('WPMgr Agent: malformed Bech32.');
        }
        $hrp  = substr($bech, 0, $pos);
        $dataPart = substr($bech, $pos + 1);

        $values = [];
        $len    = strlen($dataPart);
        for ($i = 0; $i < $len; $i++) {
            $idx = strpos(self::BECH32_CHARSET, $dataPart[$i]);
            if ($idx === false) {
                throw new \RuntimeException('WPMgr Agent: invalid Bech32 char.');
            }
            $values[] = $idx;
        }

        if (!self::bech32VerifyChecksum($hrp, $values)) {
            throw new \RuntimeException('WPMgr Agent: Bech32 checksum failed.');
        }

        $dataValues = array_slice($values, 0, -6);
        $bytes      = self::convertBits($dataValues, 5, 8, false);

        return [$hrp, self::packBytes($bytes)];
    }

    /**
     * Pack an array of byte values into a binary string.
     *
     * @param list<int> $bytes Byte values 0..255.
     * @return string
     */
    private static function packBytes(array $bytes): string
    {
        $out = '';
        foreach ($bytes as $b) {
            $out .= chr($b);
        }

        return $out;
    }

    /**
     * Convert between bit groups (e.g. 8-bit bytes <-> 5-bit groups).
     *
     * @param list<int> $data    Input values.
     * @param int       $fromBits Source group width.
     * @param int       $toBits   Target group width.
     * @param bool      $pad      Whether to pad the final group.
     * @return list<int>
     * @throws \RuntimeException On invalid padding when not padding.
     */
    private static function convertBits(array $data, int $fromBits, int $toBits, bool $pad): array
    {
        $acc     = 0;
        $bits    = 0;
        $ret     = [];
        $maxv    = (1 << $toBits) - 1;
        $maxAcc  = (1 << ($fromBits + $toBits - 1)) - 1;

        foreach ($data as $value) {
            if ($value < 0 || ($value >> $fromBits) !== 0) {
                throw new \RuntimeException('WPMgr Agent: Bech32 bit overflow.');
            }
            $acc  = (($acc << $fromBits) | $value) & $maxAcc;
            $bits += $fromBits;
            while ($bits >= $toBits) {
                $bits -= $toBits;
                $ret[] = ($acc >> $bits) & $maxv;
            }
        }

        if ($pad) {
            if ($bits > 0) {
                $ret[] = ($acc << ($toBits - $bits)) & $maxv;
            }
        } elseif ($bits >= $fromBits || (($acc << ($toBits - $bits)) & $maxv) !== 0) {
            throw new \RuntimeException('WPMgr Agent: Bech32 invalid padding.');
        }

        return $ret;
    }

    /**
     * Expand an HRP into the values used by the Bech32 checksum polynomial.
     *
     * @param string $hrp Human-readable part.
     * @return list<int>
     */
    private static function bech32HrpExpand(string $hrp): array
    {
        $out = [];
        $len = strlen($hrp);
        for ($i = 0; $i < $len; $i++) {
            $out[] = ord($hrp[$i]) >> 5;
        }
        $out[] = 0;
        for ($i = 0; $i < $len; $i++) {
            $out[] = ord($hrp[$i]) & 31;
        }

        return $out;
    }

    /**
     * Bech32 polymod over the value sequence.
     *
     * @param list<int> $values Value sequence.
     * @return int
     */
    private static function bech32Polymod(array $values): int
    {
        $gen = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3];
        $chk = 1;
        foreach ($values as $v) {
            $top = $chk >> 25;
            $chk = (($chk & 0x1ffffff) << 5) ^ $v;
            for ($i = 0; $i < 5; $i++) {
                if (($top >> $i) & 1) {
                    $chk ^= $gen[$i];
                }
            }
        }

        return $chk;
    }

    /**
     * Compute the 6-symbol Bech32 checksum.
     *
     * @param string    $hrp    Human-readable part.
     * @param list<int> $values 5-bit data values.
     * @return list<int>
     */
    private static function bech32CreateChecksum(string $hrp, array $values): array
    {
        $poly   = array_merge(self::bech32HrpExpand($hrp), $values, [0, 0, 0, 0, 0, 0]);
        $mod    = self::bech32Polymod($poly) ^ 1;
        $out    = [];
        for ($i = 0; $i < 6; $i++) {
            $out[] = ($mod >> (5 * (5 - $i))) & 31;
        }

        return $out;
    }

    /**
     * Verify a Bech32 checksum.
     *
     * @param string    $hrp    Human-readable part.
     * @param list<int> $values 5-bit data values incl. the 6-symbol checksum.
     * @return bool
     */
    private static function bech32VerifyChecksum(string $hrp, array $values): bool
    {
        return self::bech32Polymod(array_merge(self::bech32HrpExpand($hrp), $values)) === 1;
    }
}
