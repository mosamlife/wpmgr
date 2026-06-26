<?php
// File: class-file-search-command.php — FileSearchCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileSearchCommand: recursive filename or content search within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_search
 *   Authorization: Bearer <Ed25519 JWT cmd="file_search">
 *   Body: {
 *     "path":   <site-relative path to search within ('' = jail root)>,
 *     "query":  <literal substring — NOT a regex or glob>,
 *     "mode":   "name" | "content",
 *     "cursor": <opaque continuation cursor | null>
 *   }
 *
 * Response (200 OK):
 *   {
 *     "matches": [
 *       {
 *         "path":    <site-relative path>,
 *         "name":    <filename>,
 *         "size":    <int>,
 *         "mtime":   <int — Unix timestamp>,
 *         "is_dir":  <bool>,
 *         "line":    <int|null — line number in content mode>,
 *         "snippet": <string|null — matched line excerpt in content mode>
 *       }, ...
 *     ],
 *     "truncated": <bool>,
 *     "cursor":    <string|null — continuation cursor when truncated>
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable.
 *
 * SAFETY PROPERTIES:
 *   - `query` is used as a LITERAL substring (case-insensitive stripos), not a
 *     regex or glob. No ReDoS is possible.
 *   - `path` is run through FileListCommand::jailPath() (realpath + strncmp).
 *   - Content mode: reads only the first CONTENT_SNIFF_BYTES of each file.
 *     Skips files whose first bytes identify them as binary (NUL in first 8 KB).
 *     Skips sensitive files (FileReadCommand::isSensitive) and never returns their
 *     contents.
 *   - Hard caps: MAX_DIRS_VISITED, MAX_FILES_VISITED, MAX_MATCHES, MAX_DEPTH.
 *     When a cap is hit, truncated=true and cursor is set to resume.
 *   - Time budget: walks up to TIME_BUDGET_SECONDS per call; stops and sets
 *     truncated=true when the budget is exhausted.
 *   - No regex, no backtracks, no unbounded memory allocation.
 *
 * CURSOR SCHEME:
 *   The cursor is a base64-encoded JSON array of site-relative directory paths
 *   representing the DFS stack at the point of truncation: the last element is
 *   the directory being listed when the cap was hit. On resume, the walker
 *   skips ahead within that directory and then continues the DFS from the same
 *   stack. The cursor is path-bound (also encodes the root path) so a cursor
 *   from a different path is rejected.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileSearchCommand implements CommandInterface {

	/** Maximum directories visited per call (prevents runaway on huge trees). */
	public const MAX_DIRS_VISITED = 2000;

	/** Maximum files examined per call. */
	public const MAX_FILES_VISITED = 20000;

	/** Maximum matches returned (across pages). */
	public const MAX_MATCHES = 200;

	/** Maximum directory depth below the search root. */
	public const MAX_DEPTH = 30;

	/** Time budget in seconds per call. */
	public const TIME_BUDGET_SECONDS = 10;

	/** Bytes read per file in content mode (first N bytes only). */
	public const CONTENT_SNIFF_BYTES = 65536; // 64 KiB

	/** Binary-detection window: if NUL byte appears in first N bytes, treat as binary. */
	private const BINARY_DETECT_BYTES = 8192;

	/** Maximum snippet length returned for content matches. */
	private const MAX_SNIPPET_LENGTH = 200;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_search';
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params Decoded JSON body from the CP.
	 * @return array<string,mixed>
	 */
	public function execute( array $claims, array $params ): array {
		// ------------------------------------------------------------------
		// 1. Extract and validate parameters.
		// ------------------------------------------------------------------
		$relPath = isset( $params['path'] ) && is_string( $params['path'] )
			? str_replace( '\\', '/', $params['path'] )
			: '';

		if ( ! isset( $params['query'] ) || ! is_string( $params['query'] ) || $params['query'] === '' ) {
			return $this->error( 'invalid_path', 'query is required and must be a non-empty string' );
		}

		$query = $params['query'];

		$mode = 'name';
		if ( isset( $params['mode'] ) && is_string( $params['mode'] ) ) {
			if ( ! in_array( $params['mode'], [ 'name', 'content' ], true ) ) {
				return $this->error( 'invalid_path', 'mode must be "name" or "content"' );
			}
			$mode = $params['mode'];
		}

		$cursorIn = isset( $params['cursor'] ) && is_string( $params['cursor'] ) && $params['cursor'] !== ''
			? $params['cursor']
			: null;

		// ------------------------------------------------------------------
		// 2. Resolve jail root.
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'not_readable', 'file jail root could not be resolved' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the search root path.
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( (string) $jailResult['code'], (string) $jailResult['message'] );
		}

		$absSearchRoot = (string) $jailResult['abs'];
		$resolvedRel   = (string) $jailResult['rel'];

		if ( ! file_exists( $absSearchRoot ) ) {
			return $this->error( 'not_found', 'search path not found: ' . $resolvedRel );
		}

		if ( ! is_dir( $absSearchRoot ) ) {
			return $this->error( 'not_readable', 'search path is not a directory: ' . $resolvedRel );
		}

		if ( ! is_readable( $absSearchRoot ) ) {
			return $this->error( 'not_readable', 'search path is not readable: ' . $resolvedRel );
		}

		// ------------------------------------------------------------------
		// 4. Decode cursor.
		// ------------------------------------------------------------------
		$cursor = $this->decodeCursor( $cursorIn, $resolvedRel );
		if ( $cursorIn !== null && $cursor === null ) {
			return $this->error( 'invalid_path', 'cursor is invalid or from a different search path' );
		}

		// ------------------------------------------------------------------
		// 5. Walk the directory tree and collect matches.
		// ------------------------------------------------------------------
		$state = [
			'jailRoot'      => $jailRoot,
			'absSearchRoot' => $absSearchRoot,
			'resolvedRel'   => $resolvedRel,
			'query'         => $query,
			'mode'          => $mode,
			'dirsVisited'   => 0,
			'filesVisited'  => 0,
			'matches'       => [],
			'truncated'     => false,
			'deadline'      => microtime( true ) + self::TIME_BUDGET_SECONDS,
			'cursor'        => null,
		];

		$this->walkDir( $state, $absSearchRoot, $resolvedRel, 0, $cursor );

		// ------------------------------------------------------------------
		// 6. Build and return response.
		// ------------------------------------------------------------------
		$response = [
			'matches'   => $state['matches'],
			'truncated' => $state['truncated'],
		];

		if ( $state['truncated'] && $state['cursor'] !== null ) {
			$response['cursor'] = $this->encodeCursor( $resolvedRel, $state['cursor'] );
		}

		return $response;
	}

	// ------------------------------------------------------------------
	// DFS walker
	// ------------------------------------------------------------------

	/**
	 * Recursive DFS directory walker.
	 * Mutates $state in place: appends matches, increments counters, sets truncated.
	 *
	 * @param array<string,mixed> &$state  Shared mutable state.
	 * @param string               $absDir Absolute path of the directory to scan.
	 * @param string               $relDir Site-relative path of this directory.
	 * @param int                  $depth  Current recursion depth below search root.
	 * @param array<string,mixed>|null $cursor Cursor state: {skip_before: string, stack: list<string>} or null.
	 * @return void
	 */
	private function walkDir( array &$state, string $absDir, string $relDir, int $depth, ?array $cursor ): void {
		// Depth cap.
		if ( $depth > self::MAX_DEPTH ) {
			$state['truncated'] = true;
			$state['cursor']    = [ 'dir' => $relDir, 'skip_before' => '' ];
			return;
		}

		// Time budget.
		if ( microtime( true ) >= $state['deadline'] ) {
			$state['truncated'] = true;
			$state['cursor']    = [ 'dir' => $relDir, 'skip_before' => '' ];
			return;
		}

		// Dirs-visited cap.
		if ( $state['dirsVisited'] >= self::MAX_DIRS_VISITED ) {
			$state['truncated'] = true;
			$state['cursor']    = [ 'dir' => $relDir, 'skip_before' => '' ];
			return;
		}

		++$state['dirsVisited'];

		$handle = @opendir( $absDir );
		if ( $handle === false ) {
			return; // Skip unreadable dirs silently.
		}

		// Read all entries, sort for deterministic cursor resumption.
		$entries = [];
		while ( true ) {
			$entry = readdir( $handle );
			if ( $entry === false ) {
				break;
			}
			if ( $entry === '.' || $entry === '..' ) {
				continue;
			}
			$entries[] = $entry;
		}
		closedir( $handle );

		sort( $entries );

		// Cursor: if we have a cursor pointing at this dir, skip entries before skip_before.
		$skipBefore = '';
		if ( $cursor !== null && isset( $cursor['dir'] ) && $cursor['dir'] === $relDir && isset( $cursor['skip_before'] ) ) {
			$skipBefore = (string) $cursor['skip_before'];
			$cursor     = null; // Cursor consumed at this level.
		}

		foreach ( $entries as $name ) {
			// Apply cursor skip.
			if ( $skipBefore !== '' && strcmp( $name, $skipBefore ) < 0 ) {
				continue;
			}

			// Time / matches cap check.
			if ( microtime( true ) >= $state['deadline']
				|| count( $state['matches'] ) >= self::MAX_MATCHES
				|| $state['filesVisited'] >= self::MAX_FILES_VISITED
			) {
				$state['truncated'] = true;
				$state['cursor']    = [ 'dir' => $relDir, 'skip_before' => $name ];
				break;
			}

			$absEntry = $absDir . '/' . $name;
			$relEntry = $relDir === '' ? $name : $relDir . '/' . $name;

			// Never follow symlinks.
			if ( is_link( $absEntry ) ) {
				continue;
			}

			$lstat = @lstat( $absEntry );
			if ( $lstat === false ) {
				continue;
			}

			$isDir = is_dir( $absEntry );
			$size  = $isDir ? 0 : (int) ( $lstat['size'] ?? 0 );
			$mtime = (int) ( $lstat['mtime'] ?? 0 );

			if ( $state['mode'] === 'name' ) {
				// Name mode: case-insensitive literal substring match on filename.
				if ( stripos( $name, $state['query'] ) !== false ) {
					$state['matches'][] = [
						'path'    => $relEntry,
						'name'    => $name,
						'size'    => $size,
						'mtime'   => $mtime,
						'is_dir'  => $isDir,
						'line'    => null,
						'snippet' => null,
					];
				}
			} elseif ( ! $isDir ) {
				// Content mode: search file contents (skip dirs, binaries, sensitive files).
				++$state['filesVisited'];
				$basename = basename( $relEntry );

				// Skip sensitive files — never return their contents in search results.
				if ( FileReadCommand::isSensitive( $relEntry, $basename ) ) {
					continue;
				}

				// Read up to CONTENT_SNIFF_BYTES.
				if ( ! is_readable( $absEntry ) ) {
					continue;
				}

				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming file read for content search; WP_Filesystem has no streaming API; headless agent never initialises WP_Filesystem
				$fh = @fopen( $absEntry, 'rb' );
				if ( $fh === false ) {
					continue;
				}

				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- reading file content for search; WP_Filesystem has no streaming API; headless agent never initialises WP_Filesystem
				$raw = fread( $fh, self::CONTENT_SNIFF_BYTES );
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after content read
				fclose( $fh );

				if ( $raw === false || $raw === '' ) {
					continue;
				}

				// Binary detection: any NUL byte in the first BINARY_DETECT_BYTES → skip.
				if ( strpos( substr( $raw, 0, self::BINARY_DETECT_BYTES ), "\0" ) !== false ) {
					continue;
				}

				// Search line by line for the query (literal, case-insensitive).
				$lines  = explode( "\n", $raw );
				$lowerQ = strtolower( $state['query'] );
				foreach ( $lines as $lineNo => $lineContent ) {
					if ( stripos( $lineContent, $state['query'] ) !== false ) {
						$snippet = substr( trim( $lineContent ), 0, self::MAX_SNIPPET_LENGTH );
						$state['matches'][] = [
							'path'    => $relEntry,
							'name'    => $name,
							'size'    => $size,
							'mtime'   => $mtime,
							'is_dir'  => false,
							'line'    => $lineNo + 1,
							'snippet' => $snippet,
						];

						// One match per file is enough to surface the file; cap here too.
						if ( count( $state['matches'] ) >= self::MAX_MATCHES ) {
							break;
						}
					}
				}
			}

			// Recurse into directories after processing the entry.
			if ( $isDir && ! $state['truncated'] ) {
				$this->walkDir( $state, $absEntry, $relEntry, $depth + 1, $cursor );
				if ( $state['truncated'] ) {
					break;
				}
			}
		}
	}

	// ------------------------------------------------------------------
	// Cursor helpers
	// ------------------------------------------------------------------

	/**
	 * Encode a resumption cursor as opaque base64-encoded JSON.
	 *
	 * @param string              $rootRel The search-root site-relative path.
	 * @param array<string,mixed> $cursorState Internal cursor state.
	 * @return string
	 */
	private function encodeCursor( string $rootRel, array $cursorState ): string {
		$payload = (string) wp_json_encode( [ 'root' => $rootRel, 'c' => $cursorState ] );
		return base64_encode( $payload );
	}

	/**
	 * Decode and validate a cursor. Returns the internal cursor state on success,
	 * or null if the cursor is malformed or from a different search root.
	 *
	 * @param string|null $cursor    Cursor string from the caller.
	 * @param string      $rootRel   Current search-root site-relative path.
	 * @return array<string,mixed>|null
	 */
	private function decodeCursor( ?string $cursor, string $rootRel ): ?array {
		if ( $cursor === null ) {
			return null;
		}

		$raw = @base64_decode( $cursor, true );
		if ( $raw === false || $raw === '' ) {
			return null;
		}

		$data = @json_decode( $raw, true );
		if ( ! is_array( $data ) ) {
			return null;
		}

		// Cursor must be bound to the same search root.
		if ( ! isset( $data['root'] ) || $data['root'] !== $rootRel ) {
			return null;
		}

		if ( ! isset( $data['c'] ) || ! is_array( $data['c'] ) ) {
			return null;
		}

		return $data['c'];
	}

	// ------------------------------------------------------------------
	// Response helpers
	// ------------------------------------------------------------------

	/**
	 * @param string $code    Error code.
	 * @param string $message Human-readable message (no absolute host paths).
	 * @return array{error:array{code:string,message:string}}
	 */
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
