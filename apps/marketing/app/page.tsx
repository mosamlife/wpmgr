import type { Metadata } from "next";
import { Hero } from "@/components/sections/hero";
import { OpsStatus } from "@/components/sections/ops-status";
import { FeatureGrid } from "@/components/sections/feature-grid";
import { MediaShowcase } from "@/components/sections/media-showcase";
import { Steps } from "@/components/sections/steps";
import { ProofStrip } from "@/components/sections/proof-strip";
import { Provenance } from "@/components/sections/provenance";
import { FAQ } from "@/components/sections/faq";
import { CTABand } from "@/components/sections/cta-band";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";
import { ScrollProgress } from "@/components/motion/scroll-progress";
import {
  buildMetadata,
  buildSoftwareApplicationLd,
  buildFAQPageLd,
} from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import {
  HOME_HERO,
  HOME_OPS_STATUS,
  HOME_FEATURES,
  HOME_MEDIA,
  HOME_MEDIA_STEPS,
  HOME_STATS,
  HOME_PROVENANCE,
  HOME_ENROLL,
  HOME_FAQ,
  HOME_FINAL_CTA,
} from "@/lib/content/home";

export const metadata: Metadata = buildMetadata({
  title: "WPMgr: Open-Source, Self-Hosted WordPress Fleet Management",
  description:
    "Open-source, self-hostable WordPress fleet manager: Media Optimizer, full-page caching, backups, uptime monitoring, and security scanning, MIT-licensed agent.",
  canonical: "/",
});

// Home page is statically generated at build time. No dynamic data fetching.
export default function HomePage() {
  return (
    <>
      <ScrollProgress />
      <SiteHeader />
      <main>
        {/* 1. Hero: category claim, not JS-animated (LCP safety) */}
        <Hero
          badge={HOME_HERO.badge}
          heading={HOME_HERO.heading}
          subhead={HOME_HERO.subhead}
          ctas={[...HOME_HERO.ctas]}
          trust={[...HOME_HERO.trust]}
        />

        {/* 2. Live ops status: "Real-Time Operations Landing" archetype */}
        <OpsStatus
          heading={HOME_OPS_STATUS.heading}
          subhead={HOME_OPS_STATUS.subhead}
          sites={HOME_OPS_STATUS.sites}
        />

        {/* 3. Media Optimizer flagship moment (before generic feature grid) */}
        <MediaShowcase
          eyebrow={HOME_MEDIA.eyebrow}
          heading={HOME_MEDIA.heading}
          subhead={HOME_MEDIA.subhead}
          chips={[...HOME_MEDIA.chips]}
          cta={HOME_MEDIA.cta}
          demo={HOME_MEDIA.demo}
        />

        {/* 4. How Media Optimization works */}
        <Steps
          id="how-it-works"
          eyebrow={HOME_MEDIA_STEPS.eyebrow}
          heading={HOME_MEDIA_STEPS.heading}
          subhead={HOME_MEDIA_STEPS.subhead}
          steps={[...HOME_MEDIA_STEPS.steps]}
          tone="muted"
        />

        {/* 5. Proof strip: "Trust & Authority" archetype, BEFORE the feature list */}
        <ProofStrip
          eyebrow={HOME_STATS.eyebrow}
          heading={HOME_STATS.heading}
          subhead={HOME_STATS.subhead}
          items={HOME_STATS.items}
        />

        {/* 6. Full feature cluster grid */}
        <FeatureGrid
          eyebrow={HOME_FEATURES.eyebrow}
          heading={HOME_FEATURES.heading}
          subhead={HOME_FEATURES.subhead}
          clusters={HOME_FEATURES.clusters}
        />

        {/* 7. Provenance: the WordPress.org listing and the public repository */}
        <Provenance
          eyebrow={HOME_PROVENANCE.eyebrow}
          heading={HOME_PROVENANCE.heading}
          subhead={HOME_PROVENANCE.subhead}
          directory={HOME_PROVENANCE.directory}
          facts={HOME_PROVENANCE.facts}
          repository={HOME_PROVENANCE.repository}
          checks={HOME_PROVENANCE.checks}
        />

        {/* 8. Enroll steps */}
        <Steps
          id="enroll"
          eyebrow={HOME_ENROLL.eyebrow}
          heading={HOME_ENROLL.heading}
          subhead={HOME_ENROLL.subhead}
          steps={[...HOME_ENROLL.steps]}
          cta={HOME_ENROLL.cta}
          tone="muted"
        />

        {/* 9. FAQ */}
        <FAQ
          eyebrow="FAQ"
          heading="Questions, answered straight"
          subhead="The things people ask before they self-host or contribute."
          items={HOME_FAQ}
        />

        {/* 10. Final CTA band */}
        <CTABand
          heading={HOME_FINAL_CTA.heading}
          subhead={HOME_FINAL_CTA.subhead}
          body={HOME_FINAL_CTA.body}
          ctas={[...HOME_FINAL_CTA.ctas]}
        />
      </main>
      <SiteFooter />

      {/* JSON-LD */}
      <JsonLd data={buildSoftwareApplicationLd()} />
      <JsonLd data={buildFAQPageLd(HOME_FAQ)} />
    </>
  );
}
