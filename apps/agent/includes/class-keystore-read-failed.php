<?php
/**
 * KeystoreReadFailed: the database read of a stored key failed.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

// Direct-file-access guard: keep above the class declaration.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Thrown when reading a key row from the database failed, so it is not known
 * whether a key is stored. It is transient: nothing was read and nothing was
 * changed, and the same read can succeed a moment later. A caller must never
 * take it to mean "no key is stored", and must never create a key because of
 * it.
 *
 * A RuntimeException, so every caller that already catches one is unchanged.
 */
final class KeystoreReadFailed extends \RuntimeException
{
}
