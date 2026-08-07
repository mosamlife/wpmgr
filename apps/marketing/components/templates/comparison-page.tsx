import Link from "next/link";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Icon } from "@/components/ui/icon";
import { CTABand } from "@/components/sections/cta-band";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import type { ComparisonPageData } from "@/lib/content/types";

/**
 * Template for /compare/[slug].
 *
 * These are the only pages permitted to name competitor products, and the
 * layout is built around the thing that makes that defensible: every factual
 * claim about another product renders WITH its source link and the date it was
 * checked, visible to the reader. A comparison written by one of the products
 * is worth nothing unless the reader can audit it, so the audit trail is part
 * of the page rather than a footnote.
 *
 * Two structural commitments that are easy to erode later and should not be:
 *
 *   The disclosure sits ABOVE the comparison, not below it. A reader who works
 *   out halfway down that the author is one of the options has been managed,
 *   and stops believing the rest. Saying it first costs nothing and buys the
 *   whole page.
 *
 *   Every competitor renders a "does better" block, and the type makes
 *   `strengths` non-optional. A comparison in which the author wins on every
 *   axis is not read as a comparison, it is read as an advert.
 */
export function ComparisonPage({ data }: { data: ComparisonPageData }) {
  const columns = ["WPMgr", ...data.products.map((p) => p.name)];

  return (
    <>
      {/* Hero */}
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
          <h1 className="max-w-[26ch] text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            {data.hero.heading}
          </h1>
          <p className="mt-5 max-w-[70ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            {data.hero.subhead}
          </p>

          {/* Disclosure, deliberately before any comparison. */}
          <p className="mt-8 max-w-[70ch] rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-5 text-sm leading-relaxed text-[var(--muted-foreground)]">
            {data.disclosure}
          </p>
        </Container>
      </Section>

      {/* At a glance */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container>
          <Reveal>
            <SectionHeading
              title="At a glance"
              lead="Free text rather than ticks, because a tick cannot say paid add-on."
              align="left"
            />
          </Reveal>
          <div className="mt-8 overflow-x-auto">
            <table className="w-full min-w-[640px] border-collapse text-sm">
              <thead>
                <tr className="border-b border-[var(--border)]">
                  <th scope="col" className="py-3 pr-4 text-left font-semibold text-foreground">
                    <span className="sr-only">Capability</span>
                  </th>
                  {columns.map((c) => (
                    <th
                      key={c}
                      scope="col"
                      className="py-3 pr-4 text-left font-semibold text-foreground"
                    >
                      {c}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {data.table.map((row) => (
                  <tr key={row.label} className="border-b border-[var(--border)]/60">
                    <th
                      scope="row"
                      className="py-3 pr-4 text-left font-medium text-foreground align-top"
                    >
                      {row.label}
                    </th>
                    {columns.map((c) => (
                      <td
                        key={c}
                        className="py-3 pr-4 align-top text-[var(--muted-foreground)]"
                      >
                        {row.values[c] ?? "Not stated"}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Container>
      </Section>

      {/* Per product: sourced claims and what it does better */}
      <Section>
        <Container>
          <Reveal>
            <SectionHeading
              title="The detail, with sources"
              lead="Every figure below links to the page it came from and the date it was checked. Prices change; if one of these is stale, the link is how you find out."
              align="left"
            />
          </Reveal>

          <div className="mt-10 flex flex-col gap-10">
            {data.products.map((p) => (
              <div
                key={p.name}
                className="rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8"
              >
                <div className="flex flex-wrap items-baseline justify-between gap-3">
                  <h3 className="text-xl font-semibold text-foreground">{p.name}</h3>
                  <a
                    href={p.url}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary-pressed)] underline-offset-4 hover:underline"
                  >
                    Visit {p.name}
                    <Icon name="ArrowRight" size={14} aria-hidden />
                  </a>
                </div>
                <p className="mt-3 leading-relaxed text-[var(--muted-foreground)]">{p.summary}</p>

                <ul className="mt-6 flex flex-col gap-4 border-t border-[var(--border)] pt-6">
                  {p.claims.map((c) => (
                    <li key={`${c.topic}-${c.claim}`} className="text-sm leading-relaxed">
                      <span className="text-foreground">{c.claim}</span>{" "}
                      <a
                        href={c.sourceUrl}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="whitespace-nowrap text-[var(--muted-foreground)] underline underline-offset-4 hover:text-foreground"
                      >
                        Source, checked {c.verifiedOn}
                      </a>
                    </li>
                  ))}
                </ul>

                <div className="mt-6 rounded-lg border border-[var(--border)] bg-[var(--muted)]/40 p-5">
                  <p className="text-sm font-semibold text-foreground">
                    What {p.name} does better than WPMgr
                  </p>
                  <ul className="mt-3 flex flex-col gap-2">
                    {p.strengths.map((s) => (
                      <li key={s} className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                        {s}
                      </li>
                    ))}
                  </ul>
                </div>
              </div>
            ))}
          </div>
        </Container>
      </Section>

      {/* Which one suits you, including the cases where it is not us */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container>
          <Reveal>
            <SectionHeading title="Which one suits you" align="left" />
          </Reveal>
          <div className="mt-8 grid gap-6 md:grid-cols-2">
            {data.verdicts.map((v) => (
              <div
                key={v.heading}
                className="rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm"
              >
                <h3 className="text-base font-semibold text-foreground">{v.heading}</h3>
                <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">
                  {v.body}
                </p>
              </div>
            ))}
          </div>
        </Container>
      </Section>

      {/* FAQ */}
      {data.faq.length > 0 && (
        <Section>
          <Container className="max-w-3xl">
            <Reveal>
              <SectionHeading title="Questions" align="left" />
            </Reveal>
            <dl className="mt-8 flex flex-col gap-6">
              {data.faq.map((f) => (
                <div key={f.q}>
                  <dt className="font-semibold text-foreground">{f.q}</dt>
                  <dd className="mt-2 leading-relaxed text-[var(--muted-foreground)]">{f.a}</dd>
                </div>
              ))}
            </dl>
          </Container>
        </Section>
      )}

      <CTABand
        heading="Run your fleet from a dashboard you own."
        subhead="Open source, self-hostable, and free for as many sites as you like. A hosted tier exists if you would rather not run it."
        ctas={[
          {
            label: "Get started for free",
            href: signupHref("compare"),
            variant: "primary",
            icon: "ArrowRight",
          },
          // Points at the repository, not /self-host: that page is Tier 2.9 and does
          // not exist yet, and shipping a CTA to a 404 is worse than no CTA.
          { label: "Read the source", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
        ]}
      />
    </>
  );
}
