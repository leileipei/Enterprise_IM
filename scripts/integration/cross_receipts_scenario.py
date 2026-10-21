"""Transient receipt failures in real independent native processes, no manual retry."""
import json,re,time,uuid
from .cross_receipts import delivery_blocked,retry_proof

def run(actors,backend,out,result,save):
 schema=backend.config['schema'];conversation=str(uuid.UUID(backend.config['conversation']))
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('receipt_schema_unverified')
 def sql(query):
  return backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='SET search_path TO '+schema+',public; '+query)
 def phase(name,n,ad,ar,bd,ir,br,ua,ub):
  roles=[]
  for role,actor in actors.items():
   device=str(uuid.UUID(actor.state['admission']['device_id']))
   roles.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
  a=str(uuid.UUID(backend.config['actors']['alice']['user']));b=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
  query="SELECT json_build_object('messages',(SELECT count(*) FROM messages),'distinct_ids',(SELECT count(DISTINCT client_msg_id) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'roles',json_build_object("+','.join(roles)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+a+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+b+"' AND read_at IS NOT NULL)));"
  expected={'messages':n,'distinct_ids':n,'devices':3,'revoked':0,'roles':{'alice':{'delivered':ad,'read':ar},'bob_ios':{'delivered':bd,'read':ir},'bob_android':{'delivered':bd,'read':br}},'users':{'alice':ua,'bob':ub}}
  end=time.monotonic()+45
  while True:
   facts=json.loads(sql(query))
   if facts==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2));raise ValueError('receipt_database_mismatch_'+name)
   time.sleep(.2)
  facts['rows']=json.loads(sql("SELECT COALESCE(json_agg(json_build_object('message_id',id,'seq',seq::text,'client_id',client_msg_id) ORDER BY seq),'[]'::json) FROM messages;"))
  if [x['seq'] for x in facts['rows']]!=[str(i) for i in range(1,n+1)]:raise ValueError('receipt_sequence_gap')
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def events():return json.loads((out/'backend-api-events.json').read_text())
 def failed(device,action):return [e for e in events() if e.get('device')==device and e.get('action')==action and e.get('status')==503 and e.get('injected') is True and e.get('forwarded') is False]
 def wait_failed(device,action):
  end=time.monotonic()+45
  while len(failed(device,action))<2:
   if time.monotonic()>end:raise TimeoutError('receipt_automatic_retry_not_seen_'+action)
   time.sleep(.1)
 holds={};control=backend.root/'receipt-control.json'
 def release(*keys):
  for key in keys:holds.pop(key)
  temp=control.with_suffix('.new');temp.write_text(json.dumps(holds));temp.chmod(0o600);temp.replace(control)
  result.setdefault('release_observations',[]).append({'keys':list(keys),'event_count':len(events()),'holds_remaining':len(holds)});save()
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android'];recipients=[ios,android]
 for actor in recipients:
  for action in ['delivered','read']:holds[actor.state['admission']['device_id']+':'+action]=True
 # Arm before recipients open, then hold failures long enough to observe actual retries.
 temp=control.with_suffix('.new');temp.write_text(json.dumps(holds));temp.chmod(0o600);temp.replace(control)
 result['receipt_native']={};phase('registered',0,0,0,0,0,0,0,0)
 alice.command('send',text='回执失败后自动恢复消息一',seq='1');phase('first_sent',1,0,0,0,0,0,0,0)
 for actor in recipients:
  row=actor.command('open_unconfirmed');delivery_blocked(row['details']);result['receipt_native'][actor.role]={'delivery_blocked':row};save()
 for actor in recipients:wait_failed(actor.state['admission']['device_id'],'delivered')
 for actor in recipients:
  row=actor.command('receipt_snapshot',expect_unread=True);delivery_blocked(row['details']);result['receipt_native'][actor.role]['delivery_still_blocked']=row;save()
 phase('delivery_failed_no_confirmation',1,0,0,0,0,0,0,0)
 release(*(actor.state['admission']['device_id']+':delivered' for actor in recipients))
 for actor in recipients:
  row=actor.command('delivered_restored');result['receipt_native'][actor.role]['delivery_recovered']=row;save()
 phase('delivery_automatically_recovered',1,0,0,1,0,0,0,0);alice.command('summary',seq='1',status='delivered')
 for actor in recipients:actor.command('show_for_read')
 for actor in recipients:wait_failed(actor.state['admission']['device_id'],'read')
 for actor in recipients:
  row=actor.command('receipt_snapshot',expect_unread=True)
  if row['details']['pending_receipts']!=0 or row['details']['cursor']!='1' or row['details']['read_seqs'] or row['details']['network_status']!=503:raise ValueError('receipt_false_read_confirmation')
  result['receipt_native'][actor.role]['read_blocked']=row;save();actor.screenshot('read-failure-visible')
 phase('read_failed_no_confirmation',1,0,0,1,0,0,0,0);alice.command('summary',seq='1',status='delivered');alice.screenshot('delivery-only-before-read-retry')
 release(ios.state['admission']['device_id']+':read');row=ios.command('read',seqs=['1']);result['receipt_native']['bob_ios']['read_recovered']=row;save()
 phase('ios_read_recovered_android_still_unread',1,0,0,1,1,0,0,1)
 other=android.command('receipt_snapshot',expect_unread=True)
 if other['details']['read_seqs']:raise ValueError('receipt_user_read_forged_device_read')
 result['receipt_native']['bob_android']['other_device_user_read']=other;save()
 release(android.state['admission']['device_id']+':read');row=android.command('read',seqs=['1']);result['receipt_native']['bob_android']['read_recovered']=row;save()
 phase('both_read_recovered_user_deduplicated',1,0,0,1,1,1,0,1);alice.command('summary',seq='1',status='read');alice.screenshot('both-retries-read')
 observed=events();result['retry_proofs']={}
 for actor in recipients:
  for action in ['delivered','read']:
   proof=retry_proof(observed,actor.state['admission']['device_id'],action);result['retry_proofs'][actor.role+':'+action]=proof
   if actor.state['pid']!=actor.state['admission']['pid']:raise ValueError('receipt_process_replaced')
 save()
 ios.command('send',text='回执恢复后跨端回复二',seq='2');alice.command('received_read',seq='2');ios.command('summary',seq='2',status='read');phase('reply_read',2,1,1,1,1,1,1,1)
 alice.command('send',text='自动重试完成后新消息三',seq='3')
 for actor in recipients:actor.command('received_read',seq='3')
 alice.command('summary',seq='3',status='read');phase('normal_send_after_recovery',3,1,1,2,2,2,1,2);alice.screenshot('normal-send-read');ios.screenshot('new-message-read')
 if holds:raise ValueError('receipt_fault_holds_remain')
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('receipt_actor_cleanup')
 phase('done',3,1,1,2,2,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
