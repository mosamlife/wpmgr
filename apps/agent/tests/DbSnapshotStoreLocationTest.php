<?php
/**
 * WHERE DATABASE SNAPSHOTS LIVE, AND WHAT THE SOURCE SAYS ABOUT IT.
 *
 * The db_snapshot command keeps its dumps in wpmgr-snapshots/db/ under the
 * uploads directory. wp-content is used only when WordPress reports no uploads
 * path at all (StoragePaths::dataBase() picks uploads whenever `basedir` is a
 * non-empty string). An uploads directory that exists but cannot be written is
 * NOT replaced by wp-content: the command stops with a "not writable" error.
 *
 * The header comment of the command once said wp-content takes over "where
 * uploads is read-only". Someone chasing a read-only uploads directory would then
 * have looked for snapshots in the wrong place.
 *
 * What this file does:
 *
 *   - Runs the real code with an uploads directory that cannot be written, and
 *     with an uploads path that does not exist, to establish what happens.
 *   - Reads the comments of the two files that describe that choice and refuses a
 *     sentence that ties wp-content to a read-only or unwritable uploads
 *     directory. Splitting such a sentence in two is the fix when the wording is
 *     honest, because a sentence that mentions both cannot be told apart from the
 *     claim.
 *
 * It must fail, not pass, when it has nothing to read: a missing or empty file is
 * a loud failure, and the real-file test demands that the comments it scanned
 * mention the snapshot directory at all.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\AssertionFailedError;
use PHPUnit\Framework\Attributes\DataProvider;
use WPMgr\Agent\Commands\DbSnapshotCommand;
use WPMgr\Agent\Support\StoragePaths;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class DbSnapshotStoreLocationTest extends TestCase
{
    /** wp-content named in a sentence... */
    private const WP_CONTENT = '/wp-content/i';

    /** ...next to the uploads directory being read-only or unwritable. */
    private const UPLOADS_CANNOT_BE_WRITTEN = '/\bread[\s-]*only\b|\bnot\s+writable\b|\bunwritable\b|\bnon-writable\b|\bcannot\s+be\s+written\b/i';

    /** @var list<string> Directories this test created, removed in tear_down. */
    private array $created = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        foreach (array_reverse($this->created) as $dir) {
            if (is_dir($dir)) {
                @rmdir($dir);
            }
        }
        $this->created = [];
        Monkey\tearDown();
        parent::tear_down();
    }

    private static function commandSource(): string
    {
        return dirname(__DIR__) . '/includes/commands/class-db-snapshot-command.php';
    }

    private static function storagePathsSource(): string
    {
        return dirname(__DIR__) . '/includes/support/class-storage-paths.php';
    }

    /**
     * Every sentence of every comment in a PHP file, comment markers removed and
     * line breaks collapsed, so a claim wrapped across two lines is one sentence.
     * Consecutive `//` lines are one comment.
     *
     * @return list<string>
     */
    private static function commentSentences(string $path): array
    {
        self::assertFileExists($path, 'The source file this guard reads must exist.');
        $source = file_get_contents($path);
        self::assertIsString($source, 'The source file could not be read.');
        self::assertNotSame('', trim($source), 'The source file is empty, so this guard would prove nothing.');

        $blocks  = [];
        $current = '';
        $flush   = static function () use (&$blocks, &$current): void {
            if (trim($current) !== '') {
                $blocks[] = $current;
            }
            $current = '';
        };

        foreach (token_get_all($source) as $token) {
            if (!is_array($token)) {
                $flush();
                continue;
            }
            [$id, $text] = $token;
            if ($id === T_DOC_COMMENT) {
                $flush();
                $blocks[] = self::stripMarkers($text);
                continue;
            }
            if ($id === T_COMMENT) {
                $current .= ' ' . self::stripMarkers($text);
                continue;
            }
            // A single line break between two `//` lines keeps them one comment.
            if ($id === T_WHITESPACE && $current !== '' && substr_count($text, "\n") <= 1) {
                continue;
            }
            $flush();
        }
        $flush();

        $sentences = [];
        foreach ($blocks as $block) {
            $collapsed = trim((string) preg_replace('/\s+/', ' ', $block));
            $parts     = preg_split('/(?<=[.!?])\s+/', $collapsed);
            self::assertIsArray($parts, 'A comment could not be split into sentences.');
            foreach ($parts as $part) {
                if ($part !== '') {
                    $sentences[] = $part;
                }
            }
        }

        return $sentences;
    }

    private static function stripMarkers(string $comment): string
    {
        $text = (string) preg_replace('~^\s*(?:/\*+|//|#)~', '', $comment);
        $text = (string) preg_replace('~\*+/\s*$~', '', $text);

        return (string) preg_replace('~^\s*\*+ ?~m', '', $text);
    }

    /**
     * @param list<string> $sentences
     * @return list<string> The sentences that tie wp-content to an uploads directory that cannot be written.
     */
    private static function fallbackClaims(array $sentences): array
    {
        $claims = [];
        foreach ($sentences as $sentence) {
            if (preg_match(self::WP_CONTENT, $sentence) === 1
                && preg_match(self::UPLOADS_CANNOT_BE_WRITTEN, $sentence) === 1
            ) {
                $claims[] = $sentence;
            }
        }

        return $claims;
    }

    // ---------------------------------------------------------------------
    // What the code does.
    // ---------------------------------------------------------------------

    public function testAnUploadsDirectoryThatCannotBeWrittenIsReportedNotReplacedByWpContent(): void
    {
        $uploads = sys_get_temp_dir() . '/wpmgr-snapshot-location-' . bin2hex(random_bytes(6));
        $this->assertTrue(mkdir($uploads, 0755, true));
        $this->created[] = $uploads;
        $this->created[] = $uploads . '/wpmgr-snapshots';
        $this->created[] = $uploads . '/wpmgr-snapshots/db';

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploads]);
        // Stand in for a host whose uploads directory is read-only. A chmod would
        // prove nothing when the suite runs as root.
        Functions\when('is_writable')->justReturn(false);

        $result = (new DbSnapshotCommand())->execute([], ['action' => 'create']);

        $this->assertFalse($result['ok']);
        $detail = (string) ($result['detail'] ?? '');
        $this->assertStringContainsString('snapshots directory is not writable', $detail);
        $this->assertStringContainsString(
            $uploads . '/wpmgr-snapshots/db',
            $detail,
            'The command must name the uploads location it refused, not another one.'
        );
    }

    public function testTheUploadsPathIsChosenWheneverWordPressReportsOneWhetherOrNotItExists(): void
    {
        $uploads = sys_get_temp_dir() . '/wpmgr-snapshot-location-absent-' . bin2hex(random_bytes(6));
        $this->assertDirectoryDoesNotExist($uploads);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploads]);

        $this->assertSame($uploads . '/wpmgr-snapshots', StoragePaths::dataBase('snapshots'));
    }

    // ---------------------------------------------------------------------
    // What the source says about it.
    // ---------------------------------------------------------------------

    public function testNoCommentTiesWpContentToAnUploadsDirectoryThatCannotBeWritten(): void
    {
        $claims = [];
        foreach ([self::commandSource(), self::storagePathsSource()] as $path) {
            $sentences = self::commentSentences($path);

            $this->assertNotSame(
                [],
                array_filter($sentences, static fn (string $s): bool => stripos($s, 'wp-content') !== false),
                basename($path) . ' has no comment that mentions wp-content, so this scan read the wrong text.'
            );

            foreach (self::fallbackClaims($sentences) as $claim) {
                $claims[] = basename($path) . ': ' . $claim;
            }
        }

        $this->assertSame(
            [],
            $claims,
            "A comment says wp-content takes over when the uploads directory cannot be written, but it does not:\n"
            . "StoragePaths::dataBase() returns the uploads path whenever WordPress reports one, and DbSnapshotCommand\n"
            . "stops with 'snapshots directory is not writable'. wp-content is used only when no uploads path is\n"
            . "available. Sentences that claim otherwise:\n  - " . implode("\n  - ", $claims)
        );
    }

    public function testTheCommandHeaderNamesWhereSnapshotsLive(): void
    {
        $sentences = self::commentSentences(self::commandSource());
        $where     = array_values(array_filter(
            $sentences,
            static fn (string $s): bool => stripos($s, 'wpmgr-snapshots/db') !== false
        ));

        $this->assertNotSame([], $where, 'The header no longer says where snapshots live.');
        $this->assertStringContainsStringIgnoringCase(
            'uploads',
            implode(' ', $where),
            'The header must say snapshots live under the uploads directory.'
        );
    }

    public function testAMissingSourceFileFailsInsteadOfPassing(): void
    {
        $this->expectException(AssertionFailedError::class);

        self::commentSentences(__DIR__ . '/no-such-directory/class-missing.php');
    }

    // ---------------------------------------------------------------------
    // The detector fires on the claims that shipped, and not on honest copy.
    // ---------------------------------------------------------------------

    /**
     * @return array<string, array{string}>
     */
    public static function claimsThatMustBeFound(): array
    {
        return [
            'the header sentence' => [
                'The dump SQL lives in wpmgr-snapshots/db/ under the uploads directory (under wp-content/ on hosts where uploads is read-only), on the site\'s own disk, unencrypted and never uploaded to object storage.',
            ],
            'the inline comment' => ['Falls back to the legacy wp-content location for read-only-uploads hosts.'],
            'the helper comment' => [
                'Fallback: wp-content (for hosts where uploads is not yet configured or is read-only at storage-path resolution time).',
            ],
            'unwritable' => ['Snapshots fall back to wp-content if uploads is unwritable.'],
            'not writable' => ['wp-content takes over when the uploads folder is not writable.'],
            'read only without a hyphen' => ['If uploads is read only we use wp-content.'],
        ];
    }

    #[DataProvider('claimsThatMustBeFound')]
    public function testTheDetectorFiresOnAClaimThatWpContentReplacesUnwritableUploads(string $sentence): void
    {
        $this->assertCount(
            1,
            self::fallbackClaims([$sentence]),
            'The detector missed a sentence that ties wp-content to an unwritable uploads directory: ' . $sentence
        );
    }

    /**
     * @return array<string, array{string}>
     */
    public static function honestCopyThatMustPass(): array
    {
        return [
            'the corrected header sentence' => [
                'The dump SQL lives in wpmgr-snapshots/db/ under the uploads directory (under wp-content/ only when no uploads path is available), on the site\'s own disk, unencrypted and never uploaded to object storage.',
            ],
            'the unwritable case on its own' => [
                'An uploads directory that exists but cannot be written is not replaced: create fails with a "not writable" error.',
            ],
            'uploads first' => ['Snapshots are stored under uploads/wpmgr-snapshots/db rather than wp-content/.'],
            'wp-content alone' => ['wp-content is used only when no uploads path is available.'],
            'a plain permissions note' => ['The snapshot directory is created with mode 0700 and is not world readable.'],
        ];
    }

    #[DataProvider('honestCopyThatMustPass')]
    public function testTheDetectorDoesNotBlockHonestCopy(string $sentence): void
    {
        $this->assertSame(
            [],
            self::fallbackClaims([$sentence]),
            'The detector refused a sentence that is true of the shipped code.'
        );
    }

    public function testCommentSentencesJoinsAClaimWrappedAcrossTwoLines(): void
    {
        $file = tempnam(sys_get_temp_dir(), 'wpmgr-comment-');
        $this->assertIsString($file);
        try {
            file_put_contents($file, "<?php\n/**\n * Snapshots live under uploads (under wp-content/ on hosts\n * where uploads is read-only), on disk.\n */\n// Falls back to wp-content\n// when uploads is not writable.\n\$x = 1;\n");
            $claims = self::fallbackClaims(self::commentSentences($file));
        } finally {
            @unlink($file);
        }

        $this->assertCount(2, $claims, 'A claim wrapped across lines must be found as one sentence.');
    }
}
