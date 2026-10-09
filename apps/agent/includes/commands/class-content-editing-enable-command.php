<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Abilities\ServicePrincipal;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * content_editing_enable: create the content service principal.
 *
 * A separate signed command, not an ability_run mode, because enabling is a
 * site-level setup step with no catalogue entry, no input and no request_id:
 * it creates one role and one user and nothing else. Keeping it out of
 * ability_run keeps that command's contract to "one exact ability call", and
 * lets this command declare its own, gentler repeatability.
 *
 * Body: {} (no fields). Any field is refused.
 * Result: { ok, outcome: "enabled" | "already_enabled", user_id, role, caps }
 *    or   { ok:false, outcome:"refused", code, detail }.
 */
final class ContentEditingEnableCommand implements CommandInterface
{
    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'content_editing_enable';
    }

    /**
     * Effect: creates a role and a user.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * A second run with an intact principal changes nothing.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
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
        if ($params !== []) {
            return $this->fail('bad_params', 'content_editing_enable takes no parameters');
        }
        try {
            $r = ServicePrincipal::enable();
        } catch (\Throwable $e) {
            return $this->fail('internal', 'enabling content editing failed unexpectedly');
        }
        if (!$r['ok']) {
            return $this->fail($r['code'], $r['detail']);
        }

        return [
            'ok'      => true,
            'outcome' => $r['code'],
            'user_id' => $r['user_id'] ?? 0,
            'role'    => ServicePrincipal::ROLE,
            'caps'    => ServicePrincipal::CAPS,
        ];
    }

    /**
     * @param string $code   Code.
     * @param string $detail Detail.
     * @return array<string,mixed>
     */
    private function fail(string $code, string $detail): array
    {
        return ['ok' => false, 'outcome' => 'refused', 'code' => $code, 'detail' => $detail, 'retryable' => false];
    }
}
