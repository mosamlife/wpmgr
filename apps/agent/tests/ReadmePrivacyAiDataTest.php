<?php
/**
 * GH #891: the listing's privacy section names the data AI requests carry.
 *
 * readme.txt is the wordpress.org listing. Its privacy section lists, one
 * bullet each, what the agent sends to the configured control plane. AI
 * requests send two more things: the results of the reads an AI asks for
 * (published page and post text, titles and excerpts, media details, and what
 * reviewed abilities on the site return) and previews of the drafts and
 * changes it proposes. This guard requires a bullet in that list that says
 * so.
 *
 * It must fail, not pass, when it has nothing to read: a missing section, a
 * missing list, or a list without the bullets it has always carried is a
 * failure, so a parser that drifts cannot pass over an empty list.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class ReadmePrivacyAiDataTest extends TestCase
{
    private const SECTION = '== Privacy / What data is sent and where ==';

    /** The sentence that introduces the list, and scopes it to the control plane. */
    private const LIST_INTRO = 'It sends the following, only to that endpoint';

    public function test_privacy_list_has_a_bullet_for_ai_request_data(): void
    {
        $bullets = self::privacyBullets();

        $ai = array_values(array_filter($bullets, static fn (string $b): bool => preg_match('/\bAI\b/', $b) === 1));
        $this->assertCount(1, $ai, 'the privacy list must carry exactly one bullet for AI request data');
        $bullet = $ai[0];

        $this->assertMatchesRegularExpression('/\bresults of the reads\b/i', $bullet, 'the AI bullet must name the results of the reads an AI asks for');
        $this->assertMatchesRegularExpression('/\bpage and post text\b/i', $bullet, 'the AI bullet must name page and post text');
        $this->assertMatchesRegularExpression('/\btitles and excerpts\b/i', $bullet, 'the AI bullet must name titles and excerpts');
        $this->assertMatchesRegularExpression('/\bmedia details\b/i', $bullet, 'the AI bullet must name media details');
        $this->assertMatchesRegularExpression('/\breviewed abilities\b/i', $bullet, "the AI bullet must name what the site's reviewed abilities return");
        $this->assertMatchesRegularExpression('/\bpreviews of the drafts\b/i', $bullet, 'the AI bullet must name the previews of the drafts it proposes');
    }

    /**
     * The bullets of the list the privacy section introduces as going only to
     * the configured control plane.
     *
     * @return list<string>
     */
    private static function privacyBullets(): array
    {
        $raw = file_get_contents(dirname(__DIR__) . '/readme.txt');
        self::assertIsString($raw, 'readme.txt could not be read');

        $start = strpos($raw, self::SECTION);
        self::assertNotFalse($start, 'readme.txt has no privacy section');
        $body = substr($raw, (int) $start + strlen(self::SECTION));
        $next = preg_match('/^==[^=].*==\s*$/m', $body, $m, PREG_OFFSET_CAPTURE) === 1 ? (int) $m[0][1] : strlen($body);
        $section = substr($body, 0, $next);

        $intro = strpos($section, self::LIST_INTRO);
        self::assertNotFalse($intro, 'the privacy section no longer introduces its list as going only to the control plane');

        $bullets = [];
        $started = false;
        foreach (array_slice(explode("\n", substr($section, (int) $intro)), 1) as $line) {
            if (str_starts_with($line, '- ')) {
                $bullets[] = substr($line, 2);
                $started   = true;
                continue;
            }
            if ($started) {
                break;
            }
        }

        // Positive control: the list this reads is the one that has always
        // carried the backup and metadata bullets.
        self::assertNotEmpty(preg_grep('/^Backup archives\b/', $bullets), 'the parsed list has no backup bullet; the parser read the wrong list');
        self::assertNotEmpty(preg_grep('/^Site and environment metadata\b/', $bullets), 'the parsed list has no metadata bullet; the parser read the wrong list');

        return $bullets;
    }
}
