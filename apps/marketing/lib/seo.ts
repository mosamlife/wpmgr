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
};

export function buildMetadata({
  title,
  description,
  canonical,
  noindex = false,
  ogImage,
}: BuildMetadataOptions): Metadata {
  const url = canonical
    ? `${SITE_CONFIG.baseUrl}${canonical}`
    : SITE_CONFIG.baseUrl;

  return {
    title,
    description,
    alternates: { canonical: url },
    openGraph: {
      title,
      description,
      url,
      siteName: SITE_CONFIG.name,
      type: "website",
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
      title,
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
    logo: `${SITE_CONFIG.baseUrl}/logo.svg`,
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
    dateModified: dateModified ?? datePublished,
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
        url: `${SITE_CONFIG.baseUrl}/logo.svg`,
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
    url: `${SITE_CONFIG.baseUrl}/contact/`,
    description:
      "Contact WPMgr for sales enquiries, support, security reports, or to ask about contributing.",
    mainEntityOfPage: {
      "@type": "WebPage",
      "@id": `${SITE_CONFIG.baseUrl}/contact/`,
    },
  };
}
