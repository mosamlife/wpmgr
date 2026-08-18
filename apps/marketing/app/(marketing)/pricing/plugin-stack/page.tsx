// /pricing/plugin-stack: what the premium plugins WPMgr replaces cost per year.
//
// This page exists because "replaces your plugin stack" is a claim a reader
// cannot check, and a number they compute themselves is one they believe. The
// calculator is therefore the page; the prose around it only explains where
// the figures came from and where they stop.
//
// The credibility rules live in lib/content/plugin-costs.ts, next to the data
// they constrain, and the methodology section below states them to the reader
// in the same words. If you change one, change both.
import type { Metadata } from "next";
import Link from "next/link";
import { Icon } from "@/components/ui/icon";
import { Container, Section } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { FAQ } from "@/components/sections/faq";
import { CTABand } from "@/components/sections/cta-band";
import { buildMetadata, buildBreadcrumbLd, buildFAQPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { signupHref } from "@/lib/site";
import { PLUGIN_COST_CATEGORIES } from "@/lib/content/plugin-costs";
import { PRICING_TIERS, resolveTierPrices } from "@/lib/content/pricing";
import { fetchLivePricing } from "@/lib/pricing-live";
import { PluginStackCalculator } from "./calculator";

export const metadata: Metadata = buildMetadata({
  title: "What a premium WordPress plugin stack costs per year | WPMgr",
  description:
    "Add up what backups, security, caching, image optimization, database cleaning, uptime monitoring and client reporting actually cost as separate premium plugins, at your fleet size, from each vendor's own pricing page.",
  canonical: "/pricing/plugin-stack",
});

// Derived from the data file rather than written as a number, so the copy
// below cannot drift from the calculator sitting next to it the way a
// hand-typed "seven" would the moment a category is added or defaulted off.
const CATEGORY_COUNT = PLUGIN_COST_CATEGORIES.length;
const DEFAULT_ON_COUNT = PLUGIN_COST_CATEGORIES.filter((c) => !c.defaultOff).length;

const FAQ_ITEMS = [
  {
    q: "Why is my favourite plugin not on the list?",
    a: "Because it sells more than one of these categories. WP-Optimize and Perfmatters are both widely used and both deliberately absent: WP-Optimize covers caching, image compression, minification and database cleaning under one licence, so counting it in two rows would charge you twice for one purchase. Every product here sells the category it is listed under and little else, which is the only way the total means anything.",
  },
  {
    q: "Are these the prices I would pay today?",
    a: "They are the prices you would pay in year two. Several vendors in this market advertise a large first-year discount and renew at full price, and one states that in a footnote on its own pricing page. A calculator built on introductory prices would understate the stack by a third and be wrong for every year but the first, so the figures here are the recurring ones.",
  },
  {
    q: "Does WPMgr really do everything on the list?",
    a: `Every job the calculator counts, yes, and the free tier does not withhold any of it. "The list" is the ${CATEGORY_COUNT} categories the calculator bills for, not every feature of every product sitting in a row: a few of those vendor products sell more than the job they're listed under, such as an application firewall bundled with malware scanning, or staging and sandbox updates bundled with fleet management. Those extras were never on the list to begin with, so WPMgr not shipping them isn't a gap in it, which is exactly what the "Partial" chip on those rows means. What the paid tiers add is site count, managed backup storage and how often backups run. If you self-host, you get the same feature set on any number of sites for nothing, and the trade is that you run a Postgres database, a control plane and an encoder, and you keep them patched.`,
  },
  {
    q: "What is not included in this total?",
    a: `Storage and your own time, and on a few rows, part of the vendor's own product. Backups have to land somewhere, and most of these plugins bill remote storage separately or expect you to bring your own bucket, which is also true of WPMgr on the free and self-hosted tiers. Rows marked "Partial" sell something beyond what WPMgr ships too, such as staging or a managed firewall; the row's price is still the vendor's full price, not a discounted one, because no vendor publishes a "just the overlapping part" figure. The total also excludes the hours spent renewing ${CATEGORY_COUNT} licences on ${CATEGORY_COUNT} dates and updating ${CATEGORY_COUNT} plugins across every site, which is real but not something we can put a number on honestly.`,
  },
  {
    q: "Where did each number come from?",
    a: "Each product name links to the vendor's own pricing page, and the date it was checked is listed under the calculator. Several vendors in this market render prices with JavaScript, and a plain fetch of those pages returns different, stale figures, so each one was read in a real browser.",
  },
];

const METHOD = [
  {
    icon: "Layers",
    title: "No product counted twice",
    body: `Every product here sells one of these ${CATEGORY_COUNT} categories and little else. Suites that bundle caching with image compression and database cleaning are excluded on purpose, because including one would put the same licence on two lines.`,
  },
  {
    icon: "RefreshCw",
    title: "Renewal price, not first-year price",
    body: "Where a vendor advertises an introductory discount, the figure used is what you pay in year two. One vendor's own pricing page carries the footnote that all renewals are at full price.",
  },
  {
    icon: "FileSearch",
    title: "Read in a browser, on a date",
    body: "Several of these pricing pages build their prices with JavaScript, and fetching the HTML returns a stale fallback: in one case a discount banner that no visitor ever sees. Each figure was read from the rendered page, and the date is published below.",
  },
];

export default async function PluginStackPage() {
  // The same live quote /pricing renders, resolved at build time, so the two
  // pages can never show different prices for our own product.
  const prices = resolveTierPrices(await fetchLivePricing());
  const wpmgrTiers = PRICING_TIERS.map((tier) => ({
    name: tier.name,
    sites: tier.sites,
    perMonth: prices[tier.id].usd.amountMajor,
  }));

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Pricing", href: "/pricing" },
    { name: "Plugin stack cost", href: "/pricing/plugin-stack" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumbLd} />
      <JsonLd data={buildFAQPageLd(FAQ_ITEMS)} />

      <Section>
        <Container>
          <nav aria-label="Breadcrumb" className="mb-6 text-sm text-[var(--muted-foreground)]">
            <Link href="/" className="transition-colors hover:text-foreground">
              Home
            </Link>
            <span aria-hidden className="px-2">
              /
            </span>
            <Link href="/pricing" className="transition-colors hover:text-foreground">
              Pricing
            </Link>
          </nav>

          <h1 className="max-w-[20ch] text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            Add up what your plugins actually cost
          </h1>
          <p className="mt-5 max-w-[68ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            {DEFAULT_ON_COUNT} job{DEFAULT_ON_COUNT === 1 ? "" : "s"} on by default, {DEFAULT_ON_COUNT}{" "}
            licence{DEFAULT_ON_COUNT === 1 ? "" : "s"}, {DEFAULT_ON_COUNT} renewal date
            {DEFAULT_ON_COUNT === 1 ? "" : "s"}, plus a few more you can add if you actually buy them. Set
            your fleet size, add or remove anything, and every figure comes from the vendor&apos;s own
            pricing page.
          </p>

          <div className="mt-8">
            <PluginStackCalculator wpmgrTiers={wpmgrTiers} />
          </div>
        </Container>
      </Section>

      {/* Methodology. This is the section that decides whether the number above
          is believed, so it states the three rules plainly and immediately
          under the thing they constrain. */}
      <Section tone="muted" className="border-y border-[var(--border)]">
        <Container>
          <div className="grid gap-10 lg:grid-cols-[1fr_1.4fr] lg:gap-16">
            <div>
              <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
                How these figures were put together
              </h2>
              <p className="mt-4 leading-relaxed text-[var(--muted-foreground)]">
                A comparison is worth nothing if the reader suspects the total was chosen first.
                Three rules kept it honest, and each one made the number smaller.
              </p>
            </div>

            <dl className="flex flex-col gap-8">
              {METHOD.map((m, i) => (
                <Reveal key={m.title} delay={i * 0.05}>
                  <div className="flex gap-4">
                    <Icon
                      name={m.icon}
                      size={20}
                      className="mt-0.5 shrink-0 text-[var(--primary)]"
                      aria-hidden
                    />
                    <div>
                      <dt className="font-semibold text-foreground">{m.title}</dt>
                      <dd className="mt-2 max-w-[62ch] leading-relaxed text-[var(--muted-foreground)]">
                        {m.body}
                      </dd>
                    </div>
                  </div>
                </Reveal>
              ))}
            </dl>
          </div>
        </Container>
      </Section>

      {/* Sources. Every figure on the page, with the page it came from and the
          date it was read. */}
      <Section id="sources">
        <Container>
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            Sources
          </h2>
          <p className="mt-3 max-w-[68ch] text-[var(--muted-foreground)]">
            Prices change. Each link goes to the page the figure was read from, with the date it
            was read. If one of these is out of date,{" "}
            <Link
              href="/contact"
              className="text-[var(--primary-pressed)] underline underline-offset-4"
            >
              tell us and we will correct it
            </Link>
            .
          </p>

          <ul className="mt-8 flex flex-col gap-5">
            {PLUGIN_COST_CATEGORIES.flatMap((c) =>
              c.products.map((p) => (
                <li
                  key={`${c.key}-${p.name}`}
                  className="border-b border-[var(--border)]/60 pb-5 last:border-0"
                >
                  <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
                    <a
                      href={p.url}
                      target="_blank"
                      rel="noreferrer noopener nofollow"
                      className="font-medium text-foreground underline underline-offset-4 hover:text-[var(--primary-pressed)]"
                    >
                      {p.name}
                    </a>
                    <span className="text-xs text-[var(--muted-foreground)]">
                      {c.label}, checked {p.verifiedOn}
                    </span>
                  </div>
                  <p className="mt-2 max-w-[76ch] text-sm leading-relaxed text-[var(--muted-foreground)]">
                    {p.note}
                  </p>
                </li>
              )),
            )}
          </ul>
        </Container>
      </Section>

      <FAQ heading="Questions" items={FAQ_ITEMS} />

      <CTABand
        heading="One licence, one renewal date, one dashboard."
        subhead="Start free on three sites with the whole feature set, or self-host it on as many as you like for nothing."
        ctas={[
          {
            label: "Get started for free",
            href: signupHref("plugin-stack"),
            variant: "primary",
            icon: "ArrowRight",
          },
          { label: "Read the self-host guide", href: "/self-host", variant: "secondary" },
        ]}
      />
    </>
  );
}
