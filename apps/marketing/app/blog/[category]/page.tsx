import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import {
  getPostsByCategory,
  BLOG_CATEGORIES,
  type BlogCategory,
} from "@/lib/content/blog";
import { Container, Section, Badge } from "@/components/ui/primitives";
import { PostArt } from "@/components/blog/post-art";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";

export const revalidate = 3600;

// ---------------------------------------------------------------------------
// Collision-free URL structure:
//   /blog/[category]/           <- this file (cluster hub)
//   /blog/[category]/[slug]/    <- individual post (nested)
// The [category] segment is constrained to BLOG_CATEGORIES so it never
// collides with any other path at /blog/*.
// ---------------------------------------------------------------------------

export async function generateStaticParams() {
  return BLOG_CATEGORIES.map((category) => ({ category }));
}

const CATEGORY_META: Record<
  BlogCategory,
  { label: string; heading: string; description: string; solutionHref: string; solutionLabel: string }
> = {
  "wordpress-security": {
    label: "Security",
    heading: "WordPress Security",
    description:
      "Practical guides to hardening WordPress: login protection, file integrity monitoring, vulnerability scanning, and two-factor authentication.",
    solutionHref: "/solutions/wordpress-security",
    solutionLabel: "WordPress Security solution guide",
  },
  "wordpress-performance": {
    label: "Performance",
    heading: "WordPress Performance",
    description:
      "Speed up WordPress with full-page caching, AVIF/WebP images, Redis object cache, and Core Web Vitals monitoring from real visitors.",
    solutionHref: "/solutions/wordpress-performance",
    solutionLabel: "Speed up WordPress solution guide",
  },
  "wordpress-backups": {
    label: "Backups",
    heading: "WordPress Backups",
    description:
      "Build a reliable WordPress backup strategy: frequency, offsite storage, incremental backups, and verified restore procedures.",
    solutionHref: "/solutions/wordpress-backups",
    solutionLabel: "WordPress Backups solution guide",
  },
  "agency-operations": {
    label: "Agency Operations",
    heading: "Agency Operations",
    description:
      "Scale your WordPress agency: fleet management, client reporting, white-label tools, and workflows for managing dozens of sites efficiently.",
    solutionHref: "/solutions/agencies",
    solutionLabel: "For agencies solution guide",
  },
};

type Props = { params: Promise<{ category: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { category } = await params;
  if (!BLOG_CATEGORIES.includes(category as BlogCategory)) return {};
  const meta = CATEGORY_META[category as BlogCategory];
  return buildMetadata({
    title: `${meta.heading} Articles`,
    description: meta.description,
    canonical: `/blog/${category}`,
  });
}

export default async function BlogCategoryPage({ params }: Props) {
  const { category } = await params;

  if (!BLOG_CATEGORIES.includes(category as BlogCategory)) {
    notFound();
  }

  const cat = category as BlogCategory;
  const meta = CATEGORY_META[cat];
  const posts = getPostsByCategory(cat);

  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Blog", href: "/blog" },
    { name: meta.label, href: `/blog/${cat}` },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />
      <SiteHeader />
      <main>
        {/* Hero */}
        <section className="border-b border-[var(--border)] py-16 sm:py-20">
          <Container>
            <nav aria-label="Breadcrumb" className="mb-5 flex items-center gap-2 text-sm text-[var(--muted-foreground)]">
              <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
              <span aria-hidden>/</span>
              <Link href="/blog" className="hover:text-foreground transition-colors">Blog</Link>
              <span aria-hidden>/</span>
              <span className="text-foreground">{meta.label}</span>
            </nav>
            <div className="max-w-2xl">
              <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
                Category
              </p>
              <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
                {meta.heading}
              </h1>
              <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
                {meta.description}
              </p>
              <p className="mt-4 text-sm text-[var(--muted-foreground)]">
                For a comprehensive overview, see the{" "}
                <Link
                  href={meta.solutionHref}
                  className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  {meta.solutionLabel}
                </Link>
                .
              </p>
            </div>
          </Container>
        </section>

        {/* Posts */}
        <Section>
          <Container>
            {posts.length === 0 ? (
              <p className="text-[var(--muted-foreground)]">No articles yet in this category.</p>
            ) : (
              <ul className="grid gap-8 sm:grid-cols-2" role="list">
                {posts.map((post) => {
                  const { frontmatter } = post;
                  const date = new Date(frontmatter.date).toLocaleDateString("en-US", {
                    year: "numeric",
                    month: "long",
                    day: "numeric",
                  });
                  return (
                    <li key={frontmatter.slug}>
                      <Link
                        href={`/blog/${cat}/${frontmatter.slug}`}
                        className="group flex h-full flex-col overflow-hidden rounded-xl border border-[var(--border)] bg-card shadow-sm transition-shadow hover:shadow-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                      >
                        {/* Featured art. overflow-hidden on the card clips it
                            under the top corners, so it runs edge to edge. */}
                        <PostArt
                          art={frontmatter.art}
                          category={frontmatter.category}
                          className="w-full border-b border-[var(--border)]"
                        />
                        <div className="flex flex-1 flex-col p-6">
                        <div className="mb-4 flex items-center gap-3">
                          <Badge>{meta.label}</Badge>
                          <time
                            dateTime={frontmatter.date}
                            className="text-xs text-[var(--muted-foreground)]"
                          >
                            {date}
                          </time>
                        </div>
                        <h2 className="mb-2 text-base font-semibold text-foreground transition-colors group-hover:text-[var(--primary)]">
                          {frontmatter.title}
                        </h2>
                        <p className="mt-auto text-sm leading-relaxed text-[var(--muted-foreground)]">
                          {frontmatter.description}
                        </p>
                        </div>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            )}
          </Container>
        </Section>

        {/* CTA to solution */}
        <section className="border-t border-[var(--border)] py-12">
          <Container>
            <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <h2 className="text-xl font-semibold text-foreground">
                  Ready to go deeper?
                </h2>
                <p className="mt-1 text-[var(--muted-foreground)]">
                  The {meta.solutionLabel} covers strategy, tooling, and best practices in one place.
                </p>
              </div>
              <Link
                href={meta.solutionHref}
                className="inline-flex items-center justify-center rounded-[var(--radius)] bg-primary px-5 py-2.5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] whitespace-nowrap"
              >
                View solution guide
              </Link>
            </div>
          </Container>
        </section>
      </main>
      <SiteFooter />
    </>
  );
}
