#!/usr/bin/env python3
"""Shared naming convention for prepared VZ sources (no release metadata)."""
import hashlib
import re
import sys


def token(value):
    if len(value) <= 24 and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', value):
        return value
    return 'id-' + hashlib.sha256(value.encode()).hexdigest()[:20]


def names(owner, base, group, cpu, memory, count, docker_profile=""):
    prefix = f'pulse-{token(owner)}-{token(base)}'
    base_prefix = f'pulse-{token(owner)}-{token(base+"-docker")}' if docker_profile else prefix
    if docker_profile: group += '-docker-'+docker_profile
    return [base_prefix+'-base'] + [f'{prefix}-{token(group)}-{cpu}cpu-{memory}m-slot-{i:04d}' for i in range(1, count+1)]


if __name__ == '__main__':
    print('\n'.join(names(*sys.argv[1:4], *(int(v) for v in sys.argv[4:7]), docker_profile=sys.argv[7] if len(sys.argv)>7 else '')))
