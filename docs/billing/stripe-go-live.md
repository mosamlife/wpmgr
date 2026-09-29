# Runbook: Stripe go-live (Secret Manager, IAM, Cloud Run)

Wiring Stripe credentials into the control plane for the first time, and
rotating them afterwards. Procedures only — for the customer-facing billing
behaviour this config unlocks, see the billing feature docs; this file covers
the infra side alone.

- GCP project: `wpmgr-prod`
- Cloud Run service: `wpmgr-api`, region `asia-south1`
- Cloud Build (image build): region `us-central1`
- Config surface: `apps/api/internal/config/config.go` (`StripeConfig`),
  validated in `apps/api/internal/config/validate.go`
  (`validateStripeConfig`)

## Read this first

1. **Order matters (risk 22).** An image that boots without
   `WPMGR_BILLING_STRIPE_PORTAL_CONFIGURATION` set fails config validation by
   design — that is the intended, safe failure. A checkout against a Stripe
   account whose Tax setting is not yet active fails the same way a checkout
   against any misconfigured account does. Both are avoided by following the
   deploy order in this doc, not worked around after the fact.
2. **The six `WPMGR_BILLING_STRIPE_*` variables are all-or-nothing.** Today's
   code (`validateStripeConfig`) enforces five of them together:
   `WPMGR_BILLING_STRIPE_SECRET_KEY`, `WPMGR_BILLING_STRIPE_WEBHOOK_SECRET`,
   `WPMGR_BILLING_STRIPE_PRICE_STARTER`, `WPMGR_BILLING_STRIPE_PRICE_AGENCY`,
   `WPMGR_BILLING_STRIPE_PRICE_SCALE`. `WPMGR_BILLING_STRIPE_PORTAL_CONFIGURATION`
   joins that set once the slice that reads it is deployed. Confirm which set
   the running image validates before you skip it.
3. **The secret key and webhook signing secret never pass through chat, a
   shell history entry, or a committed file.** Every step below pipes them
   straight from the Stripe Dashboard into `gcloud secrets versions add`.
4. **A version number is pinned, never `:latest`.** Secret Manager keeps every
   version; Cloud Run is told the exact version number so a later, unrelated
   rotation can never silently reach this service.

## 1. Restricted key

In the Stripe Dashboard (live mode for the live key, test mode for the
sandbox key): **Developers → API keys → Create restricted key**. Grant exactly:

| Resource | Access |
|---|---|
| Checkout Sessions | Write |
| Customers | Write |
| Customer portal sessions | Write |
| Subscriptions | Write |
| Prices | Read |
| Tax | confirm in sandbox testing before deciding — leave unset until the sandbox checkout matrix confirms whether `automatic_tax` needs it |
| Everything else (Refunds, Products, Charges, Payouts, Webhook endpoints, portal-configuration write) | None |

Click **Create key**, then immediately paste the revealed value into Secret
Manager (§3) — never into a chat message, an issue, a PR, or a local file.
Stripe shows the full value exactly once.

## 2. Portal configuration

Create the WPMgr billing-portal configuration through the API (not the
Dashboard default), so the portal can never silently fall back to the
account-wide default:

```
stripe billing_portal configurations create \
  --business-profile.headline="Manage your WPMgr subscription" \
  --features.subscription_cancel.enabled=true \
  --features.payment_method_update.enabled=true \
  --features.invoice_history.enabled=true
```

Record the returned `bpc_...` id — it is `WPMGR_BILLING_STRIPE_PORTAL_CONFIGURATION`
(§5), a plain env var, not a secret.

## 3. Secret Manager

Create two secrets (live) and their sandbox counterparts. Names below are the
convention this runbook establishes; keep them if nothing in the project
already uses different ones.

```
gcloud secrets create wpmgr-billing-stripe-secret-key \
  --project wpmgr-prod --replication-policy automatic

gcloud secrets create wpmgr-billing-stripe-webhook-secret \
  --project wpmgr-prod --replication-policy automatic

gcloud secrets create wpmgr-billing-stripe-secret-key-sandbox \
  --project wpmgr-prod --replication-policy automatic

gcloud secrets create wpmgr-billing-stripe-webhook-secret-sandbox \
  --project wpmgr-prod --replication-policy automatic
```

Add the restricted key's value as the first version, piped straight from
the paste — nothing touches disk or shell history:

```
gcloud secrets versions add wpmgr-billing-stripe-secret-key \
  --project wpmgr-prod --data-file=-
# paste the rk_live_... value, then Ctrl-D
```

Note the version number `gcloud` prints back (for example `1`). That number,
not `latest`, is what §5's `--update-secrets` pins.

The webhook secret is added the same way, after §4 creates the endpoint and
reveals it.

### IAM: grant the runtime service account `secretAccessor`

Look up the service account Cloud Run actually runs as — never assume the
name, confirm it:

```
gcloud run services describe wpmgr-api --project wpmgr-prod \
  --region asia-south1 --format='value(spec.template.spec.serviceAccountName)'
```

Grant that account read access to each secret (repeat per secret):

```
gcloud secrets add-iam-policy-binding wpmgr-billing-stripe-secret-key \
  --project wpmgr-prod \
  --member="serviceAccount:<runtime-sa-from-above>" \
  --role="roles/secretmanager.secretAccessor"
```

Grant only that role, to that one service account, per secret — the same
scope the control plane's other Secret Manager bindings already use
(`docs/adr/ADR-045-email-auth-alerts.md`).

## 4. Dedicated webhook endpoint

**Dashboard → Developers → Webhooks → Add endpoint.** Do not reuse an
endpoint created for anything else — this one is dedicated to WPMgr billing.

- URL: `https://manage.wpmgr.app/webhooks/billing/stripe`
- API version: pin explicitly to `2026-06-24.dahlia` rather than "current
  default" — do not leave it on the account default, which can move under
  you on a later Stripe upgrade
- Events — subscribe to exactly these, and no others:
  - `checkout.session.completed`
  - `customer.subscription.created`
  - `customer.subscription.updated`
  - `customer.subscription.deleted`
  - `invoice.paid`
  - `invoice.payment_failed`
  - `checkout.session.expired`
  - `invoice.payment_action_required`
  - `customer.tax_id.updated`
  - **Do not add `charge.refunded`.** It is deliberately not handled.

Reveal the signing secret once, and pipe it directly into Secret Manager —
same rule as the API key, nothing in between:

```
gcloud secrets versions add wpmgr-billing-stripe-webhook-secret \
  --project wpmgr-prod --data-file=-
# paste the whsec_... value, then Ctrl-D
```

Repeat this whole section in test mode for the sandbox endpoint, storing that
signing secret in `wpmgr-billing-stripe-webhook-secret-sandbox`.

## 5. Cloud Run: secrets and env vars

Five of the six `WPMGR_BILLING_STRIPE_*` variables are plain values (three
Price ids plus the portal configuration id — none of them are secret). Two
are pinned Secret Manager versions. Get the version numbers first:

```
gcloud secrets versions list wpmgr-billing-stripe-secret-key --project wpmgr-prod
gcloud secrets versions list wpmgr-billing-stripe-webhook-secret --project wpmgr-prod
```

Then, with the actual Price ids and portal configuration id from §1/§2 and
the actual version numbers from above substituted in:

```
gcloud run services update wpmgr-api \
  --project wpmgr-prod \
  --region asia-south1 \
  --update-secrets="WPMGR_BILLING_STRIPE_SECRET_KEY=wpmgr-billing-stripe-secret-key:<VERSION>,WPMGR_BILLING_STRIPE_WEBHOOK_SECRET=wpmgr-billing-stripe-webhook-secret:<VERSION>" \
  --update-env-vars="WPMGR_BILLING_STRIPE_PRICE_STARTER=price_...,WPMGR_BILLING_STRIPE_PRICE_AGENCY=price_...,WPMGR_BILLING_STRIPE_PRICE_SCALE=price_...,WPMGR_BILLING_STRIPE_PORTAL_CONFIGURATION=bpc_..."
```

This is an env/secret-only update; it does not build or deploy a new image,
and it preserves the service's existing scaling and networking config.

## 6. Deploy order (risk 22)

Do these in order. Reversing steps 1 and 2, or running step 3 before either,
produces the two failures §"Read this first" already named.

1. **In Stripe:** Tax active for the head office (confirm below), the
   Product and Prices created, the portal configuration (§2), and the
   dedicated webhook endpoint (§4) all exist first.
2. **In GCP (this doc):** the secrets and env vars from §3 and §5 are bound
   to `wpmgr-api` *before* the image that reads them is deployed.
3. **Then, and only then,** deploy the image (the S0.3a-or-later build that
   reads these six variables) — a plain image-only
   `gcloud run deploy wpmgr-api --image <registry>/api:<x.y.z> --region asia-south1`
   preserves everything set in step 2.

Confirm Tax is active before step 1 is considered done, in each account and
mode:

```
stripe get /v1/tax/settings
```

Expect `"status": "active"`. Re-run this again right before the first live
checkout in step 3 (S0.7's launch gate), not only once during setup.

## 7. Sandbox equivalent

Run §1–§6 unchanged against Stripe test mode and the `-sandbox` secrets
(`rk_test_...` key, test-mode webhook endpoint, test-mode Tax activation).
Staging uses the same `wpmgr-api` deploy mechanism with the sandbox secret
versions bound instead of the live ones — it is a temporal stage (verify on
test keys, then re-run §5 pointing at the live secret versions), not a
separate GCP project or Cloud Run service.

Do not cut over to the live secret versions until the sandbox checkout
matrix (referenced by S0.7) is green.

## 8. Alerts

Set up log-based alert policies in Cloud Monitoring against `wpmgr-api`'s
structured (`log/slog` JSON) output, on:

- **intake 5xx** — the webhook-intake handler returning 5xx to Stripe (Stripe
  retries these itself; the alert exists so a run of them is caught before
  the retry backlog does);
- **unknown price** — a WPMgr-attributed subscription referencing a Price id
  outside the three configured tiers, for any subscription status;
- **tax ID** — a tax ID that is `unverified`, or of a type with no automatic
  verification (for example a GSTIN);
- **tax ID not proof** — a tax ID that does not count as business proof, on
  an EU billing country;
- **discarded `billing_tax_check`** — that River job exhausting its retries.

The exact log field names and match text depend on the S0.2/S0.3a code that
emits them; confirm the actual `msg` and attribute keys against the deployed
binary's logs (`gcloud logging read`) when wiring each policy, rather than
assuming the wording below is final:

```
gcloud logging read \
  'resource.type="cloud_run_revision" resource.labels.service_name="wpmgr-api" jsonPayload.level="WARN"' \
  --project wpmgr-prod --limit 20 --format=json
```

Route all five to the same notification channel used for this service's
other production alerts; create one first (`gcloud alpha monitoring channels
create`) if none exists yet.

## 9. Secret rotation

Rolling the webhook secret: create the new endpoint version's secret in
Stripe, add a new Secret Manager version (§3's `gcloud secrets versions add`),
then rebind `wpmgr-api` to the new pinned version (§5). Stripe accepts the old
signing secret for up to 24 hours after rotation, so this is safe to do without
a maintenance window — deploy the new version, confirm intake is healthy, then
the old one lapses on its own.

A `stripe-go` major version bump re-pins the endpoint's Stripe API version
(§4) in the same release that bumps the dependency.
