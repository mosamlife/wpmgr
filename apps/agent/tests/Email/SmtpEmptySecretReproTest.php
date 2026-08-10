<?php
/**
 * GH #380 regression: "smtp continue fails with no reason on website that
 * already have configured all the settings and have already sent successful
 * email", reported error "SMTP Error: Could not authenticate".
 *
 * Two production behaviours combined to produce that report:
 *
 *   1. sync_email_config treated an absent or empty `secret` field as an
 *      instruction to DELETE the stored credential. The control plane emits
 *      that same empty value whenever it cannot resolve a secret of its own,
 *      so a routine config push wiped a working password while leaving the
 *      host, port and username intact.
 *
 *   2. SmtpHandler then authenticated with that empty password. The server
 *      refuses, and the failure the operator sees is the provider's generic
 *      "SMTP Error: Could not authenticate" — indistinguishable from a wrong
 *      password, which is why re-typing the same password sometimes appeared
 *      to fix it (it re-populated what had been deleted).
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPMailer\PHPMailer\FakePhpMailerRegistry;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Commands\SyncEmailConfigCommand;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\Handlers\SmtpHandler;
use WPMgr\Agent\Email\ProviderHandlerInterface;
use WPMgr\Agent\Email\ProviderRouter;

require_once __DIR__ . '/fake-phpmailer.php';

/**
 * @covers \WPMgr\Agent\Commands\SyncEmailConfigCommand
 * @covers \WPMgr\Agent\Email\Handlers\SmtpHandler
 */
class SmtpEmptySecretReproTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    /**
     * A working SMTP config the operator has already configured and used.
     *
     * @return array<string,mixed>
     */
    private function smtp_config(): array
    {
        return [
            'host'     => 'smtp.example.com',
            'port'     => 587,
            'auth'     => true,
            'username' => 'postmaster@example.com',
        ];
    }

    /**
     * @return array<string,mixed>
     */
    private function base_mail(): array
    {
        return [
            'to'          => ['recipient@example.com'],
            'cc'          => [],
            'bcc'         => [],
            'reply_to'    => [],
            'from'        => 'sender@example.com',
            'from_name'   => 'WPMgr Test',
            'subject'     => 'Hello',
            'body_text'   => 'Plain body',
            'body_html'   => '',
            'charset'     => 'UTF-8',
            'headers'     => [],
            'attachments' => [],
            'return_path' => false,
            'x_site_id'   => 'site-abc',
            'x_tenant_id' => 'tenant-abc',
        ];
    }

    private function stub_options(): void
    {
        Functions\when('get_option')->alias(
            fn($key) => $key === EmailConfig::OPTION ? [] : false
        );
        Functions\when('update_option')->justReturn(true);
    }

    // -------------------------------------------------------------------------
    // Leg 1: a config push must never delete a stored credential
    // -------------------------------------------------------------------------

    public function test_config_push_without_a_secret_field_keeps_the_stored_credential(): void
    {
        $this->stub_options();

        $keystore = new FakeKeystore('the-working-password');
        $cmd      = new SyncEmailConfigCommand($keystore);

        // The control plane re-pushes the connection settings; it says nothing
        // at all about the secret.
        $result = $cmd->execute([], [
            'provider' => 'smtp',
            'config'   => $this->smtp_config(),
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame(
            'the-working-password',
            $keystore->get_email_secret(),
            'a push that says nothing about the secret must leave the stored credential alone'
        );
        $this->assertSame([], $keystore->stored, 'the keystore must not be written at all');
    }

    public function test_config_push_with_an_empty_secret_keeps_the_stored_credential(): void
    {
        $this->stub_options();

        $keystore = new FakeKeystore('the-working-password');
        $cmd      = new SyncEmailConfigCommand($keystore);

        // Every control plane at or before 0.61.130 always transmits the field,
        // sending "" when it could not resolve a secret of its own. That is an
        // unresolved value, not an instruction to delete a working credential.
        $result = $cmd->execute([], [
            'provider' => 'smtp',
            'config'   => $this->smtp_config(),
            'secret'   => '',
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame(
            'the-working-password',
            $keystore->get_email_secret(),
            'an empty secret is an unresolved value, not a delete instruction'
        );
    }

    // -------------------------------------------------------------------------
    // Leg 2: never authenticate with an empty password
    // -------------------------------------------------------------------------

    public function test_send_with_auth_on_and_no_password_reports_the_missing_credential(): void
    {
        $handler = new SmtpHandler();
        $result  = $handler->send($this->base_mail(), $this->smtp_config(), '');

        $this->assertFalse($result['ok']);
        $this->assertStringNotContainsString(
            'Could not authenticate',
            $result['error'],
            'a missing credential must not be reported as a rejected one'
        );
        $this->assertStringContainsString('password', strtolower($result['error']));

        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertNotNull($phpmailer);
        $this->assertSame(
            '',
            $phpmailer->Password,
            'the handler must never hand an empty password to an authenticated session'
        );
    }

    public function test_send_with_auth_on_and_a_password_still_dispatches(): void
    {
        $handler = new SmtpHandler();
        $result  = $handler->send($this->base_mail(), $this->smtp_config(), 'the-working-password');

        $this->assertTrue($result['ok'], 'a configured credential must still send');
        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertTrue($phpmailer->SMTPAuth);
        $this->assertSame('the-working-password', $phpmailer->Password);
    }

    // -------------------------------------------------------------------------
    // Diagnostic: a stored credential that will not decrypt reads back as ''
    // exactly like no credential at all. Say which one it was.
    // -------------------------------------------------------------------------

    public function test_undecryptable_stored_credential_is_named_and_never_dispatched(): void
    {
        $keystore                 = new FakeKeystore('');
        $keystore->decrypt_failed = true;

        $logger = $this->createMock(EmailLogger::class);
        $logger->method('write')->willReturn(1);

        $handler = $this->createMock(ProviderHandlerInterface::class);
        $handler->method('provider')->willReturn('smtp');
        $handler->expects($this->never())->method('send');

        $router = new ProviderRouter($keystore, $logger);
        $router->register($handler);

        $result = $router->send(
            $this->base_mail(),
            new EmailConfig(['provider' => 'smtp', 'config' => $this->smtp_config(), 'log_emails' => false])
        );

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('could not be decrypted', $result['detail']);
    }
}
