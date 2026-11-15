"""Recover a client registration inside the already-owned acceptance guest."""
from contextlib import contextmanager


@contextmanager
def fresh_client(h):
    """Use an empty registry while retaining independently trusted SSH material."""
    directory = h.work / 'recovered-client-config'
    if directory.exists():
        raise AssertionError('recovery client directory must start absent')
    previous = h.env
    h.env = dict(previous, XDG_CONFIG_HOME=str(directory))
    try:
        h.cli('server', 'connect', '--domain', 'vamos.localhost', 'local', 'root@127.0.0.1')
        yield
    finally:
        h.env = previous
