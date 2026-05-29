import { NoSitesEmpty, type NoSitesEmptyProps } from "./no-sites-empty";
import { OnboardingWizard, type OnboardingWizardProps } from "./onboarding-wizard";
import { useOnboardingState } from "./use-onboarding-state";

// Surface 4.12 — adapter that picks between the first-site OnboardingWizard
// and the steady-state NoSitesEmpty based on persisted onboarding state.
//
// Sites/index.tsx (Sprint 3, locked) renders an inline "no sites yet" block
// at the moment. A follow-up swap will replace that block with:
//
//   {sites.length === 0 ? <SitesPageEmpty cta={...} /> : null}
//
// Keeping the swap as a single line on the route side preserves the Sprint 3
// lock — Sprint 4 doesn't have to edit sites/index.tsx for this surface to
// land. The `cta` prop is forwarded to NoSitesEmpty so the route can pass an
// inert placeholder for read-only operators (same pattern as the existing
// AddSitePlaceholder).

export interface SitesPageEmptyProps {
  /** Optional CTA override forwarded to NoSitesEmpty (read-only operators). */
  cta?: NoSitesEmptyProps["cta"];
  /** Optional handoff hook forwarded to OnboardingWizard. */
  onOnboardingHandoff?: OnboardingWizardProps["onHandoff"];
}

export function SitesPageEmpty({
  cta,
  onOnboardingHandoff,
}: SitesPageEmptyProps = {}) {
  const { isOnboarding } = useOnboardingState();
  if (isOnboarding) {
    return <OnboardingWizard onHandoff={onOnboardingHandoff} />;
  }
  return <NoSitesEmpty cta={cta} />;
}
