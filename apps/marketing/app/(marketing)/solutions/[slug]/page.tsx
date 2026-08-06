import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { SolutionPage } from "@/components/templates/solution-page";
import { buildMetadata, buildBreadcrumbLd, buildFAQPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { SOLUTION_SLUGS, getSolution } from "@/lib/content/solutions";

// ---------------------------------------------------------------------------
// Static params: generate a route for every solution slug at build time.
// ---------------------------------------------------------------------------
export function generateStaticParams() {
  return SOLUTION_SLUGS.map((slug) => ({ slug }));
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
  const solution = getSolution(slug);
  if (!solution) return {};

  return buildMetadata({
    title: solution.metaTitle,
    description: solution.metaDescription,
    canonical: `/solutions/${slug}`,
  });
}

// ---------------------------------------------------------------------------
// Page: renders the 6-block SolutionPage template
// ---------------------------------------------------------------------------
export default async function SolutionSlugPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const solution = getSolution(slug);
  if (!solution) notFound();

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Solutions", href: "/solutions" },
    { name: solution.title, href: `/solutions/${slug}/` },
  ]);

  const faqLd = buildFAQPageLd(solution.faq);

  return (
    <>
      <SolutionPage data={solution} />
      <JsonLd data={breadcrumbLd} />
      <JsonLd data={faqLd} />
    </>
  );
}
