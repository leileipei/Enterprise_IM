"""Coordinate real actor commands and independent PostgreSQL group/file facts."""
import json,re,time,uuid
from .cross_group import checkpoint,download,terminal,PAYLOAD,selection

def run(actors,backend,out,result,save):
 schema=backend.config['schema']
 if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('group_schema_unverified')
 rows=[]
 for role,actor in actors.items():
  device=str(uuid.UUID(actor.state['admission']['device_id']))
  rows.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,device,device))
 alice_user=str(uuid.UUID(backend.config['actors']['alice']['user']));bob_user=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
 scalars={'messages':'SELECT count(*) FROM messages','devices':'SELECT count(*) FROM user_devices','groups':"SELECT count(*) FROM conversations WHERE kind='group'",'memberships':'SELECT count(*) FROM conversation_membership_intervals WHERE leave_seq IS NULL','organizations':'SELECT count(DISTINCT source_organization_id) FROM conversation_membership_intervals WHERE leave_seq IS NULL','files_ready':"SELECT count(*) FROM file_objects WHERE state='ready'",'attachments':'SELECT count(*) FROM message_attachments','scan_completed':"SELECT count(*) FROM file_scan_jobs WHERE status='completed'",'downloads_completed':"SELECT count(*) FROM file_download_sessions WHERE phase='completed'",'downloads_acked':"SELECT count(*) FROM file_download_sessions WHERE phase='completed' AND audit_acked",'revoked':"SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL"}
 sql='SET search_path TO '+schema+",public; SELECT json_build_object("+','.join("'%s',(%s)"%(k,v) for k,v in scalars.items())+",'roles',json_build_object("+','.join(rows)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+alice_user+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+bob_user+"' AND read_at IS NOT NULL)));"
 def phase(name):
  deadline=time.monotonic()+45
  while True:
   facts=json.loads(backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input=sql))
   try:checkpoint(name,facts);break
   except ValueError:
    if time.monotonic()>deadline:(out/('failed-checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');raise
    time.sleep(.2)
  (out/('checkpoint-'+name+'.json')).write_text(json.dumps(facts,indent=2)+'\n');result['checkpoints'].append(name);save()
 def device_events():
  records=json.loads((out/'backend-api-events.json').read_text());return [e for e in records if e.get('device')==android.state['admission']['device_id']]
 alice,ios,android=(actors[k] for k in ('alice','bob_ios','bob_android'))
 phase('registered')
 created=alice.command('create_group',bob_membership=backend.config['actors']['bob_ios']['membership']);group=str(uuid.UUID(created['details']['group_id']));result['group_id']=group;phase('created')
 selection('alice',alice.command('select_group',group_id=group)['details']);alice.command('send',text='独立群消息一',seq='1');phase('group_sent')
 selection('bob_ios',ios.command('select_group',group_id=group)['details']);selection('bob_android',android.command('select_group',group_id=group)['details']);phase('both_delivered')
 ios.command('read',seqs=['1']);android.command('read',seqs=['1']);phase('both_read');alice.command('summary',seq='1',status='read')
 ios.command('send',text='跨组织群回复二',seq='2');alice.command('received_read',seq='2');phase('reply_read')
 sent=alice.command('file_send',contents=PAYLOAD.decode());result['file_id']=sent['details']['file_id']
 ios.command('received_read',seq='3');android.command('received_read',seq='3');phase('file_delivered');alice.command('summary',seq='3',status='read')
 for actor,name in [(ios,'ios_downloaded'),(android,'both_downloaded')]:
  observed=actor.command('download',contents=PAYLOAD.decode());download(observed['details'],actor.state['admission']['device_id']);phase(name)
  actor.screenshot('attachment-received')
 present=android.command('cache_present')
 if present['details'].get('private_artifact_present') is not True:raise ValueError('private_file_not_present_before_revoke')
 ios.command('revoke_other',device_id=android.state['admission']['device_id']);phase('device_revoked')
 stopped=android.command('terminal');terminal(stopped['details']);phase('terminal');android.screenshot('device-invalid')
 # Allow already-in-flight terminal responses to settle before the quiet window.
 time.sleep(2);before=device_events();result['revoked_device_requests_before_quiet']=len(before);save()
 alice.command('send',text='撤销后有效设备继续接收四',seq='4');ios.command('received_read',seq='4');phase('survivor');alice.command('summary',seq='4',status='read')
 time.sleep(6);android.command('terminal_stable');after=device_events()
 if len(after)!=len(before):raise ValueError('revoked_device_automatic_requests_continued')
 result['revoked_device_quiet']={'settle_wait_seconds':2,'minimum_quiet_wait_seconds':6,'before':len(before),'after':len(after)};phase('stable')
 alice.screenshot('group-file-sender');ios.screenshot('surviving-device')
 for actor in actors.values():
  done=actor.command('done')
  if done.get('cleanup') is not True:raise ValueError('group_actor_cleanup_missing')
 phase('done')
 (backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
