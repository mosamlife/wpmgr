<?php
/**
 * RouterCommandFailureLogTest: #754 — a command that throws must write the
 * detailed diagnostic line to the site's debug log.
 *
 * Why a subprocess. DebugLog::write() is gated on the WPMGR_DEBUG / WP_DEBUG
 * constants, and a PHP constant cannot be undefined once set. Defining
 * WPMGR_DEBUG inside the main PHPUnit process would silently switch agent
 * logging on for every test that ran after this file, which is precisely the
 * cross-test leakage this suite works hard to avoid. So the assertion runs in
 * a child process that defines the constant, points error_log at a temp file,
 * and drives the REAL production path (handleCommand -> dispatch -> catch).
 *
 * This also makes the test honest about the gate: it proves the line appears
 * when debug is ON and — the control — that nothing is written when it is OFF.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Router
 */
final class RouterCommandFailureLogTest extends TestCase
{
	/** @var array<int,string> Temp files to clean up. */
	private array $temp = [];

	protected function tear_down(): void
	{
		foreach ( $this->temp as $path ) {
			if ( is_file( $path ) ) {
				@unlink( $path );
			}
		}
		$this->temp = [];
		parent::tear_down();
	}

	/**
	 * #754: with debug enabled, the failure writes ONE line carrying the
	 * exception class, the raw message and a file:line location. That line is
	 * what the reporter had to patch a live site by hand to obtain.
	 */
	public function test_command_failure_writes_the_detailed_line_to_the_debug_log(): void
	{
		$run = $this->runInSubprocess( true, 'Keystore: ciphertext authentication failed.' );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$log = $run['log'];

		$this->assertNotSame( '', trim( $log ), 'debug log is empty; nothing was written' );
		$this->assertStringContainsString( 'WPMgr Agent: command failed:', $log );
		$this->assertStringContainsString( 'command=boom', $log );
		$this->assertStringContainsString( 'class=RuntimeException', $log );
		$this->assertStringContainsString( 'reason=Keystore: ciphertext authentication failed.', $log );
		$this->assertMatchesRegularExpression( '/ at=\S+:\d+ /', $log, 'the log line must carry a file:line location' );
	}

	/**
	 * #754: the local log is the site owner's own file, so it may hold what the
	 * response may not — here, an absolute path. This is the asymmetry the fix
	 * depends on, so it is asserted rather than assumed: paths as thrown
	 * locally, redacted on the wire.
	 */
	public function test_debug_log_keeps_the_path_detail_the_response_redacts(): void
	{
		$secret = '/home/customer123/public_html/wp-content/uploads/wpmgr/staging';
		$run    = $this->runInSubprocess( true, 'cannot create staging dir: ' . $secret );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		// Local log: the operator gets the whole truth.
		$this->assertStringContainsString( $secret, $run['log'] );

		// Wire: the same failure, redacted.
		$this->assertStringNotContainsString( $secret, $run['stdout'] );
		$this->assertStringNotContainsString( 'customer123', $run['stdout'] );
		$this->assertStringContainsString( 'cannot create staging dir', $run['stdout'] );
	}

	/**
	 * #754: key material never reaches the log, which can be readable over the
	 * web; paths do, because they are the site owner's own diagnostic. A padded
	 * standard-base64 key and an access-key-shaped token are redacted from the
	 * logged line, and an absolute path outside every known root survives.
	 */
	public function test_debug_log_redacts_key_material_but_keeps_paths(): void
	{
		$padded = 'JpXsrsvpkJ4QU9D43mEqo/DWxzeE2nX9GPs/Zi7BpTA=';
		$akia   = 'AKIAJ7Q2ZK4XN8PLW3RD6YTBVC5MHGFS9UEO1I0A';
		$path   = '/home/customer123/public_html/private/backup-dir';

		$run = $this->runInSubprocess( true, 'unwrap failed for ' . $padded . ' and ' . $akia . ' at ' . $path );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertStringContainsString( 'WPMgr Agent: command failed:', $run['log'] );
		$this->assertStringNotContainsString( $padded, $run['log'] );
		$this->assertStringNotContainsString( 'JpXsrsvpkJ4QU9D43mEqo', $run['log'] );
		$this->assertStringNotContainsString( $akia, $run['log'] );
		$this->assertStringContainsString( 'reason=unwrap failed for <redacted> and <redacted> at ' . $path, $run['log'] );
	}

	/**
	 * #754: the log line fails closed like the response. When a key-material
	 * pass cannot complete, the line carries the withheld marker in place of
	 * the message, never the message unredacted.
	 */
	public function test_debug_log_withholds_the_message_when_a_pass_cannot_complete(): void
	{
		$padded = 'JpXsrsvpkJ4QU9D43mEqo/DWxzeE2nX9GPs/Zi7BpTA=';

		$run = $this->runInSubprocess(
			true,
			'unwrap failed for ' . $padded,
			"ini_set('pcre.jit', '0'); ini_set('pcre.backtrack_limit', '1');"
		);

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertStringContainsString( 'reason=(reason withheld: redaction could not complete)', $run['log'] );
		$this->assertStringNotContainsString( 'JpXsrsvpkJ4QU9D43mEqo', $run['log'] );
		$this->assertStringContainsString( 'command=boom', $run['log'] );
	}

	/**
	 * #764: escapeControlChars() itself fails closed, independently of
	 * logReason()'s own withheld-message case above. A mutant that made
	 * escapeControlChars() return its input on a PCRE engine failure — or
	 * that dropped the "?? self::LOG_LINE_WITHHELD" guarding the dispatch()
	 * call site — would let a raw control byte, one able to start a forged
	 * log line or hide text from a terminal, reach the log untouched. This
	 * covers the dispatch()/command-failure call site only; the
	 * authorizeCommand() call site's own "?? self::LOG_LINE_WITHHELD" is
	 * covered separately below, by
	 * test_authorize_failure_log_line_withholds_the_whole_line_when_escaping_cannot_complete().
	 *
	 * Forced the same way the test above forces logReason()'s passes to
	 * fail: pcre.jit off and a backtrack budget of 1, so any regex that has
	 * to actually match something exhausts it and preg_replace_callback()
	 * returns null. The message here is short enough that logReason()'s own
	 * two passes have nothing to match (no run anywhere near 32 characters)
	 * and hand the ESC straight through unchanged, so escapeControlChars()
	 * — and only escapeControlChars() — is what is put under test here.
	 *
	 * The withheld marker must replace the WHOLE line, not just the reason:
	 * the command name and every other field are lost along with it, which
	 * is what distinguishes this from the reason-only withholding above.
	 */
	public function test_debug_log_withholds_the_whole_line_when_escaping_cannot_complete(): void
	{
		$run = $this->runInSubprocess(
			true,
			"before\x1Bafter",
			"ini_set('pcre.jit', '0'); ini_set('pcre.backtrack_limit', '1');"
		);

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertStringContainsString(
			'WPMgr Agent: log line withheld (control-character escaping could not complete).',
			$run['log']
		);
		$this->assertStringNotContainsString( "\x1B", $run['log'], 'a raw control byte reached the log' );
		$this->assertStringNotContainsString( 'before', $run['log'], 'the withheld marker must replace the whole line' );
		$this->assertStringNotContainsString( 'command=boom', $run['log'], 'the withheld marker must replace the whole line, not just the reason' );
	}

	/**
	 * #754: one failure is ONE log line, whatever line breaks the message
	 * carries. They are written as the visible sequences \n and \r\n, so the
	 * whole text survives and nothing in the message can start a line of its
	 * own — including text shaped like another entry.
	 */
	public function test_a_multiline_message_is_logged_as_one_line(): void
	{
		$run = $this->runInSubprocess(
			true,
			"first line\nsecond line\r\nWPMgr Agent: command failed: command=forged"
		);

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$lines = array_values(
			array_filter(
				preg_split( '/\R/', $run['log'] ) ?: [],
				static function ( string $line ): bool {
					return trim( $line ) !== '';
				}
			)
		);

		$this->assertCount( 1, $lines, 'the failure was logged as ' . count( $lines ) . " lines:\n" . $run['log'] );
		$this->assertStringContainsString(
			'reason=first line\nsecond line\r\nWPMgr Agent: command failed: command=forged',
			$lines[0]
		);
		$this->assertStringContainsString( 'command=boom', $lines[0] );
	}

	/**
	 * #754: a WordPress install at a Windows drive root has ABSPATH 'D:\'. That
	 * is a filesystem root, not a known root: it must not be stripped from the
	 * message, or every path on the drive would reach the response as a
	 * relative-looking remainder the absolute-path rule no longer recognises.
	 * Both separators, as the message may carry either.
	 *
	 * Run in a child process because ABSPATH is a constant this process has
	 * already defined.
	 */
	public function test_a_drive_root_abspath_is_not_a_known_root(): void
	{
		$run = $this->runInSubprocess(
			false,
			'cannot open D:\\Users\\JohnSmith\\private\\backup or D:/Users/JohnSmith/private/backup',
			'',
			"define('ABSPATH', 'D:\\\\');"
		);

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$response = json_decode( $run['stdout'], true );
		$this->assertIsArray( $response, 'no JSON response: ' . $run['stdout'] . $run['stderr'] );
		$this->assertSame(
			'Command execution failed: RuntimeException: cannot open <path> or <path>',
			$response['message'] ?? null
		);
		$this->assertStringNotContainsString( 'JohnSmith', $run['stdout'] );
	}

	/**
	 * Control: with debug disabled the log stays empty. A production install
	 * must not start writing on every command failure.
	 */
	public function test_nothing_is_written_when_debug_is_disabled(): void
	{
		$run = $this->runInSubprocess( false, 'Keystore: ciphertext authentication failed.' );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertSame( '', trim( $run['log'] ), 'debug log must stay empty when debugging is off' );

		// The RESPONSE is still diagnostic — that is the whole point of #754:
		// a log-only fix would leave an operator without debug enabled with
		// nothing at all.
		$this->assertStringContainsString( 'ciphertext authentication failed', $run['stdout'] );
	}

	/**
	 * #764: the dispatch()/command-failure call site must not even build, let
	 * alone escape, the log line when debug logging is off. Counters on
	 * escapeControlChars() and logReason() prove the negative directly: a
	 * mutant that removed dispatch()'s DebugLog::isEnabled() gate would still
	 * write nothing to error_log() (nothing is listening on a production
	 * install with debugging off), so the empty-log assertion above cannot by
	 * itself catch that mutant — only the call count can.
	 */
	public function test_dispatch_gate_skips_building_and_escaping_when_debug_is_disabled(): void
	{
		$run = $this->runDispatchGateProbeInSubprocess( 'Keystore: ciphertext authentication failed.' );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertSame( '', trim( $run['log'] ), 'debug log must stay empty when debugging is off' );
		$this->assertStringContainsString( 'escapeControlChars_calls=0', $run['stdout'] );
		$this->assertStringContainsString( 'logReason_calls=0', $run['stdout'] );
	}

	/**
	 * Drive handleCommand() -> dispatch() in a child process with debug OFF,
	 * counting calls to escapeControlChars() and logReason() via Patchwork,
	 * restored in finally inside the subprocess script (Patchwork
	 * redefinitions leak across tests, so the fake stays self-contained even
	 * though this process exits right after). A dedicated probe rather than
	 * adding a counter to runInSubprocess(): every existing caller of
	 * runInSubprocess() parses $run['stdout'] as the bare JSON response body,
	 * and prefixing counters onto that stdout would corrupt every one of
	 * those decodes.
	 *
	 * @param string $message Exception message the command throws.
	 * @return array{status:int,stdout:string,stderr:string,log:string}
	 */
	private function runDispatchGateProbeInSubprocess( string $message ): array
	{
		$logPath    = sys_get_temp_dir() . '/wpmgr_router_gate_log_' . uniqid( '', true ) . '.log';
		$scriptPath = sys_get_temp_dir() . '/wpmgr_router_gate_log_' . uniqid( '', true ) . '.php';

		$this->temp[] = $logPath;
		$this->temp[] = $scriptPath;

		$bootstrap  = addslashes( __DIR__ . '/bootstrap.php' );
		$logEscaped = addslashes( $logPath );
		$msgB64     = base64_encode( $message );

		$script = <<<PHP
<?php
declare(strict_types=1);

// debug intentionally OFF

ini_set('log_errors', '1');
ini_set('error_log', '{$logEscaped}');

require '{$bootstrap}';

\$escapeCalls = 0;
\$escapeHandle = \\Patchwork\\redefine(
    'WPMgr\\Agent\\Router::escapeControlChars',
    function (string \$line) use (&\$escapeCalls) {
        \$escapeCalls++;
        return \\Patchwork\\relay();
    }
);

\$reasonCalls = 0;
\$reasonHandle = \\Patchwork\\redefine(
    'WPMgr\\Agent\\Router::logReason',
    function (string \$msg) use (&\$reasonCalls) {
        \$reasonCalls++;
        return \\Patchwork\\relay();
    }
);

\$command = new class implements \\WPMgr\\Agent\\Commands\\CommandInterface {
    public function name(): string
    {
        return 'boom';
    }

    public function effect(): \\WPMgr\\Agent\\Commands\\CommandEffect
    {
        return \\WPMgr\\Agent\\Commands\\CommandEffect::Read;
    }

    public function repeatability(): \\WPMgr\\Agent\\Commands\\CommandRepeatability
    {
        return \\WPMgr\\Agent\\Commands\\CommandRepeatability::Idempotent;
    }

    /** @param array<string,mixed> \$claims @param array<string,mixed> \$params @return array<string,mixed> */
    public function execute(array \$claims, array \$params): array
    {
        throw new \\RuntimeException(base64_decode('{$msgB64}'));
    }
};

\$rc        = new \\ReflectionClass(\\WPMgr\\Agent\\Connector::class);
\$connector = \$rc->newInstanceWithoutConstructor();
\$router    = new \\WPMgr\\Agent\\Router(\$connector, [\$command]);

\$request = new \\WP_REST_Request(
    [
        'command'      => 'boom',
        'wpmgr_claims' => ['sub' => 'site-uuid', 'cmd' => 'boom'],
    ]
);

try {
    \$response = \$router->handleCommand(\$request);
} finally {
    \\Patchwork\\restore(\$escapeHandle);
    \\Patchwork\\restore(\$reasonHandle);
}

if (!(\$response instanceof \\WP_Error)) {
    fwrite(STDERR, "expected WP_Error\\n");
    exit(2);
}

echo "escapeControlChars_calls={\$escapeCalls}\\n";
echo "logReason_calls={\$reasonCalls}\\n";
PHP;

		file_put_contents( $scriptPath, $script );

		$descriptors = [
			1 => [ 'pipe', 'w' ],
			2 => [ 'pipe', 'w' ],
		];

		$process = proc_open(
			[ PHP_BINARY, '-d', 'memory_limit=1G', $scriptPath ],
			$descriptors,
			$pipes
		);

		$this->assertIsResource( $process, 'could not start the subprocess' );

		$stdout = (string) stream_get_contents( $pipes[1] );
		$stderr = (string) stream_get_contents( $pipes[2] );
		fclose( $pipes[1] );
		fclose( $pipes[2] );
		$status = proc_close( $process );

		return [
			'status' => $status,
			'stdout' => $stdout,
			'stderr' => $stderr,
			'log'    => is_file( $logPath ) ? (string) file_get_contents( $logPath ) : '',
		];
	}

	// -------------------------------------------------------------------------
	// #764: every control character other than tab is escaped, not just CR/LF.
	// One test per character class, each asserting exactly one log line with
	// the character escaped and the text either side of it intact — nothing
	// after it lost, which is the NUL case's own failure mode under a raw
	// error_log() call.
	// -------------------------------------------------------------------------

	public function test_debug_log_escapes_nul(): void
	{
		$this->assertClassEscaped( "before\x00after", 'before\x00after' );
	}

	public function test_debug_log_escapes_vertical_tab(): void
	{
		$this->assertClassEscaped( "before\x0Bafter", 'before\x0Bafter' );
	}

	public function test_debug_log_escapes_form_feed(): void
	{
		$this->assertClassEscaped( "before\x0Cafter", 'before\x0Cafter' );
	}

	/**
	 * An ESC-initiated CSI sequence (here, "set foreground red"): the whole
	 * sequence's ESC byte is escaped, which is enough to stop a terminal from
	 * acting on it. The literal bytes that follow ESC are ordinary printable
	 * ASCII and are not themselves control characters, so they survive as-is.
	 */
	public function test_debug_log_escapes_escape_sequence(): void
	{
		$this->assertClassEscaped( "before\x1B[31mafter", 'before\x1B[31mafter' );
	}

	public function test_debug_log_escapes_del(): void
	{
		$this->assertClassEscaped( "before\x7Fafter", 'before\x7Fafter' );
	}

	/**
	 * NEL, U+0085 — a C1 control some readers treat as a line break in its own
	 * right, encoded here as it would arrive in a valid UTF-8 message: the
	 * two bytes \xC2\x85.
	 */
	public function test_debug_log_escapes_nel(): void
	{
		$this->assertClassEscaped( "before\xC2\x85after", 'before\u{0085}after' );
	}

	public function test_debug_log_escapes_line_separator(): void
	{
		$this->assertClassEscaped( "before\xE2\x80\xA8after", 'before\u{2028}after' );
	}

	public function test_debug_log_escapes_paragraph_separator(): void
	{
		$this->assertClassEscaped( "before\xE2\x80\xA9after", 'before\u{2029}after' );
	}

	/**
	 * Regression: CR and LF still fold to the two-character sequences \r and
	 * \n exactly as before #764, byte-identical to what
	 * test_a_multiline_message_is_logged_as_one_line already proves for a
	 * full multi-line message. This is the same check restated against the
	 * single-character corpus the rest of this battery uses, so a future
	 * change to escapeControlChars() that regresses folding fails right next
	 * to the class it broke.
	 */
	public function test_debug_log_still_folds_cr_and_lf(): void
	{
		$this->assertClassEscaped( "before\r\nafter", 'before\r\nafter' );
	}

	/**
	 * #764: every test above pins one representative member per range — NUL
	 * for C0, NEL for C1, and so on. That leaves a narrowed range undetected:
	 * a mutant that escaped only NEL out of all of U+0080-U+009F, or that
	 * dropped FS/GS/RS (0x1C-0x1E) from the C0 branch, would still pass every
	 * test above it. This test drives every other member through the same
	 * real log path in one message, so no member of a range can be quietly
	 * dropped from the escaped class without a mismatch showing up here.
	 *
	 * Covers: C0 0x01-0x08, 0x0B, 0x0C, 0x0E-0x1F (0x00, 0x09, 0x0A and 0x0D
	 * are excluded — NUL, LF and CR already have their own tests above and
	 * fold differently; tab is asserted separately, below, to NOT escape),
	 * DEL, every one of the 32 C1 controls U+0080-U+009F, and both Unicode
	 * separators U+2028 and U+2029. Each character sits between two 'A'
	 * markers so a character that was dropped instead of escaped would run
	 * two markers together rather than silently vanishing from the count.
	 */
	public function test_debug_log_escapes_every_member_of_every_control_class(): void
	{
		$chars    = [];
		$expected = [];

		foreach ( array_merge( range( 0x01, 0x08 ), [ 0x0B, 0x0C ], range( 0x0E, 0x1F ), [ 0x7F ] ) as $ord ) {
			$chars[]    = chr( $ord );
			$expected[] = sprintf( '\\x%02X', $ord );
		}

		for ( $ord = 0x80; $ord <= 0x9F; $ord++ ) {
			$chars[]    = "\xC2" . chr( $ord );
			$expected[] = sprintf( '\\u{%04X}', $ord );
		}

		$chars[]    = "\xE2\x80\xA8"; // U+2028 LINE SEPARATOR.
		$expected[] = '\u{2028}';
		$chars[]    = "\xE2\x80\xA9"; // U+2029 PARAGRAPH SEPARATOR.
		$expected[] = '\u{2029}';

		$raw  = 'A' . implode( 'A', $chars ) . 'A';
		$want = 'A' . implode( 'A', $expected ) . 'A';

		$run = $this->runInSubprocess( true, $raw );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$lines = array_values(
			array_filter(
				preg_split( '/\R/', $run['log'] ) ?: [],
				static function ( string $line ): bool {
					return trim( $line ) !== '';
				}
			)
		);

		$this->assertCount( 1, $lines, 'expected exactly one log line, got ' . count( $lines ) . ":\n" . $run['log'] );
		$this->assertStringContainsString( 'reason=' . $want, $lines[0] );

		foreach ( $chars as $raw_char ) {
			$this->assertStringNotContainsString( $raw_char, $lines[0], 'a raw control byte reached the log' );
		}
	}

	/**
	 * #764: tab is the one C0 character escapeControlChars() must leave
	 * alone. Pinned on its own so a mutant that widens the escaped class to
	 * include it cannot hide inside the "every other member" battery above,
	 * which deliberately excludes it.
	 */
	public function test_debug_log_leaves_tab_unescaped(): void
	{
		$this->assertClassEscaped( "before\tafter", "before\tafter" );
	}

	/**
	 * Drive one message through the real command-failure log path and assert
	 * the log holds exactly one line containing 'reason=' followed by
	 * $expectedEscaped.
	 *
	 * @param string $raw             Message containing the character(s) under test,
	 *                                always "before<char>after".
	 * @param string $expectedEscaped What the log line's reason= holds instead,
	 *                                always "before<escaped form>after".
	 */
	private function assertClassEscaped( string $raw, string $expectedEscaped ): void {
		$run = $this->runInSubprocess( true, $raw );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$lines = array_values(
			array_filter(
				preg_split( '/\R/', $run['log'] ) ?: [],
				static function ( string $line ): bool {
					return trim( $line ) !== '';
				}
			)
		);

		$this->assertCount( 1, $lines, 'expected exactly one log line, got ' . count( $lines ) . ":\n" . $run['log'] );
		$this->assertStringContainsString( 'reason=' . $expectedEscaped, $lines[0] );
	}

	/**
	 * Drive Router::handleCommand() in a child PHP process.
	 *
	 * @param bool   $debug   Whether to define WPMGR_DEBUG.
	 * @param string $message Exception message the command throws.
	 * @param string $before  PHP run just before the command is dispatched.
	 * @param string $prelude PHP run before the test bootstrap, e.g. to define
	 *                        a constant the bootstrap would otherwise define.
	 * @return array{status:int,stdout:string,stderr:string,log:string}
	 */
	private function runInSubprocess( bool $debug, string $message, string $before = '', string $prelude = '' ): array
	{
		$logPath    = sys_get_temp_dir() . '/wpmgr_router_log_' . uniqid( '', true ) . '.log';
		$scriptPath = sys_get_temp_dir() . '/wpmgr_router_log_' . uniqid( '', true ) . '.php';

		$this->temp[] = $logPath;
		$this->temp[] = $scriptPath;

		$bootstrap  = addslashes( __DIR__ . '/bootstrap.php' );
		$logEscaped = addslashes( $logPath );
		// base64, not addslashes(): the message can carry a raw NUL, DEL, or a
		// multi-byte UTF-8 sequence, none of which addslashes() round-trips
		// correctly through a single-quoted PHP literal (addslashes() turns a
		// raw NUL into the two-character text "\0", which single-quoted PHP
		// does not turn back into a NUL byte). base64 carries any byte
		// sequence unchanged, so the child decodes exactly what this process
		// encoded.
		$msgB64     = base64_encode( $message );
		$defineLine = $debug ? "define('WPMGR_DEBUG', true);" : '// debug intentionally OFF';

		$script = <<<PHP
<?php
declare(strict_types=1);

{$defineLine}
{$prelude}

ini_set('log_errors', '1');
ini_set('error_log', '{$logEscaped}');

require '{$bootstrap}';

\$command = new class implements \\WPMgr\\Agent\\Commands\\CommandInterface {
    public function name(): string
    {
        return 'boom';
    }

    public function effect(): \\WPMgr\\Agent\\Commands\\CommandEffect
    {
        return \\WPMgr\\Agent\\Commands\\CommandEffect::Read;
    }

    public function repeatability(): \\WPMgr\\Agent\\Commands\\CommandRepeatability
    {
        return \\WPMgr\\Agent\\Commands\\CommandRepeatability::Idempotent;
    }

    /** @param array<string,mixed> \$claims @param array<string,mixed> \$params @return array<string,mixed> */
    public function execute(array \$claims, array \$params): array
    {
        throw new \\RuntimeException(base64_decode('{$msgB64}'));
    }
};

\$rc        = new \\ReflectionClass(\\WPMgr\\Agent\\Connector::class);
\$connector = \$rc->newInstanceWithoutConstructor();
\$router    = new \\WPMgr\\Agent\\Router(\$connector, [\$command]);

\$request = new \\WP_REST_Request(
    [
        'command'      => 'boom',
        'wpmgr_claims' => ['sub' => 'site-uuid', 'cmd' => 'boom'],
    ]
);

{$before}

\$response = \$router->handleCommand(\$request);

if (!(\$response instanceof \\WP_Error)) {
    fwrite(STDERR, "expected WP_Error\\n");
    exit(2);
}

echo json_encode(
    [
        'code'    => \$response->get_error_code(),
        'message' => \$response->get_error_message(),
        'data'    => \$response->get_error_data(),
    ]
);
PHP;

		file_put_contents( $scriptPath, $script );

		$descriptors = [
			1 => [ 'pipe', 'w' ],
			2 => [ 'pipe', 'w' ],
		];

		$process = proc_open(
			[ PHP_BINARY, '-d', 'memory_limit=1G', $scriptPath ],
			$descriptors,
			$pipes
		);

		$this->assertIsResource( $process, 'could not start the subprocess' );

		$stdout = (string) stream_get_contents( $pipes[1] );
		$stderr = (string) stream_get_contents( $pipes[2] );
		fclose( $pipes[1] );
		fclose( $pipes[2] );
		$status = proc_close( $process );

		return [
			'status' => $status,
			'stdout' => $stdout,
			'stderr' => $stderr,
			'log'    => is_file( $logPath ) ? (string) file_get_contents( $logPath ) : '',
		];
	}

	/**
	 * #764: the authorize-failure log line shares escapeControlChars() with
	 * the command-failure line, so it must escape too. Connector's own
	 * verifyCommand() only ever throws from a small closed set of fixed
	 * category messages (see includes/class-connector.php) — never one built
	 * from request content — so there is no real request that puts a control
	 * character into the exception message on this path. What DOES reach this
	 * exact log line un-sanitized in this test, deliberately, is the $command
	 * argument: Router::authorizeCommand() is exercised directly here rather
	 * than through the real REST dispatch, so it is not protected by the
	 * {command} route's [a-z0-9_.]+ pattern or WordPress's sanitize_callback
	 * pass the way a real request is (see registerRoutes()). That makes this
	 * a defense-in-depth proof of the same kind already used elsewhere in
	 * Router (see the manage_options check in authorizeCommand()): the log
	 * line escapes what it is given, regardless of whether another layer
	 * would also have stopped it.
	 */
	public function test_authorize_failure_log_line_escapes_control_characters(): void
	{
		$run = $this->runAuthorizeInSubprocess( "boom\x1Bcmd" );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$lines = array_values(
			array_filter(
				preg_split( '/\R/', $run['log'] ) ?: [],
				static function ( string $line ): bool {
					return trim( $line ) !== '';
				}
			)
		);

		$this->assertCount( 1, $lines, 'expected exactly one log line, got ' . count( $lines ) . ":\n" . $run['log'] );
		$this->assertStringContainsString( 'WPMgr Agent: command authorize failed:', $lines[0] );
		$this->assertStringContainsString( 'command=boom\x1Bcmd', $lines[0] );
		$this->assertStringContainsString( 'reason=WPMgr Agent: malformed token.', $lines[0] );
	}

	/**
	 * #764: escapeControlChars() fails closed at the authorizeCommand() call
	 * site too, independently of the dispatch()/command-failure call site
	 * proved above by test_debug_log_withholds_the_whole_line_when_escaping_cannot_complete().
	 * That test only drives handleCommand() -> dispatch(); it says nothing
	 * about authorizeCommand()'s own "?? self::LOG_LINE_WITHHELD", which
	 * guards a separate call site with a separate mutation surface. A mutant
	 * that made escapeControlChars() return its input on a PCRE engine
	 * failure, or that dropped authorizeCommand()'s own "??
	 * self::LOG_LINE_WITHHELD", would let a raw control byte reach the log
	 * from this call site untouched while the dispatch-side test above stays
	 * green.
	 *
	 * Forced the same way: pcre.jit off and a backtrack budget of 1, so
	 * preg_replace_callback() inside escapeControlChars() exhausts its
	 * budget and returns null. The command carries the same single ESC byte
	 * test_authorize_failure_log_line_escapes_control_characters() uses, so
	 * this is the direct failure-mode counterpart of that test.
	 *
	 * The withheld marker must replace the WHOLE line: the command name and
	 * the category are lost along with it, not just the reason, because
	 * authorizeCommand() escapes the whole assembled line in one call.
	 */
	public function test_authorize_failure_log_line_withholds_the_whole_line_when_escaping_cannot_complete(): void
	{
		$run = $this->runAuthorizeInSubprocess(
			"boom\x1Bcmd",
			true,
			"ini_set('pcre.jit', '0'); ini_set('pcre.backtrack_limit', '1');"
		);

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );

		$lines = array_values(
			array_filter(
				preg_split( '/\R/', $run['log'] ) ?: [],
				static function ( string $line ): bool {
					return trim( $line ) !== '';
				}
			)
		);

		$this->assertCount( 1, $lines, 'expected exactly one log line, got ' . count( $lines ) . ":\n" . $run['log'] );
		$this->assertStringContainsString(
			'WPMgr Agent: log line withheld (control-character escaping could not complete).',
			$lines[0]
		);
		$this->assertStringNotContainsString( "\x1B", $run['log'], 'a raw control byte reached the log' );
		$this->assertStringNotContainsString( 'boom', $lines[0], 'the withheld marker must replace the whole line' );
		$this->assertStringNotContainsString(
			'command authorize failed',
			$lines[0],
			'the withheld marker must replace the whole line, not just the reason'
		);
	}

	/**
	 * #764: the authorize call site must not even build, let alone escape,
	 * the log line when debug logging is off. A counter on escapeControlChars()
	 * proves the negative directly: a mutant that removed
	 * authorizeCommand()'s DebugLog::isEnabled() gate would still write
	 * nothing to error_log() (nothing is listening), so asserting an empty
	 * log alone cannot catch it — it must be caught here, by the call count.
	 */
	public function test_authorize_gate_skips_escaping_when_debug_is_disabled(): void
	{
		$run = $this->runAuthorizeInSubprocess( 'boom', false );

		$this->assertSame( 0, $run['status'], 'subprocess failed: ' . $run['stdout'] . $run['stderr'] );
		$this->assertSame( '', trim( $run['log'] ), 'debug log must stay empty when debugging is off' );
		$this->assertStringContainsString( 'escapeControlChars_calls=0', $run['stdout'] );
	}

	/**
	 * Drive Router::authorizeCommand() in a child PHP process with a bearer
	 * token malformed enough (one segment, no dots) that Connector::verify()
	 * throws before touching the keystore, so the real authorizeCommand()
	 * catch block — and its DebugLog::write() call — runs without any of the
	 * signing/enrolment setup the happy path needs.
	 *
	 * Instruments escapeControlChars() with a Patchwork counter, restored in
	 * finally inside the subprocess script — the subprocess exits right after,
	 * so nothing leaks across tests, but restoring anyway keeps the fake
	 * self-contained rather than relying on process exit to undo it. The
	 * count is reported on stdout as "escapeControlChars_calls=N" so a test
	 * that never enables debug logging (and so never gets a log line to
	 * inspect) still has something to assert on.
	 *
	 * @param string $command Command name passed to authorizeCommand(); not
	 *                        run through the real route's sanitize_callback
	 *                        here, see the calling test's docblock.
	 * @param bool   $debug   Whether to define WPMGR_DEBUG.
	 * @param string $before  PHP run just before authorizeCommand() is called.
	 * @return array{status:int,stdout:string,stderr:string,log:string}
	 */
	private function runAuthorizeInSubprocess( string $command, bool $debug = true, string $before = '' ): array
	{
		$logPath    = sys_get_temp_dir() . '/wpmgr_router_authz_log_' . uniqid( '', true ) . '.log';
		$scriptPath = sys_get_temp_dir() . '/wpmgr_router_authz_log_' . uniqid( '', true ) . '.php';

		$this->temp[] = $logPath;
		$this->temp[] = $scriptPath;

		$bootstrap  = addslashes( __DIR__ . '/bootstrap.php' );
		$logEscaped = addslashes( $logPath );
		$cmdB64     = base64_encode( $command );
		$defineLine = $debug ? "define('WPMGR_DEBUG', true);" : '// debug intentionally OFF';

		$script = <<<PHP
<?php
declare(strict_types=1);

{$defineLine}

ini_set('log_errors', '1');
ini_set('error_log', '{$logEscaped}');

require '{$bootstrap}';

\$escapeCalls = 0;
\$escapeHandle = \\Patchwork\\redefine(
    'WPMgr\\Agent\\Router::escapeControlChars',
    function (string \$line) use (&\$escapeCalls) {
        \$escapeCalls++;
        return \\Patchwork\\relay();
    }
);

\$rc        = new \\ReflectionClass(\\WPMgr\\Agent\\Connector::class);
\$connector = \$rc->newInstanceWithoutConstructor();
\$router    = new \\WPMgr\\Agent\\Router(\$connector, []);

\$command = base64_decode('{$cmdB64}');

\$request = new \\WP_REST_Request(['command' => \$command]);
\$request->set_header('authorization', 'Bearer x');

{$before}

try {
    \$result = \$router->authorizeCommand(\$request, \$command);
} finally {
    \\Patchwork\\restore(\$escapeHandle);
}

if (!(\$result instanceof \\WP_Error)) {
    fwrite(STDERR, "expected WP_Error\\n");
    exit(2);
}

echo "escapeControlChars_calls={\$escapeCalls}\\n";
echo 'ok';
PHP;

		file_put_contents( $scriptPath, $script );

		$descriptors = [
			1 => [ 'pipe', 'w' ],
			2 => [ 'pipe', 'w' ],
		];

		$process = proc_open(
			[ PHP_BINARY, '-d', 'memory_limit=1G', $scriptPath ],
			$descriptors,
			$pipes
		);

		$this->assertIsResource( $process, 'could not start the subprocess' );

		$stdout = (string) stream_get_contents( $pipes[1] );
		$stderr = (string) stream_get_contents( $pipes[2] );
		fclose( $pipes[1] );
		fclose( $pipes[2] );
		$status = proc_close( $process );

		return [
			'status' => $status,
			'stdout' => $stdout,
			'stderr' => $stderr,
			'log'    => is_file( $logPath ) ? (string) file_get_contents( $logPath ) : '',
		];
	}
}
