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
mkdir -p "$work/root"/{bin,dev,proc,sys,tmp,etc,modules}
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


# Network qualification uses real nested KVM guests and the same capability
# wrapper/environment supplied by the provider's normal launch envelope.
conf=/etc/pulse-macvz/network.json
read -r bridge gateway prefix first second < <(python3 - "$conf" <<'PY'
import ipaddress,json,sys
n=json.load(open(sys.argv[1])); subnet=ipaddress.ip_network(n['subnet'])
print(n['bridge'],n['gateway'],n['prefix_length'],subnet.network_address+2,subnet.network_address+3)
PY
)
# Include the running Ubuntu kernel's virtio network dependencies in initramfs.
touch "$work/root/modules/load"
index=0
for module in virtio_mmio virtio_net; do
 while read -r directive path rest; do
  [[ $directive == insmod ]] || continue
  index=$((index+1))
  case "$path" in
   *.zst) zstd -dc "$path" > "$work/root/modules/$index.ko" ;;
   *.xz) xz -dc "$path" > "$work/root/modules/$index.ko" ;;
   *) cp "$path" "$work/root/modules/$index.ko" ;;
  esac
  echo "/bin/busybox insmod /modules/$index.ko" >> "$work/root/modules/load"
 done < <(modprobe --show-depends "$module")
done
cat > "$work/root/init" <<'INIT'
#!/bin/sh
B=/bin/busybox
$B mount -t proc proc /proc
$B mount -t sysfs sysfs /sys
$B mount -t devtmpfs devtmpfs /dev
. /modules/load
for arg in $($B cat /proc/cmdline); do
 case "$arg" in ipaddr=*|prefix=*|gateway=*|peer=*|role=*) export "$arg";; esac
done
$B ip link set lo up
$B ip link set eth0 up
$B ip addr add "$ipaddr/$prefix" dev eth0
$B ip route add default via "$gateway"
echo "nameserver $gateway" > /etc/resolv.conf
mkdir -p /tmp/www
if [ "$role" = server ]; then
 echo nested-peer > /tmp/www/index.html
 $B httpd -f -p 8080 -h /tmp/www &
 echo PULSE_NESTED_PEER_READY
 sleep 150
else
 set -e
 $B ping -c 1 -W 5 "$gateway"
 for n in $($B seq 1 30); do $B ping -c 1 -W 1 "$peer" && break; $B sleep 1; done
 $B wget -T 10 -qO /tmp/peer "http://$peer:8080/"
 $B grep -q nested-peer /tmp/peer
 $B wget -T 10 -qO /tmp/outer "http://$gateway:8080/"
 $B grep -q outer-worker /tmp/outer
 $B nslookup cache.nixos.org "$gateway"
 $B wget -T 20 -qO /tmp/external http://example.com/
 $B grep -q 'Example Domain' /tmp/external
 echo PULSE_NESTED_NETWORK_PASS
fi
$B poweroff -f
INIT
# Busybox applet symlinks used by the init script.
for name in mkdir sleep; do ln -sf busybox "$work/root/bin/$name"; done
(cd "$work/root"; find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1 > "$work/network-initrd.gz")
mkdir "$work/www"
echo outer-worker > "$work/www/index.html"
chmod 644 "$work/network-initrd.gz"
chown -R pulseworker:pulseworker "$work"
export XDG_RUNTIME_DIR=/run/user/55050 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/55050/bus
worker() { sudo -n -H -u pulseworker env XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" DBUS_SESSION_BUS_ADDRESS="$DBUS_SESSION_BUS_ADDRESS" /usr/local/bin/pulse-kvm-run "$@"; }
if systemctl is-active --quiet docker.service; then
 # The independent user manager must also acquire newly added Docker groups.
 worker systemd-run --user --quiet --wait --pipe --collect --unit="pulse-docker-network-$$" \
  /usr/local/bin/pulse-kvm-run /usr/bin/docker run --rm busybox:1.37 \
  wget -qO- http://example.com/ > "$work/docker-http"
 grep -q 'Example Domain' "$work/docker-http"
 echo PULSE_DOCKER_USER_SERVICE_NETWORK_PASS
fi
unit="pulse-network-test-$$"
tap1="pqt${$}a"; tap2="pqt${$}b"
cleanup_network() {
 worker systemctl --user stop "$unit-peer.service" "$unit-http.service" 2>/dev/null || true
 worker ip link delete "$tap1" 2>/dev/null || true
 worker ip link delete "$tap2" 2>/dev/null || true
 rm -rf "$work"
}
trap cleanup_network EXIT
# Show that TAP allocation is dynamic, not limited to the old proposed pool.
for n in $(seq 1 12); do worker ip tuntap add dev "pqt${$}n$n" mode tap; done
for n in $(seq 1 12); do worker ip link delete "pqt${$}n$n"; done
for tap in "$tap1" "$tap2"; do
 worker ip tuntap add dev "$tap" mode tap
 worker ip link set "$tap" master "$bridge"
 worker ip link set "$tap" up
done
# systemd-run returns before these services terminate. Subsequent operations
# use separate launcher sessions, proving independence from their creator.
worker systemd-run --user --quiet --collect --unit="$unit-http" \
 --setenv=XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" --setenv=DBUS_SESSION_BUS_ADDRESS="$DBUS_SESSION_BUS_ADDRESS" \
 /usr/local/bin/pulse-kvm-run /usr/bin/python3 -m http.server 8080 --bind "$gateway" --directory "$work/www"
worker systemd-run --user --quiet --collect --unit="$unit-peer" \
 --setenv=XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" --setenv=DBUS_SESSION_BUS_ADDRESS="$DBUS_SESSION_BUS_ADDRESS" \
 /usr/local/bin/pulse-kvm-run /usr/bin/qemu-system-aarch64 \
 -machine virt,gic-version=3 -accel kvm -cpu host -smp 1 -m 256 \
 -nodefaults -nographic -serial "file:$work/peer.log" -monitor none -no-reboot \
 -kernel "$work/kernel" -initrd "$work/network-initrd.gz" \
 -netdev "tap,id=net0,ifname=$tap2,script=no,downscript=no" -device virtio-net-device,netdev=net0,mac=02:00:00:00:00:03 \
 -append "console=ttyAMA0 rdinit=/init panic=1 role=server ipaddr=$second prefix=$prefix gateway=$gateway"
worker timeout 120 qemu-system-aarch64 \
 -machine virt,gic-version=3 -accel kvm -cpu host -smp 1 -m 256 \
 -nodefaults -nographic -serial stdio -monitor none -no-reboot \
 -kernel "$work/kernel" -initrd "$work/network-initrd.gz" \
 -netdev "tap,id=net0,ifname=$tap1,script=no,downscript=no" -device virtio-net-device,netdev=net0,mac=02:00:00:00:00:02 \
 -append "console=ttyAMA0 rdinit=/init panic=1 role=client ipaddr=$first prefix=$prefix gateway=$gateway peer=$second" \
 < /dev/null > "$work/network.log" 2>&1 || { tail -80 "$work/network.log"; tail -20 "$work/peer.log"; exit 1; }
grep -q '^PULSE_NESTED_NETWORK_PASS' "$work/network.log" || { tail -80 "$work/network.log"; tail -20 "$work/peer.log"; exit 1; }
printf 'PULSE_NESTED_NETWORK_PASS: two ordinary-user KVM guests; dynamic TAPs; user supervision; peer/outer HTTP; DNS; Internet HTTP\n'
