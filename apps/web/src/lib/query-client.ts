import { QueryClient } from "@tanstack/react-query";

// Single shared QueryClient. Server state lives here (TanStack Query), never in
// Zustand (ADR: do not put server state in client state).
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});
