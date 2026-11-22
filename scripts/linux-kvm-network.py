#!/usr/bin/python3
"""Owned, reboot-safe networking for a disposable Linux allocation."""
import argparse
import concurrent.futures
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import socket
import socketserver
import subprocess
import tempfile
import time
import threading
import uuid

VERSION = 1
BRIDGE = 'pulsebr0'
ALIAS = 'pulse-macvz:kvm:v1'
CONFIG = Path('/etc/pulse-macvz/network.json')
READY = Path('/var/lib/pulse-macvz/kvm/ready.json')
SELF = '/usr/local/libexec/pulse-macvz/linux-kvm-network.py'
WRAPPER = '/usr/local/bin/pulse-kvm-run'
ENV = dict(os.environ, XDG_RUNTIME_DIR='/run/user/55050',
           DBUS_SESSION_BUS_ADDRESS='unix:path=/run/user/55050/bus')


def run(*args):
    return subprocess.check_output(args, text=True, env=ENV, timeout=30).strip()


def boot_id():
    return Path('/proc/sys/kernel/random/boot_id').read_text().strip()


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, 0o644)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def occupied_networks(routes, addresses):
    result = []
    for route in routes:
        dest = route.get('dst', 'default')
        if dest != 'default' and route.get('dev') != BRIDGE:
            result.append(ipaddress.ip_network(dest, strict=False))
    for link in addresses:
        if link['ifname'] == BRIDGE:
            continue
        for address in link.get('addr_info', []):
            if address['family'] == 'inet':
                result.append(ipaddress.ip_network(
                    address['local'] + '/' + str(address['prefixlen']), strict=False))
    return result


def choose_subnet(occupied, previous=None):
    # A /16 leaves address allocation to the workload; there is no fixed TAP pool.
    candidates = [ipaddress.ip_network(previous)] if previous else (
        ipaddress.ip_network('10.%d.0.0/16' % n) for n in range(200, 255))
    for candidate in candidates:
        if candidate.version == 4 and candidate.is_private and not any(candidate.overlaps(n) for n in occupied):
            return candidate
    raise RuntimeError('no collision-free nested subnet; refusing to change unrelated routes or a saved network')


def ensure_rule(table, chain, *rule):
    command = ['iptables', '-w', '10', '-t', table]
    rule = [*rule, '-m', 'comment', '--comment', 'pulse-macvz-kvm']
    if not subprocess.run(command + ['-C', chain, *rule], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        run(*command, '-D', chain, *rule)
    run(*command, '-I', chain, '1', *rule)


def configure():
    assert os.geteuid() == 0
    READY.unlink(missing_ok=True)
    links = json.loads(run('ip', '-j', 'link', 'show'))
    existing = next((link for link in links if link['ifname'] == BRIDGE), None)
    previous = json.loads(CONFIG.read_text()) if CONFIG.exists() else None
    if previous and (previous.get('version') != VERSION or previous.get('bridge') != BRIDGE):
        raise RuntimeError('unrecognized network descriptor; refusing to overwrite')
    if existing and existing.get('ifalias') != ALIAS:
        # Linux may ignore IFLA_IFALIAS at creation. A persisted intent and the
        # explicitly assigned MAC let us recover the gap before setting alias.
        pending = (previous and previous.get('boot_id') == boot_id()
                   and not existing.get('ifalias') and previous.get('bridge_mac')
                   and previous.get('bridge_mac') == existing.get('address'))
        if not pending:
            raise RuntimeError('pulsebr0 already exists and is not managed by pulse-macvz')
    if existing and not previous:
        raise RuntimeError('owned bridge has no descriptor; refusing to guess its subnet')
    routes = json.loads(run('ip', '-j', '-4', 'route', 'show', 'table', 'all'))
    addresses = json.loads(run('ip', '-j', '-4', 'addr', 'show'))
    subnet = choose_subnet(occupied_networks(routes, addresses), previous['subnet'] if previous else None)
    gateway = str(subnet.network_address + 1)
    mac = (previous or {}).get('bridge_mac') or '02:' + ':'.join('%02x' % b for b in uuid.uuid4().bytes[:5])
    # Persist intent before creating kernel resources so an interrupted setup
    # can retry with the same subnet and prove which bridge it owns.
    write_json(CONFIG, dict(version=VERSION, bridge=BRIDGE, bridge_mac=mac, subnet=str(subnet),
                           prefix_length=subnet.prefixlen, netmask=str(subnet.netmask),
                           gateway=gateway, dns=[gateway],
                           first_guest_address=str(subnet.network_address + 2),
                           last_guest_address=str(subnet.broadcast_address - 1),
                           boot_id=boot_id()))
    if not existing:
        run('ip', 'link', 'add', BRIDGE, 'address', mac, 'type', 'bridge')
    run('ip', 'link', 'set', BRIDGE, 'alias', ALIAS)
    run('ip', 'address', 'replace', gateway + '/' + str(subnet.prefixlen), 'dev', BRIDGE)
    run('ip', 'link', 'set', BRIDGE, 'up')
    run('sysctl', '-w', 'net.ipv4.ip_forward=1')
    # Insert our narrow interface/source matches ahead of Docker's DROP policy.
    # Do not flush any chain, change default policies, or remove another rule.
    ensure_rule('filter', 'FORWARD', '-i', BRIDGE, '-j', 'ACCEPT')
    ensure_rule('filter', 'FORWARD', '-o', BRIDGE, '-m', 'conntrack', '--ctstate', 'RELATED,ESTABLISHED', '-j', 'ACCEPT')
    ensure_rule('nat', 'POSTROUTING', '-s', str(subnet), '!', '-o', BRIDGE, '-j', 'MASQUERADE')



def tap_probe():
    assert os.getuid() == 55050 and os.getgid() == 55050
    kvm = os.open('/dev/kvm', os.O_RDWR | os.O_CLOEXEC)
    try:
        vm = fcntl.ioctl(kvm, 0xae01, 0)
        os.close(vm)
    finally:
        os.close(kvm)
    conf = json.loads(CONFIG.read_text())
    name = 'pk' + uuid.uuid4().hex[:10]
    run('ip', 'tuntap', 'add', 'dev', name, 'mode', 'tap')
    try:
        run('ip', 'link', 'set', name, 'master', conf['bridge'])
        run('ip', 'link', 'set', name, 'up')
    finally:
        run('ip', 'link', 'delete', name)


def probe(preparing=False):
    assert os.getuid() == 55050 and os.getgid() == 55050
    conf = json.loads(CONFIG.read_text())
    assert conf['version'] == VERSION and conf['boot_id'] == boot_id(), 'stale network configuration'
    if not preparing:
        ready = json.loads(READY.read_text())
        assert ready['version'] == VERSION and ready['boot_id'] == boot_id(), 'stale capability readiness'
    for attempt in range(100):
        if Path('/run/user/55050/bus').exists():
            break
        time.sleep(.1)
    assert Path('/run/user/55050').stat().st_uid == 55050
    assert Path('/run/user/55050/bus').stat().st_uid == 55050
    fd = os.open('/dev/kvm', os.O_RDWR | os.O_CLOEXEC)
    try:
        assert fcntl.ioctl(fd, 0xae00, 0) == 12
        vm = fcntl.ioctl(fd, 0xae01, 0)
        os.close(vm)
    finally:
        os.close(fd)
    tap_probe()
    assert run('systemctl', 'is-active', 'pulse-kvm-dns.service') == 'active'
    for attempt in range(20):
        try:
            with socket.create_connection((conf['gateway'], 53), timeout=1):
                break
        except OSError:
            if attempt == 19:
                raise
            time.sleep(.1)
    run('systemd-run', '--user', '--quiet', '--wait', '--pipe', '--collect',
        '--unit=pulse-kvm-probe-' + uuid.uuid4().hex,
        '--setenv=XDG_RUNTIME_DIR=/run/user/55050',
        '--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/55050/bus',
        WRAPPER, '/usr/bin/python3', SELF, '--tap-probe')
    assert Path('/proc/sys/net/ipv4/ip_forward').read_text().strip() == '1'
    print('PULSE_KVM_NETWORK_READY')


def dns_servers():
    # Use the outer guest's resolver, including its loopback stub. Nested guests
    # reach this forwarder on the bridge gateway instead of inheriting loopback.
    servers = []
    for line in Path('/etc/resolv.conf').read_text().splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[0] == 'nameserver':
            servers.append(fields[1])
    if not servers:
        raise RuntimeError('outer guest has no DNS resolver')
    return servers


def read_exact(stream, size):
    out = b''
    while len(out) < size:
        part = stream.recv(size - len(out))
        if not part:
            raise EOFError('short DNS frame')
        out += part
    return out


def forward_dns(data, tcp):
    for server in dns_servers():
        try:
            family = socket.AF_INET6 if ':' in server else socket.AF_INET
            with socket.socket(family, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM) as upstream:
                upstream.settimeout(3)
                upstream.connect((server, 53))
                if tcp:
                    upstream.sendall(len(data).to_bytes(2, 'big') + data)
                    return read_exact(upstream, int.from_bytes(read_exact(upstream, 2), 'big'))
                upstream.send(data)
                return upstream.recv(65535)
        except (OSError, EOFError):
            continue
    raise RuntimeError('outer guest DNS unavailable')


class UDPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            self.request[1].sendto(forward_dns(self.request[0], False), self.client_address)
        except RuntimeError:
            pass


class TCPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            self.request.settimeout(10)
            while True:
                size = int.from_bytes(read_exact(self.request, 2), 'big')
                answer = forward_dns(read_exact(self.request, size), True)
                self.request.sendall(len(answer).to_bytes(2, 'big') + answer)
        except (OSError, EOFError, RuntimeError):
            pass


class BoundedServer:
    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        self.executor.submit(self.work, request, address)

    def work(self, request, address):
        try:
            self.finish_request(request, address)
        finally:
            self.shutdown_request(request)
            self.slots.release()


class UDPServer(BoundedServer, socketserver.UDPServer):
    pass


class TCPServer(BoundedServer, socketserver.TCPServer):
    allow_reuse_address = True


def serve_dns():
    gateway = json.loads(CONFIG.read_text())['gateway']
    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as executor:
        with UDPServer((gateway, 53), UDPHandler) as udp, TCPServer((gateway, 53), TCPHandler) as tcp:
            for server in (udp, tcp):
                server.executor = executor
                server.slots = threading.BoundedSemaphore(16)
            threading.Thread(target=udp.serve_forever, daemon=True).start()
            tcp.serve_forever()


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('operation', nargs='?', default='configure', choices=['configure', 'dns', 'certify'])
    parser.add_argument('--probe', action='store_true')
    parser.add_argument('--prepare-probe', action='store_true')
    parser.add_argument('--tap-probe', action='store_true')
    args = parser.parse_args()
    if args.tap_probe:
        tap_probe()
    elif args.probe or args.prepare_probe:
        probe(args.prepare_probe)
    elif args.operation == 'certify':
        assert os.geteuid() == 0
        run('sudo', '-n', '-H', '-u', 'pulseworker', WRAPPER, '/usr/bin/python3', SELF, '--prepare-probe')
        write_json(READY, dict(version=VERSION, boot_id=boot_id()))
    elif args.operation == 'dns':
        serve_dns()
    else:
        configure()
