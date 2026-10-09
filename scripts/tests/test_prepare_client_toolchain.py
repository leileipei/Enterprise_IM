import importlib.util
import unittest
from pathlib import Path

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('client_toolchain',ROOT/'scripts/prepare-client-toolchain.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)

class ClientToolchainPinsTests(unittest.TestCase):
 def setUp(self):
  self.pub=(ROOT/'clients/enterprise_im/pubspec.yaml').read_text()
  self.lock=(ROOT/'clients/enterprise_im/pubspec.lock').read_text()
 def test_actual_client_exact_pins_and_archive_hashes(self):
  self.assertEqual(set(module.dependency_pins(self.pub,self.lock)),set(module.PINS))
 def test_unapproved_direct_package_cannot_be_hidden_by_valid_eight_pins(self):
  changed=self.pub.replace('dependencies:\n','dependencies:\n  unapproved_package: 1.0.0\n',1)
  with self.assertRaises(ValueError):module.dependency_pins(changed,self.lock)
 def test_version_source_and_hash_changes_fail(self):
  for name,(version,sha) in module.PINS.items():
   with self.subTest(package=name):
    with self.assertRaises(ValueError):module.dependency_pins(self.pub.replace('  '+name+': '+version,'  '+name+': ^'+version),self.lock)
    with self.assertRaises(ValueError):module.dependency_pins(self.pub,self.lock.replace(sha,'0'*64))

 def test_missing_or_changed_gradle_checksum_is_rejected(self):
  actual=(ROOT/'clients/enterprise_im/android/gradle/wrapper/gradle-wrapper.properties').read_text()
  self.assertEqual(module.gradle_distribution(actual)[1],module.GRADLE_SHA256)
  for changed in [actual.replace(module.GRADLE_SHA256,'0'*64), '\n'.join(x for x in actual.splitlines() if not x.startswith('distributionSha256Sum='))]:
   with self.assertRaises(ValueError):module.gradle_distribution(changed)

if __name__=='__main__':unittest.main()
