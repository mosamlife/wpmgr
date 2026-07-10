import { createFileRoute, redirect } from "@tanstack/react-router";
import { z } from "zod";

// Compatibility redirect. POST /billing/checkout's Stripe success/cancel
// return URLs are built server-side as `{publicBaseURL}/billing?checkout=...`
// (apps/api/internal/billing/handler.go's `createCheckout`) — one path
// segment shorter than this app's actual route (`/settings/billing`). Every
// Stripe checkout return, whether started from /settings/billing or
// /welcome/checkout, lands here first; forward it straight to
// /settings/billing with the same `checkout` hint so the existing
// finalizing/success banner (`useBillingCheckoutReturn`) still fires. Purely
// a frontend routing fix — never touches the backend contract.
const searchSchema = z.object({
  checkout: z.enum(["success", "cancel"]).optional().catch(undefined),
});

export const Route = createFileRoute("/_authed/billing")({
  validateSearch: searchSchema,
  beforeLoad: ({ search }) => {
    throw redirect({
      to: "/settings/billing",
      search: search.checkout ? { checkout: search.checkout } : {},
      replace: true,
    });
  },
});
