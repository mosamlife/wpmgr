import { Container } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { Logo } from "@/components/ui/logo";
import { FOOTER_NAV, WORDPRESS_TRADEMARK_DISCLAIMER, SITE_CONFIG } from "@/lib/site";
// Derived here rather than in lib/site.ts on purpose: 14 feature files import
// signupHref from lib/site, so lib/site importing the registries would be a
// cycle. The footer is a leaf, so it can read them safely.
import { FEATURE_REGISTRY } from "@/lib/content/features";
import { SOLUTION_REGISTRY } from "@/lib/content/solutions";

export function SiteFooter() {
  return (
    <footer className="border-t border-[var(--border)] bg-[var(--muted)]/30 py-14">
      <Container>
        {/* Top grid: brand + nav mesh */}
        <div className="grid gap-10 sm:grid-cols-2 lg:grid-cols-5">
          {/* Brand column */}
          <div className="flex flex-col gap-4 lg:col-span-1">
            <a
              href="/"
              className="inline-flex items-center gap-2.5 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
              aria-label="WPMgr home"
            >
              <Logo />
            </a>
            <p className="text-sm leading-relaxed text-[var(--muted-foreground)] max-w-[220px]">
              Open-source, self-hostable WordPress fleet management you can run, read, and improve.
            </p>
            <a
              href={SITE_CONFIG.github}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-2 text-sm text-[var(--muted-foreground)] hover:text-foreground transition-colors duration-[var(--duration-fast)]"
            >
              <Icon name="Github" size={15} />
              GitHub
            </a>
          </div>

          {/* Nav columns */}
          {FOOTER_NAV.map((group) => (
            <div key={group.label} className="flex flex-col gap-3">
              <h3 className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">
                {group.label}
              </h3>
              <ul className="flex flex-col gap-2">
                {group.items.map((item) => (
                  <li key={item.href}>
                    <a
                      href={item.href}
                      {...(item.external ? { target: "_blank", rel: "noreferrer noopener" } : {})}
                      className="text-sm text-[var(--muted-foreground)] hover:text-foreground transition-colors duration-[var(--duration-fast)]"
                    >
                      {item.label}
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>

        {/* Full index of every feature and solution.

            The header mega-menu renders its panels through createPortal inside
            AnimatePresence, gated on a panel being open, so it contributes ZERO
            links to the served HTML. Measured: "features/file-manager" appeared
            on 3 pages of 55 and "solutions/hosting-providers" on 2. Neither is
            orphaned, since both index pages link everything, but ten feature
            pages were reachable only through one hub while four sat in the
            footer with a sitewide link each.

            This is a strip rather than more grid columns because 14 items in a
            single column unbalances a five-column footer, and it is derived
            from the registries rather than hand-listed so a new feature page
            cannot be added without appearing here. */}
        <nav aria-label="All features and solutions" className="mt-12 border-t border-[var(--border)] pt-8">
          <h3 className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">
            All features
          </h3>
          <ul className="mt-3 flex flex-wrap gap-x-5 gap-y-2">
            {Object.entries(FEATURE_REGISTRY).map(([slug, data]) => (
              <li key={slug}>
                <a
                  href={`/features/${slug}`}
                  className="text-sm text-[var(--muted-foreground)] hover:text-foreground transition-colors duration-[var(--duration-fast)]"
                >
                  {data.title}
                </a>
              </li>
            ))}
          </ul>

          <h3 className="mt-6 text-xs font-semibold uppercase tracking-[0.1em] text-foreground">
            All solutions
          </h3>
          <ul className="mt-3 flex flex-wrap gap-x-5 gap-y-2">
            {Object.entries(SOLUTION_REGISTRY).map(([slug, data]) => (
              <li key={slug}>
                <a
                  href={`/solutions/${slug}`}
                  className="text-sm text-[var(--muted-foreground)] hover:text-foreground transition-colors duration-[var(--duration-fast)]"
                >
                  {data.title}
                </a>
              </li>
            ))}
          </ul>
        </nav>

        {/* Bottom bar */}
        <div className="mt-12 flex flex-col gap-3 border-t border-[var(--border)] pt-8 text-xs text-[var(--muted-foreground)]">
          <p>
            Open source under AGPL-3.0 (control plane and dashboard) and MIT (WordPress agent). Contributions welcome.
          </p>
          <p>{WORDPRESS_TRADEMARK_DISCLAIMER}</p>
          <p className="mt-1">
            &copy; {new Date().getFullYear()} WPMgr contributors.
          </p>
        </div>
      </Container>
    </footer>
  );
}
