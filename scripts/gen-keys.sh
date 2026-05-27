#!/usr/bin/env bash
# Generate an Ed25519 keypair for the control-plane agent signing protocol.
# Prints base64-encoded private/public keys for WPMGR_AGENT_SIGNING_* env vars.
set -euo pipefail
if ! command -v openssl >/dev/null; then echo "openssl required"; exit 1; fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
openssl genpkey -algorithm ed25519 -out "$tmp/priv.pem" >/dev/null 2>&1
openssl pkey -in "$tmp/priv.pem" -pubout -out "$tmp/pub.pem" >/dev/null 2>&1
echo "WPMGR_AGENT_SIGNING_PRIVATE_KEY=$(base64 < "$tmp/priv.pem" | tr -d '\n')"
echo "WPMGR_AGENT_SIGNING_PUBLIC_KEY=$(base64 < "$tmp/pub.pem" | tr -d '\n')"
