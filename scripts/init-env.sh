#!/usr/bin/env bash
#
# WPMgr one-command self-host bootstrap (issue #4).
#
# What it does:
#   1. Copies .env.example -> .env (only if .env is absent, unless --force).
#   2. Generates the four boot-critical cryptographic secrets and injects them
#      into .env, but only into keys that are still empty (existing secrets are
#      preserved).
#   3. Randomizes the DB owner password, DB app password, and S3 secret key
#      — and writes the SAME S3 secret into infra/seaweedfs/s3.json so the
#      SeaweedFS auth and the .env value always match.
#   4. Prompts for the public hostname (WPMGR_PUBLIC_BASE_URL) when it is still
#      the placeholder; accepts a --hostname=<url> flag to skip the prompt.
#   5. Prints the next steps with the CORRECT host ports.
#
# The four cryptographic secrets are minted by `wpmgr-cli gen-secrets`, which
# produces them in the EXACT formats the control plane validates at boot (and
# self-tests each one through the server's own decode path). The generator is
# run via, in order of preference: a local Go toolchain, then Docker Compose,
# then host openssl + age-keygen as a last resort.
#
# Idempotent and safe to re-run: an existing .env is never overwritten without
# --force, and only EMPTY secret keys are filled.
#
# Usage:
#   scripts/init-env.sh                              # bootstrap (preserves .env)
#   scripts/init-env.sh --force                      # overwrite from .env.example
#   scripts/init-env.sh --hostname=https://wpmgr.example.com
#
set -euo pipefail

# Resolve the repo root (this script lives in scripts/).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

ENV_FILE="${ROOT_DIR}/.env"
ENV_EXAMPLE="${ROOT_DIR}/.env.example"
S3_JSON="${ROOT_DIR}/infra/seaweedfs/s3.json"
GEN_FILE=""
FORCE=0
HOSTNAME_OVERRIDE=""

# The secret keys this script manages (filled when empty OR still the committed
# DEV placeholder — see dev_sentinel below).
SECRET_KEYS=(
  WPMGR_SESSION_SECRET
  WPMGR_AGENT_SIGNING_PRIVATE_KEY
  WPMGR_AGENT_SIGNING_PUBLIC_KEY
  WPMGR_SITE_DEST_AGE_SECRET
)

# Infra password keys that are randomized once on first init (not regenerated on
# subsequent runs unless --force). Shared with compose init scripts.
INFRA_PASSWORD_KEYS=(
  WPMGR_DB_OWNER_PASSWORD
  WPMGR_DB_APP_PASSWORD
  WPMGR_S3_SECRET_KEY
)

# The first-run ownership claim (see apps/api/internal/auth/bootstrap_claim.go).
# Randomized once, same as the infra passwords above: never rotated on a
# re-run once a value is present, because rotating it before anyone has
# claimed the install is harmless but rotating it after is confusing (the
# operator who wrote it down would suddenly hold a stale value). There is no
# shipped placeholder for this key in .env.example, so an absent or empty
# value is the only "needs one" signal — needs_value already treats both that
# way.
BOOTSTRAP_CLAIM_KEY=WPMGR_BOOTSTRAP_CLAIM_SECRET

# Committed DEV placeholders shipped in .env.example. These are PUBLIC and the
# control plane rejects the agent private key in production, so a fresh install
# must replace them. We treat them like an empty value (regenerate them).
DEV_SENTINEL_WPMGR_AGENT_SIGNING_PRIVATE_KEY='aWuH1W3DSfBwuE/V/H9BEmV9IAJfK5d6F2RDfYSj/raBW+b26qHT3spd1gHSw7aXEXxZkg9E9WMspibSjSFsnQ=='
DEV_SENTINEL_WPMGR_AGENT_SIGNING_PUBLIC_KEY='gVvm9uqh097KXdYB0sO2lxF8WZIPRPVjLKYm0o0hbJ0='

# Dev placeholder values for infra passwords shipped in .env.example.
DEV_SENTINEL_WPMGR_DB_OWNER_PASSWORD='wpmgr'
DEV_SENTINEL_WPMGR_DB_APP_PASSWORD='wpmgr-app-dev-secret'
DEV_SENTINEL_WPMGR_S3_SECRET_KEY='wpmgr-dev-secret'

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cleanup() { [ -n "${GEN_FILE}" ] && rm -f "${GEN_FILE}"; }
trap cleanup EXIT

# current_value: prints the current value of KEY in .env (empty if absent/blank).
current_value() {
  grep -E "^$1=" "${ENV_FILE}" | head -n1 | cut -d= -f2- || true
}

# needs_value: true (0) when KEY is absent, present-but-empty, OR still holds the
# committed DEV placeholder for that key.
needs_value() {
  local key="$1" cur sentinel_var sentinel
  cur="$(current_value "${key}")"
  if [ -z "${cur}" ]; then
    return 0 # absent or empty
  fi
  sentinel_var="DEV_SENTINEL_${key}"
  sentinel="${!sentinel_var:-}"
  if [ -n "${sentinel}" ] && [ "${cur}" = "${sentinel}" ]; then
    return 0 # still the committed dev placeholder
  fi
  return 1 # has a real value
}

# set_env_value: portable (BSD/macOS + GNU) in-place set of KEY=VALUE in .env.
set_env_value() {
  local key="$1" value="$2" tmp esc
  tmp="$(mktemp)"
  # Escape sed replacement metacharacters in the value (\, &, and delimiter |).
  esc="$(printf '%s' "${value}" | sed -e 's/[\\&|]/\\&/g')"
  if grep -q "^${key}=" "${ENV_FILE}"; then
    sed "s|^${key}=.*|${key}=${esc}|" "${ENV_FILE}" >"${tmp}"
  else
    cp "${ENV_FILE}" "${tmp}"
    printf '%s=%s\n' "${key}" "${value}" >>"${tmp}"
  fi
  mv "${tmp}" "${ENV_FILE}"
}

gen_with_go() {
  command -v go >/dev/null 2>&1 || return 1
  log "Generating secrets with the local Go toolchain (wpmgr-cli gen-secrets)"
  (cd "${ROOT_DIR}/apps/api" && go run ./cmd/wpmgr-cli gen-secrets) >"${GEN_FILE}" 2>/dev/null
}

gen_with_docker() {
  command -v docker >/dev/null 2>&1 || return 1
  docker compose version >/dev/null 2>&1 || return 1
  log "Generating secrets via Docker (docker compose run api wpmgr-cli gen-secrets)"
  # The api image's entrypoint is the wpmgr server binary; override it to run the
  # bundled CLI. --no-deps avoids starting Postgres/Redis just to print keys.
  # Try the prod overlay first (uses the prebuilt GHCR image — works in the
  # no-clone quickstart context where no source tree is present). Fall back to
  # the base compose (which has a build: stanza, only works with a source tree).
  local compose_files=()
  if [ -f "${ROOT_DIR}/infra/docker-compose.prod.yml" ]; then
    compose_files=(-f "${ROOT_DIR}/infra/docker-compose.yml" -f "${ROOT_DIR}/infra/docker-compose.prod.yml")
  else
    compose_files=(-f "${ROOT_DIR}/infra/docker-compose.yml")
  fi
  docker compose "${compose_files[@]}" run --rm --no-deps \
    --entrypoint wpmgr-cli api gen-secrets >"${GEN_FILE}" 2>/dev/null
}

gen_with_host_tools() {
  command -v openssl >/dev/null 2>&1 || return 1
  command -v age-keygen >/dev/null 2>&1 || return 1
  command -v xxd >/dev/null 2>&1 || return 1
  log "Generating secrets with host openssl + age-keygen (fallback)"
  local tmp seed_hex pub_hex priv64_b64 pub_b64
  tmp="$(mktemp -d)"
  openssl genpkey -algorithm ed25519 -out "${tmp}/priv.pem" 2>/dev/null
  # Raw 32-byte ed25519 seed = final 32 bytes of the PKCS8 private-key DER.
  seed_hex="$(openssl pkey -in "${tmp}/priv.pem" -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n')"
  # Raw 32-byte public key = final 32 bytes of the SPKI DER.
  pub_hex="$(openssl pkey -in "${tmp}/priv.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n')"
  # The app expects base64-std of the 64-byte Go layout: seed || public.
  priv64_b64="$(printf '%s%s' "${seed_hex}" "${pub_hex}" | xxd -r -p | base64 | tr -d '\n')"
  pub_b64="$(printf '%s' "${pub_hex}" | xxd -r -p | base64 | tr -d '\n')"
  {
    printf 'WPMGR_SESSION_SECRET=%s\n' "$(openssl rand -base64 48 | tr -d '\n')"
    printf 'WPMGR_AGENT_SIGNING_PRIVATE_KEY=%s\n' "${priv64_b64}"
    printf 'WPMGR_AGENT_SIGNING_PUBLIC_KEY=%s\n' "${pub_b64}"
    printf 'WPMGR_SITE_DEST_AGE_SECRET=%s\n' "$(age-keygen 2>/dev/null | grep '^AGE-SECRET-KEY-')"
  } >"${GEN_FILE}"
  rm -rf "${tmp}"
}

print_next_steps() {
  # Read the actual host ports from .env (fall back to compose defaults).
  local api_port web_port claim_secret
  api_port="$(current_value WPMGR_API_PORT)"
  api_port="${api_port:-8081}"
  web_port="$(current_value WPMGR_WEB_PORT)"
  web_port="${web_port:-8088}"
  # Read back from .env rather than trusting a var from this run: the value
  # may have been generated just now, supplied by the operator earlier, or
  # (if something above failed silently) still absent — print exactly what is
  # actually persisted, not what this run assumed.
  claim_secret="$(current_value "${BOOTSTRAP_CLAIM_KEY}")"

  local claim_step
  if [ -n "${claim_secret}" ]; then
    claim_step="$(cat <<CLAIMEOF
  5. Claim ownership of this install. Nobody can sign up through the normal
     Sign Up form in the dashboard for the FIRST account — the provisioning
     claim only travels as a request header, deliberately kept out of the
     OpenAPI schema every generated client (including the dashboard) uses.
     Run this once, after the stack is up, to create the first owner account:

       curl -X POST http://localhost:${api_port}/auth/register \\
         -H "Content-Type: application/json" \\
         -H "X-Wpmgr-Bootstrap-Claim: ${claim_secret}" \\
         -d '{"email":"you@example.com","password":"replace-with-12+-chars"}'

     ${BOOTSTRAP_CLAIM_KEY} is saved in .env — the value above is that exact
     secret. It is not re-generated on a later re-run of this script once a
     value is present, so it stays valid until you rotate it yourself.
CLAIMEOF
)"
  else
    claim_step="  5. WARNING: ${BOOTSTRAP_CLAIM_KEY} is not set in .env — this install
     cannot be claimed by anyone until it is. Set it to a random secret and
     re-run: docker compose ... up -d (after editing .env) to pick it up."
  fi

  cat <<EOF

Next steps:
  1. Edit .env if needed:
       - Set WPMGR_PUBLIC_BASE_URL to a URL your agents can reach (already set
         if you used --hostname= or answered the prompt above).
       - Set WPMGR_S3_ENDPOINT to a URL reachable from your WordPress hosts
         (the in-network default http://seaweedfs:8333 only works inside Docker).
       - For production: set WPMGR_ENV=production.
       - DB + S3 passwords were randomized above — they are consistent across
         .env and infra/seaweedfs/s3.json; no hand-editing needed.

  2. Bring up the stack (prebuilt GHCR images, no clone required):
       docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d

     Or build from source:
       docker compose -f infra/docker-compose.yml up -d

  3. Verify (note: the host-facing port is ${api_port}, NOT :8080 — that is the
     in-container listen address):
       curl localhost:${api_port}/healthz   # {"status":"ok"}
       curl localhost:${api_port}/readyz    # 200 once DB/Redis/S3 are reachable

  4. Open the dashboard at http://localhost:${web_port}

${claim_step}

See docs/install.md for the full guide.
EOF
}

# ---------------------------------------------------------------------------
# Parse args.
# ---------------------------------------------------------------------------
for arg in "$@"; do
  case "${arg}" in
    --force) FORCE=1 ;;
    --hostname=*) HOSTNAME_OVERRIDE="${arg#--hostname=}" ;;
    -h | --help)
      sed -n '3,31p' "${BASH_SOURCE[0]}" | sed 's/^#\{0,1\} \{0,1\}//'
      exit 0
      ;;
    *)
      echo "init-env: unknown argument '${arg}' (try --help)" >&2
      exit 2
      ;;
  esac
done

# ---------------------------------------------------------------------------
# 1. Create .env from .env.example.
# ---------------------------------------------------------------------------
[ -f "${ENV_EXAMPLE}" ] || die ".env.example not found at ${ENV_EXAMPLE}"

if [ -f "${ENV_FILE}" ] && [ "${FORCE}" -eq 0 ]; then
  log ".env already exists — keeping it (use --force to overwrite from .env.example)"
else
  if [ -f "${ENV_FILE}" ] && [ "${FORCE}" -eq 1 ]; then
    cp "${ENV_FILE}" "${ENV_FILE}.bak"
    log "Backed up existing .env -> .env.bak"
  fi
  cp "${ENV_EXAMPLE}" "${ENV_FILE}"
  log "Wrote .env from .env.example"
fi

# ---------------------------------------------------------------------------
# 1b. Generate the first-run ownership claim (WPMGR_BOOTSTRAP_CLAIM_SECRET),
#     unless the operator already supplied one. Without this, POST
#     /auth/register refuses every first-run attempt with 403
#     registration_closed and the install can never be claimed.
#
#     This runs BEFORE the "everything already set" early-exit below, and is
#     not gated by it: an operator re-running this script on an .env from
#     before this variable existed (the upgrade case) must still get one, even
#     though their four SECRET_KEYS are already populated and would otherwise
#     short-circuit the rest of this script.
#
#     Idempotent, same rule as the infra passwords below: a value already
#     present (from a prior run, or supplied by the operator in .env before
#     running this script) is left untouched, never rotated. Rotating it after
#     ownership is claimed is harmless but rotating it before is confusing —
#     an operator who copied down a value would have it silently invalidated.
# ---------------------------------------------------------------------------
if needs_value "${BOOTSTRAP_CLAIM_KEY}"; then
  new_claim="$(openssl rand -base64 48 | tr -d '\n')"
  set_env_value "${BOOTSTRAP_CLAIM_KEY}" "${new_claim}"
  log "Generated ${BOOTSTRAP_CLAIM_KEY}"
else
  log "${BOOTSTRAP_CLAIM_KEY} already set in .env — left unchanged"
fi

# ---------------------------------------------------------------------------
# 2. Skip generation if every managed secret is already set.
# ---------------------------------------------------------------------------
ANY_EMPTY=0
for key in "${SECRET_KEYS[@]}"; do
  if needs_value "${key}"; then
    ANY_EMPTY=1
  fi
done

if [ "${ANY_EMPTY}" -eq 0 ]; then
  log "All managed secrets already set in .env — nothing to generate"
  print_next_steps
  exit 0
fi

# ---------------------------------------------------------------------------
# 3. Generate secrets as KEY=VALUE lines into a temp file.
# ---------------------------------------------------------------------------
GEN_FILE="$(mktemp)"

if ! gen_with_go && ! gen_with_docker && ! gen_with_host_tools; then
  die "could not generate secrets — need one of: a Go toolchain, Docker Compose, or (openssl + age-keygen + xxd)."
fi

# Sanity: the generator must have emitted all four keys with non-empty values.
for key in "${SECRET_KEYS[@]}"; do
  val="$(grep -E "^${key}=" "${GEN_FILE}" | head -n1 | cut -d= -f2-)"
  [ -n "${val}" ] || die "secret generator did not produce a value for ${key}"
done

# ---------------------------------------------------------------------------
# 4. Inject generated values into .env, ONLY for keys that need one.
# ---------------------------------------------------------------------------
# The agent private/public keys are a MATCHED PAIR — if either needs a value we
# must replace BOTH from the same freshly generated keypair, never mix an old
# half with a new one.
if needs_value WPMGR_AGENT_SIGNING_PRIVATE_KEY || needs_value WPMGR_AGENT_SIGNING_PUBLIC_KEY; then
  FORCE_AGENT_PAIR=1
else
  FORCE_AGENT_PAIR=0
fi

INJECTED=()
for key in "${SECRET_KEYS[@]}"; do
  replace=0
  case "${key}" in
    WPMGR_AGENT_SIGNING_PRIVATE_KEY | WPMGR_AGENT_SIGNING_PUBLIC_KEY)
      [ "${FORCE_AGENT_PAIR}" -eq 1 ] && replace=1
      ;;
    *)
      needs_value "${key}" && replace=1
      ;;
  esac
  if [ "${replace}" -eq 1 ]; then
    val="$(grep -E "^${key}=" "${GEN_FILE}" | head -n1 | cut -d= -f2-)"
    set_env_value "${key}" "${val}"
    INJECTED+=("${key}")
  else
    log "${key} already set in .env — left unchanged"
  fi
done

if [ "${#INJECTED[@]}" -gt 0 ]; then
  log "Injected fresh secrets: ${INJECTED[*]}"
fi

# ---------------------------------------------------------------------------
# 5. Randomize infra passwords (DB owner, DB app, S3 secret key).
#    Only replaces a value if it is still the committed dev placeholder.
#    The S3 secret key is also written into infra/seaweedfs/s3.json so that
#    the SeaweedFS auth config and .env always match.
# ---------------------------------------------------------------------------
INFRA_INJECTED=()
for key in "${INFRA_PASSWORD_KEYS[@]}"; do
  if needs_value "${key}"; then
    new_pw="$(openssl rand -base64 24 | tr -d '/+=' | head -c 32)"
    set_env_value "${key}" "${new_pw}"
    INFRA_INJECTED+=("${key}")

    # Keep DB_PASSWORD (the app-role password used at runtime) in sync with
    # WPMGR_DB_APP_PASSWORD (the compose/init-script counterpart). Both are set
    # from the same generated value so neither falls out of step.
    if [ "${key}" = "WPMGR_DB_APP_PASSWORD" ]; then
      set_env_value "WPMGR_DB_PASSWORD" "${new_pw}"
      INFRA_INJECTED+=("WPMGR_DB_PASSWORD")
    fi

    # Propagate the new S3 secret into infra/seaweedfs/s3.json so SeaweedFS
    # uses the identical credential. The json file is a simple template with
    # exactly one "secretKey" field; we update it in-place with sed.
    if [ "${key}" = "WPMGR_S3_SECRET_KEY" ] && [ -f "${S3_JSON}" ]; then
      tmp_json="$(mktemp)"
      esc_pw="$(printf '%s' "${new_pw}" | sed -e 's/[\\&|]/\\&/g')"
      sed "s|\"secretKey\":.*|\"secretKey\": \"${esc_pw}\"|" "${S3_JSON}" >"${tmp_json}"
      mv "${tmp_json}" "${S3_JSON}"
      log "Wrote matching S3 secret to ${S3_JSON}"
    fi
  else
    log "${key} already set to a non-placeholder value — left unchanged"
  fi
done

if [ "${#INFRA_INJECTED[@]}" -gt 0 ]; then
  log "Randomized infra passwords: ${INFRA_INJECTED[*]}"
fi

# ---------------------------------------------------------------------------
# 6. Set WPMGR_PUBLIC_BASE_URL — prompt interactively if not already set and
#    no --hostname flag was given.
# ---------------------------------------------------------------------------
current_pub="$(current_value WPMGR_PUBLIC_BASE_URL)"
# Every localhost placeholder .env.example has ever shipped. A value missing
# from this list is read as "the operator chose this deliberately", so the
# hostname prompt is skipped and the install quietly keeps a localhost URL that
# no agent can reach: whenever the shipped default changes, add it here.
placeholder_pub_8080="http://localhost:8080"
placeholder_pub_8081="http://localhost:8081"
placeholder_pub_8088="http://localhost:8088"
placeholder_pub_default="${placeholder_pub_8088}"

_is_placeholder_pub() {
  [ -z "${current_pub}" ] \
    || [ "${current_pub}" = "${placeholder_pub_8080}" ] \
    || [ "${current_pub}" = "${placeholder_pub_8081}" ] \
    || [ "${current_pub}" = "${placeholder_pub_8088}" ]
}

if [ -n "${HOSTNAME_OVERRIDE}" ]; then
  set_env_value "WPMGR_PUBLIC_BASE_URL" "${HOSTNAME_OVERRIDE}"
  log "Set WPMGR_PUBLIC_BASE_URL=${HOSTNAME_OVERRIDE}"
elif _is_placeholder_pub; then
  if [ -t 0 ]; then
    # stdin is a terminal — prompt.
    printf '\n==> What is the public URL of this control plane?\n'
    printf '    (Agents reach back here, emailed links and the social sign-in\n'
    printf '     callback are built from it; press Enter to keep %s)\n' "${current_pub:-${placeholder_pub_default}}"
    printf '    URL: '
    read -r input_pub </dev/tty || true
    if [ -n "${input_pub}" ] && ! { [ "${input_pub}" = "${placeholder_pub_8080}" ] || [ "${input_pub}" = "${placeholder_pub_8081}" ] || [ "${input_pub}" = "${placeholder_pub_8088}" ]; }; then
      set_env_value "WPMGR_PUBLIC_BASE_URL" "${input_pub}"
      log "Set WPMGR_PUBLIC_BASE_URL=${input_pub}"
    else
      log "WPMGR_PUBLIC_BASE_URL left at ${current_pub:-${placeholder_pub_default}}: update it before adding real sites"
    fi
  else
    warn "WPMGR_PUBLIC_BASE_URL is '${current_pub}' (localhost). Set it to a URL your agents can reach."
  fi
fi

print_next_steps
