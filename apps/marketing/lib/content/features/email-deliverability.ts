// Email deliverability feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const EMAIL_DELIVERABILITY_PAGE: FeaturePageData = {
  slug: "email-deliverability",
  title: "WordPress SMTP Per Site and Email Log",
  metaTitle: "WordPress SMTP Per Site with Central Email Log | WPMgr",
  metaDescription:
    "Configure WordPress SMTP per site with SES, SendGrid, Mailgun, Postmark, or any SMTP server. Central searchable email log, named connections with automatic failover, and webhook bounce suppression.",
  hero: {
    eyebrow: "Per-site email",
    heading: "WordPress SMTP per site with a central email delivery log",
    subhead:
      "Configure outgoing email per site with SES, SendGrid, Mailgun, Postmark, or any SMTP server. Named connections, automatic failover, and a central searchable delivery log across your whole fleet.",
    primaryCta: { label: "Configure email delivery for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "WordPress sends email through the server's mail function by default, and bounces, failures, and spam classification are invisible",
    body: "The default WordPress mail setup uses PHP's mail() function which routes email through the server's local MTA with no authentication, no TLS, and no delivery visibility. Messages land in spam or fail silently. Diagnosing a missing password reset or a WooCommerce order confirmation requires SSH access to mail logs. Per-site SMTP with a central delivery log brings visibility and reliability to email across the whole fleet.",
  },
  steps: [
    {
      n: "1",
      icon: "MailCheck",
      title: "Add a named email connection",
      desc: "Configure a named connection per site: SES with IAM credentials, SendGrid, Mailgun, or Postmark API keys, or any SMTP server with a host, port, username, and password. Credentials are encrypted at rest.",
    },
    {
      n: "2",
      icon: "Zap",
      title: "Set a failover connection",
      desc: "Designate a secondary connection to use if the primary fails. WPMgr automatically routes through the failover without manual intervention, and the delivery log marks which connection was used.",
    },
    {
      n: "3",
      icon: "Activity",
      title: "Watch the delivery log",
      desc: "Every message sent through a WPMgr-managed connection is logged with recipient, subject, status, and the connection used. Search and filter by site, date, status, or recipient across the whole fleet.",
    },
    {
      n: "4",
      icon: "ShieldAlert",
      title: "Suppress bounces and complaints",
      desc: "Webhook integrations receive bounce and complaint events from SES, SendGrid, Mailgun, Postmark, and other supported providers. Suppressed addresses are tracked per site to prevent repeat sends to known-bad recipients.",
    },
  ],
  subFeatures: [
    {
      icon: "MailCheck",
      title: "SES, SendGrid, Mailgun, Postmark, and SMTP",
      desc: "Named connections for the major transactional email providers and any standard SMTP server. API keys and SMTP credentials are encrypted at rest.",
    },
    {
      icon: "Zap",
      title: "Automatic failover",
      desc: "Designate a secondary connection per site. If the primary fails, WPMgr routes through the failover automatically and marks the delivery in the log.",
    },
    {
      icon: "Activity",
      title: "Central searchable delivery log",
      desc: "Every sent message is logged with recipient, subject, status, timestamp, and which connection was used. Searchable and filterable across the whole fleet.",
    },
    {
      icon: "ShieldAlert",
      title: "Webhook bounce and complaint suppression",
      desc: "Bounce and complaint webhooks from supported providers are received and processed. Suppressed addresses are tracked per site to prevent sending to known-bad recipients.",
    },
    {
      icon: "LayoutGrid",
      title: "Fleet-wide deliverability view",
      desc: "A fleet-wide deliverability summary shows which sites have unconfigured email, recent bounce rates, and sites with suppressed address lists that need attention.",
    },
    {
      icon: "LockKeyhole",
      title: "Credentials encrypted at rest",
      desc: "SMTP passwords and API keys are encrypted at rest in the control plane. Plain-text credentials are never stored in the database.",
    },
  ],
  faq: [
    {
      q: "Which email providers are supported?",
      a: "Amazon SES with IAM credentials, SendGrid, Mailgun, and Postmark via their respective API integrations, plus any SMTP server with host, port, username, and password. Additional providers can be added via standard SMTP.",
    },
    {
      q: "What is logged in the delivery log?",
      a: "Each log entry records the recipient address, the subject line, the sending status, the timestamp, and the named connection that was used. Log entries are retained for 14 days by default. Message bodies are not stored.",
    },
    {
      q: "How do bounce and complaint webhooks work?",
      a: "WPMgr provides a webhook endpoint per site that you configure in your email provider's dashboard. Bounces and complaints received through the webhook are recorded in the suppression list for that site. Future send attempts to suppressed addresses are blocked.",
    },
    {
      q: "Are email credentials stored securely?",
      a: "Yes. SMTP passwords and API keys are encrypted at rest using the control plane's encryption key. They are decrypted only when a send is initiated and are never exposed in plaintext via the API.",
    },
  ],
  siblingLinks: [
    { label: "White-label reports", href: "/features/client-reports" },
    { label: "Uptime and health monitoring", href: "/features/uptime-monitoring" },
  ],
  solutionLinks: [
    { label: "WPMgr for agencies", href: "/solutions/agencies" },
  ],
};
