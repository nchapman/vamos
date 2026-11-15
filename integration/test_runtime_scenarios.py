"""Offline assertion tests; no Docker, SSH, server, or guest startup."""
import copy
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import runtime_scenarios as runtime


def status_fixture():
    return {'operation': 'parent', 'services': {
        'web': {'running': True, 'available': True, 'instances': [
            {'ordinal': 1, 'container': 'web-one', 'running': True, 'available': True},
            {'ordinal': 2, 'container': 'web-two', 'running': True, 'available': True},
        ]},
        'database': {'running': True, 'available': True, 'instances': [
            {'ordinal': 1, 'container': 'db-one', 'running': True, 'available': True},
        ]},
    }, 'release': {'operation': 'hook', 'parent_operation': 'parent',
                   'service': 'web', 'phase': 'complete', 'exit_code': 0}}


class RuntimeAssertionTests(unittest.TestCase):
    def test_requires_exact_instance_schema_and_membership(self):
        status = status_fixture()
        self.assertEqual(runtime.instances(status, {'web': 2, 'database': 1}),
                         {'web': ('web-one', 'web-two'), 'database': ('db-one',)})
        mutations = [
            lambda s: s['services'].pop('database'),
            lambda s: s['services']['web'].pop('instances'),
            lambda s: s['services']['web']['instances'].pop(),
            lambda s: s['services']['web']['instances'][1].update(ordinal=1),
            lambda s: s['services']['web']['instances'][1].update(container='web-one'),
            lambda s: s['services']['web']['instances'][0].update(running=False),
            lambda s: s['services']['web']['instances'][0].update(available=False),
            lambda s: s['services']['web'].update(available=False),
        ]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                broken = copy.deepcopy(status)
                mutate(broken)
                with self.assertRaises(AssertionError):
                    runtime.instances(broken, {'web': 2, 'database': 1})

    def test_index_is_not_silently_accepted_in_place_of_ordinal(self):
        status = status_fixture()
        item = status['services']['web']['instances'][0]
        item['index'] = item.pop('ordinal')
        with self.assertRaises(KeyError):
            runtime.instances(status, {'web': 2, 'database': 1})

    def test_docker_limits_reject_missing_or_unlimited_values(self):
        good = {'cpus': 500_000_000, 'memory': 536_870_912, 'swap': 536_870_912}
        runtime.resources(good)
        for key in good:
            for value in (0, -1, good[key] * 2):
                with self.subTest(key=key, value=value):
                    with self.assertRaises(AssertionError):
                        runtime.resources(dict(good, **{key: value}))

    def test_kernel_limits_are_required_not_just_docker_metadata(self):
        runtime.cgroup_limits(b'50000 100000\n536870912\n0\n')
        for output in (b'100000 100000\n536870912\n0\n',
                       b'50000 100000\n1073741824\n0\n',
                       b'50000 100000\n536870912\n536870912\n',
                       b'max 100000\nmax\nmax\n'):
            with self.subTest(output=output):
                with self.assertRaises((AssertionError, ValueError)):
                    runtime.cgroup_limits(output)

    def test_release_requires_separate_operation_and_completed_exit(self):
        status = status_fixture()
        self.assertEqual(runtime.release_result(status, 0)['operation'], 'hook')
        for changes in ({'operation': 'parent'}, {'operation': ''},
                        {'parent_operation': 'other'}, {'service': 'worker'},
                        {'phase': 'running'}, {'exit_code': None}, {'exit_code': 23}):
            with self.subTest(changes=changes):
                broken = copy.deepcopy(status)
                broken['release'].update(changes)
                with self.assertRaises(AssertionError):
                    runtime.release_result(broken, 0)
        status['release']['exit_code'] = 23
        runtime.release_result(status, 23)
        with self.assertRaises(KeyError):
            runtime.release_result({'status': status}, 23)  # Harness returns raw Status.

    def test_async_effect_requires_worker_evidence(self):
        values = iter([{'status': 'pending'}, {'status': 'processed',
                                             'processed_at': 'now', 'worker_marker': 'worker'}])
        pauses = []
        result = runtime.wait_processed(lambda: next(values), clock=lambda: 0, sleep=pauses.append)
        self.assertEqual(result['worker_marker'], 'worker')
        self.assertEqual(pauses, [0.5])
        with self.assertRaisesRegex(AssertionError, 'incomplete worker'):
            runtime.wait_processed(lambda: {'status': 'processed', 'processed_at': None,
                                            'worker_marker': ''})

    def test_async_deadline_fails_not_skips(self):
        ticks = iter([0, 91])
        with self.assertRaisesRegex(AssertionError, 'deadline'):
            runtime.wait_processed(lambda: {'status': 'pending'}, clock=lambda: next(ticks),
                                   sleep=lambda _: self.fail('slept past deadline'))
        with self.assertRaisesRegex(AssertionError, 'unknown processing'):
            runtime.wait_processed(lambda: {'status': 'failed'})

    def test_http_needs_200_marker_and_json(self):
        self.assertEqual(runtime.parse_http(b'HTTP/1.1 200 OK\r\nx-demo-instance: opaque\r\n\r\n{"id":1}'),
                         ('opaque', {'id': 1}))
        for response in (b'HTTP/1.1 302 Redirect\r\nX-Demo-Instance: x\r\n\r\n{}',
                         b'HTTP/2 200\r\nContent-Type: application/json\r\n\r\n{}'):
            with self.assertRaises(AssertionError):
                runtime.parse_http(response)

    def test_inspection_never_requests_secret_bearing_environment(self):
        self.assertNotIn('.Config.Env', runtime.INSPECT)
        self.assertNotIn('{{json .}}', runtime.INSPECT)
        for field in ('.HostConfig.NanoCpus', '.HostConfig.MemorySwap', '.Id', '.State.Running'):
            self.assertIn(field, runtime.INSPECT)

    def test_database_is_private_manual_singleton_with_volume(self):
        db = runtime.database('ledger', 'ledger')
        self.assertEqual(db['update'], 'manual')
        self.assertEqual(db['resources'], {'cpus': 0.5, 'memory_mb': 512})
        self.assertEqual(db['volume']['mount'], '/var/lib/postgresql/data')
        self.assertNotIn('replicas', db)
        self.assertNotIn('http', db)
        self.assertNotIn('POSTGRES_PASSWORD', db['env'])


class ProjectEvidenceTests(unittest.TestCase):
    def test_web_uses_generated_app_hostname_without_service_suffix(self):
        with TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'examples' / 'fixture').mkdir(parents=True)
            h = SimpleNamespace(root=root, work=root / 'work', hosts=lambda names: None)
            with patch.object(runtime.secrets, 'token_hex', return_value='12345678'):
                project = runtime.Project(h, 'fixture', 'runtime-')
            self.assertEqual(project.host, 'runtime-12345678.vamos.localhost')

    def test_stable_rejects_restart_even_when_docker_id_did_not_change(self):
        project = object.__new__(runtime.Project)
        before = {'web': [{'id': 'same', 'started': 'before'}]}
        after = {'web': [{'id': 'same', 'started': 'after'}]}
        with patch.object(project, 'snapshot', return_value=({}, after)):
            with self.assertRaisesRegex(AssertionError, 'restarted'):
                project.stable(before, {'web': 1})
        with patch.object(project, 'snapshot', return_value=({}, copy.deepcopy(before))):
            self.assertEqual(project.stable(before, {'web': 1})[1], before)

    def test_release_requires_one_actual_docker_runner(self):
        project = object.__new__(runtime.Project)
        status = status_fixture()
        status['release']['container'] = 'vamos-run-hook'
        info = {'id': 'docker-id', 'running': False, 'cpus': 500_000_000,
                'memory': 536_870_912, 'swap': 536_870_912}
        for output in (b'', b'docker-id\nsecond-id\n', b'different-id\n'):
            project.h = SimpleNamespace(command=lambda *a, **kw: SimpleNamespace(stdout=output))
            with patch.object(project, 'inspect', return_value=info), \
                    patch.object(project, 'cli', return_value=SimpleNamespace(stdout=b'logs')):
                with self.assertRaisesRegex(AssertionError, 'once per operation'):
                    project.release(status)
        project.h = SimpleNamespace(command=lambda *a, **kw: SimpleNamespace(stdout=b'docker-id\n'))
        with patch.object(project, 'inspect', return_value=info), \
                patch.object(project, 'cli', return_value=SimpleNamespace(stdout=b'logs')) as cli:
            self.assertEqual(project.release(status)[2], b'logs')
            cli.assert_called_once_with('logs', '--run', 'hook')


class RailsDatabaseTests(unittest.TestCase):
    def test_separate_test_database_and_restored_production_context(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / '.dockerignore'
            path.write_text('/test\n')
            def run(*args):
                self.assertIn('!/test/**', path.read_text())
                self.assertIn('RAILS_ENV=test', args)
                self.assertIn('DB_TEST_NAME=rails_ledger_test', args)
                self.assertEqual(args[-3:], ('sh', '-c', 'bin/rails db:create db:migrate && exec bin/rails test'))
                return SimpleNamespace(stdout=b'12 runs, 70 assertions, 0 failures, 0 errors, 0 skips\n', stderr=b'')
            project = SimpleNamespace(directory=Path(directory), cli=run)
            runtime.rails_database_tests(project)
            self.assertEqual(path.read_text(), '/test\n')

    def test_rejects_aborted_setup_even_with_passing_tests(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / '.dockerignore'
            path.write_text('/test\n')
            passed = b'12 runs, 86 assertions, 0 failures, 0 errors, 0 skips\n'
            aborted = b'bin/rails aborted!\nActiveRecord::StatementInvalid: PG::ObjectInUse\n'
            for stdout, stderr in ((passed + aborted, b''), (passed, aborted)):
                project = SimpleNamespace(directory=Path(directory), cli=lambda *args:
                                          SimpleNamespace(stdout=stdout, stderr=stderr))
                with self.assertRaisesRegex(AssertionError, 'aborted'):
                    runtime.rails_database_tests(project)
                self.assertEqual(path.read_text(), '/test\n')

    def test_failed_or_missing_suite_never_leaves_tests_in_production_context(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / '.dockerignore'
            path.write_text('/test\n')
            for output in (b'', b'0 runs, 0 assertions, 0 failures, 0 errors, 0 skips\n',
                           b'12 runs, 70 assertions, 1 failures, 0 errors, 0 skips\n'):
                project = SimpleNamespace(directory=Path(directory),
                                          cli=lambda *args: SimpleNamespace(stdout=output, stderr=b''))
                with self.assertRaises(AssertionError):
                    runtime.rails_database_tests(project)
                self.assertEqual(path.read_text(), '/test\n')


class SummaryTests(unittest.TestCase):
    def test_reports_both_required_scenarios_without_overwriting_existing_report(self):
        with TemporaryDirectory() as directory:
            h = SimpleNamespace(artifacts=Path(directory), log=lambda _: None)
            previous = h.artifacts / 'summary.json'
            previous.write_text('existing report')
            def complete(h, summary, done):
                done('actual mock completion')
            with patch.object(runtime, 'rails', side_effect=complete) as rails, \
                    patch.object(runtime, 'python_ledger', side_effect=complete) as ledger:
                runtime.run(h)
            rails.assert_called_once()
            ledger.assert_called_once()
            report = json.loads((h.artifacts / 'runtime-summary.json').read_text())
            self.assertTrue(report['passed'])
            self.assertEqual(len(report['completed']), 2)
            self.assertEqual(previous.read_text(), 'existing report')
            self.assertNotIn('skipped', report)

    def test_failure_is_reported_and_propagated_never_marked_passed(self):
        with TemporaryDirectory() as directory:
            h = SimpleNamespace(artifacts=Path(directory), log=lambda _: None)
            with patch.object(runtime, 'rails', side_effect=AssertionError('missing replicas')), \
                    patch.object(runtime, 'python_ledger') as ledger:
                with self.assertRaisesRegex(AssertionError, 'missing replicas'):
                    runtime.run(h)
            ledger.assert_not_called()
            report = json.loads((h.artifacts / 'runtime-summary.json').read_text())
            self.assertFalse(report['passed'])
            self.assertEqual(report['completed'], [])
            self.assertEqual(report['failure'], 'AssertionError')


if __name__ == '__main__':
    unittest.main()
