"""Service plans must describe isolated, owned and local resources."""
import json
from pathlib import Path
import sys
import tempfile
import unittest
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))

class ServicePlanTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration.services import service_plan
            from integration.iam import iam_policy, product_credentials
        except ImportError:self.fail('Task 4 service/IAM interfaces are not implemented')
        self.plan=service_plan;self.policy=iam_policy;self.credentials=product_credentials
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve()
        self.images={k:{'reference':k+'@sha256:'+'a'*64} for k in ('postgres-image','redis-image','minio-image')}
        self.owner='b'*32;self.commit='c'*40

    def test_service_labels_loopback_db_and_roles(self):
        plans=self.plan(self.images,self.root,self.owner,self.commit)
        self.assertEqual(set(plans),{'postgres','redis','minio'})
        for role,plan in plans.items():
            argv=plan['argv'];self.assertIn('im.integration.owner='+self.owner,argv)
            self.assertIn('im.integration.source='+self.commit,argv)
            self.assertIn('127.0.0.1::'+plan['port'],argv)
            self.assertNotIn('--network=host',argv)
            self.assertTrue(any(v.startswith('/data:') or v.startswith('/var/lib/postgresql/data:') for v in argv) or role=='redis')
        pg=plans['postgres'];self.assertIn('ssl=on',pg['argv'])
        self.assertEqual(pg['database'],'enterprise_im_files')
        self.assertEqual(pg['product_roles'],['p431_api','p431_repair','p431_import_writer'])

    def test_four_iam_principals_and_no_admin_child_env(self):
        keys={role:(role+'-key',role+'-secret') for role in ('upload','worker','download','cleanup','bootstrap','admin')}
        env=self.credentials(keys)
        self.assertEqual(env['IM_TEST_FILE_UPLOAD_ACCESS_KEY'],'upload-key')
        self.assertEqual(env['IM_FILE_CLEANUP_S3_ACCESS_KEY'],'cleanup-key')
        self.assertFalse(any('ADMIN' in k for k in env))
        self.assertFalse(any('admin-' in v for v in env.values()))
        bucket='p426-p431-'+self.owner;negative=bucket+'-policy'
        policies={r:self.policy(r,bucket,negative) for r in ('upload','worker','download','cleanup')}
        allactions=lambda p:{a for s in p['Statement'] for a in s['Action']}
        self.assertNotIn('s3:PutObject',allactions(policies['worker']))
        self.assertNotIn('s3:PutObject',allactions(policies['download']))
        self.assertNotIn('s3:DeleteObject',allactions(policies['cleanup']))
        self.assertNotIn('s3:ListBucketVersions',allactions(policies['download']))
        self.assertIn('s3:DeleteObjectVersion',allactions(policies['cleanup']))
        statement=next(s for s in policies['cleanup']['Statement'] if 's3:DeleteObjectVersion' in s['Action'])
        self.assertEqual(statement['Condition']['Null']['s3:versionid'],'false')
        self.assertEqual(statement['Condition']['StringNotEquals']['s3:versionid'],['','null'])
        self.assertNotIn('arn:aws:s3:::*',json.dumps(policies))

    def test_probe_version_not_created_by_api(self):
        p=self.policy('upload','p426-p431-'+self.owner,'p426-p431-'+self.owner+'-policy')
        for s in p['Statement']:
            if 's3:PutObject' in s['Action']:
                self.assertTrue(all('/files/*' in resource for resource in s['Resource']))
        p=self.policy('bootstrap','p426-p431-'+self.owner,'p426-p431-'+self.owner+'-policy')
        self.assertIn('/_im_runtime/read-probe/v1',json.dumps(p))

class ServiceFailureTests(unittest.TestCase):
    def test_failed_preflight_cleans_created_resources(self):
        from integration.services import prepare_services
        from integration.registry import Registry
        from integration.model import SourceSnapshot,Toolchain
        from unittest.mock import patch
        import time
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp).resolve();root.chmod(0o700)
            owner='b'*32;commit='c'*40
            registry=Registry(root/'registry.jsonl',owner,commit)
            snapshot=SourceSnapshot(commit,root,root,root/'unused','a'*64)
            tools=Toolchain({'docker':Path('/unused/docker')},{},{},
                {k:{'reference':k+'@sha256:'+'a'*64} for k in ('postgres-image','redis-image','minio-image')},'darwin','arm64')
            cid='d'*64;present={'value':False}
            def command(_tools,args,deadline,input=None):
                if args[0]=='create':present['value']=True;return cid+'\n'
                raise ValueError('injected_service_start_failure')
            def docker(args):
                if args[0]=='ps':return [cid] if present['value'] else []
                if args[0]=='inspect':
                    return {'Id':cid,'Config':{'Labels':{'im.integration.owner':owner,'im.integration.source':commit}},'State':{'Status':'created'}} if present['value'] else None
                if args[0]=='rm':present['value']=False;return None
                self.fail('unexpected cleanup command')
            with patch('integration.services._generate_tls'),patch('integration.services.docker_environment',return_value={}),patch('integration.services._docker',side_effect=command),patch.object(registry,'docker',side_effect=docker):
                with self.assertRaisesRegex(ValueError,'injected_service_start_failure'):
                    prepare_services(snapshot,tools,registry,root/owner/'services',time.monotonic()+30)
            self.assertFalse(present['value'],'created container leaked after preparation failure')
            self.assertTrue(any(r['event']=='registered' and r['identity']==cid for r in registry.records()))

    def test_tls_serial_stays_inside_private_root_with_dot_in_path(self):
        from integration.services import _generate_tls
        from integration.model import Toolchain
        import time
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp).resolve();private=root/'owner.with-dot';private.mkdir(mode=0o700)
            tools=Toolchain({'openssl':Path('/usr/bin/openssl')},{},{},{},'darwin','arm64')
            _generate_tls(tools,private,time.monotonic()+30)
            self.assertTrue((private/'tls/ca.srl').is_file(),'LibreSSL serial escaped the private TLS directory')
            self.assertFalse((root/'owner.srl').exists(),'serial created outside the owned TLS root')

    def test_foreign_source_rejected_before_creating_resources(self):
        from integration.services import prepare_services
        from integration.registry import Registry
        from integration.model import SourceSnapshot,Toolchain
        from unittest.mock import patch
        import time
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp).resolve();root.chmod(0o700)
            registry=Registry(root/'registry.jsonl','b'*32,'c'*40)
            snapshot=SourceSnapshot('d'*40,root,root,root/'unused','a'*64)
            tools=Toolchain({}, {}, {}, {},'darwin','arm64')
            with patch('integration.services._generate_tls'),patch('integration.services.docker_environment',return_value={}):
                try:prepare_services(snapshot,tools,registry,root/registry.owner/'services',time.monotonic()+30)
                except Exception as exc:
                    self.assertIsInstance(exc,ValueError)
                    self.assertEqual(str(exc),'service_source_mismatch')
                else:self.fail('foreign source was accepted')
            self.assertEqual([r['event'] for r in registry.records()],['reserve'])

    def test_cleanup_docker_ignores_ambient_remote_context(self):
        from integration.registry import Registry
        from unittest.mock import patch
        import os
        import subprocess
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp).resolve();root.chmod(0o700)
            registry=Registry(root/'registry.jsonl','b'*32,'c'*40)
            with patch.dict(os.environ,{'DOCKER_HOST':'tcp://foreign:2375','HOME':'/foreign'}),patch('integration.registry.subprocess.run',return_value=subprocess.CompletedProcess([],0,'','')) as run:
                registry.docker(['ps','-a','--format','{{.ID}}'])
            env=run.call_args.kwargs.get('env',{})
            self.assertEqual(env.get('DOCKER_HOST'),'unix:///Users/leo.cui/.orbstack/run/docker.sock')
            self.assertNotIn('HOME',env)

if __name__=='__main__':unittest.main()
