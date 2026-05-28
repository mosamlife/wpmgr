<?php
/**
 * Tests for the age identity lifecycle over the real encrypted Keystore: the
 * PRIVATE key is generated, stored ENCRYPTED (never as plaintext), the public
 * recipient is stable, recipientMatches enforces the agent's own recipient, and
 * encrypt/decrypt round-trips through the stored identity.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\AgeIdentity
 */
final class AgeIdentityTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $options = [];

    private string $keyFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-age-test-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_ensure_recipient_generates_and_persists_encrypted(): void
    {
        $keystore = new Keystore();
        $identity = new AgeIdentity($keystore);

        $recipient = $identity->ensureRecipient();
        $this->assertStringStartsWith('age1', $recipient);

        // The identity option must be present and ENCRYPTED (not the raw secret).
        $stored = $this->options[Keystore::OPTION_AGE_IDENTITY] ?? null;
        $this->assertIsString($stored);
        $raw = base64_decode($stored, true);
        $this->assertIsString($raw);
        $this->assertGreaterThan(32, strlen($raw), 'stored form must be the AES-GCM envelope, not a bare 32-byte key');

        // The decrypted secret must derive back to the same recipient.
        $secret = $keystore->getAgeIdentity();
        $this->assertIsString($secret);
        $this->assertSame(32, strlen($secret));
        $this->assertSame($recipient, $identity->recipient());
    }

    public function test_recipient_is_stable_across_instances(): void
    {
        $keystore = new Keystore();
        $first    = (new AgeIdentity($keystore))->ensureRecipient();
        $second   = (new AgeIdentity($keystore))->ensureRecipient();
        $this->assertSame($first, $second, 'recipient must not change once provisioned');
    }

    public function test_recipient_matches_own_only(): void
    {
        $identity = new AgeIdentity(new Keystore());
        $own = $identity->ensureRecipient();
        $this->assertTrue($identity->recipientMatches($own));
        $this->assertFalse($identity->recipientMatches('age1someotherrecipient'));
        $this->assertFalse($identity->recipientMatches(''));
    }

    public function test_encrypt_then_decrypt_through_stored_identity(): void
    {
        $identity = new AgeIdentity(new Keystore());
        $identity->ensureRecipient();

        $plain = 'backup chunk plaintext';
        $ct    = $identity->encryptChunk($plain);
        $this->assertStringStartsWith('age-encryption.org/v1' . "\n", $ct);
        $this->assertSame($plain, $identity->decryptChunk($ct));
    }

    public function test_recipient_empty_when_not_provisioned(): void
    {
        $identity = new AgeIdentity(new Keystore());
        $this->assertSame('', $identity->recipient());
    }
}
