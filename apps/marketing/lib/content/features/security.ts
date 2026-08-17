// Security feature page content. Seeded from apps/landing SECURITY.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const SECURITY_PAGE: FeaturePageData = {
  slug: "security",
  title: "WordPress Security Suite",
  metaTitle: "WordPress Security Hardening and Vulnerability Scanner",
  metaDescription:
    "WordPress security hardening, IP bans, file integrity monitoring, vulnerability scanning via Wordfence Intelligence, site-user 2FA, and password policy. Open source, self-hostable, default-off.",
  hero: {
    eyebrow: "Security Suite",
    heading: "WordPress security hardening and vulnerability scanning in one dashboard",
    subhead:
      "Per-site hardening, IP ban lists, file integrity monitoring, vulnerability scanning via Wordfence Intelligence, and site-user 2FA, all opt-in and default-off, built so a mistake can never lock you out of your own sites.",
    primaryCta: { label: "Harden your sites for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Security plugins that are on by default and cannot be undone are a liability, not a safeguard",
    body: "Hardening rules that cannot be reversed lock operators out of their own sites. Ban lists without an operator allow-list block the person managing the site. File integrity checks without a review workflow advance the baseline past unreviewed changes silently. WPMgr's security suite is default-off, every control has an explicit undo path, and the operator can always recover via wp-config constants.",
  },
  steps: [
    {
      n: "1",
      icon: "ShieldCheck",
      title: "Enable hardening controls per site",
      desc: "Push hardening rules from the dashboard: disable the file editor, restrict XML-RPC and the REST API, force SSL with HSTS, block PHP in uploads, and protect system files. All controls are opt-in and can be reversed from the dashboard.",
    },
    {
      n: "2",
      icon: "ShieldAlert",
      title: "Manage the IP and user-agent ban list",
      desc: "Block individual IPs, CIDR ranges, and user agents at early-boot and at the web-server level. The operator allow-list is always honoured so a ban rule can never lock out the operator.",
    },
    {
      n: "3",
      icon: "FileScan",
      title: "Scan for file changes and vulnerabilities",
      desc: "File hashes are compared against WordPress.org checksums for core, hosted plugins, and themes, and against a learned per-site baseline for everything else. Plugins, themes, and core are checked against the Wordfence Intelligence vulnerability feed with severity, affected version, and CVE references.",
    },
    {
      n: "4",
      icon: "Users",
      title: "Enforce 2FA and password policy for site users",
      desc: "Require TOTP, email codes, or backup codes for chosen WordPress user roles, enforced at the login screen. Set a minimum password strength, block known-compromised passwords, and optionally expire passwords. Grace logins and wp-config recovery constants mean operators are never locked out.",
    },
  ],
  subFeatures: [
    {
      icon: "ShieldCheck",
      title: "WordPress hardening controls",
      desc: "Disable the file editor, restrict XML-RPC and the REST API, force SSL with HSTS, block PHP in uploads, and protect system files. All opt-in and default-off.",
    },
    {
      icon: "ShieldAlert",
      title: "IP and user-agent ban list",
      desc: "Block IPs, CIDR ranges, and user agents at early-boot and at the web-server level. The operator allow-list is always honoured so bans can never lock out the operator.",
    },
    {
      icon: "FileScan",
      title: "File integrity monitoring",
      desc: "Hashes compared against WordPress.org checksums and a learned per-site baseline. Changed, added, and removed files are reported and stay flagged until an operator explicitly reviews them.",
    },
    {
      icon: "ShieldAlert",
      title: "Vulnerability scanner",
      desc: "Plugins, themes, and core checked against the Wordfence Intelligence vulnerability feed. One-click remediation updates the vulnerable component through the existing update flow. Requires a free Wordfence Intelligence API key.",
    },
    {
      icon: "FileLock2",
      title: "Client-side encrypted backups",
      desc: "Optional end-to-end encryption for backups: the control plane stores only ciphertext and never holds the decryption key.",
    },
    {
      icon: "KeySquare",
      title: "Password policy for site users",
      desc: "Set minimum strength, block compromised passwords via a privacy-preserving prefix query, block reuse, and optionally expire passwords with a forced-change screen.",
    },
  ],
  faq: [
    {
      q: "Can a hardening rule lock me out of my own site?",
      a: "WPMgr designs every hardening control to have an explicit undo path. Controls are default-off and applied only when you enable them. If a rule causes a problem, it can be reversed from the dashboard. wp-config recovery constants are documented for every control that could affect admin access.",
    },
    {
      q: "How does the vulnerability scanner work?",
      a: "WPMgr checks installed plugins, themes, and WordPress core against the Wordfence Intelligence vulnerability feed, which is the same database that powers Wordfence's own scanner. Each finding shows severity, affected version, fixed version, and CVE references. Remediation updates the vulnerable component through the existing update flow. A free Wordfence Intelligence API key is required.",
    },
    {
      q: "What happens if file integrity monitoring flags a file I intentionally changed?",
      a: "Flagged files stay flagged until an operator explicitly reviews and accepts the change in the dashboard. The baseline never silently advances past an unreviewed change. Once you accept a change, it becomes the new baseline for that file.",
    },
    {
      q: "Can the IP ban list lock out the operator?",
      a: "No. The operator allow-list is always processed before the ban list. An operator IP in the allow-list cannot be blocked even if it also matches a ban rule.",
    },
    {
      q: "Is site-user 2FA compatible with automated logins?",
      a: "Yes. Autologin via the WPMgr agent (used for one-click wp-admin access) always bypasses the 2FA interstitial by design. wp-config recovery constants and backup codes provide additional recovery paths.",
    },
  ],
  siblingLinks: [
    { label: "Two-factor authentication", href: "/features/two-factor-auth" },
    { label: "Team and access control", href: "/features/team-access" },
  ],
  solutionLinks: [
    { label: "WordPress security solutions", href: "/solutions/wordpress-security" },
  ],
};
