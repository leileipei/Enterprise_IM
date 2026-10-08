"""Catch false acceptance from missing, malformed, or mismatched evidence."""
import copy
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from support import COMMIT, PACKAGE, HELPER, CALLER, go_rows, write_json, helper_proofs


class ResultsTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration import model, results
        except ImportError:
            self.fail('Task 1 model/results interfaces are not implemented')
        self.m, self.r = model, results
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def inventory(self, names=('TestA',), sub=()):
        return self.m.Inventory({PACKAGE}, {PACKAGE: set(names)}, set(sub))

    def event(self, tests=(('TestA', 'pass'),), name='full_repository', rc=0):
        path = write_json(self.root / (name + '.jsonl'), go_rows(PACKAGE, tests))
        return self.r.parse_go(path, name, COMMIT, rc)

    def greens(self):
        rows = []
        for name in self.r.REQUIRED_GATES:
            if name in {'toolchain', 'fixture_preflight', 'build_all', 'vet_all',
                        'evidence_integrity', 'resource_cleanup'}:
                rows.append(self.m.GateEvent(name, 'check', COMMIT, 0,
                    self.root / name, checks=['verified']))
            else:
                event = self.event(name=name)
                event.inventory = self.inventory()
                rows.append(event)
        return rows

    def test_adapter_error_survives_missing_inventory(self):
        event=self.m.GateEvent("file_components","test",COMMIT,1,self.root/"missing",failures=["stage_exception:UnboundLocalError"])
        verdict=self.r.evaluate([event],self.m.CleanupResult(True,[]),COMMIT)
        self.assertIn("file_components:stage_exception:UnboundLocalError",verdict.failures)
        self.assertIn("file_components:missing_discovered_inventory",verdict.failures)

    def test_zero_missing_subtest_nonzero_exit(self):
        # Removing coverage or trusting PASS despite exit 7 must reject.
        self.assertEqual(self.r.validate_gate(self.event(), self.inventory(), None), [])
        for event, inv in [(self.event(()), self.inventory()),
                           (self.event(), self.inventory(('TestA', 'TestB'))),
                           (self.event(), self.inventory(sub=(PACKAGE+'::TestA/required',))),
                           (self.event(rc=7), self.inventory()),
                           (self.event((('TestA','fail'),)), self.inventory()),
                           (self.event((('TestA','skip'),)), self.inventory())]:
            with self.subTest(event=event.name, inv=inv):
                self.assertTrue(self.r.validate_gate(event, inv, None))
        event=self.event((('TestA/required','pass'), ('TestA','pass')))
        self.assertEqual(self.r.validate_gate(event,
            self.inventory(sub=(PACKAGE+'::TestA/required',)), None), [])
        self.assertEqual(event.counts['top_pass'], 1)
        self.assertEqual(event.counts['sub_pass'], 1)
        self.assertEqual(event.counts['package_pass'], 1)

    def test_truncated_json_and_missing_package(self):
        event=self.event()
        event.log.write_text(event.log.read_text() + '{"Action":')
        broken=self.r.parse_go(event.log, event.name, COMMIT, 0)
        self.assertTrue(self.r.validate_gate(broken, self.inventory(), None))
        inv=self.inventory()
        inv.packages.add('example/other')
        inv.tests['example/other']={'TestOther'}
        self.assertTrue(self.r.validate_gate(self.event(), inv, None))
        rows=go_rows(PACKAGE, [('TestA','pass')])[:-1]
        path=write_json(self.root/'early', rows)
        self.assertTrue(self.r.validate_gate(
            self.r.parse_go(path, 'full_repository', COMMIT, 0), self.inventory(), None))

    def test_duplicate_source_hash_failure(self):
        # A green report cannot override a duplicate gate, changed source or hash failure.
        clean=self.greens()
        self.assertTrue(self.r.evaluate(clean, self.m.CleanupResult(True, []), COMMIT).required_gates_passed)
        cases=[clean[:-1], clean+[copy.deepcopy(clean[0])]]
        wrong=copy.deepcopy(clean); wrong[0].source_commit='d'*40; cases.append(wrong)
        bad=copy.deepcopy(clean)
        next(e for e in bad if e.name=='evidence_integrity').failures=['archive_hash_mismatch']
        cases.append(bad)
        for rows in cases:
            self.assertFalse(self.r.evaluate(rows, self.m.CleanupResult(True, []), COMMIT).required_gates_passed)
        self.assertFalse(self.r.evaluate(clean, self.m.CleanupResult(False, ['owned_pid_alive']), COMMIT).required_gates_passed)
        bad=copy.deepcopy(clean); bad[0].inventory=None
        self.assertFalse(self.r.evaluate(bad, self.m.CleanupResult(True, []), COMMIT).required_gates_passed)

    def test_helper_requires_parent_and_two_processes(self):
        tests=(('TestMultiProcessRealtimeWorkerFanoutAndReconnect','pass'), ('TestRealtimeAPIChild','skip'))
        inv=self.inventory(tuple(t for t,_ in tests))
        event=self.event(tests)
        self.assertTrue(self.r.validate_gate(event, inv, HELPER))
        event.helper_proofs=helper_proofs(event.name)
        self.assertEqual(self.r.validate_gate(event, inv, HELPER), [])
        self.assertEqual(event.counts['top_skip'], 1)
        self.assertEqual(event.counts['helper_skip'], 1)
        self.assertEqual(event.counts['ordinary_skip'], 0)
        for change in ('one', 'same_pid', 'source', 'not_ready', 'exit', 'gate', 'caller', 'no_owner'):
            bad=copy.deepcopy(event)
            if change=='one': bad.helper_proofs=bad.helper_proofs[:1]
            elif change=='same_pid': bad.helper_proofs[1]['pid']=101
            elif change=='source': bad.helper_proofs[0]['source_commit']='d'*40
            elif change=='not_ready': bad.helper_proofs[0]['ready']=False
            elif change=='exit': bad.helper_proofs[0]['actual_exit']='exit:1'
            elif change=='gate': bad.helper_proofs[0]['gate']='file_messages'
            elif change=='caller': bad.passed.discard(CALLER)
            else: bad.helper_proofs[0]['owner']=''
            with self.subTest(change=change):
                self.assertTrue(self.r.validate_gate(bad, inv, HELPER))
        event.name='file_messages'
        self.assertTrue(self.r.validate_gate(event, inv, HELPER))
        event.name='full_repository'
        self.assertTrue(self.r.validate_gate(event, inv, PACKAGE+'::OtherHelper'))

    def test_full_and_race_are_independent(self):
        for name, want in [('full_repository',(False,True)), ('race_repository',(True,False))]:
            rows=self.greens()
            next(e for e in rows if e.name==name).exit_code=1
            v=self.r.evaluate(rows, self.m.CleanupResult(True, []), COMMIT)
            self.assertEqual((v.full_suite_passed,v.race_suite_passed),want)
            self.assertFalse(v.required_gates_passed)

    def test_unittest_missing_fail_skip_and_zero_match(self):
        testid='suite.Cases.test_ready'
        inv=self.m.Inventory({'unittest'}, {'unittest':{testid}}, set())
        path=self.root/'python.log'
        base='test_ready (suite.Cases) ... ok\n\nRan 1 test in 0.001s\n\nOK\n'
        path.write_text(base)
        event=self.r.parse_unittest(path,'orchestrator_contract',COMMIT,0)
        self.assertEqual(self.r.validate_gate(event,inv,None),[])
        for log in [base.replace('ok','FAIL').replace('OK','FAILED (failures=1)'),
                    base.replace('ok',"skipped 'unavailable'").replace('OK','OK (skipped=1)'),
                    base.replace('Ran 1','Ran 2'), base.split('Ran')[0],
                    'Ran 0 tests in 0.001s\n\nOK\n']:
            path.write_text(log)
            event=self.r.parse_unittest(path,'orchestrator_contract',COMMIT,0)
            self.assertTrue(self.r.validate_gate(event,inv,None),log)
        path.write_text(base)
        event=self.r.parse_unittest(path,'orchestrator_contract',COMMIT,0)
        inv.tests['unittest'].add('suite.Cases.test_missing')
        self.assertTrue(self.r.validate_gate(event,inv,None))

    def test_duplicate_terminals_and_race_report_are_rejected(self):
        rows=go_rows(PACKAGE,[('TestA','pass'),('TestA','pass')])
        path=write_json(self.root/'duplicate',rows)
        self.assertTrue(self.r.validate_gate(self.r.parse_go(path,'full_repository',COMMIT,0),self.inventory(),None))
        rows=go_rows(PACKAGE,[('TestA','pass')])
        rows.insert(1,dict(Action='output',Package=PACKAGE,Output='WARNING: DATA RACE\n'))
        path=write_json(self.root/'race',rows)
        self.assertTrue(self.r.validate_gate(self.r.parse_go(path,'race_repository',COMMIT,0),self.inventory(),None))

    def test_no_test_package_requires_independent_inventory(self):
        rows=go_rows(PACKAGE,[('TestA','pass')])
        rows += [dict(Action='start',Package='example/no_tests'),
                 dict(Action='output',Package='example/no_tests',Output='?\texample/no_tests\t[no test files]\n'),
                 dict(Action='skip',Package='example/no_tests')]
        path=write_json(self.root/'no-tests',rows)
        inv=self.inventory();inv.packages.add('example/no_tests');inv.tests['example/no_tests']=set()
        event=self.r.parse_go(path,'full_repository',COMMIT,0)
        self.assertEqual(self.r.validate_gate(event,inv,None),[])
        self.assertEqual(event.counts['ordinary_skip'],0)
        inv.tests['example/no_tests']={'TestHidden'}
        self.assertTrue(self.r.validate_gate(event,inv,None))

    def test_verbose_counts_nested_tests_and_terminal_package(self):
        path=self.root/'verbose'
        path.write_text('=== RUN   TestA\n=== RUN   TestA/required\n--- PASS: TestA (0.00s)\n    --- PASS: TestA/required (0.00s)\nPASS\nok  '+PACKAGE+' 0.003s\n')
        event=self.r.parse_verbose(path,'file_messages',COMMIT,0)
        self.assertEqual(self.r.validate_gate(event,self.inventory(sub=(PACKAGE+'::TestA/required',)),None),[])
        self.assertEqual((event.counts['top_pass'],event.counts['sub_pass']),(1,1))
        path.write_text(path.read_text().replace('ok  '+PACKAGE+' 0.003s\n',''))
        self.assertTrue(self.r.validate_gate(self.r.parse_verbose(path,'file_messages',COMMIT,0),self.inventory(),None))


if __name__=='__main__':
    unittest.main()
