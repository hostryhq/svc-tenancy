#!/usr/bin/env bash
# Regenerates internal/gen from the api-specs checkout (default ../api-specs; override API_SPECS_DIR).
# Uses the oapi-codegen version pinned by api-specs (its Makefile) and records the spec commit.
set -euo pipefail
cd "$(dirname "$0")/.."
SPECS="${API_SPECS_DIR:-../api-specs}"
OAPI="${OAPI_CODEGEN:-$SPECS/.tools/bin/oapi-codegen}"
REDOCLY_VERSION=2.83.0
mkdir -p .tmp internal/gen
npx --yes "@redocly/cli@${REDOCLY_VERSION}" bundle "$SPECS/openapi/tenancy/v1/openapi.yaml" -o .tmp/tenancy.yaml >/dev/null
"$OAPI" -generate types,std-http-server,client -package gen -o internal/gen/tenancy.gen.go .tmp/tenancy.yaml
go run ./tools/gen -spec "$SPECS/openapi/tenancy/v1/openapi.yaml" -permissions "$SPECS/common/permissions.yaml" -out internal/gen
{
  echo "api-specs commit: $(cd "$SPECS" && git rev-parse HEAD)"
  echo "oapi-codegen: $("$OAPI" --version | tail -1)"
} > internal/gen/SPEC_VERSION
echo "generated from $(head -1 internal/gen/SPEC_VERSION)"
