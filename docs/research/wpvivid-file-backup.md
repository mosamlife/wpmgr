# WPvivid Backup & Migration — File/Directory Backup Internals

Research target: `wpvivid-backuprestore` plugin (WP.org). All citations are from
the trunk SVN at `https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/`.
The plugin ships two backup engines side-by-side — the legacy one in
`includes/class-wpvivid-backup-site.php` + `includes/class-wpvivid-zipclass.php`
+ `includes/zip/class-wpvivid-pclzip.php`, and the current "v2" engine in
`includes/new_backup/` (`class-wpvivid-backup2.php`,
`class-wpvivid-backup-task_2.php`, `class-wpvivid-zip.php`,
`class-wpvivid-restore-file-2.php`). The notes below focus on the v2 engine,
which is the one the UI now drives.

Default constants are all defined at the top of the main plugin bootstrap
(`wpvivid-backuprestore.php`):

```php
define('WPVIVID_DEFAULT_MAX_FILE_SIZE',200);   // MB per archive part
define('WPVIVID_MAX_EXECUTION_TIME',300);      // seconds, generic SAPI
define('WPVIVID_MAX_EXECUTION_TIME_FCGI',180); // seconds, FastCGI/FPM
define('WPVIVID_MEMORY_LIMIT','512M');
define('WPVIVID_DEFAULT_COMPRESS_TYPE','zip');
define('WPVIVID_DEFAULT_NO_COMPRESS',true);
define('WPVIVID_DEFAULT_USE_TEMP',1);
define('WPVIVID_RESUME_RETRY_TIMES',6);
```

## 1. tar binary vs pure PHP

WPvivid never shells out. There is no `exec()`, `proc_open()`, `system()`,
or `passthru()` call against `tar`/`zip` anywhere in the file-backup path. The
v2 engine instantiates `ZipArchive` directly when the extension is present, and
falls back to a bundled PclZip when it isn't
(`includes/new_backup/class-wpvivid-zip.php`):

```php
if ($this->check_ziparchive_available()) {
    $this->zip_object = new ZipArchive();
} else {
    $this->zip_object = new WPvivid_PclZip_2();
}
```

`WPvivid_PclZip_2` is a thin shim that buffers `addFile`/`addFromString` calls
and flushes them through the bundled `class-wpvivid-pclzip.php` (a vendored,
slightly patched copy of WordPress's own `wp-admin/includes/class-pclzip.php`)
on `close()`.

## 2. Archive format

Pure ZIP — never tar/tar.gz. `WPVIVID_DEFAULT_COMPRESS_TYPE='zip'`. ZipArchive
is opened with the standard create flag:

```php
$create_code = (version_compare(PHP_VERSION,'5.2.12','>') &&
                defined('ZIPARCHIVE::CREATE'))
             ? ZIPARCHIVE::CREATE : 1;
```

## 3. Walking wp-content

It does **not** use `RecursiveDirectoryIterator`. The v2 task object uses
classic `opendir()`/`readdir()` recursion in
`WPvivid_Backup_Task_2::get_file_cache()` and `_get_files()`
(`includes/new_backup/class-wpvivid-backup-task_2.php`):

```php
$handler = opendir($path);
while (($filename = readdir($handler)) !== false) {
    if ($filename != "." && $filename != "..") {
        if (is_dir($path.'/'.$filename)) {
            $this->_get_folders($path.'/'.$filename, ...);
        }
    }
}
@closedir($handler);
```

Rather than build the full file list in memory, the walker streams discovered
paths into **on-disk cache files** (`$cache_prefix.$n.'.cache'`, opened with
`fopen(..., 'a')`). The packer later reads back from those cache files when
populating each zip part — this keeps RAM flat regardless of file count.

Excludes are compiled into regex arrays and matched with `preg_match`:

```php
private function regex_match($regex_array, $string, $mode) {
    if ($mode == 0) {
        foreach ($regex_array as $regex) {
            if (preg_match($regex, $string)) { return false; }
        }
        return true;
    }
}
```

Patterns are built from `'#^'.preg_quote($path,'/').'#'` (path prefix) and
extension globs. Default exclusions (`default_exclude_folders()` in
`class-wpvivid-backup2.php`) cover `WP_CONTENT_DIR.'/cache'`, the upload-dir
`backwpup` folder, `WP_CONTENT_DIR.'/mysql.sql'`, and a long list of competing
plugins' backup folders.

Symlinks are gated by a per-task setting `backup_symlink_folder`:

```php
if ($backup_symlink_folder == 1 ||
    ($backup_symlink_folder == 0 && !@is_link($path.'/'.$filename))) {
    // descend / add
}
```

Default behaviour: symlinks are **skipped** (setting `0`).

## 4. Chunking — pack-N-files-then-rotate

The packer is a single linear loop that adds files until the current zip
exceeds the part-size limit OR until 55,000 entries, then closes and opens a
new part (`class-wpvivid-backup-task_2.php`, `do_backup_merge()` and
`do_backup_files()`):

```php
foreach ($files as $file) {
    if ($max_zip_file_size == 0) $max_zip_file_size = 4 * 1024 * 1024 * 1024;
    $zip->add_file($zip_file_name, $file, basename($file), dirname($file));
    $i++;
    if ((filesize($zip_file_name) > $max_zip_file_size) || ($i >= 55000)) {
        $json = $this->get_json_info('backup_merge', $json);
        $this->update_zip_file(basename($zip_file_name), 1, $json);
        $zip_file_name = $path . $this->add_zip_file('backup_merge');
    }
}
```

Default `max_file_size` is **200 MB** per part
(`WPVIVID_DEFAULT_MAX_FILE_SIZE = 200`, multiplied by `1024*1024` at use). A
user-set `0` falls back to `4 GiB`. The threshold is measured against the
**resulting archive file size** on disk (`filesize($zip_file_name)`), not the
source-bytes-added, so the rotation honours whatever compression ratio was
achieved. Output naming is `prefix_type.part001.zip`, `.part002.zip`, …
generated by `add_zip_file()`.

This is the "rotate to a fresh archive" pattern — there is never a
single-archive split via `split(1)`-style byte slicing.

## 5. Streaming vs in-memory

Per-file ingest is delegated to `ZipArchive::addFile($realPath, $localName)`,
so each entry is read by the C extension as it deflates/stores — no PHP-side
`fread` loop and no `addFromString` of the file body. The only path that
touches `addFromString` is for small JSON metadata sidecars.

The PclZip fallback **does** buffer file paths in `$this->addfiles[]` and
flushes on `close()`, but it still streams bytes one entry at a time via
PclZip's internal `PCLZIP_READ_BLOCK_SIZE = 2048` reads, so neither path loads
a whole file into memory. Practical max file size is bounded only by the
target filesystem and the 200 MB part rotation — a single source file larger
than a part will, however, produce an oversized part because the rotation
check fires only between entries.

## 6. Locked / changing files

There is no mid-backup lock detection. The packer just calls
`file_exists($file)` immediately before `addFile`:

```php
if (file_exists($file)) {
    $this->zip_object->addFile($file, $new_file);
}
```

Files that vanish between enumeration (cache-write) and packing are silently
dropped. Files mutated during backup are packed in whatever state ZipArchive
reads them — there is no snapshot, no `flock`, no retry, no consistency
guarantee.

## 7. Long-running execution

This is the single most engineered part of WPvivid. Three layers:

1. **Per-request `set_time_limit`** — `WPvivid_Backup_Task_2::set_time_limit()`
   calls `@set_time_limit($settings['max_execution_time'])` (default 300 s, 180
   s under FCGI) before every job.
2. **Stateful checkpointing in `wp_options`** — the entire task state
   (current job, current zip part, the file-cache file path, and a numeric
   index into it) is persisted on every step:

   ```php
   public function update_zipped_file_index($index) {
       $this->task['jobs'][$this->current_job]['index'] = $index;
       $this->update_task();    // -> WPvivid_Setting::update_task($id, $task)
   }
   ```

   The cache-file design from §3 is what makes this resumable: the walker's
   output is durable on disk, and the packer only needs to remember "I was on
   line N of cache file M."
3. **Shutdown handler + wp-cron resume** — `class-wpvivid-backup2.php`
   registers `deal_backup_shutdown_error()` and matches PHP's fatal message:

   ```php
   if ($preg_match('/Maximum execution time of.*$/', $error['message'])) {
       $resume_backup = true;
       $max_execution_time = true;
   }
   // ...
   wp_schedule_single_event($resume_time,
       'wpvivid_backup_2_schedule_event', array($task_id));
   ```

   Status transitions `running -> wait_resume -> running`, capped at
   `WPVIVID_RESUME_RETRY_TIMES = 6` attempts. A separate `task_monitor` cron
   watchdog re-arms stalled tasks. AJAX entry points
   (`wp_ajax_wpvivid_backup_now_2`) loop `while (!is_backup_finished())` inside
   one request; cron picks up the slack across requests.

## 8. Memory bounding

`@ini_set('memory_limit', WPVIVID_MEMORY_LIMIT)` (default `512M`) per request:

```php
public function set_memory_limit() {
    $memory_limit = isset($this->task['setting']['memory_limit'])
                  ? $this->task['setting']['memory_limit']
                  : WPVIVID_MEMORY_LIMIT;
    @ini_set('memory_limit', $memory_limit);
}
```

I did not find explicit `gc_collect_cycles()` calls or `$wp_object_cache`
flushing in the file-backup hot path. The actual memory floor stays low
because (a) the file list is on disk, not in PHP arrays, and (b) ZipArchive
does the heavy lifting in C.

## 9. Compression

Default is **store, not deflate** (`WPVIVID_DEFAULT_NO_COMPRESS = true`). The
legacy zipclass path explicitly threads `WPVIVID_PCLZIP_OPT_NO_COMPRESSION`
into PclZip's `add()`. The v2 ZipArchive path uses `addFile` without setting
`setCompressionName(..., ZipArchive::CM_DEFLATE)`, which on most builds means
the default deflate level — but the per-task `no_compress` flag (default on)
is honoured by the wrapper. There is no user-exposed compression-level slider,
just on/off.

## 10. Restore

`WPvivid_Restore_File_2::extract_by_index()` is the mirror image of the packer:

```php
public function extract_by_index($file_name,$root_path,$start,$end,$option) {
    $index = $start.'-'.$end;
    $zip_ret = $archive->extractByIndex($index,
        WPVIVID_PCLZIP_OPT_PATH, $root_path, ...);
}
```

For each `.partNNN.zip` it tracks a `[start,end]` entry-index window and
persists progress back into the `wpvivid_restore_task` option after every
slice, so restore is per-request chunked the same way backup is. Extraction
writes directly into the live `ABSPATH` / `WP_CONTENT_DIR` — there is no
staging dir for files. The DB-restore reciprocal does drop a temporary `db.php`
drop-in to keep WordPress booting while tables are rewritten, but the
file-restore path does not need a "WP-out-of-the-way" mode.

## 11. Lessons for WPMgr

phpbu's `Source\Tar` requires the `tar` binary, which is missing on plenty of
managed-WP hosts (WP Engine restricts shell, Kinsta/some Pantheon containers
strip GNU tar, Windows hosts never have it). WPvivid's choice is the proven
answer for the WordPress audience: ship a pure-PHP packer.

**Recommendation: option (a) — write a custom phpbu `Source` backed by
`ZipArchive`, modelled on WPvivid v2.** Concrete take-aways:

- Use `ZipArchive::addFile` (NOT `addFromString`) so the C extension streams.
- Walk with `opendir`/`readdir` and write the discovered paths to an on-disk
  cache file, not to an in-memory array. This is what makes resume cheap and
  keeps RAM flat on sites with 500k+ uploads.
- Rotate parts on **resulting archive bytes** (`filesize($current)`) plus an
  entry-count cap (~50k) — WPvivid's 200 MB / 55,000-entry combo is a
  sensible default.
- Persist `(current_part, cache_file, line_offset)` after every entry; on
  resume, reopen the part with `ZipArchive::CREATE` (it appends).
- Register a `register_shutdown_function` that pattern-matches PHP's
  max-execution-time fatal and schedules a `wp_schedule_single_event` resume,
  capped at N retries.
- Skip symlinks by default; expose a setting for power users.
- Keep PclZip as a fallback for hosts without the zip extension (rare but
  real on stripped-down PHP builds).

**Pros vs requiring tar:** runs everywhere PHP runs, no `proc_open`
permission needed, identical behaviour across OSes, restore can be done
entirely from PHP (browser upload of a zip, no shell).
**Cons:** ZIP central-directory rewriting at `close()` is O(entries) and can
itself blow the time budget on huge archives — the part rotation mitigates
this. Pure-PHP can't match `tar | pigz` throughput on a beefy box, so for
self-hosted users with shell access we should still prefer phpbu's
`Source\Tar` and treat the ZipArchive source as the portable fallback —
auto-select based on `which tar` + `proc_open` availability at runtime.
