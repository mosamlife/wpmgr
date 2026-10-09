#!/usr/bin/env bash
# scripts/test-s3-image_test.sh
#
# The regression suite for scripts/test-s3-image.sh.
#
# HOW IT WORKS. Each case builds a complete little tree (one Go file holding the
# declaration, one workflow that calls the script), mutates exactly one thing,
# runs the script against that tree and asserts the exit code plus what comes
# out of it. No mocking: the real script reads real files.
#
# THREE KINDS OF CASE, because a guard is wrong in two directions:
#
#   * it must FIRE: a tag with no digest, a second declaration, a literal pasted
#     into a workflow, a workflow that stopped calling the script. Each turns the
#     script red, and a red run prints nothing on standard output, so a caller
#     that captures the answer never captures half of one.
#   * it must NOT OVER-FIRE: a commented-out old pin, prose that names the
#     image, a trailing comment on the declaration, a workflow that merely runs
#     the script. A guard that reddens correct work gets switched off.
#   * it must not pass by finding NOTHING: no tests directory, no Go files, no
#     workflow files, no declaration. Those are errors, not "nothing to check".
#
# RUN IT:
#   scripts/test-s3-image_test.sh            # everything
#   scripts/test-s3-image_test.sh digest     # only cases whose name contains "digest"
#
# Point it at a different implementation to prove the suite is not vacuous
# (reintroduce a hole in a copy, watch the suite go red):
#   WPMGR_S3_IMAGE_SCRIPT=/path/to/broken-copy.sh scripts/test-s3-image_test.sh
#
# PORTABILITY. bash 3.2 and POSIX tools, same constraints as the script.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${WPMGR_S3_IMAGE_SCRIPT:-$HERE/test-s3-image.sh}"
FILTER="${1:-}"

if [ ! -f "$GUARD" ]; then
  echo "no script at $GUARD" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/wpmgr-s3-image.XXXXXX")" || exit 2
trap 'rm -rf "$WORK"' EXIT INT TERM

PASSED=0
FAILED=0
SKIPPED=0
FAILED_NAMES=''

DIGEST='1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a'
REF="chrislusf/seaweedfs:3.80@sha256:${DIGEST}"

# ---------------------------------------------------------------------------
# Tree construction
# ---------------------------------------------------------------------------
write_tree() {
  _dir="$1"
  mkdir -p "$_dir/apps/api/tests" "$_dir/.github/workflows"

  # The doc comment and the second const name the image in prose on purpose: the
  # script must read the declaration, not the first line that mentions it.
  cat >"$_dir/apps/api/tests/blobstore_integration_test.go" <<GO
package tests

// s3TestImage is the object store the tests run against: chrislusf/seaweedfs.
// It used to be minio/minio:RELEASE.2024-01-16T16-07-38Z, which no registry
// serves any more.
const s3TestImage = "${REF}"

const s3TestBucket = "wpmgr-backups"

func startBlobstore() {}
GO

  cat >"$_dir/.github/workflows/api-integration.yml" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      # The S3 image is read out of the Go constant. It used to be the literal
      # minio/minio:RELEASE.2024-01-16T16-07-38Z on the line below this one.
      - name: Pre-pull test container images
        run: |
          set -euo pipefail
          s3_image=$(../../scripts/test-s3-image.sh)
          for img in postgres:16-alpine "$s3_image"; do
            docker pull --quiet "$img"
          done
YML
}

tree() {
  _t="$WORK/$1"
  rm -rf "$_t"
  write_tree "$_t"
  printf '%s' "$_t"
}

# sub FILE SED-EXPR: edit a file in place without sed -i (BSD and GNU differ).
sub() {
  sed -E "$2" "$1" >"$1.new" && mv "$1.new" "$1"
}

go_decl() { printf '%s' "$1/apps/api/tests/blobstore_integration_test.go"; }
wf() { printf '%s' "$1/.github/workflows/api-integration.yml"; }

# ---------------------------------------------------------------------------
# Assertions
#
#   case_run NAME WANT MODE DIR [+needle] [-needle] [=exact-stdout]
#
#   WANT  pass | fail | usage      (exit 0 | exit 1 | exit 2)
#   MODE  plain | check | nope     (no flag | --check | --nope, an unknown flag)
#   +x    standard output or standard error must contain x
#   -x    they must not
#   =x    standard output must be exactly x (one line, as the caller captures it)
#
# A fail or usage run must also leave standard output EMPTY, always.
# ---------------------------------------------------------------------------
case_run() {
  _name="$1"
  _want="$2"
  _mode="$3"
  _dir="$4"
  shift 4

  if [ -n "$FILTER" ]; then
    case "$_name" in
      *"$FILTER"*) : ;;
      *)
        SKIPPED=$((SKIPPED + 1))
        return 0
        ;;
    esac
  fi

  _errf="$WORK/stderr.txt"
  case "$_mode" in
    plain) _out="$("$GUARD" "$_dir" 2>"$_errf")" ;;
    *) _out="$("$GUARD" "--$_mode" "$_dir" 2>"$_errf")" ;;
  esac
  _code=$?
  _all="$_out
$(cat "$_errf")"
  _problems=''

  case "$_want" in
    pass) [ "$_code" -eq 0 ] || _problems="$_problems
    expected exit 0, got $_code" ;;
    fail) [ "$_code" -eq 1 ] || _problems="$_problems
    expected exit 1, got $_code" ;;
    usage) [ "$_code" -eq 2 ] || _problems="$_problems
    expected exit 2, got $_code" ;;
  esac

  if [ "$_want" != pass ] && [ -n "$_out" ]; then
    _problems="$_problems
    a failed run printed on standard output: $_out"
  fi

  for _a in "$@"; do
    case "$_a" in
      +*)
        _needle="${_a#+}"
        if ! printf '%s\n' "$_all" | grep -qF -- "$_needle"; then
          _problems="$_problems
    expected the output to contain: $_needle"
        fi
        ;;
      -*)
        _needle="${_a#-}"
        if printf '%s\n' "$_all" | grep -qF -- "$_needle"; then
          _problems="$_problems
    expected the output NOT to contain: $_needle"
        fi
        ;;
      =*)
        _exact="${_a#=}"
        if [ "$_out" != "$_exact" ]; then
          _problems="$_problems
    expected standard output to be exactly: $_exact
    got: $_out"
        fi
        ;;
    esac
  done

  if [ -n "$_problems" ]; then
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES  $_name
"
    printf 'FAIL %s%s\n' "$_name" "$_problems"
    printf '%s\n' "$_all" | sed 's/^/      | /'
  else
    PASSED=$((PASSED + 1))
    printf 'ok   %s\n' "$_name"
  fi
}

# ===========================================================================
# READING THE DECLARATION: what is accepted
# ===========================================================================
t="$(tree valid)"
case_run "declaration: prints the reference and nothing else" pass plain "$t" "=$REF"

t="$(tree trailing-comment)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = \"[^\"]*\")\$|\\1 // pinned by digest|"
case_run "declaration: a trailing // comment on the line is fine" pass plain "$t" "=$REF"

t="$(tree commented-out-old-pin)"
sub "$(go_decl "$t")" 's|^const s3TestImage = |// const s3TestImage = "minio/minio:RELEASE.2024-01-16T16-07-38Z"\
const s3TestImage = |'
case_run "declaration: a commented-out older pin is not a second declaration" pass plain "$t" "=$REF"

t="$(tree registry-port)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"localhost:5000/team/img:1.2@sha256:${DIGEST}\"|"
case_run "declaration: a registry host with a port is a valid reference" pass plain "$t" "=localhost:5000/team/img:1.2@sha256:${DIGEST}"

t="$(tree digest-only)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs@sha256:${DIGEST}\"|"
case_run "declaration: a digest with no tag is a valid reference" pass plain "$t" "=chrislusf/seaweedfs@sha256:${DIGEST}"

t="$(tree declared-in-another-file)"
mkdir -p "$t/apps/api/tests/sub"
mv "$(go_decl "$t")" "$t/apps/api/tests/sub/elsewhere_test.go"
case_run "declaration: found in a subdirectory file (the search recurses)" pass plain "$t" "=$REF"

t="$(tree plain-ignores-workflows)"
printf '      - run: docker pull minio/minio:RELEASE.2024-01-16T16-07-38Z\n' >>"$(wf "$t")"
case_run "declaration: without --check a workflow literal does not stop the read" pass plain "$t" "=$REF"

# ===========================================================================
# READING THE DECLARATION: what is refused, and says so
# ===========================================================================
t="$(tree no-tests-dir)"
rm -rf "$t/apps"
case_run "refuse: no tests directory at all" fail plain "$t" "+does not exist"

t="$(tree no-go-files)"
rm -f "$(go_decl "$t")"
case_run "refuse: a tests directory with no Go files" fail plain "$t" "+no .go files"

t="$(tree no-declaration)"
sub "$(go_decl "$t")" '/^const s3TestImage = /d'
case_run "refuse: no declaration" fail plain "$t" "+no single-line"

t="$(tree grouped-const)"
cat >"$(go_decl "$t")" <<GO
package tests

const (
	s3TestImage = "${REF}"
)
GO
case_run "refuse: a grouped const block is the wrong shape, and the message says what it saw" fail plain "$t" "+no single-line" "+${DIGEST}"

t="$(tree typed-const)"
sub "$(go_decl "$t")" 's|^const s3TestImage = |const s3TestImage string = |'
case_run "refuse: a typed const is the wrong shape" fail plain "$t" "+no single-line"

t="$(tree var-form)"
sub "$(go_decl "$t")" 's|^const s3TestImage = |var s3TestImage = |'
case_run "refuse: a var is the wrong shape" fail plain "$t" "+no single-line"

t="$(tree two-declarations)"
cat >"$t/apps/api/tests/second_test.go" <<GO
package tests

const s3TestImage = "${REF}"
GO
case_run "refuse: two declarations in two files" fail plain "$t" "+2 declarations"

t="$(tree two-declarations-subpackage)"
mkdir -p "$t/apps/api/tests/gh458"
cat >"$t/apps/api/tests/gh458/second_test.go" <<GO
package gh458

const s3TestImage = "${REF}"
GO
case_run "refuse: a second declaration in a subpackage still counts" fail plain "$t" "+2 declarations"

t="$(tree empty-value)"
sub "$(go_decl "$t")" 's|^(const s3TestImage = )"[^"]*"|\1""|'
case_run "refuse: an empty string" fail plain "$t" "+empty string"

t="$(tree tag-only)"
sub "$(go_decl "$t")" 's|^(const s3TestImage = )"[^"]*"|\1"chrislusf/seaweedfs:3.80"|'
case_run "refuse: a tag with no digest" fail plain "$t" "+not pinned by digest"

t="$(tree floating-tag)"
sub "$(go_decl "$t")" 's|^(const s3TestImage = )"[^"]*"|\1"chrislusf/seaweedfs:latest"|'
case_run "refuse: a floating tag" fail plain "$t" "+not pinned by digest"

t="$(tree bare-name)"
sub "$(go_decl "$t")" 's|^(const s3TestImage = )"[^"]*"|\1"chrislusf/seaweedfs"|'
case_run "refuse: a bare repository name" fail plain "$t" "+not pinned by digest"

short="$(printf '%s' "$DIGEST" | cut -c1-63)"
t="$(tree digest-63)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80@sha256:${short}\"|"
case_run "refuse: a digest one character short" fail plain "$t" "+not pinned by digest"

t="$(tree digest-65)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80@sha256:${DIGEST}0\"|"
case_run "refuse: a digest one character long" fail plain "$t" "+not pinned by digest"

t="$(tree digest-nonhex)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80@sha256:z${short}\"|"
case_run "refuse: a digest that is not hex" fail plain "$t" "+not pinned by digest"

upper="$(printf '%s' "$DIGEST" | tr 'a-f' 'A-F')"
t="$(tree digest-upper)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80@sha256:${upper}\"|"
case_run "refuse: an uppercase digest" fail plain "$t" "+not pinned by digest"

t="$(tree digest-sha512)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80@sha512:${DIGEST}\"|"
case_run "refuse: a digest algorithm other than sha256" fail plain "$t" "+not pinned by digest"

t="$(tree uppercase-repo)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"ChrisLusf/SeaweedFS:3.80@sha256:${DIGEST}\"|"
case_run "refuse: an uppercase repository (Docker would refuse the pull)" fail plain "$t" "+not pinned by digest"

t="$(tree space-in-ref)"
sub "$(go_decl "$t")" "s|^(const s3TestImage = )\"[^\"]*\"|\\1\"chrislusf/seaweedfs:3.80 @sha256:${DIGEST}\"|"
case_run "refuse: whitespace inside the reference" fail plain "$t" "+not pinned by digest"

# ===========================================================================
# MISUSE
# ===========================================================================
case_run "misuse: ROOT is not a directory" usage plain "$WORK/does-not-exist" "+is not a directory"

t="$(tree misuse)"
case_run "misuse: an unknown option" usage nope "$t" "+unknown option"

# ===========================================================================
# --check: a second literal, and the workflow actually calls the script
# ===========================================================================
t="$(tree check-valid)"
case_run "check: a clean tree passes and prints the reference" pass check "$t" "=$REF"

t="$(tree check-workflow-minio-literal)"
printf '      - run: docker pull minio/minio:RELEASE.2024-01-16T16-07-38Z\n' >>"$(wf "$t")"
case_run "check: a minio literal on a workflow code line is a second literal" fail check "$t" "+api-integration.yml names an S3 server image"

t="$(tree check-workflow-seaweed-literal)"
printf '      - run: docker pull chrislusf/seaweedfs:3.80\n' >>"$(wf "$t")"
case_run "check: a seaweedfs literal on a workflow code line is a second literal" fail check "$t" "+names an S3 server image"

t="$(tree check-workflow-comment-literal)"
printf '      # was minio/minio:RELEASE.2024-01-16T16-07-38Z\n' >>"$(wf "$t")"
case_run "check: the same literal in a workflow comment is prose, not a finding" pass check "$t" "=$REF"

t="$(tree check-workflow-indented-comment)"
printf '          # chrislusf/seaweedfs:3.80 was here\n' >>"$(wf "$t")"
case_run "check: an indented workflow comment is prose too" pass check "$t" "=$REF"

t="$(tree check-other-go-literal)"
cat >"$t/apps/api/tests/other_test.go" <<'GO'
package tests

func startOther() string { return "minio/minio:RELEASE.2024-01-16T16-07-38Z" }
GO
case_run "check: a quoted image literal in another test file" fail check "$t" "+other_test.go names an S3 server image"

t="$(tree check-other-go-seaweed)"
cat >"$t/apps/api/tests/other_test.go" <<'GO'
package tests

var img = "chrislusf/seaweedfs:3.80"
GO
case_run "check: a second seaweedfs literal in another test file" fail check "$t" "+other_test.go names an S3 server image"

for repo in bitnami/minio bitnamilegacy/minio pgsty/minio seaweedfs/seaweedfs; do
  t="$(tree "check-other-go-$(printf '%s' "$repo" | tr '/' '-')")"
  printf 'package tests\n\nvar img = "%s:x"\n' "$repo" >"$t/apps/api/tests/other_test.go"
  case_run "check: a literal for $repo is a second literal" fail check "$t" "+other_test.go names an S3 server image"
done

t="$(tree check-other-go-comment)"
cat >"$t/apps/api/tests/other_test.go" <<'GO'
package tests

// This used to start minio/minio and now goes through startBlobstore.
func startOther() {}
GO
case_run "check: a Go comment naming the old image is prose, not a finding" pass check "$t" "=$REF"

t="$(tree check-other-go-indented-comment)"
cat >"$t/apps/api/tests/other_test.go" <<'GO'
package tests

func startOther() {
	// previously minio/minio:RELEASE.2024-01-16T16-07-38Z
}
GO
case_run "check: an indented Go comment is prose too" pass check "$t" "=$REF"

# ---- Go comment forms beyond a whole-line //. Comment text is removed before the
# match; string and rune literals are kept. Every case below puts one Go file
# beside the declaration and asserts on that file alone.
# bash 3.2 cannot parse a here-document that holds an apostrophe or a backtick
# inside $( ), so the Go source is fed to a function called at top level, which
# leaves the tree's path in GO_TREE.
go_other() { # go_other TREE-NAME  (the Go source comes on standard input)
  GO_TREE="$(tree "$1")"
  cat >"$GO_TREE/apps/api/tests/other_test.go"
}

# What must NOT be flagged: the image is named only in comment text.
go_other check-go-block-comment-one-line <<'GO'
package tests

/* previously minio/minio */
var n = 1
GO
t="$GO_TREE"
case_run "check: a one-line block comment naming the old image is prose" pass check "$t" "=$REF"

go_other check-go-trailing-comment <<'GO'
package tests

var n = 1 // previously minio/minio
GO
t="$GO_TREE"
case_run "check: a trailing comment after code is prose" pass check "$t" "=$REF"

go_other check-go-block-comment-multiline <<'GO'
package tests

/*
previously minio/minio:RELEASE.2024-01-16T16-07-38Z
and bitnami/minio before that
*/
var n = 1
GO
t="$GO_TREE"
case_run "check: a block comment spread over several lines is prose" pass check "$t" "=$REF"

go_other check-go-block-comment-mid-line <<'GO'
package tests

var n = /* was minio/minio */ 1
GO
t="$GO_TREE"
case_run "check: a block comment opened and closed inside a code line is prose" pass check "$t" "=$REF"

go_other check-go-comment-with-quotes <<'GO'
package tests

var n = 1 // it's "minio/minio" now
var m = 2
GO
t="$GO_TREE"
case_run "check: quotes inside a comment do not start a string" pass check "$t" "=$REF"

go_other check-go-comment-with-backtick <<'GO'
package tests

var n = 1 // a ` tick, and minio/minio
var m = 2
GO
t="$GO_TREE"
case_run "check: a backtick inside a comment does not open a raw string" pass check "$t" "=$REF"

go_other check-go-block-comment-with-url <<'GO'
package tests

/* see "http://x" and minio/minio */
var n = 1
GO
t="$GO_TREE"
case_run "check: a block comment holding // and quotes is prose" pass check "$t" "=$REF"

# What must STILL be flagged: the image is named in code, whatever surrounds it.
go_other check-go-string-then-comment <<'GO'
package tests

var img = "minio/minio:RELEASE.2024-01-16T16-07-38Z" // the old pin
GO
t="$GO_TREE"
case_run "check: a real image string followed by a comment is still a finding" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-code-after-block-comment <<'GO'
package tests

/* old */ var img = "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: code after a closed block comment is still scanned" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-url-in-string <<'GO'
package tests

var u, img = "http://x", "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: // inside a string is not a comment, so a literal after it is found" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-block-open-in-string <<'GO'
package tests

var s = "/* not a comment"
var img = "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: /* inside a string does not open a comment that hides the next line" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-rune-quote <<'GO'
package tests

var r, u, img = '"', "http://x", "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: a quote inside a rune literal does not flip the string state" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-rune-slash <<'GO'
package tests

var a, b, img = '/', '/', "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: two slash runes in a row are not a comment" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-escaped-quote <<'GO'
package tests

var s = "a \" // b minio/minio"
GO
t="$GO_TREE"
case_run "check: an escaped quote does not end the string early" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-raw-string-second-line <<'GO'
package tests

var s = `first line
minio/minio:x`
GO
t="$GO_TREE"
case_run "check: an image named on the second line of a raw string is found" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-raw-string-slashes <<'GO'
package tests

var s = `http://x minio/minio`
GO
t="$GO_TREE"
case_run "check: a raw string holding // is not a comment" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-raw-string-then-code <<'GO'
package tests

var s = `/* open in a raw string`
var img = "minio/minio:x"
GO
t="$GO_TREE"
case_run "check: /* inside a raw string does not open a comment that hides the next line" fail check "$t" "+other_test.go names an S3 server image"

go_other check-go-state-resets-per-file <<'GO'
package tests

/* a block comment that never closes, and names minio/minio
GO
t="$GO_TREE"
cat >"$t/apps/api/tests/zz_second_test.go" <<'GO'
package tests

var img = "minio/minio:x"
GO
case_run "check: an unclosed block comment in one file does not hide the next file" fail check "$t" "+zz_second_test.go names an S3 server image"

t="$(tree check-workflow-stopped-calling)"
cat >"$(wf "$t")" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      - name: Pre-pull test container images
        run: |
          docker pull postgres:16-alpine
YML
case_run "check: a workflow that stopped calling the script" fail check "$t" "+has no non-comment line that captures"

t="$(tree check-workflow-calls-in-comment-only)"
cat >"$(wf "$t")" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      # s3_image=$(../../scripts/test-s3-image.sh) used to be called here
      - name: Pre-pull test container images
        run: docker pull postgres:16-alpine
YML
case_run "check: a call that survives only in a comment is not a call" fail check "$t" "+has no non-comment line that captures"

t="$(tree check-workflow-names-script-in-message)"
cat >"$(wf "$t")" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      - name: Pre-pull test container images
        run: |
          s3_image=chrislusf/example-store
          if [ -z "$s3_image" ]; then
            echo "::error::scripts/test-s3-image.sh printed no image"
          fi
YML
case_run "check: naming the script in an error message is not a call" fail check "$t" "+has no non-comment line that captures"

t="$(tree check-workflow-prints-only)"
cat >"$(wf "$t")" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      - name: Show the S3 image
        run: scripts/test-s3-image.sh
      - name: Pre-pull test container images
        run: docker pull postgres:16-alpine
YML
case_run "check: running the script without capturing its output derives nothing" fail check "$t" "+has no non-comment line that captures"

t="$(tree check-workflow-captures-absolute-path)"
cat >"$(wf "$t")" <<'YML'
name: api-integration
jobs:
  integration:
    steps:
      - name: Pre-pull test container images
        run: |
          s3_image=$("$GITHUB_WORKSPACE/scripts/test-s3-image.sh")
          docker pull "$s3_image"
YML
case_run "check: capturing it by an absolute path is a call too" pass check "$t" "=$REF"

t="$(tree check-no-integration-workflow)"
rm -f "$(wf "$t")"
printf 'name: ci\n' >"$t/.github/workflows/ci.yml"
case_run "check: api-integration.yml missing" fail check "$t" "+does not exist"

t="$(tree check-no-workflows-dir)"
rm -rf "$t/.github"
case_run "check: no workflows directory" fail check "$t" "+does not exist"

t="$(tree check-no-workflow-files)"
rm -f "$(wf "$t")"
case_run "check: a workflows directory with no workflow files" fail check "$t" "+no workflow files"

t="$(tree check-other-workflow-literal)"
printf 'name: ci\njobs:\n  go:\n    steps:\n      - run: docker pull minio/minio:RELEASE.2024-01-16T16-07-38Z\n' >"$t/.github/workflows/ci.yml"
case_run "check: a literal in a different workflow is found too" fail check "$t" "+ci.yml names an S3 server image"

t="$(tree check-other-workflow-runs-script)"
printf 'name: ci\njobs:\n  security:\n    steps:\n      - name: S3 test image guard\n        run: scripts/test-s3-image.sh --check\n' >"$t/.github/workflows/ci.yml"
case_run "check: a workflow that merely runs the guard is not a finding" pass check "$t" "=$REF"

t="$(tree check-bad-declaration-still-fails)"
sub "$(go_decl "$t")" 's|^(const s3TestImage = )"[^"]*"|\1"chrislusf/seaweedfs:3.80"|'
case_run "check: --check does not soften the digest requirement" fail check "$t" "+not pinned by digest"

# ===========================================================================
# Summary
# ===========================================================================
TOTAL=$((PASSED + FAILED))
echo
echo "test-s3-image_test: $PASSED passed, $FAILED failed, $SKIPPED skipped (of $((TOTAL + SKIPPED)) cases)"

if [ "$TOTAL" -eq 0 ]; then
  echo "ERROR: no case ran (filter '$FILTER' matched nothing); a suite that ran nothing proves nothing." >&2
  exit 2
fi

if [ "$FAILED" -gt 0 ]; then
  echo "failed:"
  printf '%s' "$FAILED_NAMES"
  exit 1
fi
exit 0
