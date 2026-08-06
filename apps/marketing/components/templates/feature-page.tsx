// FeaturePage: 7-block template for all 13 feature pages.
// Server Component that composes existing sections + motion leaves.
// Client JS is confined to the visual leaf components only.
import type { ReactNode } from "react";
import Link from "next/link";
import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Container, Section, SectionHeading, Card } from "@/components/ui/primitives";
import { Steps } from "@/components/sections/steps";
import { FAQ } from "@/components/sections/faq";
import { CTABand } from "@/components/sections/cta-band";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import type { FeaturePageData } from "@/lib/content/types";

// Visual leaf: resolved at render time based on slug.
// Each visual is a 'use client' leaf so only the relevant one ships JS.
// Every feature slug maps to a distinct visual; no two pages share the same widget.
import { BackupVisual } from "@/components/sections/feature-visuals/backup-visual";
import { MediaVisual } from "@/components/sections/feature-visuals/media-visual";
import { CacheTrendVisual } from "@/components/sections/feature-visuals/cache-trend-visual";
import { RumVisual } from "@/components/sections/feature-visuals/rum-visual";
import { SecurityVisual } from "@/components/sections/feature-visuals/security-visual";
import { UpdatesVisual } from "@/components/sections/feature-visuals/updates-visual";
import { UptimeVisual } from "@/components/sections/feature-visuals/uptime-visual";
import { DatabaseCleanerVisual } from "@/components/sections/feature-visuals/database-cleaner-visual";
import { ClientReportsVisual } from "@/components/sections/feature-visuals/client-reports-visual";
import { EmailDeliverabilityVisual } from "@/components/sections/feature-visuals/email-deliverability-visual";
import { TeamAccessVisual } from "@/components/sections/feature-visuals/team-access-visual";
import { TwoFactorAuthVisual } from "@/components/sections/feature-visuals/two-factor-auth-visual";
import { ObjectCacheVisual } from "@/components/sections/feature-visuals/object-cache-visual";

const SLUG_TO_VISUAL: Record<string, ReactNode> = {
  backups: <BackupVisual />,
  updates: <UpdatesVisual />,
  "uptime-monitoring": <UptimeVisual />,
  performance: <CacheTrendVisual />,
  "object-cache": <ObjectCacheVisual />,
  "real-user-monitoring": <RumVisual />,
  "media-optimizer": <MediaVisual />,
  "database-cleaner": <DatabaseCleanerVisual />,
  security: <SecurityVisual />,
  "two-factor-auth": <TwoFactorAuthVisual />,
  "client-reports": <ClientReportsVisual />,
  "email-deliverability": <EmailDeliverabilityVisual />,
  "team-access": <TeamAccessVisual />,
};

function getVisual(slug: string): ReactNode | null {
  return SLUG_TO_VISUAL[slug] ?? null;
}

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
            <Link href="/features" className="transition-colors hover:text-foreground">
              Features
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
// Block 2: Hero (H1 = keyword, not JS-animated for LCP)
// ---------------------------------------------------------------------------
function FeatureHero({ data }: { data: FeaturePageData }) {
  const { hero } = data;
  const primaryTrailing = hero.primaryCta.icon === "ArrowRight";
  const secondaryTrailing = hero.secondaryCta?.icon === "ArrowRight";

  return (
    <section className="relative overflow-hidden py-20 sm:py-24 lg:py-28" aria-label="Hero">
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
            {hero.heading}
          </h1>
          <p className="mt-6 text-lg leading-relaxed text-[var(--muted-foreground)]">
            {hero.subhead}
          </p>
          <div className="mt-8 flex flex-wrap items-center gap-3">
            <Button
              href={hero.primaryCta.href}
              variant="primary"
              size="lg"
              {...(hero.primaryCta.href.startsWith("http") ? { target: "_blank", rel: "noreferrer noopener" } : {})}
            >
              {hero.primaryCta.icon && !primaryTrailing ? <Icon name={hero.primaryCta.icon} size={18} /> : null}
              {hero.primaryCta.label}
              {hero.primaryCta.icon && primaryTrailing ? <Icon name={hero.primaryCta.icon} size={18} /> : null}
            </Button>
            {hero.secondaryCta && (
              <Button
                href={hero.secondaryCta.href}
                variant="secondary"
                size="lg"
                {...(hero.secondaryCta.href.startsWith("http") ? { target: "_blank", rel: "noreferrer noopener" } : {})}
              >
                {hero.secondaryCta.icon && !secondaryTrailing ? <Icon name={hero.secondaryCta.icon} size={18} /> : null}
                {hero.secondaryCta.label}
                {hero.secondaryCta.icon && secondaryTrailing ? <Icon name={hero.secondaryCta.icon} size={18} /> : null}
              </Button>
            )}
          </div>
        </div>
      </Container>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Block 3: Problem to Solution
// ---------------------------------------------------------------------------
function ProblemSolution({ data }: { data: FeaturePageData }) {
  return (
    <Section tone="muted" id="problem">
      <Container>
        <div className="mx-auto max-w-2xl">
          <Reveal>
            <h2 className="text-2xl font-semibold text-foreground sm:text-3xl">
              {data.problem.heading}
            </h2>
            <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
              {data.problem.body}
            </p>
          </Reveal>
        </div>
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// Block 5: Visual showcase (varies per page)
// ---------------------------------------------------------------------------
function VisualBlock({ slug }: { slug: string }) {
  const visual = getVisual(slug);
  if (!visual) return null;

  return (
    <Section id="visual" tone="muted">
      <Container>
        <div className="mx-auto max-w-xl">
          <Reveal delay={0.06}>{visual}</Reveal>
        </div>
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// Block 6: Sub-features grid + cross-links
// ---------------------------------------------------------------------------
function SubFeaturesGrid({ data }: { data: FeaturePageData }) {
  const { subFeatures, siblingLinks, solutionLinks } = data;
  return (
    <Section id="capabilities">
      <Container>
        <Reveal>
          <SectionHeading
            title="What's included"
            lead="Every capability ships in the open-source release."
            align="left"
          />
        </Reveal>

        <Stagger className="mt-10 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {subFeatures.map((f) => (
            <StaggerItem key={f.title}>
              <Card className="flex h-full flex-col gap-3">
                <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--primary-subtle)] text-[var(--primary-pressed)]">
                  <Icon name={f.icon} size={18} />
                </span>
                <h3 className="text-sm font-semibold text-foreground">{f.title}</h3>
                <p className="text-sm leading-relaxed text-[var(--muted-foreground)]">{f.desc}</p>
              </Card>
            </StaggerItem>
          ))}
        </Stagger>

        {/* Cross-links */}
        {(siblingLinks.length > 0 || solutionLinks.length > 0) && (
          <Reveal delay={0.1}>
            <div className="mt-10 flex flex-wrap items-center gap-4 border-t border-[var(--border)] pt-6">
              {siblingLinks.length > 0 && (
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-medium text-[var(--muted-foreground)]">Related features:</span>
                  {siblingLinks.map((l) => (
                    <Link
                      key={l.href}
                      href={l.href}
                      className="inline-flex items-center gap-1 rounded-md border border-[var(--border)] bg-card px-3 py-1 text-sm font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]"
                    >
                      {l.label}
                      <Icon name="ArrowRight" size={13} className="text-[var(--muted-foreground)]" />
                    </Link>
                  ))}
                </div>
              )}
              {solutionLinks.length > 0 && (
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-medium text-[var(--muted-foreground)]">Solutions:</span>
                  {solutionLinks.map((l) => (
                    <Link
                      key={l.href}
                      href={l.href}
                      className="inline-flex items-center gap-1 rounded-md border border-[var(--border)] bg-[var(--primary-subtle)] px-3 py-1 text-sm font-medium text-[var(--primary-pressed)] transition-colors hover:bg-[var(--primary-subtle)]"
                    >
                      {l.label}
                      <Icon name="ArrowRight" size={13} />
                    </Link>
                  ))}
                </div>
              )}
            </div>
          </Reveal>
        )}
      </Container>
    </Section>
  );
}

// ---------------------------------------------------------------------------
// FeaturePage: full 7-block server compositor
// ---------------------------------------------------------------------------
export function FeaturePage({ data }: { data: FeaturePageData }) {
  const hasVisual = !!getVisual(data.slug);

  return (
    <>
      {/* 1. Breadcrumb */}
      <Breadcrumb title={data.title} />

      {/* 2. Hero (LCP element, no JS animation) */}
      <FeatureHero data={data} />

      {/* 3. Problem to Solution */}
      <ProblemSolution data={data} />

      {/* 4. How it works (Steps) */}
      <Steps
        id="how-it-works"
        eyebrow="How it works"
        heading="Under the hood"
        subhead="The steps that make it work, and what each one does."
        steps={data.steps}
        tone="base"
      />

      {/* 5. Visual block (slug-specific, client leaf) */}
      {hasVisual && <VisualBlock slug={data.slug} />}

      {/* 6. Sub-features grid with cross-links */}
      <SubFeaturesGrid data={data} />

      {/* 7a. FAQ */}
      <FAQ
        eyebrow="FAQ"
        heading="Questions answered"
        subhead="Common questions about this feature."
        items={data.faq}
      />

      {/* 7b. Closing CTA band */}
      <CTABand
        heading="Self-host it, read the code, and run your whole fleet."
        subhead="Free and open source. No per-site fee. The full release is on GitHub."
        ctas={[
          { label: "Get started for free", href: signupHref("feature-cta"), variant: "primary", icon: "ArrowRight" },
          { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
        ]}
      />
    </>
  );
}
