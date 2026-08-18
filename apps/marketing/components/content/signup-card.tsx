import { signupHref, newTabRel } from "@/lib/site";
import type { SignupSource } from "@/lib/analytics";
import { Icon } from "@/components/ui/icon";

/**
 * An in-article call to action, placed by the author part-way through a post.
 *
 * WHY THIS EXISTS. Below the `md` breakpoint the header signup button is
 * `hidden md:inline-flex`, so a reader arriving on a post from search has NO
 * signup link on screen until they reach the band at the very bottom. On a
 * 2,000 word article that is a long way to ask somebody to travel before the
 * page offers them anything. This is the template organic search will feed, so
 * the gap is not marginal.
 *
 * DELIBERATELY NOT A STICKY BAR OR A MODAL. Both cover content the reader came
 * for, and on a project whose whole argument is that you can read the source
 * before you trust it, interrupting the reading is the wrong trade. An inline
 * card is skippable, which is the point: it earns attention from position
 * rather than taking it.
 *
 * Usage inside .mdx, after the section that establishes the problem:
 *
 *   <SignupCard
 *     heading="Run this across every site at once"
 *     body="WPMgr applies the same hardening to a whole fleet from one screen."
 *   />
 */
export function SignupCard({
  heading = "Try this across your whole fleet",
  body = "WPMgr is open source and self-hostable, with a free hosted tier. No per-site fee.",
  cta = "Get started for free",
  source = "blog-inline",
}: {
  heading?: string;
  body?: string;
  cta?: string;
  /** Overrides the analytics source, so a card can be attributed per post. */
  source?: SignupSource;
}) {
  return (
    <aside className="my-10 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6 not-prose">
      <p className="text-base font-semibold text-foreground">{heading}</p>
      <p className="mt-2 text-sm leading-relaxed text-[var(--muted-foreground)]">{body}</p>
      <a
        href={signupHref(source)}
        target="_blank"
        rel={newTabRel(signupHref(source))}
        className="mt-4 inline-flex h-10 items-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
      >
        {cta}
        <Icon name="ArrowRight" size={16} aria-hidden />
      </a>
    </aside>
  );
}
