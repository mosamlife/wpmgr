#!/usr/bin/env bash
# Regenerate Go server types and the TS client from the OpenAPI source of truth.
# Codegen tooling is wired in Phase 4 (ADR-004 Go codegen, ADR-013 TS client).
set -euo pipefail
cd "$(dirname "$0")/.."
echo "OpenAPI codegen is wired in Phase 4. Source: packages/openapi/openapi.yaml"
