<?php
/**
 * SendTestEmailCommand — asks the agent to send a test email using its current
 * email config, with the fallback DISABLED so the real provider error surfaces.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/send_test_email
 *   Authorization: Bearer <Ed25519 JWT with cmd="send_test_email", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: SendTestEmailRequest (see apps/api/internal/agentcmd/email_contract.go)
 *
 * Response: { "ok": bool, "detail": string, "message_id": string? }
 *
 * The sync_email_config command MUST be called first; if no email config is
 * stored this command returns ok=false with an informative detail string.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\ProviderRouter;

/**
 * Sends a test email via the current provider config (fallback disabled).
 */
final class SendTestEmailCommand implements CommandInterface {

	private ProviderRouter $provider_router;

	/**
	 * @param ProviderRouter $provider_router The agent's shared provider router.
	 */
	public function __construct( ProviderRouter $provider_router ) {
		$this->provider_router = $provider_router;
	}

	/** @inheritDoc */
	public function name(): string {
		return 'send_test_email';
	}

	/**
	 * Effect: sends one test message through the configured provider with the fallback disabled, and logs the
	 * attempt.
	 *
	 * @return CommandEffect
	 */
	public function effect(): CommandEffect {
		return CommandEffect::Write;
	}

	/**
	 * Repeatability: a duplicate test message is noise, not harm -- unlike resend_email, the recipient is the
	 * operator testing their own configuration.
	 *
	 * @return CommandRepeatability
	 */
	public function repeatability(): CommandRepeatability {
		return CommandRepeatability::Repeatable;
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params SendTestEmailRequest fields.
	 * @return array{ok:bool,detail:string,message_id:string}
	 */
	public function execute( array $claims, array $params ): array {
		// Validate required 'to' field.
		if ( ! array_key_exists( 'to', $params ) || ! is_string( $params['to'] ) || trim( $params['to'] ) === '' ) {
			return array( 'ok' => false, 'detail' => 'missing required field: to', 'message_id' => '' );
		}

		$to = sanitize_email( $params['to'] );
		if ( $to === '' ) {
			return array( 'ok' => false, 'detail' => 'invalid recipient email address', 'message_id' => '' );
		}

		$cfg = EmailConfig::load();
		if ( ! $cfg->is_configured() ) {
			return array( 'ok' => false, 'detail' => 'no email config — run sync_email_config first', 'message_id' => '' );
		}

		$subject = array_key_exists( 'subject', $params ) && is_string( $params['subject'] ) && $params['subject'] !== ''
			? $params['subject'] : 'Test Email from WPMgr';

		$body_text = array_key_exists( 'body', $params ) && is_string( $params['body'] ) && $params['body'] !== ''
			? $params['body']
			: 'This is a test message sent by WPMgr to verify your email provider configuration is working correctly.';

		// Optional connection key: route the test via a specific named connection.
		// Defaults to '' (primary row). Fallback is always disabled for test sends.
		$connection_key = '';
		if ( array_key_exists( 'connection', $params ) && is_string( $params['connection'] ) ) {
			$connection_key = $params['connection'];
		}

		$from_address = $cfg->from_address !== '' ? $cfg->from_address : 'wordpress@' . $this->site_domain();
		$from_name    = $cfg->from_name !== '' ? $cfg->from_name : 'WordPress';

		$mail = array(
			'to'          => array( $to ),
			'cc'          => array(),
			'bcc'         => array(),
			'reply_to'    => array(),
			'from'        => $from_address,
			'from_name'   => $from_name,
			'subject'     => $subject,
			'body_text'   => $body_text,
			'body_html'   => '',
			'charset'     => 'UTF-8',
			'headers'     => array(),
			'attachments' => array(),
			'return_path' => $cfg->return_path,
			'x_site_id'   => function_exists( 'get_option' ) ? (string) ( get_option( 'wpmgr_agent_site_id', '' ) ) : '',
			'x_tenant_id' => function_exists( 'get_option' ) ? (string) ( get_option( 'wpmgr_agent_tenant_id', '' ) ) : '',
		);

		// Route via the specific connection key when provided; disable_fallback is
		// always true for test sends so the real provider error surfaces.
		$result = $this->provider_router->send_via( $mail, $cfg, $connection_key, true );

		return array(
			'ok'         => $result['ok'],
			'detail'     => $result['ok'] ? 'test email sent successfully' : $result['detail'],
			'message_id' => $result['message_id'],
		);
	}

	/**
	 * Resolve the site's domain name for a fallback From address.
	 *
	 * @return string Domain name or 'localhost'.
	 */
	private function site_domain(): string {
		if ( function_exists( 'home_url' ) ) {
			$host = wp_parse_url( home_url(), PHP_URL_HOST );
			if ( is_string( $host ) && $host !== '' ) {
				return $host;
			}
		}
		return 'localhost';
	}
}
