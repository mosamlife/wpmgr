import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Select } from "@/components/ui/select";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { toast } from "@/components/toast";
import { useMe, canManageInstanceEmail } from "@/features/auth/use-auth";
import {
  useSmtp,
  usePutSmtp,
  useTestSmtp,
  type SmtpSettings,
  type PutSmtpBody,
} from "@/features/settings/use-smtp";

export const Route = createFileRoute("/_authed/settings/smtp")({
  component: SmtpSettingsPage,
});

// ---------------------------------------------------------------------------
// Validation schemas
// ---------------------------------------------------------------------------

const smtpSchema = z.object({
  enabled: z.boolean(),
  host: z.string().min(1, "Host is required"),
  port: z.coerce
    .number()
    .int("Port must be a whole number")
    .min(1, "Port must be at least 1")
    .max(65535, "Port must be 65535 or below"),
  username: z.string(),
  password: z.string().optional(),
  from_address: z.string().email("Must be a valid email address"),
  from_name: z.string().min(1, "From name is required"),
  tls_mode: z.enum(["starttls", "tls", "none"]),
  allow_insecure_tls: z.boolean(),
});

type SmtpFormValues = z.infer<typeof smtpSchema>;

const testSchema = z.object({
  to_address: z.string().email("Must be a valid email address"),
});

// ---------------------------------------------------------------------------
// Page shell
// ---------------------------------------------------------------------------

function SmtpSettingsPage() {
  const { data: me } = useMe();
  const capable = canManageInstanceEmail(me);

  const {
    data: smtp,
    isPending,
    isError,
    error,
    refetch,
  } = useSmtp({ enabled: capable });

  // Only an instance administrator (a superadmin, or the owner of the
  // install's only organisation) may reach this page at all — the server
  // gate covers GET as well as PUT/POST, so there is no read-only view for
  // anyone else. Say so plainly rather than firing a request that can only
  // 403 and rendering the generic load-failure state with a Retry that can
  // never succeed.
  if (!capable) {
    return (
      <section
        aria-labelledby="smtp-heading"
        className="max-w-2xl space-y-6"
      >
        <PageHeader
          title="Email / SMTP"
          subline="Configure the outgoing mail relay for password-reset and notification emails."
        />
        <p
          role="alert"
          className="rounded-xl border border-[var(--color-border)] p-4 text-sm text-[var(--color-muted-foreground)]"
        >
          Only the instance administrator can change these settings. Ask
          your instance administrator to make changes.
        </p>
      </section>
    );
  }

  if (isPending) {
    return (
      <section
        aria-labelledby="smtp-heading"
        className="max-w-2xl space-y-6"
      >
        <PageHeader
          title="Email / SMTP"
          subline="Configure the outgoing mail relay for password-reset and notification emails."
        />
        <div
          role="status"
          aria-label="Loading SMTP settings"
          className="space-y-4"
        >
          <div className="h-96 animate-pulse rounded-xl bg-muted/50" />
          <div className="h-44 animate-pulse rounded-xl bg-muted/50" />
        </div>
      </section>
    );
  }

  if (isError || !smtp) {
    return (
      <section
        aria-labelledby="smtp-heading"
        className="max-w-2xl space-y-6"
      >
        <PageHeader
          title="Email / SMTP"
          subline="Configure the outgoing mail relay for password-reset and notification emails."
        />
        <PageError
          what="Could not load SMTP settings."
          why={error?.message}
          onRetry={() => void refetch()}
          retryLabel="Reload SMTP settings"
        />
      </section>
    );
  }

  return (
    <section
      aria-labelledby="smtp-heading"
      className="max-w-2xl space-y-6"
    >
      <PageHeader
        title="Email / SMTP"
        subline="Configure the outgoing mail relay for password-reset and notification emails."
      />

      <SmtpConfigCard key={smtp.updated_at} smtp={smtp} readOnly={false} />
      <TestEmailCard disabled={!smtp.enabled} />
      <DeliverabilityCard />
    </section>
  );
}

// ---------------------------------------------------------------------------
// SMTP configuration card
// ---------------------------------------------------------------------------

function SmtpConfigCard({
  smtp,
  readOnly,
}: {
  smtp: SmtpSettings;
  readOnly: boolean;
}) {
  const putSmtp = usePutSmtp();

  // Mirror the server state locally so the form is controlled.
  const [enabled, setEnabled] = useState(smtp.enabled);
  const [host, setHost] = useState(smtp.host);
  const [port, setPort] = useState(String(smtp.port || 587));
  const [username, setUsername] = useState(smtp.username);
  // Password is write-only: never pre-populated.
  const [password, setPassword] = useState("");
  const [fromAddress, setFromAddress] = useState(smtp.from_address);
  const [fromName, setFromName] = useState(smtp.from_name);
  const [tlsMode, setTlsMode] = useState<"starttls" | "tls" | "none">(
    smtp.tls_mode,
  );
  const [allowInsecureTls, setAllowInsecureTls] = useState(
    smtp.allow_insecure_tls,
  );
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  function handleSave() {
    const raw: SmtpFormValues = {
      enabled,
      host,
      port: Number(port),
      username,
      password: password || undefined,
      from_address: fromAddress,
      from_name: fromName,
      tls_mode: tlsMode,
      allow_insecure_tls: allowInsecureTls,
    };

    const result = smtpSchema.safeParse(raw);
    if (!result.success) {
      const errs: Record<string, string> = {};
      for (const issue of result.error.issues) {
        const field = issue.path[0];
        if (typeof field === "string" && !errs[field]) {
          errs[field] = issue.message;
        }
      }
      setFieldErrors(errs);
      return;
    }

    setFieldErrors({});

    const body: PutSmtpBody = {
      enabled: result.data.enabled,
      host: result.data.host,
      port: result.data.port,
      username: result.data.username,
      from_address: result.data.from_address,
      from_name: result.data.from_name,
      tls_mode: result.data.tls_mode,
      allow_insecure_tls: result.data.allow_insecure_tls,
    };
    // Only include password when the user actually typed one.
    if (result.data.password) {
      body.password = result.data.password;
    }

    putSmtp.mutate(body, { onError: () => {} });
  }

  const disabled = readOnly || putSmtp.isPending;

  return (
    <Card>
      <CardHeader>
        <CardTitle>SMTP relay</CardTitle>
        <CardDescription>
          Outgoing email is used for password-reset links and alert
          notifications. Leave disabled to suppress all outbound email.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        {/* Enabled toggle */}
        <div className="flex items-center gap-3">
          <Switch
            id="smtp-enabled"
            checked={enabled}
            onCheckedChange={setEnabled}
            disabled={disabled}
            aria-label="Enable SMTP"
          />
          <Label htmlFor="smtp-enabled">Enable outgoing email</Label>
        </div>

        {/* Host */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-host">SMTP host</Label>
          <Input
            id="smtp-host"
            value={host}
            onChange={(e) => setHost(e.target.value)}
            disabled={disabled}
            placeholder="smtp.example.com"
            autoComplete="off"
            aria-invalid={fieldErrors["host"] ? true : undefined}
            aria-describedby={fieldErrors["host"] ? "smtp-host-error" : undefined}
            className="max-w-sm"
          />
          {fieldErrors["host"] ? (
            <p
              id="smtp-host-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["host"]}
            </p>
          ) : null}
        </div>

        {/* Port */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-port">Port</Label>
          <Input
            id="smtp-port"
            type="number"
            value={port}
            onChange={(e) => setPort(e.target.value)}
            disabled={disabled}
            placeholder="587"
            min={1}
            max={65535}
            aria-invalid={fieldErrors["port"] ? true : undefined}
            aria-describedby={fieldErrors["port"] ? "smtp-port-error" : undefined}
            className="max-w-[120px]"
          />
          {fieldErrors["port"] ? (
            <p
              id="smtp-port-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["port"]}
            </p>
          ) : null}
        </div>

        {/* Username */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-username">Username</Label>
          <Input
            id="smtp-username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            disabled={disabled}
            autoComplete="username"
            aria-invalid={fieldErrors["username"] ? true : undefined}
            aria-describedby={
              fieldErrors["username"] ? "smtp-username-error" : undefined
            }
            className="max-w-sm"
          />
          {fieldErrors["username"] ? (
            <p
              id="smtp-username-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["username"]}
            </p>
          ) : null}
        </div>

        {/* Password */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-password">Password</Label>
          <Input
            id="smtp-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={disabled}
            autoComplete="new-password"
            placeholder={smtp.password_set ? "••••••••" : undefined}
            aria-invalid={fieldErrors["password"] ? true : undefined}
            aria-describedby="smtp-password-hint"
            className="max-w-sm"
          />
          <p
            id="smtp-password-hint"
            className="text-xs text-muted-foreground"
          >
            {smtp.password_set
              ? "Leave blank to keep the current password."
              : "Set a password for SMTP authentication."}
          </p>
          {fieldErrors["password"] ? (
            <p
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["password"]}
            </p>
          ) : null}
        </div>

        {/* From address */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-from-address">From address</Label>
          <Input
            id="smtp-from-address"
            type="email"
            value={fromAddress}
            onChange={(e) => setFromAddress(e.target.value)}
            disabled={disabled}
            placeholder="noreply@example.com"
            autoComplete="off"
            aria-invalid={fieldErrors["from_address"] ? true : undefined}
            aria-describedby={
              fieldErrors["from_address"]
                ? "smtp-from-address-error"
                : undefined
            }
            className="max-w-sm"
          />
          {fieldErrors["from_address"] ? (
            <p
              id="smtp-from-address-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["from_address"]}
            </p>
          ) : null}
        </div>

        {/* From name */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-from-name">From name</Label>
          <Input
            id="smtp-from-name"
            value={fromName}
            onChange={(e) => setFromName(e.target.value)}
            disabled={disabled}
            placeholder="WPMgr Notifications"
            autoComplete="off"
            aria-invalid={fieldErrors["from_name"] ? true : undefined}
            aria-describedby={
              fieldErrors["from_name"] ? "smtp-from-name-error" : undefined
            }
            className="max-w-sm"
          />
          {fieldErrors["from_name"] ? (
            <p
              id="smtp-from-name-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["from_name"]}
            </p>
          ) : null}
        </div>

        {/* TLS mode */}
        <div className="space-y-1.5">
          <Label htmlFor="smtp-tls-mode">TLS mode</Label>
          <Select
            id="smtp-tls-mode"
            value={tlsMode}
            onChange={(e) =>
              setTlsMode(e.target.value as "starttls" | "tls" | "none")
            }
            disabled={disabled}
            className="max-w-[200px]"
          >
            <option value="starttls">STARTTLS</option>
            <option value="tls">TLS (implicit)</option>
            <option value="none">None</option>
          </Select>
        </div>

        {/* Allow insecure TLS */}
        <div className="space-y-1.5">
          <div className="flex items-center gap-3">
            <Switch
              id="smtp-insecure-tls"
              checked={allowInsecureTls}
              onCheckedChange={setAllowInsecureTls}
              disabled={disabled}
              aria-label="Allow insecure TLS"
              aria-describedby="smtp-insecure-tls-hint"
            />
            <Label htmlFor="smtp-insecure-tls">Allow insecure TLS</Label>
          </div>
          <p
            id="smtp-insecure-tls-hint"
            className="text-xs text-[var(--color-destructive)]"
          >
            Disables TLS certificate verification. Only enable this for
            internal relays on a trusted private network.
          </p>
        </div>

        {putSmtp.isError ? (
          <PageError
            what="Could not save SMTP settings."
            why={putSmtp.error.message}
          />
        ) : null}

        {!readOnly ? (
          <div>
            <Button
              type="button"
              onClick={handleSave}
              disabled={putSmtp.isPending}
            >
              {putSmtp.isPending ? "Saving…" : "Save SMTP settings"}
            </Button>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Test email card
// ---------------------------------------------------------------------------

interface TestResult {
  ok: boolean;
  message: string;
}

function TestEmailCard({ disabled }: { disabled: boolean }) {
  const testSmtp = useTestSmtp();
  const [toAddress, setToAddress] = useState("");
  const [addressError, setAddressError] = useState<string | null>(null);
  const [lastResult, setLastResult] = useState<TestResult | null>(null);

  function handleSend() {
    const result = testSchema.safeParse({ to_address: toAddress });
    if (!result.success) {
      setAddressError(result.error.issues[0]?.message ?? "Invalid email");
      return;
    }
    setAddressError(null);
    setLastResult(null);

    testSmtp.mutate(
      { to_address: result.data.to_address },
      {
        onSuccess: (data) => {
          setLastResult(data);
          if (data.ok) {
            toast.success("Test email sent", {
              description: data.message,
            });
          } else {
            toast.error("Test email failed", {
              description: data.message,
            });
          }
        },
        onError: (err) => {
          setLastResult({ ok: false, message: err.message });
          toast.error("Test email failed", { description: err.message });
        },
      },
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Send test email</CardTitle>
        <CardDescription>
          Verify the relay by sending a test message. SMTP must be enabled and
          saved before testing.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="test-to-address">Recipient address</Label>
          <div className="flex max-w-sm flex-wrap gap-2">
            <Input
              id="test-to-address"
              type="email"
              value={toAddress}
              onChange={(e) => {
                setToAddress(e.target.value);
                setLastResult(null);
              }}
              disabled={disabled || testSmtp.isPending}
              placeholder="you@example.com"
              autoComplete="email"
              aria-invalid={addressError !== null ? true : undefined}
              aria-describedby={
                addressError ? "test-address-error" : undefined
              }
              className="flex-1"
            />
            <Button
              type="button"
              variant="outline"
              onClick={handleSend}
              disabled={disabled || testSmtp.isPending}
            >
              {testSmtp.isPending ? "Sending…" : "Send test"}
            </Button>
          </div>
          {addressError ? (
            <p
              id="test-address-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {addressError}
            </p>
          ) : null}
        </div>

        {disabled ? (
          <p className="text-xs text-muted-foreground">
            Save SMTP settings with "Enable outgoing email" toggled on before
            sending a test.
          </p>
        ) : null}

        {lastResult !== null ? (
          <p
            role="status"
            className={
              lastResult.ok
                ? "text-sm text-[var(--color-success,theme(colors.green.600))]"
                : "text-sm text-[var(--color-destructive)]"
            }
          >
            {lastResult.ok ? "Delivered: " : "Failed: "}
            {lastResult.message}
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Deliverability help card (static content)
// ---------------------------------------------------------------------------

function DeliverabilityCard() {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Deliverability checklist</CardTitle>
        <CardDescription>
          Transactional mail lands in inboxes only when the sending domain is
          properly authenticated. Work through the items below before going
          live.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <DeliverabilityItem heading="SPF record">
          Add a TXT record to your domain DNS authorising your relay IP or
          hostname. For example:{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            v=spf1 include:_spf.your-relay.example.com ~all
          </code>
          . Without SPF, many providers will soft-fail or reject your mail.
        </DeliverabilityItem>

        <DeliverabilityItem heading="DKIM signature">
          Enable DKIM on your relay and publish the public key as a TXT record
          under{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            selector._domainkey.yourdomain.com
          </code>
          . DKIM proves the message body was not altered in transit and is
          required by the major mailbox providers.
        </DeliverabilityItem>

        <DeliverabilityItem heading="DMARC policy">
          Publish a DMARC TXT record at{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            _dmarc.yourdomain.com
          </code>{" "}
          telling receivers what to do when SPF or DKIM fails. Start with{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            p=none
          </code>{" "}
          to monitor, then move to{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            p=quarantine
          </code>{" "}
          or{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            p=reject
          </code>{" "}
          once you are confident nothing legitimate will fail.
        </DeliverabilityItem>

        <DeliverabilityItem heading="Reverse DNS / PTR record">
          Make sure the sending IP has a PTR record that resolves to a hostname
          matching its forward A record. Many mail servers reject or heavily
          penalise mail from IPs with no PTR. Ask your hosting provider or
          VPS control panel to set one.
        </DeliverabilityItem>

        <DeliverabilityItem heading="From-address alignment">
          The "From address" you set above should be on the same domain as your
          SPF and DKIM records. Misaligned From domains cause DMARC failures
          even when SPF and DKIM individually pass. Password-reset links are
          particularly sensitive because they arrive at the user's inbox directly
          and any spam classification damages trust.
        </DeliverabilityItem>
      </CardContent>
    </Card>
  );
}

function DeliverabilityItem({
  heading,
  children,
}: {
  heading: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <p className="font-medium text-foreground">{heading}</p>
      <p className="text-muted-foreground">{children}</p>
    </div>
  );
}
