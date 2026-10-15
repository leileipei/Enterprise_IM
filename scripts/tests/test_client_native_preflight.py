import sys,tempfile,unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration.native_client import client_preflight

class ClientPreflightTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
  root=Path(self.tmp.name);(root/'cache').mkdir();(root/'go').touch()
  self.args=SimpleNamespace(target='host',flutter_sdk=root/'sdk',pub_cache=root/'cache',go=root/'go',output=root/'result',isolated_test_account=False)
 def probe(self):
  return patch('integration.native_client.subprocess.check_output',side_effect=['e8113bf45620cbeb8aff64947ee4c93e16adb4cf',b''])
 def test_unknown_target_cannot_fall_back_to_host(self):
  self.args.target='windows'
  with self.probe():result=client_preflight(self.args)
  self.assertEqual(result['status'],'not_run');self.assertEqual(result['blocked_reason'],'unsupported_client_target')
  self.assertFalse(self.args.output.exists())
 def test_relative_sdk_is_rejected_without_creating_output(self):
  self.args.flutter_sdk=Path('relative-sdk')
  with self.probe():result=client_preflight(self.args)
  self.assertEqual(result['status'],'not_run');self.assertEqual(result['blocked_reason'],'client_paths_must_be_absolute')
  self.assertFalse(self.args.output.exists())
 def test_changed_sdk_cannot_be_ready(self):
  with patch('integration.native_client.subprocess.check_output',side_effect=['0'*40,b'']):result=client_preflight(self.args)
  self.assertEqual(result['blocked_reason'],'fixed_sdk_source_mismatch')
 def test_host_preflight_does_not_require_native_profile(self):
  with self.probe():result=client_preflight(self.args)
  self.assertEqual(result['status'],'ready');self.assertFalse(result['fixtures_created'])
  self.assertFalse(self.args.output.exists())
 def test_native_without_profile_declaration_stops(self):
  self.args.target='macos'
  with self.probe(),patch('integration.native_client.sys.platform','darwin'),patch('integration.native_client.platform.machine',return_value='arm64'):
   result=client_preflight(self.args)
  self.assertEqual(result['blocked_reason'],'empty_disposable_os_profile_must_be_confirmed')
 def test_no_valid_identity_cannot_be_ready(self):
  self.args.target='macos';self.args.isolated_test_account=True
  with self.probe(),patch('integration.native_client.sys.platform','darwin'),patch('integration.native_client.platform.machine',return_value='arm64'),patch('integration.native_client.subprocess.run',side_effect=[SimpleNamespace(returncode=0),SimpleNamespace(returncode=0,stdout='0 valid identities found')]):
   result=client_preflight(self.args)
  self.assertEqual(result['status'],'not_run');self.assertFalse(result['fixtures_created'])
  self.assertEqual(result['blocked_reason'],'existing_development_signing_identity_required')

if __name__=='__main__':unittest.main()
