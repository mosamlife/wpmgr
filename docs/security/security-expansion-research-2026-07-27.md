# Security expansion research: auto-patching, malware detection, AI analysis

**Date:** 2026-07-27
**Status:** Research complete, decision-grade. Supersedes nothing; amends `patchstack-integration-research-2026-07-05.md` (see §6).
**Method:** 12-agent workflow, 6 parallel investigation tracks (codebase + 5 web research), 3 design agents, 2 adversarial verifiers (licensing, feasibility), 1 synthesis. All licensing claims verified against primary sources.

---

## 0. CRITICAL FINDING: plugin vulnerability matching is silently dead

**Severity: critical. Ship the fix before anything else in this document.**

Plugin vulnerabilities have never matched. The vuln scanner reports theme and core findings only. Since plugins are the overwhelming majority of WordPress vulnerabilities, the feature is effectively non-functional.

Verified chain, no normalization at any hop:

| Step | Location | Value |
|---|---|---|
| Agent emits | `apps/agent/includes/commands/class-metadata-command.php:322` | `'slug' => $file` = the `get_plugins()` array key, e.g. `woocommerce/woocommerce.php` |
| Stored | `sites.components` JSONB | raw |
| Decoded | `apps/api/internal/site/model.go:208` `ParsedComponents()` | pure `json.Unmarshal`, no normalization |
| Adapted | `apps/api/cmd/wpmgr/siteadapter.go:474` | `Slug: p.Slug`, raw |
| Matched | `apps/api/internal/vuln/service.go:115` | `item{KindPlugin, p.Slug, ...}` |
| Looked up | `apps/api/internal/vuln/repo.go:634` | `normSlug()` = `strings.ToLower()` **only** |
| Query | `repo.go:641` | `WHERE s.kind='plugin' AND s.slug='woocommerce/woocommerce.php'` |
| Feed stores | `wordfence_vuln_software.slug` | `woocommerce` |

Result: **zero rows, always.** Themes work (`class-metadata-command.php:372` sends the stylesheet directory, which is canonical). Core works (hardcoded `wordpress`).

**Fix:** derive the canonical slug CP-side (take the path segment before the first `/`, i.e. `strings.Cut(slug, "/")`) inside `normSlug`, so both ingest and lookup keys agree. CP-only, no agent release. Must be applied on the lookup path; ingest already receives canonical slugs from Wordfence.

**Required guards, both in the same PR:**
1. A test with a real fixture (`woocommerce/woocommerce.php` + a known WooCommerce advisory) asserting a match. The absence of such a test is how this survived.
2. A backfill: existing `site_vulnerabilities` rows carry directory-form slugs, so a rescan-all is needed after deploy.
3. Consider a canary metric: findings-by-kind counts, so a future regression to zero plugin findings is visible.

**Process lesson:** this is a contract-vocabulary collision between two domains that never agreed on what "slug" means. The update domain has the same ambiguity (see §5, auto-patch blocker #1). Pin the vocabulary explicitly before building anything that joins these domains.

---

## 1. The headline question: is there an open-source Patchstack?

**Partly. The distinction decides the architecture.**

### Data layer: yes, and you already have the best one

| Source | License | Coverage | Verdict |
|---|---|---|---|
| **Wordfence Intelligence V3** (in use) | Verified: no NC, no share-alike, no non-compete. §3.1 grants perpetual, irrevocable rights to reproduce, sublicense, distribute. | 37,791 records, WP core+plugins+themes, Wordfence is a CNA | **Keep as default.** Correct choice. |
| **WPVulnerability** (wpvulnerability.com) | **EUPL-1.2**, AGPL-compatible, no API key at all | ~48,091 vulns, 16,364 plugins, 2,283 themes; also covers PHP/nginx/MySQL | **Add as second source + zero-key self-host fallback.** CP-side only (weak copyleft must never touch the MIT agent). |
| WPScan | "Permanent storage not permitted", "caching not permitted" | good | **Eliminate permanently.** Architecturally incompatible with a cached CP corpus. |
| OSV / CVE / NVD | open | **No WordPress ecosystem** (verified: OSV returns "invalid ecosystem") | Enrichment only (CVE ID, CWE, CVSS). Never a WP source. |

### Rule layer (virtual patching): no, and nothing close

Patchstack's 12,000+ vPatch rules are proprietary and server-side. Their WordPress.org plugin is GPL; **the rules are not in it.** No dump, no mirror, no community reimplementation.

ModSecurity/OWASP CRS (Apache-2.0) is **not a substitute** — it is generic HTTP attack grammar (SQLi/XSS shapes), not per-CVE WordPress virtual patches. Marketing it as vPatch parity would be false.

**Two framing errors to kill internally:**
1. Patchstack's phrase "Open Source Vulnerability Database" means *a database of vulnerabilities in open-source software*. It does **not** mean the database is openly licensed. This is the direct cause of the "just build on the open-source Patchstack" hypothesis.
2. OSV/CVE/NVD have no WordPress ecosystem, so they cannot serve as an open aggregator layer either.

**Conclusion:** stop looking for a vPatch corpus to fork. The detection substrate must be **provenance + structure + statistics + fleet correlation** — all generatable from data already held.

---

## 2. Licensing: verified, and mostly favourable

### Wordfence Intelligence (the feed already shipped) — CLEAN

Full-document read of the Terms and Conditions (last updated 2026-01-26). §3.1 grants, verbatim: *"Subject to this Agreement, Company hereby grants you a perpetual, worldwide, non-exclusive, no-charge, royalty-free, irrevocable license to reproduce, prepare derivative works of, publicly display, publicly perform, sublicense, and distribute the Service."*

**No non-commercial clause. No share-alike. No non-compete.** Exactly what an AGPL self-host build plus a hosted SaaS needs.

**Two live compliance items:**
1. **Per-record hyperlink.** The per-record Defiant license requires any copy to *"include a hyperlink to this vulnerability record."* Currently satisfied only indirectly by passing through `references[]`. Construct a deterministic per-record link and render it on every vulnerability view.
2. **API key handling.** §2.2(1) requires keeping the key confidential and not sharing it; §2.2(2) forbids transfer/sale/sublicense; §2.1 requires an active account in good standing. Hosted must hold its own key. **The self-host image must never embed or default to a WPMgr-held key.** (Current behaviour already degrades cleanly with no key: `vuln/worker.go:247`.)

**Concentration risk, rated higher than prior research:** §2.3 permits revocation *"at any time, with or without notice... in Company's sole discretion"*, §3.2 reserves unilateral rate limiting/throttling/termination, §5.3 permits unilateral term changes, and **v2 now returns HTTP 410** so there is no unauthenticated fallback. Mitigation: the §3.1 grant is *perpetual and irrevocable*, so **the corpus already ingested stays licensed** even if access is cut — make the cached corpus durable and degrade the UI to "feed stale since X" rather than empty. Then add WPVulnerability as a second source.

### The trap: Wordfence CLI malware signatures — HARD NO

Same vendor, opposite outcome, different agreement. §3.4(4) forbids use *"in a computer service business... on a service bureau basis... as part of a hosted service, or on behalf of any third-party"* and §5.4(5) forbids making API-derived information *"available to third parties in any manner."* An agency fleet manager scanning client sites and showing results to tenants violates both. **Do not bundle, shell out to, or proxy it.** Do not let the permissive vuln-feed terms bleed across.

### Patchstack — ingestion is contractually prohibited

Governing document: `https://patchstack.com/terms-and-conditions/` (note: `/terms-of-service/` 404s). Every core WPMgr use is blocked:

- **§3.2(d)** forbids use *"by or for the benefit of any third party"* — an agency protecting client sites is definitionally that.
- **§3.2(e)** forbids use *"to create, maintain, support, or enhance a competitive or substitute service."*
- **§3.5** requires Customer *"shall not disclose Service Deliverables to any third party"* — rendering an advisory in a tenant dashboard is that disclosure.
- **§7.3** requires destroying all copies on termination — incompatible with a cached Postgres corpus.

**Money does not solve this; only a bespoke contract does.** A paid standard subscription still prohibits all of the above.

### Malware rule corpora (for §4)

| Corpus | License | Usable? |
|---|---|---|
| Panelica/malware-signatures | **MIT** | Cleanest. 154 YARA/JSON patterns. **1 commit total** → seed corpus, never a dependency. |
| php-malware-finder | **LGPL-3.0** | CP-side only (AGPL-compatible, MIT-agent-incompatible). Best *technique* reference. |
| Neo23x0/signature-base | **DRL 1.1** post-2021 | Usable with attribution **retained in rule-match messages** (finding row, DTO, email, webhook, exports). **Everything before 2021-08-13 is CC-BY-NC — poison. Pin a post-2021 tag.** |
| ClamAV | **GPL-2.0-only** | Incompatible with AGPL-3.0 for combined works. Exclude. |
| YARA-X Go bindings | BSD-3-Clause | Clean, but requires CGO. Gate behind a build tag with a pure-Go regex default. |

---

## 3. Where WPMgr actually stands

The README is **stale and materially under-sells shipped capability.** `README.md:354` claims "the scan engine covers WordPress core checksums only." Verified false: `fetchAllPluginChecksums` (`scan/worker.go:466`) already fetches wp.org plugin **and theme** manifests, and `diffFiles` (`:730`) already emits `plugin_modified`/`plugin_unknown`. Fix this line first.

Likewise `vuln/model.go:8` claims "a completed update triggers an immediate rescan." **It does not** — the only `EnqueueRescanSite` callers are `handler.go:295` and `service.go:212,250`.

### The real gaps, ranked

1. **No malware/webshell content detection** (critical) — zero signature or heuristic code exists. The benchmark's lead feature.
2. **No remediation** (critical) — the entire finding action surface is `IgnoreFinding` + `FetchFile`. WPMgr can say you are compromised and then do nothing.
3. **No automatic patching** (high) — but the execution engine is **100% built**; only policy is missing.
4. **No scan scheduler** (high, 1 week) — the River registry (`main.go:2855`) holds 27 jobs including scan GC, but **nothing that runs a scan.** Every competitor scans continuously; WPMgr scans when a human clicks.
5. **Vuln-to-update loop not closed** (high, 2-3 days).
6. **No rogue-admin / stealth-persistence detection** (high).
7. **Detect-but-cannot-repair** on integrity findings (high, 1 week).
8. **MD5-only trust chain** (high, 2-3 days) — `checksums.go:263` discards the SHA-256 wp.org already returns. Adopt: **MD5 is a negative filter only**; positive trust requires a CP-computed hash.
9. **No AI analysis** (medium) — build last.
10. **No virtual patching** (medium) — explicit non-goal, see §1.

---

## 4. Recommended architecture

### Auto-patching — the highest-ROI item

The execution engine already exists: `update/worker.go:213-306` does snapshot → apply → health probe (immediate + {3s,4s,6s,8s} retry) → **automatic rollback**. The pinned branch at `update/service.go:196` was literally built for `vuln.Remediate`.

Build `internal/autopatch` as a **policy/claim/decision driver in front of the existing `CreateRun`**. Zero agent change, zero WordPress.org review exposure.

**Blockers found by feasibility verification (must fix first):**
- **Slug collision** (§0) — blocks this entirely.
- **The agent ignores the version pin on its fallback path.** `applyViaUpgrader` (`class-update-runner.php:834`) takes `$version` then ignores it for plugins/themes, calling `wp_update_plugins()` and upgrading to whatever the transient says. Unattended pinning needs an agent fix (inject the pinned package, or `upgrader_pre_download` at `downloads.wordpress.org/plugin/{slug}.{version}.zip`).
- **The claim must be a lease, not a one-shot stamp.** Gates evaluated after claiming would permanently drop deferred patches. Either gate before claiming, or add `patch_retry_after`.
- **The health check is weaker than credited.** `ProbeResult.Healthy()` (`agentcmd/client.go:802`) treats 401/403/404/410 as healthy, and `Probe.Get` is one plain GET of the root with no cache-buster. A `strict` profile needs: cache-buster, body-length floor with a pre-patch baseline, positive-content assertion, and 4xx treated as unhealthy.
- **Map the three `CreateRun` domain errors** (`targets_in_flight`, `no_tasks`, `no_targets`) to ledger codes and do not burn attempts on them.

**For the ~13,126 vulns with no vendor patch:** ship an honest **mitigation lane** (isolate/disable the component + alert), not a counterfeit vPatch.

### Malware detection — provenance-first ladder

Architecture: **hybrid.** The agent emits **numbers only, never strings**; the CP owns all rules. This keeps signature literals out of the MIT plugin entirely, which is non-negotiable during the active WordPress.org review.

- **T0 provenance** (largest win): auto-clear wp.org-verified files so nothing downstream ever spends on them. Roughly half is already shipped.
- **T1 structural**: PHP under uploads, dotfile PHP, drop-in tampering across all eleven `_get_dropins()` entries with **anchored exact-path matching** (the GH #147 rule), mu-plugin enumeration, timestomp as a *local* anomaly vs directory siblings (not an absolute threshold — a normal `wp plugin install` of a 6-month-old release yields a 180-day ctime−mtime delta).
- **T2 statistical**: entropy, index-of-coincidence, b64/hex ratios. Hard caps required: 64 KiB per file via `count_chars($buf, 1)`, matching the existing `CONTENT_SNIFF_BYTES` precedent.
- **T3 content**: CP-side rules, PHP-token pass first so hits in comments/string-literals are downgraded and can never auto-remediate (this is the GH #266 trap — a scanner flagged our own example payload in a docblock).
- **T3.5 fleet correlation**: identical unknown-provenance hash across N unrelated tenants raises confidence with no rule at all. **Structurally impossible for competitors with per-site agents.** Must be poison-resistant: weight by tenant age/payment/enrollment recency, since open signup makes standing up 5 tenants trivial.

**Do not chase SSH-agentless.** It abandons the shared-hosting majority the agent exists to serve. Mitigate the "agent inside the blast radius" problem structurally: CP-side judgment, CP-computed hashes, DB divergence detection, fleet correlation, hash-chained audit.

### DB / persistence detection — the standout technique

**Divergence detection needs no threat intelligence at all:** compare raw `$wpdb` truth against WP API truth (`get_users`, `wp_load_alloptions`, `get_plugins`). Any mismatch **proves** a `pre_user_query`/`views_users`/`all_plugins` filter is hiding something. Deterministic tamper proof, not a heuristic.

Malicious-cron detection is free corpus reuse: `plugin_signatures.cron_hook_patterns` (m40, 152 rows) already exists from the DB-cleaner.

**Constraint:** do not put this in the synchronous ACK path of `class-db-scan-command.php` — its <5s contract is load-bearing. Page it or give it a resumable cursor.

### AI — last, adjudication only

Build `internal/ai` as a layer that **only ever sees what the deterministic tiers could not decide.** `provider_null` is the default in **both** builds. May raise confidence and write the human explanation; **may never be the sole basis for a remediation.**

Provider abstraction (Anthropic + one OpenAI-compatible adapter covering Ollama/vLLM/LM Studio) gives self-hosters full functionality via a local model with no vendor relationship. Content-hash caching so the same file is never analyzed twice fleet-wide. Ship the eval harness with a measured FP rate in the same PR as the first provider.

Table split is the privacy design: `ai_verdicts` global (hash + judgment about code only, **no free text ever**), `ai_finding_verdicts` tenant-scoped with RLS for all customer text.

---

## 5. Roadmap

| Phase | Ships | Effort |
|---|---|---|
| **0** | **Fix the slug collision (§0).** Fix README:354 and vuln/model.go:8. Per-record Wordfence link. Store the discarded wp.org SHA-256. Add `ctime` to the fileStat key list. Amend the Patchstack doc. | 3-5 days |
| **1** | Per-tenant scan scheduler with jitter + concurrency caps. Chain `RefreshInventoryWorker` completion → `EnqueueRescanSite` (off run completion, not per-task, to dodge the 30s debouncer). | 1 week |
| **2** | **Guarded auto-patch.** 2a: policy tables + pure `Decide()` + read-only dry-run preview. 2b: sweeper + claim lease + ledger, default disabled, canary-tag required. 2c: strict health profile, circuit breaker, no-fix mitigation lane. | 4 weeks |
| **3** | Provenance + structural detection (T0+T1), `malware_findings` with a surface discriminator. | 3-5 weeks |
| **4** | Reversible remediation. **Restore-from-wp.org first** (deterministic, verifiable, no judgment call), then quarantine, rogue-admin demote, app-password revoke. All off by default, audited, 1-click revertible. | 3-4 weeks |
| **5** | DB/persistence detection: divergence, hidden admins, REST backdoors, malicious cron, SEO spam. | 2-3 weeks |
| **6** | Statistical + content analysis (T2+T3), CP-side rule corpus with per-ruleset license/attribution columns. | 4-6 weeks |
| **7** | Fleet correlation + WPVulnerability as second vuln source. | 2-3 weeks |
| **8** | AI adjudication. | 4-6 weeks |
| **9** | Optional: Patchstack BYO-key read-only co-existence panel. | 1-2 weeks |

**Migration numbers must be assigned up front in ship order (m106/m107/m108)** — all three designs independently reserved m106, which is the boot-blocking failure mode this project has already hit twice.

---

## 6. Amendment to `patchstack-integration-research-2026-07-05.md`

That document's §5 conclusion (no self-serve BYO feed key exists) is **confirmed and still correct**. Two corrections:

1. **The blocker is contractual, not commercial.** The 2026-07-05 doc framed it as access/pricing. It is not, and it does not open up if you pay. Only a bespoke partner agreement expressly overriding §3.1/§3.2/§3.5/§7.3 unlocks anything.
2. **The App API entry needs its basis rewritten.** The quoted permission *"can be used commercially..."* has **zero occurrences** in current docs (verified 2026-07-27) — do not cite it. And it is **not free**: it requires the **$79/mo Developer plan** (25 sites). The recommendation survives (current docs' own example use cases bless embedding Patchstack in your product), but the stated basis and cost assumption were both wrong.

---

## 7. The strategic read

Do not try to out-signature Wordfence or out-VDP Patchstack. The defensible wedge is already in the repo and nobody in the field has it:

> **WPMgr is the only product that can auto-patch and then undo itself.**

Patchstack's free tier auto-updates vulnerable software with no snapshot and no rollback. WPMgr's update worker already does snapshot → apply → health probe → automatic rollback, including the site-wide-PHP-fatal case.

The answer to "there is a vulnerability" should not be "we blocked the exploit." It should be **"we patched it, verified the site still works, and rolled back automatically if it didn't"** — a better outcome than a virtual patch whenever a fix exists, buildable from machinery already shipped, carrying no third-party licence at all.

Second wedge: **fleet correlation**. Cross-tenant hash sightings need no corpus and are structurally impossible for any competitor with per-site agents and no fleet view.
