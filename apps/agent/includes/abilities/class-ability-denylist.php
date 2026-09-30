<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The agent-code denylist for ability names.
 *
 * It lives in code, is fail-closed, and cannot be reduced by any data the
 * control plane sends: a catalogue entry may add denied names elsewhere, but
 * it can never un-deny a name here. Code-execution, shell, raw SQL, file
 * write, install and user or role administration abilities are out of scope
 * for the engine and stay out.
 */
final class AbilityDenylist
{
    /** Names refused outright. */
    private const KNOWN = [
        'bricks/execute-php',
    ];

    /** Name segments that mark a dangerous capability. */
    private const PATTERN = '/(^|[\/-])(php|eval|exec|execute-php|run-php|code|shell|wp-cli|cli|sql|query-db|file-write|write-file|delete-file|install|activate|deactivate|plugin-install|theme-install|user-create|role|option-update|update-option)([\/-]|$)/i';

    /**
     * Is the name denied? Anything that is not a plain string is denied.
     *
     * @param mixed $name Candidate ability name.
     * @return bool
     */
    public static function denies($name): bool
    {
        if (!is_string($name) || $name === '') {
            return true;
        }
        if (in_array(strtolower($name), self::KNOWN, true)) {
            return true;
        }

        return preg_match(self::PATTERN, $name) === 1;
    }
}
