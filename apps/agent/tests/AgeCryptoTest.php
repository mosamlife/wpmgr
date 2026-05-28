<?php
/**
 * Tests for the age v1 (X25519) crypto primitive: identity generation, Bech32
 * round-trips, recipient/identity consistency, encrypt/decrypt round-trips
 * across the STREAM chunk boundary, and tamper/wrong-key rejection.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Support\AgeCrypto;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\AgeCrypto
 */
final class AgeCryptoTest extends TestCase
{
    public function test_generate_identity_shapes(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();

        $this->assertStringStartsWith('age1', $pair['recipient']);
        $this->assertStringStartsWith('AGE-SECRET-KEY-1', $pair['identity']);
        $this->assertSame(32, strlen($pair['secret']));
        // Identity is upper-case on the wire (Bech32 HRP "AGE-SECRET-KEY-").
        $this->assertSame(strtoupper($pair['identity']), $pair['identity']);
    }

    public function test_recipient_for_secret_matches_generated(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();
        $this->assertSame($pair['recipient'], $age->recipientForSecret($pair['secret']));
    }

    public function test_decode_identity_roundtrips_secret(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();
        $this->assertSame($pair['secret'], $age->decodeIdentity($pair['identity']));
    }

    public function test_decode_recipient_yields_x25519_public_key(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();
        $pub  = $age->decodeRecipient($pair['recipient']);
        $this->assertSame(32, strlen($pub));
        $this->assertSame(sodium_crypto_scalarmult_base($pair['secret']), $pub);
    }

    public function test_roundtrip_across_stream_boundary(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();

        // Sizes straddling the 64 KiB STREAM chunk boundary.
        foreach ([0, 1, 100, 65535, 65536, 65537, 131073] as $n) {
            $plain = $n === 0 ? '' : random_bytes($n);
            $ct    = $age->encrypt($plain, $pair['recipient']);
            $this->assertStringStartsWith('age-encryption.org/v1' . "\n", $ct);
            $this->assertSame($plain, $age->decrypt($ct, $pair['secret']), "roundtrip failed at n=$n");
        }
    }

    public function test_tampered_ciphertext_is_rejected(): void
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();
        $ct   = $age->encrypt('sensitive backup bytes', $pair['recipient']);

        // Flip the last byte of the payload.
        $ct[strlen($ct) - 1] = $ct[strlen($ct) - 1] === "\x00" ? "\x01" : "\x00";

        $this->expectException(\RuntimeException::class);
        $age->decrypt($ct, $pair['secret']);
    }

    public function test_wrong_identity_cannot_decrypt(): void
    {
        $age  = new AgeCrypto();
        $a    = $age->generateIdentity();
        $b    = $age->generateIdentity();
        $ct   = $age->encrypt('secret', $a['recipient']);

        $this->expectException(\RuntimeException::class);
        $age->decrypt($ct, $b['secret']);
    }

    public function test_malformed_recipient_rejected(): void
    {
        $age = new AgeCrypto();
        $this->expectException(\RuntimeException::class);
        $age->encrypt('x', 'age1notavalidrecipient');
    }
}
