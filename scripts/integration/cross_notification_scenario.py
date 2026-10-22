"""Real process-active mobile notification opt-in, OS tap and cleanup."""
import json,time,re,uuid
from .cross_login import admission

def run(actors,backend,out,result,save):
 schema=backend.config['schema']
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('notification_schema_unverified')
 def phase(name,n,a,i,d,ua,ub):
  roles=[]
  for role,actor in actors.items():
   device=str(uuid.UUID(actor.state['admission']['device_id']));roles.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
  alice=str(uuid.UUID(backend.config['actors']['alice']['user']));bob=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
  query="SET search_path TO "+schema+",public; SELECT json_build_object('messages',(SELECT count(*) FROM messages),'devices',(SELECT count(*) FROM user_devices),'roles',json_build_object("+','.join(roles)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+alice+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+bob+"' AND read_at IS NOT NULL)));"
  expected={'messages':n,'devices':3,'roles':{r:{'delivered':v,'read':v} for r,v in [('alice',a),('bob_ios',i),('bob_android',d)]},'users':{'alice':ua,'bob':ub}};end=time.monotonic()+45
  while True:
   facts=json.loads(backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input=query))
   if facts==expected:break
   if time.monotonic()>end:raise ValueError('notification_database_'+name)
   time.sleep(.2)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def gate(actor,kind,action,**kwargs):
  result['ui_gate']={'role':actor.role,'kind':kind,'owner':actor.state['name'],'udid':getattr(actor,'udid',None),'android_console':getattr(getattr(actor,'sim',None),'console',None),'state':'awaiting_real_OS_UI'};save()
  actor.number+=1;actor.write('cross-command.json',{'id':actor.number,'action':action,**kwargs});row=actor.await_state(actor.number,timeout=260)
  if row.get('device_id')!=actor.state['admission']['device_id']:raise ValueError('notification_device_replaced')
  result['ui_gate']['state']='native_command_completed';save();return row
 result['notification_proofs']={}
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android']
 for role,actor in actors.items():
  admission(actor.read()['details'])
  if role!='bob_ios':
   args=['shell','pm','grant' if role=='bob_android' else 'revoke','com.enterpriseim.client','android.permission.POST_NOTIFICATIONS'];actor.sim.adb('notification-permission-state',args)
   if role=='alice':actor.sim.adb('notification-denial-fixed',['shell','pm','set-permission-flags','com.enterpriseim.client','android.permission.POST_NOTIFICATIONS','user-set','user-fixed'])
  row=gate(actor,'ios_notification_permission','notification_permission',allowed=True) if role=='bob_ios' else actor.command('notification_permission',allowed=role!='alice')
  result['notification_proofs'][role]={'permission':row,'permission_mode':'actual_iOS_system_prompt' if role=='bob_ios' else 'controlled_Android_OS_permission_state'};save();actor.screenshot('notification-permission')
  actor.command('notification_open')
 phase('notification_registered',0,0,0,0,0,0)
 alice.command('send',text='移动通知验证消息一',seq='1')
 for actor in [ios,android]:actor.command('received_read',seq='1')
 alice.command('summary',seq='1',status='read');phase('notification_first_read',1,0,1,1,0,1)
 for actor in [ios,android]:
  proof=result['notification_proofs'][actor.role];proof['shown']=actor.command('notification_status',count=1);actor.screenshot('notification-shown');proof['armed']=actor.command('notification_arm_tap');save()
  proof['tapped']=gate(actor,'ios_notification_tap' if actor.role=='bob_ios' else 'android_notification_tap','notification_tapped');save();actor.screenshot('notification-tap-recovered')
 alice.command('send',text='移动通知退出清理消息二',seq='2')
 for actor in [ios,android]:actor.command('received_read',seq='2');actor.command('notification_status',count=1)
 alice.command('summary',seq='2',status='read');phase('notification_second_read',2,0,2,2,0,2)
 ios.command('send',text='通知拒绝后仍可收发回复三',seq='3');alice.command('received_read',seq='3');ios.command('summary',seq='3',status='read');phase('notification_denied_receipts_work',3,1,2,2,1,2)
 result['notification_proofs']['alice']['denied_no_notification']=alice.command('notification_status',count=0)
 for actor in [ios,android]:
  proof=result['notification_proofs'][actor.role];proof['logout']=actor.command('navigation_logout');proof['after_logout']=actor.command('notification_status',count=0);save();actor.screenshot('notification-logout-empty')
 alice.command('send',text='双端退出后不再产生通知四',seq='4');time.sleep(3);phase('notification_both_out_no_receipts',4,1,2,2,1,2)
 for actor in [ios,android]:actor.command('notification_status',count=0)
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('notification_cleanup')
 phase('done',4,1,2,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
