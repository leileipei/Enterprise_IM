"""Explicit fixture values cannot import parent or startup-only variables."""
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))


class EnvironmentTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration.environment import test_environment
            from integration.model import Toolchain
        except ImportError: self.fail('Task 2 environment interface is not implemented')
        self.build=test_environment
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve()
        self.tools=Toolchain({'go':Path('/opt/homebrew/bin/go')},{},{},{},'darwin','arm64')

    def test_environment_excludes_home_pg_and_child_flags(self):
        hostile={'HOME':'/foreign','PGPASSWORD':'parent-secret','GODEBUG':'override',
                 'IM_TEST_REALTIME_API_CHILD':'1','IM_APPEND_CHILD_SCHEMA':'foreign',
                 'IM_TEST_DATABASE_URL':'foreign-database','AWS_SECRET_ACCESS_KEY':'secret'}
        with patch.dict(os.environ,hostile):
            env=self.build(self.tools,self.root/'private',{'IM_TEST_DATABASE_URL':'owned-database'})
        for key in hostile:
            if key!='IM_TEST_DATABASE_URL': self.assertNotIn(key,env)
        self.assertEqual(env['IM_TEST_DATABASE_URL'],'owned-database')
        self.assertEqual(env['GOWORK'],'off')
        self.assertEqual(env['GOFLAGS'],'-mod=readonly -buildvcs=false')
        self.assertTrue(Path(env['GOCACHE']).is_relative_to(self.root))
        self.assertTrue(Path(env['GOMODCACHE']).is_relative_to(self.root))
        for key in ('HOME','USERPROFILE','PGPASSWORD','GODEBUG','IM_TEST_REALTIME_API_CHILD',
                    'IM_APPEND_CHILD_SCHEMA','IM_TEST_INVENTED','GOFLAGS','IM_DATABASE_URL'):
            with self.subTest(key=key),self.assertRaises(ValueError):
                self.build(self.tools,self.root/'private',{key:'injected'})


if __name__=='__main__':unittest.main()
