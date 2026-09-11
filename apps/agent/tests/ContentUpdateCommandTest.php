<?php
/**
 * ContentUpdateCommand unit tests.
 *
 * The fake WordPress here is a small in-memory posts+revisions store wired
 * through Brain Monkey, and it is deliberately FAITHFUL on the one behaviour
 * the command's Write label rests on: wp_update_post() saves its revision
 * AFTER the row is rewritten, so the revision core leaves behind holds the
 * NEW content (wp-includes/revision.php — "the most recent revision always
 * matches the current post"). A fake that revisioned the old content would
 * make the command look correct for a reason WordPress does not supply, and
 * would hide the exact defect the pre-flight guard exists to prevent.
 *
 * testWriteLabelWouldBeWrongWithoutThePreflight() is the proof of that: with
 * the pre-flight suppressed, the pre-update content survives nowhere.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandRepeatability;
use WPMgr\Agent\Commands\ContentUpdateCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\ContentUpdateCommand
 */
final class ContentUpdateCommandTest extends TestCase
{
    /** In-memory posts, keyed by ID. @var array<int,object> */
    private array $posts = [];

    /** In-memory revisions, newest first. @var array<int,list<object>> */
    private array $revisions = [];

    /** Next auto-increment ID for a created revision. */
    private int $nextRevisionId = 9000;

    /** Revision retention answer for wp_revisions_to_keep(). -1 is unlimited. */
    private int $revisionsToKeep = -1;

    /** When true, the fake wp_save_post_revision() does nothing. */
    private bool $suppressPreflightRevision = false;

    /** Post IDs passed to clean_post_cache(), in order. @var list<int> */
    private array $cleanPostCacheCalls = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->posts                     = [];
        $this->revisions                 = [];
        $this->nextRevisionId            = 9000;
        $this->revisionsToKeep           = -1;
        $this->suppressPreflightRevision = false;
        $this->cleanPostCacheCalls       = [];

        $this->installWordPressFake();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The fake WordPress
    // -------------------------------------------------------------------------

    /**
     * Wires the handful of WP functions the command calls to the in-memory
     * store above.
     *
     * @return void
     */
    private function installWordPressFake(): void
    {
        Functions\when('get_post')->alias(fn ($id) => $this->posts[(int) $id] ?? null);

        // The in-memory store IS the database here, so there is no cache to
        // clean; what matters is that the command calls this before every
        // authoritative read, and that a re-read returns what is stored now.
        Functions\when('clean_post_cache')->alias(function ($id): void {
            $this->cleanPostCacheCalls[] = (int) $id;
        });

        // has_blocks(), verbatim from the vendored WordPress 7.1 tree
        // (wp-includes/blocks.php:878-890), reduced to the string branch the
        // command actually uses: a plain substring test for '<!-- wp:'.
        // Measured against that source: '' => false, classic HTML => false,
        // delimiterless freeform => false, '<!-- wp:freeform -->' => true.
        Functions\when('has_blocks')->alias(
            static fn ($post) => is_string($post) && str_contains($post, '<!-- wp:')
        );

        Functions\when('sanitize_text_field')->alias(
            static fn ($str) => trim((string) wp_strip_all_tags((string) $str))
        );

        // Stands in for the real kses: strips <script>, keeps ordinary markup.
        Functions\when('wp_kses_post')->alias(
            static fn ($content) => (string) preg_replace(
                '#<script\b[^>]*>.*?</script>#is',
                '',
                (string) $content
            )
        );

        Functions\when('wp_slash')->alias(
            static fn ($value) => is_array($value) ? array_map(
                static fn ($v) => is_string($v) ? addslashes($v) : $v,
                $value
            ) : (is_string($value) ? addslashes($value) : $value)
        );

        Functions\when('is_wp_error')->alias(static fn ($thing) => $thing instanceof \WP_Error);

        Functions\when('wp_revisions_to_keep')->alias(fn () => $this->revisionsToKeep);

        Functions\when('wp_get_post_revisions')->alias(
            fn ($postId) => $this->revisions[(int) $postId] ?? []
        );

        // Faithful to core: snapshots the post AS IT STANDS NOW, and declines
        // when an identical revision already exists or revisions are off.
        Functions\when('wp_save_post_revision')->alias(function ($postId) {
            if ($this->suppressPreflightRevision) {
                return null;
            }
            return $this->storeRevision((int) $postId);
        });

        // Faithful to core's ORDER: rewrite the row, THEN revision it. The
        // revision this leaves behind holds the new content.
        Functions\when('wp_update_post')->alias(function ($postarr) {
            $id = (int) ($postarr['ID'] ?? 0);
            if (!isset($this->posts[$id])) {
                return new \WP_Error('invalid_post', 'Invalid post ID.');
            }

            foreach (['post_title', 'post_content'] as $field) {
                if (array_key_exists($field, $postarr)) {
                    $this->posts[$id]->{$field} = stripslashes((string) $postarr[$field]);
                }
            }

            $this->storeRevision($id);

            return $id;
        });
    }

    /**
     * Stores a revision of a post's CURRENT state, applying the same
     * "unchanged means no new revision" rule and the same pruning tail core
     * applies.
     *
     * @param int $postId Parent post ID.
     * @return int|null New revision ID, or null when nothing was stored.
     */
    private function storeRevision(int $postId): ?int
    {
        if (!isset($this->posts[$postId]) || $this->revisionsToKeep === 0) {
            return null;
        }

        $post     = $this->posts[$postId];
        $existing = $this->revisions[$postId] ?? [];

        if ($existing !== []) {
            $latest = $existing[0];
            if (
                $latest->post_title === $post->post_title
                && $latest->post_content === $post->post_content
            ) {
                return null;
            }
        }

        $revision               = new \stdClass();
        $revision->ID           = $this->nextRevisionId++;
        $revision->post_parent  = $postId;
        $revision->post_name    = $postId . '-revision-v1';
        $revision->post_title   = $post->post_title;
        $revision->post_content = $post->post_content;

        array_unshift($existing, $revision);

        // Core prunes down to wp_revisions_to_keep() after every save.
        if ($this->revisionsToKeep > 0 && count($existing) > $this->revisionsToKeep) {
            $existing = array_slice($existing, 0, $this->revisionsToKeep);
        }

        $this->revisions[$postId] = $existing;

        return (int) $revision->ID;
    }

    /**
     * Seeds one post. No revision is created, mirroring wp_insert_post().
     *
     * @param int    $id      Post ID.
     * @param string $type    Post type.
     * @param string $status  Post status.
     * @param string $title   Post title.
     * @param string $content Post content.
     * @return void
     */
    private function seedPost(
        int $id,
        string $type = 'post',
        string $status = 'publish',
        string $title = 'Original title',
        string $content = 'Original content'
    ): void {
        $post               = new \stdClass();
        $post->ID           = $id;
        $post->post_type    = $type;
        $post->post_status  = $status;
        $post->post_title   = $title;
        $post->post_content = $content;

        $this->posts[$id] = $post;
    }

    /**
     * The fingerprint of what a post holds RIGHT NOW.
     *
     * Recomputed here from the spec rather than called on the command, so a
     * change to the command's framing shows up as a red test instead of both
     * sides drifting together: exact stored bytes, length-framed, domain
     * separator, no normalisation of any kind.
     *
     * @param int $postId Post ID.
     * @return string
     */
    private function fingerprintOf(int $postId): string
    {
        $post    = $this->posts[$postId];
        $title   = (string) $post->post_title;
        $content = (string) $post->post_content;

        return 'sha256:' . hash(
            'sha256',
            "wpmgr.content_update.v1\n"
            . strlen($title) . "\n" . $title . "\n"
            . strlen($content) . "\n" . $content
        );
    }

    /**
     * Runs the command, filling in a CURRENT expected_fingerprint unless the
     * test supplies its own.
     *
     * Every pre-existing test here predates the precondition and means "the
     * caller is up to date", so the default keeps them testing what they were
     * written to test. A test about staleness passes its own value.
     *
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    private function runCommand(array $params): array
    {
        $postId = (int) ($params['post_id'] ?? 0);

        if (!array_key_exists('expected_fingerprint', $params) && isset($this->posts[$postId])) {
            $params['expected_fingerprint'] = $this->fingerprintOf($postId);
        }

        return (new ContentUpdateCommand())->execute([], $params);
    }

    /**
     * Every revision body currently stored for a post.
     *
     * @param int $postId Parent post ID.
     * @return list<string>
     */
    private function revisionContents(int $postId): array
    {
        return array_map(
            static fn ($r) => (string) $r->post_content,
            $this->revisions[$postId] ?? []
        );
    }

    // -------------------------------------------------------------------------
    // Identity and labels
    // -------------------------------------------------------------------------

    public function test_name_is_content_update(): void
    {
        $this->assertSame('content_update', (new ContentUpdateCommand())->name());
    }

    public function test_declares_write_and_unsafe(): void
    {
        $command = new ContentUpdateCommand();
        $this->assertSame(CommandEffect::Write, $command->effect());
        $this->assertSame(CommandRepeatability::Unsafe, $command->repeatability());
    }

    // -------------------------------------------------------------------------
    // 1. The happy path
    // -------------------------------------------------------------------------

    public function test_updates_title_and_content(): void
    {
        $this->seedPost(12);

        $result = $this->runCommand([
            'post_id' => 12,
            'title'   => 'New title',
            'content' => 'New content',
        ]);

        $this->assertTrue($result['ok'], 'update refused: ' . ($result['detail'] ?? ''));
        $this->assertSame(12, $result['post_id']);
        $this->assertSame('post', $result['post_type']);
        $this->assertSame(['title', 'content'], $result['changed']);

        $this->assertSame('New title', $this->posts[12]->post_title);
        $this->assertSame('New content', $this->posts[12]->post_content);
    }

    public function test_content_only_update_leaves_title_alone(): void
    {
        $this->seedPost(13, 'page');

        $result = $this->runCommand([
            'post_id' => 13,
            'content' => 'Just the body',
        ]);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertSame(['content'], $result['changed']);
        $this->assertSame('Original title', $this->posts[13]->post_title);
        $this->assertSame('Just the body', $this->posts[13]->post_content);
    }

    // -------------------------------------------------------------------------
    // 5. The revision assertion -- what earns the Write label
    // -------------------------------------------------------------------------

    public function test_a_revision_holds_the_previous_content_after_a_successful_update(): void
    {
        $this->seedPost(21, 'post', 'publish', 'Before title', 'Before content');

        $result = $this->runCommand([
            'post_id' => 21,
            'title'   => 'After title',
            'content' => 'After content',
        ]);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));

        $this->assertContains(
            'Before content',
            $this->revisionContents(21),
            'No revision retains the overwritten content. The Write label asserts the previous '
            . 'version survives the update; if this fails, the label is wrong.'
        );

        // The reported revision id must be the one holding the PREVIOUS
        // version, not the one core created from the new content.
        $reported = null;
        foreach ($this->revisions[21] as $revision) {
            if ((int) $revision->ID === (int) $result['revision_id']) {
                $reported = $revision;
                break;
            }
        }

        $this->assertNotNull($reported, 'reported revision_id is not a stored revision');
        $this->assertSame('Before content', $reported->post_content);
        $this->assertSame('Before title', $reported->post_title);
    }

    /**
     * The pre-flight is load-bearing, and this is the proof.
     *
     * Suppress only wp_save_post_revision() -- the call the command makes
     * BEFORE writing -- and leave core's own post-update revision intact. The
     * command must refuse; and had it written anyway, the overwritten content
     * would survive nowhere, which is Destructive, not Write.
     *
     * @return void
     */
    public function test_write_label_would_be_wrong_without_the_preflight(): void
    {
        $this->seedPost(22, 'post', 'publish', 'Before title', 'Before content');
        $this->suppressPreflightRevision = true;

        $result = $this->runCommand([
            'post_id' => 22,
            'content' => 'After content',
        ]);

        $this->assertFalse($result['ok'], 'command wrote without retaining the previous version');
        $this->assertStringContainsString('refusing', $result['detail']);

        $this->assertSame('Before content', $this->posts[22]->post_content, 'post was modified anyway');
        $this->assertNotContains('After content', $this->revisionContents(22));
    }

    public function test_refuses_when_revisions_are_disabled(): void
    {
        $this->seedPost(23);
        $this->revisionsToKeep = 0;

        $result = $this->runCommand([
            'post_id' => 23,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('revisions are disabled', $result['detail']);
        $this->assertSame('Original content', $this->posts[23]->post_content);
    }

    /**
     * Retention of 1 is the subtle one: a revision IS created, and core's
     * post-update prune then deletes it. Refusing is the only honest answer.
     *
     * @return void
     */
    public function test_refuses_when_retention_is_too_low_to_survive_the_prune(): void
    {
        $this->seedPost(24);
        $this->revisionsToKeep = 1;

        $result = $this->runCommand([
            'post_id' => 24,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('too low', $result['detail']);
        $this->assertSame('Original content', $this->posts[24]->post_content);
    }

    // -------------------------------------------------------------------------
    // 2, 3, 4. Refusals
    // -------------------------------------------------------------------------

    public function test_refuses_a_post_that_does_not_exist(): void
    {
        $result = $this->runCommand([
            'post_id'              => 404,
            'content'              => 'New content',
            // There is no post to fingerprint; any well-formed value is fine,
            // and "post not found" must win over anything about this field.
            'expected_fingerprint' => 'sha256:' . str_repeat('0', 64),
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('post not found', $result['detail']);
        $this->assertStringContainsString('404', $result['detail']);
    }

    public function test_refuses_a_trashed_post(): void
    {
        $this->seedPost(31, 'post', 'trash');

        $result = $this->runCommand([
            'post_id' => 31,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('trash', $result['detail']);
        $this->assertSame('Original content', $this->posts[31]->post_content);
    }

    public function test_refuses_when_neither_title_nor_content_is_present(): void
    {
        $this->seedPost(32);

        $result = $this->runCommand(['post_id' => 32]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('nothing to update', $result['detail']);
    }

    public function test_refuses_a_missing_post_id(): void
    {
        $result = $this->runCommand(['content' => 'New content']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('post_id', $result['detail']);
    }

    public function test_refuses_a_post_type_the_request_did_not_allow(): void
    {
        $this->seedPost(33, 'product');

        $result = $this->runCommand([
            'post_id' => 33,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('did not allow', $result['detail']);
        $this->assertSame('Original content', $this->posts[33]->post_content);
    }

    public function test_allows_a_post_type_the_request_named(): void
    {
        $this->seedPost(34, 'product');

        $result = $this->runCommand([
            'post_id'            => 34,
            'content'            => 'New content',
            'allowed_post_types' => ['product'],
        ]);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertSame('New content', $this->posts[34]->post_content);
    }

    public function test_refuses_a_malformed_allowed_post_types(): void
    {
        $this->seedPost(35);

        $result = $this->runCommand([
            'post_id'            => 35,
            'content'            => 'New content',
            'allowed_post_types' => ['post', 7],
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('allowed_post_types', $result['detail']);
        $this->assertSame('Original content', $this->posts[35]->post_content);
    }

    public function test_refuses_a_non_string_content(): void
    {
        $this->seedPost(36);

        $result = $this->runCommand([
            'post_id' => 36,
            'content' => ['not', 'a', 'string'],
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('content must be a string', $result['detail']);
    }

    // -------------------------------------------------------------------------
    // Sanitisation
    // -------------------------------------------------------------------------

    public function test_content_is_filtered_before_it_is_stored(): void
    {
        $this->seedPost(41);

        $result = $this->runCommand([
            'post_id' => 41,
            'content' => 'Safe<script>alert(1)</script> text',
        ]);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertStringNotContainsString('<script', $this->posts[41]->post_content);
        $this->assertStringContainsString('Safe', $this->posts[41]->post_content);
    }

    public function test_wp_update_post_failure_is_reported_not_swallowed(): void
    {
        $this->seedPost(42);

        Functions\when('wp_update_post')->alias(
            static fn () => new \WP_Error('db_error', 'Could not update post in the database.')
        );

        $result = $this->runCommand([
            'post_id' => 42,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('wp_update_post failed', $result['detail']);
        $this->assertStringContainsString('database', $result['detail']);
    }

    // -------------------------------------------------------------------------
    // Block documents
    //
    // Writing free-form HTML into a block document leaves markup that no
    // longer matches what the block type's save routine emits. Nothing fails
    // at write time; the damage surfaces when a human next opens the editor,
    // is offered block recovery on a page they never touched, and accepts it,
    // which discards the block. So the write must not happen at all.
    // -------------------------------------------------------------------------

    public function test_refuses_a_block_document_and_leaves_it_byte_for_byte_unchanged(): void
    {
        $blockContent = "<!-- wp:paragraph -->\n<p>Canonical markup.</p>\n<!-- /wp:paragraph -->";
        $this->seedPost(51, 'page', 'publish', 'Block page', $blockContent);

        $before = $this->fingerprintOf(51);

        $result = $this->runCommand([
            'post_id' => 51,
            'title'   => 'Rewritten title',
            'content' => '<p>Free-form HTML.</p>',
        ]);

        $this->assertFalse($result['ok'], 'a block document was written');
        $this->assertSame('failed', $result['outcome']);
        $this->assertSame('block_document_unsupported', $result['code']);

        // The refusal has to be actionable, or an automated caller retries the
        // identical call until its budget is gone.
        $this->assertStringContainsString('block document', $result['detail']);
        $this->assertStringContainsString('Do not retry', $result['detail']);

        $this->assertSame($blockContent, $this->posts[51]->post_content);
        $this->assertSame('Block page', $this->posts[51]->post_title);
        $this->assertSame($before, $this->fingerprintOf(51), 'the post changed');
        $this->assertSame([], $this->revisionContents(51), 'a refused call left a revision behind');
    }

    /**
     * The over-fire test, and it matters as much as the refusals: a guard that
     * blocks correct work gets switched off, and then it guards nothing.
     *
     * @return void
     */
    public function test_a_classic_document_with_a_matching_fingerprint_still_succeeds(): void
    {
        $this->seedPost(52, 'page', 'publish', 'Classic page', '<p>Plain old HTML.</p>');

        $result = $this->runCommand([
            'post_id' => 52,
            'content' => '<p>Replacement HTML.</p>',
        ]);

        $this->assertTrue($result['ok'], 'correct work was refused: ' . ($result['detail'] ?? ''));
        $this->assertSame('<p>Replacement HTML.</p>', $this->posts[52]->post_content);
        $this->assertSame(['content'], $result['changed']);
    }

    public function test_a_stale_fingerprint_is_a_conflict_not_a_failure_and_writes_nothing(): void
    {
        $this->seedPost(53, 'post', 'publish', 'Title as read', 'Content as read');

        $stale = $this->fingerprintOf(53);

        // Somebody else edits the post between the caller's read and its write.
        $this->posts[53]->post_content = 'Content as it is NOW';

        $result = $this->runCommand([
            'post_id'              => 53,
            'content'              => 'Content the caller planned',
            'expected_fingerprint' => $stale,
        ]);

        $this->assertFalse($result['ok']);
        $this->assertSame('conflict', $result['outcome'], 'a conflict was reported as a failure');
        $this->assertSame('expected_fingerprint_mismatch', $result['code']);
        $this->assertFalse($result['written']);
        $this->assertSame($stale, $result['expected_fingerprint']);
        $this->assertSame($this->fingerprintOf(53), $result['current_fingerprint']);

        // A conflict is not a failure, and the response must say which it is:
        // the remedies are opposite -- re-read and re-plan, versus retry.
        $this->assertStringContainsString('conflict, not a failure', $result['detail']);

        $this->assertSame('Content as it is NOW', $this->posts[53]->post_content, 'the other writer was overwritten');
    }

    /**
     * The failure outcome and the conflict outcome must not be the same value,
     * asserted side by side so a future "simplification" that collapses them
     * cannot pass.
     *
     * @return void
     */
    public function test_conflict_and_failure_are_distinguishable_outcomes(): void
    {
        $this->seedPost(54);
        $this->revisionsToKeep = 0;

        $failure = $this->runCommand(['post_id' => 54, 'content' => 'New content']);

        $this->seedPost(55);
        $conflict = $this->runCommand([
            'post_id'              => 55,
            'content'              => 'New content',
            'expected_fingerprint' => 'sha256:' . str_repeat('f', 64),
        ]);

        $this->assertFalse($failure['ok']);
        $this->assertFalse($conflict['ok']);
        $this->assertSame('failed', $failure['outcome']);
        $this->assertSame('conflict', $conflict['outcome']);
        $this->assertNotSame($failure['outcome'], $conflict['outcome']);
    }

    /**
     * The reported fingerprint must be of the bytes that are STORED, which is
     * not the same thing as the bytes that were sent.
     *
     * The proof is a filter that quietly rewrites the content on its way into
     * the database, standing in for the real thing: a page builder, a kses
     * variant, a sanitising plugin. A response echoing the request's value
     * reports a fingerprint the database does not hold, and the caller has no
     * way to see that its write did not land as asked.
     *
     * @return void
     */
    public function test_the_returned_fingerprint_is_the_re_read_stored_value(): void
    {
        $this->seedPost(56, 'post', 'publish', 'Title', 'Before');

        $sent = 'After';

        // Something between the command and the row rewrites the content.
        Functions\when('wp_update_post')->alias(function ($postarr) {
            $id                              = (int) $postarr['ID'];
            $this->posts[$id]->post_content  = 'SOMETHING ELSE ENTIRELY';
            return $id;
        });

        $result = $this->runCommand(['post_id' => 56, 'content' => $sent]);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertSame(
            $this->fingerprintOf(56),
            $result['fingerprint'],
            'the reported fingerprint is not the fingerprint of what is stored'
        );
        $this->assertNotSame(
            'sha256:' . hash('sha256', "wpmgr.content_update.v1\n5\nTitle\n5\n" . $sent),
            $result['fingerprint'],
            'the response echoed the request instead of re-reading the row'
        );
        $this->assertSame(strlen('SOMETHING ELSE ENTIRELY'), $result['content_bytes']);
        $this->assertSame(strlen('Title'), $result['title_bytes']);

        // Re-read means re-read: the object cache is dropped first, or a
        // persistent cache can answer with a version the row no longer holds.
        $this->assertContains(56, $this->cleanPostCacheCalls);
    }

    // -------------------------------------------------------------------------
    // What has_blocks() ACTUALLY does
    //
    // Measured against the vendored WordPress 7.1 source
    // (tools/plugincheck/wp/wp-includes/blocks.php:878-890), which is
    // str_contains($content, '<!-- wp:') and nothing more:
    //
    //   empty string                  => false
    //   classic HTML                  => false
    //   freeform, delimiterless       => false   <- what the block editor
    //                                               stores for classic content
    //   '<!-- wp:freeform -->' framed => true
    //
    // These assert the measured behaviour, not the behaviour one might expect
    // of something called "has blocks".
    // -------------------------------------------------------------------------

    public function test_an_empty_post_is_not_a_block_document(): void
    {
        $this->seedPost(57, 'post', 'publish', 'Empty', '');

        $result = $this->runCommand(['post_id' => 57, 'content' => 'First body.']);

        $this->assertTrue($result['ok'], 'an empty post was treated as a block document: '
            . (string) ($result['detail'] ?? ''));
        $this->assertSame('First body.', $this->posts[57]->post_content);
        $this->assertFalse($result['ownership']['is_block_document']);
    }

    /**
     * Classic content round-tripped through the block editor is stored as a
     * core/freeform block, and core/freeform serialises WITHOUT delimiters.
     * has_blocks() therefore answers false and the write is allowed — which is
     * the measured behaviour, and is also the right answer, since freeform
     * holds raw HTML and has no save routine to mismatch.
     *
     * @return void
     */
    public function test_a_delimiterless_freeform_document_is_allowed(): void
    {
        $this->seedPost(58, 'page', 'publish', 'Freeform', '<p>Classic content.</p>');

        $result = $this->runCommand(['post_id' => 58, 'content' => '<p>Replaced.</p>']);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertSame('<p>Replaced.</p>', $this->posts[58]->post_content);
    }

    /**
     * The same content WITH an explicit freeform delimiter is refused, because
     * has_blocks() is a substring test and the delimiter is present.
     *
     * @return void
     */
    public function test_a_delimited_freeform_document_is_refused(): void
    {
        $content = "<!-- wp:freeform -->\n<p>Classic content.</p>\n<!-- /wp:freeform -->";
        $this->seedPost(59, 'page', 'publish', 'Freeform', $content);

        $result = $this->runCommand(['post_id' => 59, 'content' => '<p>Replaced.</p>']);

        $this->assertFalse($result['ok']);
        $this->assertSame('block_document_unsupported', $result['code']);
        $this->assertSame($content, $this->posts[59]->post_content);
    }

    // -------------------------------------------------------------------------
    // Ownership confidence
    // -------------------------------------------------------------------------

    public function test_the_response_admits_it_did_not_check_for_a_page_builder(): void
    {
        $this->seedPost(60, 'page');

        $result = $this->runCommand(['post_id' => 60, 'content' => 'New body']);

        $this->assertTrue($result['ok'], (string) ($result['detail'] ?? ''));
        $this->assertTrue($result['ownership']['block_document_checked']);
        $this->assertFalse($result['ownership']['is_block_document']);
        $this->assertFalse(
            $result['ownership']['page_builder_checked'],
            'the response claims a page-builder verdict this slice did not earn'
        );
        $this->assertStringContainsString('page builder', $result['ownership']['detail']);
    }

    public function test_expected_fingerprint_is_required(): void
    {
        $this->seedPost(61);

        $result = (new ContentUpdateCommand())->execute([], [
            'post_id' => 61,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertSame('missing_expected_fingerprint', $result['code']);
        $this->assertSame('Original content', $this->posts[61]->post_content);
    }
}
