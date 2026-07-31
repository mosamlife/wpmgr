<?php
/**
 * ProviderHandlerInterface: contract for per-provider outgoing-mail handlers.
 *
 * Every handler receives a normalised mail payload built from the wp_mail()
 * arguments (to, subject, message, headers, attachments) plus the resolved
 * connection config and the decrypted secret. It returns a structured result
 * envelope carrying the provider HTTP status, message-id (when available), and
 * a human error string on failure.
 *
 * Address fields (to/cc/bcc/reply_to/from) intentionally stay list<string> /
 * string on this contract rather than becoming a structured {address,name}
 * shape (GH #312). Normalizing happens at the point each handler actually
 * needs a bare address, via WPMgr\Agent\Email\AddressParser, not on this
 * contract itself:
 *   - SmtpHandler and SendgridHandler both hand a raw entry straight to
 *     something that requires a bare address (PHPMailer::addAddress() et al.,
 *     or SendGrid's `email` field) and MUST parse it first.
 *   - SesHandler / PostmarkHandler / MailgunHandler build a raw header line or
 *     an API field that is itself RFC 5322 "Display Name <addr>" AND
 *     comma-list aware, so they pass entries straight through unparsed. This
 *     is not an oversight; see AddressParserTest and each handler's own
 *     class docblock.
 * Keeping the wire shape unchanged means a caller that has not been updated
 * for a new address feature still sends correct mail instead of nothing at
 * all. A structured-entry contract change would require touching all five
 * handlers, every payload builder, and every existing test fixture in one
 * commit, which is the wrong trade-off for an urgent fix.
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

/**
 * Contract implemented by each per-provider send handler.
 */
interface ProviderHandlerInterface {

	/**
	 * Send one email via the provider.
	 *
	 * @param array<string,mixed> $mail   Normalised mail payload:
	 *   to         string[]    Recipients. Each entry is a raw wp_mail() header
	 *                          value: a bare address, the RFC 5322 "Display Name
	 *                          <addr@example.com>" form, or a single entry that
	 *                          itself packs a comma-separated list (e.g. one "Cc:"
	 *                          header carrying two addresses). Entries are NOT
	 *                          pre-split or pre-parsed. Before using any entry as
	 *                          a BARE address (PHPMailer::addAddress() and
	 *                          friends, or a provider API's `email` field), run it
	 *                          through WPMgr\Agent\Email\AddressParser::parse_list()
	 *                          (or ::parse_list_verbose() to also learn what was
	 *                          rejected). Never assume an entry already is one.
	 *                          A bad entry must never fatal the whole send; drop
	 *                          it and, when the provider protocol allows it, note
	 *                          it in provider_response. When the destination field
	 *                          is itself RFC 5322 "Name <addr>" AND comma-list
	 *                          aware (a raw header line, or an API field
	 *                          documented to accept that form) the entries may be
	 *                          passed through unparsed instead.
	 *   cc         string[]    CC recipients. Same shape as `to`.
	 *   bcc        string[]    BCC recipients. Same shape as `to`.
	 *   reply_to   string[]    Reply-To addresses. Same shape as `to`.
	 *   from       string      Resolved From address (after force-from logic).
	 *                          May also carry the "Display Name <addr>" form;
	 *                          parse it the same way before using it as a bare
	 *                          address.
	 *   from_name  string      Resolved From display name.
	 *   subject    string      Subject line.
	 *   body_text  string      Plain-text body part (may be empty).
	 *   body_html  string      HTML body part (may be empty).
	 *   headers    string[]    Extra raw headers (already applied; informational).
	 *   attachments list<array{name:string,path:string,mime:string}> Attachments.
	 *   return_path bool       Whether to set a Return-Path / bounce address.
	 *   x_site_id  string      Site-ID correlation header value.
	 * @param array<string,mixed> $config Non-secret provider settings from EmailConfig.
	 * @param string              $secret Decrypted provider secret (password/API key).
	 * @return array{ok:bool,message_id:string,error:string,provider_response:string}
	 */
	public function send( array $mail, array $config, string $secret ): array;

	/**
	 * Provider slug (smtp|ses|sendgrid|mailgun|postmark).
	 *
	 * @return string
	 */
	public function provider(): string;
}
