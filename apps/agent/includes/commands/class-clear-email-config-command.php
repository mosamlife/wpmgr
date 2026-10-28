<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailKeystoreInterface;
use WPMgr\Agent\Keystore;

// Direct-file-access guard: keep above the docblock (see the note in
// class-content-update-command.php).
if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * ClearEmailConfigCommand: take a site back to "no email provider".
 *
 * Once a provider is chosen, sync_email_config can change it but never remove
 * it. This command is the way back: afterwards the mail router finds no stored
 * provider, leaves wp_mail() alone, and WordPress sends through its own mailer.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/clear_email_config
 *   Authorization: Bearer <Ed25519 JWT with cmd="clear_email_config", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {} (no field is read; a body that carries some changes nothing)
 *
 * Response (HTTP 200 for any authorized request; the outcome is in the body):
 *   {
 *     "ok": bool,        // equals "cleared"
 *     "cleared": bool,   // true only when every item below is observed gone
 *     "detail": string,
 *     "existed": { "config": bool, "secret": bool, "connection_secrets": bool }
 *   }
 * "existed" says which of the three stored items were present before this run.
 *
 * A separate command rather than a flag on sync_email_config: an agent that
 * does not know this command refuses it outright (404 wpmgr_unknown_command),
 * so a caller can tell it was not applied. A flag on the sync command would be
 * dropped without a word by an agent that predates it, and that agent would
 * still answer ok.
 *
 * WHAT IT REMOVES. Exactly three stored items and nothing else:
 *   - the wp-option wpmgr_email_config (the provider choice and its settings);
 *   - the primary provider secret, by asking the keystore to store an empty one;
 *   - the named-connection secrets, by asking the keystore to store an empty map.
 * The local email log, its retention state, the suppression list and the
 * keystore's master key are not touched.
 *
 * ORDER. The settings go first. The mail router decides whether to take a
 * message over from the stored provider, so the moment that row is gone every
 * message goes through WordPress's own mailer, which is what this command is
 * for. Removing a credential while the provider is still stored would leave a
 * site that tries to send and cannot authenticate, so when the settings row
 * survives its delete no credential is touched and the command says so. Once
 * the settings are gone each credential store is attempted on its own: one that
 * will not clear must not strand the other.
 *
 * VERIFIED, NOT ASSUMED. delete_option() answers false both for "there was no
 * such row" and for "the delete failed", and the keystore's removals return
 * nothing, so no return value can say whether a row is gone. Each row is read
 * back instead, and `cleared` is true only when none of the three is found. The
 * keystore reads '' both for a credential that is absent and for one it cannot
 * decrypt, so the read is of the stored row itself, never of a decrypted value.
 *
 * REPEATING IT. A run that cannot finish reports `cleared: false` with the
 * items it could not remove, and running it again is the retry. A run on a site
 * that holds nothing succeeds and reports that nothing existed.
 */
final class ClearEmailConfigCommand implements CommandInterface {

	private EmailKeystoreInterface $keystore;

	/**
	 * @param EmailKeystoreInterface $keystore Agent keystore (for credential removal).
	 */
	public function __construct( EmailKeystoreInterface $keystore ) {
		$this->keystore = $keystore;
	}

	/** @inheritDoc */
	public function name(): string {
		return 'clear_email_config';
	}

	/**
	 * Effect: removes the site's stored outgoing-mail configuration and the mail
	 * credentials held in the agent keystore. The control plane keeps the
	 * authoritative copy and can push it again with sync_email_config.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect {
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: removing what is already gone leaves the same end state.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability {
		return CommandRepeatability::Idempotent;
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims (unused).
	 * @param array<string,mixed> $params Request fields (unused: nothing in the request selects what is removed).
	 * @return array{ok:bool,cleared:bool,detail:string,existed:array{config:bool,secret:bool,connection_secrets:bool}}
	 */
	public function execute( array $claims, array $params ): array {
		unset( $claims, $params );

		$existed = array(
			'config'             => self::is_stored( EmailConfig::OPTION ),
			'secret'             => self::is_stored( Keystore::OPTION_EMAIL_SECRET ),
			'connection_secrets' => self::is_stored( Keystore::OPTION_EMAIL_CONN_SECRETS ),
		);

		// The settings first: see ORDER above. A delete that throws (a listener on
		// the option hooks can) is no more a verdict than a keystore that throws:
		// the stored row is read back and that decides.
		try {
			delete_option( EmailConfig::OPTION );
		} catch ( \Throwable $e ) {
			// Reported from the read-back below.
			unset( $e );
		}
		if ( self::is_stored( EmailConfig::OPTION ) ) {
			return self::result( false, 'email config not cleared; the stored settings could not be removed and nothing else was changed', $existed );
		}

		// Each credential store is attempted whether or not the other clears. A
		// keystore that throws is not a verdict: the stored rows are read back
		// below, and what they still hold is what gets reported.
		try {
			$this->keystore->storeEmailSecret( '' );
		} catch ( \Throwable $e ) {
			// Reported from the read-back below.
			unset( $e );
		}
		try {
			$this->keystore->store_connection_secrets( array() );
		} catch ( \Throwable $e ) {
			// Reported from the read-back below.
			unset( $e );
		}

		$left = array();
		if ( self::is_stored( Keystore::OPTION_EMAIL_SECRET ) ) {
			$left[] = 'email secret';
		}
		if ( self::is_stored( Keystore::OPTION_EMAIL_CONN_SECRETS ) ) {
			$left[] = 'connection secrets';
		}
		if ( $left !== array() ) {
			return self::result(
				false,
				'email settings removed; the stored ' . implode( ' and the stored ', $left ) . ' could not be removed',
				$existed
			);
		}

		return self::result(
			true,
			in_array( true, $existed, true ) ? 'email config cleared' : 'email config already clear',
			$existed
		);
	}

	/**
	 * Whether a row for this option exists, whatever it holds.
	 *
	 * get_option() hands back the default it was given only when no row exists,
	 * and a fresh object equals no stored value, so a stored empty string or
	 * false still counts as present.
	 *
	 * @param string $option Option name.
	 * @return bool
	 */
	private static function is_stored( string $option ): bool {
		$absent = new \stdClass();

		return get_option( $option, $absent ) !== $absent;
	}

	/**
	 * @param bool                                                   $cleared Whether every item was observed gone.
	 * @param string                                                 $detail  Human-readable outcome.
	 * @param array{config:bool,secret:bool,connection_secrets:bool} $existed What was stored before the run.
	 * @return array{ok:bool,cleared:bool,detail:string,existed:array{config:bool,secret:bool,connection_secrets:bool}}
	 */
	private static function result( bool $cleared, string $detail, array $existed ): array {
		return array(
			'ok'      => $cleared,
			'cleared' => $cleared,
			'detail'  => $detail,
			'existed' => $existed,
		);
	}
}
