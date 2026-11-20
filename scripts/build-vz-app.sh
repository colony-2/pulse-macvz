#!/usr/bin/env bash
# Package the cgo-free runner with the identity metadata a launchd agent needs.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /*.app || $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Usage: scripts/build-vz-app.sh /absolute/PulseVZ.app (Apple Silicon Mac)' >&2
  exit 2
fi
mkdir -p "$1/Contents/MacOS"
cat > "$1/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>dev.pulse.vz.runner</string>
<key>CFBundleExecutable</key><string>pulse-vz</string>
<key>CFBundleName</key><string>Pulse VZ</string>
<key>CFBundleDisplayName</key><string>Pulse VZ</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>0.1.0</string>
<key>LSUIElement</key><true/>
<key>NSLocalNetworkUsageDescription</key><string>Connect to local virtual machines that run Pulse jobs and their Docker engines.</string>
</dict></plist>
PLIST
bash "$root/scripts/build-vz-runner.sh" "$1/Contents/MacOS/pulse-vz"
bash "$root/scripts/sign-macos.sh" "$1" dev.pulse.vz.runner
