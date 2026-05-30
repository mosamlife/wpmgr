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
import { ensureMe, useRegister } from "@/features/auth/use-auth";

export const Route = createFileRoute("/register")({
  // Already signed in? Nothing to bootstrap — go to the app.
  beforeLoad: async ({ context }) => {
    const me = await ensureMe(context.queryClient);
    if (me) {
      throw redirect({ to: "/sites" });
    }
  },
  component: RegisterPage,
});

// First-run bootstrap. The backend accepts this only when zero users exist
// (otherwise 403); it creates the first user + tenant + owner membership and
// establishes the session.
const registerSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(12, "Use at least 12 characters"),
  name: z.string().max(200).optional(),
  tenant_name: z.string().max(200).optional(),
  tenant_slug: z
    .string()
    .regex(/^[a-z0-9]+(?:-[a-z0-9]+)*$/, "Lowercase letters, numbers, and dashes")
    .max(64)
    .optional()
    .or(z.literal("")),
});

type RegisterValues = z.infer<typeof registerSchema>;

function RegisterPage() {
  const navigate = useNavigate();
  const registerMutation = useRegister();

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<RegisterValues>({
    resolver: zodResolver(registerSchema),
    defaultValues: { email: "", password: "", name: "", tenant_name: "", tenant_slug: "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    await registerMutation.mutateAsync(
      {
        email: values.email,
        password: values.password,
        name: values.name || undefined,
        tenant_name: values.tenant_name || undefined,
        tenant_slug: values.tenant_slug || undefined,
      },
      {
        onSuccess: () => void navigate({ to: "/sites" }),
        onError: () => {},
      },
    );
  });

  const serverError = registerMutation.isError
    ? registerMutation.error.message
    : null;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-6 bg-[var(--color-background)] p-4">
      {/* WPMgr wordmark — same Globe + text treatment as the sidebar BrandStrip */}
      <div className="flex items-center gap-2">
        <Globe aria-hidden="true" className="size-5 text-[var(--color-primary)]" />
        <span className="text-sm font-semibold tracking-tight text-[var(--color-foreground)]">
          WPMgr
        </span>
      </div>

      <Card className="w-full max-w-md">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Set up WPMgr</h1>
          </CardTitle>
          <CardDescription>
            Create the first owner account and tenant. This is only available on
            a fresh install.
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
            <Field
              id="name"
              label="Your name (optional)"
              register={register}
              error={errors.name?.message}
            />
            <Field
              id="tenant_name"
              label="Tenant name (optional)"
              register={register}
              error={errors.tenant_name?.message}
            />
            <Field
              id="tenant_slug"
              label="Tenant slug (optional)"
              register={register}
              error={errors.tenant_slug?.message}
            />

            <Button
              type="submit"
              className="w-full"
              disabled={isSubmitting || registerMutation.isPending}
            >
              Create account
            </Button>
          </form>

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
    </main>
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
