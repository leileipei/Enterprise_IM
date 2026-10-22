"""Actual Shell logout and same LoginPage re-login, backfill and new sends."""
import json,time,uuid,re
from .cross_login import admission
from .cross_navigation import logged_out_page
from .cross_relogin import relogin

def run(actors,backend,out,result,save,restart=False):
 schema=backend.config['schema']
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('relogin_schema_unverified')
 known={'alice':actors['alice'].state['admission']['device_id'],'bob_ios_old':actors['bob_ios'].state['admission']['device_id'],'bob_android_old':actors['bob_android'].state['admission']['device_id']}
 labels=['alice','bob_ios_old','bob_android_old','bob_ios_new','bob_android_new']
 def phase(name,n,devices,a,io,ao,inew,anew,ua,ub):
  roles=[]
  for role in labels:
   device=str(uuid.UUID(known.get(role,'00000000-0000-0000-0000-000000000000')));roles.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
  alice=str(uuid.UUID(backend.config['actors']['alice']['user']));bob=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
  query="SET search_path TO "+schema+",public; SELECT json_build_object('messages',(SELECT count(*) FROM messages),'devices',(SELECT count(*) FROM user_devices),'revoked',(SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL),'roles',json_build_object("+','.join(roles)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+alice+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+bob+"' AND read_at IS NOT NULL)));"
  expected={'messages':n,'devices':devices,'revoked':0,'roles':{r:{'delivered':v,'read':v} for r,v in zip(labels,[a,io,ao,inew,anew])},'users':{'alice':ua,'bob':ub}};end=time.monotonic()+45
  while True:
   facts=json.loads(backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input=query))
   if facts==expected:break
   if time.monotonic()>end:
    (out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2));raise ValueError('relogin_database_'+name)
   time.sleep(.2)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def events():return json.loads((out/'backend-api-events.json').read_text())
 def logout(actor):
  row=actor.command('navigation_logout');logged_out_page(row['details'])
  if row['details'].get('logout_button_tapped') is not True:raise ValueError('relogin_no_actual_logout')
  result.setdefault('button_logout',{})[actor.role]=row;save();actor.screenshot('relogin-logout-page')
  return len(events())
 def quiet(actor,boundary):
  time.sleep(3);row=actor.command('navigation_logged_out');logged_out_page(row['details']);device=actor.state['admission']['device_id']
  if any(e.get('device')==device for e in events()[boundary:]):raise ValueError('relogin_old_device_not_quiet')
  result.setdefault('quiet_windows',{})[actor.role]={'seconds':3,'start_event':boundary,'end_event':len(events()),'old_device_requests':0,'native':row};save()
 def readmit(actor):
  storage=result['initial_login'][actor.role]['details']['storage'];proof=relogin(actor,storage)
  known[actor.role+'_new']=proof['after']['device_id'];result.setdefault('relogin_proofs',{})[actor.role]=proof;save()
 result['initial_login']={}
 for role,actor in actors.items():
  row=actor.read();admission(row['details']);result['initial_login'][role]=row
 save();alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android'];phase('initial_login',0,3,0,0,0,0,0,0,0)
 alice.command('send',text='重新登录前消息一',seq='1')
 for actor in [ios,android]:actor.command('received_read',seq='1')
 alice.command('summary',seq='1',status='read');phase('before_logout_both_read',1,3,0,1,1,0,0,0,1)
 boundary=logout(ios);phase('ios_logged_out',1,3,0,1,1,0,0,0,1)
 alice.command('send',text='iOS 退出期间消息二',seq='2');android.command('received_read',seq='2');quiet(ios,boundary);phase('ios_out_android_read',2,3,0,1,2,0,0,0,2)
 readmit(ios);row=ios.command('read',seqs=['1','2']);result.setdefault('backfill',{})['bob_ios']=row;ios.screenshot('relogin-backfill-read');phase('ios_relogin_backfill_read',2,4,0,1,2,2,0,0,2)
 ios.command('send',text='iOS 重新登录后回复三',seq='3');alice.command('received_read',seq='3');ios.command('summary',seq='3',status='read');phase('ios_relogin_reply_read',3,4,1,1,2,2,0,1,2)
 boundary=logout(android);phase('android_logged_out',3,4,1,1,2,2,0,1,2)
 alice.command('send',text='Android 退出期间消息四',seq='4');ios.command('received_read',seq='4');quiet(android,boundary);phase('android_out_ios_read',4,4,1,1,2,3,0,1,3)
 readmit(android);row=android.command('read',seqs=['1','2','4']);result['backfill']['bob_android']=row;android.screenshot('relogin-backfill-read');phase('android_relogin_backfill_read',4,5,1,1,2,3,3,1,3)
 android.command('send',text='Android 重新登录后回复五',seq='5');alice.command('received_read',seq='5');android.command('summary',seq='5',status='read');phase('android_relogin_reply_read',5,5,2,1,2,3,3,2,3)
 for actor in actors.values():actor.screenshot('relogin-final-read')
 if restart:
  from .cross_relogin_restart import run as restart_run
  restart_run(actors,backend,out,result,save,phase,events,known)
 for role in ['bob_ios','bob_android']:
  boundary=result['quiet_windows'][role]['start_event'];old=known[role+'_old']
  if any(e.get('device')==old for e in events()[boundary:]):raise ValueError('relogin_old_device_resumed')
 for path,status,count in [('/api/v1/native/oauth/token',200,5),('/api/v1/native/oauth/refresh',200,2 if restart else 0),('/api/v1/devices',201,5)]:
  if len([e for e in events() if e['method']=='POST' and e['path']==path and e['status']==status])!=count:raise ValueError('relogin_api_counts')
 if not restart and any(e['path']=='/api/v1/native/oauth/refresh' for e in events()):raise ValueError('relogin_used_refresh')
 result['device_ids']=known;save()
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('relogin_cleanup')
 if restart:phase('done',8,5,4,1,2,4,4,4,4)
 else:phase('done',5,5,2,1,2,3,3,2,3)
 (backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
