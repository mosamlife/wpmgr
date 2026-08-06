import type { MetadataRoute } from "next";
import { SITE_CONFIG } from "@/lib/site";

// Paths no crawler should index. Repeated into every named group below, and the
// repetition is load-bearing: a robots.txt group named for a specific user agent
// REPLACES the "*" group for that agent, it does not add to it. A named group
// carrying only "Allow: /" would therefore hand that one crawler the paths the
// wildcard group disallows. That is the most common way an AI-crawler block gets
// added to a robots.txt and quietly opens something.
const DISALLOW = ["/api/", "/product-hunt/"];

// Crawlers we explicitly welcome. WPMgr wants to be the answer when somebody
// asks an assistant how to manage a fleet of WordPress sites, so retrieval
// crawlers are the ones that matter: block them and you are not in the answer.
//
// Every token below was read from the vendor's own published documentation on
// 2026-08-06, because a robots.txt token that matches nothing is silent. Two
// tokens in wide circulation are deliberately NOT here for that reason:
//   anthropic-ai, Claude-Web   absent from Anthropic's current crawler doc.
// They are legacy strings copied between blog posts. Rules naming them do
// nothing at all, in either direction.
const AI_CRAWLERS = [
  // Anthropic. support.claude.com/en/articles/8896518
  "ClaudeBot", //        training corpus
  "Claude-User", //      fetches a page because a user asked Claude about it
  "Claude-SearchBot", // search quality

  // OpenAI. developers.openai.com/api/docs/bots
  "GPTBot", //           training corpus
  "OAI-SearchBot", //    surfaces sites in ChatGPT search
  "ChatGPT-User", //     fetches a page because a user asked ChatGPT about it

  // Perplexity. docs.perplexity.ai/guides/bots
  "PerplexityBot", //    indexes for Perplexity results, explicitly not training
  "Perplexity-User", //  user-initiated fetch

  // Google. Gemini training and grounding. Verified against Google's crawler
  // doc: this token has no effect on Google Search inclusion or ranking, so
  // allowing it costs nothing in search.
  "Google-Extended",

  // Apple Intelligence training and grounding.
  "Applebot-Extended",

  // Common Crawl. Not an assistant, but a corpus most model builders ingest.
  "CCBot",
];

export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      { userAgent: "*", allow: "/", disallow: DISALLOW },
      // Same permissions as the wildcard group, stated explicitly. This changes
      // no behaviour today, since "*" already allows these agents. It is here so
      // the intent survives a future edit to the wildcard group, and so anyone
      // auditing the file can see the decision was made rather than defaulted
      // into.
      //
      // NOT listed, on purpose: OAI-AdsBot. It is documented as validating pages
      // submitted as ChatGPT ads, not as a training or retrieval crawler, so it
      // does nothing for us until we buy ads there. The wildcard group still
      // allows it; this is a note, not a block.
      { userAgent: AI_CRAWLERS, allow: "/", disallow: DISALLOW },
    ],
    // Only real XML sitemaps belong here. llms.txt is NOT a sitemap and listing
    // it under this directive would hand search engines a document they cannot
    // parse. It is discoverable at its well-known path instead.
    sitemap: `${SITE_CONFIG.baseUrl}/sitemap.xml`,
  };
}
