"""Independent native group/file/device checks; no native result synthesis."""
import hashlib
PAYLOAD='跨端独立进程附件\nGroup file integrity across Android and iOS.\n'.encode()
PAYLOAD_SHA=hashlib.sha256(PAYLOAD).hexdigest()
# messages, alice D/R, bob-ios D/R, bob-android D/R, userAliceR/userBobR.
MATRIX={'registered':(0,0,0,0,0,0,0,0,0),'created':(0,0,0,0,0,0,0,0,0),'group_sent':(1,0,0,0,0,0,0,0,0),'both_delivered':(1,0,0,1,0,1,0,0,0),'both_read':(1,0,0,1,1,1,1,0,1),'reply_read':(2,1,1,1,1,1,1,1,1),'file_delivered':(3,1,1,2,2,2,2,1,2),'ios_downloaded':(3,1,1,2,2,2,2,1,2),'both_downloaded':(3,1,1,2,2,2,2,1,2),'device_revoked':(3,1,1,2,2,2,2,1,2),'terminal':(3,1,1,2,2,2,2,1,2),'survivor':(4,1,1,3,3,2,2,1,3),'stable':(4,1,1,3,3,2,2,1,3),'done':(4,1,1,3,3,2,2,1,3)}
ROLES=['alice','bob_ios','bob_android']
def expected_facts(phase):
 row=MATRIX[phase];file=phase in list(MATRIX)[6:];downloads=2 if phase in list(MATRIX)[8:] else 1 if phase=='ios_downloaded' else 0
 return {'messages':row[0],'devices':3,'groups':int(phase!='registered'),'memberships':0 if phase=='registered' else 2,'organizations':0 if phase=='registered' else 2,'files_ready':int(file),'attachments':int(file),'scan_completed':int(file),'downloads_completed':downloads,'downloads_acked':downloads,'revoked':int(phase in list(MATRIX)[9:]),'roles':{role:dict(zip(['delivered','read'],row[1+i*2:3+i*2])) for i,role in enumerate(ROLES)},'users':{'alice':row[7],'bob':row[8]}}
def checkpoint(phase,facts):
 def integers(value):
  return all(integers(v) for v in value.values()) if isinstance(value,dict) else type(value) is int
 if phase not in MATRIX or facts!=expected_facts(phase) or not integers(facts):raise ValueError('group_file_device_checkpoint_mismatch')
def download(details,device):
 if details.get('sha256')!=PAYLOAD_SHA or type(details.get('size')) is not int or details['size']!=len(PAYLOAD) or details.get('device_id')!=device:raise ValueError('independent_attachment_content_unverified')
def terminal(details):
 expected={'rejected':['sync','delivered','read','messages','download','renew'],'terminal':True,'private_artifacts_absent':True,'received_cleared':True,'can_send':False}
 if any(details.get(k)!=v for k,v in expected.items()):raise ValueError('revoked_native_device_terminal_unverified')

def selection(role,details):
 if role=='alice':
  if details.get('group_role')!='owner' or not isinstance(details.get('organizations'),list) or len(set(details['organizations']))!=2:raise ValueError('cross_group_owner_roster_unverified')
 elif details.get('group_role')!='member' or details.get('roster_denial')!={'status':403,'code':'group_permission_denied'} or 'organizations' in details:raise ValueError('cross_group_member_roster_scope_unverified')
