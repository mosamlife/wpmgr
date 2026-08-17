<?php
/**
 * EmailLogFailureBypassTest — GH #381 phase 4: ProviderRouter::maybe_log()
 * discarded a DETECTED failure whenever the site owner had turned log_emails
 * off, silently defeating phase 1's failure alerting for that tenant.
 *
 * log_emails is a volume-and-retention preference about keeping a record of
 * mail that WORKED. A failure is an incident, not a record of routine
 * traffic; someone asking for a quieter log did not ask to be blinded to
 * failures. store_body is an independent gate (EmailLogger::write()) that
 * controls whether ANY row -- success or failure -- carries the message
 * body, and it is untouched by this change: turning log_emails off must
 * still mean no bodies are ever stored, and must still mean a SUCCESSFUL
 * send is not recorded at all.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\ProviderHandlerInterface;
use WPMgr\Agent\Email\ProviderRouter;

/**
 * @covers \WPMgr\Agent\Email\ProviderRouter
 */
class EmailLogFailureBypassTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();
        Functions\when('current_time')->justReturn('2026-08-17 00:00:00');
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    /** @param array{ok:bool,message_id:string,error:string,provider_response:string} $result */
    private function make_handler(string $provider, array $result): ProviderHandlerInterface
    {
        $handler = $this->createMock(ProviderHandlerInterface::class);
        $handler->method('provider')->willReturn($provider);
        $handler->method('send')->willReturn($result);
        return $handler;
    }

    /** @return array<string,mixed> */
    private function base_mail(): array
    {
        return [
            'to'          => ['to@example.com'],
            'cc'          => [],
            'bcc'         => [],
            'reply_to'    => [],
            'from'        => 'a@example.com',
            'from_name'   => 'Sender',
            'subject'     => 'Test',
            'body_text'   => 'SECRET-BODY-TEXT',
            'body_html'   => '',
            'charset'     => 'UTF-8',
            'headers'     => [],
            'attachments' => [],
            'return_path' => false,
            'x_site_id'   => 'site-123',
        ];
    }

    /**
     * Minimal fake $wpdb: records every insert() call's $data array so a
     * test can assert row count and shape without a real database. Same
     * pattern as MailFailureCaptureTest::fakeWpdb().
     */
    private function fakeWpdb(): object
    {
        return new class () {
            public string $prefix = 'wp_';

            /** @var array<int,array<string,mixed>> */
            public array $rows = [];

            public int $insert_id = 0;

            /** @param array<string,mixed> $data */
            public function insert(string $table, array $data, array $formats): bool
            {
                $this->rows[] = $data;
                $this->insert_id++;
                return true;
            }
        };
    }

    /**
     * RED today: maybe_log() returns early on `! $cfg->log_emails` before
     * ever looking at the outcome, so a DETECTED failure (ok:false, an error
     * string already computed by send()) produces zero rows -- nothing for
     * the push to ship, nothing for the control plane to alert on.
     */
    public function test_failed_send_is_logged_even_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'smtp connect() failed: Connection refused',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertFalse($result['ok']);
        $this->assertCount(1, $fakeWpdb->rows, 'a detected failure must be logged even when log_emails is off');
        $this->assertSame('failed', $fakeWpdb->rows[0]['status']);
        $this->assertSame('smtp connect() failed: Connection refused', $fakeWpdb->rows[0]['error']);

        unset($GLOBALS['wpdb']);
    }

    /**
     * The important control: this must be GREEN both BEFORE and AFTER the
     * fix. It guards the bypass from quietly becoming "ignore the
     * preference entirely" -- a successful send is exactly the record a
     * user who turned log_emails off asked NOT to keep.
     */
    public function test_successful_send_is_still_not_logged_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => true,
            'message_id'        => 'smtp-ok-1',
            'error'             => '',
            'provider_response' => '250 OK',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertTrue($result['ok']);
        $this->assertCount(0, $fakeWpdb->rows, 'a successful send must not be recorded when log_emails is off');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Privacy gate: store_body stays independently false regardless of the
     * newly-widened failure path. Prove the raw message body never reaches
     * any column of the failure row this fix now writes.
     */
    public function test_failed_row_carries_no_message_body_when_store_body_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'smtp connect() failed: Connection refused',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $router->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertSame(0, $row['body_stored'], 'body_stored must be 0 when store_body is false, even on the newly-widened failure path');
        $this->assertNull($row['body'], 'body column must be NULL when store_body is false');
        $this->assertStringNotContainsString('SECRET-BODY-TEXT', (string) json_encode($row));
        foreach ($row as $column => $value) {
            $this->assertNotSame('SECRET-BODY-TEXT', $value, "Column '{$column}' must never carry the raw message body when store_body is false.");
        }

        unset($GLOBALS['wpdb']);
    }

    /**
     * Over-fire control: with log_emails TRUE, behaviour is completely
     * unchanged for both success and failure -- this path was never gated
     * by the outcome before, and must not become gated by it now.
     */
    public function test_log_emails_true_behaviour_is_unchanged_for_success_and_failure(): void
    {
        $cfg = new EmailConfig(['provider' => 'smtp', 'log_emails' => true, 'store_body' => false]);

        // Success leg.
        $fakeWpdbOk        = $this->fakeWpdb();
        $GLOBALS['wpdb']   = $fakeWpdbOk;
        $okHandler         = $this->make_handler('smtp', [
            'ok'                => true,
            'message_id'        => 'smtp-ok-2',
            'error'             => '',
            'provider_response' => '250 OK',
        ]);
        $okRouter = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $okRouter->register($okHandler);
        $okRouter->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdbOk->rows, 'a successful send must still be logged when log_emails is on');
        $this->assertSame('sent', $fakeWpdbOk->rows[0]['status']);
        unset($GLOBALS['wpdb']);

        // Failure leg.
        $fakeWpdbFail      = $this->fakeWpdb();
        $GLOBALS['wpdb']   = $fakeWpdbFail;
        $failHandler       = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'boom',
            'provider_response' => '',
        ]);
        $failRouter = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $failRouter->register($failHandler);
        $failRouter->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdbFail->rows, 'a failed send must still be logged when log_emails is on');
        $this->assertSame('failed', $fakeWpdbFail->rows[0]['status']);
        unset($GLOBALS['wpdb']);
    }
}
