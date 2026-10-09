#!/usr/bin/env bash
# scripts/test-s3-image.sh
#
# The object-store image the integration tests run on, read out of the one place
# that names it, and (with --check) a proof that nothing else does.
#
#   scripts/test-s3-image.sh                 print the image reference
#   scripts/test-s3-image.sh --check         print it, and fail on a second literal
#   scripts/test-s3-image.sh [--check] ROOT  run against another tree
#
# WHY THIS EXISTS (GH #819). The tests' S3 store was a MinIO image that was
# pinned twice, once in a Go file and once in a workflow's pre-pull list, as two
# literals nothing connected. Its registry stopped serving it, and the next
# pin would have drifted the same way: whoever bumps the Go constant has to
# remember the workflow, and nothing fails when they do not. So the Go constant
# is the only literal, and the workflow asks this script for it.
#
# THE DECLARATION. apps/api/tests/ holds exactly one line of the shape
#
#   const s3TestImage = "repo:tag@sha256:<64 hex>"
#
# optionally followed by a // comment. The script anchors on that declaration,
# not on a substring, so a commented-out old pin, a doc comment that names the
# image and a grouped const block are all different things to it: the first two
# are not declarations, and the third is a declaration in the wrong shape, which
# is an error with its own message rather than a silent miss.
#
# THE REFERENCE MUST BE PINNED BY DIGEST. A tag moves; a pre-pull and a test
# that resolve it at different times can run different servers. A reference
# without a sha256 digest, or with a digest that is not 64 lowercase hex
# characters, is an error. The tag may be absent; if present it is only for the
# reader.
#
# WHAT --check ADDS, all of it an error when it finds something:
#   * the declaration is the only place under apps/api/tests that names an S3
#     server image repository. Go comments (line, trailing and block, one line or
#     several) are removed before matching, so prose may say what the fixture used
#     to be; string and rune literals are kept, so an image named in code may not.
#     "//" inside a string is not a comment, and neither is "/*".
#   * no non-comment line in .github/workflows names one either, and
#     api-integration.yml has a non-comment line that CAPTURES this script's
#     output with $(...). A workflow that stopped calling it would fall back to a
#     literal sooner or later; this says so before that happens. Capturing is the
#     structure being asked for, not a mention of the file name: a pre-pull that
#     only prints the image, or an error message that names the script, derives
#     nothing.
# The list of repositories it knows is S3_IMAGE_REPOS below. It is a list of
# names, so a server that is not on it is not caught: add it when a new one is
# chosen.
#
# FAILING CLOSED. Standard output carries the reference and nothing else, and
# only on success, so a caller that captures it never captures a half answer.
# Every other outcome writes to standard error and exits non-zero. A missing
# tests directory, no declaration, no workflow files to scan: all errors, never
# "nothing found, so fine".
#
# Exit codes: 0 ok, 1 a check failed, 2 the script was misused or ROOT is not a
# directory.
#
# scripts/test-s3-image_test.sh is the regression suite. `make check-s3-image`
# and `make check-s3-image-test` are the local names for the two, and ci.yml runs
# both, the suite first, so a broken guard cannot pass by failing open.
#
# PORTABILITY. bash 3.2 (what macOS ships) and POSIX tools: no mapfile, no
# associative arrays, no sed -i, no grep -P, no \b or \s.

set -uo pipefail

DECL_NAME='s3TestImage'
TESTS_DIR='apps/api/tests'
WF_DIR='.github/workflows'
WF_INTEGRATION='.github/workflows/api-integration.yml'
SELF_NAME='scripts/test-s3-image.sh'

# Repositories of S3 server images. A code line that names one anywhere but the
# declaration is a second literal.
S3_IMAGE_REPOS='chrislusf/seaweedfs|seaweedfs/seaweedfs|minio/minio|bitnami/minio|bitnamilegacy/minio|pgsty/minio'

# The shape of a call that derives something from this script: its path inside a
# command substitution, e.g. s3_image=$(../../scripts/test-s3-image.sh).
CALL_RE='\$\([^)]*scripts/test-s3-image\.sh'

# [registry[:port]/]repo[:tag]@sha256:<64 lowercase hex>
REF_RE='^([a-z0-9.-]+(:[0-9]+)?/)?[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9_][A-Za-z0-9_.-]*)?@sha256:[0-9a-f]{64}$'

# go_code_hits FILE: print "LINE:text" for every line of a Go file whose CODE
# names an S3 server image repository, except the declaration line. Comments are
# removed first, by a small scanner that knows the four things a Go line can hold
# that look like comment syntax but are not:
#   "..."  an interpreted string, where \" does not end it
#   `...`  a raw string, which may span lines and holds // and /* as text
#   '.'    a rune literal, which can be '"' or '/'
# and the two comment forms, // to the end of the line and /* ... */ across lines.
# A block comment or raw string that is still open at the end of a line stays open
# on the next one. The scan state is reset at the start of every file.
# BEGIN go_code_hits
go_code_hits() {
  awk -v repos="$S3_IMAGE_REPOS" -v decl="^const ${DECL_NAME} = " -v sq="'" '
    function strip(line,    out, i, n, c, d, q) {
      out = ""
      n = length(line)
      i = 1
      while (i <= n) {
        c = substr(line, i, 1)
        d = substr(line, i + 1, 1)
        if (in_block) {
          if (c == "*" && d == "/") { in_block = 0; i += 2; out = out " " } else { i++ }
          continue
        }
        if (in_raw) {
          out = out c
          if (c == "`") { in_raw = 0 }
          i++
          continue
        }
        if (c == "/" && d == "/") { break }
        if (c == "/" && d == "*") { in_block = 1; i += 2; continue }
        if (c == "`") { in_raw = 1; out = out c; i++; continue }
        if (c == "\"" || c == sq) {
          q = c
          out = out c
          i++
          while (i <= n) {
            c = substr(line, i, 1)
            out = out c
            i++
            if (c == "\\") {
              if (i <= n) { out = out substr(line, i, 1); i++ }
            } else if (c == q) {
              break
            }
          }
          continue
        }
        out = out c
        i++
      }
      return out
    }
    FNR == 1 { in_block = 0; in_raw = 0 }
    {
      code = strip($0)
      if (code ~ repos && code !~ decl) { print FNR ":" $0 }
    }
  ' "$1"
}
# END go_code_hits

usage() {
  cat <<'USAGE'
Usage: scripts/test-s3-image.sh [--check] [ROOT]

Prints the digest-pinned S3 store image the integration tests run on, read from
the `const s3TestImage` declaration under apps/api/tests.

  --check   also fail when another file names an S3 server image, or when
            api-integration.yml does not call this script.

ROOT defaults to the repository this script lives in, or to
$WPMGR_S3_IMAGE_ROOT when that is set.

Exit 0: ok. Exit 1: a check failed. Exit 2: misuse.
USAGE
}

check=0
root_arg=''
for arg in "$@"; do
  case "$arg" in
    -h | --help)
      usage
      exit 0
      ;;
    --check) check=1 ;;
    -*)
      echo "ERROR: unknown option: $arg" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [ -n "$root_arg" ]; then
        echo "ERROR: more than one ROOT given: $root_arg and $arg" >&2
        exit 2
      fi
      root_arg="$arg"
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${root_arg:-${WPMGR_S3_IMAGE_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}}"

if [ ! -d "$ROOT" ]; then
  echo "ERROR: $ROOT is not a directory." >&2
  exit 2
fi
cd "$ROOT" || exit 2

errors=0
err() {
  echo "ERROR: $*" >&2
  errors=$((errors + 1))
}

# ---------------------------------------------------------------------------
# The declaration
# ---------------------------------------------------------------------------
if [ ! -d "$TESTS_DIR" ]; then
  echo "ERROR: $TESTS_DIR does not exist under $ROOT; there is nothing to read the image from." >&2
  exit 1
fi

go_files="$(find "$TESTS_DIR" -type f -name '*.go' | sort)"
if [ -z "$go_files" ]; then
  echo "ERROR: no .go files under $TESTS_DIR; there is nothing to read the image from." >&2
  exit 1
fi

DECL_RE="^const ${DECL_NAME} = \"[^\"]*\"[[:space:]]*(//.*)?\$"
# Anything that assigns the name at the start of a line, in any shape. Used only
# to explain a miss: a declaration in the wrong shape is a different mistake
# from no declaration at all.
LOOSE_RE="^[[:space:]]*(const|var)?[[:space:]]*${DECL_NAME}([[:space:]]+[A-Za-z]+)?[[:space:]]*="

decl_hits=''
loose_hits=''
decl_count=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  hits="$(grep -n -E "$DECL_RE" "$f" || true)"
  if [ -n "$hits" ]; then
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      decl_hits="${decl_hits}${f}:${h}
"
      decl_count=$((decl_count + 1))
    done <<EOF
$hits
EOF
  fi
  loose="$(grep -n -E "$LOOSE_RE" "$f" || true)"
  if [ -n "$loose" ]; then
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      loose_hits="${loose_hits}${f}:${h}
"
    done <<EOF
$loose
EOF
  fi
done <<EOF
$go_files
EOF

if [ "$decl_count" -eq 0 ]; then
  err "no single-line 'const ${DECL_NAME} = \"...\"' declaration under $TESTS_DIR."
  if [ -n "$loose_hits" ]; then
    echo "       lines that assign ${DECL_NAME} in some other shape (the script reads only the single-line const form):" >&2
    printf '%s' "$loose_hits" | sed 's/^/         /' >&2
  fi
  exit 1
fi

if [ "$decl_count" -gt 1 ]; then
  err "${decl_count} declarations of ${DECL_NAME} under $TESTS_DIR; there must be exactly one:"
  printf '%s' "$decl_hits" | sed 's/^/         /' >&2
  exit 1
fi

decl_line="$(printf '%s' "$decl_hits" | head -n 1)"
image="$(printf '%s' "$decl_line" | sed -E "s/^.*[0-9]+:const ${DECL_NAME} = \"([^\"]*)\".*\$/\\1/")"

if [ -z "$image" ]; then
  err "the ${DECL_NAME} declaration is an empty string: $decl_line"
  exit 1
fi

if ! printf '%s\n' "$image" | grep -Eq "$REF_RE"; then
  err "${DECL_NAME} is not pinned by digest: \"$image\""
  echo "       expected [registry/]repo[:tag]@sha256:<64 lowercase hex characters>." >&2
  echo "       a tag alone moves; the pre-pull and the test would not be sure to run the same server." >&2
  echo "       declared at: $decl_line" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# --check: nothing else names an S3 server image, and the workflow calls us
# ---------------------------------------------------------------------------
if [ "$check" -eq 1 ]; then
  # Go: code that is not the declaration, naming a repo. A file that never
  # mentions one cannot hold a finding, so only the files that do are scanned;
  # the scan itself is the comment-aware go_code_hits above.
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -q -E "$S3_IMAGE_REPOS" "$f" || continue
    bad="$(go_code_hits "$f")"
    if [ -n "$bad" ]; then
      err "$f names an S3 server image outside the ${DECL_NAME} declaration; use ${DECL_NAME}:"
      printf '%s\n' "$bad" | sed 's/^/         /' >&2
    fi
  done <<EOF
$go_files
EOF

  if [ ! -d "$WF_DIR" ]; then
    err "$WF_DIR does not exist; the workflow that pre-pulls the image cannot be checked."
  else
    wf_files="$(find "$WF_DIR" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)"
    if [ -z "$wf_files" ]; then
      err "no workflow files under $WF_DIR; the workflow that pre-pulls the image cannot be checked."
    else
      while IFS= read -r f; do
        [ -n "$f" ] || continue
        bad="$(grep -n -E "$S3_IMAGE_REPOS" "$f" | grep -v -E '^[0-9]+:[[:space:]]*#' || true)"
        if [ -n "$bad" ]; then
          err "$f names an S3 server image on a non-comment line; derive it with $SELF_NAME:"
          printf '%s\n' "$bad" | sed 's/^/         /' >&2
        fi
      done <<EOF
$wf_files
EOF
    fi
  fi

  if [ ! -f "$WF_INTEGRATION" ]; then
    err "$WF_INTEGRATION does not exist; nothing pre-pulls the image from the declaration."
  else
    calls="$(grep -n -E "$CALL_RE" "$WF_INTEGRATION" | grep -v -E '^[0-9]+:[[:space:]]*#' || true)"
    if [ -z "$calls" ]; then
      err "$WF_INTEGRATION has no non-comment line that captures the output of $SELF_NAME with \$(...), so its pre-pull is not derived from ${DECL_NAME}."
    fi
  fi

  if [ "$errors" -gt 0 ]; then
    exit 1
  fi
fi

printf '%s\n' "$image"
exit 0
