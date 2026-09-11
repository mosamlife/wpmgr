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

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->posts                     = [];
        $this->revisions                 = [];
        $this->nextRevisionId            = 9000;
        $this->revisionsToKeep           = -1;
        $this->suppressPreflightRevision = false;

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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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
        $result = (new ContentUpdateCommand())->execute([], [
            'post_id' => 404,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('post not found', $result['detail']);
        $this->assertStringContainsString('404', $result['detail']);
    }

    public function test_refuses_a_trashed_post(): void
    {
        $this->seedPost(31, 'post', 'trash');

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], ['post_id' => 32]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('nothing to update', $result['detail']);
    }

    public function test_refuses_a_missing_post_id(): void
    {
        $result = (new ContentUpdateCommand())->execute([], ['content' => 'New content']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('post_id', $result['detail']);
    }

    public function test_refuses_a_post_type_the_request_did_not_allow(): void
    {
        $this->seedPost(33, 'product');

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
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

        $result = (new ContentUpdateCommand())->execute([], [
            'post_id' => 42,
            'content' => 'New content',
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('wp_update_post failed', $result['detail']);
        $this->assertStringContainsString('database', $result['detail']);
    }
}
