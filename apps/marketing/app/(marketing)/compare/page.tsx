import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd, buildItemListLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { COMPARE_REGISTRY } from "@/lib/content/compare";
import { Container, Section } from "@/components/ui/primitives";

export const metadata: Metadata = buildMetadata({
  title: "Compare WordPress management tools",
  description:
    "Sourced comparisons of the tools people use to manage many WordPress sites. Every claim links to the page it came from and the date it was checked.",
  canonical: "/compare",
});

export default function CompareIndexPage() {
  const pages = Object.values(COMPARE_REGISTRY);

  return (
    // No <main> here: the (marketing) group layout already renders one, and a
    // second nested landmark is invalid HTML and duplicates the target of
    // landmark navigation.
    <>
      <Section>
        <Container>
          <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
            Compare WordPress management tools
          </h1>
          <p className="mt-5 max-w-[70ch] text-lg leading-relaxed text-[var(--muted-foreground)]">
            We build one of the products on these pages, so every factual claim links to the page
            it came from and the date we checked it. That includes the claims that do not favour
            us.
          </p>

          <ul className="mt-10 flex flex-col gap-4">
            {pages.map((p) => (
              <li key={p.slug}>
                <Link
                  href={`/compare/${p.slug}`}
                  className="block rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                >
                  <span className="text-lg font-semibold text-foreground">{p.title}</span>
                  <span className="mt-2 block text-sm leading-relaxed text-[var(--muted-foreground)]">
                    {p.metaDescription}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </Container>
      </Section>
      <JsonLd
        data={buildBreadcrumbLd([
          { name: "Home", href: "/" },
          { name: "Compare", href: "/compare" },
        ])}
      />
      <JsonLd
        data={buildItemListLd(
          pages.map((p) => ({
            name: p.title,
            description: p.metaDescription,
            url: `/compare/${p.slug}`,
          })),
        )}
      />
    </>
  );
}
