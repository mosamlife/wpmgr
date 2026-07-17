# Audit log

A tamper-evident, append-only record of every operator action across your
fleet, from **Audit** in the sidebar.

---

## What's recorded

Every consequential action any operator, API key, or the system itself
takes is written as one entry: who did it (or that the system did it
autonomously), what the action was, what it targeted, and a
metadata payload of the relevant details (never secrets or credential
values). Coverage spans the whole product, including:

- Authentication: logins, failed logins, logouts, registration, SSO.
- Membership and access: role changes, API key creation/revocation,
  per-site sharing, invitations.
- Site lifecycle: enrollment, connect/degrade/disconnect transitions,
  revoke, archive, restore, re-enrollment.
- One-click autologin: every mint and every consumption, correlated by a
  nonce ID (never the signed token itself).
- Backups and restores, update runs, and the File Manager's full read and
  write history (including denied attempts, at elevated severity for
  anything touching a sensitive path).
- Security and hardening: login-protection config, ban list changes,
  hardening toggles, site-user 2FA and password policy, hide-login
  settings, vulnerability dismiss/restore/remediate.
- Performance tooling: cache enable/disable/purge, the irreversible
  "delete everything," Database Cleaner actions, Search and Replace runs,
  database snapshots, object-cache config/enable/disable/flush/test.
- Media: sync/optimize/restore actions and the irreversible
  delete-originals consent.
- Tags: creating, renaming, merging, deleting a tag in the registry, and
  bulk tag apply across sites.
- Clients and reports: client create/update/delete, bulk site assignment,
  report-schedule changes, and report generation.
- Dashboard two-factor authentication: every enroll, verify, failure,
  disable, and recovery-code regeneration (see
  [2fa.md](./2fa.md)).
- On the hosted service, every superadmin billing action, always recorded
  under the affected tenant's own chain, with the real operator's identity
  and a required reason, never a generic system entry.

---

## The integrity model, in plain terms

Each entry is cryptographically chained to the one before it: its stored
hash is computed from its own contents plus the previous entry's hash.
Altering, deleting, or inserting a row anywhere in the tenant's history
changes what that recomputation produces, so it no longer matches what
was stored, and the break is detectable at any later point, not only if
someone happens to notice. On top of that, the database role the
application runs as has `UPDATE` and `DELETE` revoked on the audit table
entirely, so the log is append-only at the database privilege level, not
merely by application convention.

**Verify.** Click **Verify integrity** to recompute the chain and confirm
it's intact. If a break is found, the badge names the first entry where
the recomputed hash stopped matching, the "Chain break" dialog explains
what that means and lets you recheck.

**A break is not automatically an attack.** The most common real-world
cause is two entries recorded at the exact same instant by a race that a
fix has since closed (a per-tenant lock now serializes every append
specifically to prevent this). Because the log is append-only, a
historical break can never be repaired in place, and a recheck will
always report the same result. The resolution is to acknowledge it and
move the verification anchor forward (re-baseline): from that point on,
Verify only walks entries written after the anchor. Re-baselining never
alters or deletes the flagged (or any other) row, everything remains
exactly as originally written for forensic review, and the re-baseline
action is itself recorded as a normal chained entry, so the acknowledgment
lives in the same tamper-evident trail. Re-baselining requires the owner
role specifically.

---

## Filtering and reading the log

The log reads newest first, grouped by day, with a run of near-identical
consecutive reads collapsed so a burst of routine activity doesn't bury
the entries that matter. Two filters are server-side and can be combined:
an action-prefix preset (File manager, Backups, Restores, Updates,
Security, 2FA, SMTP, Cache, or all events) and a specific site. A further
client-side filter narrows the current page by outcome (all, denied,
writes, sensitive, or reads) plus free-text search. Reading the log
requires an admin role or higher.

---

## Retention

There is no automatic, time-based purge: entries accumulate indefinitely
and the application has no privilege to delete them. The only way audit
history for a tenant is removed is as an explicit, all-or-nothing part of
deleting that organization itself, never a partial or scheduled trim.
