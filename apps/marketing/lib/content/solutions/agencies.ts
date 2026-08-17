// Solution page: WordPress management for agencies.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { signupHref } from "@/lib/site";

export const AGENCIES_SOLUTION: SolutionPageData = {
  slug: "agencies",
  title: "For agencies",
  heading: "WordPress management for agencies",
  metaTitle: "WordPress Management for Agencies",
  metaDescription:
    "WPMgr gives agencies a self-hosted control plane for every client's WordPress site. White-label reports, per-site email, team access control, and automated backups in one dashboard.",
  layoutVariant: "default",
  hero: {
    eyebrow: "Agency operations",
    subhead:
      "Run every client site from one dashboard. Deliver white-label maintenance reports, automate backups before every update, and give each team member exactly the access they need, without sharing passwords or switching accounts.",
    primaryCta: { label: "Get started for free", href: signupHref("solution-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "See the features", href: "/features", variant: "secondary", icon: "ArrowRight" },
  },
  outcomes: {
    heading: "From reactive support to proactive client service",
    body: "Agencies that move from ad-hoc site management to a central fleet dashboard stop losing hours to manual update routines and client report writing. Automated backups run before every update and restore with one click if something breaks. Branded maintenance reports go out on schedule without anyone opening a spreadsheet. Per-site email routing means your clients' transactional mail is reliable and auditable. The result is more sites managed per team member, with fewer support calls and more confident clients.",
  },
  provingFeatures: [
    {
      featureSlug: "client-reports",
      icon: "ScrollText",
      title: "White-label reports",
      summary: "Branded maintenance reports by email or PDF, on a schedule or on demand, with your logo and your client's domain front and centre.",
      href: "/features/client-reports",
    },
    {
      featureSlug: "email-deliverability",
      icon: "MailCheck",
      title: "Per-site email and delivery log",
      summary: "Route each client site through SES, SendGrid, Mailgun, Postmark, or SMTP, with a central delivery log so you can audit every sent message.",
      href: "/features/email-deliverability",
    },
    {
      featureSlug: "team-access",
      icon: "Users",
      title: "Team and access control",
      summary: "Four roles, per-site sharing, OIDC SSO, and a tamper-evident audit log that shows who changed what and when.",
      href: "/features/team-access",
    },
    {
      featureSlug: "backups",
      icon: "DatabaseBackup",
      title: "Automated backups",
      summary: "Scheduled incremental backups with fleet-wide backup health. Restore any client site to any snapshot without taking it offline.",
      href: "/features/backups",
    },
  ],
  stats: [
    { icon: "Users", value: "4", label: "Permission roles for clean team separation" },
    { icon: "ScrollText", value: "100%", label: "White-label: your brand, your domain" },
    { icon: "DatabaseBackup", value: "1-click", label: "Restore to any backup snapshot" },
  ],
  faq: [
    {
      q: "Can I give clients a read-only portal to view their site health?",
      a: "Yes. The client portal lets you invite a client with a Client role. They see their site's uptime, backup history, and the latest maintenance report without any access to settings, billing, or other clients' sites.",
    },
    {
      q: "Can different team members see only the sites assigned to them?",
      a: "Yes. Per-site sharing lets you assign any team member to a specific subset of sites. A junior developer can push updates to their assigned sites without ever seeing a client they are not responsible for.",
    },
    {
      q: "How are white-label reports delivered?",
      a: "Reports are generated on a weekly or monthly schedule, or on demand. They are sent by email to your client's address and are available as a PDF download from the client portal, all with your logo and colour scheme.",
    },
    {
      q: "Is this self-hosted? Where does client data live?",
      a: "WPMgr is fully self-hostable under the AGPL. You run the control plane on your own infrastructure. Client site data, backups, and logs never leave servers you control. The hosted cloud version is on the roadmap and will have the same capability.",
    },
    {
      q: "How does per-site email work for multiple clients?",
      a: "Each site gets its own email provider configuration. One client can use Amazon SES, another can use SendGrid, and a third can use a shared SMTP relay. The central delivery log spans all sites so you can audit or debug any message from one screen.",
    },
  ],
};
