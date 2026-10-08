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
# cases, a case has empty content, any case is changed by kses, or zero cores
# were checked.
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
trap 'rm -rf "$tmp"' EXIT
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

# fetch_core <version> <sha256> <urls...>
# Leaves a verified php subset of the core at $cache/<sha256>/wordpress/wp-includes.
# Nothing is extracted from a download until its sha256 equals the pin.
fetch_core() {
  local version="$1" sha="$2" urls="$3"
  local dest="$cache/$sha"
  if [ -f "$dest/.verified" ] && [ "$(cat "$dest/.verified")" = "$sha" ] && [ -f "$dest/wordpress/wp-includes/kses.php" ]; then
    return 0
  fi
  local curl_bin tar_bin
  curl_bin="$(command -v curl || true)"
  [ -n "$curl_bin" ] || die "curl not found (needed to fetch WordPress $version)"
  tar_bin="$(command -v tar || true)"
  [ -n "$tar_bin" ] || die "tar not found (needed to unpack WordPress $version)"

  local work
  work="$(mktemp -d "$cache/.tmp.XXXXXX")" || die "cannot create a work dir under $cache"
  case "$work" in "$cache"/.tmp.*) ;; *) die "unexpected work dir: $work" ;; esac

  local url got reasons="" ok=0
  for url in $urls; do
    rm -f "$work/core.tar.gz"
    if ! "$curl_bin" -fsSL --retry 2 --retry-delay 2 --connect-timeout 20 --max-time 300 -o "$work/core.tar.gz" "$url"; then
      reasons="$reasons
  download failed: $url"
      continue
    fi
    got="$(sha256_of "$work/core.tar.gz")"
    if [ "$got" = "$sha" ]; then
      ok=1
      break
    fi
    reasons="$reasons
  sha256 mismatch for $url: got $got, pinned $sha"
  done
  if [ "$ok" -ne 1 ]; then
    rm -rf "$work"
    die "could not fetch a WordPress $version that matches its pin:$reasons"
  fi

  "$tar_bin" -tzf "$work/core.tar.gz" >"$work/all.list" || { rm -rf "$work"; die "WordPress $version tarball cannot be listed"; }
  grep -E '^wordpress/wp-includes/([^/]+\.php|html-api/[^/]+\.php)$' "$work/all.list" >"$work/php.list" || true
  if [ ! -s "$work/php.list" ]; then
    rm -rf "$work"
    die "WordPress $version tarball has no wp-includes php files"
  fi
  mkdir "$work/core"
  "$tar_bin" -xzf "$work/core.tar.gz" -C "$work/core" -T "$work/php.list" || { rm -rf "$work"; die "WordPress $version tarball cannot be unpacked"; }
  printf '%s' "$sha" >"$work/core/.verified"

  # Publish. A concurrent run may have got there first; either copy is verified.
  if [ -e "$dest" ]; then
    rm -rf "$dest"
  fi
  mv "$work/core" "$dest" || { rm -rf "$work"; die "cannot publish the verified core to $dest"; }
  rm -rf "$work"
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
