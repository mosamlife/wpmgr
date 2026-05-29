# phpbu Integration Research — WPMgr Backup Engine

**Status:** Research / pre-ADR
**Date:** 2026-05-28
**Author:** research agent (commissioned for M4 pivot)
**Repo under study:** [`sebastianfeldmann/phpbu`](https://github.com/sebastianfeldmann/phpbu) — “PHP Backup Utility”
**Context:** WPMgr M4 backup pipeline OOMs / times out because the PHP agent
synchronously walks `wp-content`, dumps the DB, chunks, age-encrypts, and
uploads — all inside one HTTP request held open by the CP. We want to pivot to
phpbu as the backup engine.

---

## 0. TL;DR

- **Latest stable: 6.0.31 (4 Feb 2025); 6.x series is current. Requires PHP ≥ 8.1**, ext-dom/json/spl. Composer-installable as `phpbu/phpbu:^6.0`.
- phpbu is a **CLI tool with a clean, library-usable PHP core**. The
  `phpbu\App\Cmd` class is a thin shim; the real engine is
  `phpbu\App\Runner + Factory + Configuration\Loader`, all directly
  instantiable from a long-running PHP process.
- **Pipeline order is locked: Source → Check → Crypt → Sync → Cleanup.**
  Crucially, **Crypt runs BEFORE Sync**, so an `age` Crypter would encrypt
  the artifact in place and the Sync stage would push the ciphertext to S3 —
  exactly the property we need.
- **Built-in `AmazonS3v3` sync uses AWS SDK v3 for PHP and accepts a custom
  `endpoint` + `usePathStyleEndpoint`** — works against SeaweedFS / MinIO out
  of the box. BUT it requires raw key/secret credentials; it does **not**
  speak presigned-PUT URLs, which is how WPMgr currently brokers uploads from
  the CP.
- phpbu has a real **event dispatcher** (`phpbu\App\Event\Dispatcher`) with
  granular events for every stage (`phpbu.app_start`,
  `phpbu.backup_start/end`, `phpbu.check_start/end`,
  `phpbu.crypt_start/end`, `phpbu.sync_start/end`,
  `phpbu.cleanup_start/end`, `phpbu.app_end`, plus per-stage `Failed`).
  Custom subscribers are registered via `Subscriber::getSubscribedEvents()` —
  this is the hook we want for real-time progress to the CP.
- Extension model is small and explicit: implement
  `phpbu\App\Backup\{Source,Sync,Crypter,Cleanup,Check}` and register the
  alias via `phpbu\App\Factory::register('crypt', 'age', '\WPMgr\Agent\Phpbu\AgeCrypter')`
  from a `--bootstrap` PHP file (or directly in our long-running process).
- **Recommended integration: Option C (Hybrid)** — phpbu as a **library**
  inside a detached PHP subprocess; a custom **`AgeCrypter`** drives the
  `age` binary; a custom **`Sync\PresignedS3`** uses the existing
  `BackupTransport::presignChunks()` + `putChunk()` path so the CP still
  brokers uploads; a custom **`Logger\WpmgrCallback`** subscribes to every
  event and POSTs progress to a new
  `POST /agent-cb/v1/backups/{snapshot_id}/progress` endpoint (Ed25519-signed
  with the existing `Signer`).

---

## 1. phpbu capabilities and architecture

### 1.1 Version, install, PHP requirement

- **Latest stable:** `6.0.31` (2025-02-04). 6.0.x is the maintenance series;
  the most recent feature work was `6.0.30` (Jun 2024) adding PHP 8.4
  support and `6.0.26` (Nov 2023) adding Google Cloud Storage. Source: [Packagist](https://packagist.org/packages/phpbu/phpbu), [GitHub Releases](https://github.com/sebastianfeldmann/phpbu/releases).
- **Required PHP:** `>= 8.1`, with `ext-dom`, `ext-json`, `ext-spl`. WPMgr
  agent currently requires PHP `>= 8.0` — **we must bump to 8.1** (this is
  already below the WordPress.org-recommended PHP 8.2; safe).
- **Install (Composer):**
  ```bash
  composer require phpbu/phpbu:^6.0
  ```
  PHAR alternative exists (`phpbu.phar`) but we want library use → Composer.
- **Production deps** (per `composer.json` `require`):
  - `sebastian/environment`
  - `sebastianfeldmann/cli` (`^3.4`) — shell-out wrapper
  - `symfony/process` (`^3.0|...|^7.0`)
  - That's it. Lean.
- **AWS SDK is in `require-dev`** (`aws/aws-sdk-php: ^3.10`). If we use
  phpbu's S3 sync, we must add `composer require aws/aws-sdk-php:^3` to
  the agent ourselves (~5 MB of vendor code). If we go with our custom
  presigned-PUT sync, **we avoid the AWS SDK entirely**.

### 1.2 Source backends (what phpbu can back up)

Per the manual chapter ["Backup Sources"](http://phpbu.de/manual/current/en/source.html):
ArangoDB, Elasticsearch, InfluxDB, LDAP, MongoDB, **MySQL (`mysqldump`)**,
PostgreSQL (`pgdump`), Redis, rsync, and **Directories (`tar`)**.

We need two for WordPress:

- **`mysqldump`** — shells out to the `mysqldump` binary. There is **no
  pure-PHP fallback** in phpbu itself; the `pathToMysqldump` option points
  at the binary. Most managed WP hosts have `mysqldump` available; for the
  ones that don't we keep our existing WP-CLI / `wp db export` path as a
  fallback (already in `class-backup-source.php`).
- **`tar` source** — backs up a directory tree by shelling to `tar`.
  Accepted options (from the [source](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Backup/Source/Tar.php)):
  `path` (mandatory), `pathToTar`, **`exclude` (comma-separated list)**,
  `incrementalFile`, `forceLevelZeroOn`, `compressProgram`, `throttle`,
  `forceLocal`, `ignoreFailedRead`, `removeSourceDir`, `dereference`. The
  `exclude` option covers our `wpmgr-snapshots`, `wpmgr-agent`, `cache`,
  `upgrade` exclusions (see existing `BackupSource::EXCLUDE_DIRS`).

### 1.3 Sync targets

From the manual: **Amazon S3 (v2 + v3)**, **WasabiS3**, **BackblazeS3**,
Azure Blob, Dropbox, Google Cloud Storage, Google Drive, OpenStack, Rsync,
SFTP, FTP, SoftLayer, Xtp. File list confirmed at
[`src/Backup/Sync/`](https://github.com/sebastianfeldmann/phpbu/tree/master/src/Backup/Sync).

#### S3 specifics (the one that matters)

The `AmazonS3v3` class instantiates `Aws\S3\S3Client` directly. Its
`setup()` (inherited from `AmazonS3`) accepts:

| Option                  | Required | Notes |
|-------------------------|----------|-------|
| `key`                   | yes      | AWS access key |
| `secret`                | yes      | AWS secret key |
| `bucket`                | yes      | |
| `region`                | yes      | |
| `path`                  | no       | Object key prefix, supports `%Y%m%d` placeholders |
| `acl`                   | no       | default `private` |
| `useMultiPartUpload`    | no       | bool |
| **`usePathStyleEndpoint`** | no    | **bool — required for SeaweedFS/MinIO** |
| **`endpoint`**          | no       | **custom endpoint URL — yes, works for SeaweedFS** |
| `signatureVersion`      | no       | |
| `bucketTTL`             | no       | |

Quote from `createClient()` (per code read):
```php
if ($this->endpoint) {
    $config['endpoint'] = $this->endpoint;
}
```

**Verdict:** phpbu's S3 sync works against SeaweedFS — but it needs raw
credentials. **It does not support presigned-PUT, and presigned URLs cannot
be retrofitted via config.** This is the single biggest architectural
mismatch with the current WPMgr design where the CP brokers all uploads.

We have two ways to reconcile this (covered in §2 and §4):

1. Give the agent direct SeaweedFS credentials and use phpbu's S3 sync.
2. Write a custom `Sync` class that calls our existing
   `BackupTransport::presignChunks() → putChunk()`. This keeps the
   bearer-credential boundary at the CP.

### 1.4 Crypt step

Built-in crypters (per [`src/Backup/Crypter/`](https://github.com/sebastianfeldmann/phpbu/tree/master/src/Backup/Crypter)):
**`mcrypt`**, **`openssl`**, **`gpg`**. All shell out to the corresponding
binary via `Executable\*` + Symfony Process. No native age support.

**Crypter interface** (`phpbu\App\Backup\Crypter`):
```php
public function setup(array $options = []): void;
public function crypt(Target $target, Result $result): void;   // throws phpbu\App\Exception
public function getSuffix(): string;
```
Optionally implement `phpbu\App\Backup\Crypter\Simulator` for `--simulate`.

This is **trivial** to implement for `age`: our `crypt()` shells to the
`age` binary (or to the existing `WPMgr\Agent\Support\AgeCrypto` class),
overwrites `$target->getPathname()` with `…age`, and returns. The `Sync`
that runs next picks up the new pathname automatically because phpbu
updates `Target` when crypt completes.

### 1.5 Pipeline order (the load-bearing detail)

From `phpbu\App\Runner\Backup::run()`:
```
executeSource()  →  executeChecks()  →  executeCrypt()
                 →  executeSyncs()    →  executeCleanup()
```

Per-stage `Result::*Start/*End` calls dispatch events through
`phpbu\App\Event\Dispatcher`. **Crypt strictly precedes Sync**, so our
age ciphertext is what hits S3. Plaintext never leaves the box.

### 1.6 Configuration model

- **Two equivalent formats**: XML (with XSD at `http://schema.phpbu.de/6.0/phpbu.xsd`)
  and JSON.
- Loaded via `phpbu\App\Configuration\Loader` — picks `Xml` or `Json` based
  on file extension.
- A typical XML block:
  ```xml
  <phpbu xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:noNamespaceSchemaLocation="http://schema.phpbu.de/6.0/phpbu.xsd">
    <backups>
      <backup name="wp-content">
        <source type="tar">
          <option name="path" value="/var/www/html/wp-content"/>
          <option name="exclude" value="wpmgr-snapshots,wpmgr-agent,cache,upgrade"/>
        </source>
        <target dirname="/var/www/html/wp-content/wpmgr-snapshots"
                filename="wp-content-%Y%m%d-%H%i.tar"
                compress="zstd"/>
        <crypt type="age">
          <option name="recipient" value="age1xyz..."/>
        </crypt>
        <sync type="wpmgr.presigned-s3">
          <option name="snapshot_id" value="snap_..."/>
          <option name="presign_endpoint" value="https://cp/.../presign"/>
        </sync>
      </backup>
    </backups>
    <logging>
      <log type="wpmgr.callback">
        <option name="progress_endpoint" value="https://cp/.../progress"/>
      </log>
    </logging>
  </phpbu>
  ```
- **Programmatic generation:** WPMgr already templates JSON for the
  existing presign protocol; emitting an XML file per backup run from PHP
  with `DOMDocument` is trivial and avoids the JSON loader's slightly less
  expressive option syntax. Write to `wp-content/wpmgr-agent/runs/{id}/phpbu.xml`,
  invoke `Runner::run(Configuration::load(...))`, delete on success.

### 1.7 Execution model

phpbu is a **CLI tool with library-grade internals**.
[`src/Cmd.php`](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Cmd.php)
shows the entire boot is:
```php
$factory    = new Factory();
$runner     = new Runner($factory);
$configFile = $this->findConfiguration();
$result     = $runner->run($this->createConfiguration($configFile, $factory));
```
We can do the same five lines inside our own long-running PHP process
(detached subprocess). Single-process, single-config, no daemon mode, no
HTTP server. Each `phpbu` invocation is one backup run.

### 1.8 Events / hooks (real-time progress)

`phpbu\App\Event\Dispatcher` is a tiny custom dispatcher (not Symfony):
```php
public function addSubscriber(Subscriber $subscriber): void;
public function dispatch(string $eventName, $event): void;
```

Subscribers implement `Subscriber::getSubscribedEvents(): array` and are
wired in via `Result::addListener()` / `Result::useEventDispatcher()`.
Events dispatched by `Result` (per code read):

| Event constant                              | NAME string                |
|---------------------------------------------|----------------------------|
| `Event\App\Start::NAME`                     | `phpbu.app_start`          |
| `Event\App\End::NAME`                       | `phpbu.app_end`            |
| `Event\Backup\Start::NAME`                  | `phpbu.backup_start`       |
| `Event\Backup\End::NAME`                    | `phpbu.backup_end`         |
| `Event\Backup\Failed::NAME`                 | `phpbu.backup_failed`      |
| `Event\Check\{Start,End,Failed}::NAME`      | `phpbu.check_*`            |
| `Event\Crypt\{Start,End,Failed,Skipped}::NAME` | `phpbu.crypt_*`         |
| `Event\Sync\{Start,End,Failed,Skipped}::NAME`  | `phpbu.sync_*`          |
| `Event\Cleanup\{Start,End,Failed}::NAME`    | `phpbu.cleanup_*`          |
| `Event\Debug::NAME` / `Event\Warning::NAME` | `phpbu.debug` / `phpbu.warning` |

Granularity is **per backup, per stage**. There are **no intra-stage
byte-progress events** (e.g. “50% through the tar”). Byte-level progress
would have to come from our own custom `Sync` (we know the chunk count) or
from polling `$target->getPathname()` size. This is acceptable for V0:
phase-level progress is what TanStack Query / SSE consumers need to render
a useful UI.

The built-in **Webhook logger** only subscribes to `phpbu.app_end`, so we
**cannot** reuse it for streaming progress — we must write our own
subscriber.

### 1.9 Cleanup / retention

Built-in `Cleanup` types: `Capacity` (size cap), `Outdated` (age),
`Quantity` (count), `Stepwise` (Grandfather-Father-Son), with
`SoftLayer`/`AmazonS3` remote cleanup variants. **WPMgr already does
retention on the CP** (River job `retention.gc` per M4) — we should set
`<cleanup>` to nothing or to a local-only `Outdated` that just trims the
agent's scratch dir, and leave server-side retention to the CP.

### 1.10 Failure handling

- Each stage respects `skipOnFailure` (continue pipeline) and the backup
  respects `stopOnError` (continue with next backup).
- Failures dispatch the matching `Failed` event and update `Result`.
- **No rollback / no transaction.** A failed `Sync` leaves the encrypted
  artifact on disk; a failed `Crypt` leaves the plaintext. Our wrapper
  must clean `wpmgr-agent/runs/{id}/` on app-end-failed.

---

## 2. Integration approach options

### Option A — CLI subprocess (`exec("php phpbu.phar -c …")`)
- **What:** Ship `phpbu.phar` (or `vendor/bin/phpbu`) inside the plugin.
  Generate `phpbu.xml` per run, exec, watch stdout/stderr for progress.
- **Install size:** ~3.5 MB PHAR; with Composer ~8 MB vendor (includes
  symfony/process).
- **Pros:** Strong process isolation; OOM in phpbu can't kill the WP
  request; survives FPM worker recycle.
- **Cons:**
  - **No real progress events** — only stdout lines, which we'd have to
    grep with brittle regexes. Misses Crypt/Sync sub-stages we want for
    the UI.
  - Hard to inject the custom `AgeCrypter` + custom `PresignedS3 Sync`
    without a `--bootstrap` PHP file that itself depends on the agent's
    autoloader → awkward.
  - Two-process debugging (WP → exec → phpbu → exec → tar/openssl).
- **Verdict:** rejected for V0. Useful as a fallback if FPM is absent.

### Option B — Library (programmatic API) in-process
- **What:** `require vendor/autoload.php`; instantiate `Factory`, `Runner`,
  `Configuration\Loader`; call `$runner->run($config)` directly inside the
  WP request handler. Register custom subscribers on the dispatcher before
  `run()`.
- **Pros:** First-class access to the event dispatcher → real-time
  progress; custom Crypter/Sync registration via `Factory::register()` is
  one line each; smallest moving parts.
- **Cons:**
  - Still runs in the WP request → still subject to FPM `request_terminate_timeout`.
  - phpbu shells out to `tar`/`mysqldump`/`openssl` via Symfony Process,
    which can fork-and-fail under low-memory FPM workers.
  - We'd have to call `fastcgi_finish_request()` to release the HTTP
    response before running, and accept that we can no longer tell the CP
    “the backup command was accepted” via the same response payload.
- **Verdict:** good engineering, wrong host. Library use is right; in-FPM
  execution is wrong.

### Option C — Hybrid (RECOMMENDED): library in a detached subprocess
- **What:**
  1. WP request `POST /wpmgr/v1/commands` receives the `backup` command,
     validates the Ed25519 JWT, persists a row in a tiny `runs` table,
     writes `wp-content/wpmgr-agent/runs/{id}/phpbu.xml`, **spawns a
     detached `php` subprocess** running a thin
     `bin/wpmgr-backup-runner.php` shim, and **ACKs the CP immediately**
     with `{status: "started", run_id: ...}`.
  2. The shim `require`s the plugin's Composer autoloader, registers our
     `AgeCrypter` and `PresignedS3 Sync` aliases via
     `Factory::register()`, attaches a `WpmgrProgressSubscriber` to the
     dispatcher, and calls `Runner::run()`.
  3. The subscriber posts progress events to
     `POST /agent-cb/v1/backups/{snapshot_id}/progress` (Ed25519-signed via
     the existing `Signer`).
- **Pros:**
  - Library-grade access to events (Option B's strength).
  - No HTTP timeout / FPM worker pinned (Option A's strength).
  - Custom Crypter + custom Sync register cleanly via `Factory::register()`.
  - The CP's existing `BackupTransport` (presignChunks / putChunk /
    submitManifest) is reused unchanged.
- **Cons:**
  - One more process to monitor. We need a heartbeat in the runner so the
    CP can detect a wedged backup (extends the existing `health.sweep`
    River job).
  - Composer footprint adds ~8-10 MB to the agent vendor dir.
- **Verdict:** **recommended for V0.**

---

## 3. age encryption integration

phpbu has no built-in age support. The interface is small; the integration
is straightforward.

### 3.1 Where to encrypt (before vs after sync)

phpbu's Crypt stage runs **before** Sync (see §1.5). It rewrites
`$target->getPathname()` to point at the ciphertext, and the Sync stage
picks that up. So **the artifact uploaded to S3 is the ciphertext** — which
is exactly our requirement.

> Do not skip the Crypt step and try to encrypt inside the Sync class.
> Crypt's `getSuffix()` is what changes the Target's pathname so the Sync
> sees the encrypted file. Inverting this order is a foot-gun.

### 3.2 Custom `AgeCrypter` (sketch)

```php
namespace WPMgr\Agent\Phpbu;

use phpbu\App\Backup\Crypter;
use phpbu\App\Backup\Target;
use phpbu\App\Result;
use WPMgr\Agent\Support\AgeCrypto;

final class AgeCrypter implements Crypter
{
    private string $recipient;
    private bool   $keepPlaintext = false;

    public function setup(array $options = []): void
    {
        if (empty($options['recipient'])) {
            throw new Crypter\Exception('age recipient required');
        }
        $this->recipient = $options['recipient'];
    }

    public function crypt(Target $target, Result $result): void
    {
        $in  = $target->getPathname();              // e.g. /…/wp-content.tar.zst
        $out = $in . '.' . $this->getSuffix();      // …/wp-content.tar.zst.age

        // Streamed — never load full artifact into memory.
        AgeCrypto::encryptFileToFile($in, $out, $this->recipient);

        if (!$this->keepPlaintext) {
            @unlink($in);
        }
        $target->setCrypter($this);                 // updates pathname to $out
        $result->debug("age: encrypted {$in} -> {$out}");
    }

    public function getSuffix(): string
    {
        return 'age';
    }
}
```

Registered with one line at runner boot:
```php
\phpbu\App\Factory::register('crypt', 'age', \WPMgr\Agent\Phpbu\AgeCrypter::class);
```

### 3.3 age implementation choice

There is **no maintained pure-PHP age binding** (per
[awesome-age](https://github.com/FiloSottile/awesome-age) and discussion
[#404](https://github.com/FiloSottile/age/discussions/404)). Options:

1. **Shell to the `age` binary** via Symfony Process (already a phpbu dep).
   Distribute the static `age` binary alongside the plugin (~3 MB) or rely
   on it being present on the host.
2. **Reuse our existing `WPMgr\Agent\Support\AgeCrypto`** class
   (`includes/support/class-age-crypto.php`) which already handles X25519
   for the chunked path. Switch from chunk-oriented to whole-file
   streaming (already in M4 code — see `class-age-crypto.php`). **Preferred**
   — zero new binaries, reuses tested code.

Recommended: option 2.

---

## 4. S3 sync configuration

### 4.1 What phpbu ships

`AmazonS3v3` (use this; `v2` is legacy SDK; `AmazonS3` is the abstract
shared base). Source:
[`src/Backup/Sync/AmazonS3v3.php`](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Backup/Sync/AmazonS3v3.php).

Verified support:
- ✅ Custom `endpoint` — works against SeaweedFS S3 gateway.
- ✅ `usePathStyleEndpoint` — required for SeaweedFS.
- ✅ Multipart upload for large objects.
- ❌ **Presigned PUT URLs are not supported.** The class uses
  `$client->putObject()` / `$client->upload()` with credentials it holds.

### 4.2 Why phpbu's S3 sync is the wrong choice for V0

WPMgr's M4 design — confirmed by reading
`apps/agent/includes/support/class-backup-transport.php` — has the CP mint
short-lived presigned URLs per ciphertext chunk and never gives the agent
SeaweedFS credentials. This is the **right** security posture (least
privilege, no agent-side bucket access).

If we adopt phpbu's S3 sync, we'd need to:
- Issue per-site SeaweedFS IAM users (SeaweedFS S3 API supports IAM but it
  doesn't have AWS's policy expressiveness).
- Distribute those creds to the agent and rotate them.
- Lose dedup across snapshots (the CP currently dedups by chunk hash
  before issuing presigned URLs — see `presignChunks()`).

**Recommendation: keep the presigned-PUT model.** Write a custom
`WPMgr\Agent\Phpbu\Sync\PresignedS3` class:

```php
final class PresignedS3 implements \phpbu\App\Backup\Sync
{
    public function setup(array $conf): void { /* read snapshot_id, presign_url, manifest_url, content-hash-chunk-size */ }

    public function sync(\phpbu\App\Backup\Target $target, \phpbu\App\Result $result): void
    {
        $path = $target->getPathname();   // already age-ciphertext
        // 1. chunk-stream the file (4 MB chunks) computing blake3 per chunk
        // 2. POST {snapshot_id, hashes:[…]} to presign_endpoint
        // 3. for each presigned URL: stream-PUT the chunk
        // 4. POST manifest to manifest_endpoint
    }
}
```

Registered with:
```php
\phpbu\App\Factory::register('sync', 'wpmgr.presigned-s3', PresignedS3::class);
```

This sync **reuses the existing `BackupTransport` class verbatim** —
zero changes to the CP-side `agent_handler.go` for the happy path.

---

## 5. Real-time progress design

### 5.1 Event source

Our `WpmgrProgressSubscriber` (registered on the phpbu dispatcher in the
runner shim) listens to every stage event:

```php
public static function getSubscribedEvents(): array
{
    return [
        Event\App\Start::NAME      => 'onAppStart',
        Event\Backup\Start::NAME   => 'onBackupStart',
        Event\Backup\End::NAME     => 'onBackupEnd',
        Event\Backup\Failed::NAME  => 'onBackupFailed',
        Event\Crypt\Start::NAME    => 'onCryptStart',
        Event\Crypt\End::NAME      => 'onCryptEnd',
        Event\Sync\Start::NAME     => 'onSyncStart',
        Event\Sync\End::NAME       => 'onSyncEnd',
        Event\App\End::NAME        => 'onAppEnd',
    ];
}
```

Inside the custom `PresignedS3 Sync` we also emit fine-grained
`chunk_uploaded` progress (we know `chunks_done / chunks_total`) by
calling a small `ProgressEmitter` injected at registration time — bypassing
phpbu's event types since phpbu has no intra-stage events.

### 5.2 Wire

- **New CP endpoint** (M4.1):
  `POST /agent-cb/v1/backups/{snapshot_id}/progress`
- Signed with the same Ed25519 scheme used by `presignChunks` /
  `submitManifest` — `Signer::signHeaders('POST', $path, $body)` is already
  available in the agent.
- Body:
  ```json
  {
    "snapshot_id":  "snap_…",
    "phase":        "uploading",
    "phase_detail": {"chunks_done": 17, "chunks_total": 42, "bytes_done": 71303168},
    "ts":           "2026-05-28T12:34:56Z"
  }
  ```
- **At-most-once, fire-and-forget**: failures to deliver progress must NOT
  fail the backup. Wrap the POST in try/catch and log to debug.

### 5.3 Granularity

| Phase                         | Source of truth                       |
|-------------------------------|---------------------------------------|
| `queued`                      | CP (existing)                         |
| `started`                     | runner shim before `Runner::run()`    |
| `dumping_db`                  | `phpbu.backup_start` of DB backup     |
| `compressing_files`           | `phpbu.backup_start` of tar backup    |
| `encrypting`                  | `phpbu.crypt_start`                   |
| `uploading_chunk_{n}_of_{N}`  | custom `PresignedS3 Sync` per chunk   |
| `submitting_manifest`         | custom `PresignedS3 Sync` final step  |
| `completed` / `failed`        | `phpbu.app_end` (with `result.wasSuccessful()`) |

### 5.4 CP-side storage

Add a `backup_snapshot_progress` JSONB column on the existing
`backup_snapshots` table — single row update per progress POST. (A
separate progress-events table is over-engineered for V0; we only need the
latest phase, not the history.)

```sql
ALTER TABLE backup_snapshots
  ADD COLUMN progress JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN progress_updated_at TIMESTAMPTZ;
```

### 5.5 Frontend (deferred but planned)

- **V0**: TanStack Query polling at 1.5 s while `state IN ('running')`.
  Cheap, no new infra, matches the existing M3 update-progress UX pattern.
- **V1**: reuse the SSE channel already plumbed for M3 update runs
  (`/api/v1/updates/{id}/events`) — add a `backup-snapshot/{id}/events`
  topic. WebSockets are not justified for one-way progress.

---

## 6. Multi-process / async pattern

Survey of the four options, scored for V0:

| Mechanism                          | Works on FPM | Works on mod_php | Works on cli-server | Survives FPM recycle | Notes |
|------------------------------------|--------------|------------------|---------------------|----------------------|-------|
| `fastcgi_finish_request()` + sleep | ✅           | ❌               | ❌                  | ❌                   | Same FPM worker stays pinned; FPM may kill on idle timeout |
| `wp_schedule_single_event`         | ✅           | ✅               | ✅                  | n/a                  | Unreliable unless DISABLE_WP_CRON + system cron; latency up to several minutes |
| `proc_open` + detached (`pcntl_fork` not available in FPM) | ✅ | ✅ | ✅ | ✅       | True detachment; survives parent death; **recommended** |
| Action Scheduler / wp-background-processing | ✅  | ✅               | ✅                  | n/a                  | Heavy, multi-table, opinionated; brings its own queueing primitives we don't need |

**Recommendation for V0: `proc_open` detached subprocess.**

Pattern:
```php
$cmd = sprintf(
    '%s %s %s >> %s 2>&1 &',
    escapeshellarg(PHP_BINARY),
    escapeshellarg(WPMGR_AGENT_DIR . 'bin/wpmgr-backup-runner.php'),
    escapeshellarg($runId),
    escapeshellarg($logPath)
);
$proc = proc_open($cmd, [], $pipes, null, null, ['bypass_shell' => true]);
proc_close($proc);
```

Caveats:
- Use `php` not `php-fpm` (we want the CLI SAPI for the runner). On most
  hosts `PHP_BINARY` from inside FPM still points at the CLI binary; if
  not, the install command can detect and persist the CLI path.
- The runner must `chdir(WP_PATH)` and `define('ABSPATH', …)` minimally
  before requiring the autoloader — or better, **not load WordPress at all**
  and only require the plugin's Composer autoloader plus a minimal config
  loader that pulls the agent JWT and `wpmgr-config.json` from disk. This
  keeps the runner's memory footprint at ~30-50 MB (phpbu + our code),
  versus 200+ MB for a full WP bootstrap.
- The handful of hosts that block `proc_open` (some shared hosts disable
  it in `disable_functions`) → fall back to **Option A (CLI subprocess via
  WP-CLI hook + system cron)**. Detect at install time, surface in agent
  health.

---

## 7. Risks / blockers

| # | Risk | Severity | Mitigation |
|---|------|----------|------------|
| R1 | `mysqldump` binary absent on host | M | Keep WP-CLI `wp db export` fallback already in `BackupSource`; emit a `mysqldump_missing` health warning |
| R2 | `proc_open` disabled by host | M | Fall back to WP-CLI scheduled subprocess; detect at install time |
| R3 | `tar` binary absent (rare, e.g. Windows hosts) | L | Add `WP-CLI archive` fallback; document Windows-host limitation in install docs |
| R4 | Composer autoload conflict with other WP plugins that ship their own `symfony/process` or `aws-sdk-php` | H | We MUST run the runner as a detached process with its OWN autoloader — the long-lived WP request never loads vendor code we don't already ship. Inside the runner we control the entire classloader. The risk only materializes if a different plugin somehow loads `phpbu\App\…` first in the WP context — extremely unlikely (phpbu is not commonly bundled). Mitigate with `php-scoper` if needed in V1. |
| R5 | PHP 8.1 minimum: current agent says 8.0 | L | Bump `composer.json` `php` constraint to `^8.1`; bump plugin header `Requires PHP: 8.1`. 8.0 is EOL since Nov 2023. |
| R6 | phpbu's `Result` accumulates in memory (debug log, all backup status) — could grow large on big runs | L | Per-run, single-backup config; deletes itself when the runner exits. Acceptable. |
| R7 | AWS SDK PHP added if we use phpbu's S3 sync (~5 MB vendor) | L | We're using a **custom `PresignedS3`** sync — AWS SDK is NOT pulled in. We only depend on Symfony Process and sebastianfeldmann/cli. |
| R8 | phpbu 6.0.x is in maintenance mode (no 6.1; last feature release Jun 2024) | M | Project is mature, not dead. Risk acceptable; we hold the Composer constraint at `^6.0`. If abandoned later, our custom Sync/Crypter are the value-add — we can fork or move off without losing the orchestration. |
| R9 | Re-running `bin/wpmgr-backup-runner.php` twice (lost-ack retry storm) | H | Per-run row in a new `wpmgr_backup_runs` plugin-side table with `pid` and `started_at` columns; refuse to start a second runner for the same `snapshot_id` within 5 min. CP also deduplicates via the existing nonce/jti cache (`class-replay-cache.php`). |
| R10 | The agent's Composer dir is the SHIPPED plugin zip (vendor/ is committed) — adding phpbu means we ship its vendor too | M | Vendor everything in CI (`composer install --no-dev --classmap-authoritative`) and ship the resulting tree in the release zip. Strip phpbu's tests/, docs/, vendor-dev. Net ~6-8 MB added to plugin zip — acceptable for an admin-installed plugin. |

**Agent Composer status (verified):** `apps/agent/composer.json` has only
**dev** requires (`brain/monkey`, `phpstan`, `phpunit`, etc.). The
`vendor/` we read on disk is dev tooling. Adding `phpbu/phpbu` is the
**first runtime composer dependency** the agent ships — set the precedent
carefully (single vendor dir, `--no-dev` in CI, locked versions).

---

## 8. Recommended integration plan (V0)

1. **Install** — in `apps/agent`:
   ```bash
   composer require phpbu/phpbu:^6.0
   ```
   Bump `composer.json` `php` to `^8.1` and `wpmgr-agent.php`
   `Requires PHP: 8.1`. Add a build step in CI that produces a `release/`
   tree with `vendor/` containing only `--no-dev` deps, stripped of
   `.git`, tests, examples.

2. **Configuration** — generate `phpbu.xml` per run from a PHP template in
   `wp-content/wpmgr-agent/runs/{run_id}/phpbu.xml`. Two `<backup>`
   elements: one `tar` source for `wp-content` (with the existing exclude
   list), one `mysqldump` source for the DB. Both end in
   `<crypt type="age">` and `<sync type="wpmgr.presigned-s3">`.

3. **Runner shim** — `apps/agent/bin/wpmgr-backup-runner.php`:
   - require `vendor/autoload.php` and the plugin's autoloader,
   - `Factory::register('crypt', 'age', AgeCrypter::class)`,
   - `Factory::register('sync', 'wpmgr.presigned-s3', PresignedS3::class)`,
   - attach `WpmgrProgressSubscriber` to the event dispatcher,
   - `$runner->run($config)`,
   - on exit, POST `app_end` event to CP and clean scratch dir.

4. **Progress endpoint** (CP, M4.1): new
   `POST /agent-cb/v1/backups/{snapshot_id}/progress` route in
   `apps/api/internal/http/agent_handler.go`, Ed25519-verified, updates
   `backup_snapshots.progress` JSONB column. Add a new Atlas migration.

5. **Async pattern** — `proc_open` detached subprocess from the
   `backup` command handler in `class-router.php` (or wherever the
   command dispatcher lives). ACK the CP immediately with
   `{accepted:true, run_id}`. The CP transitions snapshot to `running`
   when the first `progress` POST arrives, with a 60 s watchdog: if no
   progress arrives, mark `failed/stalled`.

6. **ADR title:** **ADR-027 — Backup engine: phpbu (library, hybrid)
   with custom age Crypter and presigned-S3 Sync**. Supersedes the M4
   inline backup-pipeline design in agent code paths only; CP-side
   `presignChunks` / `submitManifest` contracts are unchanged.

---

## Sources

- [phpbu — main site](https://phpbu.de/)
- [phpbu — GitHub repo](https://github.com/sebastianfeldmann/phpbu)
- [phpbu — Packagist (`phpbu/phpbu`)](https://packagist.org/packages/phpbu/phpbu)
- [phpbu — Manual root](http://phpbu.de/manual/current/en/index.html)
- [phpbu — Manual: Configuration](http://phpbu.de/manual/current/en/configuration.html)
- [phpbu — Manual: Extending PHPBU](http://phpbu.de/manual/current/en/extending-phpbu.html)
- [phpbu — Manual: Logging](http://phpbu.de/manual/current/en/logging.html)
- [phpbu — Sync class directory](https://github.com/sebastianfeldmann/phpbu/tree/master/src/Backup/Sync)
- [phpbu — Crypter class directory](https://github.com/sebastianfeldmann/phpbu/tree/master/src/Backup/Crypter)
- [phpbu — Event class directory](https://github.com/sebastianfeldmann/phpbu/tree/master/src/Event)
- [phpbu — Tar source](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Backup/Source/Tar.php)
- [phpbu — AmazonS3v3 sync](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Backup/Sync/AmazonS3v3.php)
- [phpbu — Webhook logger](https://github.com/sebastianfeldmann/phpbu/blob/master/src/Log/Webhook.php)
- [phpbu — Releases](https://github.com/sebastianfeldmann/phpbu/releases)
- [`fastcgi_finish_request` — PHP manual](https://www.php.net/manual/en/function.fastcgi-finish-request.php)
- [FiloSottile/age — discussion #404 on PHP bindings](https://github.com/FiloSottile/age/discussions/404)
- [awesome-age — language bindings inventory](https://github.com/FiloSottile/awesome-age)
- [Delicious Brains — background processing in WordPress](https://deliciousbrains.com/background-processing-wordpress/)
- [ifsnop/mysqldump-php — pure-PHP `mysqldump` fallback (reference only)](https://github.com/ifsnop/mysqldump-php)
