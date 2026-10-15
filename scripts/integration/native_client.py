"""Read-only native client preflight shared by owned backend runners."""
import os,platform,re,subprocess,sys
from .tools import verify_digest

def signing_choice_available(inventory,identity):
 if not identity:return False
 rows=re.findall(r'^\s*\d+\) ([A-Fa-f0-9]{40}) "([^"\n]+)"',inventory,re.MULTILINE)
 return any(identity==name or identity.lower()==fingerprint.lower() for fingerprint,name in rows)


def client_preflight(args):
 preflight={'target':args.target,'status':'not_run','fixtures_created':False}
 try:
  if args.target not in ('macos','host'):raise RuntimeError('unsupported_client_target')
  if any(not p.is_absolute() for p in (args.flutter_sdk,args.pub_cache,args.go,args.output)):raise RuntimeError('client_paths_must_be_absolute')
  sdk_commit=subprocess.check_output(['git','-C',str(args.flutter_sdk),'rev-parse','HEAD'],text=True).strip()
  sdk_changes=subprocess.check_output(['git','-C',str(args.flutter_sdk),'status','--porcelain','--untracked-files=no'])
  if sdk_commit!='e8113bf45620cbeb8aff64947ee4c93e16adb4cf' or sdk_changes:raise RuntimeError('fixed_sdk_source_mismatch')
  preflight['sdk_commit']=sdk_commit
  if not args.pub_cache.is_dir() or not args.go.is_file():raise RuntimeError('prepared_cache_or_go_missing')
  if args.target=='macos':
   if sys.platform!='darwin' or platform.machine()!='arm64':raise RuntimeError('native_macos_arm64_required')
   if not args.isolated_test_account:raise RuntimeError('empty_disposable_os_profile_must_be_confirmed')
   task_env=dict(os.environ,DEVELOPER_DIR='/Applications/Xcode.app/Contents/Developer')
   terms=subprocess.run(['/usr/bin/xcodebuild','-checkFirstLaunchStatus'],env=task_env,capture_output=True,timeout=30)
   preflight['xcode_first_launch_actual_exit']=terms.returncode
   if terms.returncode:raise RuntimeError('operator_xcode_first_launch_required')
   identities=subprocess.run(['/usr/bin/security','find-identity','-v','-p','codesigning'],capture_output=True,text=True,timeout=30)
   preflight['signing_identity_check_actual_exit']=identities.returncode
   if identities.returncode or '0 valid identities found' in identities.stdout:raise RuntimeError('existing_development_signing_identity_required')
   team=os.environ.get('FLUTTER_XCODE_DEVELOPMENT_TEAM','')
   identity=os.environ.get('FLUTTER_XCODE_CODE_SIGN_IDENTITY','')
   if not team or not identity:raise RuntimeError('operator_team_and_identity_selection_required')
   if not signing_choice_available(identities.stdout,identity):raise RuntimeError('selected_identity_not_in_valid_identity_inventory')
  preflight['status']='ready'
 except Exception as exc:
  preflight['blocked_reason']=str(exc) if isinstance(exc,RuntimeError) else type(exc).__name__
  return preflight
 return preflight


def prepared_file_inputs(mc,definitions,pins):
 if mc is None or not mc.is_absolute() or mc.is_symlink() or not mc.is_file():
  raise RuntimeError('prepared_mc_missing')
 if definitions is None or not definitions.is_absolute() or definitions.is_symlink() or not definitions.is_dir():
  raise RuntimeError('prepared_scanner_definitions_missing')
 for name in ('main.cvd','daily.cvd','bytecode.cvd'):
  path=definitions/name
  if path.is_symlink() or not path.is_file():raise RuntimeError('prepared_scanner_definitions_missing')
 verify_digest(mc,pins['mc-test-client-sha256-darwin-arm64'])
 return mc,definitions
