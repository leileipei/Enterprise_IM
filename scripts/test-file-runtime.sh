#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
require() { eval 'value=${'"$1"'-}'; if [ -z "$value" ]; then printf 'Missing required environment: %s\n' "$1" >&2; exit 1; fi; }
core() { for key in IM_TEST_DATABASE_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_TEST_S3_POLICY_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY; do require "$key"; done; }
scan() { core; for key in IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST IM_TEST_UNPROVEN_CLAMD_SOCKET IM_TEST_UNPROVEN_SCANNER_MANIFEST; do require "$key"; done
 [ -x "$IM_TEST_QPDF_PATH" ] && [ -S "$IM_TEST_CLAMD_SOCKET" ] && [ -f "$IM_TEST_SCANNER_MANIFEST" ] && [ -S "$IM_TEST_UNPROVEN_CLAMD_SOCKET" ] && [ -f "$IM_TEST_UNPROVEN_SCANNER_MANIFEST" ] || { printf 'Scanner dependency unavailable\n' >&2; exit 1; }
}
case "${1:-}" in
 run-s3) core; go test ./internal/objectstore -count=1 -v; go test ./internal/policystore -run TestFileTransferReal -count=1 -v ;;
 run-scan) scan; go test ./internal/filescanner -count=1 -v; go test ./internal/policystore ./internal/filetransfer -run TestFileScan -count=1 -v ;;
 run-all)
  scan; for key in IM_TEST_FILE_WORKER_ACCESS_KEY IM_TEST_FILE_WORKER_SECRET_KEY IM_TEST_FILE_RUNTIME_OUTPUT_DIR; do require "$key"; done
  output="$IM_TEST_FILE_RUNTIME_OUTPUT_DIR"
  case "$output" in /*) ;; *) printf 'Absolute private output directory required\n' >&2; exit 1;; esac
  mkdir -p "$output"; chmod 700 "$output"
  go test -json ./cmd/im-api ./cmd/im-file-worker -run 'TestFileAPIAssembly|TestFileWorkerConfig' -count=1 > "$output/assembly.json"
  go test -json ./internal/files ./internal/objectstore ./internal/filescanner ./internal/filetransfer ./internal/httpserver ./internal/policystore -run 'TestFile|TestS3|TestClamd|TestScanner|TestUploadPolicy' -count=1 > "$output/components.json"
  python3 - "$output" <<'PYJSON'
import json,sys
from pathlib import Path
for name in ["assembly.json","components.json"]:
 events=[json.loads(line) for line in (Path(sys.argv[1])/name).read_text().splitlines()]
 skipped=[e.get("Test") for e in events if e.get("Action")=="skip"]
 if skipped:raise SystemExit("Component gate cannot skip: "+str(skipped))
 print(name,"PASS",sum(e.get("Action")=="pass" and "Test" in e for e in events),"SKIP",len(skipped))
PYJSON
  architecture=$(docker info --format '{{.Architecture}}')
  case "$architecture" in aarch64|arm64) architecture=arm64 ;; x86_64|amd64) architecture=amd64 ;; *) printf 'Unsupported resource fixture architecture\n' >&2; exit 1;; esac
  python3 testdata/file-runtime/samples/generate_structure.py "$output/structure-samples"
  GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go test -c -o "$output/structure.test" ./internal/filescanner
  GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go test -c -o "$output/transfer.test" ./internal/filetransfer
  image='alpine@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40'
  docker run --rm --memory=512m --cpus=1 -v "$output:/fixtures:ro" -v "$(pwd):/source:ro" -e IM_TEST_STRUCTURE_SAMPLES=/fixtures/structure-samples "$image" /fixtures/structure.test -test.run '^TestScannerRealResourceBoundary$' -test.v > "$output/resource.log"
  docker run --rm --memory=512m --cpus=1 --tmpfs /limited:size=1048576,mode=0700 -e IM_TEST_SPOOL_FULL_DIR=/limited -v "$output:/fixtures:ro" -v "$(pwd):/source:ro" "$image" /fixtures/transfer.test -test.run '^TestFile(TransferRealDiskFull|Spool)' -test.v > "$output/disk-full.log"
  if rg --quiet -- '--- SKIP:|--- FAIL:' "$output/resource.log" "$output/disk-full.log"; then printf 'Resource gate incomplete\n' >&2; exit 1; fi
  cat "$output/resource.log" "$output/disk-full.log" ;;
 *) printf 'Usage: %s run-s3|run-scan|run-all\n' "$0" >&2; exit 2 ;;
esac
