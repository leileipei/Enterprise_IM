"""Real LoginPage/ClientShell logout button and post-logout OS cold start."""
import json,re,time,uuid
from .cross_navigation import logged_out_page,cold_start

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
 def logout(actor):
  row=actor.command('navigation_logout');logged_out_page(row['details'])
  if row['details'].get('logout_button_tapped') is not True:raise ValueError('navigation_no_logout_button')
  result.setdefault('navigation_logout',{})[actor.role]=row;save();actor.screenshot('logout-button-back-to-login')
  fresh=actor.command('navigation_reopen');logged_out_page(fresh['details'])
  if fresh['details'].get('new_api_creations')!=0:raise ValueError('navigation_reopen_authenticated')
  result.setdefault('fresh_login_page',{})[actor.role]=fresh;save();actor.screenshot('fresh-login-page-no-account')
  before={'pid':actor.state['pid'],'device':actor.state['admission']['device_id'],'storage':row['details']['storage']};actor.stop_for_restart()
  config={k:v for k,v in actor.config.items() if k!='refresh'};config.update(restart=True,logged_out_restart=True)
  boundary=len(events());actor.launch_restart(config);initial=actor.read()
  if initial['details'].get('no_seed_refresh') is not True or initial['details'].get('logged_out_restart') is not True:raise ValueError('navigation_cold_boot_seeded')
  cold=actor.command('navigation_cold_start');logged_out_page(cold['details'])
  proof={'pid':actor.state['pid'],'device':cold['device_id'],'storage':cold['details']['storage'],'old_absent':actor.state['restart_old_absent'],'no_seed_refresh':cold['details']['no_seed_refresh'],'login_api_creations':cold['details']['login_api_creations']};cold_start(before,proof)
  if any(e['path'].startswith('/api/v1/native/oauth/') or (e['method']=='POST' and e['path']=='/api/v1/devices') for e in events()[boundary:]):raise ValueError('navigation_cold_boot_network_auth')
  result.setdefault('cold_start_proofs',{})[actor.role]={'before':before,'after':proof,'native':cold,'initial':initial};save();actor.screenshot('cold-start-stays-logged-out')
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android'];phase('registered',0,0,0,0,0,0)
 alice.command('send',text='界面退出前消息一',seq='1')
 for actor in [ios,android]:
  row=actor.command('navigation_open');d=row['details']
  if d.get('login_restore_via_ui') is not True or d.get('workspace_entered') is not True or d.get('same_device') is not True:raise ValueError('navigation_actual_workspace_not_open')
  result.setdefault('navigation_admissions',{})[actor.role]=row;actor.command('read',seqs=['1']);actor.screenshot('actual-login-workspace-message-read')
 phase('actual_ui_both_read',1,0,1,1,0,1);logout(ios);phase('ios_button_logout_and_cold_start',1,0,1,1,0,1)
 android.command('send',text='界面退出后存活设备回复二',seq='2');alice.command('received_read',seq='2');android.command('summary',seq='2',status='read');phase('survivor_reply_read',2,1,1,1,1,1)
 alice.command('send',text='冷启动期间新消息三',seq='3');android.command('received_read',seq='3');phase('ios_cold_stays_out_survivor_read',3,1,1,2,1,2)
 logout(android);phase('both_button_logout_and_cold_start',3,1,1,2,1,2)
 boundary=len(events());alice.command('send',text='双方界面退出后消息四',seq='4');time.sleep(3)
 for actor in [ios,android]:
  row=actor.command('navigation_logged_out');logged_out_page(row['details']);result.setdefault('stable_native',{})[actor.role]=row
 devices={ios.state['admission']['device_id'],android.state['admission']['device_id']}
 if any(e.get('device') in devices or e['path'].startswith('/api/v1/native/oauth/') or (e['method']=='POST' and e['path']=='/api/v1/devices') for e in events()[boundary:]):raise ValueError('navigation_logout_not_quiet')
 result['quiet_proof']={'seconds':3,'start_event':boundary,'logged_out_device_requests':0,'new_auth_or_registration':0};save();phase('new_message_after_cold_no_receipts',4,1,1,2,1,2);alice.screenshot('survivor-send-after-ui-logouts')
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('navigation_actor_cleanup')
 phase('done',4,1,1,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
