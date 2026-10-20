"""Real retention admin changes and killed native processes using original vaults."""
import json,re,time,uuid
from .cross_group import PAYLOAD,download,selection
from .cross_retention import policy_proof,restart_proof

def run(actors,backend,out,result,save):
 schema=backend.config['schema']
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('retention_schema_unverified')
 def sql(query):
  return backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='SET search_path TO '+schema+',public; '+query)
 def phase(name,messages,downloads,version,days,delivered,reads):
  tenant=str(uuid.UUID(backend.config['tenant']))
  query="SELECT json_build_object('messages',(SELECT count(*) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'downloads',(SELECT count(*) FROM file_download_sessions WHERE phase='completed' AND audit_acked),'message_version',(SELECT retention_version FROM tenants WHERE id='"+tenant+"'),'file_version',(SELECT version FROM tenant_file_retention_policy WHERE tenant_id='"+tenant+"'),'message_days',(SELECT message_body_retention_days FROM tenants WHERE id='"+tenant+"'),'file_days',(SELECT file_retention_days FROM tenant_file_retention_policy WHERE tenant_id='"+tenant+"'),'delivered',(SELECT count(*) FROM message_device_receipts),'read',(SELECT count(*) FROM message_device_receipts WHERE read_at IS NOT NULL),'aged',(SELECT count(*) FROM messages WHERE accepted_at<clock_timestamp()-interval '1 day'),'bodies_present',(SELECT count(*) FROM messages WHERE text_body IS NOT NULL),'cleanup_enabled',(SELECT cleanup_enabled FROM tenant_file_retention_policy WHERE tenant_id='"+tenant+"'));"
  end=time.monotonic()+40
  while True:
   row=json.loads(sql(query))
   expected={'messages':messages,'devices':len(actors),'revoked':0,'downloads':downloads,'message_version':version,'file_version':version,'message_days':days,'file_days':days,'delivered':delivered,'read':reads,'aged':3,'bodies_present':messages,'cleanup_enabled':False}
   if row==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(row,indent=2));raise ValueError('retention_database_mismatch_'+name)
   time.sleep(.2)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(row,indent=2)+'\n');result['checkpoints'].append(name);save()
 alice=actors['alice'];recipients=[actor for role,actor in actors.items() if role!='alice'];ios=recipients[0];n=len(recipients)
 created=alice.command('create_group',bob_membership=backend.config['actors']['bob_ios']['membership']);group=str(uuid.UUID(created['details']['group_id']));result['group_id']=group
 selection('alice',alice.command('select_group',group_id=group)['details']);alice.command('send',text='独立群消息一',seq='1')
 for actor in recipients:selection(actor.role,actor.command('select_group',group_id=group)['details']);actor.command('read',seqs=['1'])
 ios.command('send',text='跨组织群回复二',seq='2');alice.command('received_read',seq='2')
 sent=alice.command('file_send',contents=PAYLOAD.decode());result['file_id']=sent['details']['file_id']
 for actor in recipients:actor.command('received_read',seq='3')
 # Only timestamps in this isolated schema are aged; policy updates below use real admin APIs.
 aged=sql("UPDATE messages SET accepted_at=clock_timestamp()-interval '2 days' WHERE conversation_id='"+group+"' RETURNING seq;")
 if aged.splitlines()!=['1','2','3']:raise ValueError('retention_age_fixture_count')
 (out/'fixture-aging.json').write_text(json.dumps({'field':'messages.accepted_at','offset_days':-2,'seqs':['1','2','3'],'scope':'owned isolated schema only; no clock change or physical purge'})+'\n')
 # Establish a long policy first. Existing defaults may differ; both start at version 0.
 policy_proof(alice.command('policy',days=30,version=1)['details'],30,1)
 for actor in recipients:
  actor.command('retention_view',hidden=False);download(actor.command('download',contents=PAYLOAD.decode())['details'],actor.state['admission']['device_id'])
 phase('long_cached',3,n,1,30,1+2*n,1+2*n)
 # Shortening removes old bodies and UI-owned content after a fresh production admission.
 policy_proof(alice.command('policy',days=1,version=2)['details'],1,2)
 for actor in actors.values():actor.command('retention_view',hidden=True);actor.screenshot('retention-hidden')
 phase('short_hidden',3,n,2,1,1+2*n,1+2*n)
 policy_proof(alice.command('policy',days=30,version=3)['details'],30,3)
 # Download again so ungraceful process deaths leave real artifacts to recover.
 alice.command('retention_view',hidden=False)
 before={}
 for actor in recipients:
  actor.command('retention_view',hidden=False);download(actor.command('download',contents=PAYLOAD.decode())['details'],actor.state['admission']['device_id'])
  prepared=actor.command('prepare_restart');before[actor.role]={'pid':actor.state['pid'],'device_id':actor.state['admission']['device_id'],'nonce':actor.config['nonce'],'storage':prepared['details']['storage']}
 phase('extended_cached',3,2*n,3,30,1+2*n,1+2*n)
 for actor in recipients:actor.stop_for_restart()
 policy_proof(alice.command('policy',days=1,version=4)['details'],1,4)
 alice.command('retention_view',hidden=True)
 alice.command('send',text='进程退出期间新消息四',seq='4');phase('processes_stopped',4,2*n,4,1,1+2*n,1+2*n)
 result['restart_proofs']={}
 for actor in recipients:
  config={k:v for k,v in actor.config.items() if k!='refresh'};config['restart']=True
  boundary=len(json.loads((out/'backend-api-events.json').read_text()))
  actor.launch_restart(config);initial=actor.read()
  if initial['details'].get('cached_before_restore') is not True:raise ValueError('restart_disk_cache_missing')
  actor.screenshot('preauth-no-body');restored=actor.command('restore');actor.command('select_group',group_id=group);actor.command('read',seqs=['4']);actor.command('retention_view',hidden=True)
  events=json.loads((out/'backend-api-events.json').read_text())[boundary:];device=before[actor.role]['device_id']
  own=[e for e in events if e.get('device')==device];renew=[e['n'] for e in own if e['path']=='/api/v1/devices/'+device+'/renew' and e['status']==200];sync=[e['n'] for e in own if e['path']=='/api/v1/conversations/'+group+'/sync' and e['status']==200]
  proof={**before[actor.role],'pid':actor.state['pid'],'old_absent':actor.state['restart_old_absent'],'preauth_hidden':initial['details'].get('preauth_hidden'),'vault_reused':restored['details'].get('vault_reused'),'cache_erased':restored['details'].get('cache_erased'),'storage':restored['details'].get('storage'),'renew_before_sync':bool(renew and sync and min(renew)<min(sync)),'restored_cursor':restored['details'].get('restored_cursor'),'renew_events':renew,'sync_events':sync}
  restart_proof(before[actor.role],proof);result['restart_proofs'][actor.role]=proof;save();actor.screenshot('restarted-short-policy')
 phase('restarted_caught_up',4,2*n,4,1,1+3*n,1+3*n)
 alice.command('summary',seq='4',status='read')
 policy_proof(alice.command('policy',days=30,version=5)['details'],30,5)
 for actor in recipients:actor.command('retention_view',hidden=False);actor.screenshot('restarted-extended-policy')
 phase('extended_after_restart',4,2*n,5,30,1+3*n,1+3*n)
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('retention_actor_cleanup_missing')
 phase('done',4,2*n,5,30,1+3*n,1+3*n)
 (backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
