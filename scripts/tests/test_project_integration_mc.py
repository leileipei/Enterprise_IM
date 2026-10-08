"""Pinned source extraction must reject corruption before any build."""
import hashlib
import io
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))

class MCSourceTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration.mc import extract_source, build_parameters
        except ImportError:
            self.fail('Pinned mc rebuild interface is not implemented')
        self.extract=extract_source; self.parameters=build_parameters
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve()
        self.commit='6ac18619cf881074fe6edcc79ab62c9c85da60b9'

    def archive(self,name=None,link=False):
        path=self.root/'source.tar.gz'
        with tarfile.open(path,'w:gz') as t:
            m=tarfile.TarInfo(name or 'mc-'+self.commit+'/go.mod')
            if link:
                m.type=tarfile.SYMTYPE;m.linkname='/foreign'
                t.addfile(m)
            else:
                data=b'module github.com/minio/mc\ngo 1.22\n'
                m.size=len(data);t.addfile(m,io.BytesIO(data))
        return path,hashlib.sha256(path.read_bytes()).hexdigest()

    def test_extracts_only_verified_source_to_fresh_directory(self):
        path,digest=self.archive()
        self.extract(path,self.root/'source',self.commit,digest)
        self.assertTrue((self.root/'source/go.mod').is_file())
        with self.assertRaises(ValueError):
            self.extract(path,self.root/'source',self.commit,digest)

    def test_corrupt_source_fails_before_destination_exists(self):
        path,digest=self.archive();path.write_bytes(path.read_bytes()+b'tampered')
        with self.assertRaisesRegex(ValueError,'tool_hash_mismatch'):
            self.extract(path,self.root/'source',self.commit,digest)
        self.assertFalse((self.root/'source').exists())

    def test_traversal_links_and_wrong_commit_root_rejected(self):
        for name,link in [('mc-'+self.commit+'/../../escaped',False),
                          ('mc-'+self.commit+'/go.mod',True),
                          ('mc-'+'a'*40+'/go.mod',False)]:
            with self.subTest(name=name,link=link):
                path,digest=self.archive(name,link)
                with self.assertRaises(ValueError):
                    self.extract(path,self.root/'source',self.commit,digest)
                self.assertFalse((self.root/'source').exists())

    def test_build_uses_fixed_metadata_and_private_environment(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ,{'HOME':'foreign','GOFLAGS':'-race','GOTOOLCHAIN':'auto','MC_HOTFIX':'foreign'}):
            argv,env=self.parameters(Path('/opt/homebrew/bin/go'),self.root,self.commit,
                                     'RELEASE.2024-11-05T11-29-45Z')
        self.assertEqual(env['GOTOOLCHAIN'],'local')
        self.assertEqual(env['CGO_ENABLED'],'0')
        self.assertNotIn('HOME',env)
        self.assertNotIn('MC_HOTFIX',env)
        self.assertNotIn('-race',argv)
        self.assertIn('-mod=readonly',argv)
        self.assertIn('-trimpath',argv)
        self.assertIn('-buildvcs=false',argv)
        flags=argv[argv.index('-ldflags')+1]
        self.assertIn('cmd.CommitID='+self.commit,flags)
        self.assertIn('cmd.ReleaseTag=RELEASE.2024-11-05T11-29-45Z',flags)
        self.assertIn('cmd.Version=2024-11-05T11:29:45Z',flags)

class MCBinaryTests(unittest.TestCase):
    def setUp(self):
        from integration.mc import provision_mc
        self.provision=provision_mc
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve()
        self.pins={'mc-test-client-sha256-darwin-arm64':'a'*64,
                   'mc-source-commit':'6ac18619cf881074fe6edcc79ab62c9c85da60b9',
                   'mc-test-client':'RELEASE.2024-11-05T11-29-45Z'}

    def test_wrong_hash_is_never_executed(self):
        binary=self.root/'mc';marker=self.root/'executed'
        binary.write_text('#!/bin/sh\n/usr/bin/touch '+str(marker)+'\n');binary.chmod(0o700)
        with self.assertRaisesRegex(ValueError,'tool_hash_mismatch'):
            self.provision(self.root,self.pins,Path('/unused/go'))
        self.assertFalse(marker.exists())

    def test_linked_binary_rejected_without_mutating_foreign_file(self):
        foreign=self.root/'foreign';foreign.write_text('foreign');foreign.chmod(0o400)
        private=self.root/'private';private.mkdir(mode=0o700)
        (private/'mc').symlink_to(foreign)
        self.pins['mc-test-client-sha256-darwin-arm64']=hashlib.sha256(foreign.read_bytes()).hexdigest()
        try:
            self.provision(private,self.pins,Path('/unused/go'))
        except Exception as exc:
            self.assertIsInstance(exc,ValueError,'must reject the link before executing it')
            self.assertEqual(str(exc),'mc_binary_link_rejected')
        else:
            self.fail('linked mc binary was accepted')
        self.assertEqual(foreign.stat().st_mode&0o777,0o400)

    def test_metadata_mismatch_is_rejected_even_with_matching_hash(self):
        binary=self.root/'mc';binary.write_text('#!/bin/sh\necho mc version DEVELOPMENT.GOGET\n');binary.chmod(0o700)
        self.pins['mc-test-client-sha256-darwin-arm64']=hashlib.sha256(binary.read_bytes()).hexdigest()
        with self.assertRaisesRegex(ValueError,'mc_build_metadata_mismatch'):
            self.provision(self.root,self.pins,Path('/unused/go'))

if __name__=='__main__':unittest.main()
