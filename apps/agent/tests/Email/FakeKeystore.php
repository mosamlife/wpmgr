<?php
/**
 * FakeKeystore — lightweight test double for EmailKeystoreInterface.
 *
 * Used by ProviderRouter and SyncEmailConfigCommand tests. Captures all
 * storeEmailSecret() calls in the public $stored array for assertions.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use WPMgr\Agent\Email\EmailKeystoreInterface;

/**
 * Minimal Keystore stand-in for email-layer unit tests.
 *
 * We cannot extend Keystore (it is final) and we cannot createMock() it
 * (same reason). Instead, tests that need to control get_email_secret() /
 * storeEmailSecret() and the new per-connection secret methods use this class.
 */
final class FakeKeystore implements EmailKeystoreInterface
{
    private string $secret;

    /** @var array<string> Captured storeEmailSecret() calls. */
    public array $stored = [];

    /** @var array<string,string> Per-connection secrets map. */
    public array $conn_secrets = [];

    /** @var array<array<string,string>> Captured store_connection_secrets() calls. */
    public array $stored_conn_secrets = [];

    public function __construct(string $initial_secret = '')
    {
        $this->secret = $initial_secret;
    }

    public function get_email_secret(): string
    {
        return $this->secret;
    }

    public function storeEmailSecret(string $secret): void
    {
        $this->stored[] = $secret;
        $this->secret   = $secret;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,string> $secrets Map of connection_key => plaintext secret.
     * @return void
     */
    public function store_connection_secrets( array $secrets ): void
    {
        $this->stored_conn_secrets[] = $secrets;
        $this->conn_secrets          = $secrets;
    }

    /**
     * {@inheritDoc}
     *
     * @param string $connection_key Operator-chosen connection slug.
     * @return string Decrypted secret, or '' when absent.
     */
    public function get_connection_secret( string $connection_key ): string
    {
        return $this->conn_secrets[ $connection_key ] ?? '';
    }
}
