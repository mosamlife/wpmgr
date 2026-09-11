<?php
/**
 * Every command class declares both effect labels, and declares them itself.
 *
 * This test is driven off the command REGISTRY -- the set of command class
 * files on disk -- and never off a list written here. A list would pass
 * forever while a newly added command sat unlabelled next to it, which is the
 * exact failure this is built to catch.
 *
 * Why the source text is checked before the class is loaded: a command that
 * omits an interface method is a PHP COMPILE-time fatal ("contains 2 abstract
 * methods and must therefore be declared abstract"), and a fatal inside a test
 * run is not a test failure -- it takes the whole process down with an opaque
 * message. Reading the file first turns that into a named, readable assertion
 * failure that says which command and which method. Only a file that passes
 * the textual check is then loaded and actually called, so the declared value
 * is verified too and not merely the presence of a method.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandInterface;
use WPMgr\Agent\Commands\CommandRepeatability;

/**
 * Registry-driven completeness check for effect() / repeatability().
 */
final class CommandEffectLabelTest extends TestCase
{
    /** Absolute path to the command class directory. */
    private const COMMAND_DIR = __DIR__ . '/../includes/commands';

    /**
     * Per-file token analysis, keyed by absolute path. Populated once per file
     * by analyzeCommandFile() and shared by registry() and missingLabels() so
     * a file is tokenized only once per test run.
     *
     * @var array<string,array{class:?string,implements:bool,has_effect:bool,has_repeatability:bool}>
     */
    private static array $analysisCache = [];

    /**
     * The registry: every command class file on disk, as
     * [short class name => absolute path].
     *
     * A file is a command when it declares a TOP-LEVEL class that implements
     * CommandInterface. Helper classes in the same directory (FileGuards) and
     * the enums themselves are excluded by that test, not by name.
     *
     * @return array<string,string>
     */
    private function registry(): array
    {
        $files = glob(self::COMMAND_DIR . '/class-*.php');
        self::assertIsArray($files, 'glob() over the command directory failed');

        $found = [];
        foreach ($files as $file) {
            $analysis = self::analyzeCommandFile($file);

            if ($analysis['class'] === null || !$analysis['implements']) {
                continue;
            }

            $found[$analysis['class']] = $file;
        }

        return $found;
    }

    /**
     * Which label declarations a command file is missing, read from its SOURCE.
     *
     * Driven by analyzeCommandFile(), and used before the class is ever
     * loaded: see the file header. An empty return means the file is safe to
     * require.
     *
     * @param string $file Absolute path to a command class file.
     * @return list<string> Human-readable reasons, empty when both are present.
     */
    private static function missingLabels(string $file): array
    {
        $analysis = self::analyzeCommandFile($file);
        $missing  = [];

        if (!$analysis['has_effect']) {
            $missing[] = 'does not declare effect(): CommandEffect';
        }
        if (!$analysis['has_repeatability']) {
            $missing[] = 'does not declare repeatability(): CommandRepeatability';
        }

        return $missing;
    }

    /**
     * Tokenizes a command file and reports its top-level class (if any),
     * whether that class implements CommandInterface, and whether it
     * DECLARES -- as real methods on its own class body, not merely
     * discussed in a comment or a string -- effect() and repeatability().
     *
     * Comments and string literals are skipped outright (T_COMMENT,
     * T_DOC_COMMENT, and string-literal tokens carry no structural meaning
     * here), so a docblock that happens to mention the phrase
     * "function effect(): CommandEffect" in prose cannot satisfy this check;
     * only an actual T_FUNCTION token, at the brace depth of the top-level
     * class body, whose name token is "effect" or "repeatability", can.
     *
     * "Top-level class body depth" is tracked explicitly so a nested or
     * anonymous class declared inside a method body -- `new class { ... }`,
     * or even a named `class Foo { ... }` written inside a function, which
     * PHP allows -- is never mistaken for the command's own class: its
     * braces open one level deeper than the top-level class's own body, and
     * `new class` is recognised by the immediately preceding T_NEW token and
     * skipped before it can be mistaken for the top-level declaration.
     *
     * Uses TOKEN_PARSE so `Foo::class` (the class-constant syntax used by
     * several commands) tokenizes as T_STRING, not as the keyword T_CLASS --
     * without that flag every `::class` reference in the file would look
     * like another class declaration.
     *
     * @param string $file Absolute path to a command class file.
     * @return array{class:?string,implements:bool,has_effect:bool,has_repeatability:bool}
     */
    private static function analyzeCommandFile(string $file): array
    {
        if (isset(self::$analysisCache[$file])) {
            return self::$analysisCache[$file];
        }

        $src    = (string) file_get_contents($file);
        $tokens = token_get_all($src, TOKEN_PARSE);
        $count  = count($tokens);

        $braceDepth           = 0;
        $classBodyDepth       = null;
        $inClassHeader        = false;
        $sawTopLevelClass     = false;
        $collectingImplements = false;
        $className            = null;
        $implementsCommand    = false;
        $prevSignificant      = null;
        $hasEffect            = false;
        $hasRepeatability     = false;

        for ($i = 0; $i < $count; $i++) {
            $tok = $tokens[$i];

            if (is_array($tok)) {
                $id   = $tok[0];
                $text = $tok[1];

                // Ignored entirely: comments, whitespace, and string-literal
                // content. None of these can declare a method or a class.
                if (
                    $id === T_COMMENT
                    || $id === T_DOC_COMMENT
                    || $id === T_WHITESPACE
                    || $id === T_CONSTANT_ENCAPSED_STRING
                    || $id === T_ENCAPSED_AND_WHITESPACE
                ) {
                    $prevSignificant = $id;
                    continue;
                }

                // String interpolation opens a brace pair whose OPEN token is
                // not the plain '{' character -- count it, or later plain '}'
                // tokens closing it would desync braceDepth.
                if ($id === T_CURLY_OPEN || $id === T_DOLLAR_OPEN_CURLY_BRACES) {
                    $braceDepth++;
                    $prevSignificant = $id;
                    continue;
                }

                if ($id === T_CLASS) {
                    if ($prevSignificant === T_NEW) {
                        // `new class ...` -- anonymous, never the top-level class.
                        $prevSignificant = $id;
                        continue;
                    }

                    if (!$sawTopLevelClass && $braceDepth === 0) {
                        $sawTopLevelClass     = true;
                        $inClassHeader        = true;
                        $collectingImplements = false;
                    }

                    $prevSignificant = $id;
                    continue;
                }

                if ($inClassHeader) {
                    if ($id === T_IMPLEMENTS) {
                        $collectingImplements = true;
                    } elseif ($id === T_EXTENDS) {
                        $collectingImplements = false;
                    } elseif (
                        $id === T_STRING
                        || $id === T_NAME_QUALIFIED
                        || $id === T_NAME_FULLY_QUALIFIED
                        || $id === T_NAME_RELATIVE
                    ) {
                        if ($className === null) {
                            $className = $text;
                        } elseif (
                            $collectingImplements
                            && ($text === 'CommandInterface' || str_ends_with($text, '\\CommandInterface'))
                        ) {
                            $implementsCommand = true;
                        }
                    }

                    $prevSignificant = $id;
                    continue;
                }

                // A method declaration only counts directly inside the
                // top-level class's own body -- not inside a nested class,
                // not inside a closure, not inside any other method.
                if ($id === T_FUNCTION && $classBodyDepth !== null && $braceDepth === $classBodyDepth) {
                    $j = $i + 1;
                    while ($j < $count) {
                        $t = $tokens[$j];
                        if (is_array($t) && ($t[0] === T_WHITESPACE || $t[0] === T_COMMENT || $t[0] === T_DOC_COMMENT)) {
                            $j++;
                            continue;
                        }
                        if ($t === '&') {
                            // By-reference return, e.g. `function &effect()`.
                            $j++;
                            continue;
                        }
                        break;
                    }

                    if ($j < $count && is_array($tokens[$j]) && $tokens[$j][0] === T_STRING) {
                        $name = $tokens[$j][1];
                        if ($name === 'effect') {
                            $hasEffect = true;
                        } elseif ($name === 'repeatability') {
                            $hasRepeatability = true;
                        }
                    }
                }

                $prevSignificant = $id;
                continue;
            }

            // Plain punctuation / operator token, e.g. '{', '}', ';', '('.
            $ch = $tok;

            if ($ch === '{') {
                if ($inClassHeader) {
                    $classBodyDepth = $braceDepth + 1;
                    $inClassHeader  = false;
                }
                $braceDepth++;
            } elseif ($ch === '}') {
                $braceDepth--;
            }

            $prevSignificant = $ch;
        }

        return self::$analysisCache[$file] = [
            'class'             => $className,
            'implements'        => $implementsCommand,
            'has_effect'        => $hasEffect,
            'has_repeatability' => $hasRepeatability,
        ];
    }

    /**
     * A guard that finds nothing must go red, never green. If the directory
     * moves, the naming convention changes, or the regex above stops matching,
     * this is what says so instead of the suite quietly verifying an empty set.
     *
     * @return void
     */
    public function testRegistryIsNotEmpty(): void
    {
        $registry = $this->registry();

        self::assertGreaterThan(
            50,
            count($registry),
            'The command registry resolved to ' . count($registry) . ' classes, which cannot be right. '
            . 'Either the discovery in registry() has broken or the commands moved; fix the discovery '
            . 'rather than lowering this number.'
        );
    }

    /**
     * The contract itself still demands both labels.
     *
     * Without this, deleting the two methods from CommandInterface would leave
     * the per-command assertions below passing on today's commands while every
     * future command was free to omit them.
     *
     * @return void
     */
    public function testInterfaceDeclaresBothLabelMethods(): void
    {
        self::assertTrue(
            method_exists(CommandInterface::class, 'effect'),
            'CommandInterface no longer declares effect(); an unlabelled command would now compile.'
        );
        self::assertTrue(
            method_exists(CommandInterface::class, 'repeatability'),
            'CommandInterface no longer declares repeatability(); an unlabelled command would now compile.'
        );
    }

    /**
     * Every command declares both methods IN ITS OWN FILE.
     *
     * "In its own file" is the part that matters. Inheriting a label from a
     * base class or a trait would let a new command arrive carrying a default
     * that nobody chose for it, which is precisely the silent-unlabelled case
     * this whole contract exists to prevent.
     *
     * @return void
     */
    public function testEveryCommandDeclaresItsOwnLabels(): void
    {
        $missing = [];

        foreach ($this->registry() as $class => $file) {
            foreach (self::missingLabels($file) as $reason) {
                $missing[] = $class . ' ' . $reason;
            }
        }

        self::assertSame(
            [],
            $missing,
            "Unlabelled command(s). Every command must answer both questions in its own class -- what it\n"
            . "does to the site, and whether repeating it is safe. There is no default and no base class to\n"
            . "inherit one from; see includes/commands/class-command-effect.php.\n  - "
            . implode("\n  - ", $missing)
        );
    }

    /**
     * Every command actually RETURNS a value from the allowed set.
     *
     * The enum return types make "from the allowed set" true by construction,
     * so what this adds over the textual check is that the method is reachable
     * and answers without throwing. newInstanceWithoutConstructor() is used
     * because these labels are constants of the class, independent of whatever
     * collaborators a given command's constructor demands.
     *
     * @return void
     */
    public function testEveryCommandReturnsAnAllowedLabel(): void
    {
        $checked = 0;

        foreach ($this->registry() as $class => $file) {
            // Requiring an unlabelled command is a compile-time fatal that would
            // kill the whole run. testEveryCommandDeclaresItsOwnLabels() is the
            // test that reports it, by name; this one steps over it so that the
            // failure stays readable instead of becoming a stack trace.
            if (self::missingLabels($file) !== []) {
                continue;
            }

            $fqcn = 'WPMgr\\Agent\\Commands\\' . $class;

            if (!class_exists($fqcn, false)) {
                require_once $file;
            }

            $instance = (new \ReflectionClass($fqcn))->newInstanceWithoutConstructor();
            self::assertInstanceOf(CommandInterface::class, $instance, $class . ' is not a CommandInterface');

            self::assertInstanceOf(
                CommandEffect::class,
                $instance->effect(),
                $class . '::effect() did not return a CommandEffect'
            );
            self::assertInstanceOf(
                CommandRepeatability::class,
                $instance->repeatability(),
                $class . '::repeatability() did not return a CommandRepeatability'
            );

            $checked++;
        }

        self::assertGreaterThan(50, $checked, 'Too few commands were actually invoked: ' . $checked);
    }
}
