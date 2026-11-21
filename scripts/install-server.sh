#!/usr/bin/env bash
# Install the signed CLI at the same path that go install uses.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
install_bin=$(go env GOBIN)
if [[ -z $install_bin ]]; then
  install_gopath=$(go env GOPATH)
  install_bin="${install_gopath%%:*}/bin"
fi
mkdir -p "$install_bin"
install_bin=$(cd "$install_bin" && pwd)
if [[ -d $install_bin/pulse-macvz && ! -L $install_bin/pulse-macvz ]]; then
  echo "Refusing to replace directory: $install_bin/pulse-macvz" >&2
  exit 2
fi
install_stage=$(mktemp -d "$install_bin/.pulse-install.XXXXXX")
trap 'rm -rf "$install_stage"' EXIT
if [[ $(uname -s)/$(uname -m) == Darwin/arm64 ]]; then
  # Keep the signed bundle metadata alongside the executable for launchd's
  # Local Network identity. The PATH entry resolves to this same executable.
  bash "$root/scripts/build-server-app.sh" "$install_bin/PulseProvider.app"
  ln -s "$install_bin/PulseProvider.app/Contents/MacOS/pulse-macvz" "$install_stage/pulse-macvz"
else
  bash "$root/scripts/build-server.sh" "$install_stage/pulse-macvz"
fi
mv -f "$install_stage/pulse-macvz" "$install_bin/pulse-macvz"
# Retire only the old symlink installed by this project's app installer.
# Leave unrelated executables and links untouched.
if [[ -L $install_bin/pulse-tart && $(readlink "$install_bin/pulse-tart") == "$install_bin/PulseProvider.app/Contents/MacOS/pulse-tart" ]]; then
  rm "$install_bin/pulse-tart"
fi
echo "Installed $install_bin/pulse-macvz"
