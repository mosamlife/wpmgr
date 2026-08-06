import type { MDXComponents } from "mdx/types";
import Link from "next/link";
import { cn } from "@/lib/utils";

import { SignupCard } from "@/components/content/signup-card";
// ---------------------------------------------------------------------------
// MDX element -> brand-token component map.
// All prose elements receive the correct Impeccable token classes so every
// blog post and guide renders consistently without per-file styling.
// ---------------------------------------------------------------------------

export function useMDXComponents(components: MDXComponents): MDXComponents {
  return {
    // Authored components, usable directly inside .mdx content.
    SignupCard,
    // Headings
    h1: ({ className, ...props }) => (
      <h1
        className={cn(
          "mt-8 scroll-mt-20 text-3xl font-semibold tracking-tight text-foreground sm:text-4xl",
          className,
        )}
        {...props}
      />
    ),
    h2: ({ className, ...props }) => (
      <h2
        className={cn(
          "mt-8 scroll-mt-20 text-2xl font-semibold tracking-tight text-foreground",
          className,
        )}
        {...props}
      />
    ),
    h3: ({ className, ...props }) => (
      <h3
        className={cn(
          "mt-6 scroll-mt-20 text-xl font-semibold text-foreground",
          className,
        )}
        {...props}
      />
    ),
    h4: ({ className, ...props }) => (
      <h4
        className={cn(
          "mt-5 scroll-mt-20 text-lg font-semibold text-foreground",
          className,
        )}
        {...props}
      />
    ),

    // Body text
    p: ({ className, ...props }) => (
      <p
        className={cn(
          "mt-4 leading-7 text-[var(--muted-foreground)] max-w-[72ch]",
          className,
        )}
        {...props}
      />
    ),

    // Links via next/link for internal navigation
    a: ({ href = "", className, children, ...props }) => {
      const isInternal = href.startsWith("/") || href.startsWith("#");
      if (isInternal) {
        return (
          <Link
            href={href}
            className={cn(
              "font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity",
              className,
            )}
            {...props}
          >
            {children}
          </Link>
        );
      }
      return (
        <a
          href={href}
          target="_blank"
          rel="noreferrer noopener"
          className={cn(
            "font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity",
            className,
          )}
          {...props}
        >
          {children}
        </a>
      );
    },

    // Lists
    ul: ({ className, ...props }) => (
      <ul
        className={cn("mt-4 list-disc pl-6 space-y-2 text-[var(--muted-foreground)]", className)}
        {...props}
      />
    ),
    ol: ({ className, ...props }) => (
      <ol
        className={cn("mt-4 list-decimal pl-6 space-y-2 text-[var(--muted-foreground)]", className)}
        {...props}
      />
    ),
    li: ({ className, ...props }) => (
      <li className={cn("leading-7", className)} {...props} />
    ),

    // Code
    code: ({ className, ...props }) => (
      <code
        className={cn(
          "rounded-md border border-[var(--border)] bg-[var(--muted)] px-1.5 py-0.5 text-sm font-mono text-foreground",
          className,
        )}
        {...props}
      />
    ),
    pre: ({ className, ...props }) => (
      <pre
        className={cn(
          "mt-4 overflow-x-auto rounded-xl border border-[var(--border)] bg-[var(--muted)] p-4 text-sm font-mono leading-relaxed",
          className,
        )}
        {...props}
      />
    ),

    // Block quote - styled as a callout
    blockquote: ({ className, ...props }) => (
      <blockquote
        className={cn(
          "mt-4 border-l-4 border-[var(--primary)] pl-4 italic text-[var(--muted-foreground)]",
          className,
        )}
        {...props}
      />
    ),

    // Horizontal rule
    hr: ({ ...props }) => (
      <hr className="my-8 border-[var(--border)]" {...props} />
    ),

    // Table
    table: ({ className, ...props }) => (
      <div className="mt-4 overflow-x-auto rounded-xl border border-[var(--border)]">
        <table
          className={cn("w-full text-sm text-[var(--muted-foreground)]", className)}
          {...props}
        />
      </div>
    ),
    thead: ({ className, ...props }) => (
      <thead
        className={cn("border-b border-[var(--border)] bg-[var(--muted)]/40", className)}
        {...props}
      />
    ),
    th: ({ className, ...props }) => (
      <th
        className={cn("px-4 py-3 text-left font-semibold text-foreground", className)}
        {...props}
      />
    ),
    td: ({ className, ...props }) => (
      <td
        className={cn("border-t border-[var(--border)] px-4 py-3", className)}
        {...props}
      />
    ),

    // Strong / em
    strong: ({ className, ...props }) => (
      <strong className={cn("font-semibold text-foreground", className)} {...props} />
    ),
    em: ({ className, ...props }) => (
      <em className={cn("italic", className)} {...props} />
    ),

    // Spread any caller-provided overrides last
    ...components,
  };
}
