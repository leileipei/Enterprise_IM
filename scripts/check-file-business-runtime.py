#!/usr/bin/env python3
import json,sys
required={f'TestFileBusinessProcessRP{i:02d}' for i in range(1,15)}
required.add('TestFileBusinessProcessRP08/slow_client_SIGTERM')
passed=set();failed=[];skipped=[]
for line in open(sys.argv[1]):
 try:e=json.loads(line)
 except json.JSONDecodeError:continue
 action=e.get('Action');name=e.get('Test','')
 if action=='pass' and name:passed.add(name)
 if action=='fail':failed.append(name or e.get('Package'))
 if action=='skip':skipped.append(name or e.get('Package'))
missing=required-passed
if int(sys.argv[2]) or missing or failed or skipped:
 print('business-process gate failed; missing:',sorted(missing),'failed:',failed,'skipped:',skipped,file=sys.stderr);sys.exit(1)
print('business-process gate passed; mandatory=14 slow-client-SIGTERM=PASS FAIL=0 SKIP=0')
