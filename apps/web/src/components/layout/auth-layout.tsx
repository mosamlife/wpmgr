import { FleetHubLogo, Wordmark } from "@/components/brand/logo";
import { FleetIllustration } from "@/components/brand/fleet-illustration";

/**
 * Shared wrapper for every unauthenticated page: login, register,
 * forgot-password, reset-password, verify-email, and the 2FA challenge.
 *
 * SPLIT SCREEN, FORM FIRST. Below `lg` the brand panel is not rendered at all
 * and the page is exactly what it was before: a centred column with the form.
 * That ordering is deliberate. On a phone, a visitor who has come here to sign
 * in should not have to scroll past a value proposition to reach the field
 * they came for, and a panel that merely stacks above the form does precisely
 * that. It is additive on large screens and absent on small ones.
 *
 * WHY A PANEL AT ALL. These pages were a form on an empty background, which is
 * the correct amount of design for a login and too little for a signup: the
 * moment someone is deciding whether to create an account is the last moment
 * we can say what the product does. The panel carries that, plus the proof
 * points that answer the two objections a developer has at signup (can I leave,
 * and what does it cost).
 *
 * The panel is decorative in the accessibility sense. It contains no
 * instructions, no controls, and nothing a screen reader user needs in order
 * to complete the form, so a visitor who never receives it loses nothing
 * functional.
 */
export function AuthLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-dvh bg-[var(--color-background)]">
      <AuthAside />

      {/* Form column. Owns the full width until lg, where it becomes the right
          half. `min-w-0` so a long error message cannot push the column wider
          than its track. */}
      <main className="flex min-w-0 flex-1 flex-col items-center justify-center gap-6 p-4 sm:p-6">
        {/* The lockup repeats here below lg, where the panel that would
            otherwise carry it is not rendered. */}
        <div className="flex items-center gap-2.5 lg:hidden">
          <FleetHubLogo size={26} />
          <Wordmark className="text-base" />
        </div>
        {children}
      </main>
    </div>
  );
}

const PROOF = [
  "Connect a site in about a minute",
  "Backups encrypted before they leave the site",
  "Open source, and self-hostable for free",
];

function AuthAside() {
  return (
    <aside
      className="relative hidden w-[46%] max-w-[42rem] shrink-0 flex-col justify-between overflow-hidden border-r border-[var(--color-border)] bg-[var(--color-muted)]/40 p-10 lg:flex xl:p-14"
      aria-hidden="true"
    >
      <div className="flex items-center gap-2.5">
        <FleetHubLogo size={28} />
        <Wordmark className="text-lg" />
      </div>

      <div className="flex flex-col items-center">
        <FleetIllustration className="h-auto w-full max-w-[24rem]" />
      </div>

      <div>
        <h2 className="max-w-[20ch] text-2xl font-semibold tracking-tight text-[var(--color-foreground)] xl:text-3xl">
          Every WordPress site you run, on one screen.
        </h2>
        <p className="mt-3 max-w-[46ch] text-sm leading-relaxed text-[var(--color-muted-foreground)]">
          Backups, updates, uptime, performance and security across the whole fleet, from a
          dashboard you can host yourself.
        </p>

        <ul className="mt-6 flex flex-col gap-2.5">
          {PROOF.map((item) => (
            <li
              key={item}
              className="flex items-start gap-2.5 text-sm text-[var(--color-muted-foreground)]"
            >
              <svg
                viewBox="0 0 16 16"
                className="mt-[3px] size-4 shrink-0 text-[var(--color-primary)]"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
              >
                <path d="M3 8.5 L6.5 12 L13 4.5" />
              </svg>
              {item}
            </li>
          ))}
        </ul>
      </div>
    </aside>
  );
}
