#!/bin/bash
# Observe an existing worker; never start another provider or provision a VM.
set -euo pipefail
umask 077
exec python3 "$(dirname "$0")/diagnose-tart.py" "$@"
