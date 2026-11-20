#!/usr/bin/env bash
# Keep Local Network consent associated with a stable Apple-issued identity.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 2 || $1 != /* || -z $2 ]]; then
  echo 'Usage: scripts/sign-macos.sh /absolute/executable signing-identifier' >&2
  exit 2
fi
identity=${PULSE_CODESIGN_IDENTITY:-}
if [[ -z $identity ]]; then
  identities=$(security find-identity -v -p codesigning)
  # Prefer the distribution identity, then a development identity. Never
  # silently choose between multiple teams or certificates of the same kind.
  for kind in 'Developer ID Application:' 'Apple Development:'; do
    candidates=()
    while IFS= read -r hash; do
      [[ -z $hash ]] || candidates+=("$hash")
    done < <(printf '%s\n' "$identities" | awk -v kind="$kind" 'index($0, kind) {print $2}')
    if [[ ${#candidates[@]} -gt 1 ]]; then
      echo 'Multiple signing identities: set PULSE_CODESIGN_IDENTITY to the desired certificate name or hash.' >&2
      exit 2
    fi
    if [[ ${#candidates[@]} == 1 ]]; then
      identity=${candidates[0]}
      break
    fi
  done
fi
identity=${identity:--}
if [[ $identity == - ]]; then
  echo 'Ad-hoc signing: Local Network consent may reset after rebuilds. Set PULSE_CODESIGN_IDENTITY to an Apple-issued identity for stable consent.' >&2
fi
# Local builds need an identity, not a notarization timestamp. Release packaging
# may re-sign with a secure timestamp before notarization/distribution.
codesign --force --timestamp=none --identifier "$2" --entitlements "$root/cmd/pulse-vz/vz.entitlements" -s "$identity" "$1"
codesign --verify --strict "$1"
