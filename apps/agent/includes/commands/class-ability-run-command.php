<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Abilities\AbilityDenylist;
use WPMgr\Agent\Abilities\AbilityGuards;
use WPMgr\Agent\Abilities\AbilityInterception;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\AbilitySideEffects;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Abilities\ServicePrincipal;
use WPMgr\Agent\Abilities\VendorAbility;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * AbilityRunCommand: the engine's one signed command.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/ability_run
 *   body   { "p": "<exact JSON text>" }
 *   claims must carry `pd` = lowercase hex sha256 of the UTF-8 bytes of `p`.
 *
 * The digest is checked over the BYTES of `p`, before anything is decoded.
 * Only then is that same string decoded, to objects (never associative
 * arrays), so `{}` and `[]` stay distinct. There is no normalisation and no
 * re-encoding on the security path. `entry` and `input` travel inside `p` as
 * strings of exact JSON text for the same reason: `entry_sha256` is the hash of
 * the entry's bytes, and precheck binds the input's bytes.
 *
 * Decoded `p` fields:
 *   mode        "read" | "precheck" | "write" | "revert" | "ledger"
 *   request_id  UUID (every mode but read)
 *   entry       string, the catalogue entry JSON text (all but ledger)
 *   entry_sha256 hex sha256 of that text (all but ledger)
 *   input       string, JSON text of an object (default "{}"); revert takes none
 *   expected    object {precheck_digest, preview_digest} (write)
 *
 * WPMgr's own wpmgr/* abilities run through their own handlers. Any other
 * ability (a vendor's or core's) runs only in read mode, only on WordPress
 * 7.1+, only as the service principal, and only while the live ability still
 * matches its reviewed entry (see vendorRead()). Nothing is ever retried.
 *
 * The one write is wpmgr/page-create: it creates a DRAFT post or page owned
 * by the content service principal, never publishes, verifies the stored
 * bytes, and records a ledger row keyed by request_id. Its revert trashes
 * that draft, taking the post id only from the ledger row of the
 * token-bound request_id, and only while the draft is unchanged.
 */
final class AbilityRunCommand implements CommandInterface
{
    /** Largest accepted `p`, entry text and input text, in bytes. */
    private const MAX_P_BYTES     = 262144;
    private const MAX_ENTRY_BYTES = 65536;
    private const MAX_INPUT_BYTES = 65536;

    /** Largest output the agent will return. */
    private const MAX_OUTPUT_BYTES = 524288;

    private const RE_HEX64 = '/^[0-9a-f]{64}$/';
    private const RE_UUID  = '/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i';
    private const RE_NAME  = '/^[a-z0-9-]+\/[a-z0-9-]+$/';

    /** Post meta naming the request that created a post (unknown-outcome recovery). */
    public const META_CREATED_BY = '_wpmgr_created_by_request';

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'ability_run';
    }

    /**
     * Effect: the command family can write, so it declares the worst case even
     * though the modes in this slice only read.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Not safe to repeat: a write mode, when it exists, must never be resent.
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
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Decoded body.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        try {
            return $this->run($claims, $params);
        } catch (\Throwable $e) {
            return $this->fail('internal', 'the ability call failed unexpectedly', false);
        }
    }

    /**
     * @param array<string,mixed> $claims Claims.
     * @param array<string,mixed> $params Params.
     * @return array<string,mixed>
     */
    private function run(array $claims, array $params): array
    {
        if (defined('WPMGR_DISABLE_ABILITY_RUN') && constant('WPMGR_DISABLE_ABILITY_RUN')) {
            return $this->fail('disabled_on_site', 'ability_run is disabled on this site');
        }

        // 1. The digest, over bytes, before any decoding.
        $p = $params['p'] ?? null;
        if (!is_string($p) || $p === '' || count($params) !== 1) {
            return $this->fail('bad_params', 'the body must be exactly {"p": "<json text>"}');
        }
        if (strlen($p) > self::MAX_P_BYTES) {
            return $this->fail('params_too_large', 'p is too large');
        }
        $pd = $claims['pd'] ?? null;
        if (!is_string($pd) || preg_match(self::RE_HEX64, $pd) !== 1
            || !hash_equals($pd, hash('sha256', $p))) {
            return $this->fail('token_params_mismatch', 'the token is not bound to these parameters');
        }

        // 2. Decode that same string; objects, not arrays, so {} != [].
        $req = json_decode($p, false, 32);
        if (!is_object($req)) {
            return $this->fail('bad_params', 'p must be a JSON object');
        }

        $mode = $req->mode ?? null;
        if (!is_string($mode)) {
            return $this->fail('bad_mode', 'mode is required');
        }
        if (!in_array($mode, ['read', 'precheck', 'write', 'revert', 'ledger'], true)) {
            return $this->fail('bad_mode', 'unknown mode');
        }

        $requestId = $req->request_id ?? null;
        if ($mode !== 'read' || $requestId !== null) {
            if (!is_string($requestId) || preg_match(self::RE_UUID, $requestId) !== 1) {
                return $this->fail('bad_request_id', 'request_id must be a UUID');
            }
        }

        if ($mode === 'ledger') {
            return $this->ledger((string) $requestId);
        }

        // 3. The entry: hash of its exact text, then its content.
        $entryText = $req->entry ?? null;
        $entrySha  = $req->entry_sha256 ?? null;
        if (!is_string($entryText) || $entryText === '' || strlen($entryText) > self::MAX_ENTRY_BYTES) {
            return $this->fail('bad_entry', 'entry must be the entry JSON text');
        }
        if (!is_string($entrySha) || preg_match(self::RE_HEX64, $entrySha) !== 1
            || !hash_equals($entrySha, hash('sha256', $entryText))) {
            return $this->fail('integration_entry_changed', 'the entry does not match its hash');
        }
        $entry = json_decode($entryText, false, 32);
        if (!is_object($entry)) {
            return $this->fail('bad_entry', 'entry must be a JSON object');
        }
        $name = $entry->name ?? null;
        if (!is_string($name) || preg_match(self::RE_NAME, $name) !== 1) {
            return $this->fail('bad_ability_name', 'entry.name is not a valid ability name');
        }
        // Undo of what was already created stays available on a disabled
        // entry; every other mode needs it enabled.
        if (($entry->enabled ?? null) !== true && $mode !== 'revert') {
            return $this->fail('ability_disabled', 'the entry is not enabled');
        }
        if (($entry->status ?? null) !== 'admitted') {
            return $this->fail('ability_not_admitted', 'the entry is not admitted');
        }

        // 4. The code denylist: fail-closed, before scope, not reducible by data.
        if (AbilityDenylist::denies($name)) {
            return $this->fail('ability_denied', 'this ability is denied by the agent');
        }

        // 5. Scope: WPMgr's own abilities take their own path; any other
        //    ability can only be read, under the checks in vendorRead().
        if (strncmp($name, 'wpmgr/', 6) !== 0) {
            return $this->vendorRead($mode, $name, $entry, $entrySha, $req);
        }
        if (!OwnAbilities::has($name)) {
            return $this->fail('ability_unknown', 'this agent does not implement that ability');
        }
        // No default: an entry without a source is not a WPMgr entry.
        if (($entry->source ?? null) !== 'wpmgr') {
            return $this->fail('entry_source_mismatch', 'a wpmgr/ ability must have source wpmgr');
        }

        $class = $entry->class ?? null;
        if ($class !== OwnAbilities::abilityClass($name)) {
            return $this->fail('mode_class_mismatch', 'the entry class does not match the ability');
        }
        if ($mode === 'read' && $class !== 'read') {
            return $this->fail('mode_class_mismatch', 'read mode needs a read ability');
        }
        if ($mode === 'write' || $mode === 'revert') {
            if ($class !== 'write') {
                return $this->fail('mode_class_mismatch', 'write and revert modes need a write ability');
            }
            if ($name !== OwnAbilities::NAME_PAGE_CREATE) {
                return $this->fail('mode_not_available', 'this agent runs no other write ability');
            }
        }
        if ($class === 'write') {
            if (($entry->approval_mode ?? null) !== 'per_call') {
                return $this->fail('entry_approval_invalid', 'a write entry must require approval per call');
            }
            if (($entry->snapshot ?? null) !== 'created_post_trash') {
                return $this->fail('snapshot_strategy_invalid', 'page-create needs the created_post_trash snapshot strategy');
            }
        }

        if ($name === OwnAbilities::NAME_PAGE_CREATE) {
            return $this->pageCreate($mode, (string) $requestId, $entrySha, $req);
        }

        // 6. Input: exact text, an object.
        $inputText = $req->input ?? '{}';
        if (!is_string($inputText) || strlen($inputText) > self::MAX_INPUT_BYTES) {
            return $this->fail('bad_input', 'input must be JSON text of an object');
        }
        $input = json_decode($inputText, false, 32);
        if (!is_object($input)) {
            return $this->fail('bad_input', 'input must be a JSON object');
        }
        $bad = OwnAbilities::validate($name, $input);
        if ($bad !== null) {
            return $this->fail('bad_input', $bad);
        }

        if ($mode === 'precheck') {
            return $this->precheck($name, (string) $requestId, $entrySha, $inputText, $input);
        }

        return $this->read($name, $entrySha, $input);
    }

    // ---------------------------------------------------------------------
    // wpmgr/page-create
    // ---------------------------------------------------------------------

    /**
     * Dispatch page-create by mode.
     *
     * @param string $mode      Mode.
     * @param string $requestId Request id.
     * @param string $entrySha  Entry hash.
     * @param object $req       Decoded p.
     * @return array<string,mixed>
     */
    private function pageCreate(string $mode, string $requestId, string $entrySha, object $req): array
    {
        $inputText = $req->input ?? '{}';
        if (!is_string($inputText) || strlen($inputText) > self::MAX_INPUT_BYTES) {
            return $this->fail('bad_input', 'input must be JSON text of an object');
        }
        $input = json_decode($inputText, false, 32);
        if (!is_object($input)) {
            return $this->fail('bad_input', 'input must be a JSON object');
        }

        if ($mode === 'revert') {
            // W3: the object comes from the ledger row, never from input.
            if (get_object_vars($input) !== []) {
                return $this->fail('bad_input', 'revert takes no input; the object comes from the ledger');
            }

            return $this->pageCreateRevert($requestId);
        }
        if ($mode === 'read') {
            return $this->fail('mode_class_mismatch', 'read mode needs a read ability');
        }

        $checked = PageCreateBuilder::validate($input);
        if (!isset($checked['spec'])) {
            return $this->fail((string) ($checked['code'] ?? 'bad_input'), (string) ($checked['detail'] ?? 'invalid input'));
        }
        $spec     = $checked['spec'];
        $inputSha = hash('sha256', $inputText);

        if ($mode === 'precheck') {
            return $this->asPrincipal(function () use ($spec, $requestId, $entrySha, $inputSha): array {
                $built = $this->pageCreateBuild($spec);
                if (isset($built['refusal'])) {
                    return $built['refusal'];
                }

                return [
                    'ok'               => true,
                    'outcome'          => 'prechecked',
                    'mode'             => 'precheck',
                    'ability'          => OwnAbilities::NAME_PAGE_CREATE,
                    'request_id'       => $requestId,
                    'valid'            => true,
                    'base_fingerprint' => $built['base_fingerprint'],
                    'preview_digest'   => $built['preview_digest'],
                    'precheck_digest'  => $this->precheckDigest($entrySha, $inputSha, $built['base_fingerprint'], $built['preview_digest']),
                    'preview'          => [
                        'post_type' => $spec['post_type'],
                        'editor'    => $spec['editor'],
                        'status'    => 'draft',
                        'title'     => $spec['title'],
                        'content'   => $built['content'],
                    ],
                ];
            });
        }

        // mode === 'write'
        $expected = $req->expected ?? null;
        $expPre   = is_object($expected) ? ($expected->precheck_digest ?? null) : null;
        $expPrev  = is_object($expected) ? ($expected->preview_digest ?? null) : null;
        if (!is_string($expPre) || preg_match(self::RE_HEX64, $expPre) !== 1
            || !is_string($expPrev) || preg_match(self::RE_HEX64, $expPrev) !== 1) {
            return $this->fail('bad_expected', 'expected.precheck_digest and expected.preview_digest are required');
        }

        // 1. Idempotency (Sec-N2): a ledger row for this request is the answer.
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }

        // 2. The atomic claim (Sec-F3).
        if (!AbilityLedger::claimRequest($requestId)) {
            return $this->fail('request_in_flight', 'this request is already running');
        }
        try {
            return $this->asPrincipal(function (int $principal) use ($spec, $requestId, $entrySha, $inputSha, $expPre, $expPrev): array {
                return $this->pageCreateWrite($principal, $spec, $requestId, $entrySha, $inputSha, $expPre, $expPrev);
            });
        } finally {
            AbilityLedger::releaseRequest($requestId);
        }
    }

    /**
     * Render and simulate the save, as the principal. Writes nothing.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec Spec.
     * @return array{refusal?:array<string,mixed>,content:string,title:string,preview_digest:string,base_fingerprint:string}
     */
    private function pageCreateBuild(array $spec): array
    {
        $empty = ['content' => '', 'title' => '', 'preview_digest' => '', 'base_fingerprint' => ''];

        $blocksOn = $this->blockEditorFor($spec['post_type']);
        if ($blocksOn === null
            || ($spec['editor'] === PageCreateBuilder::EDITOR_BLOCKS) !== $blocksOn) {
            return ['refusal' => $this->fail('editor_unavailable', 'that editor is not available for this post type on this site')] + $empty;
        }

        $content = PageCreateBuilder::render($spec);
        $title   = PageCreateBuilder::storedTitle($spec['title']);
        $problem = PageCreateBuilder::retokenise($content);
        if ($problem !== null) {
            return ['refusal' => $this->fail('create_content_invalid', $problem)] + $empty;
        }

        // Simulate the save this principal gets (kses included). Any change
        // to our bytes refuses the write; a sanitiser never edits it for us.
        $simContent = wp_unslash(apply_filters('content_save_pre', wp_slash($content))); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- core's own save filter, applied to simulate exactly what wp_insert_post will do
        $simTitle   = wp_unslash(apply_filters('title_save_pre', wp_slash($title))); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- core's own save filter, applied to simulate exactly what wp_insert_post will do
        if ($simContent !== $content || wp_kses_post($content) !== $content || $simTitle !== $title) {
            return ['refusal' => $this->fail('sanitiser_changed_new_content', 'the site would change this content on save')] + $empty;
        }

        return [
            'content'          => $content,
            // The stored title is a fixed function of the plain title the
            // digest binds, so the digest still names exactly what is stored.
            'title'            => $title,
            'preview_digest'   => PageCreateBuilder::previewDigest($spec, $content),
            'base_fingerprint' => PageCreateBuilder::baseFingerprint($spec['post_type']),
        ];
    }

    /**
     * The write pipeline for page-create. Runs as the principal, under the
     * request claim.
     *
     * @param int                                                                                 $principal Service user id.
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec      Spec.
     * @param string                                                                              $requestId Request id.
     * @param string                                                                              $entrySha  Entry hash.
     * @param string                                                                              $inputSha  Input hash.
     * @param string                                                                              $expPre    Expected precheck digest.
     * @param string                                                                              $expPrev   Expected preview digest.
     * @return array<string,mixed>
     */
    private function pageCreateWrite(int $principal, array $spec, string $requestId, string $entrySha, string $inputSha, string $expPre, string $expPrev): array
    {
        // A replay that raced the first check sees the row now.
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }

        // 3. Re-check: what we would create now must be what was approved.
        $built = $this->pageCreateBuild($spec);
        if (isset($built['refusal'])) {
            return $built['refusal'];
        }
        $precheck = $this->precheckDigest($entrySha, $inputSha, $built['base_fingerprint'], $built['preview_digest']);
        if (!hash_equals($expPrev, $built['preview_digest']) || !hash_equals($expPre, $precheck)) {
            return $this->fail('preview_changed', 'what would be created differs from what was approved');
        }

        // 4. The ledger row exists before any effect: the idempotency key is
        //    durable, and created_post_id is filled right after the insert.
        $ledger = [
            'request_id'      => $requestId,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'entry_sha256'    => $entrySha,
            'snapshot'        => 'created_post_trash',
            'phase'           => 'inserting',
            'created_post_id' => 0,
            'before_fp'       => $built['base_fingerprint'],
            'after_fp'        => '',
            'preview_digest'  => $built['preview_digest'],
            'precheck_digest' => $precheck,
            'undo_state'      => 'none',
            'created_at'      => time(),
            'result'          => null,
        ];
        if (!AbilityLedger::create($requestId, $ledger)) {
            return $this->fail('snapshot_failed', 'the ledger row could not be written; nothing was created');
        }

        // 5. Create. Always a draft, always owned by the principal.
        $postId = wp_insert_post(wp_slash([
            'post_type'    => $spec['post_type'],
            'post_status'  => 'draft',
            'post_author'  => $principal,
            'post_title'   => $built['title'],
            'post_content' => $built['content'],
            'meta_input'   => [self::META_CREATED_BY => $requestId],
        ]), true);
        if (!is_int($postId) || $postId < 1) {
            $result = $this->fail('refused_by_site', 'WordPress did not create the draft');
            AbilityLedger::update($requestId, ['phase' => 'failed', 'result' => $result]);

            return $result;
        }
        if (!AbilityLedger::update($requestId, ['phase' => 'post_created', 'created_post_id' => $postId])) {
            // Undo could never find this draft, so it is not left behind.
            $trashed = $this->trashOwn($postId);
            $result  = $this->fail('snapshot_failed', 'the ledger could not record the created draft; it was moved to the trash', false, ['post_id' => $postId, 'trashed' => $trashed]);
            AbilityLedger::update($requestId, [
                'phase'           => 'failed',
                'created_post_id' => $postId,
                'undo_state'      => $trashed ? 'trashed' : 'none',
                'result'          => $result,
            ]);

            return $result;
        }

        // 6. Verify the stored post (Sec-B2 for creation).
        clean_post_cache($postId);
        $stored  = get_post($postId);
        $problem = $this->verifyCreated($stored, $spec, $built, $principal, $requestId);
        if ($problem !== null || !is_object($stored)) {
            $trashed = $this->trashOwn($postId);
            $result  = $this->fail('verify_mismatch', (string) $problem, false, ['post_id' => $postId, 'trashed' => $trashed]);
            AbilityLedger::update($requestId, [
                'phase'      => 'failed',
                'undo_state' => $trashed ? 'trashed' : 'none',
                'result'     => $result,
            ]);

            return $result;
        }

        $afterFp = PageCreateBuilder::documentFingerprint($stored);
        $result  = [
            'ok'             => true,
            'outcome'        => 'created',
            'mode'           => 'write',
            'ability'        => OwnAbilities::NAME_PAGE_CREATE,
            'request_id'     => $requestId,
            'post_id'        => $postId,
            'post_type'      => $spec['post_type'],
            'editor'         => $spec['editor'],
            'status'         => 'draft',
            'preview_digest' => $built['preview_digest'],
            'after_fp'       => $afterFp,
            'verify'         => ['content_equal' => true, 'title_equal' => true, 'status' => 'draft'],
        ];
        // The draft exists and its id is recorded, so the caller is told it
        // was created even when the completed state cannot be recorded; the
        // ledger then reports the created post id with no stored result.
        $recorded = AbilityLedger::update($requestId, [
            'phase'      => 'completed',
            'after_fp'   => $afterFp,
            'undo_state' => 'available',
            'result'     => $result,
        ]);
        if (!$recorded) {
            $result['ledger_recorded'] = false;
        }

        return $result;
    }

    /**
     * Why a just-created post is not exactly what we built, or null.
     *
     * @param mixed                                                                               $stored    Stored post.
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec      Spec.
     * @param array{content:string,title:string}                                                  $built     Built bytes.
     * @param int                                                                                 $principal Service user.
     * @param string                                                                              $requestId Request id.
     * @return string|null
     */
    private function verifyCreated($stored, array $spec, array $built, int $principal, string $requestId): ?string
    {
        if (!is_object($stored)) {
            return 'the created draft could not be read back';
        }
        $v = get_object_vars($stored);
        if ((string) ($v['post_status'] ?? '') !== 'draft') {
            return 'the created post is not a draft';
        }
        if ((string) ($v['post_type'] ?? '') !== $spec['post_type']) {
            return 'the created post has another type';
        }
        if ((int) ($v['post_author'] ?? 0) !== $principal) {
            return 'the created post has another author';
        }
        if ((string) ($v['post_title'] ?? '') !== $built['title']) {
            return 'the stored title differs from what was built';
        }
        if ((string) ($v['post_content'] ?? '') !== $built['content']) {
            return 'the stored content differs from what was built';
        }
        if ((string) get_post_meta((int) ($v['ID'] ?? 0), self::META_CREATED_BY, true) !== $requestId) {
            return 'the created post is not marked with this request';
        }

        return null;
    }

    /**
     * Person undo for page-create: trash the created draft, only if it is
     * unchanged since creation and still a draft. The post id comes only from
     * the ledger row of this token-bound request_id (W3).
     *
     * @param string $requestId Request id.
     * @return array<string,mixed>
     */
    private function pageCreateRevert(string $requestId): array
    {
        $row = AbilityLedger::get($requestId);
        if ($row === null) {
            return $this->fail('nothing_to_revert', 'there is no ledger row for this request');
        }
        if (($row['ability'] ?? null) !== OwnAbilities::NAME_PAGE_CREATE) {
            return $this->fail('ledger_ability_mismatch', 'the ledger row is for another ability');
        }
        if (($row['undo_state'] ?? null) === 'trashed') {
            return [
                'ok'         => true,
                'outcome'    => 'already_reverted',
                'mode'       => 'revert',
                'request_id' => $requestId,
                'post_id'    => (int) ($row['created_post_id'] ?? 0),
                'trashed'    => true,
            ];
        }
        $postId  = (int) ($row['created_post_id'] ?? 0);
        $afterFp = $row['after_fp'] ?? '';
        $phase   = $row['phase'] ?? null;
        if ($postId < 1 || !is_string($afterFp)) {
            return $this->fail('not_revertible', 'this request did not create a draft');
        }
        $completed = $phase === 'completed' && $afterFp !== '';
        // A draft whose write stopped before the completed record (interrupted,
        // or its cleanup failed) is still undoable, under a stricter unchanged
        // check because no after-fingerprint was recorded.
        $recovered = !$completed && $afterFp === ''
            && in_array($phase, ['post_created', 'failed'], true)
            && ($row['undo_state'] ?? null) === 'none';
        if (!$completed && !$recovered) {
            return $this->fail('not_revertible', 'this request did not create a draft that can be undone');
        }

        // A recovered draft may belong to a write still running: undo waits.
        if ($recovered && !AbilityLedger::claimRequest($requestId)) {
            return $this->fail('request_in_flight', 'this request is still running');
        }
        try {
            return $this->pageCreateRevertClaimed($requestId, $postId, $completed ? $afterFp : null);
        } finally {
            if ($recovered) {
                AbilityLedger::releaseRequest($requestId);
            }
        }
    }

    /**
     * Revert under the target claim. $afterFp null means no after-fingerprint
     * was recorded: the draft must then be authored by the principal and
     * never modified since insert.
     *
     * @param string      $requestId Request id.
     * @param int         $postId    Post id, from the ledger row.
     * @param string|null $afterFp   Recorded after-fingerprint, or null.
     * @return array<string,mixed>
     */
    private function pageCreateRevertClaimed(string $requestId, int $postId, ?string $afterFp): array
    {
        if (!AbilityLedger::claimTarget($postId)) {
            return $this->fail('target_in_flight', 'another engine call holds this post');
        }
        try {
            return $this->asPrincipal(function (int $principal) use ($requestId, $postId, $afterFp): array {
                clean_post_cache($postId);
                $post = get_post($postId);
                if (!is_object($post)) {
                    return $this->fail('created_post_missing', 'the created draft no longer exists');
                }
                $status = (string) $post->post_status;
                if (in_array($status, ['publish', 'future', 'private'], true)) {
                    return $this->fail('created_post_published', 'this page is published now; unpublish or trash it in WordPress');
                }
                if ($status !== 'draft') {
                    return $this->fail('conflict', 'the created post is no longer a draft');
                }
                if ((string) get_post_meta($postId, self::META_CREATED_BY, true) !== $requestId) {
                    return $this->fail('conflict', 'the post is not the one this request created');
                }
                if ($afterFp !== null && !hash_equals($afterFp, PageCreateBuilder::documentFingerprint($post))) {
                    return $this->fail('conflict', 'someone edited this draft after it was created');
                }
                if ($afterFp === null) {
                    if ((int) $post->post_author !== $principal) {
                        return $this->fail('conflict', 'the draft has another author now');
                    }
                    // Core stamps the modified dates equal to the dates on
                    // insert and moves them on every later save.
                    $v = get_object_vars($post);
                    if ((string) ($v['post_modified_gmt'] ?? '') !== (string) ($v['post_date_gmt'] ?? "\0")
                        || (string) ($v['post_modified'] ?? '') !== (string) ($v['post_date'] ?? "\0")) {
                        return $this->fail('conflict', 'someone edited this draft after it was created');
                    }
                }
                $touched = $this->touchedBySomeone($postId);
                if ($touched !== null) {
                    return $this->fail('created_post_touched', $touched);
                }

                if (!$this->trashOwn($postId)) {
                    return $this->fail('revert_failed', 'the draft could not be moved to the trash');
                }
                AbilityLedger::update($requestId, ['undo_state' => 'trashed', 'reverted_at' => time()]);

                return [
                    'ok'         => true,
                    'outcome'    => 'reverted',
                    'mode'       => 'revert',
                    'request_id' => $requestId,
                    'post_id'    => $postId,
                    'trashed'    => true,
                ];
            });
        } finally {
            AbilityLedger::releaseTarget($postId);
        }
    }

    /**
     * Undo refuses a draft a person has touched: any autosave (by any user),
     * any revision, or a live edit lock. Decided before anything is trashed.
     * Fails closed when the lock check cannot be loaded.
     *
     * @param int $postId Post id.
     * @return string|null Why it was refused, or null when untouched.
     */
    private function touchedBySomeone(int $postId): ?string
    {
        // User id 0 (the int) means an autosave by any user.
        if (wp_get_post_autosave($postId, 0) !== false) {
            return 'someone has unsaved changes to this draft; open it in WordPress';
        }
        $revisions = wp_get_post_revisions($postId, ['check_enabled' => false, 'numberposts' => 1]);
        if (!empty($revisions)) {
            return 'this draft has saved revisions; open it in WordPress';
        }
        if (!function_exists('wp_check_post_lock') && defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/post.php')) {
            require_once ABSPATH . 'wp-admin/includes/post.php';
        }
        if (!function_exists('wp_check_post_lock')) {
            return 'whether someone is editing this draft could not be checked';
        }
        if (wp_check_post_lock($postId) !== false) {
            return 'someone is editing this draft right now';
        }

        return null;
    }

    /**
     * Trash a post this request created (the ledger proves it), then confirm.
     *
     * @param int $postId Post id.
     * @return bool
     */
    private function trashOwn(int $postId): bool
    {
        wp_trash_post($postId);
        clean_post_cache($postId);
        $after = get_post($postId);

        // With trash disabled core deletes outright; gone counts as undone.
        return !is_object($after) || (string) $after->post_status === 'trash';
    }

    /**
     * Run $fn as the intact service principal, and always switch back to
     * user 0. Refuses when content editing is not enabled or caps drifted.
     *
     * @param callable(int):array<string,mixed> $fn Work.
     * @return array<string,mixed>
     */
    private function asPrincipal(callable $fn): array
    {
        $who = ServicePrincipal::resolve();
        if ($who['id'] < 1) {
            return $this->fail($who['code'], $who['detail']);
        }
        try {
            wp_set_current_user($who['id']);
            $live = ServicePrincipal::liveDrift();
            if ($live !== null) {
                return $this->fail('principal_capabilities_drifted', $live);
            }

            return $fn($who['id']);
        } finally {
            wp_set_current_user(0);
        }
    }

    /**
     * Is the block editor on for this post type? Null when it cannot be told.
     *
     * @param string $postType Post type.
     * @return bool|null
     */
    private function blockEditorFor(string $postType): ?bool
    {
        if (!function_exists('use_block_editor_for_post_type')) {
            $file = ABSPATH . 'wp-admin/includes/post.php';
            if (is_readable($file)) {
                require_once $file;
            }
        }
        if (!function_exists('use_block_editor_for_post_type')) {
            return null;
        }

        return (bool) use_block_editor_for_post_type($postType);
    }

    /**
     * @param string $entrySha Entry hash.
     * @param string $inputSha Input hash.
     * @param string $baseFp   Base fingerprint.
     * @param string $preview  Preview digest.
     * @return string
     */
    private function precheckDigest(string $entrySha, string $inputSha, string $baseFp, string $preview): string
    {
        return hash('sha256', (string) json_encode([$entrySha, $inputSha, $baseFp, $preview]));
    }

    /**
     * @param string              $requestId Request id.
     * @param array<string,mixed> $row       Ledger row.
     * @return array<string,mixed>
     */
    private function alreadyApplied(string $requestId, array $row): array
    {
        return [
            'ok'         => true,
            'outcome'    => 'already_applied',
            'mode'       => 'write',
            'request_id' => $requestId,
            'phase'      => (string) ($row['phase'] ?? ''),
            'post_id'    => (int) ($row['created_post_id'] ?? 0),
            'result'     => $row['result'] ?? null,
        ];
    }

    /**
     * Precheck: validates, executes nothing, writes nothing. The digest is a
     * sha256 over a JSON array of hex fields. No base fingerprint or preview
     * exists for a read ability, so those two fields are empty strings.
     *
     * @param string $name      Ability.
     * @param string $requestId Request id.
     * @param string $entrySha  Entry hash.
     * @param string $inputText Exact input text.
     * @param object $input     Decoded input.
     * @return array<string,mixed>
     */
    private function precheck(string $name, string $requestId, string $entrySha, string $inputText, object $input): array
    {
        $inputSha = hash('sha256', $inputText);

        return [
            'ok'               => true,
            'outcome'          => 'prechecked',
            'mode'             => 'precheck',
            'ability'          => $name,
            'request_id'       => $requestId,
            'valid'            => true,
            'base_fingerprint' => '',
            'preview_digest'   => '',
            'precheck_digest'  => $this->precheckDigest($entrySha, $inputSha, '', ''),
        ];
    }

    /**
     * Execute an own read ability, under the interception guards on WP 7.1+.
     *
     * @param string $name     Ability.
     * @param string $entrySha Entry hash.
     * @param object $input    Validated input.
     * @return array<string,mixed>
     */
    private function read(string $name, string $entrySha, object $input): array
    {
        $guards = new AbilityGuards();
        $armed  = AbilityGuards::supported();
        if ($armed) {
            $guards->arm($name);
        }
        $result = null;
        try {
            try {
                $result = OwnAbilities::run($name, $input);
            } catch (AbilityInterception $e) {
                // The guards recorded why; the violations below refuse.
                $result = null;
            }
            if ($armed) {
                // Before disarm: the end-of-call check needs the hooks in place.
                $guards->checkResult($result);
                $guards->finish();
            }
        } finally {
            if ($armed) {
                $guards->disarm();
            }
        }

        $violations = $armed ? $guards->violations() : [];
        if ($violations !== []) {
            return $this->fail('ability_intercepted', 'another plugin interfered with the call', false, ['violations' => $violations]);
        }
        if (isset($result['refusal'])) {
            return $this->fail($result['refusal']['code'], $result['refusal']['detail'], false);
        }

        $output  = $result['output'] ?? [];
        $encoded = json_encode($output);
        if (!is_string($encoded) || strlen($encoded) > self::MAX_OUTPUT_BYTES) {
            return $this->fail('output_too_large', 'the ability output exceeds the cap');
        }

        return [
            'ok'           => true,
            'outcome'      => 'completed',
            'mode'         => 'read',
            'ability'      => $name,
            'entry_sha256' => $entrySha,
            'output'       => $output,
        ];
    }

    /**
     * Read through a vendor or core ability.
     *
     * Order: the entry (source, class and mode, permission mode, output
     * shape), the WordPress floor, the live ability (present, plain class,
     * owner, owner version, schema), the input against the live schema,
     * then the call as the service principal under the interception guards
     * and the side-effect recorder. A recorded side effect or a guard
     * violation refuses the call and withholds the output; otherwise the
     * output is projected onto the entry's pinned shape and capped.
     *
     * @param string $mode     Mode.
     * @param string $name     Ability name.
     * @param object $entry    Decoded entry.
     * @param string $entrySha Entry hash.
     * @param object $req      Decoded p.
     * @return array<string,mixed>
     */
    private function vendorRead(string $mode, string $name, object $entry, string $entrySha, object $req): array
    {
        $bad = VendorAbility::entryRefusal($entry, $name, $mode);
        if ($bad !== null) {
            return $this->fail($bad['code'], $bad['detail']);
        }
        if (!AbilityGuards::supported()) {
            return $this->fail('wp_too_old_for_vendor_reads', 'reading through this ability needs WordPress 7.1 or later');
        }

        $inputText = $req->input ?? '{}';
        if (!is_string($inputText) || strlen($inputText) > self::MAX_INPUT_BYTES || !is_object(json_decode($inputText, false, 32))) {
            return $this->fail('bad_input', 'input must be JSON text of an object');
        }

        $ability = VendorAbility::resolve($name);
        if ($ability === null) {
            return $this->fail('ability_not_on_site', 'this site has no ability by that name');
        }
        $verified = VendorAbility::verify($ability, $entry);
        if (isset($verified['refusal'])) {
            return $this->fail($verified['refusal']['code'], $verified['refusal']['detail']);
        }
        $owner = $verified['owner'] ?? null;
        if ($owner === null) {
            return $this->fail('ability_owner_mismatch', 'the ability owner could not be resolved');
        }
        $prepared = VendorAbility::prepareInput($ability, $inputText);
        if (isset($prepared['refusal'])) {
            return $this->fail($prepared['refusal']['code'], $prepared['refusal']['detail']);
        }

        $limits  = is_object($entry->limits ?? null) ? $entry->limits : new \stdClass();
        $effects = new AbilitySideEffects(
            array_values((array) ($limits->allowed_option_patterns ?? [])),
            array_values((array) ($limits->http_hosts ?? []))
        );
        $nested  = array_values((array) ($entry->nested_allow ?? []));
        $input   = $prepared['input'] ?? null;
        $shape   = json_decode((string) json_encode($entry->output_fields ?? null), true);

        return $this->asPrincipal(function (int $principal) use ($ability, $name, $input, $nested, $effects, $entrySha, $owner, $shape): array {
            $call    = VendorAbility::call($ability, $name, $input, $nested, $effects);
            $same    = function_exists('get_current_user_id') && (int) get_current_user_id() === $principal;
            $refusal = VendorAbility::outcomeRefusal($call, $effects, $same);
            if ($refusal !== null) {
                return $this->fail($refusal['code'], $refusal['detail'], false, $refusal['extra']);
            }

            $raw     = (string) $call['json'];
            $decoded = json_decode($raw, true);
            if ($decoded === null) {
                return $this->fail('ability_output_invalid', 'the ability output could not be read back');
            }
            $output  = VendorAbility::project($decoded, $shape);
            $encoded = json_encode($output);
            if (!is_string($encoded)) {
                return $this->fail('ability_output_invalid', 'the ability output is not JSON-encodable');
            }
            if (strlen($encoded) > self::MAX_OUTPUT_BYTES) {
                return $this->fail('output_too_large', 'the ability output exceeds the cap');
            }

            return [
                'ok'                => true,
                'outcome'           => 'completed',
                'mode'              => 'read',
                'ability'           => $name,
                'entry_sha256'      => $entrySha,
                'owner'             => $owner,
                'abilities_invoked' => $call['invoked'],
                'output'            => $output,
            ];
        });
    }

    /**
     * Ledger state for a request: the row, the in-flight marker, and the
     * created-by-request lookup for unknown-outcome recovery.
     *
     * @param string $requestId Request id.
     * @return array<string,mixed>
     */
    private function ledger(string $requestId): array
    {
        $row = AbilityLedger::get($requestId);

        return [
            'ok'              => true,
            'outcome'         => 'ledger',
            'mode'            => 'ledger',
            'request_id'      => $requestId,
            'found'           => $row !== null,
            'inflight'        => AbilityLedger::inflight($requestId),
            'phase'           => $row !== null ? (string) ($row['phase'] ?? '') : null,
            'created_post_id' => $row !== null ? (int) ($row['created_post_id'] ?? 0) : null,
            'undo_state'      => $row !== null ? (string) ($row['undo_state'] ?? '') : null,
            'result'          => $row['result'] ?? null,
        ];
    }

    /**
     * @param string              $code      Refusal code.
     * @param string              $detail    Reason.
     * @param bool                $retryable Whether a retry can succeed.
     * @param array<string,mixed> $extra     Extra fields.
     * @return array<string,mixed>
     */
    private function fail(string $code, string $detail, bool $retryable = false, array $extra = []): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'refused',
            'code'      => $code,
            'detail'    => $detail,
            'retryable' => $retryable,
        ] + $extra;
    }
}
