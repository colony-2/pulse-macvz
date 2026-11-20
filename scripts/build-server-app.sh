#!/usr/bin/env bash
# Package the cgo-free server and its VM/bridge child modes with the identity metadata a launchd agent needs.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /*.app || $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Usage: scripts/build-server-app.sh /absolute/PulseProvider.app (Apple Silicon Mac)' >&2
  exit 2
fi
source "$root/scripts/macos-app-identity.sh"
# Never rewrite a registered bundle while its signature is temporarily invalid.
# Build/sign beside the destination, then atomically replace the whole bundle.
mkdir -p "$(dirname "$1")"
app_stage=$(mktemp -d "$(dirname "$1")/.pulse-app-build.XXXXXX")
trap 'rm -rf "$app_stage"' EXIT
app_candidate="$app_stage/PulseProvider.app"
mkdir -p "$app_candidate/Contents/MacOS"
if [[ -e $1 || -L $1 ]]; then
  if [[ -L $1 || ! -d $1 || $(/usr/bin/plutil -extract CFBundleIdentifier raw -o - "$1/Contents/Info.plist") != "$pulse_app_id" ]]; then
    echo 'Refusing to replace a symlink or an app with a different bundle identifier.' >&2
    exit 2
  fi
fi
cat > "$app_candidate/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>$pulse_app_id</string>
<key>CFBundleExecutable</key><string>pulse-tart</string>
<key>CFBundleName</key><string>$pulse_app_name</string>
<key>CFBundleDisplayName</key><string>$pulse_app_name</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>0.1.0</string>
<key>LSUIElement</key><true/>
<key>NSLocalNetworkUsageDescription</key><string>Connect to local virtual machines that run Pulse jobs and their Docker engines.</string>
</dict></plist>
PLIST
bash "$root/scripts/build-server.sh" "$app_candidate/Contents/MacOS/pulse-tart"
bash "$root/scripts/sign-macos.sh" "$app_candidate" "$pulse_app_id"
codesign --verify --deep --strict "$app_candidate"
cd "$root"
CGO_ENABLED=0 go run ./scripts/install-macos-app "$app_candidate" "$1"
