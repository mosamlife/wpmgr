# WP Remote Security Modules — Gap Analysis & Build Proposal for WPMgr

**Scope:** Security Firewall (WAF), Malware/file-integrity scanning, PHP Error monitoring, plus secondary Login Protection (brute-force) and Two-Factor Auth. Source basis: the vendored WP Remote (MalCare-derived) plugin at `/Users/mosamgor/Desktop/Terminal/wpmgr/wpremote`, compared against WPMgr's Go control plane (`apps/api`), PHP agent (`apps/agent`), and React web (`apps/web`).

---

## 1. Executive Summary

**Firewall / WAF (centerpiece, greenfield — BUILD, phased).** WP Remote's "Protect" module is a server-authored **rule DSL** evaluated by a small AST interpreter on every request, fronted by an **IP block/allow store** (binary-range lookup) and a **brute-force login counter**. The engine is pure, well-bounded (depth-8 type sandbox, constant whitelist, versioned rules), and copies almost 1:1. WPMgr has **nothing** here — no rule engine, no IP store, no request interception, no `fw_requests` log. This is the single largest piece of net-new work but also the highest-differentiation security feature. **Verdict: build, but split into 3 slices** (login-protection + IP store first, request-profiling log second, full rule engine last) so value ships early without waiting on the DSL.

**Malware / file-integrity scan (greenfield — BUILD core-checksum first).** Critically, **WP Remote's plugin contains no local malware engine** — it is a thin data-collection agent that walks the filesystem, stats+md5s files, and **streams everything to blogvault's server** which owns the known-good hash DB and signature catalog. There is nothing to "copy" for the actual diff. WPMgr should **diverge** and do the integrity check **on the agent** (core checksums fetched CP-side from api.wordpress.org and shipped down; only findings travel back), which fits WPMgr's per-site-agent design far better than egressing every file hash. **Verdict: build, agent-side, core-checksum phase first (deterministic, low false-positive), heuristic signatures second.**

**PHP Error monitoring (ALREADY SHIPPED — TIGHTEN, don't rebuild).** WPMgr already has a complete, live feature (ADR-037) that is **architecturally cleaner than WP Remote**: push+cursor transport vs pull+delete, structured columns vs opaque blob, dedup-with-counter vs append-only, true eviction vs hard-stop, plus a bootstrap-time mu-plugin trap WP Remote lacks. **Verdict: do not rebuild.** Port four specific edges WP Remote has: optional 10-frame **backtrace** capture (highest value), server-pushed **error-level mask** + **ignore-list**, and fix one real **occurrence_count drift** correctness bug.

**Login Protection + 2FA (secondary, mostly greenfield — BUILD with LP, defer 2FA).** WP Remote's login protection is a clean time-windowed `COUNT(*)` engine on one table with six tunable thresholds and an `authenticate`-filter block — copies directly and pairs naturally with the firewall IP store. 2FA is standard RFC-6238 TOTP (also copies cleanly) but WP Remote ships **no recovery codes** (admin-lockout risk) and generates secrets server-side. WPMgr only passively records login events in the activity log today. **Verdict: build Login Protection alongside Firewall Slice 1; build 2FA as its own later phase and add recovery codes (a deliberate improvement over WP Remote).**

---

## 2. Feature-by-Feature

### 2.1 Firewall / WAF

#### (a) What WP Remote does — business logic to copy

A ruleset is a **JSON array of rule objects** evaluated by a small AST interpreter (`protect/fw/rule/engine.php`) on every request.

**Rule object (`WPRProtectFWRule_V647`):**
- `id` (int >0), `min_rule_engine_ver` (float, REQUIRED) + optional `max_rule_engine_ver`. Engine `VERSION = 1.2`. A rule is **skipped** if `min_ver > 1.2` or `max_ver < 1.2` — the forward/backward-compat gate so the server can ship rules targeting newer agents. **Keep this gate.**
- `rule_logic` (AST root), `actions` (`[{type: ALLOW|BLOCK|INSPECT}]`), `execute_on` (int 1..17 → `EXE_ON_BOOT` or a specific WP hook: pre_update_option, pre_delete_post, insert_user_meta, delete_user, password_reset, set_auth_cookie, user_register, add_option, wp_pre_insert_user_data, …), optional `opts.variables` and `config`.

**AST node types** (`executeStmt`/`getValue`):
- Logic: `AND`/`OR` (short-circuit), `NOT`, `FUNCTION` (dispatches to `_rf_<name>`, args recursively evaluated, return normalized via `toAllowedType()`).
- Value: `NUMBER`, `STRING`, `BOOL`, `ARRAY`, `HASH_MAP`, `CONST` (resolves a class constant; **strict whitelist** = `['DOING_CRON']` + the bundled `WPRProtectFWRule_V647::SQLIREGEX`/`XSSREGEX` constants).
- **Sandbox boundary:** `toAllowedType()` coerces everything to null/bool/int/double/string/array, walking objects into assoc arrays, with `MAX_DEPTH_TO_ALLOWED_TYPE_FUNC = 8` (null past depth 8). No PHP objects/closures ever reach a rule function. **Copy verbatim.**
- Arg validation: `processRuleFunctionParams(name, argc, args, requiredCount, [pos=>type])` throws `WPRProtectRuleError` on arity/type mismatch.
- Error handling: any `WPRProtectRuleError` in `evaluate()` is caught, recorded as an **error (not a match)**, surfaced in `rule_log.errors` with an `ex_stack` breadcrumb.

**Function library** (`_rf_*`, five files — all pure, copy 1:1): `misc.php` (match/notMatch/matchCount/equals/identical/greaterThan/lengthGreaterThan/md5Equals/isArray/getType/backtrace helpers), `string.php` (isNumeric/isLink/isIpv4/isEmail/isEmbededHtml/isFile/**isPathTraversal** `/\.{2}[\/]+/`/**isPhpEval** `eval(...(base64_decode|exec|file_get_contents|gzinflate|passthru|shell_exec|system)(`), `array.php` (inArray/digArray/arrayIntersection/…), `request.php` (getAction/getPath/getHeader/getPostParams/getIP/getJsonParams/getRawBody/wpUserCan/…), `wp.php` (sanitizeUser/currentUserCan/getOption/checkPasswordResetKey/parseResetPassCookie — guarded by `did_action`). Bundled attack regexes `SQLIREGEX` (MySQL-keyword alternation, >2-match heuristic) and `XSSREGEX` ship as class constants for rule reuse.

**Request lifecycle** (`fw.php init()`): if mode != DISABLED → register shutdown logger, `profileRequest`, set cookies, `blockRequestForBlacklistedIP`, then `handleRequestOnRuleMatch` for `EXE_ON_BOOT` rules; hook-bound rules register at priority `-9999999`. Per rule: build engine, `evaluate`; on match iterate actions — `ALLOW` ⇒ `break_rule_matching` (whitelist rest of chain, return); `BLOCK` ⇒ if PROTECT `terminateRequest` (403 + no-cache + branded "Firewall — Blocked … Reference ID" die page); `INSPECT` ⇒ dump headers/cookies/params into rule_log. Bypass paths: whitelisted IP, valid admin bypass cookie, private IP.

**Modes:** `DISABLED` / `AUDIT` (log, never block) / `PROTECT` (`isModeProtect` gates every terminate). **AUDIT must be the default** until the operator opts in.

**Request profiling** (independent of rules): every param value is fingerprinted → flags numeric/regular_word/special_word/ipv4/email/link/embeded_html/file/**sql** (SQLIREGEX count >2)/**path_traversal**/**php_eval**. This is the telemetry MalCare's cloud uses to author rules.

**IP store:** DB backend queries `ip_store` by binary range — `WHERE binIP BETWEEN start_ip_range AND end_ip_range AND is_fw=true AND type IN (categories) AND is_v6=<0|1> LIMIT 1`, returning the `type` (category: BOT_BLOCKED, COUNTRY_BLOCKED, USER_BLACKLISTED, GLOBAL_BOT_BLOCKED, WHITELISTED). No client-side expiry — server pushes inserts/deletes.

**Log schema** `fw_requests`: path, filenames, host, time, ip, method, query_string (= serialized profiled_data, capped 16k), user_agent, resp_code, referer, status (ALLOWED/BLOCKED/BYPASSED), category (int enum), rules_info (serialized rule_log capped 64k), request_id, matched_rules. DB logger caps at 100k rows (`REPLACE INTO`, deletes oldest on overflow). Sensitive keys redacted to `SensitiveData:<md5>`; values sliced at 1024.

#### (b) What WPMgr already has
**Nothing firewall-specific.** `apps/api/internal/scan/` and `apps/web/src/features/security/` are empty (`.gitkeep` only). `apps/agent/includes/commands/class-scan-command.php` is a malware stub, not a WAF. **Reusable scaffolding exists:** `class-mu-plugin-installer.php` (idempotent loader copied to `wp-content/mu-plugins/a-wpmgr-error-trap.php`, loads at `-PHP_INT_MAX` — exactly the early hook a WAF needs; clone as `a-wpmgr-waf.php`); `class-error-monitor.php` (the exact prefixed-table + md5-dedup + ROW_CAP + heartbeat `shipBatch` + `since_id` cursor pattern the fw log should mirror 1:1); `class-activity-log.php` + `apps/api/internal/activity` (event pipeline for block/login events); signed command-token transport (`class-router.php` Ed25519 JWT dispatch, `class-signer.php`).

#### (c) Gap
Everything: no rule DSL/interpreter, no early interception, no binary-range IP store, no brute-force counter, no `fw_requests` log/ship, no CP ruleset storage/delivery, no React surface.

#### (d) Proposed WPMgr design

**Slice 1 — Login Protection + IP store (no DSL needed):**
- PHP: `apps/agent/includes/support/class-login-protection.php` (port `lp.php`); `class-ip-store.php` (port binary-range lookup; **add `expires_at`** for TTL blocks, which WP Remote lacks); IP utils (`bvInetPton`, `isPrivateIP`, configurable header). Hook the IP-block check into a new `a-wpmgr-waf.php` mu-plugin (reuse `MuPluginInstaller`).
- Go CP `apps/api/internal/firewall/{model,repo,service,handler}.go`; migration `firewall_ip_rules(tenant_id, site_id, start_ip, end_ip, is_v6, scope[fw|lp], kind[block|allow], category, expires_at)`, `firewall_config(site_id, jsonb)`, login events into existing activity domain. Endpoints: `GET/PUT /sites/{id}/firewall/config`, CRUD `/sites/{id}/firewall/ip-rules`, ingest `POST /agent/firewall/events`. Deliver config to the agent as a wp-option via a new `sync_firewall` signed command.
- React `apps/web/src/features/security/`: login-protection toggle + thresholds, IP allow/block editor, recent blocked-login table.

**Slice 2 — Request profiling + blocked-request log:**
- PHP: `class-request.php` (port `request.php` capture incl. JSON body, raw-body caps) + `class-request-profiler.php` (port `profileRequestData` incl. SQLIREGEX/path_traversal/php_eval). Log to `wpmgr_fw_requests` mirroring `class-error-monitor.php` (ROW_CAP eviction, heartbeat `shipBatch` + `since_id`). CP `firewall_requests` table + ingest; React "Firewall log" tab with status/category badges.

**Slice 3 — Full rule engine (centerpiece):**
- PHP `apps/agent/includes/firewall/`: `class-rule.php`, `class-rule-engine.php` (executeStmt/getValue/`toAllowedType` depth-8 sandbox, `processRuleFunctionParams`), and the five function traits — copy almost 1:1. **Keep the VERSION gate.** Store ruleset in wp-option `wpmgr_waf_ruleset`; evaluate EXE_ON_BOOT in the mu-plugin, bind hook-rules at `-PHP_INT_MAX`.
- CP `firewall_rulesets` table (jsonb rule array + engine_ver). **Ship a curated built-in ruleset** (SQLi/XSS/path-traversal/php-eval using the bundled regex constants) as migration seed data + a CP admin push endpoint — do **not** attempt MalCare's per-site ML authoring. React: read-only ruleset viewer + per-rule match counts from the fw log.

#### (e) Effort & risk
**L overall** (Slice 1 S–M, Slice 2 M, Slice 3 L). **Risks:** (1) mu-plugin runs after WP core boot — weaker than WP Remote's `auto_prepend` "blocks before WordPress"; recommend mu-plugin first, offer `auto_prepend` as opt-in hardening. (2) **ReDoS** — `preg_match_all` on attacker input with huge regexes; **must port `WPRHelper::safePregMatch`** with a PCRE backtrack-limit + try/catch. (3) `die()` block default to **AUDIT** to avoid admin lockout. (4) IP-header spoofing — default to `REMOTE_ADDR`, configurable per site. (5) Keep the **CONST whitelist strict**.

---

### 2.2 PHP Error Monitoring

#### (a) What WP Remote does
`set_error_handler` at server-overridable `error_level` (validated `(level & E_ALL) === level`) + `register_shutdown_function` for fatals (`error_get_last`). Per error: optional `debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS, 10)` reduced to `{file,line,function}` per frame; `md5 = md5(code-message-line-file-serialized_backtrace)` (**backtrace folded into fingerprint**); `error_code/message/line/file`, `request_path` (path only), per-request `request_id`. **Dedup model: none** — append-only INSERTs; `md5` used only by the **server-pushed `md5s_to_ignore` ignore list** (`canCaptureError`). Hard stop at `max_table_length` (default 10000, no eviction). **Transport is PULL** — CP's `wings/watch` callback drains+deletes consumed rows (with an `offset_reset` auto-increment integrity guard).

#### (b) What WPMgr already has — **complete, shipped feature** (ADR-037)
- Agent `class-error-monitor.php`: capture, `md5 = md5(code:file:line:message)` (no backtrace), **INSERT-or-UPDATE with occurrence counter**, ROW_CAP 10000 **true eviction**, per-row `silenced` flag, `shipBatch` (50) with `wpmgr_agent_errors_ship_cursor`; **PUSH** via heartbeat to `/agent/v1/errors`; plus a **bootstrap mu-plugin trap** (`a-wpmgr-error-trap.php`) catching pre-`plugins_loaded` fatals — a win WP Remote lacks.
- CP `apps/api/internal/agent/errors_handler.go` + `apps/api/internal/diagnostics/{service,model,repo,handler}.go`; migration `20260530020000_m8_diagnostics_errors.sql` (`agent_php_errors`, RLS, `UNIQUE(tenant,site,md5)`, upsert `occurrence_count = GREATEST`).
- Web `apps/web/src/features/errors/` (`use-errors.ts`, table, detail drawer, severity chip, silence toggle).

WPMgr's design is **cleaner than WP Remote** on every axis. Do not rebuild.

#### (c) Gap (refinements only)
1. **No backtrace** — `backtrace_compressed` column exists but is **always written NULL**; never captured, no DTO field, no CP column, no UI. Highest-value miss (stack trace is the best fatal-debugging field).
2. **Error level hard-coded** in `install()` — no per-site operator control.
3. **No server-pushed ignore-list** — only post-hoc silence, which still costs the local write + a ship.
4. **`occurrence_count` drift (real correctness bug):** `shipBatch` filters `id > cursor`, so once a row ships, later recurrences bump the local counter but are **never re-shipped** — CP count **freezes** for long-lived errors and understates reality.
5. No CP retention/TTL.

#### (d) Proposed design (extend existing diagnostics domain — no new domain)
- **A. Backtrace (M):** in `record()` on first-seen INSERT, `debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS, 10)` → `{file,line,function}` (`array_intersect_key`) → json → gzcompress → existing `backtrace_compressed`. Add to `shipBatch`/DTO; add CP column (new migration) + model + `ListPHPErrors`; render frames in `error-detail-drawer.tsx`. **Keep backtrace OUT of the fingerprint** (otherwise every call path explodes into separate rows and breaks dedup — WPMgr's whole advantage).
- **B. Fix drift (S):** make the agent re-include recently-active fingerprints each tick (select `WHERE id>cursor OR last_seen>last_shipped`, cap by `last_seen DESC`); CP keeps `GREATEST` with absolute counts. Add `last_shipped_at`/`shipped_count`.
- **C. Ignore-list + error-level config (M):** wp-option holding `error_level` mask + `md5s_to_ignore[]`, read in `install()` and `record()` (direct port of `canCaptureError`/`isValidErrorLevel`), delivered over the existing signed-command config channel. CP per-site config + `PATCH /sites/{id}/errors/config`; small settings panel.
- **D. Retention (S):** River cron deleting `agent_php_errors` older than N days/site (mirror backup retention).

#### (e) Effort & risk
**S–M total.** Risks: backtrace leaks server paths + size — strip args, cap 10 frames, gzcompress, confirm the 2 MiB batch cap in `errors_handler.go` holds. **Keep WPMgr returning `false`** from the handler (do NOT suppress like WP Remote — would hide errors from Sentry/other logging).

---

### 2.3 Malware / File-Integrity Scan

#### (a) What WP Remote does — **and the key finding**
**The plugin has NO local malware engine and NO checksum comparison.** It is a thin agent: `callback/wings/fs.php` (`FS_WING_VERSION 1.4`) walks the filesystem and **streams `{filename,size,uid,gid,mode,mtime,md5,link}` tuples to blogvault**, which owns the known-good hash DB + signature DB + diff. Methods worth copying for the **walker** only:
- `scanFilesDfs($dir, $traversal_stack, $folder_offset, $limit, traversal_stack_max_size=100, batch_size=512)` — **resumable DFS**: explicit `$traversal_stack` of `[dirname, folder_offset]` frames so a scan pauses after `$limit` files and **resumes** next request via `seekDirectoryHandle($offset)`; caps depth at 100; returns `{links, traversal_stack, folder_offset}`.
- `fileStat($relfile, $md5)` — `@stat`, `preg_grep` keeps only size|uid|gid|mode|mtime, `readlink` for symlinks (**not followed**), md5 if requested.
- `calculateMd5(...)` — full `md5_file` or chunked `hash_init`/`fread` over a byte range (md5 a sub-range of a huge file).
- `getFilesContent($files, $withContent)` — base64 file body for **server-confirmed-suspicious** files only.
- `cwAllowedFiles = ['.htaccess','.user.ini','malcare-waf.php']` — the only files the writer wing may touch.

There is **nothing to copy for the actual integrity diff** — it lives server-side.

#### (b) What WPMgr already has
Stubs only: agent `class-scan-command.php` returns `{status: not_implemented}`; CP `internal/scan/` is `.gitkeep`. **Strong reusable foundations:** Ed25519 command transport (`class-router.php`, `class-signer.php`, `agentcmd/contract.go`+`client.go`); `class-metadata-command.php` already lists all plugin/theme/core versions (the map needed to pick the right wp.org checksums); backup file-walker (`apps/agent/includes/backup/class-files-archiver.php` + `class-backup-source.php`) already does a streamed ABSPATH walk with excludes; hashing (`class-blake3.php` + md5); the **diagnostics domain** (`internal/diagnostics/{model,repo,service,handler}.go` + migration m8 + River enqueuer) is the exact CP template to clone; SSRF-safe httpclient already in repo.

#### (c) Gap
Zero malware/integrity functionality. Missing: agent walk-and-hash routine; **core-checksum source** (WP Remote got it from blogvault — WPMgr must fetch `api.wordpress.org/core/checksums/1.0/?version=X&locale=Y` CP-side and ship the map down); heuristic/signature catalog (proprietary at MalCare — WPMgr must author its own small set); CP scan domain (migration, model/repo/service/handler, OpenAPI, River job, `agentcmd.Client.Scan()`); React surface; quarantine path.

#### (d) Proposed design — **diverge from WP Remote: scan on the agent, only findings travel**
- **Agent (PHP, M):** flesh out `class-scan-command.php`. Params from CP: `{kind: core|files|full, wp_version, locale, core_checksums:{relpath=>md5}, signatures:[{id,pattern,type}], paths_limit, time_budget_s, resume_cursor}` — CP supplies checksums + signatures so the agent needs **no outbound internet**. Reuse the backup walker (extract a shared read-only `FileWalker`). For `core`: md5 each path in `core_checksums` → classify `core_modified` (mismatch) / `core_missing` (absent) / `core_unknown` (present in wp-admin|wp-includes but not in map → injected). For `files`: walk wp-content `.php` under a size cap, `preg_match` signatures (`eval\s*\(`, `base64_decode`, `gzinflate`, `str_rot13`, `\$_(POST|REQUEST)\s*\(`, `create_function`, `assert\s*\(`, webshell markers) → `suspicious_pattern` with a redacted ±80-char snippet. Honor `time_budget_s` (mirror backup's `fastcgi_finish_request` pattern), return `{status: partial|done, cursor, findings_batch, files_scanned}`; CP re-invokes until done. Separate **capability-gated** `get_file` (size-capped) only when the operator explicitly requests a finding's body.
- **CP (Go `internal/scan`, M — clone diagnostics shape):** migration `m11_scan` — `scan_runs(id, tenant_id, site_id, kind, status, files_scanned, issues_found, started_at, finished_at, summary jsonb)`, `scan_findings(id, scan_run_id, tenant_id, site_id, finding_type, severity, path, expected_hash, actual_hash, size, mtime, signature_id, snippet, status[open|ignored|quarantined|resolved], first_seen_at, last_seen_at, dedup_key)` with `UNIQUE(tenant,site,dedup_key=md5(type:path))`, `malware_signatures(id, name, pattern, pattern_type[regex|hash], severity, enabled)` (global, no tenant). RLS per m8/m4. Endpoints: `POST /sites/{id}/scans`, `GET /sites/{id}/scans`, `GET /sites/{id}/scans/{runId}`, `POST /findings/{id}/ignore`. River job: fetch site `wp_version/locale` from metadata → fetch+cache wp.org checksums CP-side → load enabled signatures → loop `agentcmd.Client.Scan()` with the resume cursor until done, upserting findings (dedup, bump last_seen). Add `Scan(ctx, siteID, siteURL, ScanRequest)` to `agentcmd/client.go` + a `scan_contract.go` (mirror `backup_contract.go`). Add OpenAPI path + regenerate ogen.
- **Web (S–M):** `features/scan` under site detail — last-run card + Run Scan button, findings table grouped by severity with type badges, finding drawer (path, expected vs actual hash, snippet, Ignore). Reuse diagnostics/errors fetching + table primitives.

#### (e) Effort & risk
**M (agent) + M (CP) + S/M (web).** Risks: **core-checksum is the only deterministic, low-false-positive signal — prioritize it.** Heuristic regex on wp-content has high false positives (legit plugins use `base64_decode`/`eval`) — severity-tier and make findings ignorable, **never auto-action**. Full md5 walk **will** exceed PHP limits — the resumable-cursor + time-budget model is mandatory. Fetch checksums CP-side (works on locked-down hosts). Plugin/theme hashes are **not** in the wp.org core API — scope Phase 1 to core + heuristic, treat plugin/theme integrity as best-effort. Quarantine is destructive — out of scope for the read path; design `findings.status` to allow it later.

---

### 2.4 Login Protection (brute-force)

#### (a) What WP Remote does
`protect/lp.php` (`WPRProtectLP_V647`). **Three modes:** DISABLED (no hooks) / AUDIT (log only) / PROTECT (block via `die()`). **Hooks:** `add_filter('authenticate', loginInit, 30, 3)` (priority 30 — after WP's check_password@20 and 2FA@25, so it sees the final result), `wp_login`→loginSuccess, `wp_login_failed`→loginFailed.

**Default thresholds (all int-validated, overridable):** `captcha_limit=3`, `temp_block_limit=10`, `block_all_limit=100`, `failed_login_gap=1800s`, `success_login_gap=1800s`, `all_blocked_gap=1800s`.

**`loginInit` decision tree (first match wins):** (1) `isUnBlockedIP` (transient `bvlp_unblock_ip{IP}` with decrementing attempts, TTL `600*attempts` — the captcha-solved grace) → skip blocking; (2) compute `failed_attempts = getLoginCount(FAILURE, ip, failed_login_gap)`; whitelisted→BYPASSED; private IP→PRIVATEIP; blacklisted→terminate; `isKnownLogin` (≥1 success from IP within `success_login_gap`)→BYPASSED; global lockout (`getLoginCount(FAILURE, null, all_blocked_gap) >= block_all_limit` and no `bvlp_allow_logins`)→ALL_BLOCKED terminate; `failed_attempts >= temp_block_limit`→30-min TEMP_BLOCK; `failed_attempts >= captcha_limit`→CAPTCHA_BLOCK. **The entire counting engine is `SELECT COUNT(*) FROM lp_requests WHERE status=%d AND time > now-gap [AND ip=%s]`** — per-IP for captcha/temp, global (ip=null) for all-blocked.

**Storage:** `lp_requests(ip, status[1 fail|2 success|3 blocked], time, category, username, request_id, message)`, capped 100k. Block screen = branded 403 `die()` template keyed by category; AUDIT logs but never dies. Runtime block/unblock state lives in **transients** pushed by the server (`blklogins`/`unblklogins`/`unblkip` wings).

#### (b) What WPMgr already has
`class-activity-log.php` hooks `wp_login_failed` (`onLoginFailed`, severity HIGH) and `wp_login` — but **only records events** for the CP to alert on a burst. **No blocking, no counter, no thresholds, no authenticate filter.**

#### (c) Gap
All enforcement: no time-windowed counting, no captcha/temp/all-block thresholds, no lockout, no per-IP-vs-global keying, no branded block screen. (Passive recording only.)

#### (d) Proposed design (ships with Firewall Slice 1)
- PHP `class-login-protection.php` (port `lp.php`): `add_filter('authenticate',…,30,3)` + `wp_login`/`wp_login_failed` when mode != disabled. New table `wpmgr_login_events(ip,status,time,username,request_id)` via `class-schema.php`; same time-windowed `COUNT(*)`. Config (mode + 6 thresholds + ip header + allow/deny CIDRs) from a new `wpmgr_security_config` Settings option, delivered by CP. Branded `die()` in PROTECT, log-only in AUDIT. Emit a structured activity-log entry on block (CP gets it free). Use `set_transient` for runtime block/unblock state. **Replace the captcha-server loop** (WP Remote bounces to a hosted `/captcha/solve` WPMgr lacks) with a dashboard **"Unblock IP"** button → signed command.
- CP: into the new `firewall`/`security` domain — config + endpoints `GET/PUT /sites/{id}/security/login-protection`, `POST /sites/{id}/security/unblock-ip`, `POST .../block-logins`. Reuse activity for blocked-attempt analytics.
- React: Login Protection tab — mode toggle (audit/protect), threshold inputs, recent blocked-attempt list from the activity feed.

#### (e) Effort & risk
**M.** Risks: the global all-blocked limit (100 failures across all IPs) can lock out the whole site under a distributed attack — **default AUDIT, require explicit PROTECT opt-in**. `die()` bypasses normal WP flow — test with caching/login plugins. IPv6 binary-range queries are heavy — for MVP consider simple CIDR allow/deny in config JSON rather than the full `ip_store` table + country DBs.

---

### 2.5 Two-Factor Auth (TOTP)

#### (a) What WP Remote does
`wp_2fa/authenticator.php`: **RFC 6238 TOTP** (not email). `code_length=6`, `period=30`, HMAC-SHA1 over `(4 zero bytes + pack('N', floor(time/30)))`, dynamic truncation, mod 10^6. Secret is custom **Base32** (A-Z2-7), decoded secret must be **exactly 32 bytes (160-bit)** or login rejects. `verifyCode($secret,$code,$discrepancy=1)` loops `time_slice ± discrepancy` with **timing-safe `hash_equals`** (login ±1 = 90s; server verify ±2). **Secrets generated server-side**, pushed via the `stupwp2fa` callback; stored in user_meta `wpr_2fa_secret = {secret: base64, is_encrypted}` + `wpr_2fa_enabled`, gated by option `wprWp2faConf.enabled`. **Encryption at rest:** AES-256-CBC keyed by `SECURE_AUTH_KEY`, ciphertext = `IV(16) . raw`, base64 (hard-fails if `SECURE_AUTH_KEY` absent).

**Login flow** (`authenticate` filter @25 — after WP auth@20, before LP@30): only acts on an already-valid `WP_User`; if enabled and no `$_POST['twofa_code']` → `wp_send_json_success(['twofa_enabled'=>true])` + exit (the "now ask for the code" handshake); if code present → decrypt secret, validate 32 bytes, `ctype_digit`, `verifyCode` → return user or `WP_Error`. Frontend JS intercepts the login submit, detects `twofa_enabled`, injects a 6-digit input, resubmits. **No recovery codes** — lockout recovery = admin deletes the meta.

#### (b/c) What WPMgr has / gap
Nothing — entirely greenfield (no TOTP, no secret storage, no login interception, no verify).

#### (d) Proposed design (own later phase)
- PHP `class-totp.php` (verbatim port of `authenticator.php` — RFC6238/base32/`hash_equals`/±1) + `class-two-factor.php` (port `authenticate`@25 + login_form JS injector). Secret in user_meta `wpmgr_2fa_secret {secret(base64), is_encrypted}` + `wpmgr_2fa_enabled`; encrypt via `SECURE_AUTH_KEY` **or reuse the existing AgeCrypto keystore** (decision below). **ADD recovery codes** (store hashed in user_meta) — closes WP Remote's admin-lockout gap.
- CP: `two_factor_enrollments(site_id, wp_user_id, secret_encrypted, enabled, recovery_codes_hashed, status)`; service generates the TOTP secret + `otpauth://` URI for the dashboard QR; endpoints `GET/POST/DELETE /sites/{id}/security/2fa`, `POST /sites/{id}/security/2fa/{userId}/verify`. Deliver via a `security` signed command (mirror `stupwp2fa`/`vrfywp2fa`/`dltewp2fa`).
- React: Two-Factor tab — per-user enable, QR enrollment modal, recovery codes display, disable.

#### (e) Effort & risk
**M.** Risks: decide CP-generated secret + dashboard QR (matches WP Remote, simplest) vs user self-enroll. **Add recovery codes.** Decide encryption key: `SECURE_AUTH_KEY` (simplest port, hard-fails if absent) vs AGE keystore (consistent with WPMgr). The @25/@30 filter interplay + JSON-handshake JS is fragile across themes/login plugins — test thoroughly.

> **Note on "whitelabel":** WP Remote's login whitelabel (`wp_login_whitelabel.php`) is **purely cosmetic** login-page branding (logo data-URI via `login_head`, label via `login_message`). There is **no custom-login-URL / wp-login.php-hiding** anywhere in the source, despite the brief's framing. If the user wants login-URL rename, that is **net-new**, not a WP Remote port — confirm before scoping. Effort **S**, low risk.

---

## 3. Cross-Cutting: Config Delivery & Infra Fit

**WP Remote's delivery model:** server→plugin commands ride a normal front-controller page load (`bvplugname==wpremote`), authenticated by **RSA `openssl_verify`** (bundled `m_public.pub`) with a per-account shared secret folded into the signed data, an **HMAC-SHA1/256 over the `bvprms` payload** (`hash_equals`), and a **5-min replay window** (`bvLastRecvTime`). Rulesets/IP lists/2FA secrets/thresholds are all **server-pushed** into wp-options/user-meta/custom tables or runtime transients. The `watch` wing **pulls** log rows (drain+delete with an `offset_reset` cursor guard).

**WPMgr already has a strict superset of this security model.** Inbound CP→agent commands are `POST {site}/wp-json/wpmgr/v1/command/{command}` authorized by an **Ed25519 (EdDSA/JWT) bearer token** whose claims `{jti, exp≤60s, iat, iss, aud=site_id, cmd}` are verified byte-for-byte (`Connector::verifyCommand`) with anti-replay (`jti`), **aud-binding** (defeats cross-tenant), **cmd-binding** (defeats cross-command reuse), and `manage_options` defense-in-depth (`class-router.php`). Outbound agent→CP is Ed25519-signed over canonical `METHOD\nPATH\nTS\nNONCE\nsha256(body)` (`class-signer.php`).

**Reconciliation — reuse, do not reinvent:**
- **Firewall ruleset + malware signatures + LP thresholds + 2FA secrets** are delivered as **wp-options/user-meta written by new signed commands** (`sync_firewall`, `scan` params, `security`), exactly as WPMgr already pushes updates/diagnostics/backup config. This replaces WP Remote's RSA wings 1:1 with WPMgr's stronger token model. **Crucially, signatures and core-checksums are shipped *as scan-command parameters*** so the agent needs no outbound internet (works on locked-down hosts).
- **Log/event ship-back** reuses the **`class-error-monitor.php` pattern** verbatim: prefixed table → md5 dedup (where applicable) → ROW_CAP eviction → heartbeat `shipBatch` with a `since_id` cursor → CP server-side dedup. `fw_requests`, `login_events`, and `scan_findings` all mirror this — **push+cursor, not WP Remote's pull+delete** (cleaner, already proven in production).
- **Block/login/scan events** feed the **existing `activity` domain** rather than inventing transports, so CP-side alerting (brute-force bursts, new findings) comes for free.
- **CP domain shape** for every new feature **clones `internal/diagnostics`** (model/repo/service/handler + migration + River enqueuer + RLS by tenant_id) — the established, reviewed pattern.

---

## 4. Recommended Phased Plan

**Guiding principle:** PHP Errors is done — only polish it. Lead with the highest value/effort ratio that needs no DSL (Login Protection + IP store), then the deterministic malware win (core checksums), then the firewall log, then the DSL engine, then 2FA. Each phase ships independently.

| Sprint | Deliverable | Effort | Depends on |
|---|---|---|---|
| **S1** | **PHP-error tightening:** backtrace capture end-to-end + fix `occurrence_count` drift + ignore-list/error-level config + retention cron | S–M | existing diagnostics domain |
| **S2** | **Login Protection + IP store** (Firewall Slice 1): `class-login-protection.php`, `class-ip-store.php`, `a-wpmgr-waf.php` mu-plugin; CP `firewall`/`security` domain + migration + config/ip-rules/unblock endpoints; React Login-Protection tab. **Default AUDIT.** | M | new `security` domain, command transport (exists) |
| **S3** | **Malware Phase 1 — core-checksum scan:** agent `class-scan-command.php` (resumable walker + core md5 diff) + `get_file` capability; CP `internal/scan` domain (clone diagnostics) + `scan_runs`/`scan_findings`/`malware_signatures` migration + River driver fetching wp.org checksums; React scan surface | M+M | metadata-command (exists), backup walker (exists), SSRF httpclient (exists) |
| **S4** | **Firewall Slice 2 — request profiling + `fw_requests` log:** `class-request.php`, `class-request-profiler.php`, ship pipeline (mirror error-monitor); CP `firewall_requests` ingest; React firewall log tab | M | S2 mu-plugin + config plumbing |
| **S5** | **Malware Phase 2 — heuristic signatures:** signature catalog seed + agent `preg_match` scan + redacted snippets; CP signature delivery as scan params; severity-tiered ignorable findings | M | S3 |
| **S6** | **Firewall Slice 3 — full rule engine (centerpiece):** port AST interpreter + 5 function traits + `safePregMatch` guard + VERSION gate; CP `firewall_rulesets` + curated seed ruleset (SQLi/XSS/path-traversal/php-eval) + push endpoint; React read-only ruleset viewer with per-rule match counts | L | S4 (request capture + fw log) |
| **S7** | **2FA (TOTP):** `class-totp.php` + `class-two-factor.php` + recovery codes; CP `two_factor_enrollments` + enroll/verify/disable endpoints + QR URI; React 2FA tab. **+ optional cosmetic login whitelabel (S).** | M (+S) | S2 `security` domain |

**Dependencies:** S6 (rule engine) depends on S4 (request capture + fw log surface it writes to). S3/S5 (malware) and S7 (2FA) are independent of the firewall track and can interleave by team capacity. S1 is independent and can run first or in parallel.

---

## 5. Open Questions for the User

1. **WAF interception depth:** mu-plugin (runs after WP core boot, safe, reversible — recommended start) vs a true `auto_prepend_file` installer (`.htaccess`/`.user.ini`/wp-config include — real "blocks before WordPress" but can white-screen a site if mis-written). Ship mu-plugin first, offer auto_prepend as opt-in hardening?
2. **Firewall rule authoring:** MalCare authors per-site rules from profiled-request telemetry via a cloud ML backend WPMgr does not have. Confirm WPMgr ships a **curated static ruleset + CP push** (not per-site dynamic rules) — acceptable scope?
3. **Malware architecture:** confirm **scan-on-agent, findings-only-travel** (my recommendation, fits WPMgr's per-site agent) vs WP Remote's stream-all-hashes-to-CP (needs a CP-side known-good hash DB and egresses the whole filesystem). Recommend agent-side.
4. **Malware Phase-1 scope:** OK to scope to **core checksums + heuristic only**, treating plugin/theme integrity as best-effort (versions known, hashes mostly unavailable from wp.org)?
5. **2FA secret + key:** CP-generated secret shown once as a dashboard QR (matches WP Remote, simplest) vs user self-enroll? And encrypt the secret with `SECURE_AUTH_KEY` (simple port) or the existing **AGE keystore** (WPMgr-consistent)? Confirm we **add recovery codes** (WP Remote has none — admin-lockout risk).
6. **"Whitelabel" vs login-URL hiding:** WP Remote's whitelabel is **cosmetic only** (logo + label). Do you actually want **wp-login.php URL rename** (net-new, not a port)? If so, scope it separately.
7. **PHP-error server-side counts:** the `occurrence_count` drift fix matters only if you want accurate server-side counts for long-lived errors (likely yes for alerting) — confirm priority.
8. **AUDIT-first default:** confirm both the firewall and login protection default to AUDIT (log-only) until the operator explicitly enables PROTECT, to avoid locking admins out — especially given the global all-blocked threshold.
9. **Captcha/unblock UX:** WP Remote's unblock loop depends on its hosted `/captcha/solve`. WPMgr will replace it with a **dashboard "Unblock IP" button → signed command** (no hosted captcha). Acceptable, or do you want a self-serve email-link unblock?