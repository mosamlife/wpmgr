<?php
/**
 * TokenRejected: Connector refused a signed command token.
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
 * Thrown by Connector at every check that refuses a token. failure() names
 * the check, so a caller reads a typed category instead of matching on the
 * message, which is free text for the site's debug log and may change.
 *
 * A RuntimeException, so every caller that already catches one is unchanged.
 */
final class TokenRejected extends \RuntimeException
{
    private TokenFailure $failure;

    /**
     * @param TokenFailure    $failure  The check that refused the token.
     * @param string          $message  Fixed text for the site's debug log.
     * @param \Throwable|null $previous What made the check fail, if anything did.
     */
    public function __construct(TokenFailure $failure, string $message, ?\Throwable $previous = null)
    {
        parent::__construct($message, 0, $previous);
        $this->failure = $failure;
    }

    /**
     * @return TokenFailure The check that refused the token.
     */
    public function failure(): TokenFailure
    {
        return $this->failure;
    }
}
