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

    def test_browser_closes_private_profile_without_host_home(self):
        try:
            from integration.scanner import probe_browser
        except ImportError:self.fail('Task5 browser interface is not implemented')
        from integration.model import FixtureBundle,Toolchain
        from integration.registry import Registry
        runtime=Path('/Users/leo.cui/.cache/codex-runtimes/codex-primary-runtime/dependencies/node')
        tools=Toolchain({'node':runtime/'bin/node','node_modules':runtime/'node_modules','chrome':Path('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome')},
                        {'playwright':'1.62.1','chrome':'154.0.8037.98'},{},{},'darwin','arm64')
        registry=Registry(self.root/'registry.jsonl','b'*32,'c'*40)
        bundle=FixtureBundle({},set(),registry.path,{'b'*32},{'source_commit':'c'*40})
        with patch.dict(os.environ,{'HOME':'/foreign','USERPROFILE':'/foreign'}):
            event=probe_browser(bundle,tools,registry,self.root/('b'*32),time.monotonic()+45)
        self.assertEqual(event.exit_code,0,event.failures)
        self.assertIn('browser_launched',event.checks)
        self.assertIn('browser_closed',event.checks)
        self.assertFalse(any('profile' in path.name for path in (self.root/('b'*32)).iterdir()))
        rows=registry.records();self.assertTrue(any(r['event']=='registered' and r['kind']=='process' for r in rows))
        self.assertTrue(registry.cleanup(time.monotonic()+10).removed)

if __name__=='__main__':unittest.main()
