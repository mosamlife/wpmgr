#!/usr/bin/env bash
#
# WPMgr self-host quickstart — one-shot bootstrap (no repo clone needed).
#
# Fetches every file the prod compose stack needs, generates all secrets,
# and prints the exact command to start WPMgr.
#
# Usage (pipe from GitHub raw):
#   curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh | bash
#
# Or, after cloning:
#   ./scripts/quickstart-selfhost.sh [--hostname=https://wpmgr.example.com]
#
# Options:
#   --hostname=URL   Set WPMGR_PUBLIC_BASE_URL non-interactively.
#   --version=vX.Y.Z Pin a specific GHCR release tag (default: latest).
#   --dir=PATH       Write all files into PATH (default: ./wpmgr).
#   --force          Re-download files even if they already exist.
#   -h / --help      Show this help.
#
# What it does:
#   1. Creates the working directory (default: ./wpmgr).
#   2. Downloads every file the compose stack bind-mounts from the host:
#        infra/docker-compose.yml
#        infra/docker-compose.prod.yml
#        infra/seaweedfs/s3.json          <- SeaweedFS S3 auth
#        infra/dex/config.yaml            <- Dex OIDC provider config
#        infra/postgres/init/01-app-role.sh  <- Postgres role bootstrap
#        .env.example
#        scripts/init-env.sh
#      The observability-profile files (prometheus.yml, grafana/) are also
#      downloaded so the --profile observability option works without a clone.
#   3. Runs scripts/init-env.sh, which:
#        - copies .env.example -> .env
#        - generates cryptographic secrets (session, agent signing, age)
#        - randomises DB passwords and S3 secret, keeping s3.json in sync
#        - optionally sets WPMGR_PUBLIC_BASE_URL
#   4. Pins WPMGR_VERSION in .env if --version was given.
#   5. Prints the exact `docker compose ... up -d` command and verify URLs.
#
# Host requirements:
#   - Docker 24+ with the Compose plugin (docker compose)
#   - curl or wget  (for downloading files)
#   - openssl       (for secret generation fallback)
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults.
# ---------------------------------------------------------------------------
REPO_RAW="https://raw.githubusercontent.com/mosamlife/wpmgr/main"
WPMGR_VERSION=""
WORK_DIR="./wpmgr"
HOSTNAME_OVERRIDE=""
FORCE=0

# ---------------------------------------------------------------------------
# Parse args.
# ---------------------------------------------------------------------------
for arg in "$@"; do
  case "${arg}" in
    --hostname=*) HOSTNAME_OVERRIDE="${arg#--hostname=}" ;;
    --version=*)  WPMGR_VERSION="${arg#--version=}" ;;
    --dir=*)      WORK_DIR="${arg#--dir=}" ;;
    --force)      FORCE=1 ;;
    -h|--help)
      sed -n '3,42p' "${BASH_SOURCE[0]}" | sed 's/^#\{0,1\} \{0,1\}//'
      exit 0
      ;;
    *)
      printf 'quickstart-selfhost: unknown argument "%s" (try --help)\n' "${arg}" >&2
      exit 2
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------
log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# fetch_file <relative-path> <destination>
# Downloads from the repo's raw URL unless the destination already exists (or
# --force was given).
fetch_file() {
  local rel_path="$1" dest="$2"
  if [ -f "${dest}" ] && [ "${FORCE}" -eq 0 ]; then
    info "already present: ${dest}"
    return 0
  fi
  local url="${REPO_RAW}/${rel_path}"
  mkdir -p "$(dirname "${dest}")"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${url}" -o "${dest}" || die "failed to download ${url}"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "${dest}" "${url}" || die "failed to download ${url}"
  else
    die "neither curl nor wget found — install one and re-run."
  fi
  info "downloaded: ${dest}"
}

# set_env_value <key> <value> — portable in-place set in .env.
set_env_value() {
  local key="$1" value="$2" tmp esc
  local env_file="${WORK_DIR}/.env"
  tmp="$(mktemp)"
  esc="$(printf '%s' "${value}" | sed -e 's/[\\&|]/\\&/g')"
  if grep -q "^${key}=" "${env_file}"; then
    sed "s|^${key}=.*|${key}=${esc}|" "${env_file}" >"${tmp}"
  else
    cp "${env_file}" "${tmp}"
    printf '%s=%s\n' "${key}" "${value}" >>"${tmp}"
  fi
  mv "${tmp}" "${env_file}"
}

# ---------------------------------------------------------------------------
# 0. Pre-flight checks.
# ---------------------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  die "docker not found. Install Docker 24+ with the Compose plugin and re-run.\n    See: https://docs.docker.com/engine/install/"
fi
if ! docker compose version >/dev/null 2>&1; then
  die "'docker compose' (Compose plugin v2) not found.\n    Install it: https://docs.docker.com/compose/install/"
fi

# ---------------------------------------------------------------------------
# 1. Create working directory and enter it.
# ---------------------------------------------------------------------------
mkdir -p "${WORK_DIR}"
log "Working directory: $(cd "${WORK_DIR}" && pwd)"
cd "${WORK_DIR}"

# ---------------------------------------------------------------------------
# 2. Download every file the compose stack needs from the host.
#
# COMPLETE manifest — derived by grepping `- ./` bind mounts from both
# docker-compose.yml and docker-compose.prod.yml:
#
#   docker-compose.yml bind mounts (from the infra/ directory context):
#     ./postgres/init/01-app-role.sh          <- postgres service (always)
#     ./seaweedfs/s3.json                     <- seaweedfs service (always)
#     ./dex/config.yaml                       <- dex service (always)
#     ./prometheus/prometheus.yml             <- otel-lgtm (observability profile)
#     ./grafana/provisioning/...              <- otel-lgtm (observability profile)
#     ./grafana/dashboards/...               <- otel-lgtm (observability profile)
#
# All base-stack files (postgres init, seaweedfs, dex) are downloaded
# unconditionally. Observability files are downloaded so the
# --profile observability option works without further setup.
# ---------------------------------------------------------------------------
log "Downloading compose files and config..."

# Compose files (these live at the repo root and one level up from infra/).
fetch_file "infra/docker-compose.yml"      "infra/docker-compose.yml"
fetch_file "infra/docker-compose.prod.yml" "infra/docker-compose.prod.yml"
fetch_file ".env.example"                  ".env.example"
fetch_file "scripts/init-env.sh"           "scripts/init-env.sh"
chmod +x "scripts/init-env.sh"

# Postgres init (bind-mount is a directory; only one script today).
fetch_file "infra/postgres/init/01-app-role.sh" "infra/postgres/init/01-app-role.sh"
chmod +x "infra/postgres/init/01-app-role.sh"

# SeaweedFS S3 auth config (always required — the compose file hard-mounts it).
fetch_file "infra/seaweedfs/s3.json" "infra/seaweedfs/s3.json"

# Dex OIDC config (always required — the compose file hard-mounts it).
fetch_file "infra/dex/config.yaml" "infra/dex/config.yaml"

# Observability profile (opt-in; downloaded so --profile observability works).
fetch_file "infra/prometheus/prometheus.yml"                                  "infra/prometheus/prometheus.yml"
fetch_file "infra/grafana/provisioning/dashboards/dashboards.yml"             "infra/grafana/provisioning/dashboards/dashboards.yml"
fetch_file "infra/grafana/provisioning/datasources/datasources.yml"           "infra/grafana/provisioning/datasources/datasources.yml"
fetch_file "infra/grafana/dashboards/wpmgr-api.json"                          "infra/grafana/dashboards/wpmgr-api.json"

log "All required files present."

# ---------------------------------------------------------------------------
# 3. Bootstrap the .env — copies .env.example -> .env and generates secrets.
#    Pass --hostname= through so init-env.sh can set WPMGR_PUBLIC_BASE_URL
#    non-interactively.
# ---------------------------------------------------------------------------
log "Running init-env.sh to generate secrets and randomize passwords..."
hostname_flag=""
[ -n "${HOSTNAME_OVERRIDE}" ] && hostname_flag="--hostname=${HOSTNAME_OVERRIDE}"

# init-env.sh resolves paths relative to the repo root (the directory that
# contains scripts/). When running from a downloaded copy, that root is the
# current working directory.
bash scripts/init-env.sh ${hostname_flag:+"${hostname_flag}"}

# ---------------------------------------------------------------------------
# 4. Pin WPMGR_VERSION if --version was given.
# ---------------------------------------------------------------------------
if [ -n "${WPMGR_VERSION}" ]; then
  set_env_value "WPMGR_VERSION" "${WPMGR_VERSION}"
  log "Pinned WPMGR_VERSION=${WPMGR_VERSION} in .env"
fi

# ---------------------------------------------------------------------------
# 5. Read back final port values for the summary.
# ---------------------------------------------------------------------------
# Source only the port vars we need; avoid eval of arbitrary .env content.
api_port="$(grep '^WPMGR_API_PORT=' .env | head -n1 | cut -d= -f2-)"
web_port="$(grep '^WPMGR_WEB_PORT=' .env | head -n1 | cut -d= -f2-)"
api_port="${api_port:-8081}"
web_port="${web_port:-8088}"
ver_tag="${WPMGR_VERSION:-latest}"

# ---------------------------------------------------------------------------
# 6. Final instructions.
# ---------------------------------------------------------------------------
cat <<EOF

${WPMGR_HEADER:-}
WPMgr is ready to start. Run:

  cd $(pwd)
  docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d

This includes the media-encoder (screenshots + image optimizer to WebP/AVIF) by
default — no extra flag needed. It runs headless Chromium, so it adds some RAM;
to skip it on a constrained host, add --scale media-encoder=0 to the command
above.

Verify:
  curl localhost:${api_port}/healthz    # {"status":"ok"}
  curl localhost:${api_port}/readyz     # 200 once DB/Redis/S3 are ready

Open the dashboard at:
  http://localhost:${web_port}

IMPORTANT:
  - WPMGR_S3_ENDPOINT in .env must be a URL reachable by your WordPress hosts.
    The default (http://seaweedfs:8333) only resolves inside Docker. For remote
    WordPress sites, set it to a tunnel/public URL.
  - Review docs/install.md for the full self-host guide.

EOF
