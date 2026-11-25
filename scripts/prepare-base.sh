#!/bin/bash
# Run in a disposable image builder. The provider/updater waits for clean shutdown.
set -Eeuo pipefail
set +x
umask 077
bundle=$1
seconds=$2
expected_os=$3
mode=${4:-install}
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
[[ $seconds =~ ^[0-9]+$ && $seconds -ge 60 && $seconds -le 7200 ]]
[[ $expected_os =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]
[[ $(/usr/bin/sw_vers -productVersion) == "$expected_os" ]]
# Storage helpers are kept in this script so reuse probes receive the same policy
# as fresh installs. Only the unused builder service calls the grow operation.
macos_free_space() {
  local path available
  for path in /System/Volumes/Data /nix/store; do
    [[ -d $path ]] || { echo "Missing worker filesystem: $path" >&2; return 1; }
    available=$(/bin/df -k "$path" | /usr/bin/awk 'NR == 2 {print $4}')
    [[ $available =~ ^[0-9]+$ ]] || return 1
    echo "macOS storage: $path has $((available / 1024)) MiB available; requires 30720 MiB"
    if (( available < 30 * 1024 * 1024 )); then
      echo "Insufficient macOS worker space: at least 30 GiB free is required" >&2
      return 1
    fi
  done
}
grow_macos_storage() {
  local info container store whole disk_bytes container_bytes path
  [[ $(/usr/sbin/sysctl -n hw.model) == VirtualMac* ]] || {
    echo 'Refusing disk growth outside a macOS VM' >&2; return 1;
  }
  for path in /var/db/pulse-tart/intent /var/db/pulse-macvz/intent; do
    [[ ! -e $path ]] || { echo 'Refusing disk growth on an assigned worker' >&2; return 1; }
  done
  if /usr/bin/pgrep -u 55050 >/dev/null 2>&1; then
    echo 'Refusing disk growth while worker processes exist' >&2; return 1
  fi
  info=$(/usr/sbin/diskutil info -plist /System/Volumes/Data)
  container=$(/usr/bin/plutil -extract APFSContainerReference raw -o - - <<<"$info")
  store=$(/usr/bin/plutil -extract APFSPhysicalStores.0.APFSPhysicalStore raw -o - - <<<"$info")
  container_bytes=$(/usr/bin/plutil -extract APFSContainerSize raw -o - - <<<"$info")
  [[ $container =~ ^disk[0-9]+$ && $store =~ ^disk[0-9]+s[0-9]+$ ]] || {
    echo 'Invalid APFS container or physical store identity' >&2; return 1;
  }
  info=$(/usr/sbin/diskutil info -plist "$store")
  whole=$(/usr/bin/plutil -extract ParentWholeDisk raw -o - - <<<"$info")
  [[ $whole =~ ^disk[0-9]+$ ]] || return 1
  info=$(/usr/sbin/diskutil info -plist "$whole")
  disk_bytes=$(/usr/bin/plutil -extract TotalSize raw -o - - <<<"$info")
  [[ $disk_bytes =~ ^[0-9]+$ && $container_bytes =~ ^[0-9]+$ ]] || return 1
  # The normal image has a 500 MiB ISC partition preceding its main container.
  # Leave already-expanded disks alone. Never delete or move other partitions.
  if (( disk_bytes - container_bytes > 1024 * 1024 * 1024 )); then
    echo "Growing macOS APFS container $container on $whole (virtual disk $disk_bytes bytes)"
    /usr/sbin/diskutil repairDisk "$whole" <<<y
    /usr/sbin/diskutil apfs resizeContainer "$container" 0
  fi
}
state=/var/db/pulse-tart-base
mkdir -p "$state"
if [[ ${PULSE_BASE_SERVICE:-0} != 1 ]]; then
  [[ ! -e $state/failed ]] || { echo PULSE_BASE_FAILED; exit 1; }
  # An older prepared source may contain a ready receipt but still have a
  # smaller filesystem. Revalidate it through the unused-builder service.
  if [[ -f $state/ready && $(cat "$state/storage-policy" 2>/dev/null || true) != disk80-free30-v1 ]]; then
    rm -f "$state/ready"
  fi
  if [[ ! -f $state/ready ]]; then
    if [[ $mode == install && ! -d $state/setup ]]; then cp -R "$bundle" "$state/setup"; fi
    cp "$0" "$state/prepare-base.sh"
    cat > /Library/LaunchDaemons/dev.pulse-tart.base.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>dev.pulse-tart.base</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>$state/prepare-base.sh</string><string>$state/setup</string><string>$seconds</string><string>$expected_os</string><string>$mode</string></array>
<key>EnvironmentVariables</key><dict><key>PULSE_BASE_SERVICE</key><string>1</string><key>HOME</key><string>/var/root</string></dict>
<key>RunAtLoad</key><true/><key>StandardOutPath</key><string>/var/log/pulse-tart-base.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-base.log</string></dict></plist>
PLIST
    chmod 600 /Library/LaunchDaemons/dev.pulse-tart.base.plist
    launchctl print system/dev.pulse-tart.base >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.base.plist
  fi
  for ((i=0;i<seconds;i++)); do
    if [[ -f $state/ready ]]; then
      cat /opt/pulse-tart-image/c2j-version 2>/dev/null || true
      echo PULSE_BASE_READY
      sync
      /sbin/shutdown -h now
      exit 0
    fi
    if [[ -f $state/failed ]]; then echo PULSE_BASE_FAILED; tail -c 6000 /var/log/pulse-tart-base.log; exit 1; fi
    sleep 1
  done
  echo PULSE_BASE_FAILED
  /sbin/shutdown -h now
  exit 1
fi
# launchd can restart this unused builder after a guest reboot. Receipts belong
# to the current attempt: a recovered install must not retain an earlier failure,
# and a failed retry must not expose an earlier successful receipt.
attempt=$(/usr/bin/uuidgen)
printf '%s\n' "$attempt" > "$state/attempt.tmp"
mv -f "$state/attempt.tmp" "$state/attempt"
rm -f "$state/ready" "$state/failed"
current_attempt() { [[ $(cat "$state/attempt") == "$attempt" ]]; }
fail_attempt() {
  current_attempt || return 0
  printf '%s\n' "$attempt" > "$state/failed.tmp"
  mv -f "$state/failed.tmp" "$state/failed"
  /sbin/shutdown -h now
}
(sleep "$seconds"; fail_attempt) &
guard=$!
trap 'kill "$guard" 2>/dev/null || true' EXIT
trap 'fail_attempt' ERR
[[ $(/usr/bin/sw_vers -productVersion) == "$expected_os" ]]
grow_macos_storage
if [[ $mode == install ]]; then
  /bin/bash "$bundle/prepare-image.sh" "$bundle"
  rm -f /Library/LaunchDaemons/dev.pulse-tart.template.plist
  rm -rf /var/db/pulse-tart-template
fi
for tool in c2j pulse-tart-guest git node pnpm uv python3; do test -x "/opt/pulse-tart/bin/$tool"; done
test -f /opt/pulse-tart-image/bootstrap-guest.sh
test -f /opt/pulse-tart-image/prepare-template.sh
/opt/pulse-tart/bin/c2j version
macos_free_space
rm -f /Library/LaunchDaemons/dev.pulse-tart.base.plist
rm -rf "$state/setup"
sync
kill "$guard" 2>/dev/null || true
wait "$guard" 2>/dev/null || true
trap - EXIT
current_attempt
printf '%s\n' disk80-free30-v1 > "$state/storage-policy"
printf '%s\n' "$attempt" > "$state/ready.tmp"
mv -f "$state/ready.tmp" "$state/ready"
