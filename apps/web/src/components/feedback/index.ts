// Phase 6 (harden) — feedback primitives used by every page that fetches
// data. They are presentational: props in, JSX out. No data fetching, no
// route knowledge. Composed by routes (PageError) and by the AppShell
// (OfflineBanner).

export { PageError } from "./page-error";
export type { PageErrorProps } from "./page-error";

export { OfflineBanner } from "./offline-banner";
export type { OfflineBannerProps } from "./offline-banner";

export { PlannedFeature } from "./planned-feature";
export type { PlannedFeatureProps } from "./planned-feature";
