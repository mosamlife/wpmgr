import { create } from "zustand";
import { persist } from "zustand/middleware";

// Session stub (client/UI state only).
//
// WARNING: This is NOT real authentication. There is no auth endpoint yet —
// real auth (login, tokens, refresh) lands in Phase 5 / M1. For the skeleton
// we just store a fake session object so the route guard and logout flow can
// be wired and tested end to end.
export interface FakeSession {
  email: string;
  loggedInAt: number;
}

interface SessionState {
  session: FakeSession | null;
  signIn: (email: string) => void;
  signOut: () => void;
}

export const useSessionStore = create<SessionState>()(
  persist(
    (set) => ({
      session: null,
      signIn: (email) =>
        set({ session: { email, loggedInAt: Date.now() } }),
      signOut: () => set({ session: null }),
    }),
    { name: "wpmgr.session" },
  ),
);

/** Non-reactive read for use in router `beforeLoad` guards. */
export function getSession(): FakeSession | null {
  return useSessionStore.getState().session;
}
