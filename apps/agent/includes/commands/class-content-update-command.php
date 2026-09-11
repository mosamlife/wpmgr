<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

// Plugin Check's Direct_File_Access_Check only scans the first 50 lines of
// the file (after the opening `<?php`) for this guard via its regex
// fallback; its AST path never matches an `if (!defined('ABSPATH'))` guard
// here because it is nested inside this file's `namespace` statement and the
// AST walker does not recurse into it for that check. A guard placed past
// line ~50 is therefore invisible to the check AND disqualifies the file
// from the checker's separate "no guard needed, it's just a class" exemption
// (a top-level If-with-exit is not one of its recognised "safe" statements)
// — which is *worse* than having no guard at all. Keep this guard here,
// above the file docblock below, not after it.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * ContentUpdateCommand: change the title and/or body of an EXISTING post or
 * page, and guarantee before writing that the version being overwritten is
 * retained somewhere the site can reach.
 *
 * This is the agent's first content-write command. Everything about its scope
 * is deliberately narrow: it updates, it never creates and never deletes, it
 * touches only post_title and post_content, and it refuses any post type the
 * request did not name. Meta, taxonomies, status transitions and page-builder
 * payloads are out of scope here.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/content_update
 *   Authorization: Bearer <Ed25519 JWT, cmd="content_update", aud=<siteId>>
 *   Body: {
 *     "post_id":            <int, required>,
 *     "title":              <string, optional>,
 *     "content":            <string, optional>,
 *     "allowed_post_types": <list<string>, optional; defaults to post + page>
 *   }
 *   At least one of title / content must be present.
 *
 * Response (200 OK):
 *   {
 *     "ok": true,
 *     "post_id":     <int>,
 *     "post_type":   <string>,
 *     "changed":     <list<string> — subset of ["title","content"]>,
 *     "revision_id": <int — the revision holding the PRE-UPDATE version>,
 *     "detail":      "updated"
 *   }
 *
 * Errors follow the agent's { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback has already enforced the Ed25519 +
 * anti-replay JWT contract (Connector::verifyCommand) before execute() runs.
 * This command adds no capability check of its own and invents no new
 * mechanism: a verified signed command IS the authorisation, exactly as it is
 * for file_write and every other write command here.
 *
 * SANITISATION. The body is filtered as an editor's submission from a user
 * WITHOUT the unfiltered_html capability would be: wp_kses_post() on the
 * content, sanitize_text_field() on the title. There is deliberately no
 * unfiltered-HTML escape hatch and no parameter that opens one — allowing raw
 * HTML through a remote command would put script injection one signed request
 * away, and that is a decision to be taken in the open rather than smuggled in
 * behind a flag. The filtered values are then wp_slash()ed, because
 * wp_update_post() documents its input as already escaped
 * (wp-includes/post.php:5316, "Arrays are expected to be escaped, i.e. passed
 * through wp_slash()") and unslashes it again internally.
 *
 * @package WPMgr\Agent\Commands
 */

/**
 * Updates the title and/or content of one existing post or page.
 */
final class ContentUpdateCommand implements CommandInterface
{
    /**
     * Post types accepted when the request does not name its own set.
     *
     * Both support revisions in core, which is what makes the retention
     * guarantee below satisfiable for the default case.
     *
     * @var list<string>
     */
    private const DEFAULT_ALLOWED_POST_TYPES = ['post', 'page'];

    /**
     * Minimum revision retention this command will run under.
     *
     * Two, not one, and the reason is the pruning tail of
     * wp_save_post_revision(): after our update core saves a revision of the
     * NEW content and then deletes the oldest revisions down to
     * wp_revisions_to_keep(). At a retention of 1 the revision we took of the
     * pre-update version is the one that gets deleted, so the copy we just
     * proved existed is gone by the time the response is written.
     */
    private const MIN_REVISIONS_TO_KEEP = 2;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'content_update';
    }

    /**
     * Effect: replaces the title and/or body of a live post, and is a Write
     * rather than a Destructive only because it refuses to run unless the
     * version it is about to overwrite is retained in a post revision.
     *
     * That condition is enforced, not assumed, and the reason it has to be is
     * that WordPress does NOT retain the old version by itself. Verified
     * against core (wp-includes/revision.php in the WordPress 7.1 tree under
     * tools/plugincheck/wp): wp_save_post_revision() is reached from
     * wp_save_post_revision_on_insert(), hooked on wp_after_insert_post AFTER
     * the row has already been rewritten, and its own docblock states "Creates
     * a revision for the current version of a post ... the most recent
     * revision always matches the current post". The revision wp_update_post()
     * leaves behind therefore holds the NEW content. On a post carrying no
     * earlier revision — one created by an importer, by WP-CLI, or by any
     * plain wp_insert_post(), none of which revision anything, because
     * wp_save_post_revision_on_insert() returns early when $update is false —
     * the previous version would simply cease to exist.
     *
     * So ensureRevisionRetained() takes the revision itself, BEFORE the write,
     * and verifies by reading it back that a revision now holds the
     * pre-update title and content. Three paths cannot satisfy that and are
     * refused rather than mislabelled: a post type that does not support
     * revisions, WP_POST_REVISIONS disabled (both surface as
     * wp_revisions_to_keep() === 0), and a retention too low to survive core's
     * post-update pruning. On every path this command actually executes, the
     * overwritten version is retained — which is what Write asserts.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: the request pins the post ID, but not the post's current
     * content. A blind retry writes the request's title and body over whatever
     * the post holds NOW, discarding any edit — by a human in wp-admin, or by
     * another command — made between the two runs. That is harm the first run
     * did not do, which is rule 1 of the enum.
     *
     * The pre-update revision this command guarantees makes that recoverable,
     * and recoverable is not the same outcome: someone has to notice and
     * restore it. Determine what the first run actually did before sending a
     * second.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Unsafe;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused; the
     *   Router already bound aud + cmd before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        // ------------------------------------------------------------------
        // 1. Parameters.
        // ------------------------------------------------------------------
        if (!array_key_exists('post_id', $params)) {
            return $this->fail('missing required field: post_id');
        }

        $rawId = $params['post_id'];
        if (!is_int($rawId) && !(is_string($rawId) && ctype_digit($rawId))) {
            return $this->fail('post_id must be a positive integer');
        }

        $postId = (int) $rawId;
        if ($postId <= 0) {
            return $this->fail('post_id must be a positive integer');
        }

        $hasTitle   = array_key_exists('title', $params);
        $hasContent = array_key_exists('content', $params);

        if (!$hasTitle && !$hasContent) {
            return $this->fail('nothing to update: provide title, content, or both');
        }
        if ($hasTitle && !is_string($params['title'])) {
            return $this->fail('title must be a string');
        }
        if ($hasContent && !is_string($params['content'])) {
            return $this->fail('content must be a string');
        }

        $allowedTypes = $this->resolveAllowedTypes($params);
        if ($allowedTypes === []) {
            return $this->fail('allowed_post_types must be a non-empty list of strings');
        }

        // ------------------------------------------------------------------
        // 2. The post must exist, be out of the trash, and be an allowed type.
        // ------------------------------------------------------------------
        $post = get_post($postId);
        if (!is_object($post) || !isset($post->ID) || (int) $post->ID !== $postId) {
            return $this->fail('post not found: ' . $postId);
        }

        $postType   = isset($post->post_type) ? (string) $post->post_type : '';
        $postStatus = isset($post->post_status) ? (string) $post->post_status : '';

        if ($postStatus === 'trash') {
            return $this->fail('post ' . $postId . ' is in the trash; restore it before updating');
        }
        if (!in_array($postType, $allowedTypes, true)) {
            return $this->fail('post ' . $postId . ' is of type "' . $postType . '", which this request did not allow');
        }

        // ------------------------------------------------------------------
        // 3. Nothing is overwritten until the current version is retained.
        // ------------------------------------------------------------------
        $retained = $this->ensureRevisionRetained($post);
        if (!is_int($retained)) {
            return $this->fail($retained);
        }

        // ------------------------------------------------------------------
        // 4. Sanitise, then write.
        // ------------------------------------------------------------------
        $update  = ['ID' => $postId];
        $changed = [];

        if ($hasTitle) {
            $title = sanitize_text_field((string) $params['title']);
            if ($title !== (string) ($post->post_title ?? '')) {
                $changed[] = 'title';
            }
            $update['post_title'] = $title;
        }

        if ($hasContent) {
            $content = wp_kses_post((string) $params['content']);
            if ($content !== (string) ($post->post_content ?? '')) {
                $changed[] = 'content';
            }
            $update['post_content'] = $content;
        }

        // wp_update_post() expects already-slashed input and unslashes it
        // internally; skipping this mangles every backslash in the body.
        $result = wp_update_post(wp_slash($update), true);

        if (is_wp_error($result)) {
            return $this->fail('wp_update_post failed: ' . $result->get_error_message());
        }
        if (!is_int($result) && !(is_string($result) && ctype_digit($result))) {
            return $this->fail('wp_update_post returned an unexpected value for post ' . $postId);
        }
        if ((int) $result === 0) {
            return $this->fail('wp_update_post did not update post ' . $postId);
        }

        return [
            'ok'          => true,
            'post_id'     => $postId,
            'post_type'   => $postType,
            'changed'     => $changed,
            'revision_id' => $retained,
            'detail'      => 'updated',
        ];
    }

    /**
     * Guarantees that the post's CURRENT version is stored in a revision
     * before the caller overwrites it, and returns that revision's ID.
     *
     * Returns a string reason instead when the guarantee cannot be made, which
     * the caller turns into a refusal. Never returns a revision ID it has not
     * read back and compared field by field: wp_save_post_revision() answers
     * null both when it declined to act and when an identical revision already
     * existed, and those two cases mean opposite things here.
     *
     * @param object $post The post as it stands before the update.
     * @return int|string Revision ID, or a refusal reason.
     */
    private function ensureRevisionRetained(object $post): int|string
    {
        $postId = (int) $post->ID;

        // wp_revisions_to_keep() folds in both the post-type support check and
        // WP_POST_REVISIONS, and returns -1 for unlimited.
        $keep = (int) wp_revisions_to_keep($post);

        if ($keep === 0) {
            return 'revisions are disabled for post type "' . (string) $post->post_type
                . '", so the version this would overwrite could not be retained; refusing';
        }
        if ($keep > 0 && $keep < self::MIN_REVISIONS_TO_KEEP) {
            return 'revision retention for post type "' . (string) $post->post_type . '" is ' . $keep
                . ', too low to keep the overwritten version past this update; refusing';
        }

        // Snapshot the CURRENT version. A null answer here is not yet a
        // failure: it also means "an identical revision already exists".
        wp_save_post_revision($postId);

        $latest = $this->latestRevision($postId);
        if ($latest === null) {
            return 'no revision could be created for post ' . $postId
                . ', so the version this would overwrite could not be retained; refusing';
        }

        $sameTitle   = (string) ($latest->post_title ?? '') === (string) ($post->post_title ?? '');
        $sameContent = (string) ($latest->post_content ?? '') === (string) ($post->post_content ?? '');

        if (!$sameTitle || !$sameContent) {
            return 'the newest revision of post ' . $postId
                . ' does not match its current version, so the overwritten content would not be retained; refusing';
        }

        return (int) $latest->ID;
    }

    /**
     * The newest genuine revision of a post, skipping autosaves.
     *
     * Autosaves live in the same table and are returned by
     * wp_get_post_revisions(); core distinguishes a real revision by the
     * "<parent>-revision" marker in post_name, and so does this.
     *
     * @param int $postId Parent post ID.
     * @return object|null
     */
    private function latestRevision(int $postId): ?object
    {
        $revisions = wp_get_post_revisions($postId);
        if (!is_array($revisions)) {
            return null;
        }

        foreach ($revisions as $revision) {
            if (!is_object($revision) || !isset($revision->post_name)) {
                continue;
            }
            $parent = isset($revision->post_parent) ? (int) $revision->post_parent : $postId;
            if (strpos((string) $revision->post_name, $parent . '-revision') !== false) {
                return $revision;
            }
        }

        return null;
    }

    /**
     * Normalises allowed_post_types, defaulting when the request omits it.
     *
     * An empty list is returned for a malformed value so the caller refuses
     * rather than silently falling back to the default, which would widen what
     * the request asked for.
     *
     * @param array<string,mixed> $params Request parameters.
     * @return list<string>
     */
    private function resolveAllowedTypes(array $params): array
    {
        if (!array_key_exists('allowed_post_types', $params)) {
            return self::DEFAULT_ALLOWED_POST_TYPES;
        }

        $raw = $params['allowed_post_types'];
        if (!is_array($raw) || $raw === []) {
            return [];
        }

        $types = [];
        foreach ($raw as $type) {
            if (!is_string($type) || $type === '') {
                return [];
            }
            $types[] = $type;
        }

        return $types;
    }

    /**
     * The refusal envelope every other command here uses.
     *
     * @param string $detail Human-readable reason.
     * @return array{ok:bool,detail:string}
     */
    private function fail(string $detail): array
    {
        return ['ok' => false, 'detail' => $detail];
    }
}
