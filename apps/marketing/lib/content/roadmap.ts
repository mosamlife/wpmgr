// /roadmap page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
//
// Source of truth is GitHub issue #474 ("Roadmap"). This file is a copy of
// that issue's structure and claims, not a new statement: shipping now,
// committed next, under research, explicit non-goals, and a closing note on
// how to read the list. Dates are deliberately absent throughout, matching
// the issue: this is a statement of intent, not a delivery schedule.
import { SITE_CONFIG } from "@/lib/site";

const issueHref = (n: number) => `${SITE_CONFIG.github}/issues/${n}`;

export const ROADMAP = {
  metaTitle: "Roadmap: what we are building and what we are not",
  metaDescription:
    "What WPMgr is building next, what is only under research, and what we have explicitly decided not to build. No dates: we would rather move an item than defend one.",
  hero: {
    heading: "Roadmap",
    subhead: "What we are building, and what we have decided not to build.",
    note: "This is a statement of intent, not a delivery schedule. Dates are deliberately absent, because we would rather move an item than defend a date.",
  },

  shippingNow: {
    eyebrow: "Shipping now",
    heading: "In build today",
    items: [
      {
        title: "Scheduled operations",
        badge: "In build",
        summary: "Set a time for updates, backups, and scans, and have them run at that time.",
        context:
          "Today scheduled_at is accepted and stored but not honoured: a run submitted for 02:00 executes immediately. That is being fixed properly rather than patched.",
        points: [
          "Work waits for its start time without being mistaken for stalled work.",
          "A run that could not start reports that it did not start, and that nothing was sent to your sites, rather than failing silently or arriving hours late at business open.",
          "A scheduled run does not block an urgent update to the same plugin in the meantime.",
          "Runs on our own infrastructure. No third-party scheduling service.",
        ],
        tracking: { label: "Tracked in #463", href: issueHref(463), note: "The first piece is merged." },
      },
    ],
  },

  committedNext: {
    eyebrow: "Committed next",
    heading: "Decided, not yet shipped",
    items: [
      {
        title: "Honest uptime history",
        summary:
          "Our 90-day availability chart currently derives most of its cells from a single 7-day figure, and a site that has never been probed renders as 90 days of outage.",
        detail:
          "Both are being replaced with measured data and an explicit state for a site that has not been measured yet.",
        tracking: { label: "Tracked in #460", href: issueHref(460) },
      },
      {
        title: "Backup and restore depth",
        points: [
          "Bring your own storage: S3, Backblaze B2, Google Drive, Dropbox.",
          "A destination chosen per backup rather than one global setting.",
          "Restore to a different domain, with URLs rewritten inside serialised data.",
          "Retention policies.",
          "Verification that asks the destination whether the bytes are retrievable, rather than only confirming our own counters agree.",
        ],
      },
      {
        title: "Clone and host-change detection",
        summary:
          "A restored backup or a staging clone currently carries live control-plane authority for the site it was copied from.",
        detail: "Detecting that a site has moved, and requiring re-enrolment, closes it.",
      },
    ],
  },

  underResearch: {
    eyebrow: "Under research, not yet committed",
    heading: "Assistant and AI integration",
    disclaimer: "This is not a commitment. It has no shipping date and no decided shape.",
    body: [
      "We are not announcing a shape for this yet, deliberately.",
      "WordPress has published an official MCP adapter and an Abilities API. Whether the right answer is to implement that standard or to build our own connector is a decision we have not finished making, and committing publicly before making it would be committing to the wrong thing.",
      "What we can say: the capability an assistant would drive already exists. Every operation is an authorised, per-site, per-command signed instruction with an audit record. The work is exposure and authorisation, not new capability.",
    ],
    prerequisitesLabel: "Two things we consider prerequisites rather than follow-ons:",
    prerequisites: [
      "A read-only ceiling that applies at discovery as well as execution, so an assistant cannot reach what it cannot see.",
      "Approval as a distinct act by a person, enforced by the server. A flag an automated caller can set for itself is not an approval, and an instruction in a description is not a control.",
    ],
  },

  nonGoals: {
    eyebrow: "Explicit non-goals",
    heading: "What we will not build",
    lead: "Published so they stop being re-proposed.",
    items: [
      {
        title: "Prompt-to-site generation",
        body: "Not our product. Generating a site from a description is a different business from operating sites that already exist.",
      },
      {
        title: "Starter-template libraries",
        body: "A content-production investment, not an engineering one, and only useful alongside a generator we are not building.",
      },
      {
        title: "Credit-metered AI billing",
        body: "Metering the iteration that makes an answer useful is a bad trade for the operator.",
      },
      {
        title: "A chat interface inside our dashboard",
        body: "If you are using an assistant, that assistant is the interface. Building a second one is the thing the protocol exists to avoid.",
      },
    ],
  },

  howToRead: {
    heading: "How to read this",
    points: [
      "Items move down this list, not up.",
      "Anything under Under research may change shape entirely, or be dropped.",
      "Anything under Non-goals will not appear later without a public change of mind and a stated reason.",
      "Issues are the source of truth for detail. This page is the intent behind them.",
    ],
  },
} as const;
