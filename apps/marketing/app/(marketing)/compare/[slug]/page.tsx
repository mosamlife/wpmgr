import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { ComparisonPage } from "@/components/templates/comparison-page";
import { buildMetadata, buildBreadcrumbLd, buildFAQPageLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { COMPARE_SLUGS, getComparison } from "@/lib/content/compare";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";

export function generateStaticParams() {
  return COMPARE_SLUGS.map((slug) => ({ slug }));
}

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  const page = getComparison(slug);
  if (!page) return {};
  return buildMetadata({
    title: page.metaTitle,
    description: page.metaDescription,
    canonical: `/compare/${slug}`,
  });
}

export default async function ComparePage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const page = getComparison(slug);
  if (!page) notFound();

  const breadcrumbLd = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Compare", href: "/compare" },
    { name: page.title, href: `/compare/${slug}` },
  ]);

  return (
    <>
      <SiteHeader />
      <main>
        <ComparisonPage data={page} />
      </main>
      <SiteFooter />
      <JsonLd data={breadcrumbLd} />
      {page.faq.length > 0 && <JsonLd data={buildFAQPageLd(page.faq)} />}
    </>
  );
}
