#!/usr/bin/env python3
"""Guest-only external command gate. This file is also the installed wrapper."""
import json
import math
import os
import socket
import subprocess
import sys

CONFIG = '/run/vamos-integration/gate.json'
MAX_EVENT = 65536
RELEASE = b'R'


def matches(config, name, args):
    if name == 'docker' and args[:2] == ['--host', 'unix:///var/run/docker.sock']:
        args = args[2:]
    prefix = config.get('prefix')
    return (name in ('docker', 'caddy') and config.get('command') == name
            and isinstance(prefix, list) and bool(prefix)
            and all(isinstance(a, str) for a in prefix)
            and args[:len(prefix)] == prefix
            and (config.get('contains') is None or config['contains'] in args)
            and config.get('when') in ('before', 'after'))


def handshake(config, args):
    payload = json.dumps(dict(token=config['token'], pid=os.getpid(), argv=args)).encode() + b'\n'
    if len(payload) > MAX_EVENT:
        raise RuntimeError('gate event too large')
    timeout = config.get('timeout', 120)
    if type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout <= 1200:
        raise RuntimeError('invalid gate timeout')
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        peer.settimeout(timeout)
        peer.connect(config['socket'])
        peer.sendall(payload)
        if peer.recv(1) != RELEASE:
            raise RuntimeError('gate closed without release')


def gate_execute(name, args, realpath, configpath):
    try:
        with open(configpath) as stream:
            config = json.load(stream)
    except FileNotFoundError:
        config = {}
    if not matches(config, name, args):
        os.execv(realpath, [realpath, *args])
    if config['when'] == 'before':
        handshake(config, args)
        os.execv(realpath, [realpath, *args])
    result = subprocess.run([realpath, *args])  # Inherit all streams, especially tar/secrets stdin.
    if result.returncode == 0:
        handshake(config, args)
    if result.returncode < 0:
        import signal
        signal.signal(-result.returncode, signal.SIG_DFL)
        os.kill(os.getpid(), -result.returncode)
    return result.returncode


def main():
    name = os.path.basename(sys.argv[0])
    if name not in ('docker', 'caddy'):
        raise RuntimeError('unsupported wrapper name')
    return gate_execute(name, sys.argv[1:], '/usr/bin/' + name, CONFIG)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as exc:
        print('command gate: ' + str(exc), file=sys.stderr)
        sys.exit(125)
