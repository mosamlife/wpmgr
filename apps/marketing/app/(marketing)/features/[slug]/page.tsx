import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { FeaturePage } from "@/components/templates/feature-page";
import { buildMetadata, buildBreadcrumbLd, buildFAQPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { FEATURE_SLUGS, getFeature } from "@/lib/content/features";

// ---------------------------------------------------------------------------
// Static params: generate a route for every feature slug at build time.
// ---------------------------------------------------------------------------
export function generateStaticParams() {
  return FEATURE_SLUGS.map((slug) => ({ slug }));
}

// ---------------------------------------------------------------------------
// Per-page metadata (unique title, description, canonical, OG)
// ---------------------------------------------------------------------------
export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  const feature = getFeature(slug);
  if (!feature) return {};

  return buildMetadata({
    title: feature.metaTitle,
    description: feature.metaDescription,
    canonical: `/features/${slug}`,
  });
}

// ---------------------------------------------------------------------------
// Page: renders the 7-block FeaturePage template
// ---------------------------------------------------------------------------
export default async function FeatureSlugPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const feature = getFeature(slug);
  if (!feature) notFound();

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Features", href: "/features" },
    { name: feature.title, href: `/features/${slug}` },
  ]);

  const faqLd = buildFAQPageLd(feature.faq);

  return (
    <>
      <FeaturePage data={feature} />
      <JsonLd data={breadcrumbLd} />
      <JsonLd data={faqLd} />
    </>
  );
}
