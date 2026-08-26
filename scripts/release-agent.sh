#!/usr/bin/env bash
#
# release-agent.sh — publish a WPMgr agent release for CP-driven self-update
# (ADR-042). Reads the already-built zip at release/wpmgr-agent.zip, derives the
# version from the zip's own main file, computes the package sha256 + byte size,
# writes latest.json, and uploads BOTH to object storage:
#
#   gs://$BUCKET/$PREFIX/<version>/wpmgr-agent.zip   (immutable package)
#   gs://$BUCKET/$PREFIX/latest.json                 (the pointer the CP reads)
#
# Ordering is load-bearing: the versioned package is uploaded FIRST, then
# latest.json LAST, so the manifest never points at a package that is not yet in
# place. The agent's signature + downgrade-guard + sha256 checks (ADR-042 §2) are
# the real protection; this script is the trust boundary that must not foot-gun.
#
# Usage:
#   scripts/release-agent.sh [--dry-run] [--out PATH]
#
#   --dry-run   compute + write the manifest, upload nothing.
#   --out PATH  write the manifest to PATH instead of release/latest.json.
#
# Combining the two emits the manifest and touches no storage at all, which is
# how release.yml produces the agent-release.json GitHub Release asset. That
# asset carries the fields the GitHub Releases API cannot express (min_version,
# requires, requires_php, tested, sections), so a self-hosted control plane can
# mirror our public release into its OWN bucket with the same guarantees as the
# object-storage channel. Both channels are generated here, from the same zip,
# by the same field construction, so they describe a build identically.
#
# Env overrides:
#   WPMGR_RELEASE_BUCKET   (default: wpmgr-chunks-prod)
#   WPMGR_RELEASE_PREFIX   (default: agent-releases)
#   WPMGR_AGENT_MIN_VERSION(default: 0.0.0)   minimum on-disk version this applies to
#   WPMGR_AGENT_TESTED     (default: parsed from apps/agent/readme.txt's own
#                            "Tested up to:" line; an explicit override skips
#                            that parse entirely — see GH #515)
#
set -euo pipefail

die() { echo "release-agent: $*" >&2; exit 1; }

DRY_RUN=0
OUT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --out)
      [[ $# -ge 2 && -n "$2" ]] || die "--out requires a path"
      OUT="$2"; shift 2 ;;
    --out=*)
      OUT="${1#--out=}"
      [[ -n "$OUT" ]] || die "--out requires a path"
      shift ;;
    -h|--help)
      echo "usage: release-agent.sh [--dry-run] [--out PATH]"
      exit 0 ;;
    *) die "unknown argument: $1 (usage: release-agent.sh [--dry-run] [--out PATH])" ;;
  esac
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

BUCKET="${WPMGR_RELEASE_BUCKET:-wpmgr-chunks-prod}"
PREFIX="${WPMGR_RELEASE_PREFIX:-agent-releases}"
MIN_VERSION="${WPMGR_AGENT_MIN_VERSION:-0.0.0}"
# TESTED is resolved below, after the zip's own header is read — its default
# is derived from apps/agent/readme.txt, not a literal (GH #515).

ZIP="release/wpmgr-agent.zip"
SLUG="wpmgr-agent"
PLUGIN_FILE="wpmgr-agent/wpmgr-agent.php"

# --- preconditions -----------------------------------------------------------
command -v unzip >/dev/null || die "unzip is required"
[[ -f "$ZIP" ]] || die "missing $ZIP — run 'make agent-zip' first"
if [[ "$DRY_RUN" -eq 0 ]]; then
  command -v gcloud >/dev/null || die "gcloud is required (or pass --dry-run)"
fi

# sha256 tool (macOS: shasum; Linux: sha256sum) ------------------------------
sha256_of() {
  if command -v shasum >/dev/null; then shasum -a 256 "$1" | awk '{print $1}';
  elif command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}';
  else die "need shasum or sha256sum"; fi
}

# --- validate the zip's structure (stable slug is mandatory) -----------------
top_dirs="$(unzip -Z1 "$ZIP" | sed 's#/.*##' | sort -u)"
[[ "$top_dirs" == "$SLUG" ]] || die "zip top-level must be exactly '$SLUG/' but was: $(echo "$top_dirs" | tr '\n' ' ')"
unzip -l "$ZIP" "$PLUGIN_FILE" >/dev/null 2>&1 || die "zip is missing $PLUGIN_FILE"

# --- derive version + requirements from the zip's OWN main file --------------
# Every extraction pipeline below ends in `|| true` so a non-match never trips
# `set -o pipefail` before the explicit `[[ -n ... ]] || die` check gets to run
# — without it, a failed grep mid-pipeline kills the script under `set -e`
# with bash's own bare exit 1 and no message, which is silent in a different
# costume. The explicit check is what actually produces the actionable error.
header="$(unzip -p "$ZIP" "$PLUGIN_FILE")"
VERSION="$(printf '%s\n' "$header" | grep -oE "WPMGR_AGENT_VERSION', *'[^']+'" | head -1 | sed -E "s/.*'([^']+)'.*/\1/" || true)"
[[ -n "$VERSION" ]] || die "could not parse WPMGR_AGENT_VERSION from $PLUGIN_FILE"
REQUIRES="$(printf '%s\n' "$header" | grep -oiE 'Requires at least: *[0-9.]+' | head -1 | grep -oE '[0-9.]+' || true)"
[[ -n "$REQUIRES" ]] || die "could not parse 'Requires at least' from $PLUGIN_FILE's header — refusing to publish a guessed minimum WordPress version"
REQUIRES_PHP="$(printf '%s\n' "$header" | grep -oiE 'Requires PHP: *[0-9.]+' | head -1 | grep -oE '[0-9.]+' || true)"
[[ -n "$REQUIRES_PHP" ]] || die "could not parse 'Requires PHP' from $PLUGIN_FILE's header — refusing to publish a guessed minimum PHP version"

# --- derive "Tested up to" from readme.txt, the wordpress.org listing page --
# GH #515: this used to be `${WPMGR_AGENT_TESTED:-6.8}` and nobody ever set the
# override, so every published manifest claimed the agent was tested only up
# to WordPress 6.8 regardless of what readme.txt actually declared. The
# default is now the parsed value. WPMGR_AGENT_TESTED remains available as an
# explicit override (and, when set, skips the readme parse entirely); when it
# is unset, a missing file or an unparseable line is a hard error — a wrong
# compatibility floor published silently is the defect being fixed, so a
# fallback here would only reintroduce it in a new costume.
if [[ -n "${WPMGR_AGENT_TESTED:-}" ]]; then
  TESTED="$WPMGR_AGENT_TESTED"
else
  README_FILE="apps/agent/readme.txt"
  [[ -f "$README_FILE" ]] || die "missing $README_FILE — cannot derive 'Tested up to'; refusing to publish a guessed WordPress compatibility floor (set WPMGR_AGENT_TESTED to override explicitly)"
  TESTED="$(grep -iE '^Tested up to:' "$README_FILE" | head -1 | grep -oE '[0-9]+(\.[0-9]+)*' | head -1 || true)"
  [[ -n "$TESTED" ]] || die "could not parse a 'Tested up to:' version from $README_FILE — refusing to publish a guessed WordPress compatibility floor (set WPMGR_AGENT_TESTED to override explicitly)"
fi

SHA256="$(sha256_of "$ZIP")"
SIZE="$(wc -c < "$ZIP" | tr -d ' ')"
OBJECT_KEY="${PREFIX}/${VERSION}/wpmgr-agent.zip"

# --- write the manifest ------------------------------------------------------
# One construction, both channels: latest.json for object storage and the
# agent-release.json GitHub Release asset are byte-identical for a given zip.
LATEST="${OUT:-release/latest.json}"
mkdir -p "$(dirname "$LATEST")"
cat > "$LATEST" <<JSON
{
  "slug": "${SLUG}",
  "plugin": "${PLUGIN_FILE}",
  "version": "${VERSION}",
  "min_version": "${MIN_VERSION}",
  "package_object_key": "${OBJECT_KEY}",
  "package_sha256": "${SHA256}",
  "package_size": ${SIZE},
  "requires": "${REQUIRES}",
  "requires_php": "${REQUIRES_PHP}",
  "tested": "${TESTED}",
  "sections": {
    "description": "WPMgr Agent ${VERSION}. Connects this WordPress site to a WPMgr control plane for backups, updates, monitoring, and security scanning."
  }
}
JSON

echo "release-agent: version=${VERSION} sha256=${SHA256} size=${SIZE}B"
echo "release-agent: package  -> gs://${BUCKET}/${OBJECT_KEY}"
echo "release-agent: manifest -> gs://${BUCKET}/${PREFIX}/latest.json"
echo "release-agent: wrote ${LATEST}"

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "release-agent: --dry-run, not uploading. ${LATEST} contents:"
  cat "$LATEST"
  exit 0
fi

# --- upload: package FIRST, manifest LAST ------------------------------------
gcloud storage cp --content-type=application/zip "$ZIP" "gs://${BUCKET}/${OBJECT_KEY}"
gcloud storage cp --content-type=application/json --cache-control="no-store" "$LATEST" "gs://${BUCKET}/${PREFIX}/latest.json"

echo "release-agent: published ${VERSION}."
