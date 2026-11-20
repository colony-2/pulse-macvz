#!/usr/bin/env python3
"""Require kernel evidence of a memory-limit kill for the specified container."""
import json
import re
import subprocess
import sys


def evidence(container_id, kernel_log):
    if not re.fullmatch(r'[0-9a-f]{64}', container_id):
        raise ValueError('invalid OOM container ID')
    # The production engine uses systemd cgroups. Match the limiting cgroup,
    # not just task_memcg: a global/ancestor OOM must never pass this check.
    scope = ',oom_memcg=/system.slice/docker-' + container_id + '.scope,'
    for line in kernel_log.splitlines():
        if 'oom-kill:constraint=CONSTRAINT_MEMCG,' in line and scope in line:
            return line
    raise ValueError('no container-specific memory-limit kill in kernel log')


if __name__ == '__main__':
    container_id = sys.argv[1]
    log = subprocess.check_output(
        ['journalctl', '-k', '--no-pager', '-o', 'cat', '--since', '-5 minutes',
         '--grep', 'oom-kill:constraint='], text=True, timeout=15)
    print(json.dumps({'container_id': container_id,
                      'kernel_evidence': evidence(container_id, log)}))
    print('CONTAINER_OOM_CONFIRMED')
