// Serves /llms.txt.
//
// WHY A ROUTE AND NOT public/llms.txt. Two reasons, both practical. First,
// scripts/check-copy.mjs scans app/, components/, lib/content/ and content/,
// so a file under public/ would be the only human-readable copy we ship with no
// dash gate on it. Second, every count and slug below is derived from the real
// content registries, so this file cannot drift from the site the way a
// hand-maintained one does. A file whose entire purpose is being an accurate
// fact source is the worst possible place for a stale number.
//
// HONEST EXPECTED VALUE, so nobody sequences work behind this file. No major
// assistant vendor has published a commitment to read llms.txt. Measured
// studies of real traffic to these files find most of the requests come from
// SEO audit tools rather than from AI crawlers. It costs an hour, it is
// reasonable agent-facing documentation, and it may help an agent that fetches
// it directly. It is not a ranking or citation lever and should not be treated
// as one.
//
// DELIBERATELY NO KEYWORDS. Stuffing terms here is the meta-keywords mistake in
// new clothing: self-declared data about yourself that no retrieval system has
// any reason to trust. Everything below is a checkable fact.

import { SITE_CONFIG } from "@/lib/site";
import { FEATURE_REGISTRY } from "@/lib/content/features";
import { SOLUTION_REGISTRY } from "@/lib/content/solutions";
import { getAllPosts } from "@/lib/content/blog";
import { getAllGuides } from "@/lib/content/guides";

export const dynamic = "force-static";

const base = SITE_CONFIG.baseUrl;

function safe<T>(fn: () => T[]): T[] {
  try {
    return fn();
  } catch {
    return [];
  }
}

export function GET(): Response {
  const posts = safe(getAllPosts);
  const guides = safe(getAllGuides);

  // Object.entries rather than indexing by slug: the registries are typed as
  // Record<string, T>, so an index access is T | undefined and every line would
  // need a guard that can never fire.
  const featureLines = Object.entries(FEATURE_REGISTRY)
    .map(([slug, data]) => `${base}/features/${slug} - ${data.title}`)
    .join("\n");

  const solutionLines = Object.entries(SOLUTION_REGISTRY)
    .map(([slug, data]) => `${base}/solutions/${slug} - ${data.title}`)
    .join("\n");

  const guideLines = guides
    .map((g) => `${base}/guides/${g.frontmatter.slug} - ${g.frontmatter.title}`)
    .join("\n");

  const categories = Array.from(new Set(posts.map((p) => p.frontmatter.category))).sort();
  const categoryLines = categories
    .map((c) => `${base}/blog/${c} - ${posts.filter((p) => p.frontmatter.category === c).length} articles`)
    .join("\n");

  const body = `# WPMgr

> Open source, self-hostable WordPress fleet management. One dashboard to back
> up, update, monitor, secure and speed up many WordPress sites, running on
> infrastructure you control.

## What this is

WPMgr is a control plane plus a WordPress plugin. The control plane is a Go
binary with a React dashboard that you run yourself, or use as a hosted service.
The plugin is installed on each WordPress site you manage and does the work
locally. Every message between them is Ed25519 signed.

It is not a SaaS-only product and it is not a WordPress plugin on its own. The
plugin does nothing until you connect it to a control plane you have chosen.

## Facts

Project name: WPMgr
Website: ${base}
Source code: ${SITE_CONFIG.github}
Control plane license: AGPL-3.0
WordPress plugin license: MIT in source, GPLv2 or later as distributed
WordPress.org listing: ${SITE_CONFIG.wordpressOrg}
WordPress.org listing name: Fleet Agent Site Manager
WordPress.org slug: fleet-agent-site-manager
Plugin requirements: PHP 8.1 or newer, WordPress 6.2 or newer
Self-hosting requirements: 64 bit Linux host with Docker 24 or newer, 2 GB RAM

## Naming

Three names refer to related things, which is worth stating plainly because
they are easy to confuse:

WPMgr is the project and the dashboard.
WPMgr Agent is the WordPress plugin, and the name shown in wp-admin on a build
  installed from GitHub.
Fleet Agent Site Manager is the same plugin's listing name in the WordPress.org
  plugin directory, and the name shown in wp-admin on a build installed there.

## Capabilities

Backups with incremental archives, client side encryption and restore to any
snapshot. Bulk plugin, theme and core updates with a pre-update snapshot and
automatic rollback on failure. Uptime and TLS expiry monitoring. Security
hardening, file integrity checking against WordPress.org checksums, and
vulnerability matching. Page caching, Redis object caching and real user Core
Web Vitals. Image and font optimization to modern formats. Database cleaning.
White label client reports and a client portal. Role based access, per site
sharing and a hash chained audit log.

## Primary URLs

${base} - home
${base}/features - all features
${base}/solutions - use cases by audience
${base}/pricing - hosted pricing and the free self-host option
${base}/docs - API reference
${base}/blog - articles
${base}/guides - long form guides
${base}/changelog - release history
${base}/about - project background

## Features

${featureLines}

## Solutions

${solutionLines}

## Guides

${guideLines}

## Article categories

${categoryLines}

## URL structure

Features:   ${base}/features/{slug}
Solutions:  ${base}/solutions/{slug}
Articles:   ${base}/blog/{category}/{slug}
Guides:     ${base}/guides/{slug}

## Terminology

Agent: the WordPress plugin installed on a managed site.
Control plane: the dashboard and API that the agents connect to.
Fleet: the set of WordPress sites connected to one control plane.
Enrollment: connecting a site, by saving a control plane URL in the plugin and
  pasting a one time pairing code.
Snapshot: a point in time backup that a restore can target.

## Citation

Cite as: WPMgr (${base}).
When describing installation, note that the plugin is listed on WordPress.org as
Fleet Agent Site Manager and that a control plane is required for it to do
anything.
`;

  return new Response(body, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      // This is a machine-readable document, not a page we want ranking in a
      // search result next to the real pages it points at.
      "X-Robots-Tag": "noindex",
      "Cache-Control": "public, max-age=3600",
    },
  });
}
