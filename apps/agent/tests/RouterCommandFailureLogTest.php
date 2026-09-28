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
	 * Drive Router::handleCommand() in a child PHP process.
	 *
	 * @param bool   $debug   Whether to define WPMGR_DEBUG.
	 * @param string $message Exception message the command throws.
	 * @param string $before  PHP run just before the command is dispatched.
	 * @return array{status:int,stdout:string,stderr:string,log:string}
	 */
	private function runInSubprocess( bool $debug, string $message, string $before = '' ): array
	{
		$logPath    = sys_get_temp_dir() . '/wpmgr_router_log_' . uniqid( '', true ) . '.log';
		$scriptPath = sys_get_temp_dir() . '/wpmgr_router_log_' . uniqid( '', true ) . '.php';

		$this->temp[] = $logPath;
		$this->temp[] = $scriptPath;

		$bootstrap  = addslashes( __DIR__ . '/bootstrap.php' );
		$logEscaped = addslashes( $logPath );
		$msgEscaped = addslashes( $message );
		$defineLine = $debug ? "define('WPMGR_DEBUG', true);" : '// debug intentionally OFF';

		$script = <<<PHP
<?php
declare(strict_types=1);

{$defineLine}

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
        throw new \\RuntimeException('{$msgEscaped}');
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
}
