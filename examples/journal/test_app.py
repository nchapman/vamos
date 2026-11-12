import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import app


class InitializationTests(unittest.TestCase):
    def test_restart_preserves_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            app.initialize(root)
            with sqlite3.connect(root / 'journal.sqlite') as db:
                db.execute("INSERT INTO entries(text) VALUES ('marker')")
            (root / 'uploads' / 'probe').write_bytes(b'marker')
            app.initialize(root)
            with sqlite3.connect(root / 'journal.sqlite') as db:
                self.assertEqual(db.execute('SELECT text FROM entries').fetchall(), [('marker',)])
            self.assertEqual((root / 'uploads' / 'probe').read_bytes(), b'marker')

    def test_existing_incompatible_data_never_chowned(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'existing').write_bytes(b'keep')
            with patch.object(app, 'UID', -1), patch.object(app.os, 'chown') as chown, patch.object(app.os, 'chmod') as chmod:
                with self.assertRaises(RuntimeError):
                    app.initialize_permissions(root)
                chown.assert_not_called()
                chmod.assert_not_called()
            self.assertEqual((root / 'existing').read_bytes(), b'keep')


if __name__ == '__main__':
    unittest.main()
