<?php
/**
 * Regression tests for GH #709: enrollment fataled with an uncaught
 * SodiumException on any host without the native libsodium PHP extension.
 *
 * HOW THE POLYFILL PATH IS EXERCISED
 * ----------------------------------
 * The bug only appears when WordPress's bundled sodium_compat polyfill
 * services the call, and this machine (like most CI runners) has the native
 * extension, so a test that merely calls the code proves nothing.
 *
 * These tests therefore make the platform behave exactly as an
 * extension-less host does, via tests/sodium-memzero-shim.php: a namespaced
 * WPMgr\Agent\Support\sodium_memzero() that shadows the global builtin for
 * the one namespace SecureMemory lives in, and raises the SodiumException
 * sodium_compat raises, message verbatim from
 * wp-includes/sodium_compat/src/Compat.php.
 *
 * That file's header explains why this is not a Patchwork redefinable-internal:
 * Patchwork forwards a wrapped internal's arguments by value, which both emits
 * a by-reference warning on every wipe in the suite (failOnWarning) and breaks
 * the very contract sodium_memzero() exists to provide.
 *
 * Production code is not modified, not parameterised for the test, and not told
 * it is under test: it takes the same branch it takes on a real CloudLinux host.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\SecureMemory;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\SecureMemory
 */
final class SecureMemoryTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $options = [];

    private string $keyFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // The capability answer is cached per request; every test starts by
        // forgetting it so each one re-probes against its own simulated host.
        SecureMemory::resetCapabilityCache();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-secmem-test-' . bin2hex(random_bytes(8)) . '.key';
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
        // The shim is process-wide, so it must be handed back inert or the
        // next test inherits a platform that refuses to wipe.
        SodiumPlatform::reset();
        SecureMemory::resetCapabilityCache();

        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Make this process behave like a host with no native libsodium: every
     * sodium_memzero() call raises sodium_compat's refusal.
     *
     * @return void
     */
    private function simulateHostWithoutNativeLibsodium(): void
    {
        SodiumPlatform::refuse();
    }

    // -----------------------------------------------------------------
    // The bug itself.
    // -----------------------------------------------------------------

    /**
     * The crux. Before the fix this call site was a bare sodium_memzero()
     * and this test fataled with an uncaught SodiumException.
     */
    public function test_wipe_does_not_throw_when_the_platform_refuses(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $secret = 'super-secret-key-material';
        SecureMemory::wipe($secret);

        $this->assertNull($secret, 'The value must still be cleared when the native wipe is unavailable.');
    }

    /**
     * The reported crash path end to end: generating the site keypair reaches
     * Keystore::encrypt() -> masterKey() -> ... -> the wipe. On a host without
     * native libsodium this fataled, which is why enrollment white-screened.
     */
    public function test_site_keypair_generation_completes_without_native_libsodium(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $keystore  = new Keystore();
        $publicKey = $keystore->generateSiteKeypair();

        $this->assertSame(
            SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES,
            strlen($publicKey),
            'A full Ed25519 public key must still be produced on a host without native libsodium.'
        );
        $this->assertNotSame('', (string) ($this->options[Keystore::OPTION_SITE_KEYPAIR] ?? ''));
    }

    /**
     * The enrollment entry point from the issue's backtrace
     * (Enrollment::buildEnrollPayload -> Signer::agentPublicKeyBase64).
     */
    public function test_agent_public_key_base64_completes_without_native_libsodium(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $signer = new Signer(new Keystore());
        $base64 = $signer->agentPublicKeyBase64();

        $this->assertNotSame('', $base64);
        $this->assertSame(
            SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES,
            strlen((string) base64_decode($base64, true)),
            'agentPublicKeyBase64() must return a decodable 32-byte Ed25519 key.'
        );
    }

    /**
     * Request signing shares the same code path and was equally unreachable.
     */
    public function test_request_signing_completes_without_native_libsodium(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $keystore = new Keystore();
        $signer   = new Signer($keystore);
        $signer->agentPublicKey();

        $headers = $signer->signHeaders('POST', '/v1/agent/heartbeat', '{}');

        $this->assertNotSame('', $headers[Signer::HEADER_SIGNATURE] ?? '');
        $this->assertNotSame('', $headers[Signer::HEADER_KEY] ?? '');
    }

    /**
     * The refusal is probed once, not on every secret. Without the cache each
     * key operation would build and throw a fresh exception forever.
     */
    public function test_platform_refusal_is_probed_only_once(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        for ($i = 0; $i < 25; $i++) {
            $secret = 'secret-' . $i;
            SecureMemory::wipe($secret);
        }

        $this->assertSame(1, SodiumPlatform::$calls, 'The platform must be asked once, then the answer reused.');
        $this->assertFalse(SecureMemory::hasNativeWipe());
    }

    // -----------------------------------------------------------------
    // The over-fire arm: the wipe must NOT be skipped where it works.
    // -----------------------------------------------------------------

    /**
     * On a platform that can wipe, the real sodium_memzero() is still called.
     * A "fix" that simply stopped wiping would pass every test above and fail
     * this one.
     */
    public function test_native_wipe_is_still_performed_when_the_platform_supports_it(): void
    {
        SodiumPlatform::reset();

        $secret = 'super-secret-key-material';
        SecureMemory::wipe($secret);

        $this->assertSame(1, SodiumPlatform::$calls, 'sodium_memzero() must still be called where supported.');
        $this->assertSame(
            'super-secret-key-material',
            SodiumPlatform::$wiped[0],
            'It must receive the real value to wipe.'
        );
        // The shim delegates to the real builtin, so this is the genuine wipe.
        $this->assertNull($secret, 'The native wipe must actually have cleared the value.');
        $this->assertTrue(SecureMemory::hasNativeWipe());
    }

    /**
     * And it keeps being called for every subsequent secret, rather than the
     * capability cache turning one success into a permanent skip.
     */
    public function test_native_wipe_is_performed_for_every_secret(): void
    {
        SodiumPlatform::reset();

        foreach (['alpha', 'bravo', 'charlie'] as $value) {
            $secret = $value;
            SecureMemory::wipe($secret);
        }

        $this->assertSame(['alpha', 'bravo', 'charlie'], SodiumPlatform::$wiped);
    }

    /**
     * The same over-fire arm, proven outside this harness.
     *
     * Every assertion above still runs through a shim, however thin, and a
     * shim is a thing that can be wrong. So this arm runs in a clean
     * subprocess with no shim, no PHPUnit and no stubs: real PHP, the real
     * class file, the real global sodium_memzero(), the real platform. It is
     * the assertion a "fix" that quietly stopped wiping would fail.
     */
    public function test_wipe_really_clears_the_value_on_an_uninstrumented_platform(): void
    {
        $classFile = dirname(__DIR__) . '/includes/support/class-secure-memory.php';
        $script    = sprintf(
            '<?php define("ABSPATH", "/wp/"); require %s; $s = "real-platform-secret"; '
                . '\\WPMgr\\Agent\\Support\\SecureMemory::wipe($s); '
                . 'echo (extension_loaded("sodium") ? "native" : "polyfill"), "|", var_export($s, true), "|", '
                . 'var_export(\\WPMgr\\Agent\\Support\\SecureMemory::hasNativeWipe(), true);',
            var_export($classFile, true)
        );

        $scriptFile = sys_get_temp_dir() . '/wpmgr-secmem-subprocess-' . bin2hex(random_bytes(8)) . '.php';
        file_put_contents($scriptFile, $script);

        $output = (string) shell_exec(escapeshellarg(PHP_BINARY) . ' ' . escapeshellarg($scriptFile) . ' 2>&1');
        @unlink($scriptFile);

        $parts = explode('|', trim($output));
        $this->assertCount(3, $parts, 'Subprocess did not complete cleanly. Output: ' . $output);

        [$platform, $wiped, $usedNative] = $parts;

        $this->assertSame('NULL', $wiped, 'The secret must be cleared on a real, uninstrumented platform.');

        if ($platform === 'native') {
            $this->assertSame(
                'true',
                $usedNative,
                'With the native extension present the real sodium_memzero() must still be the one doing the work.'
            );
        } else {
            $this->assertSame('false', $usedNative);
        }
    }

    // -----------------------------------------------------------------
    // Drop-in equivalence with the sodium_memzero() calls it replaced.
    // -----------------------------------------------------------------

    /**
     * Several call sites wipe an array element (AgeIdentity wipes
     * $pair['secret']), so the by-reference contract must hold for those too.
     */
    public function test_wipe_clears_an_array_element_by_reference(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $pair = ['secret' => 'private-key-bytes', 'recipient' => 'age1public'];
        SecureMemory::wipe($pair['secret']);

        $this->assertNull($pair['secret']);
        $this->assertSame('age1public', $pair['recipient'], 'Only the named element may be touched.');
    }

    /**
     * The helper replaced a call that fataled; it must not be able to
     * introduce a fatal of its own on an unexpected argument.
     */
    public function test_wipe_tolerates_a_non_string_value(): void
    {
        $this->simulateHostWithoutNativeLibsodium();

        $value = null;
        SecureMemory::wipe($value);
        $this->assertNull($value);

        $empty = '';
        SecureMemory::wipe($empty);
        $this->assertNull($empty);
    }

    /**
     * No sodium primitive other than memzero may be guarded away: the sweep
     * for #709 confirmed sodium_compat implements all of them, so they must
     * keep being called directly rather than routed through a fallback.
     */
    public function test_only_memzero_was_replaced(): void
    {
        $dir   = dirname(__DIR__) . '/includes';
        $files = new \RecursiveIteratorIterator(new \RecursiveDirectoryIterator($dir));

        $helper = $dir . '/support/class-secure-memory.php';

        $found = [];
        foreach ($files as $file) {
            if (!$file->isFile() || $file->getExtension() !== 'php') {
                continue;
            }
            // The helper is the one place allowed to make the raw call: it is
            // what wraps it in the guard.
            if ($file->getPathname() === $helper) {
                continue;
            }
            $source = (string) file_get_contents($file->getPathname());
            if (preg_match('/\bsodium_memzero\s*\(/', $source) === 1) {
                $found[] = $file->getPathname();
            }
        }

        $this->assertSame(
            [],
            $found,
            'sodium_memzero() must not be called directly; use SecureMemory::wipe(). Offenders: '
                . implode(', ', $found)
        );
    }
}
