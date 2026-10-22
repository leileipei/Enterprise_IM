"""Fresh-login admission contract; no customer-browser/SSO claim."""
from .cross_device import actor_config

def admission(row):
 expected={'fresh_login_button':True,'no_seed_refresh':True,'real_mobile_callback':True,'wrong_state_ignored':True,'callback_via_os':True,'callback_completions':1,'authorize_requests':1,'token_requests':1,'refresh_requests':0,'workspace_entered':True,'vault_saved':True,'same_process':True}
 if any(type(row.get(k)) is not type(v) or row[k]!=v for k,v in expected.items()):raise ValueError('fresh_native_login_unproven')

def bootstrap(config,role,nonce):
 result=actor_config(config,role,nonce);result.pop('refresh');result['fresh_login']=True;return result
