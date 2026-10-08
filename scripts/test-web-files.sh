#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${1:-}" != run-all || $# != 1 ]]; then echo 'usage: scripts/test-web-files.sh run-all' >&2; exit 2; fi
for name in IM_TEST_DATABASE_URL IM_TEST_REDIS_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY IM_TEST_FILE_UPLOAD_ACCESS_KEY IM_TEST_FILE_UPLOAD_SECRET_KEY IM_TEST_FILE_WORKER_ACCESS_KEY IM_TEST_FILE_WORKER_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST IM_TEST_BROWSER_NODE CHROMIUM_EXECUTABLE; do
 if [[ -z "${!name:-}" ]]; then echo "required environment missing: $name" >&2; exit 2; fi
done
umask 077
output="${IM_TEST_WEB_FILE_OUTPUT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/im-p425-evidence.XXXXXX")}"
if [[ "$output" != /* || -L "$output" ]]; then echo 'absolute private output directory required' >&2; exit 2; fi
mkdir -p "$output"
chmod 700 "$output"
log="$output/web-files.jsonl"
if [[ -L "$log" ]]; then echo 'refuse symlink output' >&2; exit 2; fi
status=0
go test -json -timeout=30m ./internal/policystore -run '^TestWebFileReal(Lifecycle|RejectedScan|Settings|ProductionClosed|Search|SearchFinalBoundary|ContextIsolation|UnknownUploadSend|DownloadFaults|Revocation|PolicyConflict)$' -count=1 > "$log" 2> "$log.stderr" || status=$?
python3 - "$log" "$status" <<'PY'
import json,sys
required={'TestWebFileRealLifecycle','TestWebFileRealRejectedScan','TestWebFileRealSettings','TestWebFileRealProductionClosed','TestWebFileRealSearch','TestWebFileRealSearchFinalBoundary','TestWebFileRealContextIsolation','TestWebFileRealUnknownUploadSend','TestWebFileRealDownloadFaults','TestWebFileRealRevocation','TestWebFileRealPolicyConflict'}
passed=set();failed=[];skipped=[]
for line in open(sys.argv[1]):
 try:e=json.loads(line)
 except json.JSONDecodeError:continue
 name=e.get('Test','');a=e.get('Action')
 if a=='pass' and name:passed.add(name)
 if a=='fail':failed.append(name or e.get('Package'))
 if a=='skip':skipped.append(name or e.get('Package'))
missing=required-passed
if int(sys.argv[2]) or failed or skipped or missing:
 print('web-file gate failed; missing:',sorted(missing),'failed:',failed,'skipped:',skipped,file=sys.stderr);sys.exit(1)
print('web-file gate passed; required:',len(required),'test pass events:',len(passed),'FAIL=0 SKIP=0')
PY
printf 'private evidence: %s\n' "$log"
