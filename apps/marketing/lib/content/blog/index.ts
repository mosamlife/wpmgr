// Blog content collection loader.
// Reads MDX files from content/blog/**/*.mdx at build time using gray-matter.
// Each MDX file has a YAML frontmatter block; this module exposes typed helpers
// for generating static params, listing posts, and loading individual posts.
//
// URL structure:
//   /blog/                          <- all posts index
//   /blog/[category]/               <- cluster page (wordpress-security, etc.)
//   /blog/[category]/[slug]/        <- individual post
//
// This nested structure avoids the collision that would occur between
// /blog/[category] and /blog/[slug] at the same dynamic-segment level.
// Each post lives under its cluster so /blog/wordpress-security/harden-your-wp-login
// is unambiguous with /blog/wordpress-security (the cluster hub).

import { readFileSync, readdirSync, statSync, existsSync } from "fs";
import path from "path";
import matter from "gray-matter";

const CONTENT_DIR = path.join(process.cwd(), "content", "blog");

export const BLOG_CATEGORIES = [
  "wordpress-security",
  "wordpress-performance",
  "wordpress-backups",
  "agency-operations",
] as const;

export type BlogCategory = (typeof BLOG_CATEGORIES)[number];

export type BlogPostFrontmatter = {
  title: string;
  description: string;
  category: BlogCategory;
  date: string;
  /** ISO date, set ONLY when the piece has been genuinely revised. Absent means
   *  never revised: see buildArticleLd for why this must not default to `date`. */
  updated?: string;
  slug: string;
  /** Optional: author display name. Defaults to "WPMgr Team". */
  author?: string;
  /** Optional: path relative to /public for the OG thumbnail */
  image?: string;
  /**
   * Which featured illustration to draw, from the registry in
   * components/blog/post-art.tsx. Optional: an unset or unrecognised value
   * falls back to the post's category illustration, so a card is never blank.
   */
  art?: string;
  /** 1 to 3 tags for internal cross-linking */
  tags?: string[];
  /** Funnel: the feature page this post links toward */
  featureSlug?: string;
  /** Funnel: the solution slug this post links toward */
  solutionSlug?: string;
};

export type BlogPost = {
  frontmatter: BlogPostFrontmatter;
  /** file path (needed for dynamic import in MDX rendering) */
  filePath: string;
};

function readPostsInCategory(category: string): BlogPost[] {
  const dir = path.join(CONTENT_DIR, category);
  if (!existsSync(dir)) return [];
  const files = readdirSync(dir).filter((f) => f.endsWith(".mdx"));
  return files.map((file) => {
    const filePath = path.join(dir, file);
    const raw = readFileSync(filePath, "utf-8");
    const { data } = matter(raw);
    const slug = file.replace(/\.mdx$/, "");
    return {
      frontmatter: {
        ...(data as Omit<BlogPostFrontmatter, "slug" | "category">),
        slug,
        category: category as BlogCategory,
      },
      filePath,
    };
  });
}

/** All posts across all categories, sorted newest first. */
export function getAllPosts(): BlogPost[] {
  const posts = BLOG_CATEGORIES.flatMap((cat) => readPostsInCategory(cat));
  return posts.sort(
    (a, b) =>
      new Date(b.frontmatter.date).getTime() - new Date(a.frontmatter.date).getTime(),
  );
}

/** Posts for a single category, sorted newest first. */
export function getPostsByCategory(category: BlogCategory): BlogPost[] {
  return readPostsInCategory(category).sort(
    (a, b) =>
      new Date(b.frontmatter.date).getTime() - new Date(a.frontmatter.date).getTime(),
  );
}

/** Single post lookup by category + slug. Returns null if not found. */
export function getPost(category: BlogCategory, slug: string): BlogPost | null {
  const filePath = path.join(CONTENT_DIR, category, `${slug}.mdx`);
  if (!existsSync(filePath)) return null;
  const raw = readFileSync(filePath, "utf-8");
  const { data } = matter(raw);
  return {
    frontmatter: {
      ...(data as Omit<BlogPostFrontmatter, "slug" | "category">),
      slug,
      category,
    },
    filePath,
  };
}

/** All static params for /blog/[category]/[slug]/page.tsx */
export function getBlogStaticParams(): Array<{ category: string; slug: string }> {
  return BLOG_CATEGORIES.flatMap((cat) => {
    const posts = readPostsInCategory(cat);
    return posts.map((p) => ({ category: cat, slug: p.frontmatter.slug }));
  });
}

// File-size guard: if content/blog is missing on disk (e.g. CI dry-run),
// all functions return empty arrays without throwing.
export function contentDirExists(): boolean {
  try {
    return statSync(CONTENT_DIR).isDirectory();
  } catch {
    return false;
  }
}
