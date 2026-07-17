# Monitoring

Uptime probing, Real User Monitoring (RUM), and the three fleet-wide
dashboards that roll every managed site's health up into one view.

---

## Uptime probing

**How it works.** Every enrolled site is probed on a fixed schedule
(default every 60 seconds; self-host operators can change this with
`WPMGR_UPTIME_PROBE_INTERVAL`), independently of any other site. The probe
is a single timed HTTP(S) GET issued through the same SSRF-hardened
transport used everywhere a site URL is fetched, so a probe can never be
pointed at a private, loopback, or link-local address even if a site's URL
is later changed to one. Each probe records a phase breakdown (DNS,
connect, TLS handshake, time to first byte, total) plus the TLS
certificate's issuer, subject, and expiry when the site is HTTPS.

**Up or down.** A response in the 200 to 499 range counts as up (a 404 is
still a responding server); a 500-series response, a timeout, or a
connection error counts as down.

**The "critical error page" case.** A WordPress fatal error or a lost
database connection is served with HTTP 200, since PHP has already sent
headers by the time it happens, so status-code classification alone would
report the site as up while it is actually broken. Every probe scans the
(bounded, in-memory) response body for the structural signature of
WordPress's own critical-error screen or its database-connection-error
screen, locale-independent (it keys on markup WordPress core never
translates, not on the visible message text), and reclassifies the probe
as down when it matches. An incident or alert caused by this shows up
labeled "WordPress fatal error page" or "Database connection error page"
rather than a generic reachability failure, so you know at a glance the
server responded but the site itself is broken.

**Incidents.** A site enters an incident after a configurable number of
consecutive down probes (default 2); it exits the incident on the next
successful probe. Only these two transitions are recorded, so a site
flapping between the same two states never spams the incident list.
Incidents are persisted rows (not just a live status derivation), each
carrying its start time, end time (null while ongoing), peak status, and
the last HTTP status/reason observed, so you can review history rather
than only the current state.

**Alerts.** Each tenant has one alert channel configuration (Settings >
Alerts), shared by uptime and high-severity security events: an email
recipient list and/or an HMAC-SHA256-signed webhook. An alert fires exactly
once per transition, a downtime alert when a site's incident opens, a
recovery alert when it closes, never a flood of repeated notifications
while a site stays down. Every fired alert is recorded to the audit log
together with the real delivery outcome of each channel (sent, failed, or
skipped because nothing was configured), not merely that a recipient
existed.

**Data retention.** Raw probe rows are kept 90 days on the default Postgres
backend, matching the longest chart window the dashboard offers.
ClickHouse is selectable as an alternative time-series backend at boot
(`WPMGR_CLICKHOUSE_ADDR`) for larger fleets; it uses its own 90-day TTL.

---

## Real User Monitoring (RUM)

**What it measures.** A tiny first-party collector beacons the Core Web
Vitals from real visitor page loads back to the control plane: Largest
Contentful Paint (LCP), Interaction to Next Paint (INP), Cumulative Layout
Shift (CLS), First Contentful Paint (FCP), and Time to First Byte (TTFB).
Each sample also carries the device class (desktop, mobile, tablet), a
coarse network-connection type, and a country code capped to the top
reporting countries per site (the remainder buckets as "other") so the
dashboard can break results down without an unbounded per-visitor
dimension.

**Where the script loads.** The collector is enqueued through WordPress's
own `wp_enqueue_scripts` action on every front-end request, independent of
whether page caching is on, off, or handled by a third-party cache. It
loads asynchronously from `<head>` (required so the Cumulative Layout
Shift measurement, which is gated on the First Contentful Paint firing,
has time to register before a visitor can navigate away) and beacons over
`sendBeacon`, which respects a strict Content-Security-Policy `connect-src`
allowlist.

**Off by default.** RUM is a per-site toggle with an adjustable sample
rate; a site with it off sends nothing. When enabled, the beacon is
authenticated with a per-site key (rotatable from the dashboard), not a
public write endpoint.

**Reading the numbers.** Every metric is reported as p75 (the value 75% of
real visits were at or better than), the same convention the Chrome UX
Report uses, computed from a fixed set of histogram bucket boundaries so
externally reported CrUX numbers and WPMgr's own numbers are directly
comparable. Results are shown per URL and per device, with a
good/needs-improvement/poor distribution bar and a 28-day trend chart with
the CWV threshold lines drawn on for reference.

**Per-site vs. fleet.** A single site's Core Web Vitals live on its Health
tab (a summary tile) and, in full detail (per-URL breakdown, trend, device
tabs), on the fleet Performance dashboard scoped to that one site. Leaving
the scope at "all sites" shows the fleet-wide aggregate instead: a
worst-first table of every reporting site sorted by LCP p75, and a 28-day
fleet-wide trend.

**Data retention.** Individual raw events are kept briefly (24 hours
self-host, 48 hours hosted) since they exist only to feed the rollups.
Hourly rollups (used for the recent trend and per-URL breakdown) are kept
7 days self-host, 14 days hosted. Daily rollups (used for the 28-day trend
and longer-range history) are kept 90 days self-host, about 13 months
hosted.

**Privacy.** No cookies and no cross-site identifiers. Page paths are
stored with the query string stripped. The visitor's IP address is used
only transiently to resolve a coarse country code and is never stored.

---

## The three fleet dashboards

### Uptime (`/uptime`)

At-a-glance status for every site in the fleet: a status-matrix grid (one
cell per site, colored by up/degraded/down/unknown), a table with a 90-day
day-by-day status strip and a latency sparkline per site, and an incidents
panel listing every open and recently-closed incident across the fleet.
"Degraded" here is a display-only classification (a site that responded
but slower than 2 seconds), separate from the down/up status that actually
drives incidents and alerts. Clicking an incident opens its detail: the
full probe timeline for that window, the peak status reached, and how many
times that site has had an incident in the last 30 days.

### Backups (`/backups`)

A row-per-site view of backup protection across the fleet, not a
snapshot browser. Each site is classified into one status:

| Status | Meaning |
|---|---|
| Protected | A recent completed backup exists and nothing has failed since. |
| Stale | A completed backup exists, but it is older than twice the schedule's cadence (or 48 hours if no schedule is set). |
| Failed | The most recent completed event was a failure. |
| In flight | A backup is currently pending or running and there is no recent completed backup to anchor a status yet. |
| Unprotected | No completed backup has ever been recorded for the site. |

Summary tiles at the top let you filter straight to, for example, every
Unprotected site. Each row shows the last good backup's age, the next
scheduled run, and the latest size, with row actions to run a backup,
browse its snapshots, or restore, without leaving the dashboard.

### Performance (`/performance`)

The fleet-wide Core Web Vitals and database-health view described under
Real User Monitoring above, plus a database-health panel aggregating the
[Database Cleaner](./database-cleaner.md)'s per-site table health across
the fleet. Selecting a specific site (via the scope selector, or by
clicking a row) switches to that site's full per-site detail without
leaving the page.

---

## Per-site Health tab

Every site's own **Health** tab opens with four tiles sharing one card
(uptime, last backup, vulnerabilities, performance), each linking out to
the page that owns the corresponding action, followed by the full
diagnostics surface (WordPress Site Health data plus WPMgr's own extended
checks, grouped and independently fault-isolated so one failing check
never blanks the rest of the screen).
