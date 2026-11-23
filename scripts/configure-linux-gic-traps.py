#!/usr/bin/env python3
"""Standard nested-KVM boot policy for a disposable ARM64 Ubuntu worker image builder."""
import os
from pathlib import Path
import platform
import stat
import subprocess
import tempfile

NAME = '99-pulse-kvm-gic-traps.cfg'
PARAMETERS = tuple('kvm-arm.' + name + '=1' for name in (
    'vgic_v3_group0_trap', 'vgic_v3_group1_trap', 'vgic_v3_common_trap'))
CONTENT = ('# Managed by pulse-macvz: outer ARM64 KVM host, not nested guests.\n'
           'GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} ' + ' '.join(PARAMETERS) + '"\n')


def configure(directory):
    """Only replace our own drop-in; preserve the defaults and other drop-ins."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    target = directory / NAME
    try:
        mode = target.lstat().st_mode
    except FileNotFoundError:
        mode = None
    if mode is not None:
        if not stat.S_ISREG(mode):
            raise ValueError('GIC trap drop-in must be a regular file')
        if target.read_text() == CONTENT:
            return
    fd, temporary = tempfile.mkstemp(prefix='.pulse-gic-', dir=directory)
    try:
        with os.fdopen(fd, 'w') as stream:
            stream.write(CONTENT)
            stream.flush()
            os.fsync(stream.fileno())
            os.fchmod(stream.fileno(), 0o644)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    if os.geteuid() != 0 or platform.system() != 'Linux' or platform.machine() != 'aarch64':
        raise SystemExit('GIC trap preparation requires root in an ARM64 Linux builder')
    for root in ('/var/db/pulse-macvz', '/var/db/pulse-tart'):
        for marker in ('intent', 'fingerprint'):
            if (Path(root) / marker).exists():
                raise SystemExit('refusing boot-policy changes on an allocation with job delivery')
    configure('/etc/default/grub.d')
    # Always regenerate, including recovery from interruption after writing the
    # drop-in. The builder shuts down; fresh allocations consume the new policy.
    subprocess.run(['update-grub'], check=True)
    print('PULSE_GIC_TRAPS_CONFIGURED: takes effect on the next boot', flush=True)


if __name__ == '__main__':
    main()
