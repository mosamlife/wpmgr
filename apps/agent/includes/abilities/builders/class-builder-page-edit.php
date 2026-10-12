<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-edit: the precheck and the write of one batch of operations on
 * a page-builder draft WPMgr created for the AI.
 *
 * The command keeps the modes, the expected digests, idempotency, the
 * request claim and the principal; everything from the input to the answer
 * is here.
 *
 * plan() writes nothing. In order, the first that fails refusing:
 *
 *   1. the input passes PageEditValidator::parse() (bad_input, ops_invalid);
 *   2. the post is a draft DraftEligibility admits with the signed list
 *      (target_not_eligible, the reason as detail);
 *   3. a page builder the entry enables and this agent has compiled in holds
 *      the post's layout (target_not_eligible, not_builder_page or
 *      owner_unknown; the registry's refusal when none can be used), and can
 *      be used on the site now (builder_not_available), and the site's posts
 *      and postmeta tables support transactions, so a failed write is put
 *      back all or nothing (builder_not_available, detail
 *      tables_not_transactional, or table_engine_unreadable when that
 *      cannot be read; BuilderDocumentRestore::transactionProblem());
 *   4. the post can take an edit (ElementorDocument::editTargetProblem()):
 *      someone's autosave or someone else's edit lock is conflict with
 *      detail autosave_pending or editor_open, an unreadable element tree is
 *      data_unreadable, every other reason target_not_eligible with the
 *      reason as detail;
 *   5. the page's builder_document_v1 fingerprint is the input's
 *      base_fingerprint (conflict, changed_since_read);
 *   6. the operations hold against the page's projection
 *      (PageEditValidator::againstPage(), op_index naming the operation);
 *   7. the new images resolve through the command's media resolver;
 *   8. the adapter plans the edit over the stored tree (planEdit()), node
 *      ids from the request id;
 *   9. Elementor would store the edited page as planned
 *      (ElementorDocument::precheckEditTree()).
 *
 * The preview:
 *
 *   {post_id, builder, builder_version, format, title, changes,
 *    after_outline, tree_sha256, tree}
 *
 * title is the stored post title (site text); changes is planEdit()'s, one
 * per operation; after_outline the projection of the edited tree as
 * wpmgr/page-structure answers nodes (Projection::toArray(), at most
 * BuilderContract::MAX_STRUCTURE_NODES nodes and MAX_STRUCTURE_BYTES);
 * tree the whole edited tree and tree_sha256 the sha256 of its canonical
 * bytes (json_encode, default flags).
 *
 *   preview_digest = sha256(json_encode([DIGEST_DOMAIN, builder,
 *                    builder_version, post_id, base_fingerprint, tree_json]))
 *
 * json_encode with default flags; tree_json is the edited tree's canonical
 * bytes. The same request, input and stored page give the same bytes, so a
 * write's re-plan matches the approved precheck.
 *
 * Every refusal carries "retryable", true only for conflict with detail
 * editor_open: an edit lock lapses on its own once the editor is closed, so
 * the same call can succeed later. Every other refusal, conflict with
 * autosave_pending or changed_since_read included, needs a person or a new
 * read first, and is false.
 *
 * write() re-plans under a claim on the target post, requires both digests
 * to be the approved ones, records the ledger row, snapshots the page
 * (BuilderDocumentSnapshot), checks the snapshot is the page the approval
 * saw, reads the live page's fingerprint again immediately before the save
 * (conflict, changed_since_read, when another save landed after the
 * snapshot; nothing is written or put back), saves the edited tree through
 * the adapter, and reads the whole page
 * back (verifyEdited()). Any failure after the snapshot puts the page back
 * from it (BuilderDocumentRestore::full()) and answers whether it did
 * ("restored"); a page that could not be put back is restore_mismatch. A
 * write that completes records what it changed, each meta key and posts
 * column with the hash of its bytes before and after
 * (BuilderDocumentRestore::changes()), the fingerprints, the snapshot's hash
 * and the revisions its save made, and answers "applied". The stored
 * result carries the snapshot's hash, so a lost reply recovered from the
 * ledger carries it too.
 *
 * revert() is a person's undo of one applied edit. The control plane signs
 * back the snapshot hash the write answered, and the undo runs only when
 * that hash, the hash on the ledger row and the hash of the snapshot's
 * stored bytes are one hash (snapshot_tampered otherwise; nothing is
 * written). The post, and the keys and columns to put back, come only from
 * the ledger row. Only what the edit wrote goes back, and only while each
 * of those keys and columns still holds what the edit left, so edits are
 * undone newest first and a later change to anything else on the page, a
 * featured image say, is kept.
 */
final class BuilderPageEdit
{
    /** First member of the preview digest's array. */
    public const DIGEST_DOMAIN = 'wpmgr.page_edit.v1';

    /** The refusal for a post outside what the edit covers. */
    public const CODE_NOT_ELIGIBLE = 'target_not_eligible';

    /** The refusal for a page that moved on, or is open, since it was read. */
    public const CODE_CONFLICT = 'conflict';

    /** conflict: the page's fingerprint is not the base_fingerprint. */
    public const CHANGED_SINCE_READ = 'changed_since_read';

    /** The snapshot strategy of every page-edit ledger row. */
    public const SNAPSHOT = 'builder_document';

    /** The format every adapter edits in. */
    public const FORMAT = 'classic';

    /** The one member of a revert's signed parameters: the snapshot's hash. */
    public const REVERT_HASH = 'snapshot_sha256';

    /** revert(): the snapshot, its ledger record and the signed hash are not one. */
    public const CODE_TAMPERED = 'snapshot_tampered';

    /** revert(): the page is published, scheduled or private now. */
    public const CODE_PUBLISHED = 'refused_published';

    /** revert(): the page is not a draft now, or is gone. */
    public const CODE_NOT_DRAFT = 'target_not_draft';

    /** undo_state of an edit whose undo started and may have put the page back. */
    public const UNDO_RESTORING = 'restoring';

    /** Post statuses a revert answers CODE_PUBLISHED for. */
    private const PUBLISHED = ['publish', 'future', 'private'];

    /** Codes a failed save may answer with; anything else is builder_save_refused. */
    private const SAVE_CODES = [
        ElementorDocument::CODE_REFUSED,
        ElementorDocument::CODE_CRASHED,
        ElementorDocument::CODE_SIDE_EFFECT,
    ];

    /** editTargetProblem() reasons answered as conflict, with the reason as detail. */
    private const CONFLICTS = [ElementorDocument::TARGET_AUTOSAVE, ElementorDocument::TARGET_LOCKED];

    /**
     * Plan the edit and answer the preview. Writes nothing; runs as the
     * principal.
     *
     * On a refusal only "refusal" carries meaning.
     *
     * @param string                                  $inputText    The exact input text.
     * @param object                                  $entry        The catalogue entry, decoded with objects.
     * @param array<mixed>                            $allowedIds   The drafts the signed parameters name (DraftEligibility::signedIds()).
     * @param string                                  $requestId    The request; new node ids derive from it.
     * @param callable(list<int>): array<string,mixed> $media        Resolves attachment ids to image facts: {facts} or {refusal}.
     * @param array<string, BuilderAdapter>|null      $compiledSeam Tests only: stands in for the compiled set. Production passes none.
     * @return array<string, mixed> {refusal} or {post_id, adapter, base_fingerprint, builder_version, preview, preview_digest, doc, changes, touched}
     */
    public static function plan(string $inputText, object $entry, array $allowedIds, string $requestId, callable $media, ?array $compiledSeam = null): array
    {
        $parsed = PageEditValidator::parse($inputText);
        if (!isset($parsed['input'])) {
            return self::refused((string) ($parsed['code'] ?? 'bad_input'), (string) ($parsed['detail'] ?? 'the input is not a page-edit input'));
        }
        $input  = $parsed['input'];
        $postId = $input['post_id'];

        $eligibility = DraftEligibility::check($postId, $allowedIds);
        if (!$eligibility['eligible']) {
            return self::refused(self::CODE_NOT_ELIGIBLE, $eligibility['reason']);
        }

        $owner = self::owner($entry, $postId, $compiledSeam);
        if (!isset($owner['adapter'])) {
            return ['refusal' => self::fail((string) ($owner['code'] ?? self::CODE_NOT_ELIGIBLE), (string) ($owner['detail'] ?? 'not_builder_page'))];
        }
        $a = $owner['adapter'];
        if (!$a instanceof ElementorAdapter) {
            return self::refused(AdapterStatus::CODE, 'not_compiled');
        }
        $status = $a->status();
        $usable = $status->refusal();
        if ($usable !== null) {
            return self::refused($usable['code'], $usable['detail']);
        }
        $version = $status->version;
        if (!is_string($version) || $version === '') {
            return self::refused(AdapterStatus::CODE, 'version_unverified');
        }
        $tables = BuilderDocumentRestore::transactionProblem();
        if ($tables !== null) {
            return self::refused(AdapterStatus::CODE, $tables);
        }

        $target = $a->document()->editTargetProblem($postId, $a->facts());
        if ($target !== null) {
            if (in_array($target, self::CONFLICTS, true)) {
                return self::refused(self::CODE_CONFLICT, $target);
            }

            return $target === LayoutOps::CODE_UNREADABLE
                ? self::refused(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read')
                : self::refused(self::CODE_NOT_ELIGIBLE, $target);
        }

        $keys = $a->descriptor()->exactKeys;
        try {
            $stored = BuilderDocumentFingerprint::read($postId, $keys);
            if ($stored === null) {
                return self::refused(self::CODE_NOT_ELIGIBLE, 'missing');
            }
            $baseFp = BuilderDocumentFingerprint::compute($stored['post'], $stored['rows'], $keys);
        } catch (\Throwable $e) {
            return self::refused(LayoutOps::CODE_UNREADABLE, 'the page could not be read');
        }
        if (!hash_equals($input['base_fingerprint'], $baseFp)) {
            return self::refused(self::CODE_CONFLICT, self::CHANGED_SINCE_READ);
        }
        $tree = $a->storedTree($stored['rows']);
        if ($tree === null) {
            return self::refused(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read');
        }

        try {
            $nodes = $a->project($tree)->nodes();
            $rule  = PageEditValidator::againstPage($input, $nodes, $a->capabilities());
        } catch (\InvalidArgumentException $e) {
            return self::refused(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read');
        }
        if ($rule !== null) {
            return ['refusal' => self::fail($rule['code'], $rule['detail'], ['op_index' => $rule['op_index']])];
        }

        $mediaById = self::media($input['operations'], $media);
        if (isset($mediaById['refusal'])) {
            return ['refusal' => $mediaById['refusal']];
        }

        try {
            $planned = $a->planEdit($postId, $input['operations'], new IdSeed($requestId), $mediaById['by_id']);
        } catch (\Throwable $e) {
            return self::refused('bad_input', 'the operations could not be applied');
        }
        $doc = $planned['doc'] ?? null;
        if (!$doc instanceof NativeDocument || !isset($planned['changes'], $planned['touched'])) {
            return ['refusal' => self::fail((string) ($planned['code'] ?? 'bad_input'), (string) ($planned['detail'] ?? 'the operations could not be applied'), ['op_index' => $planned['op_index'] ?? null])];
        }
        $problem = $a->document()->precheckEditTree($tree, $doc->tree, $planned['touched']);
        if ($problem !== null) {
            return self::refused($problem['code'], $problem['detail']);
        }

        try {
            $after  = $a->project($doc->tree)->toArray(BuilderContract::MAX_STRUCTURE_NODES);
            $digest = self::previewDigest($a->id(), $version, $postId, $baseFp, $doc->canonicalBytes);
        } catch (\InvalidArgumentException | \JsonException $e) {
            return self::refused(LayoutOps::CODE_UNREADABLE, 'the edited page could not be encoded');
        }

        return [
            'post_id'          => $postId,
            'adapter'          => $a,
            'base_fingerprint' => $baseFp,
            'builder_version'  => $version,
            'preview'          => [
                'post_id'         => $postId,
                'builder'         => $a->id(),
                'builder_version' => $version,
                'format'          => self::FORMAT,
                'title'           => $stored['post']['post_title'],
                'changes'         => $planned['changes'],
                'after_outline'   => $after,
                'tree_sha256'     => hash('sha256', $doc->canonicalBytes),
                'tree'            => $doc->tree,
            ],
            'preview_digest'   => $digest,
            'doc'              => $doc,
            'changes'          => $planned['changes'],
            'touched'          => $planned['touched'],
        ];
    }

    /**
     * Apply an approved edit. Runs as the principal, under the command's
     * request claim, after the command found no ledger row for the request.
     *
     * $precheckDigest computes the command's precheck digest for a base
     * fingerprint and a preview digest.
     *
     * @param string                                  $inputText      The exact input text.
     * @param object                                  $entry          The catalogue entry.
     * @param array<mixed>                            $allowedIds     The drafts the signed parameters name at dispatch.
     * @param string                                  $requestId      The request.
     * @param string                                  $entrySha       The entry's hash.
     * @param string                                  $expPre         The approved precheck digest.
     * @param string                                  $expPrev        The approved preview digest.
     * @param callable(string, string): string         $precheckDigest (base_fingerprint, preview_digest) => precheck digest.
     * @param callable(list<int>): array<string,mixed> $media          Resolves attachment ids to image facts.
     * @param array<string, BuilderAdapter>|null      $compiledSeam   Tests only.
     * @return array<string, mixed>
     */
    public static function write(string $inputText, object $entry, array $allowedIds, string $requestId, string $entrySha, string $expPre, string $expPrev, callable $precheckDigest, callable $media, ?array $compiledSeam = null): array
    {
        $parsed = PageEditValidator::parse($inputText);
        if (!isset($parsed['input'])) {
            return self::fail((string) ($parsed['code'] ?? 'bad_input'), (string) ($parsed['detail'] ?? 'the input is not a page-edit input'));
        }
        $postId = $parsed['input']['post_id'];
        if (!AbilityLedger::claimTarget($postId)) {
            return self::fail('target_in_flight', 'another engine call holds this post');
        }
        try {
            // Eligibility is checked again with the list the control plane
            // signed for this dispatch, inside the claim.
            $plan = self::plan($inputText, $entry, $allowedIds, $requestId, $media, $compiledSeam);
            if (isset($plan['refusal'])) {
                return $plan['refusal'];
            }
            $precheck = $precheckDigest($plan['base_fingerprint'], $plan['preview_digest']);
            if (!hash_equals($expPrev, $plan['preview_digest']) || !hash_equals($expPre, $precheck)) {
                return self::fail('preview_changed', 'the edit would differ from what was approved');
            }

            $row = [
                'request_id'      => $requestId,
                'ability'         => OwnAbilities::NAME_PAGE_EDIT,
                'entry_sha256'    => $entrySha,
                'snapshot'        => self::SNAPSHOT,
                'phase'           => 'started',
                'target_post_id'  => $postId,
                'before_fp'       => $plan['base_fingerprint'],
                'after_fp'        => '',
                'preview_digest'  => $plan['preview_digest'],
                'precheck_digest' => $precheck,
                'undo_state'      => 'none',
                'created_at'      => time(),
                'result'          => null,
            ];
            if (!AbilityLedger::create($requestId, $row)) {
                return self::fail('snapshot_failed', 'the ledger row could not be written; nothing was changed');
            }

            return self::apply($postId, $requestId, $plan, static fn (array $fields): bool => AbilityLedger::update($requestId, $fields));
        } finally {
            AbilityLedger::releaseTarget($postId);
        }
    }

    /**
     * The preview digest.
     *
     * @param string $builder  Adapter id.
     * @param string $version  Builder version.
     * @param int    $postId   Post id.
     * @param string $baseFp   The page's fingerprint before the edit.
     * @param string $treeJson The edited tree's canonical bytes.
     * @return string
     * @throws \JsonException When a value is not valid UTF-8.
     */
    public static function previewDigest(string $builder, string $version, int $postId, string $baseFp, string $treeJson): string
    {
        return hash('sha256', json_encode([self::DIGEST_DOMAIN, $builder, $version, $postId, $baseFp, $treeJson], JSON_THROW_ON_ERROR));
    }

    /**
     * The snapshot hash a revert's signed parameters carry: p.revert must be
     * exactly {"snapshot_sha256": <lowercase sha256 hex>}. Null for anything
     * else.
     *
     * @param mixed $revert The decoded p.revert.
     * @return string|null
     */
    public static function revertHash(mixed $revert): ?string
    {
        if (!is_object($revert)) {
            return null;
        }
        $members = get_object_vars($revert);
        $hash    = $members[self::REVERT_HASH] ?? null;
        if (array_keys($members) !== [self::REVERT_HASH] || !is_string($hash)) {
            return null;
        }

        return preg_match('/^[0-9a-f]{64}$/D', $hash) === 1 ? $hash : null;
    }

    /**
     * A person's undo of one applied edit. Runs as the principal. The post,
     * the snapshot and what goes back come from the request's ledger row and
     * the snapshot stored under the request, never from the call.
     *
     * In order, the first that fails refusing, with nothing written:
     *
     *   1. the request has a page-edit ledger row (nothing_to_revert,
     *      ledger_ability_mismatch); an undone edit answers already_reverted;
     *   2. the edit completed and its undo is open (not_revertible);
     *   3. the hash on the ledger row, $signedHash and the hash of the
     *      snapshot's stored bytes are one hash, and those bytes are this
     *      request's snapshot of the row's post (snapshot_tampered;
     *      snapshot_unreadable when the database cannot answer);
     *   4. this agent has the edit's page builder compiled in
     *      (builder_not_available);
     *   5. no other engine call holds the post (target_in_flight);
     *   6. the post is a draft (refused_published when it is published,
     *      scheduled or private; target_not_draft otherwise, or when it is
     *      gone);
     *   7. nobody has the post open or unsaved changes to it (conflict, with
     *      editor_open or autosave_pending as detail);
     *   8. every meta key and posts column the edit wrote still holds what the
     *      edit left (conflict, changed_after_this_change).
     *
     * Then those keys and columns, and nothing else, go back to the snapshot
     * (BuilderDocumentRestore::scoped(); restore_mismatch when they do not
     * read back as they were), and the ledger row records the undo and the
     * page's fingerprint after it.
     *
     * Immediately before the restore the ledger row's undo_state becomes
     * UNDO_RESTORING; an undo that cannot record that is data_unreadable and
     * writes nothing. An undo that put the page back but cannot record it is
     * never answered as done: it is data_unreadable with restored true, and
     * the row keeps UNDO_RESTORING. The next undo of the edit then finishes
     * it: when every key and column the edit wrote already holds what it held
     * before the edit, nothing is written and the undo is recorded; otherwise
     * the undo runs as above. A refusal that wrote nothing leaves undo_state
     * as the undo found it. The row's undo_state is read again inside the
     * claim on the post, from the options table rather than this request's
     * cached copy, so an undo another request finished while this one waited
     * for the claim answers already_reverted and writes nothing.
     *
     * @param string                             $requestId    The token-bound request.
     * @param string                             $signedHash   The snapshot hash the signed parameters carry (revertHash()).
     * @param array<string, BuilderAdapter>|null $compiledSeam Tests only: stands in for the compiled set. Production passes none.
     * @return array<string, mixed>
     */
    public static function revert(string $requestId, string $signedHash, ?array $compiledSeam = null): array
    {
        $row = AbilityLedger::get($requestId);
        if ($row === null) {
            return self::fail('nothing_to_revert', 'there is no ledger row for this request');
        }
        if (($row['ability'] ?? null) !== OwnAbilities::NAME_PAGE_EDIT) {
            return self::fail('ledger_ability_mismatch', 'the ledger row is for another ability');
        }
        $postId = $row['target_post_id'] ?? null;
        if (!is_int($postId) || $postId < 1) {
            return self::fail('not_revertible', 'this request has no change that can be undone');
        }
        if (($row['undo_state'] ?? null) === 'restored') {
            return ['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => $requestId, 'post_id' => $postId, 'restored' => true];
        }
        $keys    = $row['changed_keys'] ?? null;
        $fields  = $row['changed_fields'] ?? null;
        $builder = $row['builder'] ?? null;
        if (($row['phase'] ?? null) !== 'completed' || !in_array($row['undo_state'] ?? null, ['available', self::UNDO_RESTORING], true)
            || !is_array($keys) || !is_array($fields) || !is_string($builder)) {
            return self::fail('not_revertible', 'this request has no change that can be undone');
        }

        $echoed   = self::echoedSnapshot($requestId, $postId, $row['snapshot_sha256'] ?? null, $signedHash);
        $snapshot = $echoed['snapshot'] ?? null;
        if ($snapshot === null) {
            return $echoed['refusal'] ?? self::fail(self::CODE_TAMPERED, 'the copy of the page kept for this change is not the one recorded for it; nothing was changed');
        }
        $a = BuilderRegistry::compiledAdapter($builder, $compiledSeam);
        if (!$a instanceof ElementorAdapter) {
            return self::fail(AdapterStatus::CODE, 'not_compiled');
        }

        if (!AbilityLedger::claimTarget($postId)) {
            return self::fail('target_in_flight', 'another engine call holds this post');
        }
        try {
            // Inside the claim: the row as the last undo of this edit left it,
            // read from the table, not from this request's cache.
            $state = (AbilityLedger::getStored($requestId) ?? [])['undo_state'] ?? null;
            if ($state === 'restored') {
                return ['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => $requestId, 'post_id' => $postId, 'restored' => true];
            }
            if ($state !== 'available' && $state !== self::UNDO_RESTORING) {
                return self::fail('not_revertible', 'this request has no change that can be undone');
            }
            try {
                $stored = BuilderDocumentFingerprint::read($postId, []);
            } catch (\Throwable $e) {
                return self::fail(LayoutOps::CODE_UNREADABLE, 'the page could not be read');
            }
            if ($stored === null) {
                return self::fail(self::CODE_NOT_DRAFT, 'the page no longer exists');
            }
            $status = $stored['post']['post_status'];
            if (in_array($status, self::PUBLISHED, true)) {
                return self::fail(self::CODE_PUBLISHED, 'this page is published now; change it in WordPress');
            }
            if ($status !== 'draft') {
                return self::fail(self::CODE_NOT_DRAFT, 'the page is no longer a draft');
            }
            $open = $a->document()->openProblem($postId);
            if ($open !== null) {
                return self::fail(self::CODE_CONFLICT, $open);
            }

            // Recorded before anything is written, so an undo whose own record
            // is lost after the page is back is finished by the next undo.
            $resume = $state === self::UNDO_RESTORING;
            if (!$resume && !AbilityLedger::update($requestId, ['undo_state' => self::UNDO_RESTORING])) {
                return self::fail(LayoutOps::CODE_UNREADABLE, 'the undo could not be recorded; nothing was changed', ['post_id' => $postId]);
            }
            try {
                $problem = BuilderDocumentRestore::scoped($postId, $snapshot, $keys, $fields, $a->descriptor(), $a, $resume);
            } catch (\InvalidArgumentException $e) {
                $problem = ['code' => 'not_revertible', 'detail' => 'this request has no change that can be undone'];
            }
            if ($problem !== null) {
                if (!$resume && $problem['detail'] !== BuilderDocumentRestore::DETAIL_READ_BACK) {
                    // Nothing was written: the undo is open as it was.
                    AbilityLedger::update($requestId, ['undo_state' => 'available']);
                }

                return self::fail($problem['code'], $problem['detail'], ['post_id' => $postId]);
            }

            try {
                $restoredFp = BuilderDocumentFingerprint::ofPost($postId, $a->descriptor()->exactKeys);
            } catch (\Throwable $e) {
                $restoredFp = null;
            }
            if (!AbilityLedger::update($requestId, ['undo_state' => 'restored', 'reverted_at' => time(), 'restored_fp' => $restoredFp])) {
                // The row keeps UNDO_RESTORING: the next undo records it.
                return self::fail(LayoutOps::CODE_UNREADABLE, 'the page was put back, but the undo could not be recorded; undo again to finish it', ['post_id' => $postId, 'restored' => true]);
            }

            return ['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => $requestId, 'post_id' => $postId, 'restored' => true];
        } finally {
            AbilityLedger::releaseTarget($postId);
        }
    }

    /**
     * Snapshot, save, verify and record, after the ledger row exists. Any
     * failure after the snapshot puts the page back.
     *
     * @param int                                  $postId       The target post.
     * @param string                               $requestId    The request.
     * @param array<string, mixed>                 $plan         What plan() answered for this write.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    private static function apply(int $postId, string $requestId, array $plan, callable $ledgerUpdate): array
    {
        $a      = $plan['adapter'] ?? null;
        $doc    = $plan['doc'] ?? null;
        $baseFp = $plan['base_fingerprint'] ?? null;
        if (!$a instanceof BuilderAdapter || !$doc instanceof NativeDocument || !is_string($baseFp)) {
            return self::failed($ledgerUpdate, self::fail('bad_input', 'the edit was not planned for this write; nothing was changed'));
        }

        try {
            $taken = BuilderDocumentSnapshot::take($postId, $requestId);
        } catch (\Throwable $e) {
            $taken = ['ok' => false, 'code' => 'snapshot_failed'];
        }
        if (($taken['ok'] ?? null) !== true || !isset($taken['sha256'], $taken['bytes'])) {
            $code = ($taken['code'] ?? null) === 'snapshot_too_large' ? 'snapshot_too_large' : 'snapshot_failed';

            return self::failed($ledgerUpdate, self::fail($code, $code === 'snapshot_too_large'
                ? 'the page is too large to keep a copy of; nothing was changed'
                : 'a copy of the page could not be kept; nothing was changed'));
        }
        $sha = $taken['sha256'];
        if (!$ledgerUpdate(['phase' => 'snapshot_taken', 'snapshot_sha256' => $sha, 'snapshot_bytes' => $taken['bytes']])) {
            return self::failed($ledgerUpdate, self::fail('snapshot_failed', 'the copy of the page could not be recorded; nothing was changed'));
        }

        // The snapshot must be the page the approval saw: the bytes stored,
        // decoded for this request and post, with the approved fingerprint.
        $before = self::loadSnapshot($requestId, $postId, $sha);
        if ($before === null) {
            return self::failed($ledgerUpdate, self::fail('snapshot_failed', 'the copy of the page could not be read back; nothing was changed'));
        }
        if (self::snapshotFingerprint($before, $a->descriptor()->exactKeys) !== $baseFp) {
            return self::failed($ledgerUpdate, self::fail(self::CODE_CONFLICT, self::CHANGED_SINCE_READ));
        }

        // Immediately before the save, the live page must still be the page
        // the approval saw: a save by anyone else since the copy was taken is
        // refused, never overwritten.
        try {
            $liveFp = BuilderDocumentFingerprint::ofPost($postId, $a->descriptor()->exactKeys);
        } catch (\Throwable $e) {
            return self::failed($ledgerUpdate, self::fail(LayoutOps::CODE_UNREADABLE, 'the page could not be read; nothing was changed'));
        }
        if ($liveFp === null || !hash_equals($baseFp, $liveFp)) {
            return self::failed($ledgerUpdate, self::fail(self::CODE_CONFLICT, self::CHANGED_SINCE_READ));
        }

        $saved = $a->write($postId, $doc);
        $scope = is_array($saved['scope'] ?? null) ? $saved['scope'] : [];
        $own   = self::scopeRevisions($scope);
        if (($saved['ok'] ?? null) !== true) {
            $code = $saved['code'] ?? null;
            $code = is_string($code) && in_array($code, self::SAVE_CODES, true) ? $code : ElementorDocument::CODE_REFUSED;
            $why  = $saved['detail'] ?? null;

            return self::putBack($postId, $before, $a, $baseFp, $own, $code, is_string($why) && $why !== '' ? $why : 'the page builder did not save the page', $ledgerUpdate);
        }

        $problem = $a->verifyEdited($postId, $doc, $before);
        if ($problem !== null) {
            return self::putBack($postId, $before, $a, $baseFp, $own, 'verify_mismatch', 'the edited page is not what was planned: ' . $problem, $ledgerUpdate);
        }
        try {
            $written = $scope['target_meta_keys'] ?? [];
            $changed = BuilderDocumentRestore::changes($postId, $before, is_array($written) ? $written : []);
            $afterFp = BuilderDocumentFingerprint::ofPost($postId, $a->descriptor()->exactKeys);
        } catch (\Throwable $e) {
            $changed = null;
            $afterFp = null;
        }
        if ($changed === null || $afterFp === null) {
            return self::putBack($postId, $before, $a, $baseFp, $own, 'verify_mismatch', 'the edited page is not what was planned: post_unreadable', $ledgerUpdate);
        }

        $result = [
            'ok'              => true,
            'outcome'         => 'applied',
            'mode'            => 'write',
            'ability'         => OwnAbilities::NAME_PAGE_EDIT,
            'request_id'      => $requestId,
            'post_id'         => $postId,
            'before_fp'       => $baseFp,
            'after_fp'        => $afterFp,
            'snapshot_sha256' => $sha,
            'preview_digest'  => $plan['preview_digest'],
            'changes_applied' => count($plan['changes']),
        ];
        $recorded = $ledgerUpdate([
            'phase'            => 'completed',
            'after_fp'         => $afterFp,
            'undo_state'       => 'available',
            'builder'          => $a->id(),
            'builder_version'  => $plan['builder_version'],
            'format'           => self::FORMAT,
            'snapshot_sha256'  => $sha,
            'changed_keys'     => $changed['changed_keys'],
            'changed_fields'   => $changed['changed_fields'],
            'own_revision_ids' => $own,
            'touched_refs'     => array_values($plan['touched']),
            'result'           => $result,
        ]);
        if (!$recorded) {
            // A change the ledger cannot hold could never be undone.
            return self::putBack($postId, $before, $a, $baseFp, $own, 'snapshot_failed', 'the change could not be recorded', $ledgerUpdate);
        }

        return $result;
    }

    /**
     * The adapter whose builder holds the post's layout, among the builders
     * the entry enables that this agent knows and has compiled in.
     *
     * @param object                             $entry        The catalogue entry.
     * @param int                                $postId       The post.
     * @param array<string, BuilderAdapter>|null $compiledSeam Tests only.
     * @return array{adapter?: BuilderAdapter, code?: string, detail?: string}
     */
    private static function owner(object $entry, int $postId, ?array $compiledSeam): array
    {
        $ids = OwnAbilities::buildersEnabled($entry);
        if ($ids === null) {
            return ['code' => 'bad_input', 'detail' => 'the entry\'s limits.builders_enabled is not a list of page builder ids'];
        }
        $limits   = get_object_vars($entry)['limits'] ?? null;
        $refusal  = null;
        $resolved = false;
        $unknown  = false;
        foreach ($ids as $id) {
            if (!in_array($id, BuilderRegistry::IDS, true)) {
                continue;
            }
            $r = BuilderRegistry::resolve('builder:' . $id, $limits, $compiledSeam);
            if (!isset($r['adapter'])) {
                $refusal ??= $r;
                continue;
            }
            $resolved = true;
            $owns     = $r['adapter']->owns($postId);
            if ($owns === true) {
                return ['adapter' => $r['adapter']];
            }
            $unknown = $unknown || $owns === null;
        }
        if ($resolved) {
            return ['code' => self::CODE_NOT_ELIGIBLE, 'detail' => $unknown ? 'owner_unknown' : 'not_builder_page'];
        }

        return $refusal ?? ['code' => 'builder_not_enabled', 'detail' => 'the catalogue entry enables no page builder'];
    }

    /**
     * The new images of the operations' outlines, resolved, with the media
     * library's alt text, by attachment id.
     *
     * @param list<array<string, mixed>>               $ops   Parsed operations.
     * @param callable(list<int>): array<string,mixed> $media The resolver.
     * @return array{by_id: array<int, array<string, mixed>>, refusal?: array<string, mixed>}
     */
    private static function media(array $ops, callable $media): array
    {
        $outline = [];
        foreach ($ops as $op) {
            if (is_array($op['outline'] ?? null)) {
                array_push($outline, ...array_values($op['outline']));
            }
        }
        $ids = PageCreateBuilder::mediaIds(['outline' => $outline]);
        if ($ids === []) {
            return ['by_id' => []];
        }
        $resolved = $media($ids);
        $facts    = $resolved['facts'] ?? null;
        if (!is_array($facts)) {
            $refusal = $resolved['refusal'] ?? null;

            return ['by_id' => [], 'refusal' => is_array($refusal) ? $refusal : self::fail('image_not_available', 'an image could not be resolved')];
        }
        $byId = [];
        foreach ($facts as $fact) {
            if (!is_array($fact) || PageCreateBuilder::mediaFactProblem($fact) !== null) {
                return ['by_id' => [], 'refusal' => self::fail('image_not_available', 'the image facts are incomplete')];
            }
            $alt = get_post_meta($fact['id'], BuilderPageCreate::ALT_META_KEY, true);
            if (is_string($alt)) {
                $fact['alt'] = $alt;
            }
            $byId[$fact['id']] = $fact;
        }

        return ['by_id' => $byId];
    }

    /**
     * The request's snapshot for a revert, decoded, when the hash on its
     * ledger row, the signed hash and the hash of its stored bytes are one
     * hash and the bytes are this request's snapshot of $postId; otherwise
     * the refusal.
     *
     * @param string $requestId  The request.
     * @param int    $postId     The post the ledger row names.
     * @param mixed  $recorded   The ledger row's snapshot_sha256.
     * @param string $signedHash The hash the signed parameters carry.
     * @return array{snapshot?: array<string, mixed>, refusal?: array<string, mixed>}
     */
    private static function echoedSnapshot(string $requestId, int $postId, mixed $recorded, string $signedHash): array
    {
        try {
            $loaded = BuilderDocumentSnapshot::load($requestId);
        } catch (\InvalidArgumentException $e) {
            $loaded = [];
        }
        if (($loaded['code'] ?? null) === 'snapshot_unreadable') {
            return self::refused('snapshot_unreadable', 'the copy of the page kept for this change could not be read; nothing was changed');
        }
        $json     = $loaded['json'] ?? null;
        $computed = $loaded['sha256'] ?? null;
        $tampered = self::refused(self::CODE_TAMPERED, 'the copy of the page kept for this change is not the one recorded for it; nothing was changed');
        if (!is_string($recorded) || !is_string($json) || !is_string($computed)) {
            return $tampered;
        }
        if (!hash_equals($recorded, $signedHash) || !hash_equals($signedHash, $computed) || !hash_equals($recorded, $computed)) {
            return $tampered;
        }
        $snapshot = BuilderDocumentSnapshot::decode($json, $requestId, $postId);

        return $snapshot === null ? $tampered : ['snapshot' => $snapshot];
    }

    /**
     * The request's snapshot as stored, when its bytes still hash to $sha,
     * decoded for this request and post; null otherwise.
     *
     * @param string $requestId The request.
     * @param int    $postId    The post.
     * @param string $sha       The hash take() answered.
     * @return array<string, mixed>|null
     */
    private static function loadSnapshot(string $requestId, int $postId, string $sha): ?array
    {
        try {
            $loaded = BuilderDocumentSnapshot::load($requestId);
            $json   = $loaded['json'] ?? null;
            if (!is_string($json) || !hash_equals($sha, (string) ($loaded['sha256'] ?? ''))) {
                return null;
            }

            return BuilderDocumentSnapshot::decode($json, $requestId, $postId);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * The builder_document_v1 fingerprint of a decoded snapshot, or null
     * when its rows cannot be fingerprinted.
     *
     * @param array<string, mixed> $snapshot Decoded snapshot.
     * @param list<string>         $keys     The descriptor keys.
     * @return string|null
     */
    private static function snapshotFingerprint(array $snapshot, array $keys): ?string
    {
        $post = $snapshot['post'] ?? null;
        $meta = $snapshot['meta'] ?? null;
        if (!is_array($post) || !is_array($meta)) {
            return null;
        }
        $fields = [];
        foreach (BuilderDocumentFingerprint::POST_FIELDS as $field) {
            $fields[$field] = $post[$field] ?? null;
        }
        foreach (BuilderDocumentFingerprint::PLACEMENT_FIELDS as $field) {
            $fields[$field] = is_string($post[$field] ?? null) ? (int) $post[$field] : null;
        }
        $rows = [];
        foreach ($meta as $pair) {
            if (is_array($pair) && in_array($pair[0] ?? null, $keys, true)) {
                $rows[$pair[0]][] = $pair[1] ?? null;
            }
        }
        try {
            return BuilderDocumentFingerprint::compute($fields, $rows, $keys);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * Put the page back from its snapshot after a failed save or read-back,
     * record the failure, and answer it.
     *
     * @param int                                  $postId       The target post.
     * @param array<string, mixed>                 $before       The decoded snapshot.
     * @param BuilderAdapter                       $a            The adapter.
     * @param string                               $baseFp       The page's fingerprint before the write.
     * @param list<int>                            $own          Revisions the save made.
     * @param string                               $code         What failed.
     * @param string                               $detail       Why, WPMgr's own words.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    private static function putBack(int $postId, array $before, BuilderAdapter $a, string $baseFp, array $own, string $code, string $detail, callable $ledgerUpdate): array
    {
        try {
            $restored = BuilderDocumentRestore::full($postId, $before, $a->descriptor(), $a, $baseFp) === null;
        } catch (\Throwable $e) {
            $restored = false;
        }
        $result = $restored
            ? self::fail($code, $detail, ['post_id' => $postId, 'restored' => true])
            : self::fail(BuilderDocumentRestore::CODE_MISMATCH, 'the page could not be put back as it was after: ' . $code, ['post_id' => $postId, 'restored' => false]);
        $ledgerUpdate(['phase' => 'failed', 'restored' => $restored, 'own_revision_ids' => $own, 'result' => $result]);

        return $result;
    }

    /**
     * Record a failure before anything was changed, and answer it.
     *
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @param array<string, mixed>                 $result       The refusal.
     * @return array<string, mixed>
     */
    private static function failed(callable $ledgerUpdate, array $result): array
    {
        $ledgerUpdate(['phase' => 'failed', 'result' => $result]);

        return $result;
    }

    /**
     * The revision ids a save's write scope recorded.
     *
     * @param array<mixed> $scope The save's scope outcome.
     * @return list<int>
     */
    private static function scopeRevisions(array $scope): array
    {
        $ids = $scope['revision_ids'] ?? [];
        $out = [];
        foreach (is_array($ids) ? $ids : [] as $id) {
            if (is_int($id) && $id > 0 && !in_array($id, $out, true)) {
                $out[] = $id;
            }
        }

        return $out;
    }

    /**
     * A plan() refusal.
     *
     * @param string $code   Refusal code.
     * @param string $detail Reason, WPMgr's own words or a token.
     * @return array{refusal: array<string, mixed>}
     */
    private static function refused(string $code, string $detail): array
    {
        return ['refusal' => self::fail($code, $detail)];
    }

    /**
     * A refusal in the command's shape. retryable is true only for conflict
     * with detail editor_open.
     *
     * @param string               $code   Refusal code.
     * @param string               $detail Reason.
     * @param array<string, mixed> $extra  Extra fields.
     * @return array<string, mixed>
     */
    private static function fail(string $code, string $detail, array $extra = []): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'refused',
            'code'      => $code,
            'detail'    => $detail,
            'retryable' => $code === self::CODE_CONFLICT && $detail === ElementorDocument::TARGET_LOCKED,
        ] + $extra;
    }
}
