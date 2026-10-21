"""Native session logout while actual production sync200 waits in TLS fixture."""
import json,re,time,uuid
from .cross_logout import logout_proof,response_proof

def run(actors,backend,out,result,save):
 schema=backend.config['schema'];conversation=str(uuid.UUID(backend.config['conversation']))
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('logout_schema_unverified')
 def sql(q):
  return backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='SET search_path TO '+schema+',public; '+q)
 def phase(name,n,alice,ios,android,ua,ub):
  roles=[]
  for role,actor in actors.items():
   device=str(uuid.UUID(actor.state['admission']['device_id']));roles.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
  a=str(uuid.UUID(backend.config['actors']['alice']['user']));b=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
  query="SELECT json_build_object('messages',(SELECT count(*) FROM messages),'distinct_ids',(SELECT count(DISTINCT client_msg_id) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'roles',json_build_object("+','.join(roles)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+a+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+b+"' AND read_at IS NOT NULL)));"
  expected={'messages':n,'distinct_ids':n,'devices':3,'revoked':0,'roles':{r:{'delivered':v,'read':v} for r,v in [('alice',alice),('bob_ios',ios),('bob_android',android)]},'users':{'alice':ua,'bob':ub}};end=time.monotonic()+45
  while True:
   facts=json.loads(sql(query))
   if facts==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2));raise ValueError('logout_database_mismatch_'+name)
   time.sleep(.2)
  rows=json.loads(sql("SELECT COALESCE(json_agg(json_build_object('message_id',id,'seq',seq::text,'client_id',client_msg_id) ORDER BY seq),'[]'::json) FROM messages;"));facts['rows']=rows
  if [row['seq'] for row in rows]!=[str(i) for i in range(1,n+1)]:raise ValueError('logout_sequence_gap')
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def events():return json.loads((out/'backend-api-events.json').read_text())
 def wait_event(device,phase):
  end=time.monotonic()+40
  while True:
   rows=[e for e in events() if e.get('device')==device and e.get('phase')==phase]
   if rows:return rows[-1]
   if time.monotonic()>end:raise TimeoutError('logout_response_'+phase+'_not_observed')
   time.sleep(.1)
 holds={};control=backend.root/'logout-control.json'
 def set_hold(device,seq=None):
  if seq is None:holds.pop(device,None)
  else:holds[device]=seq
  tmp=control.with_suffix('.new');tmp.write_text(json.dumps(holds));tmp.chmod(0o600);tmp.replace(control)
 def log_out(actor,cursor,target,text):
  actor.command('logout_prepare',cursor=cursor);device=actor.state['admission']['device_id'];set_hold(device,target)
  alice.command('send',text=text,seq=target);held=wait_event(device,'held')
  if held.get('next_seq')!=target or held.get('upstream_status')!=200:raise ValueError('logout_wrong_response_held')
  actor.screenshot('sync-held-before-logout');row=actor.command('logout_while_held',cursor=cursor,blocked_seq=target);logout_proof(row['details']);result.setdefault('logout_native',{})[actor.role]=row;save();actor.screenshot('logged-out-response-still-held')
  # Only after native vault/database/UI teardown has completed, release the
  # exact real response. An OS socket write does not prove client delivery.
  set_hold(device);wait_event(device,'released');proof=response_proof(events(),device,target);result.setdefault('late_response_proofs',{})[actor.role]=proof;save()
  time.sleep(1);stable=actor.command('logout_stable');logout_proof(stable['details']);result.setdefault('stable_native',{})[actor.role]=stable;save();actor.screenshot('late-response-no-resurrection')
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android'];phase('registered',0,0,0,0,0,0)
 alice.command('send',text='退出竞态消息一',seq='1')
 for actor in [ios,android]:actor.command('read',seqs=['1'])
 phase('both_read_before_logout',1,0,1,1,0,1)
 log_out(ios,'1','2','退出前在途消息二');android.command('received_read',seq='2');phase('ios_logged_out_survivor_read',2,0,1,2,0,2)
 android.command('send',text='存活设备回复三',seq='3');alice.command('received_read',seq='3');android.command('summary',seq='3',status='read');phase('survivor_reply_read',3,1,1,2,1,2)
 log_out(android,'3','4','Android 退出前在途消息四');phase('both_logged_out',4,1,1,2,1,2)
 boundary=len(events());alice.command('send',text='双方退出后消息五',seq='5');time.sleep(3)
 for actor in [ios,android]:
  row=actor.command('logout_stable');logout_proof(row['details']);result['stable_native'][actor.role]=row
 own={ios.state['admission']['device_id'],android.state['admission']['device_id']};unexpected=[e for e in events()[boundary:] if e.get('device') in own or (e.get('method')=='POST' and e.get('path')=='/api/v1/devices')]
 if unexpected:raise ValueError('logout_old_devices_still_issue_requests')
 result['quiet_proof']={'seconds':3,'start_event':boundary,'logged_out_device_requests':0,'new_registrations':0};save();phase('new_message_after_logout_no_receipts',5,1,1,2,1,2);alice.screenshot('survivor-send-after-logout')
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('logout_actor_cleanup')
 phase('done',5,1,1,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
