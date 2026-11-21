"""Plan a Caddy publish from a Git tag and skopeo list-tags JSON on stdin."""
import datetime
import json
import re
import sys


def plan(ref, tags):
    match = re.fullmatch(r'caddy-([0-9]{8})\.(0|[1-9][0-9]*)', ref)
    if not match or len(ref.removeprefix('caddy-')) > 128:
        raise ValueError('expected caddy-YYYYMMDD.N (no leading zeros in N)')
    line, patch = match.groups()
    datetime.date.fromisoformat(f'{line[:4]}-{line[4:6]}-{line[6:]}')
    if not isinstance(tags, list) or not all(isinstance(t, str) for t in tags):
        raise ValueError('invalid registry tag list')
    patches = [int(m[1]) for t in tags
               if (m := re.fullmatch(rf'{line}\.(0|[1-9][0-9]*)', t))]
    latest = max(patches, default=-1)
    if latest == -1 and line in tags:
        raise ValueError('line exists without an immutable version')
    if int(patch) not in (latest, latest + 1):
        raise ValueError(f'cannot skip or roll back a patch (latest: {latest})')
    return dict(version=f'{line}.{patch}', line=line,
                build='yes' if int(patch) > latest else 'no')


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit('usage: release.py caddy-YYYYMMDD.N < registry-tags.json')
    try:
        listing = json.load(sys.stdin)
        release = plan(sys.argv[1], listing.get('Tags') if isinstance(listing, dict) else None)
    except ValueError as error:
        sys.exit(f'cannot plan Caddy release: {error}')
    for key, value in release.items():
        print(f'{key}={value}')
