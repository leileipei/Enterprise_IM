#!/usr/bin/env python3
"""Verify P4-29 from immutable Git source and a disposable, isolated PG16 fixture."""
import argparse
import hashlib
import json
import os
import re
import subprocess
import tarfile
import time
import uuid
from pathlib import Path

REQUIRED_GATES = ['unit', 'race', 'cli_linux', 'comparison_database', 'tls',
                  'append_oracle', 'resource_process', 'offline_linux', 'offline_oracle',
                  'groupdb_regression', 'access_regression', 'ack_regression',
                  'oidc_regression']
REQUIRED_TESTS = {
    'comparison_database': ['TestComparePGSnapshotReadOnly', 'TestComparePGIsolation',
        'TestComparePGProfile', 'TestComparePGColumnWritePermission',
        'TestComparePGStructuralProfile', 'TestComparePGResourceEdges',
        'TestComparePGUnsupportedTimes', 'TestComparePGGlobalOccupancy',
        'TestComparePGDDLAndRevoke', 'TestComparePGQueryBudget',
        'TestCompareProcessEnvIsolation', 'TestCompareProcessActualSIGTERM'],
    'cli_linux': ['TestCompareProcessNoNetwork', 'TestCompareProcessBadPrivateWorker',
        'TestCompareProcessSharedAbsoluteDeadline', 'TestCompareProcessSignalAndPipe'],
    'tls': ['TestComparePGTLS/verify_full', 'TestComparePGTLS/wrong_ca',
        'TestComparePGTLS/wrong_hostname'] + ['TestCompareProcessTLS/'+k for k in ['verify_full_home_traps','wrong_ca','wrong_hostname','downgrade']],
    'resource_process': ['TestCompareProcessResourceEdges/'+k for k in ['rows_20000','rows_20001','bytes_64_mib','bytes_plus_one','cell_4097']],
    'append_oracle': ['TestComparePGOracle/' + x for x in
        ['new', 'pk', 'unique', 'interval', 'dependency_fk', 'additional_check']],
    'offline_oracle': ['TestPreflightPostgresOracle'],
    'ack_regression': ['TestMessageACKUsesPersistedTime'],
    'oidc_regression': ['TestSignedTokenThroughHTTPToAuditedAdminAndOrdinaryDirectory'],
}
REQUIRED_COMMANDS = ['build_all', 'vet_all', 'provenance', 'fixture_cleanup', 'signal_repeat']
ENTITIES = ['tenants', 'legal_entities', 'organizations', 'departments', 'users',
            'user_organizations', 'user_departments', 'external_identities', 'admin_grants']


def counts_valid(report):
    try:
        counts, classes = report['counts'], report['classification_counts']
        if set(counts) != set(ENTITIES + ['total']) or set(classes) != set(counts):
            return False
        for key, number in counts.items():
            c = classes[key]
            if type(number) is not int or number < 0 or set(c) != {'new', 'identical', 'conflict'}:
                return False
            if any(type(v) is not int or v < 0 for v in c.values()) or sum(c.values()) != number:
                return False
        return (sum(counts[k] for k in ENTITIES) == counts['total'] and
                all(sum(classes[k][col] for k in ENTITIES) == classes['total'][col]
                    for col in ['new', 'identical', 'conflict']))
    except (KeyError, TypeError):
        return False


def validate_required_gates(events):
    groups = {}
    duplicate = False
    for event in events:
        name = event.get('name')
        duplicate |= name in groups
        groups[name] = event
    def green(event):
        c = event.get('counts', {})
        return (event.get('exit_code') == 0 and c.get('fail') == 0 and c.get('skip') == 0
                and type(c.get('top_pass')) is int and c['top_pass'] > 0)
    failures = [name for name in REQUIRED_GATES if name not in groups or
                not green(groups[name]) or
                not set(REQUIRED_TESTS.get(name, [])).issubset(groups[name].get('executed_tests', []))]
    reports = groups.get('comparison_database', {}).get('reports', [])
    if not reports or not all(counts_valid(r) for r in reports):
        failures.append('classification_conservation')
    if not groups.get('cleanup', {}).get('removed', False):
        failures.append('cleanup')
    if duplicate:
        failures.append('duplicate_gate')
    for name in REQUIRED_COMMANDS:
        if name not in groups or groups[name].get('exit_code') != 0:
            failures.append(name)
    full = groups.get('full_suite', {})
    return {'required_gates_passed': not failures, 'full_suite_passed': green(full),
            'failed_gates': failures,
            'failed_tests': full.get('failed_tests', []),
            'skipped_tests': full.get('skipped_tests', [])}


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def checked(args, cwd=None, env=None, timeout=120):
    return subprocess.check_output(args, cwd=cwd, env=env, stderr=subprocess.STDOUT,
                                   timeout=timeout).decode().strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--image', default='postgres:16-alpine')
    args = parser.parse_args()
    repo = Path(checked(['git', 'rev-parse', '--show-toplevel']))
    commit = checked(['git', 'rev-parse', '--verify', args.commit + '^{commit}'], repo)
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    source, bins, tls = (out / x for x in ['source', 'bin', 'tls-private'])
    for path in [source, bins, tls]:
        path.mkdir(mode=0o700)
    archive = out / 'source.tar'
    with archive.open('wb') as f:
        subprocess.run(['git', 'archive', '--format=tar', commit], cwd=repo, stdout=f, check=True)
    with tarfile.open(archive) as tar:
        for member in tar.getmembers():
            if (not (member.isfile() or member.isdir()) or Path(member.name).is_absolute()
                    or '..' in Path(member.name).parts):
                raise RuntimeError('unsafe archive member')
        tar.extractall(source)
    owner, container = str(uuid.uuid4()), None
    events = []
    result = {'source_commit': commit, 'source_archive_sha256': sha(archive), 'owner': owner,
              'customer_acceptance': 'not_executed', 'events': events, 'cleanup': {'removed': False}}

    def gate(name, argv, env=None, parse='none', cwd=source):
        log = out / (name + '.log')
        start = time.monotonic()
        with log.open('wb') as f:
            try:
                cp = subprocess.run(argv, cwd=cwd, env=env, stdout=f, stderr=subprocess.STDOUT, timeout=1500)
                code = cp.returncode
            except subprocess.TimeoutExpired:
                code = 124
        text = log.read_text(errors='replace')
        counts = dict(top_pass=0, sub_pass=0, fail=0, skip=0)
        passed, failed, skipped, reports = [], [], [], []
        outcomes = []
        if parse == 'json':
            for line in text.splitlines():
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                action, test = row.get('Action'), row.get('Test')
                if action in ['pass', 'fail', 'skip'] and (test or action == 'fail'):
                    outcomes.append((action, test or '<package>', row.get('Package', '')))
        elif parse == 'verbose':
            outcomes = [(a.lower(), test, '') for a, test in
                        re.findall(r'^\s*--- (PASS|FAIL|SKIP): (\S+)', text, re.M)]
        for action, test, package in outcomes:
            if action == 'pass':
                counts['sub_pass' if '/' in test else 'top_pass'] += 1
                passed.append(test)
            else:
                counts[action] += 1
                (failed if action == 'fail' else skipped).append(package + '::' + test)
        for line in text.splitlines():
            marker = 'COMPARISON_REPORT '
            if marker in line:
                reports.append(json.loads(line.split(marker, 1)[1]))
        event = dict(name=name, exit_code=code, counts=counts, executed_tests=passed,
                     failed_tests=failed, skipped_tests=skipped, reports=reports,
                     query_counts=[int(n) for n in re.findall(r'SNAPSHOT_SQL_COUNT (\d+)', text)],
                     seconds=round(time.monotonic()-start, 3), log_sha256=sha(log), command=argv)
        events.append(event)
        print(name, code, counts, flush=True)
        # Full-suite failures are evidence, never hidden or promoted to a green gate.
        if name != 'full_suite' and (code or counts['fail'] or name in REQUIRED_GATES
                                    and (counts['skip'] or not counts['top_pass'])):
            raise RuntimeError('gate failed: ' + name)
        return text

    try:
        result['go_version'] = checked(['go', 'version'])
        env = {k: v for k, v in os.environ.items() if not k.startswith('IM_')}
        env.update(GOFLAGS='-mod=readonly', GOWORK='off')
        node = Path.home()/'.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin'
        if (node/'node').is_file():
            env['PATH'] = str(node) + os.pathsep + env.get('PATH', '')
        packages = ['./internal/importpreflight', './internal/importinput', './internal/importcompare',
                    './cmd/im-import-preflight', './cmd/im-import-compare']
        skip = '^TestComparePG|^TestPreflightPostgresOracle$|^TestCompareProcessEnvIsolation$|^TestCompareProcessActualSIGTERM$|^TestCompareProcessTLS$|^TestCompareProcessResourceEdges$'
        for name, extra in [('unit', []), ('race', ['-race'])]:
            gate(name, ['go', 'test', '-json', '-timeout=5m', '-count=1', '-skip', skip] + extra + packages,
                 env=env, parse='json')
        gate('build_all', ['go', 'build', './...'], env=env)
        gate('vet_all', ['go', 'vet', './...'], env=env)
        linux = dict(env, GOOS='linux', GOARCH='arm64', CGO_ENABLED='0')
        for command in ['im-import-preflight', 'im-import-compare']:
            for platform, build_env in [('darwin-arm64', env), ('linux-arm64', linux)]:
                gate(command+'_'+platform, ['go', 'build', '-buildvcs=false', '-o',
                     str(bins/(command+'-'+platform)), './cmd/'+command], env=build_env)
        gate('windows_build', ['go','build','-o',str(bins/'im-import-compare-windows-amd64.exe'),
             './cmd/im-import-compare'], env=dict(env, GOOS='windows', GOARCH='amd64', CGO_ENABLED='0'))
        paths = {'compare_cli':'cmd/im-import-compare', 'preflight_cli':'cmd/im-import-preflight'}
        paths.update({p:'internal/'+p for p in ['importcompare','importpreflight','groupdb','access','oidcauth','policystore']})
        for pkg, path in paths.items():
            gate(pkg+'_build', ['go', 'test', '-c', '-o', str(bins/(pkg+'.test')), './'+path], env=linux)
        result['bin_sha256'] = {p.name:sha(p) for p in sorted(bins.iterdir())}
        inspect = json.loads(checked(['docker', 'image', 'inspect', args.image]))[0]
        if inspect['Architecture'] != 'arm64':
            raise RuntimeError('requires cached arm64 PG16 image')
        result['image_id'] = inspect['Id']
        # Certificates belong only to this offline fixture; no system trust stores are changed.
        for prefix in ['ca', 'wrong-ca']:
            checked(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2',
                     '-subj','/CN=IM comparison fixture '+prefix,'-keyout',str(tls/(prefix+'.key')),
                     '-out',str(tls/(prefix+'.crt'))])
        checked(['openssl','req','-newkey','rsa:2048','-nodes','-subj','/CN=127.0.0.1',
                 '-keyout',str(tls/'server.key'),'-out',str(tls/'server.csr')])
        (tls/'server.ext').write_text('subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n')
        checked(['openssl','x509','-req','-in',str(tls/'server.csr'),'-CA',str(tls/'ca.crt'),
                 '-CAkey',str(tls/'ca.key'),'-CAserial',str(tls/'ca.srl'),'-CAcreateserial','-days','2','-extfile',str(tls/'server.ext'),
                 '-out',str(tls/'server.crt')])
        for path in tls.iterdir():
            path.chmod(0o600)
        result['certificate_sha256'] = {p.name:sha(p) for p in tls.glob('*.crt')}
        container = checked(['docker','run','-d','--rm','--name','im-compare-'+owner[:12],
            '--label','im.compare.owner='+owner,'--network','none','--tmpfs','/var/lib/postgresql/data:rw',
            '-e','POSTGRES_HOST_AUTH_METHOD=trust','-e','POSTGRES_DB=im_compare',
            '--mount','type=bind,src='+str(source)+',dst=/source,readonly',
            '--mount','type=bind,src='+str(bins)+',dst=/bins,readonly',
            '--mount','type=bind,src='+str(tls)+',dst=/tls,readonly',inspect['Id']])
        result['container_id'] = container
        for _ in range(100):
            cp = subprocess.run(['docker','exec',container,'pg_isready','-h','127.0.0.1','-U','postgres','-d','im_compare'],
                                stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
            if cp.returncode == 0:
                break
            time.sleep(.1)
        else:
            raise RuntimeError('fixture not ready')
        meta = json.loads(checked(['docker','inspect',container]))[0]
        if meta['HostConfig']['NetworkMode'] != 'none' or meta['HostConfig']['PortBindings']:
            raise RuntimeError('fixture exposure changed')
        psql = ['docker','exec',container,'psql','-U','postgres','-d','im_compare','-Atc']
        result['postgres_version'] = checked(psql + ['select version()'])
        inventory_sql = "select 'schema:'||nspname from pg_namespace union all select 'role:'||rolname from pg_roles order by 1"
        baseline_inventory = checked(psql + [inventory_sql])
        checked(['docker','exec',container,'sh','-c',
                 'cp /tls/server.key /tls/server.crt /var/lib/postgresql/data/; chown postgres:postgres /var/lib/postgresql/data/server.*; chmod 600 /var/lib/postgresql/data/server.key'])
        for key, value in [('ssl','on'),('ssl_cert_file','/var/lib/postgresql/data/server.crt'),
                           ('ssl_key_file','/var/lib/postgresql/data/server.key')]:
            checked(psql + ["ALTER SYSTEM SET " + key + " = '" + value + "'"])
        checked(psql + ['select pg_reload_conf()'])
        for _ in range(100):
            if checked(psql + ['show ssl']) == 'on':
                break
            time.sleep(.1)
        else:
            raise RuntimeError('SSL reload failed')
        def dbgate(name, pkg, extra=None):
            command = ['docker','exec','-w','/source/'+paths[pkg],container,'env','-i',
                'PATH=/usr/local/bin:/usr/bin:/bin','TZ=UTC',
                'IM_TEST_DATABASE_URL=host=/var/run/postgresql port=5432 user=postgres dbname=im_compare sslmode=disable',
                'IM_COMPARE_TEST_BINARY=/bins/im-import-compare-linux-arm64',
                'IM_PREFLIGHT_TEST_BINARY=/bins/im-import-preflight-linux-arm64',
                'IM_COMPARE_TEST_CA=/tls/ca.crt','IM_COMPARE_TEST_WRONG_CA=/tls/wrong-ca.crt',
                '/bins/'+pkg+'.test','-test.v','-test.timeout=12m','-test.count=1'] + (extra or [])
            return gate(name, command, parse='verbose')
        dbgate('cli_linux','compare_cli')
        dbgate('comparison_database','importcompare', ['-test.run=^TestComparePG|^TestCompareProcessEnvIsolation$|^TestCompareProcessActualSIGTERM$',
                '-test.skip=^TestComparePGTLS$|^TestComparePGOracle$'])
        dbgate('signal_repeat','importcompare',['-test.run=^TestCompareProcessActualSIGTERM$','-test.count=3'])
        dbgate('tls','importcompare',['-test.run=^TestComparePGTLS$|^TestCompareProcessTLS$'])
        dbgate('resource_process','importcompare',['-test.run=^TestCompareProcessResourceEdges$'])
        dbgate('append_oracle','importcompare',['-test.run=^TestComparePGOracle$'])
        dbgate('offline_linux','preflight_cli')
        dbgate('offline_oracle','importpreflight',['-test.run=^TestPreflightPostgresOracle$'])
        for pkg in ['groupdb','access','oidcauth']:
            dbgate(('oidc' if pkg == 'oidcauth' else pkg)+'_regression',pkg)
        ack = '^TestMessageACKUsesPersistedTime$|^TestSendTextMessage|^TestSendGroupTextMessage|^TestFileMessageDirect|^TestFileMessageGroup|^TestTextSend|^TestTextReplay|^TestFileMessageHistory|^TestFileMessageIdempotency|^TestValidateClientMessageIDWindowAndVersion$|^TestMessageContentBounds$|^TestFileMessageDigestCanonical$'
        dbgate('ack_regression','policystore',['-test.run='+ack])
        # All per-test schema and role cleanup must succeed before disposing the container.
        residue = checked(psql + ["select (select count(*) from pg_namespace where nspname like 'im_compare_%') + (select count(*) from pg_roles where rolname like 'im_compare_%')"])
        if residue != '0' or checked(psql + [inventory_sql]) != baseline_inventory:
            raise RuntimeError('comparison fixture schema/role residue')
        events.append({'name':'fixture_cleanup','exit_code':0,'schema_role_residue':0})
        sample = source/'internal/importpreflight/testdata/sample_data_v1.json'
        host_report = subprocess.check_output([str(bins/'im-import-preflight-darwin-arm64'),'--input',str(sample)],timeout=35)
        linux_report = subprocess.check_output(['docker','exec',container,'/bins/im-import-preflight-linux-arm64',
            '--input','/source/internal/importpreflight/testdata/sample_data_v1.json'],timeout=35)
        expected = (source/'internal/importpreflight/testdata/sample_report_v1.json').read_bytes()
        if host_report != linux_report or host_report != expected:
            raise RuntimeError('offline report bytes changed')
        (out/'offline-report.json').write_bytes(host_report)
        for locked in ['go.mod','go.sum']:
            previous = subprocess.check_output(['git','show','96c32564bdda035de74647573a51af9be3e0d6bc:'+locked],cwd=repo)
            if sha(source/locked) != hashlib.sha256(previous).hexdigest():
                raise RuntimeError('dependency lock changed')
        if checked(['git','diff','--name-only','96c32564bdda035de74647573a51af9be3e0d6bc',commit,'--','db/migrations'],repo):
            raise RuntimeError('production migrations changed')
        events.append({'name':'provenance','exit_code':0,'sample_sha256':sha(sample),
                       'sample_report_sha256':sha(out/'offline-report.json')})
        gate('full_suite',['go','test','-json','-timeout=10m','-count=1','./...'],env=env,parse='json')
    except Exception as error:
        result['failure'] = str(error)
    finally:
        if container:
            cp = subprocess.run(['docker','inspect',container],capture_output=True,text=True,timeout=10)
            if cp.returncode == 0:
                owned = json.loads(cp.stdout)[0]
                if owned['Config']['Labels'].get('im.compare.owner') == owner:
                    subprocess.run(['docker','stop','-t','3',container],check=True,stdout=subprocess.DEVNULL,timeout=15)
                    for _ in range(100):
                        remaining = subprocess.run(['docker','inspect',container],capture_output=True,timeout=5)
                        if remaining.returncode != 0:
                            result['cleanup']['removed'] = True
                            break
                        time.sleep(.1)
                else:
                    result['cleanup']['failure'] = 'ownership mismatch'
            else:
                result['cleanup']['removed'] = True
        # Fixture private keys never belong to the delivered evidence package.
        for path in tls.iterdir():
            if path.suffix in ['.key','.csr','.srl','.ext']:
                path.unlink()
        events.append(dict(name='cleanup', **result['cleanup']))
        result.update(validate_required_gates(events))
        if 'failure' in result:
            result['required_gates_passed'] = False
        (out/'verification.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
        print('required_gates_passed',result['required_gates_passed'],
              'full_suite_passed',result['full_suite_passed'],'cleanup',result['cleanup'],flush=True)
    return 0 if result['required_gates_passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
