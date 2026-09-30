<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Abilities\AbilityDenylist;
use WPMgr\Agent\Abilities\AbilityGuards;
use WPMgr\Agent\Abilities\OwnAbilities;

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
 *   mode        "read" | "precheck" | "ledger"   ("write"/"revert" are refused)
 *   request_id  UUID (precheck, ledger)
 *   entry       string, the catalogue entry JSON text (read, precheck)
 *   entry_sha256 hex sha256 of that text (read, precheck)
 *   input       string, JSON text of an object (read, precheck; default "{}")
 *
 * This slice runs only WPMgr's own wpmgr/* abilities. Any other name is
 * refused in code, whatever the entry says. Nothing is ever retried.
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
        if ($mode === 'write' || $mode === 'revert') {
            return $this->fail('mode_not_available', 'this agent does not run write modes yet');
        }
        if (!in_array($mode, ['read', 'precheck', 'ledger'], true)) {
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
        if (($entry->enabled ?? null) !== true) {
            return $this->fail('ability_disabled', 'the entry is not enabled');
        }
        if (($entry->status ?? null) !== 'admitted') {
            return $this->fail('ability_not_admitted', 'the entry is not admitted');
        }

        // 4. The code denylist: fail-closed, before scope, not reducible by data.
        if (AbilityDenylist::denies($name)) {
            return $this->fail('ability_denied', 'this ability is denied by the agent');
        }

        // 5. E1 scope: only WPMgr's own abilities run.
        if (strncmp($name, 'wpmgr/', 6) !== 0) {
            return $this->fail('ability_not_runnable_yet', 'only WPMgr\'s own abilities can run on this agent version');
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
            'precheck_digest'  => hash('sha256', (string) json_encode([$entrySha, $inputSha, '', ''])),
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
        try {
            $result = OwnAbilities::run($name, $input);
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
     * Ledger state for a request. This slice writes no ledger, so nothing is
     * ever found.
     *
     * @param string $requestId Request id.
     * @return array<string,mixed>
     */
    private function ledger(string $requestId): array
    {
        return [
            'ok'         => true,
            'outcome'    => 'ledger',
            'mode'       => 'ledger',
            'request_id' => $requestId,
            'found'      => false,
            'inflight'   => false,
            'result'     => null,
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
