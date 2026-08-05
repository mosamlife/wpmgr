// useDebouncedValue. A trailing-edge debounce for a value that drives a query.
//
// GH #349. The Sites search input and the command palette both write the typed
// term SYNCHRONOUSLY (to the URL and to local state respectively) so typing
// never feels laggy, then feed a debounced MIRROR of that term to the server.
// Without the split, every keystroke would be its own request; without the
// synchronous write, the input would feel like it drops characters.
//
// The first value is adopted immediately, with no leading delay, so a shared
// link such as /sites?q=acme fetches on the first paint instead of a beat
// later.
//
// setState happens inside the timer callback, never synchronously in the
// effect body, matching lib/use-now.ts and the react-hooks purity rules.
import { useEffect, useState } from "react";

export function useDebouncedValue<T>(value: T, delayMs = 250): T {
  const [debounced, setDebounced] = useState<T>(value);

  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);

  return debounced;
}
