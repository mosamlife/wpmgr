import { createFileRoute, redirect } from "@tanstack/react-router";

// `/` simply redirects to the sites list (the protected `_authed` layout will
// in turn bounce to /login if there's no session).
export const Route = createFileRoute("/")({
  beforeLoad: () => {
    throw redirect({ to: "/sites" });
  },
});
