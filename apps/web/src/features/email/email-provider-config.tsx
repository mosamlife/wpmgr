import { useState, useId } from "react";
import {
  Eye,
  EyeOff,
  CheckCircle2,
  ExternalLink,
  RefreshCw,
  Building2,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  CardDescription,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import type {
  PutEmailConfigRequest,
  EmailProviderSpec,
  EmailProviderField,
} from "@wpmgr/api";
import { useProviders, usePutEmailConfig, useEmailConfig, useOrgEmailConfig, useSyncEmailConfig } from "./use-email";
import { EmailWebhookConfigCard } from "./email-webhook-config";

// ---------------------------------------------------------------------------
// Provider picker
// ---------------------------------------------------------------------------

export interface ProviderPickerProps {
  value: string;
  providers: EmailProviderSpec[];
  onChange: (slug: string) => void;
  disabled?: boolean;
}

export function ProviderPicker({
  value,
  providers,
  onChange,
  disabled,
}: ProviderPickerProps) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Provider</Label>
      <Select
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
      >
        <option value="">Select a provider</option>
        {providers.map((p) => (
          <option key={p.slug} value={p.slug}>
            {p.label}
          </option>
        ))}
      </Select>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Dynamic field renderer — per-provider field schema from the catalog
// ---------------------------------------------------------------------------

export interface DynamicFieldProps {
  field: EmailProviderField;
  /** Current non-secret config values keyed by field.key */
  config: Record<string, unknown>;
  /** Current secret input value (write-only, never prefilled) */
  secretValue: string;
  /** Whether the secret is already stored (secret_set from the GET) */
  secretSet: boolean;
  /**
   * Whether the stored secret belongs to the organisation rather than this
   * site (`inherited` from the GET). When it is true, `secretSet` describes a
   * credential this site does not own, so the field says so instead of
   * claiming the site is configured.
   */
  inherited?: boolean;
  /**
   * Whether the endpoint has been edited away from the organisation's. The
   * organisation credential is only sent to the endpoint it was issued for, so
   * a diverged config needs a password of its own.
   */
  endpointDiverged?: boolean;
  /** Whether the operator has opened the "Replace" affordance */
  replacingSecret: boolean;
  onConfigChange: (key: string, value: unknown) => void;
  onSecretChange: (value: string) => void;
  onStartReplaceSecret: () => void;
  disabled?: boolean;
}

export function DynamicField({
  field,
  config,
  secretValue,
  secretSet,
  inherited = false,
  endpointDiverged = false,
  replacingSecret,
  onConfigChange,
  onSecretChange,
  onStartReplaceSecret,
  disabled,
}: DynamicFieldProps) {
  const id = useId();
  const [showSecret, setShowSecret] = useState(false);

  if (field.is_secret) {
    // Secret fields are write-only. Show an indicator when one is stored and an
    // affordance to enter a new value. The GET response NEVER returns the
    // actual value — only secret_set, plus inherited to say whose it is.
    const showStoredIndicator = secretSet && !replacingSecret;
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        {showStoredIndicator ? (
          <div className="flex items-center gap-2">
            {inherited ? (
              <Badge variant="muted" className="gap-1">
                <Building2 aria-hidden="true" className="size-3" />
                From organisation
              </Badge>
            ) : (
              <Badge variant="success" className="gap-1">
                <CheckCircle2 aria-hidden="true" className="size-3" />
                Configured
              </Badge>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onStartReplaceSecret}
              disabled={disabled}
            >
              {inherited ? "Use a different one" : "Replace"}
            </Button>
          </div>
        ) : (
          <div className="relative">
            <Input
              id={id}
              type={showSecret ? "text" : "password"}
              value={secretValue}
              onChange={(e) => onSecretChange(e.target.value)}
              placeholder={
                field.help ?? `Enter ${field.label.toLowerCase()}`
              }
              autoComplete="new-password"
              className="pr-9"
              disabled={disabled}
              aria-describedby={field.help ? `${id}-help` : undefined}
            />
            <button
              type="button"
              aria-label={showSecret ? "Hide secret" : "Show secret"}
              onClick={() => setShowSecret((v) => !v)}
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {showSecret ? (
                <EyeOff aria-hidden="true" className="size-4" />
              ) : (
                <Eye aria-hidden="true" className="size-4" />
              )}
            </button>
          </div>
        )}
        {inherited && endpointDiverged && secretValue.trim() === "" ? (
          <p className="text-xs text-[var(--color-destructive)]">
            These settings no longer match the organisation&rsquo;s, so the
            organisation credential will not be sent to them. Enter a{" "}
            {field.label.toLowerCase()} for this site, or restore the
            organisation settings.
          </p>
        ) : inherited && showStoredIndicator ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            This site uses the organisation credential. It is only sent while
            the settings above still match the organisation&rsquo;s.
          </p>
        ) : null}
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  const rawValue = config[field.key] ?? field.default ?? "";
  const currentValue =
    typeof rawValue === "string" ||
    typeof rawValue === "number" ||
    typeof rawValue === "boolean"
      ? rawValue
      : "";

  if (field.type === "select" && field.options && field.options.length > 0) {
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        <Select
          id={id}
          value={String(currentValue)}
          onChange={(e) => onConfigChange(field.key, e.target.value)}
          disabled={disabled}
          aria-describedby={field.help ? `${id}-help` : undefined}
        >
          {field.options.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </Select>
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  if (field.type === "boolean") {
    const checked = Boolean(currentValue === "true" || currentValue === true);
    return (
      <div className="flex items-center justify-between gap-3">
        <div className="flex flex-col gap-0.5">
          <Label htmlFor={id} className="cursor-pointer">
            {field.label}
            {field.is_required && (
              <span className="ml-1 text-[var(--color-destructive)]">*</span>
            )}
          </Label>
          {field.help ? (
            <p className="text-xs text-[var(--color-muted-foreground)]">
              {field.help}
            </p>
          ) : null}
        </div>
        <Switch
          id={id}
          checked={checked}
          onCheckedChange={(v) => onConfigChange(field.key, v)}
          disabled={disabled}
          aria-label={field.label}
        />
      </div>
    );
  }

  if (field.type === "number") {
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        <Input
          id={id}
          type="number"
          value={String(currentValue)}
          onChange={(e) => onConfigChange(field.key, e.target.value)}
          disabled={disabled}
          aria-describedby={field.help ? `${id}-help` : undefined}
        />
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  // Default: text input
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>
        {field.label}
        {field.is_required && (
          <span className="ml-1 text-[var(--color-destructive)]">*</span>
        )}
      </Label>
      <Input
        id={id}
        type="text"
        value={String(currentValue)}
        onChange={(e) => onConfigChange(field.key, e.target.value)}
        disabled={disabled}
        aria-describedby={field.help ? `${id}-help` : undefined}
      />
      {field.help ? (
        <p
          id={`${id}-help`}
          className="text-xs text-[var(--color-muted-foreground)]"
        >
          {field.help}
        </p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Inherited-credential rules (mirror of the control plane)
// ---------------------------------------------------------------------------

/**
 * The config fields that decide where a credential is sent and who it
 * authenticates as, per provider. This mirrors credentialAudienceFields in
 * apps/api/internal/email/service.go: the control plane only lends the
 * organisation credential to a site whose config still matches the
 * organisation's on every one of these, so this is what the page watches in
 * order to warn honestly rather than promise a credential that will not be sent.
 */
const CREDENTIAL_AUDIENCE_FIELDS: Record<string, string[]> = {
  smtp: ["host", "port", "username", "encryption", "auth"],
  ses: ["access_key", "region"],
  mailgun: ["domain_name", "region"],
  sendgrid: [],
  postmark: [],
};

/**
 * Normalises one config value for comparison. Every audience field in the
 * catalog is a string, number or boolean, and the two sides come from the same
 * JSONB column, so comparing their printed form is enough. It also absorbs the
 * one real mismatch: a number input writes back the string "587" over an
 * organisation row holding the number 587.
 */
function audienceValue(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  // No audience field is a structured value; comparing the serialised form is
  // the honest fallback rather than "[object Object]" for everything.
  return JSON.stringify(v) ?? "";
}

export function endpointDivergedFromOrg(
  provider: string,
  config: Record<string, unknown>,
  orgProvider: string,
  orgConfig: Record<string, unknown>,
): boolean {
  if (provider !== orgProvider) return true;
  const fields = CREDENTIAL_AUDIENCE_FIELDS[provider];
  // An unknown provider slug is never treated as a match, exactly as the
  // control plane refuses to share a credential it cannot reason about.
  if (fields === undefined) return true;
  return fields.some(
    (key) => audienceValue(config[key]) !== audienceValue(orgConfig[key]),
  );
}

// ---------------------------------------------------------------------------
// Provider Config Card (main export)
// ---------------------------------------------------------------------------

interface EmailProviderConfigProps {
  siteId: string;
}

export function EmailProviderConfig({ siteId }: EmailProviderConfigProps) {
  const configQuery = useEmailConfig(siteId);
  const providersQuery = useProviders();
  const save = usePutEmailConfig(siteId);
  const sync = useSyncEmailConfig(siteId);

  // Local state mirrors the server config. We derive it from the query data
  // once it loads. Per-field "saving" state is tracked per field so row-level
  // feedback is possible without a global flip.
  const [selectedProvider, setSelectedProvider] = useState("");
  const [fromAddress, setFromAddress] = useState("");
  const [fromName, setFromName] = useState("");
  const [forceFromEmail, setForceFromEmail] = useState(false);
  const [forceFromName, setForceFromName] = useState(false);
  const [returnPath, setReturnPath] = useState(false);
  const [logEmails, setLogEmails] = useState(false);
  const [storeBody, setStoreBody] = useState(false);
  const [retentionDays, setRetentionDays] = useState(14);
  const [providerConfig, setProviderConfig] = useState<Record<string, unknown>>(
    {},
  );
  const [secretValue, setSecretValue] = useState("");
  const [replacingSecret, setReplacingSecret] = useState(false);
  const [initialized, setInitialized] = useState(false);
  /**
   * The organisation's provider + config as first loaded, kept only when this
   * site has no config of its own. An inherited GET response IS the org row, so
   * this needs no second request, and it is what the divergence warning
   * compares against as the operator edits.
   */
  const [orgBaseline, setOrgBaseline] = useState<{
    provider: string;
    config: Record<string, unknown>;
  } | null>(null);

  // Populate local state from server data once on first load
  const serverConfig = configQuery.data;
  if (serverConfig && !initialized) {
    setSelectedProvider(serverConfig.provider ?? "");
    setFromAddress(serverConfig.from_address ?? "");
    setFromName(serverConfig.from_name ?? "");
    setForceFromEmail(serverConfig.force_from_email ?? false);
    setForceFromName(serverConfig.force_from_name ?? false);
    setReturnPath(serverConfig.return_path ?? false);
    setLogEmails(serverConfig.log_emails ?? false);
    setStoreBody(serverConfig.store_body ?? false);
    setRetentionDays(serverConfig.retention_days ?? 14);
    setProviderConfig(serverConfig.config ?? {});
    if (serverConfig.inherited === true) {
      setOrgBaseline({
        provider: serverConfig.provider ?? "",
        config: serverConfig.config ?? {},
      });
    }
    setInitialized(true);
  }

  // True while the stored credential shown on this page belongs to the
  // organisation rather than to this site.
  const inherited = serverConfig?.inherited === true;
  const endpointDiverged =
    orgBaseline !== null &&
    endpointDivergedFromOrg(
      selectedProvider,
      providerConfig,
      orgBaseline.provider,
      orgBaseline.config,
    );

  function handleProviderChange(slug: string) {
    setSelectedProvider(slug);
    // Reset provider-specific config when switching providers
    setProviderConfig({});
    setSecretValue("");
    setReplacingSecret(false);
  }

  function handleConfigChange(key: string, value: unknown) {
    setProviderConfig((prev) => ({ ...prev, [key]: value }));
  }

  function buildPayload(): PutEmailConfigRequest {
    const body: PutEmailConfigRequest = {
      provider: selectedProvider,
      from_address: fromAddress,
      from_name: fromName,
      force_from_email: forceFromEmail,
      force_from_name: forceFromName,
      return_path: returnPath,
      log_emails: logEmails,
      store_body: storeBody,
      retention_days: retentionDays,
      config: providerConfig,
    };
    // Nil-sentinel: only include `secret` when the operator has explicitly
    // typed a new value. An omitted `secret` preserves the stored credential.
    if (secretValue.trim() !== "") {
      body.secret = secretValue;
    } else if (replacingSecret && secretValue === "") {
      // Operator opened the Replace affordance but left it blank — preserve
      // existing credential (do not send empty string which clears it).
    }
    return body;
  }

  const isPending = configQuery.isPending || providersQuery.isPending;
  const isError = configQuery.isError || providersQuery.isError;

  if (isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }

  if (isError) {
    return (
      <PageError
        what="Could not load email configuration."
        why={
          (configQuery.error ?? providersQuery.error)?.message ??
          "Unknown error"
        }
        onRetry={() => {
          void configQuery.refetch();
          void providersQuery.refetch();
        }}
        retryLabel="Reload config"
      />
    );
  }

  const providers = providersQuery.data?.providers ?? [];
  const currentProviderSpec = providers.find(
    (p) => p.slug === selectedProvider,
  );

  return (
    <div className="space-y-6">
      {/* First-run hint: no per-site row and no org default yet. */}
      {!serverConfig ? (
        <div className="rounded-[var(--radius)] border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-4 py-3 text-sm text-[var(--color-muted-foreground)]">
          Email is not configured for this site yet. Choose a provider below and
          save to start routing this site&rsquo;s mail through it.
        </div>
      ) : null}

      {/* This site has no config of its own; everything below is the org's. */}
      {inherited ? (
        <div className="rounded-[var(--radius)] border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-4 py-3 text-sm text-[var(--color-muted-foreground)]">
          These are your organisation&rsquo;s email settings. This site has none
          of its own, and it sends using the organisation credential. Saving
          gives the site its own copy; it keeps using the organisation
          credential only while these settings still match the
          organisation&rsquo;s.
        </div>
      ) : null}

      {/* Provider selection */}
      <Card>
        <CardHeader>
          <CardTitle>Provider</CardTitle>
          <CardDescription>
            Choose which email service sends mail for this site.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <ProviderPicker
            value={selectedProvider}
            providers={providers}
            onChange={handleProviderChange}
            disabled={save.isPending}
          />
          {currentProviderSpec?.docs_url ? (
            <a
              href={currentProviderSpec.docs_url}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 text-sm text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {currentProviderSpec.label} docs
              <ExternalLink aria-hidden="true" className="size-3.5" />
            </a>
          ) : null}
        </CardContent>
      </Card>

      {/* Dynamic provider-specific fields */}
      {currentProviderSpec && currentProviderSpec.fields.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>{currentProviderSpec.label} settings</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            {currentProviderSpec.fields.map((field) => (
              <DynamicField
                key={field.key}
                field={field}
                config={providerConfig}
                secretValue={secretValue}
                secretSet={serverConfig?.secret_set ?? false}
                inherited={inherited}
                endpointDiverged={endpointDiverged}
                replacingSecret={replacingSecret}
                onConfigChange={handleConfigChange}
                onSecretChange={setSecretValue}
                onStartReplaceSecret={() => setReplacingSecret(true)}
                disabled={save.isPending}
              />
            ))}
          </CardContent>
        </Card>
      ) : null}

      {/* Webhook config (API providers only) */}
      {serverConfig ? (
        <EmailWebhookConfigCard siteId={siteId} config={serverConfig} />
      ) : null}

      {/* Sender identity */}
      <Card>
        <CardHeader>
          <CardTitle>Sender identity</CardTitle>
          <CardDescription>
            The From address and name used for outgoing mail.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <FromAddressField
            value={fromAddress}
            onChange={setFromAddress}
            disabled={save.isPending}
          />
          <FromNameField
            value={fromName}
            onChange={setFromName}
            disabled={save.isPending}
          />
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Force From address</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Override any plugin-supplied From address with the one above.
              </span>
            </div>
            <Switch
              checked={forceFromEmail}
              onCheckedChange={setForceFromEmail}
              disabled={save.isPending}
              aria-label="Force From address"
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Force From name</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Override any plugin-supplied From name.
              </span>
            </div>
            <Switch
              checked={forceFromName}
              onCheckedChange={setForceFromName}
              disabled={save.isPending}
              aria-label="Force From name"
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Set Return-Path</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Set the Return-Path / Bounce-To header to the From address.
              </span>
            </div>
            <Switch
              checked={returnPath}
              onCheckedChange={setReturnPath}
              disabled={save.isPending}
              aria-label="Set Return-Path header"
            />
          </div>
        </CardContent>
      </Card>

      {/* Email log settings */}
      <Card>
        <CardHeader>
          <CardTitle>Email log</CardTitle>
          <CardDescription>
            Control whether sent emails are recorded in the log.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Log outgoing emails</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Record each send attempt in the email log.
              </span>
            </div>
            <Switch
              checked={logEmails}
              onCheckedChange={setLogEmails}
              disabled={save.isPending}
              aria-label="Log outgoing emails"
            />
          </div>
          {logEmails ? (
            <>
              <div className="flex items-center justify-between gap-3">
                <div className="flex flex-col gap-0.5">
                  <span className="text-sm font-medium">Store email body</span>
                  <span className="text-xs text-[var(--color-muted-foreground)]">
                    Persist the full message body (off by default for privacy).
                  </span>
                </div>
                <Switch
                  checked={storeBody}
                  onCheckedChange={setStoreBody}
                  disabled={save.isPending}
                  aria-label="Store email body"
                />
              </div>
              <RetentionDaysField
                value={retentionDays}
                onChange={setRetentionDays}
                disabled={save.isPending}
              />
            </>
          ) : null}
        </CardContent>
      </Card>

      {/* Save */}
      <div className="flex items-center justify-end gap-3">
        {save.isError ? (
          <p
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            {save.error.message}
          </p>
        ) : null}
        <Button
          type="button"
          variant="outline"
          onClick={() => sync.mutate()}
          disabled={!serverConfig || sync.isPending}
        >
          <RefreshCw aria-hidden="true" className="mr-1.5 size-4" />
          {sync.isPending ? "Syncing…" : "Sync to site"}
        </Button>
        <Button
          type="button"
          onClick={() => save.mutate(buildPayload())}
          disabled={save.isPending || !selectedProvider}
        >
          {save.isPending ? "Saving…" : "Save email settings"}
        </Button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Small sub-components for fields that exist outside the dynamic catalog
// ---------------------------------------------------------------------------

function FromAddressField({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>From address</Label>
      <Input
        id={id}
        type="email"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="no-reply@example.com"
        disabled={disabled}
      />
    </div>
  );
}

function FromNameField({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>From name</Label>
      <Input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="My Site"
        disabled={disabled}
      />
    </div>
  );
}

function RetentionDaysField({
  value,
  onChange,
  disabled,
}: {
  value: number;
  onChange: (v: number) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Log retention (days)</Label>
      <Input
        id={id}
        type="number"
        min={1}
        max={365}
        value={String(value)}
        onChange={(e) => onChange(Number(e.target.value))}
        disabled={disabled}
      />
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Log entries older than this are pruned automatically (1 to 365 days).
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Org config panel (same shape, org-wide defaults)
// ---------------------------------------------------------------------------

export interface EmailOrgConfigPanelProps {
  /** Used as the owner label ("Org default") */
  label?: string;
}

export function EmailOrgConfigPanel({
  label = "Org default",
}: EmailOrgConfigPanelProps) {
  const configQuery = useOrgEmailConfig();
  const providersQuery = useProviders();

  if (configQuery.isPending || providersQuery.isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }

  if (configQuery.isError || providersQuery.isError) {
    return (
      <PageError
        what="Could not load org email configuration."
        why={
          (configQuery.error ?? providersQuery.error)?.message ?? "Unknown error"
        }
        onRetry={() => {
          void configQuery.refetch();
          void providersQuery.refetch();
        }}
      />
    );
  }

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)] px-4 py-3">
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        {label}
      </p>
      <p className="mt-0.5 text-xs text-[var(--color-muted-foreground)]">
        Provider:{" "}
        <span className="font-medium">
          {providersQuery.data?.providers.find(
            (p) => p.slug === configQuery.data?.provider,
          )?.label ?? configQuery.data?.provider ?? "Not set"}
        </span>
        {" · "}
        From:{" "}
        <span className="font-medium">
          {configQuery.data?.from_address ?? "Not set"}
        </span>
      </p>
    </div>
  );
}

