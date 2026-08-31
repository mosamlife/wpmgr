import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { SITE_CONFIG } from "@/lib/site";

export const revalidate = 3600;

export const metadata: Metadata = buildMetadata({
  title: "Changelog: WPMgr Release Notes",
  description:
    "Every WPMgr release, newest first. See what shipped, when, and which features were added, changed, or fixed.",
  canonical: "/changelog",
});

// ---------------------------------------------------------------------------
// Curated release entries (newest first, harvested from CHANGELOG.md).
// This is the whole curated history rather than a recent window: it runs back
// to 0.54.0, with some entries grouping several releases into one. Anything
// older lives on GitHub Releases.
// The total is deliberately not written down here. It is derived from RELEASES,
// so a number in this comment is right only until the release that forgets to
// bump it. Count it when you actually want it, anchored on the entry indent so
// the ChangeEntry type's own field is not counted as an entry:
//   grep -cE '^    version:' 'apps/marketing/app/(marketing)/changelog/page.tsx'
// CI keeps the newest entry within 5 releases of the top CHANGELOG.md entry
// (scripts/check-version-surfaces.sh), reading the newest version in a grouped
// entry rather than the first one on the line. That guard reads the first
// quoted version field in the whole file, so nothing above RELEASES may carry
// one, which is why the line above does not quote the value it matches.
// ---------------------------------------------------------------------------

type ChangeTag = "Added" | "Changed" | "Fixed" | "Security";

type ChangeEntry = {
  version: string;
  date: string;
  summary: string;
  items: Array<{ tag: ChangeTag; text: string }>;
  featureLinks?: Array<{ label: string; href: string }>;
};

const TAG_COLOR: Record<ChangeTag, string> = {
  Added: "var(--success, oklch(55% 0.15 145))",
  Changed: "var(--info, oklch(55% 0.12 235))",
  Fixed: "var(--warning, oklch(65% 0.15 75))",
  Security: "var(--destructive)",
};

const RELEASES: ChangeEntry[] = [
  {
    version: "0.61.150",
    date: "2026-08-31",
    summary:
      "The MCP consent screen now states the truth about how long a connection lasts, instead of claiming it never expires on its own.",
    items: [
      {
        tag: "Fixed",
        text: "The consent screen now states the truth about how long a connection lasts. It previously said the connection does not expire on its own and that the key a client holds is short-lived and renews itself. Every grant carries a 90-day absolute expiry, enforced by the control plane, and the screen now shows the term it is actually consenting to instead of a claim computed in the browser.",
      },
    ],
  },
  {
    version: "0.61.149",
    date: "2026-08-30",
    summary:
      "Connection tokens can now be minted straight from the dashboard, for the cases the browser sign-in flow cannot reach: CI, an SSH session, a container. Grants carry an expiry, an idle expiry and a capability set, the connections list now shows real activity instead of always reading \"never used,\" and every grant, revocation and tool call is now audited under a new assistant actor kind.",
    items: [
      {
        tag: "Added",
        text: "Connection tokens can now be minted directly from the dashboard: a new endpoint plus a wizard that walks an operator through picking a client, naming the connection, choosing which sites it may reach, choosing how it authenticates, and revealing the token exactly once.",
      },
      {
        tag: "Added",
        text: "A site-access explanation on the consent screen, stating plainly that scoping is a check made at the moment the assistant asks rather than a boundary enforced inside the database, and that a site added to a scoped tag later is included without anyone approving it.",
      },
      {
        tag: "Changed",
        text: "Grants now carry an expiry, an idle expiry and a capability set, all enforced inside the same authorization check that already refused a revoked grant.",
      },
      {
        tag: "Changed",
        text: "The connections list now records real activity. It previously reported every connection as \"never used,\" including one actively reading the fleet; it now reports the truth.",
      },
      {
        tag: "Changed",
        text: "The MCP surface now writes audit events for grant creation, revocation and tool calls, under a new assistant actor kind, inside the same transaction as the thing they record.",
      },
      {
        tag: "Fixed",
        text: "A connection whose grant holds no capability now answers with a permission error rather than an authentication error, so a client stops re-running a handshake that could not change the outcome. An empty site allowlist is now a valid thing to approve, and a tag selection that only partly resolves is refused rather than silently narrowed.",
      },
    ],
  },
  {
    version: "0.61.148",
    date: "2026-08-30",
    summary:
      "The AI connection surface is now usable end to end. There is a page for it in the dashboard, a wizard that writes the configuration block for the client you pick, a list of what is connected and when it was last used, and revoke. OAuth discovery documents let a client with no endpoint field find the authorization endpoints for itself, and the bundled proxy now routes the paths those clients need. Self-hosted operators running their own reverse proxy have one routing change to make; the changelog entry lists the exact paths.",
    items: [
      {
        tag: "Added",
        text: "A dashboard home for AI connections, with a connection wizard. Pick your client and the wizard generates the configuration block for it. The per-client differences come from a tested table rather than hand-written snippets, each entry carrying the date it was last verified, and a configuration shape we have no source for is refused rather than guessed at.",
      },
      {
        tag: "Added",
        text: "A connections list showing which clients are connected, what each negotiated, and when it was last used, plus revoke. A client that has never connected reads differently from one that connected without declaring a protocol version, and a list that failed to load is distinguishable from an organisation that has no connections yet.",
      },
      {
        tag: "Added",
        text: "OAuth discovery documents, so a client with no field in which to be told where to authorize can find the authorization endpoints for itself. The issuer is derived from the configured public address rather than a fixed value, so a self-hosted install advertises its own origin; an install with that value unset returns an error naming the setting rather than a document pointing somewhere real and wrong.",
      },
      {
        tag: "Changed",
        text: "Revoking a connection now revokes its tokens in the same step, so the client stops working on its next request rather than continuing until its token expires.",
      },
      {
        tag: "Changed",
        text: "The bundled reverse proxy and the development proxy now route the AI connection endpoint and the discovery documents. These sit at the root rather than under the API prefix and were previously answered by the dashboard itself, so an endpoint copied out of the wizard returned a web page. Self-hosted operators running their own proxy in place of the bundled one need to forward those paths; the install guide lists all four and explains why one of them is easy to miss.",
      },
      {
        tag: "Added",
        text: "The production routing configuration now lives in the repository, with a check on every change that proves each route the API actually serves has a rule pointing at it. The route list is read from the running engine rather than kept by hand, so a route that ships unreachable is caught before the deploy rather than after it.",
      },
      {
        tag: "Security",
        text: "Organisation-scope enforcement is now applied to the AI connection authorization endpoints. Upgrading is recommended for any install with that surface reachable. The session secret rotation guidance from 0.61.147 still stands and is not superseded; if you are coming from 0.61.146 or earlier, read that entry's upgrade note before rotating.",
      },
    ],
  },
  {
    version: "0.61.147",
    date: "2026-08-30",
    summary:
      "A read-only AI connection surface, so an AI client can be granted scoped read access to a fleet behind an explicit consent screen. Governed per-site and organisation context arrives with it: five screens for writing the context an AI client reads, with version history, diff and restore. Also: network-activated plugins on a multisite network now report as active, the site inventory stops presenting \"never collected\" as a collection date, and authentication rate limits now derive the client address from the deployment's proxy configuration. Self-hosted operators should read the upgrade note in the changelog before updating.",
    items: [
      {
        tag: "Added",
        text: "A read-only AI connection surface. An AI client can be granted scoped read access to a fleet through a standard authorization flow, with an explicit consent screen before anything is granted. A client that asks for no recognised scope is refused rather than given a default, and every tool call is checked against the connection's own policy in its own right, so a tool that was filtered out of what a connection can see cannot be invoked by naming it directly.",
      },
      {
        tag: "Added",
        text: "The AI-connection consent screen shows what a client is asking for before you grant it, and marks a client's self-declared identity as unverified rather than presenting it as established.",
      },
      {
        tag: "Added",
        text: "Governed per-site and organisation context: five screens for the standing context an AI client reads, including an effective-context preview, separate site and organisation editors, and version history with diff and restore. Where a layer could not be loaded or had to be truncated, the screens say so rather than rendering a partial answer as a complete one.",
      },
      {
        tag: "Changed",
        text: "Authentication rate limits now derive the client address from the deployment's proxy configuration rather than assuming one topology. Self-hosters running the bundled deployment or their own reverse proxy should set the new proxy hop count deliberately; the install guide explains how to count it.",
      },
      {
        tag: "Changed",
        text: "Updating your own profile now returns your scope and role with it, so a client no longer needs a second call to learn what the account it just updated can do.",
      },
      {
        tag: "Fixed",
        text: "On a multisite network, a plugin activated for the whole network now reports as active in site metadata. It was reported inactive, so it went missing from the plugin inventory and from everything that reads it, including update and vulnerability checks.",
      },
      {
        tag: "Fixed",
        text: "The site inventory no longer presents \"never collected\" as though it were a collection date. Inventory age is stamped from when the component list was actually gathered rather than from the site's last heartbeat, a site with nothing gathered renders an explicit unknown state, and a failed load is now distinguishable from a genuinely empty inventory.",
      },
      {
        tag: "Security",
        text: "The control plane now validates its session secret at boot and refuses to start on a value unfit to hold confidentially, naming the setting in the error. Self-hosted operators upgrading should rotate that value as part of the upgrade; see the changelog entry for the one step to take first.",
      },
    ],
  },
  {
    version: "0.61.146",
    date: "2026-08-26",
    summary:
      "Breaking change: the application-password two-factor control now actually runs, so on a 2FA-enabled site an application password stops working for an enrolled or role-required user and returns HTTP 401. Also fixed: a database restore that could silently drop the dump's last statement, write into a site's live tables, or make a later resend send the wrong email, all while still reporting success; resending a failed outgoing email; a WordPress agent crash on every user profile edit; a user-agent ban that could lock administrators out of their own login page; and a cache toggle that could destroy a site's stored CDN credentials.",
    items: [
      {
        tag: "Security",
        text: "The application-password two-factor control now actually runs; it was registered against a name nothing in WordPress ever fires, so it silently never took effect on any site. Breaking change for integrations: on update, a 2FA-enabled site will have application passwords stop working for a user who has a second factor enrolled or whose role requires one.",
      },
      {
        tag: "Fixed",
        text: "A database restore could silently drop the dump's last SQL statement while still reporting success, when the restorer could not tell whether the very end of the dump was nothing but comments. Proven against two otherwise identical dumps that differed only in whether the last statement ended with a semicolon: the one that landed in the discarded tail lost a row, silently. It now aborts the restore instead of finishing on an uncertain guess.",
      },
      {
        tag: "Fixed",
        text: "A database restore could also write straight into a site's live tables while still reporting success, when the staging step could not tell which table a dump statement named. It now aborts rather than guessing, proven end to end against a live database with a sentinel row that survives the abort.",
      },
      {
        tag: "Fixed",
        text: "Resend could send a different email than the one selected, after a site's database restore, because the row id resend uses is a local auto-increment counter that a restore rolls back. wpmgr now confirms the row before sending and refuses the resend on a mismatch. A site running an older version of the plugin still resends exactly as it always has; it simply cannot be confirmed, and when it cannot the dashboard now shows the specific reason instead of a plain success, for one resend or a whole batch.",
      },
      {
        tag: "Fixed",
        text: "Resending a failed outgoing email now actually works. It previously failed on every attempt with an error shown verbatim to the operator. A failed attempt no longer inflates the resend counter or writes an audit entry claiming the email went out.",
      },
      {
        tag: "Fixed",
        text: "Editing a user's profile in wp-admin no longer crashes with a critical error, on every connected site.",
      },
      {
        tag: "Fixed",
        text: "The disk-size check behind Site Health's Directory sizes panel, the agent's own daily size walk, and multisite media uploads no longer crashes when its cache is cold, which is any fresh install or cache flush.",
      },
      {
        tag: "Fixed",
        text: "A database-cleanup scan for orphaned options could fail outright with a fatal error every time it ran. It now completes.",
      },
      {
        tag: "Fixed",
        text: "A generic, operator-defined user-agent ban could 403 a site's own login page, with no recovery path. The login page is now exempt from user-agent bans, and an overly short or generic pattern is now refused outright. The documented recovery constant also now releases the automatic login lockout.",
      },
      {
        tag: "Fixed",
        text: "Enabling or disabling the page cache, or rotating a site's beacon key, could destroy that site's stored CDN credentials on a transient read failure while still reporting success. Control-plane only, and already deployed.",
      },
    ],
    featureLinks: [{ label: "Email deliverability", href: "/features/email-deliverability" }],
  },
  {
    version: "0.61.144",
    date: "2026-08-22",
    summary:
      "An API key can now be scoped to exactly the access it needs instead of a whole role, and restricted to specific sites.",
    items: [
      {
        tag: "Added",
        text: "An API key can now be granted a specific set of capabilities instead of a whole role. Previously, a key that could read files could transitively manage members, mint further keys, read the audit log, and log into sites as a user. A key can now also be restricted to named sites. Existing keys keep exactly the access they have today.",
      },
      {
        tag: "Fixed",
        text: "The published API contract now correctly marks fields that can be null as nullable, instead of declaring them always present when the control plane can send a JSON null value.",
      },
    ],
  },
  {
    version: "0.61.143",
    date: "2026-08-20",
    summary:
      "Several control-plane reliability fixes: a stopped agent update run can no longer be reported as finished after the fact, a backup snapshot can no longer be left in an inconsistent state by two writes racing each other, and a rare bug that could leak a database connection's live-update subscription under load is fixed.",
    items: [
      {
        tag: "Fixed",
        text: "An agent update run that was stopped by the kill switch or a withdrawn release could later be overwritten to show as completed if work already in flight finished afterward. Operators now see the stopped state as final.",
      },
      {
        tag: "Fixed",
        text: "A backup snapshot's claim and its final outcome are now protected against being written out of order, which previously could strand the storage a failed backup used, or leave a good backup marked unusable.",
      },
      {
        tag: "Fixed",
        text: "A shared database connection could stay subscribed to another request's live update notifications after that request ended, letting notifications cross between unrelated connections under load. Fixed.",
      },
    ],
  },
  {
    version: "0.61.142",
    date: "2026-08-20",
    summary:
      "Fleet uptime now has a 90-day history view. Also fixed: paused sites are correctly skipped by scheduled updates, database cleaning and vulnerability scans; a site with no monitoring data shows as unmeasured instead of 0% uptime; and a lock-release bug that could stall backups, per-tenant storage cleanup and organization deletion for up to 30 minutes.",
    items: [
      {
        tag: "Added",
        text: "Fleet uptime now has a 90-day availability view, one entry per day, in place of a single derived 7-day figure.",
      },
      {
        tag: "Fixed",
        text: "A site paused after an update run was already scheduled is no longer dispatched to. Scheduled database cleaning and the vulnerability digest now respect a paused site the same way.",
      },
      {
        tag: "Fixed",
        text: "A site with no uptime measurement yet now reports as unmeasured instead of showing 0% uptime.",
      },
      {
        tag: "Fixed",
        text: "A lock-release bug that could silently leak a database lock is fixed. While leaked, it could fleet-wide stall scheduled backups, stall per-tenant storage cleanup, and block organization deletion or restore for up to 30 minutes.",
      },
      {
        tag: "Fixed",
        text: "A cleanup worker for old webhook records is now actually running; it existed in code but was never wired in.",
      },
    ],
  },
  {
    version: "0.61.141",
    date: "2026-08-19",
    summary:
      "Scheduled update runs now actually wait for their scheduled time instead of firing immediately, and can be canceled before they run.",
    items: [
      {
        tag: "Changed",
        text: "Scheduled update runs now wait for their scheduled time instead of dispatching immediately. Runs and tasks show new statuses (scheduled, dispatching, expired), and a run whose start time passed more than two hours ago is marked expired rather than dispatched late.",
      },
      {
        tag: "Added",
        text: "A scheduled update run can now be canceled any time before dispatch claims it. Once dispatch has started, use the existing halt control instead.",
      },
      {
        tag: "Fixed",
        text: "The self-hosted setup script could report failure on a successful, fully configured re-run. It now exits cleanly as it should.",
      },
    ],
  },
  {
    version: "0.61.140",
    date: "2026-08-19",
    summary:
      "Two operational changes for self-hosters. First-run ownership now requires a provisioning claim, and the control plane validates its update-timing configuration at startup rather than allowing a combination that could dispatch the same update twice. Also fixes a race in claiming an update task.",
    items: [
      {
        tag: "Changed",
        text: "The control plane now validates its update-timing configuration at startup and refuses to boot when the apply-timeout setting is high enough that the derived claim-staleness bound reaches the stale-task reaper's threshold, naming the setting to lower. The default has roughly 25 minutes of headroom, so no ordinary install is affected.",
      },
      {
        tag: "Changed",
        text: "Self-hosted installs now require a bootstrap claim secret to establish first-run ownership. scripts/init-env.sh generates it and both quickstart paths route through that script. An install that already has an owner is unaffected; one that was stood up and never claimed needs the operator to re-run the script or set the variable, then restart the api service.",
      },
      {
        tag: "Fixed",
        text: "Claiming an update task could race, letting two workers both dispatch the same task and apply one item to a site twice. Claiming a task is now a compare-and-swap against the row's own status.",
      },
    ],
  },
  {
    version: "0.61.139",
    date: "2026-08-17",
    summary:
      "Email delivery failure detection now works on sites that send through their own SMTP setup, not only sites that route mail through WPMgr, and the Notifications page states how many connected sites can actually report a failure. Separately, a bug in WPMgr's own mail routing that reported a failed send as successful is fixed, so forms and other flows now learn honestly when mail did not go out.",
    items: [
      {
        tag: "Added",
        text: "Email delivery failure detection now also listens to WordPress's own mail-failure signal, so a site is covered even when it sends through its own SMTP setup instead of through WPMgr. The Notifications page now states how many connected sites can actually report a failure, and says so plainly when none can, rather than promising coverage it cannot deliver.",
      },
      {
        tag: "Added",
        text: "A detected failure is now recorded even on a site that has email logging turned off. That preference controls whether successful sends are kept for review; a failure is an incident and is written regardless, though the recipient, subject, sender and message body are withheld unless the site has opted into email logging.",
      },
      {
        tag: "Fixed",
        text: "wp_mail() reported success on a failed send, so a contact form or password reset could tell a visitor the message went out when nothing was delivered. wp_mail() now returns false on a failed send and fires WordPress's own mail-failure hook, so forms and flows that check the result will start surfacing failures they were previously hiding.",
      },
    ],
    featureLinks: [{ label: "Email deliverability", href: "/features/email-deliverability" }],
  },
  {
    version: "0.61.138",
    date: "2026-08-16",
    summary:
      "The \"Monitoring paused\" badge was illegible in dark mode, an amber outline around text with almost no contrast against the card behind it, and on some sites the pill looked completely empty. It now renders with normal contrast in both themes.",
    items: [
      {
        tag: "Fixed",
        text: "The \"Monitoring paused\" badge was illegible in dark mode: an amber outline around text you effectively could not read, and on some sites the pill looked completely empty. The badge has no warning-colored fill, only a border, so its text always sits on the ordinary card surface, and it was using the color meant for text sitting directly on a solid warning-colored background instead of the color meant for warning-tinted text on an ordinary surface. Measured contrast on the dark surfaces where the badge appears ranged from 1.0:1 to 1.14:1; at the low end the text was rendered in exactly the same color as the surface behind it. It now uses the correct color, and contrast on those same surfaces ranges from 11.1:1 to 12.65:1, well past the 4.5:1 minimum for readable text.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring" }],
  },
  {
    version: "0.61.137",
    date: "2026-08-16",
    summary:
      "Monitoring can now be paused on a site, with a reason and an optional resume time, from the site's row menu, its own page, or in bulk from the sites list. Pausing stops uptime checks, uptime alerts, weekly screenshots and scheduled vulnerability rescans; it never stops backups, connection tracking, or anything you click yourself. The monthly report's uptime section now goes by whether a pause actually overlapped the reporting period, not by whether the site happens to be paused today, so a measured period is no longer discarded and a partly-covered one says so instead of looking complete.",
    items: [
      {
        tag: "Added",
        text: "Monitoring can now be paused on a site, from the site's row menu, from its own page, or in bulk from the sites list. A pause takes a reason and, optionally, a resume time, and both are shown wherever the paused state appears, in all three places. Pausing stops uptime checks, uptime alerts, the weekly screenshot fanout, and scheduled vulnerability rescans. It does not stop backups, the WP-Cron kick, connection checks, RUM, or retention, and it never stops anything triggered by a click, including an on-demand rescan or update check: pausing stops the schedule, never the operator. Resuming happens by hand, or automatically at the stored resume time if one was set, exactly once either way, and both the pause and the resume are recorded in the audit log.",
      },
      {
        tag: "Changed",
        text: "The monthly report's uptime section is now keyed on whether a pause actually overlapped the reporting period, not on whether the site happens to be paused when the report runs. A period a pause never touched is measured and shown in full, even for a site that is paused today. A period a pause partly covered is shown with a note naming the hours that went unmeasured, instead of being presented as a complete month. A period a pause covered in full still names the site and states that monitoring was paused and why, rather than showing an empty section. If the pause history itself cannot be read, the report says coverage is unconfirmed rather than defaulting to a clean bill.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring" }],
  },
  {
    version: "0.61.136",
    date: "2026-08-15",
    summary:
      "The \"28d distribution\" bar on the performance tab's worst-offenders table has been showing a picture derived from a site's rating word, not its real data, for about two months. It now shows the real histogram for whichever metric produced the rating, labelled with that metric's name, and shows \"Insufficient samples\" when there is not enough data to be meaningful. If you drew a conclusion about a specific site from that bar since mid-June, the picture it showed did not reflect that site. Nothing was stored wrongly, no other number in the product was affected, and no action is required beyond upgrading.",
    items: [
      {
        tag: "Fixed",
        text: "The \"28d distribution\" bar on the performance tab did not show a site's real data. It read only the row's rating word and looked up one of two fixed pictures: every site rated needs-improvement showed 40% good, 45% needs-improvement, 15% poor, and every site rated poor showed 10% good, 30% needs-improvement, 60% poor, whatever that site's real numbers were. The table only lists sites rated poor or needs-improvement, so those two pictures were the only ones the column ever produced. The sample count shown beside the bar was real, which made the fabricated bar look measured. It shipped as an acknowledged placeholder in mid-June, the code comment recording that was removed the same day while the placeholder itself stayed, and it was live for about two months. If you drew a conclusion about a specific site from that bar in that time, the picture it showed did not reflect that site. Nothing was stored wrongly, no other number in the product was affected, and no action is required beyond upgrading.",
      },
      {
        tag: "Fixed",
        text: "The bar now shows the real histogram for whichever metric, LCP, INP or CLS, produced the row's rating, built from data already being gathered. It shows \"Insufficient samples\" instead of a bar when a site does not have enough of that metric's data to be meaningful.",
      },
      {
        tag: "Changed",
        text: "The distribution bar now names the metric it is showing rather than being labelled \"Overall\". A site's LCP, INP and CLS distributions have no single combined meaning, and \"Overall\" implied a share of pageviews good on all three, a figure this product does not compute.",
      },
    ],
    featureLinks: [{ label: "Real User Monitoring", href: "/features/real-user-monitoring" }],
  },
  {
    version: "0.61.135",
    date: "2026-08-14",
    summary:
      "A rebuild on a newer Go compiler, which closes seven vulnerabilities in the Go standard library that the previous release's binaries carry. None of them is remote code execution and the realistic exposure is a process that stops answering, so this is worth taking at your next convenient window rather than tonight. Nothing in WPMgr itself changed, and there is no symptom to look for in a running install. If you self-host, pull the new images; the fix arrives only with a rebuild.",
    items: [
      {
        tag: "Security",
        text: "The Go compiler this software is built with moves from 1.26.5 to 1.26.6, which closes seven vulnerabilities in the Go standard library. None of them is remote code execution. Five are denial of service: two decoders that could be driven down through deeply nested input until they exhausted the stack, a URL routine whose cost grew quadratically with the length of its input, a header timeout that was not applied on one server path, and an unbounded number of messages accepted after a TLS connection had finished negotiating. The other two are input validation rather than exhaustion: a name-encoding check on outbound requests, and escaping context tracking in the HTML templating package. The affected packages sit under the object storage client, the database connection pool, outbound HTTP, every outbound TLS connection, and the web server itself, which is to say under code that handles input from outside. Rebuilding on the newer compiler is the entire fix and no WPMgr code changed.",
      },
      {
        tag: "Security",
        text: "There was nothing here for anyone to have noticed. No code changed and no behaviour changed on either side of this. A live advisory database was updated overnight, and the same unchanged commit that passed our checks in the evening failed them the next morning. Whether a build of the previous release carries these depends on the day that build ran, because the images followed a floating compiler tag until this release pinned it, and a running install shows no symptom that would tell you which one you have, so the only reliable way to know which compiler produced a binary is to read it out of that binary rather than infer it from a date.",
      },
      {
        tag: "Changed",
        text: "The published container images now name an exact Go version instead of a floating one. They previously built from a tag that resolved to whichever patch release it happened to point at on the day the build ran, so the compiler that produced a shipped image was not recoverable from the source repository and could move without anyone deciding it should. If you build these images yourself you now get the same compiler our own builds do, and a compiler change becomes a commit somebody made rather than something that happened to you.",
      },
    ],
  },
  {
    version: "0.61.134",
    date: "2026-08-14",
    summary:
      "An administrator in your organisation could change the owner's role or remove the owner from the organisation, and could separately create an API key carrying the owner role. Neither is possible now: nobody can act on a member who outranks them, and nobody can create a key more powerful than they are. Upgrading does not revoke keys that already exist, so if administrators in your organisation can create API keys, review the list and revoke any owner-role key you did not expect. The dashboard now offers the owner role only to an owner, and an owner can nominate a second owner and hand ownership over.",
    items: [
      {
        tag: "Security",
        text: "An administrator could change the owner's role, or remove the owner from the organisation outright. Both actions are allowed for anyone who can manage members, which an administrator can, and that was the only question asked on the remove path: the acting person's own role was never read. The change path compared the acting person only against the role being handed out, never against the role the person being changed already held, so nothing anywhere compared an administrator to an owner. An organisation with one owner looked safe, but the refusal it gave came from the separate rule that keeps the last owner in place, not from any question of rank, so an organisation with two owners had no protection at all. Both now read the other person's role first and refuse when the acting person does not outrank it. An owner acting on another owner still works, because that is how ownership is handed over, and an administrator acting on another administrator or on anyone below is unchanged.",
      },
      {
        tag: "Security",
        text: "An administrator could create an API key carrying the owner role, which is an owner-grade credential obtained without going near the members page at all. The role was taken from the request as given, with nothing comparing it to the person asking. Asking for a role above your own is now refused outright rather than quietly lowered to one you may have, so a script that asks for something it is not entitled to gets an error instead of a key that silently does less than it thinks. The default is unchanged and an owner creating an owner-role key still works.",
      },
      {
        tag: "Security",
        text: "Upgrading does not revoke any API key that already exists. A key created with the owner role while the above was possible keeps working, with that role, until somebody revokes it, so reviewing them is a step you take yourself. The API keys page in your settings lists every key with the role it carries and revokes any of them, and the same is available over the API. Revoke any owner-role key that an administrator created. Which account created a given key is recoverable from the audit log, which has recorded it against the key since long before this release, with two limits: that record is written on a best-effort basis, so a failed write leaves a key with no trail, and permanently purging an organisation takes its audit history with it.",
      },
      {
        tag: "Fixed",
        text: "Two people demoting two different owners at the same moment could leave the organisation with no owner at all. The rule that keeps the last owner in place counted the owners and then saved the change as two separate steps, so both requests could see two owners, both conclude that one would be left, and both go ahead. That was already possible before this release; what changed is the cost of it. An organisation that lost its last owner used to be able to put itself right from inside the product, by promoting somebody or by creating an owner-role key, and those are exactly the two routes the fixes above close, so the same accident would now need somebody with direct database access to undo. Counting the owners, checking the rule and saving the change now happen as one step that no second request can interrupt. The rule itself is unchanged.",
      },
      {
        tag: "Fixed",
        text: "The dashboard no longer offers the owner role to somebody who is not an owner. On the members page and on the API keys page it appeared for anyone who could manage members, which includes an administrator, so the dashboard was offering something the control plane now refuses. The API keys page had a second version of the same problem, where the form would still send the owner role after the option itself was hidden. Both now go by your own role, and the refusals that come back are shown as messages you can read rather than a generic failure.",
      },
      {
        tag: "Fixed",
        text: "An owner can hand ownership over. An owner sees a second owner's row as an ordinary row and can change or remove it, which is what lets an organisation that has reached two owners come back to one, and an administrator still sees an owner's row as plain text with nothing to click. What an owner cannot do is act on their own row, so stepping down takes two people: an owner promotes somebody else to owner, and that person then demotes or removes the first. That is deliberate rather than missing, because it is what stops an owner demoting themselves and leaving the organisation with nobody in charge of it.",
      },
    ],
    featureLinks: [{ label: "Team and access control", href: "/features/team-access" }],
  },
  {
    version: "0.61.133",
    date: "2026-08-12",
    summary:
      "Deleting an organisation that had no sites left in it used to leave the bulk of its backups sitting in object storage forever, and freed nothing off your storage bill. Removing the organisation removed the only inventory naming those files, so nothing could find them again afterwards, including you, because the account they were filed under was gone too. Deleting an emptied organisation now clears its storage within the hour. Storage orphaned by a delete made before this release is still not cleaned up on its own, and there is now a supported command for clearing it, which replaces the recovery instructions published with the last release: those needed a database superuser and did not work on the connection a self-hosted install actually has.",
    items: [
      {
        tag: "Fixed",
        text: "Deleting an emptied organisation no longer strands its deduplicated backup chunks in storage forever. Those chunks are the bulk of what your backups occupy. Deleting an organisation with no sites and no other members removes it on the spot, and that same removal destroyed the stored inventory of its chunks while freeing no storage at all, so the objects were left named by nothing: not by the collector, whose list of accounts is built from that inventory, and not by you, because the account id was gone with it. The delete now records what it left behind, in the same database transaction that removes the organisation, and an hourly drain frees every one of that account's storage folders afterwards. The same transaction is the point: the record exists if and only if the organisation really went.",
      },
      {
        tag: "Fixed",
        text: "Backups belonging to a live site in another organisation cannot be caught by that drain, by construction rather than by care. Chunk storage is namespaced per account and deduplication never crosses an account boundary, so two accounts holding byte-identical WordPress files hold two separate objects, and draining one cannot reach the other. Before deleting anything the drain re-checks that the organisation, its sites and its chunk inventory are all still absent, and refuses if any of them came back, which is what makes a restored database backup, or a second control plane pointed at the same bucket, safe rather than lucky. It works in bounded batches, resumes where it stopped rather than reporting a half-finished cleanup as complete, and if it is refused or cannot finish it keeps its record and says so on every sweep, because that record is by then the last thing that knows those files exist.",
      },
      {
        tag: "Fixed",
        text: "The recovery instructions published with the previous release did not work on the connection you have. Both statements shipped there were written for a database superuser: run as the ordinary application role a self-hosted install uses, one was refused outright, and the other was worse, because the row it targeted was hidden from that role and the statement reported success while changing nothing at all. There is now a supported command family, wpmgr-cli reclaim, which lists outstanding work, hands a deleted site or a deleted organisation to the sweeps, and reopens a record that got stuck. It runs as the ordinary application role, it changes no permission or policy to do so, and every subcommand exits with an error when it changed nothing, which is the property that makes it a recovery path rather than another thing that claims success having done nothing. The message in the logs now names that command instead of printing a statement that cannot work.",
      },
      {
        tag: "Fixed",
        text: "Storage orphaned by a delete made before the previous release is still not cleaned up automatically, and nothing in this release goes looking for it on its own. The record that drives the cleanup is written by the delete, so it exists only for organisations and sites deleted from these versions onwards, and anything you deleted earlier is still in your bucket and still on your storage bill. Clearing it is now a deliberate step you take rather than a hand-written database statement. For organisations, wpmgr-cli reclaim backfill-tenants finds the ones the control plane still holds a deletion trail for and queues each one. For organisations removed by the separate cleanup of orphaned accounts, which leaves no such trail, wpmgr-cli reclaim discover --report-only reads the bucket and prints the candidates whose account no longer exists; it deletes nothing and queues nothing, so a person decides, and every safety check above still applies to whatever they hand over. For a single deleted site there is wpmgr-cli reclaim site.",
      },
      {
        tag: "Changed",
        text: "The published api image now contains wpmgr-cli. The install guide has been telling you to run that tool inside this image since long before today, and the image only ever carried the server itself, so the instruction could not work as written.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.132",
    date: "2026-08-11",
    summary:
      "Deleting a site used to leave that site's backup manifests in object storage for good. Removing the site removed the only records naming those files, so nothing could ever find them again, and one account was left carrying 90 of them on its storage bill. Deleting a site now clears its stored manifests within the hour. Files orphaned by a delete made before this release are not cleaned up by it, and nothing in it finds them, so clearing those stays a step you take yourself.",
    items: [
      {
        tag: "Fixed",
        text: "Deleting one site no longer strands its backup manifests in object storage forever. Removing the site removed every snapshot record it had, and those records were the only thing naming the site's stored manifest files, so nothing could ever find them again. The delete now writes a reclamation record in the same database transaction that removes the site, and an hourly sweep clears that site's storage folder afterwards. The same transaction is the point: a record written separately, before the delete, could outlive a delete that then failed, and that would leave a standing instruction to erase a live site's backups.",
      },
      {
        tag: "Fixed",
        text: "Files orphaned before this release are not cleaned up by it, and nothing in it finds them. The reclamation record is written by the delete, so it exists only for sites deleted from this version onwards. A site you deleted on an earlier version left no record anywhere, which is the defect itself, so its manifests are still in your bucket and still on your storage bill: the account that reported this is carrying 90 of them today. Clearing them is a deliberate step you take yourself, one folder per deleted site. Rather than deleting by hand you can hand a site you know is gone to the sweep with a single insert, which keeps every safety check in play, and the exact statement is written out in the database migration that ships with this release. It has to be run on a superuser connection.",
      },
      {
        tag: "Fixed",
        text: "Deduplicated chunks, which are the bulk of what your backups occupy, are deliberately untouched by that sweep. They are shared across every site in the account, they sit under a different storage root, and the sweep is structurally unable to reach them, so a chunk another site still needs cannot be deleted by this path however it goes wrong. A sweep that cannot finish, or cannot prove the site is really gone, leaves the files where they are and keeps its record rather than discarding it, because that record is by then the last thing that knows those files exist.",
      },
      {
        tag: "Fixed",
        text: "A site whose backups go to your own storage bucket has its manifests swept like any other. Manifests are always written to the bucket this instance is itself configured with, whatever destination the backup payload was sent to, so all of them are in scope and none of them are stranded. What this instance does not touch is the backup payload sitting in your own bucket, which it holds no credentials for.",
      },
      {
        tag: "Fixed",
        text: "An account that lost its last backup-carrying site stopped being garbage collected at all, so its stored chunks leaked as well as its manifests. The list of accounts to collect came only from completed backups, and deleting the site that held the final one removed the account from that list permanently. It now also considers accounts that still have chunks in storage, which changes only which accounts get looked at: every existing check on what may actually be deleted is unchanged, and a backup running at the time still protects everything it touches. Deleting the emptied organisation afterwards still strands its chunks, and that is deliberately left open here.",
      },
      {
        tag: "Fixed",
        text: "A batch of fixes to signing in with Google or GitHub. A provider that stops reporting an address, which is what GitHub does once someone makes theirs private, no longer erases the last address it did report. Connecting a provider, creating an account from one, and a stored provider record changing issuer are now written to the audit log as what they are, instead of all three reading as an ordinary sign-in, and a change to what an account can sign in with is now recorded even when that account belongs to no organisation, which describes a site collaborator, a client with portal access, and every brand new account. Signing in with Google is now bounded by a timeout, as signing in with GitHub already was, and connecting a second account from a provider you already have connected reports a conflict you can act on instead of a server error.",
      },
      {
        tag: "Changed",
        text: "The self-host install guide no longer hands you a stack from 190 releases ago. It still told you to pull v0.19.0, one link below the current pull commands in the README, so following that link rather than staying on the page you were already reading got you a control plane predating a long list of fixes. Both pages now also say what running without the media encoder actually costs, which is site screenshots, the Media Optimizer and WOFF2 font transcoding, and nothing else. Keeping every published version number honest is now a script anyone can run before pushing, with a test suite of its own, rather than shell buried in the build.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.131",
    date: "2026-08-10",
    summary:
      "A routine save, test or sync of a site's email settings could silently delete the site's working SMTP password, and outgoing email then failed with \"SMTP Error: Could not authenticate,\" the same message a wrong password gives. This release stops the deletion, restores an already affected site's password automatically the next time its settings sync, and closes off your organisation's own email password being reachable from a single site's settings page. Sites are not fixed until their plugin updates to this version.",
    items: [
      {
        tag: "Fixed",
        text: "Saving, testing or syncing a site's email settings could send the site an empty password, which the site read as an instruction to delete the working one it already had. The dashboard now says nothing about the password unless it has a real one to send, so a site keeps the credential it already holds. A site that uses your organisation's password, rather than one of its own, now keeps using it instead of losing it the moment anything was saved on that site's own email page.",
      },
      {
        tag: "Fixed",
        text: "A site now refuses an email settings update it cannot read, instead of acting on the part it understood, and reports it as a failure when your settings cannot actually be written to it rather than reporting success regardless. A save or an organisation-wide update is no longer sent to your sites at all when the stored password cannot be read back, which used to be able to empty the password on every site in the organisation from one unreadable record.",
      },
      {
        tag: "Security",
        text: "Your organisation's email password can no longer be sent to a mail server chosen on a single site, and somebody invited to only one site can no longer change your organisation's email settings from that site's page. A password saved against a mail connection, a site or your organisation is now dropped rather than carried across whenever the mail server, mailbox user or provider it was issued for changes.",
      },
    ],
  },
  {
    version: "0.61.130",
    date: "2026-08-10",
    summary:
      "Signing in with Google or GitHub is finished: you can see and manage which sign-in methods your account has, you are told when one is added, and pressing a provider button no longer stores anything on the server. Email and password sign-in is unchanged, and both providers stay off until whoever runs the instance sets them up.",
    items: [
      {
        tag: "Added",
        text: "Account security settings now show which sign-in methods your account has, and let you disconnect one or connect another. An account created with a provider, which has no password at all, can set one there while signed in. Password reset deliberately will not do it: a reset link that could create a password where there was none would turn \"forgot password\" into a way for anyone who knows your address to take an account.",
      },
      {
        tag: "Added",
        text: "You are now told when a new way of signing in is added to your account. The message goes to the address this instance confirmed, names the provider and the time, and links to the page where sign-in methods are reviewed and removed. It used to happen in silence.",
      },
      {
        tag: "Security",
        text: "Pressing a Google or GitHub button no longer makes the instance store anything. That button needs no account and no session, so each press used to leave a record behind for a week, and one machine pressing it in a loop could fill the shared store that everybody's signed-in session lives in. The handshake now travels in a short-lived, tamper-proof cookie in your own browser, so there is nothing left to fill. A sign-in left half finished for more than ten minutes has to be started again.",
      },
      {
        tag: "Fixed",
        text: "A provider that cannot be reached now returns you to the sign-in page with a sentence saying so, instead of a page of raw error text with no way back. Every other failure on that route already did this.",
      },
      {
        tag: "Fixed",
        text: "Connecting a provider to an account with two-factor authentication now attaches nothing until the second factor has been entered, and disconnecting a provider is refused when it would leave the account with no way to sign in at all.",
      },
    ],
  },
  {
    version: "0.61.129",
    date: "2026-08-07",
    summary:
      "Sign in with Google or GitHub, and two fixes to single sign-on: a disabled user could still get in, and an identity could be attached to an account whose address nobody had confirmed.",
    items: [
      {
        tag: "Added",
        text: "You can now sign in and sign up with Google or GitHub. Both are optional and set up separately by whoever runs the instance, so an install that configures neither carries on with email and password only. Signing up with a provider skips the verification email, because the provider has already confirmed the address, and supplies your name.",
      },
      {
        tag: "Fixed",
        text: "A disabled user could still sign in through single sign-on. Signing in with a password had always refused disabled and unverified accounts; the single sign-on route refused neither, so switching a user off in the admin area did not actually keep them out.",
      },
      {
        tag: "Fixed",
        text: "Single sign-on could attach an identity to an account whose email address nobody had ever confirmed. Because anyone can register an address without proving they own it, that let someone claim an address ahead of its real owner and keep access to the account the owner then signed in to. Connecting now requires the address to have been confirmed on this instance first, and we send that confirmation link at the moment it is needed.",
      },
      {
        tag: "Fixed",
        text: "Both of the above came from the sign-on rules existing twice in the codebase, once for each route, and only one copy being kept current. There is now a single set of rules that every provider goes through. Separately, accounts that were never sent a verification email, which includes the first account on a new install and everyone added by invitation, can now request one.",
      },
    ],
  },
  {
    version: "0.61.128",
    date: "2026-08-07",
    summary:
      "Signing up now asks for an email address and a password, and nothing else. The sign-in and sign-up pages also show what the product does alongside the form.",
    items: [
      {
        tag: "Changed",
        text: "Creating an account now asks for two things. The form also collected a display name, an organisation name and an organisation slug. All three were optional, none was needed to create the account, and every one of them is editable in settings afterwards, so they only ever added decisions to the screen where people are most likely to give up.",
      },
      {
        tag: "Changed",
        text: "An account's organisation is now named from the signup email instead of being called \"Default\". A work address gives the organisation, so an address at acme.com creates \"Acme\", while a personal mailbox gives the person, so sarah.jones at a consumer provider creates \"Sarah Jones\". It is a starting point rather than a claim to be right, and it is renamed in settings like any other. Accounts created before this release keep the name they have.",
      },
      {
        tag: "Changed",
        text: "The sign-in, sign-up, password reset and email verification pages now show what the product does alongside the form, instead of putting a form on an empty page. On a phone the form still comes first and nothing sits above it, because someone who came to sign in should not have to scroll past an explanation to reach the field they came for.",
      },
    ],
  },
  {
    version: "0.61.127",
    date: "2026-08-06",
    summary:
      "The password policy on a site's Security page only offered the five roles a stock WordPress install ships with, so a WooCommerce shop manager or a membership plugin's own roles could not be given a password rule at all. The policy now offers the roles the site actually has.",
    items: [
      {
        tag: "Fixed",
        text: "The password policy on a site's Security page only ever offered administrator, editor, author, contributor and subscriber. On a WooCommerce store the roles that matter are the ones the store added: a shop manager can edit orders, refund customers and see every buyer's address, and there was no way to require a strong password of them, because there was no way to select them. Reported by an agency running a WooCommerce site whose roles are a shop manager, a translator, two customer tiers and a staff role, none of which appeared. The policy now offers the roles the site actually has, which covers membership, LMS and booking plugins too, and any role an agency created by hand for its own staff.",
      },
      {
        tag: "Fixed",
        text: "Enforcement was never the problem and has not changed. The agent has always applied a policy to whatever role a user really holds, so a rule naming a shop manager would have worked from the day it was written. What was missing was any way to write it.",
      },
      {
        tag: "Fixed",
        text: "Role names now read the way they read on the site itself. An Italian site shows \"Amministratore\" and \"Gestore negozio\", so those are the names in the policy, not their English originals. The rule itself is still stored against the underlying role identifier, so renaming or translating a role never changes who a policy covers. Where a name alone cannot identify a role, because two plugins can each add a role called \"Staff\", the identifier is shown next to it.",
      },
      {
        tag: "Fixed",
        text: "A rule that names a role the site no longer has, because the plugin that created it was deactivated, keeps that role on screen and marks it as no longer present rather than dropping it. An operator can now see why a rule stopped applying, and can remove it.",
      },
      {
        tag: "Fixed",
        text: "A site whose agent has not yet reported its roles still shows the standard WordPress roles, but says on screen that it is doing so and that plugin-added roles are missing from the list. The silent version of that fallback is what hid this problem. Updating the agent on the site, or re-checking the site from its page, loads the real list. Sites with a large number of roles stay workable: the list is scroll-bounded, gains a filter box, and the number of roles carried per site is capped.",
      },
    ],
  },
  {
    version: "0.61.126",
    date: "2026-08-05",
    summary:
      "Searching the Sites list only searched the fifty most recently added sites, so a larger fleet was told \"no results\" for a site it owns. Search and ordering now run across the whole organisation, and sites can be ordered by name, date added or last check-in.",
    items: [
      {
        tag: "Fixed",
        text: "Searching the Sites list only ever searched the sites that page had already loaded, and that page is the fifty most recently added. An agency with more than fifty sites was told \"no results\" for a site it owns and can reach in two clicks, with nothing on screen to say the search had looked at part of the fleet rather than all of it. Reported by an agency running twenty four sites. Searching now happens in the control plane, across every site in the organisation, so a page of results is the best matches rather than the newest fifty filtered after the fact.",
      },
      {
        tag: "Fixed",
        text: "This is why raising the number of sites fetched was not the fix. Filtering a list the server has already cut short is wrong at any size: it only moves the point at which the product starts quietly lying about what it searched. The filter and the cut now happen in the same place, in the right order.",
      },
      {
        tag: "Fixed",
        text: "A search still matches a site's name, its address and its tags, the same three things it matched before, and still ignores case. It is a plain substring search: a percent sign or an underscore in the search box now looks for that character instead of behaving as a wildcard. Tags are searched from the same list of tags shown on the site, so a tag an operator can see is always a tag that finds it.",
      },
      {
        tag: "Added",
        text: "Sites can be ordered by name, by date added, or by last check-in, in either direction. The order is applied across the whole organisation before the page is cut, so the first page is genuinely the first page of that order and not the newest fifty rearranged among themselves. Ordering by name ignores case, so \"Acme\" and \"acme client\" sit next to each other.",
      },
      {
        tag: "Added",
        text: "A site that has never checked in has no last check-in time to order by. Those sites sit at the end of the list in both directions of the last check-in order: they never take the top of a \"most recently seen\" list, and they never vanish from one either.",
      },
      {
        tag: "Added",
        text: "Every order is settled down to the last row. Two sites that share a name, or that were added in the same second, keep a fixed position relative to each other, because paging through a list whose order is undecided between equal rows can show one site twice and skip another entirely. Nothing changes for anyone who does not ask for an order: the list is still newest first.",
      },
      {
        tag: "Added",
        text: "For self-hosted installs and API users, GET /api/v1/sites now takes q and sort, documented in the OpenAPI specification. They combine with the existing tag, status and client filters rather than replacing any of them. An order the control plane does not recognise is refused rather than quietly ignored.",
      },
    ],
  },
  {
    version: "0.61.125",
    date: "2026-08-05",
    summary:
      "A site's Uptime card could take up to thirty seconds to load the first time it was opened after a quiet period. It now reads the same running per-day totals the fleet views have used for a while, and reports the same numbers.",
    items: [
      {
        tag: "Fixed",
        text: "A site's Uptime card could take up to thirty seconds to load the first time it was opened after a quiet period, then load in half a second on every attempt after that. Measured over a week of production requests, the same page's fleet summary answered in a fifth of a second every time, including on the page loads where the per-site card took four seconds or more. The cause was not the amount of history, a missing index, a busy database or a cold container: the per-site view was still adding up every individual probe in the window by hand, about forty three thousand of them for a thirty day view, every time it was asked.",
      },
      {
        tag: "Fixed",
        text: "The fleet-wide view stopped doing that some time ago: it reads a running per-day total kept up to date as each probe lands, and only looks at individual probes for the two part days at the very edges of the window. The per-site view predates that work and never received it. It does now, using the same code to decide which days are complete rather than a second copy that could quietly disagree, so a site's Uptime card and that same site's row in the fleet views cannot drift apart.",
      },
      {
        tag: "Fixed",
        text: "The reported uptime percentage is unchanged, to the decimal. A day that falls only partly inside the window is still counted only for the part that is inside it, so an outage starting an hour before a thirty day window opens counts for exactly the minutes within it, not for the whole day and not for none of it. The three lookups behind the card also now happen at the same time as each other rather than one after another, since none of them ever needed another's result.",
      },
      {
        tag: "Changed",
        text: "The uptime chart on a site draws one point per day for any window of a day or longer, rather than a hundred points of whatever width divides the window evenly. For a thirty day view that is about thirty points instead of a hundred points seven hours wide, which is the same information at a resolution the chart can actually show. Windows shorter than a day are untouched and still show every minute.",
      },
      {
        tag: "Changed",
        text: "The average response time on a site's Uptime card is now the average across successful checks only, matching what the Sites list and the fleet dashboards have always shown for the same site. It previously also included the response times of failed checks, so a site with a spell of server errors had two different average response times depending on which screen it was read from. The uptime percentage was never affected by this.",
      },
    ],
  },
  {
    version: "0.61.124",
    date: "2026-08-05",
    summary:
      "\"Check now\" for the agent release reference is now on the Sites page, under the freshness text, for the viewers who may actually use it. Hosted multi-organisation installs are unchanged.",
    items: [
      {
        tag: "Added",
        text: "\"Check now\" for the agent release reference is now in the Agent column popover on the Sites page, directly under the freshness text, where the reporter asked for it. Reading \"may be stale, last confirmed 14h ago\" is exactly the moment an operator wants to act, and until now acting meant leaving the page for the admin console. The previous release granted the permission to the owner of a single-organisation install but left the only button behind a console that same owner cannot open, so the feature was unreachable for the person it was built for.",
      },
      {
        tag: "Added",
        text: "The button appears only for a viewer who may actually use it, and the dashboard does not work that out for itself. The fleet agent response now carries the control plane's own answer for the asking viewer, computed by the same code that decides whether the endpoint would accept the request. There is one decision, so a button that always refuses and a permission nobody is offered are both impossible rather than merely unlikely.",
      },
      {
        tag: "Added",
        text: "Nothing appears on an install with more than one organisation, which is every hosted account: the answer is false there for everyone except a superadmin, and a superadmin is redirected away from the Sites page anyway, so the admin console remains their route to the same action. The answer is also false whenever release mirroring is switched off, since there is no run to trigger at all then.",
      },
      {
        tag: "Added",
        text: "The three outcomes read the same here as in the admin console, because both use the same code. Queued is a success, and neither \"a check is already running\" nor \"the mirror must wait before its next request\" is shown as an error: being skipped by the thirty minute spacing is the system working as designed. The button also does not claim the check has happened, because the control plane answers that a run was queued, not that anything was confirmed.",
      },
    ],
  },
  {
    version: "0.61.123",
    date: "2026-08-05",
    summary:
      "On a single-organisation install, the owner can now check the agent release reference without being made a superadmin. Multi-organisation installs are unchanged.",
    items: [
      {
        tag: "Changed",
        text: "On an install with exactly one organisation, the owner of that organisation can now use \"Check now\" for the agent release reference (Admin > Agent mirror) without being made a superadmin first. Reported by a self-hoster who had to set WPMGR_SUPERADMIN_EMAILS and restart the control plane to click what is, for them, a monthly refresh of their own fleet's data, then found the seeding only ever adds the flag and never removes it. The permission exists so one organisation cannot spend another organisation's share of the install's shared, unauthenticated GitHub request budget, and on an install with exactly one organisation there is no other organisation for that to protect.",
      },
      {
        tag: "Changed",
        text: "The owner does not become a superadmin as a result. No environment variable, no restart, no flag written anywhere, and none of the side effects: every other admin action still refuses them, including the vulnerability feed sync, and they are not redirected away from the Sites page the way a real superadmin is. Only the owner role passes; an admin, operator or viewer in that organisation is refused, as is an API key.",
      },
      {
        tag: "Changed",
        text: "Nothing changes on an install with more than one organisation, which is every hosted account: the action stays superadmin only there. The organisation count is read fresh on every request and never cached, so creating a second organisation closes this path again on the very next call. An organisation that has been deleted but is still inside its restore window does not count towards the total, because nobody can act as one; restoring it closes the path immediately.",
      },
    ],
  },
  {
    version: "0.61.122",
    date: "2026-08-05",
    summary:
      "A warning heading in the Agent column popover was unreadable in dark mode, and the fleet agent rollout is now reachable from the command palette.",
    items: [
      {
        tag: "Fixed",
        text: "The warning heading inside the Agent column's information popover was almost unreadable in dark mode, rendering near black on the dark panel while the explanatory text below it was fine. It was using the text colour meant for content sitting on an amber background, which is deliberately near black in both themes, rather than the one for amber-tinted text on an ordinary surface.",
      },
      {
        tag: "Added",
        text: "\"Update agent on all sites\" is now in the command palette, alongside \"Run backup on all sites\" and \"Sync metadata on all sites\". The fleet agent rollout had the same shape and audience as those two but was the only one that still needed selecting sites and opening a menu to reach. It opens the Sites page filtered to outdated agents rather than starting the rollout outright, because a wave-gated update touching every agent in a fleet should not begin from a single keystroke, and it appears only for an owner or admin on an install where the rollout is enabled.",
      },
    ],
  },
  {
    version: "0.61.121",
    date: "2026-08-04",
    summary:
      "A site's Updates tab said everything was up to date when all it knew was that the managed components were. The WPMgr agent is now stated on that tab as its own line.",
    items: [
      {
        tag: "Fixed",
        text: "A site's Updates tab said \"All up to date\", with a green check, while that same site's own WordPress dashboard was offering a WPMgr agent update. The tab only ever knew about the components WPMgr updates for you: plugins, themes and WordPress core. The agent itself is deliberately not one of them, so its update was never in the count. The tab now says \"All managed components are up to date\", so it no longer speaks for anything WPMgr does not update.",
      },
      {
        tag: "Fixed",
        text: "The agent is now shown on that tab as its own line, whether or not anything else needs updating. It is deliberately not selectable and has no update button, because an agent update applied the way a plugin update is applied means the plugin overwriting its own running files inside the request that has to report the result, with no rollback armed for its own directory. Where the fleet agent update channel is turned on and you can use it, the line links straight to it; where it is not, it says to update the agent from that site's own Plugins screen instead.",
      },
      {
        tag: "Fixed",
        text: "When an install has no published agent release to compare against, the line says so rather than guessing, and never reports a site as behind on a comparison that did not happen.",
      },
    ],
  },
  {
    version: "0.61.120",
    date: "2026-08-04",
    summary:
      "Update runs can be retried from the run page, for agent, plugin, theme and core updates alike, and the retry says what it did with every update you selected.",
    items: [
      {
        tag: "Added",
        text: "An update run can now be retried from the run page itself. Retrying used to mean going back to the sites list and re-selecting every site by hand, which is how a 21-site fleet agent rollout whose canary failed, correctly cancelling the other 20 sites without touching them, turned into 20 checkboxes to find again. The retry is sourced from the run, so nothing has to be re-picked, and it always creates a new run rather than altering the one it came from.",
      },
      {
        tag: "Added",
        text: "The retry defaults to the updates that never succeeded: the ones that failed, and the ones that were cancelled before they were ever attempted. An update that was skipped, or that applied and was then rolled back, can be selected deliberately but is never included by default, because a rollback means the update did apply and was taken back, so retrying it may reproduce the same break. An update that succeeded, or has not finished yet, is never retryable.",
      },
      {
        tag: "Added",
        text: "The retry accounts for every update you selected. If 20 were selected and 17 became work, the 3 that did not are each named with a reason: the site is no longer enrolled, the site no longer exists, the same target already has an update in progress in another run, or, for an agent rollout, the site is no longer behind the published agent version. Enrollment and the published agent version are re-checked at retry time rather than copied from the old run, so reverting an agent release mid-incident excludes the sites that are no longer behind instead of quietly upgrading them to a build you withdrew.",
      },
      {
        tag: "Added",
        text: "Retrying an agent rollout re-runs the whole staged rollout with a fresh canary. A retry has proven nothing about the new attempt, so it starts from one site again rather than dispatching every previously-cancelled site at once.",
      },
    ],
  },
  {
    version: "0.61.119",
    date: "2026-08-04",
    summary:
      "Self-hosted installs mirroring our agent releases were correctly refusing them, because the same agent version was being republished with different bytes.",
    items: [
      {
        tag: "Fixed",
        text: "If you run a self-hosted install that mirrors our agent releases, it was refusing them with \"upstream republished the same version with different bytes\". That refusal was correct. The agent's version only changes when the agent itself changes, which is deliberate, because a release that only touches the dashboard should not push a new agent to every site in your fleet. But the archive was rebuilt on every release and was not byte-reproducible, since it recorded file modification times and the packaging step reinstalls the vendored libraries from scratch each run. A dashboard-only release therefore republished the same agent version with different bytes, which a mirror cannot tell apart from tampering. Packaging is now deterministic, verified by building twice and comparing, and a release-time check refuses to publish an archive whose bytes differ from an already-published release carrying the same agent version.",
      },
    ],
  },
  {
    version: "0.61.118",
    date: "2026-08-04",
    summary:
      "A fleet agent update could install nothing on one site while the same rollout succeeded everywhere else. The apply now carries the build it verified instead of looking it up in a cache any other plugin can answer for.",
    items: [
      {
        tag: "Fixed",
        text: "A fleet agent update could report \"the plugin update transient carried no entry for this plugin\" and install nothing, on one site, while the same rollout installed cleanly everywhere else. The apply looked up the build it was about to install in WordPress's shared plugin update cache, and that cache is one any other plugin on the site is allowed to answer for, rewrite or delete. A security or \"disable updates\" plugin answering that read first, a managed host's own must-use plugin doing the same, or simply an ordinary plugin update finishing on that site at that moment, was enough to leave WordPress's installer with nothing to install, after which the rollout stopped at the canary and no other site was touched. The apply no longer looks anything up: it carries the build it verified moments earlier and hands that straight to the installer. What gets installed is unchanged, and is still re-verified from scratch against the signed manifest before a single byte is written.",
      },
      {
        tag: "Fixed",
        text: "The previous release had made this more likely, not less. 0.61.114 correctly made an agent update wait for any other update on the same site to finish first, and the process it waits for is exactly the one that clears the cache the apply was standing on, which widened the window from milliseconds to as much as four minutes. That window is now closed, because the apply no longer depends on that cache at all.",
      },
      {
        tag: "Fixed",
        text: "The update offer shown on a site's own WordPress dashboard is now self correcting. An offer naming a build the fleet has since moved past is rewritten from the fresh signed manifest at the moment an install starts; an offer for a withdrawn release is retired the first time anything acts on it; and an offer the site has already overtaken, for example because its files were replaced by a deploy or a restore, is retired by the first page load that sees it instead of standing for up to twelve hours. A control plane that is briefly unreachable retires nothing, so an outage can never blank a fleet's update offers.",
      },
      {
        tag: "Fixed",
        text: "When a commanded agent update does fail, the site's own dashboard is now left holding a verified offer for the same build, so the one-click update inside wp-admin, which is the recovery route for a build whose fleet update is broken, is available immediately.",
      },
      {
        tag: "Changed",
        text: "As with every fix to the agent's own update path, this one cannot be delivered by the path it fixes. A site whose fleet agent update is failing this way needs one update from its own WordPress dashboard, after which fleet updates work normally again.",
      },
    ],
  },
  {
    version: "0.61.117",
    date: "2026-08-04",
    summary:
      "Starting an update run could report a server error while the run had actually been created, leaving tasks that nothing would pick up for up to six hours.",
    items: [
      {
        tag: "Fixed",
        text: "Starting an update run could report a server error while the run had in fact been created. If the background job queue was briefly unavailable at the moment the run was saved, the run and its tasks were already written to the database, but the response discarded them and returned a failure. You were told nothing happened, while a real run sat there with tasks nothing would pick up, and they stayed that way until the stale-task sweeper failed them 45 minutes later, or 6 hours later for an agent rollout. Starting the update again could then be refused, because the first run's tasks still counted as in flight, so a brief queue hiccup looked like a broken product. The run is now returned as created and its tasks are visible on the run page.",
      },
    ],
  },
  {
    version: "0.61.116",
    date: "2026-08-04",
    summary:
      "The client portal showed Cumulative Layout Shift a thousand times too large. A good CLS of 0.1 was shown to clients as 100.000, next to a badge that correctly said Good.",
    items: [
      {
        tag: "Fixed",
        text: "The Core Web Vitals card in the client portal displayed Cumulative Layout Shift multiplied by a thousand. A perfectly good CLS of 0.1 appeared to your clients as \"100.000\", directly beside a rating badge that correctly read \"Good\", so the score contradicted its own rating and showed a number that no CLS can take. It now reads 0.100, the same as on the operator dashboard and in every other report. This was client facing, so it was visible to the people you send portal links to.",
      },
      {
        tag: "Fixed",
        text: "Timing metrics in the client portal now switch to seconds above one second, so a client and an operator looking at the same site read the same figure. Previously the portal showed \"4200ms\" where the dashboard showed \"4.20 s\".",
      },
    ],
  },
  {
    version: "0.61.115",
    date: "2026-08-04",
    summary:
      "The Core Web Vitals charts stated the wrong Good threshold. LCP's Good line was labelled 3 seconds; the real threshold is 2.5 seconds. Threshold labels, axis units and axis scales are all fixed.",
    items: [
      {
        tag: "Fixed",
        text: "The Core Web Vitals trend charts stated the wrong Good threshold. The dashed Good line on the LCP chart was labelled 3 seconds. The real Web Vitals Good threshold for LCP is 2.5 seconds, and the line was always drawn in the right place; only the label was wrong, because the number was rounded to whole seconds before it was printed. Anyone who read that label and treated 3 seconds as the target was working to a threshold that does not exist. The same rounding mislabelled the FCP Good line as 2 seconds (it is 1.8) and the TTFB needs-improvement line as 2 seconds (it is 1.8). Every threshold label now shows its true value.",
      },
      {
        tag: "Fixed",
        text: "The same charts printed the unit twice, so an axis label read \"5sms\" and \"3sms\" instead of \"5s\" and \"3s\", and the threshold labels read \"Good 3sms\" and \"NI 4sms\".",
      },
      {
        tag: "Fixed",
        text: "A single vertical axis could mix two scales at once, showing \"650ms\" and \"2sms\" as neighbouring labels, which made the values impossible to compare by eye, and rounding to whole seconds meant four different heights on one LCP axis could print the same text. Each axis now picks one scale for all of its labels, and no two labels on an axis can read the same.",
      },
      {
        tag: "Fixed",
        text: "On a site comfortably inside the Good band, neither threshold line was drawn at all, so there was no way to tell \"this site is passing\" from \"no thresholds are configured\". The Good line is now always in frame. The threshold lines also shared a colour with the data line on the LCP and INP charts, making the target indistinguishable from the measurement; they now use the standard pass and warning colours, which are defined for dark mode.",
      },
      {
        tag: "Changed",
        text: "The small trend sparklines in the fleet tables are drawn directly rather than through the charting library. A fleet table showing 100 sites was building 100 full chart engines to draw 100 tiny decorations with no axes, no tooltips and nothing to interact with; the tables now render an order of magnitude faster and the Uptime and Backups pages no longer download the charting engine at all. They look the same.",
      },
    ],
  },
  {
    version: "0.61.114",
    date: "2026-08-03",
    summary: "Two updates could run against the same site at once and corrupt each other. Updates, rollbacks and agent upgrades on one site are now serialized, and a busy site retries instead of failing.",
    items: [
      {
        tag: "Fixed",
        text: "Two updates could previously be dispatched to the same site at the same time, for example two plugins in the same bulk run, or an update and a rollback overlapping, running more than one WordPress installer against the same site concurrently. WordPress's own updater is not built for that: a second installer can delete files the first one is still relying on, corrupting the update in progress and, in the reported case, leaving the site briefly returning errors. Updates, rollbacks and agent upgrades against one site are now serialized: only one may run at a time, whichever channel it came from.",
      },
      {
        tag: "Fixed",
        text: "A site that is busy with another update no longer fails the update that was turned away. It is retried automatically, with the reason shown on the task, for up to 6 hours before being recorded as not attempted rather than failed, and being busy never counts as a failure, so it can never fail a canary or halt a fleet-wide rollout by itself.",
      },
      {
        tag: "Fixed",
        text: "Separately, when an update fails before it has touched anything on the site (for example a corrupted download), the site no longer runs an automatic restore over a plugin or theme directory it never modified.",
      },
      {
        tag: "Changed",
        text: "Updating many items on one site is correspondingly slower, since they now run strictly one at a time on that site instead of several in parallel: a 30-plugin update on a single site that previously took roughly 6 to 12 minutes now typically takes 15 to 50 minutes. Updates spread across different sites are unaffected and still run in parallel. A brief window also remains at the moment a plugin or theme's files are swapped in, where a page load could in principle hit a half-updated directory, the same exposure WordPress's own core updater has always had for an admin-initiated single-plugin update; with updates now serialized one at a time, that window is measured in microseconds. A single plugin update started from a site's own WordPress dashboard still does not take part in this lock, so that one collision between a fleet-triggered update and a person using wp-admin at the same moment remains open.",
      },
    ],
  },
  {
    version: "0.61.113",
    date: "2026-08-02",
    summary: "The fleet Agent column can now show when the upstream release was last confirmed, and superadmins can check for a new release on demand from the admin console instead of waiting up to six hours.",
    items: [
      {
        tag: "Added",
        text: "The fleet Agent column's header popover can now say when the upstream agent release reference was last confirmed, instead of just showing a plain \"current\" badge computed against a reference that, on a self-hosted install running the upstream mirror, could quietly be hours behind. The fleet agent view now reports the mirror's own status (ok, stale, pending, standing down, misconfigured, or disabled), the time of the last successful confirmation against upstream, and the time and outcome of the last attempt, kept as two separate facts on purpose: a run that failed a few minutes ago is never reported as \"checked a few minutes ago\" while an older confirmation sits behind it unmentioned.",
      },
      {
        tag: "Added",
        text: "Superadmins on a self-hosted install with the upstream mirror enabled can now trigger an immediate check from the admin console's Agent mirror page instead of waiting for the next scheduled one, up to six hours away. This is an install-level action, not a per-site one, so it lives in the admin console rather than on the Sites page. A request made too soon after the last one is refused honestly with a wait time, never a false success, and a check already in progress is reported as such rather than starting a second one.",
      },
      {
        tag: "Changed",
        text: "Being rate limited is no longer treated the same as a real failure. The mirror now records and reports that outcome separately from a genuine problem such as the upstream being unreachable or this install's own storage failing to write, so an operator is never alarmed by an outcome that is normal and expected.",
      },
    ],
  },
  {
    version: "0.61.112",
    date: "2026-07-31",
    summary: "Outgoing email failed entirely when a plugin set a Reply-To in the usual Name <email> form. Fixed, along with three related address bugs.",
    items: [
      {
        tag: "Fixed",
        text: "Outgoing email failed completely whenever a plugin set a Reply-To header in the ordinary \"Name <email@example.com>\" form, which WooCommerce, Fluent Forms and many others do by default. The agent stored that header exactly as written and then handed the whole string, display name included, to the mail transport as if it were the address. The transport rejected it, and because one bad address aborted the entire message, nothing was sent. Addresses are now parsed properly wherever a bare address is required, the display name is kept rather than discarded, and a single bad address costs that one recipient instead of the whole email.",
      },
      {
        tag: "Fixed",
        text: "The same defect applied to the To, Cc and Bcc headers, not only Reply-To, on the SMTP and SendGrid providers. Amazon SES, Postmark and Mailgun were unaffected, because those build a raw header where this form is already valid. A header carrying more than one address, for example a Cc listing two recipients, was also treated as one malformed address and lost the whole message; address lists are now split correctly, including when a quoted display name contains a comma.",
      },
      {
        tag: "Fixed",
        text: "A display name could redirect an email to a different address than the one shown. Because header values are commonly assembled by dropping a user-supplied name into a template, a name that itself contained an address in angle brackets could take over as the real destination while the intended address was still displayed. Any entry of that shape is now refused rather than delivered somewhere the site owner did not intend.",
      },
    ],
  },
  {
    version: "0.61.111",
    date: "2026-07-31",
    summary: "A no-op release, so a site that has been moved onto 0.61.110 has something real for the fleet update path to install.",
    items: [
      {
        tag: "Changed",
        text: "Version bump only, with no functional change to the agent, the control plane or the dashboard. 0.61.110 removed a restriction that had stopped fleet agent updates from running on common Apache mod_php hosting. That restriction lived in the agent itself, so a site still on 0.61.108 or 0.61.109 refuses the update using its own installed copy of the rule, before it can install the release that removes it. Such a site needs one manual update from its own WordPress dashboard to reach a fixed build; this release then gives that build something genuine to install so the path can be exercised end to end. Sites will be offered an update whose only difference is the version number, which is safe to take.",
      },
    ],
  },
  {
    version: "0.61.110",
    date: "2026-07-31",
    summary: "Fleet agent updates now run on Apache with mod_php and plain CGI hosting instead of refusing outright, and a rollout halt banner no longer misreports a site that answered as one that was never reached.",
    items: [
      {
        tag: "Fixed",
        text: "Fleet agent updates no longer refuse to run on Apache with mod_php or plain CGI hosting, which is common on shared and self managed servers. The previous release declined to update the agent itself unless the web server could hand the connection back to WordPress before the file swap started, reasoning that a lost connection mid swap was unsafe there. That reasoning did not hold up: WordPress's own plugin and core updater performs exactly the same file swap, protected by exactly the same safeguards, on that same kind of hosting every day, whenever an operator clicks \"Update now\" in wp-admin. The fleet update now runs that identical, already safe swap instead of refusing outright, so a site on this kind of hosting updates itself from the fleet dashboard the same way it already updates from its own wp-admin.",
      },
      {
        tag: "Fixed",
        text: "Because the control plane now waits out the whole install on this kind of hosting instead of getting an instant acknowledgement, the time it is willing to wait for that one request was raised from 5 to 8 minutes, so a slower host has room to finish both the download and the file swap in the same request.",
      },
      {
        tag: "Fixed",
        text: "A control plane request that times out while an agent update is still applying is no longer recorded as a failed rollout. On this kind of hosting the agent's acknowledgement is only written after the whole swap finishes, so a slow answer is not evidence anything went wrong. The rollout now waits for the site's own report of the version it is running before deciding the outcome, exactly as it already does for an ordinary acknowledgement.",
      },
      {
        tag: "Fixed",
        text: "A rollout's halt banner could read \"The rollout was halted before any site could be contacted\" for a site that was, in fact, contacted and answered, when all that actually happened was the site politely declining the update rather than failing or never receiving it at all. The summary now counts a declined site as contacted and says so plainly.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.109",
    date: "2026-07-31",
    summary: "A deliberate no-op release, so the rebuilt agent update path from 0.61.108 has something real to install.",
    items: [
      {
        tag: "Changed",
        text: "Version bump only, with no functional change to the agent, the control plane or the dashboard. 0.61.108 rebuilt how a fleet agent update installs itself, and that path can only be tested by an agent that already has it: a site still on an older build applies updates with the old, broken step, so pointing it at 0.61.108 exercises nothing. Publishing a release that is identical in behaviour gives a site already on 0.61.108 something genuine to install, so the rebuilt path can be run end to end before anyone depends on it. Sites will be offered an update whose only difference is the version number, which is safe to take.",
      },
    ],
  },
  {
    version: "0.61.108",
    date: "2026-07-30",
    summary: "Fleet agent updates actually apply now: a transient-deletion bug meant every run silently did nothing, and the apply moved to where WordPress's own rollback still works.",
    items: [
      {
        tag: "Fixed",
        text: "Fleet agent self-updates have never actually applied on any site, in any release. WordPress's plugin upgrader looks for the update_plugins transient to find the package it's about to install, and the code that started the apply was deleting that same transient right before the upgrader read it. With nothing to find, the upgrader quietly did nothing while the run still reported an acknowledgement, so every fleet agent self-update to date has been a no-op that looked like progress. The apply now rebuilds that transient immediately before calling the upgrader, the same way WordPress's own background updater does, so the build a run verifies is the build that actually gets installed.",
      },
      {
        tag: "Fixed",
        text: "The apply now runs inside the same request as the control plane's command instead of a separate WordPress cron event, which is what lets WordPress's own automatic restore of a failed update work again. The previous design ran the apply from a point past where WordPress's restore could still fire, so a failed swap had no rollback at all. It now runs right after the acknowledgement is written to the response and the connection released, which keeps it inside the part of the request where WordPress's own restore still runs.",
      },
      {
        tag: "Fixed",
        text: "The site's own WordPress maintenance mode now covers the swap, the same as it does for any other plugin update: visitors see the maintenance page for the few seconds the plugin directory is actually being replaced, then the site serves normally again. That maintenance mode is guaranteed to clear even if the apply fails partway through.",
      },
      {
        tag: "Fixed",
        text: "A new outcome, sapi_cannot_detach, covers hosting where PHP has no way to release the connection back to the control plane before the swap starts, such as plain mod_php or CGI setups without PHP-FPM or LiteSpeed. On hosting like that, the agent now touches nothing and records a plain explanation in the task detail instead of a rollout that silently never reaches the site; use the one-click update in the site's own WordPress dashboard there instead.",
      },
      {
        tag: "Changed",
        text: "A fleet agent update confirmed success as soon as the site reported a newer version, without checking whether the agent's own record of the upgrade actually named this run as the cause. Normally that made no difference, since the version moved because this run's own command told the agent to install it. But if a site's version happened to move for some other reason while an unrelated record from an earlier attempt was still sitting on the agent, the control plane could credit that unrelated movement to a rollout that never touched the site, and a canary confirming a move it did not cause could open every later wave on evidence that was never real. The control plane now checks a per-apply identifier the agent stamps into its own outcome record and compares it against the one this run sent, so a version movement only counts toward a rollout's evidence when the agent's own record agrees it was the cause. A site whose agent does not yet report this identifier still confirms on its version report alone, exactly as before.",
      },
      {
        tag: "Changed",
        text: "When a confirmation times out, the explanation the dashboard shows now holds the agent's leftover apply record to the same standard: a record that cannot be tied to the run that timed out is still shown in full, but it is no longer described as an account of what happened in this run.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.107",
    date: "2026-07-30",
    summary: "A no-op release published to give the agent update path something to install.",
    items: [
      {
        tag: "Changed",
        text: "Version bump only, with no functional change to the agent, the control plane or the dashboard. The agent's fleet update path was rebuilt across the preceding releases, and publishing a release identical in behaviour gave that path something genuine to install so it could be exercised. Sites were offered an update whose only difference is the version number.",
      },
    ],
  },
  {
    version: "0.61.106",
    date: "2026-07-30",
    summary: "Fleet agent updates no longer depend on WordPress's scheduler, plus clearer reporting when an update can't proceed.",
    items: [
      {
        tag: "Fixed",
        text: "The step that installs a new agent version used to run only from WordPress's own scheduled task system, so a site where that system was blocked, unreliable, or never triggered would never get the update, even though everything else about the site worked fine. WPMgr's other background work stopped depending on that scheduler releases ago for the same reason; this step now works the same way, running on an ordinary page request whenever an update is waiting. The install itself still happens in a separate request from the one that starts it, since the agent can't safely replace its own files while it's the one reporting the result.",
      },
      {
        tag: "Fixed",
        text: "A fleet agent update could report that a site had accepted the job and then go quiet, only for the run to report twenty minutes later that it couldn't be confirmed. The agent asks WordPress to schedule the actual work in a separate request, which WordPress can decline, and the agent wasn't checking whether it had. It now checks, and reports a clear error immediately instead of leaving the rollout waiting on something that was never going to happen.",
      },
      {
        tag: "Fixed",
        text: "Some situations where the install step decided not to proceed used to leave no record at all. Every outcome is now recorded and reported back, so the dashboard can say why, and only one install can ever run at a time even if two requests start together.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.105",
    date: "2026-07-30",
    summary: "Agent updates now complete on slower sites instead of failing forever.",
    items: [
      {
        tag: "Fixed",
        text: "The agent's self-update download was bound by a single 60 second limit for the whole transfer, which needed roughly 55 kilobytes per second sustained. Sites downloading at the roughly 25 to 40 kilobytes per second this feature is meant to serve were cut off every time, discarded the incomplete file, and retried forever with no way to finish. The limit is now 300 seconds, comfortably covering slower connections.",
      },
      {
        tag: "Fixed",
        text: "Nothing raised PHP's execution time limit while an update was being applied, so on hosts that stop scripts after 30 seconds by default, an update could be cut off even within its own download budget. The apply step now raises it to 900 seconds, the same bound the ordinary plugin update path uses, before the download starts.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.104",
    date: "2026-07-30",
    summary: "Fleet agent updates work now: the command that starts them was rejecting every request, and a second command had been silently broken since launch.",
    items: [
      {
        tag: "Fixed",
        text: "Starting a fleet agent update failed immediately with a \"takes no parameters\" error, and the rollout stopped after its first site. The agent was mixing its own verified command details into the request body, so a command expecting an empty body saw something in it and refused. This never showed up in manual testing, since only a request sent with the JSON content type triggered it. The agent no longer does that, and commands that expect an empty body now ignore anything that arrives instead of refusing.",
      },
      {
        tag: "Fixed",
        text: "The Refresh inventory action on a site was affected the same way and had never worked: it reported success while doing nothing, because the control plane only checked that the request was delivered, not what the agent said back. It reads the agent's answer now, so refreshing a site's inventory actually refreshes it.",
      },
      {
        tag: "Fixed",
        text: "More broadly, the control plane checked only whether a command reached a site, not whether the agent accepted it. A refused rollback could be recorded as \"rolled back\", an update dry run was always recorded as successful, and some jobs would wait forever for a result a refusal never sends. All of these now treat a refusal as a failure. If you're reviewing past update history, a task marked rolled back may not have been, for this reason.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.103",
    date: "2026-07-30",
    summary: "Self-hosted installs can now get agent updates, mirrored from GitHub into their own storage and off by default.",
    items: [
      {
        tag: "Added",
        text: "Self-hosted installs previously had no way to get agent updates at all: the published release lives in the hosted service's storage, which a self-hosted install never receives (GH #302, driven by GH #310 and GH #255). WPMgr can now mirror the published agent release from our public GitHub releases into your own storage instead of hand-building and uploading the zip yourself. Off by default, turned on with a single setting; once it's on, the dashboard, wp-admin, and the fleet update flow all work exactly as they do on the hosted service.",
      },
      {
        tag: "Added",
        text: "The control plane downloads the release once, not once per site, and sites never contact GitHub themselves; they only talk to the control plane they already trust. The download is verified three ways before anything is published: a checksum published with the release, the checksum GitHub reports for the asset, and one computed over the bytes actually received.",
      },
      {
        tag: "Added",
        text: "Sites no longer need a per-site setting to trust where the package comes from, since the control plane now serves it from its own address. WPMgr never overwrites a release you published yourself, and a mirrored release only ever replaces an older one, so it can't move a fleet backwards.",
      },
      {
        tag: "Fixed",
        text: "Downloading the agent package no longer fails partway through on a slow connection; a download that genuinely stops making progress is still ended. Shutting down the control plane now waits for in-progress agent downloads to finish.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.102",
    date: "2026-07-29",
    summary: "Agent updates from GitHub release assets now agree with the built-in update channel on version numbers.",
    items: [
      {
        tag: "Fixed",
        text: "The agent plugin attached to each GitHub release and the one published to the built-in update channel could carry different version numbers, since the release workflow stamped the asset with the release tag instead of the agent's own version. Both channels now publish the same version. The agent version moves to 0.61.102, deliberately, to clear the numbers published by mistake; no action is needed on affected sites, and the next update offer will simply work.",
      },
    ],
  },
  {
    version: "0.61.101",
    date: "2026-07-29",
    summary: "Deleting or resending selected entries on a site's Email Log actually works now.",
    items: [
      {
        tag: "Fixed",
        text: "On a site's Email Log, selecting entries and clicking Delete showed \"0 log entries deleted\" and deleted nothing (GH #307). The dashboard and the control plane disagreed about the name of one field in the request, so the control plane received an empty list and truthfully reported deleting nothing. The Resend button had the same bug for the same reason and also did nothing. Both are fixed, and a deletion that removed nothing is no longer written into the audit log as though it had happened.",
      },
      {
        tag: "Fixed",
        text: "Sending an empty list to either endpoint used to return a confusing success with a count of zero; both now reject it with a clear error instead.",
      },
      {
        tag: "Fixed",
        text: "An automated check that compares every API endpoint against its published specification existed but was never wired into the checks that run on every change, which is how a mismatch like this reached a release. It runs on every change now, alongside a new check covering every request body field, including nested fields.",
      },
    ],
  },
  {
    version: "0.61.100",
    date: "2026-07-28",
    summary: "The Sites table shares its width sensibly across every column now, on any screen size.",
    items: [
      {
        tag: "Fixed",
        text: "On a wide monitor, the Sites table used to hand nearly all its width to the Site column and squeeze everything else into a sliver on the right (GH #261). On a 5120 pixel display, Site took about three quarters of the table; it now takes about eight percent, with the rest shared out and any true leftover left as empty space instead of stretched into one column.",
      },
      {
        tag: "Fixed",
        text: "At ordinary widths the opposite could happen: the Agent, Updates and Backup columns could overlap, with Uptime pushed off screen (reported on a 22 site fleet, GH #255). The header and the rows now share one definition of every column's width, and two columns were trimmed to fit: Backup no longer repeats its own heading, and Agent no longer repeats a status word on every row.",
      },
      {
        tag: "Fixed",
        text: "The Agent column's note about comparing against your own fleet rather than a published release, added in 0.61.99, moved from every row to the column heading, where it belongs.",
      },
      {
        tag: "Fixed",
        text: "The Sites table's loading placeholder was missing two columns and drifted out of step with the real table, so the table appeared to shift sideways once it finished loading. It is now built from the same column definitions as the table itself.",
      },
    ],
  },
  {
    version: "0.61.99",
    date: "2026-07-28",
    summary: "Self-hosted installs no longer show every site's agent version as an unreadable \"unknown\".",
    items: [
      {
        tag: "Fixed",
        text: "Self-hosted installs could show the Agent version card as \"0 of 24 sites on unknown, 24 unknown\", with the status filter looking like it did nothing (GH #255). The fleet agent-version feature compares each site against the currently published version, which only the hosted service ever receives, so a self-hosted install had nothing to compare against and every site fell back to unknown. When there's no published version, self-hosted installs now compare against the newest agent version already running in that install's own fleet, clearly labeled as a fleet-relative comparison, and say so directly when there's genuinely nothing to compare against.",
      },
      {
        tag: "Fixed",
        text: "On the hosted service, a brief failure reading the published version used to get cached like a real one, which could briefly report every site as current when some were actually behind. The last known-good version is now kept across a brief failure, a failure retries quickly instead of sticking, and a version that has gone stale is no longer presented as current.",
      },
      {
        tag: "Fixed",
        text: "The switch that turns on the fleet-wide agent update from 0.61.98 wasn't reported to the dashboard, so the action stayed hidden even once an operator enabled it. It's now reported correctly; the feature still ships off by default.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.98",
    date: "2026-07-28",
    summary: "Update the WPMgr agent across the fleet from the dashboard, rolled out in waves and shipped turned off.",
    items: [
      {
        tag: "Added",
        text: "Fleet-wide agent updates (GH #255, phase 2 of two). An owner or administrator can start an agent update across selected sites from the Sites list instead of visiting each site's wp-admin. This ships turned off behind a control-plane switch and will be turned on once it has been proven on real sites.",
      },
      {
        tag: "Added",
        text: "A rollout goes out in waves: one site, then a small percentage, then the rest of the fleet, and a wave only opens once every site in the one before it has confirmed by reporting its new agent version back. A failed wave stops the run and cancels what's left, and a stop control halts every agent update across the fleet at once.",
      },
      {
        tag: "Added",
        text: "The update itself runs in a background request rather than the one reporting the result, since the agent is what lets WPMgr reach the site in the first place; a site that can't complete that step is reported as unconfirmed, not failed. Sites on the WordPress.org build, and sites running an agent too old for this channel, are skipped with a reason instead of counted as failures.",
      },
      {
        tag: "Fixed",
        text: "A rollout whose target version stopped being published partway through now stops instead of reporting success.",
      },
    ],
    featureLinks: [{ label: "Updates", href: "/features/updates" }],
  },
  {
    version: "0.61.97",
    date: "2026-07-28",
    summary: "The agent can no longer update itself into a corner, and the dashboard now shows which sites are running an outdated agent.",
    items: [
      {
        tag: "Fixed",
        text: "A bulk update run across the fleet could target the WPMgr agent's own plugin (GH #255). Nothing prevented it: the agent appears in the plugin inventory like any other plugin, and WordPress advertises an update for it the same way. Updating the agent this way meant its own code was overwriting its own files from inside the request that had to report the result, with none of the snapshot-and-rollback protection every other plugin update gets, since the thing that would perform the rollback is the thing being replaced.",
      },
      {
        tag: "Fixed",
        text: "The agent now refuses any update task aimed at its own directory, identifying itself by plugin name as well as folder so a renamed install is still recognized, and the control plane independently stops offering the agent as an updatable component. The agent stays visible in the inventory with its version; only the actionable update is withheld, and its normal one-click update inside wp-admin is unchanged.",
      },
      {
        tag: "Added",
        text: "Fleet-wide agent version visibility (GH #255, phase 1 of two). The Sites list shows and filters by each site's agent version (current, outdated, unknown, or not self-updating), and the Updates page summarizes how many sites are current and how many are behind. Sites on the WordPress.org build are marked \"not self-updating\" rather than \"outdated\", since that build has no self-updater to run. Triggering an agent update across the fleet is phase two.",
      },
    ],
  },
  {
    version: "0.61.95",
    date: "2026-07-27",
    summary: "A failed backup no longer leaves its working files behind on the site.",
    items: [
      {
        tag: "Fixed",
        text: "A failed backup left its working directory (upload parts, copied plugins and themes, the database dump) on the site instead of cleaning up after itself (GH #256). One reported site was left with about 1.4 GB behind. Four separate give-up paths in the agent's backup watchdog now all reclaim it, but only once the same run-lock a live backup holds confirms the backup is truly stopped, so a slow but still-running backup is never touched.",
      },
      {
        tag: "Fixed",
        text: "The routine cleanup for old backup working directories, and the separate one for restore leftovers, depended entirely on WP-Cron and so never ran at all on a site where WP-Cron is disabled, unreliable, or gets no visitors. Both now also run on an ordinary page load, throttled so a busy site pays almost nothing for them. A bug that could permanently wedge the restore cleanup after one missed run is also fixed.",
      },
      {
        tag: "Fixed",
        text: "The Sites grid could show a green \"Backups\" indicator next to a red \"Failed\" badge for the same site; the misleading indicator has been removed, and the backup chip beside it already shows the real status.",
      },
      {
        tag: "Fixed",
        text: "The backup delete dialog said deleting a backup reclaims the site's storage, which was not true since the site's own temporary files stayed on the host; the wording now says what actually happens.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.93",
    date: "2026-07-27",
    summary: "Spot a site whose WordPress has failed even when its cache keeps serving visitors.",
    items: [
      {
        tag: "Fixed",
        text: "A site whose WordPress had completely failed could still show as fully up (GH #291). Uptime checks request the homepage, and on a site with page caching that page can be served straight from the cache without WordPress running at all, so a broken site kept returning a healthy response for hours. WPMgr now also checks whether WordPress itself is actually running, using a request a cache does not answer, and shows a site that is serving cached pages while WordPress is down as degraded with an explanation of what is likely broken.",
      },
      {
        tag: "Fixed",
        text: "An update that broke a site could be reported as successful and never rolled back. The check that decides whether to undo an update could be answered from the cache with the pre-update page. WPMgr now asks the site agent directly first, over a request that cannot be cached and only works if WordPress actually loaded, and still checks the public homepage afterwards so a front-end problem is caught too.",
      },
      {
        tag: "Added",
        text: "Optional alerts for application health, off by default on existing installs so an upgrade cannot wake anyone. An alert only fires on a genuine, repeated WordPress failure, never on an uncertain result such as a cached response or a site in maintenance. If many sites report a failure at once, WPMgr sends one summary instead of an alert per site.",
      },
      {
        tag: "Fixed",
        text: "Plugin vulnerabilities are now detected. The scanner compared the plugin identifier WordPress reports internally against the slug the vulnerability feed publishes, so the two never matched and no plugin vulnerability was ever reported. Themes and WordPress core were unaffected.",
      },
      {
        tag: "Added",
        text: "A per-site default account for one-click wp-admin sign in, so a site with several administrators always signs you in as the account you picked, and the audit trail records which one.",
      },
    ],
  },
  {
    version: "0.61.88",
    date: "2026-07-24",
    summary: "Choose which account one-click wp-admin login uses, per site.",
    items: [
      {
        tag: "Added",
        text: "A per-site default account for one-click sign in (GH #286). On a site with several administrators, one-click login used to land on whichever admin had the lowest user ID, which was opaque and hard to audit. You can now pick the default account in Site settings under Access, or set it while logging in as a specific user, and the wp-admin button shows which account it will use. Leaving it blank keeps the previous behavior of signing in as the first administrator. The audit trail now records the actual account used, and existing site agents honor the setting with no update.",
      },
    ],
  },
  {
    version: "0.61.87",
    date: "2026-07-24",
    summary: "Backup reliability improvements for large sites and slow servers.",
    items: [
      {
        tag: "Fixed",
        text: "Full backups on slow servers no longer fail at the upload stage (GH #279). A large backup that went quiet for a while during archiving could be wrongly marked failed while it was still running. The control plane now flags a quiet backup as taking longer than expected but keeps it running, and only fails it after a much longer, configurable timeout.",
      },
      {
        tag: "Fixed",
        text: "An interrupted backup now resumes cleanly instead of failing (GH #283). If the server stopped a backup partway through its upload, the resumed run could fail looking for a chunk it had already uploaded and cleaned up. The agent now records its upload progress durably as it goes and skips work it has already completed on resume.",
      },
      {
        tag: "Fixed",
        text: "Archiving a site now stops its scheduled backups (GH #282). An archived or removed site kept running its nightly backup, which then failed and sent a misleading failure email. Archived and removed sites are now skipped, while a temporarily unreachable site still attempts its backup and alerts you.",
      },
      {
        tag: "Fixed",
        text: "Backups no longer time out on OpenLiteSpeed and LiteSpeed servers (GH #274). The agent now acknowledges a backup request and continues the work in the background using whichever mechanism the server supports.",
      },
    ],
  },
  {
    version: "0.61.81",
    date: "2026-07-22",
    summary: "Fixes command updates failing on sites where another plugin globally intercepts the Authorization header.",
    items: [
      {
        tag: "Fixed",
        text: "Agent command updates (and other control-plane commands) could fail on sites running some third-party plugins that globally decode the Authorization header on every request, for example as part of their own JWT-based auth (GH #269). Such a plugin could error out on the agent's own signed request before the agent had a chance to verify it, causing the request to fail outright. The agent now moves its own signed Authorization value out of the request before any other plugin's code runs, so this class of conflict can no longer occur.",
      },
    ],
  },
  {
    version: "0.61.75",
    date: "2026-07-19",
    summary: "Get notified when a new vulnerability is found, instead of having to check the dashboard.",
    items: [
      {
        tag: "Added",
        text: "Vulnerability alerts (GH #247). WPMgr now emails you when a new vulnerability is found on your sites: one summary email per scan (site, component, installed and fixed versions, severity, and CVE), batched so a feed update matching many sites sends one email, not one per site. Set a minimum severity (High and above by default; unscored findings are always included), fire a signed webhook for Slack or custom integrations, and add an open-findings section to the daily digest. Configured on the Alerts page alongside downtime alerts, opt-in and off by default.",
      },
    ],
  },
  {
    version: "0.61.72",
    date: "2026-07-19",
    summary: "Vulnerability severities are now accurate, with an honest state when severity data is unavailable.",
    items: [
      {
        tag: "Fixed",
        text: "Vulnerability findings no longer all show as \"Low\" (GH #245). A request-spacing bug rate-limited the severity-enrichment feed on every sync, so every finding was stored without a CVSS score and fell back to the lowest severity, meaning a critical core vulnerability could appear with a \"Low\" badge. Severities now populate correctly. A finding with genuinely no severity data is shown as \"Unknown\", ranked for attention rather than hidden as Low, and when the enrichment feed is unreachable the Vulnerabilities page and admin feed status say so explicitly.",
      },
    ],
  },
  {
    version: "0.61.70 - 0.61.71",
    date: "2026-07-18",
    summary: "Honest cache reporting, working configuration dots, and a complete API reference.",
    items: [
      {
        tag: "Fixed",
        text: "The dashboard no longer under-reports a working page cache (GH #243). On sites where the managed web-server rules serve cached pages directly from disk, those hits never reach PHP and cannot be counted there; the Cache tab now shows a \"Served at the web-server level\" state, labels the chart as the PHP-layer ratio, and explains how to verify caching via the x-wpmgr-source response header. No numbers are fabricated.",
      },
      {
        tag: "Fixed",
        text: "The site card's Page Cache and Object Cache dots now reflect the real per-site configuration. They previously looked for plugin entries that can never exist (both features are drop-ins), so they showed gray for every site.",
      },
      {
        tag: "Fixed",
        text: "The agent's admin-bar \"Manage in WPMgr\" link now opens the site's Cache tab. It previously pointed at a page that never existed; the dashboard also redirects the old link target so already-installed agents work immediately, and unknown dashboard paths render a proper page instead of a bare \"Not Found\".",
      },
      {
        tag: "Changed",
        text: "The API reference at wpmgr.app/docs now documents the full control-plane surface (about 97 previously missing endpoints), kept in lockstep with the live routes by a new contract test. New user guides cover the file manager, security suite, monitoring, clients and portal, object cache, and audit log.",
      },
    ],
  },
  {
    version: "0.61.69",
    date: "2026-07-17",
    summary: "Site tags: organize, filter, and bulk-manage your fleet with colored tags.",
    items: [
      {
        tag: "Added",
        text: "Sites can now be organized with tags. Create tags on the fly from a keyboard-first picker (type to search, press Enter to create), assign multiple tags per site from the site card, table row, or site settings, and every tag gets a consistent color automatically, with an optional custom color per tag.",
      },
      {
        tag: "Added",
        text: "Filter the Sites list by one or more tags with match-any or match-all semantics. Filters live in the URL so a filtered view can be bookmarked or shared, and clicking any tag chip jumps straight to that tag's sites.",
      },
      {
        tag: "Added",
        text: "Bulk tagging: select multiple sites and add or remove tags across all of them in one action, with a clear indicator when only some of the selected sites carry a tag.",
      },
      {
        tag: "Added",
        text: "A tag management page under Settings: rename a tag everywhere at once, merge duplicates, change colors, and delete with a usage count shown before anything is removed. Existing site tags are registered automatically on upgrade.",
      },
      {
        tag: "Fixed",
        text: "The Sites list stays fast even after long idle periods: uptime data shown on the list is now read from a compact per-site rollup instead of scanning the full probe history, with uptime percentages unchanged and exact.",
      },
      {
        tag: "Changed",
        text: "The Sites list shows each site's last backup as a relative time (for example \"2h ago\"), with the exact date and time on hover (GH #231).",
      },
    ],
  },
  {
    version: "0.61.64 - 0.61.65",
    date: "2026-07-16",
    summary: "Scheduled backups can no longer stall silently, and slow pages screenshot correctly.",
    items: [
      {
        tag: "Fixed",
        text: "Scheduled backups no longer stall permanently at \"queued\" (GH #232). A scheduled run previously depended on WordPress cron, which never fires on quiet or DISABLE_WP_CRON sites, and its watchdog ran on that same cron. Backups now always start in-process, a request-driven sweeper re-dispatches genuinely stalled tasks, and a connection-independent file lock prevents a second runner from ever corrupting an in-progress backup.",
      },
      {
        tag: "Fixed",
        text: "Website screenshots of slow-loading or uncached pages are no longer blank (GH #229). Capture now waits for page load, network idle, and the DOM to settle, bounded by a hard timeout so a slow page degrades to a best-effort partial capture.",
      },
      {
        tag: "Fixed",
        text: "Switching organizations while viewing a single site no longer lands on a dead \"no website\" page; you are routed to the new organization's Sites list (GH #233).",
      },
      {
        tag: "Changed",
        text: "Stored secrets that can no longer be decrypted because the server's encryption key changed now fail loudly and clearly instead of looking like a wrong two-factor code (GH #215): the control plane logs a key fingerprint at startup, warns at boot when stored secrets no longer decrypt (with the exact remediation, pin a stable WPMGR_SITE_DEST_AGE_SECRET), and the two-factor prompt shows a precise \"the server's encryption key changed\" message.",
      },
    ],
  },
  {
    version: "0.61.62",
    date: "2026-07-15",
    summary: "Pre-update rollback snapshots are cleaned up reliably instead of accumulating forever.",
    items: [
      {
        tag: "Fixed",
        text: "Rollback snapshots captured before each plugin, theme, or core update are now reclaimed reliably (GH #226). Cleanup previously depended on WordPress cron, so a site that updated once and went quiet kept every snapshot forever, quietly consuming disk space. Cleanup now runs on ordinary agent activity, a snapshot whose update succeeded is reclaimed within about an hour once safely past the rollback window, and any backlog is swept on the first request after upgrading. Snapshots are never removed while a rollback could still be needed.",
      },
    ],
  },
  {
    version: "0.61.58 - 0.61.60",
    date: "2026-07-14",
    summary: "Self-hosted secrets now survive restarts, plus a batch of fixes across updates, backups, hide-login, and two-factor.",
    items: [
      {
        tag: "Fixed",
        text: "Stored secrets (SMTP password, destination and object-cache credentials, two-factor secrets) now survive a restart on a self-hosted install that has not set an explicit secrets-at-rest key. The key is now derived stably from the already-required WPMGR_SESSION_SECRET, so nothing is lost on reboot. Upgrade note: installs that were on the old per-boot key need to re-enter affected secrets once.",
      },
      {
        tag: "Fixed",
        text: "A failed, empty backup snapshot can now be deleted on its own; the chain-safety guard now checks the real parent-snapshot chain of custody instead of generation numbers (GH #221).",
      },
      {
        tag: "Fixed",
        text: "Bulk update runs now make one guaranteed-fresh update check per run instead of one per item (GH #218), and updates no longer fail with \"Could not copy file\" on hosts whose shared temp directory has grown pathologically overloaded (GH #216).",
      },
      {
        tag: "Fixed",
        text: "The fleet backup health view no longer fails entirely when a site has never completed a backup; such a site now reports as Unprotected (GH #214).",
      },
      {
        tag: "Fixed",
        text: "Hide-login no longer blocks front-end AJAX: admin-ajax.php and admin-post.php are excluded while the login and dashboard pages stay hidden (GH #219).",
      },
      {
        tag: "Fixed",
        text: "Two-factor sign-in is clearer and slightly more forgiving: the single-use message explains why a re-submitted code is rejected, the accepted window widened to plus or minus 60 seconds, and the setup code can no longer be reused for the first login (GH #215). The bulk update wizard's tab badges now count only components with a real pending update (GH #217).",
      },
    ],
  },
  {
    version: "0.61.55 - 0.61.56",
    date: "2026-07-11",
    summary: "Update rollback now recovers a site even when the update takes it fully down.",
    items: [
      {
        tag: "Added",
        text: "A new update watchdog loads before regular plugins and, when a plugin or theme update leaves the site fataling on every request, restores the pre-update snapshot directly at the filesystem level without needing WordPress to boot (GH #210). It fires only for a genuine post-update fatal within a short window, and disarms as soon as the site boots healthily, so it can never revert a working update.",
      },
      {
        tag: "Fixed",
        text: "Phantom updates whose target version equals the installed version are suppressed everywhere (GH #211), and the Refresh button on available updates now forces a real check against WordPress.org instead of returning a cached list (GH #212).",
      },
      {
        tag: "Fixed",
        text: "The self-hosted media-encoder no longer crash-loops on boot with an unprivileged database role, screenshots work on the bundled image, and an infrastructure capture failure is retried a bounded number of times instead of being recorded as success (GH #207).",
      },
      {
        tag: "Fixed",
        text: "Updates targeting \"latest\" no longer report \"already up to date\" from a momentarily stale cache, and heavy updates no longer spuriously report failed at the 30-second dispatch timeout (GH #208).",
      },
    ],
  },
  {
    version: "0.61.54",
    date: "2026-07-10",
    summary: "Critical self-host fix: a misconfigured media-encoder could silently stop every scheduled job.",
    items: [
      {
        tag: "Fixed",
        text: "On self-hosted installs where the media-encoder ran in the API's default database schema, it could silently take over background-job leader election and stop every scheduled fleet job, including backups, uptime checks, and cleanups, with no error anywhere (GH #205). The media-encoder now refuses to start in that misconfiguration, the safe dedicated schema is the built-in default with no configuration change needed, and any jobs left behind in the shared schema are cleaned up automatically.",
      },
    ],
  },
  {
    version: "0.61.49 - 0.61.53",
    date: "2026-07-10",
    summary: "Sign up straight into a paid plan, honest fleet vitals, and the media-encoder on by default for self-host.",
    items: [
      {
        tag: "Added",
        text: "Choosing Starter, Agency, or Scale on the pricing page now carries that choice through signup and email verification into a checkout for the plan you picked, with a \"Skip for now\" option. Prices on the pricing page are fetched live from the payment providers at build time with a USD and INR currency toggle, backed by Stripe and Razorpay. Hosted only; self-host is unaffected.",
      },
      {
        tag: "Changed",
        text: "Self-hosted installs now run the media-encoder by default, so site screenshots and the Media Optimizer work on a default docker compose up; opt out with --scale media-encoder=0 (GH #187).",
      },
      {
        tag: "Fixed",
        text: "Fleet Core Web Vitals now count a site as passing only when LCP, INP, and CLS all pass (GH #195), the worst-offenders table shows each site's name and URL (GH #202), and the CLS distribution bar agrees with its p75 rating (GH #185).",
      },
      {
        tag: "Fixed",
        text: "Audit-log entries for backups, restores, and updates now show their site and match the site filter (GH #201), the \"sites with items to review\" database-health stat expands into the full linked list (GH #197), and the database-size 90-day history now populates automatically from each site's daily diagnostics (GH #196).",
      },
      {
        tag: "Fixed",
        text: "Switching organizations takes effect immediately (GH #186), backup detail pages have a real breadcrumb trail back to the site's backup list (GH #188), and a screenshot refresh that cannot run now stops with a clear warning instead of spinning forever (GH #187).",
      },
    ],
  },
  {
    version: "0.61.41 - 0.61.48",
    date: "2026-07-08",
    summary: "A concentrated run of plugin and theme update reliability fixes, plus admin panel improvements.",
    items: [
      {
        tag: "Fixed",
        text: "A series of update-reliability fixes across restricted and standard hosts: a pre-update snapshot failure no longer blocks the update on open_basedir or symlinked wp-content setups; a stale filesystem cache no longer causes a perfectly good update to be reported failed and rolled back; a bulk update including a premium plugin that manages its own updates no longer fatals and strands the site in maintenance mode (GH #182); and the temporary-directory pin that collided with WordPress's own unpack folder on standard hosts was removed, so updates work everywhere again while restricted hosts keep a dedicated safe fallback.",
      },
      {
        tag: "Added",
        text: "Failed or rolled-back update tasks now show the complete agent log, including WordPress's own explanation of what went wrong, with a \"View log\" toggle and a copy button.",
      },
      {
        tag: "Fixed",
        text: "Two-factor settings are now reachable for every account, including accounts without an organization and the instance superadmin; the instance-admin console is full width with cleaner navigation; and the Accounts list pagination advances correctly.",
      },
    ],
  },
  {
    version: "0.61.40",
    date: "2026-07-08",
    summary: "Organizations can now be deleted, with a safety grace window.",
    items: [
      {
        tag: "Added",
        text: "Organization owners can delete an organization from Settings, Organization, with a type-the-name confirmation. An empty organization is removed immediately; a populated one is scheduled with a grace window: hidden right away and recoverable until the window passes, then permanently removed along with its sites' agent connections and stored data.",
      },
      {
        tag: "Fixed",
        text: "Switching the active organization now reconnects live dashboard updates to the newly active one immediately, instead of requiring a full page reload.",
      },
      {
        tag: "Fixed",
        text: "The deletion flow no longer shows a misleading \"recoverable during the grace window\" message for an empty organization, which is removed immediately and has no grace window; that message now appears only when a deletion is genuinely recoverable.",
      },
    ],
  },
  {
    version: "0.61.39",
    date: "2026-07-08",
    summary: "Real User Monitoring data collection fixed, on self-host and under any cache.",
    items: [
      {
        tag: "Fixed",
        text: "Real User Monitoring collected no data at all on self-hosted installs: the bundled reverse-proxy configuration had no route for the RUM endpoint, so every beacon request was rejected before it reached the application (the same gap silently dropped inbound email and billing webhooks too). Both are now routed correctly, and a new check exercises the real proxy configuration against every public endpoint so a gap like this is caught before release.",
      },
      {
        tag: "Fixed",
        text: "Real User Monitoring also collected no data on sites served by a third-party page cache, because the collector script was only injected inside WPMgr's own cache output. It is now injected on a standard WordPress hook during page generation, so whichever cache serves the page still captures the data.",
      },
      {
        tag: "Fixed",
        text: "The RUM beacon key could get permanently stuck: if the one-time delivery of the key to a site was ever lost, the site kept showing RUM as enabled while silently collecting nothing. The control plane now tracks whether the site has actually confirmed it holds a key and automatically reissues one when it is missing. A manual \"Rotate beacon key\" action was added, and the dashboard now warns when RUM is on but unconfirmed.",
      },
    ],
    featureLinks: [{ label: "Real User Monitoring", href: "/features/real-user-monitoring" }],
  },
  {
    version: "0.61.37",
    date: "2026-07-07",
    summary: "Fixed the hide-login security feature: the secret URL now actually serves a login form.",
    items: [
      {
        tag: "Fixed",
        text: "Turning on \"hide login\" showed a harmless but alarming \"policy stored but agent push failed\" error even though it saved and applied correctly. More seriously, the secret login URL did not show a login form at all (it bounced to the home page), which could leave you unable to sign in through the browser. The secret URL now serves the login form correctly while the default wp-login.php stays hidden. Review also closed two smaller gaps: the secret URL is no longer exposed in page links to logged-out visitors, and the access cookie is now signed so it cannot be forged.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security" }],
  },
  {
    version: "0.61.36",
    date: "2026-07-07",
    summary: "Fixed a bug that could permanently break an incremental backup chain.",
    items: [
      {
        tag: "Fixed",
        text: "An incremental backup could fail with a \"stalled\" error because retention cleanup had deleted a parent snapshot's file-list data while it was still needed, permanently breaking the chain. Retention now always protects the completed snapshot at each chain position and, as a ground-truth safety net, never deletes data that any surviving snapshot still references. A broken chain now fails fast with a clear \"run a full backup\" message instead of stalling silently.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.35",
    date: "2026-07-07",
    summary: "Vulnerability and performance tiles on the Health tab now show live data.",
    items: [
      {
        tag: "Fixed",
        text: "The Vulnerabilities and Performance tiles on a site's Health tab were placeholders that always read \"Not scanned yet\" and \"Not measured yet\", regardless of the real data. They now show live results: open-vulnerability count and worst severity, and Core Web Vitals (LCP) from real visitor data, each linking through to the full view. A site that has not been scanned, or has no visitor data yet, shows an honest empty state instead of a fabricated number.",
      },
    ],
    featureLinks: [
      { label: "Security suite", href: "/features/security" },
      { label: "Real User Monitoring", href: "/features/real-user-monitoring" },
    ],
  },
  {
    version: "0.61.33",
    date: "2026-07-07",
    summary: "Your own backup destinations now actually work, end to end.",
    items: [
      {
        tag: "Fixed",
        text: "Configuring a local folder or your own S3-compatible bucket as a backup destination looked like it worked (the \"Test connection\" check passed), but every backup still went to managed storage regardless. Backups, full and incremental, now run to the destination you configured, and restore reads back from it, across all three types: managed storage, a local folder on the server, and your own S3-compatible bucket. For your own bucket, the control plane signs the uploads and downloads so the site never holds your storage credentials.",
      },
      {
        tag: "Fixed",
        text: "Temporary backup working files left behind by a failed or interrupted run are now cleaned up automatically by a daily job, instead of slowly accumulating on disk.",
      },
      {
        tag: "Changed",
        text: "Hosted service only: managed backup storage is now a paid-plan feature. On the free plan, backups target your own storage (a local folder or your own S3-compatible bucket) instead. Self-hosted installs are unaffected and always keep managed storage, and restoring an existing backup is never restricted.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.31",
    date: "2026-07-07",
    summary: "Restore now verifies the site actually loads, and rolls back automatically if it does not.",
    items: [
      {
        tag: "Added",
        text: "After the files and database are swapped, restore now runs two checks: first that the restored database is intact while the site is still in maintenance mode, then, once the site is live, that it is not showing a fatal error. If either check finds a genuine failure, the restore automatically reverts both the files and the database to their pre-restore state and reports the run as failed with the reason, instead of leaving a broken site behind and reporting success. The checks fail open on a network blip, so they can never roll back a good restore.",
      },
      {
        tag: "Fixed",
        text: "Restore could also silently drop plugin or theme files whose path happened to contain a reserved drop-in name (for example a plugin's own class-db.php), leaving the site broken while the restore reported success. Restore now matches its protected-file exclusions by exact path instead of substring, so only genuine WordPress root drop-ins and config files are held back. Sites affected by an earlier restore can recover by re-restoring from the same snapshot.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.30",
    date: "2026-07-07",
    summary: "Uptime incidents now have real history, with a detail view.",
    items: [
      {
        tag: "Added",
        text: "Uptime incidents are now recorded and kept, so the Incidents panel shows real history: past incidents with accurate durations and a flapping indicator for sites that go down repeatedly. Clicking an incident opens a detail view with the site's live status, the probe results across the incident window, a timeline of what else happened around that time (updates, backups, activity, PHP errors), 7- and 30-day uptime, and quick actions.",
      },
      {
        tag: "Fixed",
        text: "An ongoing incident previously showed the wrong severity (\"Degraded\" for a site that was down), a duration that read \"NaNh\", and a blank site name; incidents now show the correct severity, read \"ongoing\" while open, and always show the site name. Also fixed: unreadable dropdown menus in dark mode, two dead breadcrumb links, and an \"Open site\" action that did nothing.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring" }],
  },
  {
    version: "0.61.26",
    date: "2026-07-06",
    summary: "Uptime: TLS expiry now shows, and downtime alert emails send reliably.",
    items: [
      {
        tag: "Fixed",
        text: "TLS certificate expiry now shows for monitored sites. The uptime prober only read the certificate during a fresh TLS handshake, which almost never happened after the first probe on a reused connection, so the TLS expiry column stayed empty from the start. It now reads the certificate on every probe, fresh or reused.",
      },
      {
        tag: "Fixed",
        text: "Downtime and recovery alert emails were silently skipped when SMTP was configured only in the dashboard (Settings, Email/SMTP) and not also set as environment variables. Alerts now send through the same saved relay used everywhere else, with environment variables kept only as a fallback.",
      },
      {
        tag: "Fixed",
        text: "The audit log recorded \"Emailed: Yes\" for a downtime alert whenever recipients were configured, even when the send was skipped or failed. It now records the true outcome (Sent, Skipped, or Failed) with the reason, for both email and webhook delivery.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring" }],
  },
  {
    version: "0.61.17",
    date: "2026-07-05",
    summary: "Bulk, chain-aware backup deletion.",
    items: [
      {
        tag: "Added",
        text: "Select multiple backups with checkboxes, including whole incremental chains via a tri-state chain checkbox, with one-click filters for all failed or all zero-byte runs, and delete the batch in one action. Selecting a snapshot automatically includes the later generations that depend on it, shown before you confirm, so a chain can never be left broken.",
      },
      {
        tag: "Added",
        text: "A batch of failed or zero-byte runs needs one plain confirmation; a batch containing any completed backup asks you to type one phrase for the whole batch. Deleting a snapshot, single or bulk, now also refuses while a restore that reads it is in progress.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.15",
    date: "2026-07-04",
    summary: "Fleet Audit log redesigned, with several reliability fixes.",
    items: [
      {
        tag: "Changed",
        text: "Redesigned the fleet Audit log. Events now read as plain sentences instead of raw internal codes, the operator who performed each action is shown by name, and long runs of routine file reads collapse into one expandable line so writes, deletions, and denied actions stand out. Added an outcome filter (Denied, Writes, Sensitive), a search box, exact timestamps, and a per-event detail view.",
      },
      {
        tag: "Fixed",
        text: "The event list was ordered oldest-first while presented as newest-first, so once a tenant passed one page of events, recent activity was paged off the end; it now lists newest events first (no audit data was ever lost). Also fixed a false \"Chain break\" integrity warning caused by two actions recorded at the same instant, and a page-load failure for accounts with automated activity like uptime alerts and backups.",
      },
      {
        tag: "Added",
        text: "Owners can now acknowledge a \"Chain break\" that predates an older fix, since the audit log is append-only and its rows can never be altered. Acknowledging moves the integrity anchor to the current point so verification runs forward cleanly; new tampering is still detected, and the acknowledgment itself is recorded in the audit log.",
      },
    ],
    featureLinks: [{ label: "Team and access control", href: "/features/team-access" }],
  },
  {
    version: "0.61.10",
    date: "2026-07-02",
    summary: "Bulk updates only touch sites that actually need them.",
    items: [
      {
        tag: "Fixed",
        text: "Multi-site bulk update now only creates a task for a site that actually has the selected update pending, instead of also creating a task, and then failing it, for every selected site that does not have that plugin, theme, or core update available. Sites the update does not apply to are now reported as skipped, not failed.",
      },
    ],
  },
  {
    version: "0.61.9",
    date: "2026-07-02",
    summary: "A failed update no longer strands a site in maintenance mode.",
    items: [
      {
        tag: "Fixed",
        text: "A failed plugin or theme update no longer leaves a site stuck showing WordPress's maintenance page to every visitor. The post-update health check now tolerates a brief, transient 503 from an in-progress database migration instead of treating it as a failure and rolling back an update that actually succeeded.",
      },
    ],
  },
  {
    version: "0.61.6",
    date: "2026-07-02",
    summary: "Reliable downtime alerts and a faster Sites list.",
    items: [
      {
        tag: "Fixed",
        text: "Downtime email alerts now fire reliably. A race in the alert state machine could let a sustained outage go unalerted; alert state now transitions atomically so every qualifying outage sends its alert.",
      },
      {
        tag: "Fixed",
        text: "The Notifications settings page now saves correctly, including the daily email digest toggle.",
      },
      {
        tag: "Fixed",
        text: "Fixed a slow first load of the Sites list after the dashboard had been idle for a while.",
      },
    ],
  },
  {
    version: "0.61.3",
    date: "2026-06-26",
    summary: "A batch of dashboard, backup, and update fixes.",
    items: [
      {
        tag: "Fixed",
        text: "Backups now correctly preserve plugin and theme vendor code that happens to live in a folder named cache or upgrade, while still excluding real runtime cache and update staging files.",
      },
      {
        tag: "Fixed",
        text: "The backup schedule form no longer rejects valid day-of-week, day-of-month, or hourly-interval selections, and update run history now shows accurate task counts and completion progress.",
      },
      {
        tag: "Fixed",
        text: "Closing any dialog no longer leaves the page unclickable, and bulk plugin and theme updates now default to only the items that actually have an update available.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups" }],
  },
  {
    version: "0.61.0",
    date: "2026-06-23",
    summary: "File Manager for every managed site.",
    items: [
      {
        tag: "Added",
        text: "Browse, preview, edit, upload, download, zip, and restore prior versions of files on any managed WordPress site, right from the dashboard, no SFTP or cPanel required. A sensitive-file deny list and a separate write-access toggle keep it safe, and every action is written to the audit log. Off by default; only owners and admins can turn it on.",
      },
      {
        tag: "Added",
        text: "CloudPanel sites now get integrated cache purging: WPMgr clears its own page cache and CloudPanel's Varnish cache together, no separate plugin needed.",
      },
    ],
    featureLinks: [{ label: "File Manager", href: "/features/file-manager" }],
  },
  {
    version: "0.57.7",
    date: "2026-06-21",
    summary: "New multipage marketing website.",
    items: [
      {
        tag: "Changed",
        text: "The public site at wpmgr.app is now a multipage, server-rendered site: a dedicated page for every feature, solution pages by audience and by job, pricing, this changelog, a resources area, and a self-hosted API reference generated from the OpenAPI spec. Faster, fully crawlable, and easier to extend.",
      },
    ],
  },
  {
    version: "0.57.0",
    date: "2026-06-21",
    summary: "Vulnerability feed configuration from the dashboard.",
    items: [
      {
        tag: "Added",
        text: "Instance administrators can configure the Wordfence Intelligence API key from a new admin page instead of an environment variable. The page shows live connection status, lets you save or remove the key, and provides a Sync now action. The key is encrypted at rest.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security" }],
  },
  {
    version: "0.56.0",
    date: "2026-06-20",
    summary: "Vulnerability scanner across your fleet.",
    items: [
      {
        tag: "Added",
        text: "WPMgr now checks every managed site's plugins, themes, and WordPress core against the Wordfence Intelligence vulnerability feed. Each finding shows severity, affected version range, fixed version, and CVE references. One-click remediation updates the vulnerable item using the existing update flow. Findings appear per-site on the Security tab and fleet-wide on the Vulnerabilities page.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security" }],
  },
  {
    version: "0.55.0",
    date: "2026-06-20",
    summary: "2FA enrollment flow for site users + redesigned Security tab.",
    items: [
      {
        tag: "Added",
        text: "After an operator requires 2FA for a user role, affected users now see a guided enrollment screen on next login: scan a QR code, confirm a code, save backup codes. Users can also start enrollment proactively from their WordPress profile.",
      },
      {
        tag: "Changed",
        text: "The per-site Security tab is now a card-based layout with a status overview strip and collapsible setting groups: Login and Two-Factor, Password policy, Hardening, File integrity, Bans, and Hide login.",
      },
    ],
    featureLinks: [
      { label: "Two-factor auth", href: "/features/two-factor-auth" },
      { label: "Security suite", href: "/features/security" },
    ],
  },
  {
    version: "0.54.0",
    date: "2026-06-20",
    summary: "2FA for WordPress site users, password policy, and hidden login.",
    items: [
      {
        tag: "Added",
        text: "Operators can require 2FA for chosen user roles, enforced at the WordPress login screen. Methods: authenticator app (TOTP), email one-time code, backup codes. A grace period lets users enroll before enforcement. The control plane and wp-config bypass can never be locked out.",
      },
      {
        tag: "Added",
        text: "Per-site password policy: minimum strength, known-compromised password blocking (privacy-preserving prefix query), reuse blocking, and optional expiry.",
      },
      {
        tag: "Added",
        text: "Hide login page: move wp-login.php to a secret address per site. All three controls are per-site, opt-in, and off by default.",
      },
    ],
    featureLinks: [{ label: "Two-factor auth", href: "/features/two-factor-auth" }],
  },
];

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
  });
}

export default function ChangelogPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Changelog", href: "/changelog" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Changelog
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              What shipped and when
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Every WPMgr release, newest first. Each entry links to the relevant feature pages.
              For the full history and release artifacts, see{" "}
              <a
                href={`${SITE_CONFIG.github}/releases`}
                target="_blank"
                rel="noreferrer noopener"
                className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
              >
                GitHub Releases
              </a>
              .
            </p>
          </div>
        </Container>
      </section>

      {/* Release feed */}
      <Section>
        <Container className="max-w-4xl">
          <div className="relative">
            {/* Timeline spine */}
            <div
              aria-hidden
              className="absolute left-[15px] top-0 bottom-0 w-px bg-[var(--border)] sm:left-[19px]"
            />

            <ol className="space-y-12" aria-label="Release history">
              {RELEASES.map((release) => (
                <li key={release.version} className="relative pl-10 sm:pl-14">
                  {/* Dot */}
                  <div
                    aria-hidden
                    className="absolute left-0 top-1.5 h-[30px] w-[30px] sm:h-[38px] sm:w-[38px] flex items-center justify-center rounded-full border-2 border-[var(--border)] bg-[var(--background)] text-[10px] font-bold text-[var(--primary)]"
                  >
                    <span className="hidden sm:inline text-[9px]">v</span>
                    <span className="text-[8px] sm:text-[8px] leading-none">
                      {release.version.split(".").slice(0, 2).join(".")}
                    </span>
                  </div>

                  {/* Content */}
                  <div>
                    <div className="flex flex-wrap items-baseline gap-3">
                      <a
                        href={`${SITE_CONFIG.github}/releases/tag/v${release.version}`}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="font-mono text-lg font-semibold text-foreground hover:text-[var(--primary)] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        v{release.version}
                      </a>
                      <time
                        dateTime={release.date}
                        className="text-sm text-[var(--muted-foreground)]"
                      >
                        {formatDate(release.date)}
                      </time>
                    </div>

                    <p className="mt-1 font-medium text-foreground">{release.summary}</p>

                    <ul className="mt-4 space-y-3" aria-label="Changes in this release">
                      {release.items.map((item, i) => (
                        <li key={i} className="flex items-start gap-3">
                          <span
                            className="mt-0.5 inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-semibold"
                            style={{
                              background: `color-mix(in oklch, ${TAG_COLOR[item.tag]} 15%, transparent)`,
                              color: TAG_COLOR[item.tag],
                            }}
                          >
                            {item.tag}
                          </span>
                          <span className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                            {item.text}
                          </span>
                        </li>
                      ))}
                    </ul>

                    {release.featureLinks && release.featureLinks.length > 0 && (
                      <div className="mt-4 flex flex-wrap gap-3">
                        {release.featureLinks.map((link) => (
                          <Link
                            key={link.href}
                            href={link.href}
                            className="inline-flex items-center gap-1.5 rounded-full border border-[var(--border)] bg-card px-3 py-1 text-xs font-medium text-[var(--muted-foreground)] transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                          >
                            {link.label}
                          </Link>
                        ))}
                      </div>
                    )}
                  </div>
                </li>
              ))}
            </ol>
          </div>

          {/* Full history link */}
          <div className="mt-14 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6 text-center">
            <p className="text-[var(--muted-foreground)]">
              This page shows the most recent releases. For the complete release history and all
              release artifacts:
            </p>
            <a
              href={`${SITE_CONFIG.github}/releases`}
              target="_blank"
              rel="noreferrer noopener"
              className="mt-3 inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              View all releases on GitHub
            </a>
          </div>
        </Container>
      </Section>
    </>
  );
}
