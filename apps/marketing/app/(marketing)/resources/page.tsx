import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section, Card } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "Resources: Blog, Guides, Changelog, and Docs",
  description:
    "WordPress operations resources from WPMgr: practical blog posts, cornerstone guides, release changelog, and API documentation.",
  canonical: "/resources",
});

const RESOURCES = [
  {
    icon: "BookOpen",
    title: "Blog",
    description:
      "Practical articles on WordPress security, performance, backups, and agency operations.",
    href: "/blog",
    external: false,
    cta: "Browse articles",
  },
  {
    icon: "FileText",
    title: "Guides",
    description:
      "Cornerstone long-form guides: WordPress maintenance from start to finish, and Core Web Vitals optimisation.",
    href: "/guides/wordpress-maintenance",
    external: false,
    cta: "Read the guides",
  },
  {
    icon: "History",
    title: "Changelog",
    description:
      "Every release, every fix, every new feature, newest first. See what shipped and when.",
    href: "/changelog",
    external: false,
    cta: "View changelog",
  },
  {
    icon: "Code2",
    title: "Docs and API Reference",
    description:
      "Full product documentation and the REST API reference, hosted on the dashboard at manage.wpmgr.app.",
    href: SITE_CONFIG.docs,
    external: true,
    cta: "Open docs",
  },
];

export default function ResourcesPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Resources", href: "/resources" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <nav
            aria-label="Breadcrumb"
            className="mb-5 flex items-center gap-2 text-sm text-[var(--muted-foreground)]"
          >
            <Link href="/" className="hover:text-foreground transition-colors">
              Home
            </Link>
            <span aria-hidden>/</span>
            <span className="text-foreground">Resources</span>
          </nav>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Resources
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              Learn how to run WordPress well
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Practical guides, detailed blog posts, release notes, and full product documentation.
              Everything you need to get the most from WPMgr and from WordPress at scale.
            </p>
          </div>
        </Container>
      </section>

      {/* Resource cards */}
      <Section>
        <Container>
          <ul className="grid gap-6 sm:grid-cols-2" role="list">
            {RESOURCES.map((resource) => (
              <li key={resource.title}>
                <Card className="flex h-full flex-col gap-5">
                  <div className="inline-flex h-11 w-11 items-center justify-center rounded-lg bg-[var(--primary)]/10 text-[var(--primary)]">
                    <Icon name={resource.icon as Parameters<typeof Icon>[0]["name"]} size={22} />
                  </div>
                  <div className="flex-1">
                    <h2 className="text-lg font-semibold text-foreground">{resource.title}</h2>
                    <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">
                      {resource.description}
                    </p>
                  </div>
                  <div className="mt-auto">
                    {resource.external ? (
                      <a
                        href={resource.href}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)] hover:opacity-80 transition-opacity focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        {resource.cta}
                        <Icon name="ExternalLink" size={14} />
                      </a>
                    ) : (
                      <Link
                        href={resource.href}
                        className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)] hover:opacity-80 transition-opacity focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        {resource.cta}
                        <Icon name="ArrowRight" size={14} />
                      </Link>
                    )}
                  </div>
                </Card>
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      {/* Blog cluster quick links */}
      <Section tone="muted">
        <Container>
          <h2 className="mb-8 text-2xl font-semibold text-foreground">Blog by topic</h2>
          <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4" role="list">
            {[
              {
                cat: "wordpress-security",
                label: "WordPress Security",
                icon: "ShieldCheck",
                desc: "Hardening, vulnerability scanning, file integrity.",
              },
              {
                cat: "wordpress-performance",
                label: "WordPress Performance",
                icon: "Gauge",
                desc: "Page speed, Core Web Vitals, image optimisation.",
              },
              {
                cat: "wordpress-backups",
                label: "WordPress Backups",
                icon: "DatabaseBackup",
                desc: "Backup strategy, restore procedures, recovery.",
              },
              {
                cat: "agency-operations",
                label: "Agency Operations",
                icon: "Handshake",
                desc: "Fleet management, client reporting, scale.",
              },
            ].map((item) => (
              <li key={item.cat}>
                <Link
                  href={`/blog/${item.cat}`}
                  className="flex h-full flex-col gap-3 rounded-xl border border-[var(--border)] bg-card p-5 transition-shadow hover:shadow-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  <div className="inline-flex h-9 w-9 items-center justify-center rounded-md bg-[var(--primary)]/10 text-[var(--primary)]">
                    <Icon name={item.icon as Parameters<typeof Icon>[0]["name"]} size={18} />
                  </div>
                  <div>
                    <p className="text-sm font-semibold text-foreground">{item.label}</p>
                    <p className="mt-1 text-xs leading-relaxed text-[var(--muted-foreground)]">
                      {item.desc}
                    </p>
                  </div>
                </Link>
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      {/* GitHub CTA */}
      <section className="py-12">
        <Container>
          <div className="flex flex-col items-start gap-4 rounded-xl border border-[var(--border)] bg-card p-8 sm:flex-row sm:items-center sm:justify-between">
            <div>
              <h2 className="text-xl font-semibold text-foreground">Contribute to WPMgr</h2>
              <p className="mt-1 text-[var(--muted-foreground)]">
                Open-source under AGPL-3.0. Read the code, file issues, submit pull requests.
              </p>
            </div>
            <a
              href={SITE_CONFIG.github}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] whitespace-nowrap"
            >
              <Icon name="Github" size={16} />
              View on GitHub
            </a>
          </div>
        </Container>
      </section>
    </>
  );
}
