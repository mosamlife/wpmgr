# WPvivid Restore — code-level deep dive

Companion to `wpvivid-async-progress-restore.md`. Source: WP.org SVN
trunk; no public GitHub mirror. All line numbers are against trunk as of
fetch.

- Orchestrator: `includes/new_backup/class-wpvivid-restore2.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore2.php>
- DB restore: `includes/new_backup/class-wpvivid-restore-db-2.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore-db-2.php>
- DB driver: `includes/new_backup/class-wpvivid-restore-db-method-2.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore-db-method-2.php>
- File restore: `includes/new_backup/class-wpvivid-restore-file-2.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore-file-2.php>
- UI / AJAX poller: `admin/partials/wpvivid-backup-restore-page-display.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/admin/partials/wpvivid-backup-restore-page-display.php>
- Constants: `wpvivid-backuprestore.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/wpvivid-backuprestore.php>
- Downloader: `includes/class-wpvivid-downloader.php`
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-downloader.php>

There is no `includes/restore/` directory, no `class-wpvivid-restore.php`,
no `class-wpvivid-restore-database.php`, no `class-wpvivid-restore-files.php`.
The "v2" files under `includes/new_backup/` are the live restore engine on
trunk; the legacy classes are gone.

---

## 1. DB restore state machine

### 1.1 Where the .sql.gz comes from

Restore expects the artifacts already on disk under
`WP_CONTENT_DIR/wpvividbackups/`. The `wpvivid_init_restore_task_2`
handler (`restore2.php:36`) hits
`WPvivid_Backuplist::get_backup_by_id()` and assumes a `WPvivid_Backup_Item`
with `get_local_path()` and `get_backup_path($file)` resolves to a local
file. If the artifact lives only on a remote (S3/FTP/…), a **separate**
download pre-step (`wpvivid_download_restore_file`,
`restore-page-display.php:1231`) pulls it down via the
`WPvivid_downloader::download_ex()` chunked-callback streamer
(`downloader.php:115` →
`$remote->download($file,$local_path,array($this,'download_callback_v2'))`).
The UI does not start the restore handler until the file is fully on
local disk. **Download and restore are two AJAX phases, never
interleaved.**

### 1.2 SQL artifact format and streaming

The DB backup is wrapped in a zip (look at
`restore-db-2.php:130` calling `get_sql_file()`, which inspects the zip
table-of-contents via `WPvivid_ZipClass::list_file()`). The first step of
the DB sub-task (`restore-db-2.php:66`–`168`) extracts the `.sql` file
out of that zip into `local_path` using PclZip, then records each
extracted file in `sub_task['exec_sql']['sql_files'][$name]` with
`sql_file_size = filesize(...)`, `sql_offset = 0`, `finished = 0`.
`sub_task['unzip_file']['unzip_finished'] = 1` flips after extraction
and the loop proceeds to import.

Streaming is **`fopen` + `fseek` + `fgets`** line by line
(`restore-db-2.php:488`–`710`):

```php
$sql_handle = fopen($sql_file,'r');
fseek($sql_handle,$sub_task['exec_sql']['sql_files'][$sql_file_name]['sql_offset']);
…
$this->execute_sql('START TRANSACTION',$sub_task);
while(!feof($sql_handle))
{
    if(empty($query)) {
        $sub_task['exec_sql']['sql_files'][$sql_file_name]['sql_offset']=ftell($sql_handle);
        …
        if($read_offset>$max_buffer_size) {
            fclose($sql_handle);
            $this->execute_sql('COMMIT',$sub_task);
            $sub_task['exec_sql']['sql_files'][$sql_file_name]['finished']=0;
            return ['result'=>'success','sub_task'=>$sub_task]; // yield to next AJAX
        }
    }
    $line = fgets($sql_handle);
    …
    if ($endWith == ';') {
        if (preg_match('#^\\s*CREATE TABLE#', $query))         { $sub_task['exec_sql']['current_table']=$this->create_table($query,…); }
        else if (preg_match('#^\\s*INSERT INTO#', $query))     { $this->insert($query,$sub_task); }
        else if (preg_match('#^\\s*DROP TABLE #', $query))     { $this->drop_table($query,$sub_task); }
        else if (preg_match('#\/*!#', $query))                  { $this->replace_table_execute_sql(…); }
        else                                                    { $this->execute_sql($query,$sub_task); }
        $query = '';
    }
}
```

That's not a real SQL splitter — it's a *line ending in `;`* splitter.
Multi-line strings with semicolons inside `'…'` would explode. WPvivid
gets away with it because the dump is produced by their own dumper
(`class-wpvivid-mysqldump2.php`), which emits one statement per line
and escapes embedded newlines as `\n` inside the value.

The buffer-based yielding ceiling is
`restore_detail_options['sql_file_buffer_pre_request'] * 1024 * 1024`
(`restore-db-2.php:503`). When the per-request budget runs out, the loop
exits and the task option carries `sql_offset = ftell(...)` so the next
HTTP request resumes mid-file. Default is 2 MB per request unless the
user overrides it in Advanced Settings.

### 1.3 Solving the "can't restore the DB you're running on" problem

The prior dossier was correct. Every CREATE/INSERT/DROP rewrites the
table name from `wp_X` to `tmp<db_id>_X` before execution
(`restore-db-2.php:1207` `create_table()`):

```php
$temp_new_prefix='tmp'.$sub_task['exec_sql']['db_id'].'_';
…
$new_table_name = $this->temp_new_prefix.substr($table_name,strlen($this->old_prefix));
$query=str_replace($table_name,$new_table_name,$query);
```

So WordPress keeps reading and writing `wp_options` (including the
running `wpvivid_restore_task` record itself) the whole time. Only at
the very end, after **all** SQL is replayed, does
`rename_db()` swap things atomically per-table
(`restore-db-2.php:3198`–`3302`):

```php
$wpdb->query('SET FOREIGN_KEY_CHECKS=0;');
…
foreach ($new_tables as $table) {
    $new_table=$this->str_replace_first($temp_new_prefix,$wpdb->prefix,$table);
    if($wpdb->query('DROP TABLE IF EXISTS ' . $new_table)===false) { return $ret; }
}
foreach ($new_tables as $table) {
    $new_table=$this->str_replace_first($temp_new_prefix,$wpdb->prefix,$table);
    if($wpdb->query("RENAME TABLE {$table} TO {$new_table}")===false) { return $ret; }
}
wp_cache_flush();
```

What about the running `wpvivid_restore_task` option that lives in
`wp_options`? **WPvivid's own state survives the swap by coincidence:**
the option row is being read out of the *new* table from the next
request onward (since the imported dump contained an `wpvivid_restore_task`
row too — same name, the post-rename `wp_options` contains the value
that was in the source dump). But by that point, `rename_db()` has
already finished its inner loop, the restore is **functionally done**,
and the very next AJAX call hits `wpvivid_finish_restore_2` which
*expects* to re-read the task and find the post-swap reality. The
`finish_restore` handler (`restore2.php:1265`) immediately calls
`delete_option('wpvivid_restore_task')` (line 1421), then `wp_cache_flush()`.
The swap window is short enough that nothing material relies on the
pre-restore state.

The implication for WPMgr: **don't bother trying to preserve our own
restore-task row across the swap.** Mark the task as
`status=completed` server-side in a separate store (a row in CP, not in
the target's `wp_options`) *before* the rename happens; treat the WP
option as ephemeral.

### 1.4 Per-statement error handling

`execute_sql()` (`restore-db-2.php:3112`) just delegates to
`$this->db_method->execute_sql($query)`. The driver
(`restore-db-method-2.php:134`) does:

```php
if ($wpdb->get_results($query)===false)
{
    $this->last_error=$wpdb->last_error;
    return false;
}
```

The caller loop continues on `false`:

```php
if ( $this->execute_sql($query,$sub_task)===false)
{
    $this->log->WriteLog($this->db_method->get_last_error(),'notice');
    $query = '';
    continue;
}
```

**Errors are logged and skipped, not aborted.** Statements are one-at-a-
time via `$wpdb` — no `mysqli_multi_query`. Statements are wrapped in
`START TRANSACTION` / `COMMIT` around each per-request batch, but only
to amortize fsync; commit is unconditional even after errors.

### 1.5 CREATE TABLE / DROP TABLE

The dumper emits `DROP TABLE IF EXISTS` then `CREATE TABLE`. Because all
table names are rewritten to `tmp<id>_X`, neither collides with the live
`wp_X`. The `DROP TABLE` path for the temp table is
`restore-db-2.php:1647` `drop_table()`; the CREATE TABLE path is
`create_table()` at `:1207`.

### 1.6 Charset / collation handling

If `restore_detail_options['replace_table_character_set']` is set, the
create-table rewriter looks at the source `ENGINE=`, `CHARSET=`,
`COLLATE=` and replaces with a supported one — fetched at restore start
via `SHOW ENGINES`, `SHOW CHARACTER SET`, `SHOW COLLATION`
(`restore-db-2.php:1102`–`1119`). The replace logic
(`restore-db-2.php:1295`–`1443`) walks the table SUPPORTED list and
falls back to the default charset/collate. Specifically intended for the
utf8mb4 → utf8mb3 migration. No special handling for utf8mb4 to utf8mb4
on MySQL ≥ 8.

`sql_mode` is forced to a permissive value at restore start
(`restore-db-method-2.php:110`):

```php
$temp_sql_mode = str_replace('NO_ENGINE_SUBSTITUTION','',$sql_mod);
$temp_sql_mode = 'ALLOW_INVALID_DATES,NO_AUTO_VALUE_ON_ZERO,'.$temp_sql_mode;
$wpdb->get_results('SET SESSION sql_mode = "'.$temp_sql_mode.'"',ARRAY_A);
```

`ALLOW_INVALID_DATES` and `NO_AUTO_VALUE_ON_ZERO` are exactly the flags
that make legacy WordPress dumps actually replay on a strict-mode MySQL
8.

### 1.7 AUTO_INCREMENT, triggers, views, stored procs

`AUTO_INCREMENT=` is preserved verbatim because it's inside the CREATE
TABLE statement, which is replayed largely as-is (only the table name
and possibly engine/charset are rewritten). The dumper (and therefore
the restorer) does **not** emit or consume triggers, views, stored
procedures, or events — `class-wpvivid-mysqldump2.php` dumps user tables
only. Those object types are silently dropped at backup time and never
restored.

### 1.8 Search-and-replace for siteurl

Two layers. Phase A is the "during-import" rewrite for the bare option
table: after `exec_sql_finished`, `finish_restore_db()`
(`restore-db-2.php:3119`–`3196`) runs *before* the table swap:

```php
$option_table = $this->temp_new_prefix.'options';
$update_query ='UPDATE '.$option_table.' SET option_value="'.$this->new_site_url.'" WHERE option_name="siteurl";';
…
$update_query ='UPDATE '.$option_table.' SET option_value="'.$this->new_home_url.'" WHERE option_name="home";';
```

So `wp_options.siteurl` / `home` are corrected on the *tmp* table.
That's enough for non-migrate restores (where old_url == new_url, the
update is a no-op).

Phase B is the full migration sweep (`is_migrate==true`, triggered when
`old_prefix != new_prefix` OR `old_site_url != new_site_url`, see
`init_restore_db()` at `:1192`). That's `replace_tables_rows()` → `do_replace_row_ex()` → `replace_row_ex()`
which iterates every row of every table whose old-prefix name is in the
WordPress "og table" set
(`restore-db-2.php:3334` `is_og_table()`: posts, postmeta, options,
links, etc.). For each row, every string column is run through
`replace_row_data()` (`restore-db-2.php:2661`):

```php
private function replace_row_data($old_data)
{
    try {
        $unserialize_data = @unserialize($old_data, array('allowed_classes' => false));
        if($unserialize_data===false) {
            $old_data=$this->replace_string_v2($old_data);
        } else {
            $old_data=$this->replace_serialize_data($unserialize_data);
            $old_data=serialize($old_data);
        }
    } catch (Error $error) {
        $old_data=$this->replace_string_v2($old_data);
    }
    return $old_data;
}
```

That's the canonical safe serialized-PHP-aware S&R: try to unserialize,
walk the tree replacing strings, re-serialize. `replace_string_v2()`
(`:2792`) generates replacement pairs for both `http://`, `https://`,
scheme-relative `//`, and JSON-escaped `\/\/` forms, plus the mu-site
upload URL and the bare `old_site_url → new_site_url`.

`replace_rows_pre_request` (default 100,000) bounds rows per AJAX call;
`offset` is persisted per-table in
`$sub_task['exec_sql']['replace_tables'][$table_name]['offset']`.

### 1.9 Migration mode vs Restore mode

There is no separate "Migration" entry point. `is_migrate` is detected
automatically from the dump preamble vs the current site
(`restore-db-2.php:1192`). When false, the heavy `replace_tables_rows()`
sweep is skipped (`:951`–`958`). The post-restore `finish_restore()`
also gates extra work on `is_migrate`: SSL plugin culling, Elementor
cache flush, htaccess regeneration, oxygen/divi warnings.

---

## 2. File restore state machine

### 2.1 Archive provenance and extractor

Same artifact-already-on-disk assumption as DB. The archive may be a
single zip or a "parent zip containing child zips" — the
`unzip_file.files[]` entries carry `has_child` and `parent_file` when
the latter applies, and `restore-file-2.php:45`–`70` extracts the child
out of the parent first.

Extraction is via **PclZip** (the bundled pure-PHP zip library) every
time, not `ZipArchive`:

```php
$archive = new WPvivid_PclZip($file_name);
$zip_ret = $archive->extract(
    WPVIVID_PCLZIP_OPT_PATH, $root_path,
    WPVIVID_PCLZIP_OPT_REPLACE_NEWER,
    WPVIVID_PCLZIP_CB_PRE_EXTRACT, 'wpvivid_function_pre_extract_callback_2',
    WPVIVID_PCLZIP_OPT_TEMP_FILE_THRESHOLD, 16);
```

(`restore-file-2.php:312`–`313`). Two flavours:

- `extract()` — extract everything in one shot;
- `extract_by_index($file_name,$root_path,$start,$start+$N)` —
  per-request chunked, where `$N =
  restore_detail_options['unzip_files_pre_request']`
  (`restore-file-2.php:355` + `:161`).

Which one runs is gated on the user's `use_index` setting; default in
the free version: chunked. See `:130`–`180`.

### 2.2 Stage-then-rename? No. In-place overwrite.

`WPVIVID_PCLZIP_OPT_REPLACE_NEWER` overwrites the destination if the
archived entry is newer (and unconditionally if file timestamps match —
this is a quirk of PclZip, not WPvivid). The unzip target is the *live*
`WP_CONTENT_DIR` / `ABSPATH` / `wp-content/uploads/` etc., depending on
`root_flag`. **There is no staging directory, no atomic rename, no
rollback snapshot.** The `WPVIVID_DEFAULT_ROLLBACK_DIR =
'wpvivid-old-files'` constant (`main.php:67`) is defined but not
referenced anywhere in `restore-file-2.php`.

If the AJAX request times out mid-extraction, the `unzip_file.start`
cursor is persisted and the next request resumes at `start+N` — but
the files unzipped so far are already overwritten. Live site is
half-merged until the loop finishes.

### 2.3 Conflict handling, permissions, symlinks

Files with the same name as live ones are silently overwritten (the
PclZip flag). No checksum, no diff, no backup of the replaced file.

Unix permissions on extracted files come from PclZip's defaults, which
use the umask of the PHP-FPM user — **archive-time mode bits are not
preserved**.

Symlinks: PclZip can't even emit symlinks; the backup-side dumper just
emits regular file entries for whatever the symlink points to (or the
symlink is skipped at backup, depending on PHP version). On restore,
nothing in `restore-file-2.php` distinguishes a symlink target from a
file.

### 2.4 Path traversal defense

PclZip uses `dirname()`/`basename()` cleansing internally
(`includes/zip/class-wpvivid-pclzip.php`, not quoted here — it's the
unmodified upstream PclZip). On top of that, the
`wpvivid_function_pre_extract_callback_2` hook
(`restore-file-2.php:644`) returns `0` (skip) for known dangerous paths:

```php
if(strpos($p_header['filename'], $content_path.'advanced-cache.php')!==false) return 0;
if(strpos($p_header['filename'], $content_path.'db.php')!==false) return 0;
if(strpos($p_header['filename'], $content_path.'object-cache.php')!==false) return 0;
if(strpos($p_header['filename'],$plugins.'/wpvivid-backuprestore')!==false) return 0;
if(strpos($p_header['filename'],'wp-config.php')!==false) return 0;
if(strpos($p_header['filename'],'wpvivid_package_info.json')!==false) return 0;
if(strpos($p_header['filename'],'.htaccess')!==false) return 0;   // unless restore_htaccess
if(strpos($p_header['filename'],'.user.ini')!==false) return 0;
if(strpos($p_header['filename'],'wordfence-waf.php')!==false) return 0;
```

So WPvivid **explicitly skips** `wp-config.php`, `.htaccess` (default),
`.user.ini`, drop-ins (`db.php`, `object-cache.php`,
`advanced-cache.php`), the wpvivid plugin itself, and Wordfence's WAF
bootstrap. That's the "keep the site running" exclusion list. It's
substring-based though, so a file named `notwp-config.php` would also
be skipped — false positive, not a security hole.

### 2.5 Resume across PHP requests

Yes. The per-archive `index` (file-within-zip cursor) is saved into
`sub_task['unzip_file']['files'][$index]['index']`
(`restore-file-2.php:167`):

```php
$sub_task['unzip_file']['files'][$index]['index']=$start+$unzip_files_pre_request;
…
if($start+$unzip_files_pre_request>=$sum) {
    $sub_task['unzip_file']['files'][$index]['finished']=1;
}
```

Next AJAX call reads `index` and continues from there. So the **file
restore checkpoints per-file-within-zip**, just like the backup side
checkpoints per-file-being-archived.

### 2.6 PHP/OS mismatch

WPvivid doesn't gate on it. A backup taken on PHP 8.2 / Linux can be
restored on PHP 7.4 / Windows; the failure mode is whatever PHP says
when running the restored code. There's no `requires_php` check.

### 2.7 The `restore_reset` ("clean before restore") option

If `restore_reset=true` the destination folder is wiped before
extraction (`reset_restore()` at `:437`):

- `themes` → `delete_theme()` every installed theme
- `plugin` → `_delete_plugins(array_keys(get_plugins()))` (sparing
  `wpvivid-backuprestore` and `wpvivid-backup-pro`)
- `upload` → recursive delete of upload dir
- `wp-content` → delete every top-level subdir except a whitelist
  (`mu-plugins`, `plugins`, `themes`, `uploads`, the backup dir
  itself)
- `wp-core` → delete every entry in WP's `$_old_files` array

Recursive delete uses `scandir()` + `wp_delete_file()` / `rmdir()`,
single-threaded. No retry, no error reporting beyond log lines.

---

## 3. Async + progress for restore

### 3.1 Cadence and payload

Different from backup. Restore is **browser-driven AJAX chunking**:
`wpvivid_do_restore_2` does one chunk per call, returns 200 OK
immediately, the UI polls `wpvivid_get_restore_progress_2` every 2 s
(`restore-page-display.php:1602`–`1612`):

```js
else if(jsonarray.status=='doing sub task') {
    setTimeout(function(){ wpvivid_get_restore_progress(); }, 2000);
}
else if(jsonarray.status=='no response') {
    setTimeout(function(){ wpvivid_get_restore_progress(); }, 2000);
}
```

When progress returns `status='sub task finished'`, the UI fires another
`wpvivid_do_restore_2`. So the actual work is split across many short
PHP requests; the *browser* drives the resume.

Progress payload (`restore2.php:906`–`1242`) is JSON:

```php
$ret['result']='success';
$ret['status']='doing sub task' | 'sub task finished' | 'task finished' | 'no response' | 'ready';
$ret['do_sub_task']=$sub_task['type'];
$ret['main_msg']='doing restore '.$sub_task['type'];
$ret['sub_tasks_progress'][$progress_id]    = '...html...';
$ret['sub_tasks_progress_detail'][$progress_detail_id]= ['html'=>$last_msg,'show'=>bool];
$ret['main_task_progress_total']  = count($sub_tasks);
$ret['main_task_progress_finished']= $count_finished;
$ret['main_progress']='<span class="action-progress-bar-percent ..." style="width:N%; ...">N% completed</span>';
$ret['log']=tail-of-log-file (capped at 100 KB);
```

DB sub-task percent is computed as
`(sum sql_offset) / (sum sql_file_size) * 100` (or `*50` plus
`replace_tables` percent if `is_migrate`). File sub-task percent is
`(files_finished/files_total)*100 + (start/sum)/files_total*100`.

### 3.2 Cancel

There is no "cancel" button. The page-level "Restore failed" path
(`wpvivid_restore_failed_2` → `restore_failed()` at `:1501`) is only
fired when the JS sees `result=failed` on a progress call. If the
operator closes the tab mid-flight, the last `do_restore` request
finishes its chunk, persists state, and… nothing pulls the next chunk.
The site is left in maintenance mode with the temp tables present.

### 3.3 PHP dies mid-restore

`do_restore()` (`restore2.php:604`) registers
`deal_restore_shutdown_error` which, on memory exhaustion, marks
`status=error` + `error_memory_limit=true`. The UI sees the error and
calls `wpvivid_restore_failed_2`. There is no resume — the user must
restart.

On a *timeout* (not fatal), the per-progress-call check
(`restore2.php:987`):

```php
if(time()-$restore_task['update_time']>$restore_max_execution_time) {
    $restore_task['restore_timeout_count']++;
    update_option('wpvivid_restore_task',$restore_task,'no');
    if($restore_task['restore_timeout_count']>6) {
        $ret['result']='failed';
        $ret['error']='restore timeout';
    } else {
        $ret['status']='sub task finished'; // trick UI into firing do_restore again
    }
}
```

So up to 6 forced-resumes per stuck sub-task. After that, the option is
left in place but `wpvivid_restore_failed_2` is fired by the UI, which
deletes the option after running `delete_temp_tables` + `delete_temp_files`.

There is **no wp-cron watchdog for restore** — `task_monitor` is
backup-only.

---

## 4. State machine + resume

### 4.1 Option schema

`option_name = 'wpvivid_restore_task'`, autoload=no
(`restore2.php:303`). Schema:

```php
$restore_task = [
  'backup_id'      => 'wpvivid-xxxx',
  'restore_options'=> [...],
  'update_time'    => (unix ts of last persist; used as liveness),
  'restore_timeout_count' => 0..6,
  'is_migrate'     => bool,
  'sub_tasks'      => [
    [
      'type'        => 'themes'|'plugin'|'wp-content'|'upload'|'wp-core'|'custom'|'databases',
      'priority'    => 1..8,
      'options'     => [...],
      'restore_reset'=> bool,
      'restore_reset_finished' => bool,
      'finished'    => 0|1,
      'last_msg'    => 'human string',
      'unzip_file'  => [
        'files'       => [ ['file_name'=>..., 'finished'=>0|1, 'index'=>N, 'has_child'=>?, 'parent_file'=>?, 'options'=>... ], ... ],
        'unzip_finished' => 0|1,
        'last_action' => 'waiting...' | 'Unzipping',
        'last_unzip_file' => '',
        'last_unzip_file_index' => 0,
        'sum'         => zip_entry_count,
        'start'       => zip_entry_cursor,
      ],
      // for databases sub_task only:
      'exec_sql'    => [
        'db_id'                 => uniqid (3-char), gives 'tmp<id>_' prefix
        'sql_files'             => [name => ['sql_file_name', 'sql_file_size', 'sql_offset', 'finished']],
        'init_sql_finished'     => 0|1,
        'create_snapshot_finished' => 0|1,
        'exec_sql_finished'     => 0|1,
        'replace_rows_finished' => 0|1,
        'current_table'         => 'tmpXXX_posts',
        'current_old_table'     => 'wp_posts',
        'replace_tables'        => [name => ['current_table','current_old_table','finished','offset']],
        'last_action'           => 'Importing',
        'last_query'            => '...last SQL...',
      ],
      'db_info'     => [ 'default_engine','default_charsets','default_collates',
                         'base_prefix','new_prefix','temp_new_prefix',
                         'old_site_url','new_site_url','old_home_url','new_home_url',
                         'old_content_url','new_content_url','old_upload_url','new_upload_url',
                         'old_prefix','is_migrate', ... ],
    ],
    ...sorted by priority...
  ],
  'do_sub_task'    => index|false,
  'status'         => 'ready'|'doing sub task'|'sub task finished'|'error',
  'restore_detail_options' => [ 'restore_memory_limit','restore_max_execution_time',
                                'sql_file_buffer_pre_request','replace_rows_pre_request',
                                'unzip_files_pre_request','use_index',
                                'max_allowed_packet','replace_table_character_set',
                                'restore_db_reset','db_connect_method','restore_htaccess' ],
  'log'            => 'absolute path to log file',
  'last_log'       => 'short string',
  'error'          => 'msg, if any',
  'error_memory_limit' => bool,
];
```

### 4.2 Phases

Hard-coded priority order (`restore2.php:148`–`240`): `themes(1)` →
`plugin(2)` → `wp-content(3)` → `upload(4)` → `wp-core(5)` →
`custom(6)` → `additional(7)` → `databases(8)`. DB is **last** so that
all the plugin/theme code is on disk by the time it loads. Within the
DB sub-task, the inner phase order is fixed by flag-flipping order:
`unzip_finished` → `init_sql_finished` → `exec_sql_finished` →
`replace_rows_finished` → (rename_db happens later in `finish_restore`,
not in `do_restore` loop).

### 4.3 Persistence cadence

Every meaningful state change calls `update_sub_task()` →
`update_option('wpvivid_restore_task', …)`. Inside the inner SQL loop,
that's every 100 KB of consumed file (`restore-db-2.php:578`):

```php
if($sub_task['exec_sql']['sql_files'][$sql_file_name]['sql_offset']-$progress_offset>1024*100) {
    $progress_offset=$sub_task['exec_sql']['sql_files'][$sql_file_name]['sql_offset'];
    $this->update_sub_task($sub_task);
}
```

Plus once per yield-out (when the per-request buffer ceiling is hit).
File extraction persists once per chunk of `unzip_files_pre_request`
files.

---

## 5. Failure / rollback

### 5.1 DB failure mid-import

Caught at the per-statement level: errors are logged and the loop
continues — so a single bad row doesn't abort. But on `restore_failed`
(triggered by JS when `result=failed`), the handler
(`restore2.php:1501`) calls `delete_temp_tables()` which iterates the
`databases` sub-task and calls `WPvivid_Restore_DB_2::remove_tmp_table()`
(`restore-db-2.php:365`):

```php
$temp_new_prefix='tmp'.$sub_task['exec_sql']['db_id'].'_';
$tables = $wpdb->get_col($wpdb->prepare('SHOW TABLES LIKE %s', array($temp_new_prefix . '%')));
foreach ($tables as $table) {
    $wpdb->query('DROP TABLE IF EXISTS `' . $table.'`');
}
```

So orphan `tmp<id>_*` tables get cleaned up *if* the user clicks
through "restore failed" UI. If they don't (e.g. they close the tab and
re-run), the orphan tables linger — the next restore uses a *different*
`db_id` so they don't clash, but they consume disk and never get GC'd.

### 5.2 File failure mid-extract

Live site is half-merged. No staging dir to delete, no replaced-files
backup to restore. The `restore_failed` handler does **not** roll files
back; it only deletes temp SQL files and runs `delete_temp_tables()`.
The operator's site is broken until they restore again, restore from
host backup, or `wp-cli core download` + manual fix.

### 5.3 Preflight checks

Almost none. `init_restore_task` checks the backup exists in
`WPvivid_Backuplist`. No disk space check, no archive integrity check,
no DB connectivity test, no plugin/theme version match warning, no
PHP/WP version check. The free version's only refusal is "encrypted
database — pay for Pro" (`restore-db-2.php:142`).

---

## 6. Safety / preflight summary

| Check | WPvivid? |
|---|---|
| Disk space before extract | No |
| DB connectivity / credentials | No (errors surface at first `wpdb` call) |
| Plugin/theme version mismatch | No |
| WordPress version mismatch | No |
| ZIP archive integrity (CRC) | Partial — PclZip CRCs are checked per-entry but no top-level prevalidate |
| "Are you sure?" enforced server-side | No — the `confirm()` is client-only |

The "are you sure" prompt at `wpvivid_start_restore`
(`restore-page-display.php:1463`) is a JS `confirm()`. Server-side,
`init_restore_task` will happily run again on the same backup with no
cooldown.

---

## 7. Recommended adapter design for WPMgr's ADR-033 backup shape

Our backup shape (no encryption, `.sql.gz` for DB, `wp-content.partNNN.zip`
for files, per-part manifest entries) means most of WPvivid's complexity
falls away on the artifact side. Carry forward only what fights MySQL/PHP.

### 7.1 What to keep from WPvivid

1. **Temp-prefix + atomic `RENAME TABLE` swap.** Per-table rename under
   `SET FOREIGN_KEY_CHECKS=0;` is the right pattern. Generate the temp
   prefix at restore-task start, persist it, use it for every CREATE /
   INSERT / DROP. Swap last.
2. **`SET SESSION sql_mode = 'ALLOW_INVALID_DATES,NO_AUTO_VALUE_ON_ZERO,...'`.**
   This is the one-line trick that makes 5+ year old dumps replay on
   strict-mode MySQL 8.
3. **Permissive per-statement error handling** (log, don't abort) — but
   surface a "N statements failed" count in the manifest so the operator
   can decide whether to redo.
4. **Serialized-PHP-aware S&R for migration.** Lift `replace_row_data()`
   pattern (try `unserialize` with `allowed_classes=false`, walk tree
   replacing strings, re-serialize) — never naive string replace, ever.
5. **Per-request buffer ceilings.** `sql_file_buffer_pre_request` (MB),
   `unzip_files_pre_request` (count), `replace_rows_pre_request`
   (rows). Persist cursor; resume from cursor on next chunk.
6. **Exclusion list for files.** Skip `wp-config.php`, drop-ins,
   `.htaccess` (configurable), `.user.ini`, our own agent code path,
   and any active security WAF bootstrap. Done in a pre-extract hook,
   not after.
7. **Maintenance mode.** Drop a `.maintenance` file with the
   `enable_maintenance_mode` filter at the top of the restore, delete
   on success or failure handler. Use the same trick (POST flag) to
   allow the agent's own AJAX through.
8. **Sub-task priority ordering.** Files (themes → plugins → wp-content
   → uploads → wp-core) before DB. Always.

### 7.2 What to fix from WPvivid

1. **Stage-then-rename for files, with rollback snapshot.** Unzip the
   `.partNNN.zip` files into `wp-content/.wpmgr-staging-<run_id>/`.
   When the staging tree is complete, do a directory-subtree atomic
   move (`rename(staging/themes/X, themes/X)` per top-level entry)
   and *first* move the existing `themes/X` to
   `wp-content/.wpmgr-old-files-<run_id>/themes/X`. Keep the old-files
   tree for 24 h. Single-call rollback = swap the trees back.
2. **Preflight phase.** Before any destructive work:
   `df` on `WP_CONTENT_DIR` vs sum of artifact sizes × 2;
   `mysql_ping`; `SHOW VARIABLES LIKE 'max_allowed_packet'` ≥ 32M;
   ZIP CRC pre-scan; sentinel file in target with `wpmgr-restore-lock`.
   Fail loudly if any preflight fails — *before* maintenance mode.
3. **CP-side state, not target's `wp_options`.** Persist the task
   record on CP via the agent's existing TaskRunner. The target only
   gets the resume-cursor row, scoped to a single key.
4. **Watchdog for restore too.** A wp-cron `wpvivid_task_monitor`-style
   event + a CP-side timeout monitor. Restore is more dangerous, not
   less.
5. **Cancel button.** Server-side mark `cancelled=true`, the next
   chunk loop honors it, runs rollback, exits maintenance mode.
6. **Use `ZipArchive`, not PclZip.** PHP ≥ 7.2 ships it; faster, and it
   supports `extractTo($dir, ['entry1','entry2'])` for per-chunk
   extract that doesn't need PclZip's index math. Fall back to PclZip
   only if `class_exists('ZipArchive')` is false.

### 7.3 Recommended phase enum

```ts
enum RestorePhase {
  PREFLIGHT          = 'preflight',        // disk, DB, archive CRC, lock
  DOWNLOAD_ARTIFACTS = 'download_artifacts',// pull .sql.gz + .partNNN.zip from S3
  VERIFY_ARTIFACTS   = 'verify_artifacts',  // checksum vs manifest
  MAINTENANCE_ON     = 'maintenance_on',    // .maintenance file
  STAGE_FILES        = 'stage_files',       // unzip parts → .wpmgr-staging-<id>/
  SWAP_FILES         = 'swap_files',        // atomic dir-subtree rename, old → .wpmgr-old-files-<id>/
  RESTORE_DB         = 'restore_db',        // gunzip + replay into tmp<id>_ prefix
  MIGRATE_DB         = 'migrate_db',        // serialized-aware search/replace (skipped if no URL change)
  SWAP_DB            = 'swap_db',           // DROP TABLE wp_X; RENAME TABLE tmp_X TO wp_X
  POST_HOOKS         = 'post_hooks',        // flush_rewrite_rules, wp_cache_flush, opcache_reset
  MAINTENANCE_OFF    = 'maintenance_off',   // delete .maintenance
  CLEANUP            = 'cleanup',           // drop tmp_ tables, schedule old-files GC
  COMPLETED          = 'completed',
  FAILED             = 'failed',
  ROLLED_BACK        = 'rolled_back',
}
```

### 7.4 Recommended persistence shape (agent-side)

```json
{
  "run_id": "wpmgr-restore-<uuid>",
  "backup_id": "wpmgr-backup-<uuid>",
  "phase": "restore_db",
  "phase_started_at": 1716950000,
  "phase_updated_at": 1716950042,
  "tmp_prefix": "tmp_<short>_",
  "old_files_dir": "wp-content/.wpmgr-old-files-<run_id>",
  "staging_dir": "wp-content/.wpmgr-staging-<run_id>",
  "artifacts": {
    "db": { "path": "...db.sql.gz", "size": 12345678, "sha256": "...", "offset": 4321000 },
    "files": [
      { "path": "...part001.zip", "size": ..., "sha256": "...", "entries_total": 2380, "entries_done": 2380, "finished": true },
      { "path": "...part002.zip", "size": ..., "sha256": "...", "entries_total": 2410, "entries_done": 812,  "finished": false }
    ]
  },
  "migrate": { "old_url": "https://staging.x.test", "new_url": "https://x.com", "tables_total": 47, "tables_done": 12, "current_table": "wp_postmeta", "current_offset": 50000 },
  "errors": [],
  "cancelled": false
}
```

### 7.5 SSE shape

Mirror the backup SSE event names and add per-phase byte/entry counters.
One event type, multiple records:

```
event: progress
data: {"phase":"restore_db","percent":47,"bytes_done":4321000,"bytes_total":12345678,"current_table":"wp_posts","tables_done":8,"tables_total":47,"eta_s":120}
```

`phase` is the closed enum above. Done.

---

### Source line index (post-fetch, may drift on next trunk update)

- `restore2.php:36` `init_restore_task`
- `restore2.php:117` `create_restore_task` (sub_task schema)
- `restore2.php:604` `do_restore`
- `restore2.php:704` `do_sub_task` (dispatcher)
- `restore2.php:804` `_enable_maintenance_mode`
- `restore2.php:906` `get_restore_progress`
- `restore2.php:1265` `finish_restore`
- `restore2.php:1501` `restore_failed`
- `restore2.php:1600` `delete_temp_tables`
- `restore-db-2.php:54` `restore`
- `restore-db-2.php:365` `remove_tmp_table`
- `restore-db-2.php:378` `exec_sql` (the main SQL loop)
- `restore-db-2.php:1002` `init_restore_db`
- `restore-db-2.php:1207` `create_table`
- `restore-db-2.php:2223` `replace_row`
- `restore-db-2.php:2661` `replace_row_data`
- `restore-db-2.php:3119` `finish_restore_db`
- `restore-db-2.php:3198` `rename_db`
- `restore-db-method-2.php:110` `init_sql_mode`
- `restore-db-method-2.php:134` `execute_sql`
- `restore-file-2.php:17` `restore`
- `restore-file-2.php:299` `extract`
- `restore-file-2.php:355` `extract_by_index`
- `restore-file-2.php:437` `reset_restore`
- `restore-file-2.php:644` `wpvivid_function_pre_extract_callback_2`
- `restore-page-display.php:1515` `wpvivid_init_restore` (JS)
- `restore-page-display.php:1557` `wpvivid_do_restore` (JS)
- `restore-page-display.php:1574` `wpvivid_get_restore_progress` (JS, 2 s polling)
- `restore-page-display.php:1633` `wpvivid_finish_restore` (JS)
- `restore-page-display.php:1671` `wpvivid_restore_failed` (JS)
- `wpvivid-backuprestore.php:42` `WPVIVID_RESTORE_MAX_EXECUTION_TIME = 300`
- `wpvivid-backuprestore.php:44` `WPVIVID_RESTORE_MEMORY_LIMIT = 512M`
- `wpvivid-backuprestore.php:67` `WPVIVID_DEFAULT_ROLLBACK_DIR = 'wpvivid-old-files'` (defined, **unused on trunk**)
