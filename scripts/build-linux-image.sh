#!/bin/bash
# Build a native Ubuntu 26.04 Tart source from a verified Ubuntu cloud disk.
# Publishing and selecting its registry digest are separate steps.
set -euo pipefail
umask 077

if [[ $# != 4 || $1 == --help ]]; then
  echo "Usage: $0 arm64|amd64 VM_NAME CLOUD_IMAGE_QCOW2 SHA256" >&2
  echo "Requires a matching Mac, tart, packer, ansible, qemu-img, mkisofs and git." >&2
  echo "Use Ubuntu's ubuntu-26.04-server-cloudimg-ARCH.img and its published SHA256." >&2
  exit 2
fi
arch=$1
vm_name=$2
cloud_image=$3
checksum=$4
case $arch in
  arm64) host_arch=arm64; guest_arch=aarch64 ;;
  amd64) host_arch=x86_64; guest_arch=x86_64 ;;
  *) echo "Architecture must be arm64 or amd64." >&2; exit 2 ;;
esac
[[ $vm_name =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ && $checksum =~ ^[a-f0-9]{64}$ ]] || {
  echo "Invalid local VM name or SHA256." >&2; exit 2;
}
[[ -f $cloud_image ]] || { echo "Cloud disk does not exist: $cloud_image" >&2; exit 2; }
[[ $(uname -s) == Darwin && $(uname -m) == "$host_arch" ]] || {
  echo "Building $arch requires a $host_arch Mac; Tart does not cross-build guests." >&2; exit 1;
}
for tool in tart packer ansible-playbook qemu-img mkisofs git shasum; do
  command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
done
actual=$(shasum -a 256 < "$cloud_image")
[[ ${actual%% *} == "$checksum" ]] || { echo "Cloud disk SHA256 mismatch." >&2; exit 1; }
if tart get "$vm_name" >/dev/null 2>&1; then
  echo "VM already exists; choose a new name: $vm_name" >&2
  exit 1
fi

build_dir=$(mktemp -d "${TMPDIR:-/tmp}/pulse-ubuntu-26.04.XXXXXX")
echo "Build files and logs: $build_dir"
# Use the same cloud-image import, SSH/DHCP setup and guest agent as Cirrus.
# Keep the inputs on failure so an incomplete build can be inspected.
git -C "$build_dir" init -q
git -C "$build_dir" remote add origin https://github.com/cirruslabs/linux-image-templates.git
git -C "$build_dir" fetch --depth 1 origin 69112d2b9c6fb6986f70ac91b0fd6d7c394bd3c7
git -C "$build_dir" checkout --detach -q FETCH_HEAD
qemu-img convert -f qcow2 -O raw "$cloud_image" "$build_dir/image.raw"
cp "$build_dir/cloud-init/user-data.distro-with-admin-group" "$build_dir/cloud-init/user-data"
printf 'local-hostname: ubuntu\ninstance-id: %s\n' "$vm_name" > "$build_dir/cloud-init/meta-data"
mkisofs -output "$build_dir/cloud-init.iso" -volid cidata -joliet -rock "$build_dir/cloud-init/"

# A new LTS uses its GA kernel; a HWE package is not required. Check the actual
# guest release before preparation, and again after the upstream reboot step.
sed 's/linux-generic-hwe-{{ ansible_distribution_version }}/linux-generic/' \
  "$build_dir/playbook.yml" > "$build_dir/playbook.new"
mv "$build_dir/playbook.new" "$build_dir/playbook.yml"
cat > "$build_dir/verify.yml" <<YAML
- hosts: default
  tasks:
    - name: Verify Ubuntu 26.04 $arch guest
      ansible.builtin.assert:
        that:
          - ansible_distribution == 'Ubuntu'
          - ansible_distribution_version == '26.04'
          - ansible_architecture == '$guest_arch'
YAML
{
  echo '- import_playbook: verify.yml'
  echo '- import_playbook: playbook.yml'
  echo '- import_playbook: verify.yml'
} > "$build_dir/build.yml"
sed -e 's/headless = false/headless = true/' \
    -e 's|./playbook.yml|./build.yml|' \
  "$build_dir/cloud-init.pkr.hcl" > "$build_dir/cloud-init.new"
mv "$build_dir/cloud-init.new" "$build_dir/cloud-init.pkr.hcl"
(
  cd "$build_dir"
  packer init .
  packer validate -var "vm_name=$vm_name" .
)
tart create --linux "$vm_name"
# Only replace the disk of the VM just created; tart create refuses collisions.
vm_dir="${TART_HOME:-$HOME/.tart}/vms/$vm_name"
[[ -d $vm_dir && -f $vm_dir/config.json ]] || { echo "Missing new Tart VM directory." >&2; exit 1; }
mv "$build_dir/image.raw" "$vm_dir/disk.img"
(
  cd "$build_dir"
  packer build -var "vm_name=$vm_name" . 2>&1 | tee build.log
)
printf '\nBuilt local Ubuntu 26.04 %s VM: %s\n' "$arch" "$vm_name"
echo "Publish with tart push, then use its immutable registry digest in Config.Bases."
echo "Set OSVersion=26.04, Arch=$arch and Name=ubuntu-26.04-$arch in Config.Bases."
