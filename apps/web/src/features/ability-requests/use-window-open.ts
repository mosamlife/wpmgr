import { useEffect, useState } from "react";

/**
 * True while a window's end is still ahead of the client clock. A missing end
 * is a window with no expiry. One timer runs to the expiry and flips it off;
 * there is no polling clock, and no timer at all unless `armed`.
 */
export function useWindowOpen(until: string | null | undefined, armed = true): boolean {
  const end = until ? Date.parse(until) : null;
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!armed || end === null || !Number.isFinite(end) || end <= now) return;
    // One timer to the expiry. setTimeout caps at a signed 32-bit delay, so a
    // longer wait re-arms itself because `now` is a dependency.
    const t = setTimeout(() => setNow(Date.now()), Math.min(Math.max(end - Date.now(), 0), 2_147_483_647));
    return () => clearTimeout(t);
  }, [armed, end, now]);
  if (end === null) return true;
  return Number.isFinite(end) && end > now;
}
