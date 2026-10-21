"""Reject partial logout navigation or a rebuilt/reseeded cold-start proof."""
def logged_out_page(row):
 expected={'login_heading':True,'client_shell_count':0,'message_page_count':0,'workspace_entry':False,'restore_button':False,'account_absent':True,'vault_absent':True,'database_files_absent':True,'login_button_enabled':True}
 if any(row.get(k)!=v for k,v in expected.items()):raise ValueError('logout_navigation_incomplete')

def cold_start(before,after):
 if type(after.get('pid')) is not int or after['pid']<=0 or after['pid']==before['pid'] or after.get('device')!=before['device'] or after.get('storage')!=before['storage'] or after.get('old_absent') is not True or after.get('no_seed_refresh') is not True or after.get('login_api_creations')!=0:raise ValueError('logout_cold_start_not_proven')
