// Client reports feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const CLIENT_REPORTS_PAGE: FeaturePageData = {
  slug: "client-reports",
  title: "White-Label WordPress Reports",
  metaTitle: "White-Label WordPress Reports and Client Maintenance Reports",
  metaDescription:
    "Send branded WordPress maintenance reports to clients on a schedule or on demand. Uptime, backups, updates, and Core Web Vitals in one HTML email or PDF, footer removable.",
  hero: {
    eyebrow: "Client reports",
    heading: "White-label WordPress maintenance reports for clients",
    subhead:
      "Branded maintenance reports delivered by email or available as a PDF on a schedule or on demand. Uptime, backups, updates, Core Web Vitals, and email deliverability in one report, with your logo, your colour, and your sign-off.",
    primaryCta: { label: "Start sending reports for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Clients do not see the maintenance work that keeps their sites healthy, and assembling a monthly report manually takes hours you do not have",
    body: "Backup runs, update history, uptime percentage, security scans, and email deliverability are all in separate parts of the dashboard. Assembling them into a client-ready email monthly is repetitive work with a high chance of copy-paste errors. WPMgr generates the report automatically from live data, applies your brand, and delivers it by email or makes it available for download, on a schedule you set.",
  },
  steps: [
    {
      n: "1",
      icon: "Briefcase",
      title: "Group sites under client records",
      desc: "Assign one or more sites to a named client record with a brand colour, logo, contacts, and notes. The client record becomes the unit of reporting, so a report spans all the sites for that client.",
    },
    {
      n: "2",
      icon: "ScrollText",
      title: "Customise the report sections",
      desc: "Toggle which sections appear in each report: uptime, backups, updates, Core Web Vitals, email deliverability. Add a custom introduction and a closing message. Remove the powered-by footer.",
    },
    {
      n: "3",
      icon: "Mail",
      title: "Schedule or send on demand",
      desc: "Set a monthly or weekly schedule for each client, or generate a report immediately from the client page. Reports are delivered as branded HTML email and are available as a print-ready page or a vector-chart PDF.",
    },
    {
      n: "4",
      icon: "LayoutDashboard",
      title: "Share via the client portal",
      desc: "Clients can log in to a read-only branded portal to view their sites, download past reports, and check uptime and backup health without contacting you for a status update.",
    },
  ],
  subFeatures: [
    {
      icon: "ScrollText",
      title: "Scheduled and on-demand reports",
      desc: "Set a monthly or weekly schedule per client or generate a report immediately. Each report pulls live data at generation time.",
    },
    {
      icon: "Briefcase",
      title: "Brand colour, logo, and sign-off",
      desc: "Each client record carries your brand colour and logo. Reports use them throughout. The powered-by footer is removable.",
    },
    {
      icon: "LayoutDashboard",
      title: "HTML email, print page, and PDF",
      desc: "Reports are delivered as a branded HTML email, a print-ready page, and a vector-chart PDF. All three formats are generated from the same data.",
    },
    {
      icon: "MailCheck",
      title: "Per-section toggles",
      desc: "Include or exclude uptime, backups, updates, Core Web Vitals, and email deliverability per report. Sections can be re-ordered and custom text added to introduction and closing.",
    },
    {
      icon: "Users",
      title: "Client portal access",
      desc: "Invite a client to a read-only portal where they can see their sites, download reports, and check uptime and backup health. Access is revoked instantly on removal.",
    },
    {
      icon: "Activity",
      title: "Live data at generation time",
      desc: "Reports always reflect the most recent data: uptime percentage, last backup date and status, pending updates, and Core Web Vitals p75 values are pulled at the moment the report is generated.",
    },
  ],
  faq: [
    {
      q: "Can I remove the WPMgr branding from reports?",
      a: "Yes. The powered-by footer can be removed from any report. Your logo and brand colour appear throughout the report in place of WPMgr branding.",
    },
    {
      q: "What data goes into a report?",
      a: "Reports can include uptime percentage and incident history, backup status and last successful backup date, update history for plugins, themes, and core, Core Web Vitals p75 values, and email deliverability summary. Each section can be enabled or disabled per report.",
    },
    {
      q: "Can clients view the report without me sending it?",
      a: "Yes. The client portal gives clients a read-only, branded view of their sites where they can download past reports and check current uptime and backup health. Portal access is controlled from the WPMgr dashboard and revoked instantly when removed.",
    },
    {
      q: "How are reports delivered?",
      a: "Reports are sent as branded HTML email using your configured per-site or instance SMTP settings. They are also available as a print-ready page and a downloadable PDF from the report detail in the dashboard and the client portal.",
    },
  ],
  siblingLinks: [
    { label: "Per-site email and deliverability", href: "/features/email-deliverability" },
    { label: "Team and access control", href: "/features/team-access" },
  ],
  solutionLinks: [
    { label: "WPMgr for agencies", href: "/solutions/agencies" },
  ],
};
