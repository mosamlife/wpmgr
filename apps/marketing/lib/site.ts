// Canonical site configuration and navigation. Used by layout, seo.ts, and
// footer. All URLs are absolute where external, relative where internal.

export const SITE_CONFIG = {
  name: "WPMgr",
  baseUrl: "https://wpmgr.app",
  description:
    "Open-source, self-hostable WordPress fleet manager. Backups and restore, safe updates, Media Optimizer (AVIF and WebP), full-page caching, Database Cleaner, uptime, and security scanning, with a signed MIT-licensed agent you can audit and contribute to.",
  github: "https://github.com/mosamlife/wpmgr",
  wordpressOrg: "https://wordpress.org/plugins/fleet-agent-site-manager/",
  dashboard: "https://manage.wpmgr.app",
  signup: "https://manage.wpmgr.app/register",
  docs: "/docs",
  themeStorageKey: "wpmgr-landing-theme",
} as const;

export type NavItem = {
  label: string;
  href: string;
  external?: boolean;
};

export type NavGroup = {
  label: string;
  items: NavItem[];
};

export const HEADER_NAV: NavItem[] = [
  { label: "Features", href: "/features" },
  { label: "Solutions", href: "/solutions" },
  { label: "Pricing", href: "/pricing" },
  { label: "Resources", href: "/resources" },
  { label: "Docs", href: SITE_CONFIG.docs },
];

export const FOOTER_NAV: NavGroup[] = [
  {
    label: "Product",
    items: [
      { label: "Features", href: "/features" },
      { label: "Media Optimizer", href: "/features/media-optimizer" },
      { label: "Backups", href: "/features/backups" },
      { label: "Performance", href: "/features/performance" },
      { label: "Security", href: "/features/security" },
      { label: "Pricing", href: "/pricing" },
    ],
  },
  {
    label: "Solutions",
    items: [
      { label: "For agencies", href: "/solutions/agencies" },
      { label: "For freelancers", href: "/solutions/freelancers" },
      { label: "WordPress backups", href: "/solutions/wordpress-backups" },
      { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
      { label: "WordPress security", href: "/solutions/wordpress-security" },
      { label: "Manage multiple sites", href: "/solutions/manage-multiple-sites" },
    ],
  },
  {
    label: "Resources",
    items: [
      { label: "Changelog", href: "/changelog" },
      { label: "Blog", href: "/blog" },
      { label: "Guides", href: "/guides" },
      { label: "API reference", href: SITE_CONFIG.docs },
      { label: "GitHub", href: SITE_CONFIG.github, external: true },
      { label: "WordPress.org listing", href: SITE_CONFIG.wordpressOrg, external: true },
      { label: "Contributing", href: `${SITE_CONFIG.github}/blob/main/docs/contributing.md`, external: true },
    ],
  },
  {
    label: "Company",
    items: [
      { label: "About", href: "/about" },
      { label: "Pricing", href: "/pricing" },
      { label: "Contact", href: "/contact" },
    ],
  },
  {
    label: "Legal",
    items: [
      { label: "Legal hub", href: "/legal" },
      { label: "Security policy", href: "/legal/security-policy" },
      { label: "Terms", href: "/terms" },
      { label: "Privacy", href: "/privacy" },
      { label: "Refund policy", href: "/refunds" },
      { label: "License", href: `${SITE_CONFIG.github}/blob/main/LICENSE`, external: true },
    ],
  },
];

export const WORDPRESS_TRADEMARK_DISCLAIMER =
  "WordPress is a trademark of the WordPress Foundation. WPMgr is an independent, self-hostable project and is not endorsed by, affiliated with, or sponsored by the WordPress Foundation or Automattic.";
