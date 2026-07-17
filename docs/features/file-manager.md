# File Manager

Browse, read, edit, upload, and manage files on a managed WordPress site
directly from the dashboard, no SFTP or shell access required.

---

## What it is

The Files tab on a site (`Site → Files`) is a per-site file browser backed by
signed commands to the agent. Every operation, from listing a directory to
extracting a zip, is a single Ed25519-signed CP-to-agent command; the agent
enforces the same guards independently, so the control plane is never the
sole line of defense.

---

## Safety model

- **Off by default, per site.** A site has no file access at all until an
  admin explicitly turns it on for that site.
- **Read and write are separate toggles.** Turning on the browser only grants
  read access (`enabled`). A second, independent toggle (`write_enabled`)
  turns on edit, upload, and delete affordances. Write requires read to
  already be on.
- **Owner/admin only.** Browsing, reading, and ordinary writes (edit, upload,
  rename, mkdir, chmod) require an admin-or-above role. Deleting a file, and
  writing, renaming, uploading, or restoring anything the deny-lists below
  flag as executable or sensitive, requires the owner role specifically; a
  caller who isn't an owner is rejected before the agent is ever contacted,
  and the denial is written to the audit log.
- **Every command is Ed25519-signed.** Each file operation is a short-lived,
  signed command scoped to one site and one action, the same transport used
  for backups and updates. There is no separate, unauthenticated file API.
- **Path jail.** Every path is resolved against a jail root (the site's
  `ABSPATH` by default) with segment checks (no `..`, no NUL bytes), a
  `realpath()` containment check, and a symlink guard. A path that resolves
  outside the jail is rejected, not silently clamped.
- **Executable-extension deny-list.** Writes, renames, uploads, and zip
  extractions are blocked when the target name matches a deny-list covering
  PHP variants (`php`, `php3` through `php9`, `phtml`, `pht`, `phar`,
  `phpt`, and more), `.htaccess`, `.htpasswd`, `.ini`, and ASP/JSP/CGI
  extensions, including double-extension tricks like `shell.php.jpg` or a
  trailing dot. Text content is additionally scanned for a PHP open tag
  (`<?php`, `<?=`, or a bare `<?` not followed by `xml`), so renaming a PHP
  payload to `.txt` doesn't bypass the check. Overriding the block requires
  the owner role and an explicit confirmation flag from the UI.
- **Sensitive-file deny-list.** Reading, downloading, archiving, writing, or
  restoring a version of `wp-config.php` (and its backup variants),
  `.env*`, certificate/key files (`.pem`, `.key`, `.crt`, `.p12`, `.pfx`,
  `.ppk`), SSH private keys, `.htpasswd`, `auth.json`, `.npmrc`,
  `.git-credentials`, anything under `.aws/credentials`, or anything inside
  a `.git/` directory requires owner permission and an explicit
  confirmation. Both the control plane and the agent enforce this list
  independently.
- **Protected core roots.** `wp-admin/`, `wp-includes/`, and the site's core
  bootstrap files (`wp-login.php`, `wp-settings.php`, `wp-load.php`,
  `wp-blog-header.php`), plus the active theme's directory and the WPMgr
  agent's own plugin directory, refuse deletion outright.
- **Versioned edits with restore.** Every successful write over an existing
  file first copies the previous content into an AES-256-GCM-encrypted
  per-file backup before the new bytes land. Restoring an old version is a
  one-click action from a per-file history panel.
- **Full audit trail.** Every read, write, delete, upload, mkdir, rename,
  chmod, archive, extraction, search, version list, and version restore is
  written to the operator audit log, including denied attempts (missing
  confirmation, insufficient permission, or an agent-side rejection). Reads
  and writes touching a sensitive path are logged at an elevated severity
  with the full path, distinct from ordinary file activity.

---

## Enabling it

1. Open a site and go to the **Files** tab.
2. An admin clicks **Enable** to turn on read access.
3. To allow edits, uploads, deletes, and other write operations, toggle
   **Write mode** from the browser toolbar (also admin+; the underlying
   permission split is finer, see below).

Both flags live per site: enabling the browser on one site has no effect on
any other site.

---

## What you can do

With read access enabled:

- **Browse** directories one level at a time (directories sorted first,
  then files, case-insensitively), with cursor-based pagination for large
  directories.
- **Read** a file's content inline.
- **Download** a file. The control plane stages it via presigned
  object-storage URLs so large files never round-trip through the CP's own
  response path.
- **Search** by filename or by content, under a chosen root path.
- **Archive** one or more paths into a zip and download it.
- **View version history** for a file (see below).

With write mode also enabled:

- **Edit** a file's content (atomic temp-write-then-rename; no partial
  writes are ever visible).
- **Upload** a file (chunked, checksum-verified, atomically swapped into
  place on completion).
- **Create a directory.**
- **Rename or move** a file or directory.
- **Change permissions** (the agent validates the requested mode against a
  safe allowlist: no setuid, no world-writable modes).
- **Delete** a file or directory. Owner-only; requires typing `DELETE` to
  confirm; refuses non-empty directories unless recursive delete is
  explicitly requested.
- **Extract a zip archive** into a chosen destination, with zip-slip and
  zip-bomb protection (see Limits below). Extraction is atomic: the archive
  is fully validated and expanded into a quarantine directory outside the
  web root first, then moved into place in one step, so a crash or a
  rejected entry never leaves a half-extracted tree.
- **Restore a prior version** of a file from its encrypted backup history.

---

## Limits

| Limit | Value |
|---|---|
| Inline read/write size | 256 KiB per file (larger files use the chunked upload/download path) |
| Chunked upload size | 160 MiB per file (32 chunks of 5 MiB each) |
| Directory listing page size | 1,000 entries per call (paginated beyond that) |
| Version history returned | 50 most recent versions per file |
| Zip extraction: entry count | 50,000 entries |
| Zip extraction: total uncompressed size | 1 GiB |
| Zip extraction: per-entry uncompressed size | 256 MiB |
| Zip extraction: compression ratio | rejected above 200:1 (zip-bomb heuristic) |
| Archive creation: total source size | 512 MiB |

---

## Troubleshooting

**"The file manager is not enabled for this site."**
Read access is off. An admin needs to turn it on from the Files tab.

**"The file manager write mode is not enabled for this site."**
Read access is on but write mode is off. Toggle Write mode from the Files
tab toolbar (admin+).

**"Writing executable or sensitive files requires owner-level permission",
or a 403 on delete, sensitive read, or version restore.**
That action requires the owner role specifically; admin is not enough for
destructive or executable/sensitive-path operations. Ask an owner to
perform it, or check the confirmation checkbox if you already hold the
owner role (the UI surfaces it once the deny-list match is detected).

**Upload or download is unavailable, or "object storage is not
configured".**
Uploads and downloads stage through presigned object-storage URLs. On a
self-hosted install this means S3-compatible storage (the bundled
SeaweedFS service, or your own) must be reachable, see
[install.md](../install.md#s3-networking). Inline read/edit of small files
does not need object storage and works regardless.

**A write or extraction was blocked as "executable".**
The target name or content matched the executable-extension deny-list (see
Safety model above). This is deliberate: writing PHP (or PHP disguised
with a double extension or a bare open tag) through the file manager is
the highest-risk action it exposes. An owner can override per-write with
the "I understand, write anyway" confirmation in the UI.
