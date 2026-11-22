#!/bin/bash
# Shared fresh-base / UNUSED-allocation preparation. Never repair accepted work.
set -euo pipefail
[[ $EUID == 0 && $(uname -s) == Linux ]]
for state in /var/db/pulse-tart /var/db/pulse-macvz; do
 for receipt in launch.json fingerprint intent; do
  if [[ -e $state/$receipt ]]; then echo 'KVM preparation refuses an accepted allocation' >&2; exit 1; fi
 done
done
[[ -c /dev/kvm ]] || exit 0
[[ $(id -u pulseworker) == 55050 && $(id -g pulseworker) == 55050 ]]
for tool in ip iptables setpriv setcap loginctl systemd-run python3; do command -v "$tool" >/dev/null; done
getent group kvm >/dev/null || groupadd --system kvm
usermod -aG kvm pulseworker
install -d -m 755 /etc/udev/rules.d /usr/local/libexec/pulse-macvz /etc/pulse-macvz /var/lib/pulse-macvz/kvm
printf 'KERNEL=="kvm", GROUP="kvm", MODE="0660"\n' > /etc/udev/rules.d/70-pulse-kvm.rules
chgrp kvm /dev/kvm
chmod 660 /dev/kvm
test -c /dev/net/tun
# A private copy avoids changing the distribution's setpriv or the Nix store.
# File capabilities regain NET_ADMIN after old launchers drop to the job UID;
# setpriv then passes precisely that capability through ordinary execs.
install -m 750 -o root -g pulseworker "$(command -v setpriv)" /usr/local/libexec/pulse-macvz-net-exec
setcap cap_net_admin=ep /usr/local/libexec/pulse-macvz-net-exec
cat > /usr/local/bin/pulse-kvm-run <<'WRAPPER'
#!/bin/sh
exec /usr/local/libexec/pulse-macvz-net-exec --inh-caps=+net_admin --ambient-caps=+net_admin -- "$@"
WRAPPER
chmod 755 /usr/local/bin/pulse-kvm-run
install -m 644 "$(dirname "$0")/linux-kvm-network.py" /usr/local/libexec/pulse-macvz/linux-kvm-network.py
cat > /etc/systemd/system/pulse-kvm-network.service <<'UNIT'
[Unit]
Description=Pulse allocation nested guest network
Wants=network-online.target
After=network-online.target docker.service
ConditionPathExists=/dev/kvm
[Service]
Type=oneshot
ExecStart=/usr/bin/python3 /usr/local/libexec/pulse-macvz/linux-kvm-network.py
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
UNIT
cat > /etc/systemd/system/pulse-kvm-dns.service <<'UNIT'
[Unit]
Description=Pulse nested guest DNS forwarding
ConditionPathExists=/dev/kvm
Requires=pulse-kvm-network.service
After=pulse-kvm-network.service
[Service]
DynamicUser=yes
User=pulse-kvm-dns
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ExecStart=/usr/bin/python3 /usr/local/libexec/pulse-macvz/linux-kvm-network.py dns
Restart=on-failure
RestartSec=1
[Install]
WantedBy=multi-user.target
UNIT
cat > /etc/systemd/system/pulse-kvm-ready.service <<'UNIT'
[Unit]
Description=Verify Pulse ordinary-user KVM networking and supervision
ConditionPathExists=/dev/kvm
Requires=pulse-kvm-network.service pulse-kvm-dns.service user@55050.service
After=pulse-kvm-network.service pulse-kvm-dns.service user@55050.service
[Service]
Type=oneshot
ExecStart=/usr/bin/python3 /usr/local/libexec/pulse-macvz/linux-kvm-network.py certify
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
UNIT
loginctl enable-linger pulseworker
systemctl daemon-reload
systemctl enable pulse-kvm-network.service pulse-kvm-dns.service pulse-kvm-ready.service
# Reapply after Docker activation; the accepted-work guard above is mandatory.
systemctl restart pulse-kvm-network.service
systemctl restart pulse-kvm-dns.service
systemctl start user@55050.service
# Refresh stale supplementary groups (KVM or newly enabled Docker) only while
# unused. Otherwise preserve the existing user manager and its services.
manager_pid=$(systemctl show user@55050.service -p MainPID --value)
if ! python3 - "$manager_pid" <<'PYGROUPS'
import subprocess,sys
expected=set(subprocess.check_output(['id','-G','pulseworker'],text=True).split())
actual=set()
for line in open('/proc/'+sys.argv[1]+'/status'):
 fields=line.split()
 if fields and fields[0] in ('Groups:', 'Gid:'): actual.update(fields[1:])
sys.exit(0 if expected <= actual else 1)
PYGROUPS
then
 systemctl restart user@55050.service
fi
for ((i=0;i<100;i++)); do [[ -S /run/user/55050/bus ]] && break; sleep .1; done
systemctl restart pulse-kvm-ready.service
