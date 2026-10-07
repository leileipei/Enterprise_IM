"""Construct test environments from literal keys rather than ambient variables."""
from pathlib import Path
from typing import Dict
from .model import Toolchain

# Startup-only process/crash switches are deliberately absent.
FIXTURE_KEYS = set('''IM_FILE_S3_ACCESS_KEY IM_FILE_S3_SECRET_KEY IM_TEST_FILE_RUNTIME_OUTPUT_DIR
IM_TEST_FILE_MESSAGE_OUTPUT_DIR IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR IM_TEST_WEB_FILE_OUTPUT_DIR
IM_TEST_DATABASE_URL IM_TEST_REDIS_URL IM_TEST_S3_ENDPOINT
IM_TEST_S3_BUCKET IM_TEST_S3_POLICY_BUCKET IM_TEST_S3_ADMIN_ACCESS_KEY
IM_TEST_S3_ADMIN_SECRET_KEY IM_TEST_QPDF_PATH IM_TEST_CLAMD_SOCKET
IM_TEST_SCANNER_MANIFEST IM_TEST_UNPROVEN_CLAMD_SOCKET
IM_TEST_UNPROVEN_SCANNER_MANIFEST IM_TEST_STRUCTURE_SAMPLES IM_TEST_SPOOL_FULL_DIR
IM_TEST_BROWSER_NODE CHROMIUM_EXECUTABLE IM_TEST_FILE_BUSINESS_BUILD_SHA
IM_TEST_FILE_BUSINESS_OUTPUT_DIR IM_IMPORT_APPLY_TEST_ADMIN_URL
IM_IMPORT_APPLY_TEST_DATABASE_URL IM_COMPARE_TEST_BINARY IM_COMPARE_TEST_CA
IM_COMPARE_TEST_WRONG_CA IM_PREFLIGHT_TEST_BINARY IM_FILE_CLEANUP_S3_ACCESS_KEY
IM_FILE_CLEANUP_S3_SECRET_KEY IM_TEST_INTEGRATION_REGISTRY IM_TEST_INTEGRATION_OWNER
IM_TEST_INTEGRATION_SOURCE_SHA IM_TEST_INTEGRATION_GATE IM_TEST_INTEGRATION_PROBE_VERSION'''.split())
for _role in ('UPLOAD','WORKER','DOWNLOAD','BOOTSTRAP'):
    for _suffix in ('ACCESS_KEY','SECRET_KEY'):
        FIXTURE_KEYS.add('IM_TEST_FILE_'+_role+'_'+_suffix)


def test_environment(tools: Toolchain,private: Path,values: Dict[str,str]) -> Dict[str,str]:
    if set(values)-FIXTURE_KEYS:raise ValueError('unknown_test_environment_key')
    if any(not isinstance(value,str) or '\x00' in value for value in values.values()):
        raise ValueError('invalid_test_environment_value')
    private=Path(private).resolve();private.mkdir(mode=0o700,parents=True,exist_ok=True)
    directories={key:private/name for key,name in
                 [('TMPDIR','tmp'),('GOCACHE','go-build'),('GOMODCACHE','go-mod'),('GOPATH','go-path')]}
    for directory in directories.values():directory.mkdir(mode=0o700,exist_ok=True)
    paths=sorted({str(path.parent) for key,path in tools.paths.items() if key!='node_modules'})
    env=dict(PATH=':'.join(paths+['/opt/homebrew/bin','/usr/local/bin','/usr/bin','/bin','/usr/sbin','/sbin']),
             LANG='C.UTF-8',TZ='UTC',GOWORK='off',GOFLAGS='-mod=readonly -buildvcs=false',
             GOENV='off',PYTHONDONTWRITEBYTECODE='1')
    env.update({key:str(value) for key,value in directories.items()})
    if 'node_modules' in tools.paths:env['NODE_PATH']=str(tools.paths['node_modules'])
    env.update(values)
    return env
