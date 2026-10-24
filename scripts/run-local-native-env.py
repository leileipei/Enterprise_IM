#!/usr/bin/env python3
"""Start/status/stop an isolated local synthetic native-client test environment."""
import argparse
import json
from pathlib import Path
import sys
import time
from integration import local_env_state as state

def main():
 parser=argparse.ArgumentParser(description=__doc__);sub=parser.add_subparsers(dest='command',required=True)
 for command in ['start','status','stop']:
  p=sub.add_parser(command);p.add_argument('--state-dir',type=Path,required=True)
  if command=='start':p.add_argument('--go',type=Path,required=True);p.add_argument('--lease-seconds',type=int,default=1800)
  if command=='stop':p.add_argument('--wait-seconds',type=int,default=45)
 args=parser.parse_args()
 try:
  if args.command=='start':
   from integration.local_env import Environment
   return Environment(Path(__file__).resolve().parents[1],args).run()
  if args.command=='status':print(json.dumps(state.snapshot(args.state_dir)));return 0
  if not 1<=args.wait_seconds<=120:raise ValueError('stop_wait_out_of_range')
  result=state.request_stop(args.state_dir)
  if result['status']=='recovery_required':print(json.dumps(result));return 2
  end=time.monotonic()+args.wait_seconds
  while time.monotonic()<end:
   current=state.read(args.state_dir)
   if current['phase'] in ['stopped','failed']:
    print(json.dumps({'status':current['phase'],'private_runtime_root_removed':not (args.state_dir/current['owner']).exists()}));return 0 if current['phase']=='stopped' else 1
   time.sleep(.1)
  print(json.dumps({'status':'stop_pending'}));return 2
 except Exception:print(json.dumps({'status':'failed','reason':'local_environment_unavailable'}));return 1

if __name__=='__main__':raise SystemExit(main())
