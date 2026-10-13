import importlib.util
import unittest
from pathlib import Path

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('macos_backend',ROOT/'scripts/test-client-macos-backend.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)

class NativeSigningSelectionTests(unittest.TestCase):
 INVENTORY='  1) AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "Apple Development: Owned Fixture (TEAM1)"\n     1 valid identities found\n'
 def test_exact_hash_and_full_name_are_accepted(self):
  self.assertTrue(module.signing_choice_available(self.INVENTORY,'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'))
  self.assertTrue(module.signing_choice_available(self.INVENTORY,'Apple Development: Owned Fixture (TEAM1)'))
 def test_partial_name_is_not_certificate_selection(self):
  self.assertFalse(module.signing_choice_available(self.INVENTORY,'Apple Development'))
 def test_team_substring_is_not_certificate_selection(self):
  self.assertFalse(module.signing_choice_available(self.INVENTORY,'TEAM1'))
 def test_summary_text_is_not_certificate_selection(self):
  self.assertFalse(module.signing_choice_available(self.INVENTORY,'valid identities found'))
 def test_empty_choice_and_no_valid_rows_are_rejected(self):
  self.assertFalse(module.signing_choice_available(self.INVENTORY,''))
  self.assertFalse(module.signing_choice_available('0 valid identities found','0'))
 def test_sha1_case_does_not_select_another_identity(self):
  self.assertTrue(module.signing_choice_available(self.INVENTORY,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'))

if __name__=='__main__':unittest.main()
