# WPvivid Backup & Migration — Database Backup Internals

Research target: `wpvivid-backuprestore` (WordPress.org plugin).
Source root: <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/>
No GitHub mirror exists; SVN is canonical.

## 1. Does WPvivid require `mysqldump`?

**No.** WPvivid never shells out. It is a 100% pure-PHP dumper. The DB-export entry point is `includes/class-wpvivid-mysqldump.php`, which defines `class WPvivid_Mysqldump`. A grep of the file finds zero calls to `exec`, `shell_exec`, `popen`, `passthru`, or `proc_open`; the only query path is the adapter call `$this->typeAdapter->query($query_string)` (line ~1044), which executes through either PDO or `$wpdb`. The companion file `class-wpvivid-mysqldump-method.php` provides the type-adapter factory (`TypeAdapterMysql`, plus stubs for SQLite/PostgreSQL/DBLIB) and a `WPvividCompressManagerFactory`; it likewise never shells out.

Source: <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/includes/class-wpvivid-mysqldump.php>

## 2. How it dumps tables (pure-PHP path)

`WPvivid_Mysqldump::start()` enumerates tables via the adapter:

```php
foreach ($this->query($this->typeAdapter->show_tables($this->dbName)) as $row)
```

Schema is produced by `SHOW CREATE TABLE`:

```php
$stmt = $this->typeAdapter->show_create_table($tableName);
foreach ($this->query($stmt) as $r) {
    $this->compressManager->write(
        $this->typeAdapter->create_table($r, $this->dumpSettings));
}
```

Row data is exported per-table by `listValues()`. On the `wpdb` driver path it pages with LIMIT/OFFSET in **5 000-row batches** and streams each row to disk immediately:

```php
$limit_count = 5000;
while ($sum > $start) {
    $limit = " LIMIT {$limit_count} OFFSET {$start}";
    $query = $stmt . $limit;
    $resultSet = $this->query($query);
    foreach ($resultSet as $row) { /* escape + write */ }
    $this->typeAdapter->closeCursor($resultSet);
    $start += $limit_count;
}
```

On the PDO path the same `listValues()` runs an unbounded `SELECT *` and relies on PDO's unbuffered cursor. Either way nothing is accumulated in PHP memory beyond the current batch — every row (or extended-insert chunk) is sent straight through `$this->compressManager->write(...)`, which is an `fwrite` / `gzwrite` wrapper.

## 3. Locking / consistency

The dumper exposes both modes via `$dumpSettings`:

```php
'single-transaction' => true,    // line 110
'lock-tables'        => false,   // line 113
```

`single-transaction` triggers `setup_transaction()` + `start_transaction()` (issuing `SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ; START TRANSACTION WITH CONSISTENT SNAPSHOT`). If `lock-tables` is enabled, the dumper calls `$this->typeAdapter->lock_table($tableName)` per-table; a separate `start_add_lock_table()` emits `LOCK TABLES … WRITE` into the dump file itself so restore is faster. There is **no `FLUSH TABLES WITH READ LOCK`** and no engine-aware switching — InnoDB and MyISAM tables share the same code path; mixed-engine sites are documented (in the upstream ifsnop project) as a limitation of the single-transaction mode.

## 4. Special-cased types and objects

- **BLOBs**: HEX-encoded by default (`'hex-blob' => true`). The column statement is rewritten to `HEX(\`col\`) AS \`col\`` and the value emitted as `0x{hex}` — avoids escaping pitfalls and binary corruption.
- **Routines / triggers / views / events**: each has a dedicated method that uses the corresponding `SHOW CREATE …` adapter call — `getProcedureStructure()` (~line 984), `getTriggerStructure()` (~line 965), `getViewStructureView()` (~line 777), `getEventStructure()` (~line 1001). All gated by settings (`routines`, `skip-triggers`, `events`).
- **Foreign keys**: no explicit `SET FOREIGN_KEY_CHECKS=0` is written into the dump header in the visible code — restore relies on table ordering and `DROP TABLE IF EXISTS`.
- **Charset**: `init_commands` always include `SET NAMES utf8mb4`, and optionally `SET TIME_ZONE='+00:00'` unless `skip-tz-utc` is set.

## 5. Output format

Plain SQL — `INSERT INTO \`table\` VALUES (…)`. With `extended-insert` enabled (default) rows are concatenated as `,(…),(…)` until the line size approaches `net_buffer_length` (default `MAXLINESIZE = 1_000_000` bytes ≈ MySQL's `max_allowed_packet` heuristic), then a new INSERT begins:

```php
$lineSize += $this->compressManager->write(",(" . implode(",", $vals) . ")");
```

So the "batch size" is byte-bounded (~1 MB per INSERT statement), not row-count-bounded. Escaping is delegated to the type adapter (`PDO::quote` on the PDO path; `$wpdb->_real_escape()` on the wpdb path), with BLOB values bypassed via the HEX shortcut described above.

## 6. Memory bounding

There is **no** `memory_limit` probe, no `gc_collect_cycles()` call, and no targeted `unset()` inside the row loop. Memory is bounded structurally: 5 000-row LIMIT pages on wpdb, unbuffered cursors on PDO, and immediate streaming through `compressManager->write()`. The only per-row bookkeeping is a cancel check every 200 000 rows:

```php
if ($i >= 200000) {
    $count += $i;
    if (++$i_check_cancel > 5) {
        $wpvivid_plugin->check_cancel_backup($this->task_id);
        // …
    }
}
```

## 7. Surviving `max_execution_time`

This is the most interesting part. The dumper itself does **not** chunk mid-table — it runs `start()` straight through. The survival logic lives in the orchestrator (`includes/class-wpvivid.php`, plus the task-manager class):

- Backup methods call `@set_time_limit($second)` and `@ignore_user_abort(true)`.
- Task state (`status['str']` ∈ {`ready`, `running`, `wait_resume`, `completed`}, plus `data['backup']['finished']` / `progress`) is persisted via `WPvivid_Setting::update_task($id, $this->task)` so any request can resume.
- A `register_shutdown_function(array($this,'deal_shutdown_error'), $task_id)` catches PHP fatals (including OOM) and bumps a `resume_count` field.
- On timeout or fatal, a resume is queued via WP-Cron:

  ```php
  wp_schedule_single_event($resume_time, WPVIVID_RESUME_SCHEDULE_EVENT, array($task_id));
  ```

  with handler `add_action(WPVIVID_RESUME_SCHEDULE_EVENT, array($this,'resume_schedule'))`.
- The user-facing entry is `add_action('wp_ajax_wpvivid_backup_now', array($this,'backup_now'))`; a polling AJAX endpoint reads `get_backup_task_info()` and flags `need_next_schedule = true` once `running_stamp` (heartbeat age) exceeds 180 s, which the JS uses to kick the cron event again.

So the model is: **single big run with set_time_limit(0) + ignore_user_abort, persistent task state, and WP-Cron-based resume on failure**, fronted by an AJAX progress poller. Resume granularity is per-table-section, not per-row.

## 8. Compression

Compression is done in PHP via a factory:

```php
const GZIP = 'Gzip'; const BZIP2 = 'Bzip2'; const NONE = 'None';
$this->compressManager = WPvividCompressManagerFactory::create($this->dumpSettings['compress']);
```

Streamed inline during the dump using `gzopen()` / `gzwrite()` (or `bzopen()` / `bzwrite()`), never shelled out and never post-processed. This is why the dumper can stream straight to a `.sql.gz` without ever materialising the uncompressed SQL on disk.

## 9. Library dependencies

The dumper is a fork of **`ifsnop/mysqldump-php`** (GPL), credited in the file header:

```php
/**
 * @link   https://github.com/ifsnop/mysqldump-php
 * @author Michael J. Calkins <clouddueling@github.com>
 * @author Diego Torres <ifsnop@github.com>
 */
```

It is vendored (renamed `WPvivid_Mysqldump`) rather than pulled via Composer. The only other third-party libraries in `includes/lib/` are unrelated to DB dump: `aws-sdk-php-2.8.31` and `google-api-php-client` (cloud destinations).

## 10. Implications for WPMgr / phpbu

phpbu's `mysqldump` source assumes the binary exists; on shared/managed hosts (WP Engine, Kinsta legacy plans, many cPanel boxes) it does not, and `exec()` is often disabled outright. Three options:

**(a) Bundle `ifsnop/mysqldump-php` as a phpbu source plugin.** Pros: zero original code, GPL-compatible, battle-tested (WPvivid, UpdraftDraft-style forks, BackWPup all use it), supports streaming + gzip + hex-blob + routines/views/triggers out of the box. Cons: phpbu would need a thin `Source` adapter (`ifsnop\Mysqldump\Mysqldump` → write to phpbu's target path), and we inherit the library's lack of engine-aware locking and lack of `FOREIGN_KEY_CHECKS=0` header.

**(b) Write our own dumper modelled on WPvivid.** Pros: full control of session pragmas (`FOREIGN_KEY_CHECKS=0`, `SQL_MODE=''`), can add per-row resume state for WP-Cron-friendly chunking. Cons: 1 000+ lines of new code, BLOB/charset/view edge cases that took ifsnop years to shake out, ongoing maintenance burden.

**(c) Require `mysqldump` and document the limitation.** Pros: simplest. Cons: silently breaks on a large fraction of WPMgr's target hosts — the very hosts where managed backup tooling is most valuable.

**Recommendation: (a) with a thin wrapper.** Vendor `ifsnop/mysqldump-php` (≈1 file, MIT/GPL dual), expose it as `phpbu\App\Backup\Source\MysqldumpPhp`, and have the agent detect `mysqldump` availability and fall through automatically. Bolt on what WPvivid bolted on outside the dumper: a state-persisting task wrapper and a `set_time_limit(0) + ignore_user_abort(true) + register_shutdown_function` harness so a 30-second `max_execution_time` doesn't kill long dumps. Skip WPvivid's WP-Cron resume initially — phpbu runs are usually triggered out-of-band (cron/CLI) where execution-time caps don't bite.
