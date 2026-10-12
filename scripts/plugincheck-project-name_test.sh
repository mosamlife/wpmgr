#!/usr/bin/env bash
# scripts/plugincheck-project-name_test.sh
#
# Regression suite for scripts/plugincheck-project-name.sh and for the way
# tools/plugincheck/run.sh uses it. The plugin check stands up a database and a
# wp-cli container with docker compose and removes them, volumes included, when
# it ends; compose scopes both to a project name. The name used to be the fixed
# name of the harness directory, so two runs from two checkouts of this
# repository (several agents, each in its own worktree) removed each other's
# containers. This suite proves a name is derived per checkout, is always one
# compose accepts, is overridden by COMPOSE_PROJECT_NAME, and that run.sh puts
# the same name on every docker call it makes, its cleanup included.
#
# It proves the name two ways. The helper is run on directories laid out like
# two worktrees. run.sh is then run from two copies of the harness, each in a
# checkout of its own, against a stand-in docker that records the project name
# every call carried: that is the path production takes, and it needs no Docker
# daemon.
#
# It also proves what it must NOT block: an honest checkout, the same checkout
# spelled through a symlink or a dot-dot path, a directory name with spaces,
# capitals or non-ASCII characters, and an override that is a valid name.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd -P)"
root="$(cd "$here/.." && pwd -P)"
helper="$here/plugincheck-project-name.sh"
tmp="$(mktemp -d -t plugincheck-name-test.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT
unset COMPOSE_PROJECT_NAME

pass=0
failed=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { failed=$((failed + 1)); echo "FAIL $1"; }

# eq <name> <want> <got>
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi; }
# differ <name> <a> <b>   both must be non-empty and unequal.
differ() { if [ -n "$2" ] && [ -n "$3" ] && [ "$2" != "$3" ]; then ok "$1"; else bad "$1 (got '$2' and '$3')"; fi; }
# match <name> <extended regex> <got>
match() { if printf '%s' "$3" | grep -E -q -- "$2"; then ok "$1"; else bad "$1 (got '$3', want a match for $2)"; fi; }
# has <name> <text> <haystack>
has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1 (expected to contain: $2; got: $3)" ;; esac; }

# derive <dir>   the name for a checkout at <dir>, with no override: OUT, ERR and RC.
derive() {
  OUT="$(env -u COMPOSE_PROJECT_NAME "$helper" "$1" 2>"$tmp/derive.err")"
  RC=$?
  ERR="$(cat "$tmp/derive.err")"
}

if [ ! -x "$helper" ]; then
  bad "setup: $helper is missing or not executable"
  echo
  echo "plugincheck-project-name_test: $pass passed, $failed failed"
  exit 1
fi

valid='^[a-z0-9][a-z0-9_-]*$'

# ---------------------------------------------------------------------------------
# The derivation
# ---------------------------------------------------------------------------------
mkdir -p "$tmp/wt/wf_one" "$tmp/wt/wf_two" "$tmp/wt/a/wpmgr" "$tmp/wt/b/wpmgr"

derive "$tmp/wt/wf_one"
name_one="$OUT"
eq "a derived name exits 0" 0 "$RC"
match "the default name is derived per checkout, not the fixed name of the harness directory" '^plugincheck-[a-z0-9_-]+-[0-9a-f]{8}$' "$name_one"
derive "$tmp/wt/wf_two"
differ "two worktrees get two names" "$name_one" "$OUT"

derive "$tmp/wt/a/wpmgr"
name_a="$OUT"
derive "$tmp/wt/b/wpmgr"
differ "two checkouts whose directories share a name get two names" "$name_a" "$OUT"

derive "$tmp/wt/wf_one"
eq "the same checkout gets the same name on a second run" "$name_one" "$OUT"
ln -s "$tmp/wt/wf_one" "$tmp/wt/link_to_one"
derive "$tmp/wt/link_to_one"
eq "the same checkout reached through a symlink gets the same name" "$name_one" "$OUT"
derive "$tmp/wt/a/../wf_one"
eq "the same checkout reached through a dot-dot path gets the same name" "$name_one" "$OUT"

# A directory name compose would refuse, or that is longer than a container name should be.
hostile=("My Checkout" "UPPER.case" ".hidden" "-lead" "_lead" "$(printf 'n\303\274\303\257')" "@@@" "$(printf 'x%.0s' $(seq 1 200))")
for d in "${hostile[@]}"; do
  mkdir -p "$tmp/hostile/$d"
  derive "$tmp/hostile/$d"
  label="a checkout directory named '$(printf '%s' "$d" | cut -c1-20)'"
  eq "$label derives a name (exit 0)" 0 "$RC"
  match "$label gives a name compose accepts" "$valid" "$OUT"
  if [ "${#OUT}" -le 48 ]; then ok "$label gives a name of at most 48 characters"; else bad "$label gives a name of ${#OUT} characters"; fi
done

derive "$tmp/does-not-exist"
eq "a directory that does not exist is red (exit 2)" 2 "$RC"
has "  and it says so" "not a directory" "$ERR"

# No hash tool: the helper must say so, not fall back to a name that two checkouts share.
mkdir -p "$tmp/nohash-bin"
for t in tr cut sed cat head; do
  p="$(command -v "$t" || true)"
  if [ -z "$p" ]; then bad "setup: $t not found"; continue; fi
  ln -sf "$p" "$tmp/nohash-bin/$t"
done
nohash_out="$(env -u COMPOSE_PROJECT_NAME PATH="$tmp/nohash-bin" /bin/bash "$helper" "$tmp/wt/wf_one" 2>&1)"
nohash_rc=$?
eq "no sha256 tool is red (exit 2), not a name two checkouts could share" 2 "$nohash_rc"
has "  and it names the tool" "sha256sum" "$nohash_out"

# ---------------------------------------------------------------------------------
# The override
# ---------------------------------------------------------------------------------
over="$(COMPOSE_PROJECT_NAME=pc-w2-31 "$helper" "$tmp/wt/wf_one" 2>&1)"
eq "COMPOSE_PROJECT_NAME overrides the derived name" "pc-w2-31" "$over"
for badname in "Bad Name" "-x" "a.b" "UPPER" "_x"; do
  out="$(COMPOSE_PROJECT_NAME="$badname" "$helper" "$tmp/wt/wf_one" 2>&1)"
  rc=$?
  eq "an override compose would refuse is red (exit 2): '$badname'" 2 "$rc"
  has "  and it names the variable" "COMPOSE_PROJECT_NAME" "$out"
done
empty="$(COMPOSE_PROJECT_NAME= "$helper" "$tmp/wt/wf_one" 2>&1)"
eq "an empty COMPOSE_PROJECT_NAME is no override" "$name_one" "$empty"

# ---------------------------------------------------------------------------------
# run.sh, from two checkouts, against a stand-in docker
# ---------------------------------------------------------------------------------
zip_bin="$(command -v zip || true)"
if [ -z "$zip_bin" ]; then
  bad "setup: zip not found (the stand-in plugin zip is made with it)"
else
  mkdir -p "$tmp/zipsrc/fake-plugin"
  printf '<?php\n' >"$tmp/zipsrc/fake-plugin/fake-plugin.php"
  (cd "$tmp/zipsrc" && "$zip_bin" -q -r "$tmp/fake-plugin.zip" fake-plugin)
fi

# The stand-in records the project name each call carried, answers the health poll, and succeeds.
mkdir -p "$tmp/stub"
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'printf "project=%s args=%s\n" "${COMPOSE_PROJECT_NAME-<unset>}" "$*" >>"$DOCKER_LOG"'
  printf '%s\n' 'case "$*" in *"ps db --format"*) echo healthy ;; esac'
  printf '%s\n' 'exit 0'
} >"$tmp/stub/docker"
chmod +x "$tmp/stub/docker"

# make_checkout <dir>   the files run.sh needs, laid out as in the repository.
make_checkout() {
  mkdir -p "$1/tools/plugincheck" "$1/scripts"
  cp "$root/tools/plugincheck/run.sh" "$root/tools/plugincheck/docker-compose.yml" "$1/tools/plugincheck/"
  cp "$helper" "$1/scripts/"
}

# run_check <checkout> <log> <out> [env ...]   run.sh from that checkout; status in RUN_RC.
run_check() {
  local co="$1" log="$2" out="$3"
  shift 3
  : >"$log"
  (cd "$co/tools/plugincheck" && env -u COMPOSE_PROJECT_NAME "$@" PATH="$tmp/stub:$PATH" PLUGIN_ZIP="$tmp/fake-plugin.zip" DOCKER_LOG="$log" ./run.sh >"$out" 2>&1)
  RUN_RC=$?
}

if [ -f "$tmp/fake-plugin.zip" ]; then
  make_checkout "$tmp/co/wf_one"
  make_checkout "$tmp/co/wf_two"

  run_check "$tmp/co/wf_one" "$tmp/one.log" "$tmp/one.out"
  eq "run.sh from the first checkout exits 0" 0 "$RUN_RC"
  run_check "$tmp/co/wf_two" "$tmp/two.log" "$tmp/two.out"
  eq "run.sh from the second checkout exits 0" 0 "$RUN_RC"

  calls_one="$(grep -c '^project=' "$tmp/one.log" || true)"
  if [ "${calls_one:-0}" -gt 0 ]; then ok "the first run reached docker ($calls_one calls)"; else bad "the first run never reached docker, so it proves nothing"; fi
  projects_one="$(sed -n 's/^project=\([^ ]*\) .*/\1/p' "$tmp/one.log" | sort -u)"
  projects_two="$(sed -n 's/^project=\([^ ]*\) .*/\1/p' "$tmp/two.log" | sort -u)"
  derive "$tmp/co/wf_one"
  want_one="$OUT"
  derive "$tmp/co/wf_two"
  want_two="$OUT"
  eq "every docker call of the first checkout carried the name derived for it" "$want_one" "$projects_one"
  eq "every docker call of the second checkout carried the name derived for it" "$want_two" "$projects_two"
  differ "the two checkouts ran under two different project names" "$projects_one" "$projects_two"
  has "run.sh says which project it is using" "compose project $want_one" "$(cat "$tmp/one.out")"
  last_one="$(tail -n 1 "$tmp/one.log")"
  has "the cleanup tears down the first checkout's own project" "project=$want_one args=compose down -v" "$last_one"
  last_two="$(tail -n 1 "$tmp/two.log")"
  has "the cleanup tears down the second checkout's own project" "project=$want_two args=compose down -v" "$last_two"

  run_check "$tmp/co/wf_one" "$tmp/ov.log" "$tmp/ov.out" COMPOSE_PROJECT_NAME=pc-w2-31
  eq "run.sh with an override exits 0" 0 "$RUN_RC"
  eq "COMPOSE_PROJECT_NAME overrides the name on every docker call run.sh makes" "pc-w2-31" "$(sed -n 's/^project=\([^ ]*\) .*/\1/p' "$tmp/ov.log" | sort -u)"

  run_check "$tmp/co/wf_one" "$tmp/badov.log" "$tmp/badov.out" "COMPOSE_PROJECT_NAME=Bad Name"
  eq "run.sh with an override compose would refuse is red (exit 2)" 2 "$RUN_RC"
  eq "  and it stopped before docker was touched" 0 "$(grep -c '^project=' "$tmp/badov.log" || true)"
fi

echo
echo "plugincheck-project-name_test: $pass passed, $failed failed"
if [ "$failed" -ne 0 ]; then
  exit 1
fi
exit 0
