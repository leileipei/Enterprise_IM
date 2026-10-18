#!/usr/bin/env python3
"""Check native-client evidence offline. Does not launch builds or device tests."""
import argparse
import json
from pathlib import Path
import sys
from integration.native_results import EvidenceError, assess, read_json


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo',type=Path,default=Path(__file__).resolve().parents[1])
    parser.add_argument('--source',required=True,help='full immutable Git commit')
    inputs=parser.add_mutually_exclusive_group(required=True)
    inputs.add_argument('--manifest',type=Path)
    inputs.add_argument('--runner-output',type=Path,action='append',help='completed mobile output containing acceptance sidecars; repeat for another platform')
    parser.add_argument('--output',type=Path,required=True,help='new JSON file; never overwritten')
    parser.add_argument('--defer-physical',action='store_true',help='label absent physical cases deferred; never waives them')
    args=parser.parse_args()
    try:
        if args.runner_output:
            from integration.native_runner_adapter import collect_mobile_exports
            manifest=collect_mobile_exports(args.repo,args.runner_output,args.source)
        else:
            manifest=read_json(args.manifest)
        report=assess(args.repo,manifest,args.source,defer_physical=args.defer_physical)
    except EvidenceError:
        report={'schema':'native_acceptance_results_v1','status':'invalid','four_platform_complete':False,'customer_acceptance':False,'production_acceptance':False,'reason':'input_json_invalid'}
    try:
        with args.output.open('x',encoding='utf-8') as stream:
            json.dump(report,stream,ensure_ascii=False,indent=2);stream.write('\n')
    except OSError:
        print('output_not_created; existing files are preserved',file=sys.stderr)
        return 1
    print(json.dumps({'status':report['status'],'four_platform_complete':report['four_platform_complete'],'mode':'offline_evidence_contract_check_only'}))
    return 0 if report['status']=='passed' else 2 if report['status']=='incomplete' else 1


if __name__=='__main__':
    raise SystemExit(main())
