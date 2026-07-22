<?php
/**
 * ConnectionFinisherAdoptionGuardTest — a static guard for GitHub issue #274:
 * every in-process early-ACK site must go through ConnectionFinisher's
 * SAPI-aware ladder (fastcgi_finish_request -> litespeed_finish_request ->
 * fallback), never a raw `function_exists('fastcgi_finish_request')` guard
 * on its own, which is exactly the pre-#274 bug — it silently drops the
 * LiteSpeed rung. This scans for that specific call-pattern (a
 * function_exists() check naming either finish-request function as a
 * string literal), NOT every narrative mention of the function names in
 * docblocks — those are expected and accurate now that the docs describe
 * the SAPI-aware ladder.
 *
 * The only place allowed to contain that pattern is
 * assets/wpmgr-advanced-cache.php — a WordPress drop-in that runs BEFORE the
 * plugin autoloader (so it cannot use the ConnectionFinisher class) and
 * keeps its OWN inline fastcgi/litespeed/fallback ladder instead.
 * includes/support/class-connection-finisher.php itself uses the injected
 * $available seam, not a literal function_exists() call, so it is not an
 * exception to this guard — it simply doesn't match the pattern.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\ConnectionFinisher
 */
final class ConnectionFinisherAdoptionGuardTest extends TestCase
{
    /** Files allowed to contain a raw function_exists('...finish_request') guard. */
    private const ALLOWED_FILES = [
        'assets/wpmgr-advanced-cache.php',
    ];

    /** The anti-pattern this guard scans for: a raw function_exists() check on either finish-request function. */
    private const ANTI_PATTERN = '/function_exists\s*\(\s*[\'"](?:fastcgi|litespeed)_finish_request[\'"]\s*\)/';

    public function test_no_php_file_reimplements_the_fastcgi_ladder_outside_the_allowed_files(): void
    {
        $root      = dirname(__DIR__);
        $offenders = [];

        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($root, \FilesystemIterator::SKIP_DOTS),
            \RecursiveIteratorIterator::LEAVES_ONLY
        );

        foreach ($iterator as $file) {
            if (!$file instanceof \SplFileInfo || $file->getExtension() !== 'php') {
                continue;
            }

            $relative = ltrim(str_replace($root, '', $file->getPathname()), '/\\');
            $relative = str_replace('\\', '/', $relative);

            // Skip vendor/, tests/, node_modules/, tools/ — only shipped
            // plugin source is in scope for this guard.
            if (
                str_starts_with($relative, 'vendor/')
                || str_starts_with($relative, 'tests/')
                || str_starts_with($relative, 'node_modules/')
                || str_starts_with($relative, 'tools/')
                || str_starts_with($relative, 'release/')
            ) {
                continue;
            }

            if (in_array($relative, self::ALLOWED_FILES, true)) {
                continue;
            }

            $contents = file_get_contents($file->getPathname());
            if ($contents === false) {
                continue;
            }

            if (preg_match(self::ANTI_PATTERN, $contents) === 1) {
                $offenders[] = $relative;
            }
        }

        self::assertSame(
            [],
            $offenders,
            'These files re-implement the raw function_exists(\'fastcgi_finish_request\'/'
            . '\'litespeed_finish_request\') guard directly instead of routing through '
            . 'WPMgr\\Agent\\Support\\ConnectionFinisher: '
            . implode(', ', $offenders)
        );
    }
}
