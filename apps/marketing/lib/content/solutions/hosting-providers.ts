// Solution page: managed WordPress tooling for hosting providers.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { SITE_CONFIG } from "@/lib/site";

export const HOSTING_PROVIDERS_SOLUTION: SolutionPageData = {
  slug: "hosting-providers",
  title: "For hosting providers",
  heading: "Managed WordPress tooling for hosting providers",
  metaTitle: "Managed WordPress Tooling for Hosting Providers",
  metaDescription:
    "WPMgr is open-source WordPress fleet management to embed in your hosting stack: security hardening, uptime monitoring, and team access, without building it yourself.",
  layoutVariant: "compact",
  hero: {
    eyebrow: "Hosting integrations",
    subhead:
      "Offer your customers a self-service WordPress management layer without building it from scratch. Open-source, AGPL-licensed, and designed to run on the infrastructure you already operate.",
    primaryCta: { label: "Explore the code", href: SITE_CONFIG.github, variant: "primary", icon: "Github" },
    secondaryCta: { label: "See all features", href: "/features", variant: "secondary", icon: "ArrowRight" },
  },
  outcomes: {
    heading: "A managed-WordPress layer without the engineering cost",
    body: "Hosting providers that want to offer value-added WordPress management typically face a choice: build a proprietary tooling layer from scratch, or bundle a third-party SaaS product whose pricing and roadmap you do not control. WPMgr is a third option: a fully open-source, self-hostable control plane you can fork, embed, and brand. The agent is MIT-licensed and Ed25519-signed so your customers can audit every message it sends. Security hardening, uptime monitoring, and team access control are ready to ship on day one. You bring the infrastructure; WPMgr brings the tooling.",
  },
  provingFeatures: [
    {
      featureSlug: "team-access",
      icon: "Users",
      title: "Team and access control",
      summary: "Four permission roles, per-site sharing, OIDC SSO, and a tamper-evident audit log so your support team can operate safely.",
      href: "/features/team-access",
    },
    {
      featureSlug: "security",
      icon: "ShieldCheck",
      title: "Security hardening suite",
      summary: "Hardening rules, file integrity monitoring, vulnerability scanning powered by Wordfence Intelligence, and IP ban lists across the fleet.",
      href: "/features/security",
    },
    {
      featureSlug: "uptime-monitoring",
      icon: "Activity",
      title: "Uptime and health monitoring",
      summary: "Per-site uptime checks, TLS expiry alerts, and a fleet status matrix that gives your support team an instant overview.",
      href: "/features/uptime-monitoring",
    },
  ],
  stats: [
    { icon: "FileBadge", value: "MIT", label: "Agent license: permissive, auditable, forkable" },
    { icon: "KeyRound", value: "Ed25519", label: "Every agent message is cryptographically signed" },
    { icon: "ShieldCheck", value: "AGPL", label: "Full source control plane available to read and fork" },
  ],
  faq: [
    {
      q: "Can I white-label the dashboard for my customers?",
      a: "WPMgr is fully open-source under the AGPL, so you can fork and rebrand the control plane. White-label maintenance reports with your logo are available in the current release for agency and client-facing deployments.",
    },
    {
      q: "How does the agent communicate with the control plane?",
      a: "The MIT-licensed WordPress agent communicates over HTTPS using Ed25519-signed messages. Every command the control plane sends is signed with a key the site owner can verify, so your customers can confirm that nothing happens to their site you cannot account for.",
    },
    {
      q: "What does the security suite cover?",
      a: "The security suite includes hardening rule application, file integrity monitoring with baseline comparison, IP ban lists fed from the Wordfence Intelligence feed, and ongoing vulnerability scanning for installed plugins and themes. All of this runs via the agent on the customer's own server.",
    },
    {
      q: "Is there an API for provisioning sites programmatically?",
      a: "Yes. The WPMgr control plane exposes a full REST API documented at manage.wpmgr.app/docs. You can add sites, trigger backups, run update jobs, and retrieve health data programmatically from your own automation layer.",
    },
  ],
};
