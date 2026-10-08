"""Source isolation rejects dirty modules, unsafe archives, and missing inventory."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import io
import tarfile
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
ENTRY=Path(__file__).resolve().parents[1]/'test-project-integration.py'


class SourceTests(unittest.TestCase):
    def setUp(self):
        if not ENTRY.is_file(): self.fail('Task 2 bootstrap is not implemented')
        spec=importlib.util.spec_from_file_location('bootstrap_under_test',ENTRY)
        self.entry=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.entry)
        try:
            from integration import source, tools
        except ImportError: self.fail('Task 2 source/tools interfaces are not implemented')
        self.source,self.tools=source,tools
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();self.repo=self.root/'repo';self.repo.mkdir()
        self.git('init','-q')
        (self.repo/'scripts/integration').mkdir(parents=True)
        shutil.copyfile(ENTRY,self.repo/'scripts/test-project-integration.py')
        (self.repo/'scripts/integration/__init__.py').write_text('')
        (self.repo/'scripts/integration/probe.py').write_text("MARKER='committed'\n")
        (self.repo/'scripts/integration/run.py').write_text("from .probe import MARKER\ndef execute_bootstrap(data, output):\n    (output/'marker').write_text(MARKER)\n    return 0\n")
        self.sha=self.commit()

    def git(self,*args):
        return subprocess.check_output(['git','-C',str(self.repo),*args],stderr=subprocess.DEVNULL).decode().strip()

    def commit(self):
        self.git('add','.')
        self.git('-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-qm','fixture')
        return self.git('rev-parse','HEAD')

    def test_dirty_module_never_executes(self):
        (self.repo/'scripts/integration/probe.py').write_text("MARKER='dirty'\n")
        data=self.entry.bootstrap(self.repo,self.sha,self.root/'out')
        runner=self.entry.load_archived_runner(data)
        self.assertEqual(runner(data,self.root/'out'),0)
        self.assertEqual((self.root/'out/marker').read_text(),'committed')
        snapshot=self.source.verify_snapshot(data)
        self.assertEqual(snapshot.commit,self.sha)
        (snapshot.root/'scripts/integration/probe.py').write_text("MARKER='tampered'\n")
        with self.assertRaises(ValueError): self.source.verify_snapshot(data)

    def test_git_replace_cannot_change_fixed_source(self):
        (self.repo/'scripts/integration/probe.py').write_text("MARKER='replacement'\n")
        replacement=self.commit()
        self.git('replace',self.sha,replacement)
        out=self.root/'replaced'
        data=self.entry.bootstrap(self.repo,self.sha,out)
        self.entry.load_archived_runner(data)(data,out)
        self.assertEqual((out/'marker').read_text(),'committed')

    def test_archive_link_traversal_and_output_reuse(self):
        (self.repo/'unsafe').symlink_to('/etc/passwd')
        bad=self.commit()
        with self.assertRaises(ValueError): self.entry.bootstrap(self.repo,bad,self.root/'linked')
        out=self.root/'exists';out.mkdir()
        with self.assertRaises(ValueError): self.entry.bootstrap(self.repo,self.sha,out)
        link=self.root/'out-link';link.symlink_to(out,target_is_directory=True)
        with self.assertRaises(ValueError): self.entry.bootstrap(self.repo,self.sha,link/'child')
        for unsafe in ('../escape', '/absolute', 'safe/../../escape'):
            archive=self.root/'hostile.tar'
            with tarfile.open(archive,'w') as tar:
                info=tarfile.TarInfo(unsafe);info.size=1
                tar.addfile(info,io.BytesIO(b'x'))
            with self.assertRaises(ValueError):
                self.entry.extract_archive(archive,self.root/'extracted')
        for sha in ('HEAD',self.sha[:8],'f'*40):
            with self.assertRaises(ValueError): self.entry.bootstrap(self.repo,sha,self.root/('invalid-'+sha[:8]))

    def test_entry_digest_and_wrong_repository(self):
        (self.repo/'scripts/test-project-integration.py').write_text('# altered entry\n')
        bad=self.commit()
        with self.assertRaises(ValueError): self.entry.bootstrap(self.repo,bad,self.root/'different')
        with self.assertRaises(ValueError): self.entry.bootstrap(self.repo/'scripts',self.sha,self.root/'subdir')

    def test_inventory_go_list_and_test_list(self):
        from integration.model import SourceSnapshot,Toolchain
        from integration.environment import test_environment
        go=Path('/opt/homebrew/bin/go')
        self.assertTrue(go.is_file(),'Approved Go tool missing')
        module=self.root/'tiny';module.mkdir()
        (module/'go.mod').write_text('module example.invalid/fixture\n\ngo 1.27\n')
        (module/'sample.go').write_text('package fixture\n')
        (module/'sample_test.go').write_text('package fixture\nimport "testing"\nfunc TestRealList(t *testing.T) {}\n')
        (module/'other_linux_test.go').write_text('//go:build linux\n\npackage fixture\nimport "testing"\nfunc TestLinuxOnly(t *testing.T) {}\n')
        tool=Toolchain({'go':go},{},{},{},'darwin','arm64')
        env=test_environment(tool,self.root/'private',{})
        snap=SourceSnapshot(self.sha,self.repo,module,self.root/'archive','a'*64)
        inv=self.source.collect_inventory(snap,tool,env,None)
        self.assertEqual(inv.packages,{'example.invalid/fixture'})
        self.assertEqual(inv.tests,{'example.invalid/fixture':{'TestRealList'}})
        selected=self.source.collect_inventory(snap,tool,env,{'example.invalid/fixture':'^TestMissing$'})
        self.assertEqual(selected.tests,{'example.invalid/fixture':set()})
        (module/'sample.go').write_text('invalid Go')
        with self.assertRaises(ValueError): self.source.collect_inventory(snap,tool,env,None)

    def test_tampered_entry_is_rejected_before_execution(self):
        data=self.entry.bootstrap(self.repo,self.sha,self.root/'before-exec')
        marker=self.root/'executed-unverified'
        entry=Path(data['root'])/'scripts/test-project-integration.py'
        entry.write_text("from pathlib import Path\nPath("+repr(str(marker))+").write_text('bad')\n"+entry.read_text())
        with self.assertRaises(ValueError): self.source.verify_snapshot(data)
        self.assertFalse(marker.exists(),'tampered source executed before verification')

    def test_freshclam_version_does_not_require_shared_config(self):
        self.assertTrue(hasattr(self.tools,'probe_version'),'version probe interface missing')
        self.assertEqual(self.tools.probe_version(Path('/opt/homebrew/bin/freshclam'),
            'freshclam','1.5.4'),'1.5.4')

    def test_docker_multicall_path_preserves_cli_name(self):
        self.assertTrue(hasattr(self.tools,'docker_cli'),'Docker CLI discovery interface missing')
        cli=self.tools.docker_cli()
        result=subprocess.run([str(cli),'--version'],capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('Docker version',result.stdout)
        self.assertEqual(cli.name,'docker')

    def test_tool_digest_mismatch_and_missing_mc(self):
        # A local executable must not bypass its publisher digest.
        target=self.root/'mc';target.write_bytes(b'not-the-pinned-tool')
        with self.assertRaises(ValueError): self.tools.verify_digest(target,'e745d9866fc40ff7cf876abeb28e05e153a8cfeba601bcc8daa6e124b81384c5')
        with self.assertRaises(ValueError): self.tools.verify_digest(self.root/'missing','a'*64)


if __name__=='__main__':unittest.main()
