"""Strict scope of an ended organizational membership in native clients."""
def terminal_proof(row,ended,other):
 expected={'latest_network_status':403,'latest_network_code':'invalid_identity','context_cleared':True,'can_send':False,'seqs':[],'read_seqs':[],'cursor':None,'pending_receipts':0,'pending_sends':0,'ui_body_absent':True,'proof_was_real':True,'account_active':True,'remaining_memberships':[other],'same_device':True}
 if ended==other or any(row.get(k)!=v for k,v in expected.items()):raise ValueError('permission_native_scope_or_erasure')
 if row.get('rejections')!={a:{'status':403,'code':'invalid_identity'} for a in ['sync','history','delivered','read','send']}:raise ValueError('permission_old_context_not_rejected')

def database_proof(facts):
 expected={'messages':2,'devices':3,'revoked':0,'delivered':3,'read':3,'user_read':2,'ended_status':'ended','other_status':'active','user_status':'active','end_audits':1}
 if facts!=expected:raise ValueError('permission_database_scope_or_audit')
