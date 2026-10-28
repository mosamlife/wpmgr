<?php
/**
 * FilesArchiver: streaming wp-content packer that emits rotated
 * `<component>.partNNN.zip` archive parts, using a streaming per-component
 * archive engine.
 *
 * Track 5 / 0.9.6 — PER-COMPONENT split. Previously this class emitted a single
 * rotating sequence `wp-content.partNNN.zip` lumping the entire wp-content tree.
 * Now each well-known wp-content subtree gets its own rotating sequence so
 * operators can restore plugins / themes / uploads independently:
 *
 *   plugins/        -> plugins.part001.zip, plugins.part002.zip, ...
 *   themes/         -> themes.part001.zip, ...
 *   uploads/        -> uploads.part001.zip, uploads.part002.zip, ...
 *   wp-content/*    -> wp-content.part001.zip, ...  (EVERYTHING ELSE — mu-plugins,
 *                                                    languages, drop-ins, custom dirs)
 *
 * The component buckets are mutually exclusive: an entry under `plugins/` is
 * NEVER also in `wp-content.partNNN.zip`. The "wp-content" bucket is defined
 * as "all wp-content entries that don't fall into a more specific bucket".
 *
 * Each emitted part archive's manifest entry carries an `entry_kind` matching
 * the component name ('plugin' | 'theme' | 'upload' | 'wp-content'). The
 * restore side routes per-component and stages + swaps each subdirectory
 * independently when only some components are selected.
 *
 * M5.6 / ADR-033 — pure-PHP wp-content archiver. Replaces the phpbu tar
 * source from ADR-032: that source shells out to `tar`, which is missing on
 * a large slice of managed WP hosting (Kinsta, some Pantheon, WP Engine
 * restricted shells, Windows). This class runs everywhere PHP runs.
 *
 * WHY ZipArchive:
 *   - Built into PHP via ext-zip — no `tar` binary dependency, no
 *     `proc_open` permission.
 *   - The C extension streams source bytes through deflate/store without
 *     loading the file body into PHP memory (the only path that buffers in
 *     PHP is `addFromString`, which we never use for file contents).
 *   - This is the same choice made by leading backup plugins for the same
 *     audience.
 *
 * OOM defense (the two memory cliffs this approach must guard against):
 *   1. Path discovery streams the relative-path list to an on-disk cache
 *      file (one path per line). A site with 500k uploads would otherwise
 *      pin >100 MB just holding the file list as a PHP array.
 *   2. Per-file ingest is `ZipArchive::addFile($abs, $relative)` — the C
 *      extension reads the source file as it writes the entry; PHP-side
 *      memory stays flat regardless of any single file's size.
 *
 * Resume / watchdog recovery:
 *   - On a fresh run we create `$outDir/paths.cache` and walk the source dir
 *     into it, then start packing. The cache has every entry tagged with the
 *     component bucket it belongs to (one tagged path per line).
 *   - The caller's checkpoint callback receives a durable cursor after
 *     discovery and after every closed part. A worker the host ends resumes
 *     from that cursor: closed parts are kept, every other part file is
 *     deleted first, and each component continues at the first line its
 *     open part held. Worst case loss is one open part per component.
 *
 * Part rotation (per component, independently):
 *   - Same triggers as before: `estimated_part_bytes >= max_part_bytes`
 *     (default 200 MiB) OR `entry_count >= max_part_entries`
 *     (default 55,000).
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Support\ArrayShape;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Pure-PHP streaming wp-content archiver with on-disk path cache and
 * checkpointed resume. See file docblock for the rationale.
 */
final class FilesArchiver
{
    /** @var array<string,bool> Paths omitted from this archive. */
    private array $skippedSources = [];

    /** @var array<string,array{size:int,mtime:int}> Skipped existing sources retained from the parent. */
    private array $carriedSources = [];

    /** @var array<string,array<string,bool>> Per component: skips inside its open part, not yet settled. */
    private array $pendingSkips = [];

    private string $filesListPath = '';

    /** @var \Closure():\ZipArchive Creates each new or reopened archive handle. */
    private \Closure $zipFactory;

    /** Default part size before rotation (bytes). */
    public const DEFAULT_MAX_PART_BYTES = 200 * 1024 * 1024;

    /** Hard cap on entries per part — defends against many-small-files cases. */
    public const DEFAULT_MAX_PART_ENTRIES = 55_000;

    /**
     * Default segment exclude list (matched against `/`-separated segments of
     * the path relative to the source dir). These are WPMgr scratch names and
     * secret/state filenames that must be skipped wherever they appear.
     *
     * @var list<string>
     */
    public const DEFAULT_EXCLUDES = [
        'wpmgr-snapshots',
        'wpmgr-agent',
        // Object-cache credentials file: contains the plaintext Redis password;
        // must never be packed into a backup archive.
        'wpmgr-object-cache-config.php',
        // FD-6: Object-cache reconnect cool-down state file (timestamps/counters,
        // no secrets). Excluded so a restored backup does not replay a stale
        // cool-down window that could suppress Redis on a healthy site.
        '.wpmgr-oc-state.json',
        // Update locks and in-flight update markers: transient agent state
        // that a restore must never bring back.
        'wpmgr-update-inflight',
        'wpmgr-update-watchdog',
        // File-extraction temp root.
        'wpmgr-file-extract-tmp',
    ];

    /**
     * Segment prefixes reserved for the agent's own transient files, matched
     * at the start of any path segment: restore staging and pre-restore trees
     * (`.wpmgr-staging-*`, `.wpmgr-old-files-*`), atomic-write temp files
     * (`.wpmgr-tmp-*`, `.wpmgr-ul-*`), state files and their temp copies, and
     * the temp copy of the object-cache credentials file.
     *
     * @var list<string>
     */
    public const AGENT_RESERVED_PREFIXES = [
        '.wpmgr-',
        'wpmgr-object-cache-config.php.tmp.',
    ];

    /**
     * Default path exclude list (matched as anchored path prefixes relative to
     * the source dir). These cover wp-content runtime cache and update staging
     * roots without excluding same-named directories inside plugins or themes.
     * Nested plugin/upload cache stays included intentionally; preserving source
     * code is safer than excluding secondary cache, so keep these anchored.
     *
     * @var list<string>
     */
    public const DEFAULT_PATH_EXCLUDES = [
        'cache',
        'uploads/cache',
        'upgrade',
        'upgrade-temp-backup',
    ];

    /**
     * Component partition definitions, in match-order. The first matching
     * `root` prefix claims the entry; anything that matches no listed root
     * falls into the `wp-content` (others) bucket.
     *
     * Each component emits its own rotating part archive sequence with a
     * filename prefix matching the array key. The `entry_kind` on the
     * resulting manifest entry also matches the array key.
     *
     * Note on `wp-content`: it is the catch-all bucket — its root is the
     * empty string, which never matches as a prefix; anything that fell
     * through the more specific roots ends up here.
     *
     * Note on the `wpmgr-agent` self-exclude: the DEFAULT_EXCLUDES segment
     * filter ALSO drops anything containing a `wpmgr-agent` segment, so the
     * plugin's own tree never makes it into the plugins bucket. This is
     * defense in depth because excludes are checked before component
     * classification.
     *
     * Track 5 ordering rationale: `plugin`/`theme`/`upload` first, exact
     * prefix match. Operator intent is "restore my plugins" — they want a
     * narrow, deterministic bucket; the catch-all only sweeps everything
     * else.
     *
     * @var array<string,array{root:string,kind:string}>
     */
    private const COMPONENT_PARTITIONS = [
        'plugins'    => ['root' => 'plugins/',    'kind' => 'plugin'],
        'themes'     => ['root' => 'themes/',     'kind' => 'theme'],
        'uploads'    => ['root' => 'uploads/',    'kind' => 'upload'],
        'wp-content' => ['root' => '',            'kind' => 'wp-content'],
    ];

    /**
     * Maps the canonical CP singular entry_kind vocabulary (plugin | theme |
     * upload | wp-content) to the internal COMPONENT_PARTITIONS bucket key.
     * This is the normalization layer for the CP->agent contract: the CP sends
     * singular kinds everywhere (backup_contract.go Components, OpenAPI enum);
     * the internal archiver is keyed by the plural directory prefix that each
     * bucket archives.
     *
     * 'db' and 'core' are NOT file-archiver buckets and must NOT appear here;
     * they are handled by separate code paths (DbDumper and CoreFilesArchiver).
     *
     * @var array<string,string>  key = CP singular kind, value = COMPONENT_PARTITIONS key.
     */
    private const KIND_TO_BUCKET = [
        'plugin'     => 'plugins',
        'theme'      => 'themes',
        'upload'     => 'uploads',
        'wp-content' => 'wp-content',
    ];

    /** Marks a resume cursor as a durable checkpoint of this format. */
    public const CHECKPOINT_VERSION = 2;

    /** A closed part name: component, generation and part number. */
    private const CLOSED_PART_NAME = '/^(plugins|themes|uploads|wp-content)\.g(\d{3,})\.part(\d{3,})\.zip$/D';

    /** Any part file of the file components, or its libzip temporary output. */
    private const STALE_PART_NAME = '/^(?:plugins|themes|uploads|wp-content)\.(?:g\d+\.)?part\d+\.zip(?:\.[A-Za-z0-9]{1,32})?$/D';

    /** Emit a progress callback every N packed files. */
    private const PROGRESS_EVERY_FILES = 50;

    /**
     * GH #279: default wall-clock heartbeat interval, in seconds. Emitted in
     * ADDITION to the N-files trigger above so a stretch of few-but-large
     * files (each `ZipArchive::addFile()` call slow, but the file count never
     * reaching PROGRESS_EVERY_FILES) still refreshes the CP's
     * progress_updated_at before the watchdog's soft-stall threshold
     * (default 180s). Also gates a heartbeat emitted immediately before each
     * `closeActivePart()` call, since `ZipArchive::close()` on a large part
     * is a single blocking finalize with no progress hook of its own.
     * Overridable via the `heartbeat_interval_seconds` constructor option
     * (tests use a tiny value to assert the wall-clock gate without sleeping
     * for real).
     */
    public const DEFAULT_HEARTBEAT_INTERVAL_SECONDS = 30.0;

    /** Name of the on-disk path-discovery cache (created in $outDir). */
    private const PATHS_CACHE_NAME = 'paths.cache';

    /**
     * Name of the file holding the random id of the current discovery. A
     * fresh pass writes a new id before it changes anything else in $outDir,
     * and a checkpoint is accepted only while its id is the one on disk, so
     * a checkpoint can never be applied to another pass's file list.
     */
    public const DISCOVERY_ID_NAME = 'discovery.id';

    /**
     * Name of the per-snapshot packed-files manifest emitted alongside every
     * archive run (full and incremental alike). Format: one line per packed
     * file — `<relpath>\t<size>\t<mtime>\n`.
     *
     * This is the ADR-051 "files.list" artifact: the agent emits it for free
     * during buildPathCache (SplFileInfo already has size/mtime), then uploads
     * it as a normal manifest entry so the next increment can diff against it.
     */
    public const FILES_LIST_NAME = 'files.list';

    /**
     * Name of the tombstones list emitted by an incremental run when files
     * that existed in the parent snapshot have been deleted from disk.
     * Format: one `<relpath>\n` per deleted path.
     */
    public const TOMBSTONES_LIST_NAME = 'tombstones.list';

    /** Absolute path of the source root (typically WP_CONTENT_DIR). */
    private string $sourceDir;

    /**
     * Merged segment exclude list (segment defaults + caller-provided).
     * Matched against `/`-separated segments of the path relative to
     * $sourceDir.
     *
     * @var list<string>
     */
    private array $excludes;

    /**
     * Normalized default path excludes matched as anchored path prefixes
     * relative to $sourceDir.
     *
     * @var list<string>
     */
    private array $pathExcludes;

    /** Effective per-part size cap (bytes). */
    private int $maxPartBytes;

    /** Effective per-part entry-count cap. */
    private int $maxPartEntries;

    /**
     * File extensions to exclude (without leading dot, lower-cased). Empty = no
     * extension filter. A3/A4 exclusion (#187).
     *
     * @var list<string>
     */
    private array $excludeExtensions;

    /**
     * Skip files whose size exceeds this value (bytes). 0 = no size filter.
     * A5 exclusion (#187).
     */
    private int $excludeFileSizeBytes;

    /**
     * A1 (#187): selective component backup. Set of COMPONENT_PARTITIONS keys
     * that are EXCLUDED from this archive run. Empty = include all components.
     * When the operator picks e.g. ["plugins","themes"], uploads and wp-content
     * bucket entries are skipped in buildPathCache so they never enter the
     * paths.cache and produce zero part files.
     *
     * @var array<string,true>  key = component name (e.g. 'uploads'), value = true.
     */
    private array $excludeComponents;

    /**
     * Snapshot generation this archive run belongs to. Part filenames are
     * namespaced by it (`<component>.gNNN.partMMM.zip`) so the parts of two
     * different generations in the same chain never share a name on the
     * restore overlay (the agent restore stages every part into a flat
     * `<scratch>/<logical_path>` keyed by logical name, so a collision would
     * silently overwrite an earlier generation's carry-forward part). 0 for a
     * base (gen-0) full backup; >0 for each increment.
     */
    private int $generation;

    /**
     * GH #279: wall-clock heartbeat interval (seconds). See
     * DEFAULT_HEARTBEAT_INTERVAL_SECONDS for the rationale.
     */
    private float $heartbeatIntervalSeconds;

    /**
     * Wall-clock timestamp (microtime(true)) of the most recent progress
     * emit. Reset at the start of every archive() call and refreshed by
     * emitProgress() on every call (whether file-count-triggered,
     * rotation-triggered, or heartbeat-triggered) so the heartbeat gate never
     * double-fires right after a real emit.
     */
    private float $lastProgressEmitAt = 0.0;

    /**
     * @param string              $sourceDir Absolute path of the root to back up
     *                                       (typically WP_CONTENT_DIR).
     * @param list<string>        $excludes  Path-segment names to skip; matched
     *                                       against the RELATIVE path under
     *                                       $sourceDir. Merged additively with
     *                                       self::DEFAULT_EXCLUDES.
     * @param array<string,mixed> $opts      Optional overrides:
     *                                         max_part_bytes (int),
     *                                         max_part_entries (int),
     *                                         zip_factory (\Closure():\ZipArchive),
     *                                         exclude_extensions (list<string>),
     *                                         exclude_file_size_mb (int),
     *                                         include_components (list<string> of
     *                                           COMPONENT_PARTITIONS keys to INCLUDE;
     *                                           absent/empty = include all),
     *                                         heartbeat_interval_seconds (float,
     *                                           GH #279 wall-clock progress
     *                                           heartbeat gate; defaults to
     *                                           DEFAULT_HEARTBEAT_INTERVAL_SECONDS).
     * @param int                 $generation Snapshot generation (0 = base full,
     *                                        >0 = increment). Namespaces the part
     *                                        filenames so overlay restore never
     *                                        collides part names across generations.
     * @throws \RuntimeException If ext-zip is unavailable or $sourceDir is
     *                           not a readable directory.
     */
    public function __construct(string $sourceDir, array $excludes = [], array $opts = [], int $generation = 0)
    {
        $this->zipFactory = $opts['zip_factory'] ?? static fn (): \ZipArchive => new \ZipArchive();
        $this->generation = max(0, $generation);
        if (!class_exists(\ZipArchive::class)) {
            // V0 ships no PclZip fallback. Most managed WP hosts ship ext-zip;
            // we'll add a fallback if surveys show hosts without it. Failing
            // loudly here is preferable to silently producing a non-zip.
            throw new \RuntimeException('FilesArchiver requires ext-zip');
        }

        $real = realpath($sourceDir);
        if ($real === false || !is_dir($real)) {
            throw new \RuntimeException('FilesArchiver: sourceDir is not a readable directory: ' . esc_html($sourceDir));
        }
        $this->sourceDir = rtrim($real, DIRECTORY_SEPARATOR);

        // Normalize default runtime/staging roots as anchored path prefixes.
        // Caller-provided exclude_paths intentionally stay segment-based below.
        $pathExcludes = [];
        foreach (self::DEFAULT_PATH_EXCLUDES as $entry) {
            $entry = str_replace('\\', '/', (string) $entry);
            $entry = trim($entry, "/ \t\n\r\0\x0B");
            if ($entry === '') {
                continue;
            }
            $pathExcludes[$entry] = true;
        }
        $this->pathExcludes = array_map('strval', array_keys($pathExcludes));

        // Merge defaults + caller excludes, dedupe, drop empties. Segment
        // names only — slashes in entries would never match an exploded
        // single segment so we strip them.
        $merged = [];
        foreach (array_merge(self::DEFAULT_EXCLUDES, $excludes) as $entry) {
            $entry = trim((string) $entry, "/ \t\n\r\0\x0B");
            if ($entry === '') {
                continue;
            }
            $merged[$entry] = true;
        }
        $this->excludes = array_map('strval', array_keys($merged));

        $this->maxPartBytes = isset($opts['max_part_bytes'])
            ? max(1, (int) $opts['max_part_bytes'])
            : self::DEFAULT_MAX_PART_BYTES;
        $this->maxPartEntries = isset($opts['max_part_entries'])
            ? max(1, (int) $opts['max_part_entries'])
            : self::DEFAULT_MAX_PART_ENTRIES;

        // GH #279: wall-clock heartbeat gate. 0 is a valid override (heartbeat
        // on every loop tick) for tests; never negative.
        $this->heartbeatIntervalSeconds = isset($opts['heartbeat_interval_seconds']) && is_numeric($opts['heartbeat_interval_seconds'])
            ? max(0.0, (float) $opts['heartbeat_interval_seconds'])
            : self::DEFAULT_HEARTBEAT_INTERVAL_SECONDS;

        // A4 (#187): exclude by file extension. Normalise to lower-case, no dot.
        $rawExts = isset($opts['exclude_extensions']) && is_array($opts['exclude_extensions'])
            ? $opts['exclude_extensions'] : [];
        $normExts = [];
        foreach ($rawExts as $ext) {
            $ext = strtolower(ltrim(trim((string) $ext), '.'));
            if ($ext !== '') {
                $normExts[$ext] = true;
            }
        }
        $this->excludeExtensions = array_map('strval', array_keys($normExts));

        // A5 (#187): exclude by file size. 0 means disabled.
        $excludeMb = isset($opts['exclude_file_size_mb']) && is_numeric($opts['exclude_file_size_mb'])
            ? max(0, (int) $opts['exclude_file_size_mb']) : 0;
        $this->excludeFileSizeBytes = $excludeMb > 0 ? $excludeMb * 1024 * 1024 : 0;

        // A1 (#187): selective component backup. Compute the EXCLUDE set as
        // (all known components) minus (requested include_components). When
        // include_components is absent or empty we include all components
        // (excludeComponents stays empty).
        //
        // Normalization: the CP sends singular entry_kind values (plugin, theme,
        // upload, wp-content) matching the canonical manifest vocabulary. We
        // accept BOTH the singular CP kinds AND the internal plural bucket keys
        // so that callers at any layer (BackupCommand, TaskRunner, test fixtures)
        // can use either form. KIND_TO_BUCKET maps singular -> bucket key; if the
        // value is already a bucket key it passes through as-is.
        // Whether the caller provided an explicit include_components filter.
        // A present key (even an empty array) means "filter is active"; an absent
        // key means "no filter — archive all components" (legacy / full-backup path).
        $filterActive = array_key_exists('include_components', $opts) && is_array($opts['include_components']);
        $rawInclude   = $filterActive
            ? array_values(array_filter($opts['include_components'], 'is_string'))
            : [];
        $include = [];
        foreach ($rawInclude as $entry) {
            if (isset(self::KIND_TO_BUCKET[$entry])) {
                // Singular CP kind (e.g. 'plugin') -> translate to bucket key ('plugins').
                $include[] = self::KIND_TO_BUCKET[$entry];
            } elseif (isset(self::COMPONENT_PARTITIONS[$entry])) {
                // Already a bucket key (e.g. 'plugins') -> pass through unchanged.
                $include[] = $entry;
            }
            // 'db' and 'core' are not file-archiver buckets; they are silently
            // ignored here (handled by TaskRunner/DbDumper/CoreFilesArchiver).
        }
        $this->excludeComponents = [];
        if ($filterActive) {
            // Filter is active: exclude every bucket not in the resolved include set.
            // When $include is empty (e.g. components=["core"] or components=["db"]
            // — neither maps to a file-archiver bucket) ALL buckets are excluded so
            // FilesArchiver produces zero parts from wp-content.  This is the correct
            // behavior: core is archived by CoreFilesArchiver; db by DbDumper.
            // Contrast with $filterActive===false (key absent): that is the full-backup
            // / legacy-no-filter path where all components are included.
            $includeSet = array_flip($include);
            foreach (array_keys(self::COMPONENT_PARTITIONS) as $compName) {
                if (!isset($includeSet[$compName])) {
                    $this->excludeComponents[$compName] = true;
                }
            }
        }
    }

    /**
     * Parse a files.list flat file into a prev-map suitable for incremental
     * change detection. The files.list format is `<relpath>\t<size>\t<mtime>\n`
     * (one line per file, as emitted by buildPathCache). Lines that don't
     * parse (e.g. truncated) are silently skipped.
     *
     * This is the ADR-051 "load prev once into PHP map" step. The caller
     * supplies the path to the parent snapshot's files.list (fetched via
     * PrevFilesListChunks presigned chunks and assembled to a local scratch
     * file by TaskRunner before calling archive()). Memory cost ≈ a few MB
     * for 100 k files; never persisted to sub_state.
     *
     * @param string $filesListPath Absolute path to the files.list scratch file.
     * @return array<string,array{size:int,mtime:int}> Map keyed by relpath.
     */
    public static function loadPrevMap(string $filesListPath): array
    {
        $map = [];
        if (!is_file($filesListPath)) {
            return $map;
        }
        $handle = @fopen($filesListPath, 'rb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
        if ($handle === false) {
            return $map;
        }
        while (($line = fgets($handle)) !== false) {
            $line = rtrim($line, "\r\n");
            if ($line === '') {
                continue;
            }
            $parts = explode("\t", $line, 3);
            if (count($parts) !== 3) {
                continue;
            }
            [$rel, $size, $mtime] = $parts;
            if ($rel === '') {
                continue;
            }
            $map[$rel] = ['size' => (int) $size, 'mtime' => (int) $mtime];
        }
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
        return $map;
    }

    /**
     * Walk the source dir and pack into per-component rotated part files
     * inside $outDir. Resumable: a durable checkpoint saved by an earlier
     * call (see $checkpoint) continues after that call's last closed part.
     *
     * Returns a flat list of part filenames (one rotating sequence per
     * component) AND a parallel list of per-part component kinds so the
     * caller can tag the manifest entries.
     *
     * ADR-051 incremental mode: when $prevMap is non-null, only files that
     * are NEW or CHANGED (by mtime/size) relative to the previous snapshot
     * are packed. The files.list sidecar is always written (full picture of
     * all packed files). Tombstones are returned in result['tombstones'].
     *
     * Durable progress (GH #954): after discovery, after every closed part
     * and once every part is closed, $checkpoint receives the cursor a later
     * call needs to continue: the closed parts with their kinds and sizes,
     * each component's next part number and the first paths.cache line it
     * still needs, the discovery counts, and the skipped sources whose lines
     * are settled. A call resumed from that cursor first deletes every part
     * file (and libzip temporary output) the cursor does not list, so the
     * scratch dir never holds the parts of two passes, and it archives again
     * the lines of any part that was open when the earlier call ended. A
     * cursor that fails validation, or none at all, starts one fresh pass
     * with no part of an earlier pass left on disk.
     *
     * @param string                                          $outDir   Absolute scratch dir for the part
     *                                                                  archives (created if missing).
     * @param array<string,mixed>                             $resume   Empty for a fresh run; else the
     *                                                                  last cursor $checkpoint received.
     * @param callable                                        $progress function(string $phase, array $detail): void.
     *                                                                  $phase is always 'archiving_files'.
     *                                                                  Per-tick $detail keys: files_done,
     *                                                                  files_total, parts_done,
     *                                                                  bytes_written, current_file. On
     *                                                                  completion: done=true, parts,
     *                                                                  part_kinds, files_total,
     *                                                                  bytes_written.
     * @param array<string,array{size:int,mtime:int}>|null   $prevMap  Previous-snapshot file map for
     *                                                                  incremental change detection (ADR-051).
     *                                                                  null = full-backup mode.
     * @param callable|null                                   $checkpoint function(array $cursor): void, called
     *                                                                  with the durable resume cursor. An
     *                                                                  exception it throws ends the call.
     * @return array<string,mixed> On completion: `done: true` + parts list +
     *                              parallel `part_kinds` list + optional `tombstones` list.
     * @throws \RuntimeException On unrecoverable error.
     */
    public function archive(string $outDir, array $resume, callable $progress, ?array $prevMap = null, ?callable $checkpoint = null): array
    {
        $this->skippedSources = [];
        $this->carriedSources = [];
        $this->pendingSkips   = [];
        // Lift caller-imposed time/abort guards. Watchdog handles
        // stall recovery; we want this loop to run as long as the SAPI
        // will let it.
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        @ignore_user_abort(true);

        if (!is_dir($outDir) && !wp_mkdir_p($outDir) && !is_dir($outDir)) {
            throw new \RuntimeException('FilesArchiver: cannot create outDir: ' . esc_html($outDir));
        }
        $realOut = realpath($outDir);
        $outDir  = $realOut === false ? '' : rtrim($realOut, DIRECTORY_SEPARATOR);
        if ($outDir === '' || !is_dir($outDir)) {
            // Never fall back to an empty base: every path below is built on it.
            throw new \RuntimeException('FilesArchiver: cannot resolve outDir');
        }

        // ----- Phase 0: the path-discovery cache. -----
        // The cache file format is one line per file: "<component>\t<relpath>".
        // The tab keeps the relpath bytes intact (no escaping needed; we never
        // pack literal tabs in WP filenames in practice, and a stray tab in a
        // user-uploaded filename would still parse — only the FIRST tab is the
        // delimiter). The cache and its sidecars always live in $outDir.
        $cachePath           = $outDir . DIRECTORY_SEPARATOR . self::PATHS_CACHE_NAME;
        $this->filesListPath = $outDir . DIRECTORY_SEPARATOR . self::FILES_LIST_NAME;
        $incremental         = $prevMap !== null;
        // ADR-051 instrumentation: prevMap size (entries parsed from the parent's
        // files.list). prevMap===null => full mode; [] => base/empty signal.
        $prevMapSize = is_array($prevMap) ? count($prevMap) : 0;

        $saved = $this->durableCheckpoint($resume, $outDir, $cachePath, $incremental);
        if ($saved !== null) {
            // Keep the closed parts the checkpoint lists; delete every other
            // part file before any work, then continue after those parts.
            $this->removeStaleParts($outDir, $saved['parts_completed']);
            $run            = $saved['run'];
            $partsCompleted = $saved['parts_completed'];
            $partKinds      = $saved['part_kinds'];
            $partSizes      = $saved['part_sizes'];
            $currentParts   = array_column($saved['components'], 'current_part', 'name');
            $resumeFrom     = array_column($saved['components'], 'resume_from', 'name');
            $complete       = $saved['complete'];
            foreach ($saved['skipped'] as $rel) {
                $this->skippedSources[$rel] = true;
            }
        } else {
            // One fresh pass. Its new discovery id comes first, so no earlier
            // checkpoint validates again even if this pass ends before its own
            // first save; then no part of an abandoned pass may remain.
            $discoveryId = $this->renewDiscoveryId($outDir);
            $this->removeStaleParts($outDir, []);
            $totalFiles = isset($resume['total_files']) ? (int) $resume['total_files'] : 0;
            // ADR-051 instrumentation: carry-forward count (unchanged files skipped from the archive).
            $filesCarried    = isset($resume['files_carried']) ? (int) $resume['files_carried'] : 0;
            // Tombstone tracking: on-disk path + count (never an in-memory array).
            $tombstonesFile  = $this->ownTombstonesFile($resume['tombstones_file'] ?? '', $outDir);
            $tombstonesCount = isset($resume['tombstones_count']) ? (int) $resume['tombstones_count'] : 0;
            // A cursor without a checkpoint marker (hand-built) may name a
            // cached discovery; a checkpoint that failed validation never does.
            if (array_key_exists('checkpoint', $resume) || !is_file($cachePath) || $totalFiles <= 0) {
                // Fresh discovery walk. Truncate any stale cache.
                $cacheResult     = $this->buildPathCache($cachePath, $prevMap);
                $totalFiles      = $cacheResult['count'];
                $filesCarried    = isset($cacheResult['carried']) ? (int) $cacheResult['carried'] : 0;
                $tombstonesFile  = $cacheResult['tombstones_file'];
                $tombstonesCount = $cacheResult['tombstones_count'];
            }
            clearstatcache(true, $cachePath);
            $run = [
                'discovery_id'     => $discoveryId,
                'total_files'      => $totalFiles,
                'cache_bytes'      => (int) @filesize($cachePath),
                'files_carried'    => $filesCarried,
                'tombstones_file'  => $tombstonesFile,
                'tombstones_count' => $tombstonesCount,
                'incremental'      => $incremental,
            ];
            $partsCompleted = [];
            $partKinds      = [];
            $partSizes      = [];
            $currentParts   = array_fill_keys(array_keys(self::COMPONENT_PARTITIONS), 1);
            $resumeFrom     = array_fill_keys(array_keys(self::COMPONENT_PARTITIONS), 0);
            $complete       = false;
        }
        $totalFiles      = (int) $run['total_files'];
        $filesCarried    = (int) $run['files_carried'];
        $tombstonesFile  = (string) $run['tombstones_file'];
        $tombstonesCount = (int) $run['tombstones_count'];
        $bytesWritten    = (int) array_sum($partSizes);

        // Per-component current-part trackers. Each component gets its OWN
        // rotating sequence: `<component>.gNNN.partMMM.zip`. The trackers carry
        // the next part number (1-indexed), the open ZipArchive handle, and
        // `pending_from`: the first paths.cache line held only by the open
        // part, which is where a resume must start again for this component.
        //
        // We open lazily: the first time a component sees an entry, we open
        // its next part. A component with zero matching entries emits zero
        // part files (e.g. a site with no `mu-plugins`/`languages` ends up
        // with no `wp-content.partNNN.zip`).
        $compState = [];
        foreach (self::COMPONENT_PARTITIONS as $compName => $cfg) {
            $compState[$compName] = [
                'component'            => $compName,
                'current_part'         => (int) $currentParts[$compName],
                'part_entries'         => 0,
                'part_estimated_bytes' => 0,
                'zip'                  => null,
                'part_path'            => '',
                'kind'                 => $cfg['kind'],
                'pending_from'         => null,
                'queued_sources'       => [],
            ];
        }

        // The first line any component still needs; every line before a
        // component's own resume_from is already in its closed parts or settled.
        $fileIndex = (int) min($resumeFrom);
        if ($saved === null && $checkpoint !== null) {
            // A worker ended before its first closed part keeps this discovery.
            $checkpoint($this->checkpointCursor($run, $compState, $fileIndex, $partsCompleted, $partKinds, $partSizes, false));
        }

        // ----- Phase 1: pack. -----
        $currentRel = '';
        if (!$complete) {
            $cacheHandle = @fopen($cachePath, 'rb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
            if ($cacheHandle === false) {
                throw new \RuntimeException('FilesArchiver: cannot reopen path cache: ' . esc_html($cachePath));
            }

            // Seek to the requested line by counting newlines.
            $seeked = 0;
            while ($seeked < $fileIndex && fgets($cacheHandle) !== false) {
                $seeked++;
            }
            $fileIndex = $seeked;

            $filesSinceProgress = 0;

            // GH #279: start the wall-clock heartbeat window at the top of the
            // pack loop, not at zero, so a resumed run doesn't immediately fire a
            // heartbeat before it's had a chance to make any progress.
            $this->lastProgressEmitAt = microtime(true);

            while (($line = fgets($cacheHandle)) !== false) {
                $lineIndex = $fileIndex;
                $line      = rtrim($line, "\r\n");
                if ($line === '') {
                    $fileIndex++;
                    continue;
                }
                // Split "<component>\t<relpath>". Defensive: a malformed line
                // without a tab is treated as a wp-content (catch-all) entry.
                $tabPos = strpos($line, "\t");
                if ($tabPos === false) {
                    $compName = 'wp-content';
                    $rel      = $line;
                } else {
                    $compName = substr($line, 0, $tabPos);
                    $rel      = substr($line, $tabPos + 1);
                }
                if (!isset($compState[$compName])) {
                    $compName = 'wp-content';
                }
                if ($rel === '' || $lineIndex < $resumeFrom[$compName]) {
                    // Already in this component's closed parts, or settled.
                    $fileIndex++;
                    continue;
                }
                $currentRel = $rel;
                $abs        = $this->sourceDir . DIRECTORY_SEPARATOR . $rel;

                $metadata = $this->sourceMetadata($rel);
                if ($metadata !== null) {
                    // Lazy-open the active part for this component.
                    if ($compState[$compName]['zip'] === null) {
                        $this->openActivePart($compState[$compName], $compName, $outDir, $partsCompleted);
                    }
                    /** @var \ZipArchive $zip */
                    $zip = $compState[$compName]['zip'];
                    if ($zip->addFile($abs, $rel)) {
                        $compState[$compName]['queued_sources'][$rel] = $metadata;
                        $compState[$compName]['part_entries'] = $zip->numFiles;
                        $compState[$compName]['part_estimated_bytes'] += $metadata['size'];
                        if ($compState[$compName]['pending_from'] === null) {
                            $compState[$compName]['pending_from'] = $lineIndex;
                        }
                    } else {
                        $this->skipSource($compState[$compName], $rel);
                    }
                } else {
                    $this->skipSource($compState[$compName], $rel);
                }
                $fileIndex++;
                $filesSinceProgress++;

                // Per-component rotation triggers.
                if ($compState[$compName]['zip'] !== null) {
                    $needRotate = $compState[$compName]['part_entries'] >= $this->maxPartEntries
                        || $compState[$compName]['part_estimated_bytes'] >= $this->maxPartBytes;
                    if ($needRotate) {
                        // GH #279: ZipArchive::close() on a large part is a single
                        // blocking finalize with no progress hook of its own; emit
                        // a heartbeat immediately before it so a slow finalize
                        // doesn't leave CP progress_updated_at stale.
                        $this->emitProgress($progress, [
                            'files_done'    => $fileIndex,
                            'files_total'   => $totalFiles,
                            'parts_done'    => count($partsCompleted),
                            'bytes_written' => $bytesWritten,
                            'current_file'  => $currentRel,
                            'component'     => $compName,
                            'stage'         => 'finalizing_part',
                        ]);
                        $closed = $this->closeActivePart($compState[$compName], $progress);
                        if ($closed['size'] > 0) {
                            $bytesWritten     += $closed['size'];
                            $partsCompleted[] = $closed['name'];
                            $partKinds[]      = $compState[$compName]['kind'];
                            $partSizes[]      = $closed['size'];
                        }
                        $this->settlePart($compState[$compName]);
                        $this->emitProgress($progress, [
                            'files_done'    => $fileIndex,
                            'files_total'   => $totalFiles,
                            'parts_done'    => count($partsCompleted),
                            'bytes_written' => $bytesWritten,
                            'current_file'  => $currentRel,
                            'component'     => $compName,
                        ]);
                        gc_collect_cycles();

                        $compState[$compName]['current_part']++;
                        $compState[$compName]['part_entries']         = 0;
                        $compState[$compName]['part_estimated_bytes'] = 0;
                        if ($checkpoint !== null) {
                            $checkpoint($this->checkpointCursor($run, $compState, $fileIndex, $partsCompleted, $partKinds, $partSizes, false));
                        }
                    }
                }

                if ($filesSinceProgress >= self::PROGRESS_EVERY_FILES) {
                    $this->emitProgress($progress, [
                        'files_done'    => $fileIndex,
                        'files_total'   => $totalFiles,
                        'parts_done'    => count($partsCompleted),
                        // The active part's bytes aren't visible on disk until
                        // close(); report only the durable total here.
                        'bytes_written' => $bytesWritten,
                        'current_file'  => $currentRel,
                    ]);
                    $filesSinceProgress = 0;
                }

                // GH #279: wall-clock heartbeat, IN ADDITION to the file-count
                // trigger above. Guards the case where files are few but large;
                // each addFile() call can take a while, so PROGRESS_EVERY_FILES
                // may never be reached even though real wall-clock time is
                // passing. No-ops (via the elapsed-time check) whenever a real
                // emit -- rotation or file-count -- just fired this same tick.
                $this->maybeEmitHeartbeat($progress, [
                    'files_done'    => $fileIndex,
                    'files_total'   => $totalFiles,
                    'parts_done'    => count($partsCompleted),
                    'bytes_written' => $bytesWritten,
                    'current_file'  => $currentRel,
                ]);
            }

            fclose($cacheHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
        }

        // Close every still-open per-component active part.
        foreach (array_keys($compState) as $compName) {
            if ($compState[$compName]['zip'] === null) {
                continue;
            }
            // GH #279: same rationale as the rotation-triggered close above.
            // This is the tail-of-run finalize (no more file-loop iterations
            // left to trip PROGRESS_EVERY_FILES), so it's the one place a
            // large final part could close with zero heartbeat otherwise.
            $this->emitProgress($progress, [
                'files_done'    => $fileIndex,
                'files_total'   => $totalFiles,
                'parts_done'    => count($partsCompleted),
                'bytes_written' => $bytesWritten,
                'component'     => $compName,
                'stage'         => 'finalizing_part',
            ]);
            $closed = $this->closeActivePart($compState[$compName], $progress);
            if ($closed['size'] > 0) {
                $bytesWritten     += $closed['size'];
                $partsCompleted[] = $closed['name'];
                $partKinds[]      = $compState[$compName]['kind'];
                $partSizes[]      = $closed['size'];
            }
            $this->settlePart($compState[$compName]);
            $compState[$compName]['current_part']++;
            if ($checkpoint !== null) {
                $checkpoint($this->checkpointCursor($run, $compState, $fileIndex, $partsCompleted, $partKinds, $partSizes, false));
            }
        }
        if ($checkpoint !== null) {
            // Every line is archived or settled: a resume from here only
            // finishes the inventories below, which are safe to repeat.
            $checkpoint($this->checkpointCursor($run, $compState, $fileIndex, $partsCompleted, $partKinds, $partSizes, true));
        }

        if ($this->skippedSources !== []) {
            if ($prevMap !== null) {
                foreach (array_intersect_key($prevMap, $this->skippedSources) as $rel => $metadata) {
                    $absolute = $this->sourceDir . DIRECTORY_SEPARATOR . $rel;
                    clearstatcache(true, $absolute);
                    if (file_exists($absolute) || is_link($absolute)) { $this->carriedSources[$rel] = $metadata; }
                }
                $filesCarried += count($this->carriedSources);
                if ($this->carriedSources !== [] && $tombstonesFile !== '') {
                    $tombstonesCount = $this->rewriteInventory($tombstonesFile, fn (string $rel): ?string => isset($this->carriedSources[$rel]) ? null : $rel);
                }
            }
            $totalFiles = $this->prunePathsCache($cachePath);
            $this->rewriteFilesList([]);
            if ($prevMap !== null) {
                $pending = array_diff_key(array_intersect_key($prevMap, $this->skippedSources), $this->carriedSources);
                $path = $tombstonesFile !== '' ? $tombstonesFile : $outDir . DIRECTORY_SEPARATOR . self::TOMBSTONES_LIST_NAME;
                if ($pending !== [] && $tombstonesFile !== '') {
                    $handle = fopen($path, 'rb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming inventory read
                    if ($handle === false) { throw new \RuntimeException('FilesArchiver: cannot read tombstones'); }
                    try {
                        while (($line = fgets($handle)) !== false) { unset($pending[rtrim($line, "\r\n")]); }
                        if (!feof($handle)) { throw new \RuntimeException('FilesArchiver: incomplete tombstone read'); }
                    } finally { fclose($handle); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming inventory read
                }
                if ($pending !== []) {
                    $handle = fopen($path, $tombstonesFile === '' ? 'wb' : 'ab'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming inventory append
                    if ($handle === false) { throw new \RuntimeException('FilesArchiver: cannot append tombstones'); }
                    try {
                        foreach ($pending as $rel => $_) {
                            $line = $rel . "\n";
                            if (fwrite($handle, $line) !== strlen($line)) { throw new \RuntimeException('FilesArchiver: cannot write tombstone'); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- streaming inventory append
                            $tombstonesCount++;
                        }
                    } finally { fclose($handle); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming inventory append
                    $tombstonesFile = $path;
                }
            }
        }

        return $this->buildDoneResult($partsCompleted, $partKinds, $totalFiles, $bytesWritten, $progress, $cachePath, $tombstonesFile, $tombstonesCount, $filesCarried, $prevMapSize, ($prevMap !== null));
    }

    /**
     * Validate a durable checkpoint against the scratch dir it describes.
     * Every field is re-checked: the task row is input like any other. A
     * cursor that fails any check yields null, and the caller starts one
     * fresh pass instead.
     *
     * @param array<string,mixed> $resume Cursor handed to archive().
     * @return array{run:array<string,mixed>,parts_completed:list<string>,part_kinds:list<string>,part_sizes:list<int>,components:list<array{name:string,current_part:int,resume_from:int}>,complete:bool,skipped:list<string>}|null
     */
    private function durableCheckpoint(array $resume, string $outDir, string $cachePath, bool $incremental): ?array
    {
        if (($resume['checkpoint'] ?? null) !== self::CHECKPOINT_VERSION || ($resume['incremental'] ?? null) !== $incremental) {
            return null;
        }
        // Bound to the discovery it was taken in: any later pass renewed the id.
        $discoveryId = $resume['discovery_id'] ?? null;
        $onDisk      = $this->currentDiscoveryId($outDir);
        if (!is_string($discoveryId) || $onDisk === '' || !hash_equals($onDisk, $discoveryId)) {
            return null;
        }
        $total = $resume['total_files'] ?? null;
        $bytes = $resume['cache_bytes'] ?? null;
        $carried = $resume['files_carried'] ?? null;
        $tombstonesCount = $resume['tombstones_count'] ?? null;
        if (!is_int($total) || $total < 0 || !is_int($bytes) || $bytes < 0 || !is_int($carried) || $carried < 0
            || !is_int($tombstonesCount) || $tombstonesCount < 0 || !is_file($cachePath)) {
            return null;
        }
        $tombstonesFile = $resume['tombstones_file'] ?? null;
        if (!is_string($tombstonesFile) || ($tombstonesFile !== '' && $tombstonesFile !== $outDir . DIRECTORY_SEPARATOR . self::TOMBSTONES_LIST_NAME)) {
            return null;
        }
        $complete = ($resume['complete'] ?? null) === true;
        clearstatcache(true, $cachePath);
        // Only the end-of-run prune rewrites the cache, after every part closed.
        if (!$complete && (int) @filesize($cachePath) !== $bytes) {
            return null;
        }

        $parts = $resume['parts_completed'] ?? null;
        $kinds = $resume['part_kinds'] ?? null;
        $sizes = $resume['part_sizes'] ?? null;
        if (!is_array($parts) || !is_array($kinds) || !is_array($sizes) || !ArrayShape::isList($parts) || !ArrayShape::isList($kinds)
            || !ArrayShape::isList($sizes) || count($kinds) !== count($parts) || count($sizes) !== count($parts)) {
            return null;
        }
        $highest = [];
        $seen    = [];
        foreach ($parts as $i => $name) {
            if (!is_string($name) || isset($seen[$name]) || preg_match(self::CLOSED_PART_NAME, $name, $m) !== 1) {
                return null;
            }
            $seen[$name] = true;
            $component   = $m[1];
            if ((int) $m[2] !== $this->generation || $kinds[$i] !== self::COMPONENT_PARTITIONS[$component]['kind'] || !is_int($sizes[$i]) || $sizes[$i] <= 0) {
                return null;
            }
            $path = $outDir . DIRECTORY_SEPARATOR . $name;
            clearstatcache(true, $path);
            if (is_link($path) || !is_file($path) || (int) @filesize($path) !== $sizes[$i]) {
                return null;
            }
            $highest[$component] = max($highest[$component] ?? 0, (int) $m[3]);
        }

        $components = $resume['components'] ?? null;
        if (!is_array($components)) {
            return null;
        }
        $cursor = [];
        foreach (array_keys(self::COMPONENT_PARTITIONS) as $component) {
            $entry = $components[$component] ?? null;
            if (!is_array($entry) || !is_int($entry['current_part'] ?? null) || !is_int($entry['resume_from'] ?? null)
                || $entry['current_part'] <= ($highest[$component] ?? 0) || $entry['resume_from'] < 0) {
                // A next part number at or below a closed one would overwrite it.
                return null;
            }
            $cursor[] = ['name' => $component, 'current_part' => $entry['current_part'], 'resume_from' => $entry['resume_from']];
        }

        $skipped = $resume['skipped'] ?? null;
        if (!is_array($skipped)) {
            return null;
        }
        foreach ($skipped as $rel) {
            if (!is_string($rel) || $rel === '') {
                return null;
            }
        }

        return [
            'run'             => [
                'discovery_id'     => $discoveryId,
                'total_files'      => $total,
                'cache_bytes'      => $bytes,
                'files_carried'    => $carried,
                'tombstones_file'  => $tombstonesFile,
                'tombstones_count' => $tombstonesCount,
                'incremental'      => $incremental,
            ],
            'parts_completed' => array_values($parts),
            'part_kinds'      => array_values($kinds),
            'part_sizes'      => array_values($sizes),
            'components'      => $cursor,
            'complete'        => $complete,
            'skipped'         => array_values($skipped),
        ];
    }

    /**
     * The durable resume cursor after the parts closed so far. A component
     * whose open part holds lines resumes at the first of them; every other
     * component resumes at $fileIndex. Skips inside an open part are left out
     * so a resume decides them again.
     *
     * @param array<string,mixed>                $run       Discovery facts for the whole pass.
     * @param array<string,array<string,mixed>>  $compState Per-component trackers.
     * @param list<string>                       $parts     Closed part filenames.
     * @param list<string>                       $kinds     Parallel entry kinds.
     * @param list<int>                          $sizes     Parallel part sizes.
     * @return array<string,mixed>
     */
    private function checkpointCursor(array $run, array $compState, int $fileIndex, array $parts, array $kinds, array $sizes, bool $complete): array
    {
        $components = [];
        $start      = $fileIndex;
        foreach ($compState as $name => $state) {
            $from              = $state['pending_from'] ?? $fileIndex;
            $components[$name] = ['current_part' => (int) $state['current_part'], 'resume_from' => (int) $from];
            $start             = min($start, (int) $from);
        }
        $pending = [];
        foreach ($this->pendingSkips as $skips) {
            $pending += $skips;
        }
        $settled = array_map('strval', array_keys(array_diff_key($this->skippedSources, $pending)));

        return $run + [
            'checkpoint'            => self::CHECKPOINT_VERSION,
            'complete'              => $complete,
            'file_index'            => $start,
            'components'            => $components,
            'parts_completed'       => $parts,
            'part_kinds'            => $kinds,
            'part_sizes'            => $sizes,
            'bytes_written'         => (int) array_sum($sizes),
            'skipped'               => $settled,
            'skipped_sources_count' => count($settled),
            'skipped_sources'       => $this->skippedSourceSample($settled),
        ];
    }

    /**
     * Delete every part file and libzip temporary output of the file
     * components in $outDir, except the closed parts named in $keep. Runs
     * before any packing, so the scratch dir never holds two passes.
     *
     * @param list<string> $keep Closed part filenames to keep.
     * @throws \RuntimeException When the dir cannot be listed or a stale part remains.
     */
    private function removeStaleParts(string $outDir, array $keep): void
    {
        $names = @scandir($outDir);
        if ($names === false) {
            throw new \RuntimeException('FilesArchiver: cannot list scratch dir');
        }
        $keep = array_flip($keep);
        foreach ($names as $name) {
            $name = (string) $name;
            if (isset($keep[$name]) || preg_match(self::STALE_PART_NAME, $name) !== 1) {
                continue;
            }
            $path = $outDir . DIRECTORY_SEPARATOR . $name;
            if (is_dir($path) && !is_link($path)) {
                continue;
            }
            wp_delete_file($path);
            clearstatcache(true, $path);
            if (file_exists($path) || is_link($path)) {
                throw new \RuntimeException('FilesArchiver: cannot remove stale part: ' . esc_html($name));
            }
        }
    }

    /**
     * Write a new random discovery id, atomically, and return it.
     *
     * @throws \RuntimeException When the id cannot be written: without it, an
     *                           earlier checkpoint could still validate.
     */
    private function renewDiscoveryId(string $outDir): string
    {
        $id        = bin2hex(random_bytes(16));
        $path      = $outDir . DIRECTORY_SEPARATOR . self::DISCOVERY_ID_NAME;
        $temporary = $path . '.tmp';
        $written   = @file_put_contents($temporary, $id);
        if ($written !== strlen($id) || !@rename($temporary, $path) || $this->currentDiscoveryId($outDir) !== $id) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-directory swap of a scratch file; WP_Filesystem::move() is not atomic
            if (is_file($temporary)) { wp_delete_file($temporary); }
            throw new \RuntimeException('FilesArchiver: cannot write the discovery id');
        }
        return $id;
    }

    /** The discovery id on disk, or '' when absent or malformed. */
    private function currentDiscoveryId(string $outDir): string
    {
        $path = $outDir . DIRECTORY_SEPARATOR . self::DISCOVERY_ID_NAME;
        clearstatcache(true, $path);
        if (is_link($path) || !is_file($path)) {
            return '';
        }
        $id = @file_get_contents($path, false, null, 0, 64);
        return is_string($id) && preg_match('/^[0-9a-f]{32}$/D', $id) === 1 ? $id : '';
    }

    /** A closed part settles its component: no line before the cursor is pending. */
    private function settlePart(array &$state): void
    {
        $state['pending_from'] = null;
        unset($this->pendingSkips[(string) $state['component']]);
    }

    /** Accept only this scratch dir's own tombstones.list from a cursor. */
    private function ownTombstonesFile($path, string $outDir): string
    {
        if (!is_string($path) || $path === '' || basename($path) !== self::TOMBSTONES_LIST_NAME) {
            return '';
        }
        $dir = realpath(dirname($path));
        return $dir !== false && rtrim($dir, DIRECTORY_SEPARATOR) === $outDir ? $outDir . DIRECTORY_SEPARATOR . self::TOMBSTONES_LIST_NAME : '';
    }

    /**
     * Open the next part for the given component tracker, always as a new,
     * empty archive. Mutates $state in-place.
     *
     * Resume policy: a part is never reopened. A resume deletes every part
     * its checkpoint does not list and archives that part's lines again, so
     * a closed part is never written twice.
     *
     * @param array<string,mixed> $state    Per-component tracker (by reference).
     * @param string              $compName Component key (filename prefix).
     * @param string              $outDir   Absolute scratch dir.
     * @param list<string>        $closed   Closed part filenames of this pass.
     * @throws \RuntimeException On zip open failure, or a part number that names a closed part.
     */
    private function openActivePart(array &$state, string $compName, string $outDir, array $closed): void
    {
        $partPath = $this->partPath($outDir, $compName, (int) $state['current_part']);
        if (in_array(basename($partPath), $closed, true)) {
            throw new \RuntimeException('FilesArchiver: part number reuses a closed part: ' . esc_html(basename($partPath)));
        }
        $zip       = ($this->zipFactory)();
        $openFlags = \ZipArchive::CREATE | \ZipArchive::OVERWRITE;
        if ($zip->open($partPath, $openFlags) !== true) {
            throw new \RuntimeException('FilesArchiver: cannot open zip part: ' . esc_html($partPath));
        }
        $state['zip']                  = $zip;
        $state['part_path']            = $partPath;
        $state['open_flags']           = $openFlags;
        $state['queued_sources']       = [];
        $state['part_entries']         = 0;
        $state['part_estimated_bytes'] = 0;
    }

    /**
     * Close the currently-open part for the given component tracker. Returns
     * the on-disk size + part filename so the caller can record the durable
     * bytes-written sum and the completed-parts list.
     *
     * Empty parts (`part_entries === 0`) are unlinked rather than recorded;
     * an empty zip never enters the manifest.
     *
     * @param array<string,mixed> $state Per-component tracker (by reference).
     * @return array{name:string,size:int}
     * @throws \RuntimeException On zip close failure.
     */
    private function closeActivePart(array &$state, ?callable $progress = null): array
    {
        /** @var \ZipArchive $zip */
        $zip      = $state['zip'];
        $partPath = (string) $state['part_path'];
        $entries  = (int) $state['part_entries'];

        if ($entries === 0) {
            // Best-effort drop the empty placeholder. ZipArchive::close on an
            // empty new zip can still write a tiny central-directory stub;
            // unlink covers that.
            @$zip->close();
            if (is_file($partPath)) {
                wp_delete_file($partPath);
            }
            $state['zip']       = null;
            $state['part_path'] = '';
            $state['queued_sources'] = [];
            $state['part_estimated_bytes'] = 0;
            return ['name' => '', 'size' => 0];
        }

        if (!@$zip->close()) {
            $failure = new ZipCloseException(basename($partPath), $zip->status, $zip->getStatusString());
            for ($attempt = 1; $attempt <= 2; $attempt++) {
                if (in_array($failure->libzipStatus(), [\ZipArchive::ER_TMPOPEN, \ZipArchive::ER_WRITE, \ZipArchive::ER_CLOSE, \ZipArchive::ER_RENAME], true)
                    || !$this->hasSourceSuspects($state['queued_sources'])) {
                    throw $failure; // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- ZipCloseException owns message escaping
                }
                // A part is always new, so a rebuild starts from an empty archive.
                $zip = ($this->zipFactory)();
                if ($zip->open($partPath, $state['open_flags']) !== true) { throw $failure; } // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- retains the libzip close diagnostic
                $state['zip'] = $zip;
                foreach ($state['queued_sources'] as $rel => $_) {
                    $metadata = $this->sourceMetadata((string) $rel);
                    if ($metadata === null) {
                        $this->skipSource($state, (string) $rel);
                        continue;
                    }
                    if (!$zip->addFile($this->sourceDir . DIRECTORY_SEPARATOR . $rel, (string) $rel)) {
                        $this->skipSource($state, (string) $rel);
                        continue;
                    }
                    $state['queued_sources'][$rel] = $metadata;
                }
                $state['part_entries'] = $zip->numFiles;
                $state['part_estimated_bytes'] = array_sum(array_column($state['queued_sources'], 'size'));
                if ($progress !== null) {
                    $this->emitProgress($progress, ['stage' => 'rebuilding_part', 'part' => basename($partPath), 'rebuild_attempt' => $attempt]);
                }
                if (@$zip->close()) {
                    $this->rewriteFilesList($state['queued_sources'], false);
                    $failure = null;
                    break;
                }
                $failure = new ZipCloseException(basename($partPath), $zip->status, $zip->getStatusString());
            }
            if ($failure !== null) { throw $failure; } // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- ZipCloseException owns message escaping
        }
        if ($state['part_entries'] === 0) {
            if (is_file($partPath)) { wp_delete_file($partPath); }
            $state['zip'] = null;
            $state['queued_sources'] = [];
            $state['part_path'] = '';
            return ['name' => '', 'size' => 0];
        }
        clearstatcache(true, $partPath);
        $size = (int) @filesize($partPath);
        $state['part_estimated_bytes'] = $size;
        $state['zip']       = null;
        $state['part_path'] = '';
        $state['queued_sources'] = [];
        return ['name' => basename($partPath), 'size' => $size];
    }

    /** @return array{size:int,mtime:int}|null */
    private function sourceMetadata(string $relativePath): ?array
    {
        $path = $this->sourceDir . DIRECTORY_SEPARATOR . $relativePath;
        clearstatcache(true, $path);
        if (is_link($path) || !is_file($path) || !is_readable($path)) { return null; }
        $size = @filesize($path);
        return $size === false ? null : ['size' => (int) $size, 'mtime' => (int) @filemtime($path)];
    }

    /** @param array<string,array{size:int,mtime:int}> $sources Queued source metadata. */
    private function hasSourceSuspects(array $sources): bool
    {
        foreach ($sources as $rel => $queued) {
            $current = $this->sourceMetadata((string) $rel);
            if ($current === null || $current['size'] !== $queued['size']) { return true; }
        }
        return false;
    }

    /** @param array<string,mixed> $state Open part state. */
    private function skipSource(array &$state, string $relativePath): void
    {
        $this->skippedSources[$relativePath] = true;
        if ($state['pending_from'] !== null) {
            // Inside an open part: settled only when that part closes.
            $this->pendingSkips[(string) $state['component']][$relativePath] = true;
        }
        unset($state['queued_sources'][$relativePath]);
        $zip = $state['zip'];
        if ($zip !== null) {
            if ($zip->locateName($relativePath) !== false && !$zip->deleteName($relativePath)) {
                throw new ZipCloseException(basename($state['part_path']), $zip->status, $zip->getStatusString()); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- ZipCloseException owns message escaping
            }
            $state['part_entries'] = $zip->numFiles;
        }
    }

    /**
     * @param array<string,array{size:int,mtime:int}> $sources     Durable queued metadata.
     * @param bool                                    $dropSkipped Drop skipped sources; only the end of the run does,
     *                                                             since a resume may still archive a skip it repeats.
     */
    private function rewriteFilesList(array $sources, bool $dropSkipped = true): void
    {
        $pendingCarry = $this->carriedSources;
        $this->rewriteInventory($this->filesListPath, function (string $line) use ($sources, $dropSkipped, &$pendingCarry): ?string {
            $rel = explode("\t", $line, 2)[0];
            if (isset($pendingCarry[$rel])) {
                $metadata = $pendingCarry[$rel];
                unset($pendingCarry[$rel]);
                return $rel . "\t" . $metadata['size'] . "\t" . $metadata['mtime'];
            }
            if ($dropSkipped && isset($this->skippedSources[$rel])) { return null; }
            if (isset($sources[$rel])) { return $rel . "\t" . $sources[$rel]['size'] . "\t" . $sources[$rel]['mtime']; }
            return $line;
        });
        if ($pendingCarry !== []) {
            $handle = fopen($this->filesListPath, 'ab'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- append retained parent metadata to the streaming inventory
            if ($handle === false) { throw new \RuntimeException('FilesArchiver: cannot append carried sources'); }
            try {
                foreach ($pendingCarry as $rel => $metadata) {
                    $line = $rel . "\t" . $metadata['size'] . "\t" . $metadata['mtime'] . "\n";
                    if (fwrite($handle, $line) !== strlen($line)) { throw new \RuntimeException('FilesArchiver: cannot write carried source'); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- streaming inventory append
                }
            } finally { fclose($handle); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- close the streaming inventory
        }
    }

    private function prunePathsCache(string $path): int
    {
        return $this->rewriteInventory($path, function (string $line): ?string {
            $tab = strpos($line, "\t");
            $rel = $tab === false ? $line : substr($line, $tab + 1);
            return isset($this->skippedSources[$rel]) ? null : $line;
        });
    }

    /** Replace a streaming inventory only after every retained row is written. */
    private function rewriteInventory(string $path, callable $map): int
    {
        $input = @fopen($path, 'rb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming inventory rewrite
        if ($input === false) { throw new \RuntimeException('FilesArchiver: cannot read inventory: ' . esc_html(basename($path))); }
        $temporary = $path . '.rewrite';
        $output = @fopen($temporary, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming inventory rewrite
        if ($output === false) {
            fclose($input); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming inventory rewrite
            throw new \RuntimeException('FilesArchiver: cannot rewrite inventory: ' . esc_html(basename($path)));
        }
        $count = 0;
        try {
            try {
                while (($line = fgets($input)) !== false) {
                    $mapped = $map(rtrim($line, "\r\n"));
                    if ($mapped === null || $mapped === '') { continue; }
                    $line = $mapped . "\n";
                    if (fwrite($output, $line) !== strlen($line)) { throw new \RuntimeException('FilesArchiver: incomplete inventory write'); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- streaming inventory rewrite
                    $count++;
                }
                if (!feof($input) || !fflush($output)) { throw new \RuntimeException('FilesArchiver: incomplete inventory rewrite'); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fflush -- durable inventory rewrite
            } finally {
                fclose($input); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming inventory rewrite
                fclose($output); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming inventory rewrite
            }
            if (!@rename($temporary, $path)) { throw new \RuntimeException('FilesArchiver: cannot replace inventory'); } // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic inventory replacement
        } finally {
            if (is_file($temporary)) { wp_delete_file($temporary); }
        }
        return $count;
    }

    /**
     * Walk $sourceDir and write the path-relative file list to $cachePath.
     * Returns the total line count (== total files in scope).
     *
     * Format: one line per file as "<component>\t<relpath>". Tagging at
     * discovery time means the pack loop is a pure dispatch — it never has
     * to recompute the component for an entry it's already seen.
     *
     * Memory stays flat: we never accumulate the list in a PHP array; each
     * discovered path is fwrite'd line-by-line.
     *
     * ADR-051: Also emits a sibling `files.list` file (relpath TAB size TAB
     * mtime, one line per packed file) so the next increment can diff against
     * this snapshot without a CP file-index round-trip. The files.list is
     * written to the SAME directory as $cachePath.
     *
     * When $prevMap is non-null (incremental mode) only CHANGED or NEW files
     * are written to paths.cache. A file is CHANGED iff:
     *   !isset($prevMap[$rel]) || (int)$size !== $prevMap[$rel]['size']
     *                          || (int)$mtime > $prevMap[$rel]['mtime']
     * Additionally, every $prevMap key NOT seen during the full-tree walk is
     * written to an on-disk tombstones.list sidecar (one relpath per line) and
     * returned as `tombstones_file` + `tombstones_count`. The tombstone list is
     * NEVER accumulated in a PHP array — only the line count and the file path
     * are returned so sub_state stays small regardless of deletion count.
     *
     * @param string                                     $cachePath Absolute path of the cache file to create.
     * @param array<string,array{size:int,mtime:int}>|null $prevMap   Previous snapshot file map (incremental mode);
     *                                                                null for a full-backup walk (include everything).
     * @param string|null                                $filesListPath  Absolute path of the files.list file; defaults
     *                                                                   to same dir as $cachePath.
     * @param string|null                                $tombstonesPath Absolute path of tombstones.list; defaults to
     *                                                                   same dir as $cachePath.
     * @return array{count:int,tombstones_file:string,tombstones_count:int} Count of cache lines written + tombstone on-disk info.
     * @throws \RuntimeException On unwritable cache file or unreadable source.
     */
    private function buildPathCache(
        string $cachePath,
        ?array $prevMap = null,
        ?string $filesListPath = null,
        ?string $tombstonesPath = null
    ): array {
        $handle = @fopen($cachePath, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
        if ($handle === false) {
            throw new \RuntimeException('FilesArchiver: cannot create path cache: ' . esc_html($cachePath));
        }

        // files.list sidecar — free to emit since SplFileInfo already has size+mtime.
        $outDir        = dirname($cachePath);
        $flPath        = $filesListPath ?? $outDir . DIRECTORY_SEPARATOR . self::FILES_LIST_NAME;
        $flHandle      = @fopen($flPath, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
        // Non-fatal if we can't open; the backup itself is unaffected.
        if ($flHandle === false) {
            $flHandle = null;
        }

        $count   = 0;
        $carried = 0; // ADR-051 instrumentation: unchanged files skipped (carry-forward).
        $srcLen  = strlen($this->sourceDir) + 1; // +1 for the separator
        $seenRels = []; // Used for tombstone computation in incremental mode.

        $isIncremental = ($prevMap !== null);

        // Tombstones sidecar: written incrementally, never accumulated in RAM.
        $tbPath   = $tombstonesPath ?? $outDir . DIRECTORY_SEPARATOR . self::TOMBSTONES_LIST_NAME;
        $tbHandle = null; // Opened lazily on first deletion found.
        $tombstonesCount = 0;
        $tombstonesFile  = '';

        try {
            $iterator = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator(
                    $this->sourceDir,
                    \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                ),
                \RecursiveIteratorIterator::SELF_FIRST
            );
        } catch (\UnexpectedValueException $e) {
            fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
            if ($flHandle !== null) {
                fclose($flHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
            }
            throw new \RuntimeException('FilesArchiver: cannot iterate sourceDir: ' . esc_html($e->getMessage()), 0, $e); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
        }

        /** @var \SplFileInfo $info */
        foreach ($iterator as $info) {
            $abs = (string) $info->getPathname();

            // Drop symlinks first (covers symlinked dirs AND files).
            // is_link() works on dirs in PHP.
            if (is_link($abs)) {
                continue;
            }

            // Normalise the relative path with forward slashes (zip standard).
            $rel = substr($abs, $srcLen);
            $rel = str_replace(DIRECTORY_SEPARATOR, '/', $rel);

            if ($this->isExcluded($rel)) {
                continue;
            }

            // Only file entries make it into the cache.
            if (!$info->isFile()) {
                continue;
            }

            $size  = (int) $info->getSize();
            $mtime = (int) $info->getMTime();

            // A5 (#187): file-size exclusion — applied after isFile() guard so
            // we only check real file sizes, not directory stat artefacts.
            if ($this->isExcludedBySize($size)) {
                continue;
            }

            if ($isIncremental) {
                // Track every rel we encounter for tombstone diff.
                $seenRels[$rel] = true;

                // Gate: only include in the paths.cache if the file is new or changed.
                $prev = $prevMap[$rel] ?? null;
                if ($prev !== null && $prev['size'] === $size && $prev['mtime'] >= $mtime) {
                    // Unchanged — skip from paths.cache, but still emit to files.list
                    // so the new snapshot's files.list is a complete picture.
                    if ($flHandle !== null) {
                        fwrite($flHandle, $rel . "\t" . $size . "\t" . $mtime . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
                    }
                    $carried++;
                    continue;
                }
            }

            $component = $this->classifyComponent($rel);

            // A1 (#187): selective component backup. Skip components the
            // operator did not include. Incremental runs honour this too —
            // if a component is excluded the operator deliberately doesn't
            // want it, so it must not appear in the files.list sidecar either
            // (omitting it from files.list means the next increment treats
            // those files as "not in prev snapshot" and re-archives them if
            // the component is re-added later — correct semantics).
            if (isset($this->excludeComponents[$component])) {
                continue;
            }

            fwrite($handle, $component . "\t" . $rel . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
            if ($flHandle !== null) {
                fwrite($flHandle, $rel . "\t" . $size . "\t" . $mtime . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
            }
            $count++;
        }

        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
        if ($flHandle !== null) {
            fclose($flHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
        }

        // Compute tombstones: prev keys absent from the fresh full-tree walk.
        // Written incrementally to tombstones.list on disk — never accumulated
        // in a PHP array — so deletion of a plugin with thousands of files
        // costs only a constant amount of RAM.
        if ($isIncremental && $prevMap !== null) {
            foreach ($prevMap as $prevRel => $_) {
                if (isset($seenRels[$prevRel])) {
                    continue;
                }
                // Lazy-open the tombstones file on first deletion found.
                if ($tbHandle === null) {
                    $tbHandle = @fopen($tbPath, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
                    if ($tbHandle === false) {
                        $tbHandle = null;
                        // Non-fatal: tombstones.list write failure leaves
                        // tombstones_count = 0; the sidecar-spill in
                        // saveTaskState is still a correct fallback.
                        continue;
                    }
                    $tombstonesFile = $tbPath;
                }
                fwrite($tbHandle, $prevRel . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
                $tombstonesCount++;
            }

            if ($tbHandle !== null) {
                fclose($tbHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
            }
        }

        return [
            'count'            => $count,
            'carried'          => $carried,
            'tombstones_file'  => $tombstonesFile,
            'tombstones_count' => $tombstonesCount,
        ];
    }

    /**
     * Map a wp-content-relative path to its component bucket name. Path-
     * prefix match against COMPONENT_PARTITIONS roots; the `wp-content`
     * catch-all takes anything that didn't match a specific root.
     *
     * @param string $relativePath Path relative to $sourceDir, `/`-separated.
     * @return string Component key (one of the COMPONENT_PARTITIONS keys).
     */
    private function classifyComponent(string $relativePath): string
    {
        // Normalize: strip any leading slash so prefix match is deterministic.
        $p = ltrim($relativePath, '/');
        foreach (self::COMPONENT_PARTITIONS as $compName => $cfg) {
            $root = (string) $cfg['root'];
            if ($root === '') {
                // Catch-all — always matches; we save it for last by virtue
                // of being the last entry in COMPONENT_PARTITIONS.
                continue;
            }
            if (strncmp($p, $root, strlen($root)) === 0) {
                return $compName;
            }
        }
        return 'wp-content';
    }

    /**
     * Test whether a relative path should be excluded. Default runtime and
     * update-staging roots are matched as anchored path prefixes; WPMgr
     * scratch names, secret/state filenames, and caller excludes are matched
     * by exact segment name. Also applies A4 extension exclusion (#187).
     *
     * @param string $relativePath Path relative to $sourceDir, `/`-separated.
     * @return bool
     */
    private function isExcluded(string $relativePath): bool
    {
        $p = ltrim($relativePath, '/');
        foreach ($this->pathExcludes as $root) {
            if ($p === $root || strncmp($p, $root . '/', strlen($root) + 1) === 0) {
                return true;
            }
        }

        $segments = explode('/', $relativePath);

        // The agent's own transient files, anchored at a segment start.
        foreach ($segments as $segment) {
            foreach (self::AGENT_RESERVED_PREFIXES as $prefix) {
                if (strncmp($segment, $prefix, strlen($prefix)) === 0) {
                    return true;
                }
            }
        }

        // Segment-name exclusion (WPMgr defaults + caller excludes).
        if ($this->excludes !== []) {
            foreach ($segments as $segment) {
                if ($segment === '') {
                    continue;
                }
                if (in_array($segment, $this->excludes, true)) {
                    return true;
                }
            }
        }

        // A4 (#187): extension exclusion. Only applies to file entries (last
        // segment has a dot). Dirs have no extension so they pass through and
        // are only dropped later when !isFile() is checked.
        if ($this->excludeExtensions !== []) {
            $last = end($segments);
            if ($last !== false && $last !== '') {
                $dotPos = strrpos($last, '.');
                if ($dotPos !== false) {
                    $ext = strtolower(substr($last, $dotPos + 1));
                    if (in_array($ext, $this->excludeExtensions, true)) {
                        return true;
                    }
                }
            }
        }

        return false;
    }

    /**
     * Test whether a file should be excluded by size. A5 (#187): skip files
     * whose byte-size exceeds the configured cap. Returns false when no cap is
     * set or for directories (size = 0).
     */
    private function isExcludedBySize(int $sizeBytes): bool
    {
        return $this->excludeFileSizeBytes > 0 && $sizeBytes > $this->excludeFileSizeBytes;
    }

    /**
     * Build the absolute path of part N for the given component (1-indexed,
     * 3-digit zero-padded). Each component has its own filename prefix and the
     * part name is namespaced by the snapshot generation:
     * `plugins.gNNN.partMMM.zip`, `themes.gNNN.partMMM.zip`, etc.
     *
     * The generation infix (`.gNNN.`) is what keeps gen-0 and gen-1 parts from
     * colliding by name on the restore overlay — both used to emit
     * `plugins.part001.zip`, so the later generation silently clobbered the
     * earlier one's carry-forward part in the agent's flat staging map.
     *
     * @param string $outDir   Absolute scratch dir.
     * @param string $compName Component key (used as the filename prefix).
     * @param int    $n        Part number.
     * @return string
     */
    private function partPath(string $outDir, string $compName, int $n): string
    {
        return $outDir . DIRECTORY_SEPARATOR . self::partName($compName, $this->generation, $n);
    }

    /**
     * Build the generation-namespaced part filename for a component.
     * Format: `<component>.gNNN.partMMM.zip` (both NNN and MMM 3-digit padded).
     *
     * @param string $compName Component key (filename prefix).
     * @param int    $generation Snapshot generation (0 = base).
     * @param int    $n        Part number (1-indexed).
     * @return string
     */
    public static function partName(string $compName, int $generation, int $n): string
    {
        return $compName
            . '.g' . str_pad((string) max(0, $generation), 3, '0', STR_PAD_LEFT)
            . '.part' . str_pad((string) $n, 3, '0', STR_PAD_LEFT)
            . '.zip';
    }

    /**
     * Classify a part filename to its component kind, tolerant of the
     * generation namespace infix. Matches `<component>.[gNNN.]partMMM.zip`
     * for the four known components and returns the manifest entry_kind:
     * 'plugin' | 'theme' | 'upload' | 'wp-content'. Returns '' when the name
     * is not a recognised component part (caller falls back to the legacy
     * 'file' kind for pre-namespace / pre-Track-5 part names).
     *
     * Both the backup-side manifest classifier (EncryptAndUpload::entryKind)
     * and the restore-side artifact classifier (RestoreRunner) share this so
     * the namespaced part name round-trips through one rule.
     *
     * @param string $logical Part filename (any case).
     * @return string Component entry_kind, or '' if not a component part.
     */
    public static function componentKindFromPartName(string $logical): string
    {
        $lower = strtolower($logical);
        if (substr($lower, -4) !== '.zip') {
            return '';
        }
        // <component>.  (optionally  gNNN.)  partMMM.zip
        foreach (self::COMPONENT_PARTITIONS as $compName => $cfg) {
            $prefix = $compName . '.';
            if (strncmp($lower, $prefix, strlen($prefix)) !== 0) {
                continue;
            }
            $rest = substr($lower, strlen($prefix));
            // Accept both the namespaced `gNNN.partMMM.zip` and the legacy
            // `partMMM.zip` (so a mid-upgrade chain still classifies).
            if (preg_match('/^(g\d+\.)?part\d+\.zip$/', $rest) === 1) {
                return (string) $cfg['kind'];
            }
        }
        return '';
    }

    /**
     * @param list<string>|null $paths Paths to sample; null samples every skipped source.
     * @return list<string> UTF-8 display names; source bookkeeping retains exact bytes.
     */
    private function skippedSourceSample(?array $paths = null): array
    {
        return array_map(static fn ($path): string => json_decode(json_encode((string) $path, JSON_INVALID_UTF8_SUBSTITUTE), true), array_slice($paths ?? array_keys($this->skippedSources), 0, 20));
    }

    /**
     * Wrap the progress callback so a buggy callback can never abort the
     * archive run. Backup progress is observability, not correctness.
     *
     * Also stamps `$lastProgressEmitAt` (GH #279) on every call: file-count,
     * rotation, or heartbeat-triggered alike, so `maybeEmitHeartbeat()`'s
     * wall-clock gate always measures from the most recent emit, whichever
     * trigger fired it.
     *
     * @param callable             $progress User-supplied callback.
     * @param array<string,mixed>  $detail   Detail payload.
     * @return void
     */
    private function emitProgress(callable $progress, array $detail): void
    {
        $detail['skipped_sources_count'] = count($this->skippedSources);
        $detail['skipped_sources'] = $this->skippedSourceSample();
        if (empty($detail['done'])) {
            foreach (['files_done', 'files_total'] as $field) {
                if (isset($detail[$field])) { $detail[$field] = max(0, $detail[$field] - count($this->skippedSources)); }
            }
        }
        $this->lastProgressEmitAt = microtime(true);
        try {
            $progress('archiving_files', $detail);
        } catch (\Throwable $e) { // phpcs:ignore -- intentional swallow.
            // Swallow. Don't even surface in a return — the backup itself
            // is making forward progress and that's what matters.
        }
    }

    /**
     * GH #279: emit a heartbeat progress tick if `$heartbeatIntervalSeconds`
     * has elapsed since the last emit. Called on every pack-loop iteration
     * (cheap: one `microtime(true)` call) so a stretch of few-but-large files
     * still refreshes CP `progress_updated_at` before the watchdog's
     * soft-stall threshold, even when the file-count trigger
     * (PROGRESS_EVERY_FILES) would otherwise stay quiet for a long time.
     * Routed through `emitProgress()`, so a broken progress callback can
     * never abort the archive run, and every emit (this one included)
     * resets the wall-clock window.
     *
     * @param callable             $progress User-supplied callback.
     * @param array<string,mixed>  $detail   Detail payload (a `heartbeat`
     *                                       flag is merged in so the caller
     *                                       can distinguish this tick from a
     *                                       normal file-count/rotation tick).
     * @return void
     */
    private function maybeEmitHeartbeat(callable $progress, array $detail): void
    {
        if ((microtime(true) - $this->lastProgressEmitAt) < $this->heartbeatIntervalSeconds) {
            return;
        }
        $detail['heartbeat'] = true;
        $this->emitProgress($progress, $detail);
    }

    /**
     * Build the terminal "done" sub-state and fire the final progress tick.
     *
     * Tombstones are carried as an on-disk file path + count, NOT as an in-memory
     * array. This keeps sub_state small regardless of deletion count (a deletion-
     * heavy increment removing thousands of files adds only two scalar fields to
     * sub_state instead of a multi-KB array).
     *
     * @param list<string> $parts              Closed part filenames.
     * @param list<string> $partKinds          Parallel list of per-part entry_kinds.
     * @param int          $totalFiles         Total file count from the cache.
     * @param int          $bytesWritten       Sum of part sizes on disk.
     * @param callable     $progress           Progress callback (see archive()).
     * @param string|null  $cachePath          Cache file to clean up, if any.
     * @param string       $tombstonesFile     Absolute path to the on-disk tombstones.list
     *                                         ('' when no deletions).
     * @param int          $tombstonesCount    Number of deleted paths written to $tombstonesFile.
     * @return array<string,mixed>
     */
    private function buildDoneResult(
        array $parts,
        array $partKinds,
        int $totalFiles,
        int $bytesWritten,
        callable $progress,
        ?string $cachePath = null,
        string $tombstonesFile = '',
        int $tombstonesCount = 0,
        int $filesCarried = 0,
        int $prevMapSize = 0,
        bool $incremental = false
    ): array {
        // Cache file is no longer needed once we're done; leaving it under
        // the per-run scratch dir means it gets cleaned with the scratch.
        // We don't unlink so a post-mortem can inspect.
        unset($cachePath);

        $this->emitProgress($progress, [
            'done'              => true,
            'parts'             => $parts,
            'part_kinds'        => $partKinds,
            'files_total'       => $totalFiles,
            'bytes_written'     => $bytesWritten,
            'tombstones_file'   => $tombstonesFile,
            'tombstones_count'  => $tombstonesCount,
            // ADR-051 instrumentation: changed/new packed (files_total) vs
            // unchanged carried-forward (files_carried) vs prevMap entry count.
            'files_changed'     => $totalFiles,
            'files_carried'     => $filesCarried,
            'prevmap_size'      => $prevMapSize,
            'incremental'       => $incremental,
        ]);

        return [
            'done'              => true,
            'parts'             => $parts,
            'part_kinds'        => $partKinds,
            'parts_total'       => count($parts),
            'files_total'       => $totalFiles,
            'bytes_written'     => $bytesWritten,
            'tombstones_file'   => $tombstonesFile,
            'tombstones_count'  => $tombstonesCount,
            // ADR-051 instrumentation surfaced to TaskRunner -> CP progress.
            'files_changed'     => $totalFiles,
            'files_carried'     => $filesCarried,
            'prevmap_size'      => $prevMapSize,
            'incremental'       => $incremental,
            'skipped_sources_count' => count($this->skippedSources),
            'skipped_sources' => $this->skippedSourceSample(),
        ];
    }
}
