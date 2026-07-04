// Action label map + severity classifier for the fleet audit log.
//
// The action-key vocabulary is control-plane owned (apps/api/internal/audit/
// audit.go's Action* constants, plus each domain's audit.Record call sites —
// files/model.go, backup/worker.go, security/handler.go, scan/worker.go,
// and about a dozen others). This module mirrors every key emitted as of this
// writing so the log reads as plain verbs instead of raw dotted event names.
//
// Two rules this module exists to enforce:
//   1. A raw dotted action key NEVER becomes the visible row title. Known
//      keys get a hand-written label; unknown/future keys get a clean
//      sentence-case fallback derived from the key (see unknownActionLabel).
//      The raw key still surfaces in a tooltip and in the row's detail panel.
//   2. Severity is a first-class classification (denied / sensitive / write /
//      read), not a narrow "is this a delete" check. Reads are the quiet
//      default; genuine mutations and denials are the ones that stand out.

export type AuditSeverity = "denied" | "sensitive" | "write" | "read";

const ACTION_LABELS: Record<string, string> = {
  // Auth & session
  "auth.login.success": "Signed in",
  "auth.login.failure": "Failed sign-in attempt",
  "auth.logout": "Signed out",
  "auth.register": "Registered account",
  "auth.oidc.login": "Signed in with SSO",

  // Members & organization
  "member.add": "Added member",
  "member.update": "Updated member",
  "member.remove": "Removed member",
  "member.role_changed": "Changed member role",
  "member.removed": "Removed member",
  "member.invited": "Invited member",
  "org.created": "Created organization",
  "org.renamed": "Renamed organization",

  // API keys
  "apikey.create": "Created API key",
  "apikey.revoke": "Revoked API key",

  // Sites & connection lifecycle
  "site.create": "Added site",
  "site.delete": "Deleted site",
  "tenant.create": "Created account",
  "site.enrolled": "Enrolled site",
  "pairing_code.created": "Created pairing code",
  "site.tags.set": "Updated site tags",
  "site.connected": "Site connected",
  "site.degraded": "Site connection degraded",
  "site.disconnected": "Site disconnected",
  "site.revoked": "Revoked site connection",
  "site.archived": "Archived site",
  "site.restored": "Restored site",
  "site.reenrolled": "Re-enrolled site",

  // Updates
  "update.refresh.requested": "Requested update refresh",
  "update.refresh.unsupported": "Update refresh unsupported",
  "update.run.created": "Started update run",
  "update.task.succeeded": "Applied update",
  "update.task.failed": "Update failed",
  "update.task.rolled_back": "Rolled back update",

  // One-click login
  "autologin.requested": "Requested one-click login",
  "autologin.consumed": "Used one-click login",
  "autologin.failed": "One-click login failed",

  // Media optimizer
  "media.sync.started": "Started media sync",
  "media.optimize.started": "Started media optimization",
  "media.restore.started": "Started media restore",
  "media.delete_originals.confirmed": "Deleted original media files",
  "media.cancelled": "Cancelled media job",
  "media.settings.updated": "Updated media settings",

  // Performance / page cache
  "site.cache.enabled": "Enabled cache",
  "site.cache.disabled": "Disabled cache",
  "site.cache.purged": "Purged cache",
  "site.cache.delete_everything": "Deleted all cache data",
  "site.perf.config.updated": "Updated performance settings",

  // Database tools
  "site.db.cleaned": "Cleaned database",
  "site.db.table.action": "Ran database table action",
  "site.db.orphan.delete": "Deleted orphaned data",
  "site.db.search.replace": "Ran search and replace",
  "site.db.snapshot": "Managed database snapshot",

  // Media cleaner
  "site.media.clean.scan": "Scanned unused media",
  "site.media.clean.isolate": "Quarantined media files",
  "site.media.clean.restore": "Restored quarantined media",
  "site.media.clean.delete": "Deleted quarantined media",
  "site.media.clean.quarantine": "Viewed media quarantine",

  // Email
  "site.email.config.updated": "Updated email settings",
  "site.email.log.resent": "Resent email",
  "site.email.log.deleted": "Deleted email log entries",
  "site.email.suppression.added": "Added email suppression",
  "site.email.suppression.deleted": "Removed email suppression",
  "smtp.settings.update": "Updated SMTP settings",

  // Agency clients
  "client.created": "Created client",
  "client.updated": "Updated client",
  "client.deleted": "Deleted client",
  "client.sites.assigned": "Assigned sites to client",
  "client.report_schedule.updated": "Updated report schedule",
  "client.report.generated": "Generated report",
  "client.report.deleted": "Deleted report",
  "client_member.accepted": "Accepted client invite",
  "client_member.invited": "Invited client member",
  "client_member.added": "Added client member",
  "client_member.removed": "Removed client member",
  "client_member.invite_revoked": "Revoked client invite",
  "client_member.invite_regenerated": "Regenerated client invite",

  // Object cache
  "site.objectcache.config.updated": "Updated object cache settings",
  "site.objectcache.enabled": "Enabled object cache",
  "site.objectcache.disabled": "Disabled object cache",
  "site.objectcache.flushed": "Flushed object cache",
  "site.objectcache.tested": "Tested object cache",

  // File manager
  "site.files.read": "Read file",
  "site.files.sensitive.read": "Read sensitive file",
  "site.files.sensitive.denied": "Blocked sensitive file access",
  "site.files.settings.changed": "Changed file manager settings",
  "site.files.write": "Edited file",
  "site.files.mkdir": "Created folder",
  "site.files.rename": "Renamed file",
  "site.files.delete": "Deleted file",
  "site.files.delete.denied": "Blocked file deletion",
  "site.files.chmod": "Changed file permissions",
  "site.files.upload": "Uploaded file",
  "site.files.write_code": "Edited code file",
  "site.files.write_code.denied": "Blocked code file edit",
  "site.files.archive": "Created archive",
  "site.files.archive.sensitive.read": "Archived sensitive file",
  "site.files.archive.sensitive.denied": "Blocked sensitive file archive",
  "site.files.extract": "Extracted archive",
  "site.files.extract.denied": "Blocked archive extraction",
  "site.files.search": "Searched files",
  "site.files.versions.list": "Viewed file version history",
  "site.files.versions.list.denied": "Blocked version history access",
  "site.files.version.restore": "Restored file version",
  "site.files.version.restore.sensitive": "Restored sensitive file version",
  "site.files.version.restore.denied": "Blocked file version restore",

  // Dashboard 2FA
  "auth.2fa.totp.enrolled": "Enabled authenticator app",
  "auth.2fa.totp.disabled": "Disabled authenticator app",
  "auth.2fa.totp.verified": "Verified authenticator code",
  "auth.2fa.totp.failed": "Failed authenticator code",
  "auth.2fa.recovery_codes.regenerated": "Regenerated recovery codes",
  "auth.2fa.recovery_code.used": "Used a recovery code",
  "auth.2fa.webauthn.credential.added": "Added passkey",
  "auth.2fa.webauthn.credential.removed": "Removed passkey",
  "auth.2fa.webauthn.verified": "Verified passkey",
  "auth.2fa.webauthn.failed": "Failed passkey verification",
  "auth.2fa.webauthn.cloned_authenticator": "Blocked a cloned passkey",
  "auth.2fa.challenge.issued": "Issued 2FA challenge",
  "auth.2fa.challenge.expired": "2FA challenge locked out",
  "auth.2fa.trusted_device.added": "Trusted this device",
  "auth.2fa.trusted_device.revoked": "Revoked a trusted device",
  "auth.2fa.trusted_device.revoked_all": "Revoked all trusted devices",

  // Vulnerabilities
  "site_vuln.rescan": "Rescanned for vulnerabilities",
  "site_vuln.dismiss": "Dismissed vulnerability",
  "site_vuln.restore": "Restored dismissed vulnerability",
  "site_vuln.remediate": "Remediated vulnerability",

  // Sharing
  "share.granted": "Granted site access",
  "share.invited": "Invited collaborator",
  "share.revoked": "Revoked site access",
  "share.invitation_revoked": "Revoked invitation",
  "share.invitation_regenerated": "Regenerated invitation",
  "share.accepted": "Accepted site invitation",

  // Login branding
  "site_login_brand.update": "Updated login branding",

  // Security
  "site_security_config.update": "Updated security settings",
  "site_security.unblock_ip": "Unblocked IP address",
  "site_security_hardening.update": "Updated hardening settings",
  "site_security_ban.create": "Created ban rule",
  "site_security_ban.delete": "Deleted ban rule",
  "site_security_policy.update": "Updated security policy",
  "site_security_policy_group.upsert": "Saved security policy group",
  "site_security_policy_group.delete": "Deleted security policy group",

  // Superadmin
  "admin.vuln_feed.key.set": "Set vulnerability feed key",
  "admin.vuln_feed.key.clear": "Cleared vulnerability feed key",
  "admin.vuln_feed.sync": "Synced vulnerability feed",

  // File integrity scanning
  "scan.created": "Scheduled file scan",
  "scan.started": "Started file scan",
  "scan.completed": "Completed file scan",
  "scan.failed": "File scan failed",
  "scan.file_fetched": "Fetched file for scan",
  "scan.baseline_established": "Established file baseline",
  "scan.file_change_detected": "Detected file changes",
  "scan_finding.baseline_advance_failed": "Baseline update failed",
  "scan_finding.ignore": "Ignored scan finding",

  // Backup destinations
  "site_destination.create": "Added backup destination",
  "site_destination.update": "Updated backup destination",
  "site_destination.delete": "Deleted backup destination",

  // Diagnostics / error monitoring
  "site_diagnostics.refresh": "Refreshed diagnostics",
  "php_error.silence": "Silenced PHP error",
  "site_error_config.update": "Updated error settings",

  // Backups & restores
  "backup.started": "Started backup",
  "backup.completed": "Completed backup",
  "backup.failed": "Backup failed",
  "backup.deleted": "Deleted backup",
  "backup.canceled": "Canceled backup",
  "restore.started": "Started restore",
  "restore.completed": "Completed restore",
  "restore.failed": "Restore failed",
  "backup.schedule.changed": "Changed backup schedule",

  // Uptime & alerts
  "uptime.alert.sent": "Sent downtime alert",
  "alert.config.changed": "Changed alert settings",
};

/** Turn "some.dotted_key" into "Some dotted key" — a dot never survives. */
function unknownActionLabel(action: string): string {
  const words = action
    .split(".")
    .join(" ")
    .replace(/_/g, " ")
    .trim()
    .replace(/\s+/g, " ");
  if (!words) return "Unknown event";
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/**
 * Human-readable label for an action key. Exact matches win first (this is
 * how a denied variant gets its own clear "Blocked ..." wording instead of a
 * generic "(denied)" suffix); a ".denied" suffix on an otherwise-unmapped key
 * recurses onto its base label; anything left over gets a clean, dot-free
 * sentence-case fallback. A raw dotted key is never the return value.
 */
export function actionLabel(action: string): string {
  const exact = ACTION_LABELS[action];
  if (exact) return exact;
  if (action.endsWith(".denied")) {
    const base = action.slice(0, -".denied".length);
    return `${actionLabel(base)} (denied)`;
  }
  return unknownActionLabel(action);
}

// ---------------------------------------------------------------------------
// Severity classification
// ---------------------------------------------------------------------------

/**
 * Explicit allowlist for the "sensitive" tier: security-, credential-, and
 * access-control-adjacent actions that deserve a distinct signal from an
 * ordinary write even though most of them aren't denials. This is a curated
 * list rather than a substring heuristic because getting this tier wrong in
 * either direction defeats the point of the redesign.
 */
const SENSITIVE_ACTIONS = new Set<string>([
  "site_security_config.update",
  "site_security.unblock_ip",
  "site_security_hardening.update",
  "site_security_ban.create",
  "site_security_ban.delete",
  "site_security_policy.update",
  "site_security_policy_group.upsert",
  "site_security_policy_group.delete",
  "smtp.settings.update",
  "site.cache.disabled",
  "site.files.sensitive.read",
  "site.files.archive.sensitive.read",
  "site.files.version.restore.sensitive",
  "site_vuln.dismiss",
  "scan.baseline_established",
  "scan.file_change_detected",
  "scan_finding.ignore",
  "admin.vuln_feed.key.set",
  "admin.vuln_feed.key.clear",
  "apikey.create",
  "apikey.revoke",
  "site_destination.create",
  "site_destination.update",
  "site_destination.delete",
  "auth.login.failure",
  "auth.2fa.totp.disabled",
  "auth.2fa.totp.failed",
  "auth.2fa.webauthn.failed",
  "auth.2fa.webauthn.credential.removed",
  "auth.2fa.webauthn.cloned_authenticator",
  "auth.2fa.challenge.expired",
  "auth.2fa.recovery_code.used",
  "auth.2fa.trusted_device.revoked",
  "auth.2fa.trusted_device.revoked_all",
  "member.role_changed",
  "share.granted",
  "share.revoked",
  "share.accepted",
]);

// Forces "write" for keys the stem heuristic below cannot see the verb of
// (the action string alone doesn't carry the mutating verb) or where the
// blast radius is large enough that defaulting to "read" would be wrong.
const WRITE_OVERRIDES = new Set<string>([
  "site.db.table.action",
  "site.db.search.replace",
  "site.db.snapshot",
]);

// The heuristic below would otherwise flag these as writes (they contain a
// write-shaped segment) even though the control plane documents them as
// read-only listing/preview endpoints.
const READ_OVERRIDES = new Set<string>([
  "site.media.clean.scan",
  "site.media.clean.quarantine",
]);

// Verb stems recognized as a mutation regardless of domain. Matched against
// each dot/underscore-separated segment as a prefix so both base and
// past-tense forms hit ("delete" and "deleted", "restore" and "restored").
const WRITE_STEMS = [
  "writ", // write, write_code
  "upload",
  "creat", // create, created
  "delet", // delete, deleted
  "restor", // restore, restored
  "extract",
  "chmod",
  "renam", // rename, renamed
  "mkdir",
  "archiv", // archive, archived
  "purg", // purge, purged
  "flush",
  "enroll", // enroll, enrolled
  "reenroll",
  "revok", // revoke, revoked
  "cancel",
  "assign",
  "confirm",
  "regenerat", // regenerate, regenerated
  "isolat", // isolate, isolated
  "clean", // clean, cleaned
  "unblock",
  "disabl", // disable, disabled
  "enabl", // enable, enabled
  "updat", // update, updated
  "chang", // change, changed
  "remov", // remove, removed
  "add", // add, added
  "invit", // invite, invited
  "roll", // rolled_back
  "start", // started
  "silenc", // silence, silenced
  "remediat", // remediate, remediated
];

// Matched as a whole segment (not a prefix) to avoid false hits like
// "settings".startsWith("set").
const WRITE_EXACT_SEGMENTS = new Set<string>(["set"]);

function isWriteAction(action: string): boolean {
  const segments = action.split(/[._]/).filter(Boolean);
  return segments.some(
    (seg) =>
      WRITE_EXACT_SEGMENTS.has(seg) ||
      WRITE_STEMS.some((stem) => seg.startsWith(stem)),
  );
}

/**
 * Classify an action into the four severity tiers that drive the row's rail
 * color, pill, and read-burst collapsing eligibility:
 *
 *   denied    — any ".denied" action. Always the loudest signal.
 *   sensitive — security/credential/access-control actions (curated list).
 *   write     — a mutation: create/update/delete/restore/etc.
 *   read      — everything else: the quiet default (list/search/verify/
 *               status-report events). Only "read" rows are ever eligible
 *               for the same-actor/action/site run collapsing in group-runs.ts.
 */
export function classifySeverity(action: string): AuditSeverity {
  if (action.endsWith(".denied")) return "denied";
  if (SENSITIVE_ACTIONS.has(action)) return "sensitive";
  if (READ_OVERRIDES.has(action)) return "read";
  if (WRITE_OVERRIDES.has(action) || isWriteAction(action)) return "write";
  return "read";
}
