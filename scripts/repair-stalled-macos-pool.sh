#!/bin/bash
# One-off repair for the pool in the November 18 diagnosis archives.
set -euo pipefail
umask 077
exec python3 "$(dirname "$0")/repair-stalled-macos-pool.py" "$@"
