#!/bin/bash
# Run only inside a disposable prepared Ubuntu ARM VM, never on the host.
set -euo pipefail
trap 'echo "KVM test failed at line $LINENO"' ERR
[[ $EUID == 0 && $(uname -s) == Linux && $(uname -m) == aarch64 ]]
uname -r
if [[ ! -c /dev/kvm ]]; then
 dmesg | grep -Ei 'kvm|hyp|el2|virtualization' || true
 echo 'Guest kernel did not expose /dev/kvm'
 exit 1
fi
[[ $(id -u pulseworker) == 55050 ]]
# Prove the ordinary account can use KVM before installing the test VMM.
sudo -n -H -u pulseworker /opt/pulse-tart/bin/python3 - <<'PY'
import fcntl, os
with open('/dev/kvm', 'r+b', buffering=0) as kvm:
    assert fcntl.ioctl(kvm, 0xae00, 0) == 12
    vm = fcntl.ioctl(kvm, 0xae01, 0)
    os.close(vm)
print('PULSE_KVM_USER_API_PASS')
PY
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends qemu-system-arm busybox-static cpio zstd > /var/tmp/pulse-kvm-packages.log 2>&1 || { tail -40 /var/tmp/pulse-kvm-packages.log; exit 1; }
work=$(mktemp -d /var/tmp/pulse-kvm-test.XXXXXX)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/root"/{bin,dev,proc,sys}
cp /bin/busybox "$work/root/bin/busybox"
ln -s busybox "$work/root/bin/sh"
mknod "$work/root/dev/console" c 5 1
mknod "$work/root/dev/null" c 1 3
cat > "$work/root/init" <<'INIT'
#!/bin/sh
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
echo PULSE_NESTED_KVM_BOOT_PASS
/bin/busybox uname -m
/bin/busybox poweroff -f
INIT
chmod 755 "$work/root/init"
(cd "$work/root"; find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1 > "$work/initrd.gz")
# Ubuntu's newer kernels use an EFI zboot wrapper. Older QEMU versions cannot
# decompress its zstd payload; extract it using the upstream zboot header's
# payload offset/size and compression fields, rather than downloading a kernel.
/opt/pulse-tart/bin/python3 - "/boot/vmlinuz-$(uname -r)" "$work/kernel" <<'PY'
import gzip, lzma, pathlib, struct, subprocess, sys
data = pathlib.Path(sys.argv[1]).read_bytes()
if data[4:8] == b'zimg':
    offset, size = struct.unpack_from('<II', data, 8)
    compression = data[24:32].split(b'\0')[0]
    assert 32 <= offset < len(data) and 0 < size <= len(data) - offset
    payload = data[offset:offset+size]
    if compression == b'zstd':
        data = subprocess.check_output(['zstd', '-d', '-c'], input=payload)
    elif compression == b'gzip':
        data = gzip.decompress(payload)
    elif compression == b'xz':
        data = lzma.decompress(payload)
    else:
        raise RuntimeError('unsupported zboot compression: ' + repr(compression))
pathlib.Path(sys.argv[2]).write_bytes(data)
PY
chmod 644 "$work/kernel" "$work/initrd.gz"
chown -R pulseworker:pulseworker "$work"
# -accel kvm is mandatory: software emulation must never turn a broken KVM
# configuration into a passing test. There is no nested network or disk device.
sudo -n -H -u pulseworker timeout 90 qemu-system-aarch64 \
  -machine virt,gic-version=3 -accel kvm -cpu host -smp 1 -m 512 \
  -nodefaults -nographic -serial stdio -monitor none -no-reboot \
  -kernel "$work/kernel" -initrd "$work/initrd.gz" \
  -append 'console=ttyAMA0 rdinit=/init panic=1' < /dev/null > "$work/boot.log" 2>&1 || { tail -60 "$work/boot.log"; exit 1; }
grep -q '^PULSE_NESTED_KVM_BOOT_PASS' "$work/boot.log"
grep -q '^aarch64' "$work/boot.log"
printf 'PULSE_NESTED_KVM_BOOT_PASS: Linux ARM64 booted with -accel kvm as pulseworker\n'
