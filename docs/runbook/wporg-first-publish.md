# Runbook: first publish to WordPress.org

Publishing `fleet-agent-site-manager` to the WordPress.org plugin directory for
the first time.

- SVN repository: `https://plugins.svn.wordpress.org/fleet-agent-site-manager`
- Public listing: `https://wordpress.org/plugins/fleet-agent-site-manager`
- SVN username: `Mosamlife`
- Workflow: `.github/workflows/wporg-deploy.yml`

## Read this first

Three facts govern everything below.

1. **A version number can never be reused.** Once `tags/X.Y.Z` exists it cannot
   be deleted, edited or replaced. The only remedy for a bad release is to ship
   the next version.
2. **Users receive whatever `trunk/readme.txt` names as `Stable tag`.** Not
   trunk's code. If `Stable tag` points at a tag that does not exist, the plugin
   becomes uninstallable for everyone. If it is left behind after a release,
   nobody receives the release and nothing reports an error.
3. **The public listing page is rendered from the stable tag's `readme.txt`,
   not from trunk's.** A copy fix after launch has to reach both paths.

Trunk is empty today. Nothing has ever been published. That makes the first
commit the one you cannot rehearse against real history, which is why the
sequence below runs three passes instead of one.

Every step is marked **REVERSIBLE** or **IRREVERSIBLE**.

## Phase 0: one-time setup (REVERSIBLE)

All three items below live on ONE page, `Settings > Environments >
wordpress-org`, and the order matters: the environment has to exist before it
can hold anything.

### 0.1 GitHub environment (this is the safety property)

Settings > Environments > New environment

```
Name: wordpress-org
```

Then add yourself under **Required reviewers** and save.

This is what makes the publish job pause and wait for a human click, after
Plugin Check has already gone green. **A GitHub environment that does not exist
does not fail a job that names it.** The job simply runs with no gate at all.
The build job therefore queries the environment through the API and fails the
run if it is missing or has no required reviewers, so you cannot lose this
silently. Confirm it anyway on the dry run: the run must show `Waiting for
review` before the publish job starts.

### 0.2 Environment variable

On the `wordpress-org` environment page: **Environment variables > Add**

```
Name:  WPORG_SVN_USERNAME
Value: Mosamlife
```

A variable and not a secret on purpose. GitHub masks every secret value in
logs, so a secret username turns every SVN log line into `***` and makes a
failed publish unreadable. The username is public anyway.

### 0.3 Environment secret

On the same page: **Environment secrets > Add secret**

```
Name:  WPORG_SVN_PASSWORD
Value: <your wordpress.org account password>
```

Type it straight into GitHub. Do not paste it into a chat, a file, or a commit.

**Environment scope, not repository scope, and do not create a repository
secret of this name.** This is not a filing preference, it is the entire
security property. A repository secret is readable by every workflow in the
repo, including one with no approval gate, so putting it there would let the
credential be reached without passing the approval that protects it. An
environment secret is readable only by a job that declares
`environment: wordpress-org`, and declaring it is exactly what forces that job
to stop for a human. Scoped this way the credential and the gate cannot be
separated. Only `publish` declares the environment; `build` runs Plugin Check
with no credentials at all.

Verify the scope rather than trusting the click, since both scopes look
identical from the workflow's side until the day it matters:

```sh
gh api repos/mosamlife/wpmgr/actions/secrets --jq '.secrets[].name'
# must print NOTHING. Any WPORG_ name here is repository scope: delete it.

gh api repos/mosamlife/wpmgr/environments/wordpress-org/secrets --jq '.secrets[].name'
# must print WPORG_SVN_PASSWORD
```

### 0.4 Confirm the WordPress.org profile slug (REVERSIBLE)

Open `https://profiles.wordpress.org/mosamlife/` and confirm it resolves to you.

The readme header says `Contributors: mosamlife` (lowercase) while the SVN
login is `Mosamlife`. Those are allowed to differ, but `Contributors` must be
the profile slug or the listing renders with no author, which on a brand new
plugin reads as abandonware.

### 0.5 Listing assets (REVERSIBLE)

Listing assets live at the repository root in `.wordpress-org/`. Their contents
are copied into SVN's top-level `assets/`, which is a **sibling of `trunk/` and
`tags/`**, never inside either. They never enter the plugin zip.

```
.wordpress-org/icon-128x128.png
.wordpress-org/icon-256x256.png
.wordpress-org/icon.svg            (optional; the PNGs are still required)
.wordpress-org/banner-772x250.png
.wordpress-org/banner-1544x500.png
.wordpress-org/screenshot-1.png ... screenshot-N.png
```

Rules that fail **silently** if broken, meaning no error and the asset simply
never appears:

- All filenames lowercase.
- Only `.png`, `.jpg` or `.gif`. No `.jpeg`, no `.webp`, no `.avif`.
- Screenshots numbered contiguously from 1, and there must be exactly one file
  per numbered line in the readme's `== Screenshots ==` section, in order.
- Icons max 1MB, banners max 4MB, screenshots max 10MB each.

**Assets are not version specific and need no version bump.** They can be
replaced at any time by rerunning the workflow in `assets-only` mode.

The build job checks the lowercase and extension rules for you and fails the
run rather than letting an asset vanish quietly.

## Phase 1: preflight (REVERSIBLE)

Run these locally before you trigger anything.

```bash
cd /path/to/wpmgr

# 1. The version you are about to publish forever.
grep -n "WPMGR_AGENT_VERSION" apps/agent/wpmgr-agent.php | head -1

# 2. Build the artifact and read the stamped Stable tag with your own eyes.
make agent-zip-wporg
rm -rf /tmp/wporg && mkdir -p /tmp/wporg
unzip -q release/fleet-agent-site-manager.zip -d /tmp/wporg
grep -E '^Stable tag:' /tmp/wporg/fleet-agent-site-manager/readme.txt
grep -E '^ \* (Plugin Name|Version):' /tmp/wporg/fleet-agent-site-manager/fleet-agent-site-manager.php

# 3. The self-updater must not be there. This must print nothing.
ls /tmp/wporg/fleet-agent-site-manager/includes/support/class-update-checker.php 2>/dev/null

# 4. The authoritative WordPress.org check. Zero ERROR rows is the gate.
make agent-plugincheck

# 5. The shipped-copy gate (no em dashes, no en dashes, no competitor names).
node apps/marketing/scripts/check-copy.mjs
```

Then paste the finished `apps/agent/readme.txt` into
`https://wordpress.org/plugins/developers/readme-validator/`. That validator is
the arbiter for readme format, not any local check.

Last, install `/tmp/wporg/fleet-agent-site-manager` on a real WordPress site and
confirm two things on the plugin's admin screen, because screenshots must be
captured from this build and not from a dev checkout:

- the page heading reads **Fleet Agent Site Manager**, not "WPMgr Agent"
- there is **no "Agent update" section**, because the wp.org build has no
  self-updater

## Phase 2: pass 1, dry run (REVERSIBLE, no credentials touched)

Actions > wp.org deploy > Run workflow

```
Use workflow from: main   (or the tag carrying the launch version)
version:  0.61.127        (whatever WPMGR_AGENT_VERSION says; no leading v)
mode:     release
dry_run:  true
```

What this proves, without committing anything:

- the version input matches `WPMGR_AGENT_VERSION` on that ref
- the `wordpress-org` approval gate exists and has a reviewer
- `tags/<version>` is free upstream
- `make agent-zip-wporg` builds and Plugin Check is green
- the built tree's `Stable tag`, plugin header `Version` and
  `WPMGR_AGENT_VERSION` all equal the version
- the staged `svn status` is what you expect

**Read the printed `svn status` carefully.** On a first publish every line is an
`A` (add) and there is nothing to diff against, so this listing is your only
rehearsal. Check that `trunk/` contains the plugin files at its top level and
not a nested `fleet-agent-site-manager/` directory.

The dry run still requires you to approve the environment gate. That is
deliberate: it proves the gate works before it is protecting anything.

The password is never read on a dry run.

## Phase 3: pass 2, assets only (IRREVERSIBLE, low consequence)

```
version:  0.61.127
mode:     assets-only
dry_run:  false
```

This commits `assets/` and nothing else. It touches no code, no tag and no
`Stable tag`, and it needs no version bump. The build job still builds and
still runs Plugin Check, because one code path is worth more than ten saved
minutes here.

This is deliberately your credentials smoke test, on the one commit where a
mistake costs nothing: assets are versionless and replaceable, and nothing is
public yet because trunk still has no code.

Approve at the environment gate. Then wait for CDN propagation, which runs from
minutes to hours, and eyeball the icon and banner.

The publish job asserts that nothing outside `assets/` was staged, so an
`assets-only` run cannot quietly touch trunk.

## Phase 4: pass 3, release (IRREVERSIBLE AND PERMANENT)

```
version:  0.61.127
mode:     release
dry_run:  false
```

Plugin Check runs again inside this same run. **Approve at the environment gate
only after it is green.**

This commits `trunk/`, `tags/<version>/` and any asset changes in **one atomic
revision**. There is no revision in which the tag exists without trunk naming
it, which is what makes the "tag published but nobody receives it" failure
structurally impossible rather than merely unlikely.

After the commit the workflow re-reads the **server** and fails the run loudly
if:

- `tags/<version>` is not there, or
- the server's `trunk/readme.txt` does not name `Stable tag: <version>`, or
- `tags/<version>/readme.txt` does not name it either

If that verification step goes red, read Phase 6.

`tags/<version>` can never be deleted, edited or reused from this moment.

## Phase 5: verify the listing (REVERSIBLE)

Allow about 15 minutes for the listing page to build.

Open `https://wordpress.org/plugins/fleet-agent-site-manager` and check, in this
order:

1. the title and short description render, and the short description is not cut
   off with an ellipsis
2. the icon and banner appear (if not, give the CDN longer before assuming
   anything is wrong)
3. every screenshot has its caption and none is broken
4. the Privacy and External services sections are present and are **not** cut
   off with an ellipsis, which is what the directory does when the description
   budget overflows
5. the offered download version equals the version you published

Then install from the directory onto a clean site and complete the pairing flow
exactly as the readme's Installation steps describe it: save the control plane
URL, get a pairing code from the dashboard, paste it, click Enroll. If any step
does not work as written, fix the readme before any traffic arrives (Phase 7).

Only now go to Product Hunt.

## Phase 6: when something is wrong

### The workflow refuses to run at the same version again

That is intended. `tags/X.Y.Z` exists, so `mode=release` at `X.Y.Z` is refused.
Somebody under launch pressure will read this as a bug. It is not. Bump
`WPMGR_AGENT_VERSION`, land it on main, and publish the next number.

### The release published but the server verification went red

The commit is atomic, so this should not happen. If it does, the message names
which of the three checks failed. The fix for a stale `Stable tag` is a commit
to `trunk/readme.txt` alone and **needs no version bump**:

```bash
svn checkout --depth immediates https://plugins.svn.wordpress.org/fleet-agent-site-manager svn
cd svn
svn update --set-depth infinity trunk
# edit trunk/readme.txt so the Stable tag line names the published version
svn commit --username Mosamlife -m "Point Stable tag at 0.61.127"
```

### A published release is broken

There is no unpublish. Two options, in order:

1. Ship the fix as the next version. This is almost always the right answer.
2. As an emergency stop only, point `Stable tag` in `trunk/readme.txt` at the
   previous good tag using the commands above. Every site is then offered a
   downgrade, which WordPress will apply. Do this only if the broken release is
   actively harming sites.

The workflow refuses to publish a version lower than trunk's current
`Stable tag`, so option 2 cannot be done through it. That is on purpose.

## Phase 7: after launch, the thing that will bite you

**A readme-only fix must reach both `trunk/readme.txt` and
`tags/<stable tag>/readme.txt`.** The public listing page is rendered from the
stable tag's readme, so a commit to trunk alone changes nothing that anyone can
see. It needs no version bump, but it does need both paths.

Use the workflow:

```
version:  0.61.127      (the version currently published, not a new one)
mode:     readme-only
dry_run:  true          (then false)
```

`readme-only` refuses to run unless `tags/<version>` exists **and** trunk's
`Stable tag` already equals that version, so it cannot be pointed at the wrong
tag.

Also add to the release checklist: `Tested up to:` in `apps/agent/readme.txt`
goes stale on every WordPress release, and a stale value is the single field
most likely to make a healthy listing look abandoned.

## Appendix: doing it by hand

Only if the workflow is unavailable. Every safety check above is then yours to
perform. You need `svn` locally (`brew install subversion`).

```bash
SVN=https://plugins.svn.wordpress.org/fleet-agent-site-manager
V=0.61.127

# Build the exact artifact. Never copy apps/agent/readme.txt from the source
# tree: it carries a placeholder Stable tag and only the build stamps the real
# value. Publishing the source copy points Stable tag at a tag that does not
# exist and makes the plugin uninstallable.
make agent-zip-wporg
make agent-plugincheck
rm -rf /tmp/wporg && mkdir -p /tmp/wporg
unzip -q release/fleet-agent-site-manager.zip -d /tmp/wporg
grep -E '^Stable tag:' /tmp/wporg/fleet-agent-site-manager/readme.txt   # must equal $V

# Check out shallow. A full checkout pulls every historical tag.
rm -rf /tmp/svn
svn checkout --depth immediates "$SVN" /tmp/svn
cd /tmp/svn
svn update --set-depth infinity trunk
svn update --set-depth infinity assets   # if assets/ does not exist: svn mkdir assets

# Stage the code. -c compares by checksum so unchanged files are left alone.
rsync -rc --delete --exclude '.svn/' /tmp/wporg/fleet-agent-site-manager/ trunk/

# Stage the listing assets (sibling of trunk; no version bump involved).
rsync -rc --delete --exclude '.svn/' /path/to/wpmgr/.wordpress-org/ assets/

# Both halves, or the release ships a stale tree.
# New files are unversioned until svn add sees them.
svn add --force --no-ignore trunk assets
# Files rsync deleted are "missing" until svn rm sees them. Skip this and the
# release keeps shipping files you deleted months ago, forever. The trailing @
# stops svn parsing an @ in a filename as a peg revision.
svn status | sed -n 's/^![[:space:]]*//p' | while IFS= read -r p; do svn rm --force "$p@"; done

# Serve the images with the right content type.
find assets -type f -name '*.png' -exec svn propset svn:mime-type image/png {} +
find assets -type f -name '*.jpg' -exec svn propset svn:mime-type image/jpeg {} +
find assets -type f -name '*.svg' -exec svn propset svn:mime-type image/svg+xml {} +

# Copy the WORKING COPY, not the URL, so the pending adds are carried into the
# tag and trunk plus tag land in ONE commit.
svn cp trunk "tags/$V"

# Look before you leap.
svn status
grep -E '^Stable tag:' trunk/readme.txt        # must equal $V
grep -E '^Stable tag:' "tags/$V/readme.txt"    # must equal $V

# IRREVERSIBLE.
svn commit --username Mosamlife -m "Release $V"

# Verify against the server, not the working copy.
svn ls "$SVN/tags/$V" >/dev/null && echo "tag OK"
svn cat "$SVN/trunk/readme.txt" | grep -E '^Stable tag:'
```

Two notes on the by-hand path. There is no `svn import` step even though trunk
is empty: importing produces a trunk that diverges from what the workflow later
copies, and the first automated run then shows a large and alarming diff. And
`svn` will prompt for the password interactively, which is correct here; do not
put it on the command line, where it lands in your shell history and in the
process list.
