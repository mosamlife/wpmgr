// Shared status -> icon/label/color-class maps for uptime status kinds
// (up/degraded/down/unknown). Extracted from the /uptime route so both the
// page itself and the incident detail dialog render the exact same
// triple-encoded (colour + label + icon) status language (GH #148) for
// accessibility, without the dialog importing across the router's
// auto-code-split route boundary (route files are their own chunk; a
// feature module importing from one would create an import cycle since the
// route also imports the feature to mount it).

import {
  CheckCircle2,
  AlertTriangle,
  XCircle,
  HelpCircle,
} from "lucide-react";

import type { UptimeStatusKind } from "./fleet-types";

export const STATUS_ICON: Record<UptimeStatusKind, typeof CheckCircle2> = {
  up: CheckCircle2,
  degraded: AlertTriangle,
  down: XCircle,
  unknown: HelpCircle,
};

export const STATUS_LABEL: Record<UptimeStatusKind, string> = {
  up: "Up",
  degraded: "Degraded",
  down: "Down",
  unknown: "Unknown",
};

export const STATUS_COLOR_CLASS: Record<UptimeStatusKind, string> = {
  up: "text-[var(--color-success-subtle-fg)]",
  degraded: "text-[var(--color-warning-subtle-fg)]",
  down: "text-[var(--color-destructive-subtle-fg)]",
  unknown: "text-[var(--color-muted-foreground)]",
};
