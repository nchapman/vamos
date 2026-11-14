"""Pure driver assertions: never create/run a guest or container."""
import copy
import importlib.util
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
spec = importlib.util.spec_from_file_location('plan_acceptance', Path(__file__).with_name('plan_acceptance.py'))
plan = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plan)


class PlanTests(unittest.TestCase):
    def fixture(self):
        status = {'services': {'web': {'incarnation': 'web-id'}, 'database': {'incarnation': 'db-id'}},
                  'volumes': [{'id': 'volume-id', 'service': 'web', 'missing': False}]}
        volume = {'id': 'volume-id', 'name': 'data', 'mount': '/data'}
        record = {'revision': 'a' * 64, 'bindings': {
            'sources': {'primary': {'service': 'database', 'incarnation': 'db-id', 'credential_service': 'database', 'credential_incarnation': 'db-id'},
                        'entries': {'service': 'web', 'incarnation': 'web-id', 'volume': volume},
                        'uploads': {'service': 'web', 'incarnation': 'web-id', 'volume': copy.deepcopy(volume)}},
            'stops': {'web': 'web-id'}, 'repositories': {'fixture-new': {'id': 'b' * 64, 'config_digest': 'c' * 64}}}}
        return record, status

    def test_fixture_has_distinct_service_owned_volumes(self):
        manifest = plan.fixture_manifest('plan-test')
        volumes = [s['volume']['name'] for s in manifest['services'].values()]
        self.assertEqual(len(volumes), len(set(volumes)))
        self.assertEqual(manifest['services']['web']['volume'], {'name': 'data', 'mount': '/data'})
        self.assertEqual(manifest['services']['database']['volume']['mount'], '/var/lib/postgresql/data')

    def test_all_bindings_and_drift(self):
        record, status = self.fixture()
        plan.validate_bindings(record, status, 'b' * 64)
        mutations = [
            lambda r: r['bindings']['sources'].pop('uploads'),
            lambda r: r['bindings']['sources']['primary'].update(credential_incarnation='new'),
            lambda r: r['bindings']['sources']['entries'].update(incarnation='new'),
            lambda r: r['bindings']['sources']['uploads']['volume'].update(id='other'),
            lambda r: r['bindings']['stops'].update(database='db-id'),
            lambda r: r['bindings']['repositories']['fixture-new'].update(id='wrong'),
            lambda r: r.update(revision='short'),
        ]
        for mutate in mutations:
            changed = copy.deepcopy(record)
            mutate(changed)
            with self.assertRaises(AssertionError):
                plan.validate_bindings(changed, status, 'b' * 64)

    def test_runtime_and_record_equality_is_exact(self):
        before = {'id': 'owned', 'started': 'original', 'restarts': 0, 'policy': {'Name': 'always'}}
        plan.unchanged(before, copy.deepcopy(before), 'runtime')
        for key, value in [('id', 'replacement'), ('started', 'later'), ('restarts', 1), ('policy', {})]:
            with self.assertRaises(AssertionError):
                plan.unchanged(before, dict(before, **{key: value}), 'runtime')
        with self.assertRaises(AssertionError):
            plan.unchanged(b'original record', b'replacement record', 'record')

    def test_private_record_requires_root_and_exact_mode(self):
        path = Mock()
        for uid, mode, good in [(0, 0o600, True), (1, 0o600, False), (0, 0o644, False)]:
            path.stat.return_value = Mock(st_uid=uid, st_mode=mode)
            if good:
                plan.private_root(path, 0o600)
            else:
                with self.assertRaises(AssertionError):
                    plan.private_root(path, 0o600)

    def test_guest_gate_precedes_all_commands(self):
        harness = Mock()
        with patch.object(plan, 'OWNER') as owner, patch.object(plan, 'require_guest', side_effect=RuntimeError('not owned')):
            owner.read_text.return_value = 'a' * 32
            with self.assertRaisesRegex(RuntimeError, 'not owned'):
                plan.run(harness, 'b' * 64, {})
        harness.command.assert_not_called()


if __name__ == '__main__':
    unittest.main()
