import { createFileRoute, useNavigate, Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  Loader2,
  MailCheck,
} from "lucide-react";

import { AuthLayout } from "@/components/layout/auth-layout";
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
import {
  useVerifyEmail,
  useResendVerification,
} from "@/features/auth/use-auth";
import { readPendingPlan } from "@/features/billing/pending-plan";

// Unauthenticated — NOT under _authed, no beforeLoad guard.
const searchSchema = z.object({
  token: z.string().optional(),
});

export const Route = createFileRoute("/verify-email")({
  validateSearch: searchSchema,
  component: VerifyEmailPage,
});

// ---------------------------------------------------------------------------
// Resend form — shared by the "no token" landing and the "expired token" state
// ---------------------------------------------------------------------------

const resendSchema = z.object({
  email: z.email("Enter a valid email address"),
});

type ResendValues = z.infer<typeof resendSchema>;

function ResendForm({ defaultEmail }: { defaultEmail?: string }) {
  const resendMutation = useResendVerification();
  const [sent, setSent] = useState(false);

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<ResendValues>({
    resolver: zodResolver(resendSchema),
    defaultValues: { email: defaultEmail ?? "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    await resendMutation.mutateAsync(values, {
      // Swallow errors — always show neutral confirmation.
      onError: () => {},
    });
    setSent(true);
  });

  if (sent) {
    return (
      <div className="flex items-start gap-2.5 rounded-md border border-[var(--color-primary)]/30 bg-[var(--color-card)] px-3 py-2.5">
        <CheckCircle2
          aria-hidden="true"
          className="mt-0.5 size-4 shrink-0 text-[var(--color-primary)]"
        />
        <p className="text-sm text-[var(--color-foreground)]">
          If an account exists for that address we've sent a new verification
          link. Check your inbox (and spam folder).
        </p>
      </div>
    );
  }

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="space-y-3">
      <div className="space-y-2">
        <Label htmlFor="resend-email">Email address</Label>
        <Input
          id="resend-email"
          type="email"
          autoComplete="email"
          aria-invalid={errors.email ? true : undefined}
          aria-describedby={errors.email ? "resend-email-error" : undefined}
          {...register("email")}
        />
        {errors.email ? (
          <p
            id="resend-email-error"
            role="alert"
            className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
          >
            <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
            {errors.email.message}
          </p>
        ) : null}
      </div>
      <Button
        type="submit"
        variant="outline"
        className="w-full"
        disabled={isSubmitting || resendMutation.isPending}
      >
        Resend verification email
      </Button>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Token-present path — auto-POST on mount, branch on result
// ---------------------------------------------------------------------------

function VerifyWithToken({ token }: { token: string }) {
  const navigate = useNavigate();
  const verifyMutation = useVerifyEmail();
  // Guard so StrictMode double-invoke doesn't fire two requests.
  const firedRef = useRef(false);

  useEffect(() => {
    if (firedRef.current) return;
    firedRef.current = true;

    verifyMutation.mutate(
      { token },
      {
        onSuccess: (result) => {
          if (result.status === 200) {
            // Session is now live (me is in the query cache). A hosted
            // instance where this account captured a paid-plan intent at
            // signup (Me.desired_plan, single-use — see use-auth.ts) skips
            // straight to checkout instead of an empty Sites page. The
            // local same-browser stash only ever supplies a `?currency=`
            // hint here; `desired_plan`+`hosted` alone decide whether to go.
            const me = result.me;
            if (me?.desired_plan && me.hosted) {
              const stash = readPendingPlan();
              void navigate({
                to: "/welcome/checkout",
                search: { plan: me.desired_plan, currency: stash?.currency },
              });
            } else {
              void navigate({ to: "/sites" });
            }
          }
          // 410/429 handled by rendering below.
        },
        onError: () => {
          // Unexpected transport error — mutation.isError handles render.
        },
      },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  // Pending / in-flight state.
  if (verifyMutation.isPending || (!verifyMutation.isSuccess && !verifyMutation.isError)) {
    return (
      <AuthLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Verifying your email</h1>
            </CardTitle>
            <CardDescription>Just a moment…</CardDescription>
          </CardHeader>
          <CardContent className="flex justify-center py-4">
            <Loader2 aria-hidden="true" className="size-6 animate-spin text-[var(--color-muted-foreground)]" />
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  // Unexpected transport/server error.
  if (verifyMutation.isError) {
    return (
      <AuthLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Verification failed</h1>
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div
              role="alert"
              className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
            >
              <AlertTriangle
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
              />
              <p className="text-sm text-[var(--color-destructive)]">
                {verifyMutation.error.message}
              </p>
            </div>
            <ResendForm />
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  const result = verifyMutation.data;

  // 429 — rate limited.
  if (result.status === 429) {
    return (
      <AuthLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Too many attempts</h1>
            </CardTitle>
            <CardDescription>
              Please wait a few minutes before trying again.
            </CardDescription>
          </CardHeader>
        </Card>
      </AuthLayout>
    );
  }

  // 410 — invalid or expired token.
  if (result.status === 410) {
    return (
      <AuthLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Link expired</h1>
            </CardTitle>
            <CardDescription>
              This verification link is invalid or has expired. Enter your email
              below to receive a fresh one.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <ResendForm />
          </CardContent>
        </Card>
      </AuthLayout>
    );
  }

  // 200 — success: navigate fires in onSuccess; render a brief success card in
  // case the navigate hasn't resolved yet.
  return (
    <AuthLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <div className="flex items-center gap-2">
            <CheckCircle2
              aria-hidden="true"
              className="size-5 shrink-0 text-[var(--color-primary)]"
            />
            <CardTitle asChild>
              <h1>Email verified</h1>
            </CardTitle>
          </div>
          <CardDescription>Taking you to your dashboard…</CardDescription>
        </CardHeader>
      </Card>
    </AuthLayout>
  );
}

// ---------------------------------------------------------------------------
// No-token path — "check your inbox" + resend form
// ---------------------------------------------------------------------------

function CheckEmailLanding() {
  return (
    <AuthLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <div className="flex items-center gap-2">
            <MailCheck
              aria-hidden="true"
              className="size-5 shrink-0 text-[var(--color-primary)]"
            />
            <CardTitle asChild>
              <h1>Check your email</h1>
            </CardTitle>
          </div>
          <CardDescription>
            We sent a verification link to the address you signed up with. Click
            it to activate your account.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Didn't receive it? Check your spam folder or request a new link
            below.
          </p>
          <ResendForm />
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

// ---------------------------------------------------------------------------
// Page root — branches on token presence
// ---------------------------------------------------------------------------

function VerifyEmailPage() {
  const search = Route.useSearch();
  const token = search.token;

  if (!token) {
    return <CheckEmailLanding />;
  }

  return <VerifyWithToken token={token} />;
}
