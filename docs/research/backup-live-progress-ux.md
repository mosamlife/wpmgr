# How mature WP backup tools deliver live progress UX

What transport, what cadence, what the user actually sees — and what WPMgr V0
should copy. Source code excerpts taken from WP.org SVN trunk and GitHub
mirrors on 2026-05-28.

---

## TL;DR table

| Product           | Transport         | Wire cadence     | Payload                              | Cancel       | Survives tab close |
| ----------------- | ----------------- | ---------------- | ------------------------------------ | ------------ | ------------------ |
| WPvivid           | AJAX poll         | **3000 ms**      | JSON + server-rendered `progress_html` | AJAX action  | Yes (detached)     |
| UpdraftPlus       | AJAX poll         | **~5500 ms** *   | JSON, jobdata fields                 | AJAX action  | Yes (wp-cron resumed) |
| BackWPup          | AJAX poll         | **750 ms**       | JSON `{step_percent, log_text, ...}` | GET to URL   | Yes (cron/CLI)     |
| MainWP (self-host)| AJAX poll         | **1000 ms**      | JSON `{filesize, progress}`          | n/a (PID check) | Yes              |
| ManageWP (SaaS)   | Worker-streamed HTTP messages from dashboard backend | sub-second perceived | Streamed text `"60% at 16MB/s"` | Cloud-side  | Yes (cloud-owned)  |

\* UpdraftPlus has a 1250ms `setInterval` but the function early-exits unless
5500ms has elapsed. Net effective UI refresh ≈ every 5.5 s.

None use SSE or WebSocket. Every plugin polls `admin-ajax.php`.

---

## 1. WPvivid — 3 s poll, server-rendered HTML

`admin/js/wpvivid-admin.js`:

```js
function wpvivid_manage_task() {
    if (m_need_update === true) { m_need_update = false; wpvivid_check_runningtask(); }
    else setTimeout(function(){ wpvivid_manage_task(); }, 3000);
}
```

AJAX action `wpvivid_list_tasks` returns a JSON envelope whose `progress_html`
is a pre-rendered DOM fragment the JS slams in:

```js
jQuery('#wpvivid_postbox_backup_percent').html(value.progress_html);
```

UX: percent bar updates in ~3 s jumps, with sub-text "Total Size / Uploaded /
Speed / Network / Current doing". The bar is byte-weighted across phases
(`db_size`, `files_size['sum']`, `backup_percent`). No CSS easing — visible
chunky steps. Cancel is a separate AJAX call. Restore polls a different
action (`wpvivid_get_restore_progress_2`) every 2 s.
Tab close is irrelevant because backup runs detached via
`fastcgi_finish_request()` + `ignore_user_abort(true)` (see
`docs/research/wpvivid-async-progress-restore.md` §1, §3).
Source: <https://plugins.svn.wordpress.org/wpvivid-backuprestore/trunk/admin/js/wpvivid-admin.js>

## 2. UpdraftPlus — 1250 ms timer, 5500 ms effective floor

`includes/updraft-admin-common.js`, inside `updraft_backupnow_inpage_go()`:

```js
updraft_activejobs_update_timer = setInterval(function () {
    updraft_activejobs_update(false);
}, 1250);
```

But the worker function itself throttles:

```js
var timenow = (new Date).getTime();
if (false == force && timenow < updraft_activejobs_nextupdate) { return; }
updraft_activejobs_nextupdate = timenow + 5500;
```

So three out of every four `setInterval` ticks no-op. Real network cadence is
**~5.5 s**. The 1250 ms timer exists so a `force=true` call (e.g. user clicks
"Refresh") fires immediately without waiting.

Payload is JSON parsed by `updraft_process_status_check`. For per-download
items the bar uses `dstatus.p` for percent, `dstatus.a` for "age in seconds
since last activity" (used as the stalled trigger — a download with `a > 90 &&
sincelastrestart > 60000` gets auto-restarted; that's UpdraftPlus's
stalled-but-recovering UX). Bar width is just `.width(dstatus.p + '%')` — no
animation.

Cancel: `updraft_send_command('activejobs_delete', jobid, ...)`. Tab close
doesn't kill the backup — UpdraftPlus relies on wp-cron resumption (it has its
own resume-on-next-pageload model similar to WPvivid's `task_monitor`).
A separate 30 s timer (`updraft_historytimer`) refreshes the backup-history
table; another fires `/wp-cron.php` every 210 s to keep wp-cron warm.
Source: <https://plugins.svn.wordpress.org/updraftplus/trunk/includes/updraft-admin-common.js>

## 3. BackWPup — 750 ms, the fastest of the bunch

`assets/js/backwpup-generate.js`:

```js
backwpup_show_working = function () {
    $.ajax({ type: 'GET', url: ajaxurl, data: {
        action: 'backwpup_working',
        logpos: $('#logpos').val(),
        logfile: backwpupApi.logfile,
        _ajax_nonce: backwpupApi.nonce_generate
    }, success: function (rundata) { ... } });
    setTimeout('backwpup_show_working()', 750);
};
```

Notable: `logpos` is sent on each poll — server tails the log from that
offset, returns only new bytes. So the response is small and the live log
modal feels like a console. JSON fields include `step_percent`,
`warning_count`, `error_count`, `log_text`. Two progress bars (overall +
current step). Abort is a simple `GET` to a per-job URL passed via
`data-url`. Backup itself runs out-of-band (cron or CLI), so tab close is
fine.
Source: <https://plugins.svn.wordpress.org/backwpup/trunk/assets/js/backwpup-generate.js>

## 4. MainWP — 1 s poll on backup creation + upload

`assets/js/mainwp-backups.js` runs three concurrent pollers on the dashboard:

```js
// file growth during creation
setTimeout(function () { fnc(fnc); }, 1000);  // action: mainwp_createbackup_getfilesize

// download to dashboard
setInterval(function () {
    jQuery.post(ajaxurl, mainwp_secure_data({
        action: 'mainwp_backup_getfilesize', local: file
    }), ...);
}, 1000);

// remote upload progress
let data2 = mainwp_secure_data({
    action: 'mainwp_backup_upload_getprogress', unique: pUnique
}, false);
```

Plus a 10 s `mainwp_backup_checkpid` for stall detection. The dashboard does
**not** poll the child site for backup status — it polls itself; the child
streams file content over a single long HTTP call while the dashboard
introspects local temp-file sizes. That's the trick: progress = local stat().
Source: <https://plugins.svn.wordpress.org/mainwp/trunk/assets/js/mainwp-backups.js>

## 5. ManageWP (SaaS) — only one that feels truly live

ManageWP's Worker plugin streams messages over the HTTP response body using
Guzzle on the receiving cloud side; their dev-diary describes UI text like
`"Backup file upload in progress – 60% at 16MB/s"` updating in near-real-time.
This is effectively HTTP chunked transfer (long-poll-streaming) — closer to
SSE than to AJAX polling. Cancel, retry, and stalled detection are all
cloud-orchestrated because ManageWP owns the dashboard process; the WP plugin
is a dumb worker.
Source: <https://managewp.com/managewp-orion-developer-diary-3-bulletproof-backup-solution/>

---

## Cross-cutting observations

- **No one uses SSE.** Not even ManageWP (they stream a single HTTP body,
  which is similar but simpler). The WordPress shared-hosting reality —
  Apache+mod_php FPM workers with limited concurrency, proxies that buffer
  text/event-stream, etc. — makes SSE risky to ship as a default. Polling
  works on every host.
- **No one uses WebSockets.** Same reasons, plus needing a separate server
  process.
- **Cadence sweet spot for "live-feeling" is ≤1 s, useless below ~300 ms.**
  BackWPup at 750 ms feels like a console. WPvivid at 3 s feels lethargic.
  UpdraftPlus at 5.5 s feels broken when you watch it.
- **Server-rendered HTML (WPvivid) vs JSON+client-render (UpdraftPlus,
  BackWPup, MainWP)** — JSON wins. Smaller payloads, easier to evolve, easier
  to animate the bar with CSS transitions instead of innerHTML reflow.
- **Stalled-vs-failed UX** comes from a single client-side counter ("seconds
  since last activity"), not from the server reporting "I'm stuck". UpdraftPlus
  does this with `dstatus.a`; others rely on PID/file-mtime checks.
- **Polling cost is trivial.** 1 req/s of `admin-ajax.php` is far below the
  background noise of WordPress heartbeat (15 s) plus autosave plus block
  editor traffic. The only real concern is request-stampede when the
  page refocuses (browsers fire all queued timers) — UpdraftPlus's
  setInterval+throttle pattern is the right defence.

---

## Recommendation for WPMgr V0

**Pick (a) with a refinement: poll every 1000 ms, smooth client-side.**

Rationale:

1. **1.5 s → 1.0 s** matches BackWPup/MainWP, which are the products that
   feel live in practice. 300-500 ms is overkill — you can't tell the
   difference at the eyeball level, and you 3× the request rate for nothing.
   At 1 s, an active backup on a 100-site dashboard generates ~100 req/s
   of `/api/runs/:id/progress` — fine for our Hono backend, negligible vs
   our existing heartbeats.
2. **Animate the bar client-side with CSS `transition: width 1s linear`.**
   Two adjacent samples will visually fill the gap, so the bar appears to
   move continuously even though data arrives in discrete 1 s ticks. This
   is the single highest-leverage UX upgrade — none of the WP plugins do it,
   and it's ~3 lines of CSS.
3. **Adopt UpdraftPlus's `dstatus.a` pattern.** Server returns
   `last_activity_at`; client computes age and shows
   "Working… (last update 12 s ago)" → "Stalled, retrying…" → "Failed" using
   thresholds (e.g. >30 s amber, >120 s red). This is the cheapest way to
   distinguish "alive but slow" from "dead".
4. **Adopt BackWPup's `logpos` tail trick** for the optional log drawer —
   server returns only bytes past the client's last offset. Keeps payloads
   tiny when verbose phases produce a lot of log.
5. **Defer SSE (option b) to V1.** It is architecturally cleaner and it's
   what ManageWP effectively does, but the gains over 1 s polling + CSS
   animation are not worth shipping a new transport during V0. Add it when
   you need fan-out to multiple watchers of the same run (e.g. a shared
   ops dashboard), at which point SSE on a Hono `EventStream` route is a
   2-hour change. Skip WebSocket entirely until there's a real bidirectional
   need (e.g. user pause/resume that needs sub-second ack).

Concretely for V0: change the dashboard `useBackupProgress` hook from
`refetchInterval: 1500` to `refetchInterval: 1000`, add the CSS transition
on `<ProgressBar>`, add `last_activity_at` to the run schema and a
"stalled-but-retrying" amber state. That's a half-day of work and matches
the perceptual ceiling of every shipped competitor short of ManageWP.
