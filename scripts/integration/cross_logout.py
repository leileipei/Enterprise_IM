"""Only real held upstream success plus completed native logout is acceptance."""
import re

def logout_proof(row):
 expected={'account_absent':True,'vault_absent':True,'database_files_absent':True,'context_cleared':True,'can_send':False,'seqs':[],'read_seqs':[],'ui_body_absent':True,'cursor':None,'same_process':True}
 if any(row.get(k)!=v for k,v in expected.items()):raise ValueError('logout_native_state_resurrected')

def response_proof(events,device,seq):
 rows=[e for e in events if e.get('device')==device and e.get('phase') in ['held','released','timeout','invalid_control']]
 if len(rows)!=2 or [e.get('phase') for e in rows]!=['held','released']:raise ValueError('logout_response_window_incomplete')
 first,last=rows;digest=first.get('body_sha256')
 if not isinstance(digest,str) or not re.fullmatch('[a-f0-9]{64}',digest) or last.get('body_sha256')!=digest or any(e.get('upstream_status')!=200 or e.get('next_seq')!=seq for e in rows) or last.get('request_cancelled') is not True or type(first.get('n')) is not int or type(last.get('n')) is not int or last['n']<=first['n']:raise ValueError('logout_not_real_late_cancelled_success')
 return {'device':device,'next_seq':seq,'body_sha256':digest,'held_n':first['n'],'released_n':last['n'],'request_cancelled':True}
