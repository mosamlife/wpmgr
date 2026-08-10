<?php
/**
 * EmailKeystoreInterface — thin seam over the agent Keystore for the email layer.
 *
 * Extracted so ProviderRouter and SyncEmailConfigCommand are testable without
 * depending on the concrete (final) Keystore class directly.
 *
 * @package WPMgr\Agent\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Email;

/**
 * Minimal email-secret storage contract.
 */
interface EmailKeystoreInterface
{
    /**
     * Retrieve and decrypt the per-site email provider secret.
     * Returns an empty string when no secret has been stored.
     *
     * @return string
     */
    public function get_email_secret(): string;

    /**
     * Persist the per-site email provider secret, encrypted.
     * Passing an empty string removes any stored secret.
     *
     * @param string $secret Plaintext secret.
     * @return void
     */
    public function storeEmailSecret(string $secret): void;

    /**
     * Persist the per-connection secret map, AES-256-GCM-encrypted.
     *
     * The map is keyed by connection_key (slug) and values are plaintext
     * secrets. Stored as a single AES-encrypted JSON blob under option
     * wpmgr_agent_email_conn_secrets; replaced atomically on every sync.
     * Passing an empty array removes any stored connection secrets.
     *
     * @param array<string,string> $secrets Map of connection_key => plaintext secret.
     * @return void
     */
    public function store_connection_secrets( array $secrets ): void;

    /**
     * Retrieve the decrypted plaintext secret for a named connection.
     * Returns an empty string when no secret is stored for that key.
     *
     * @param string $connection_key Operator-chosen connection slug.
     * @return string Decrypted secret, or '' when absent.
     */
    public function get_connection_secret( string $connection_key ): string;
}
