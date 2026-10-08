#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${1:-}" != run-all || $# != 1 ]]; then
  echo 'usage: scripts/test-file-messages.sh run-all' >&2
  exit 2
fi
for name in IM_TEST_DATABASE_URL IM_TEST_REDIS_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST IM_TEST_UNPROVEN_CLAMD_SOCKET IM_TEST_UNPROVEN_SCANNER_MANIFEST IM_TEST_BROWSER_NODE; do
  if [[ -z "${!name:-}" ]]; then echo "required environment missing: $name" >&2; exit 2; fi
done
if [[ -n "${IM_TEST_FILE_MESSAGE_OUTPUT_DIR:-}" ]]; then
  output="$IM_TEST_FILE_MESSAGE_OUTPUT_DIR"
  if [[ "$output" != /* || -L "$output" ]]; then echo 'output must be an absolute private directory' >&2; exit 2; fi
  mkdir -p "$output"
else
  output="$(mktemp -d "${TMPDIR:-/tmp}/im-p423-evidence.XXXXXX")"
fi
chmod 700 "$output"
log="$output/file-messages.jsonl"
if [[ -L "$log" ]]; then echo 'refuse symlink output' >&2; exit 2; fi
umask 077
status=0
go test -json ./internal/policystore ./cmd/im-api -run '^TestFileMessage(Real|Production|RetiredHTTP)|^TestFileScanReal(Trusted|Unproven)Runtime$' -count=1 > "$log" 2> "$log.stderr" || status=$?
python3 - "$log" "$status" <<'PY'
import json,sys
required={'TestFileMessageRealScanSendPull','TestFileMessageRealRealtime','TestFileMessageRealBrowserLegacy','TestFileMessageProductionClosed','TestFileMessageProductionClosedConfiguration','TestFileMessageRetiredHTTP','TestFileScanRealTrustedRuntime','TestFileScanRealUnprovenRuntime'}
passed=set();failed=[];skipped=[]
for line in open(sys.argv[1]):
 try:event=json.loads(line)
 except json.JSONDecodeError:continue
 name=event.get('Test','');action=event.get('Action')
 if action=='pass' and name:passed.add(name)
 if action=='fail':failed.append(name or event.get('Package'))
 if action=='skip':skipped.append(name or event.get('Package'))
missing=required-passed
if int(sys.argv[2]) or failed or skipped or missing:
 print('file-message gate failed; missing:',sorted(missing),'failed:',failed,'skipped:',skipped,file=sys.stderr);sys.exit(1)
print('file-message gate passed; selected tests:',len(required),'test pass events:',len(passed),'FAIL=0 SKIP=0')
PY
printf 'private evidence: %s\n' "$log"
