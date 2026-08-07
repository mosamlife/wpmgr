import type { NextConfig } from "next";
import path from "path";
import createMDX from "@next/mdx";

// MDX compilation for the blog and guide content collections.
//
// WHY NOT experimental.mdxRs. The Rust compiler was used here for native GFM
// and to dodge Turbopack's "non-serializable options" error, but it does not
// strip YAML frontmatter. Every .mdx file in content/ opens with a `---` block,
// so the compiled output rendered that block as page content: a thematic break
// followed by `title: "..." description: "..." date: "..."` set as a heading,
// directly under the real heading. It shipped that way on every blog post and
// guide, which is what /guides/wordpress-maintenance was showing publicly.
//
// remark-frontmatter teaches the parser that the block is frontmatter and not
// content, so it is dropped from the output. The metadata is unaffected: the
// loaders in lib/content/ read frontmatter with gray-matter from the RAW FILE
// on disk, never from the compiled module, so stripping it at compile time
// removes the duplicate render and nothing else.
//
// remark-gfm has to come back explicitly, since it was mdxRs providing GFM.
//
// PLUGINS ARE NAMED AS STRINGS, NOT IMPORTED. Turbopack passes loader options
// across a serialization boundary and rejects functions with "does not have
// serializable options", which is the error the previous comment here was
// working around. A module name is a plain string, so Turbopack resolves it on
// the other side and the whole problem disappears. Importing these and passing
// the functions fails the build; this does not.
const withMDX = createMDX({
  options: {
    remarkPlugins: [["remark-frontmatter"], ["remark-gfm"]],
  },
});

const nextConfig: NextConfig = {
  output: "standalone",
  // CRITICAL monorepo gotcha: without this, the file-trace roots at
  // apps/marketing and drops hoisted workspace dependencies.
  outputFileTracingRoot: path.join(__dirname, "../../"),
  // Allow .mdx files as pages (page-extension pattern for MDX route files).
  // Content-collection MDX (blog/guides) uses gray-matter + fs loader, not
  // page-extension MDX, so standalone MDX route files stay as .tsx.
  pageExtensions: ["tsx", "ts", "mdx"],
  // experimental.mdxRs is deliberately absent. See the createMDX comment above:
  // the Rust compiler does not strip frontmatter, and the remark plugins that
  // do cannot be passed to it.
};

export default withMDX(nextConfig);
