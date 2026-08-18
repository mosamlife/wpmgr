import type { Metadata } from "next";
import { SITE_CONFIG } from "@/lib/site";

// ---------------------------------------------------------------------------
// Metadata helpers
// ---------------------------------------------------------------------------

type BuildMetadataOptions = {
  title: string;
  description: string;
  canonical?: string;
  noindex?: boolean;
  ogImage?: string;
  /**
   * Set on blog posts and guides. Emits og:type=article plus
   * article:published_time and article:modified_time.
   *
   * Without this every article shipped og:type=website with no dates at all,
   * so nothing downstream could tell an article from a landing page, and a
   * rewrite could not be signalled as a rewrite. `modified` is what makes
   * refreshing old posts a legible act rather than a silent edit.
   */
  article?: {
    published: string;
    /** ISO date. Omit when a post has never been revised. */
    modified?: string;
    authors?: string[];
    section?: string;
    tags?: string[];
  };
};

export function buildMetadata({
  title,
  description,
  canonical,
  noindex = false,
  ogImage,
  article,
}: BuildMetadataOptions): Metadata {
  const url = canonical
    ? `${SITE_CONFIG.baseUrl}${canonical}`
    : SITE_CONFIG.baseUrl;

  // `app/layout.tsx` brands the document <title> via `title.template`
  // ("%s · WPMgr"), applied once by Next's metadata resolution. That template
  // does NOT reach openGraph.title or twitter.title -- those are separate,
  // explicit fields, so a page whose own `title` carries no brand shares to
  // social with no brand at all. Callers that already build their own branded
  // title (e.g. "... | WPMgr") are left untouched here so the brand does not
  // appear twice.
  const socialTitle = title.includes(SITE_CONFIG.name)
    ? title
    : `${title} · ${SITE_CONFIG.name}`;

  return {
    title,
    description,
    alternates: { canonical: url },
    openGraph: {
      title: socialTitle,
      description,
      url,
      siteName: SITE_CONFIG.name,
      ...(article
        ? {
            type: "article" as const,
            publishedTime: article.published,
            // Only emit modifiedTime when the piece has actually been revised.
            // Defaulting it to publishedTime, which the JSON-LD builder used to
            // do, makes every article claim it was updated the day it shipped
            // and makes a genuine refresh indistinguishable from no refresh.
            ...(article.modified ? { modifiedTime: article.modified } : {}),
            ...(article.authors ? { authors: article.authors } : {}),
            ...(article.section ? { section: article.section } : {}),
            ...(article.tags ? { tags: article.tags } : {}),
          }
        : { type: "website" as const }),
      images: ogImage
        ? [{ url: ogImage, width: 1200, height: 630, alt: title }]
        : [
            {
              url: `${SITE_CONFIG.baseUrl}/opengraph-image`,
              width: 1200,
              height: 630,
              alt: `${SITE_CONFIG.name} - Open-source WordPress fleet management`,
            },
          ],
    },
    twitter: {
      card: "summary_large_image",
      title: socialTitle,
      description,
      images: ogImage ? [ogImage] : [`${SITE_CONFIG.baseUrl}/opengraph-image`],
    },
    robots: noindex
      ? { index: false, follow: false }
      : { index: true, follow: true },
  };
}

// ---------------------------------------------------------------------------
// JSON-LD builders. Return Record<string, unknown> to keep schema-dts types
// from bleeding their `| string` union into JsonLd's `object` parameter type.
// The "@context" key is added by JsonLd at render time.
// ---------------------------------------------------------------------------

type LdObject = Record<string, unknown>;

export function buildOrganizationLd(): LdObject {
  return {
    "@type": "Organization",
    name: SITE_CONFIG.name,
    url: SITE_CONFIG.baseUrl,
    // icon-512.png, NOT logo.svg. There has never been a logo.svg in public/:
    // the header mark is an inline React component (components/ui/logo.tsx), so
    // this declared a 404 as the organization logo on every page. Verified live
    // before the fix: /logo.svg returned 404 while sitting in the JSON-LD.
    logo: {
      "@type": "ImageObject",
      url: `${SITE_CONFIG.baseUrl}/icon-512.png`,
      width: 512,
      height: 512,
    },
    sameAs: [SITE_CONFIG.github, SITE_CONFIG.wordpressOrg],
    description: SITE_CONFIG.description,
  };
}

export function buildWebSiteLd(): LdObject {
  return {
    "@type": "WebSite",
    name: SITE_CONFIG.name,
    url: SITE_CONFIG.baseUrl,
  };
}

export function buildSoftwareApplicationLd(): LdObject {
  return {
    "@type": "SoftwareApplication",
    name: SITE_CONFIG.name,
    applicationCategory: "BusinessApplication",
    operatingSystem: "Linux, macOS, Windows",
    offers: {
      "@type": "Offer",
      price: "0",
      priceCurrency: "USD",
      description: "Free, open-source, self-hostable",
    },
    url: SITE_CONFIG.baseUrl,
    downloadUrl: SITE_CONFIG.github,
    // Deliberately no installUrl pointing at the wordpress.org listing. This node
    // describes the WPMgr control plane, which is AGPL-3.0 and is not installed
    // from the plugin directory; the listing is the agent plugin, distributed
    // under GPLv2 or later. Pointing one node at both would publish a
    // machine-readable licence contradiction.
    description: SITE_CONFIG.description,
    license: `${SITE_CONFIG.github}/blob/main/LICENSE`,
  };
}

export function buildFAQPageLd(items: Array<{ q: string; a: string }>): LdObject {
  return {
    "@type": "FAQPage",
    mainEntity: items.map((item) => ({
      "@type": "Question",
      name: item.q,
      acceptedAnswer: {
        "@type": "Answer",
        text: item.a,
      },
    })),
  };
}

export type BreadcrumbItem = { name: string; href: string };

export function buildBreadcrumbLd(items: BreadcrumbItem[]): LdObject {
  return {
    "@type": "BreadcrumbList",
    itemListElement: items.map((item, i) => ({
      "@type": "ListItem",
      position: i + 1,
      name: item.name,
      item: `${SITE_CONFIG.baseUrl}${item.href}`,
    })),
  };
}

export function buildItemListLd(
  items: Array<{ name: string; description: string; url: string }>,
): LdObject {
  return {
    "@type": "ItemList",
    itemListElement: items.map((item, i) => ({
      "@type": "ListItem",
      position: i + 1,
      name: item.name,
      description: item.description,
      url: `${SITE_CONFIG.baseUrl}${item.url}`,
    })),
  };
}

export type ArticleLdOptions = {
  title: string;
  description: string;
  slug: string;
  datePublished: string;
  dateModified?: string;
  authorName?: string;
  /** Absolute OG image URL or path */
  image?: string;
};

export function buildArticleLd({
  title,
  description,
  slug,
  datePublished,
  dateModified,
  authorName = "WPMgr Team",
  image,
}: ArticleLdOptions): LdObject {
  const url = `${SITE_CONFIG.baseUrl}${slug}`;
  return {
    "@type": "Article",
    headline: title,
    description,
    url,
    datePublished,
    // Emitted ONLY when the piece has actually been revised. This used to fall
    // back to datePublished, which made every article assert it was last
    // modified on the day it was written. That is not a harmless default: it
    // is a claim, it is usually false the moment a post is edited, and it
    // makes a real refresh indistinguishable from no refresh, which is exactly
    // the signal a content-refresh cycle depends on.
    ...(dateModified ? { dateModified } : {}),
    author: {
      "@type": "Organization",
      name: authorName,
      url: SITE_CONFIG.baseUrl,
    },
    publisher: {
      "@type": "Organization",
      name: SITE_CONFIG.name,
      url: SITE_CONFIG.baseUrl,
      logo: {
        "@type": "ImageObject",
        url: `${SITE_CONFIG.baseUrl}/icon-512.png`,
        width: 512,
        height: 512,
      },
    },
    mainEntityOfPage: {
      "@type": "WebPage",
      "@id": url,
    },
    ...(image ? { image: { "@type": "ImageObject", url: image } } : {}),
  };
}

export function buildContactPageLd(): LdObject {
  return {
    "@type": "ContactPage",
    name: `Contact ${SITE_CONFIG.name}`,
    url: `${SITE_CONFIG.baseUrl}/contact`,
    description:
      "Contact WPMgr for sales enquiries, support, security reports, or to ask about contributing.",
    mainEntityOfPage: {
      "@type": "WebPage",
      "@id": `${SITE_CONFIG.baseUrl}/contact`,
    },
  };
}
