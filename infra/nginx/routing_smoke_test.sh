#!/usr/bin/env bash
# infra/nginx/routing_smoke_test.sh
#
# Regression test for GH #175 (and its two same-class latent siblings).
#
# `nginx -t` only validates SYNTAX. It happily passes on a config that is
# missing an entire `location` block for a public route the Go API actually
# serves — which is exactly how self-hosted RUM ingest shipped completely
# broken: every `POST /rum/ingest` fell through nginx's SPA `try_files`
# catch-all to a static-file 405, and never reached the correctly-registered
# Go handler.
#
# This script runs the REAL shipped infra/nginx/nginx.conf (by default via
# the actual published infra/Dockerfile.web image, so it exercises the
# artifact we ship — not just the config file in isolation) against a stub
# upstream, and asserts observable HTTP OUTCOMES: proxied-to-upstream vs.
# nginx's own SPA-fallback 405. `nginx -t` cannot express this; only a real
# request through a real config can.
#
# Usage:
#   infra/nginx/routing_smoke_test.sh
#       Builds infra/Dockerfile.web fresh and runs the full suite.
#
#   WPMGR_WEB_IMAGE=<tag> infra/nginx/routing_smoke_test.sh
#       Reuse an already-built wpmgr-web image instead of rebuilding.
#
#   WPMGR_NGINX_SMOKE_BINDMOUNT=1 infra/nginx/routing_smoke_test.sh
#       Skip the full web image build; instead run plain nginx:1.27-alpine
#       with infra/nginx/nginx.conf bind-mounted straight in. Faster, but
#       does not exercise infra/Dockerfile.web itself (only the config file).
#
# Exit code is non-zero if any assertion fails.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="$$-$RANDOM"
NET="wpmgr-nginx-smoke-net-${RUN_ID}"
API_CONTAINER="wpmgr-nginx-smoke-api-${RUN_ID}"
WEB_CONTAINER="wpmgr-nginx-smoke-web-${RUN_ID}"
WEB_PORT="${WPMGR_NGINX_SMOKE_PORT:-18080}"
TMPDIR="$(mktemp -d)"
BUILT_IMAGE=""

cleanup() {
  docker rm -f "$API_CONTAINER" "$WEB_CONTAINER" >/dev/null 2>&1 || true
  if [ -n "$BUILT_IMAGE" ]; then
    docker rmi -f "$BUILT_IMAGE" >/dev/null 2>&1 || true
  fi
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

echo "==> creating docker network $NET"
docker network create "$NET" >/dev/null

# ---- stub upstream ---------------------------------------------------------
# Echoes "backend <METHOD> <URI>" with HTTP 200 for ANY method and ANY path.
# nginx's `return` directive (unlike static-file serving / try_files) is not
# method-restricted, so this responds identically to GET/POST/OPTIONS/etc.
# A 200 from here proves the request actually reached the api upstream; a 405
# proves nginx answered it itself (the SPA try_files catch-all).
cat >"$TMPDIR/stub.conf" <<'EOF'
server {
    listen 8080;
    location / {
        add_header Content-Type text/plain always;
        return 200 "backend $request_method $request_uri\n";
    }
}
EOF

echo "==> starting stub upstream ($API_CONTAINER), network alias 'api' to match nginx.conf's \$api_upstream"
docker run -d --name "$API_CONTAINER" --network "$NET" --network-alias api \
  -v "$TMPDIR/stub.conf:/etc/nginx/conf.d/default.conf:ro" \
  nginx:1.27-alpine >/dev/null

# ---- web/nginx under test --------------------------------------------------
if [ "${WPMGR_NGINX_SMOKE_BINDMOUNT:-0}" = "1" ]; then
  echo "==> WPMGR_NGINX_SMOKE_BINDMOUNT=1: using nginx:1.27-alpine + bind-mounted nginx.conf"
  echo "    (fallback path — exercises the config file only, NOT the built infra/Dockerfile.web image)"
  mkdir -p "$TMPDIR/html"
  echo "<!doctype html><html><body>wpmgr spa stub</body></html>" >"$TMPDIR/html/index.html"
  docker run -d --name "$WEB_CONTAINER" --network "$NET" -p "${WEB_PORT}:80" \
    -v "$REPO_ROOT/infra/nginx/nginx.conf:/etc/nginx/conf.d/default.conf:ro" \
    -v "$TMPDIR/html:/usr/share/nginx/html:ro" \
    nginx:1.27-alpine >/dev/null
else
  IMAGE="${WPMGR_WEB_IMAGE:-}"
  if [ -z "$IMAGE" ]; then
    IMAGE="wpmgr-web-routing-smoke:${RUN_ID}"
    BUILT_IMAGE="$IMAGE"
    echo "==> building the real wpmgr-web image ($IMAGE) from infra/Dockerfile.web"
    docker build -f "$REPO_ROOT/infra/Dockerfile.web" -t "$IMAGE" "$REPO_ROOT"
  else
    echo "==> reusing prebuilt image $IMAGE (WPMGR_WEB_IMAGE set)"
  fi
  echo "==> starting web ($WEB_CONTAINER) from $IMAGE"
  docker run -d --name "$WEB_CONTAINER" --network "$NET" -p "${WEB_PORT}:80" "$IMAGE" >/dev/null
fi

BASE="http://127.0.0.1:${WEB_PORT}"

echo "==> waiting for web to accept connections on :${WEB_PORT}"
ready=0
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "${BASE}/healthz" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "web container did not become ready in time" >&2
  docker logs "$WEB_CONTAINER" || true
  exit 1
fi

FAILED=0

status() {
  local method="$1" path="$2"
  curl -s -o /dev/null -w '%{http_code}' -X "$method" "${BASE}${path}"
}

# Regression guard: this exact route must NOT come back 405 (the GH #175
# symptom — nginx answering it itself via the SPA fallback instead of ever
# reaching the api upstream).
assert_not_405() {
  local method="$1" path="$2" label="$3"
  local code
  code="$(status "$method" "$path")"
  if [ "$code" = "405" ]; then
    echo "FAIL (regression guard): $label -> $method $path returned 405 (fell through to the SPA fallback instead of reaching the api upstream)"
    FAILED=1
  else
    echo "ok   (regression guard): $label -> $method $path = $code (not 405)"
  fi
}

# Positive control: this route must reach the stub upstream (200), proving
# nginx actually proxied it rather than merely returning some other
# non-405 status.
assert_proxied() {
  local method="$1" path="$2" label="$3"
  local code
  code="$(status "$method" "$path")"
  if [ "$code" != "200" ]; then
    echo "FAIL (positive control): $label -> $method $path = $code (expected 200 from stub upstream; request never reached api)"
    FAILED=1
  else
    echo "ok   (positive control): $label -> $method $path = 200 (reached stub upstream)"
  fi
}

echo
echo "---- regression guards: GH #175 class (must NOT be 405) ----"
assert_not_405 POST    /rum/ingest                     "RUM beacon ingest"
assert_not_405 OPTIONS /rum/ingest                     "RUM CORS preflight"
assert_not_405 POST    /webhooks/email/postmark/tok    "email webhook (bounce/complaint)"
assert_not_405 POST    /webhooks/billing/stripe        "billing webhook (Stripe)"

echo
echo "---- positive controls: known-good proxied routes (must reach the stub, 200) ----"
assert_proxied POST /enroll                       "agent enrollment"
assert_proxied POST /agent/v1/x                    "agent-authenticated endpoint"
assert_proxied GET  /api/v1/x                      "dashboard API"
assert_proxied POST /api/v1/invitations/accept     "dashboard API (prefix preserved)"
assert_proxied POST /auth/login                    "session auth"
assert_proxied GET  /healthz                       "healthz"
assert_proxied GET  /readyz                        "readyz"
# Re-assert the GH #175 routes as positive controls too: not-405 alone could
# be a false pass (e.g. a stray 404); confirm they actually reach the stub.
assert_proxied POST /rum/ingest                    "RUM beacon ingest (proxied)"
assert_proxied POST /webhooks/email/postmark/tok   "email webhook (proxied)"
assert_proxied POST /webhooks/billing/stripe       "billing webhook (proxied)"

echo
echo "---- class invariant: SPA fallback intact, and a truly-unrouted public POST still 405 ----"
spa_code="$(status GET /some/spa/route)"
if [ "$spa_code" != "200" ]; then
  echo "FAIL (class invariant): GET /some/spa/route = $spa_code (expected 200 index.html; SPA fallback broken)"
  FAILED=1
else
  echo "ok   (class invariant): GET /some/spa/route = 200 (SPA fallback intact)"
fi

unknown_code="$(status POST /definitely/unknown/path)"
if [ "$unknown_code" != "405" ]; then
  echo "FAIL (class invariant): POST /definitely/unknown/path = $unknown_code (expected 405)"
  FAILED=1
else
  echo "ok   (class invariant): POST /definitely/unknown/path = 405"
  echo "     (this pair is what makes it a CLASS guard: a future public root POST added in"
  echo "      Go with no matching nginx location behaves exactly like this one, and fails"
  echo "      its own NOT-405 assertion above)"
fi

echo
if [ "$FAILED" != "0" ]; then
  echo "ROUTING SMOKE TEST: FAILED"
  exit 1
fi
echo "ROUTING SMOKE TEST: PASSED"
