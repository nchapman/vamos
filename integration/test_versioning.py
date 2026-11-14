import unittest

from versioning import mismatched_executor


class VersioningTests(unittest.TestCase):
    def test_changes_only_one_identity_byte(self):
        for first in (b'0', b'a'):
            data = b'ELF-prefix-soar-bundle-v1:' + first * 64 + b':dev:end-soar-bundle-suffix'
            changed = mismatched_executor(data)
            self.assertEqual(len(changed), len(data))
            self.assertEqual(sum(a != b for a, b in zip(data, changed)), 1)
            self.assertTrue(changed.endswith(b':dev:end-soar-bundle-suffix'))

    def test_refuses_missing_or_ambiguous_markers(self):
        stamp = b'soar-bundle-v1:' + b'a' * 64 + b':dev:end-soar-bundle'
        for data in (b'raw executable', stamp + stamp):
            with self.assertRaises(AssertionError):
                mismatched_executor(data)
