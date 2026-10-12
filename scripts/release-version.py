#!/usr/bin/env python3
"""Compute an idempotent SemVer release from reachable tags and Conventional Commits."""
import re
import subprocess

def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()

def parse(tag):
    m = re.fullmatch(r'v(\d+)\.(\d+)\.(\d+)', tag)
    return tuple(map(int, m.groups())) if m else None

def next_version():
    existing = [parse(t) for t in git('tag', '--points-at', 'HEAD').splitlines()]
    existing = [v for v in existing if v]
    if existing:
        return 'v' + '.'.join(map(str, max(existing)))
    tags = [(parse(t), t) for t in git('tag', '--merged', 'HEAD', '--list', 'v*').splitlines()]
    tags = [(v, t) for v, t in tags if v]
    if not tags:
        return 'v1.0.0'
    (major, minor, patch), last = max(tags)
    commits = git('log', last + '..HEAD', '--format=%s%n%b')
    if re.search(r'^[a-z]+(?:\([^\n]+\))?!:', commits, re.M) or re.search(r'^BREAKING[ -]CHANGE:', commits, re.M):
        major, minor, patch = major + 1, 0, 0
    elif re.search(r'^feat(?:\([^\n]+\))?:', commits, re.M):
        minor, patch = minor + 1, 0
    else:
        patch += 1
    return f'v{major}.{minor}.{patch}'

if __name__ == '__main__':
    print(next_version())
