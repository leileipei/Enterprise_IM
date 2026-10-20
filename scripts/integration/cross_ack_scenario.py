"""Lost production ACK -> actual native death -> original pending UI retry."""
import json,re,time,uuid
from .cross_ack import lost_ack,recovered_ack

def run(actors,backend,out,result,save):
 schema=backend.config['schema'];conversation=str(uuid.UUID(backend.config['conversation']))
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('ack_schema_unverified')
 def sql(query):
  return backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='SET search_path TO '+schema+',public; '+query)
 def messages():
  return json.loads(sql("SELECT COALESCE(json_agg(json_build_object('message_id',id,'seq',seq::text,'client_id',client_msg_id) ORDER BY seq),'[]'::json) FROM messages;"))
 def phase(name,n,alice,bob,alice_reads,bob_reads):
  roles=[]
  for role,actor in actors.items():
   device=str(uuid.UUID(actor.state['admission']['device_id']))
   roles.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
  a=str(uuid.UUID(backend.config['actors']['alice']['user']));b=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
  query="SELECT json_build_object('messages',(SELECT count(*) FROM messages),'distinct_ids',(SELECT count(DISTINCT client_msg_id) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'roles',json_build_object("+','.join(roles)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+a+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+b+"' AND read_at IS NOT NULL)));"
  expected={'messages':n,'distinct_ids':n,'devices':3,'revoked':0,'roles':{role:{'delivered':alice if role=='alice' else bob,'read':alice if role=='alice' else bob} for role in actors},'users':{'alice':alice_reads,'bob':bob_reads}}
  end=time.monotonic()+45
  while True:
   facts=json.loads(sql(query))
   if facts==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2));raise ValueError('ack_database_mismatch_'+name)
   time.sleep(.2)
  rows=messages()
  if [x['seq'] for x in rows]!=[str(i) for i in range(1,n+1)]:raise ValueError('ack_sequence_gap')
  facts['rows']=rows;(out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def events():return json.loads((out/'backend-api-events.json').read_text())
 def posts():return [e for e in events() if e['method']=='POST' and e['path']=='/api/v1/conversations/'+conversation+'/messages']
 def loss(actor,text,seq):
  row=actor.command('send_lost_ack',text=text)['details'];db=messages()
  if len(db)!=int(seq):raise ValueError('ack_commit_count')
  db=db[-1];event=posts()[-1]
  lost_ack({**row,'db_client_id':db['client_id'],'upstream_status':event.get('upstream_status'),'downstream_dropped':event.get('downstream_dropped'),'duplicate':event.get('ack',{}).get('duplicate')})
  if event['device']!=actor.state['admission']['device_id'] or event['ack']['message_id']!=db['message_id'] or event['ack']['seq']!=seq:raise ValueError('ack_commit_identity')
  proof={**row,'device_id':actor.state['admission']['device_id'],'pid':actor.state['pid'],'message_id':db['message_id'],'seq':seq}
  result.setdefault('lost_ack_proofs',{})[actor.role]=proof;save();actor.screenshot('lost-ack-pending');return proof
 def restart(actor,before):
  actor.stop_for_restart();boundary=len(events());post_count=len(posts());config={k:v for k,v in actor.config.items() if k!='refresh'};config['restart']=True
  actor.launch_restart(config);initial=actor.read();actor.screenshot('preauth-hidden');restored=actor.command('restore_pending');pending=actor.command('pending_restored')
  if restored['details']['pending_id']!=before['pending_id'] or pending['details']['pending_id']!=before['pending_id'] or pending['details']['pending_count']!=1:raise ValueError('ack_pending_replaced')
  time.sleep(3)
  stable=actor.command('pending_restored');automatic=len(posts())-post_count
  if automatic or stable['details']['pending_id']!=before['pending_id']:raise ValueError('ack_automatic_resend')
  actor.screenshot('restored-pending-before-retry')
  retried=actor.command('retry_pending',seq=before['seq']);e=posts()[-1]
  if len(posts())!=post_count+1 or e.get('downstream_dropped') is not False or e.get('upstream_status')!=200 or e['device']!=before['device_id'] or e['ack']!={'message_id':before['message_id'],'seq':before['seq'],'duplicate':True}:raise ValueError('ack_explicit_retry_transport')
  own=[e for e in events()[boundary:] if e.get('device')==before['device_id']];renew=[e['n'] for e in own if e['path']=='/api/v1/devices/'+before['device_id']+'/renew' and e['status']==200];sync=[e['n'] for e in own if e['path']=='/api/v1/conversations/'+conversation+'/sync' and e['status']==200]
  proof={**retried['details'],'pid':actor.state['pid'],'device_id':retried['device_id'],'old_absent':actor.state['restart_old_absent'],'preauth_hidden':initial['details'].get('preauth_hidden'),'vault_reused':restored['details'].get('vault_reused'),'automatic_posts':automatic,'renew_before_sync':bool(renew and sync and min(renew)<min(sync)),'renew_events':renew,'sync_events':sync,'quiet_seconds':3}
  recovered_ack(before,proof)
  if proof.get('confirmation_saved') is not True:raise ValueError('ack_confirmation_not_durable')
  result.setdefault('restart_proofs',{})[actor.role]=proof;save();actor.screenshot('retry-confirmed')
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android']
 phase('registered',0,0,0,0,0)
 first=loss(alice,'确认丢失后的 Android 原消息','1')
 for actor in [ios,android]:actor.command('read',seqs=['1'])
 phase('android_pending_read',1,0,1,0,1)
 restart(alice,first);alice.command('summary',seq='1',status='read');phase('android_retry_same_message',1,0,1,0,1)
 second=loss(ios,'确认丢失后的 iOS 原消息','2');alice.command('received_read',seq='2');phase('ios_pending_read',2,1,1,1,1)
 restart(ios,second);ios.command('summary',seq='2',status='read');phase('ios_retry_same_message',2,1,1,1,1)
 alice.command('send',text='恢复后正常新消息三',seq='3')
 for actor in [ios,android]:actor.command('received_read',seq='3')
 alice.command('summary',seq='3',status='read');phase('normal_send_after_retries',3,1,2,1,2)
 alice.screenshot('normal-send-read');ios.screenshot('normal-message-read');observed=posts()
 if len(observed)!=5 or [e['ack']['seq'] for e in observed]!=['1','1','2','2','3'] or [e['ack']['duplicate'] for e in observed]!=[False,True,False,True,False] or [e['downstream_dropped'] for e in observed]!=[True,False,True,False,False] or not all(e['device_headers'] and e['upstream_status']==200 for e in observed):raise ValueError('ack_final_transport_observation')
 result['ack_events']=observed;save()
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('ack_actor_cleanup')
 phase('done',3,1,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
