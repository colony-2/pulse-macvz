#!/bin/bash
# Run only after the selected Nix environment has been successfully installed.
set -euo pipefail
bundle=$1
revision=$(sed -nE 's|.*inputs\.c2j\.url = "github:colony-2/c2j/([0-9a-f]{40})";.*|\1|p' "$bundle/flake.nix")
[[ $revision =~ ^[0-9a-f]{40}$ ]]
version=$(/opt/pulse-tart/bin/c2j version)
version=${version#c2j version }
[[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+/-]*$ ]]
mkdir -p /opt/pulse-tart-image
printf '%s\n' "$revision" > /opt/pulse-tart-image/c2j-revision
printf 'PULSE_C2J %s %s\n' "$revision" "$version" > /opt/pulse-tart-image/c2j-version
chmod 644 /opt/pulse-tart-image/c2j-revision /opt/pulse-tart-image/c2j-version
