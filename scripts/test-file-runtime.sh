#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
require() { eval 'value=${'"$1"'-}'; if [ -z "$value" ]; then printf 'Missing required environment: %s\n' "$1" >&2; exit 1; fi; }
case "${1:-}" in
 run-s3)
  for key in IM_TEST_DATABASE_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY; do require "$key"; done
  go test ./internal/objectstore -count=1 -v
  go test ./internal/policystore -run TestFileTransferReal -count=1 -v ;;
 run-scan)
  for key in IM_TEST_DATABASE_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST; do require "$key"; done
  [ -x "$IM_TEST_QPDF_PATH" ] && [ -S "$IM_TEST_CLAMD_SOCKET" ] && [ -f "$IM_TEST_SCANNER_MANIFEST" ] || { printf 'Scanner dependency unavailable\n' >&2; exit 1; }
  go test ./internal/filescanner -count=1 -v ;;
 *) printf 'Usage: %s run-s3|run-scan\n' "$0" >&2; exit 2 ;;
esac
