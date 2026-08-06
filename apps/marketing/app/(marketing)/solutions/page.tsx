import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd, buildItemListLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Icon } from "@/components/ui/icon";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { CTABand } from "@/components/sections/cta-band";
import { cn } from "@/lib/utils";
import { SOLUTION_HUB_CARDS } from "@/lib/content/solutions";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const metadata: Metadata = buildMetadata({
  title: "WordPress Management Solutions | WPMgr",
  description:
    "WPMgr covers every WordPress management use case: agencies, freelancers, hosting providers, WordPress security, backups, performance, and managing multiple sites. One open-source platform, no per-site fee.",
  canonical: "/solutions",
});

function Breadcrumb() {
  return (
    <nav aria-label="Breadcrumb" className="py-4">
      <Container>
        <ol className="flex flex-wrap items-center gap-1.5 text-sm text-[var(--muted-foreground)]">
          <li>
            <Link href="/" className="transition-colors hover:text-foreground">
              Home
            </Link>
          </li>
          <li aria-hidden className="select-none">/</li>
          <li className="text-foreground font-medium" aria-current="page">
            Solutions
          </li>
        </ol>
      </Container>
    </nav>
  );
}

function SolutionCard({
  slug,
  icon,
  title,
  summary,
}: {
  slug: string;
  icon: string;
  title: string;
  summary: string;
}) {
  return (
    <Link
      href={`/solutions/${slug}`}
      className={cn(
        "group relative flex flex-col gap-4 rounded-2xl border border-[var(--border)] bg-card p-7 shadow-sm",
        "transition-all duration-[var(--duration-base)] hover:shadow-lg hover:border-[var(--primary)]/40",
        "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
      )}
    >
      <span className="inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
        <Icon name={icon} size={22} />
      </span>
      <div>
        <h3 className="text-base font-semibold text-foreground transition-colors duration-[var(--duration-fast)] group-hover:text-[var(--primary)]">
          {title}
        </h3>
        <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">{summary}</p>
      </div>
      <span className="mt-auto inline-flex items-center gap-1 text-xs font-medium text-[var(--primary)] transition-all duration-[var(--duration-fast)] group-hover:gap-2">
        Explore solution
        <Icon name="ArrowRight" size={12} />
      </span>
    </Link>
  );
}

export default function SolutionsHubPage() {
  const audienceCards = SOLUTION_HUB_CARDS.filter((c) => c.group === "audience");
  const jtbdCards = SOLUTION_HUB_CARDS.filter((c) => c.group === "jtbd");

  const allSolutionsForLd = SOLUTION_HUB_CARDS.map((c) => ({
    name: c.title,
    description: c.summary,
    url: `/solutions/${c.slug}/`,
  }));

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Solutions", href: "/solutions" },
  ]);

  const itemListLd = buildItemListLd(allSolutionsForLd);

  return (
    <>
      <Breadcrumb />

      {/* Hero: H1 not JS-animated */}
      <section
        className="relative overflow-hidden py-16 sm:py-20"
        aria-label="Solutions hub hero"
      >
        <div aria-hidden className="dot-field pointer-events-none absolute inset-0" />
        <Container className="relative">
          <div className="mx-auto max-w-3xl">
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl leading-[1.1]">
              The right approach for every WordPress use case
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Whether you run sites for clients, operate your own portfolio, or need to solve a specific problem fast, WPMgr has a clear path. Choose by who you are or by what you need to get done.
            </p>
          </div>
        </Container>
      </section>

      {/* Audience solutions */}
      <Section id="audience">
        <Container>
          <Reveal>
            <SectionHeading
              title="Choose by who you are"
              lead="Capabilities tuned for agencies, independent WordPress professionals, and hosting teams."
              align="left"
            />
          </Reveal>
          <Stagger className="mt-10 grid gap-5 sm:grid-cols-3">
            {audienceCards.map((card) => (
              <StaggerItem key={card.slug}>
                <SolutionCard {...card} />
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* Divider */}
      <div aria-hidden className="border-t border-[var(--border)]" />

      {/* JTBD solutions */}
      <Section id="jobs-to-be-done">
        <Container>
          <Reveal>
            <SectionHeading
              title="Choose by what you need to get done"
              lead="High-intent workflows where WPMgr gives you a definitive, open-source answer."
              align="left"
            />
          </Reveal>
          <Stagger className="mt-10 grid gap-5 sm:grid-cols-2 lg:grid-cols-2">
            {jtbdCards.map((card) => (
              <StaggerItem key={card.slug}>
                <SolutionCard {...card} />
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* Feature link bridge */}
      <Section tone="muted" id="features-bridge">
        <Container>
          <Reveal>
            <div className="mx-auto max-w-2xl text-center">
              <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
                Every solution is built on real features
              </h2>
              <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                Each solution page links to the specific feature pages that prove it. Start with your use case, then dig into the technical detail when you are ready.
              </p>
              <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
                <Link
                  href="/features"
                  className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  Browse all 13 features
                  <Icon name="ArrowRight" size={14} />
                </Link>
                <Link
                  href="/pricing"
                  className="inline-flex items-center gap-2 rounded-[var(--radius)] bg-primary px-5 py-2.5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  See pricing
                  <Icon name="ArrowRight" size={14} />
                </Link>
              </div>
            </div>
          </Reveal>
        </Container>
      </Section>

      <CTABand
        heading="One platform. Every WordPress use case."
        subhead="Free and open source. Self-host the full control plane, no per-site fee, no lock-in."
        ctas={[
          {
            label: "Get started for free",
            href: signupHref("solutions-index"),
            variant: "primary",
            icon: "ArrowRight",
          },
          {
            label: "Star on GitHub",
            href: SITE_CONFIG.github,
            variant: "secondary",
            icon: "Github",
          },
        ]}
      />

      <JsonLd data={breadcrumbLd} />
      <JsonLd data={itemListLd} />
    </>
  );
}
