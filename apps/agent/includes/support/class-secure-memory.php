<?php
/**
 * Best-effort wiping of sensitive values from PHP memory.
 *
 * WHY THIS EXISTS
 * ---------------
 * sodium_memzero() is not universally callable. When a host has no native
 * libsodium PHP extension, WordPress falls back to its bundled pure-PHP
 * polyfill (wp-includes/sodium_compat), and that polyfill's memzero()
 * deliberately throws SodiumException rather than pretend to have wiped
 * anything -- PHP genuinely cannot scrub a buffer it does not own.
 *
 * Calling sodium_memzero() unguarded therefore turns a best-effort hygiene
 * step into an uncaught fatal on an entire class of host. CloudLinux/cPanel
 * shared hosting commonly ships with the extension off by default, so on
 * those sites the agent could not enrol at all: the first key derivation
 * fataled and the admin screen went white.
 *
 * WHAT THIS DOES
 * --------------
 * wipe() calls the real sodium_memzero() whenever the platform can actually
 * perform it, and degrades to the best overwrite PHP itself can manage when
 * it cannot. The wipe is hygiene, not a security boundary: no key material
 * is handled differently, no algorithm is weakened, and nothing about the
 * cryptography changes. Only the post-use scrub degrades, on precisely the
 * hosts where PHP was never able to do it in the first place.
 *
 * CAPABILITY DETECTION, NOT ENVIRONMENT SNIFFING
 * ----------------------------------------------
 * supportsNativeWipe() probes the capability by attempting it once and
 * remembering the answer for the rest of the request. It does not test a PHP
 * version, a host name, or a SAPI, and it deliberately does not test
 * extension_loaded('sodium') either: a host carrying the older PECL
 * "libsodium" extension has no 'sodium' extension yet still wipes
 * successfully, because sodium_compat routes memzero() through
 * \Sodium\memzero() when that is present. Asking the platform to do the work
 * is the only check that is right on every host.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Platform-aware, never-fatal replacement for a bare sodium_memzero() call.
 */
final class SecureMemory
{
    /**
     * Tri-state cache of whether this platform can perform a real
     * sodium_memzero(): true = yes, false = no, null = not yet probed.
     *
     * Cached per request so the SodiumException raised by the polyfill is
     * constructed at most once, no matter how many secrets are wiped.
     *
     * @var bool|null
     */
    private static ?bool $nativeWipe = null;

    /**
     * Wipe a sensitive value, by reference, as thoroughly as this platform
     * allows. Never throws, and never leaves the value readable.
     *
     * Accepts the value untyped and by reference so that it is a drop-in for
     * sodium_memzero() at every existing call site, including array elements
     * such as $pair['secret']. It is untyped on purpose: this function exists
     * to remove a fatal from the key paths, so it must not be capable of
     * introducing a different one via an argument type error.
     *
     * @param mixed $value Value to wipe. Modified in place.
     * @return void
     */
    public static function wipe(&$value): void
    {
        if (is_string($value) && self::supportsNativeWipe($value)) {
            return;
        }

        self::overwrite($value);
    }

    /**
     * Attempt the native wipe, caching whether this platform supports it.
     *
     * On the first call the wipe is genuinely attempted; that attempt is both
     * the capability probe and the real work, so a capable platform pays no
     * extra cost. Once the platform has refused, no further attempt is made.
     *
     * @param string $value Value to wipe. Modified in place on success.
     * @return bool True when the native wipe was performed.
     */
    private static function supportsNativeWipe(string &$value): bool
    {
        if (self::$nativeWipe === false) {
            return false;
        }

        if (!function_exists('sodium_memzero')) {
            self::$nativeWipe = false;

            return false;
        }

        try {
            sodium_memzero($value);
            self::$nativeWipe = true;

            return true;
        } catch (\SodiumException $e) {
            // sodium_compat refusing the call: PHP has no native libsodium and
            // cannot securely wipe memory. Remember it and fall back. Not
            // logged -- it is an expected property of the host, not a fault,
            // and it would otherwise repeat on every key operation forever.
            self::$nativeWipe = false;

            return false;
        }
    }

    /**
     * The best overwrite PHP can perform without libsodium.
     *
     * Zeroing byte-by-byte first is not ceremony: PHP mutates a string in
     * place through offset assignment when nothing else references it, so on
     * that path the original bytes really are overwritten. When the string is
     * shared, PHP separates it first and the loop scrubs only the copy, which
     * is exactly as good as the plain reassignment that follows and no worse.
     * Note that WordPress's bundled sodium_compat throws *without* clearing
     * the variable, so this overwrite is doing real work rather than
     * repeating something the polyfill already did.
     *
     * The value is finally set to null, not to '': that is precisely what the
     * native sodium_memzero() leaves behind, so every call site sees the same
     * post-condition it saw before this helper existed, on every host.
     *
     * @param mixed $value Value to overwrite. Modified in place.
     * @return void
     */
    private static function overwrite(&$value): void
    {
        if (is_string($value)) {
            $length = strlen($value);
            for ($i = 0; $i < $length; $i++) {
                $value[$i] = "\0";
            }
        }

        $value = null;
    }

    /**
     * Whether this platform can perform a real sodium_memzero().
     *
     * Nothing in the plugin surfaces this today, by choice: after this class
     * exists the missing extension no longer costs the operator anything they
     * can act on. It is public, cheap and side-effect-free so that surfacing
     * it later -- on the admin screen, or in the info command's payload -- is
     * one call rather than a second copy of the detection logic. Probing here
     * uses a throwaway string so asking never touches a caller's value.
     *
     * @return bool
     */
    public static function hasNativeWipe(): bool
    {
        if (self::$nativeWipe === null) {
            $probe = 'wpmgr-probe';
            self::supportsNativeWipe($probe);
        }

        return (bool) self::$nativeWipe;
    }

    /**
     * Discard the cached capability answer.
     *
     * Exists for tests, which need to re-probe after simulating a host
     * without native libsodium. Harmless in production: the next wipe simply
     * probes again.
     *
     * @return void
     */
    public static function resetCapabilityCache(): void
    {
        self::$nativeWipe = null;
    }
}
