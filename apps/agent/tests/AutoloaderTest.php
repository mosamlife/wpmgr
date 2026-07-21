<?php
/**
 * Tests for the pure WPMgr\Agent\* class-name resolver (GH #262).
 *
 * The plugin's own spl_autoload_register() closure previously assumed a
 * purely mechanical StudlyCase -> kebab-case mapping that could not resolve
 * 19 of the plugin's own symbols. That bug was masked whenever Composer's
 * vendor/ directory was present (its classmap autoloader resolves everything
 * and runs first), so it only surfaced as a fatal on installs without
 * `composer install` -- a git clone or a GitHub source ZIP.
 *
 * These tests exercise Autoloader::resolve() directly. It is a pure
 * function -- no require/include, no other side effects, and no dependency
 * on Composer -- so it is meaningful to test in isolation even though this
 * PHPUnit process itself loads Composer's autoloader via tests/bootstrap.php.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

// Required directly rather than relying on Composer's classmap: the point of
// this suite is to prove the resolver stands on its own, so the class under
// test is loaded from exactly the one file it ships in.
require_once dirname(__DIR__) . '/includes/class-autoloader.php';

use WPMgr\Agent\Autoloader;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Autoloader
 */
final class AutoloaderTest extends TestCase
{
    /**
     * The 19 symbols the previous mechanical kebab-caser could not resolve,
     * grouped by the root cause described in GH #262:
     *   (A) interfaces that drop the "Interface" suffix in their filename;
     *   (B) the ObjectCache namespace segment vs. the "object-cache" directory;
     *   (C) integration brand filenames with internal dashes dropped;
     *   (D) digit/casing edge cases.
     *
     * @return array<string,string> FQCN => expected file path relative to the plugin root.
     */
    private static function formerlyBroken19(): array
    {
        return [
            // (A) interface files that drop the trailing "Interface".
            'WPMgr\\Agent\\Email\\EmailKeystoreInterface'          => 'includes/email/interface-email-keystore.php',
            'WPMgr\\Agent\\Email\\ProviderHandlerInterface'        => 'includes/email/interface-provider-handler.php',
            'WPMgr\\Agent\\Email\\SuppressionCheckerInterface'     => 'includes/email/interface-suppression-checker.php',
            'WPMgr\\Agent\\Optimizer\\FontTranscodeClientInterface' => 'includes/optimizer/interface-font-transcode-client.php',
            // (B) sub-directory not kebab-cased (ObjectCache -> object-cache).
            'WPMgr\\Agent\\ObjectCache\\ObjectCacheConfig'         => 'includes/object-cache/class-object-cache-config.php',
            'WPMgr\\Agent\\ObjectCache\\ObjectCacheDropinInstaller' => 'includes/object-cache/class-object-cache-dropin-installer.php',
            'WPMgr\\Agent\\ObjectCache\\ObjectCacheHeartbeat'      => 'includes/object-cache/class-object-cache-heartbeat.php',
            'WPMgr\\Agent\\ObjectCache\\RedisConnection'           => 'includes/object-cache/class-redis-connection.php',
            // (C) integration brand files that dropped internal dashes.
            'WPMgr\\Agent\\Integrations\\CloudPanel'               => 'includes/integrations/class-cloudpanel.php',
            'WPMgr\\Agent\\Integrations\\GridPane'                 => 'includes/integrations/class-gridpane.php',
            'WPMgr\\Agent\\Integrations\\RocketNet'                => 'includes/integrations/class-rocketnet.php',
            'WPMgr\\Agent\\Integrations\\RunCloud'                 => 'includes/integrations/class-runcloud.php',
            'WPMgr\\Agent\\Integrations\\SiteGround'               => 'includes/integrations/class-siteground.php',
            'WPMgr\\Agent\\Integrations\\SpinupWP'                 => 'includes/integrations/class-spinupwp.php',
            'WPMgr\\Agent\\Integrations\\WPCloud'                  => 'includes/integrations/class-wpcloud.php',
            'WPMgr\\Agent\\Integrations\\WPEngine'                 => 'includes/integrations/class-wpengine.php',
            // (D) digit/casing edge cases.
            'WPMgr\\Agent\\Optimizer\\IFrame'                      => 'includes/optimizer/class-iframe.php',
            'WPMgr\\Agent\\Security\\Site2faModule'                => 'includes/security/class-site-2fa-module.php',
            'WPMgr\\Agent\\Support\\UpdateInFlight'                => 'includes/support/class-update-inflight.php',
        ];
    }

    /**
     * A representative sample of the 212 symbols that already resolved
     * correctly before this fix, including the trickiest-but-correct cases:
     * ones whose kebab slug happens to embed dashes at every capital
     * (CpUrlProvider, SiteTwoFactorProvider) and the one interface that
     * legitimately KEEPS its full "-interface" suffix in its filename
     * (CommandInterface), which must not regress when the new
     * interface-suffix-stripped candidate is added.
     *
     * @return array<string,string> FQCN => expected file path relative to the plugin root.
     */
    private static function sampleOf212(): array
    {
        return [
            'WPMgr\\Agent\\Commands\\CommandInterface'             => 'includes/commands/interface-command-interface.php',
            'WPMgr\\Agent\\Security\\CpUrlProvider'                => 'includes/security/interface-cp-url-provider.php',
            'WPMgr\\Agent\\Security\\RequestSigner'                => 'includes/security/interface-request-signer.php',
            'WPMgr\\Agent\\Security\\SiteTwoFactorProvider'        => 'includes/security/interface-site-two-factor-provider.php',
            'WPMgr\\Agent\\Email\\Handlers\\SesHandler'            => 'includes/email/handlers/class-ses-handler.php',
            'WPMgr\\Agent\\Email\\Handlers\\SmtpHandler'           => 'includes/email/handlers/class-smtp-handler.php',
            'WPMgr\\Agent\\Security\\WafCidrGuard'                 => 'includes/security/class-waf-cidr-guard.php',
            'WPMgr\\Agent\\Security\\TotpProvider'                 => 'includes/security/class-totp-provider.php',
            'WPMgr\\Agent\\Security\\QrEncoder'                    => 'includes/security/class-qr-encoder.php',
            'WPMgr\\Agent\\Support\\Blake3'                        => 'includes/support/class-blake3.php',
            'WPMgr\\Agent\\Support\\AgeCrypto'                     => 'includes/support/class-age-crypto.php',
            'WPMgr\\Agent\\Support\\IpUtils'                       => 'includes/support/class-ip-utils.php',
            'WPMgr\\Agent\\Keystore'                               => 'includes/class-keystore.php',
            'WPMgr\\Agent\\Plugin'                                 => 'includes/class-plugin.php',
        ];
    }

    /**
     * @return array<string,array{0:string,1:string}> Data-provider rows: [fqcn, expectedRelativePath].
     */
    public static function provideFormerlyBroken19(): array
    {
        $rows = [];
        foreach (self::formerlyBroken19() as $fqcn => $relPath) {
            $rows[$fqcn] = [$fqcn, $relPath];
        }
        return $rows;
    }

    /**
     * @return array<string,array{0:string,1:string}> Data-provider rows: [fqcn, expectedRelativePath].
     */
    public static function provideSampleOf212(): array
    {
        $rows = [];
        foreach (self::sampleOf212() as $fqcn => $relPath) {
            if ($relPath === null) {
                continue;
            }
            $rows[$fqcn] = [$fqcn, $relPath];
        }
        return $rows;
    }

    /**
     * All 19 formerly-unresolvable symbols now resolve to the CORRECT
     * existing file, and that file genuinely declares the expected
     * class/interface (guards against a resolver that returns a plausible
     * but wrong path).
     *
     * @dataProvider provideFormerlyBroken19
     * @param string $fqcn            Fully-qualified class name.
     * @param string $expectedRelPath Expected file path relative to the plugin root.
     */
    public function test_resolves_all_19_formerly_broken_symbols(string $fqcn, string $expectedRelPath): void
    {
        $resolved = Autoloader::resolve($fqcn);

        $this->assertNotNull($resolved, "resolve() returned null for $fqcn");
        $this->assertFileExists($resolved);
        $this->assertSame(
            realpath($this->agentRoot() . '/' . $expectedRelPath),
            realpath($resolved),
            "resolve($fqcn) returned an unexpected file"
        );
        $this->assertFileDeclaresSymbol($resolved, $fqcn);
    }

    /**
     * A representative sample of the 212 previously-working symbols,
     * including the trickiest-but-correct cases, still resolve correctly
     * (no regression from adding the interface-suffix fast path or the
     * scan fallback).
     *
     * @dataProvider provideSampleOf212
     * @param string $fqcn            Fully-qualified class name.
     * @param string $expectedRelPath Expected file path relative to the plugin root.
     */
    public function test_resolves_sample_of_212_previously_working_symbols(string $fqcn, string $expectedRelPath): void
    {
        $resolved = Autoloader::resolve($fqcn);

        $this->assertNotNull($resolved, "resolve() returned null for $fqcn");
        $this->assertSame(
            realpath($this->agentRoot() . '/' . $expectedRelPath),
            realpath($resolved),
            "resolve($fqcn) returned an unexpected file"
        );
        $this->assertFileDeclaresSymbol($resolved, $fqcn);
    }

    /**
     * CommandInterface must keep resolving via the FULL-slug candidate
     * (interface-command-interface.php), not the new interface-suffix
     * fast path (which would look for the non-existent interface-command.php).
     * A regression here would mean the trimmed-suffix candidate started
     * winning over the exact-slug candidate.
     */
    public function test_command_interface_keeps_its_full_suffix_filename(): void
    {
        $resolved = Autoloader::resolve('WPMgr\\Agent\\Commands\\CommandInterface');

        $this->assertNotNull($resolved);
        $this->assertSame('interface-command-interface.php', basename($resolved));
    }

    /**
     * CpUrlProvider must still resolve to its dash-heavy exact-slug file.
     */
    public function test_cp_url_provider_resolves_to_exact_slug_file(): void
    {
        $resolved = Autoloader::resolve('WPMgr\\Agent\\Security\\CpUrlProvider');

        $this->assertNotNull($resolved);
        $this->assertSame('interface-cp-url-provider.php', basename($resolved));
    }

    /**
     * A bogus symbol inside the plugin's own namespace resolves to null
     * rather than throwing or guessing a nearby file.
     */
    public function test_returns_null_for_bogus_wpmgr_symbol(): void
    {
        $this->assertNull(Autoloader::resolve('WPMgr\\Agent\\Nope\\DoesNotExist'));
    }

    /**
     * A class outside the WPMgr\Agent\ namespace is left alone (returns
     * null), so other registered autoloaders remain free to handle it.
     */
    public function test_returns_null_for_non_wpmgr_class(): void
    {
        $this->assertNull(Autoloader::resolve('Yoast\\PHPUnitPolyfills\\TestCases\\TestCase'));
        $this->assertNull(Autoloader::resolve('DateTime'));
    }

    /**
     * Known finding: includes/optimizer/ contains both
     * class-font-transcode-client.php (the FontTranscodeClient class) and
     * interface-font-transcode-client.php (the FontTranscodeClientInterface
     * interface). Both normalize to the same scan-fallback key
     * ("fonttranscodeclient"), which would be an unresolvable collision IF
     * the scan fallback were ever consulted for either symbol. It never is:
     * both resolve via a fast-path candidate (the exact class-<slug>.php for
     * the class, and the interface-suffix-stripped candidate for the
     * interface), so the dormant collision is harmless in practice. This
     * test pins that behavior AND proves the fallback's collision guard
     * actually refuses to guess when a lookup that can ONLY be satisfied by
     * the scan does hit that same collision.
     */
    public function test_font_transcode_client_collision_is_dormant_but_fallback_still_refuses_to_guess(): void
    {
        $classResolved = Autoloader::resolve('WPMgr\\Agent\\Optimizer\\FontTranscodeClient');
        $this->assertNotNull($classResolved);
        $this->assertSame('class-font-transcode-client.php', basename($classResolved));

        $interfaceResolved = Autoloader::resolve('WPMgr\\Agent\\Optimizer\\FontTranscodeClientInterface');
        $this->assertNotNull($interfaceResolved);
        $this->assertSame('interface-font-transcode-client.php', basename($interfaceResolved));

        // A synthetic name that normalizes to the SAME collided key
        // ("fonttranscodeclient") but cannot hit either fast-path candidate
        // (its exact kebab slug embeds underscores neither real file has,
        // and it does not end in "Interface") must fall through to the scan
        // and come back null rather than guessing between the two files.
        $this->assertNull(Autoloader::resolve('WPMgr\\Agent\\Optimizer\\Font_Transcode_Client'));
    }

    /**
     * Integration-style: with Composer's autoloader temporarily unregistered
     * (leaving the plugin's own Autoloader-backed closure as the ONLY
     * handler for WPMgr\Agent\* symbols, exactly as on an install without
     * vendor/), interface_exists() for the symbol that used to fatal on
     * activation resolves successfully, and the concrete class that
     * implements it loads without a fatal error.
     *
     * Runs in a separate process so it cannot be short-circuited by another
     * test in this suite having already loaded these classes via Composer.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_resolves_email_keystore_interface_with_composer_autoloader_absent(): void
    {
        // Located WHILE Composer is still registered, so this assertion
        // itself runs with the framework's normal autoloading intact.
        $composerLoader = $this->findComposerAutoloader();
        $this->assertNotNull($composerLoader, 'expected to find a registered Composer ClassLoader');

        // Unregistering happens inline here (not via a helper call) and
        // registering the plugin closure immediately follows, so the window
        // in which Composer is absent is as small as possible.
        spl_autoload_unregister($composerLoader);
        spl_autoload_register(static function (string $class): void {
            $file = Autoloader::resolve($class);
            if ($file !== null) {
                require_once $file;
            }
        });

        // Only raw PHP calls happen inside this window -- no PHPUnit
        // assertion/framework code, which itself may need to lazily autoload
        // internal PHPUnit classes that neither our plugin closure nor the
        // (currently unregistered) Composer loader could resolve. Composer
        // is restored BEFORE any assertion runs.
        try {
            $ifaceLoaded = interface_exists('WPMgr\\Agent\\Email\\EmailKeystoreInterface', true);
            $classLoaded = class_exists('WPMgr\\Agent\\Keystore', true);
        } finally {
            spl_autoload_register($composerLoader);
        }

        $this->assertTrue($ifaceLoaded, 'EmailKeystoreInterface should autoload via the plugin resolver alone');
        $this->assertTrue($classLoaded, 'Keystore (which implements EmailKeystoreInterface) should load without a fatal');
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @return string Absolute path to the plugin root (apps/agent).
     */
    private function agentRoot(): string
    {
        return dirname(__DIR__);
    }

    /**
     * Assert that $file textually declares `class $shortName` or
     * `interface $shortName` (the short, unqualified name of $fqcn). This is
     * a content check rather than an actual class-load so the test suite
     * never risks a "cannot redeclare class" fatal from a symbol that some
     * other test in this process has already loaded via Composer.
     *
     * @param string $file Absolute file path.
     * @param string $fqcn Fully-qualified class name.
     */
    private function assertFileDeclaresSymbol(string $file, string $fqcn): void
    {
        $shortName = substr($fqcn, strrpos($fqcn, '\\') !== false ? strrpos($fqcn, '\\') + 1 : 0);
        $source    = (string) file_get_contents($file);

        $this->assertMatchesRegularExpression(
            '/\b(?:class|interface|trait)\s+' . preg_quote($shortName, '/') . '\b/',
            $source,
            "$file does not appear to declare $shortName"
        );
    }

    /**
     * Find (without unregistering) the Composer ClassLoader currently
     * registered via spl_autoload_register().
     *
     * @return callable|null The registered callable, or null if none was found.
     */
    private function findComposerAutoloader(): ?callable
    {
        foreach (spl_autoload_functions() ?: [] as $function) {
            if (is_array($function) && isset($function[0]) && is_object($function[0])
                && $function[0] instanceof \Composer\Autoload\ClassLoader
            ) {
                return $function;
            }
        }

        return null;
    }
}
