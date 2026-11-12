import http.client
import json
import threading
import unittest
from unittest.mock import MagicMock, patch
from http.server import ThreadingHTTPServer

import ledger
import migrate


class ValidationTests(unittest.TestCase):
    def test_valid_signed_amount(self):
        self.assertEqual(ledger.validate_entry({'amount_cents': -25, 'description': "O'Reilly"}), (-25, "O'Reilly"))

    def test_invalid(self):
        for amount in (True, 1.5, '1', 2**63, -2**63-1, None):
            with self.subTest(amount=amount), self.assertRaises(ValueError):
                ledger.validate_entry({'amount_cents': amount, 'description': 'ok'})
        for description in ('', ' ', 'a'*501, '\0', None):
            with self.subTest(description=description), self.assertRaises(ValueError):
                ledger.validate_entry({'amount_cents': 1, 'description': description})
        for value in ([], {}, {'amount_cents': 1, 'description': 'ok', 'extra': 1}):
            with self.assertRaises(ValueError):
                ledger.validate_entry(value)

    def test_parameterized_insert(self):
        conn = MagicMock()
        conn.__enter__.return_value = conn
        conn.execute.return_value.fetchone.return_value = (1, 2, "'); DROP TABLE entries;--")
        with patch('ledger.connect', return_value=conn):
            ledger.query('/entries', (2, "'); DROP TABLE entries;--"))
        sql, params = conn.execute.call_args.args
        self.assertNotIn('DROP TABLE', sql)
        self.assertEqual(params[1], "'); DROP TABLE entries;--")


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(('127.0.0.1', 0), ledger.Handler)
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()
        self.addCleanup(self.stop)

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, method, path, body=None, headers=None):
        conn = http.client.HTTPConnection(*self.server.server_address, timeout=3)
        try:
            conn.request(method, path, body, headers or {})
            response = conn.getresponse()
            return response.status, json.loads(response.read())
        finally:
            conn.close()

    def test_errors_and_success(self):
        with patch('ledger.query') as query:
            self.assertEqual(self.request('GET', '/missing')[0], 404)
            self.assertEqual(self.request('POST', '/entries', 'x'*4097)[0], 413)
            self.assertEqual(self.request('POST', '/entries', '{}')[0], 415)
            self.assertEqual(self.request('POST', '/entries', '{', {'Content-Type': 'application/json'})[0], 400)
            query.assert_not_called()
            query.return_value = {'id': 1}
            self.assertEqual(self.request('POST', '/entries', '{"amount_cents":1,"description":"ok"}', {'Content-Type': 'application/json'}), (201, {'id': 1}))
            query.side_effect = RuntimeError('secret-password')
            status, body = self.request('GET', '/readyz')
            self.assertEqual(status, 503)
            self.assertNotIn('secret-password', str(body))


class MigrationTests(unittest.TestCase):
    def test_retry_then_transaction(self):
        conn = MagicMock()
        connector = MagicMock(side_effect=[OSError(), conn])
        sleep = MagicMock()
        migrate.migrate(connector, sleep, attempts=2)
        sleep.assert_called_once_with(1)
        self.assertIn('IF NOT EXISTS', conn.execute.call_args_list[0].args[0])
        conn.__exit__.assert_called_once_with(None, None, None)

    def test_bounded_failure(self):
        connector = MagicMock(side_effect=OSError())
        sleep = MagicMock()
        with self.assertRaises(OSError):
            migrate.migrate(connector, sleep, attempts=3)
        self.assertEqual(connector.call_count, 3)
        self.assertEqual(sleep.call_count, 2)

    def test_schema_failure_not_retried(self):
        conn = MagicMock()
        conn.execute.side_effect = RuntimeError('schema error')
        connector = MagicMock(return_value=conn)
        with self.assertRaises(RuntimeError):
            migrate.migrate(connector, MagicMock())
        connector.assert_called_once()
        self.assertIs(conn.__exit__.call_args.args[0], RuntimeError)

    def test_main_redacts_errors(self):
        with patch('migrate.migrate', side_effect=RuntimeError('password')), patch('sys.stderr') as stderr:
            self.assertEqual(migrate.main(), 1)
        self.assertNotIn('password', str(stderr.write.call_args_list))


if __name__ == '__main__':
    unittest.main()
