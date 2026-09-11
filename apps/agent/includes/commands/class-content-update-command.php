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
 *     "post_id":              <int, required>,
 *     "expected_fingerprint": <string, required — see FINGERPRINT below>,
 *     "title":                <string, optional>,
 *     "content":              <string, optional>,
 *     "allowed_post_types":   <list<string>, optional; defaults to post + page>
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
 *     "fingerprint": <string — of the bytes RE-READ after the write>,
 *     "title_bytes":   <int>,
 *     "content_bytes": <int>,
 *     "ownership":   <object — see OWNERSHIP below>,
 *     "detail":      "updated"
 *   }
 *
 * Refusals follow the agent's { "ok": false, "detail": "<reason>" } envelope
 * and additionally carry "outcome" and "code". "outcome" is "failed" for every
 * refusal EXCEPT a fingerprint mismatch, which is "conflict": the post changed
 * under the caller and NOTHING was written. The two need different remedies —
 * a conflict means re-read and re-plan, a failure means the call itself was
 * wrong — so they are not the same outcome.
 *
 * FINGERPRINT. expected_fingerprint pins the version the caller believes it is
 * replacing. It is computed over the EXACT stored bytes of post_title and
 * post_content, length-framed so no title/content boundary shift can collide:
 *
 *   "sha256:" . sha256( "wpmgr.content_update.v1\n"
 *                       . strlen(title) . "\n" . title . "\n"
 *                       . strlen(content) . "\n" . content )
 *
 * Nothing is normalised, trimmed, entity-decoded or re-serialised first: a
 * fingerprint over a tidied form passes over exactly the whitespace-only and
 * encoding-only edits it exists to catch. The agent re-derives it from a
 * cache-cleaned read immediately before the write, and returns the fingerprint
 * of the bytes it reads back AFTER the write — never the value it was sent,
 * because a response that echoes the request cannot detect a write that did
 * not land.
 *
 * OWNERSHIP. A page builder keeps the canonical layout somewhere other than
 * post_content and filters the rendered output so its copy wins; writing
 * post_content on such a page changes a field nothing renders, and the write
 * still succeeds. This command therefore states what it actually checked:
 * block_document_checked is true (core has_blocks(), below), and
 * page_builder_checked is FALSE. A successful write here is not evidence that
 * the published page changed. Builder detection is a later slice and this
 * field is where its verdict goes; until it exists the field says so rather
 * than inventing a verdict.
 *
 * BLOCK DOCUMENTS are refused outright. The column IS the document, but it is
 * HTML framed by block-delimiter comments, and each block type's JavaScript
 * save routine defines the canonical markup for a given set of attributes.
 * Free-form HTML written into that document does not match what the routine
 * would emit, so the next human to open the editor — on a page they never
 * touched — is shown a block-recovery prompt, and accepting recovery DISCARDS
 * the block. The damage surfaces days later and nowhere near this command, so
 * the only safe answer at write time is to refuse.
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
     * Domain separator baked into every fingerprint.
     *
     * Versioned so that if the framing ever changes, an old fingerprint cannot
     * silently match a new one.
     */
    private const FINGERPRINT_DOMAIN = 'wpmgr.content_update.v1';

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

        if (!array_key_exists('expected_fingerprint', $params)) {
            return $this->fail(
                'missing required field: expected_fingerprint — the fingerprint of the '
                . 'title and content this call believes it is replacing',
                'missing_expected_fingerprint'
            );
        }
        if (!is_string($params['expected_fingerprint']) || $params['expected_fingerprint'] === '') {
            return $this->fail(
                'expected_fingerprint must be a non-empty string',
                'invalid_expected_fingerprint'
            );
        }
        $expected = (string) $params['expected_fingerprint'];

        $allowedTypes = $this->resolveAllowedTypes($params);
        if ($allowedTypes === []) {
            return $this->fail('allowed_post_types must be a non-empty list of strings');
        }

        // ------------------------------------------------------------------
        // 2. The post must exist, be out of the trash, and be an allowed type.
        // ------------------------------------------------------------------
        $post = $this->readStored($postId);
        if ($post === null) {
            return $this->fail('post not found: ' . $postId, 'post_not_found');
        }

        $postType   = isset($post->post_type) ? (string) $post->post_type : '';
        $postStatus = isset($post->post_status) ? (string) $post->post_status : '';

        if ($postStatus === 'trash') {
            return $this->fail('post ' . $postId . ' is in the trash; restore it before updating');
        }
        if (!in_array($postType, $allowedTypes, true)) {
            return $this->fail(
                'post ' . $postId . ' is of type "' . $postType . '", which this request did not allow',
                'post_type_not_allowed'
            );
        }

        // ------------------------------------------------------------------
        // 3. A block document is refused, and the refusal is terminal.
        //
        // has_blocks() is handed the stored STRING deliberately. Given
        // anything that is not a string it runs get_post() and returns false
        // unless the result is a WP_Post instance (wp-includes/blocks.php,
        // WordPress 7.1) — measured: an object of any other class answers
        // false. That failure direction is silent and permissive, which is the
        // one direction this guard must never fail in. The string branch skips
        // the instanceof entirely and tests the exact bytes already read.
        // ------------------------------------------------------------------
        if (has_blocks($this->storedContent($post))) {
            return $this->fail(
                'post ' . $postId . ' is a block document: its stored content carries block '
                . 'delimiters, which core has_blocks() detects. Free-form content cannot be '
                . 'written into a block document safely — the stored markup would no longer '
                . 'match what the block type\'s save routine emits, so the next person to open '
                . 'the editor is shown a block-recovery prompt on a page nobody touched, and '
                . 'accepting recovery discards the block. Do not retry this call in this form: '
                . 'no parameter forces it, and editing a block document needs the block-aware '
                . 'path, which does not exist yet.',
                'block_document_unsupported'
            );
        }

        // ------------------------------------------------------------------
        // 4. The caller must be replacing the version it thinks it is.
        // ------------------------------------------------------------------
        $current = $this->fingerprint($post);
        if (!hash_equals($current, $expected)) {
            return $this->conflict($postId, $expected, $current);
        }

        // ------------------------------------------------------------------
        // 5. Nothing is overwritten until the current version is retained.
        // ------------------------------------------------------------------
        $retained = $this->ensureRevisionRetained($post);
        if (!is_int($retained)) {
            return $this->fail($retained, 'retention_not_guaranteed');
        }

        // ------------------------------------------------------------------
        // 6. Re-derive immediately before the write. Step 5 runs core hooks
        //    (wp_save_post_revision fires _wp_put_post_revision and friends),
        //    and a third party is free to write the post from one of them, so
        //    the read the precondition was checked against is not necessarily
        //    still true here. This is the check that is load-bearing; step 4
        //    is the cheap one that refuses before taking a revision.
        // ------------------------------------------------------------------
        $post = $this->readStored($postId);
        if ($post === null) {
            return $this->fail('post ' . $postId . ' disappeared before the write', 'post_not_found');
        }

        $current = $this->fingerprint($post);
        if (!hash_equals($current, $expected)) {
            return $this->conflict($postId, $expected, $current);
        }

        // ------------------------------------------------------------------
        // 7. Sanitise, then write.
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
            return $this->fail('wp_update_post failed: ' . $result->get_error_message(), 'write_failed');
        }
        if (!is_int($result) && !(is_string($result) && ctype_digit($result))) {
            return $this->fail(
                'wp_update_post returned an unexpected value for post ' . $postId,
                'write_failed'
            );
        }
        if ((int) $result === 0) {
            return $this->fail('wp_update_post did not update post ' . $postId, 'write_failed');
        }

        // ------------------------------------------------------------------
        // 8. What is reported is what is STORED. Every field below comes from
        //    a fresh read, not from $update and not from the request: a
        //    response assembled out of the values that were sent cannot tell
        //    the difference between a write that landed and one that a filter,
        //    a builder or a failed query quietly dropped.
        // ------------------------------------------------------------------
        $stored = $this->readStored($postId);
        if ($stored === null) {
            return $this->fail(
                'post ' . $postId . ' could not be re-read after the write, so what is now '
                . 'stored cannot be reported; treat the result as unknown and re-read the post',
                'read_back_failed'
            );
        }

        return [
            'ok'            => true,
            'post_id'       => $postId,
            'post_type'     => $postType,
            'changed'       => $changed,
            'revision_id'   => $retained,
            'fingerprint'   => $this->fingerprint($stored),
            'title_bytes'   => strlen($this->storedTitle($stored)),
            'content_bytes' => strlen($this->storedContent($stored)),
            'ownership'     => $this->ownership(),
            'detail'        => 'updated',
        ];
    }

    /**
     * What this command verified about who owns the page, and what it did not.
     *
     * Stated rather than implied, because the honest answer is partial: the
     * document is not a block document, and whether a page builder owns the
     * layout is UNKNOWN. A builder keeps its canonical copy in post meta or
     * its own tables and filters the rendered output at request time, so a
     * write to post_content on such a page succeeds, is retained, is audited —
     * and changes nothing a visitor sees. Confirming that needs per-builder
     * storage facts verified against real installs, which this slice does not
     * have, and guessing them is how a fleet tool corrupts a customer's page.
     *
     * @return array<string,mixed>
     */
    private function ownership(): array
    {
        return [
            'block_document_checked' => true,
            'is_block_document'      => false,
            'page_builder_checked'   => false,
            'detail'                 => 'verified with core has_blocks() that post_content is not '
                . 'a block document; did NOT verify that no page builder owns this page. A builder '
                . 'that stores its layout outside post_content and filters the rendered output will '
                . 'leave this write invisible on the front end, and nothing here detects that yet.',
        ];
    }

    /**
     * Fingerprint of a post's stored title and content.
     *
     * Over the EXACT stored bytes, length-framed. No trim, no normalisation,
     * no entity decoding, no re-serialisation: a fingerprint over a tidied
     * form passes over precisely the whitespace-only and encoding-only edits
     * it exists to catch. The length prefixes stop a boundary shift (title
     * "ab" + content "c" vs title "a" + content "bc") from colliding.
     *
     * @param object $post Post row as stored.
     * @return string
     */
    private function fingerprint(object $post): string
    {
        $title   = $this->storedTitle($post);
        $content = $this->storedContent($post);

        return 'sha256:' . hash(
            'sha256',
            self::FINGERPRINT_DOMAIN . "\n"
            . strlen($title) . "\n" . $title . "\n"
            . strlen($content) . "\n" . $content
        );
    }

    /**
     * Reads a post from the database, bypassing the object cache.
     *
     * clean_post_cache() first, deliberately. get_post() answers from the
     * object cache, and on a site running a PERSISTENT one (Redis, Memcached)
     * that entry can outlive the request that filled it. A precondition
     * checked against a cached copy would confirm a version the database no
     * longer holds, which is the one way this guard could pass while being
     * wrong. The cost is that clean_post_cache() fires its action on a call
     * that may yet refuse; that action is core's own post-invalidated signal
     * and costs a cache purge, which is cheap next to a lost edit.
     *
     * @param int $postId Post ID.
     * @return object|null The post, or null when it is not there.
     */
    private function readStored(int $postId): ?object
    {
        clean_post_cache($postId);

        $post = get_post($postId);
        if (!is_object($post) || !isset($post->ID) || (int) $post->ID !== $postId) {
            return null;
        }

        return $post;
    }

    /**
     * A post's stored title, as bytes.
     *
     * @param object $post Post row.
     * @return string
     */
    private function storedTitle(object $post): string
    {
        return (string) ($post->post_title ?? '');
    }

    /**
     * A post's stored content, as bytes.
     *
     * @param object $post Post row.
     * @return string
     */
    private function storedContent(object $post): string
    {
        return (string) ($post->post_content ?? '');
    }

    /**
     * The conflict envelope: the post moved under the caller, nothing written.
     *
     * Deliberately NOT a failure. A failure says the call was wrong and may be
     * worth retrying as sent; a conflict says the call was fine but the world
     * moved, and retrying it unchanged would overwrite whatever the other
     * writer just did. The remedies are opposite, so the outcomes are
     * different values and not two shades of the same one.
     *
     * @param int    $postId   Post ID.
     * @param string $expected Fingerprint the caller sent.
     * @param string $current  Fingerprint of what is stored now.
     * @return array<string,mixed>
     */
    private function conflict(int $postId, string $expected, string $current): array
    {
        return [
            'ok'                   => false,
            'outcome'              => 'conflict',
            'code'                 => 'expected_fingerprint_mismatch',
            'written'              => false,
            'post_id'              => $postId,
            'expected_fingerprint' => $expected,
            'current_fingerprint'  => $current,
            'detail'               => 'post ' . $postId . ' changed after the caller read it: the '
                . 'stored title and content do not match expected_fingerprint. Nothing was '
                . 'written. This is a conflict, not a failure — re-read the post, re-plan the '
                . 'edit against what it holds now, and send a fresh expected_fingerprint. '
                . 'Retrying this request unchanged would overwrite the other writer\'s edit.',
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
     * The refusal envelope every other command here uses, plus the machine
     * fields a caller needs to branch on.
     *
     * "outcome" is always present so a caller can switch on one field rather
     * than infer the kind of refusal from prose; it is "failed" here and
     * "conflict" only in conflict(), which is the distinction that matters.
     *
     * @param string $detail Human-readable reason.
     * @param string $code   Stable machine code for the refusal.
     * @return array{ok:bool,outcome:string,code:string,detail:string}
     */
    private function fail(string $detail, string $code = 'failed'): array
    {
        return [
            'ok'      => false,
            'outcome' => 'failed',
            'code'    => $code,
            'detail'  => $detail,
        ];
    }
}
