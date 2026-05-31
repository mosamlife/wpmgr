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
#   scripts/release-agent.sh [--dry-run]
#
# Env overrides:
#   WPMGR_RELEASE_BUCKET   (default: wpmgr-chunks-prod)
#   WPMGR_RELEASE_PREFIX   (default: agent-releases)
#   WPMGR_AGENT_MIN_VERSION(default: 0.0.0)   minimum on-disk version this applies to
#   WPMGR_AGENT_TESTED     (default: 6.8)     "Tested up to" WP version for the UI
#
set -euo pipefail

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

BUCKET="${WPMGR_RELEASE_BUCKET:-wpmgr-chunks-prod}"
PREFIX="${WPMGR_RELEASE_PREFIX:-agent-releases}"
MIN_VERSION="${WPMGR_AGENT_MIN_VERSION:-0.0.0}"
TESTED="${WPMGR_AGENT_TESTED:-6.8}"

ZIP="release/wpmgr-agent.zip"
SLUG="wpmgr-agent"
PLUGIN_FILE="wpmgr-agent/wpmgr-agent.php"

die() { echo "release-agent: $*" >&2; exit 1; }

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
header="$(unzip -p "$ZIP" "$PLUGIN_FILE")"
VERSION="$(printf '%s\n' "$header" | grep -oE "WPMGR_AGENT_VERSION', *'[^']+'" | head -1 | sed -E "s/.*'([^']+)'.*/\1/")"
[[ -n "$VERSION" ]] || die "could not parse WPMGR_AGENT_VERSION from $PLUGIN_FILE"
REQUIRES="$(printf '%s\n' "$header" | grep -oiE 'Requires at least: *[0-9.]+' | head -1 | grep -oE '[0-9.]+' || echo '6.0')"
REQUIRES_PHP="$(printf '%s\n' "$header" | grep -oiE 'Requires PHP: *[0-9.]+' | head -1 | grep -oE '[0-9.]+' || echo '8.1')"

SHA256="$(sha256_of "$ZIP")"
SIZE="$(wc -c < "$ZIP" | tr -d ' ')"
OBJECT_KEY="${PREFIX}/${VERSION}/wpmgr-agent.zip"

# --- write latest.json -------------------------------------------------------
LATEST="release/latest.json"
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
  echo "release-agent: --dry-run, not uploading. latest.json contents:"
  cat "$LATEST"
  exit 0
fi

# --- upload: package FIRST, manifest LAST ------------------------------------
gcloud storage cp --content-type=application/zip "$ZIP" "gs://${BUCKET}/${OBJECT_KEY}"
gcloud storage cp --content-type=application/json --cache-control="no-store" "$LATEST" "gs://${BUCKET}/${PREFIX}/latest.json"

echo "release-agent: published ${VERSION}."
