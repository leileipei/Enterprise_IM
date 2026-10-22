"""Actual fresh LoginPage/native callback followed by production messaging."""
import json,time,uuid,re
from .cross_login import admission

def run(actors,backend,out,result,save):
 schema=backend.config['schema']
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('login_schema_unverified')
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
   if time.monotonic()>end:raise ValueError('fresh_login_database_'+name)
   time.sleep(.2)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 result['login_admissions']={}
 for role,actor in actors.items():
  row=actor.read();admission(row['details']);result['login_admissions'][role]=row;actor.screenshot('fresh-login-workspace')
 save();phase('fresh_login_registered',0,0,0,0,0,0)
 alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android']
 alice.command('send',text='原生回调登录后消息一',seq='1')
 for actor in [ios,android]:actor.command('received_read',seq='1')
 alice.command('summary',seq='1',status='read');phase('first_message_both_read',1,0,1,1,0,1)
 alice.command('send',text='PKCE 登录后消息二',seq='2')
 for actor in [ios,android]:actor.command('received_read',seq='2')
 alice.command('summary',seq='2',status='read');phase('second_message_both_read',2,0,2,2,0,2)
 ios.command('send',text='原生回调登录后回复三',seq='3');alice.command('received_read',seq='3');ios.command('summary',seq='3',status='read');phase('reply_read',3,1,2,2,1,2)
 for actor in actors.values():actor.screenshot('fresh-login-final-read')
 events=json.loads((out/'backend-api-events.json').read_text())
 for path,status,count in [('/api/v1/native/oauth/token',200,3),('/api/v1/native/oauth/refresh',200,0),('/api/v1/devices',201,3)]:
  if len([e for e in events if e['method']=='POST' and e['path']==path and e['status']==status])!=count:raise ValueError('fresh_login_api_counts')
 for actor in actors.values():
  if actor.command('done').get('cleanup') is not True:raise ValueError('fresh_login_cleanup')
 phase('done',3,1,2,2,1,2);(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
