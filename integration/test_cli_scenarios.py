import os
import sys
import unittest

from cli_scenarios import Capture


@unittest.skipIf(sys.platform == 'win32', 'select on pipes is Unix-only; guest uses Linux')
class CaptureTests(unittest.TestCase):
    def test_reads_binary_events_until_marker(self):
        read, write = os.pipe()
        try:
            os.write(write, b'\x00\xffprefix marker suffix')
            output = Capture([read]).until(b'marker')
            self.assertEqual(output, b'\x00\xffprefix marker suffix')
        finally:
            os.close(write)
            os.close(read)

    def test_eof_is_failure_not_success(self):
        read, write = os.pipe()
        os.close(write)
        try:
            with self.assertRaisesRegex(AssertionError, 'did not produce'):
                Capture([read]).until(b'absent')
        finally:
            os.close(read)

    def test_deadline_is_bounded(self):
        read, write = os.pipe()
        try:
            with self.assertRaisesRegex(AssertionError, 'did not produce'):
                Capture([read]).until(b'absent', timeout=0)
        finally:
            os.close(write)
            os.close(read)
