#!/usr/bin/env python3
"""Read-only proof that a failed Tart clone actually published a complete image."""
import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import stat
import sys

NAME = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]*$')
MAC = re.compile(r'^(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$')


class Unverified(Exception):
    pass


def identity(info):
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns


def verify(home, source, target, expected_state):
    if (not NAME.fullmatch(source) or not NAME.fullmatch(target) or source == target
            or not source.startswith('pulse-build-') or expected_state not in ('stopped', 'suspended')):
        raise Unverified('invalid_input')
    paths = [home / 'vms' / name for name in (source, target)]
    checks = []
    with contextlib.ExitStack() as stack:
        # Tart clone takes flock on TART_HOME. Keep replacement/cloning serialized
        # while comparing. PIDLock uses POSIX write locks on each config.json.
        fd = os.open(home, os.O_RDONLY | os.O_DIRECTORY)
        stack.callback(os.close, fd)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        directories = [path.lstat() for path in paths]
        if any(not stat.S_ISDIR(info.st_mode) for info in directories):
            raise Unverified('not_regular_vm_directories')
        if (directories[0].st_dev, directories[0].st_ino) == (directories[1].st_dev, directories[1].st_ino):
            raise Unverified('same_vm_directory')

        def open_file(path, writable=False):
            flags = (os.O_RDWR if writable else os.O_RDONLY) | os.O_NOFOLLOW | os.O_NONBLOCK
            fd = os.open(path, flags)
            stack.callback(os.close, fd)
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_size == 0:
                raise Unverified('missing_or_invalid_vm_file')
            return fd, info

        configs = [open_file(path / 'config.json', writable=True) for path in paths]
        for fd, _ in configs:
            fcntl.lockf(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # Do not reopen/close config.json while locked: POSIX locks would drop.
        for path in paths:
            if (path / 'overlay.asif').exists() or (path / 'manifest.json').exists():
                raise Unverified('unsupported_vm_layout')
            saved = os.path.lexists(path / 'state.vzvmsave')
            if saved != (expected_state == 'suspended'):
                raise Unverified('saved_state_mismatch')
        settings = []
        for fd, info in configs:
            if info.st_size > 1 << 20:
                raise Unverified('invalid_config')
            config = json.loads(os.pread(fd, info.st_size, 0))
            mac = config.get('macAddress', '')
            if not isinstance(mac, str) or not MAC.fullmatch(mac) or int(mac[:2], 16) & 1:
                raise Unverified('invalid_mac')
            if expected_state == 'stopped':
                # Tart regenerates the MAC on stopped clones. No other hardware
                # differences are permitted, including the machine identifier.
                config.pop('macAddress')
            settings.append(config)
        if settings[0] != settings[1]:
            raise Unverified('config_mismatch')
        checks.append('config_matches')
        for filename in ('disk.img', 'nvram.bin') + (('state.vzvmsave',) if expected_state == 'suspended' else ()):
            files = [open_file(path / filename) for path in paths]
            if files[0][1].st_size != files[1][1].st_size:
                raise Unverified(filename + '_size_mismatch')
            for offset in range(0, files[0][1].st_size, 4 << 20):
                length = min(4 << 20, files[0][1].st_size - offset)
                left, right = (os.pread(fd, length, offset) for fd, _ in files)
                if len(left) != length or left != right:
                    raise Unverified(filename + '_content_mismatch')
            for path, (fd, info) in zip(paths, files):
                if identity(os.fstat(fd)) != identity(info) or identity((path / filename).lstat()) != identity(info):
                    raise Unverified('files_changed_during_verification')
            checks.append(filename + '_bytes_match')
        for path, directory, (fd, info) in zip(paths, directories, configs):
            current = path.lstat()
            if (current.st_dev, current.st_ino) != (directory.st_dev, directory.st_ino):
                raise Unverified('directory_changed_during_verification')
            if identity(os.fstat(fd)) != identity(info) or identity((path / 'config.json').lstat()) != identity(info):
                raise Unverified('config_changed_during_verification')
        return {'outcome': 'verified', 'state': expected_state, 'checks': checks}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source')
    parser.add_argument('target')
    parser.add_argument('state', choices=('stopped', 'suspended'))
    parser.add_argument('--tart-home', type=Path, default=Path(os.environ.get('TART_HOME') or Path.home() / '.tart'))
    args = parser.parse_args()
    def expired(*_):
        raise Unverified('verification_timeout')
    signal.signal(signal.SIGALRM, expired)
    signal.alarm(300)
    try:
        result = verify(args.tart_home.expanduser(), args.source, args.target, args.state)
        status = 0
    except Unverified as error:
        result, status = {'outcome': 'unverified', 'reason': str(error)}, 1
    except (OSError, ValueError, TypeError, AttributeError) as error:
        # No private JSON/config contents or untrusted exception messages.
        result, status = {'outcome': 'unverified', 'error_type': type(error).__name__}, 1
    finally:
        signal.alarm(0)
    print(json.dumps(result))
    return status


if __name__ == '__main__':
    sys.exit(main())
