# WPvivid Backup & Migration — async execution, progress, restore

Source: WP.org SVN trunk
<https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/>. No public
GitHub mirror. Excerpts below are verbatim.

---

## 1. Async execution model — "fire, flush, self-resume"

WPvivid does **not** spawn a detached subprocess (no `proc_open`/`nohup`)
and doesn't use Action Scheduler. The model is a hybrid:

1. Browser POSTs `wpvivid_prepare_backup` → server creates the task.
2. Browser POSTs `wpvivid_backup_now` → server **answers immediately**,
   then keeps running the same PHP request to do the actual work while
   the browser polls.
3. If that long request dies (FPM kill, OOM, timeout), a
   `wp_schedule_single_event` watchdog re-fires the work as a wp-cron event.

The "answer immediately, keep working" trick is in `flush()` in
`includes/class-wpvivid.php`:

```php
public function flush($txt, $from_mainwp=false) {
    ...
    if(!headers_sent()){
        header('Content-Length: '.( ( ! empty( $txt ) ) ? strlen( $txt ) : '0' ));
        header('Connection: close');
        header('Content-Encoding: none');
    }
    if (session_id()) session_write_close();
    echo wp_json_encode($ret);
    if(function_exists('fastcgi_finish_request')) {
        fastcgi_finish_request();
    } else {
        if(ob_get_level()>0) ob_flush();
        flush();
    }
}
```

Then `backup_now()` invokes `$this->backup($task_id)`, which calls
`@ignore_user_abort(true);` so the worker keeps running after the socket is
released. **If the user closes the tab, the backup keeps going** — the
browser was never driving execution, only polling. (Source:
`includes/class-wpvivid.php` `backup_now`, `backup`.)

## 2. State machine + persistence

Task state lives in a WordPress **option** named `wpvivid_task_list`
(accessed via `WPvivid_Setting::get_tasks()` / `update_task()`,
`includes/class-wpvivid-taskmanager.php`). Schema:

```php
$task['id']             // 'wpvivid-' . uniqid
$task['action']         // 'backup'
$task['type']           // 'Manual' | 'Cron'
$task['status']['start_time']    $task['status']['run_time']
$task['status']['timeout']       $task['status']['str']    // ready|running|wait_resume|no_responds|completed|error
$task['status']['resume_count']  $task['status']['error']
$task['options']        // user-selected + log_file_name + file_prefix
$task['data']['doing']                  // 'backup' | 'upload'
$task['data']['backup']['sub_job'][...] // chunked work items, each w/ finished/progress
$task['data']['upload'][...]
```

Phase advance is a per-sub-job `finished` flag plus the top-level
`data['doing']`. The state record itself is the checkpoint — there's no
separate journal.

## 3. Progress reporting

The admin JS polls **every 3 s** (`admin/js/wpvivid-admin.js`):

```js
function wpvivid_manage_task() {
    if(m_need_update === true){ m_need_update = false; wpvivid_check_runningtask(); }
    else setTimeout(function(){ wpvivid_manage_task(); }, 3000);
}
```

The poll hits AJAX action `wpvivid_list_tasks`. The handler
(`includes/class-wpvivid.php` line 4532) returns the task plus a
**server-rendered HTML fragment** (`progress_html`) containing the percent
bar, "Total Size / Uploaded / Speed / Network Connection / Current doing"
text and Cancel/Log buttons. Progress is byte-weighted per phase
(`db_size`, `files_size['sum']`, `backup_percent`). For restore, the JS
polls action `wpvivid_get_restore_progress_2` **every 2 s**; payload:

```php
$ret['main_progress']='<span...>'.$main_progress.'% completed</span>';
$ret['sub_tasks_progress'][$id]      = $sub_task_progress;
$ret['sub_tasks_progress_detail'][$id]= ['html'=>$sub_task['last_msg'], ...];
$ret['log'] = $buffer; // tail of log
```

## 4. Heartbeat / liveness — `task_monitor`

When `backup()` starts it schedules a wp-cron watchdog 120 s out via
`add_monitor_event($task_id, 120)`. The watchdog (`task_monitor`,
`includes/class-wpvivid.php`) compares `time() - status.timeout` against
`WPVIVID_MAX_EXECUTION_TIME` (300 s). If both the budget AND
180 s of last-active inactivity are exceeded:

```php
$status['resume_count']++;
if($status['resume_count']>$max_resume_count) { /* mark error */ }
else {
    if($this->add_resume_event($task_id))
        WPvivid_taskmanager::update_backup_task_status($task_id,false,'wait_resume',...);
}
```

`add_resume_event()` does `wp_schedule_single_event(time()+60,
WPVIVID_RESUME_SCHEDULE_EVENT, [$task_id])`. The resume handler is
`resume_schedule($task_id)` which inspects `data.doing` and **re-enters
either `backup()` or `upload()` from the persisted checkpoint** — no
restart from scratch. Up to `WPVIVID_RESUME_RETRY_TIMES = 6` attempts.

A `register_shutdown_function(deal_shutdown_error)` adds belt-and-braces:
if PHP dies fatally inside the worker, the shutdown hook also schedules a
resume event before the process exits. Constants
(`wpvivid-backuprestore.php`): `WPVIVID_RESUME_INTERVAL=60`,
`WPVIVID_MAX_EXECUTION_TIME=300`, `WPVIVID_RESUME_RETRY_TIMES=6`,
`WPVIVID_RESUME_SCHEDULE_EVENT='wpvivid_resume_schedule_event'`,
`WPVIVID_TASK_MONITOR_EVENT='wpvivid_task_monitor_event'`.

## 5. Concurrency

`backup_now()` short-circuits via `WPvivid_taskmanager::is_tasks_backup_running()`:

```php
if (WPvivid_taskmanager::is_tasks_backup_running()) {
    $ret['result']='failed';
    $ret['error']=__('A task is already running...','wpvivid-backuprestore');
    echo wp_json_encode($ret); die();
}
```

`is_tasks_backup_running()` iterates `wpvivid_task_list` and returns true
if any task has `status.str` ∈ {`running`,`no_responds`}. Refusal model,
not queue.

## 6. Backup ID / artifact layout

ID = `'wpvivid-'.uniqid()`. Files land in
`WP_CONTENT_DIR/wpvividbackups/` (`WPVIVID_DEFAULT_BACKUP_DIR =
'wpvividbackups'`, override via `WPvivid_Setting::get_backupdir()`):

```php
$backup_data['path']=WP_CONTENT_DIR.DIRECTORY_SEPARATOR.
    $this->task['options']['backup_options']['dir'].DIRECTORY_SEPARATOR;
$this->task['options']['file_prefix'] = $backup_prefix.'_'.$this->task['id']
    .'_'.gmdate('Y-m-d-H-i', $this->task['status']['start_time']+$offset*60*60);
```

One flat directory, files named `<sitename>_wpvivid-<uniqid>_<UTC-stamp>_*`.
Logs go to `wpvividbackups/wpvivid_log/`.

## 7. Remote upload (S3/FTP/SFTP/…)

**Post-backup**, never inline. In `backup()`:

```php
if(WPvivid_taskmanager::get_task_options($task_id,'remote_options')!=false) {
    $this->upload($task_id,false);
}
```

`upload()` is another `ignore_user_abort()` step under the same monitor
watchdog. Retries are per-provider (`WPVIVID_REMOTE_CONNECT_RETRY_TIMES=3`,
`_INTERVAL=3`). S3/FTP classes stream from disk in 4 KB reads; the
Plupload uploader path uses chunked POST with `max_retries=3`.

## 8. RESTORE — the careful part

Frontend handler chain
(`admin/partials/wpvivid-backup-restore-page-display.php`,
`includes/new_backup/class-wpvivid-restore2.php`):

`wpvivid_init_restore_page` → `wpvivid_init_restore_task_2` (builds the
restore task and persists to option `wpvivid_restore_task`) →
`wpvivid_do_restore_2` (runs ONE sub-task per AJAX call) →
`wpvivid_get_restore_progress_2` (polled every 2 s) →
`wpvivid_finish_restore_2`. **Restore is browser-orchestrated AJAX
chunking** — the opposite of backup. Each `do_restore` call does as much
work as it can in one PHP request, persists state, returns.

Restore-task schema (option `wpvivid_restore_task`):

```php
$restore_task['backup_id'] $restore_task['restore_options']
$restore_task['sub_tasks']       // priority 1..8: themes/plugins/wp-content/upload/wp-core/custom/databases
$restore_task['do_sub_task']     // current sub index
$restore_task['status']          // ready|doing sub task|sub task finished|error
$restore_task['log']
```

Each sub_task tracks `unzip_file.files[]`, `unzip_file.unzip_finished`,
`finished`.

### DB chicken-and-egg: temp-prefix + atomic RENAME TABLE

WPvivid imports the dump into a **separate temporary prefix**
`tmp<db_id>_` (so WordPress keeps running on the live `wp_*` prefix
while the dump streams in). After insert is complete AND
`finish_restore_db` has fixed up `siteurl`/`home` in
`tmp<db_id>_options`, `rename_db()` does the swap
(`includes/new_backup/class-wpvivid-restore-db-2.php` line 3198):

```php
public function rename_db($sub_task) {
    global $wpdb;
    $wpdb->query('SET FOREIGN_KEY_CHECKS=0;');
    ...
    $temp_new_prefix='tmp'.$sub_task['exec_sql']['db_id'].'_';
    $tables = $wpdb->get_results('SHOW TABLE STATUS');
    ...
    foreach ($new_tables as $table) {
        $new_table=$this->str_replace_first($temp_new_prefix,$wpdb->prefix,$table);
        if($wpdb->query('DROP TABLE IF EXISTS ' . $new_table)===false) { ... return $ret; }
    }
    foreach ($new_tables as $table) {
        $new_table=$this->str_replace_first($temp_new_prefix,$wpdb->prefix,$table);
        if($wpdb->query("RENAME TABLE {$table} TO {$new_table}")===false) { ... }
    }
    wp_cache_flush();
```

So: drop existing `wp_*` table, `RENAME TABLE tmp..._x TO wp_x`. No
`db.php` drop-in, no transaction. Maintenance mode is enabled before the
restore loop (`_enable_maintenance_mode()` writes
`ABSPATH/.maintenance`) and disabled in `finish_restore()`.

### File restore: in-place overwrite, no rollback

`class-wpvivid-restore-file-2.php` extracts archives directly into the
target with `WPVIVID_PCLZIP_OPT_REPLACE_NEWER`. It **does** chunk
(`extract_by_index($file_name, $root_path, $start, $start+$unzip_files_pre_request)`)
so each AJAX call only unzips N files, persisting progress in
`unzip_file.last_action`. But there is **no staging dir, no atomic
rename, no rollback** — the `WPVIVID_DEFAULT_ROLLBACK_DIR =
'wpvivid-old-files'` constant exists in the main plugin file but is not
referenced by the restore-2 code path on trunk. A mid-flight file
failure leaves the site half-written; only DB swap is atomic per-table.

`finish_restore` runs cleanup: disable maintenance mode, call
`check_restore_db` → `rename_db`, delete unzipped temp SQL files,
`activate_plugins($current,'',false,true)`, `delete_option('wpvivid_restore_task')`.

## 9. Cron / scheduled backups

Stored in option `wpvivid_schedule_setting` (keys: `enable`, `type`,
`event`, `start_time`, `backup{...}`). Registered as a recurring wp-cron
event via `wp_schedule_event($schedule_data['start_time'],
$schedule_data['type'], $schedule_data['event'])` (hook
`WPVIVID_MAIN_SCHEDULE_EVENT = 'wpvivid_main_schedule_event'`). The hook
runs `main_schedule()`, which is just `pre_backup()` → `flush()` →
`backup()` — the same async pipeline as a manual click. **No external
system cron required**, but as everyone knows wp-cron only fires on page
hits, so quiet sites can drift.

## 10. What WPMgr can learn (M5.6 / ADR-032)

WPvivid validates fire-and-detach but the implementation details refine it:

- **Validates `proc_open`.** WPvivid's `flush() + fastcgi_finish_request() +
  ignore_user_abort()` is the in-WP equivalent. Your `proc_open` is cleaner
  because it survives FPM worker recycling. Keep it.
- **Watchdog regardless.** Copy `task_monitor`: persist `(start_time,
  last_heartbeat, resume_count)`; have CP declare stalled if heartbeat
  >180 s and resume from checkpoint. A subprocess can still SIGKILL on OOM.
- **Checkpoint per sub-job, not per phase.** WPvivid's `sub_job` array with
  per-item `finished/progress` makes resume cheap. Mirror it in phpbu: each
  DB-dump-N, archive-chunk-N, upload-chunk-N persisted before advancing.
- **Progress UX.** WPvivid ships server-rendered `progress_html`. Do
  better — return JSON `{phase, percent, bytes_done, bytes_total,
  current_file, eta_s}` and render in the dashboard. 3 s backup / 2 s
  restore polling intervals are sensible defaults; consider SSE later.
- **Concurrent-click: refuse, don't queue.** WPvivid's
  `is_tasks_backup_running()` returns a polite error. CP should hold a
  per-`site_id` advisory lock (row in `backup_runs` with status IN
  ('queued','running')) and reject duplicates server-side.
- **Split orchestration by direction.** WPvivid runs backup detached but
  restore browser-driven AJAX (one chunk per HTTP call, 2 s poll). Mirror
  that: restore is dangerous, keep the operator's browser holding the
  rope — easier to cancel than killing a detached subprocess.
- **Adopt their DB trick, fix their file weakness.** The atomic-per-table
  `RENAME TABLE tmp_<id>_x TO wp_x` after temp-prefix import is the right
  DB pattern. But WPvivid's file restore overwrites in place with **no
  rollback**. For WPMgr, stage-then-rename: unzip into
  `wp-content/.wpmgr-staging-<id>/`, atomically rename directory subtrees
  onto live, retain replaced files in `wpmgr-old-files-<id>/` for 24 h
  for one-call rollback. Maintenance mode (`.maintenance`) throughout,
  same as WPvivid.

---

### Source URLs

- Main orchestration / AJAX hooks / `backup_now` / `task_monitor` /
  `flush` / `list_tasks`:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid.php>
- Constants:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/wpvivid-backuprestore.php>
- Task state schema:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-taskmanager.php>
- Schedule:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-schedule.php>
- Backup engine (paths, sub_jobs):
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-backup.php>
- Upload chunking:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-backup-uploader.php>
- Restore orchestration (`init`/`do`/`progress`/`finish`):
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore2.php>
- DB restore + `rename_db` swap:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore-db-2.php>
- File restore (in-place unzip, chunked):
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/new_backup/class-wpvivid-restore-file-2.php>
- Frontend poll loop and intervals:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/admin/js/wpvivid-admin.js>
- Restore UI handlers:
  <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/admin/partials/wpvivid-backup-restore-page-display.php>
