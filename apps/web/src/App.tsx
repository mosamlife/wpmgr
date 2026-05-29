import { useEffect } from "react";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClientProvider } from "@tanstack/react-query";

import { router } from "./router";
import { queryClient } from "@/lib/query-client";
import { configureApiClient } from "@/lib/api";
import { applyTheme, useThemeStore } from "@/lib/theme-store";
import { Toaster } from "@/components/toast";

// Configure the generated API client once at module load (baseUrl -> /api).
configureApiClient();

export function App() {
  const theme = useThemeStore((s) => s.theme);

  // Keep the <html> `.dark` class in sync with the persisted theme.
  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      {/* Surface 4.15 — Toaster mounts here (above the router so it floats
          over every authed and unauthed surface). Position, theme, and the
          verb-action chrome live inside the wrapper; this site is just the
          mount point. */}
      <Toaster />
    </QueryClientProvider>
  );
}
