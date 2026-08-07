import { createFileRoute, redirect, useNavigate, Link } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { useState } from "react";
import { AlertCircle, AlertTriangle, CheckCircle2, Sparkles } from "lucide-react";

import { AuthLayout } from "@/components/layout/auth-layout";
import { SocialButtons } from "@/features/auth/social-buttons";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ensureMe, useRegister, useResendVerification } from "@/features/auth/use-auth";
import { planCatalogEntry } from "@/features/billing/plan-catalog";
import { stashPendingPlan } from "@/features/billing/pending-plan";
import type { BillingCurrency, CheckoutTierId } from "@/features/billing/use-billing";

// M16 Phase C2 — signup-to-premium: a plan chosen on the marketing pricing
// page is carried through `/register?plan=...&currency=...` (see
// registerSearchSchema below). Unknown/`free` values are silently ignored
// (`.catch(undefined)`, same idiom as admin/accounts/index.tsx's own note on
// why `z.enum()` needs a literal tuple) rather than erroring the whole route
// out — a stray or stale query param must never break the signup page.
const registerSearchSchema = z.object({
  plan: z.enum(["starter", "agency", "scale"]).optional().catch(undefined),
  currency: z.enum(["USD", "INR"]).optional().catch(undefined),
});

export const Route = createFileRoute("/register")({
  validateSearch: registerSearchSchema,
  // Already signed in? Nothing to bootstrap — go to the app.
  beforeLoad: async ({ context }) => {
    const me = await ensureMe(context.queryClient);
    if (me) {
      throw redirect({ to: "/sites" });
    }
  },
  component: RegisterPage,
});

// TWO FIELDS. The form previously also asked for a display name, an
// organization name, and an organization slug, all optional and all asked at
// the worst possible moment: before the person has seen anything work.
//
// None of them was load-bearing. The API already derives an organization name
// and a globally-unique slug from the email when they are absent (see
// apps/api/internal/auth/register.go), and every one of the three is editable
// in settings afterwards. So the fields bought nothing at signup and cost
// three extra decisions on the screen with the highest drop-off in the
// product. Anything not required to create the account is asked later.
const registerSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(12, "Use at least 12 characters"),
});

type RegisterValues = z.infer<typeof registerSchema>;

function RegisterPage() {
  const navigate = useNavigate();
  const search = Route.useSearch();
  const chosenPlan: CheckoutTierId | undefined = search.plan;
  const chosenCurrency: BillingCurrency | undefined = search.currency;
  const registerMutation = useRegister();
  const resendMutation = useResendVerification();
  // When the self-serve path succeeds we flip to a confirmation view showing
  // the email address that was registered.
  const [pendingEmail, setPendingEmail] = useState<string | null>(null);
  const [resendSent, setResendSent] = useState(false);

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<RegisterValues>({
    resolver: zodResolver(registerSchema),
    defaultValues: { email: "", password: "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    await registerMutation.mutateAsync(
      {
        email: values.email,
        password: values.password,
        plan: chosenPlan,
      },
      {
        onSuccess: (result) => {
          if (result.pending) {
            // Normal self-serve path: must verify email before logging in.
            // Stash the chosen plan as a same-browser fast path — the
            // canonical carrier is still the backend (persisted against the
            // verification token, surfaced back as Me.desired_plan).
            if (chosenPlan) {
              stashPendingPlan({ plan: chosenPlan, currency: chosenCurrency });
            }
            setPendingEmail(values.email);
          } else if (result.me?.desired_plan && result.me.hosted) {
            // First-account bootstrap path (session established immediately)
            // with a captured paid-plan intent on a hosted instance: skip
            // straight to checkout instead of landing on an empty Sites page.
            void navigate({
              to: "/welcome/checkout",
              search: { plan: result.me.desired_plan, currency: chosenCurrency },
            });
          } else {
            // First-account path, no paid intent (or self-hosted): go into the app.
            void navigate({ to: "/sites" });
          }
        },
        onError: () => {},
      },
    );
  });

  // --- Email-verification pending confirmation state ---
  if (pendingEmail !== null) {
    function handleResend() {
      void resendMutation.mutateAsync(
        { email: pendingEmail! },
        { onError: () => {}, onSuccess: () => setResendSent(true) },
      );
    }

    return (
      <AuthLayout>
        <Card className="w-full max-w-md">
          <CardHeader className="space-y-1">
            <div className="flex items-center gap-2">
              <CheckCircle2
                aria-hidden="true"
                className="size-5 shrink-0 text-[var(--color-primary)]"
              />
              <CardTitle asChild>
                <h1>Check your email</h1>
              </CardTitle>
            </div>
            <CardDescription>
              We sent a verification link to{" "}
              <span className="font-medium text-[var(--color-foreground)]">
                {pendingEmail}
              </span>
              . Click it to activate your account and sign in.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Didn't receive it? Check your spam folder or{" "}
              {resendSent ? (
                <span className="text-[var(--color-foreground)]">
                  we've resent the link.
                </span>
              ) : (
                <button
                  type="button"
                  className="text-[var(--color-foreground)] underline underline-offset-4 disabled:opacity-50"
                  disabled={resendMutation.isPending}
                  onClick={handleResend}
                >
                  resend the verification email.
                </button>
              )}
            </p>
            <p className="text-center text-xs text-[var(--color-muted-foreground)]">
              Already verified?{" "}
              <Link
                to="/login"
                className="text-[var(--color-foreground)] underline underline-offset-4"
              >
                Sign in
              </Link>
            </p>
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  // --- Registration form ---

  const serverError = registerMutation.isError
    ? registerMutation.error.message
    : null;

  return (
    <AuthLayout>
      <Card className="w-full max-w-md">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Create an account</h1>
          </CardTitle>
          <CardDescription>
            Sign up for WPMgr. We'll send you a verification email to activate
            your account.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {chosenPlan ? <PlanChip plan={chosenPlan} /> : null}

          <form
            onSubmit={(e) => void onSubmit(e)}
            noValidate
            className="space-y-4"
          >
            {serverError ? (
              <div
                role="alert"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
              >
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
                />
                <p className="text-sm text-[var(--color-destructive)]">
                  {serverError}
                </p>
              </div>
            ) : null}

            <Field
              id="email"
              label="Email"
              type="email"
              autoComplete="email"
              register={register}
              error={errors.email?.message}
            />
            <Field
              id="password"
              label="Password"
              type="password"
              autoComplete="new-password"
              register={register}
              error={errors.password?.message}
            />

            <Button
              type="submit"
              className="w-full"
              disabled={isSubmitting || registerMutation.isPending}
            >
              Create account
            </Button>
          </form>

          {/* Signing up with a provider skips the verification email entirely:
              the provider has already proven the address, so the account is
              active immediately. It also supplies a display name, which is why
              the form no longer asks for one. */}
          <SocialButtons label="Sign up with" />

          <p className="mt-4 text-center text-xs text-[var(--color-muted-foreground)]">
            Already have an account?{" "}
            <Link
              to="/login"
              className="text-[var(--color-foreground)] underline underline-offset-4"
            >
              Sign in
            </Link>
          </p>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}

/**
 * Small summary chip shown above the form when the signup URL carries a
 * `?plan=` hint (e.g. from the marketing pricing page). Purely informational
 * — the actual plan is threaded through the register call and, on a hosted
 * instance, resolved server-side; this never blocks a free signup.
 */
function PlanChip({ plan }: { plan: CheckoutTierId }) {
  const entry = planCatalogEntry(plan);
  if (!entry) return null;
  return (
    <div className="mb-4 flex items-center gap-2 rounded-full border border-[var(--color-primary)]/30 bg-[var(--color-primary)]/10 px-3 py-1.5 text-xs font-medium text-[var(--color-primary)]">
      <Sparkles aria-hidden="true" className="size-3.5 shrink-0" />
      You&apos;re signing up for the {entry.name} plan
      <span className="text-[var(--color-primary)]/70">
        &middot; {entry.priceLabel}
      </span>
    </div>
  );
}

function Field({
  id,
  label,
  type = "text",
  autoComplete,
  register,
  error,
}: {
  id: keyof RegisterValues;
  label: string;
  type?: string;
  autoComplete?: string;
  register: ReturnType<typeof useForm<RegisterValues>>["register"];
  error?: string;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type={type}
        autoComplete={autoComplete}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : undefined}
        {...register(id)}
      />
      {error ? (
        <p
          id={`${id}-error`}
          role="alert"
          className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
        >
          <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
          {error}
        </p>
      ) : null}
    </div>
  );
}
