import type { MetadataRoute } from "next";
import { SITE_CONFIG } from "@/lib/site";
import { FEATURE_SLUGS } from "@/lib/content/features";
import { SOLUTION_SLUGS } from "@/lib/content/solutions";
import { getAllPosts } from "@/lib/content/blog";
import { getAllGuides } from "@/lib/content/guides";
import { COMPARE_SLUGS } from "@/lib/content/compare";

const base = SITE_CONFIG.baseUrl;

// URLs here carry NO trailing slash, deliberately. Next serves this site without
// one, so `/pricing/` 308-redirects to `/pricing`. Every URL in this file used to
// carry a slash, which meant the sitemap advertised ~50 redirects and every
// canonical tag pointed at one. Match what is actually served.

// lastModified is omitted wherever we do not hold a real date. An absent lastmod
// is neutral; a wrong one is a liability. Stamping every URL with the build time,
// which is what this file used to do, tells a crawler the whole site changed
// today, every single deploy, and the honest response to that is to stop
// believing the field. Only dates derived from content are emitted below.

/** Newest date in a list, or undefined when the list is empty. */
function newest(dates: Date[]): Date | undefined {
  if (dates.length === 0) return undefined;
  return dates.reduce((a, b) => (a > b ? a : b));
}

export default function sitemap(): MetadataRoute.Sitemap {
  // Content is read once and reused for both the entries and the index dates.
  let posts: ReturnType<typeof getAllPosts> = [];
  try {
    posts = getAllPosts();
  } catch {
    // content/ is unavailable during some static analysis passes; degrade to
    // an empty list rather than failing the build.
  }

  let guides: ReturnType<typeof getAllGuides> = [];
  try {
    guides = getAllGuides();
  } catch {
    // as above
  }

  const postDates = posts.map((p) => new Date(p.frontmatter.date));
  const guideDates = guides.map((g) => new Date(g.frontmatter.date));

  const categoryDate = (category: string) =>
    newest(
      posts
        .filter((p) => p.frontmatter.category === category)
        .map((p) => new Date(p.frontmatter.date)),
    );

  // An index page is genuinely as fresh as the newest thing it lists, so these
  // dates are real rather than invented.
  const blogIndexDate = newest(postDates);
  const guidesIndexDate = newest(guideDates);

  const coreRoutes: MetadataRoute.Sitemap = [
    { url: base, changeFrequency: "weekly", priority: 1.0 },
    { url: `${base}/features`, changeFrequency: "weekly", priority: 0.9 },
    { url: `${base}/solutions`, changeFrequency: "weekly", priority: 0.9 },
    { url: `${base}/pricing`, changeFrequency: "monthly", priority: 0.8 },
    { url: `${base}/about`, changeFrequency: "monthly", priority: 0.6 },
    { url: `${base}/changelog`, changeFrequency: "weekly", priority: 0.7 },
    { url: `${base}/resources`, changeFrequency: "monthly", priority: 0.7 },
    { url: `${base}/contact`, changeFrequency: "monthly", priority: 0.6 },
    { url: `${base}/docs`, changeFrequency: "monthly", priority: 0.7 },
    { url: `${base}/legal`, changeFrequency: "yearly", priority: 0.3 },
    { url: `${base}/compare`, changeFrequency: "monthly", priority: 0.8 },
    { url: `${base}/self-host`, changeFrequency: "monthly", priority: 0.7 },
    { url: `${base}/legal/security-policy`, changeFrequency: "monthly", priority: 0.5 },
    // Indexable, linked from the Legal column of the footer on every page, and
    // absent from this file until 2026-08-06.
    { url: `${base}/privacy`, changeFrequency: "yearly", priority: 0.3 },
    { url: `${base}/terms`, changeFrequency: "yearly", priority: 0.3 },
    { url: `${base}/refunds`, changeFrequency: "yearly", priority: 0.3 },
    // Blog index + cluster pages
    { url: `${base}/guides`, lastModified: guidesIndexDate, changeFrequency: "monthly", priority: 0.75 },
    { url: `${base}/blog`, lastModified: blogIndexDate, changeFrequency: "weekly", priority: 0.75 },
    {
      url: `${base}/blog/wordpress-security`,
      lastModified: categoryDate("wordpress-security"),
      changeFrequency: "weekly",
      priority: 0.7,
    },
    {
      url: `${base}/blog/wordpress-performance`,
      lastModified: categoryDate("wordpress-performance"),
      changeFrequency: "weekly",
      priority: 0.7,
    },
    {
      url: `${base}/blog/wordpress-backups`,
      lastModified: categoryDate("wordpress-backups"),
      changeFrequency: "weekly",
      priority: 0.7,
    },
    {
      url: `${base}/blog/agency-operations`,
      lastModified: categoryDate("agency-operations"),
      changeFrequency: "weekly",
      priority: 0.7,
    },
  ];

  // No date field exists on feature or solution content, so none is emitted.
  // Adding one to the content types is the honest fix, not a build timestamp.
  const featureRoutes: MetadataRoute.Sitemap = FEATURE_SLUGS.map((slug) => ({
    url: `${base}/features/${slug}`,
    changeFrequency: "monthly" as const,
    priority: slug === "media-optimizer" ? 0.85 : 0.8,
  }));

  // Comparison pages. No date field: the claims carry their own verifiedOn,
  // which is the date that actually matters to a reader here.
  const compareRoutes: MetadataRoute.Sitemap = COMPARE_SLUGS.map((slug) => ({
    url: `${base}/compare/${slug}`,
    changeFrequency: "monthly" as const,
    priority: 0.8,
  }));

  const solutionRoutes: MetadataRoute.Sitemap = SOLUTION_SLUGS.map((slug) => ({
    url: `${base}/solutions/${slug}`,
    changeFrequency: "monthly" as const,
    priority: 0.8,
  }));

  const blogPostRoutes: MetadataRoute.Sitemap = posts.map((post) => ({
    url: `${base}/blog/${post.frontmatter.category}/${post.frontmatter.slug}`,
    lastModified: new Date(post.frontmatter.date),
    changeFrequency: "monthly" as const,
    priority: 0.65,
  }));

  const guideRoutes: MetadataRoute.Sitemap = guides.map((guide) => ({
    url: `${base}/guides/${guide.frontmatter.slug}`,
    lastModified: new Date(guide.frontmatter.date),
    changeFrequency: "monthly" as const,
    priority: 0.75,
  }));

  return [
    ...coreRoutes,
    ...featureRoutes,
    ...solutionRoutes,
    ...compareRoutes,
    ...blogPostRoutes,
    ...guideRoutes,
  ];
}
