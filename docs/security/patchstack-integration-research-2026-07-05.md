# Patchstack Integration Research — Decision Grade

**File:** `docs/security/patchstack-integration-research-2026-07-05.md`
**Status:** Research complete — decisions required (see §9)
**Author context:** WPMgr already ingests the free Wordfence Intelligence V3 feed (CP-side domain `apps/api/internal/vuln/`). This document evaluates adding Patchstack as a vulnerability-data provider and, separately, as an active-protection (virtual patching) capability.
**Clean-room note:** Patchstack is evaluated here as a potential data vendor / integration partner. This is an internal architecture document. Per house rules, if any of this ships into agent/plugin source, competitor *plugins* are not to be named as technique sources in shipped comments — that constraint does not apply to this vendor-evaluation doc, but does apply to any code it produces.

---

## 1. Executive summary + recommendation

Patchstack is two products wearing one brand, and the distinction decides everything:

1. A **vulnerability data layer** (the Threat Intelligence API + public DB). This is **near-parity** with the free Wordfence Intelligence feed WPMgr already consumes. Patchstack's edge here is *timing* (it is the #1 WordPress CVE Numbering Authority and originated ~73% of 2023 WP disclosures via its managed VDP, so it often has an advisory "up to 48h ahead of public disclosure") plus operational triage signals (`is_exploited`, `patch_priority`).
2. An **active-protection layer** — **vPatch** (virtual patching): in-application JSON firewall rules that *block exploitation of a known-vuln component without updating it*. This is a genuine capability WPMgr does not have and cannot cheaply build. Today WPMgr can only **report** "plugin X has CVE Y, update it"; it has no enforcement point that blocks an exploit before a fix lands. Patchstack tracks **13,065 vulns with no official vendor patch** — precisely the cases a report-only tool cannot remediate.

**The make-or-break finding (see §5):** the vulnerability **feed** WPMgr would consume (Threat Intelligence API) is **NOT self-serve at any individual plan level**. Access is "Custom pricing, activated on request — contact us," and the "contact us" link points at the **for-Hosts** partner page; the bulk `/all` endpoint is Beta-gated to "selected partners working directly with Patchstack." The legacy self-serve-ish "Standard" tier (5,000 req/24h) is **closed to new customers**. Therefore a **true per-tenant BYO-key on the vuln feed does not exist** — a normal WPMgr tenant cannot go buy their own Threat Intelligence key. The only way WPMgr gets the feed is by holding **one master key under a for-Hosts/Enterprise contract** (unpublished price, cost lands on WPMgr's hosted margin).

The **only** thing a tenant *can* self-serve is an **App API key** (free with a Developer plan, generated at `app.patchstack.com/settings/integrations`). But the App API is a control/reporting API for *sites the tenant already enrolled in Patchstack App* (Patchstack plugin installed + site added). It re-surfaces Patchstack's own findings for already-protected sites; it is **not** a general vuln feed to match WPMgr's own inventory against.

### Recommendation

| Question | Recommendation | Confidence |
|---|---|---|
| Integrate as a **feed-only additive provider** replacing/augmenting Wordfence? | **Not as the near-term default.** The data layer is parity with the $0 Wordfence feed, and the feed is contact-sales-gated with unpublished pricing. Do **not** replace Wordfence. Build the **provider abstraction** (it is good hygiene regardless), but only light up Patchstack-as-feed if/when a for-Hosts contract is signed. | High |
| Add **vPatch**? | **Defer to a distinct Phase 2, and only as "orchestrate Patchstack's own mu-plugin."** Never build a WPMgr-agent WAF (re-implements their core IP; high effort/risk/licensing exposure). Surface a `vpatch_available` badge in the UI far earlier than we can actually apply one. | High |
| **BYO-key vs master-key?** | **Feed = master-key only** (BYO on the feed is impossible — no self-serve individual key). **Protection status = BYO App-API key** (the one genuinely self-serve path; lets WPMgr *read* vPatch/protection status for tenants who independently pay Patchstack). These are two different keys for two different jobs. | High |
| **Hosted-only vs self-host?** | Feed/master-key path is **hosted-only** (WPMGR_HOSTED), because the contract + cost sit with WPMgr. The **BYO App-API-key co-existence** path can be offered to **both** hosted and self-host tenants (each brings their own Patchstack subscription). | High |

**Bottom line:** The cheapest, most honest first step is a **read-only co-existence integration via the tenant's own App API key** (self-serve, free-with-plan, works for self-host and hosted) that surfaces Patchstack protection/vPatch status alongside WPMgr's existing Wordfence findings. Treat a real Patchstack **feed** and **resold vPatch** as a **premium, hosted-only, for-Hosts-partnership** track to open only if there is demonstrated product demand for active protection — its pricing is entirely contact-sales and lands on WPMgr's margin, and there is **no version where it matches the $0 of the Wordfence feed.**

---

## 2. What Patchstack is + product matrix + what it adds over Wordfence

### 2.1 Product matrix

**Consumer / practitioner**
- **Free vulnerability database** (`patchstack.com/database`) — public searchable web UI, API-backed; ~48,977 mitigations tracked, ~15,801 mitigation rules, **13,065 vulns with no official vendor patch**, plus a triage queue. WP plugins/themes/core focused. **No downloadable JSON dump / static feed file** (unlike Wordfence's free production feed) — browse-only on the web.
- **Free WordPress plugin ("Personal" plan)** — 40,000+ active installs. **Detection + email alerts ONLY**: matches installed plugins/themes/core against the vuln DB *locally* (no external scanning), optional auto-update of *only vulnerable* components, security snapshot reports; up to 3 sites; 48h early warning. **No firewall, no vPatch, no hardening, no 2FA.**
- **Paid App (per-site, ~$5/site/mo)** — adds vPatch, hardening + login protection (reCAPTCHA, .htaccess rules), community IP blocklist, access to the ~12,000 vPatch rules.
- **Developer plan** — $79/mo monthly or $69/mo annual ($828/yr); **25 sites, 3 seats**; extra sites $12.50/mo per 5 (~$2.50/site), extra seats $24/seat/mo. All protection modules incl. vPatch ("RapidMitigate"), App API integrations, remote software management, "protection up to 48h ahead." This is the agency/MSP tier.

**Host / enterprise**
- **Web Host plan ("for Hosts")** — custom billing; fleet application-layer security, reseller revenue stream, plug-and-play widget, dedicated rollout + tech + marketing support.
- **Enterprise** — custom billing; extended API endpoints, **SLA, DPA**, SOC2/PCI-DSS 4.0, enterprise support.

**APIs**
- **Threat Intelligence API** — the vuln DB as a feed. Extended tier = custom pricing (contact-sales); legacy Standard tier closed to new customers; Beta (npm/cursor/`/all`) limited to selected partners. `PSKey` header auth.
- **App API** — remote account/site management (provision sites, protection logs, reports, custom rules, SSO). Free to Developer-plan users, "unlimited requests, no rate limiting." Self-serve `PSKey`.

**Vendor / researcher side (context, not integration surface)**
- **mVDP** (managed Vulnerability Disclosure Program) — Patchstack runs the VDP for plugin/theme vendors (intake, false-positive rejection, patch validation, coordinated disclosure). **Free** for paid AND free plugins/themes. ~1,142 active researchers submit through it. This is the engine behind the "faster/earlier" disclosure claim.

### 2.2 What Patchstack adds over the Wordfence Intelligence feed WPMgr already has

1. **Virtual patching / auto-mitigation (the differentiator).** Wordfence-feed WPMgr can only report + delegate to the update pipeline. Patchstack can **block exploitation without updating the plugin**, via targeted in-application JSON rules pushed "within hours" of disclosure — covering the 13,065 no-patch-available vulns and the can't-update-right-now cases.
2. **Faster / earlier / exclusive intelligence (mVDP).** As #1 CNA and originator of the majority of WP disclosures, Patchstack frequently has the vuln (and a mitigation) *before* public CVE publication — "up to 48h ahead." It also validates reports to strip false positives / "beg-bounty" noise.
3. **Contextual prioritization + exploitation signal.** Payloads carry `cvss_score`, `cve[]`, `is_exploited` (in-the-wild), `patch_priority`, `patched_in_ranges` — so a fleet tool can *rank* remediation, not just list CVEs. (Wordfence gives CVSS + CWE + references but not `is_exploited`/`patch_priority`.)
4. **Hardening / login protection, community IP blocklist, PCI-DSS 4.0 support** around the raw data.

**Strategic read:** the **data layer is largely parity** with Wordfence (which WPMgr gets for $0). The differentiators worth acquiring are (a) **vPatch** (report → actually protect) and (b) **earlier mVDP-sourced timing**. WPMgr already ships a full plugin/agent on every site — the hard prerequisite vPatch needs — so an integration is about *licensing rules/feed*, not building new site-side plumbing. But (critically, §5) the licensable feed is partner-gated.

---

## 3. How vPatch works + deployment model

### 3.1 vPatch mechanics (in-application, not edge)

Patchstack's own docs are explicit: vPatch is an **in-application (endpoint) firewall, NOT a cloud/edge WAF.** A WAF "inspects web traffic at the web server software level with limited knowledge of protected applications" and "activates all firewall rules against every request." vPatch instead "operates within the application itself rather than at the edge," giving **context-aware blocking** using data only the app understands (`user authorization, software versions, etc.`). Because the engine "is directly connected to the application, we know exactly which vulnerabilities are present," so it runs "only rules that are applicable to each website" → "vPatches tend to be more efficient and cause less resource usage compared to a WAF."

A vPatch is "sending a rule (or a bunch of rules) that will mitigate a specific vulnerability in software without changing the vulnerable code itself" — a customized firewall rule as a "virtual shield." Rules are **JSON-based** with different instruction types.

**Two-phase execution engine** (docs: "Firewall engine WordPress mu-plugins"):
1. **MU-PLUGIN phase** — runs rules needing **no privilege check**, *before* WordPress initializes the session, blocking exploitation as early as possible. Guarded by a `PS_FW_MU_RAN` constant; init settings include `autoblockAttempts=10`, `autoblockMinutes=30`, `autoblockTime=60`, `mustUsePluginCall=true`.
2. **PLUGIN phase** — runs only privilege-related rules (`current_user_cannot`) *after* core initializes user sessions.
Rules load from local JSON via `file_get_contents` + `json_decode($rules, true)`.

**Rule creation/delivery:** vPatches are auto-generated via "crowdsourced security research and AI/ML source-code analysis"; Patchstack's team writes a virtual patch and "pushes that patch to paid subscribers within hours" — including "0-days not yet known to the public." **Delivery is PULL:** the on-site plugin "fetches mitigation rules that will run in the plugin on each request," and the cloud "only ship[s] the mitigation rules that the site needs" — i.e., the ruleset is **tailored to that site's actual plugin/theme inventory**, cached locally, executed per-request.

### 3.2 Deployment model

Two things run: an **on-site component** and Patchstack's **cloud**.

- **On-site:** the Patchstack plugin (standard `wp-content/plugins` install, WordPress.org). The **free** plugin "only runs scheduled tasks" (local match + email alerts; no firewall). The **paid** plugin additionally installs the firewall engine (mu-plugin + main plugin two-phase). **Enforcement is in-PHP on the site, not at the edge.**
- **Cloud:** hosts the vuln DB, generates/curates JSON vPatch rules, serves each site a **tailored** ruleset; alerts/reports/dashboard are SaaS.

**Fleet deployment for a host** — driven by the **App API** (`api.patchstack.com/app-api`):
- `POST /monitor/site/add` → provisions a site, returns per-site `siteid` + `apikey`.
- `POST /monitor/sites/list/basic` → enumerate all sites.
- `GET /monitor/site/view/{siteid}` → per-site info; `GET /monitor/site/{siteid}/delete` → deprovision.
- `POST /monitor/site/{site}/sso/generate` → mint **iframe SSO tokens** to embed the per-site Patchstack dashboard inside the host's own panel (the white-label/embed hook).
- Per-box scripting: `wp plugin install patchstack --activate`, or signed download `/download/wordpress/{site}`.
- Also exposes protection logs, security reports, site settings, add sites, **create custom rules**. Zapier/IFTTT supported.

### 3.3 CRITICAL GAP — can vPatch run *without* their plugin (host-applied rules)?

**Not per public docs.** There is **no documented server-level `auto_prepend_file` / global mu-plugin auto-injection** to enforce vPatches fleet-wide with zero per-site plugin. The documented enforcement path **always requires the Patchstack plugin** (which drops the mu-plugin) on each site. A true agent-less, host-injected deployment is **not published** — it would be a **custom "for Hosts" rollout item to confirm with their partner team (UNKNOWN).**

**Implication for WPMgr:** because our agent is *already* a full plugin on every site, WPMgr's two realistic paths are (i) consume Patchstack's Threat Intelligence API for **reporting only** (no Patchstack plugin), or (ii) **ship Patchstack's mitigation rules through WPMgr's existing agent** as a custom integration — but (ii) means either coordinating the Patchstack mu-plugin or re-implementing their rule engine (their proprietary IP; not recommended). White-label depth (logos, custom domains, reseller billing) beyond the SSO-iframe + per-site apikey + fleet-list primitives is part of the negotiated for-Hosts/Enterprise package, **not documented self-serve (UNKNOWN).**

---

## 4. API surface + auth + schema + feed-vs-query, mapped to WPMgr's vuln record

### 4.1 Two distinct APIs — keep them separate

**(A) Threat Intelligence API** — the raw vuln DB (the WPMgr-relevant one).
Base: `https://patchstack.com/database/api/v2/`. Header: `PSKey: <key>`.
- `GET /product/{type}/{name}/{version}` — advisories for one plugin/theme/core version (`type` = `plugin|theme|wordpress`).
- `GET /product/{type}/{name}/{version}/exists` — boolean "is this version vulnerable" (fast path, Extended).
- `GET /latest` — rolling feed of the **20 most recent** vulns (Extended). Closest thing to incremental, but a fixed small window, **no `since` param**.
- `POST /batch` — bulk lookup **up to 50 products/request**; results keyed by `product_slug` (duplicate slugs collapse) (Extended).
- `GET /all` — **full bulk enumeration** with `platform=wordpress|npm`, `page`+`per_page` (1–500) offset mode OR opaque `cursor` mode, `include=details`. **This is the feed-sync endpoint — but it lives in the Threat Intelligence BETA surface, exposed only to selected partners.**
- `GET /vulnerability/{id}` — advisory-by-id detail (Extended).
- **No CVE-lookup-by-CVE-id endpoint** — you query by product+version and read the `cve[]` off each record. **No per-site protection endpoint on this API.**

**(B) App API** — account/site management, **NOT** a vuln feed.
Base host: `https://api.patchstack.com/` (`api.patchstack.com/app-api/documentation`). 100+ endpoints: protection logs, security reports, add/remove sites, toggle settings, control vPatch + custom firewall rules, pull attacker IPs, per-site vuln data — **only for sites enrolled in the tenant's Patchstack App account.** Same `PSKey` header. A partner "Firewall Rules API" also exists (same header) for hosts.

### 4.2 Auth + where keys come from (decisive for BYO — see §5)

Both APIs use a single header `PSKey: <key>`.
- **App API key:** fully **self-serve** — log into Patchstack App → Settings → Integrations (`app.patchstack.com/settings/integrations`). Per-account, scoped (IP allowlist, read-only, expiry). Available to Developer/Enterprise, "unlimited requests," free with plan.
- **Threat Intelligence API key:** **NOT self-serve** — "Custom pricing, activated on request — contact us," with "contact us" → `patchstack.com/for-hosts`. Legacy Standard tier closed; Extended + Beta both require contacting Patchstack; Beta = "selected partners working directly with Patchstack." **No dashboard page to mint a Threat Intelligence key at an individual plan level.** Account/contract-scoped, provisioned by Patchstack on activation.

### 4.3 Response schema (two schemas — a real discrepancy to plan around)

**(1) Per-product / Extended record** (`GET /product/...`, `POST /batch`, `GET /latest`) — leaner:
`id`, `product_id`, `product_slug` (lowercased for matching), `product_name`, `product_name_premium|null`, `product_type` (`Plugin|Theme|WordPress`), `title`, `description`, `vuln_type`, `cvss_score` (decimal|null; older records null), `cve[]` (may be empty), `is_exploited` (bool), `patch_priority` (int|null; 1=within 30d, 2=7d, 3+=immediate), `affected_in` (range string: `<= x.x.x`, `x.x.x-x.x.x`, or comma-separated), `fixed_in` (may be empty), `patched_in_ranges[]` ({`from_version`, `to_version`, `fixed_in`} for LTS backports), `disclosure_date`/`disclosed_at`, `created_at`, `direct_url`.
**GAPS:** **NO `cwe`, NO `ghsa`, NO external reference URLs beyond `direct_url`.** Many fields nullable → null-handling required.

**(2) Beta `GET /all` record** — richer: `id`, `title`, `disclosed_at`, `created_at`, `url`, `vuln_type`, `cve`, `is_exploited`, `patch_priority`, `product` (object), `cvss`, **`cwe`, `ghsa`, `references[]`** (external URLs), and `version_info` (nested affected/fixed). Adds CWE/GHSA/references the Extended schema lacks.

**No `vpatch_available` boolean exists on the Threat Intelligence record** in either schema. `patch_priority` + `is_exploited` are the only triage signals; actual vPatch coverage is observable only via the **App API** for Patchstack-protected sites.

### 4.4 Mapping to WPMgr's existing (Wordfence-derived) vuln record

WPMgr's internal `FeedRecord`/`VulnSoftware` (`model.go:60-81`) is provider-neutral once parsed. Field mapping:

| WPMgr internal field | Patchstack Extended | Patchstack Beta `/all` | Wordfence (today) |
|---|---|---|---|
| vuln identity (`vuln_id` PK) | `id` (Patchstack int) | `id` | Wordfence UUID |
| CVE | `cve[]` | `cve` | CVE |
| CVSS | `cvss_score` (nullable) | `cvss` | CVSS + vector |
| affected slug | `product_slug` | `product.slug` | software slug |
| affected version range | `affected_in` (string) | `version_info` (nested) | `from_version`/`to_version` + `from_inclusive`/`to_inclusive` bools |
| fixed version | `fixed_in` / `patched_in_ranges[]` | `version_info` | patched version |
| type | `vuln_type` | `vuln_type` | type |
| **CWE** | **absent** | `cwe` | CWE |
| **references** | **only `direct_url`** | `references[]` | full `references[]` |
| disclosure date | `disclosed_at` | `disclosed_at` | published date |
| exploited-in-wild | `is_exploited` | `is_exploited` | *(absent in WF)* |
| triage priority | `patch_priority` | `patch_priority` | *(absent in WF)* |

**Key semantic difference for the parser:** Wordfence uses explicit `from_version`/`to_version` + inclusive booleans per software entry (which `wpversion.AffectedVersionRange` already consumes); Patchstack Extended gives a **string range** (`affected_in`) that must be parsed into WPMgr's range model — a **provider-specific adapter** on `parseAffectedVersions` (`service.go:359-385`). The matcher itself (`wpversion.IsVulnerable`, `compare.go:279`; `BestFixedVersion`, `compare.go:300`) is **provider-agnostic and reusable as-is.** Patchstack also splits richer metadata (`cwe`/`ghsa`/`references`) into the **partner-gated Beta `/all`** endpoint — so if WPMgr uses the widely-available Extended per-product path, it **loses CWE + references** relative to the Wordfence feed.

### 4.5 Feed-vs-query — which model maps to WPMgr's current architecture

WPMgr today does **bulk-feed-and-match**: `FeedWorker` (`worker.go:101-234`) fetches the whole Wordfence dump hourly, mirrors it to `wordfence_vuln_feed`/`wordfence_vuln_software`, and `RescanSite` joins it against inventory. Patchstack support for that model:
- **BULK FEED:** `GET /all` (offset or cursor). **Partner/Beta-gated.** There is **no downloadable static feed file** — unlike Wordfence's free production dump.
- **INCREMENTAL "SINCE":** **no native `updated_since`**. `GET /latest` = 20 newest (poll-and-dedupe); `/all` can be paged newest-first via cursor and stopped at already-seen IDs. Incremental sync is achievable **by convention, not by API.**
- **PER-LOOKUP:** `GET /product/...` (one at a time) and `POST /batch` (≤50/request) — Extended, custom-priced.

**Net:** WPMgr's hourly-full-dump pattern maps to `/all` (partner-gated) or repeated `/batch` (Extended, custom-priced). **Both require a Patchstack contract; neither is self-serve; neither supports per-tenant BYO-key.** A polling `/latest` + `/batch` design is possible but sits behind the same paid/contract gate.

**Integration primitives (all `PSKey: <key>`, base `.../v2/`):**
- Feed sync: `GET /all?platform=wordpress&cursor=<next>&include=details`, loop on `cursor.has_more` (Beta-gated).
- Catch-up: poll `GET /latest` and/or page `/all` newest-first until known IDs.
- Match one: `GET /product/plugin/{slug}/{version}` (full) or `.../exists` (boolean).
- Match many: `POST /batch` (≤50), read by `product_slug`.
- Detail: `GET /vulnerability/{id}`.

**SDK:** no official Patchstack SDK. Reference clients exist (`patchstack/wpcli-patchstack`, `10up/wpcli-vulnerability-scanner`); the docs ship a PHP example that matches API results against installed plugins/themes/core, plus an OpenAPI-style api-reference and a machine-readable `llms-full.txt`.

---

## 5. BYO-KEY analysis — the make-or-break question

**Verdict: a true per-tenant BYO-key on the vulnerability feed is effectively NOT available.** API access to the data WPMgr wants is gated to host/enterprise contracts. BYO-key is only feasible against a *different, narrower* API (the App API) that does not power an independent fleet scan.

### 5.1 Why feed BYO-key is blocked

1. The raw DB WPMgr would match inventory against is the **Threat Intelligence API**, which is **not self-serve at an individual plan level.** Access is sales/contract-gated ("Custom pricing, activated on request — contact us"), the contact target is the for-Hosts page, and the bulk `/all` endpoint is **Beta-limited to selected partners.** A normal WPMgr tenant **cannot sign up and obtain their own Threat Intelligence key.** → no per-tenant BYO-key for vuln data.
2. This is the same barrier that makes **WPMgr holding one master key the only practical feed option** — and that master key itself **requires a host/partner contract** with Patchstack.

### 5.2 What BYO-key *can* do (the App API, narrow)

- Every Developer/Enterprise Patchstack App account can **self-generate a scoped `PSKey`** at `app.patchstack.com/settings/integrations`; WPMgr could let each tenant paste theirs.
- **But** the App API only manages/reports on sites the tenant **enrolled in Patchstack App** (Patchstack plugin installed + site added + tenant paying Patchstack). It surfaces per-site vulns, vPatch status, protection logs **for those enrolled sites** — it is **not** a "give me the whole vuln DB to match my own inventory" feed.
- So a BYO-App-key integration **only works for sites the tenant already pays Patchstack to protect**, and **re-surfaces Patchstack's own findings** rather than powering WPMgr's independent fleet scan. This is a **co-existence / read-status** integration, not a feed replacement.

### 5.3 ToS / redistribution — UNKNOWN, must confirm

The public docs do **not** state whether an integrator may operate on behalf of tenants using per-tenant keys, or whether feed data may be **re-exposed in WPMgr's UI**. This is **UNKNOWN from public docs** and governed by the for-Hosts/partner contract. **Flag:** any master-key model, and any resale/re-display of feed data, needs an explicit partner agreement. WPMgr already carries hard attribution obligations for Wordfence (Defiant + MITRE, rendered per-view, `model.go:10-17`); Patchstack's attribution/license terms must be reviewed and stored+surfaced the same way.

### 5.4 Verdict + fallback ladder

| Model | Feasible today? | Notes |
|---|---|---|
| **Per-tenant BYO-key on the FEED** | **NO** | No self-serve individual Threat Intelligence key at any published price. |
| **WPMgr master key on the FEED** | Yes, **only via a for-Hosts/Extended contract** | Unpublished custom price; cost on WPMgr's hosted margin; ToS on re-display must be confirmed. **Hosted-only.** |
| **Per-tenant BYO App-API key** (co-existence / read protection status) | **YES** | Self-serve, free-with-Developer-plan; works for **self-host + hosted**; but only for sites the tenant already enrolled + pays Patchstack. Not a feed. |
| **for-Hosts partnership** (feed + resold vPatch under WPMgr brand) | Yes, negotiated | Sales-led, custom billing, unpublished terms; SSO-iframe + per-site apikey + fleet-list are the concrete embed primitives; deeper white-label is negotiated. |

**Recommended fallback:** ship **BYO App-API-key co-existence** first (only self-serve path, self-host-friendly, zero WPMgr cost). Reserve the **master-key feed** and **resold vPatch** for a **for-Hosts partnership** opened only on demonstrated demand.

---

## 6. Pricing scenarios — who pays what

### 6.1 Published numbers (and what's unpublished)

- **Personal:** $0 (3 slots, alerts only). Turn on protection = **$5/site/mo**.
- **Developer:** $79/mo monthly or **$69/mo annual** ($828/yr); 25 sites + 3 seats; +$12.50/mo per 5 sites (~$2.50/site); +$24/seat/mo. Effective **~$2.76/site/mo (annual)** or **~$3.16/site/mo (monthly)** at 25 sites.
- **Enterprise:** custom / contact-sales — **no published numbers.**
- **Web Host (for-Hosts):** custom / contact-sales — **no wholesale rate card.** Only published numbers are in the retail calculator: hosts resell at **$3–$5/site/mo (avg $4)**, assume **~5% conversion**, "~100% markup" → **implied host wholesale ~$2/site (never stated outright — UNKNOWN).** No published volume tiers, minimums, or revenue-share %. **The same for-Hosts contact path is how you request Threat Intelligence API (feed) access.**
- **App API:** free, unlimited, with Developer/Enterprise. Control API only — **not a resellable vuln feed.**
- **Threat Intelligence API:** Standard (legacy, 5,000 req/24h) **closed**; Extended = "custom pricing, activated on request" → for-Hosts; **no free tier, no self-serve, no published price.** Almost certainly a **flat monthly platform contract, not per-site (UNKNOWN exact figure).**

### 6.2 Who-pays-what by model

| Model | Who holds the key | Who pays Patchstack | Cost shape | Lands on | vPatch delivered? | Feed access? |
|---|---|---|---|---|---|---|
| **Status quo (Wordfence)** | WPMgr (instance-global) | nobody | **$0** | — | No | Yes (free full dump) |
| **BYO App-API key (co-existence)** | Each tenant | **The tenant** (Personal $5/site or Developer $69–79/mo) | per-site or per-plan on **tenant's** bill | **Tenant** | Yes — by Patchstack's own plugin (WPMgr only *reads* status) | **No** (App API is not a feed) |
| **WPMgr master feed key (hosted)** | WPMgr (one Extended contract) | **WPMgr** | **flat custom monthly** platform fee | **WPMgr hosted margin** (on top of ~$470/mo infra floor) | No (feed only) | Yes (Extended/Beta) |
| **for-Hosts partnership (resell)** | WPMgr (partner contract) | WPMgr wholesale (~$2/site implied), resells $3–5/site | wholesale/site, margin on resale | **Split** — WPMgr wholesale cost vs end-customer retail | Yes — resold Patchstack plugin + rules | Yes (partner feed) |

### 6.3 Per-site economics vs $0 Wordfence

- Personal add-on **$5/site/mo**; Developer **~$2.76–$3.16/site/mo** at 25 sites (incremental **$2.50/site/mo**); reseller retail **$3–5/site/mo**, implied wholesale **~$2/site**; feed = **unpublished flat contract**.
- Versus WPMgr's **$0** Wordfence feed, **every Patchstack path adds real cost.** Under the **central feed model**, a flat contract is exactly the kind of fixed cost that **erodes margin on Free/Starter tenants** (WPMgr's per-site cost ≈ $0 today per the unit-economics memo). Under the **BYO/per-site vPatch model**, cost lands on the **tenant's** Patchstack bill, not WPMgr's — but requires the Patchstack plugin per site and gives no feed API.
- **There is no version where Patchstack matches the $0 of the Wordfence feed.**

---

## 7. Integration design mapped to the WPMgr codebase

### 7.1 Current architecture (what exists today)

- **Feed sync:** `vuln.FeedWorker` (`worker.go:101-234`) fetches two hard-coded Wordfence URLs (`worker.go:59-62`), stream-decodes the UUID-keyed root object (`fetchFeed`, `worker.go:284-404`), parses with a Wordfence-typed parser (`parseFeedRecord`, `worker.go:588-699`; `wfRecord`/`wfSoftware`/`wfCVSS`/`wfCopyrights`, `worker.go:461-503`), merges Scanner+Production (`mergeEnrichment`, `worker.go:790-820`), bulk-ingests in one tx (`ingestRecords`, `worker.go:247-279`). Hourly River periodic job, `RunOnStart:false`, deduped by `UniqueOpts` (`main.go:2771-2790`); 429 stamps status and succeeds rather than retrying (`worker.go:69,163-167`); each refresh enqueues a cross-tenant rescan (`worker.go:229`).
- **DB tables (m79, `20260712000000_m79_vuln_scanner.sql`):** `wordfence_vuln_feed` (PK `vuln_id`=Wordfence UUID, **no RLS**, public reference cache), `wordfence_vuln_software` (PK `(vuln_id,kind,slug)`, lookup idx `(kind,slug)`, **no RLS**), `wordfence_vuln_feed_meta` (**singleton id=1**: `fetched_at`, `record_count`, Defiant/MITRE attribution, `last_error`), `site_vulnerabilities` (tenant-scoped, unique `(site_id,vuln_id,kind,slug)`, **RLS ENABLE+FORCE** with `_tenant_isolation` + `_agent`). m81 added reference URLs.
- **Matching:** pure CP-side join, **no agent involvement** (`model.go:4-8`). `Service.RescanSite` (`service.go:78-187`) loads WP version + plugins/themes via `SiteLoader.GetSiteForVuln` (`service.go:20-28`, impl `newVulnSiteAdapter` in `cmd/wpmgr/siteadapter.go` using existing `Site.Components`), calls `repo.LookupSoftware(kind,slug)` (`repo.go:459`), runs `wpversion.IsVulnerable` + `BestFixedVersion`, upserts + resolves stale under `InTenantTx`. `RescanAll` (`service.go:193-252`) fans out per-site River jobs; `RescanSiteWorker` (`worker.go:828-854`, queue MaxWorkers 8).
- **Endpoints:** tenant `GET /api/v1/vulnerabilities`; per-site `/api/v1/sites/:siteId/vulnerabilities` (list, `/rescan`, `/:id/dismiss|restore|remediate`); superadmin `/admin/vuln-feed/{status,key,sync}`.
- **Web:** `apps/web/src/routes/_authed/vulnerabilities.tsx`, `features/security/vuln-panel.tsx` + `use-vuln.ts`, admin `admin/vuln-feed.tsx` + `use-admin-vuln-feed.ts`.
- **Key storage (Wordfence, today):** **INSTANCE-GLOBAL**, in `instance_settings` (m80, `20260713000000_m80_instance_settings.sql`) — key/value, age-encrypted bytea value, **no `tenant_id`**, RLS app.agent-only, superadmin-gated. Key name `"vuln_feed.wordfence_api_key"`. `VulnFeedKeyService` (`admin/vuln_feed.go:135-270`) encrypts via `cryptbox` age identity (`siteDestAgeID`, `main.go:1284`), `ResolveAPIKey` (`vuln_feed.go:187-203`) decrypts in-process at job-run, priority UI-key > env-key > none; plaintext never returned/logged.

### 7.2 The only existing seam, and what an abstraction must add

There is **no source/provider abstraction** — Wordfence is hard-wired at every layer (table names, PK = Wordfence UUID, hard-coded URLs, Wordfence-typed parser, Defiant/MITRE attribution). The **only** existing seam is `APIKeyResolver` (`worker.go:80-99`) — it abstracts the **key**, not the **source**.

A **`VulnSource` provider interface** should abstract exactly the Wordfence-coupled layer (fetch + parse), because the internal representation is already provider-neutral:
1. `FetchNormalizedFeed(ctx, apiKey) → []FeedRecord, []SoftwareRow` — returns the **already-normalized** internal types (`FeedRecord`, `VulnSoftware`, `model.go:60-81`).
2. `Provider()` identity string (`"wordfence"|"patchstack"`).
3. Per-provider **attribution metadata**.
4. Per-provider **affected-version adapter** — Patchstack's `affected_in` string range vs Wordfence's from/to objects; only `parseAffectedVersions` (`service.go:359-385`) + the range builder need a provider branch. **The matcher (`wpversion.IsVulnerable`) is untouched.**

### 7.3 Dedup with Wordfence — net-new design

Merging two providers reporting the same CVE is **not possible in the current schema.** `vuln_id` is provider-specific (Wordfence UUID vs Patchstack int), so the same CVE lands as two rows and generates **two findings** on a site (unique key `(site_id,vuln_id,kind,slug)` — different `vuln_id` ⇒ no collision). To dedup you need:
- a **`provider` column** on the feed *and* on `site_vulnerabilities`;
- a **canonical dedup key** — but **CVE is nullable** (many WP vulns have no CVE), so CVE-only dedup is insufficient; fall back to `(kind, slug, affected-range)` heuristics;
- a **merge-precedence policy** (e.g., prefer the provider with a `fixed_in`, or union both with **source badges** in the UI).

**Recommendation:** for Phase 1, **do not attempt automatic cross-provider merge.** Tag every finding with `provider` and render **source badges** (union with dedup deferred). This avoids a fragile CVE/range heuristic while keeping the door open.

### 7.4 Key storage — two patterns already exist; pick by decision

- **Instance-global (mirrors Wordfence exactly):** add a new key name `"vuln_feed.patchstack_api_key"` to `instance_settings` (m80) — **no schema change** — plus a `PatchstackKeyService` clone of `VulnFeedKeyService` with `ResolveAPIKey`. This fits the **master-key** model.
- **Per-tenant BYO-key (mirrors SMTP/CDN):** follow **`site_email_config`** (m59, `20260622000000_m59_site_email.sql`) — the established per-tenant encrypted-secret precedent: `tenant_id NOT NULL` + nullable `site_id`, a `provider` text column, `provider_secret_encrypted` bytea (age-encrypted, never returned — only `secret_set: bool` surfaced), partial unique index for an org-wide-default row plus per-site overrides, **RLS = tenant_isolation (`app.tenant_id` GUC) + agent policies.** Analogous encrypted-secret code: `sitedestination/model.go` (`SecretKeyEnc`→`SecretKeyPlain` at use-site) and `smtp_settings.password_enc` (m30). Decrypt-in-process at feed-sync time via the same `cryptbox` age identity.

Given §5, the **feed** key is master-key (instance-global). The **BYO App-API key** for co-existence follows the **per-tenant `site_email_config`** pattern (tenant-scoped, `secret_set` surfaced, tenant Security-settings UI).

### 7.5 Feed-sync worker

- **Master-key feed:** clone the Wordfence single-fetch shape exactly — a `PatchstackFeedWorker` / `FeedRefreshArgs` analog (`worker.go:24-31`), registered like `main.go:2771-2790`, writing **provider-tagged** records. Adapt for `/all` cursor paging (or `/latest`+`/batch` polling); replicate the clean no-op when no key is configured (`worker.go:146-151`) so Patchstack is **off-by-default**.
- **BYO co-existence (App API):** this is **not a feed sync** — it's a per-tenant **status pull**. A per-tenant worker iterates tenants with a configured App-API key, calls the App API for their enrolled sites' vuln/vPatch/protection status, and writes a *read-only* status view (not into `site_vulnerabilities` as independent findings unless clearly provider-tagged and non-authoritative). Fan-out is **per-tenant** (resolve+decrypt each tenant's key).

### 7.6 New schema (Phase 1)

1. **`provider` column** on the feed record table **and** on `site_vulnerabilities` (finding uniqueness must include provider). Two options:
   - **Generalize** `wordfence_vuln_feed → vuln_feed` with a `provider` column and composite PK `(provider, vuln_id)` — cleaner for dedup, **bigger migration** (every repo query — `BulkUpsertFeedRecords`, `LookupSoftware`, `PruneMissingVulns` — assumes the Wordfence-keyed shape).
   - **Parallel tables** `patchstack_vuln_feed` / `patchstack_vuln_software` — smaller blast radius, but the rescan join must span both software indexes.
   **Recommendation:** parallel tables for Phase 1 (lower risk); revisit generalization if/when dedup is prioritized.
2. **`vpatch_available` boolean** (+ optional patch-id/URL) on the feed record — surfaceable in the UI ("a virtual patch exists") even in feed-only phase. **Caveat:** the Threat Intelligence API does **not** expose this flag; it is only observable via the **App API** for protected sites — so `vpatch_available` is only populatable in the **co-existence** path, not the pure feed path (UNKNOWN whether a partner feed exposes it — confirm).
3. **Key storage:** master-key ⇒ new `instance_settings` key (no schema change); per-tenant ⇒ new tenant-scoped settings table on the `site_email_config` pattern.
4. **`feed_meta` must become per-provider** — today it's a **singleton id=1** (`m79:132-133`). Make freshness/`record_count`/`last_error`/attribution a **per-provider row** so each source is tracked independently. Extend attribution storage to hold **Patchstack's own attribution/license text** (legally load-bearing, rendered per-view).

### 7.7 Config flag + web surface

- **Config:** boolean to enable the Patchstack provider (env for master-key; tenant/instance setting for BYO). Reuse the clean no-op-when-unconfigured behavior.
- **Web (master-key):** extend `admin/vuln-feed.tsx` with a **provider toggle + Patchstack key field** (write-only, same UX as the Wordfence card).
- **Web (BYO co-existence):** a new **tenant Security-settings** surface — enable toggle + BYO App-API-key field + `secret_set` indicator, following the site-email settings UI.
- **Fleet + per-site pages** (`vulnerabilities.tsx`, `vuln-panel.tsx`) render findings the same way but show a **per-finding source badge** once findings carry `provider`.

### 7.8 CP-only vs agent release

- **Phase 1 (feed / co-existence): CP + web only. NO agent release.** Detection is a pure CP-side join against inventory the CP already holds; the agent's existing `refresh_inventory` keeps `Site.Components` fresh (`model.go:4-8`: "No agent change is required"). Adding a provider changes only *what the CP matches against*.
- **Phase 2 (vPatch): the ONLY part that touches the agent**, and it is explicitly deferred. Realistic path = a new agent command to **install/activate/configure the Patchstack mu-plugin** (keyed by the tenant's Patchstack license) with WPMgr orchestrating install + key + reporting. **Do NOT** build a request-level WAF in the PHP agent — that re-implements Patchstack's proprietary engine (rule format, request hooks, performance, false-positive risk, security review, questionable licensing/value). Either Phase-2 path requires a new agent release **and a mandatory security review** (per the security-suite precedent where every phase's review caught real blockers).

---

## 8. Risks, open questions, licensing/IP, ToS

**Make-or-break / high-impact**
- **Feed is contact-sales-gated (confirmed).** No self-serve individual Threat Intelligence key; bulk `/all` is Beta/partner-only. → per-tenant BYO feed is impossible; master-key requires a for-Hosts/Extended contract. **This alone means "add Patchstack as a feed" is a partnership decision, not an engineering decision.**
- **Pricing is entirely unpublished** for the feed and for-Hosts (Extended = "custom, activated on request"; wholesale/rev-share never stated). Every Patchstack path adds cost vs $0 Wordfence; a flat feed contract erodes Free/Starter margin. **UNKNOWN — must obtain from Patchstack sales.**
- **ToS / redistribution UNKNOWN.** Whether an integrator may operate per-tenant keys, and whether feed data may be re-displayed in WPMgr's UI, is not in public docs — governed by the partner contract. **Must confirm before any master-key re-display.**

**Attribution / legal**
- WPMgr already carries **hard, per-view attribution** for Wordfence (Defiant + MITRE, `model.go:10-17`, `m79:31-38`, UI footer). **Patchstack's attribution/license text must be reviewed, stored (per-provider `feed_meta`), and surfaced the same way.** Assume it is load-bearing.
- **Clean-room / house rules:** if any Patchstack-derived logic ships into agent/plugin source, do **not** name competitor plugins as technique sources in shipped comments; describe techniques neutrally.

**vPatch-specific**
- **No documented agent-less host-injection** (`auto_prepend_file`/global mu-plugin) — enforcement always needs the Patchstack plugin per site (**UNKNOWN** whether a custom for-Hosts rollout offers otherwise). So resold vPatch means coordinating **their** plugin, not WPMgr applying rules.
- **No `vpatch_available` flag on the Threat Intelligence feed** — only observable via App API for protected sites. A "virtual patch exists" badge is only truthful in the co-existence path unless a partner feed exposes it (**UNKNOWN**).

**Schema / data-model**
- Dedup with Wordfence is **net-new**; CVE is nullable so CVE-only dedup fails → heuristic fallback. Recommend **badges, not auto-merge**, in Phase 1.
- **ClickHouse-install caveat** (per `wpmgr-clickhouse-metrics-backend` memory): any new read path must not hit Postgres-only tables directly on ClickHouse installs — keep vuln reads on the existing repo layer.
- **sqlc discipline** (per memory): any new column/query ⇒ run pinned `sqlc generate` (v1.31.1), never hand-edit `*.sql.go`; keep the diff to real query files.
- **RLS on any new tenant table** is mandatory (per feature-build-pitfalls memory) — the per-tenant BYO-key table must ship `tenant_isolation` + agent policies from day one.

**Operational**
- **No native `since` param** → incremental sync is poll-and-dedupe convention; plan the worker accordingly.
- **Two response schemas** (Extended lean vs Beta rich) — CWE/references only in Beta `/all`; if WPMgr only has Extended access, findings are **poorer than the current Wordfence records** (lose CWE + references). Confirm which tier the contract grants.

---

## 9. Phased build plan + LOCKED-decisions-needed

### Phase 0 — Provider abstraction (do regardless; unblocks everything)
- Introduce the `VulnSource` seam in `internal/vuln` (fetch+parse only): `FetchNormalizedFeed`, `Provider()`, per-provider attribution, per-provider affected-version adapter. Wordfence becomes the default `VulnSource` implementation with **zero behavior change** (refactor-only; matcher and repo untouched).
- Add a `provider` column to `site_vulnerabilities` (default `'wordfence'`) and make `feed_meta` per-provider. Regen sqlc; ship RLS-safe migration; verify regen no-op.
- **Value:** clean seam, source badges become possible, no dependency on any Patchstack decision. **CP + web only, no agent.**

### Phase 1a — BYO App-API-key co-existence (recommended first real integration)
- Per-tenant encrypted key storage on the `site_email_config` pattern (tenant Security settings, `secret_set` surfaced, `cryptbox` age).
- Per-tenant status-pull worker against the **App API** for the tenant's enrolled sites; render Patchstack protection/vPatch status + a `vpatch_available` badge **alongside** Wordfence findings, clearly provider-tagged and non-authoritative.
- Works **self-host + hosted**; **zero WPMgr Patchstack cost** (tenant pays Patchstack). **CP + web only, no agent.**

### Phase 1b — Master-key feed (only if a for-Hosts/Extended contract is signed; hosted-only)
- Instance-global key in `instance_settings` + `PatchstackKeyService`; `PatchstackFeedWorker` cloning the Wordfence single-fetch shape (adapted to `/all` cursor or `/latest`+`/batch`); parallel `patchstack_vuln_*` tables; provider-tagged findings + source badges (no auto-merge).
- Admin `vuln-feed.tsx` provider toggle + Patchstack key card. **Gated behind WPMGR_HOSTED.** **CP + web only, no agent.**

### Phase 2 — vPatch (deferred; only on demonstrated demand; needs agent release + security review)
- **Orchestrate Patchstack's mu-plugin** (new agent command: drop-in/activate/configure, keyed by tenant license) + reporting. **Never** build a WPMgr WAF.
- Requires a for-Hosts partnership (resale terms, white-label depth), a new agent release, and a **mandatory security review**.

### LOCKED-decisions needed from the user (blocking)

1. **Feed-only vs vPatch — confirm Phase 1 is report/status-only and vPatch is deferred.** (Recommend: yes.)
2. **BYO-key vs master-key:**
   - Feed: **master-key only** (BYO on the feed is impossible). Confirm we will **not** pursue the master-key feed until a for-Hosts/Extended contract exists.
   - Co-existence: confirm we ship **BYO App-API-key** (per-tenant, `site_email_config` pattern) as the first integration.
3. **Additive vs replace:** confirm Patchstack is **additive with source badges** (no auto-dedup/merge with Wordfence in Phase 1), keeping Wordfence as the $0 baseline.
4. **Hosted-only vs self-host:** confirm **master-key feed = hosted-only (WPMGR_HOSTED)**; **BYO co-existence = both**.
5. **Go/no-go on contacting Patchstack sales** to obtain the UNKNOWNS that gate any feed/vPatch work: (a) Extended/for-Hosts **pricing** + whether it's flat or per-site; (b) **ToS on operating per-tenant keys and re-displaying feed data**; (c) which **schema tier** (Extended vs Beta `/all`, i.e., whether we get CWE/references + `vpatch_available`); (d) whether a **server-level host-injected vPatch** (no per-site plugin) exists as a custom rollout; (e) **rate limits** per contract.
6. **Attribution/licensing sign-off:** confirm legal will review Patchstack's attribution/license terms and that we will store+render them per-provider exactly as we do Defiant/MITRE.

**Honest closing read:** the vuln **data** is parity with what WPMgr already gets free. The only reasons to spend here are **active protection (vPatch)** and **earlier mVDP timing** — and the only self-serve, self-host-friendly, zero-WPMgr-cost way to touch either today is the **BYO App-API-key co-existence** path. Everything richer (a real feed, resold vPatch, white-label) is a **contact-sales for-Hosts partnership** with **unpublished pricing that lands on WPMgr's margin**, and should not be built until that contract's terms are known.
---

## 10. AMENDMENT (2026-07-27) — the blocker is contractual, not commercial

Appended after the security-expansion research of 2026-07-27 (see `security-expansion-research-2026-07-27.md`). The decision trail above is preserved deliberately; this section corrects it rather than rewriting it.

**§5's core conclusion is CONFIRMED and still correct:** there is no self-serve BYO feed key. Two material corrections follow.

### 10.1 The blocker is CONTRACTUAL. Money does not solve it.

This document treats the obstacle as commercial (contact-sales gating, unpublished pricing landing on WPMgr's margin) and therefore as something a signed contract would unlock. That framing is wrong. §9 item 5(b) correctly listed "ToS on operating per-tenant keys and re-displaying feed data" as a blocking unknown. **That unknown is now answered, and the answer is prohibitive.**

Governing document, verified 2026-07-27: `https://patchstack.com/terms-and-conditions/` (titled "Patchstack Terms of Service", Patchstack OU, Estonia). Note `https://patchstack.com/terms-of-service/` returns HTTP 404, so any earlier citation to that URL is broken.

Under the PUBLIC terms, every core WPMgr use is prohibited:

- **§3.2(d)** forbids use "by or for the benefit of any third party or by any direct or indirect competitor of Patchstack." An agency using WPMgr to protect CLIENT sites is definitionally third-party benefit.
- **§3.2(e)** forbids use "to create, maintain, support, or enhance a competitive or substitute service, product, or offering."
- **§3.5** defines Service Deliverables to expressly include "vulnerability and mitigation data" and requires that Customer "shall not disclose Service Deliverables to any third party." Rendering an advisory in a tenant's dashboard is that disclosure.
- **§3.1** grants only a "non-sublicensable, non-transferable" licence "exclusively in support of Customer Operations", and the preamble states the Solution is "not to protect the websites, platforms, or operations of any third party."
- **§7.3** requires destroying all copies of Service Deliverables on termination, which is structurally incompatible with a cached Postgres corpus (exactly how the vuln domain works).

A standard paid subscription would still prohibit all of the above. **Only a separately negotiated for-Hosts/Enterprise agreement expressly overriding §3.1, §3.2, §3.5 and §7.3 unlocks anything.** §9 item 5 should be read as: do not contact sales expecting price discovery to resolve this; the first question is whether they will contractually override four clauses.

Do not rely on the §1.2 "owned or operated by Customer" definition as a workaround for agencies. It is in direct tension with the preamble's third-party exclusion, and betting the product on resolving that ambiguity in our favour is not sound.

### 10.2 The App API entry needs its stated basis and cost corrected

The recommendation (BYO App-API-key co-existence as the only clean path) SURVIVES, but two supporting facts in this document are wrong:

1. **The permission quote is gone.** The cited sentence "Patchstack App API can be used commercially for building custom tools and integrating third party platforms" has **zero occurrences** on `https://docs.patchstack.com/api-solutions/app-api/patchstack-app-api/` as of 2026-07-27. Do not cite it. The permission is now only IMPLIED, though the current docs do corroborate the intended shape through their own example use cases (integrating Patchstack inside your own product so customers control it from your platform).
2. **It is not free.** This document calls the App API key "free with a Developer plan." Current docs state the App API is "available for the Developer and Enterprise plan users." An API gated behind a **$79/mo minimum** (25 sites; $69/mo annual) is not free. The claim that this path "costs WPMgr nothing" remains true only because the **tenant** bears the $79/mo, and that cost must be stated explicitly when the feature is pitched, because it materially shrinks the addressable set of tenants.
3. **"Unlimited requests / no rate limiting" is UNVERIFIED.** No such statement appears on the current App API docs. Do not plan against it.

### 10.3 Net position

Unchanged in direction, firmer in reasoning: **do not ingest Patchstack data**, keep Wordfence Intelligence as the $0 default (its terms were verified clean: no non-commercial clause, no share-alike, no non-compete), and treat the BYO App-API-key co-existence panel as a late, optional retention nicety rather than a strategy.

The strategic answer to Patchstack's vPatch moat is not to reproduce it. It is that **guarded auto-patch with verified rollback is a better remedy than a virtual patch whenever a fix exists**, and it is buildable from machinery already in this repo with no third-party licence at all.
