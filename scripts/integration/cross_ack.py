"""Strict independent evidence contracts; absent facts never become success."""
def lost_ack(v):
 if v.get('upstream_status')!=200 or v.get('downstream_dropped') is not True or v.get('duplicate') is not False or not v.get('pending_id') or v.get('pending_id')!=v.get('db_client_id') or v.get('native_ack') is not None or v.get('network_status') is not None or v.get('network_error')!='unavailable':raise ValueError('lost_ack_unproven')

def recovered_ack(before,after):
 if any(not before.get(k) or after.get(k)!=before[k] for k in ('pending_id','device_id','storage','message_id','seq')) or type(after.get('pid')) is not int or after['pid']<=0 or after['pid']==before['pid'] or any(after.get(k) is not True for k in ('old_absent','vault_reused','preauth_hidden','duplicate','renew_before_sync')) or after.get('automatic_posts')!=0 or after.get('pending_after')!=0:raise ValueError('recovered_ack_unproven')
