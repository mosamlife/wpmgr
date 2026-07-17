# Object cache (Redis)

A persistent, per-site WordPress object cache backed by Redis, installed as
a drop-in on the site itself.

---

## What it is

WordPress's object cache API (`wp_cache_get`/`wp_cache_set` and friends) is
a no-op by default: every request re-fetches from MySQL. This feature
installs a generated `object-cache.php` drop-in in the site's `wp-content`
directory that implements the full WordPress cache API surface (single and
multi-key get/set/add/replace/delete, `flush_group`, `flush_runtime`,
`wp_cache_supports`, and `switch_to_blog` for multisite) against Redis, so
logged-in traffic, wp-admin screens, and anything the page cache cannot
serve gets the speed-up. The generated file is fully self-contained: one
file inlines the connection layer, the engine, and the config loader, with
no runtime dependency on the plugin's folder name or install location.

---

## Requirements

- A reachable Redis server (or Redis-protocol-compatible service): TCP,
  TLS, or a Unix socket.
- The `phpredis` PHP extension on the WordPress host. There is no
  pure-PHP fallback client; without `phpredis` the connect test fails and
  the feature cannot be enabled.
- Optional codecs the connect test can detect and use if present:
  `igbinary` serialization, and `lzf`/`lz4`/`zstd` compression. Requesting
  a codec that the connect test reports as unavailable is rejected up
  front with a clear message, rather than silently degrading.

Self-host operators bring their own Redis (the bundled Compose stack
includes one for sessions/cache/dedup that this feature can also target).
There is no bundled managed Redis on the hosted service either: you point
each site at a Redis instance you provision.

---

## Enabling it per site

From a site's **Object cache** panel:

1. Enter the connection details (host/port or Unix socket, optional
   password, database index, key prefix, TLS).
2. Click **Test connection**. The agent dials the candidate configuration
   without persisting anything, reports whether phpredis is available,
   which codecs the server and client both support, and the server's
   configured eviction policy.
3. Enabling is blocked until a test has passed for that exact
   configuration. Changing any connection field invalidates the prior
   pass and requires a fresh test.
4. Click **Enable**.

The connection password (when set) is age-encrypted (X25519) on the
control plane and delivered to the site only via the signed command
channel, written to a 0600 file in `wp-content`. It is never present in
any API response, log line, SSE payload, test result, or heartbeat; the
API only ever reports whether a password is currently set.

---

## The safety model

**Config hash and drift detection.** Every push of a validated
configuration is hashed; the drop-in reports the hash of the config file
it is actually reading back on its heartbeat. If that reported hash ever
diverges from the hash of what the control plane has stored, the
dashboard flags a drift, since the two should always match on a healthy
site.

**Two-level graceful degradation.** If Redis is unreachable at boot, the
drop-in swaps in a pure in-memory array cache for that request rather than
taking the site down, so a fully down Redis never becomes a fully down
site. Within a request, a Redis error mid-flight is treated as a cache
miss with one reconnect attempt, then the rest of that request degrades
the same way. A failure does not retry on every subsequent request either:
a persisted reconnect cool-down (starting at 15 seconds, doubling on each
consecutive failure, capped at 5 minutes) stops a genuinely down Redis
from being re-dialed on every page load.

**Fully self-contained drop-in, with self-healing.** Because the generated
file inlines everything, there is nothing else on disk it depends on. The
agent invalidates the drop-in's OPcache entry on every version change, so
an outdated cached copy of the file self-heals on the next heartbeat
instead of silently running stale code.

**Per-site key isolation.** Every key is prefixed per site, so multiple
sites sharing one Redis instance (a common self-host setup) can never read
or flush each other's cache entries. A flush always targets only the
calling site's own prefix.

---

## Stats on the dashboard

The panel shows live connection status (connected, degraded, or down)
updated over SSE, roughly every 10 seconds. Charts cover hit ratio, used
memory, average command latency, and operations per second, with a 7-day
detailed window and a 90-day daily-downsampled history.

### The debug response header

When the **Debug response header** setting is on, front-end responses
carry a header showing per-request cache activity:

```
x-wpmgr-object-cache: state=connected hits=42 misses=3 reads=45 writes=12 ms=1.4
```

Logged-in administrators always receive this header on the front end
regardless of the setting (a built-in visibility path for diagnosing a
site without turning the setting on for every visitor); the setting only
controls whether anonymous visitors also see it. It is never emitted on
REST API, admin-ajax, WP-Cron, wp-admin, or wp-login.php requests, and it
carries only aggregate counters, never the Redis host, port, socket path,
key prefix, username, database index, or any key name.

Pages served entirely by the page cache do not carry this header, since
WordPress (and therefore the object cache) never runs on a cache hit.

---

## Troubleshooting

**Enable is greyed out, or saving the config does nothing.** A connect
test must pass first, for the exact configuration you're trying to save.
Re-run **Test connection** after any change.

**"serializer ... is not available" / "compression ... is not
available".** The connect test detected that phpredis (or the Redis
server) does not actually support the codec you selected. Pick a codec the
test reported as available, or install the missing PHP extension
(`igbinary`, or the relevant compression extension) on the WordPress host.

**Status shows "degraded" or "down".** The site has fallen back to the
in-memory array cache (down: the boot connection failed) or hit a
mid-request Redis error and is riding out the reconnect cool-down
(degraded). Check that Redis is reachable from the WordPress host on the
configured address/port, and that the password (if any) is still correct;
re-running **Test connection** will confirm.

**Two sites on the same Redis show each other's data, or a flush on one
site affects another.** This should not happen (keys are always
prefixed per site); if it does, confirm both sites' key prefixes are
actually distinct, since a prefix collision (for example, two sites
manually set to the same custom prefix) defeats the isolation.
