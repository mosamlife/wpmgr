import { createFileRoute, redirect, useNavigate, Link } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { AlertCircle, AlertTriangle, Globe } from "lucide-react";

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
import { ensureMe, useLogin } from "@/features/auth/use-auth";

const searchSchema = z.object({
  // Where to land after a successful login (defaults to /sites).
  redirect: z.string().optional(),
});

export const Route = createFileRoute("/login")({
  validateSearch: searchSchema,
  // If a valid session already exists, skip the login page.
  beforeLoad: async ({ context, search }) => {
    const me = await ensureMe(context.queryClient);
    if (me) {
      throw redirect({ to: search.redirect ?? "/sites" });
    }
  },
  component: LoginPage,
});

const loginSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(1, "Password is required"),
});

type LoginValues = z.infer<typeof loginSchema>;

function LoginPage() {
  const navigate = useNavigate();
  const search = Route.useSearch();
  const loginMutation = useLogin();

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    resolver: zodResolver(loginSchema),
    defaultValues: { email: "", password: "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    await loginMutation.mutateAsync(values, {
      onSuccess: () => {
        void navigate({ to: search.redirect ?? "/sites" });
      },
      // Swallow the rejection here; the error is rendered from mutation state.
      onError: () => {},
    });
  });

  // Begin OIDC login via a full-page redirect to the backend, which 302s to the
  // provider. If OIDC is unconfigured the backend returns 501; the user simply
  // lands on that response and can navigate back — we keep the button always
  // visible to avoid a config probe on the login screen.
  function signInWithSso() {
    window.location.href = "/api/auth/oidc/login";
  }

  const serverError = loginMutation.isError ? loginMutation.error.message : null;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-6 bg-[var(--color-background)] p-4">
      {/* WPMgr wordmark — same Globe + text treatment as the sidebar BrandStrip */}
      <div className="flex items-center gap-2">
        <Globe aria-hidden="true" className="size-5 text-[var(--color-primary)]" />
        <span className="text-sm font-semibold tracking-tight text-[var(--color-foreground)]">
          WPMgr
        </span>
      </div>

      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Sign in</h1>
          </CardTitle>
          <CardDescription>
            Use your email and password, or single sign-on.
          </CardDescription>
        </CardHeader>
        <CardContent>
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

            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                autoComplete="email"
                aria-invalid={errors.email ? true : undefined}
                aria-describedby={errors.email ? "email-error" : undefined}
                {...register("email")}
              />
              {errors.email ? (
                <p
                  id="email-error"
                  role="alert"
                  className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
                >
                  <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
                  {errors.email.message}
                </p>
              ) : null}
            </div>

            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                aria-invalid={errors.password ? true : undefined}
                aria-describedby={
                  errors.password ? "password-error" : undefined
                }
                {...register("password")}
              />
              {errors.password ? (
                <p
                  id="password-error"
                  role="alert"
                  className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
                >
                  <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
                  {errors.password.message}
                </p>
              ) : null}
            </div>

            <Button
              type="submit"
              className="w-full"
              disabled={isSubmitting || loginMutation.isPending}
            >
              Sign in
            </Button>
          </form>

          <div className="my-4 flex items-center gap-3 text-xs text-[var(--color-muted-foreground)]">
            <span className="h-px flex-1 bg-[var(--color-border)]" />
            <span>or</span>
            <span className="h-px flex-1 bg-[var(--color-border)]" />
          </div>

          <Button
            type="button"
            variant="outline"
            className="w-full"
            onClick={signInWithSso}
          >
            Sign in with SSO
          </Button>

          <p className="mt-4 text-center text-xs text-[var(--color-muted-foreground)]">
            First time here?{" "}
            <Link
              to="/register"
              className="text-[var(--color-foreground)] underline underline-offset-4"
            >
              Set up the first account
            </Link>
          </p>
        </CardContent>
      </Card>
    </main>
  );
}
