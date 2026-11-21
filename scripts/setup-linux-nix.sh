#!/bin/bash
# Shared Ubuntu Nix setup, used by base builders and unused warm allocations.
# Callers must serialize against handoff before running this in an allocation.
set -euo pipefail
set +x
[[ $EUID == 0 && $(uname -s) == Linux ]]
install -d -m 755 /etc/nix
# Append an include, rather than replacing administrator or distribution settings.
# extra-* retains the default cache/key and any locally configured caches/keys.
/usr/bin/python3 - <<'PY'
import os, pathlib, tempfile
directory = pathlib.Path('/etc/nix')
def write(path, data):
    if path.exists() and path.read_bytes() == data:
        return
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o644
    fd, name = tempfile.mkstemp(dir=directory, prefix='.pulse-nix-')
    try:
        with os.fdopen(fd, 'wb') as f:
            os.fchmod(f.fileno(), mode)
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name): os.unlink(name)
write(directory / 'pulse-macvz.conf', b'''# Managed by pulse-macvz: shared Ubuntu worker Nix configuration.
extra-experimental-features = nix-command flakes
extra-substituters = https://colony2.cachix.org
extra-trusted-public-keys = colony2.cachix.org-1:KFMHSiaf14yDf8qopSAgsFj9gs783OlFuE7Hs2AU4Hc=
''')
path = directory / 'nix.conf'
data = path.read_bytes() if path.exists() else b''
include = b'include pulse-macvz.conf'
if include not in [line.split(b'#', 1)[0].strip() for line in data.splitlines()]:
    write(path, data + (b'\n' if data and not data.endswith(b'\n') else b'') + include + b'\n')
PY
# A daemon reads nix.conf at startup. Repeat on retry even if a previous run
# wrote the files but failed before restarting it. No accepted worker runs here.
systemctl restart nix-daemon
if id pulseworker >/dev/null 2>&1; then
 if getent group nix-users >/dev/null; then usermod -aG nix-users pulseworker; fi
 sudo -n -H -u pulseworker /usr/bin/nix --extra-experimental-features nix-command store ping --store daemon
fi
