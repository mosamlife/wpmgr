<?php
/**
 * Tests for ClearEmailConfigCommand: the way back to "no email provider" (GH #968).
 *
 * Before this command the only way to stop a site's mail going through a chosen
 * provider was to deactivate the plugin: sync_email_config refuses an empty
 * provider and nothing else removed the stored configuration.
 *
 * Every test here reaches the command through the same store production uses:
 * the real Keystore over an in-memory option table. Where a test needs the
 * keystore to fail, it says so by name.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use ReflectionProperty;
use WPMgr\Agent\Commands\ClearEmailConfigCommand;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandRepeatability;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\MailRouter;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;

/**
 * @covers \WPMgr\Agent\Commands\ClearEmailConfigCommand
 */
class ClearEmailConfigCommandTest extends TestCase
{
    private string $keyFile = '';

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private string $cpSecret = '';

    /** @var array<string,mixed> In-memory wp_options table. */
    private array $options = [];

    /** @var list<string> Option names passed to delete_option(), in order. */
    private array $deleted = [];

    /** @var list<string> Option names passed to update_option() or add_option(), in order. */
    private array $written = [];

    /** @var list<string> Option names whose delete_option() is refused: the row stays and false is returned. */
    private array $refuseDelete = [];

    /** @var list<string> Option names whose delete_option() throws before the row is touched. */
    private array $throwBeforeDelete = [];

    /** @var list<string> Option names whose delete_option() removes the row and then throws. */
    private array $throwAfterDelete = [];

    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();

        // Another test class may have left a global $wpdb behind; the connector
        // would then try to use it for its anti-replay table.
        unset($GLOBALS['wpdb']);

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-clearmail-' . bin2hex(random_bytes(8)) . '.key';
        file_put_contents($this->keyFile, random_bytes(32));

        $this->deleted           = [];
        $this->written           = [];
        $this->refuseDelete      = [];
        $this->throwBeforeDelete = [];
        $this->throwAfterDelete  = [];
        // Pin the keystore's master key to this test's own file so the run is
        // independent of the salts and WPMGR_AGENT_KEY_FILE other tests define.
        $this->options = [
            Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'file', 'path' => $this->keyFile],
        ];

        Functions\when('get_option')->alias(
            fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default
        );
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->written[]       = (string) $name;
            $this->options[$name]  = $value;
            return true;
        });
        Functions\when('add_option')->alias(function ($name, $value) {
            $this->written[]       = (string) $name;
            $this->options[$name]  = $value;
            return true;
        });
        Functions\when('delete_option')->alias(function ($name) {
            $this->deleted[] = (string) $name;
            if (in_array($name, $this->throwBeforeDelete, true)) {
                throw new \RuntimeException('a delete_option listener threw');
            }
            if (in_array($name, $this->refuseDelete, true)) {
                return false;
            }
            $existed = array_key_exists($name, $this->options);
            unset($this->options[$name]);
            if (in_array($name, $this->throwAfterDelete, true)) {
                throw new \RuntimeException('a deleted_option listener threw');
            }
            return $existed;
        });
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));

        // MailRouter::build_mail_payload() reads these.
        Functions\when('apply_filters')->returnArg(2);
        Functions\when('home_url')->justReturn('https://example.com/');

        // Router::authorizeCommand() and the REST registration.
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('register_rest_route')->justReturn(true);

        $this->resetShieldStash();
    }

    protected function tearDown(): void
    {
        $this->resetShieldStash();
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tearDown();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Seed a site that is fully configured: settings, the primary secret and a
     * named connection's secret, all written the way sync_email_config writes
     * them. Returns the keystore the command should be given.
     */
    private function seedConfiguredSite(): Keystore
    {
        $this->options[EmailConfig::OPTION] = [
            'provider'     => 'smtp',
            'from_address' => 'hello@example.com',
            'config'       => ['host' => 'smtp.example.com', 'port' => 587],
            'connections'  => ['backup' => ['provider' => 'sendgrid', 'config' => []]],
        ];

        $keystore = new Keystore();
        $keystore->storeEmailSecret('smtp-password');
        $keystore->store_connection_secrets(['backup' => 'sendgrid-api-key']);

        // Seeding is not part of what the command did.
        $this->written = [];
        $this->deleted = [];

        return $keystore;
    }

    /** @return list<string> The three option names the command may delete. */
    private function ownedOptions(): array
    {
        return [EmailConfig::OPTION, Keystore::OPTION_EMAIL_SECRET, Keystore::OPTION_EMAIL_CONN_SECRETS];
    }

    private function assertNoEmailRowsRemain(string $why): void
    {
        foreach ($this->ownedOptions() as $name) {
            $this->assertArrayNotHasKey($name, $this->options, $why . ': ' . $name . ' is still stored');
        }
    }

    // -------------------------------------------------------------------------
    // Identity
    // -------------------------------------------------------------------------

    public function test_declares_its_name_and_an_idempotent_write(): void
    {
        $cmd = new ClearEmailConfigCommand(new FakeKeystore());

        $this->assertSame('clear_email_config', $cmd->name());
        $this->assertSame(CommandEffect::Write, $cmd->effect());
        $this->assertSame(CommandRepeatability::Idempotent, $cmd->repeatability());
    }

    // -------------------------------------------------------------------------
    // The clear
    // -------------------------------------------------------------------------

    public function test_clears_settings_and_both_credential_stores_and_verifies_them_gone(): void
    {
        $keystore = $this->seedConfiguredSite();
        $this->assertSame('smtp-password', $keystore->get_email_secret(), 'precondition: the secret is stored');
        $this->assertSame('sendgrid-api-key', $keystore->get_connection_secret('backup'), 'precondition: the connection secret is stored');

        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        $this->assertTrue($result['ok']);
        $this->assertTrue($result['cleared']);
        $this->assertSame(
            ['config' => true, 'secret' => true, 'connection_secrets' => true],
            $result['existed']
        );
        $this->assertNoEmailRowsRemain('after a successful clear');
        $this->assertSame('', $keystore->get_email_secret());
        $this->assertSame('', $keystore->get_connection_secret('backup'));
        $this->assertFalse(EmailConfig::load()->is_configured(), 'the site must read as having no provider');
    }

    public function test_reports_which_items_existed(): void
    {
        $keystore = new Keystore();

        // Only the settings row.
        $this->options[EmailConfig::OPTION] = ['provider' => 'smtp'];
        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);
        $this->assertSame(['config' => true, 'secret' => false, 'connection_secrets' => false], $result['existed']);

        // Only the primary secret.
        $keystore->storeEmailSecret('only-the-secret');
        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);
        $this->assertSame(['config' => false, 'secret' => true, 'connection_secrets' => false], $result['existed']);

        // Only the connection secrets.
        $keystore->store_connection_secrets(['backup' => 'only-this']);
        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);
        $this->assertSame(['config' => false, 'secret' => false, 'connection_secrets' => true], $result['existed']);
        $this->assertTrue($result['cleared']);
    }

    public function test_a_stored_secret_that_cannot_be_decrypted_still_counts_as_existing_and_is_removed(): void
    {
        // A credential sealed under a key that is not this site's reads back as
        // '' through the keystore, so only the stored row itself can say it exists.
        $this->options[Keystore::OPTION_EMAIL_SECRET] = base64_encode(random_bytes(60));
        $keystore = new Keystore();
        $this->assertSame('', $keystore->get_email_secret(), 'precondition: the keystore cannot read it');

        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        $this->assertTrue($result['cleared']);
        $this->assertTrue($result['existed']['secret'], 'an unreadable row is still a stored credential');
        $this->assertNoEmailRowsRemain('after clearing an unreadable credential');
    }

    public function test_clearing_a_site_with_nothing_stored_succeeds_and_repeating_converges(): void
    {
        $keystore = new Keystore();
        $cmd      = new ClearEmailConfigCommand($keystore);

        $first = $cmd->execute([], []);
        $this->assertTrue($first['ok']);
        $this->assertTrue($first['cleared']);
        $this->assertSame(['config' => false, 'secret' => false, 'connection_secrets' => false], $first['existed']);

        // And on a configured site, the second run finds nothing left to do.
        $configured = $this->seedConfiguredSite();
        $cmd        = new ClearEmailConfigCommand($configured);
        $one        = $cmd->execute([], []);
        $two        = $cmd->execute([], []);

        $this->assertTrue($one['cleared']);
        $this->assertSame(['config' => true, 'secret' => true, 'connection_secrets' => true], $one['existed']);
        $this->assertTrue($two['ok']);
        $this->assertTrue($two['cleared']);
        $this->assertSame(['config' => false, 'secret' => false, 'connection_secrets' => false], $two['existed']);
        $this->assertNoEmailRowsRemain('after running twice');
    }

    // -------------------------------------------------------------------------
    // What the site does afterwards
    // -------------------------------------------------------------------------

    public function test_wp_mail_goes_back_to_wordpress_own_mailer_after_the_clear(): void
    {
        $keystore = $this->seedConfiguredSite();

        $sent           = 0;
        $providerRouter = $this->createMock(ProviderRouter::class);
        $providerRouter->method('send')->willReturnCallback(function () use (&$sent): array {
            ++$sent;
            return ['ok' => true, 'message_id' => 'm-1', 'detail' => ''];
        });
        $mailRouter = new MailRouter($providerRouter, new Settings());
        $atts       = ['to' => 'someone@example.com', 'subject' => 'Subject', 'message' => 'Body', 'headers' => ''];

        // Control: while a provider is stored the router takes the mail over.
        $before = $mailRouter->intercept(null, $atts);
        $this->assertIsArray($before, 'precondition: a configured site routes through the provider');
        $this->assertSame(1, $sent);

        (new ClearEmailConfigCommand($keystore))->execute([], []);

        // `null` is the one value that lets wp_mail() run WordPress's own mailer.
        $after = $mailRouter->intercept(null, $atts);
        $this->assertNull($after, 'with no provider stored the router must leave wp_mail() to WordPress');
        $this->assertSame(1, $sent, 'the provider must not be offered another message');
    }

    // -------------------------------------------------------------------------
    // Failure: never report a clear that did not happen
    // -------------------------------------------------------------------------

    public function test_leaves_the_credentials_alone_when_the_settings_cannot_be_removed(): void
    {
        $keystore = $this->seedConfiguredSite();
        // The settings row survives a delete: the DB refused, or a hook vetoed it.
        $this->refuseDelete = [EmailConfig::OPTION];

        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        $this->assertFalse($result['ok']);
        $this->assertFalse($result['cleared']);
        $this->assertStringContainsString('settings', $result['detail']);
        $this->assertSame(['config' => true, 'secret' => true, 'connection_secrets' => true], $result['existed']);
        // Removing the credentials while the provider is still configured would
        // leave a site that tries to send and cannot authenticate.
        $this->assertSame('smtp-password', $keystore->get_email_secret());
        $this->assertSame('sendgrid-api-key', $keystore->get_connection_secret('backup'));
        $this->assertSame([EmailConfig::OPTION], $this->deleted, 'nothing but the settings row may be attempted');
    }

    public function test_reports_a_credential_that_could_not_be_removed_and_still_clears_the_rest(): void
    {
        $keystore = $this->seedConfiguredSite();
        $this->refuseDelete = [Keystore::OPTION_EMAIL_SECRET];

        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        $this->assertFalse($result['ok']);
        $this->assertFalse($result['cleared'], 'a credential is still stored, so the site is not clear');
        $this->assertStringContainsString('secret', $result['detail']);
        $this->assertArrayNotHasKey(EmailConfig::OPTION, $this->options);
        $this->assertArrayNotHasKey(Keystore::OPTION_EMAIL_CONN_SECRETS, $this->options, 'one stuck row must not strand the other');
        $this->assertArrayHasKey(Keystore::OPTION_EMAIL_SECRET, $this->options);

        // The retry the control plane will make converges once the store works.
        $this->refuseDelete = [];
        $retry              = (new ClearEmailConfigCommand($keystore))->execute([], []);
        $this->assertTrue($retry['cleared']);
        $this->assertNoEmailRowsRemain('after the retry');
    }

    public function test_a_settings_delete_that_throws_before_removing_the_row_is_reported_and_touches_nothing_else(): void
    {
        $keystore = $this->seedConfiguredSite();
        $this->throwBeforeDelete = [EmailConfig::OPTION];

        // The exception must not escape: it would reach the control plane as an
        // opaque 500 instead of the answer it needs.
        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        $this->assertFalse($result['ok']);
        $this->assertFalse($result['cleared']);
        $this->assertStringContainsString('settings', $result['detail']);
        $this->assertArrayHasKey(EmailConfig::OPTION, $this->options);
        $this->assertSame('smtp-password', $keystore->get_email_secret());
        $this->assertSame('sendgrid-api-key', $keystore->get_connection_secret('backup'));
        $this->assertSame([EmailConfig::OPTION], $this->deleted);
    }

    public function test_a_settings_delete_that_throws_after_removing_the_row_still_clears_the_credentials(): void
    {
        $keystore = $this->seedConfiguredSite();
        $this->throwAfterDelete = [EmailConfig::OPTION];

        $result = (new ClearEmailConfigCommand($keystore))->execute([], []);

        // The row is gone, which is what was asked; a listener's fault afterwards
        // does not change that.
        $this->assertTrue($result['cleared']);
        $this->assertNoEmailRowsRemain('after a listener threw once the settings were already removed');
    }

    public function test_a_keystore_that_throws_is_reported_not_swallowed(): void
    {
        $this->seedConfiguredSite();
        $broken                                = new FakeKeystore('stuck');
        $broken->fail_store_email_secret       = true;
        $broken->fail_store_connection_secrets = true;

        $result = (new ClearEmailConfigCommand($broken))->execute([], []);

        $this->assertFalse($result['ok']);
        $this->assertFalse($result['cleared']);
        $this->assertStringContainsString('secret', $result['detail']);
        $this->assertStringContainsString('connection', $result['detail']);
        $this->assertArrayNotHasKey(EmailConfig::OPTION, $this->options, 'the provider must still be switched off');
    }

    // -------------------------------------------------------------------------
    // Scope: nothing else
    // -------------------------------------------------------------------------

    public function test_touches_only_its_three_options_and_ignores_the_request_body(): void
    {
        $keystore = $this->seedConfiguredSite();
        // Neighbours that must survive: the log retention state, the master key
        // pin and another plugin's option.
        $this->options['wpmgr_email_log_cursor'] = 41;
        $this->options['some_other_plugin']      = 'keep me';
        $before                                  = $this->options;
        unset($before[EmailConfig::OPTION], $before[Keystore::OPTION_EMAIL_SECRET], $before[Keystore::OPTION_EMAIL_CONN_SECRETS]);

        // A sync-shaped body must not turn this into a write.
        $result = (new ClearEmailConfigCommand($keystore))->execute(
            [],
            ['provider' => 'smtp', 'secret' => 'attacker', 'config' => ['host' => 'evil.example'], 'clear_secret' => false]
        );

        $this->assertTrue($result['cleared']);
        $deleted = $this->deleted;
        sort($deleted);
        $expected = $this->ownedOptions();
        sort($expected);
        $this->assertSame($expected, $deleted, 'the command may delete exactly these options');
        $this->assertSame([], $this->written, 'the command must never write an option');
        $this->assertSame($before, $this->options, 'every other option must be byte-for-byte unchanged');
    }

    // -------------------------------------------------------------------------
    // Registration and the signed-command gate
    // -------------------------------------------------------------------------

    /**
     * Fully-qualified names of every class the plugin's command list
     * instantiates, read from the source of Plugin::commands().
     *
     * @return list<string>
     */
    private function commandsInstantiatedByPlugin(): array
    {
        $file   = dirname(__DIR__, 2) . '/includes/class-plugin.php';
        $tokens = token_get_all((string) file_get_contents($file), TOKEN_PARSE);
        $count  = count($tokens);

        // Top-level imports: short name => fully-qualified name.
        $imports = [];
        $depth   = 0;
        for ($i = 0; $i < $count; $i++) {
            $tok = $tokens[$i];
            if ($tok === '{') {
                $depth++;
            } elseif ($tok === '}') {
                $depth--;
            } elseif (is_array($tok) && $tok[0] === T_USE && $depth === 0) {
                $name = '';
                for ($j = $i + 1; $j < $count && $tokens[$j] !== ';'; $j++) {
                    if (is_array($tokens[$j]) && in_array($tokens[$j][0], [T_NAME_QUALIFIED, T_STRING], true)) {
                        $name = $tokens[$j][1];
                    }
                }
                if ($name !== '') {
                    $imports[substr($name, (int) strrpos('\\' . $name, '\\'))] = $name;
                }
            }
        }

        // The body of function commands().
        $start = null;
        for ($i = 0; $i < $count - 2; $i++) {
            if (is_array($tokens[$i]) && $tokens[$i][0] === T_FUNCTION) {
                $j = $i + 1;
                while (is_array($tokens[$j]) && $tokens[$j][0] === T_WHITESPACE) {
                    $j++;
                }
                if (is_array($tokens[$j]) && $tokens[$j][0] === T_STRING && $tokens[$j][1] === 'commands') {
                    $start = $j;
                    break;
                }
            }
        }
        if ($start === null) {
            return [];
        }

        $names = [];
        $depth = 0;
        $open  = false;
        for ($i = $start; $i < $count; $i++) {
            $tok = $tokens[$i];
            if ($tok === '{') {
                $depth++;
                $open = true;
            } elseif ($tok === '}') {
                $depth--;
                if ($open && $depth === 0) {
                    break;
                }
            } elseif ($open && is_array($tok) && $tok[0] === T_NEW) {
                $j = $i + 1;
                while (is_array($tokens[$j]) && $tokens[$j][0] === T_WHITESPACE) {
                    $j++;
                }
                if (is_array($tokens[$j]) && in_array($tokens[$j][0], [T_STRING, T_NAME_QUALIFIED, T_NAME_FULLY_QUALIFIED], true)) {
                    $short = $tokens[$j][1];
                    if ($tokens[$j][0] === T_NAME_FULLY_QUALIFIED) {
                        $names[] = ltrim($short, '\\');
                    } else {
                        $names[] = $imports[$short] ?? 'WPMgr\\Agent\\' . $short;
                    }
                }
            }
        }

        return $names;
    }

    public function test_is_registered_in_the_plugin_command_list_beside_its_sibling(): void
    {
        $names = $this->commandsInstantiatedByPlugin();

        // Positive controls: a reader that finds nothing, or misses the sibling,
        // would pass the real assertion vacuously.
        $this->assertGreaterThan(50, count($names), 'the command list reader found too few commands to be trusted');
        $this->assertContains('WPMgr\\Agent\\Commands\\SyncEmailConfigCommand', $names);

        $this->assertContains('WPMgr\\Agent\\Commands\\ClearEmailConfigCommand', $names, 'Plugin::commands() must register the command');
        // A missing `use` import would resolve the bare name into the wrong
        // namespace and fatal when the list is built, so the name must load.
        $this->assertTrue(class_exists('WPMgr\\Agent\\Commands\\ClearEmailConfigCommand'));
        foreach ($names as $name) {
            if (str_ends_with($name, 'ClearEmailConfigCommand')) {
                $this->assertSame('WPMgr\\Agent\\Commands\\ClearEmailConfigCommand', $name);
            }
        }
    }

    private function routerFor(Keystore $keystore): Router
    {
        $pair           = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($pair);
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($pair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;
        // Registration seeded rows too; the command's own deletions start here.
        $this->written = [];

        return new Router(new Connector($keystore, new Settings()), [new ClearEmailConfigCommand($keystore)]);
    }

    private function request(?string $token): \WP_REST_Request
    {
        $request = new \WP_REST_Request('POST', '/wpmgr/v1/command/clear_email_config');
        $request->set_url_params(['command' => 'clear_email_config']);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        if ($token !== null) {
            $request->set_header('Authorization', 'Bearer ' . $token);
        }
        $request->set_body('{}');

        return $request;
    }

    private function mintToken(string $cmd, ?string $signingKey = null): string
    {
        $key = $signingKey ?? $this->cpSecret;
        if ($key === '') {
            $this->fail('no signing key: routerFor() must run before a token is minted');
        }

        $segments = [
            $this->b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])),
            $this->b64((string) json_encode([
                'aud' => $this->siteId,
                'cmd' => $cmd,
                'jti' => bin2hex(random_bytes(8)),
                'exp' => time() + 30,
            ])),
        ];
        $segments[] = $this->b64(sodium_crypto_sign_detached(implode('.', $segments), $key));

        return implode('.', $segments);
    }

    private function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    private function resetShieldStash(): void
    {
        $prop = new ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }

    public function test_a_validly_signed_request_clears_the_site_over_the_router(): void
    {
        $keystore = $this->seedConfiguredSite();
        $router   = $this->routerFor($keystore);
        $request  = $this->request($this->mintToken('clear_email_config'));

        $this->assertTrue($router->authorizeCommand($request, 'clear_email_config'), 'the signed request was not authorized');
        $response = $router->handleCommand($request);

        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertSame(200, $response->status);
        $this->assertIsArray($response->data);
        $this->assertTrue($response->data['ok']);
        $this->assertTrue($response->data['cleared']);
        $this->assertSame(['config' => true, 'secret' => true, 'connection_secrets' => true], $response->data['existed']);
        $this->assertNoEmailRowsRemain('after the signed command');
    }

    public function test_a_request_with_no_token_is_refused_and_clears_nothing(): void
    {
        $keystore = $this->seedConfiguredSite();
        $router   = $this->routerFor($keystore);

        $result = $router->authorizeCommand($this->request(null), 'clear_email_config');

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame([], $this->deleted);
        $this->assertArrayHasKey(EmailConfig::OPTION, $this->options);
        $this->assertSame('smtp-password', $keystore->get_email_secret());
    }

    public function test_a_token_minted_for_the_sibling_command_is_refused(): void
    {
        $keystore = $this->seedConfiguredSite();
        $router   = $this->routerFor($keystore);

        // A genuine, unexpired token the control plane signed for sync_email_config
        // must not be accepted on this route.
        $result = $router->authorizeCommand($this->request($this->mintToken('sync_email_config')), 'clear_email_config');

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame([], $this->deleted);
        $this->assertArrayHasKey(EmailConfig::OPTION, $this->options);
    }

    public function test_a_token_signed_by_a_different_key_is_refused(): void
    {
        $keystore = $this->seedConfiguredSite();
        $router   = $this->routerFor($keystore);
        $other    = sodium_crypto_sign_secretkey(sodium_crypto_sign_keypair());

        $result = $router->authorizeCommand($this->request($this->mintToken('clear_email_config', $other)), 'clear_email_config');

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame([], $this->deleted);
        $this->assertArrayHasKey(EmailConfig::OPTION, $this->options);
    }
}
