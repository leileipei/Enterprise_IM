"""Independent evidence of automatic native delivery/read recovery."""
import re

def retry_proof(events,device,action):
 rows=[e for e in events if e.get('device')==device and e.get('action')==action]
 failures=[];success=None
 for e in rows:
  if e.get('status')==200 and e.get('forwarded') is True and e.get('injected') is False:
   success=e;break
  if e.get('status')!=503 or e.get('forwarded') is not False or e.get('injected') is not True:raise ValueError('receipt_false_failure_evidence')
  failures.append(e)
 if len(failures)<2 or success is None:raise ValueError('receipt_retry_not_observed')
 digest=failures[0].get('body_sha256')
 if not isinstance(digest,str) or not re.fullmatch('[a-f0-9]{64}',digest) or any(e.get('body_sha256')!=digest for e in failures+[success]):raise ValueError('receipt_request_changed')
 minimum=1900 if action=='read' else 950
 times=[e.get('elapsed_ms') for e in failures+[success]]
 if any(type(t) is not int or t<0 for t in times) or any(b-a<minimum for a,b in zip(times,times[1:])):raise ValueError('receipt_retry_too_early')
 return {'device':device,'action':action,'failed_attempts':len(failures),'first_success_n':success.get('n'),'body_sha256':digest,'attempt_times_ms':times,'minimum_gap_ms':min(b-a for a,b in zip(times,times[1:]))}

def delivery_blocked(row):
 if row.get('cursor')!='1' or row.get('pending_receipts')!=1 or row.get('pending_next')!=['1'] or row.get('seqs')!=[] or row.get('read_seqs')!=[] or row.get('can_send') is not False or row.get('observed_503') is not True:raise ValueError('receipt_false_local_confirmation')
