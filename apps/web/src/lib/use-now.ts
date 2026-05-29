// useNow — pure-render clock hook.
//
// React Compiler (and react-hooks/purity) requires that render be a pure
// function of props + state. Calling Date.now() directly during render is an
// impure side-read: the value changes between invocations of the same render
// pass, which breaks referential consistency checks the compiler relies on.
//
// This hook exposes the current epoch ms as React state so renders read a
// stable snapshot value, and the clock advances via a setInterval inside
// useEffect (never during the render phase itself).
import { useEffect, useState } from "react";

export function useNow(intervalMs = 1000): number {
  // Lazy initializer: Date.now() runs once outside the render phase during
  // the first commit, not inside the render function itself.
  const [now, setNow] = useState<number>(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);

  return now;
}
