<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Thrown by AbilityGuards from inside a filter that runs before an ability's
 * execute callback, when the guarded value or the guards themselves were
 * tampered with. It aborts the ability call before the callback runs; the
 * engine catches it and refuses the call. Never thrown when not armed.
 */
final class AbilityInterception extends \RuntimeException
{
}
