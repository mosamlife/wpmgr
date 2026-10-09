#!/usr/bin/env bash
# scripts/check-page-kses.sh
#
# Proves the agent's generated page markup (PageCreateBuilder, block editor and
# classic editor output) survives WordPress's own sanitiser byte for byte.
#
# The user the agent creates a page as has no unfiltered_html, so core runs the
# content through kses when the page is saved, and a byte it does not keep is
# rewritten. The agent refuses to write a page when that would happen. This
# check finds the same thing in CI: it runs every generated case through the
# real save filters and wp_kses_post of each pinned WordPress core listed in
# scripts/page-blocks/kses-cores.txt (a sha256-pinned tarball per version).
#
# Fails, never skips, when: php/curl/tar/a sha256 tool cannot be found, the
# generator fails, the cores file is missing, empty or malformed, a requested
# version is not in it, a download fails or does not match its sha256, a core is
# not the version its pin claims, a markup file is missing, empty or has zero
# cases, a case has empty content, any case is changed by kses (a case listed in
# the known-changes file only when it is rewritten exactly as pinned there), a
# cached core cannot be replaced because the lock on it cannot be taken in
# PAGE_KSES_LOCK_WAIT seconds, or zero cores were checked.
#
# Test seams (used by check-page-kses_test.sh only):
#   PAGE_KSES_MARKUP         space separated markup JSON files, used instead of
#                            running scripts/page-blocks/generate.php
#   PAGE_KSES_PHP            php binary to run
#   PAGE_KSES_CORES_FILE     the version/sha256/url table to read
#   PAGE_KSES_KNOWN_FILE     the known-changes list (default:
#                            scripts/page-blocks/kses-known-changes.txt); "-"
#                            for none. Set to nothing, or a missing file, it is red.
#   PAGE_KSES_VERSIONS       space separated versions from the table (default:
#                            every version in it). Set to nothing, it is red.
#   PAGE_KSES_CACHE          where verified cores are kept between runs
#                            (default: ${XDG_CACHE_HOME:-~/.cache}/wpmgr-page-kses)
#   PAGE_KSES_LOCK_WAIT      how many one second polls a run waits for the lock
#                            on a cached core it has to replace (default 60)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
dir="$here/page-blocks"
cores_file="${PAGE_KSES_CORES_FILE:-$dir/kses-cores.txt}"

die() {
  echo "check-page-kses: $*" >&2
  exit 1
}

# --- tools: resolved here, a missing one is red ---------------------------------
PHP="${PAGE_KSES_PHP:-$(command -v php || true)}"
if [ -z "$PHP" ] || [ ! -x "$PHP" ]; then
  die "php not found"
fi

# --- cache dir: absolute, and never somewhere a cleanup could hurt --------------
cache="${PAGE_KSES_CACHE:-${XDG_CACHE_HOME:-${HOME:-}/.cache}/wpmgr-page-kses}"
case "$cache" in
  /*) ;;
  *) die "cache dir must be an absolute path: '$cache'" ;;
esac
case "$cache" in
  / | /tmp | /var | /usr | /etc | "${HOME:-/nonexistent}") die "refusing to use '$cache' as the cache dir" ;;
esac
mkdir -p "$cache" || die "cannot create the cache dir $cache"

lock_wait="${PAGE_KSES_LOCK_WAIT-60}"
case "$lock_wait" in
  '' | *[!0-9]* | 0) die "PAGE_KSES_LOCK_WAIT must be a whole number of seconds, 1 or more: '$lock_wait'" ;;
esac

# --- the pin table ---------------------------------------------------------------
[ -f "$cores_file" ] || die "cores file missing: $cores_file"
table_v=()
table_sha=()
table_urls=()
# Kept in variables: that is the form of [[ =~ ]] that behaves the same on the
# bash 3.2 macOS ships and on the bash 5 CI runs.
version_re='^[0-9]+(\.[0-9]+){1,2}$'
sha_re='^[0-9a-f]{64}$'
url_re='^https://[A-Za-z0-9._~:/?#@!$&()*+,;=%-]+$'
while IFS= read -r line || [ -n "$line" ]; do
  trimmed="${line#"${line%%[![:space:]]*}"}"
  case "$trimmed" in '' | '#'*) continue ;; esac
  read -r -a f <<<"$trimmed"
  if [ "${#f[@]}" -lt 3 ]; then
    die "cores file line needs <version> <sha256> <url>: '$trimmed'"
  fi
  if ! [[ ${f[0]} =~ $version_re ]]; then
    die "cores file: bad version '${f[0]}'"
  fi
  if ! [[ ${f[1]} =~ $sha_re ]]; then
    die "cores file: version ${f[0]} has a sha256 that is not 64 lowercase hex characters"
  fi
  urls=""
  for u in "${f[@]:2}"; do
    if ! [[ $u =~ $url_re ]]; then
      die "cores file: version ${f[0]} has a url that is not plain https: '$u'"
    fi
    urls="$urls $u"
  done
  if [ "${#table_v[@]}" -gt 0 ]; then
    for seen in "${table_v[@]}"; do
      [ "$seen" != "${f[0]}" ] || die "cores file lists version ${f[0]} twice"
    done
  fi
  table_v+=("${f[0]}")
  table_sha+=("${f[1]}")
  table_urls+=("${urls# }")
done <"$cores_file"
if [ "${#table_v[@]}" -eq 0 ]; then
  die "cores file has no cores: $cores_file"
fi

# --- the known-changes list ---------------------------------------------------------
known_file="${PAGE_KSES_KNOWN_FILE-$dir/kses-known-changes.txt}"
if [ "$known_file" != "-" ] && [ ! -f "$known_file" ]; then
  die "known-changes file missing: '$known_file'"
fi

# --- which versions to run --------------------------------------------------------
selected=()
if [ -n "${PAGE_KSES_VERSIONS+x}" ]; then
  read -r -a want <<<"$PAGE_KSES_VERSIONS" || true
  if [ "${#want[@]}" -eq 0 ]; then
    die "PAGE_KSES_VERSIONS is set but names no versions"
  fi
  for w in "${want[@]}"; do
    found=0
    for t in "${table_v[@]}"; do
      [ "$t" != "$w" ] || found=1
    done
    [ "$found" -eq 1 ] || die "version $w is not in $cores_file"
    selected+=("$w")
  done
else
  selected=("${table_v[@]}")
fi

# --- the markup -----------------------------------------------------------------
tmp="$(mktemp -d -t page-kses.XXXXXX)"
work_dir="" # the private dir a core is built in, while one is
lock_held="" # the lock dir this run holds, while it holds one
# Every way out of the script, a die and a signal included, ends here: the lock
# is given back (only if it is still this run's) and nothing private is left.
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  if [ -n "$lock_held" ] && [ "$(cat "$lock_held/pid" 2>/dev/null || true)" = "$$" ]; then
    rm -rf "$lock_held"
  fi
  if [ -n "$work_dir" ]; then
    rm -rf "$work_dir"
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
markup_files=()
if [ -n "${PAGE_KSES_MARKUP+x}" ]; then
  read -r -a markup_files <<<"$PAGE_KSES_MARKUP" || true
  if [ "${#markup_files[@]}" -eq 0 ]; then
    die "PAGE_KSES_MARKUP is set but names no files"
  fi
else
  "$PHP" "$dir/generate.php" "$tmp/blocks.json" "$tmp/classic.json" || die "the generator failed"
  markup_files=("$tmp/blocks.json" "$tmp/classic.json")
fi
for m in "${markup_files[@]}"; do
  if [ ! -s "$m" ]; then
    die "markup file missing or empty: $m"
  fi
done

# --- sha256 of a file --------------------------------------------------------------
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "no sha256 tool found (sha256sum or shasum)"
  fi
}

# --- the cache of verified cores -----------------------------------------------------
#
# $cache/<sha256> holds one core, and several runs on one machine share it. What
# keeps them from hurting each other:
#   - a core is built in a private dir ($work_dir) and enters the cache by one
#     rename of a finished tree that already carries its .verified marker, so a
#     reader sees the whole core or none of it;
#   - that rename names the cache dir as its target and the tree is already
#     called <sha256>, so it can neither put one core inside another nor replace
#     a core that is there: when the name is taken, the rename fails;
#   - a core that passes core_verified is never moved, removed or rewritten by
#     any run, so a run reading it is never disturbed;
#   - only a core that FAILS core_verified is replaced, by one run at a time
#     under a per-core lock, and only after that run has looked at it again
#     under the lock.
# A run that loses the race keeps the winner's copy and drops its own.

# core_verified <dir> <sha256>
# True for a core this script finished and marked: the marker names the pin and
# the file every later step needs is there. Anything else is not trusted.
core_verified() {
  [ -f "$1/.verified" ] && [ "$(cat "$1/.verified" 2>/dev/null || true)" = "$2" ] && [ -f "$1/wordpress/wp-includes/kses.php" ]
}

# pid_alive <pid>   True when a process with that pid exists on this machine.
# kill -0 fails for a live process another user owns, so ps has the last word.
pid_alive() {
  kill -0 "$1" 2>/dev/null && return 0
  ps -p "$1" >/dev/null 2>&1
}

# reclaim_lock <lock dir> <reclaim dir> <pid>
# Removes the lock when its owner is still the gone process the caller saw.
# Waiters that saw the same dead owner take turns on a second lock and judge the
# owner again inside it. Without that, both would remove, and the second removal
# would take the lock the first had since won. A reclaim lock left behind by a
# reclaimer that was killed is not removed here: the wait bound reports it.
# Returns 0 only when the lock is gone afterwards.
reclaim_lock() {
  local ld="$1" rd="$2" seen="$3" now
  mkdir "$rd" 2>/dev/null || return 1
  now="$(cat "$ld/pid" 2>/dev/null || true)"
  if [ "$now" = "$seen" ] && ! pid_alive "$now"; then
    rm -rf "$ld"
    echo "check-page-kses: removed the lock $ld, left by pid $seen, which is gone" >&2
  fi
  rm -rf "$rd"
  [ ! -e "$ld" ]
}

# lock_core <sha256>
# Takes the lock on one cached core, polling once a second at most
# $lock_wait times. mkdir either creates the directory or fails, so only one run
# can hold it. The owner's pid is recorded inside; that is how a lock left by a
# run that was killed is told from one in use. Pids mean something on one
# machine, so the cache must not be shared between machines or containers.
# A lock it cannot take, or cannot prove stale, is reported and the run fails:
# it never waits without a bound, and never treats a lock it cannot judge as free.
lock_core() {
  local ld="$cache/.lock.$1" rd="$cache/.lock.$1.reclaim" polls=0 owner="" alive=""
  case "$ld" in "$cache"/.lock.*) ;; *) die "unexpected lock dir: $ld" ;; esac
  command -v ps >/dev/null 2>&1 || die "ps not found (needed to tell a lock in use from a stale one)"
  while :; do
    if mkdir "$ld" 2>/dev/null; then
      if printf '%s\n' "$$" >"$ld/pid" && [ "$(cat "$ld/pid" 2>/dev/null || true)" = "$$" ]; then
        lock_held="$ld"
        return 0
      fi
      rm -rf "$ld"
      die "cannot record the owner of the lock $ld"
    fi
    owner="$(cat "$ld/pid" 2>/dev/null || true)"
    case "$owner" in *[!0-9]*) owner="" ;; esac
    alive="no owner is recorded in it"
    if [ -n "$owner" ]; then
      if pid_alive "$owner"; then
        alive="held by pid $owner, which is running"
      else
        alive="left by pid $owner, which is gone"
        if reclaim_lock "$ld" "$rd" "$owner"; then
          polls=$((polls + 1))
          [ "$polls" -lt "$lock_wait" ] || die "gave up on the lock $ld after $polls tries"
          continue
        fi
      fi
    fi
    polls=$((polls + 1))
    if [ "$polls" -ge "$lock_wait" ]; then
      die "gave up waiting for the lock $ld after $polls poll(s): $alive. If no run is using $cache, remove it with: rm -rf $ld $rd"
    fi
    sleep 1
  done
}

# unlock_core   Gives the lock back, if it is still this run's.
unlock_core() {
  if [ -n "$lock_held" ] && [ "$(cat "$lock_held/pid" 2>/dev/null || true)" = "$$" ]; then
    rm -rf "$lock_held"
  fi
  lock_held=""
}

# replace_core <sha256>
# $cache/<sha256> is taken and is not a verified core. Replaces it with the
# finished tree in $work_dir, one run at a time. The tree already there goes into
# $work_dir by rename (never deleted in place) and is removed with it.
replace_core() {
  local sha="$1" dest="$cache/$1"
  lock_core "$sha"
  if core_verified "$dest" "$sha"; then
    : # the run that held the lock before this one replaced it; keep that copy
  else
    if [ -e "$dest" ] || [ -L "$dest" ]; then
      mv "$dest" "$work_dir/replaced" || die "cannot move the unverified core $dest out of the way"
    fi
    mv "$work_dir/$sha" "$cache/" || die "cannot publish the verified core to $dest"
  fi
  unlock_core
}

# fetch_core <version> <sha256> <urls...>
# Leaves a verified php subset of the core at $cache/<sha256>/wordpress/wp-includes.
# Nothing is extracted from a download until its sha256 equals the pin.
fetch_core() {
  local version="$1" sha="$2" urls="$3"
  local dest="$cache/$sha"
  if core_verified "$dest" "$sha"; then
    return 0
  fi
  local curl_bin tar_bin
  curl_bin="$(command -v curl || true)"
  [ -n "$curl_bin" ] || die "curl not found (needed to fetch WordPress $version)"
  tar_bin="$(command -v tar || true)"
  [ -n "$tar_bin" ] || die "tar not found (needed to unpack WordPress $version)"

  # Built here, in a dir only this run knows, whatever any other run is doing.
  # It is inside the cache dir so that the rename which publishes it never has
  # to copy across filesystems.
  work_dir="$(mktemp -d "$cache/.tmp.XXXXXX")" || die "cannot create a work dir under $cache"
  case "$work_dir" in "$cache"/.tmp.*) ;; *) work_dir="" && die "unexpected work dir" ;; esac

  local url got reasons="" ok=0
  for url in $urls; do
    rm -f "$work_dir/core.tar.gz"
    if ! "$curl_bin" -fsSL --retry 2 --retry-delay 2 --connect-timeout 20 --max-time 300 -o "$work_dir/core.tar.gz" "$url"; then
      reasons="$reasons
  download failed: $url"
      continue
    fi
    got="$(sha256_of "$work_dir/core.tar.gz")"
    if [ "$got" = "$sha" ]; then
      ok=1
      break
    fi
    reasons="$reasons
  sha256 mismatch for $url: got $got, pinned $sha"
  done
  if [ "$ok" -ne 1 ]; then
    die "could not fetch a WordPress $version that matches its pin:$reasons"
  fi

  "$tar_bin" -tzf "$work_dir/core.tar.gz" >"$work_dir/all.list" || die "WordPress $version tarball cannot be listed"
  grep -E '^wordpress/wp-includes/([^/]+\.php|html-api/[^/]+\.php)$' "$work_dir/all.list" >"$work_dir/php.list" || true
  if [ ! -s "$work_dir/php.list" ]; then
    die "WordPress $version tarball has no wp-includes php files"
  fi
  mkdir "$work_dir/$sha"
  "$tar_bin" -xzf "$work_dir/core.tar.gz" -C "$work_dir/$sha" -T "$work_dir/php.list" || die "WordPress $version tarball cannot be unpacked"
  printf '%s' "$sha" >"$work_dir/$sha/.verified"

  # Publish by one rename into the cache dir. It fails when the name is taken.
  if ! mv "$work_dir/$sha" "$cache/" 2>/dev/null; then
    if core_verified "$dest" "$sha"; then
      : # another run published the same verified core first; its copy stays
    else
      replace_core "$sha"
    fi
  fi
  core_verified "$dest" "$sha" || die "no verified WordPress $version core at $dest after publishing it"
  rm -rf "$work_dir"
  work_dir=""
}

# --- run ----------------------------------------------------------------------------
fail=0
ran=0
for v in "${selected[@]}"; do
  sha=""
  urls=""
  i=0
  while [ "$i" -lt "${#table_v[@]}" ]; do
    if [ "${table_v[$i]}" = "$v" ]; then
      sha="${table_sha[$i]}"
      urls="${table_urls[$i]}"
    fi
    i=$((i + 1))
  done
  [ -n "$sha" ] || die "version $v is not in $cores_file"
  fetch_core "$v" "$sha" "$urls"
  echo "== WP $v"
  if "$PHP" "$dir/kses-check.php" "$cache/$sha" "$v" "$known_file" "${markup_files[@]}"; then
    :
  else
    echo "check-page-kses: FAILED on WP $v" >&2
    fail=1
  fi
  ran=$((ran + 1))
done

if [ "$ran" -eq 0 ]; then
  die "no cores were checked"
fi
if [ "$fail" -eq 0 ]; then
  echo "check-page-kses: $ran core(s) checked, no byte changed"
fi
exit "$fail"
