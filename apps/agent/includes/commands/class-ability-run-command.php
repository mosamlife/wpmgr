<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Abilities\AbilityDenylist;
use WPMgr\Agent\Abilities\AbilityGuards;
use WPMgr\Agent\Abilities\AbilityInterception;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\AbilitySideEffects;
use WPMgr\Agent\Abilities\Builders\BuilderAdapter;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\BuilderPageStructure;
use WPMgr\Agent\Abilities\Builders\BuilderRegistry;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Abilities\RestCall;
use WPMgr\Agent\Abilities\RestGuards;
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
 *   allowed_draft_ids  list of at most one post id: the drafts the control
 *               plane names as WPMgr's, for wpmgr/page-structure and
 *               wpmgr/page-edit (optional)
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
 *
 * A page-builder editor ("builder:<id>") resolves through BuilderRegistry
 * against the entry's limits.builders_enabled before anything is built; the
 * build, the save and the undo guard of a builder draft are
 * BuilderPageCreate's. Idempotency, the claims, the digest re-check and the
 * ledger row are the same for every editor.
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
     * @param (\Closure(): array<string, BuilderAdapter>)|null $builderSeam Tests only: builds stand-ins for the compiled page builder adapters, afresh for each call. Production passes none.
     */
    public function __construct(private readonly ?\Closure $builderSeam = null)
    {
    }

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
            if (!in_array($name, [OwnAbilities::NAME_PAGE_CREATE, OwnAbilities::NAME_REST_WRITE, OwnAbilities::NAME_PAGE_EDIT], true)) {
                return $this->fail('mode_not_available', 'this agent runs no other write ability');
            }
        }
        if ($class === 'write') {
            if (($entry->approval_mode ?? null) !== 'per_call') {
                return $this->fail('entry_approval_invalid', 'a write entry must require approval per call');
            }
            $want = [OwnAbilities::NAME_REST_WRITE => 'post_fields', OwnAbilities::NAME_PAGE_EDIT => BuilderPageEdit::SNAPSHOT][$name] ?? 'created_post_trash';
            if (($entry->snapshot ?? null) !== $want) {
                return $this->fail('snapshot_strategy_invalid', 'this write needs the ' . $want . ' snapshot strategy');
            }
        }

        if ($name === OwnAbilities::NAME_PAGE_CREATE) {
            return $this->pageCreate($mode, (string) $requestId, $entrySha, $req, $entry);
        }
        if ($name === OwnAbilities::NAME_PAGE_EDIT) {
            return $this->pageEdit($mode, (string) $requestId, $entrySha, $req, $entry);
        }
        if ($name === OwnAbilities::NAME_REST_READ || $name === OwnAbilities::NAME_REST_WRITE) {
            return $this->restCall($mode, $name, (string) $requestId, $entrySha, $req);
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
        if ($name === OwnAbilities::NAME_PAGE_STRUCTURE) {
            return $this->pageStructure($entry, $entrySha, $input, $req);
        }

        return $this->read($name, $entrySha, $input);
    }

    /**
     * wpmgr/page-structure: the drafts the signed parameters name and the
     * adapter the entry enables, then the read (BuilderPageStructure).
     *
     * @param object $entry    The catalogue entry.
     * @param string $entrySha Entry hash.
     * @param object $input    Validated input.
     * @param object $req      Decoded p.
     * @return array<string,mixed>
     */
    private function pageStructure(object $entry, string $entrySha, object $input, object $req): array
    {
        $allowed = DraftEligibility::signedIds($req);
        if ($allowed === null) {
            return $this->fail('bad_params', 'allowed_draft_ids must be a list of at most one post id');
        }
        $resolved = BuilderPageStructure::adapterFor($entry, $this->builderSeam === null ? null : ($this->builderSeam)());
        $adapter  = $resolved['adapter'] ?? null;
        if ($adapter === null) {
            return $this->fail((string) ($resolved['code'] ?? 'builder_not_enabled'), (string) ($resolved['detail'] ?? 'no page builder can be used'));
        }

        return $this->read(OwnAbilities::NAME_PAGE_STRUCTURE, $entrySha, $input, static fn (): array => BuilderPageStructure::run($input, $allowed, $adapter));
    }

    /**
     * wpmgr/page-edit by mode: the input text, the drafts the signed
     * parameters name, then BuilderPageEdit's precheck, or its write under
     * the request claim after the replay check. Undo of an edit is not
     * offered by this agent.
     *
     * @param string $mode      Mode.
     * @param string $requestId Request id.
     * @param string $entrySha  Entry hash.
     * @param object $req       Decoded p.
     * @param object $entry     The catalogue entry.
     * @return array<string,mixed>
     */
    private function pageEdit(string $mode, string $requestId, string $entrySha, object $req, object $entry): array
    {
        if ($mode === 'revert') {
            return $this->fail('mode_not_available', 'this agent does not undo a page edit');
        }
        $inputText = $req->input ?? null;
        if (!is_string($inputText) || strlen($inputText) > self::MAX_INPUT_BYTES) {
            return $this->fail('bad_input', 'input must be JSON text of an object');
        }
        $allowed = DraftEligibility::signedIds($req);
        if ($allowed === null) {
            return $this->fail('bad_params', 'allowed_draft_ids must be a list of at most one post id');
        }
        $seam     = $this->builderSeam === null ? null : ($this->builderSeam)();
        $media    = fn (array $ids): array => $this->pageCreateMedia($ids);
        $inputSha = hash('sha256', $inputText);

        if ($mode === 'precheck') {
            return $this->asPrincipal(function () use ($inputText, $entry, $allowed, $requestId, $media, $seam, $entrySha, $inputSha): array {
                $plan = BuilderPageEdit::plan($inputText, $entry, $allowed, $requestId, $media, $seam);
                if (isset($plan['refusal'])) {
                    return $plan['refusal'];
                }

                return [
                    'ok'               => true,
                    'outcome'          => 'prechecked',
                    'mode'             => 'precheck',
                    'ability'          => OwnAbilities::NAME_PAGE_EDIT,
                    'request_id'       => $requestId,
                    'valid'            => true,
                    'base_fingerprint' => $plan['base_fingerprint'],
                    'preview_digest'   => $plan['preview_digest'],
                    'precheck_digest'  => $this->precheckDigest($entrySha, $inputSha, $plan['base_fingerprint'], $plan['preview_digest']),
                    'preview'          => $plan['preview'],
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
        BuilderDocumentSnapshot::sweep();
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }
        if (!AbilityLedger::claimRequest($requestId)) {
            return $this->fail('request_in_flight', 'this request is already running');
        }
        try {
            return $this->asPrincipal(function () use ($inputText, $entry, $allowed, $requestId, $media, $seam, $entrySha, $inputSha, $expPre, $expPrev): array {
                $row = AbilityLedger::get($requestId);
                if ($row !== null) {
                    return $this->alreadyApplied($requestId, $row);
                }
                $digest = fn (string $baseFp, string $previewDigest): string => $this->precheckDigest($entrySha, $inputSha, $baseFp, $previewDigest);

                return BuilderPageEdit::write($inputText, $entry, $allowed, $requestId, $entrySha, $expPre, $expPrev, $digest, $media, $seam);
            });
        } finally {
            AbilityLedger::releaseRequest($requestId);
        }
    }

    // ---------------------------------------------------------------------
    // wpmgr/rest-read and wpmgr/rest-write
    // ---------------------------------------------------------------------

    /**
     * One reviewed REST route. RC1 runs in full before anything dispatches:
     * the route row against its hash, its class against the entry, the input
     * against the row (typed path, query and body; no raw route), the code
     * denylists, then the call as the service principal under the REST
     * guards. Reads and prechecks also run under the side-effect recorder.
     *
     * @param string $mode      Mode.
     * @param string $name      Ability.
     * @param string $requestId Request id ('' for a read).
     * @param string $entrySha  Entry hash.
     * @param object $req       Decoded p.
     * @return array<string,mixed>
     */
    private function restCall(string $mode, string $name, string $requestId, string $entrySha, object $req): array
    {
        if ($mode === 'revert') {
            // W3: the post and the prior values come from the ledger row only.
            $inputText = $req->input ?? '{}';
            $decoded   = is_string($inputText) ? json_decode($inputText, false, 4) : null;
            if (!is_object($decoded) || get_object_vars($decoded) !== []) {
                return $this->fail('bad_input', 'revert takes no input; the post comes from the ledger');
            }

            return $this->restRevert($requestId);
        }
        $allowed = $name === OwnAbilities::NAME_REST_READ ? ['read'] : ['precheck', 'write'];
        if (!in_array($mode, $allowed, true)) {
            return $this->fail('mode_class_mismatch', 'this mode is not available for ' . $name);
        }

        $routeSha = $req->route_sha256 ?? null;
        $parsed   = RestCall::parseRoute($req->route ?? null, $routeSha, $name, AbilityGuards::wpVersion());
        if (!isset($parsed['route'])) {
            return $this->fail($parsed['refusal']['code'] ?? 'route_not_reviewed', $parsed['refusal']['detail'] ?? 'the route was refused');
        }
        $route     = $parsed['route'];
        $inputText = $req->input ?? null;
        $checked   = RestCall::parseInput($inputText, $route);
        if (!isset($checked['call'])) {
            return $this->fail($checked['refusal']['code'] ?? 'bad_input', $checked['refusal']['detail'] ?? 'the input was refused');
        }
        $call     = $checked['call'];
        $routeSha = (string) $routeSha;
        $inputSha = hash('sha256', (string) $inputText);

        if ($mode === 'read') {
            return $this->restRead($route, $call, $entrySha, $routeSha);
        }
        if ($mode === 'precheck') {
            return $this->asPrincipal(function (int $principal) use ($route, $call, $requestId, $entrySha, $routeSha, $inputSha): array {
                $effects = new AbilitySideEffects();
                $effects->arm();
                try {
                    $out = $this->restPrecheck($route, $call, $requestId, $entrySha, $routeSha, $inputSha);
                } finally {
                    $effects->disarm();
                }
                if ($effects->detected()) {
                    return $this->fail('read_side_effect_detected', 'the precheck changed the site or called out', false, ['side_effects' => $effects->details()]);
                }
                $intact = $this->principalRefusal($principal);

                return $intact ?? $out;
            });
        }

        // mode === 'write'
        $expected = $req->expected ?? null;
        $expPre   = is_object($expected) ? ($expected->precheck_digest ?? null) : null;
        $expBase  = is_object($expected) ? ($expected->base_fingerprint ?? null) : null;
        if (!is_string($expPre) || preg_match(self::RE_HEX64, $expPre) !== 1
            || !is_string($expBase) || preg_match(self::RE_HEX64, $expBase) !== 1) {
            return $this->fail('bad_expected', 'expected.precheck_digest and expected.base_fingerprint are required');
        }

        // 1. Idempotency (Sec-N2).
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }
        // 2. The claims (Sec-F3): the request, then the target post.
        if (!AbilityLedger::claimRequest($requestId)) {
            return $this->fail('request_in_flight', 'this request is already running');
        }
        try {
            $postId = $call['target_id'];
            if (!AbilityLedger::claimTarget($postId)) {
                return $this->fail('target_in_flight', 'another engine call holds this post');
            }
            try {
                return $this->asPrincipal(function () use ($route, $call, $requestId, $entrySha, $routeSha, $inputSha, $expPre, $expBase): array {
                    return $this->restWrite($route, $call, $requestId, $entrySha, $routeSha, $inputSha, $expPre, $expBase);
                });
            } finally {
                AbilityLedger::releaseTarget($postId);
            }
        } finally {
            AbilityLedger::releaseRequest($requestId);
        }
    }

    /**
     * A read: the call as the principal, under the REST guards and the
     * side-effect recorder; published content only; output projected onto
     * the row's fields and capped.
     *
     * @param array<string,mixed>                                                                 $route    Route.
     * @param array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int} $call     Call.
     * @param string                                                                              $entrySha Entry hash.
     * @param string                                                                              $routeSha Route hash.
     * @return array<string,mixed>
     */
    private function restRead(array $route, array $call, string $entrySha, string $routeSha): array
    {
        return $this->asPrincipal(function (int $principal) use ($route, $call, $entrySha, $routeSha): array {
            $effects = new AbilitySideEffects();
            $effects->arm();
            try {
                $d = $this->restDispatch($route, $call);
            } finally {
                $effects->disarm();
            }
            if ($effects->detected()) {
                return $this->fail('read_side_effect_detected', 'the read changed the site or called out; its output was withheld', false, ['side_effects' => $effects->details()]);
            }
            $refusal = $this->restOutcomeRefusal($d, $principal, true);
            if ($refusal !== null) {
                return $refusal;
            }

            $response = $d['response'];
            $data     = is_object($response) && method_exists($response, 'get_data') ? RestCall::plain($response->get_data()) : null;
            if ($data === null) {
                return $this->fail('rest_error', 'the route output could not be read', false, ['status' => (int) $d['status'], 'error_code' => 'output_invalid']);
            }
            $data = RestCall::dropUnpublishedParents($data, $route);
            if ($data === null || !RestCall::publishedOnly($data, $route)) {
                return $this->fail('rest_not_published', 'the route returned content that is not published; nothing was returned');
            }
            $output  = VendorAbility::project($data, $route['output_fields']);
            $encoded = json_encode($output);
            if (!is_string($encoded) || strlen($encoded) > self::MAX_OUTPUT_BYTES) {
                return $this->fail('output_too_large', 'the route output exceeds the cap');
            }

            return [
                'ok'           => true,
                'outcome'      => 'completed',
                'mode'         => 'read',
                'ability'      => OwnAbilities::NAME_REST_READ,
                'entry_sha256' => $entrySha,
                'route_id'     => $route['route_id'],
                'route_sha256' => $routeSha,
                'status'       => (int) $d['status'],
                'output'       => $output,
            ];
        });
    }

    /**
     * The refusal for a finished dispatch, or null when its response may be
     * used. $strict false is the write path: an after-callbacks change alone
     * makes the response unknown rather than refused, and the stored post
     * decides.
     *
     * @param array{response:mixed,status:int,violations:list<string>,handler_refused:bool,exception:bool} $d         Dispatch.
     * @param int                                                                                         $principal Service user.
     * @param bool                                                                                        $strict    Refuse an after-callbacks change.
     * @return array<string,mixed>|null
     */
    private function restOutcomeRefusal(array $d, int $principal, bool $strict): ?array
    {
        $intact = $this->principalRefusal($principal);
        if ($intact !== null) {
            return $intact;
        }
        if ($d['handler_refused']) {
            return $this->fail('rest_handler_not_core', 'the route is not answered by WordPress core');
        }
        $violations = $strict ? $d['violations'] : array_values(array_diff($d['violations'], ['after_callbacks']));
        if ($violations !== []) {
            return $this->fail('rest_intercepted', 'another plugin interfered with the call', false, ['violations' => $violations]);
        }
        if ($d['exception']) {
            return $this->fail('rest_error', 'the route failed', false, ['status' => 500, 'error_code' => 'exception']);
        }
        if ($d['status'] >= 400 || $d['status'] < 100) {
            return $this->fail('rest_error', 'the route refused the call', false, ['status' => $d['status'], 'error_code' => $this->restErrorCode($d['response'])]);
        }

        return null;
    }

    /**
     * The refusal when the current user is no longer the intact principal.
     *
     * @param int $principal Service user.
     * @return array<string,mixed>|null
     */
    private function principalRefusal(int $principal): ?array
    {
        if (!function_exists('get_current_user_id') || (int) get_current_user_id() !== $principal) {
            return $this->fail('principal_switched', 'the current user changed during the call; its output was withheld');
        }
        $drift = ServicePrincipal::liveDrift();
        if ($drift !== null) {
            return $this->fail('principal_capabilities_drifted', $drift);
        }

        return null;
    }

    /**
     * Dispatch one request through core's REST server, under the guards.
     *
     * @param array<string,mixed>                                                                 $route Route.
     * @param array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int} $call  Call.
     * @return array{response:mixed,status:int,violations:list<string>,handler_refused:bool,exception:bool}
     */
    private function restDispatch(array $route, array $call): array
    {
        $out = ['response' => null, 'status' => 0, 'violations' => [], 'handler_refused' => false, 'exception' => false];
        if (!function_exists('rest_do_request') || !function_exists('rest_get_server') || !class_exists('WP_REST_Request')) {
            $out['violations'] = ['pre_dispatch'];

            return $out;
        }
        // The server class is filterable; only core's own dispatches.
        $server = rest_get_server();
        if (!is_object($server) || get_class($server) !== 'WP_REST_Server') {
            $out['violations'] = ['pre_dispatch'];

            return $out;
        }
        $request  = RestCall::buildRequest($route, $call);
        $guards   = new RestGuards();
        $response = null;
        $guards->arm($request, (string) $route['core_pattern'], (string) $route['method']);
        try {
            try {
                $response = rest_do_request($request);
            } catch (\Throwable $e) {
                $response         = null;
                $out['exception'] = true;
            }
            if (!$out['exception']) {
                $guards->verify($response);
            }
        } finally {
            $guards->disarm();
        }
        $out['response']        = $response;
        $out['status']          = is_object($response) && method_exists($response, 'get_status') ? (int) $response->get_status() : 0;
        $out['violations']      = $guards->violations();
        $out['handler_refused'] = $guards->handlerRefused();

        return $out;
    }

    /**
     * Core's error code from an error response, or a fixed label. Never the
     * message text.
     *
     * @param mixed $response Response.
     * @return string
     */
    private function restErrorCode($response): string
    {
        $data = is_object($response) && method_exists($response, 'get_data') ? $response->get_data() : null;
        $code = is_array($data) ? ($data['code'] ?? null) : null;

        return is_string($code) && preg_match('/^[a-z0-9_]{1,64}$/', $code) === 1 ? $code : 'unknown';
    }

    /**
     * The target post of a write, as the principal: it exists, is the row's
     * post type, is a live post, and the principal may edit it.
     *
     * @param array<string,mixed> $route  Route.
     * @param int                 $postId Post id.
     * @return array{post?:object,refusal?:array<string,mixed>}
     */
    private function restTarget(array $route, int $postId): array
    {
        clean_post_cache($postId);
        $post = get_post($postId);
        $type = (string) ($route['target']['post_type'] ?? '');
        if (!is_object($post) || self::pf($post, 'post_type') !== $type
            || in_array(self::pf($post, 'post_status'), ['trash', 'auto-draft', 'inherit'], true)
            || !current_user_can('edit_post', $postId)) {
            return ['refusal' => $this->fail('post_not_editable', 'no ' . $type . ' with that id that the service user may edit')];
        }
        // Core publishes a past-due scheduled post when it is updated.
        if (self::pf($post, 'post_status') === 'future') {
            return ['refusal' => $this->fail('post_scheduled', 'this page is scheduled; change its title in WordPress')];
        }

        return ['post' => $post];
    }

    /**
     * What the site would store for $value in $field, as the current user.
     *
     * @param string $field  post_title or post_excerpt.
     * @param string $value  Bytes.
     * @param int    $postId Post id.
     * @return string
     */
    private function restSimulate(string $field, string $value, int $postId): string
    {
        $out = wp_unslash(sanitize_post_field($field, wp_slash($value), $postId, 'db'));
        $out = is_string($out) ? $out : '';
        if (!current_user_can('unfiltered_html') && wp_kses_post($out) !== $out) {
            return "\0kses";
        }

        return $out;
    }

    /**
     * The bytes a post_fields write stores per body key, and the first
     * key the site would change, if any.
     *
     * @param array<string,mixed> $body   Body.
     * @param int                 $postId Post id.
     * @return array{stored:array<string,string>,changed:string|null}
     */
    private function restStored(array $body, int $postId): array
    {
        $stored  = [];
        $changed = null;
        foreach (RestCall::POST_FIELDS as $key => $field) {
            if (!array_key_exists($key, $body)) {
                continue;
            }
            $bytes        = RestCall::storedValue((string) $body[$key]);
            $stored[$key] = $bytes;
            if ($changed === null && $this->restSimulate($field, $bytes, $postId) !== $bytes) {
                $changed = $key;
            }
        }

        return ['stored' => $stored, 'changed' => $changed];
    }

    /**
     * Precheck a post_fields write. Writes nothing.
     *
     * @param array<string,mixed>                                                                 $route     Route.
     * @param array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int} $call      Call.
     * @param string                                                                              $requestId Request id.
     * @param string                                                                              $entrySha  Entry hash.
     * @param string                                                                              $routeSha  Route hash.
     * @param string                                                                              $inputSha  Input hash.
     * @return array<string,mixed>
     */
    private function restPrecheck(array $route, array $call, string $requestId, string $entrySha, string $routeSha, string $inputSha): array
    {
        $postId = $call['target_id'];
        $target = $this->restTarget($route, $postId);
        if (!isset($target['post'])) {
            return $target['refusal'] ?? $this->fail('post_not_editable', 'the post cannot be edited');
        }
        $post   = $target['post'];
        $stored = $this->restStored($call['body'], $postId);
        if ($stored['changed'] !== null) {
            return $this->fail('sanitiser_changed_value', 'the site would change the ' . $stored['changed'] . ' on save', false, ['key' => $stored['changed']]);
        }
        if ($this->restExistingChanges($post) !== []) {
            return $this->contentWouldChange();
        }
        $baseFp  = RestCall::postFingerprint($post);
        $changes = [];
        foreach ($stored['stored'] as $key => $bytes) {
            $changes[] = [
                'key'    => $key,
                'before' => self::pf($post, RestCall::POST_FIELDS[$key]),
                'after'  => (string) $call['body'][$key],
                'stored' => $bytes,
            ];
        }
        $status = self::pf($post, 'post_status');

        return [
            'ok'               => true,
            'outcome'          => 'prechecked',
            'mode'             => 'precheck',
            'ability'          => OwnAbilities::NAME_REST_WRITE,
            'request_id'       => $requestId,
            'route_id'         => $route['route_id'],
            'valid'            => true,
            'base_fingerprint' => $baseFp,
            'precheck_digest'  => RestCall::precheckDigest($entrySha, $routeSha, $inputSha, $baseFp),
            'target_facts'     => [
                'id'             => $postId,
                'post_type'      => self::pf($post, 'post_type'),
                'status'         => $status,
                'live'           => $status === 'publish',
                'title_before'   => self::pf($post, 'post_title'),
                'excerpt_before' => self::pf($post, 'post_excerpt'),
            ],
            'changes'          => $changes,
            'undo_exact'       => $this->restUndoExact($post),
        ];
    }

    /**
     * The columns of the existing row, other than title and excerpt, that a
     * save by the current user would change: every save-time field filter
     * core applies through sanitize_post(..., 'db') when wp_update_post
     * re-saves the merged row, kses included for a user without
     * unfiltered_html.
     *
     * @param object $post Post.
     * @return list<string>
     */
    private function restExistingChanges(object $post): array
    {
        $changed = [];
        $vars    = get_object_vars($post);
        foreach (RestCall::POST_COLUMNS as $column) {
            if (in_array($column, ['ID', 'comment_count'], true) || in_array($column, RestCall::POST_FIELDS_MOVING, true)
                || !array_key_exists($column, $vars) || !is_scalar($vars[$column])) {
                continue;
            }
            $value = (string) $vars[$column];
            $saved = wp_unslash(sanitize_post_field($column, wp_slash($value), (int) self::pf($post, 'ID'), 'db'));
            if (!is_scalar($saved) || (string) $saved !== $value) {
                $changed[] = $column;
            }
        }

        return $changed;
    }

    /**
     * @return array<string,mixed>
     */
    private function contentWouldChange(): array
    {
        return $this->fail('post_content_would_change', 'this page has content the WPMgr user may not save; edit the title in WordPress');
    }

    /**
     * Columns other than title, excerpt and the modified dates that differ
     * between the row before and after the write, from the closed column
     * list.
     *
     * @param object $before Before.
     * @param object $after  After.
     * @return list<string>
     */
    private function restOtherColumnsChanged(object $before, object $after): array
    {
        $out      = [];
        $redating = self::coreRedatesOnUpdate($before);
        foreach (RestCall::POST_COLUMNS as $column) {
            if (in_array($column, RestCall::POST_FIELDS_MOVING, true) || $column === 'comment_count') {
                continue;
            }
            if ($redating && $column === 'post_date') {
                continue;
            }
            if (self::pf($before, $column) !== self::pf($after, $column)) {
                $out[] = $column;
            }
        }

        return $out;
    }

    /**
     * Does core's wp_update_post() set post_date to the current time on this
     * update? It does for a draft, pending or auto-draft post whose
     * post_date_gmt is the zero date, unless the update sets edit_date; the
     * update this engine sends never does. Only post_date moves: core then
     * leaves post_date_gmt at the zero date for such a post.
     *
     * @param object $post Post before the update.
     * @return bool
     */
    private static function coreRedatesOnUpdate(object $post): bool
    {
        return in_array(self::pf($post, 'post_status'), ['draft', 'pending', 'auto-draft'], true)
            && self::pf($post, 'post_date_gmt') === '0000-00-00 00:00:00';
    }

    /**
     * Hashes of the target's post meta and term ids, recorded around a
     * write. Plugins commonly write their own meta on save, so a change is
     * reported, not refused.
     *
     * @param int $postId Post id.
     * @return array{meta:string,terms:string}
     */
    private function targetState(int $postId): array
    {
        $meta  = function_exists('get_post_meta') ? get_post_meta($postId) : null;
        $terms = null;
        if (function_exists('wp_get_object_terms') && function_exists('get_object_taxonomies')) {
            $type  = function_exists('get_post_type') ? get_post_type($postId) : false;
            $taxes = is_string($type) ? get_object_taxonomies($type) : [];
            $terms = $taxes === [] ? [] : wp_get_object_terms($postId, $taxes, ['fields' => 'ids']);
        }
        if (is_array($meta)) {
            unset($meta['_edit_lock'], $meta['_edit_last']);
            ksort($meta);
        }
        if (is_array($terms)) {
            sort($terms);
        }

        return [
            'meta'  => hash('sha256', (string) json_encode($meta)),
            'terms' => hash('sha256', (string) json_encode(is_array($terms) ? $terms : null)),
        ];
    }

    /**
     * Would the prior values survive a save by the principal byte for byte?
     *
     * @param object $post Post.
     * @return bool
     */
    private function restUndoExact(object $post): bool
    {
        foreach (RestCall::POST_FIELDS as $field) {
            $prior = self::pf($post, $field);
            if ($this->restSimulate($field, $prior, (int) self::pf($post, 'ID')) !== $prior) {
                return false;
            }
        }

        return true;
    }

    /**
     * The post_fields write pipeline. Runs as the principal, under both
     * claims.
     *
     * @param array<string,mixed>                                                                 $route     Route.
     * @param array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int} $call      Call.
     * @param string                                                                              $requestId Request id.
     * @param string                                                                              $entrySha  Entry hash.
     * @param string                                                                              $routeSha  Route hash.
     * @param string                                                                              $inputSha  Input hash.
     * @param string                                                                              $expPre    Expected precheck digest.
     * @param string                                                                              $expBase   Expected base fingerprint.
     * @return array<string,mixed>
     */
    private function restWrite(array $route, array $call, string $requestId, string $entrySha, string $routeSha, string $inputSha, string $expPre, string $expBase): array
    {
        $principal = (int) get_current_user_id();
        // A replay that raced the first check sees the row now.
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }

        // 3. Re-check: the post, the editor lock, the digest, the sanitiser.
        $postId = $call['target_id'];
        $target = $this->restTarget($route, $postId);
        if (!isset($target['post'])) {
            return $target['refusal'] ?? $this->fail('post_not_editable', 'the post cannot be edited');
        }
        $post   = $target['post'];
        $baseFp = RestCall::postFingerprint($post);
        if (!hash_equals($expBase, $baseFp)) {
            return $this->fail('conflict', 'the post changed after it was checked');
        }
        if ($this->restExistingChanges($post) !== []) {
            return $this->contentWouldChange();
        }
        $locked = $this->postLocked($postId);
        if ($locked !== false) {
            return $this->fail('conflict', $locked === null ? 'whether someone is editing this post could not be checked' : 'someone is editing this post right now', false, ['reason' => 'editor_open']);
        }
        if (!hash_equals($expPre, RestCall::precheckDigest($entrySha, $routeSha, $inputSha, $baseFp))) {
            return $this->fail('preview_changed', 'what would change differs from what was approved');
        }
        $stored = $this->restStored($call['body'], $postId);
        if ($stored['changed'] !== null) {
            return $this->fail('sanitiser_changed_value', 'the site would change the ' . $stored['changed'] . ' on save', false, ['key' => $stored['changed']]);
        }
        // 4. The snapshot, before any effect, read back before the effect.
        $prior  = ['post_title' => self::pf($post, 'post_title'), 'post_excerpt' => self::pf($post, 'post_excerpt')];
        $ledger = [
            'request_id'      => $requestId,
            'ability'         => OwnAbilities::NAME_REST_WRITE,
            'entry_sha256'    => $entrySha,
            'route_id'        => $route['route_id'],
            'route_sha256'    => $routeSha,
            'snapshot'        => 'post_fields',
            'phase'           => 'snapshotted',
            'created_post_id' => 0,
            'target_post_id'  => $postId,
            'prior'           => $prior,
            'stored'          => $stored['stored'],
            'before_fp'       => $baseFp,
            'after_fp'        => '',
            'precheck_digest' => $expPre,
            'undo_state'      => 'none',
            'created_at'      => time(),
            'result'          => null,
        ];
        if (!AbilityLedger::create($requestId, $ledger)) {
            return $this->fail('snapshot_failed', 'the snapshot could not be written; nothing was changed');
        }
        $saved = AbilityLedger::get($requestId);
        if ($saved === null || ($saved['prior'] ?? null) !== $prior) {
            AbilityLedger::update($requestId, ['phase' => 'failed']);

            return $this->fail('snapshot_failed', 'the snapshot could not be read back; nothing was changed');
        }

        // 5. Tripwire baseline. 6. Dispatch.
        $baseline     = $this->tripwires($postId);
        $targetBefore = $this->targetState($postId);
        AbilityLedger::update($requestId, ['phase' => 'dispatching']);
        // What is sent is what was approved and what verify expects stored.
        $sent         = $call;
        $sent['body'] = $stored['stored'];
        $d            = $this->restDispatch($route, $sent);

        // 7-8. Re-read and verify the stored fields; the tripwires.
        clean_post_cache($postId);
        $after   = get_post($postId);
        $changed = !is_object($after) || RestCall::postFingerprint($after) !== $baseFp;
        $refusal = $this->restOutcomeRefusal($d, $principal, false);
        $problem = $refusal === null ? $this->verifyPostFields($after, $post, $stored['stored']) : null;
        $columns = is_object($after) ? $this->restOtherColumnsChanged($post, $after) : [];
        $tripped = $this->tripwires($postId) !== $baseline;

        if ($refusal !== null || $problem !== null || $tripped || $columns !== [] || !is_object($after)) {
            $result = $refusal ?? $this->fail('verify_mismatch', (string) ($problem ?? 'the post could not be read back'));
            if ($columns !== []) {
                $result = $this->fail('side_effect_detected', 'the write changed more of this post than its title and excerpt', false, ['columns' => $columns]);
                $changed = true;
            }
            if ($tripped) {
                $result = $this->fail('side_effect_detected', 'the site changed outside this post during the write', false, $columns !== [] ? ['columns' => $columns] : []);
            }
            // 9. Undo our own change when anything changed. Restored means
            //    the whole row is back as it was, apart from the modified
            //    dates; anything still different is named and left
            //    undoable by a person.
            $ledgerFields = ['phase' => 'failed', 'undo_state' => 'none'];
            if ($changed || $tripped) {
                $undo = $this->restorePrior($postId, $prior);
                clean_post_cache($postId);
                $now  = get_post($postId);
                $left = is_object($now) ? $this->restOtherColumnsChanged($post, $now) : RestCall::POST_COLUMNS;
                $result['restored'] = $undo['restored'] && $left === [];
                $result['exact']    = $undo['exact'] && $left === [];
                if ($left !== []) {
                    $result['columns_still_changed'] = array_values($left);
                }
                if ($result['restored']) {
                    $ledgerFields['undo_state'] = 'restored';
                } elseif (is_object($now)) {
                    $ledgerFields = [
                        'phase'                 => 'failed_needs_attention',
                        'undo_state'            => 'available',
                        'after_fp'              => RestCall::postFingerprint($now),
                        'touch_marker'          => $this->touchMarker($postId),
                        'columns_still_changed' => array_values($left),
                    ];
                }
            } else {
                $result['restored'] = false;
                $result['changed']  = false;
            }
            AbilityLedger::update($requestId, $ledgerFields + ['result' => $result]);

            return $result;
        }

        // 10-11. Record and return.
        $afterFp  = RestCall::postFingerprint($after);
        $response = $d['response'];
        $output   = in_array('after_callbacks', $d['violations'], true) || !is_object($response) || !method_exists($response, 'get_data')
            ? $this->postFieldsOutput($after)
            : VendorAbility::project(RestCall::plain($response->get_data()), $route['output_fields']);
        $targetNow = $this->targetState($postId);
        $result   = [
            'ok'         => true,
            'outcome'    => 'updated',
            'mode'       => 'write',
            'ability'    => OwnAbilities::NAME_REST_WRITE,
            'request_id' => $requestId,
            'route_id'   => $route['route_id'],
            'post_id'    => $postId,
            'post_type'  => self::pf($after, 'post_type'),
            'status'     => self::pf($after, 'post_status'),
            'live'       => self::pf($after, 'post_status') === 'publish',
            'after_fp'   => $afterFp,
            'verify'     => [
                'fields_equal'         => true,
                'other_columns'        => 'unchanged',
                'tripwires'            => 'clean',
                'target_meta_changed'  => $targetNow['meta'] !== $targetBefore['meta'],
                'target_terms_changed' => $targetNow['terms'] !== $targetBefore['terms'],
            ],
            'output'     => $output,
        ];
        $recorded = AbilityLedger::update($requestId, [
            'phase'        => 'completed',
            'after_fp'     => $afterFp,
            'touch_marker' => $this->touchMarker($postId),
            'undo_state'   => 'available',
            'result'       => $result,
        ]);
        if (!$recorded) {
            $result['ledger_recorded'] = false;
        }

        return $result;
    }

    /**
     * Why the stored post is not exactly the approved change, or null.
     *
     * @param mixed                $after  Stored post after the call.
     * @param object               $before Post before the call.
     * @param array<string,string> $stored Bytes we sent, per body key.
     * @return string|null
     */
    private function verifyPostFields($after, object $before, array $stored): ?string
    {
        if (!is_object($after)) {
            return 'the post could not be read back';
        }
        if (self::pf($after, 'post_type') !== self::pf($before, 'post_type') || self::pf($after, 'post_status') !== self::pf($before, 'post_status')) {
            return 'the post type or status changed';
        }
        foreach (RestCall::POST_FIELDS as $key => $field) {
            $want = array_key_exists($key, $stored) ? $stored[$key] : self::pf($before, $field);
            if (self::pf($after, $field) !== $want) {
                return 'the stored ' . $key . ' differs from what was approved';
            }
        }

        return null;
    }

    /**
     * Put the prior title and excerpt back, as the principal, and check.
     * restored: the site now holds what it stores for the prior bytes;
     * exact: those are the prior bytes themselves.
     *
     * @param int                  $postId Post id, from the ledger.
     * @param array<string,string> $prior  Prior bytes.
     * @return array{restored:bool,exact:bool}
     */
    private function restorePrior(int $postId, array $prior): array
    {
        $title   = (string) ($prior['post_title'] ?? '');
        $excerpt = (string) ($prior['post_excerpt'] ?? '');
        wp_update_post(wp_slash(['ID' => $postId, 'post_title' => $title, 'post_excerpt' => $excerpt]), true);
        clean_post_cache($postId);
        $now = get_post($postId);
        if (!is_object($now)) {
            return ['restored' => false, 'exact' => false];
        }
        $exact = self::pf($now, 'post_title') === $title && self::pf($now, 'post_excerpt') === $excerpt;

        return [
            'restored' => $exact || (self::pf($now, 'post_title') === $this->restSimulate('post_title', $title, $postId)
                && self::pf($now, 'post_excerpt') === $this->restSimulate('post_excerpt', $excerpt, $postId)),
            'exact'    => $exact,
        ];
    }

    /**
     * Output facts read from the stored post, used when the route's own
     * response cannot be trusted.
     *
     * @param object $post Post.
     * @return array<string,mixed>
     */
    private function postFieldsOutput(object $post): array
    {
        return [
            'id'           => (int) self::pf($post, 'ID'),
            'modified_gmt' => self::pf($post, 'post_modified_gmt'),
            'status'       => self::pf($post, 'post_status'),
        ];
    }

    /**
     * Person undo for rest-write: put back the prior title and excerpt. The
     * post id and the prior bytes come only from the ledger row of this
     * token-bound request_id (W3), and only while the post is unchanged since
     * our write and nobody has touched it.
     *
     * @param string $requestId Request id.
     * @return array<string,mixed>
     */
    private function restRevert(string $requestId): array
    {
        $row = AbilityLedger::get($requestId);
        if ($row === null) {
            return $this->fail('nothing_to_revert', 'there is no ledger row for this request');
        }
        if (($row['ability'] ?? null) !== OwnAbilities::NAME_REST_WRITE) {
            return $this->fail('ledger_ability_mismatch', 'the ledger row is for another ability');
        }
        $postId = (int) ($row['target_post_id'] ?? 0);
        if (in_array($row['undo_state'] ?? null, ['restored', 'restored_fields_only'], true)) {
            return ['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => $requestId, 'post_id' => $postId, 'restored' => ($row['undo_state'] ?? null) === 'restored'];
        }
        $prior   = $row['prior'] ?? null;
        $afterFp = $row['after_fp'] ?? null;
        if (!in_array($row['phase'] ?? null, ['completed', 'failed_needs_attention'], true) || ($row['undo_state'] ?? null) !== 'available'
            || $postId < 1 || !is_array($prior) || !is_string($afterFp) || $afterFp === ''
            || !is_string($prior['post_title'] ?? null) || !is_string($prior['post_excerpt'] ?? null)) {
            return $this->fail('not_revertible', 'this request has no change that can be undone');
        }
        if (!AbilityLedger::claimTarget($postId)) {
            return $this->fail('target_in_flight', 'another engine call holds this post');
        }
        try {
            return $this->asPrincipal(function (int $principal) use ($requestId, $postId, $prior, $afterFp, $row): array {
                clean_post_cache($postId);
                $post = get_post($postId);
                if (!is_object($post)) {
                    return $this->fail('conflict', 'the post no longer exists');
                }
                if (!hash_equals($afterFp, RestCall::postFingerprint($post))) {
                    return $this->fail('conflict', 'the post changed after WPMgr changed it');
                }
                $touched = $this->touchedSinceWrite($postId, $row['touch_marker'] ?? null);
                if ($touched !== null) {
                    return $this->fail('post_touched', $touched);
                }
                $undo = $this->restorePrior($postId, $prior);
                if (!$undo['restored']) {
                    return $this->fail('revert_failed', 'the previous title and excerpt could not be put back', false, ['exact' => false]);
                }
                // Columns another plugin changed during a failed write are
                // not ours to put back; they are reported, not claimed.
                $left = is_array($row['columns_still_changed'] ?? null) ? array_values(array_intersect(RestCall::POST_COLUMNS, $row['columns_still_changed'])) : [];
                AbilityLedger::update($requestId, ['undo_state' => $left === [] ? 'restored' : 'restored_fields_only', 'reverted_at' => time()]);
                $intact = $this->principalRefusal($principal);
                if ($intact !== null) {
                    return $intact + ['restored' => $left === []];
                }
                $out = [
                    'ok'         => true,
                    'outcome'    => 'reverted',
                    'mode'       => 'revert',
                    'request_id' => $requestId,
                    'post_id'    => $postId,
                    'restored'   => $left === [],
                    'exact'      => $undo['exact'] && $left === [],
                ];
                if ($left !== []) {
                    $out['columns_still_changed'] = $left;
                }

                return $out;
            });
        } finally {
            AbilityLedger::releaseTarget($postId);
        }
    }

    /**
     * Is the post edit-locked by someone? Null when it cannot be checked.
     *
     * @param int $postId Post id.
     * @return bool|null
     */
    private function postLocked(int $postId): ?bool
    {
        if (!function_exists('wp_check_post_lock') && defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/post.php')) {
            require_once ABSPATH . 'wp-admin/includes/post.php';
        }
        if (!function_exists('wp_check_post_lock')) {
            return null;
        }

        return wp_check_post_lock($postId) !== false;
    }

    /**
     * What a later save or autosave would change: the newest revision id and
     * the autosave's id and modified time.
     *
     * @param int $postId Post id.
     * @return string
     */
    private function touchMarker(int $postId): string
    {
        $revisions = wp_get_post_revisions($postId, ['check_enabled' => false, 'numberposts' => 1]);
        $newest    = is_array($revisions) && $revisions !== [] ? (int) array_key_first($revisions) : 0;
        $autosave  = wp_get_post_autosave($postId, 0);
        $auto      = is_object($autosave) ? ((int) $autosave->ID) . '@' . ((string) $autosave->post_modified_gmt) : '';

        return $newest . '|' . $auto;
    }

    /**
     * Undo refuses a post someone touched after our write: a newer revision
     * or autosave than the ones recorded, or a live edit lock. Fails closed.
     *
     * @param int   $postId Post id.
     * @param mixed $marker Marker recorded after our write.
     * @return string|null
     */
    private function touchedSinceWrite(int $postId, $marker): ?string
    {
        if (!is_string($marker) || $this->touchMarker($postId) !== $marker) {
            return 'someone has saved or has unsaved changes to this post since WPMgr changed it; open it in WordPress';
        }
        $locked = $this->postLocked($postId);
        if ($locked === null) {
            return 'whether someone is editing this post could not be checked';
        }
        if ($locked) {
            return 'someone is editing this post right now';
        }

        return null;
    }

    /**
     * The tripwire set: the newest change to any other post (revisions of
     * the target aside), the pinned security options read raw, and the
     * number of administrators.
     *
     * @param int $postId Target post id.
     * @return array<string,string>
     */
    private function tripwires(int $postId): array
    {
        global $wpdb;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the tripwire must read the table itself; a cached value would hide another writer
        $others = $wpdb->get_var($wpdb->prepare("SELECT MAX(post_modified_gmt) FROM {$wpdb->posts} WHERE ID <> %d AND NOT (post_type = 'revision' AND post_parent = %d)", $postId, $postId));
        $names  = ['siteurl', 'home', 'active_plugins', 'users_can_register', 'default_role', 'admin_email', 'template', 'stylesheet', $wpdb->prefix . 'user_roles'];
        $values = [];
        foreach ($names as $name) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- read raw, past the options cache, so a change by any writer is seen
            $values[$name] = $wpdb->get_var($wpdb->prepare("SELECT option_value FROM {$wpdb->options} WHERE option_name = %s", $name));
        }
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the administrator count must come from the table, not a cached user query
        $admins  = $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM {$wpdb->usermeta} WHERE meta_key = %s AND meta_value LIKE %s", $wpdb->prefix . 'capabilities', '%' . $wpdb->esc_like('"administrator"') . '%'));
        $network = '';
        if (function_exists('is_multisite') && is_multisite() && function_exists('get_site_option')) {
            $network = (string) json_encode([get_site_option('active_sitewide_plugins'), get_site_option('site_admins')]);
        }

        return [
            'others'  => is_scalar($others) ? (string) $others : '',
            'options' => hash('sha256', (string) json_encode($values)),
            'admins'  => is_scalar($admins) ? (string) $admins : '',
            'network' => hash('sha256', $network),
        ];
    }

    // ---------------------------------------------------------------------
    /**
     * A post field as a string, read without assuming the object's class.
     *
     * @param object $post  Post.
     * @param string $field Field.
     * @return string
     */
    private static function pf(object $post, string $field): string
    {
        $v = get_object_vars($post)[$field] ?? '';

        return is_scalar($v) ? (string) $v : '';
    }

    // wpmgr/page-create
    // ---------------------------------------------------------------------

    /**
     * Dispatch page-create by mode.
     *
     * @param string $mode      Mode.
     * @param string $requestId Request id.
     * @param string $entrySha  Entry hash.
     * @param object $req       Decoded p.
     * @param object $entry     The catalogue entry.
     * @return array<string,mixed>
     */
    private function pageCreate(string $mode, string $requestId, string $entrySha, object $req, object $entry): array
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

        // A page-builder editor builds only through a builder this agent
        // knows, has compiled in, and the entry enables.
        $adapter = null;
        if (preg_match(BuilderRegistry::RE_EDITOR, $spec['editor']) === 1) {
            if (OwnAbilities::buildersEnabled($entry) === null) {
                return $this->fail('bad_input', 'the entry\'s limits.builders_enabled is not a list of page builder ids');
            }
            $resolved = BuilderRegistry::resolve($spec['editor'], get_object_vars($entry)['limits'] ?? null, $this->builderSeam === null ? null : ($this->builderSeam)());
            if (!isset($resolved['adapter'])) {
                return $this->fail((string) ($resolved['code'] ?? 'bad_input'), (string) ($resolved['detail'] ?? 'that page builder cannot be used'));
            }
            $adapter = $resolved['adapter'];
        }

        if ($mode === 'precheck' && $adapter !== null) {
            return $this->asPrincipal(function () use ($spec, $adapter, $requestId, $entrySha, $inputSha): array {
                $built = $this->builderBuild($spec, $adapter, $requestId);
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
                    'preview'          => $built['preview'],
                ];
            });
        }
        if ($mode === 'precheck') {
            return $this->asPrincipal(function () use ($spec, $requestId, $entrySha, $inputSha): array {
                $built = $this->pageCreateBuild($spec);
                if (isset($built['refusal'])) {
                    return $built['refusal'];
                }
                $preview = [
                    'post_type' => $spec['post_type'],
                    'editor'    => $spec['editor'],
                    'status'    => 'draft',
                    'title'     => $spec['title'],
                    'content'   => $built['content'],
                ];
                // Only an outline with an image carries media, so a text-only
                // answer stays exactly what older control planes decode.
                if ($built['media'] !== []) {
                    $preview['media'] = $built['media'];
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
                    'preview'          => $preview,
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
            return $this->asPrincipal(function (int $principal) use ($spec, $adapter, $requestId, $entrySha, $inputSha, $expPre, $expPrev): array {
                if ($adapter !== null) {
                    return $this->builderPageCreateWrite($principal, $spec, $adapter, $requestId, $entrySha, $inputSha, $expPre, $expPrev);
                }

                return $this->pageCreateWrite($principal, $spec, $requestId, $entrySha, $inputSha, $expPre, $expPrev);
            });
        } finally {
            AbilityLedger::releaseRequest($requestId);
        }
    }

    /**
     * Resolve the outline's images and build the page with a page builder,
     * as the principal. Writes nothing.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>,elementor_format?:string} $spec Spec with a builder editor.
     * @param BuilderAdapter      $adapter   The resolved adapter.
     * @param string              $requestId Request id; node ids derive from it.
     * @return array<string,mixed> BuilderPageCreate::precheck()'s answer, or {refusal}.
     */
    private function builderBuild(array $spec, BuilderAdapter $adapter, string $requestId): array
    {
        $resolved = $this->pageCreateMedia(PageCreateBuilder::mediaIds($spec));
        if (!isset($resolved['facts'])) {
            return ['refusal' => $resolved['refusal'] ?? $this->fail('image_not_available', 'an image could not be resolved')];
        }
        $facts = $adapter instanceof ElementorAdapter ? $adapter->facts() : [];

        return BuilderPageCreate::precheck($spec, $adapter, $facts, $resolved['facts'], $requestId);
    }

    /**
     * The write pipeline for a builder page, under the request claim, as the
     * principal: the same replay check, digest re-check and ledger row as
     * every page-create, then BuilderPageCreate's write.
     *
     * @param int                 $principal Service user id.
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>,elementor_format?:string} $spec Spec with a builder editor.
     * @param BuilderAdapter      $adapter   The resolved adapter.
     * @param string              $requestId Request id.
     * @param string              $entrySha  Entry hash.
     * @param string              $inputSha  Input hash.
     * @param string              $expPre    Expected precheck digest.
     * @param string              $expPrev   Expected preview digest.
     * @return array<string,mixed>
     */
    private function builderPageCreateWrite(int $principal, array $spec, BuilderAdapter $adapter, string $requestId, string $entrySha, string $inputSha, string $expPre, string $expPrev): array
    {
        $row = AbilityLedger::get($requestId);
        if ($row !== null) {
            return $this->alreadyApplied($requestId, $row);
        }

        $built = $this->builderBuild($spec, $adapter, $requestId);
        if (isset($built['refusal'])) {
            return $built['refusal'];
        }
        $precheck = $this->precheckDigest($entrySha, $inputSha, $built['base_fingerprint'], $built['preview_digest']);
        if (!hash_equals($expPrev, $built['preview_digest']) || !hash_equals($expPre, $precheck)) {
            return $this->fail('preview_changed', 'what would be created differs from what was approved');
        }

        if (!AbilityLedger::create($requestId, $this->pageCreateLedgerRow($requestId, $entrySha, $built['base_fingerprint'], $built['preview_digest'], $precheck))) {
            return $this->fail('snapshot_failed', 'the ledger row could not be written; nothing was created');
        }

        return BuilderPageCreate::write(
            $principal,
            $spec,
            $adapter,
            $requestId,
            $built,
            static fn (array $fields): bool => AbilityLedger::update($requestId, $fields)
        );
    }

    /**
     * The ledger row a page-create write records before any effect: the
     * idempotency key is durable, and created_post_id is filled right after
     * the insert.
     *
     * @param string $requestId     Request id.
     * @param string $entrySha      Entry hash.
     * @param string $baseFp        Base fingerprint.
     * @param string $previewDigest Preview digest.
     * @param string $precheck      Precheck digest.
     * @return array<string,mixed>
     */
    private function pageCreateLedgerRow(string $requestId, string $entrySha, string $baseFp, string $previewDigest, string $precheck): array
    {
        return [
            'request_id'      => $requestId,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'entry_sha256'    => $entrySha,
            'snapshot'        => 'created_post_trash',
            'phase'           => 'inserting',
            'created_post_id' => 0,
            'before_fp'       => $baseFp,
            'after_fp'        => '',
            'preview_digest'  => $previewDigest,
            'precheck_digest' => $precheck,
            'undo_state'      => 'none',
            'created_at'      => time(),
            'result'          => null,
        ];
    }

    /**
     * Resolve, render and simulate the save, as the principal. Writes nothing.
     *
     * @param array{post_type:string,editor:string,title:string,outline:list<array<string,mixed>>} $spec Spec.
     * @return array{refusal?:array<string,mixed>,content:string,title:string,preview_digest:string,base_fingerprint:string,media:list<array<string,mixed>>}
     */
    private function pageCreateBuild(array $spec): array
    {
        $empty = ['content' => '', 'title' => '', 'preview_digest' => '', 'base_fingerprint' => '', 'media' => []];

        $blocksOn = $this->blockEditorFor($spec['post_type']);
        if ($blocksOn === null
            || ($spec['editor'] === PageCreateBuilder::EDITOR_BLOCKS) !== $blocksOn) {
            return ['refusal' => $this->fail('editor_unavailable', 'that editor is not available for this post type on this site')] + $empty;
        }

        $resolved = $this->pageCreateMedia(PageCreateBuilder::mediaIds($spec));
        if (!isset($resolved['facts'])) {
            return ['refusal' => $resolved['refusal'] ?? $this->fail('image_not_available', 'an image could not be resolved')] + $empty;
        }
        $facts = $resolved['facts'];
        $byId  = [];
        foreach ($facts as $fact) {
            $byId[(int) $fact['id']] = $fact;
        }

        $content = PageCreateBuilder::render($spec, $byId);
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
            // With images, every fact of every image is bound, so a write
            // after the image changed answers preview_changed.
            'base_fingerprint' => PageCreateBuilder::baseFingerprint($spec['post_type'], $facts),
            'media'            => $facts,
        ];
    }

    /**
     * Resolve the outline's images, as the principal. An image is usable only
     * when it is an attachment already in the media library, of an allowed
     * raster type, published (unattached, or attached to a published post
     * with no password), and readable by the principal. Nothing is fetched:
     * every fact comes from the site's own records.
     *
     * @param list<int> $ids Distinct attachment ids, in document order.
     * @return array{facts?:list<array<string,mixed>>,refusal?:array<string,mixed>}
     */
    private function pageCreateMedia(array $ids): array
    {
        $facts = [];
        foreach ($ids as $id) {
            $gone = ['refusal' => $this->fail('image_not_available', 'attachment_id ' . $id)];
            $post = get_post($id);
            if (!is_object($post) || (string) $post->post_type !== 'attachment' || (string) $post->post_password !== ''
                || !wp_attachment_is_image($id)) {
                return $gone;
            }
            $mime = (string) get_post_mime_type($id);
            if (!in_array($mime, PageCreateBuilder::IMAGE_MIMES, true)) {
                return $gone;
            }
            // Core resolves inherit: unattached counts as published, else the
            // parent's status. The parent is also checked directly, so a
            // trashed, draft, private or password-protected parent refuses.
            if (get_post_status($id) !== 'publish') {
                return $gone;
            }
            $parentId = (int) $post->post_parent;
            if ($parentId > 0) {
                $parent = get_post($parentId);
                if (!is_object($parent) || (string) $parent->post_status !== 'publish' || (string) $parent->post_password !== '') {
                    return $gone;
                }
            }
            if (!current_user_can('read_post', $id)) {
                return $gone;
            }

            $src = wp_get_attachment_image_src($id, 'large');
            $url = is_array($src) && isset($src[0]) && is_string($src[0]) ? $src[0] : '';
            if (PageCreateBuilder::imageUrlProblem($url) !== null) {
                return ['refusal' => $this->fail('image_url_unusable', 'attachment_id ' . $id)];
            }
            $file = get_attached_file($id);
            $meta = wp_get_attachment_metadata($id);
            $meta = is_array($meta) ? $meta : [];
            $fact = [
                'id'           => $id,
                'url'          => $url,
                // The file's base name only, never the server path.
                'filename'     => is_string($file) && $file !== '' ? wp_basename($file) : '',
                'mime'         => $mime,
                'width'        => self::imageDimension($meta['width'] ?? null),
                'height'       => self::imageDimension($meta['height'] ?? null),
                'modified_gmt' => (string) $post->post_modified_gmt,
            ];
            if (PageCreateBuilder::mediaFactProblem($fact) !== null) {
                return $gone;
            }
            $facts[] = $fact;
        }

        return ['facts' => $facts];
    }

    /**
     * An image dimension from attachment metadata: a whole number of pixels,
     * 0 when absent or out of range.
     *
     * @param mixed $value Stored value.
     * @return int
     */
    private static function imageDimension($value): int
    {
        if (is_string($value) && preg_match('/^[0-9]{1,6}$/D', $value) === 1) {
            $value = (int) $value;
        }

        return is_int($value) && $value >= 0 && $value <= PageCreateBuilder::MAX_IMAGE_DIMENSION ? $value : 0;
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
        $ledger = $this->pageCreateLedgerRow($requestId, $entrySha, $built['base_fingerprint'], $built['preview_digest'], $precheck);
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
        // A builder draft is checked by its builder's guard: the save made
        // revisions of the draft, and its fingerprint covers the builder rows.
        $builderRow = array_key_exists('builder', $row) ? $row : null;
        try {
            return $this->pageCreateRevertClaimed($requestId, $postId, $completed ? $afterFp : null, $builderRow);
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
     * A builder draft ($builderRow set) is instead checked by
     * BuilderPageCreate::revertProblem() against its ledger row.
     *
     * @param string                   $requestId  Request id.
     * @param int                      $postId     Post id, from the ledger row.
     * @param string|null              $afterFp    Recorded after-fingerprint, or null.
     * @param array<string,mixed>|null $builderRow The ledger row of a builder draft, or null.
     * @return array<string,mixed>
     */
    private function pageCreateRevertClaimed(string $requestId, int $postId, ?string $afterFp, ?array $builderRow = null): array
    {
        if (!AbilityLedger::claimTarget($postId)) {
            return $this->fail('target_in_flight', 'another engine call holds this post');
        }
        try {
            return $this->asPrincipal(function (int $principal) use ($requestId, $postId, $afterFp, $builderRow): array {
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
                if ($builderRow !== null) {
                    $problem = BuilderPageCreate::revertProblem($postId, $builderRow);
                    if ($problem !== null) {
                        return $this->fail('created_post_touched', $problem);
                    }
                } else {
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
            'post_id'    => (int) (($row['created_post_id'] ?? 0) ?: ($row['target_post_id'] ?? 0)),
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
     * @param string                                 $name     Ability.
     * @param string                                 $entrySha Entry hash.
     * @param object                                 $input    Validated input.
     * @param (\Closure(): array<string,mixed>)|null $run      The read, when it is not OwnAbilities::run().
     * @return array<string,mixed>
     */
    private function read(string $name, string $entrySha, object $input, ?\Closure $run = null): array
    {
        $guards = new AbilityGuards();
        $armed  = AbilityGuards::supported();
        if ($armed) {
            $guards->arm($name);
        }
        $result = null;
        try {
            try {
                $result = $run === null ? OwnAbilities::run($name, $input) : $run();
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
            'target_post_id'  => $row !== null ? (int) ($row['target_post_id'] ?? 0) : null,
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
