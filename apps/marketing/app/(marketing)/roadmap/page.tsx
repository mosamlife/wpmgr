// This route is intentionally unlinked from nav, footer, and every other
// page. It goes live only after #460 ships: our uptime page currently derives
// most of its 90-day history from a single 7-day figure, and a never-probed
// site renders as 90 days of solid outage. A roadmap invites people to check
// our work, and that is the wrong pair of facts to have live while inviting
// it. Do not add a link to this page anywhere until #460 is deployed and
// verified against the running system.
import type { Metadata } from "next";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section, SectionHeading, Eyebrow } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { ROADMAP } from "@/lib/content/roadmap";

export const metadata: Metadata = buildMetadata({
  title: ROADMAP.metaTitle,
  description: ROADMAP.metaDescription,
  canonical: "/roadmap",
  // Not announced yet. Kept out of the sitemap and out of search results
  // until the gate above clears, so the first place anyone finds this page
  // is a deliberate link, not a crawl.
  noindex: true,
});

export default function RoadmapPage() {
  return (
    <>
      <Section className="pb-14 sm:pb-16">
        <Container>
          <Eyebrow>Roadmap</Eyebrow>
          <h1 className="mt-4 max-w-[20ch] text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            {ROADMAP.hero.subhead}
          </h1>
          <p className="mt-5 max-w-[62ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            {ROADMAP.hero.note}
          </p>
        </Container>
      </Section>

      {/* Shipping now */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container>
          <Reveal>
            <SectionHeading
              align="left"
              eyebrow={ROADMAP.shippingNow.eyebrow}
              title={ROADMAP.shippingNow.heading}
            />
          </Reveal>

          <Stagger className="mt-10 flex flex-col gap-6">
            {ROADMAP.shippingNow.items.map((item) => (
              <StaggerItem key={item.title}>
                <div className="rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8">
                  <div className="flex flex-wrap items-center gap-3">
                    <h3 className="text-xl font-semibold text-foreground">{item.title}</h3>
                    <span className="inline-flex items-center gap-1.5 rounded-full bg-[var(--warning-subtle)] px-2.5 py-1 text-xs font-semibold uppercase tracking-[0.06em] text-[var(--warning-subtle-fg)]">
                      <Icon name="RefreshCw" size={12} />
                      {item.badge}
                    </span>
                  </div>
                  <p className="mt-4 text-lg leading-relaxed text-foreground">{item.summary}</p>
                  <p className="mt-3 leading-relaxed text-[var(--muted-foreground)]">
                    {item.context}
                  </p>
                  <ul className="mt-6 flex flex-col gap-3 border-t border-[var(--border)] pt-6">
                    {item.points.map((point) => (
                      <li key={point} className="flex gap-3 leading-relaxed text-foreground">
                        <Icon
                          name="Check"
                          size={16}
                          className="mt-1 shrink-0 text-[var(--primary)]"
                        />
                        {point}
                      </li>
                    ))}
                  </ul>
                  <p className="mt-6 text-sm text-[var(--muted-foreground)]">
                    <a
                      href={item.tracking.href}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="font-medium text-[var(--primary-pressed)] underline-offset-4 hover:underline"
                    >
                      {item.tracking.label}
                    </a>
                    {". "}
                    {item.tracking.note}
                  </p>
                </div>
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* Committed next */}
      <Section>
        <Container>
          <Reveal>
            <SectionHeading
              align="left"
              eyebrow={ROADMAP.committedNext.eyebrow}
              title={ROADMAP.committedNext.heading}
            />
          </Reveal>

          <Stagger className="mt-10 grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
            {ROADMAP.committedNext.items.map((item) => (
              <StaggerItem key={item.title}>
                <div className="flex h-full flex-col gap-4 rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm">
                  <div className="flex items-center gap-2">
                    <span className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-[var(--success-subtle)] text-[var(--success-subtle-fg)]">
                      <Icon name="Check" size={16} />
                    </span>
                    <h3 className="text-lg font-semibold text-foreground">{item.title}</h3>
                  </div>

                  {"summary" in item && item.summary ? (
                    <p className="leading-relaxed text-foreground">{item.summary}</p>
                  ) : null}
                  {"detail" in item && item.detail ? (
                    <p className="leading-relaxed text-[var(--muted-foreground)]">{item.detail}</p>
                  ) : null}
                  {"points" in item && item.points ? (
                    <ul className="flex flex-col gap-2.5">
                      {item.points.map((point) => (
                        <li
                          key={point}
                          className="flex gap-2.5 text-sm leading-relaxed text-foreground"
                        >
                          <Icon
                            name="Check"
                            size={14}
                            className="mt-0.5 shrink-0 text-[var(--primary)]"
                          />
                          {point}
                        </li>
                      ))}
                    </ul>
                  ) : null}

                  {"tracking" in item && item.tracking ? (
                    <a
                      href={item.tracking.href}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="mt-auto inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary-pressed)] underline-offset-4 hover:underline"
                    >
                      {item.tracking.label}
                      <Icon name="ArrowRight" size={14} />
                    </a>
                  ) : null}
                </div>
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* Under research, not yet committed */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container className="max-w-3xl">
          <Reveal>
            <div className="rounded-xl border border-dashed border-[var(--border)] bg-card p-6 sm:p-8">
              <div className="flex flex-wrap items-center gap-3">
                <span className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-[var(--muted)] text-foreground">
                  <Icon name="HelpCircle" size={18} />
                </span>
                <span className="text-xs font-semibold uppercase tracking-[0.14em] text-[var(--muted-foreground)]">
                  {ROADMAP.underResearch.eyebrow}
                </span>
              </div>

              <h2 className="mt-5 text-2xl font-semibold text-foreground sm:text-3xl">
                {ROADMAP.underResearch.heading}
              </h2>

              <p className="mt-3 inline-flex rounded-md border border-dashed border-[var(--border)] bg-[var(--muted)] px-3 py-1.5 text-sm font-medium text-[var(--muted-foreground)]">
                {ROADMAP.underResearch.disclaimer}
              </p>

              <div className="mt-5 flex flex-col gap-4">
                {ROADMAP.underResearch.body.map((p) => (
                  <p key={p} className="leading-relaxed text-foreground">
                    {p}
                  </p>
                ))}
              </div>

              <p className="mt-6 text-sm font-semibold text-[var(--muted-foreground)]">
                {ROADMAP.underResearch.prerequisitesLabel}
              </p>
              <ul className="mt-3 flex flex-col gap-3">
                {ROADMAP.underResearch.prerequisites.map((point) => (
                  <li key={point} className="flex gap-3 leading-relaxed text-foreground">
                    <Icon
                      name="HelpCircle"
                      size={16}
                      className="mt-1 shrink-0 text-[var(--muted-foreground)]"
                    />
                    {point}
                  </li>
                ))}
              </ul>
            </div>
          </Reveal>
        </Container>
      </Section>

      {/* Explicit non-goals: the section this page exists to publish, so it
          gets the biggest heading on the page and cards with real weight
          rather than a footnote list. */}
      <Section>
        <Container>
          <Reveal>
            <div className="max-w-2xl">
              <Eyebrow>{ROADMAP.nonGoals.eyebrow}</Eyebrow>
              <h2 className="mt-4 text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
                {ROADMAP.nonGoals.heading}
              </h2>
              <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
                {ROADMAP.nonGoals.lead}
              </p>
            </div>
          </Reveal>

          <Stagger className="mt-12 grid gap-6 sm:grid-cols-2">
            {ROADMAP.nonGoals.items.map((item) => (
              <StaggerItem key={item.title}>
                <div className="flex h-full gap-5 rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8">
                  <span className="inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-md border border-[var(--border)] bg-[var(--muted)] text-foreground">
                    <Icon name="X" size={20} />
                  </span>
                  <div>
                    <h3 className="text-xl font-semibold text-foreground">{item.title}</h3>
                    <p className="mt-2 leading-relaxed text-[var(--muted-foreground)]">
                      {item.body}
                    </p>
                  </div>
                </div>
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      {/* How to read this: a closing note, deliberately low-key. */}
      <Section tone="muted" className="border-t border-[var(--border)] py-14 sm:py-16">
        <Container className="max-w-3xl">
          <h2 className="text-lg font-semibold text-foreground">{ROADMAP.howToRead.heading}</h2>
          <ul className="mt-4 flex flex-col gap-2.5">
            {ROADMAP.howToRead.points.map((point) => (
              <li
                key={point}
                className="text-sm leading-relaxed text-[var(--muted-foreground)]"
              >
                {point}
              </li>
            ))}
          </ul>
        </Container>
      </Section>

      <JsonLd
        data={buildBreadcrumbLd([
          { name: "Home", href: "/" },
          { name: "Roadmap", href: "/roadmap" },
        ])}
      />
    </>
  );
}
