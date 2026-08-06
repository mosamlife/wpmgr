// SolutionPage: 6-block template for all 7 solution pages.
// Server Component that composes existing sections + motion leaves.
// Distinct from FeaturePage: problem-framed hero, outcome narrative,
// proving-feature cards, stats strip, FAQ, CTA band.
// Client JS is confined to FAQ (accordion) and motion leaves only.
import Link from "next/link";
import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Container, Section, SectionHeading, Card } from "@/components/ui/primitives";
import { FAQ } from "@/components/sections/faq";
import { CTABand } from "@/components/sections/cta-band";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import type { SolutionPageData } from "@/lib/content/types";

// ---------------------------------------------------------------------------
// Block 1: Breadcrumb
// ---------------------------------------------------------------------------
function Breadcrumb({ title }: { title: string }) {
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
          <li>
            <Link href="/solutions" className="transition-colors hover:text-foreground">
              Solutions
            </Link>
          </li>
          <li aria-hidden className="select-none">/</li>
          <li className="text-foreground font-medium" aria-current="page">
            {title}
          </li>
        </ol>
      </Container>
    </nav>
  );
}

// ---------------------------------------------------------------------------
// Block 2: Hero (H1 = solution keyword, not JS-animated for LCP)
// ---------------------------------------------------------------------------
function SolutionHero({ data }: { data: SolutionPageData }) {
  const { hero, heading } = data;
  const primaryTrailing = hero.primaryCta.icon === "ArrowRight";
  const secondaryTrailing = hero.secondaryCta?.icon === "ArrowRight";

  return (
    <section
      className="relative overflow-hidden py-20 sm:py-24 lg:py-28"
      aria-label="Hero"
    >
      <div aria-hidden className="dot-field pointer-events-none absolute inset-0" />
      <Container className="relative">
        <div className="mx-auto max-w-3xl">
          {hero.eyebrow && (
            <div className="mb-4 flex items-center gap-2">
              <span className="inline-flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
                <span aria-hidden className="h-px w-6 bg-[var(--primary)]/60" />
                {hero.eyebrow}
              </span>
            </div>
          )}
          {/* H1 is the keyword phrase: never JS-animated (LCP safety) */}
          <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl lg:text-[3rem] leading-[1.1]">
            {heading}
          </h1>
          <p className="mt-6 text-lg leading-relaxed text-[var(--muted-foreground)]">
            {hero.subhead}
          </p>
          <div className="mt-8 flex flex-wrap items-center gap-3">
            <Button
              href={hero.primaryCta.href}
              variant="primary"
              size="lg"
              {...(hero.primaryCta.href.startsWith("http")
                ? { target: "_blank", rel: "noreferrer noopener" }
                : {})}
            >
              {hero.primaryCta.icon && !primaryTrailing ? (
                <Icon name={hero.primaryCta.icon} size={18} />
              ) : null}
              {hero.primaryCta.label}
              {hero.primaryCta.icon && primaryTrailing ? (
                <Icon name={hero.primaryCta.icon} size={18} />
              ) : null}
            </Button>
            {hero.secondaryCta && (
              <Button
                href={hero.secondaryCta.href}
                variant="secondary"
                size="lg"
                {...(hero.secondaryCta.href.startsWith("http")
                  ? { target: "_blank", rel: "noreferrer noopener" }
                  : {})}
              >
                {hero.secondaryCta.icon && !secondaryTrailing ? (
                  <Icon name={hero.secondaryCta.icon} size={18} />
                ) : null}
                {hero.secondaryCta.label}
                {hero.secondaryCta.icon && secondaryTrailing ? (
                  <Icon name={hero.secondaryCta.icon} size={18} />
                ) : null}
              </Button>
            )}
          </div>
        </div>
      </Container>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Block 3: Outcome narrative
// ---------------------------------------------------------------------------
function OutcomeNarrative({ data }: { data: SolutionPageData }) {
  return (
    <Section tone="muted" id="outcomes">
      <Container>
        <Reveal>
          <div className="mx-auto max-w-2xl">
            <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
              {data.outcomes.heading}
            </h2>
            <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
              {data.outcomes.body}
            </p>
          </div>
        </Reveal>
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// Block 4: Proving features grid (links down to feature pages)
// ---------------------------------------------------------------------------
function ProvingFeatureCard({
  icon,
  title,
  summary,
  href,
}: {
  icon: string;
  title: string;
  summary: string;
  href: string;
}) {
  return (
    <Link
      href={href}
      className="group flex h-full flex-col gap-4 rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm transition-all duration-[var(--duration-base)] hover:shadow-md hover:scale-[1.02] hover:border-[var(--primary)]/30 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
    >
      <div className="flex items-start gap-4">
        <span className="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
          <Icon name={icon} size={20} />
        </span>
        <div className="flex-1 min-w-0">
          <h3 className="text-base font-semibold text-foreground transition-colors duration-[var(--duration-fast)] group-hover:text-[var(--primary)]">
            {title}
          </h3>
          <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">{summary}</p>
        </div>
      </div>
      <span className="mt-auto inline-flex items-center gap-1 text-xs font-medium text-[var(--primary)] transition-all duration-[var(--duration-fast)] group-hover:gap-2">
        See the feature
        <Icon name="ArrowRight" size={12} />
      </span>
    </Link>
  );
}

function ProvingFeatures({ data }: { data: SolutionPageData }) {
  const features = data.provingFeatures;
  // Vary grid columns by count so layouts feel distinct
  const gridCols =
    features.length <= 2
      ? "sm:grid-cols-2"
      : features.length === 3
        ? "sm:grid-cols-2 lg:grid-cols-3"
        : features.length <= 6
          ? "sm:grid-cols-2 lg:grid-cols-3"
          : "sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4";

  return (
    <Section id="features">
      <Container>
        <Reveal>
          <SectionHeading
            title="The features that prove it"
            lead="Everything listed below ships in the free, open-source release with no add-on required."
            align="left"
          />
        </Reveal>
        <Stagger className={`mt-10 grid gap-4 auto-rows-fr ${gridCols}`}>
          {features.map((f) => (
            <StaggerItem key={f.featureSlug} className="h-full">
              <ProvingFeatureCard
                icon={f.icon}
                title={f.title}
                summary={f.summary}
                href={f.href}
              />
            </StaggerItem>
          ))}
        </Stagger>
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// Block 5: Stats strip (audience proof)
// ---------------------------------------------------------------------------
function StatsStrip({ data }: { data: SolutionPageData }) {
  return (
    <Section tone="muted" id="proof">
      <Container>
        <Stagger className="grid gap-4 sm:grid-cols-3">
          {data.stats.map((stat) => (
            <StaggerItem key={stat.value}>
              <Card className="flex flex-col gap-3">
                <Icon name={stat.icon} size={20} className="text-[var(--primary)]" />
                <span
                  className="font-mono text-2xl font-semibold text-foreground"
                  style={{ fontVariantNumeric: "tabular-nums" }}
                >
                  {stat.value}
                </span>
                <span className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                  {stat.label}
                </span>
              </Card>
            </StaggerItem>
          ))}
        </Stagger>
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// SolutionPage: full 6-block server compositor
// ---------------------------------------------------------------------------
export function SolutionPage({ data }: { data: SolutionPageData }) {
  return (
    <>
      {/* 1. Breadcrumb */}
      <Breadcrumb title={data.title} />

      {/* 2. Hero (H1 = keyword, not JS-animated for LCP) */}
      <SolutionHero data={data} />

      {/* 3. Outcome narrative */}
      <OutcomeNarrative data={data} />

      {/* 4. Proving feature cards (link down to feature pages) */}
      <ProvingFeatures data={data} />

      {/* 5. Stats strip (audience proof) */}
      <StatsStrip data={data} />

      {/* 6a. FAQ */}
      <FAQ
        eyebrow="FAQ"
        heading="Common questions"
        subhead="Specific questions about this use case."
        items={data.faq}
      />

      {/* 6b. Closing CTA band */}
      <CTABand
        heading="Self-host it, read the code, run your whole fleet."
        subhead="Free and open source. No per-site fee. The full release is on GitHub."
        ctas={[
          {
            label: "Get started for free",
            href: signupHref("solution-cta"),
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
    </>
  );
}
