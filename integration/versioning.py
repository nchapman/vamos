"""Check bundle pairing before any Vamos bootstrap in the owned disposable guest."""
import re
from pathlib import Path


def mismatched_executor(data):
    matches = list(re.finditer(rb'vamos-bundle-v1:([a-f0-9]{64}):[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}:end-vamos-bundle', data))
    if len(matches) != 1:
        raise AssertionError('expected one executor build marker')
    offset = matches[0].start(1)
    replacement = b'0' if data[offset:offset + 1] != b'0' else b'1'
    return data[:offset] + replacement + data[offset + 1:]


def run(h, executor):
    client_version = h.cli('version').stdout
    executor_version = h.command([executor, 'version']).stdout
    if client_version != executor_version or b'(build ' not in client_version:
        raise AssertionError('client/executor build identities do not match')
    owned_paths = [Path('/var/lib/vamos/host.json'), Path('/usr/local/lib/vamos/executor')]
    if any(path.exists() for path in owned_paths):
        raise AssertionError('version check must precede Vamos bootstrap')
    stale = h.work / 'mismatched-executor'
    stale.write_bytes(mismatched_executor(executor.read_bytes()))
    try:
        h.cli('server', 'add', '--yes', '--domain', 'versioncheck.vamos.localhost',
              '--executor', str(stale), 'versioncheck', 'root@127.0.0.1',
              expect_error='does not match client build')
        if any(path.exists() for path in owned_paths):
            raise AssertionError('mismatched bundle mutated host ownership')
    finally:
        stale.unlink()
    h.log('Bundle versions match; mismatched executor refused before host bootstrap')
