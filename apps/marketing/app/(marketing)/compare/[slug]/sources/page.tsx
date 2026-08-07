import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { COMPARE_SLUGS, getComparison } from "@/lib/content/compare";
import { Container, Section } from "@/components/ui/primitives";

// The claim register behind a comparison page.
//
// It exists so every figure on the comparison itself can be checked without
// the comparison having to carry 89 sourced sentences in front of the
// argument, which is what made the first version unreadable. Footnotes on the
// individual figures link straight to the anchor here.
//
// noindex on purpose: this is an audit trail for a reader who doubts a
// specific number, not a page we want competing in search with the comparison
// it supports.

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
    title: `Sources: ${page.title}`,
    description: `Every claim made on the ${page.title} comparison, with the page it came from and the date it was checked.`,
    canonical: `/compare/${slug}/sources`,
    noindex: true,
  });
}

export default async function CompareSourcesPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const page = getComparison(slug);
  if (!page) notFound();

  const total = page.products.reduce((n, p) => n + p.claims.length, 0);

  return (
    <>
      <Section>
        <Container className="max-w-4xl">
          <nav aria-label="Breadcrumb" className="mb-6 text-sm text-[var(--muted-foreground)]">
            <Link href="/compare" className="hover:text-foreground transition-colors">
              Compare
            </Link>
            <span aria-hidden className="px-2">/</span>
            <Link href={`/compare/${slug}`} className="hover:text-foreground transition-colors">
              {page.title}
            </Link>
          </nav>

          <h1 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            Sources
          </h1>
          <p className="mt-4 max-w-[70ch] leading-relaxed text-[var(--muted-foreground)]">
            Every claim behind{" "}
            <Link
              href={`/compare/${slug}`}
              className="text-[var(--primary-pressed)] underline underline-offset-4"
            >
              {page.title}
            </Link>
            , {total} in total, with the page it came from and the date it was checked. Each was
            fetched from the product owner&apos;s own site, documentation, repository or
            WordPress.org listing. Prices change: if one of these is stale, the link is how you
            find out.
          </p>

          {page.products.map((p) => (
            <div key={p.key} className="mt-12">
              <h2 className="text-xl font-semibold text-foreground">{p.name}</h2>
              <ol className="mt-5 flex flex-col gap-5">
                {p.claims.map((c) => (
                  <li key={c.id} id={c.id} className="scroll-mt-24 border-l-0 pl-0">
                    <div className="flex gap-3">
                      <span className="mt-0.5 shrink-0 font-mono text-xs text-[var(--muted-foreground)]">
                        {c.id}
                      </span>
                      <div>
                        <p className="text-sm leading-relaxed text-foreground">{c.claim}</p>
                        <a
                          href={c.sourceUrl}
                          target="_blank"
                          rel="noreferrer noopener"
                          className="mt-1 inline-block break-all text-xs text-[var(--muted-foreground)] underline underline-offset-4 hover:text-foreground"
                        >
                          {c.sourceUrl}
                        </a>
                        <span className="ml-2 text-xs text-[var(--muted-foreground)]">
                          checked {c.verifiedOn}
                        </span>
                      </div>
                    </div>
                  </li>
                ))}
              </ol>
            </div>
          ))}
        </Container>
      </Section>
      <JsonLd
        data={buildBreadcrumbLd([
          { name: "Home", href: "/" },
          { name: "Compare", href: "/compare" },
          { name: page.title, href: `/compare/${slug}` },
          { name: "Sources", href: `/compare/${slug}/sources` },
        ])}
      />
    </>
  );
}
