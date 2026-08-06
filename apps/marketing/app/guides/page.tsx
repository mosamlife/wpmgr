import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd, buildItemListLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section, Eyebrow } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";
import { CTABand } from "@/components/sections/cta-band";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "WordPress Guides: Maintenance and Core Web Vitals",
  description:
    "Cornerstone long-form guides on running WordPress well. Start with the complete WordPress maintenance guide or the Core Web Vitals optimization guide.",
  canonical: "/guides",
});

// Static guide metadata. Intentionally separate from the file-system loader so
// this index renders at build time without touching the MDX files.
const GUIDES = [
  {
    slug: "wordpress-maintenance",
    icon: "ServerCog",
    eyebrow: "Cornerstone guide",
    title: "The Complete WordPress Maintenance Guide",
    description:
      "A comprehensive guide to WordPress maintenance: updates, backups, security, performance, and monitoring. Build a system that keeps every site healthy.",
    readTime: "25 min read",
    topics: ["Updates", "Backups", "Security", "Performance", "Monitoring"],
  },
  {
    slug: "core-web-vitals",
    icon: "Gauge",
    eyebrow: "Cornerstone guide",
    title: "Core Web Vitals for WordPress: The Complete Optimization Guide",
    description:
      "A complete guide to improving LCP, CLS, and INP on WordPress sites: measurement, root causes, and fixes with real techniques and measurable results.",
    readTime: "20 min read",
    topics: ["LCP", "CLS", "INP", "Caching", "Images"],
  },
] as const;

export default function GuidesIndexPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Resources", href: "/resources/" },
    { name: "Guides", href: "/guides/" },
  ]);

  const itemList = buildItemListLd(
    GUIDES.map((g) => ({
      name: g.title,
      description: g.description,
      url: `/guides/${g.slug}/`,
    })),
  );

  return (
    <>
      <JsonLd data={breadcrumb} />
      <JsonLd data={itemList} />
      <SiteHeader />
      <main>
        {/* Hero */}
        <section className="border-b border-[var(--border)] py-16 sm:py-20">
          <Container>
            <nav
              aria-label="Breadcrumb"
              className="mb-5 flex flex-wrap items-center gap-2 text-sm text-[var(--muted-foreground)]"
            >
              <Link href="/" className="hover:text-foreground transition-colors">
                Home
              </Link>
              <span aria-hidden>/</span>
              <Link href="/resources/" className="hover:text-foreground transition-colors">
                Resources
              </Link>
              <span aria-hidden>/</span>
              <span className="text-foreground">Guides</span>
            </nav>
            <div className="max-w-2xl">
              <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
                Guides
              </p>
              <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
                Learn to run WordPress well
              </h1>
              <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
                Cornerstone guides covering the full operational lifecycle of WordPress sites.
                Each guide goes deep on the mechanics, not just a checklist.
              </p>
            </div>
          </Container>
        </section>

        {/* Guide cards */}
        <Section>
          <Container>
            <ul className="grid gap-8 sm:grid-cols-2" role="list">
              {GUIDES.map((guide) => (
                <li key={guide.slug}>
                  <Link
                    href={`/guides/${guide.slug}/`}
                    className="group flex h-full flex-col rounded-xl border border-[var(--border)] bg-card p-7 shadow-sm transition-shadow hover:shadow-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    {/* Icon */}
                    <div className="mb-5 inline-flex h-11 w-11 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                      <Icon name={guide.icon} size={22} />
                    </div>

                    {/* Eyebrow + title */}
                    <div className="mb-3">
                      <Eyebrow>{guide.eyebrow}</Eyebrow>
                    </div>
                    <h2 className="text-lg font-semibold leading-snug text-foreground transition-colors group-hover:text-[var(--primary)]">
                      {guide.title}
                    </h2>

                    {/* Description */}
                    <p className="mt-3 flex-1 text-sm leading-relaxed text-[var(--muted-foreground)]">
                      {guide.description}
                    </p>

                    {/* Topics */}
                    <div className="mt-5 flex flex-wrap gap-2">
                      {guide.topics.map((topic) => (
                        <span
                          key={topic}
                          className="inline-flex items-center rounded-full border border-[var(--border)] bg-[var(--muted)]/40 px-2.5 py-0.5 text-xs font-medium text-[var(--muted-foreground)]"
                        >
                          {topic}
                        </span>
                      ))}
                    </div>

                    {/* Read time + CTA */}
                    <div className="mt-6 flex items-center justify-between">
                      <span className="text-xs text-[var(--muted-foreground)]">{guide.readTime}</span>
                      <span className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)]">
                        Read guide
                        <Icon name="ArrowRight" size={14} />
                      </span>
                    </div>
                  </Link>
                </li>
              ))}
            </ul>
          </Container>
        </Section>

        {/* Why guides matter */}
        <Section tone="muted">
          <Container>
            <div className="mx-auto max-w-2xl text-center">
              <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                Put it into practice with WPMgr
              </h2>
              <p className="mt-4 text-[var(--muted-foreground)] leading-relaxed">
                These guides explain the mechanics. WPMgr automates the work: scheduled backups,
                safe bulk updates, file integrity monitoring, page caching, and media optimization
                from one open-source control plane you can run on your own infrastructure.
              </p>
            </div>
            <div className="mt-10 grid gap-5 sm:grid-cols-3">
              {[
                {
                  icon: "DatabaseBackup",
                  title: "Automated backups",
                  desc: "Scheduled incremental backups with point-in-time restore across your whole fleet.",
                  href: "/features/backups/",
                },
                {
                  icon: "ShieldCheck",
                  title: "Security suite",
                  desc: "Hardening rules, file integrity scanning, vulnerability detection, and IP ban lists.",
                  href: "/features/security/",
                },
                {
                  icon: "Gauge",
                  title: "Performance and caching",
                  desc: "Full-page caching, AVIF and WebP media, Redis object cache, and Core Web Vitals from real visitors.",
                  href: "/features/performance/",
                },
              ].map((item) => (
                <Link
                  key={item.href}
                  href={item.href}
                  className="flex flex-col gap-3 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm transition-shadow hover:shadow-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  <div className="inline-flex h-9 w-9 items-center justify-center rounded-md bg-[var(--primary)]/10 text-[var(--primary)]">
                    <Icon name={item.icon} size={18} />
                  </div>
                  <p className="text-sm font-semibold text-foreground">{item.title}</p>
                  <p className="text-xs leading-relaxed text-[var(--muted-foreground)]">{item.desc}</p>
                </Link>
              ))}
            </div>
          </Container>
        </Section>

        <CTABand
          heading="Free, open-source, and self-hostable."
          subhead="No per-site fee. Run WPMgr on your own infrastructure or use the hosted cloud version."
          ctas={[
            { label: "Get started for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
            { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
          ]}
        />
      </main>
      <SiteFooter />
    </>
  );
}
