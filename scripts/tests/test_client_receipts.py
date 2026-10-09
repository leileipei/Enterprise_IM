"""Synthetic acceptance rejection tests, never feature integration evidence."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

ENTRY=Path(__file__).resolve().parents[1]/'test-client-receipts.py'
COMMIT='a'*40

class ClientReceiptContractTests(unittest.TestCase):
    def setUp(self):
        if not ENTRY.is_file():self.fail('receipt acceptance runner not implemented')
        spec=importlib.util.spec_from_file_location('receipt_runner_under_test',ENTRY)
        self.runner=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.runner)
        self.data={'source_commit':COMMIT,'required_gates_passed':True,'full_suite_passed':True,'race_suite_passed':True,'failures':[], 'cleanup':{'removed':True,'failures':[]},'tools':{'versions':{k:'locked' for k in ('go','node','chrome','playwright')},'hashes':{k:'b'*64 for k in ('go','node','chrome','playwright_package')}},'events':[{'name':'fixture_preflight','exit_code':0,'checks':['postgres_actual_tls','redis_ping','actual_iam_permission_matrix'],'failures':[]}]}
        for gate in ('full_repository','race_repository'):
            self.data['events'].append({'name':gate,'exit_code':0,'failures':[],'counts':{'ordinary_skip':0},'passed':sorted(self.runner.REQUIRED_TESTS),'executed':sorted(self.runner.REQUIRED_TESTS)})
    def reject(self,data,code=0):
        with self.assertRaises(ValueError):self.runner.validate_claims(data,COMMIT,code)
    def test_complete_protocol_claims(self):
        self.runner.validate_claims(self.data,COMMIT,0)
    def test_missing_database_redis_or_tools(self):
        for field in ('postgres_actual_tls','redis_ping','actual_iam_permission_matrix'):
            data=copy.deepcopy(self.data);data['events'][0]['checks'].remove(field);self.reject(data)
        for field in ('go','node','chrome','playwright'):
            data=copy.deepcopy(self.data);del data['tools']['versions'][field];self.reject(data)
    def test_actual_command_nonzero_or_uncompleted(self):
        for code in (1,137,None,False):self.reject(self.data,code)
        for index in (1,2):
            data=copy.deepcopy(self.data);data['events'][index]['exit_code']=1;self.reject(data)
            data=copy.deepcopy(self.data);data['events'][index]['passed'].pop();self.reject(data)
    def test_cleanup_false_and_failure_override_green(self):
        for cleanup in ({'removed':False,'failures':[]},{'removed':True,'failures':['retained_child']},{}):
            data=copy.deepcopy(self.data);data['cleanup']=cleanup;self.reject(data)
    def test_missing_gate_skip_and_wrong_source(self):
        data=copy.deepcopy(self.data);data['source_commit']='c'*40;self.reject(data)
        data=copy.deepcopy(self.data);data['events'].pop();self.reject(data)
        data=copy.deepcopy(self.data);data['events'][1]['counts']['ordinary_skip']=1;self.reject(data)
        data=copy.deepcopy(self.data);data['events'].append(copy.deepcopy(data['events'][1]));self.reject(data)
    def test_output_failure_cannot_publish_success(self):
        with tempfile.TemporaryDirectory() as root:
            path=Path(root)/'receipt-results.json'
            with mock.patch.object(self.runner.os,'replace',side_effect=OSError('disk full')):
                with self.assertRaises(OSError):self.runner.write_summary(path,{'overall_passed':True})
            self.assertFalse(path.exists())
    def test_resource_missing_exit_or_mismatch(self):
        row={'schema_version':1,'source_commit':COMMIT,'owner':'d'*32,'kind':'process','identity':'12','event':'registered','fingerprint':{'start_time':'fixed','executable_sha256':'e'*64},'detail':{'gate':'full_repository','test':'TestTwoRealBrowsersDeliveryReadAndRecovery'}}
        exited={**row,'event':'exited','detail':{**row['detail'],'expected_exit':'exit:0','actual_exit':'exit:0'}}
        self.runner.validate_child_exits([row,exited],COMMIT)
        with self.assertRaises(ValueError):self.runner.validate_child_exits([row],COMMIT)
        bad=copy.deepcopy(exited);bad['detail']['actual_exit']='exit:1'
        with self.assertRaises(ValueError):self.runner.validate_child_exits([row,bad],COMMIT)

if __name__=='__main__':unittest.main()
