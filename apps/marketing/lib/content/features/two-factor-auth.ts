// Two-factor authentication feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const TWO_FACTOR_AUTH_PAGE: FeaturePageData = {
  slug: "two-factor-auth",
  title: "WordPress Two-Factor Authentication",
  metaTitle: "WordPress 2FA: TOTP and Email Code Authentication",
  metaDescription:
    "Add WordPress two-factor authentication for site users with TOTP authenticator apps, email codes, and backup codes. Enforced per role, with grace logins and wp-config recovery so operators are never locked out.",
  hero: {
    eyebrow: "Two-factor authentication",
    heading: "WordPress two-factor authentication for site users, enforced per role",
    subhead:
      "Require TOTP authenticator codes, email codes, or backup codes for chosen WordPress user roles, enforced at the login screen. Grace logins, remember-device windows, and wp-config recovery constants mean operators are never locked out.",
    primaryCta: { label: "Enable 2FA for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Stolen passwords are the leading cause of WordPress site compromises, and most 2FA implementations lock operators out when they lose a device",
    body: "A strong password alone does not stop credential stuffing, phishing, or a leaked database. Two-factor authentication closes that gap but only if the implementation handles device loss and recovery gracefully. An overly strict 2FA setup that locks administrators out of production sites creates a security control that is worse than having none.",
  },
  steps: [
    {
      n: "1",
      icon: "ShieldCheck",
      title: "Choose which roles require 2FA",
      desc: "Configure 2FA requirements per WordPress user role from the WPMgr dashboard. Administrator, editor, author, contributor, and subscriber roles can each be set to required, optional, or not available.",
    },
    {
      n: "2",
      icon: "Smartphone",
      title: "Users set up their preferred method",
      desc: "Users who are required to set up 2FA are guided through the setup flow on their next login. They can choose a TOTP authenticator app or an email code delivered to their WordPress email address.",
    },
    {
      n: "3",
      icon: "KeyRound",
      title: "Backup codes for device loss",
      desc: "Each user generates a set of single-use backup codes during setup. Codes are hashed in the database and can be regenerated from the user profile at any time while 2FA is set up.",
    },
    {
      n: "4",
      icon: "RotateCcw",
      title: "Recovery via wp-config constants",
      desc: "Every 2FA control that could affect admin access has a documented wp-config constant for recovery. If a user is locked out and has lost their backup codes, an operator with server access can disable 2FA for that user.",
    },
  ],
  subFeatures: [
    {
      icon: "Smartphone",
      title: "TOTP authenticator app",
      desc: "Compatible with any TOTP authenticator app. The setup flow generates a QR code and a manual entry key. Replay protection burns each code after use.",
    },
    {
      icon: "Mail",
      title: "Email code fallback",
      desc: "Users can choose to receive a one-time code by email instead of using an authenticator app. Email codes expire after a short window.",
    },
    {
      icon: "KeyRound",
      title: "Single-use backup codes",
      desc: "Each user receives a set of backup codes during setup. Codes are single-use and hashed in the database. Generating a new set of codes immediately invalidates the previous set.",
    },
    {
      icon: "ShieldCheck",
      title: "Per-role enforcement",
      desc: "2FA requirements are configured per WordPress user role. Administrator and editor roles can be required while subscriber and contributor remain optional.",
    },
    {
      icon: "RotateCcw",
      title: "wp-config recovery constants",
      desc: "Every 2FA control that could block access has a documented recovery constant in wp-config.php. Operators with server access can always recover a locked-out user.",
    },
    {
      icon: "Activity",
      title: "Audit log of all 2FA events",
      desc: "Setup, successful logins, failed attempts, and backup code use are all recorded in the tamper-evident audit log.",
    },
  ],
  faq: [
    {
      q: "Which authenticator apps are supported?",
      a: "Any TOTP-compatible authenticator app works, including Authy, Google Authenticator, and 1Password. The setup flow generates a QR code and a manual entry key for apps that do not support QR scanning.",
    },
    {
      q: "What if a user loses their authenticator device?",
      a: "Each user generates backup codes during setup. Any backup code can be used in place of a TOTP code for a one-time login. After using a backup code, the user can set up 2FA again on their new device. If all backup codes are exhausted, a wp-config constant can be used by an operator with server access to reset 2FA for that user.",
    },
    {
      q: "Can autologin from the WPMgr dashboard bypass 2FA?",
      a: "Yes. The one-click wp-admin autologin used by the WPMgr dashboard bypasses the 2FA interstitial by design. It uses a short-lived signed token rather than the standard WordPress login flow.",
    },
    {
      q: "Is dashboard 2FA (for the WPMgr control plane) different from site-user 2FA?",
      a: "Yes. Dashboard 2FA protects access to the WPMgr control plane itself (TOTP and WebAuthn passkeys for the operator). Site-user 2FA protects WordPress user accounts on connected sites (TOTP and email codes). Both are configured separately.",
    },
  ],
  siblingLinks: [
    { label: "Security suite", href: "/features/security" },
    { label: "Team and access control", href: "/features/team-access" },
  ],
  solutionLinks: [
    { label: "WordPress security solutions", href: "/solutions/wordpress-security" },
  ],
};
