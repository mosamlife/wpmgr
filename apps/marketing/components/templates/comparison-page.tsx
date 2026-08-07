import Link from "next/link";
import { Container, Section } from "@/components/ui/primitives";
import { CTABand } from "@/components/sections/cta-band";
import { CompareMatrix } from "@/components/sections/compare/matrix";
import { StackCollapse } from "@/components/sections/compare/stack-collapse";
import { CostModelSection } from "@/components/sections/compare/cost-model";
import { DataLocality } from "@/components/sections/compare/data-locality";
import { signupHref } from "@/lib/site";
import { Icon } from "@/components/ui/icon";
import type { ComparisonPageData } from "@/lib/content/types";

/**
 * Template for /compare/[slug].
 *
 * SIX SECTIONS, and the count is the point. The first version of this page ran
 * to 9,400 words of sourced claims below the table and was rejected for being
 * a wall of text. Every section here either SHOWS something or lets the reader
 * COMPUTE something; the prose between them is under 400 words in total.
 *
 * The claims did not disappear. They moved to /compare/[slug]/sources and are
 * reachable from numbered footnotes on the individual figures they back, which
 * is where a reader wants them: at the moment they doubt a specific number,
 * not stacked in front of the argument.
 *
 * This template does NOT render SiteHeader or SiteFooter. It lives inside
 * app/(marketing)/, whose layout already provides both, and rendering them
 * again is exactly the defect that shipped two of each on the first version.
 * The same layout also mounts ScrollProgress.
 */
export function ComparisonPage({ data }: { data: ComparisonPageData }) {
  return (
    <>
      {/* 1. Hero */}
      <Section>
        <Container>
          <nav aria-label="Breadcrumb" className="mb-6 text-sm text-[var(--muted-foreground)]">
            <Link href="/" className="hover:text-foreground transition-colors">
              Home
            </Link>
            <span aria-hidden className="px-2">
              /
            </span>
            <Link href="/compare" className="hover:text-foreground transition-colors">
              Compare
            </Link>
          </nav>

          <h1 className="max-w-[24ch] text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            {data.hero.heading}
          </h1>
          <p className="mt-5 max-w-[62ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            {data.hero.subhead}
          </p>

          <div className="mt-7 flex flex-wrap gap-3">
            <a
              href={signupHref("compare")}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex h-12 items-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-7 text-base font-medium text-[var(--primary-foreground)] shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              Get started for free
              <Icon name="ArrowRight" size={18} aria-hidden />
            </a>
            <a
              href="#replaces"
              className="inline-flex h-12 items-center rounded-[var(--radius)] border border-[var(--border)] bg-card px-7 text-base font-medium text-foreground transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              See what it replaces
            </a>
          </div>

          <ul className="mt-8 flex flex-wrap gap-x-6 gap-y-2">
            {data.hero.chips.map((chip) => (
              <li
                key={chip}
                className="flex items-center gap-2 text-sm text-[var(--muted-foreground)]"
              >
                <Icon name="Check" size={14} className="text-[var(--primary)]" aria-hidden />
                {chip}
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      {/* 2. The matrix. The one section the first version got right. */}
      <CompareMatrix data={data} />

      {/* 3. Hero visual: what one tool replaces. */}
      <StackCollapse data={data} />

      {/* 4. The reader computes their own number, and the CTA sits here. */}
      <CostModelSection data={data} />

      {/* 5. The difference a competitor cannot ship its way out of. */}
      <DataLocality data={data} />

      {/* 6. Questions, framed around switching. */}
      {data.faq.length > 0 && (
        <Section tone="muted" className="border-y border-[var(--border)]">
          <Container className="max-w-3xl">
            <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
              Questions
            </h2>
            <dl className="mt-8 flex flex-col gap-6">
              {data.faq.map((f) => (
                <div key={f.q}>
                  <dt className="font-semibold text-foreground">{f.q}</dt>
                  <dd className="mt-2 leading-relaxed text-[var(--muted-foreground)]">{f.a}</dd>
                </div>
              ))}
            </dl>
            <p className="mt-10 text-sm text-[var(--muted-foreground)]">
              Every figure on this page links to the page it came from.{" "}
              <Link
                href={`/compare/${data.slug}/sources`}
                className="text-[var(--primary-pressed)] underline underline-offset-4"
              >
                See all sources and the dates they were checked
              </Link>
              .
            </p>
          </Container>
        </Section>
      )}

      <CTABand
        heading="Run your fleet from a dashboard you own."
        subhead="Open source, self-hostable, unlimited sites. Connect one site and compare on your own fleet before you move anything."
        ctas={[
          {
            label: "Get started for free",
            href: signupHref("compare"),
            variant: "primary",
            icon: "ArrowRight",
          },
        ]}
      />
    </>
  );
}
