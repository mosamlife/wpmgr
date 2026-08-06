// Solution page: WordPress security.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { SITE_CONFIG } from "@/lib/site";

export const WORDPRESS_SECURITY_SOLUTION: SolutionPageData = {
  slug: "wordpress-security",
  title: "WordPress security",
  heading: "WordPress security for your whole fleet",
  metaTitle: "WordPress Security | WPMgr",
  metaDescription:
    "WPMgr delivers fleet-wide WordPress security hardening, vulnerability scanning via Wordfence Intelligence, file integrity monitoring, two-factor authentication, and IP ban lists from a single self-hosted dashboard.",
  layoutVariant: "default",
  hero: {
    eyebrow: "Security suite",
    subhead:
      "Harden every site in your fleet, scan for known vulnerabilities, enforce two-factor authentication for site users, and maintain a tamper-evident audit trail, all from one self-hosted dashboard with no per-site security plugin sprawl.",
    primaryCta: { label: "Start securing your fleet", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  outcomes: {
    heading: "Security that covers every site, not just the ones you remember",
    body: "WordPress security problems compound quickly when you manage more than a handful of sites. A single outdated plugin with a known CVE, one site user without two-factor authentication, or a file that changed without an audit trail is enough to cause an incident that takes days to recover from. WPMgr approaches security at the fleet level: hardening rules applied consistently, vulnerability data pulled from Wordfence Intelligence refreshed daily, file integrity checked against a verified baseline, and 2FA enforced per role across all site users. The audit log is hash-chained so any tampering is immediately detectable. You close the gaps before an attacker finds them.",
  },
  provingFeatures: [
    {
      featureSlug: "security",
      icon: "ShieldCheck",
      title: "Security hardening suite",
      summary: "Hardening rules, IP ban lists refreshed from a live threat feed, file integrity monitoring, and vulnerability scanning powered by Wordfence Intelligence.",
      href: "/features/security",
    },
    {
      featureSlug: "two-factor-auth",
      icon: "Smartphone",
      title: "Two-factor authentication",
      summary: "Enforce TOTP or email code 2FA for WordPress site users per role, with backup codes and recovery paths that cannot lock out an admin.",
      href: "/features/two-factor-auth",
    },
    {
      featureSlug: "team-access",
      icon: "Users",
      title: "Audit log and access control",
      summary: "A tamper-evident hash-chained audit log of every action, combined with per-site permission roles and OIDC SSO.",
      href: "/features/team-access",
    },
  ],
  stats: [
    { icon: "ShieldCheck", value: "Daily", label: "Vulnerability feed refresh via Wordfence Intelligence" },
    { icon: "FileSearch", value: "100%", label: "File integrity checked against a verified baseline" },
    { icon: "Lock", value: "Hash-chained", label: "Audit log entries are tamper-evident by design" },
  ],
  faq: [
    {
      q: "Where does the vulnerability data come from?",
      a: "WPMgr pulls from the Wordfence Intelligence vulnerability feed, a publicly available database of known WordPress plugin and theme CVEs. The feed is refreshed daily and matched against installed plugins and themes on every site in your fleet.",
    },
    {
      q: "Will enforcing 2FA lock out an administrator?",
      a: "No. WPMgr's 2FA enforcement is designed with lockout prevention as a hard invariant. The autologin path used by the agent always bypasses the 2FA challenge. Administrators always have access to backup codes, and a wp-config.php escape hatch is documented for recovery if every other method is unavailable.",
    },
    {
      q: "What does file integrity monitoring check?",
      a: "WPMgr establishes a cryptographic baseline of your WordPress core files, active theme, and active plugins. Subsequent scans compare the live filesystem against this baseline and report any additions, deletions, or modifications that did not come from a managed update.",
    },
    {
      q: "Can I see all security events across my fleet in one place?",
      a: "Yes. The activity log and audit trail in the dashboard aggregates security events from every connected site. The log is hash-chained, meaning each entry cryptographically references the previous one, so any deletion or modification of a past entry is immediately detectable.",
    },
    {
      q: "What hardening rules does WPMgr apply?",
      a: "Hardening rules include disabling XML-RPC, blocking directory traversal, setting secure file permissions, removing version leakage from headers, and applying site-specific .htaccess or Nginx directives depending on the detected server stack. Rules are applied without overwriting your existing configuration.",
    },
  ],
};
