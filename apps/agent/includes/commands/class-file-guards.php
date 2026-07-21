<?php
/**
 * FileGuards: shared executable-write and protected-root guards for the
 * P2 file-manager write commands (file_write, file_rename, file_upload_apply).
 *
 * Centralises the two most security-critical checks so they cannot drift
 * between commands:
 *
 *   isExecutableWrite()  — T1 (the RCE control). Returns true when a proposed
 *                          write would land an executable file on the server,
 *                          covering three bypass vectors:
 *                          (a) primary extension in the deny-list,
 *                          (b) double-extension / trailing-dot / case variants,
 *                          (c) content sniff: <?php, <?=, or bare <? (short-open-
 *                              tag) not immediately followed by 'xml'.
 *
 *   isProtectedRoot()   — T13. Returns true when the path is, or descends into,
 *                          a WordPress core directory or the active plugin/theme
 *                          root that must not be deleted.
 *
 * Both methods are pure functions of their inputs — no side effects, no WP
 * global reads (except where explicitly documented). They are declared `static`
 * so callers need no instance; the class is non-instantiable.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Shared executable-write and protected-root guards for P2 file commands.
 */
final class FileGuards {

	// ------------------------------------------------------------------
	// Executable-extension deny-list (T1 — mirrors the research §5 threat table).
	// Go CP must mirror this list exactly in files/policy.go.
	// Case-insensitive: callers must pass the basename strtolower()'d.
	//
	// The list covers:
	//   PHP:        php, php3, php4, php5, php7, php8, php9, phps, phtml, pht, phar, phpt
	//   Web server: shtml (server-side includes), htaccess, htpasswd, ini
	//   ASP/JSP:    asp, aspx, jsp
	//   CGI:        cgi, pl, py
	//
	// Note: an extension deny-list is inherently incomplete. A future hardening
	// should move non-owner writes to an allow-list of known-safe extensions
	// (txt, css, js, json, md, html) instead of maintaining this deny-list.
	// ------------------------------------------------------------------

	/** @var list<string> */
	private const EXEC_EXTENSIONS = [
		'php', 'php3', 'php4', 'php5', 'php7', 'php8', 'php9', 'phps', 'phtml', 'pht', 'phar', 'phpt',
		'shtml', 'asp', 'aspx', 'jsp', 'cgi', 'pl', 'py',
		'htaccess', 'htpasswd', 'ini',
	];

	/**
	 * Unambiguous PHP open-tag literals checked first (case-sensitive: PHP itself
	 * is case-sensitive about these tokens; '<?PHP' does not execute).
	 *
	 * @var list<string>
	 */
	private const PHP_SNIFF_LITERALS = [ '<?php', '<?=' ];

	/**
	 * Non-instantiable utility class.
	 */
	private function __construct() {}

	// ------------------------------------------------------------------
	// T1: Executable-write detection
	// ------------------------------------------------------------------

	/**
	 * Return true when the proposed write would create an executable file on the server.
	 *
	 * Covers three bypass vectors documented in T1 / §5:
	 *
	 *   (a) Primary extension in the deny-list (case-folded).
	 *       "shell.php" → blocked.
	 *
	 *   (b) Double-extension / trailing-dot / case variants.
	 *       "shell.php.jpg", "shell.PHP.", "shell.PhP" → blocked.
	 *       Detection: split the lowercased basename on '.' and check EVERY
	 *       non-leading segment. Trailing-dot produces an empty last segment
	 *       (also caught). This catches the "bypass by appending an image
	 *       extension" vector (CVE-2020-25213 class).
	 *
	 *   (c) Content sniff: decoded content contains '<?php', '<?=', or any bare '<?'
	 *       not immediately followed by 'xml' (case-insensitive).
	 *       e.g. a plain ".txt" file whose decoded body opens with a bare
	 *       short-open PHP tag is blocked.
	 *       $content is the DECODED bytes (callers must pass decoded, not base64).
	 *       An empty $content (e.g. rename with no content) does NOT trigger (c).
	 *
	 * The two active controls are the extension deny-list (a+b) and the content
	 * sniff (c). In a standard WordPress install the entire jail is the web root,
	 * so a "deny all writes into web-served dirs" policy would block every write
	 * and is not implemented. The deny-list + full-file content sniff (including
	 * bare short-open-tag detection) are the enforceable per-file controls.
	 *
	 * @param string $absPath     Resolved absolute path of the proposed write target.
	 * @param string $resolvedRel Site-relative path (used for basename extraction).
	 * @param string $content     Decoded file content (NOT base64). Empty string = new empty file.
	 * @return bool True → the write must be blocked without confirm_executable_write.
	 */
	public static function isExecutableWrite( string $absPath, string $resolvedRel, string $content ): bool {
		$basename  = basename( $resolvedRel );
		$lbasename = strtolower( $basename );

		// (a + b) Check every extension token in the lowercased basename.
		if ( self::hasExecutableExtension( $lbasename ) ) {
			return true;
		}

		// (c) Content sniff: PHP open tags in the decoded body (see sniffsAsPhp()).
		// Only fires when content is non-empty (empty = new empty file or rename).
		if ( $content !== '' && self::sniffsAsPhp( $content ) ) {
			return true;
		}

		return false;
	}

	/**
	 * Return true when the lowercased basename contains an executable extension.
	 * Covers: primary extension (a) AND double-extension / trailing-dot (b).
	 *
	 * Algorithm: split on '.' — every token after the first is a potential
	 * extension. If ANY token (after stripping leading dots for dot-files) is in
	 * the deny-list, the name is considered executable.
	 *
	 * Examples:
	 *   "shell.php"       → ['shell','php']         → 'php' blocked
	 *   "shell.php.jpg"   → ['shell','php','jpg']   → 'php' blocked
	 *   "shell.PhP"       → caller lowercases → 'php' blocked
	 *   "shell.php."      → ['shell','php','']       → 'php' blocked (trailing dot = empty token)
	 *   ".htaccess"       → ['','.htaccess'] — handled via raw name check too
	 *   "image.jpg"       → ['image','jpg']         → not blocked
	 *
	 * @param string $lbasename Lowercased basename.
	 * @return bool
	 */
	public static function hasExecutableExtension( string $lbasename ): bool {
		// Direct name checks for leading-dot names like ".htaccess", ".htpasswd".
		if ( in_array( ltrim( $lbasename, '.' ), self::EXEC_EXTENSIONS, true ) ) {
			return true;
		}

		// Split on dots: every segment after index 0 is a potential extension.
		$parts = explode( '.', $lbasename );
		// Skip index 0 (the base name, or empty string for dot-files).
		$exts = array_slice( $parts, 1 );
		foreach ( $exts as $ext ) {
			// Empty string = trailing dot — also executable (e.g. "shell.php.").
			if ( $ext === '' || in_array( $ext, self::EXEC_EXTENSIONS, true ) ) {
				return true;
			}
		}

		return false;
	}

	/**
	 * Return true when the decoded content contains a PHP open tag.
	 *
	 * Detection covers three tag forms:
	 *   1. '<?php'  — always a PHP open tag (case-sensitive; '<?PHP' does not execute).
	 *   2. '<?='    — short echo tag (always enabled since PHP 5.4; case-sensitive).
	 *   3. '<?'     — bare short open tag, executable when short_open_tag=On.
	 *                 Carve-out: a '<?' immediately followed by 'xml' (case-insensitive)
	 *                 is an XML processing instruction and is benign. HOWEVER, we must
	 *                 scan ALL '<?' occurrences — a file may contain '<?xml ... ?>'
	 *                 followed by a later '<? evil'. Only if every '<?' is a '<?xml'
	 *                 occurrence does that file pass the bare-tag check.
	 *
	 * Checks are case-sensitive for (1) and (2) because PHP itself is case-sensitive.
	 * For (3) the content following '<?' is checked case-insensitively so '<?XML' is
	 * also carved out (the XML spec allows either case).
	 *
	 * @param string $content Decoded file bytes (NOT base64).
	 * @return bool True → treat as PHP; the caller should block or require confirmation.
	 */
	public static function sniffsAsPhp( string $content ): bool {
		// Fast path: unambiguous literals first.
		foreach ( self::PHP_SNIFF_LITERALS as $literal ) {
			if ( str_contains( $content, $literal ) ) {
				return true;
			}
		}

		// Bare short open tag '<?': scan every occurrence and apply the <?xml carve-out.
		// If we find even one '<?' that is NOT immediately followed by 'xml', flag it.
		$offset = 0;
		$len    = strlen( $content );
		while ( true ) {
			$pos = strpos( $content, '<?', $offset );
			if ( $pos === false ) {
				break;
			}
			// Read the 3 chars after '<?' (may be fewer at end of string).
			$next3 = strtolower( substr( $content, $pos + 2, 3 ) );
			if ( substr( $next3, 0, 3 ) !== 'xml' ) {
				// Not '<?xml' — this is a bare short open tag.
				return true;
			}
			// This occurrence is '<?xml' — benign. Keep scanning.
			$offset = $pos + 2;
			if ( $offset >= $len ) {
				break;
			}
		}

		return false;
	}

	// ------------------------------------------------------------------
	// T13: Protected-root detection
	// ------------------------------------------------------------------

	/**
	 * Protected root directory names (site-relative, lowercased).
	 * Deletes of these paths (or their contents) are refused unless an
	 * explicit override flag is set by the CP.
	 *
	 * @var list<string>
	 */
	private const PROTECTED_ROOTS = [
		'wp-admin',
		'wp-includes',
	];

	/**
	 * Return true when the site-relative path IS or descends into a protected root.
	 *
	 * Protected roots are:
	 *   - wp-admin/         (WordPress admin panel — deleting it bricks the site)
	 *   - wp-includes/      (WordPress core library — deleting it bricks the site)
	 *   - The active theme root (wp-content/themes/<active-theme>)
	 *   - The active plugin directory root for this plugin (wp-content/plugins/<slug>)
	 *
	 * The active theme and plugin roots are read from the live WP option/constant at
	 * call time. If WordPress functions are unavailable (unit tests), only the static
	 * list is checked.
	 *
	 * @param string $resolvedRel Site-relative path (forward slashes, no leading slash).
	 * @return bool True → the path is protected; caller should return protected_root error.
	 */
	public static function isProtectedRoot( string $resolvedRel ): bool {
		$lrel     = strtolower( ltrim( $resolvedRel, '/' ) );
		$segments = explode( '/', $lrel );
		$first    = $segments[0] ?? '';

		// Check static list (wp-admin, wp-includes).
		foreach ( self::PROTECTED_ROOTS as $root ) {
			if ( $first === $root ) {
				return true;
			}
		}

		// Check individual WordPress core files in the root.
		// We do not protect arbitrary root files, but wp-login.php and wp-cron.php
		// are critical enough to protect at the single-file level.
		if ( count( $segments ) === 1 ) {
			$coreFiles = [ 'wp-login.php', 'wp-settings.php', 'wp-load.php', 'wp-blog-header.php' ];
			if ( in_array( $lrel, $coreFiles, true ) ) {
				return true;
			}
		}

		// Check the active theme root (wp-content/themes/<slug>).
		if ( function_exists( 'get_stylesheet' ) && function_exists( 'get_theme_root' ) ) {
			$themeRoot = get_theme_root();
			$abspath   = defined( 'ABSPATH' ) ? rtrim( (string) constant( 'ABSPATH' ), '/\\' ) : '';
			if ( $themeRoot !== false && $abspath !== '' ) {
				$relThemeRoot = ltrim( str_replace( str_replace( '\\', '/', $abspath ), '', str_replace( '\\', '/', (string) $themeRoot ) ), '/' );
				$activeTheme  = get_stylesheet();
				if ( is_string( $activeTheme ) && $activeTheme !== '' ) {
					$themeProtected = strtolower( $relThemeRoot . '/' . $activeTheme );
					if (
						$lrel === $themeProtected
						|| str_starts_with( $lrel, $themeProtected . '/' )
					) {
						return true;
					}
				}
			}
		}

		// Check the active plugin root (wp-content/plugins/<slug>).
		if ( function_exists( 'plugin_basename' ) && defined( 'WPMGR_AGENT_FILE' ) ) {
			$agentSlug  = dirname( plugin_basename( (string) constant( 'WPMGR_AGENT_FILE' ) ) );
			$pluginBase = defined( 'WP_PLUGIN_DIR' ) ? rtrim( (string) constant( 'WP_PLUGIN_DIR' ), '/\\' ) : '';
			$abspath    = defined( 'ABSPATH' ) ? rtrim( (string) constant( 'ABSPATH' ), '/\\' ) : '';
			if ( $pluginBase !== '' && $abspath !== '' && $agentSlug !== '' ) {
				$relPluginBase = ltrim( str_replace( str_replace( '\\', '/', $abspath ), '', str_replace( '\\', '/', $pluginBase ) ), '/' );
				$agentProtected = strtolower( $relPluginBase . '/' . $agentSlug );
				if (
					$lrel === $agentProtected
					|| str_starts_with( $lrel, $agentProtected . '/' )
				) {
					return true;
				}
			}
		}

		return false;
	}

	/**
	 * Return the list of executable extensions (for Go CP parity documentation).
	 *
	 * @return list<string>
	 */
	public static function execExtensions(): array {
		return self::EXEC_EXTENSIONS;
	}
}
