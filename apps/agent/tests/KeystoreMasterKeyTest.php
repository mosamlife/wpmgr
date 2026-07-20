<?php
/**
 * Tests for portable, deterministic master-key acquisition in the keystore:
 * salt derivation (incl. the placeholder-skip + loosened-length gate), file
 * fallback (uploads-first candidate order + hardening + atomic self-heal),
 * legacy-file reuse, the last-resort database-stored key (GH #257, including
 * its atomic-claim race guard), and source pinning for cross-request
 * determinism.
 *
 * Final resolution ladder exercised here: constant -> salts ->
 * file (uploads -> wp-content -> dirname(ABSPATH), backward-compat last) ->
 * database key. There is no system-temp-directory tier (removed: predictable
 * per-site path on shared /tmp is a symlink-plant / directory-ownership risk
 * for a co-tenant).
 *
 * Each test runs in a separate process because master-key resolution depends on
 * process-global constants (WPMGR_AGENT_KEY_FILE, ABSPATH, WP salts) that can
 * only be defined once per process.
 *
 * Design note: bootstrap.php defines ABSPATH before any test code runs. Tests
 * in this class therefore operate on the fixed bootstrap ABSPATH rather than
 * trying to redefine it. dirname(ABSPATH) is a controlled subdirectory of
 * sys_get_temp_dir() created by bootstrap — not sys_get_temp_dir() itself —
 * so its writability and contents can be managed per-test.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Keystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class KeystoreMasterKeyTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /**
     * Autoload flag captured from the third update_option()/fourth
     * add_option() argument, keyed by option name. Used to assert the
     * DB-fallback master key is stored non-autoloaded.
     *
     * @var array<string,mixed>
     */
    private array $optionAutoload = [];

    /** @var list<string> Temp paths/dirs to clean up. */
    private array $cleanup = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options        = [];
        $this->optionAutoload = [];
        $this->cleanup        = [];

        Functions\when('update_option')->alias(function ($name, $value, $autoload = null) {
            $this->options[$name]        = $value;
            $this->optionAutoload[$name] = $autoload;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name], $this->optionAutoload[$name]);
            return true;
        });
        // Real add_option() semantics: INSERT-only, returns false (and does
        // NOT overwrite) if the option row already exists. This is what makes
        // it usable as an atomic claim for keyFromDatabase()'s race guard.
        Functions\when('add_option')->alias(function ($name, $value, $deprecated = '', $autoload = null) {
            if (array_key_exists($name, $this->options)) {
                return false;
            }
            $this->options[$name]        = $value;
            $this->optionAutoload[$name] = $autoload;
            return true;
        });
    }

    protected function tear_down(): void
    {
        foreach ($this->cleanup as $path) {
            if (is_file($path)) {
                @unlink($path);
            } elseif (is_dir($path)) {
                @array_map('unlink', glob($path . '/*') ?: []);
                @rmdir($path);
            }
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Resolve dirname(ABSPATH) — the legacy-file candidate path and (now)
     * last-resort key-write candidate the keystore checks.
     */
    private function absParent(): string
    {
        return rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\');
    }

    /** A set of realistic, high-entropy salt values (64 chars each). */
    private function defineRealSalts(): void
    {
        foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY',
                  'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $i => $name) {
            if (!defined($name)) {
                define($name, str_repeat(chr(97 + $i), 8) . bin2hex(random_bytes(28)));
            }
        }
    }

    /**
     * Force every file candidate (uploads, wp-content, dirname(ABSPATH)) to
     * be unusable, so the discovery ladder is forced all the way to the
     * database tier. Uploads is left unmocked (the default test stub reports
     * no basedir, which candidateKeyDirs() treats identically to "unwritable":
     * the candidate is simply not added).
     */
    private function blockEveryFileCandidate(): void
    {
        $content = sys_get_temp_dir() . '/wpmgr-content-ro-' . bin2hex(random_bytes(6));
        mkdir($content, 0500, true);
        define('WP_CONTENT_DIR', $content);
        $this->cleanup[] = $content;

        $absParent = $this->absParent();
        chmod($absParent, 0500);
        register_shutdown_function(static function () use ($absParent): void {
            @chmod($absParent, 0755);
        });
    }

    public function test_salt_derivation_is_stable_and_32_bytes(): void
    {
        // Remove any legacy key file that might have been left by a prior run.
        // The keystore checks for a legacy file at dirname(ABSPATH) before it
        // attempts salt derivation, so a stale file would cause a false 'file'
        // source result instead of the expected 'salts'.
        $legacyPath = $this->absParent() . '/.wpmgr-agent-master.key';
        @unlink($legacyPath);

        $this->defineRealSalts();

        $keystore = new Keystore();

        // Round-trip exercises masterKey() twice (encrypt + decrypt).
        $envelope = $keystore->encrypt('payload-under-salts');
        $this->assertSame('payload-under-salts', $keystore->decrypt($envelope));

        // Source must be pinned to salts.
        $this->assertSame(['source' => 'salts'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);

        // A fresh instance (new request) must derive the SAME key and decrypt.
        $fresh = new Keystore();
        $this->assertSame('payload-under-salts', $fresh->decrypt($envelope));
    }

    /**
     * GOLDEN BACKWARD-COMPAT test: a fixed, reproducible set of 8 real salts
     * (no placeholders among them) must derive the EXACT SAME key before and
     * after this change (and its follow-up security-review fixes). Because
     * none of these salts are placeholders, the placeholder-skip loop change
     * cannot alter their handling; because their combined length already
     * clears the original SALT_MIN_COMBINED_LENGTH gate, the new OR'd
     * SALT_MIN_VALUE_BYTES gate cannot alter the accept/reject decision
     * either. None of the file/DB-tier hardening touches keyFromSalts() at
     * all. The hex below was computed from the ORIGINAL (pre-fix)
     * keyFromSalts() implementation against these exact salt values —
     * asserting it is still produced today is the guard that proves an
     * existing salts-pinned install is never re-keyed by this change.
     */
    public function test_golden_salt_derivation_key_is_unchanged_by_the_fix(): void
    {
        $legacyPath = $this->absParent() . '/.wpmgr-agent-master.key';
        @unlink($legacyPath);

        foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY',
                  'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $i => $name) {
            define($name, str_repeat((string) $i, 64));
        }

        // ReflectionMethod::setAccessible() is unneeded (and deprecated since
        // PHP 8.5): PHP 8.1+ reflection can invoke a private method directly.
        $keystore = new Keystore();
        $method   = new \ReflectionMethod(Keystore::class, 'masterKey');
        /** @var string $key */
        $key = $method->invoke($keystore);

        $this->assertSame(
            '90c0edaa74209232fa76daea675adafb6a250b8130f4dbdcc16c59815dc1c160',
            bin2hex($key)
        );
        $this->assertSame(['source' => 'salts'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
    }

    public function test_single_placeholder_salt_is_skipped_and_remaining_real_salts_still_derive(): void
    {
        $legacyPath = $this->absParent() . '/.wpmgr-agent-master.key';
        @unlink($legacyPath);

        // A single placeholder salt no longer poisons the whole derivation:
        // it is skipped, and the two remaining real 64-char salts are enough
        // to derive (their combined length alone already clears the original
        // 96-byte gate).
        define('AUTH_KEY', 'put your unique phrase here');
        define('SECURE_AUTH_KEY', str_repeat('z', 64));
        define('LOGGED_IN_KEY', str_repeat('y', 64));

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('payload');
        $this->assertSame('payload', $keystore->decrypt($envelope));

        $this->assertSame(['source' => 'salts'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);

        // A fresh instance derives the identical key.
        $fresh = new Keystore();
        $this->assertSame('payload', $fresh->decrypt($envelope));
    }

    public function test_all_placeholder_salts_fall_through_to_file(): void
    {
        // Every salt is still the wp-config-sample.php placeholder (the
        // untouched-config scenario): the whole derivation is unusable and
        // falls through to the file tier, same as before this change.
        foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY',
                  'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $name) {
            define($name, 'put your unique phrase here');
        }

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('payload');
        $this->assertSame('payload', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertFileExists($marker['path']);

        $this->cleanup[] = $marker['path'];
        $this->cleanup[] = dirname($marker['path']);
    }

    public function test_single_real_salt_meets_loosened_value_bytes_gate(): void
    {
        $legacyPath = $this->absParent() . '/.wpmgr-agent-master.key';
        @unlink($legacyPath);

        // A single 64-char salt: "AUTH_KEY=" + 64 chars + "\n" = 74 bytes of
        // combined material, BELOW the original 96-byte
        // SALT_MIN_COMBINED_LENGTH gate (the old code would have rejected
        // this and fallen through to a file). The raw salt VALUE alone (64
        // bytes) clears the new SALT_MIN_VALUE_BYTES=32 gate, so the loosened
        // OR'd check now accepts it.
        define('AUTH_KEY', str_repeat('q', 64));

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('single-salt-payload');
        $this->assertSame('single-salt-payload', $keystore->decrypt($envelope));
        $this->assertSame(['source' => 'salts'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
    }

    public function test_short_salts_are_rejected(): void
    {
        // Too little combined material under BOTH gates (old and new) ->
        // salts unusable; falls to the file tier.
        define('AUTH_KEY', 'short');
        define('NONCE_SALT', 'tiny');

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('p');
        $this->assertSame('p', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);

        $this->cleanup[] = $marker['path'];
        $this->cleanup[] = dirname($marker['path']);
    }

    public function test_uploads_first_candidate_is_chosen_and_hardened(): void
    {
        // No salts. Uploads is now the FIRST file candidate for a brand-new
        // install (GH #257): mock wp_upload_dir() to a fresh writable dir and
        // confirm it wins over any other candidate.
        $uploads = sys_get_temp_dir() . '/wpmgr-uploads-' . bin2hex(random_bytes(6));
        mkdir($uploads, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploads]);

        $this->cleanup[] = $uploads . '/wpmgr-agent';
        $this->cleanup[] = $uploads;

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('uploads-payload');
        $this->assertSame('uploads-payload', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($uploads . '/wpmgr-agent/master.key', $marker['path']);

        // Web-root directory must be hardened.
        $dir = dirname($marker['path']);
        $this->assertFileExists($dir . '/index.php');
        $this->assertFileExists($dir . '/.htaccess');
        $this->assertStringContainsString('Require all denied', (string) file_get_contents($dir . '/.htaccess'));
    }

    public function test_wp_content_candidate_used_when_uploads_unavailable_and_hardens_webroot(): void
    {
        // No salts, uploads unavailable (default test stub returns no
        // basedir) -> WP_CONTENT_DIR is the next candidate in the new
        // uploads-first order (previously it was reached only after the now-
        // deprioritized dirname(ABSPATH) candidate).
        $content = sys_get_temp_dir() . '/wpmgr-content-' . bin2hex(random_bytes(6));
        mkdir($content, 0700, true);
        define('WP_CONTENT_DIR', $content);

        $this->cleanup[] = $content . '/wpmgr-agent';
        $this->cleanup[] = $content;

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('webroot-payload');
        $this->assertSame('webroot-payload', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($content . '/wpmgr-agent/master.key', $marker['path']);

        // Web-root directory must be hardened.
        $dir = dirname($marker['path']);
        $this->assertFileExists($dir . '/index.php');
        $this->assertFileExists($dir . '/.htaccess');
        $this->assertStringContainsString('Require all denied', (string) file_get_contents($dir . '/.htaccess'));
    }

    public function test_dirname_abspath_used_as_last_file_candidate_before_db_tier(): void
    {
        // No salts, uploads unavailable, WP_CONTENT_DIR undefined -> falls
        // directly to dirname(ABSPATH), the last file candidate, since the
        // system-temp tier has been removed entirely (co-tenant symlink-plant
        // risk on shared, non-CageFS /tmp — see MUST-FIX 2 of the #257
        // security review). The database tier is never reached here because
        // dirname(ABSPATH) is writable by default in this test environment.
        $keystore = new Keystore();
        $envelope = $keystore->encrypt('abspath-fallback-payload');
        $this->assertSame('abspath-fallback-payload', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($this->absParent() . '/.wpmgr-agent-master.key', $marker['path']);

        $this->cleanup[] = $marker['path'];
    }

    public function test_preexisting_legacy_key_file_is_reused(): void
    {
        // No salts. Plant a legacy key file at the path existingKeyFilePaths()
        // checks: dirname(ABSPATH)/.wpmgr-agent-master.key. The keystore must
        // detect it (via the READ-side backward-compat check, which is
        // unaffected by the candidateKeyDirs() WRITE-side reorder) and pin
        // source='file' pointing to that exact path.
        $legacyPath = $this->absParent() . '/.wpmgr-agent-master.key';
        $knownKey   = random_bytes(32);
        file_put_contents($legacyPath, $knownKey);

        $this->cleanup[] = $legacyPath;

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('legacy-payload');

        // Pinned to the legacy file path; no new key generated.
        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($legacyPath, $marker['path']);
        $this->assertSame($knownKey, (string) file_get_contents($legacyPath));

        // A fresh instance reads the same legacy file and decrypts.
        $fresh = new Keystore();
        $this->assertSame('legacy-payload', $fresh->decrypt($envelope));
    }

    public function test_uploads_existing_valid_key_is_adopted_not_overwritten(): void
    {
        // Plant a valid 32-byte key at the uploads candidate path BEFORE any
        // Keystore runs (simulates a first-run this test doesn't itself
        // trigger, or a concurrent writer that already created it). The
        // atomic O_EXCL create in createKeyFile() must fail with EEXIST and
        // ADOPT this key rather than overwrite it.
        $uploads = sys_get_temp_dir() . '/wpmgr-uploads-' . bin2hex(random_bytes(6));
        mkdir($uploads . '/wpmgr-agent', 0755, true);
        $existingKey = random_bytes(32);
        file_put_contents($uploads . '/wpmgr-agent/master.key', $existingKey);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploads]);

        $this->cleanup[] = $uploads . '/wpmgr-agent';
        $this->cleanup[] = $uploads;

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('adopt-payload');

        // The file on disk is UNCHANGED — never overwritten.
        $this->assertSame(
            $existingKey,
            (string) file_get_contents($uploads . '/wpmgr-agent/master.key')
        );

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($uploads . '/wpmgr-agent/master.key', $marker['path']);

        $fresh = new Keystore();
        $this->assertSame('adopt-payload', $fresh->decrypt($envelope));
    }

    public function test_lost_pin_readopts_existing_uploads_key_before_deriving_from_newly_available_salts(): void
    {
        // Simulate an install that ORIGINALLY established its key at the
        // uploads candidate (no salts were available then). Its pin marker is
        // lost/corrupt (no OPTION_MASTER_KEY_SOURCE stored), AND wp-config.php
        // has since gained real salts. The existing uploads key file must be
        // re-adopted with priority over deriving a brand-new key from salts —
        // otherwise every prior ciphertext under the uploads key is orphaned.
        $uploads = sys_get_temp_dir() . '/wpmgr-uploads-' . bin2hex(random_bytes(6));
        mkdir($uploads . '/wpmgr-agent', 0755, true);
        $existingKey = random_bytes(32);
        file_put_contents($uploads . '/wpmgr-agent/master.key', $existingKey);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploads]);
        $this->cleanup[] = $uploads . '/wpmgr-agent';
        $this->cleanup[] = $uploads;

        $this->defineRealSalts();

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('reconverge-payload');

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($uploads . '/wpmgr-agent/master.key', $marker['path']);

        // File on disk unchanged.
        $this->assertSame(
            $existingKey,
            (string) file_get_contents($uploads . '/wpmgr-agent/master.key')
        );

        $fresh = new Keystore();
        $this->assertSame('reconverge-payload', $fresh->decrypt($envelope));
    }

    public function test_partial_write_fails_candidate_and_falls_through_to_db_without_pinning_file(): void
    {
        // No salts, uploads unavailable, WP_CONTENT_DIR undefined -> the only
        // file candidate reached is dirname(ABSPATH), which stays genuinely
        // writable. Force its write to land short (16 of 32 bytes),
        // simulating a disk-full/quota condition mid-write.
        Functions\when('fwrite')->justReturn(16);

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('partial-write-payload');
        $this->assertSame('partial-write-payload', $keystore->decrypt($envelope));

        // Must NOT have pinned 'file' with a half-written key: the candidate
        // failed cleanly and the ladder fell through to the database tier.
        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('db', $marker['source']);

        // The half-written file must have been rolled back (deleted), not
        // left behind as a corrupt stub future requests could stumble over.
        $this->cleanup[] = $this->absParent() . '/.wpmgr-agent-master.key';
        $this->assertFileDoesNotExist($this->absParent() . '/.wpmgr-agent-master.key');
    }

    public function test_constant_path_takes_priority_and_creates_file(): void
    {
        $path = sys_get_temp_dir() . '/wpmgr-const-' . bin2hex(random_bytes(6)) . '.key';
        define('WPMGR_AGENT_KEY_FILE', $path);
        $this->cleanup[] = $path;

        // Salts also present, but the constant wins.
        $this->defineRealSalts();

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('const-payload');
        $this->assertSame('const-payload', $keystore->decrypt($envelope));

        $this->assertFileExists($path);
        $this->assertSame(32, strlen((string) file_get_contents($path)));
        $this->assertSame(['source' => 'constant'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
    }

    public function test_pinned_constant_source_throws_when_constant_is_removed(): void
    {
        // Simulate a request AFTER an earlier one pinned 'constant', but this
        // process never defines WPMGR_AGENT_KEY_FILE (the operator removed
        // it from wp-config.php). Must throw rather than silently deriving a
        // DIFFERENT key from salts/file, which would orphan everything
        // already encrypted under the original constant-backed key.
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'constant'];
        $this->defineRealSalts();

        $keystore = new Keystore();
        $this->expectException(\RuntimeException::class);
        $keystore->encrypt('irrelevant');
    }

    public function test_unknown_pinned_source_throws(): void
    {
        // A corrupt/foreign/future OPTION_MASTER_KEY_SOURCE marker must fail
        // closed like every recognised pinned source, not silently
        // re-discover (which could mint/derive a DIFFERENT key and re-key an
        // install that already has ciphertext).
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'quantum-entangled'];

        $keystore = new Keystore();
        $this->expectException(\RuntimeException::class);
        $keystore->encrypt('irrelevant');
    }

    public function test_db_key_is_used_when_salts_and_all_file_candidates_fail(): void
    {
        $this->blockEveryFileCandidate();

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('db-payload');
        $this->assertSame('db-payload', $keystore->decrypt($envelope));

        $this->assertSame(['source' => 'db'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $this->assertArrayHasKey(Keystore::OPTION_DB_MASTER_KEY, $this->options);
        // Stored non-autoloaded (the add_option() fourth-argument autoload flag).
        $this->assertFalse($this->optionAutoload[Keystore::OPTION_DB_MASTER_KEY] ?? null);

        $storedRaw = base64_decode((string) $this->options[Keystore::OPTION_DB_MASTER_KEY], true);
        $this->assertIsString($storedRaw);
        $this->assertSame(32, strlen($storedRaw));

        // A fresh instance (new request) re-reads the SAME stored key rather
        // than generating a new one — if it regenerated, this decrypt would
        // fail.
        $fresh = new Keystore();
        $this->assertSame('db-payload', $fresh->decrypt($envelope));
    }

    public function test_db_key_add_option_race_adopts_concurrent_winner(): void
    {
        // Simulate a concurrent request (e.g. an admin_init keystore retry
        // overlapping an in-flight Enroll POST) that has already won the
        // atomic claim by the time THIS request's add_option() call runs:
        // add_option() returns false (the row already exists), exactly like
        // the real wp_options UNIQUE(option_name) constraint would after a
        // losing INSERT. The winner's key is what actually lands in the
        // option store.
        $winnerKey = random_bytes(32);
        Functions\when('add_option')->alias(function ($name, $value, $deprecated = '', $autoload = null) use ($winnerKey) {
            // The "winner" already stored its key before this call arrives.
            $this->options[$name] = base64_encode($winnerKey);
            return false;
        });

        $this->blockEveryFileCandidate();

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('race-payload');

        // Must decrypt correctly, proving the LOSER adopted the winner's key
        // instead of using its own freshly generated (and discarded) one.
        $this->assertSame('race-payload', $keystore->decrypt($envelope));
        $this->assertSame(
            base64_encode($winnerKey),
            $this->options[Keystore::OPTION_DB_MASTER_KEY],
            'the loser must adopt the concurrently-persisted winner key, not overwrite it'
        );
    }

    public function test_pinned_db_source_throws_when_stored_key_is_missing(): void
    {
        // A prior request pinned 'db', but the option holding the actual key
        // is gone (deleted/corrupted). Must throw rather than silently
        // generating a brand-new key, which would orphan everything already
        // encrypted under the original database-stored key.
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'db'];

        $keystore = new Keystore();
        $this->expectException(\RuntimeException::class);
        $keystore->encrypt('irrelevant');
    }

    public function test_disable_db_key_constant_reaches_final_throw_without_writing_a_db_key(): void
    {
        define('WPMGR_AGENT_DISABLE_DB_KEY', true);

        // Same "everything else fails" setup as
        // test_db_key_is_used_when_salts_and_all_file_candidates_fail(), so
        // the only remaining tier is the (now disabled) database fallback.
        $this->blockEveryFileCandidate();

        $keystore = new Keystore();
        $threw    = false;
        try {
            $keystore->encrypt('x');
        } catch (\RuntimeException $e) {
            $threw = true;
        }

        $this->assertTrue($threw, 'Expected the final master-key establishment throw.');
        $this->assertArrayNotHasKey(Keystore::OPTION_DB_MASTER_KEY, $this->options);
    }
}
