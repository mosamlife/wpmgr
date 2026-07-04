Thanks. This was an automated re-scan, so the per-item rationale for the intentional patterns is already in my previous reply in this thread and still applies: the streaming `mysqli` connection (needed because `$wpdb` buffers the whole result set and OOM-fatals on large-table backups), the backup/restore use of `ABSPATH`/`WP_CONTENT_DIR` (a backup tool has to resolve the real site tree; writable plugin data goes under `wp_upload_dir()`), the page-cache inline output (written to a static file served before WordPress loads, so `wp_enqueue` never runs for that response), and `FORCE_SSL_ADMIN` (defined only when the owner enables the Force-SSL toggle, behind a `!defined()` guard).

One flagged item was a real one and I fixed it: the 2FA login interstitial's **resume** path now binds to the originating browser (an HttpOnly, session-scoped cookie verified before anything renders), so a pending setup screen can no longer be shown from a guessed user ID.

On the other three:

- **`.maintenance`** is WordPress core's own maintenance sentinel (`<?php $upgrading = …`), written only during an active restore to show the standard maintenance page, and removed on completion. It is the same file core writes during its own updates, not plugin code.
- The operator **file-write / restore** commands write only inside a realpath jail (the resolved target must stay within the jail root, with an executable-extension deny-list and a `<?php` content sniff), and every command is gated by a single-use, short-lived Ed25519-signed token verified server-side.
- The plugin **does not create users**. The login flow is an operator-initiated, one-click login into an existing administrator, carried by a single-use signed token; the 2FA step sets the auth cookie only after WordPress has already authenticated the user. This mirrors the established management plugins (MainWP Child, ManageWP Worker).

Happy to clarify any specific line.

Thanks,
Mosam
