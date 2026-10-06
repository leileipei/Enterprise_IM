#!/usr/bin/env python3
"""Run P4-27 gates from an immutable Git commit using an owned offline PG fixture."""
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


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def checked(args, cwd=None, env=None, timeout=1500):
    return subprocess.check_output(args, cwd=cwd, env=env, stderr=subprocess.STDOUT,
                                   timeout=timeout).decode().strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--image', default='postgres:16-alpine')
    args = parser.parse_args()
    repo = Path(checked(['git', 'rev-parse', '--show-toplevel']))
    commit = checked(['git', 'rev-parse', '--verify', args.commit + '^{commit}'], repo)
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)  # preserve all previous attempts
    source = out / 'source'
    source.mkdir()
    archive = out / 'source.tar'
    with archive.open('wb') as f:
        subprocess.run(['git', 'archive', '--format=tar', commit], cwd=repo, stdout=f, check=True)
    with tarfile.open(archive) as tar:
        # Python 3.9 on macOS lacks extraction filters. Git archives contain only
        # regular files/directories here; reject links and traversal before extracting.
        for member in tar.getmembers():
            if (not (member.isfile() or member.isdir()) or Path(member.name).is_absolute()
                    or '..' in Path(member.name).parts):
                raise RuntimeError('unsafe source archive member')
        tar.extractall(source)
    bins = out / 'bin'
    bins.mkdir()
    result = {'source_commit': commit, 'source_archive_sha256': sha(archive),
              'gates': {}, 'cleanup': {'removed': False}, 'bin_sha256': {},
              'offline_product_only': True, 'customer_acceptance': 'not_executed'}
    container = None
    owner = str(uuid.uuid4())

    def gate(name, argv, cwd=source, env=None, parse='none', critical=False):
        log = out / (name + '.log')
        started = time.monotonic()
        with log.open('wb') as f:
            cp = subprocess.run(argv, cwd=cwd, env=env, stdout=f, stderr=subprocess.STDOUT,
                                timeout=1500)
        text = log.read_text(errors='replace')
        counts = {'top_pass': 0, 'sub_pass': 0, 'fail': 0, 'skip': 0}
        if parse == 'json':
            for line in text.splitlines():
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                action, test = row.get('Action'), row.get('Test')
                if action == 'pass' and test:
                    counts['sub_pass' if '/' in test else 'top_pass'] += 1
                elif action == 'fail':
                    counts['fail'] += 1
                elif action == 'skip':
                    counts['skip'] += 1
        elif parse == 'verbose':
            for action, test in re.findall(r'^\s*--- (PASS|FAIL|SKIP): (\S+)', text, re.M):
                if action == 'PASS':
                    counts['sub_pass' if '/' in test else 'top_pass'] += 1
                else:
                    counts[action.lower()] += 1
        result['gates'][name] = {'exit_code': cp.returncode, 'counts': counts,
                                 'seconds': round(time.monotonic() - started, 3),
                                 'log_sha256': sha(log), 'command': argv}
        print(name, cp.returncode, counts, flush=True)
        if cp.returncode or counts['fail'] or critical and (counts['skip'] or not counts['top_pass']):
            raise RuntimeError('gate failed: ' + name)
        return text

    try:
        result['go_version'] = checked(['go', 'version'])
        env = dict(os.environ, GOFLAGS='-mod=readonly', GOWORK='off')
        for name, flags in [('host_unit', ['-timeout=5m']), ('host_race', ['-race', '-timeout=10m'])]:
            gate(name, ['go', 'test', '-json'] + flags + ['./internal/importpreflight',
                 './cmd/im-import-preflight', '-skip', '^TestPreflightPostgresOracle$', '-count=1'],
                 env=env, parse='json', critical=True)
        gate('build_all', ['go', 'build', './...'], env=env)
        gate('vet_all', ['go', 'vet', './...'], env=env)
        linux_env = dict(env, GOOS='linux', GOARCH='arm64', CGO_ENABLED='0')
        gate('linux_build', ['go', 'build', '-buildvcs=false', '-o', str(bins/'im-import-preflight-linux-arm64'),
                            './cmd/im-import-preflight'], env=linux_env)
        gate('darwin_build', ['go', 'build', '-buildvcs=false', '-o', str(bins/'im-import-preflight-darwin-arm64'),
                             './cmd/im-import-preflight'], env=env)
        for pkg in ['importpreflight', 'cli', 'groupdb', 'access', 'oidcauth']:
            path = './cmd/im-import-preflight' if pkg == 'cli' else './internal/' + pkg
            gate(pkg+'_test_build', ['go', 'test', '-c', '-o', str(bins/(pkg+'.test')), path], env=linux_env)
        result['bin_sha256'] = {p.name: sha(p) for p in sorted(bins.iterdir())}
        inspect = json.loads(checked(['docker', 'image', 'inspect', args.image]))[0]
        result['image_id'] = inspect['Id']
        if inspect['Architecture'] != 'arm64':
            raise RuntimeError('requires cached arm64 image')
        name = 'im-preflight-' + owner[:12]
        container = checked(['docker', 'run', '--detach', '--rm', '--name', name,
                             '--label', 'im.preflight.owner=' + owner, '--network', 'none',
                             '--tmpfs', '/var/lib/postgresql/data:rw',
                             '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', '-e', 'POSTGRES_DB=im_preflight',
                             '--mount', 'type=bind,src='+str(source)+',dst=/source,readonly',
                             '--mount', 'type=bind,src='+str(bins)+',dst=/bins,readonly', inspect['Id']])
        result['container_id'] = container
        result['owner'] = owner
        for _ in range(100):
            cp = subprocess.run(['docker', 'exec', container, 'pg_isready', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'im_preflight'],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            if cp.returncode == 0:
                break
            time.sleep(.1)
        else:
            raise RuntimeError('owned PostgreSQL did not become ready')
        meta = json.loads(checked(['docker', 'inspect', container]))[0]
        if meta['HostConfig']['NetworkMode'] != 'none' or meta['HostConfig']['PortBindings']:
            raise RuntimeError('fixture exposure changed')
        result['postgres_version'] = checked(['docker', 'exec', container, 'psql', '-U', 'postgres',
                                              '-d', 'im_preflight', '-Atc', 'select version()'])
        dsn = 'host=/var/run/postgresql user=postgres dbname=im_preflight sslmode=disable'
        def docker_test(name, pkg, path, extra=None, critical=False):
            argv = ['docker', 'exec', '-e', 'IM_TEST_DATABASE_URL='+dsn,
                    '-e', 'IM_PREFLIGHT_TEST_BINARY=/bins/im-import-preflight-linux-arm64',
                    '-w', '/source/'+path, container, '/bins/'+pkg+'.test',
                    '-test.v', '-test.timeout=12m', '-test.count=1'] + (extra or [])
            return gate(name, argv, parse='verbose', critical=critical)
        docker_test('linux_cli', 'cli', 'cmd/im-import-preflight', critical=True)
        docker_test('linux_unit', 'importpreflight', 'internal/importpreflight',
                    ['-test.skip=^TestPreflightPostgresOracle$'], critical=True)
        docker_test('postgres_oracle', 'importpreflight', 'internal/importpreflight',
                    ['-test.run=^TestPreflightPostgresOracle$'], critical=True)
        for pkg in ['groupdb', 'access', 'oidcauth']:
            docker_test('regression_'+pkg, pkg, 'internal/'+pkg, critical=True)
        command = ['docker', 'exec', container, '/bins/im-import-preflight-linux-arm64', '--input',
                   '/source/internal/importpreflight/testdata/sample_data_v1.json']
        report = subprocess.check_output(command, timeout=15)
        (out/'sample-report-linux.json').write_bytes(report)
        host_report = subprocess.check_output([str(bins/'im-import-preflight-darwin-arm64'), '--input',
                              str(source/'internal/importpreflight/testdata/sample_data_v1.json')], timeout=15)
        (out/'sample-report-darwin.json').write_bytes(host_report)
        sample = source/'internal/importpreflight/testdata/sample_data_v1.json'
        parsed = json.loads(report)
        if report != host_report or parsed['status'] != 'valid' or parsed['counts']['total'] != 74 or parsed['input_sha256'] != sha(sample):
            raise RuntimeError('sample report provenance failed')
        result['sample_sha256'] = sha(sample)
        result['sample_report_sha256'] = hashlib.sha256(report).hexdigest()
        # The archive must leave dependency locks and production schema unchanged.
        for locked in ['go.mod', 'go.sum']:
            if sha(source/locked) != hashlib.sha256(subprocess.check_output(['git','show',
                          '80424dbd5a537023c33e56654f4a12b522885a21:'+locked],cwd=repo)).hexdigest():
                raise RuntimeError('dependency lock changed')
        result['status'] = 'passed'
    except Exception as error:
        result['status'] = 'failed'
        result['failure'] = str(error)
        raise
    finally:
        if container:
            cp = subprocess.run(['docker', 'inspect', container], capture_output=True, text=True, timeout=10)
            if cp.returncode == 0:
                owned = json.loads(cp.stdout)[0]
                if owned['Config']['Labels'].get('im.preflight.owner') != owner:
                    result['cleanup']['failure'] = 'ownership mismatch; did not stop'
                else:
                    subprocess.run(['docker', 'stop', '-t', '3', container], check=True,
                                   stdout=subprocess.DEVNULL, timeout=15)
                    for _ in range(100):
                        remaining = subprocess.run(['docker', 'inspect', container], capture_output=True, timeout=5)
                        if remaining.returncode != 0:
                            result['cleanup']['removed'] = True
                            break
                        time.sleep(.1)
            else:
                result['cleanup']['removed'] = True
        if container and not result['cleanup']['removed']:
            result['status'] = 'failed'
            result.setdefault('failure', 'fixture cleanup not confirmed')
        (out/'verification.json').write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
        print('verification', result['status'], 'cleanup', result['cleanup'], flush=True)
    if not result['cleanup']['removed']:
        raise RuntimeError('fixture cleanup not confirmed')


if __name__ == '__main__':
    main()
