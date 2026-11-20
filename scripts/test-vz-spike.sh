#!/usr/bin/env bash
# Feasibility gate only. Source VM directories must be stopped cold bases.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
if [[ $# != 2 || $(uname -s) != Darwin || $(uname -m) != arm64 ]]; then
  echo 'Usage (Apple Silicon): scripts/test-vz-spike.sh /absolute/macos-base /absolute/linux-base' >&2
  exit 2
fi
mac_base=$1
linux_base=$2
[[ $mac_base == /* && $linux_base == /* ]] || { echo 'Base paths must be absolute.' >&2; exit 2; }
mkdir -p test-results
out=$(mktemp -d "$root/test-results/vz-spike.XXXXXX")
chmod 700 "$out"
runner="$out/vz-spike"
pids=()
cleanup() {
  local pid
  for pid in ${pids[@]+"${pids[@]}"}; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid" 2>/dev/null || true; done
  echo "VZ spike artifacts: $out"
}
trap cleanup EXIT
export CGO_ENABLED=0
go build -o "$runner" ./experimental/vz-spike
codesign --force --entitlements experimental/vz-spike/vz.entitlements -s - "$runner"
# Fail before creating clones if the environment cannot start a VM.
"$runner" probe 2>&1 | tee "$out/probe.log"

"$runner" clone "$linux_base" "$out/linux"
"$runner" run -fresh "$out/linux" >"$out/linux.log" 2>&1 &
pids=("$!")
"$runner" ssh "$out/linux" | tee "$out/linux-ssh.log"
kill -TERM "${pids[0]}"
wait "${pids[0]}"
pids=()

# Sequential preparation with distinct identities reproduces the important case.
for slot in 1 2; do
  template="$out/template-$slot"
  "$runner" clone "$mac_base" "$template"
  "$runner" run -fresh "$template" >"$out/template-$slot.log" 2>&1 &
  pids=("$!")
  "$runner" ssh "$template" | tee "$out/template-$slot-ssh.log"
  kill -USR1 "${pids[0]}"
  wait "${pids[0]}"
  pids=()
  [[ -s $template/state.vzvmsave ]]
done

for round in 1 2; do
  for slot in 1 2; do
    clone="$out/round-$round-slot-$slot"
    "$runner" clone "$out/template-$slot" "$clone"
    "$runner" run "$clone" >"$out/round-$round-slot-$slot.log" 2>&1 &
    pids+=("$!")
  done
  for slot in 1 2; do
    "$runner" ssh "$out/round-$round-slot-$slot" | tee "$out/round-$round-slot-$slot-ssh.log"
  done
  for pid in "${pids[@]}"; do kill -TERM "$pid"; done
  for pid in "${pids[@]}"; do wait "$pid"; done
  pids=()
done
printf 'VZ_SPIKE_PASS: Linux EFI; macOS headless; sequential independent templates; two concurrent restores, two rounds.\n' | tee "$out/result.txt"
