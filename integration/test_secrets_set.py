import copy
import hashlib
import unittest

from secrets_set import INSPECT, PROBE, compare, digests, payload, response, secret_args


class SecretsSetTests(unittest.TestCase):
    def test_flags_precede_names(self):
        self.assertEqual(secret_args('set', 'fixture', ['key-one', 'key-two']),
                         ('secrets', 'set', '--stdin', '--json', '--server', 'local',
                          '--app', 'fixture', 'key-one', 'key-two'))

    def test_single_raw(self):
        self.assertEqual(payload({'key-one': b'a=b\n'}), b'a=b\n')

    def test_batch_literal(self):
        self.assertEqual(payload({'key-one': b'a=b', 'key-two': b' c '}),
                         b'key-one=a=b\nkey-two= c \n')

    def test_batch_rejects_newlines(self):
        with self.assertRaises(AssertionError):
            payload({'a': b'x\ny', 'b': b'z'})

    def test_response_shapes(self):
        response({'version': 1, 'secret': {'name': 'a'}, 'secret_applied': True}, ['a'], True)
        response({'version': 1, 'secret_versions': [{'name': 'b'}, {'name': 'a'}]}, ['a', 'b'])
        with self.assertRaises(AssertionError):
            response({'version': 1, 'secret_versions': [{'name': 'a'}, {'name': 'a'}]}, ['a', 'b'])
        with self.assertRaises(AssertionError):
            response({'version': 1, 'secret': {'name': 'a'}}, ['a'], True)

    def test_snapshot_scope_and_image(self):
        before = {'web': [{'id': 'one', 'image': 'sha', 'started': 'then'}],
                  'worker': [{'id': 'two', 'image': 'sha', 'started': 'then'}]}
        after = copy.deepcopy(before)
        after['web'][0]['id'] = 'new'
        compare(before, after, {'web'})
        with self.assertRaises(AssertionError):
            compare(before, after, set())
        after['web'][0]['image'] = 'different'
        with self.assertRaises(AssertionError):
            compare(before, after, {'web'})

    def test_digest_and_inspection_allowlist(self):
        self.assertEqual(digests({'ONE': b'example'}),
                         {'ONE': hashlib.sha256(b'example').hexdigest()})
        self.assertNotIn('.Config.Env', INSPECT)
        self.assertNotIn('os.environ.items', PROBE)
        self.assertNotIn('dict(os.environ)', PROBE)


if __name__ == '__main__':
    unittest.main()
