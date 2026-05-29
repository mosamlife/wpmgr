// useStatusPulse — extracted from status-dot.tsx so that the component file
// exports only components (react-refresh/only-export-components).
//
// Returns a monotonically-incrementing key that bumps every time `value`
// changes (after the first render). Pair with `motion`'s `animate` keyed off
// the value to fire a one-shot pulse on transition.
//
// Exposed for surfaces that compose their own indicator and don't want the
// `<StatusDot>` chrome — e.g. the run-detail page that paints a custom
// progress dot inline.

import { useEffect, useRef, useState } from "react";

/**
 * useStatusPulse — returns a monotonically-incrementing key that bumps every
 * time `value` changes (after the first render). Pair with `motion`'s
 * `animate` keyed off the value to fire a one-shot pulse on transition.
 *
 * Exposed for surfaces that compose their own indicator and don't want the
 * `<StatusDot>` chrome — e.g. the run-detail page that paints a custom
 * progress dot inline. Internal `StatusDot` consumers should just set
 * `pulseOnChange`.
 */
export function useStatusPulse<T>(value: T): number {
  const [key, setKey] = useState(0);
  const previous = useRef<T>(value);
  useEffect(() => {
    if (previous.current !== value) {
      previous.current = value;
      setKey((k) => k + 1);
    }
  }, [value]);
  return key;
}
