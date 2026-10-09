#!/usr/bin/env python3
"""P5 receipt acceptance using the immutable project integration runner."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys

PREFIX='github.com/leileipei/Enterprise_IM/'
REQUIRED_TESTS={
 PREFIX+'internal/policystore::'+name for name in (
 'TestSyncProofMatchesFinalVisiblePage','TestDeliveredRejectsMissingPageAndForgedProof',
 'TestDeliveredConcurrentReplayProducesOneUserFact','TestDeliveredFiveHundredAndInt64Sequences',
 'TestReadRequiresOwnDeviceDeliveredAndCurrentVisibility','TestReadAuditAndOutboxFailureRollBack',
 'TestReadFiveHundredAndInt64Sequences','TestGroupReceiptSummaryVisibilityAndDedup',
 'TestUnreadUsesVisibleFactsAndBoundedLowerCount','TestUnreadSQLBudgetReturnsOnlyProvedLowerBound',
 'TestReceiptWorkerCrashReplayTwoAPIProcesses','TestExpiredProofCleanupPreservesReceipts',
 'TestTwoRealBrowsersDeliveryReadAndRecovery','TestReceiptContainerDatabaseTLSMapping','TestFileRuntimeDeviceOwnerAuditCompatibility','TestReceiptDeniedOperationsAreAudited','TestReceiptDeniedAuditFailureIsUnavailable','TestSyncZeroCacheHasBoundedOnlineGrant')}
REQUIRED_TESTS.update(PREFIX+'internal/webclient::'+n for n in ('TestWebReceiptPageAndVisibility','TestWebDeviceMemoryAndQuota','TestWebReceiptAppLeaseAndDeviceInvalidation'))
REQUIRED_TESTS.update(PREFIX+'internal/httpserver::'+n for n in ('TestReceiptHTTPUsesApprovedPUTContract','TestReceiptSyncProofAndExpiryAreDistinct','TestReceiptOnlineLeaseIsClampedToJWT'))
PROCESS_TESTS={'TestReceiptWorkerCrashReplayTwoAPIProcesses','TestTwoRealBrowsersDeliveryReadAndRecovery'}


def validate_claims(data,commit,exit_code):
    if type(exit_code) is not int or exit_code!=0:raise ValueError('actual_runner_exit_not_zero')
    if not re.fullmatch('[a-f0-9]{40}',commit) or data.get('source_commit')!=commit:raise ValueError('source_mismatch')
    if not all(data.get(k) is True for k in ('required_gates_passed','full_suite_passed','race_suite_passed')) or data.get('failures'):raise ValueError('required_gates_failed')
    cleanup=data.get('cleanup',{})
    if cleanup.get('removed') is not True or cleanup.get('failures'):raise ValueError('cleanup_not_proven')
    events=data.get('events',[]);names=[e.get('name') for e in events]
    if len(names)!=len(set(names)):raise ValueError('duplicate_gate')
    by_name={e['name']:e for e in events}
    fixture=by_name.get('fixture_preflight',{})
    if fixture.get('exit_code')!=0 or fixture.get('failures') or not {'postgres_actual_tls','redis_ping','actual_iam_permission_matrix'}<=set(fixture.get('checks',[])):raise ValueError('real_database_redis_fixture_missing')
    tools=data.get('tools',{})
    if not all(tools.get('versions',{}).get(k) for k in ('go','node','chrome','playwright')) or not all(re.fullmatch('[a-f0-9]{64}',tools.get('hashes',{}).get(k,'')) for k in ('go','node','chrome','playwright_package')):raise ValueError('locked_tools_missing')
    for name in ('full_repository','race_repository'):
        gate=by_name.get(name,{})
        if gate.get('exit_code')!=0 or gate.get('failures') or gate.get('counts',{}).get('ordinary_skip')!=0:raise ValueError('full_gate_not_passed:'+name)
        if not REQUIRED_TESTS<=set(gate.get('passed',[])) or not REQUIRED_TESTS<=set(gate.get('executed',[])):raise ValueError('receipt_test_terminal_missing:'+name)


def validate_child_exits(records,commit):
    for row in records:
        if row.get('source_commit')!=commit:raise ValueError('foreign_resource_source')
        if row.get('event')!='registered' or row.get('kind') not in {'process','container'} or row.get('detail',{}).get('test') not in PROCESS_TESTS:continue
        def matches(other):
            return all(other.get(k)==row.get(k) for k in ('source_commit','owner','kind','identity','fingerprint')) and all(other.get('detail',{}).get(k)==row.get('detail',{}).get(k) for k in ('gate','test'))
        exits=[r for r in records if r.get('event')=='exited' and matches(r)]
        if len(exits)!=1:raise ValueError('child_exit_receipt_missing')
        detail=exits[0]['detail']
        if not detail.get('expected_exit') or detail.get('actual_exit')!=detail['expected_exit']:raise ValueError('child_actual_exit_mismatch')


def write_summary(path,data):
    path=Path(path);temporary=path.with_name(path.name+'.new')
    fd=os.open(temporary,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    try:
        with os.fdopen(fd,'w') as stream:json.dump(data,stream,ensure_ascii=False,indent=2);stream.write('\n');stream.flush();os.fsync(stream.fileno())
        os.replace(temporary,path)
    finally:
        if temporary.exists():temporary.unlink()


def load_file(name,path):
    spec=importlib.util.spec_from_file_location(name,path);module=importlib.util.module_from_spec(spec)
    previous=sys.dont_write_bytecode;sys.dont_write_bytecode=True
    try:spec.loader.exec_module(module)
    finally:sys.dont_write_bytecode=previous
    return module


def run(commit,output):
    here=Path(__file__).resolve();repository=here.parents[1]
    bootstrap=load_file('_receipt_immutable_bootstrap',repository/'scripts/test-project-integration.py')
    data=bootstrap.bootstrap(repository,commit,output)
    archived_entry=Path(data['root'])/'scripts/test-client-receipts.py'
    if not archived_entry.is_file() or bootstrap.digest(archived_entry)!=bootstrap.digest(here):raise ValueError('receipt_entry_hash_mismatch')
    archived=load_file('_receipt_archived_acceptance',archived_entry)
    runner=bootstrap.load_archived_runner(data)
    actual_exit=runner(data,output)
    report=Path(output)/'verification.json';payload=json.loads(report.read_text())
    failures=[]
    try:
        archived.validate_claims(payload,commit,actual_exit)
        archived.validate_child_exits(payload.get('resource_records',[]),commit)
        package=runner.__module__.rsplit('.',1)[0]
        evidence=__import__(package+'.evidence',fromlist=['validate_delivery'])
        evidence.validate_delivery(commit,report,repository)
        for gate in ('full_repository','race_repository'):
            resources=[r for r in payload.get('resource_records',[]) if r.get('event')=='registered' and r.get('detail',{}).get('gate')==gate and r.get('detail',{}).get('test')=='TestReceiptWorkerCrashReplayTwoAPIProcesses' and r.get('kind')=='container']
            if len(resources)!=4:raise ValueError('two_api_crash_recovery_resources_missing:'+gate)
    except (ValueError,OSError,KeyError,TypeError):failures.append('receipt_evidence_validation_failed')
    summary={'schema_version':'p501_acceptance_v1','source_commit':commit,'source_archive_sha256':data['archive_sha256'],'actual_project_runner_exit':actual_exit,'overall_passed':not failures,'failures':failures,'verification_path':'verification.json','verification_sha256':bootstrap.digest(report),'receipt_orchestrator_sha256':bootstrap.digest(archived_entry),'required_receipt_tests':sorted(REQUIRED_TESTS),'cleanup':payload.get('cleanup',{}),'customer_acceptance':'not_executed','production_acceptance':'not_executed','native_four_platform_acceptance':'not_executed'}
    archived.write_summary(Path(output)/'receipt-results.json',summary)
    print('P5-01 fixed source acceptance', 'PASS' if not failures else 'FAIL', 'cleanup',summary['cleanup'].get('removed'),flush=True)
    return int(bool(failures))


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--commit',required=True);parser.add_argument('--output-root',required=True,type=Path);args=parser.parse_args(argv)
    try:return run(args.commit,args.output_root)
    except (ValueError,OSError,KeyError,TypeError,ImportError):
        print('receipt_acceptance_failed',file=sys.stderr);return 1

if __name__=='__main__':sys.exit(main())
