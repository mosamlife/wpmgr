# Media Optimizer

Convert a site's JPEG/PNG library to **WebP** or **AVIF** (or re-compress the
originals) without touching the WordPress host's CPU. Encoding runs on WPMgr's
own software — Discord's MIT-licensed `lilliput` — on a separate control-plane
service. No third-party SaaS, no per-image API fees.

Design: [ADR-043](../adr/ADR-043-media-optimizer-architecture.md).
Architecture: [architecture/media-optimizer.md](../architecture/media-optimizer.md).
API: [api/media.md](../api/media.md).

> **Optional self-host component.** The encoder is opt-in. A self-hoster who
> wants image optimization runs `docker compose --profile media up`; one who
> doesn't simply omits the profile and their core API stays a minimal static
> binary. See [install.md](../install.md).

## The Media tab

Each site detail page has a **Media** tab with a four-step lifecycle:

| Step | What it does | Reversible |
|------|--------------|------------|
| **Sync** | Scans the WP media library and mirrors each attachment (id, path, URL, size, mime) into the dashboard. Nothing is changed on disk. | n/a |
| **Optimize** | Encodes the selected (or all-pending) attachments and their registered sizes to the chosen format/quality, then applies the results on the site. | Yes — Restore |
| **Restore** | Reverses an optimization: deletes the optimized files, puts the original bytes back, and reverses the DB URL rewrite. | n/a |
| **Delete originals** | Permanently removes the archived originals for already-optimized assets. **Irreversible** — restore is no longer possible afterwards. | **No** |

The table shows per-asset status, current format, bytes saved, and per-size
reasons for anything that couldn't be optimized. Progress streams live over SSE
— no refresh.

## Formats & quality

| Source | Target options | Default (lossy) | Lossless mode |
|--------|----------------|-----------------|---------------|
| JPEG / PNG | **AVIF** | quality 50, speed 6 | quality 100 |
| JPEG / PNG | **WebP** | quality 80 | quality 100 |
| JPEG | **original** (re-compress) | quality 82, progressive (mozjpeg-class) | quality 95 |
| PNG | **original** (re-compress) | lossless, max compression | lossless, max compression |

The source format is detected from the file's **magic bytes**, never the mime
WordPress claims. Only `image/jpeg` and `image/png` are optimizable; anything
else is recorded `excluded` with a reason. One optimize job covers one
attachment and all its registered sizes (full, thumbnail, medium, large, …),
capped at 10 variants per job. A failed size surfaces a human reason and does
**not** block its siblings.

## Browser compatibility (the Accept-header fallback)

AVIF and WebP are not understood by every browser. WPMgr handles this the same
way the source URL stays modern but degrades transparently:

- When you optimize **JPEG → AVIF/WebP** (a different extension), the new
  `banner.avif` is written **alongside** the untouched `banner.jpg`, and the
  database/HTML is rewritten to reference `banner.avif`.
- The agent installs an **`.htaccess` Accept-header rule**: if the requesting
  browser does *not* advertise AVIF/WebP support **and** a legacy twin exists on
  disk, Apache serves `banner.jpg` instead — same URL, transparent downgrade.
  `Vary: Accept` keeps shared caches/CDNs from cross-poisoning clients.
- On **nginx** (no `.htaccess`), the agent does not edit server config; it shows
  an admin notice with the equivalent `location` snippet to paste in.

The original IS the fallback, which is why a different-extension optimization
keeps it on disk. See [agent.md → Media Optimizer](../agent.md#media-optimizer).

## Originals are kept and restorable

Until you explicitly **Delete originals**, every optimized asset is reversible:

- **Different extension (JPEG → AVIF/WebP):** the original `.jpg` is never moved;
  Restore deletes the optimized file and reverses the URL rewrite.
- **Same extension (re-compress to original format):** the original bytes are
  archived on disk as `banner.wpmgr-original.jpg` before the optimized bytes are
  written in place; Restore deletes the optimized file and un-renames the
  archive.

The pre-optimization `_wp_attachment_metadata` snapshot is kept verbatim on the
site as the restore anchor. **Delete originals** removes those archives and sets
the asset to `originals_deleted` — Restore is then refused.

## Permissions (RBAC)

| Action | Minimum role | Permission |
|--------|--------------|------------|
| View Media tab | viewer | `site:read` |
| Sync / Optimize / Restore / Cancel | **operator** | `site:write` |
| **Delete originals** (irreversible) | **admin** | `media:delete_originals` |

Delete-originals is gated above the operator level and requires a type-the-
hostname confirmation in the UI; the consenting actor is recorded in the
hash-chained audit log. Site-scoped collaborators are gated per-site on every
route.
