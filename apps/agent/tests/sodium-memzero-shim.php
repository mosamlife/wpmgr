<?php
/**
 * Test-only shim that lets the suite run as if this machine had no native
 * libsodium PHP extension (GH #709).
 *
 * WHY NOT PATCHWORK
 * -----------------
 * The obvious approach -- listing sodium_memzero in patchwork.json's
 * redefinable-internals -- does not work. Patchwork wraps a redefinable
 * internal in a generated function that forwards its arguments by value, so
 * every call in preprocessed code starts emitting
 *
 *   sodium_memzero(): Argument #1 ($string) must be passed by reference
 *
 * That is a PHP warning on a by-reference builtin, it fires on all 267 tests
 * that reach a wipe, and phpunit.xml.dist sets failOnWarning="true". It also
 * silently breaks the by-reference contract the function exists to provide.
 *
 * WHAT THIS DOES INSTEAD
 * ----------------------
 * PHP resolves an unqualified function call inside a namespace against that
 * namespace first and only then against the global scope. SecureMemory lives
 * in WPMgr\Agent\Support and is the single place in the plugin that calls
 * sodium_memzero(), so defining WPMgr\Agent\Support\sodium_memzero() here
 * shadows exactly that one call site -- by reference, with no instrumentation,
 * and with no effect on any other namespace or on production code, which never
 * loads this file.
 *
 * With SodiumPlatform::$refuses off, the shim simply delegates to the real
 * global function, so the suite behaves exactly as it did before this file
 * existed. It is loaded from bootstrap.php rather than from a test case so the
 * namespaced function is defined before any call site is first executed.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests {

    /**
     * Switchboard for the shim below.
     */
    final class SodiumPlatform
    {
        /**
         * The refusal sodium_compat raises, verbatim, from
         * wp-includes/sodium_compat/src/Compat.php.
         */
        public const REFUSAL_MESSAGE = 'This is not implemented in sodium_compat, as it is not possible to '
            . 'securely wipe memory from PHP. To fix this error, make sure libsodium is installed and the PHP '
            . 'extension is enabled.';

        /**
         * When true, every wipe raises sodium_compat's refusal, exactly as it
         * does on a host with no native libsodium extension.
         */
        public static bool $refuses = false;

        /** Number of times the shim has been entered since the last reset. */
        public static int $calls = 0;

        /** Values the shim was asked to wipe since the last reset. */
        public static array $wiped = [];

        /**
         * Behave like a host with no native libsodium extension.
         *
         * @return void
         */
        public static function refuse(): void
        {
            self::reset();
            self::$refuses = true;
        }

        /**
         * Restore this machine's real behaviour.
         *
         * @return void
         */
        public static function reset(): void
        {
            self::$refuses = false;
            self::$calls   = 0;
            self::$wiped   = [];
        }
    }
}

namespace WPMgr\Agent\Support {

    use WPMgr\Agent\Tests\SodiumPlatform;

    /**
     * Namespaced shadow of the global sodium_memzero().
     *
     * @param string $string Value to wipe, by reference.
     * @return void
     * @throws \SodiumException When simulating a host without native libsodium.
     */
    function sodium_memzero(&$string): void
    {
        SodiumPlatform::$calls++;
        SodiumPlatform::$wiped[] = is_string($string) ? $string : null;

        if (SodiumPlatform::$refuses) {
            throw new \SodiumException(SodiumPlatform::REFUSAL_MESSAGE);
        }

        \sodium_memzero($string);
    }
}
