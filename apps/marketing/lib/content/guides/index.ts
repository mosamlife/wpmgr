// Guide content collection loader.
// Long-form pillar content under content/guides/*.mdx

import { readFileSync, readdirSync, existsSync } from "fs";
import path from "path";
import matter from "gray-matter";

const CONTENT_DIR = path.join(process.cwd(), "content", "guides");

export const GUIDE_SLUGS = [
  "wordpress-maintenance",
  "core-web-vitals",
] as const;

export type GuideSlug = (typeof GUIDE_SLUGS)[number];

export type GuideFrontmatter = {
  title: string;
  description: string;
  date: string;
  /** ISO date, set ONLY when the piece has been genuinely revised. */
  updated?: string;
  slug: string;
  author?: string;
  image?: string;
  /** Funnel links */
  featureSlug?: string;
  solutionSlug?: string;
};

export type Guide = {
  frontmatter: GuideFrontmatter;
  filePath: string;
};

export function getAllGuides(): Guide[] {
  if (!existsSync(CONTENT_DIR)) return [];
  const files = readdirSync(CONTENT_DIR).filter((f) => f.endsWith(".mdx"));
  return files.map((file) => {
    const filePath = path.join(CONTENT_DIR, file);
    const raw = readFileSync(filePath, "utf-8");
    const { data } = matter(raw);
    const slug = file.replace(/\.mdx$/, "");
    return { frontmatter: { ...(data as Omit<GuideFrontmatter, "slug">), slug }, filePath };
  });
}

export function getGuide(slug: GuideSlug | string): Guide | null {
  const filePath = path.join(CONTENT_DIR, `${slug}.mdx`);
  if (!existsSync(filePath)) return null;
  const raw = readFileSync(filePath, "utf-8");
  const { data } = matter(raw);
  return { frontmatter: { ...(data as Omit<GuideFrontmatter, "slug">), slug }, filePath };
}

export function getGuideStaticParams(): Array<{ slug: string }> {
  if (!existsSync(CONTENT_DIR)) return GUIDE_SLUGS.map((s) => ({ slug: s }));
  const files = readdirSync(CONTENT_DIR).filter((f) => f.endsWith(".mdx"));
  return files.map((f) => ({ slug: f.replace(/\.mdx$/, "") }));
}
