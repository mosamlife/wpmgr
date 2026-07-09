# Sites dashboard

The Sites dashboard is the central view of your fleet. It supports two display
modes (list and grid) and a set of composable filters that persist in the URL.

---

## List vs. grid toggle

A toggle in the top-right corner of the Sites dashboard switches between:

| Mode | Best for |
|------|----------|
| **List** | Dense, sortable rows — fast scanning of connection state, last-seen, and status across a large fleet. |
| **Grid** | Rich cards with screenshots and a labeled capability summary per site. |

The chosen mode persists in the URL (`?view=list` / `?view=grid`) so a filtered
grid or list view is shareable and survives a reload.

---

## Grid cards

Each grid card is structured as follows (top to bottom):

1. **Screenshot** — a real screenshot of the site's front page, or a favicon /
   monogram fallback until a capture lands.
2. **Site name + connection state badge.**
3. **Site configuration group** — labeled on/off indicators for:
   - Page Cache
   - Object Cache
   - HTTPS
   - Backups
   - Multisite
4. **Pending updates count** (if any).
5. **Key/value metadata** — Versions (WP, PHP, agent), Host, Client, Tags.
6. **Health row** — uptime percentage, average latency, SSL expiry, and backup
   health (last backup age).

Cards line up row-for-row regardless of which optional data a site has; every
section reserves its height with a calm empty state.

---

## Website screenshots

Screenshots are captured server-side by headless Chromium running inside the
media-encoder service. No client-side cross-origin capture is involved.

### When captures happen

| Trigger | Notes |
|---------|-------|
| Site connects (enrollment or reconnect) | Runs automatically in the background. |
| Weekly scheduled refresh | Keeps the thumbnail current. |
| On demand | Click the camera icon on a card or the refresh action on the site detail page. |

The dashboard polls after a capture request and updates the card when the image
is ready, with no manual reload.

### Screenshot security

- Every request the browser makes during capture passes through an in-process
  SSRF proxy that re-validates the destination at dial time. Private, link-local,
  loopback, and cloud-metadata addresses are rejected.
- QUIC, HTTP/3, and non-proxied WebRTC are disabled so no request can escape the
  proxy over UDP.
- Captures run with bounded memory and time limits.
- The screenshot table is tenant-isolated with a restrictive row policy.
- Only signed presigned URLs from the control plane are served to the browser;
  the raw site URL is never exposed.

### Self-host note

Screenshots require the `media-encoder` service (headless Chromium). It is part
of the base Compose stack and starts automatically with a plain
`docker compose -f infra/docker-compose.yml up -d` — no profile or extra flag
needed.

If you disabled it (e.g. `--scale media-encoder=0` on a RAM-constrained host —
see [install.md](../install.md#media-encoder)), screenshot requests are
accepted but captures never run; cards degrade to favicon / monogram
permanently.

---

## Filters

The Sites dashboard supports composable filters:

| Filter | Behavior |
|--------|----------|
| **Search** | Free-text match against site name and URL. |
| **Status** | Multi-select connection states (`connected`, `degraded`, `disconnected`, `revoked`, `pending_enrollment`). |
| **Tags** | Multi-select — shows only sites that have all selected tags. |
| **Client** | Single-select client grouping. |
| **Archived** | Toggle to include archived sites. |

All active filters are reflected in the URL and compose with each other. An
applied-count badge shows how many filters are active; a **Clear all** control
resets them.

---

## Uptime, latency, and SSL on the card

The grid card draws uptime percentage, average latency, and SSL expiry directly
from the joined uptime-monitor data returned with each site in the list response.
No per-site request is required to populate these fields.

Uptime and SSL data appear only when active uptime monitoring is configured for
the site.

---

## Related

- [Site connection lifecycle](./site-lifecycle.md) — enrollment, states, revoke, re-enroll.
- [Monitoring and health](./performance-suite.md) — uptime, response-time charts, alerts.
