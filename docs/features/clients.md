# Clients, portal, and white-label reports

Group sites under agency customers, send them branded status reports, and
give them a read-only portal of their own, without giving them access to
your dashboard.

---

## Creating clients and assigning sites

A client is a tenant-level record: name, contact email, company, phone,
notes, a brand color, a logo URL, and a timezone (governs when scheduled
reports send, defaults to UTC). Create, edit, or delete clients from the
**Clients** page. Sites are assigned to a client by bulk "Set client" on
the fleet Sites view, or from a client's own detail page; a site belongs
to at most one client at a time, and unassigning is the same action with no
client chosen. Clients are tenant-isolated at the database level (Row-Level
Security plus a composite constraint), so an assignment can never cross
into another tenant's sites even by mistake.

Creating, editing, deleting, and assigning clients are all organization-
scoped actions: a collaborator whose access is limited to specific shared
sites cannot see or touch the client roster at all, only full organization
members with client-management permission can.

---

## White-label reports

A scheduled or one-off PDF/HTML status report for a client, covering every
site currently assigned to them.

**What's in it.** Six independently toggleable sections: an overview
summary, uptime, backups, updates, Core Web Vitals (performance), and
email deliverability. Each section aggregates across every site the client
owns for the report's period, plus a per-site breakdown.

**Scheduling.** A client's report schedule is weekly or monthly (weekly:
pick a day of week; monthly: pick a day of the month), with a send hour,
evaluated in the client's own configured timezone rather than the
tenant's. Recipients are an explicit email list, independent of client
portal membership. A **Generate now** action produces a report for any
custom period on demand, up to 92 days, without waiting for or disturbing
the schedule.

**Branding.** The client's brand color and logo appear throughout the
report. An operator can add custom intro and closing text, and the
"powered by WPMgr" footer line can be removed entirely.

**Delivery.** A generated report renders to both HTML (emailed directly to
the recipient list, and viewable as a print-optimized page) and a PDF with
server-rendered vector charts (not rasterized screenshots), stored in
object storage and downloadable later from the report history.

---

## The client portal

A separate, read-only area at `/portal` for the client's own people, with
their brand color and logo applied, entirely distinct from the operator
dashboard.

**What a client sees:**
- A **summary** landing page: a status banner, headline counters (sites
  monitored, average uptime, backups, updates applied, a speed rating),
  a month-at-a-glance section (fleet uptime trend and Core Web Vitals
  distribution), per-site cards with 30-day sparklines, and a day-grouped
  "recent work" timeline.
- A **sites** view listing every site assigned to the client, with
  deliberately softened status wording ("Monitoring active" instead of a
  raw connection state, "Needs attention" instead of a technical error),
  per-site uptime and incident history, backup inventory, applied updates,
  and Core Web Vitals field data.
- A **reports** view of every completed white-label report generated for
  them, downloadable.

Everything shown is filtered strictly to the client's own assigned sites;
there is no cross-client visibility even for an agency with many clients
in the same tenant.

**The invite flow.** From a client's **Portal access** tab, add a member
by email. If that email already belongs to an existing WPMgr user account,
they're added to the portal immediately with no email sent. If it's a new
address, an invitation is created: a single-use, 7-day-expiry link is
emailed to them (the invite screen also shows a copyable fallback link in
case email delivery is unavailable). Accepting the link creates their
account, scoped only to portal access for that client. Pending invitations
can be listed, revoked, or regenerated (which mints a fresh 7-day link and
invalidates the old one) from the same tab.

**RoleClient scoping.** A portal member's role, `client`, is deliberately
the lowest-ranked role in the system and, unlike every other role, holds
*zero* entries in the standard permission matrix: a client principal
cannot satisfy any regular operator permission check no matter what, by
construction. Portal routes are gated by a completely separate check
(client-membership derived from the session, requiring the exact `client`
role and at least one assigned client) rather than the normal role-minimum
checks every operator route uses. This means a bug that accidentally
under-restricts an operator route can never accidentally expose it to a
portal user, since a client principal fails a different gate entirely, not
merely a lower bar on the same one. Removing a member, archiving a client,
or deleting a client revokes portal access immediately.

---

## Privacy boundaries

What a client can never see, by design:

- **Any site outside their own assignment.** Every portal query is scoped
  to the client's own site list; there is no client-facing endpoint that
  accepts an arbitrary site ID.
- **The operator dashboard, or any of its data.** The portal is a
  completely separate route tree with its own gate; a client account
  cannot log into `/sites`, `/backups`, `/audit`, or any other operator
  surface, even by guessing a URL.
- **Raw technical detail.** Portal copy is deliberately softened (status
  labels, incident summaries) rather than exposing the same
  operator-facing diagnostic detail the dashboard shows.
- **Other clients in the same tenant**, or the tenant's own
  organization-level settings, members, billing, or audit log.
- **Credentials of any kind.** Backup destination secrets, SMTP passwords,
  object-cache passwords, and API keys are never surfaced anywhere in the
  portal; the portal has no settings surface for any of them.
