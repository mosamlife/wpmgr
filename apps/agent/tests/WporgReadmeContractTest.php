<?php
/**
 * THE WORDPRESS.ORG LISTING IS AN IRREVERSIBLE PUBLISH, AND ITS LIMITS ARE SILENT.
 *
 * readme.txt is the only file in this repository whose rendering is performed by
 * someone else's parser, on someone else's server, with no error surfaced to us
 * when it refuses part of the file. Three of its limits fail QUIETLY:
 *
 *   1. Tags are hard-sliced at 5. Tag six onward is DISCARDED, not deprioritised.
 *   2. The short description is cut at 150 characters mid-sentence.
 *   3. THE ONE NOBODY KNOWS ABOUT. Every section whose heading is not one of the
 *      parser's known names (description, installation, faq, screenshots,
 *      changelog, upgrade_notice, other_notes) is merged into other_notes and
 *      then APPENDED TO DESCRIPTION. So "== Privacy ==", "== External services ==",
 *      "== Third-party / Credits ==" and "== Source code ==" are not sections at
 *      all: they are Description, and they spend Description's single budget of
 *      2500 words. Over budget, the parser truncates the merged blob and appends
 *      an ellipsis. Whatever sits LAST is eaten first, and what sits last here is
 *      the reviewer-required external-service and licence disclosure.
 *
 * A listing that silently drops those disclosures is a listing that can be pulled.
 * The budget is not visible anywhere: not in the file, not in the zip, not in
 * Plugin Check, which does not model it. This test is the only place it exists.
 *
 * Every number below is taken from the directory's own parser,
 * plugin-directory/readme/class-parser.php:
 *   $maximum_field_lengths = array(
 *       'short_description' => 150,
 *       'section'           => 2500,
 *       'section-changelog' => 5000,
 *       'section-faq'       => 5000,
 *   );
 * and trim_length(), which in 'words' mode splits on /(\s+)/u with
 * PREG_SPLIT_DELIM_CAPTURE and compares the PIECE count against $length * 2.
 * N whitespace-separated words produce 2N-1 pieces, so 2500 "words" is 5000
 * pieces. The trim fires when the count reaches that number, hence "<", not "<=".
 *
 * The trim runs on the RAW section text, before Markdown rendering, which is why
 * counting the source file is the correct simulation.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class WporgReadmeContractTest extends TestCase
{
    /** Parser: 'short_description' => 150, applied in characters. */
    private const SHORT_DESCRIPTION_CHARS = 150;

    /** Parser: count( $this->tags ) > 5 then array_slice( $this->tags, 0, 5 ). */
    private const MAX_TAGS = 5;

    /** Parser: 'section' => 2500, applied as $length * 2 pieces. */
    private const SECTION_PIECES = 5000;

    /** Parser: 'section-faq' and 'section-changelog' => 5000, likewise doubled. */
    private const LONG_SECTION_PIECES = 10000;

    /**
     * Section slugs the parser knows. Anything else is merged into other_notes
     * and appended to description. Source: class-parser.php.
     */
    private const KNOWN_SECTIONS = [
        'description',
        'installation',
        'faq',
        'screenshots',
        'changelog',
        'upgrade_notice',
        'other_notes',
    ];

    /** Tags the parser silently drops before counting. */
    private const IGNORED_TAGS = ['plugin', 'wordpress'];

    private string $raw = '';

    /** @var array<string,string> lower-cased header field => value */
    private array $headers = [];

    private string $shortDescription = '';

    /** @var array<string,string> section slug => body */
    private array $sections = [];

    /** @var list<string> raw headings of sections the parser does not know */
    private array $unknownHeadings = [];

    /** The merged description the parser actually budgets. */
    private string $mergedDescription = '';

    protected function set_up(): void
    {
        $path = dirname(__DIR__) . '/readme.txt';
        $this->assertFileExists($path, 'apps/agent/readme.txt is the wp.org listing and must exist.');

        $raw = file_get_contents($path);
        $this->assertIsString($raw, 'readme.txt could not be read.');
        $this->raw = $raw;

        $lines = explode("\n", $raw);

        // Header block: every line up to the first blank line.
        $i = 1;
        for (; $i < count($lines) && trim($lines[$i]) !== ''; $i++) {
            if (preg_match('/^([A-Za-z][A-Za-z0-9 _-]*):\s*(.*)$/', $lines[$i], $m) === 1) {
                $this->headers[strtolower(trim($m[1]))] = trim($m[2]);
            }
        }
        for (; $i < count($lines) && trim($lines[$i]) === ''; $i++) {
            // Skip the blank run before the short description.
        }
        // Short description: the paragraph before the first == Section ==.
        $short = [];
        for (; $i < count($lines) && trim($lines[$i]) !== '' && !str_starts_with($lines[$i], '=='); $i++) {
            $short[] = trim($lines[$i]);
        }
        $this->shortDescription = implode(' ', $short);

        // Sections.
        $current = null;
        $body    = [];
        for (; $i < count($lines); $i++) {
            if (preg_match('/^==\s*(.+?)\s*==\s*$/', $lines[$i], $m) === 1) {
                if ($current !== null) {
                    $this->storeSection($current, implode("\n", $body));
                }
                $current = $m[1];
                $body    = [];
                continue;
            }
            if ($current !== null) {
                $body[] = $lines[$i];
            }
        }
        if ($current !== null) {
            $this->storeSection($current, implode("\n", $body));
        }

        // What the parser does last: other_notes is appended to description.
        $this->mergedDescription = ($this->sections['description'] ?? '')
            . "\n" . ($this->sections['other_notes'] ?? '');
    }

    private function storeSection(string $heading, string $body): void
    {
        $slug = preg_replace('/[^a-z0-9_]+/', '_', strtolower(trim($heading))) ?? '';
        $slug = trim($slug, '_');
        $slug = match ($slug) {
            'frequently_asked_questions' => 'faq',
            'change_log'                 => 'changelog',
            'screenshot'                 => 'screenshots',
            default                      => $slug,
        };

        if (!in_array($slug, self::KNOWN_SECTIONS, true)) {
            $this->unknownHeadings[] = $heading;
            // The parser prefixes each merged block with an <h3> of its title.
            $body = '<h3>' . $heading . '</h3>' . "\n" . $body;
            $slug = 'other_notes';
        }

        $this->sections[$slug] = ($this->sections[$slug] ?? '') . "\n" . $body;
    }

    /**
     * trim_length()'s piece count, verbatim: preg_split on /(\s+)/u with
     * PREG_SPLIT_DELIM_CAPTURE, then count().
     */
    private function pieces(string $text): int
    {
        $text = trim($text);
        if ($text === '') {
            return 0;
        }
        $split = preg_split('/(\s+)/u', $text, -1, PREG_SPLIT_DELIM_CAPTURE);

        return is_array($split) ? count($split) : 0;
    }

    public function testShortDescriptionFitsThe150CharacterCut(): void
    {
        $this->assertNotSame('', $this->shortDescription, 'The short description is the listing subtitle and must not be empty.');
        $this->assertLessThanOrEqual(
            self::SHORT_DESCRIPTION_CHARS,
            mb_strlen($this->shortDescription),
            sprintf(
                'The short description is %d characters. Over %d the directory cuts it and appends an ellipsis: %s',
                mb_strlen($this->shortDescription),
                self::SHORT_DESCRIPTION_CHARS,
                $this->shortDescription
            )
        );
    }

    public function testNoTagIsSilentlyDiscarded(): void
    {
        $declared = array_values(array_filter(array_map('trim', explode(',', $this->headers['tags'] ?? ''))));
        $kept     = array_values(array_filter(
            $declared,
            static fn (string $t): bool => !in_array(strtolower($t), self::IGNORED_TAGS, true)
        ));

        $this->assertNotSame([], $kept, 'The listing must declare at least one usable tag.');
        $this->assertLessThanOrEqual(
            self::MAX_TAGS,
            count($kept),
            sprintf(
                'Tags 6 and up are DISCARDED by the parser, not deprioritised. Declared: %s',
                implode(', ', $declared)
            )
        );
        $this->assertSame(
            $declared,
            $kept,
            'The parser silently drops the tags "plugin" and "wordpress", so a slot spent on either is a slot lost.'
        );
    }

    /**
     * The one that matters. Description plus every custom section share ONE budget.
     */
    public function testMergedDescriptionStaysUnderTheSectionBudget(): void
    {
        $count = $this->pieces($this->mergedDescription);

        $this->assertLessThan(
            self::SECTION_PIECES,
            $count,
            sprintf(
                "Description plus the custom sections (%s) total %d pieces against the parser's %d. "
                . 'Over budget the directory truncates the END of that blob, which is the external-service '
                . 'and licence disclosure the reviewers required. Compress prose; do not delete a disclosure.',
                implode(', ', $this->unknownHeadings),
                $count,
                self::SECTION_PIECES
            )
        );
    }

    public function testSectionsWithTheirOwnBudgetsStayUnderThem(): void
    {
        $limits = [
            'installation'   => self::SECTION_PIECES,
            'screenshots'    => self::SECTION_PIECES,
            'faq'            => self::LONG_SECTION_PIECES,
            'changelog'      => self::LONG_SECTION_PIECES,
            'upgrade_notice' => self::LONG_SECTION_PIECES,
        ];

        foreach ($limits as $slug => $limit) {
            if (!isset($this->sections[$slug])) {
                continue;
            }
            $count = $this->pieces($this->sections[$slug]);
            $this->assertLessThan(
                $limit,
                $count,
                sprintf('Section "%s" is %d pieces against a limit of %d.', $slug, $count, $limit)
            );
        }
    }

    /**
     * C1: the disclosures the reviewers required. The register may be rewritten;
     * a disclosure may not be removed.
     */
    public function testEveryReviewerRequiredDisclosureIsStillPresent(): void
    {
        foreach (
            [
            'Privacy / What data is sent and where',
            'External services',
            'Third-party / Credits',
            'Source code',
            ] as $heading
        ) {
            $this->assertContains(
                $heading,
                $this->unknownHeadings,
                sprintf('The "%s" section is a reviewer-required disclosure and must not be removed.', $heading)
            );
        }

        $this->assertStringContainsString(
            'no default endpoint',
            strtolower($this->raw),
            'The listing must keep the statement that the plugin has no default endpoint and is inert until configured.'
        );
        $this->assertStringContainsString(
            'matthiasmullie/minify',
            $this->raw,
            'readme.txt is the only place the bundled library MIT attribution travels in the shipped zip.'
        );
    }

    /**
     * C2: the wp.org build physically excludes the self-updater, so the listing
     * may never describe one. This is the sentence that says so.
     */
    public function testTheListingKeepsTheDirectoryOnlyUpdateStatement(): void
    {
        $this->assertMatchesRegularExpression(
            '/no separate update channel in this build/i',
            $this->raw,
            'The wp.org build ships without a self-updater. The FAQ answer saying updates arrive through the '
            . 'directory is a compliance answer and must stay.'
        );
    }

    /**
     * Every caption line renders against the SVN asset of the same index, so a
     * gap or a restart makes a screenshot render with somebody else's caption.
     */
    public function testScreenshotCaptionsAreContiguousFromOne(): void
    {
        $this->assertArrayHasKey('screenshots', $this->sections, 'The listing must declare its screenshots.');

        preg_match_all('/^\s*(\d+)\.\s+\S/m', $this->sections['screenshots'], $m);
        $numbers = array_map('intval', $m[1]);

        $this->assertNotSame([], $numbers, 'The Screenshots section must contain a numbered caption list.');
        $this->assertSame(
            range(1, count($numbers)),
            $numbers,
            'Screenshot captions are matched to assets/screenshot-N.png by index, so they must run 1..N with no gaps.'
        );
    }

    public function testStableTagIsAReleasableVersion(): void
    {
        $this->assertArrayHasKey('stable tag', $this->headers, 'readme.txt must carry a Stable tag.');
        $this->assertMatchesRegularExpression(
            '/^\d+\.\d+\.\d+$/',
            $this->headers['stable tag'],
            'Stable tag names the tags/ directory the directory serves to every installed site. It must be a '
            . 'plain MAJOR.MINOR.PATCH. The wp.org build target restamps it from the staged plugin header, but a '
            . 'readme-only SVN commit ships whatever is written here.'
        );
    }

    /** House rule, and this is the most public copy the project ships. */
    public function testNoEmOrEnDashes(): void
    {
        $this->assertSame(0, substr_count($this->raw, "\u{2014}"), 'readme.txt contains an em dash.');
        $this->assertSame(0, substr_count($this->raw, "\u{2013}"), 'readme.txt contains an en dash.');
    }
}
