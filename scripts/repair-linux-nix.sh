#!/bin/bash
# Repair existing Pulse Linux workers from their Tart host.
set -euo pipefail
umask 077
exec python3 "$(dirname "$0")/repair-linux-nix.py" "$@"
