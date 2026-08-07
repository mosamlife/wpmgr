// PricingPage: real, priced hosted tiers plus the permanent free self-host
// option. Tier data lives in lib/content/pricing.ts so it is edited in one
// place. Prices are fetched live from the CP at build time (SSG) via
// lib/pricing-live.ts, with a static fallback if that fetch fails for any
// reason -- see resolveTierPrices. SoftwareApplication JSON-LD lists every
// tier as a separate Offer per currency it is actually priced in.
import type { Metadata } from "next";
import Link from "next/link";
import { Icon } from "@/components/ui/icon";
import { Container, Section, SectionHeading } from "@/components/ui/primitives";
import { Reveal } from "@/components/motion/reveal";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { FAQ } from "@/components/sections/faq";
import { CTABand } from "@/components/sections/cta-band";
import { buildMetadata, buildFAQPageLd, buildBreadcrumbLd, buildSoftwareApplicationLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import { PRICING_TIERS, PRICING_NOTE, PRICING_FAQ, PRICING_CTAS, resolveTierPrices } from "@/lib/content/pricing";
import { fetchLivePricing } from "@/lib/pricing-live";
import { PricingTierCards } from "./pricing-tiers";

export const metadata: Metadata = buildMetadata({
  title: "Pricing | WPMgr",
  description:
    "WPMgr pricing: a permanent free tier for 3 sites, and hosted plans from $15/mo with managed backup storage and more frequent backups. Self-hosting stays free and unlimited forever.",
  canonical: "/pricing",
});

export default async function PricingPage() {
  const livePricing = await fetchLivePricing();
  const prices = resolveTierPrices(livePricing);

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Pricing", href: "/pricing" },
  ]);

  const appLd = {
    ...buildSoftwareApplicationLd(),
    // One Offer per (tier, currency) actually priced -- USD always, INR only
    // when the tier has a live INR quote (see TierDisplayPrice.inr).
    offers: PRICING_TIERS.flatMap((tier) => {
      const tierPrice = prices[tier.id];
      const offers = [
        {
          "@type": "Offer",
          name: `${tier.name} plan`,
          price: String(tierPrice.usd.amountMajor),
          priceCurrency: "USD",
          description: tier.audience,
          url: `${SITE_CONFIG.baseUrl}/pricing`,
        },
      ];
      if (tierPrice.inr) {
        offers.push({
          "@type": "Offer",
          name: `${tier.name} plan`,
          price: String(tierPrice.inr.amountMajor),
          priceCurrency: "INR",
          description: tier.audience,
          url: `${SITE_CONFIG.baseUrl}/pricing`,
        });
      }
      return offers;
    }),
  };

  const faqLd = buildFAQPageLd(PRICING_FAQ);

  return (
    <>
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
              Pricing
            </li>
          </ol>
        </Container>
      </nav>

      {/* Hero */}
      <section className="relative overflow-hidden py-20 sm:py-24 lg:py-28" aria-label="Pricing hero">
        <div aria-hidden className="dot-field pointer-events-none absolute inset-0" />
        <Container className="relative">
          <div className="mx-auto max-w-3xl text-center">
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl leading-[1.1]">
              Simple pricing for every fleet size
            </h1>
            <p className="mt-6 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Start free with 3 sites, forever. Upgrade when you need more sites, more managed
              backup storage, or more frequent backups. Every plan gets the full feature set, no
              tier locks a capability behind a higher price. Choose Razorpay, Stripe, or Paddle at
              checkout; billed monthly.
            </p>
            <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
              <Link
                href={signupHref("pricing-free")}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-2 rounded-[var(--radius)] bg-primary px-6 py-3 text-base font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                Get started for free
                <Icon name="ArrowRight" size={18} />
              </Link>
              <Link
                href={SITE_CONFIG.github}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-6 py-3 text-base font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
              >
                <Icon name="Github" size={18} />
                Or self-host it
              </Link>
            </div>
          </div>
        </Container>
      </section>

      {/* Tier cards */}
      <Section id="tiers">
        <Container>
          <PricingTierCards prices={prices} />

          <Reveal delay={0.1}>
            <p className="mx-auto mt-8 max-w-2xl text-center text-sm leading-relaxed text-[var(--muted-foreground)]">
              {PRICING_NOTE}{" "}
              <Link
                href={SITE_CONFIG.github}
                target="_blank"
                rel="noreferrer noopener"
                className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
              >
                Read the source on GitHub
              </Link>
              .
            </p>
          </Reveal>
        </Container>
      </Section>

      {/* Cross-links */}
      <Section tone="muted" id="explore">
        <Container>
          <Reveal>
            <SectionHeading
              title="Dig into what you are getting"
              lead="Every plan includes the same feature set. Explore the feature detail or find the solution that matches how you work."
            />
          </Reveal>
          <Stagger className="mt-10 flex flex-wrap justify-center gap-4">
            {[
              { label: "All 13 features", href: "/features" },
              { label: "For agencies", href: "/solutions/agencies" },
              { label: "For freelancers", href: "/solutions/freelancers" },
              { label: "WordPress security", href: "/solutions/wordpress-security" },
              { label: "WordPress backups", href: "/solutions/wordpress-backups" },
              { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
            ].map((link) => (
              <StaggerItem key={link.href}>
                <Link
                  href={link.href}
                  className="inline-flex items-center gap-1.5 rounded-lg border border-[var(--border)] bg-card px-4 py-2 text-sm font-medium text-foreground transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  {link.label}
                  <Icon name="ArrowRight" size={13} className="text-[var(--muted-foreground)]" />
                </Link>
              </StaggerItem>
            ))}
          </Stagger>
        </Container>
      </Section>

      <FAQ
        eyebrow="FAQ"
        heading="Pricing questions answered"
        subhead="Common questions about plans, billing, and self-hosting."
        items={PRICING_FAQ}
      />

      <CTABand
        heading="Start free today. Upgrade only when you need to."
        subhead="No credit card required for the Free plan. Paid plans are billed monthly through your chosen payment provider and can be cancelled anytime."
        ctas={PRICING_CTAS}
      />

      <JsonLd data={breadcrumbLd} />
      <JsonLd data={appLd} />
      <JsonLd data={faqLd} />
    </>
  );
}
