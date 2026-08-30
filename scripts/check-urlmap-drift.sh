#!/usr/bin/env bash
# scripts/check-urlmap-drift.sh
#
# The COMMITTED url-map versus the one actually live in GCP.
#
# DELIBERATELY SEPARATE from scripts/check-urlmap-routes.sh. That guard is the
# valuable one — it proves every route the API mounts is routed, it needs no
# cloud access, and it runs on every PR. This one needs credentials, so it
# cannot run on a PR from a fork and must never be a precondition for the
# credential-free check. Keeping them in one script would have gated the half
# that matters behind the half that cannot always run.
#
# RUN IT (needs `gcloud auth login` and read access to the project):
#   make check-urlmap-drift
#   scripts/check-urlmap-drift.sh
#
# Exit 0 = live matches committed. Exit 1 = drift. Exit 2 = could not check.
# "Could not check" is never reported as a pass.
#
# EXPECTED DRIFT RIGHT NOW: the three /.well-known OAuth discovery paths are in
# the committed file and not yet applied to GCP, so this exits 1 until someone
# runs the import command it prints. That is the file doing its job — it is the
# desired state, not a snapshot.
#
# fingerprint: is stripped from the live side before comparing. It is an
# optimistic-locking token that changes on every update, so comparing it would
# report drift after every unrelated apply.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"

URLMAP_FILE="${WPMGR_URLMAP_FILE:-$REPO_ROOT/infra/urlmap.yaml}"
URLMAP_NAME="${WPMGR_URLMAP_NAME:-wpmgr-urlmap}"
PROJECT="${WPMGR_GCP_PROJECT:-wpmgr-prod}"

fatal() { printf 'check-urlmap-drift: FATAL: %s\n' "$1" >&2; exit 2; }

[ -f "$URLMAP_FILE" ] || fatal "committed url-map not found: $URLMAP_FILE"

GCLOUD="$(command -v gcloud || true)"
# Resolve the binary rather than assuming it. A step that cannot find its
# binary fails loudly; it is never skipped, because a skipped drift check that
# prints nothing is indistinguishable from a clean one.
[ -n "$GCLOUD" ] || fatal "gcloud not found on PATH. Install the Google Cloud SDK or run this where it is available.
  This check is not optional-by-absence: it reports 'could not check', not 'no drift'."

WORK="$(mktemp -d)" || fatal "could not create a temp dir"
trap 'rm -rf "$WORK"' EXIT INT TERM

LIVE="$WORK/live.yaml"
if ! "$GCLOUD" compute url-maps export "$URLMAP_NAME" --global --project="$PROJECT" > "$LIVE" 2>"$WORK/err"; then
  printf 'check-urlmap-drift: FATAL: could not export %s from project %s.\n' "$URLMAP_NAME" "$PROJECT" >&2
  sed 's/^/  | /' "$WORK/err" >&2
  printf '  Check `gcloud auth list` and that the account can read compute url-maps.\n' >&2
  exit 2
fi

if [ ! -s "$LIVE" ]; then
  fatal "the live export was empty. Refusing to report 'no drift' over an empty comparison."
fi

# Normalise both sides: drop comments, blank lines and the fingerprint token,
# then sort-independent compare is NOT used — ordering is significant in a
# url-map (first matching path rule wins), so this is a strict ordered diff.
normalise() {
  sed -e 's/[[:space:]]*$//' -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' -e '/^fingerprint:/d' "$1"
}

normalise "$LIVE"        > "$WORK/live.norm"
normalise "$URLMAP_FILE" > "$WORK/committed.norm"

if [ ! -s "$WORK/committed.norm" ]; then
  fatal "the committed url-map is empty after stripping comments: $URLMAP_FILE"
fi

if diff -u "$WORK/live.norm" "$WORK/committed.norm" > "$WORK/diff" 2>&1; then
  printf 'check-urlmap-drift: OK — live %s in %s matches %s.\n' "$URLMAP_NAME" "$PROJECT" "$URLMAP_FILE"
  exit 0
fi

printf '\ncheck-urlmap-drift: DRIFT between the live url-map and the committed one.\n' >&2
printf '  -  live in GCP (%s / %s)\n' "$PROJECT" "$URLMAP_NAME" >&2
printf '  +  committed (%s)\n\n' "$URLMAP_FILE" >&2
sed 's/^/  /' "$WORK/diff" >&2
printf '\nIf the committed file is the intended state, apply it:\n' >&2
printf '  gcloud compute url-maps import %s --global --source=%s --project=%s\n' \
  "$URLMAP_NAME" "$URLMAP_FILE" "$PROJECT" >&2
printf 'If GCP is right and the file is stale, re-export and commit the result.\n\n' >&2
exit 1
