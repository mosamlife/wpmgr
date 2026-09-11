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
     * The registry: every command class file on disk, as
     * [short class name => absolute path].
     *
     * A file is a command when it declares a class that implements
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
            $src = file_get_contents($file);
            self::assertIsString($src, 'unreadable command file: ' . $file);

            if (!preg_match('/^(?:final\s+)?(?:abstract\s+)?class\s+(\w+)[^{]*implements\s+CommandInterface/m', $src, $m)) {
                continue;
            }

            $found[$m[1]] = $file;
        }

        return $found;
    }

    /**
     * Which label declarations a command file is missing, read from its SOURCE.
     *
     * Textual on purpose, and used before the class is ever loaded: see the
     * file header. An empty return means the file is safe to require.
     *
     * @param string $file Absolute path to a command class file.
     * @return list<string> Human-readable reasons, empty when both are present.
     */
    private static function missingLabels(string $file): array
    {
        $src     = (string) file_get_contents($file);
        $missing = [];

        if (!preg_match('/function\s+effect\s*\(\s*\)\s*:\s*CommandEffect/', $src)) {
            $missing[] = 'does not declare effect(): CommandEffect';
        }
        if (!preg_match('/function\s+repeatability\s*\(\s*\)\s*:\s*CommandRepeatability/', $src)) {
            $missing[] = 'does not declare repeatability(): CommandRepeatability';
        }

        return $missing;
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
