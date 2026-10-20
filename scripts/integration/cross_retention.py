"""Independent evidence gates for retained native recovery."""
def restart_proof(before,after):
 if after.get('restored_cursor')!='3' or type(after.get('pid')) is not int or after['pid']<=0 or after['pid']==before['pid'] or any(after.get(k)!=before[k] for k in ('device_id','storage','nonce')) or any(after.get(k) is not True for k in ('old_absent','preauth_hidden','cache_erased','vault_reused','renew_before_sync')):raise ValueError('retention_restart_unproven')
def policy_proof(row,days,version,*,message_days=None,message_version=None):
 message_days=days if message_days is None else message_days
 message_version=version if message_version is None else message_version
 if type(row.get('message_days')) is not int or row['message_days']!=message_days or row.get('file_days')!=str(days) or type(row.get('message_version')) is not int or row['message_version']!=message_version or row.get('file_version')!=str(version) or row.get('cleanup_enabled') is not False or row.get('existing_files') is not True:raise ValueError('retention_policy_unproven')
