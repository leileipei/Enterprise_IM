#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ $# != 1 || "$1" != run-all ]]; then echo 'usage: scripts/test-file-business-runtime.sh run-all' >&2; exit 2; fi
for name in IM_TEST_DATABASE_URL IM_TEST_REDIS_URL IM_TEST_S3_ENDPOINT IM_TEST_S3_BUCKET IM_TEST_S3_POLICY_BUCKET IM_TEST_FILE_UPLOAD_ACCESS_KEY IM_TEST_FILE_UPLOAD_SECRET_KEY IM_TEST_FILE_WORKER_ACCESS_KEY IM_TEST_FILE_WORKER_SECRET_KEY IM_TEST_FILE_DOWNLOAD_ACCESS_KEY IM_TEST_FILE_DOWNLOAD_SECRET_KEY IM_FILE_CLEANUP_S3_ACCESS_KEY IM_FILE_CLEANUP_S3_SECRET_KEY IM_TEST_FILE_BOOTSTRAP_ACCESS_KEY IM_TEST_FILE_BOOTSTRAP_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET IM_TEST_SCANNER_MANIFEST IM_TEST_BROWSER_NODE CHROMIUM_EXECUTABLE; do
 if [[ -z "${!name:-}" ]]; then echo "required process fixture missing: $name" >&2; exit 2; fi
done
umask 077
if [[ -z "${IM_TEST_FILE_BUSINESS_BUILD_SHA:-}" ]]; then
 if ! git diff --quiet || ! git diff --cached --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then echo 'fixed clean source required' >&2; exit 2; fi
 IM_TEST_FILE_BUSINESS_BUILD_SHA="$(git rev-parse HEAD)"
fi
export IM_TEST_FILE_BUSINESS_BUILD_SHA
if [[ -z "${IM_TEST_FILE_BUSINESS_OUTPUT_DIR:-}" ]]; then
 IM_TEST_FILE_BUSINESS_OUTPUT_DIR="$(python3 -c 'import pathlib,tempfile; print(pathlib.Path(tempfile.mkdtemp(prefix="im-p426-evidence-")).resolve())')"
fi
export IM_TEST_FILE_BUSINESS_OUTPUT_DIR
python3 - <<'PY'
import os,pathlib,re,stat,sys,urllib.parse
try:
 root=pathlib.Path(os.environ['IM_TEST_FILE_BUSINESS_OUTPUT_DIR'])
 if not root.is_absolute():raise ValueError()
 for p in [root,*root.parents]:
  if p.exists() or p.is_symlink():
   if stat.S_ISLNK(os.lstat(p).st_mode):raise ValueError()
 root.mkdir(mode=0o700,parents=True,exist_ok=True)
 s=root.stat()
 if s.st_uid!=os.geteuid() or stat.S_IMODE(s.st_mode)!=0o700:raise ValueError()
 if not re.fullmatch('[0-9a-f]{40}',os.environ['IM_TEST_FILE_BUSINESS_BUILD_SHA']):raise ValueError()
 d=urllib.parse.urlparse(os.environ['IM_TEST_DATABASE_URL'])
 if d.hostname not in ['127.0.0.1','localhost'] or d.path!='/enterprise_im_files':raise ValueError()
 if os.environ['IM_TEST_S3_BUCKET']==os.environ['IM_TEST_S3_POLICY_BUCKET']:raise ValueError()
 if not all(os.environ[k].startswith('p426-') for k in ['IM_TEST_S3_BUCKET','IM_TEST_S3_POLICY_BUCKET']):raise ValueError()
 keys=[os.environ[k] for k in ['IM_TEST_FILE_UPLOAD_ACCESS_KEY','IM_TEST_FILE_WORKER_ACCESS_KEY','IM_TEST_FILE_DOWNLOAD_ACCESS_KEY','IM_FILE_CLEANUP_S3_ACCESS_KEY']]
 if len(set(keys))!=4:raise ValueError()
 for k in ['IM_TEST_QPDF_PATH','IM_TEST_BROWSER_NODE','CHROMIUM_EXECUTABLE']:
  if not pathlib.Path(os.environ[k]).is_absolute() or not os.access(os.environ[k],os.X_OK):raise ValueError()
 if not stat.S_ISSOCK(os.stat(os.environ['IM_TEST_CLAMD_SOCKET']).st_mode):raise ValueError()
 if pathlib.Path(os.environ['IM_TEST_SCANNER_MANIFEST']).is_symlink():raise ValueError()
except (KeyError,OSError,ValueError):
 print('invalid or unowned private process fixture',file=sys.stderr);sys.exit(2)
PY
log="$IM_TEST_FILE_BUSINESS_OUTPUT_DIR/business-runtime.jsonl"
if [[ -e "$log" || -L "$log" ]]; then echo 'fresh evidence path required' >&2; exit 2; fi
status=0
go test -json -timeout=30m ./internal/policystore -run '^TestFileBusinessProcessRP(0[1-9]|1[0-4])$' -count=1 > "$log" 2>&1 || status=$?
python3 - "$log" "$status" <<'PY'
import json,sys
required={f'TestFileBusinessProcessRP{i:02d}' for i in range(1,15)}
passed=set();failed=[];skipped=[]
for line in open(sys.argv[1]):
 try:e=json.loads(line)
 except json.JSONDecodeError:continue
 action=e.get('Action');name=e.get('Test','')
 if action=='pass' and name:passed.add(name)
 if action=='fail':failed.append(name or e.get('Package'))
 if action=='skip':skipped.append(name or e.get('Package'))
missing=required-passed
if int(sys.argv[2]) or missing or failed or skipped:
 print('business-process gate failed; missing:',sorted(missing),'failed:',failed,'skipped:',skipped,file=sys.stderr);sys.exit(1)
print('business-process gate passed; mandatory=14 FAIL=0 SKIP=0')
PY
printf 'private evidence: %s\n' "$log"
