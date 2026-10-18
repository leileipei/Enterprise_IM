"""Reject stale definitions, changed bindings, and leaked browser resources."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration.tools import _lock,snapshot_binary

class ScannerTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration.scanner import parse_signature_info, write_manifest, validate_binding
        except ImportError:self.fail('Task5 scanner interfaces are not implemented')
        self.parse=parse_signature_info;self.write=write_manifest;self.validate=validate_binding
        self.tmp=tempfile.TemporaryDirectory(dir='/private/tmp',prefix='ip5-');self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();self.root.chmod(0o700)

    def test_stale_future_definition_refused(self):
        now=datetime.datetime(2026,10,7,13,0,tzinfo=datetime.timezone.utc)
        template='Build time: {date}\nVersion: 28146\nVerification OK.\n'
        info=self.parse(template.format(date='07 Oct 2026 06:24 +0000'),now,True)
        self.assertEqual(info['version'],'28146')
        for date in ('05 Oct 2026 06:24 +0000','08 Oct 2026 06:24 +0000'):
            with self.subTest(date=date),self.assertRaises(ValueError):
                self.parse(template.format(date=date),now,True)
        with self.assertRaises(ValueError):self.parse('Build time: 07 Oct 2026 06:24 +0000\nVersion: 28146',now,True)

    def _prepare_window(self, old_hours, updated_hours, seconds=7200, update_code=0, update_error=None):
        from integration.scanner import prepare_scanner, NAMES
        from integration.model import FixtureBundle,Toolchain
        from integration.registry import Registry
        from types import SimpleNamespace
        seed=self.root/'seed';seed.mkdir()
        for name in NAMES:(seed/name).write_bytes(name.encode())
        registry=Registry(self.root/'registry.jsonl','b'*32,'c'*40)
        bundle=FixtureBundle({},set(),registry.path,{'b'*32},{'source_commit':'c'*40,'definition_source':str(seed)})
        tools=Toolchain({key:self.root/key for key in ['freshclam','clamd','qpdf']},{},{},{},'darwin','arm64')
        now=datetime.datetime.now(datetime.timezone.utc)
        def info(hours):return {name:{'version':'1','build_time':(now-datetime.timedelta(hours=hours)).isoformat(),'sha256':'a'*64} for name in NAMES}
        class ReadyForDaemon(Exception):pass
        with patch('integration.scanner.check_definitions',side_effect=[info(old_hours),info(updated_hours)]) as check,patch('integration.scanner.subprocess.run',return_value=SimpleNamespace(returncode=update_code),side_effect=update_error) as update,patch('integration.scanner.subprocess.Popen',side_effect=ReadyForDaemon) as launch:
            try:prepare_scanner(bundle,tools,registry,self.root/registry.owner/'scanner',time.monotonic()+30,required_validity_seconds=seconds)
            except ReadyForDaemon:outcome='ready'
            except ValueError as error:outcome=str(error)
        for name in NAMES:self.assertEqual((seed/name).read_bytes(),name.encode(),'shared seed must remain unchanged')
        return outcome,update,launch,check

    def test_signed_valid_seed_refreshes_for_remaining_attempt(self):
        outcome,update,launch,check=self._prepare_window(23,1)
        self.assertEqual(outcome,'ready');self.assertEqual(update.call_count,1);self.assertEqual(launch.call_count,1);self.assertEqual(check.call_count,2)
        config=next(self.root.glob('*/scanner/freshclam.conf'))
        self.assertIn(str(config),update.call_args.args[0][1]);self.assertIn(str(config.parent/'definitions'),config.read_text())

    def test_updated_signed_seed_without_remaining_window_refused(self):
        outcome,update,launch,_=self._prepare_window(23,23)
        self.assertEqual(outcome,'definition_validity_window_insufficient');self.assertEqual(update.call_count,1);launch.assert_not_called()
        record=json.loads(next(self.root.glob('*/scanner/validity-window.json')).read_text());self.assertEqual(record['status'],'failed');self.assertTrue(record['current_definitions_verified']);self.assertEqual(record['last_verified_daily']['source'],'updated_copy');self.assertEqual(record['error'],'definition_validity_window_insufficient')

    def test_window_update_failure_stops_before_daemon(self):
        outcome,update,launch,_=self._prepare_window(23,1,update_code=42)
        self.assertEqual(outcome,'definition_update_failed');self.assertEqual(update.call_count,1);launch.assert_not_called()
        record=json.loads(next(self.root.glob('*/scanner/validity-window.json')).read_text());self.assertEqual(record['status'],'failed');self.assertFalse(record['current_definitions_verified']);self.assertEqual(record['last_verified_daily']['source'],'seed');self.assertEqual(record['update_exit_code'],42)

    def test_window_update_timeout_keeps_verified_seed_diagnostic(self):
        import subprocess
        with self.assertRaises(subprocess.TimeoutExpired):self._prepare_window(23,1,update_error=subprocess.TimeoutExpired('freshclam',1))
        record=json.loads(next(self.root.glob('*/scanner/validity-window.json')).read_text());self.assertEqual(record['status'],'failed');self.assertFalse(record['current_definitions_verified']);self.assertEqual(record['last_verified_daily']['source'],'seed');self.assertEqual(record['error'],'preparation_TimeoutExpired')

    def test_window_update_cancel_keeps_verified_seed_diagnostic(self):
        with self.assertRaises(KeyboardInterrupt):self._prepare_window(23,1,update_error=KeyboardInterrupt())
        record=json.loads(next(self.root.glob('*/scanner/validity-window.json')).read_text());self.assertEqual(record['status'],'failed');self.assertFalse(record['current_definitions_verified']);self.assertEqual(record['last_verified_daily']['source'],'seed');self.assertEqual(record['error'],'preparation_KeyboardInterrupt')

    def test_signed_seed_covering_attempt_is_not_updated(self):
        outcome,update,launch,_=self._prepare_window(1,1)
        self.assertEqual(outcome,'ready');update.assert_not_called();self.assertEqual(launch.call_count,1)

    def test_invalid_definition_window_refused_before_copy(self):
        from integration.scanner import prepare_scanner
        for value in [-1,float('nan'),float('inf'),86400,'7200',True]:
            with self.subTest(value=str(value)),self.assertRaisesRegex(ValueError,'definition_validity_window_invalid'):
                prepare_scanner(None,None,None,None,None,required_validity_seconds=value)

    def fixture(self):
        definitions=self.root/'definitions';definitions.mkdir()
        for name in ('main.cvd','daily.cvd','bytecode.cvd'):(definitions/name).write_bytes(name.encode())
        config=self.root/'clamd.conf';config.write_text('private config');config.chmod(0o400)
        binary=self.root/'clamd';binary.write_text('binary')
        qpdf=self.root/'qpdf';qpdf.write_text('qpdf')
        sock=self.root/'clamd.sock';listener=socket.socket(socket.AF_UNIX);listener.bind(str(sock));sock.chmod(0o600);self.addCleanup(listener.close)
        digest=lambda path:hashlib.sha256(path.read_bytes()).hexdigest()
        runtime={'pid':12345,'binary':str(binary),'config':str(config),'socket':str(sock),'qpdf':str(qpdf),'definitions':str(definitions),
                 'fingerprint':{'pid':12345,'uid':os.getuid(),'start_time':'bound-start','executable_path':str(binary),'executable_sha256':digest(binary),'workdir':str(self.root),'pgid':12345},
                 'hashes':{'binary':digest(binary),'config':digest(config),'qpdf':digest(qpdf)},
                 'definition_hashes':{name:digest(definitions/name) for name in ('main.cvd','daily.cvd','bytecode.cvd')}}
        runtime['manifest']=str(self.root/'manifest.json');self.write(runtime)
        return runtime

    def test_manifest_pid_command_socket_hash_and_modes(self):
        runtime=self.fixture()
        with patch('integration.scanner.process_fingerprint',return_value=runtime['fingerprint']):
            self.validate(runtime)
            manifest=Path(runtime['manifest']);original=json.loads(manifest.read_text())
            manifest.chmod(0o600)
            with self.assertRaises(ValueError):self.validate(runtime)
            changed=dict(original,pid=99999);manifest.write_text(json.dumps(changed));manifest.chmod(0o400)
            with self.assertRaises(ValueError):self.validate(runtime)
            manifest.chmod(0o600);manifest.write_text(json.dumps(original));manifest.chmod(0o400)
            Path(runtime['socket']).chmod(0o666)
            with self.assertRaises(ValueError):self.validate(runtime)

    def test_definition_changes_after_start_refused(self):
        runtime=self.fixture()
        with patch('integration.scanner.process_fingerprint',return_value=runtime['fingerprint']):
            self.validate(runtime)
            (Path(runtime['definitions'])/'daily.cvd').write_bytes(b'changed after launch')
            with self.assertRaises(ValueError):self.validate(runtime)

    def test_browser_rejects_mismatched_version_and_cleans_its_processes(self):
        self._browser_smoke(mismatch=True)

    def test_browser_closes_private_profile_without_host_home(self):
        self._browser_smoke(mismatch=False)

    def _browser_smoke(self,mismatch):
        try:
            from integration.scanner import probe_browser
        except ImportError:self.fail('Task5 browser interface is not implemented')
        from integration.model import FixtureBundle,Toolchain
        from integration.registry import Registry
        runtime=Path('/Users/leo.cui/.cache/codex-runtimes/codex-primary-runtime/dependencies/node')
        tools=Toolchain({'node':snapshot_binary(runtime/'bin/node',self.root/'node'),'node_modules':runtime/'node_modules','chrome':Path('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome')},
                        {key:value for key,value in _lock(Path(__file__).resolve().parents[2]/'testdata/project-integration/versions.lock').items() if key in {'playwright','chrome'}},{},{},'darwin','arm64')
        if mismatch:tools.versions['chrome']='0.0.0.0'
        registry=Registry(self.root/'registry.jsonl','b'*32,'c'*40)
        bundle=FixtureBundle({},set(),registry.path,{'b'*32},{'source_commit':'c'*40})
        with patch.dict(os.environ,{'HOME':'/foreign','USERPROFILE':'/foreign'}):
            event=probe_browser(bundle,tools,registry,self.root/('b'*32),time.monotonic()+45)
        if mismatch:
            self.assertEqual(event.exit_code,1)
            self.assertNotIn('browser_version_verified',event.checks)
            self.assertTrue(registry.cleanup(time.monotonic()+10).removed)
            return
        self.assertEqual(event.exit_code,0,event.failures)
        self.assertIn('browser_launched',event.checks)
        self.assertIn('browser_closed',event.checks)
        self.assertFalse(any('profile' in path.name for path in (self.root/('b'*32)).iterdir()))
        rows=registry.records();self.assertTrue(any(r['event']=='registered' and r['kind']=='process' for r in rows))
        self.assertTrue(registry.cleanup(time.monotonic()+10).removed)

if __name__=='__main__':unittest.main()
