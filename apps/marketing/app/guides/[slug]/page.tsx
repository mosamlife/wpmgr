import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { buildMetadata, buildBreadcrumbLd, buildArticleLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { getGuide, getGuideStaticParams } from "@/lib/content/guides";
import { Container } from "@/components/ui/primitives";
import { SiteHeader } from "@/components/sections/header";
import { SiteFooter } from "@/components/sections/footer";
import { ScrollProgress } from "@/components/motion/scroll-progress";
import { CTABand } from "@/components/sections/cta-band";
import { SITE_CONFIG } from "@/lib/site";

export const revalidate = 3600;

type Props = { params: Promise<{ slug: string }> };

export async function generateStaticParams() {
  return getGuideStaticParams();
}

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { slug } = await params;
  const guide = getGuide(slug);
  if (!guide) return {};
  const { frontmatter: fm } = guide;
  return buildMetadata({
    title: fm.title,
    description: fm.description,
    canonical: `/guides/${slug}`,
    ogImage: `${SITE_CONFIG.baseUrl}/opengraph-image`,
  });
}

export default async function GuidePage({ params }: Props) {
  const { slug } = await params;

  const guide = getGuide(slug);
  if (!guide) notFound();

  const { frontmatter: fm } = guide;

  const date = new Date(fm.date).toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
  });

  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Resources", href: "/resources/" },
    { name: "Guides", href: "/guides/" },
    { name: fm.title, href: `/guides/${slug}/` },
  ]);

  const articleLd = buildArticleLd({
    title: fm.title,
    description: fm.description,
    slug: `/guides/${slug}/`,
    datePublished: fm.date,
    authorName: fm.author ?? "WPMgr Team",
  });

  // Dynamically import the compiled MDX module.
  let MDXContent: React.ComponentType;
  try {
    const mod = (await import(
      `@/content/guides/${slug}.mdx`
    )) as { default: React.ComponentType };
    MDXContent = mod.default;
  } catch {
    notFound();
  }

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
            <nav
              aria-label="Breadcrumb"
              className="mb-6 flex flex-wrap items-center gap-2 text-sm text-[var(--muted-foreground)]"
            >
              <Link href="/" className="hover:text-foreground transition-colors">Home</Link>
              <span aria-hidden>/</span>
              <Link href="/resources/" className="hover:text-foreground transition-colors">Resources</Link>
              <span aria-hidden>/</span>
              <span className="text-foreground line-clamp-1 max-w-[200px] sm:max-w-none">
                {fm.title}
              </span>
            </nav>

            <div className="mb-4 flex flex-wrap items-center gap-3">
              <span className="inline-flex items-center rounded-full border border-[var(--border)] bg-card px-3 py-1 text-xs font-medium text-[var(--muted-foreground)]">
                Cornerstone guide
              </span>
              <time dateTime={fm.date} className="text-sm text-[var(--muted-foreground)]">
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

        {/* Footer nav */}
        <section className="border-t border-[var(--border)] py-10">
          <Container className="max-w-4xl">
            <div className="flex flex-wrap gap-8">
              <div>
                <p className="text-sm text-[var(--muted-foreground)]">More guides</p>
                <Link
                  href="/resources/"
                  className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  Back to Resources
                </Link>
              </div>
              {fm.featureSlug && (
                <div>
                  <p className="text-sm text-[var(--muted-foreground)]">Related feature</p>
                  <Link
                    href={`/features/${fm.featureSlug}/`}
                    className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                  >
                    Explore the feature
                  </Link>
                </div>
              )}
              <div>
                <p className="text-sm text-[var(--muted-foreground)]">Browse all features</p>
                <Link
                  href="/features/"
                  className="mt-1 block font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
                >
                  Feature overview
                </Link>
              </div>
            </div>
          </Container>
        </section>

        {/* Closing CTA so no guide is a dead-end */}
        <CTABand
          heading="Put this into practice with WPMgr."
          subhead="Free, self-hostable, open-source WordPress fleet management. No per-site fee."
          ctas={[
            { label: "Get started for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
            { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
          ]}
        />
      </main>
      <SiteFooter />
    </>
  );
}
