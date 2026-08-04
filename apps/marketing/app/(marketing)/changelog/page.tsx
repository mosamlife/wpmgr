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
  canonical: "/changelog/",
});

// ---------------------------------------------------------------------------
// Curated release entries (newest first, harvested from CHANGELOG.md).
// We show the ~20 most recent meaningful releases. The full history is
// available on GitHub Releases.
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Updates", href: "/features/updates/" }],
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
    featureLinks: [{ label: "Real User Monitoring", href: "/features/real-user-monitoring/" }],
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
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
      { label: "Security suite", href: "/features/security/" },
      { label: "Real User Monitoring", href: "/features/real-user-monitoring/" },
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring/" }],
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
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring/" }],
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
    featureLinks: [{ label: "Team and access control", href: "/features/team-access/" }],
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
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
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
    featureLinks: [{ label: "File Manager", href: "/features/file-manager/" }],
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
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
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
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
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
      { label: "Two-factor auth", href: "/features/two-factor-auth/" },
      { label: "Security suite", href: "/features/security/" },
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
    featureLinks: [{ label: "Two-factor auth", href: "/features/two-factor-auth/" }],
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
    { name: "Changelog", href: "/changelog/" },
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
