import { createFileRoute, redirect } from "@tanstack/react-router";
import { z } from "zod";

// Compatibility redirect. POST /billing/checkout's Stripe success/cancel
// return URLs are built server-side as
// `{publicBaseURL}/billing?checkout=success&session_id={CHECKOUT_SESSION_ID}`
// or `{publicBaseURL}/billing?checkout=cancel`
// (apps/api/internal/billing/handler.go's `createCheckout`) — one path
// segment shorter than this app's actual route (`/settings/billing`). Every
// Stripe checkout return, whether started from /settings/billing or
// /welcome/checkout, lands here first; forward it straight to
// /settings/billing with the same `checkout` and `session_id` so
// settings/billing.tsx's confirm-on-return effect (stripe-design-v9.md
// 5.10) still fires — dropping `session_id` here would silently skip the
// confirm call and fall back to the 30s poll alone. Purely a frontend
// routing fix — never touches the backend contract.
const searchSchema = z.object({
  checkout: z.enum(["success", "cancel"]).optional().catch(undefined),
  session_id: z.string().optional().catch(undefined),
});

export const Route = createFileRoute("/_authed/billing")({
  validateSearch: searchSchema,
  beforeLoad: ({ search }) => {
    throw redirect({
      to: "/settings/billing",
      search: {
        ...(search.checkout ? { checkout: search.checkout } : {}),
        ...(search.session_id ? { session_id: search.session_id } : {}),
      },
      replace: true,
    });
  },
});
