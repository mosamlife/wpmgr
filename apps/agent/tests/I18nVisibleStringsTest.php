<?php
/**
 * NO VISIBLE ENGLISH LITERAL IN THE LOGIN-RELATED FILES MAY BYPASS GETTEXT (GH #967).
 *
 * The plugin now loads its own catalogue (see TextDomainTest), which only helps
 * a string that goes through __() or one of its relatives. A literal that is
 * shown to a person and is not wrapped stays English for every translator.
 *
 * This is a tripwire, not a proof. It reads the token stream of each guarded
 * file (comments dropped), finds every string literal that is not an argument
 * of a gettext call, and flags the ones that read as prose: two words of
 * letters separated by whitespace, after HTML tags and tag fragments are
 * removed. A literal that is genuinely not shown to a person (a log line, an
 * HTTP header) is listed in ALLOWED with the reason. Everything else is a
 * finding.
 *
 * What it does not see: a visible string assembled from pieces that are each
 * too short to read as prose, or one that arrives through a variable from
 * another file. It is also scoped by method where a file mixes login screens
 * with unrelated code, so adding a visible string to a method outside the
 * scoped list is not covered; widen the list when you add one.
 *
 * It must fail, not pass, when it has nothing to read: every guarded file is
 * required to exist and to contain string literals, and the scanner's own
 * behaviour is pinned by the control tests below (a bare literal is flagged, a
 * wrapped one is not).
 *
 * Out of scope on purpose: includes/security/class-site-2fa-module.php and
 * includes/security/class-qr-encoder.php. Other changes were editing both when
 * this guard was written; add them to GUARDED once their strings are wrapped.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use PHPUnit\Framework\Attributes\DataProvider;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class I18nVisibleStringsTest extends TestCase
{
    /** The WordPress functions whose arguments are translatable text. */
    private const GETTEXT_FUNCTIONS = [
        '__', '_e', 'esc_html__', 'esc_html_e', 'esc_attr__', 'esc_attr_e',
        '_x', '_ex', 'esc_html_x', 'esc_attr_x', '_n', '_nx',
    ];

    /**
     * Guarded files, relative to apps/agent, each with the methods to scan
     * (null scans the whole file).
     *
     * @var array<string,list<string>|null>
     */
    private const GUARDED = [
        'includes/support/class-login-protection.php'     => ['onAuthenticate', 'terminate', 'blockMessage'],
        'includes/security/class-password-policy-module.php' => null,
        'includes/security/class-email-code-provider.php' => null,
        'includes/security/class-backup-codes-provider.php' => null,
        'includes/security/class-totp-provider.php'       => ['label', 'renderForm'],
        'includes/security/class-hide-backend-module.php' => null,
        'includes/security/class-hardening-module.php'    => ['applyRestRestrict', 'applyForceUniqueNickname'],
        'includes/abilities/class-service-principal.php'  => ['refuseAuthenticate', 'refuseAuthenticateUser'],
    ];

    /**
     * Prose-looking literals that are not shown to a person. Keyed by file,
     * each with the reason. A stale entry (one that no longer appears) fails
     * the allowlist test, so this list cannot rot into a blanket exemption.
     *
     * @var array<string,array<string,string>>
     */
    private const ALLOWED = [
        'includes/security/class-password-policy-module.php' => [
            'wpmgr-agent: PasswordPolicyModule::validatePassword caught exception: ' => 'WP_DEBUG-only developer log line',
        ],
    ];

    /**
     * Every string literal in $source that is outside a gettext call and reads
     * as prose.
     *
     * @param string               $source  PHP source.
     * @param list<string>|null    $methods Method names to scan; null scans everything.
     * @return array{findings:list<array{line:int,text:string}>,inspected:int}
     */
    public static function scan(string $source, ?array $methods = null): array
    {
        $toks = [];
        $line = 1;
        foreach (token_get_all($source) as $t) {
            if (is_array($t)) {
                $line = $t[2];
                if ($t[0] === T_WHITESPACE || $t[0] === T_COMMENT || $t[0] === T_DOC_COMMENT) {
                    continue;
                }
                $toks[] = ['id' => $t[0], 'text' => $t[1], 'line' => $line];
            } else {
                $toks[] = ['id' => 0, 'text' => $t, 'line' => $line];
            }
        }

        $count = count($toks);

        // Token indexes inside a gettext call's parentheses are covered.
        $covered = [];
        for ($i = 0; $i < $count - 1; $i++) {
            $name = ltrim($toks[$i]['text'], '\\');
            if ($toks[$i]['id'] === 0 || !in_array($name, self::GETTEXT_FUNCTIONS, true) || $toks[$i + 1]['text'] !== '(') {
                continue;
            }
            $close = self::matching($toks, $i + 1, '(', ')');
            for ($j = $i + 2; $j < $close; $j++) {
                $covered[$j] = true;
            }
        }

        // Token ranges of the scoped methods.
        $ranges = null;
        if ($methods !== null) {
            $ranges = [];
            for ($i = 0; $i < $count - 2; $i++) {
                if ($toks[$i]['id'] !== T_FUNCTION || !in_array($toks[$i + 1]['text'], $methods, true)) {
                    continue;
                }
                $open = $i + 2;
                while ($open < $count && $toks[$open]['text'] !== '{') {
                    $open++;
                }
                if ($open < $count) {
                    $ranges[] = [$open, self::matching($toks, $open, '{', '}')];
                }
            }
        }

        $findings  = [];
        $inspected = 0;
        for ($i = 0; $i < $count; $i++) {
            if ($toks[$i]['id'] !== T_CONSTANT_ENCAPSED_STRING && $toks[$i]['id'] !== T_ENCAPSED_AND_WHITESPACE) {
                continue;
            }
            if ($ranges !== null) {
                $inRange = false;
                foreach ($ranges as [$from, $to]) {
                    if ($i > $from && $i < $to) {
                        $inRange = true;
                        break;
                    }
                }
                if (!$inRange) {
                    continue;
                }
            }
            ++$inspected;
            if (isset($covered[$i])) {
                continue;
            }
            $value = $toks[$i]['text'];
            if ($toks[$i]['id'] === T_CONSTANT_ENCAPSED_STRING) {
                $value = substr($value, 1, -1);
            }
            if (self::readsAsProse($value)) {
                $findings[] = ['line' => $toks[$i]['line'], 'text' => $value];
            }
        }

        return ['findings' => $findings, 'inspected' => $inspected];
    }

    /**
     * Index of the bracket closing the one at $open.
     *
     * @param list<array{id:int,text:string,line:int}> $toks
     */
    private static function matching(array $toks, int $open, string $openCh, string $closeCh): int
    {
        $depth = 0;
        $count = count($toks);
        for ($i = $open; $i < $count; $i++) {
            if ($toks[$i]['text'] === $openCh) {
                ++$depth;
            } elseif ($toks[$i]['text'] === $closeCh) {
                --$depth;
                if ($depth === 0) {
                    return $i;
                }
            }
        }

        return $count - 1;
    }

    /**
     * Two words of letters separated by whitespace, once markup is removed.
     */
    private static function readsAsProse(string $literal): bool
    {
        $text = preg_replace(['/<[^<>]*>/', '/^[^<>]*>/', '/<[^<>]*$/'], '', $literal);
        $text = is_string($text) ? $text : $literal;

        return preg_match('/[A-Za-z]{2,}\s+[A-Za-z]{2,}/', $text) === 1;
    }

    // -------------------------------------------------------------------------
    // The scanner itself, so a guard that finds nothing cannot pass for one that works.
    // -------------------------------------------------------------------------

    public function test_the_scanner_flags_a_bare_prose_literal(): void
    {
        $result = self::scan('<?php return new \WP_Error("wpmgr_x", "Too many failed login attempts.");');
        $this->assertCount(1, $result['findings']);
        $this->assertSame('Too many failed login attempts.', $result['findings'][0]['text']);
    }

    public function test_the_scanner_accepts_a_gettext_wrapped_literal(): void
    {
        $result = self::scan("<?php return new \\WP_Error('wpmgr_x', esc_html__('Too many failed login attempts.', 'wpmgr-agent'));");
        $this->assertSame([], $result['findings']);
        $this->assertGreaterThan(0, $result['inspected'], 'the wrapped literal was still inspected');
    }

    public function test_the_scanner_flags_prose_between_html_tags_and_ignores_markup(): void
    {
        $bare = self::scan("<?php \$h = '<h2>Login Protection</h2>' . '<p style=\"color:#999;\">Reference ID: ' . \$id;");
        $this->assertCount(2, $bare['findings']);

        $markup = self::scan("<?php \$h = '<div style=\"text-align:center;padding:3em;\">' . '\" required autofocus></p>' . '<input type=\"text\" id=\"';");
        $this->assertSame([], $markup['findings'], 'attributes and tag fragments are not prose');
    }

    public function test_the_scanner_honours_the_method_scope(): void
    {
        $src = '<?php final class C { public function shown() { return "Visible words here."; } public function other() { return "Other words here."; } }';
        $this->assertCount(1, self::scan($src, ['shown'])['findings']);
        $this->assertCount(2, self::scan($src)['findings']);
    }

    // -------------------------------------------------------------------------
    // The guarded files.
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string,1:list<string>|null}>
     */
    public static function guardedFiles(): array
    {
        $cases = [];
        foreach (self::GUARDED as $file => $methods) {
            $cases[$file] = [$file, $methods];
        }

        return $cases;
    }

    /**
     * @param list<string>|null $methods
     */
    #[DataProvider('guardedFiles')]
    public function test_no_visible_literal_bypasses_gettext(string $file, ?array $methods): void
    {
        $path   = dirname(__DIR__) . '/' . $file;
        $source = file_get_contents($path);
        $this->assertIsString($source, $file . ' could not be read');
        $this->assertNotSame('', trim($source), $file . ' is empty, so this guard would prove nothing');

        $result = self::scan($source, $methods);
        $this->assertGreaterThan(
            0,
            $result['inspected'],
            $file . ': the scan inspected no string literal at all; the method scope or the scanner is broken'
        );

        $allowed  = self::ALLOWED[$file] ?? [];
        $findings = array_values(array_filter(
            $result['findings'],
            static fn (array $f): bool => !isset($allowed[$f['text']])
        ));

        $report = array_map(static fn (array $f): string => $file . ':' . $f['line'] . '  ' . $f['text'], $findings);
        $this->assertSame(
            [],
            $report,
            "these literals are shown to a person and are not wrapped in a gettext function:\n" . implode("\n", $report)
        );
    }

    /**
     * An ALLOWED entry that no longer appears is dead weight that would hide a
     * future regression of the same text.
     */
    public function test_every_allowlist_entry_still_appears(): void
    {
        foreach (self::ALLOWED as $file => $entries) {
            $this->assertArrayHasKey($file, self::GUARDED, $file . ' has an allowlist but is not guarded');
            $source = (string) file_get_contents(dirname(__DIR__) . '/' . $file);
            $seen   = array_column(self::scan($source, self::GUARDED[$file])['findings'], 'text');
            foreach (array_keys($entries) as $text) {
                $this->assertContains($text, $seen, $file . ': allowlist entry no longer appears: ' . $text);
            }
        }
    }
}
