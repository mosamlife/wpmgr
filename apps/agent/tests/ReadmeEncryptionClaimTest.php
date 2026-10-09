<?php
/**
 * THE LISTING MAY NOT SAY BACKUPS ARE ENCRYPTED WHILE THE AGENT DOES NOT ENCRYPT THEM.
 *
 * readme.txt is rendered verbatim as the public wordpress.org listing, the one
 * page a site owner reads before installing. The agent uploads backup chunks as
 * written: EncryptAndUpload::ENCRYPT_CHUNKS is a compile-time false, restore
 * reads the same constant, and no filter, option or environment variable turns
 * it on. A listing that says archives are "encrypted before leaving the server"
 * is therefore a false statement about where the owner's data is exposed.
 *
 * What this guard does:
 *
 *   - Reads the listing, drops the Changelog and Upgrade Notice sections (they
 *     are history and may describe what an old release did), and splits the rest
 *     into sentences.
 *   - Flags a sentence that pairs an encryption word with a backup word, or with
 *     "before leaving the server", unless the sentence negates the encryption
 *     ("does not encrypt", "not encrypted", "no client-side encryption",
 *     "unencrypted"). An honest listing must be able to say what is true, so a
 *     negation is allowed; only the positive claim is refused.
 *   - Skips question sentences, because an FAQ heading such as "Are my backups
 *     encrypted?" carries no claim.
 *
 * What it does not do: it is a tripwire for this claim and its close paraphrases,
 * not a proof that no sentence anywhere over-promises. It says nothing about
 * transport security; the agent uploads to whatever address the control plane
 * supplies, so the listing must not promise a scheme either way.
 *
 * It must fail, not pass, when it has nothing to read. A missing, empty or
 * section-less readme.txt is a loud failure, and the real-file test also demands
 * that the text it scanned mentions backups at all.
 *
 * When backup encryption becomes a runtime setting (#725), the constant this
 * test reads is replaced and the test errors on purpose. Rewrite it then to
 * assert that the listing matches the default.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use PHPUnit\Framework\AssertionFailedError;
use PHPUnit\Framework\Attributes\DataProvider;
use WPMgr\Agent\Backup\EncryptAndUpload;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class ReadmeEncryptionClaimTest extends TestCase
{
    /** A sentence has to be about encryption to be a claim about it. */
    private const ENCRYPTION = '/encrypt|ciphertext/i';

    /** ...and about backups. */
    private const BACKUP_NOUNS = '/\b(?:backups?|archives?|chunks?|snapshots?|dumps?)\b/i';

    /** The old claim named no backup noun in its last clause, only where the bytes were protected. */
    private const LEAVES_THE_SERVER = '/\bbefore\s+(?:leaving|leaves?|they\s+leave|it\s+leaves?)\s+(?:the|your|this)\s+(?:server|site)\b/i';

    /**
     * The encryption is denied: "not encrypted", "does not encrypt", "doesn't
     * encrypt", "never encrypts", "no client-side encryption", "without
     * encryption", "unencrypted". Up to two words may sit between the negator
     * and "encrypt", and "but" may not, so "not just compressed but encrypted"
     * is still a positive claim.
     */
    private const NEGATED = '/(?:\b(?:not|never|no|without)|n\'t)\s+(?:(?!but\b)[\w-]+\s+){0,2}encrypt|\bunencrypted\b/i';

    private static function readmePath(): string
    {
        return dirname(__DIR__) . '/readme.txt';
    }

    /**
     * Load a listing, failing loudly when there is nothing meaningful to scan.
     */
    private static function readListing(string $path): string
    {
        self::assertFileExists($path, 'readme.txt is the wordpress.org listing and must exist.');

        $raw = file_get_contents($path);
        self::assertIsString($raw, 'readme.txt could not be read.');
        self::assertNotSame('', trim($raw), 'readme.txt is empty, so this guard would prove nothing.');
        self::assertMatchesRegularExpression(
            '/^==\s*Description\s*==\s*$/mi',
            $raw,
            'readme.txt has no Description section, so it is not the listing this guard is meant to read.'
        );

        return $raw;
    }

    /**
     * The live listing: every section except the Changelog and Upgrade Notice.
     */
    private static function liveListing(string $raw): string
    {
        $parts = preg_split('/^(==\s*[^=\n]+?\s*==)[ \t]*$/m', $raw, -1, PREG_SPLIT_DELIM_CAPTURE);
        self::assertIsArray($parts, 'The listing could not be split into sections.');

        $live = (string) array_shift($parts);
        for ($i = 0; $i + 1 < count($parts); $i += 2) {
            $heading = (string) $parts[$i];
            $body    = (string) $parts[$i + 1];
            if (preg_match('/^==\s*(?:change\s*log|upgrade\s+notice)\s*==$/i', $heading) === 1) {
                continue;
            }
            $live .= $heading . $body;
        }

        return $live;
    }

    /**
     * Whether the agent can produce encrypted backup chunks at all. Read through
     * reflection so that removing or renaming the constant is a loud
     * ReflectionException here, never a guard that quietly stops guarding.
     */
    private static function chunkEncryptionIsOn(): bool
    {
        $value = (new \ReflectionClassConstant(EncryptAndUpload::class, 'ENCRYPT_CHUNKS'))->getValue();

        return $value === true;
    }

    /**
     * @return list<string> The sentences of $text that claim backups are encrypted.
     */
    private static function encryptionClaims(string $text): array
    {
        $units = preg_split('/(?<=[.!?])\s+|\R+/u', $text);
        self::assertIsArray($units, 'The listing could not be split into sentences.');

        $claims = [];
        foreach ($units as $unit) {
            // An FAQ heading is written "= Question? =" and asserts nothing. A list
            // item's own "- " marker is not part of the sentence.
            $unit = trim((string) $unit, " \t=");
            $unit = (string) preg_replace('/^[-*]\s+/', '', $unit);
            if ($unit === '' || str_ends_with($unit, '?')) {
                continue;
            }
            if (preg_match(self::ENCRYPTION, $unit) !== 1) {
                continue;
            }
            $aboutBackups = preg_match(self::BACKUP_NOUNS, $unit) === 1
                || preg_match(self::LEAVES_THE_SERVER, $unit) === 1;
            if (!$aboutBackups) {
                continue;
            }
            if (preg_match(self::NEGATED, $unit) === 1) {
                continue;
            }
            $claims[] = $unit;
        }

        return $claims;
    }

    /**
     * Run $then with the path of a temporary file holding $contents.
     *
     * @param callable(string): void $then
     */
    private function withTemporaryFile(string $contents, callable $then): void
    {
        $path = tempnam(sys_get_temp_dir(), 'wpmgr-readme-');
        $this->assertIsString($path, 'Could not create a temporary file.');
        try {
            file_put_contents($path, $contents);
            $then($path);
        } finally {
            @unlink($path);
        }
    }

    // ---------------------------------------------------------------------
    // The guard, against the real listing.
    // ---------------------------------------------------------------------

    public function testTheListingDoesNotClaimBackupsAreEncrypted(): void
    {
        if (self::chunkEncryptionIsOn()) {
            $this->markTestSkipped('Chunk encryption is on, so the listing may describe it.');
        }

        $live = self::liveListing(self::readListing(self::readmePath()));

        // The scan has to have read the backup text, or a green result means nothing.
        $this->assertStringContainsStringIgnoringCase(
            'backup',
            $live,
            'The live listing mentions no backups at all, so this scan read the wrong text.'
        );

        $claims = self::encryptionClaims($live);

        $this->assertSame(
            [],
            $claims,
            "The wordpress.org listing (apps/agent/readme.txt) says backups are encrypted, but the agent does not\n"
            . "encrypt them (EncryptAndUpload::ENCRYPT_CHUNKS is false). Sentences that claim it:\n  - "
            . implode("\n  - ", $claims)
            . "\nSay what is true instead: the plugin does not encrypt archives, so protection at rest comes from the\n"
            . 'storage destination. A sentence may mention encryption next to backups only to deny it.'
        );
    }

    // ---------------------------------------------------------------------
    // The guard goes red, not green, when it has nothing to read.
    // ---------------------------------------------------------------------

    public function testAMissingReadmeFailsInsteadOfPassing(): void
    {
        $this->expectException(AssertionFailedError::class);

        self::readListing(__DIR__ . '/no-such-directory/readme.txt');
    }

    public function testAnEmptyReadmeFailsInsteadOfPassing(): void
    {
        $this->expectException(AssertionFailedError::class);

        $this->withTemporaryFile('', static function (string $path): void {
            self::readListing($path);
        });
    }

    public function testAWhitespaceOnlyReadmeFailsInsteadOfPassing(): void
    {
        $this->expectException(AssertionFailedError::class);

        $this->withTemporaryFile("  \n\t\n", static function (string $path): void {
            self::readListing($path);
        });
    }

    public function testAFileWithNoDescriptionSectionFailsInsteadOfPassing(): void
    {
        $this->expectException(AssertionFailedError::class);

        $this->withTemporaryFile("Some other file that is not a listing.\n", static function (string $path): void {
            self::readListing($path);
        });
    }

    // ---------------------------------------------------------------------
    // The detector fires on the claims that shipped.
    // ---------------------------------------------------------------------

    /**
     * @return array<string, array{string}>
     */
    public static function claimsThatMustBeFound(): array
    {
        return [
            // The four claims the 0.61.x listing carried, verbatim.
            'description paragraph' => [
                'Archives are encrypted on the site before upload, and an incremental run uses a content-addressed chunk store so only changed blocks move.',
            ],
            'privacy bullet, heading and verb' => [
                '- Backup archives (encrypted): when you run or schedule a backup, the agent archives your database and/or files, encrypts the archive, and uploads it to the storage destination your control plane configured.',
            ],
            'privacy bullet, closing clause' => [
                'Archive contents may include your site\'s content and personal data, and are encrypted before leaving the server.',
            ],
            'external services, control plane' => [
                'What is sent: site URL and name, WordPress and PHP versions, active plugin and theme inventory, Site Health results, rendered HTML of selected pages (for used-CSS computation), encrypted backup archives, transcoded font bytes, and cache and performance statistics.',
            ],
            'external services, object storage' => [
                'What is sent: encrypted backup archives, restored backup chunks, optimized media files and transcoded font bytes, over short-lived presigned URLs the control plane supplies.',
            ],
            // Paraphrases the same mistake would come back as.
            'plain restatement' => ['Your backups are always encrypted.'],
            'named cipher' => ['Every backup is encrypted with age before upload.'],
            'no backup noun, only where' => ['Everything is encrypted before it leaves your server.'],
            'end to end' => ['Backups use end-to-end encryption.'],
            'chunks' => ['Chunks are age-encrypted.'],
            'a not that does not deny it' => ['Archives are encrypted, not just compressed.'],
            'a but after the not' => ['Backups are not just compressed but encrypted.'],
        ];
    }

    #[DataProvider('claimsThatMustBeFound')]
    public function testTheDetectorFiresOnAClaimThatBackupsAreEncrypted(string $sentence): void
    {
        // Each case is one sentence, so exactly one claim must come back.
        $this->assertCount(
            1,
            self::encryptionClaims($sentence),
            'The detector missed a sentence that claims backups are encrypted: ' . $sentence
        );
    }

    // ---------------------------------------------------------------------
    // ...and does not block honest copy.
    // ---------------------------------------------------------------------

    /**
     * @return array<string, array{string}>
     */
    public static function honestCopyThatMustPass(): array
    {
        return [
            'denied in the heading' => [
                '- Backup archives (not encrypted by this plugin): when you run or schedule a backup, the agent archives your database and/or files and uploads them to the storage destination your control plane configured.',
            ],
            'denied by the plugin' => ['This plugin does not encrypt the archive, so protection at rest comes from the storage destination.'],
            'denied with a contraction' => ['The plugin doesn\'t encrypt backups.'],
            'flipped in place' => [
                'Archives are not encrypted by the plugin, and an incremental run uses a content-addressed chunk store so only changed blocks move.',
            ],
            'a word between the negator and the verb' => ['Backups are not currently encrypted.'],
            'no client-side encryption' => ['No client-side encryption is applied to backups.'],
            'unencrypted' => ['Backups are uploaded unencrypted.'],
            'never' => ['The plugin never encrypts the chunks it uploads.'],
            'an FAQ question' => ['= Are my backups encrypted? ='],
            'encryption of something that is not a backup' => ['Passwords for email connections are stored encrypted.'],
            'the destination encryption, with no backup noun' => ['Protection at rest comes from the storage destination, including any server-side encryption it offers.'],
            'backups with no mention of encryption' => ['Full and incremental backups of the database and files, scheduled per site or run on demand.'],
        ];
    }

    #[DataProvider('honestCopyThatMustPass')]
    public function testTheDetectorDoesNotBlockHonestCopy(string $sentence): void
    {
        $this->assertSame(
            [],
            self::encryptionClaims($sentence),
            'The detector refused a sentence that is true of the shipped agent.'
        );
    }

    // ---------------------------------------------------------------------
    // The scan region.
    // ---------------------------------------------------------------------

    public function testReleaseNotesAreHistoryAndAreNotScanned(): void
    {
        $listing = "=== Name ===\nTags: backup\n\nShort.\n\n== Description ==\nBackups run on a schedule.\n\n"
            . "== Changelog ==\n= 1.0 =\n* Fixed: progress is reported during long archive and encryption passes.\n"
            . "* Added: encrypted backup archives.\n\n== Upgrade Notice ==\n= 1.0 =\nEncrypted backup archives.\n";

        $live = self::liveListing($listing);

        $this->assertStringContainsString('Backups run on a schedule.', $live);
        $this->assertStringNotContainsString('Added: encrypted backup archives', $live, 'The Changelog section was not dropped.');
        $this->assertStringNotContainsString('Upgrade Notice', $live, 'The Upgrade Notice section was not dropped.');
        $this->assertSame([], self::encryptionClaims($live));
    }

    public function testAClaimInALiveSectionIsFoundEvenWhenTheChangelogComesFirst(): void
    {
        $listing = "=== Name ===\n\nShort.\n\n== Changelog ==\n= 1.0 =\n* Fixed: something.\n\n"
            . "== Description ==\nEncrypted backup archives are uploaded.\n";

        $this->assertNotSame([], self::encryptionClaims(self::liveListing($listing)));
    }
}
