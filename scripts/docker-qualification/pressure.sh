#!/bin/bash
# Bounded container cgroups, never host-wide memory/CPU exhaustion.
set -euo pipefail
trap 'status=$?; printf "PRESSURE_FAILURE line=%s status=%s\n" "$LINENO" "$status" >&2' ERR
export DOCKER_HOST=unix:///var/run/docker.sock
cleanup() { docker rm -f qualification-pressure qualification-oom >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker run -d --name qualification-pressure --cpus=3.5 --memory=6g --memory-swap=6g python:3.13-alpine python -c '
import multiprocessing
def stress():
    data=bytearray(1024*1024*1024)
    while True:
        data[0]=(data[0]+1)%256
workers=[multiprocessing.Process(target=stress) for _ in range(4)]
for worker in workers: worker.start()
for worker in workers: worker.join()
' >/dev/null
sleep 15
quota=$(docker inspect -f '{{.HostConfig.NanoCpus}}' qualification-pressure)
echo "CPU_NANOS=$quota"
test "$quota" = 3500000000
docker exec qualification-pressure python -c 'from pathlib import Path; s=dict(x.split() for x in Path("/sys/fs/cgroup/cpu.stat").read_text().splitlines()); memory=int(Path("/sys/fs/cgroup/memory.current").read_text()); print("CPU_THROTTLED="+s["nr_throttled"],"MEMORY_BYTES="+str(memory),flush=True); assert int(s["nr_throttled"])>0, "CPU quota was not exercised"; assert memory>3*1024**3, "memory pressure did not reach 3 GiB"'
docker rm -f qualification-pressure >/dev/null
docker run -d --name qualification-oom --memory=64m --memory-swap=64m python:3.13-alpine python -c 'x=bytearray(256*1024*1024); print(len(x))' >/dev/null
oom_exit=$(docker wait qualification-oom)
oom_killed=$(docker inspect -f '{{.State.OOMKilled}}' qualification-oom)
oom_id=$(docker inspect -f '{{.Id}}' qualification-oom)
oom_limit=$(docker inspect -f '{{.HostConfig.Memory}}' qualification-oom)
echo "OOM_CONTAINER=$oom_id"
echo "OOM_LIMIT_BYTES=$oom_limit"
echo "OOM_EXIT=$oom_exit OOM_KILLED=$oom_killed"
test "$oom_limit" = 67108864
test "$oom_exit" = 137
# The caller must verify this exact container's kernel OOM record on the engine.
# The pinned Docker/containerd stack can omit OOMKilled even after a memcg kill.
# Exit 137 alone is not proof: an ordinary SIGKILL has the same exit status.
echo CONTAINER_OOM_EXIT_OBSERVED
