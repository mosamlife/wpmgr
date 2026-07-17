# Sites dashboard

The Sites dashboard is the central view of your fleet. It supports two display
modes (list and grid) and a set of composable filters that persist in the URL.

---

## List vs. grid toggle

A toggle in the top-right corner of the Sites dashboard switches between:

| Mode | Best for |
|------|----------|
| **List** | Dense, sortable rows: fast scanning of connection state, last-seen, and status across a large fleet. |
| **Grid** | Rich cards with screenshots and a labeled capability summary per site. |

The chosen mode persists in the URL (`?view=list` / `?view=grid`) so a filtered
grid or list view is shareable and survives a reload.

---

## Grid cards

Each grid card is structured as follows (top to bottom):

1. **Screenshot**: a real screenshot of the site's front page, or a favicon
   or monogram fallback until a capture lands.
2. **Site name plus connection state badge.**
3. **Site configuration group**: labeled on/off indicators for:
   - Page Cache
   - Object Cache
   - HTTPS
   - Backups
   - Multisite
4. **Pending updates count** (if any).
5. **Key/value metadata**: Versions (WP, PHP, agent), Host, Client, Tags.
6. **Health row**: uptime percentage, average latency, SSL expiry, and
   backup health, shown as a relative time (for example "3h ago"; hover for
   the exact timestamp) with a status color for on time, overdue, and
   never-backed-up states.

Cards line up row-for-row regardless of which optional data a site has; every
section reserves its height with a calm empty state.

List view shows the same relative last-backup time in its own **Backup**
column, with an identical hover-for-exact-timestamp affordance, so the two
views stay consistent.

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
`docker compose -f infra/docker-compose.yml up -d`, no profile or extra flag
needed.

If you disabled it (for example `--scale media-encoder=0` on a
RAM-constrained host, see [install.md](../install.md#media-encoder)),
screenshot requests are accepted but captures never run; cards degrade to
favicon / monogram permanently.

---

## Filters

The Sites dashboard supports composable filters:

| Filter | Behavior |
|--------|----------|
| **Search** | Free-text match against site name, URL, and tag values. |
| **Status** | Multi-select connection states (`connected`, `degraded`, `disconnected`, `revoked`, `pending_enrollment`, `archived`). ORs within the axis. |
| **Tags** | Multi-select from the tag registry, with a match-any/match-all mode. See [Tags](#tags) below. |
| **Client** | Single-select client grouping. |
| **Archived** | Toggle to include archived sites. |

All active filters are reflected in the URL and compose with each other (AND
across axes; OR within a multi-select axis). An applied-count badge shows how
many filters are active; a **Clear all** control resets them.

---

## Tags

Tags are a tenant-wide, shared vocabulary: creating or renaming a tag affects
every site carrying it, and the same tag (name plus color) is available for
every site in the fleet, not just the one it was created from.

### Assigning tags

Add or remove a tag from a single site from any of these places:

- The **Tags** cell in the list view's table row.
- The tag row on a grid card (shows up to 2 tags plus an overflow count;
  click the overflow chip or the **+** control to open the picker).
- The site's own settings page.

All three open the same tag picker: type to search the tag registry, or type
a name that doesn't exist yet and press **Enter** (or select the
**Create "..."** row) to create it and assign it in one step. Selecting a
tag that's already assigned removes it; the picker is a toggle, not an
add-only list. Toggling applies immediately; there is no separate save step.

### Colors

Every tag has a color. Leave it on **Auto** and the dashboard derives a
consistent color from the tag's name (the same name always resolves to the
same one of 12 hues, in any browser, for any tenant), so tags get a color
with zero setup and never visibly shuffle between reloads. To pick a
specific color instead, choose one of the 12 palette swatches from the color
picker (there is no free-form hex entry in the UI); a tag can also carry an
arbitrary hex color if it was set through the API, in which case it renders
as its own translucent chip.

### Filtering by tag

Open the **Tags** dropdown in the toolbar to filter by one or more tags from
the registry (including tags with zero sites, since the list is
registry-backed, not derived from what's currently loaded). With two or more
tags selected, a **Match any / Match all** control appears above the list:

- **Match any** (default): a site needs at least one of the selected tags.
- **Match all**: a site needs every selected tag.

Selected tags render as removable chips next to the Tags button, and the
filter (including match mode) is written to the URL, so a filtered link is
shareable and survives a reload. You can also filter to a single tag
instantly by clicking any tag chip on a site card or table row, or by
clicking a tag's usage count in Settings > Tags.

### Bulk add / remove

Select multiple sites (checkbox in list view, or **Select all** in grid
view) and choose **Tag N sites** from the bulk-actions bar. The same tag
picker opens, but each tag now has three states instead of two:

- **Checked**: every selected site already has this tag.
- **Unchecked**: no selected site has this tag.
- **Mixed**: some, but not all, selected sites have this tag (shown with a
  dash instead of a checkmark).

Clicking a tag cycles it (mixed to checked to unchecked to checked, and so
on); nothing is applied to any site until you confirm. On confirm, every
toggled tag is added to or removed from every selected site in one batched
request.

### Settings > Tags

The full tag registry lives at **Settings > Tags**, reachable from the
sidebar or via the "Manage tags" link at the bottom of the Tags filter
dropdown. Every tag in the tenant is listed, including unused ones, with:

- **Usage count**, linking to the Sites list pre-filtered to that tag.
- **Rename**: renames the tag everywhere it's used, fleet-wide, in one
  action. Renaming to a name that already exists offers to **merge** the
  two tags instead of failing outright.
- **Merge into...**: explicitly folds one tag into another; every site
  carrying the source tag ends up with the target tag instead, and the
  source tag is removed from the registry.
- **Recolor**: same 12-swatch picker (or Auto) as the assignment picker.
- **Delete**: removes the tag from the registry and from every site
  carrying it; the confirmation dialog states exactly how many sites will
  lose the tag.

Creating, renaming, merging, and deleting a tag are registry-wide,
fleet-affecting actions and are scoped to full organization members. A
collaborator with access to only one shared site cannot rename or delete a
tag that might be attached to sites outside their access, even though they
can still assign or remove existing tags on the site(s) they can reach.

---

## Uptime, latency, and SSL on the card

The grid card draws uptime percentage, average latency, and SSL expiry directly
from the joined uptime-monitor data returned with each site in the list response.
No per-site request is required to populate these fields.

Uptime and SSL data appear only when active uptime monitoring is configured for
the site.

---

## Related

- [Site connection lifecycle](./site-lifecycle.md): enrollment, states, revoke, re-enroll.
- [Monitoring and health](./performance-suite.md): uptime, response-time charts, alerts.
