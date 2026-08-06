import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildItemListLd, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Icon } from "@/components/ui/icon";
import { Container, Section } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { cn } from "@/lib/utils";
import { HUB_CLUSTERS } from "@/lib/content/features";
import { CTABand } from "@/components/sections/cta-band";
import { SITE_CONFIG } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "WordPress Management Features | WPMgr",
  description:
    "All WordPress fleet management features in one open-source platform: backups and restore, safe updates, Media Optimizer (AVIF and WebP), full-page caching, Redis object cache, Real User Monitoring, database cleanup, security hardening, vulnerability scanning, 2FA, client reports, per-site email, and team access control.",
  canonical: "/features",
});

function Breadcrumb() {
  return (
    <nav aria-label="Breadcrumb" className="py-4">
      <Container>
        <ol className="flex flex-wrap items-center gap-1.5 text-sm text-[var(--muted-foreground)]">
          <li><Link href="/" className="transition-colors hover:text-foreground">Home</Link></li>
          <li aria-hidden className="select-none">/</li>
          <li className="text-foreground font-medium" aria-current="page">Features</li>
        </ol>
      </Container>
    </nav>
  );
}

function FeatureCard({ slug, icon, title, summary }: {
  slug: string;
  icon: string;
  title: string;
  summary: string;
}) {
  return (
    <Link
      href={`/features/${slug}/`}
      className={cn(
        "group flex h-full flex-col gap-3 rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm",
        "transition-all duration-[var(--duration-base)] hover:shadow-md hover:scale-[1.02] hover:border-[var(--primary)]/30",
        "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
      )}
    >
      <div className="flex items-center gap-3">
        <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
          <Icon name={icon} size={18} />
        </span>
        <h3 className="text-sm font-semibold text-foreground transition-colors duration-[var(--duration-fast)] group-hover:text-[var(--primary)]">
          {title}
        </h3>
      </div>
      <p className="flex-1 text-sm leading-relaxed text-[var(--muted-foreground)]">{summary}</p>
      <span className="mt-auto inline-flex items-center gap-1 text-xs font-medium text-[var(--primary)] transition-all duration-[var(--duration-fast)] group-hover:gap-2">
        Learn more
        <Icon name="ArrowRight" size={12} />
      </span>
    </Link>
  );
}

function ClusterSection({ cluster }: { cluster: (typeof HUB_CLUSTERS)[0] }) {
  const colClass =
    cluster.features.length === 1
      ? "max-w-sm"
      : cluster.features.length === 2
        ? "sm:grid-cols-2"
        : cluster.features.length === 3
          ? "sm:grid-cols-2 lg:grid-cols-3"
          : "sm:grid-cols-2 lg:grid-cols-4";

  return (
    <div id={cluster.id} className="scroll-mt-20">
      <Reveal>
        <div className="flex items-start gap-3 mb-7">
          <span className="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-[var(--primary)] text-[var(--primary-foreground)]">
            <Icon name={cluster.icon} size={20} />
          </span>
          <div>
            <h2 className="text-xl font-semibold text-foreground">{cluster.name}</h2>
            <p className="text-sm text-[var(--muted-foreground)]">{cluster.tagline}</p>
          </div>
        </div>
      </Reveal>

      <Stagger className={cn("grid gap-4 auto-rows-fr", colClass)}>
        {cluster.features.map((f) => (
          <StaggerItem key={f.slug} className="h-full">
            <FeatureCard {...f} />
          </StaggerItem>
        ))}
      </Stagger>
    </div>
  );
}

export default function FeaturesHubPage() {
  const allFeatures = HUB_CLUSTERS.flatMap((c) =>
    c.features.map((f) => ({
      name: f.title,
      description: f.summary,
      url: `/features/${f.slug}/`,
    })),
  );

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Features", href: "/features/" },
  ]);

  const itemListLd = buildItemListLd(allFeatures);

  return (
    <>
      <Breadcrumb />

      {/* Hero: H1 not JS-animated */}
      <section className="relative overflow-hidden py-16 sm:py-20" aria-label="Features hub hero">
        <div aria-hidden className="dot-field pointer-events-none absolute inset-0" />
        <Container className="relative">
          <div className="mx-auto max-w-3xl">
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl leading-[1.1]">
              WordPress management features, all in the open release
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Five capability clusters, 13 feature areas, every line of code open to read, fork, and extend. One dashboard, no add-on sprawl.
            </p>
          </div>
        </Container>
      </section>

      {/* Bento-grid hub */}
      <Section id="features">
        <Container>
          <div className="flex flex-col gap-20">
            {HUB_CLUSTERS.map((cluster) => (
              <ClusterSection key={cluster.id} cluster={cluster} />
            ))}
          </div>
        </Container>
      </Section>

      <CTABand
        heading="Every feature ships in the free, open-source release."
        subhead="Self-host the full control plane, read every line, and run it on your own infrastructure."
        ctas={[
          { label: "Get started for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
          { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
        ]}
      />

      <JsonLd data={breadcrumbLd} />
      <JsonLd data={itemListLd} />
    </>
  );
}
