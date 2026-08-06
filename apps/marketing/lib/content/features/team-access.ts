// Team access feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const TEAM_ACCESS_PAGE: FeaturePageData = {
  slug: "team-access",
  title: "WordPress Team Access Control and Audit Log",
  metaTitle: "WordPress Team Access Control with Audit Log | WPMgr",
  metaDescription:
    "Four-role access control, per-site sharing, OIDC SSO, and a tamper-evident audit log for your WordPress fleet. Share one site without exposing the rest. Self-hosted and open source.",
  hero: {
    eyebrow: "Team and access",
    heading: "WordPress team access control with a tamper-evident audit log",
    subhead:
      "Four roles from owner to viewer, per-site sharing, OIDC SSO, and a hash-chained audit log of every action across the fleet. Share a single site with a collaborator without exposing the rest of the portfolio.",
    primaryCta: { label: "Set up team access for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Sharing WordPress fleet access with a collaborator should not mean giving them a view of every client's sites",
    body: "Most fleet management tools treat access as all-or-nothing: either a team member sees every site or nothing. Agencies and freelancers often need to share a single site with a client, a contractor, or a developer without exposing the rest of the portfolio. WPMgr's per-site sharing model gives a collaborator the exact scope they need, no more.",
  },
  steps: [
    {
      n: "1",
      icon: "UserPlus",
      title: "Invite team members by email",
      desc: "Send email invites directly from the dashboard. New members verify their email address and are prompted to set a password. OIDC SSO can be configured for company-wide single sign-on.",
    },
    {
      n: "2",
      icon: "Users",
      title: "Assign a role with least privilege",
      desc: "Four roles: owner, admin, member, and viewer. Owner has full access including billing and org settings. Viewer can see dashboards and reports but cannot make changes. Roles apply fleet-wide.",
    },
    {
      n: "3",
      icon: "Network",
      title: "Share individual sites",
      desc: "Share a single site with any user at a specific role level. The user sees only that site in their dashboard. Revoke access at any time from the site or from the user's profile.",
    },
    {
      n: "4",
      icon: "ScrollText",
      title: "Review the audit log",
      desc: "Every login, role change, site action, backup, update, and configuration change is recorded in a hash-chained audit log. The chain break detector shows if any record was tampered with or deleted.",
    },
  ],
  subFeatures: [
    {
      icon: "Users",
      title: "Four-role access model",
      desc: "Owner, admin, member, and viewer. Roles apply fleet-wide. Per-site overrides allow narrower or broader access for specific collaborators.",
    },
    {
      icon: "Network",
      title: "Per-site sharing",
      desc: "Share exactly one site with a user without giving them fleet access. Revoke access instantly without affecting their access to other sites.",
    },
    {
      icon: "KeySquare",
      title: "OIDC SSO",
      desc: "Configure your company's identity provider for single sign-on. Members authenticate with their corporate account; WPMgr does not store their password.",
    },
    {
      icon: "ScrollText",
      title: "Tamper-evident audit log",
      desc: "Every action is recorded in a hash-chained log. The chain break detector identifies gaps, modifications, and deletions. An integrity report explains the cause of any detected break.",
    },
    {
      icon: "KeyRound",
      title: "Dashboard two-factor authentication",
      desc: "TOTP and WebAuthn passkeys protect dashboard access. Trusted devices, recovery codes, and a single-use code flow ensure operators can always recover access.",
    },
    {
      icon: "ShieldCheck",
      title: "API key management",
      desc: "Create named API keys with scoped permissions for automation and integrations. Keys can be revoked individually without affecting other keys or team member access.",
    },
  ],
  faq: [
    {
      q: "Can I give someone access to just one site, not the whole fleet?",
      a: "Yes. Per-site sharing gives a specific user access to a single site at a role level you choose. They see only that site in their dashboard. Revoking the share removes access to that site without affecting their access to anything else.",
    },
    {
      q: "What does the viewer role allow?",
      a: "Viewers can see the dashboard, read site health data, view reports, and download backups, but cannot trigger updates, run cleanups, change settings, or modify access. The viewer role is designed for clients and stakeholders who need visibility without control.",
    },
    {
      q: "Is the audit log tamper-proof?",
      a: "The audit log uses a hash chain where each record includes the hash of the previous record. If a record is modified or deleted the chain breaks at that point. WPMgr's chain break detector identifies breaks and classifies them as missing events, link mismatches, content modifications, or chain start missing.",
    },
    {
      q: "Can I use OIDC SSO with an identity provider like Okta or Azure AD?",
      a: "Yes. WPMgr supports any OIDC-compliant identity provider. Configure the provider URL, client ID, and client secret in the organisation settings. Members who sign in via SSO do not need a separate WPMgr password.",
    },
  ],
  siblingLinks: [
    { label: "Security suite", href: "/features/security" },
    { label: "White-label reports", href: "/features/client-reports" },
  ],
  solutionLinks: [
    { label: "WPMgr for agencies", href: "/solutions/agencies" },
    { label: "WordPress security solutions", href: "/solutions/wordpress-security" },
  ],
};
