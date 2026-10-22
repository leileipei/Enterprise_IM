"""Re-login must replace the erased device grant in the same UI/process."""
import uuid

def readmission(before,after,storage):
 if any(after.get(k)!=before[k] for k in ('role','platform','pid','user','nonce')):raise ValueError('relogin_owner_changed')
 try:
  old=str(uuid.UUID(before['device_id']));new=str(uuid.UUID(after['device_id']))
 except (KeyError,TypeError,ValueError,AttributeError):raise ValueError('relogin_device_invalid') from None
 if old==new:raise ValueError('relogin_reused_erased_device')
 row=after.get('details',{})
 expected={'old_device_id':old,'storage':storage,'relogin_from_same_page':True,'fresh_pkce':True,'wrong_state_ignored':True,'callback_via_os':True,'old_owner_stopped':True,'old_view_stopped':True,'vault_saved':True,'workspace_entered':True,'no_seed_refresh':True,'token_requests':2,'refresh_requests':0,'callback_completions':2,'authorize_requests':2}
 if any(type(row.get(k)) is not type(v) or row[k]!=v for k,v in expected.items()):raise ValueError('relogin_admission_unproven')

def relogin(actor,storage):
 before=dict(actor.state['admission']);actor.number+=1;actor.write('cross-command.json',{'id':actor.number,'action':'navigation_relogin'})
 row=actor.await_state(actor.number);readmission(before,row,storage)
 actor.state['admission']={k:row[k] for k in ('role','platform','nonce','pid','user','device_id')};actor.save()
 return {'before':before,'after':row}
