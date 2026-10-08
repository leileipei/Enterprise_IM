#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${1:-}" != run-all || $# != 1 ]]; then echo 'usage: scripts/test-file-download-retention.sh run-all' >&2;exit 2;fi
for name in IM_TEST_DATABASE_URL IM_TEST_REDIS_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_TEST_S3_POLICY_BUCKET IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY IM_TEST_FILE_UPLOAD_ACCESS_KEY IM_TEST_FILE_UPLOAD_SECRET_KEY IM_TEST_FILE_WORKER_ACCESS_KEY IM_TEST_FILE_WORKER_SECRET_KEY IM_FILE_CLEANUP_S3_ACCESS_KEY IM_FILE_CLEANUP_S3_SECRET_KEY IM_TEST_S3_ADMIN_ACCESS_KEY IM_TEST_S3_ADMIN_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST IM_TEST_UNPROVEN_CLAMD_SOCKET IM_TEST_UNPROVEN_SCANNER_MANIFEST IM_TEST_BROWSER_NODE;do
 if [[ -z "${!name:-}" ]];then echo "required environment missing: $name" >&2;exit 2;fi
done
for file in "$IM_TEST_SCANNER_MANIFEST" "$IM_TEST_UNPROVEN_SCANNER_MANIFEST";do [[ -f "$file" && ! -L "$file" ]]||{ echo 'fresh scanner manifest required' >&2;exit 2;};done
if [[ -n "${IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR:-}" ]];then output="$IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR";[[ "$output" == /* && ! -L "$output" ]]||{ echo 'absolute private output directory required' >&2;exit 2;};mkdir -p "$output";else output="$(mktemp -d "${TMPDIR:-/tmp}/im-p424-evidence.XXXXXX")";fi
chmod 700 "$output";umask 077
log="$output/file-download-retention.jsonl";[[ ! -L "$log" ]]||{ echo 'refuse symlink output' >&2;exit 2;}
status=0
go test -json ./internal/policystore ./cmd/im-api ./cmd/im-file-cleaner -run '^TestFileDownload(Real|Production)|^TestFileDeleteReal|^TestFileCleanerDefaultClosed$|^TestFileScanReal(Trusted|Unproven)Runtime$' -count=1 > "$log" 2> "$log.stderr"||status=$?
python3 - "$log" "$status" <<'PY'
import json,sys
required={'TestFileDownloadRealOIDCScan','TestFileDownloadRealTokenExpiryBlockedWrite','TestFileDownloadRealRevocation','TestFileDownloadRealAuditRepair','TestFileDownloadRealBrowserLegacy','TestFileDeleteRealVersionsIAM','TestFileDeleteRealUnknownDelete','TestFileDeleteRealHoldOrdering','TestFileDeleteRealOrphanQuarantine','TestFileDownloadProductionClosed','TestFileDownloadRealTCPRevocation','TestFileDownloadRealTotalDeadline','TestFileDeleteRealMarker403','TestFileCleanerDefaultClosed','TestFileScanRealTrustedRuntime','TestFileScanRealUnprovenRuntime'}
passed=set();failed=[];skipped=[]
for line in open(sys.argv[1]):
 try:e=json.loads(line)
 except json.JSONDecodeError:continue
 action=e.get('Action');name=e.get('Test','')
 if action=='pass' and name:passed.add(name)
 if action=='fail':failed.append(name or e.get('Package'))
 if action=='skip':skipped.append(name or e.get('Package'))
missing=required-passed
if int(sys.argv[2]) or failed or skipped or missing:
 print('download-retention gate failed; missing:',sorted(missing),'failed:',failed,'skipped:',skipped,file=sys.stderr);sys.exit(1)
print('download-retention gate passed; required:',len(required),'test pass events:',len(passed),'FAIL=0 SKIP=0')
PY
printf 'private evidence: %s\n' "$log"
