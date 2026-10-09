<?php
/**
 * TokenFailure: which check refused a signed command token.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

// Direct-file-access guard: keep above the type declaration.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The checks Connector runs on a command token, one case per refusal.
 *
 * The backing value is the category: what the site's debug log records and
 * what Connector's callers branch on. responseCode() is what the refusal
 * response carries, prefixed with "wpmgr_". That code reaches the control
 * plane, which uses it to explain the refusal, so an existing value must
 * never be renamed; a case may be added.
 */
enum TokenFailure: string
{
    /** Not three segments, a segment that is not a JSON object, or an alg other than EdDSA. */
    case MalformedJwt = 'malformed_jwt';

    /** This site holds no usable control-plane public key. */
    case KeyNotProvisioned = 'key_not_provisioned';

    /** This site's stored control-plane public key cannot be decrypted. */
    case KeyUnreadable = 'key_unreadable';

    /** The signature is the wrong size or does not verify against the stored key. */
    case SigFailed = 'sig_failed';

    /** The token carries no numeric exp claim. */
    case MissingExp = 'missing_exp';

    /** The token's exp is not after this site's clock. */
    case TokenExpired = 'token_expired';

    /**
     * The token's exp is further ahead of this site's clock than
     * Connector::MAX_FUTURE_EXP allows. The usual cause is a site clock
     * running behind the control plane's.
     */
    case TokenSkew = 'token_skew';

    /** The token carries no jti claim. */
    case MissingJti = 'missing_jti';

    /** The token's jti was already used inside the replay window. */
    case TokenReplay = 'token_replay';

    /** This site has no enrolled site id to compare the aud claim with. */
    case SiteNotEnrolled = 'site_not_enrolled';

    /** The token carries no aud claim. */
    case MissingAud = 'missing_aud';

    /** The token's aud names a different site. */
    case AudMismatch = 'aud_mismatch';

    /**
     * The caller asked for verification against an empty command name. The
     * Router refuses an empty command itself, with this same code, before
     * any token is read.
     */
    case MissingCommand = 'missing_command';

    /** The token carries no cmd claim. */
    case MissingCmd = 'missing_cmd';

    /** The token's cmd names a different command. */
    case CmdMismatch = 'cmd_mismatch';

    /**
     * The code a refusal response carries, without the "wpmgr_" prefix.
     *
     * Equal to the category for every check that runs after the signature
     * has verified, because only a token the control plane signed reaches
     * those. A check that runs before the signature answers a caller who may
     * be anyone, so its response must not depend on whether this site holds
     * a usable control-plane key: the two key cases answer sig_failed, the
     * same code a forged signature gets from a site that holds one. The
     * site's debug log keeps the precise category.
     *
     * @return string Lower-case letters and underscores.
     */
    public function responseCode(): string
    {
        return match ($this) {
            self::KeyNotProvisioned, self::KeyUnreadable => self::SigFailed->value,
            self::MalformedJwt,
            self::SigFailed,
            self::MissingExp,
            self::TokenExpired,
            self::TokenSkew,
            self::MissingJti,
            self::TokenReplay,
            self::SiteNotEnrolled,
            self::MissingAud,
            self::AudMismatch,
            self::MissingCommand,
            self::MissingCmd,
            self::CmdMismatch => $this->value,
        };
    }
}
