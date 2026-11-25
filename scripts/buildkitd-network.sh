#!/bin/sh
set -eu

# Swarm services mount /sys/fs/cgroup read-only; runc needs write access for build sandboxes.
if grep -q ' /sys/fs/cgroup .*[, ]ro[, ]' /proc/mounts; then
	mount -o remount,rw /sys/fs/cgroup
fi

# CNI's firewall jumps to CNI-ADMIN before accepting worker traffic.
iptables -C INPUT -i buildkit0 -j DROP 2>/dev/null || iptables -I INPUT -i buildkit0 -j DROP
iptables -N CNI-ADMIN 2>/dev/null || iptables -F CNI-ADMIN
for subnet in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 \
	172.16.0.0/12 192.0.0.0/24 192.168.0.0/16 198.18.0.0/15 224.0.0.0/3 \
	${BUILDKIT_BLOCKED_SUBNETS:?Missing Swarm network subnets}; do
	case "$subnet" in
		*:*) continue ;;
	esac
	iptables -A CNI-ADMIN -i buildkit0 -d "$subnet" -j DROP
done
ip6tables -C INPUT -i buildkit0 -j DROP 2>/dev/null || ip6tables -I INPUT -i buildkit0 -j DROP
ip6tables -C FORWARD -i buildkit0 -j DROP 2>/dev/null || ip6tables -I FORWARD -i buildkit0 -j DROP

exec /usr/bin/buildkitd-entrypoint
