import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { buildMetadata, buildBreadcrumbLd, buildArticleLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import {
  getPost,
  getBlogStaticParams,
  BLOG_CATEGORIES,
  type BlogCategory,
} from "@/lib/content/blog";
import { FEATURE_REGISTRY } from "@/lib/content/features";
import { SOLUTION_REGISTRY } from "@/lib/content/solutions";
import { Container } from "@/components/ui/primitives";
import { PostArt } from "@/components/blog/post-art";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";
import { ScrollProgress } from "@/components/motion/scroll-progress";
import { CTABand } from "@/components/sections/cta-band";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const revalidate = 3600;

type Props = { params: Promise<{ category: string; slug: string }> };

export async function generateStaticParams() {
  return getBlogStaticParams();
}

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { category, slug } = await params;
  const post = getPost(category as BlogCategory, slug);
  if (!post) return {};
  const { frontmatter: fm } = post;
  const ogImageUrl = `${SITE_CONFIG.baseUrl}/blog/${category}/${slug}/opengraph-image`;
  return buildMetadata({
    title: fm.title,
    description: fm.description,
    canonical: `/blog/${category}/${slug}`,
    ogImage: ogImageUrl,
    article: {
      published: fm.date,
      ...(fm.updated ? { modified: fm.updated } : {}),
      authors: [fm.author ?? "WPMgr Team"],
      section: CATEGORY_LABELS[category] ?? category,
      ...(fm.tags ? { tags: fm.tags } : {}),
    },
  });
}

const CATEGORY_LABELS: Record<string, string> = {
  "wordpress-security": "Security",
  "wordpress-performance": "Performance",
  "wordpress-backups": "Backups",
  "agency-operations": "Agency Operations",
};

export default async function BlogPostPage({ params }: Props) {
  const { category, slug } = await params;

  if (!BLOG_CATEGORIES.includes(category as BlogCategory)) {
    notFound();
  }

  const post = getPost(category as BlogCategory, slug);
  if (!post) notFound();

  const { frontmatter: fm } = post;
  const catLabel = CATEGORY_LABELS[category] ?? category;

  const date = new Date(fm.date).toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
  });

  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Blog", href: "/blog" },
    { name: catLabel, href: `/blog/${category}` },
    { name: fm.title, href: `/blog/${category}/${slug}` },
  ]);

  const articleLd = buildArticleLd({
    title: fm.title,
    description: fm.description,
    slug: `/blog/${category}/${slug}`,
    datePublished: fm.date,
    ...(fm.updated ? { dateModified: fm.updated } : {}),
    authorName: fm.author ?? "WPMgr Team",
    image: `${SITE_CONFIG.baseUrl}/blog/${category}/${slug}/opengraph-image`,
  });

  // Dynamically import the compiled MDX module at request time.
  // The file path is resolved from the content/ directory.
  // Each MDX file becomes a React component when compiled by @next/mdx.
  let MDXContent: React.ComponentType;
  try {
    const mod = (await import(
      `@/content/blog/${category}/${slug}.mdx`
    )) as { default: React.ComponentType };
    MDXContent = mod.default;
  } catch {
    notFound();
  }

  const solutionSlug = fm.solutionSlug;

  return (
    <>
      <JsonLd data={breadcrumb} />
      <JsonLd data={articleLd} />
      <ScrollProgress />
      <SiteHeader />
      <main>
        {/* Header */}
        <section className="border-b border-[var(--border)] py-14 sm:py-18">
          <Container className="max-w-4xl">
            <nav aria-label="Breadcrumb" className="mb-6 flex flex-wrap items-center gap-2 text-sm text-[var(--muted-foreground)]">
              <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
              <span aria-hidden>/</span>
              <Link href="/blog" className="hover:text-foreground transition-colors">Blog</Link>
              <span aria-hidden>/</span>
              <Link href={`/blog/${category}`} className="hover:text-foreground transition-colors">
                {catLabel}
              </Link>
              <span aria-hidden>/</span>
              <span className="text-foreground line-clamp-1 max-w-[200px] sm:max-w-none">
                {fm.title}
              </span>
            </nav>

            <div className="mb-4 flex flex-wrap items-center gap-3">
              <Link
                href={`/blog/${category}`}
                className="inline-flex items-center rounded-full border border-[var(--border)] bg-card px-3 py-1 text-xs font-medium text-[var(--muted-foreground)] transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)]"
              >
                {catLabel}
              </Link>
              <time
                dateTime={fm.date}
                className="text-sm text-[var(--muted-foreground)]"
              >
                {date}
              </time>
              {fm.author && (
                <span className="text-sm text-[var(--muted-foreground)]">{fm.author}</span>
              )}
            </div>

            <h1 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl lg:text-5xl">
              {fm.title}
            </h1>
            <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)] max-w-[72ch]">
              {fm.description}
            </p>

            {/* Featured art. Below the deck rather than above the h1: the
                headline is the LCP element on this page and must not be pushed
                under the fold by a decorative band. */}
            <PostArt
              art={fm.art}
              category={category as BlogCategory}
              className="mt-10 w-full overflow-hidden rounded-xl border border-[var(--border)]"
            />
          </Container>
        </section>

        {/* Content */}
        <section className="py-12 sm:py-16">
          <Container className="max-w-4xl">
            <article className="prose-custom">
              <MDXContent />
            </article>
          </Container>
        </section>

        {/* Navigation footer: cluster + feature + solution links, no dead-ends */}
        <section className="border-t border-[var(--border)] py-10">
          <Container className="max-w-4xl">
            <div className="flex flex-wrap gap-8">
              <div>
                <p className="text-sm text-[var(--muted-foreground)]">More in this category</p>
                <Link
                  href={`/blog/${category}`}
                  className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  All {catLabel} articles
                </Link>
              </div>
              {fm.featureSlug && (
                <div>
                  <p className="text-sm text-[var(--muted-foreground)]">Related feature</p>
                  <Link
                    href={`/features/${fm.featureSlug}`}
                    className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    {FEATURE_REGISTRY[fm.featureSlug]?.title ?? "Explore the feature"}
                  </Link>
                </div>
              )}
              {solutionSlug && (
                <div>
                  <p className="text-sm text-[var(--muted-foreground)]">Solution guide</p>
                  <Link
                    href={`/solutions/${solutionSlug}`}
                    className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    {SOLUTION_REGISTRY[solutionSlug]?.title ?? "See the full solution"}
                  </Link>
                </div>
              )}
            </div>
          </Container>
        </section>

        {/* Closing CTA so no post is a dead-end */}
        <CTABand
          heading="Manage your WordPress fleet with one open-source dashboard."
          subhead="Free, self-hostable, no per-site fee. Read every line before you run it."
          ctas={[
            { label: "Get started for free", href: signupHref("blog-post"), variant: "primary", icon: "ArrowRight" },
          ]}
        />
      </main>
      <SiteFooter />
    </>
  );
}
