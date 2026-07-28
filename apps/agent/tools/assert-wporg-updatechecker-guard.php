<?php
/**
 * BUILD-TIME ASSERTION: no staged wp.org file may resolve the self-updater
 * class outside a guard.
 *
 * `make agent-zip-wporg` physically removes
 * includes/support/class-update-checker.php from the distribution build
 * (Guideline 8). Several files that DO survive that rsync still name
 * WPMgr\Agent\Support\UpdateChecker, and that is fine as long as the naming
 * never causes the plugin's autoloader to go looking for the missing file:
 *
 *   ALLOWED anywhere
 *     `use WPMgr\Agent\Support\UpdateChecker;`   an import, never resolved
 *     `?UpdateChecker $x` / `: ?UpdateChecker`   a nullable type declaration,
 *                                               never resolved while the value
 *                                               is null, which it always is in
 *                                               the wp.org build
 *
 *   ALLOWED only inside a guard
 *     `new UpdateChecker(...)`  `UpdateChecker::CONST`  `UpdateChecker::method()`
 *     `instanceof UpdateChecker` and every other executable naming. Each of
 *     these is a hard class fetch that triggers the autoloader on EVERY request
 *     of EVERY wp.org install, producing:
 *       Fatal error: Uncaught Error: Class "WPMgr\Agent\Support\UpdateChecker" not found
 *
 * A guard is an `if` whose condition tests WPMGR_WPORG_BUILD, or tests the
 * updateChecker collaborator against null.
 *
 * WHY A BUILD-TIME ASSERT AND NOT ONLY A TEST
 * -------------------------------------------
 * The risky line sits among roughly a dozen visually identical unconditional
 * add_action() calls, so dedenting it out of the guard is a plausible refactor.
 * `wp plugin check` is static analysis and never executes the plugin, so it
 * cannot catch it. This assert runs against the ARTIFACT THAT ACTUALLY SHIPS,
 * which is the strongest place to check. It mirrors the SELF_PLUGIN_FOLDER
 * assertion the same Makefile target already performs.
 *
 * Usage:
 *   php tools/assert-wporg-updatechecker-guard.php <staged-tree-dir>
 *
 * Exit codes:
 *   0  The staged tree is safe.
 *   1  A violation was found, or the tree does not look like a staged wp.org build.
 *
 * The analyser is also required directly by the PHPUnit companion
 * (tests/AgentSelfUpdateWporgBuildTest.php) so the source tree and the staged
 * artifact are judged by exactly one implementation.
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tools;

/**
 * Tokenizes PHP source and reports every executable naming of the self-updater
 * class that is not wrapped in a guard.
 */
final class WporgUpdateCheckerGuard
{
    /** Short class name of the self-updater. */
    public const CLASS_NAME = 'UpdateChecker';

    /** Path, relative to a plugin root, of the file the wp.org build removes. */
    public const EXCLUDED_FILE = 'includes/support/class-update-checker.php';

    /**
     * Analyse one PHP source string.
     *
     * @param string $source PHP source code.
     * @return array{violations:list<array{line:int,context:string}>,resolving:int,allowed:int}
     *         `resolving` counts executable namings that ARE correctly guarded;
     *         `allowed` counts imports and nullable type declarations.
     */
    public static function analyse(string $source): array
    {
        $tokens = token_get_all($source);
        $count  = count($tokens);

        $depth      = 0;
        $braceStack = [];   // one entry per open brace: true when it is a guard block.
        $guardAt    = -1;   // token index of the `{` that opens a guard block.
        $guardValue = false;

        $violations = [];
        $resolving  = 0;
        $allowed    = 0;

        for ($i = 0; $i < $count; $i++) {
            $token = $tokens[$i];

            // ---- brace bookkeeping -------------------------------------------
            if ($token === '{') {
                $depth++;
                $braceStack[] = ($guardAt === $i) ? $guardValue : false;
                if ($guardAt === $i) {
                    $guardAt    = -1;
                    $guardValue = false;
                }
                continue;
            }
            if (is_array($token) && ($token[0] === T_CURLY_OPEN || $token[0] === T_DOLLAR_OPEN_CURLY_BRACES)) {
                // String interpolation opens a brace that a plain '}' closes;
                // count it so the stack stays balanced.
                $depth++;
                $braceStack[] = false;
                continue;
            }
            if ($token === '}') {
                array_pop($braceStack);
                $depth--;
                continue;
            }

            if (!is_array($token)) {
                continue;
            }

            // ---- remember the condition of an `if` that opens a block --------
            if ($token[0] === T_IF || $token[0] === T_ELSEIF) {
                $open = self::nextSignificant($tokens, $i);
                if ($open === -1 || $tokens[$open] !== '(') {
                    continue;
                }
                $close = self::matchParen($tokens, $open);
                if ($close === -1) {
                    continue;
                }
                $brace = self::nextSignificant($tokens, $close);
                if ($brace === -1 || $tokens[$brace] !== '{') {
                    // A braceless `if` cannot contain anything, so nothing to arm.
                    continue;
                }
                $guardAt    = $brace;
                $guardValue = self::isGuardCondition(self::text($tokens, $open, $close));
                continue;
            }

            // ---- the naming itself -------------------------------------------
            if (!self::namesTheClass($token)) {
                continue;
            }

            $kind = self::classifyNaming($tokens, $i);
            if ($kind !== 'resolving') {
                $allowed++;
                continue;
            }

            if (in_array(true, $braceStack, true)) {
                $resolving++;
                continue;
            }

            $violations[] = [
                'line'    => (int) $token[2],
                'context' => self::lineOf($source, (int) $token[2]),
            ];
        }

        return [
            'violations' => $violations,
            'resolving'  => $resolving,
            'allowed'    => $allowed,
        ];
    }

    /**
     * Does this token name the self-updater class?
     *
     * Comments, doc blocks and quoted strings are different token types, so
     * prose that merely mentions the class and the deliberately string-keyed
     * class name inside the command shell are both invisible here, which is
     * exactly right: an autoloader never sees either.
     *
     * @param array{0:int,1:string,2:int} $token Tokenizer token.
     * @return bool
     */
    private static function namesTheClass(array $token): bool
    {
        $types = [T_STRING];
        if (defined('T_NAME_QUALIFIED')) {
            $types[] = T_NAME_QUALIFIED;
        }
        if (defined('T_NAME_FULLY_QUALIFIED')) {
            $types[] = T_NAME_FULLY_QUALIFIED;
        }
        if (!in_array($token[0], $types, true)) {
            return false;
        }

        $text = $token[1];

        return $text === self::CLASS_NAME || str_ends_with($text, '\\' . self::CLASS_NAME);
    }

    /**
     * Classify one naming as 'import', 'nullable-type' or 'resolving'.
     *
     * Anything that is not provably one of the first two is treated as
     * resolving. The assertion is deliberately pessimistic: a new syntax it
     * does not recognise fails the build rather than shipping the fatal.
     *
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $index  Index of the naming.
     * @return string
     */
    private static function classifyNaming(array $tokens, int $index): string
    {
        $prev = self::prevSignificant($tokens, $index);
        if ($prev === -1) {
            return 'resolving';
        }

        // `?UpdateChecker` in a property, parameter or return type. The class is
        // not fetched while the value is null.
        if ($tokens[$prev] === '?') {
            return 'nullable-type';
        }

        // `use WPMgr\Agent\Support\UpdateChecker;` (also the multi-name form).
        if (self::statementStartsWithUse($tokens, $index)) {
            return 'import';
        }

        return 'resolving';
    }

    /**
     * Walk back to the start of the statement containing $index and report
     * whether its first significant token is `use`.
     *
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $index  Index inside the statement.
     * @return bool
     */
    private static function statementStartsWithUse(array $tokens, int $index): bool
    {
        for ($i = $index - 1; $i >= 0; $i--) {
            $token = $tokens[$i];
            if ($token === ';' || $token === '{' || $token === '}') {
                break;
            }
            if (is_array($token) && $token[0] === T_OPEN_TAG) {
                break;
            }
            if (self::isSignificant($token)) {
                $first = $token;
            }
        }

        return isset($first) && is_array($first) && $first[0] === T_USE;
    }

    /**
     * Is this condition text a wp.org build guard?
     *
     * @param string $condition Raw source of the `if` condition, parens included.
     * @return bool
     */
    private static function isGuardCondition(string $condition): bool
    {
        if (str_contains($condition, 'WPMGR_WPORG_BUILD')) {
            return true;
        }

        return str_contains($condition, 'updateChecker') && str_contains($condition, 'null');
    }

    // -------------------------------------------------------------------------
    // Token helpers
    // -------------------------------------------------------------------------

    /**
     * @param array{0:int,1:string,2:int}|string $token Tokenizer token.
     * @return bool
     */
    private static function isSignificant($token): bool
    {
        if (!is_array($token)) {
            return true;
        }

        return !in_array($token[0], [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true);
    }

    /**
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $from   Index to search after.
     * @return int Index of the next significant token, or -1.
     */
    private static function nextSignificant(array $tokens, int $from): int
    {
        $count = count($tokens);
        for ($i = $from + 1; $i < $count; $i++) {
            if (self::isSignificant($tokens[$i])) {
                return $i;
            }
        }

        return -1;
    }

    /**
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $from   Index to search before.
     * @return int Index of the previous significant token, or -1.
     */
    private static function prevSignificant(array $tokens, int $from): int
    {
        for ($i = $from - 1; $i >= 0; $i--) {
            if (self::isSignificant($tokens[$i])) {
                return $i;
            }
        }

        return -1;
    }

    /**
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $open   Index of the `(`.
     * @return int Index of the matching `)`, or -1.
     */
    private static function matchParen(array $tokens, int $open): int
    {
        $count = count($tokens);
        $level = 0;
        for ($i = $open; $i < $count; $i++) {
            if ($tokens[$i] === '(') {
                $level++;
            } elseif ($tokens[$i] === ')') {
                $level--;
                if ($level === 0) {
                    return $i;
                }
            }
        }

        return -1;
    }

    /**
     * @param list<array{0:int,1:string,2:int}|string> $tokens Full token list.
     * @param int                                      $from   First index (inclusive).
     * @param int                                      $to     Last index (inclusive).
     * @return string Raw source text of the range.
     */
    private static function text(array $tokens, int $from, int $to): string
    {
        $out = '';
        for ($i = $from; $i <= $to; $i++) {
            $out .= is_array($tokens[$i]) ? $tokens[$i][1] : $tokens[$i];
        }

        return $out;
    }

    /**
     * @param string $source PHP source code.
     * @param int    $line   1-based line number.
     * @return string Trimmed source line, for the failure message.
     */
    private static function lineOf(string $source, int $line): string
    {
        $lines = explode("\n", $source);

        return trim($lines[$line - 1] ?? '');
    }
}

// -----------------------------------------------------------------------------
// CLI entry point. Skipped when the file is merely required by the test suite.
// -----------------------------------------------------------------------------

if (PHP_SAPI !== 'cli' || !isset($argv[0]) || realpath($argv[0]) !== realpath(__FILE__)) {
    return;
}

$stagedRoot = $argv[1] ?? '';
if ($stagedRoot === '' || !is_dir($stagedRoot)) {
    fwrite(STDERR, "assert-wporg-updatechecker-guard: usage: php tools/assert-wporg-updatechecker-guard.php <staged-tree-dir>\n");
    exit(1);
}
$stagedRoot = rtrim($stagedRoot, '/');

// Premise 1: the staged tree really is a wp.org build, i.e. the self-updater
// source is gone. Without this the guard assertion below would be meaningless.
if (file_exists($stagedRoot . '/' . WporgUpdateCheckerGuard::EXCLUDED_FILE)) {
    fwrite(
        STDERR,
        "assert-wporg-updatechecker-guard: FAILED. " . WporgUpdateCheckerGuard::EXCLUDED_FILE
        . " is still present in the staged tree, so this is not a wp.org build. Check the rsync excludes in the agent-zip-wporg target.\n"
    );
    exit(1);
}

$pluginFile = $stagedRoot . '/includes/class-plugin.php';
if (!is_file($pluginFile)) {
    fwrite(STDERR, "assert-wporg-updatechecker-guard: FAILED. includes/class-plugin.php is missing from the staged tree.\n");
    exit(1);
}

// Sweep every staged PHP file outside vendor/: any file that survives the rsync
// can introduce the fatal, not just class-plugin.php.
$iterator = new \RecursiveIteratorIterator(
    new \RecursiveDirectoryIterator($stagedRoot, \FilesystemIterator::SKIP_DOTS)
);

$failed         = false;
$scanned        = 0;
$guardedTotal   = 0;
$pluginResolved = 0;

/** @var \SplFileInfo $file */
foreach ($iterator as $file) {
    if (!$file->isFile() || $file->getExtension() !== 'php') {
        continue;
    }

    $path = $file->getPathname();
    if (str_contains($path, '/vendor/') || str_contains($path, '/node_modules/')) {
        continue;
    }

    $source = file_get_contents($path);
    if ($source === false) {
        fwrite(STDERR, "assert-wporg-updatechecker-guard: FAILED. Could not read {$path}.\n");
        exit(1);
    }

    if (!str_contains($source, WporgUpdateCheckerGuard::CLASS_NAME)) {
        $scanned++;
        continue;
    }

    $report = WporgUpdateCheckerGuard::analyse($source);
    $scanned++;
    $guardedTotal += $report['resolving'];

    $relative = ltrim(substr($path, strlen($stagedRoot)), '/');
    if ($relative === 'includes/class-plugin.php') {
        $pluginResolved = $report['resolving'];
    }

    foreach ($report['violations'] as $violation) {
        $failed = true;
        fwrite(
            STDERR,
            "assert-wporg-updatechecker-guard: FAILED. {$relative}:{$violation['line']} resolves the self-updater class"
            . " outside a WPMGR_WPORG_BUILD / updateChecker-not-null guard:\n"
            . "    {$violation['context']}\n"
            . "  The wp.org build ships no " . WporgUpdateCheckerGuard::EXCLUDED_FILE . ", so this line would raise\n"
            . "  Fatal error: Uncaught Error: Class \"WPMgr\\Agent\\Support\\UpdateChecker\" not found\n"
            . "  on every request of every wp.org install. Move it back inside the guard.\n"
        );
    }
}

if ($failed) {
    exit(1);
}

// Premise 2: class-plugin.php must still carry at least one guarded resolving
// reference. If it carries none the assertion above has quietly become
// vacuous, which is itself a regression in the check.
if ($pluginResolved < 1) {
    fwrite(
        STDERR,
        "assert-wporg-updatechecker-guard: FAILED. includes/class-plugin.php names the self-updater nowhere in"
        . " executable code, so this assertion no longer proves anything. If the self-update wiring genuinely moved,"
        . " update this tool along with it.\n"
    );
    exit(1);
}

echo "assert-wporg-updatechecker-guard: OK ({$scanned} staged PHP files scanned, {$guardedTotal} guarded self-updater references, 0 unguarded)\n";
exit(0);
