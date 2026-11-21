#!/bin/bash
# Prepare a reusable, cold Linux base. No allocation disk, keys, or job state.
set -euo pipefail
set +x
umask 077
[[ $(uname -s) == Linux && $EUID == 0 ]]
. /etc/os-release
[[ $ID == ubuntu && ( $VERSION_ID == 24.04 || $VERSION_ID == 26.04 ) ]]
version=$1
[[ ! -e /var/db/pulse-tart/intent && ! -e /var/db/pulse-macvz/intent && ! -e /var/lib/pulse-docker/disk-ready ]]
export DEBIAN_FRONTEND=noninteractive
if ! command -v docker >/dev/null || [[ $(docker --version) != *" $version,"* ]]; then
 apt-get update
 # Drain the complete producer under pipefail, including large inventories.
 selected=$(apt-cache madison docker.io | awk -v v="$version" '!found && $3 ~ "^([0-9]+:)?" v "([+~.-]|$)" {print $3;found=1}')
 [[ -n $selected ]]
 apt-get install -y "docker.io=$selected" docker-buildx docker-compose-v2 openssh-server e2fsprogs
fi
[[ $(docker --version) == *" $version,"* ]]
docker buildx version
docker compose version
command -v mkfs.ext4 >/dev/null
test -x /usr/sbin/sshd
# Package installation may start services. Publish software, never daemon state.
systemctl stop docker.socket docker.service containerd.service
systemctl disable docker.service docker.socket containerd.service
! systemctl is-active --quiet docker.service docker.socket containerd.service
rm -f /var/run/docker.sock
rm -rf /var/lib/docker /var/lib/containerd
sync
echo PULSE_DOCKER_ENGINE_BASE_READY
