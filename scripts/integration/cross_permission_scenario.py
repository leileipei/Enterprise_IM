"""End one membership by real admin API while native clients remain alive."""
import json,re,time,uuid
from .cross_permission import terminal_proof,database_proof

def run(actors,backend,out,result,save):
 schema=backend.config['schema'];conversation=str(uuid.UUID(backend.config['conversation']))
 ended=str(uuid.UUID(backend.config['actors']['bob_ios']['membership']));other=str(uuid.UUID(backend.config['permission_other_membership']));bob=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('permission_schema_unverified')
 def sql(q):
  return backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='SET search_path TO '+schema+',public; '+q)
 query="SELECT json_build_object('messages',(SELECT count(*) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'delivered',(SELECT count(*) FROM message_device_receipts),'read',(SELECT count(*) FROM message_device_receipts WHERE read_at IS NOT NULL),'user_read',(SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL),'ended_status',(SELECT status FROM user_organizations WHERE id='"+ended+"'),'other_status',(SELECT status FROM user_organizations WHERE id='"+other+"'),'user_status',(SELECT status FROM users WHERE id='"+bob+"'),'end_audits',(SELECT count(*) FROM audit_events WHERE action='membership_end' AND outcome='allow' AND resource_id='"+ended+"'));"
 def phase(name,after=False):
  expected={'messages':2,'devices':3,'revoked':0,'delivered':3,'read':3,'user_read':2,'ended_status':'active','other_status':'active','user_status':'active','end_audits':0}
  if after:expected.update(ended_status='ended',end_audits=1)
  end=time.monotonic()+45
  while True:
   row=json.loads(sql(query))
   if row==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(row,indent=2));raise ValueError('permission_database_mismatch_'+name)
   time.sleep(.2)
  if after:database_proof(row)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(row,indent=2)+'\n');result['checkpoints'].append(name);save()
 def events():return json.loads((out/'backend-api-events.json').read_text())
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android'];recipients=[ios,android]
 alice.command('send',text='任职变更前消息一',seq='1')
 for actor in recipients:actor.command('read',seqs=['1'])
 ios.command('send',text='任职变更前回复二',seq='2');alice.command('received_read',seq='2');ios.command('summary',seq='2',status='read')
 for actor in recipients:
  row=actor.command('permission_prepare')
  if row['details'].get('proof_was_real') is not True:raise ValueError('permission_missing_real_proof')
  actor.screenshot('before-membership-end')
 phase('before_membership_end');boundary=len(events())
 ended_state=alice.command('end_membership',membership=ended)
 if ended_state['details'].get('ended_membership')!=ended:raise ValueError('permission_admin_ack_mismatch')
 phase('membership_ended_by_admin',True)
 result['permission_native']={}
 for actor in recipients:
  row=actor.command('permission_terminal',other_membership=other);terminal_proof(row['details'],ended,other)
  result['permission_native'][actor.role]=row;save();actor.screenshot('ended-membership-no-body')
 phase('both_native_terminal',True)
 changes=events()[boundary:];admin=[e for e in changes if e['path']=='/api/v1/admin/memberships/'+ended+':end' and e['method']=='POST']
 if len(admin)!=1 or admin[0]['status']!=204:raise ValueError('permission_admin_transport_mismatch')
 for actor in recipients:
  device=actor.state['admission']['device_id'];own=[e for e in changes if e.get('device')==device]
  for action in ['sync','delivered','read','messages']:
   rows=[e for e in own if e['path']=='/api/v1/conversations/'+conversation+'/'+action and e['status']==403 and e['device_headers']]
   if not rows:raise ValueError('permission_actual_denial_missing_'+action)
 # No further native traffic for the ended context, registration or renewal.
 # Passive host wait and a local-only native snapshot cannot trigger recovery.
 quiet_start=len(events());time.sleep(3)
 for actor in recipients:
  stable=actor.command('permission_stable');result.setdefault('stable_native',{})[actor.role]=stable
  if stable['device_id']!=actor.state['admission']['device_id'] or stable['pid']!=actor.state['admission']['pid']:raise ValueError('permission_actor_changed')
 bob_devices={a.state['admission']['device_id'] for a in recipients}
 quiet=[e for e in events()[quiet_start:] if e.get('device') in bob_devices or (e['method']=='POST' and e['path']=='/api/v1/devices')]
 if quiet:raise ValueError('permission_terminal_still_issues_requests')
 result['quiet_proof']={'seconds':3,'start_event':quiet_start,'ended_device_requests':0,'new_registrations':0};save()
 if alice.command('identity_still_active')['details'].get('account_active') is not True:raise ValueError('permission_admin_account_lost')
 phase('stable_scoped_end',True)
 result['permission_memberships']={'ended':ended,'surviving':other,'user':bob};result['permission_events']=changes;save()
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('permission_actor_cleanup')
 phase('done',True);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
