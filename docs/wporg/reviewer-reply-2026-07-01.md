Hi, thanks for the re-review. A corrected version is uploaded. Notes on the flagged items:

**permission_callback (autologin, preload/run):** both are intentionally public endpoints — they run for unauthenticated / control-plane-triggered contexts, so a capability check does not apply — but I moved the real authorization into the permission_callback rather than `__return_true`. Autologin now verifies the token's Ed25519 signature, expiry, audience (the enrolled site), and command in the permission_callback (read-only); preload/run verifies its self-HMAC there with a constant-time comparison. The handlers still perform the authoritative verification and single-use consumption.

**Direct file access (advanced-cache.php):** the `if (!defined('ABSPATH')) exit;` guard is now the first statement, before the drop-in version `define()`.

**Unsafe SQL (mysqli in backup dump/restore/search-replace):** intentional and safe. `$wpdb` buffers the entire result set into PHP memory and OOM-fatals on large tables; these paths open a dedicated streaming connection instead, which `$wpdb` does not expose. No value is user-controlled: credentials come from the `wp-config` `DB_*` constants, identifiers are backtick-escaped, values are escaped (binary blobs hex-encoded), and table names are enumerated server-side. Each call carries an inline justification.

**File functions on remote files:** there are no remote `file_get_contents`/`fopen` calls in the plugin's own code. `class-task-runner.php` routes all http(s) through `wp_remote_get`; its `file_get_contents` is reached only for `file://` test fixtures. The `readfile` in the cache drop-in and the `file_get_contents` in the snapshot command read only local, plugin-generated files.

The slug (`fleet-agent-site-manager`), text domain, External Services documentation, and file-location helpers addressed in the previous round remain in place.

Thanks,
Mosam
