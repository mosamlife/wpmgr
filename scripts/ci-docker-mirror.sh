#!/usr/bin/env bash
# scripts/ci-docker-mirror.sh
#
# Send a CI runner's Docker pulls through Google's public Docker Hub mirror, so
# they stop counting against Docker Hub's unauthenticated pull limit. No secret
# is involved.
#
#   scripts/ci-docker-mirror.sh             configure the daemon, then prove it took
#   scripts/ci-docker-mirror.sh --report    say which registries the daemon tried
#
# WHY. Docker Hub meters anonymous pulls per source address, and the hosted
# runners share addresses with everyone else's jobs. When the quota runs out
# the Go tests' testcontainers postgres, the schema.sql sync guard, the RLS
# cross-tenant guard and the nginx smoke test all die with
#   toomanyrequests: You have reached your unauthenticated pull rate limit
# which is an infrastructure failure that reads exactly like a red test.
#
# WHY THE DAEMON AND NOT A RENAMED IMAGE. Four kinds of client pull in these
# jobs: testcontainers-go (through the daemon API, including its Ryuk reaper
# image), `docker run` in two guard scripts, `docker build` in the nginx smoke
# test (its FROM lines are Docker Hub images too) and `make agent-zip`
# (composer:2). A registry mirror in daemon.json reaches every one of them and
# changes no image reference, so every digest pin stays exactly as written.
#
# Renaming references instead means teaching each client separately.
# testcontainers-go's TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX reaches only
# testcontainers, and only a reference that names no registry host (the
# prefix is joined onto it; an image written docker.io/... is left alone). It
# does not reach the build in the nginx smoke test.
#
# WHAT IT DOES. Logs out of Docker Hub (see forget_hub_credentials: the
# daemon hands the runner's Docker Hub credentials to the mirror, which rejects
# them, so a logged-in runner never uses the mirror at all). Then merges
#   {"registry-mirrors": [<mirror>, ...whatever was there], "debug": true}
# into the daemon's JSON config, keeping every other key, restarts the daemon,
# waits for it with a bounded number of polls, and then checks that the RUNNING
# daemon reports the mirror in `docker info`. `debug` is what makes the daemon
# log which endpoint each pull tries; --report reads that back, so a run's own
# log says where its images came from instead of leaving it to inference.
#
# CI ONLY. It edits the daemon config, restarts the daemon and logs out of
# Docker Hub, so configure mode refuses to run unless GITHUB_ACTIONS=true (or
# WPMGR_DOCKER_MIRROR_ALLOW_LOCAL=1 for a throwaway machine).
#
# EXIT CODES
#   0  the mirror is active (or the report was printed)
#   2  it could not be made active, and that is never a pass: a binary is
#      missing, the config is unreadable or not a JSON object, the restart
#      failed, the daemon did not come back, or it came back without the
#      mirror. A job that was meant to pull through the mirror and cannot goes
#      red at this step, with the reason, instead of running unmirrored and
#      failing later on a pull limit that reads like a test failure.
#   --report never fails the job: it describes, it does not gate.
#
# ENVIRONMENT (the test suite uses these; CI sets none of them)
#   WPMGR_DOCKER_MIRROR            mirror URL, default https://mirror.gcr.io
#   WPMGR_DOCKER_DAEMON_JSON       config path, default /etc/docker/daemon.json
#   WPMGR_DOCKER_MIRROR_POLLS      daemon polls after the restart, default 60
#   WPMGR_DOCKER_MIRROR_POLL_SLEEP seconds between polls, default 1
#
# Needs docker, jq, and (unless root) sudo and systemctl. All are on the hosted
# ubuntu runners. Each is resolved with `command -v` and a miss is exit 2.
#
# PORTABILITY. bash 3.2 and POSIX tools, same as the other guards.

set -euo pipefail

ME="$(basename "$0")"
MIRROR="${WPMGR_DOCKER_MIRROR:-https://mirror.gcr.io}"
DAEMON_JSON="${WPMGR_DOCKER_DAEMON_JSON:-/etc/docker/daemon.json}"
POLLS="${WPMGR_DOCKER_MIRROR_POLLS:-60}"
POLL_SLEEP="${WPMGR_DOCKER_MIRROR_POLL_SLEEP:-1}"

usage() {
  printf 'usage: %s [--report]\n' "$ME"
}

# broken MESSAGE -- exit 2 with a reason on stderr and a workflow annotation on
# stdout, so the reason shows on the run's summary page and not only in the log.
broken() {
  printf '%s: %s\n' "$ME" "$*" >&2
  printf '::error title=Docker registry mirror::%s\n' "$*"
  exit 2
}

need() {
  command -v "$1" >/dev/null 2>&1 || broken "$1 not found on PATH, so the Docker mirror cannot be configured"
}

MODE=configure
if [ "$#" -gt 1 ]; then usage >&2; exit 2; fi
case "${1:-}" in
  '') ;;
  --report) MODE=report ;;
  -h | --help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

SUDO=''
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null 2>&1 || broken "not root and sudo not found on PATH, so ${DAEMON_JSON} cannot be written"
  SUDO=sudo
fi

MIRROR="${MIRROR%/}"

# The mirrors the RUNNING daemon reports, one per line, trailing slash removed
# (the daemon prints https://mirror.gcr.io/). Non-zero when the daemon does not
# answer. Docker prints null when no mirror is configured.
reported_mirrors() {
  docker info --format '{{json .RegistryConfig.Mirrors}}' 2>/dev/null | jq -r '(. // [])[] | sub("/+$"; "")'
}

daemon_reports_mirror() {
  local out
  out="$(reported_mirrors)" || return 1
  printf '%s\n' "$out" | grep -Fxq "$MIRROR"
}

# stdin: the current config. stdout: the same config with the mirror first and
# debug on, keys sorted so two runs compare byte for byte. Fails (and so ends
# the run) on anything that is not a JSON object, and on a registry-mirrors
# that is not an array, rather than guess what the owner of the file meant.
want_json() {
  jq -S --arg m "$MIRROR" '
    (.["registry-mirrors"] // []) as $cur
    | if ($cur | type) != "array" then error("registry-mirrors is not an array") else . end
    | .["registry-mirrors"] = ([$m] + ($cur | map(select(. != $m))))
    | .debug = true
  '
}

# The daemon forwards the Docker Hub credentials the CLI holds to every endpoint
# it tries for an image on docker.io, mirrors included. The hosted runners carry
# some (the token requests name account=githubactions), the mirror's token
# endpoint answers them with "unauthorized: authentication failed", the daemon
# gives the mirror up and goes to Docker Hub, and the mirror is never used.
# Logging out leaves the pulls anonymous, which is all the mirror needs. A
# failure here is a warning and not an exit: the mirror is then probably
# useless, but the job can still pull from Docker Hub as it did before.
forget_hub_credentials() {
  local out
  if out="$(docker logout 2>&1)"; then
    echo "docker logout: ${out}"
  else
    echo "::warning title=Docker registry mirror::docker logout failed, so the mirror may reject the runner's Docker Hub credentials: ${out}"
  fi
}

configure() {
  need docker
  need jq
  need systemctl

  # This rewrites the daemon config, restarts the daemon and logs out of Docker
  # Hub. On a developer machine that is somebody's session, so it is refused
  # unless this is a GitHub Actions runner or the caller says it is throwaway.
  if [ "${GITHUB_ACTIONS:-}" != true ] && [ "${WPMGR_DOCKER_MIRROR_ALLOW_LOCAL:-}" != 1 ]; then
    broken "refusing to rewrite the Docker daemon config and log out of Docker Hub outside GitHub Actions; set WPMGR_DOCKER_MIRROR_ALLOW_LOCAL=1 on a throwaway machine"
  fi

  local current wanted normalized i

  forget_hub_credentials

  current='{}'
  if [ -e "$DAEMON_JSON" ]; then
    current="$($SUDO cat "$DAEMON_JSON")" || broken "cannot read ${DAEMON_JSON}"
    if [ -z "$(printf '%s' "$current" | tr -d '[:space:]')" ]; then current='{}'; fi
  fi

  wanted="$(printf '%s' "$current" | want_json)" \
    || broken "${DAEMON_JSON} is not a JSON object with an array registry-mirrors; left untouched"
  normalized="$(printf '%s' "$current" | jq -S .)" \
    || broken "${DAEMON_JSON} is not valid JSON; left untouched"

  if [ "$wanted" = "$normalized" ] && daemon_reports_mirror; then
    echo "docker registry mirror already active: ${MIRROR}"
    return 0
  fi

  $SUDO mkdir -p "$(dirname "$DAEMON_JSON")" || broken "cannot create the directory of ${DAEMON_JSON}"
  printf '%s\n' "$wanted" | $SUDO tee "$DAEMON_JSON" >/dev/null || broken "cannot write ${DAEMON_JSON}"
  echo "wrote ${DAEMON_JSON}:"
  printf '%s\n' "$wanted"

  $SUDO systemctl restart docker || broken "systemctl restart docker failed"

  i=0
  while ! docker info >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge "$POLLS" ]; then
      broken "the Docker daemon did not answer 'docker info' within ${POLLS} polls of ${POLL_SLEEP}s after the restart"
    fi
    sleep "$POLL_SLEEP"
  done

  daemon_reports_mirror \
    || broken "the daemon restarted but 'docker info' does not list ${MIRROR}, so pulls would still go to Docker Hub"
  echo "docker registry mirror active: ${MIRROR}"
}

# What the daemon's own log says about which registry each pull was resolved
# against. At debug level the daemon logs one line per image reference it
# resolves, in one of two shapes depending on the image store it runs:
#   containerd store   msg=resolving host=<registry> ... url="https://<registry>/v2/<repo>/manifests/<ref>..."
#   classic store      msg="Trying to pull <image> from https://<registry>/"
# BuildKit, which runs inside the daemon for `docker build`, logs the first
# shape whatever the image store. This counts both shapes and lists the
# distinct lines, so a run's own log shows whether its images came from the
# mirror or from Docker Hub. Describes; it never fails the job.
report() {
  local log host re_host pat_mirror pat_hub mirror_n hub_n
  if ! command -v journalctl >/dev/null 2>&1; then
    echo "docker mirror report: journalctl not found, nothing to report"
    return 0
  fi
  if ! log="$($SUDO journalctl -u docker --no-pager -o cat 2>/dev/null)"; then
    echo "docker mirror report: could not read the docker journal, nothing to report"
    return 0
  fi

  host="${MIRROR#*://}"
  re_host="$(printf '%s' "$host" | sed 's/\./\\./g')"
  # The classic-store message ends at the closing quote of msg="...", so the
  # boundary after the host has to allow a quote as well as a slash, colon or
  # space; without it a Docker Hub line (which has no trailing slash) is missed.
  pat_mirror="msg=resolving host=${re_host}( |\$)|Trying to pull .* from https?://${re_host}([/: \"]|\$)"
  pat_hub='msg=resolving host=registry-1\.docker\.io( |$)|Trying to pull .* from https?://registry-1\.docker\.io([/: "]|$)'
  mirror_n="$(printf '%s\n' "$log" | grep -Ec "$pat_mirror" || true)"
  hub_n="$(printf '%s\n' "$log" | grep -Ec "$pat_hub" || true)"

  echo "docker mirror report (from the daemon journal, debug level):"
  echo "  image references resolved against ${host}:  ${mirror_n}"
  echo "  image references resolved against Docker Hub (registry-1.docker.io):  ${hub_n}"
  printf '%s\n' "$log" | grep -E "$pat_mirror|$pat_hub" \
    | sed -E -e 's/^time="[^"]*" //' -e 's/^level=[a-z]+ //' \
             -e 's/^msg=resolving (host=[^ ]+).* url="([^"]*)".*$/resolving \1 \2/' \
    | sort | uniq -c | head -60 || true

  # Why an endpoint was given up on, in the daemon's own words: the lines above
  # debug level that talk about pulls, mirrors or endpoints.
  echo "  daemon lines above debug level about pulls, mirrors or endpoints:"
  printf '%s\n' "$log" | grep -E 'level=(info|warning|error)' | grep -Ei 'pull|mirror|endpoint|registry' \
    | sed -E -e 's/^time="[^"]*" //' | sort | uniq -c | head -30 || true
}

case "$MODE" in
  configure) configure ;;
  report) report ;;
esac
