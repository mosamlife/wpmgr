import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { getAllPosts, BLOG_CATEGORIES } from "@/lib/content/blog";
import { Container, Section, Badge } from "@/components/ui/primitives";
import { PostArt } from "@/components/blog/post-art";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";

export const revalidate = 3600;

export const metadata: Metadata = buildMetadata({
  title: "Blog: WordPress Operations, Security, and Performance",
  description:
    "Practical articles on WordPress security, performance, and backups. Written for site operators, developers, and agencies who run WordPress at scale.",
  canonical: "/blog",
});

const CATEGORY_META: Record<string, { label: string; description: string; color: string }> = {
  "wordpress-security": {
    label: "Security",
    description: "Hardening, vulnerability scanning, file integrity, and access control.",
    color: "var(--destructive)",
  },
  "wordpress-performance": {
    label: "Performance",
    description: "Page speed, Core Web Vitals, image optimisation, and caching.",
    color: "var(--primary)",
  },
  "wordpress-backups": {
    label: "Backups",
    description: "Backup strategy, restore procedures, and disaster recovery.",
    color: "var(--success, oklch(55% 0.15 145))",
  },
  "agency-operations": {
    label: "Agency",
    description: "Fleet management, client reporting, and agency workflows.",
    color: "var(--info, oklch(55% 0.12 235))",
  },
};

export default function BlogIndexPage() {
  const posts = getAllPosts();

  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Blog", href: "/blog" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />
      <SiteHeader />
      <main>
        {/* Hero */}
        <section className="border-b border-[var(--border)] py-16 sm:py-20">
          <Container>
            <div className="max-w-2xl">
              <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
                Blog
              </p>
              <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
                WordPress operations, security, and performance
              </h1>
              <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
                Practical articles for site operators, developers, and agencies who run WordPress at
                scale.
              </p>
            </div>
          </Container>
        </section>

        {/* Category nav */}
        <section className="border-b border-[var(--border)] py-6">
          <Container>
            <nav aria-label="Blog categories" className="flex flex-wrap gap-3">
              {BLOG_CATEGORIES.map((cat) => {
                const meta = CATEGORY_META[cat];
                if (!meta) return null;
                return (
                  <Link
                    key={cat}
                    href={`/blog/${cat}`}
                    className="inline-flex items-center gap-2 rounded-full border border-[var(--border)] bg-card px-4 py-1.5 text-sm font-medium text-[var(--muted-foreground)] transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                  >
                    {meta.label}
                  </Link>
                );
              })}
            </nav>
          </Container>
        </section>

        {/* Post grid */}
        <Section>
          <Container>
            {posts.length === 0 ? (
              <p className="text-[var(--muted-foreground)]">No posts found.</p>
            ) : (
              <ul className="grid gap-8 sm:grid-cols-2 lg:grid-cols-3" role="list">
                {posts.map((post) => {
                  const { frontmatter } = post;
                  const catMeta = CATEGORY_META[frontmatter.category];
                  const date = new Date(frontmatter.date).toLocaleDateString("en-US", {
                    year: "numeric",
                    month: "long",
                    day: "numeric",
                  });
                  return (
                    <li key={`${frontmatter.category}/${frontmatter.slug}`}>
                      <Link
                        href={`/blog/${frontmatter.category}/${frontmatter.slug}`}
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
                          <Badge>{catMeta ? catMeta.label : frontmatter.category}</Badge>
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
      </main>
      <SiteFooter />
    </>
  );
}
