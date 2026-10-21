#!/bin/sh
# Starts the BuildKit daemon the builder module talks to. Run it once, as root, on
# a node of the swarm (a manager is fine).
#
# BuildKit needs --privileged, which a Swarm service cannot have, so it runs as
# a plain container. It listens on a network of its own that only the builder
# module and the registry join: that network is its only access control.
set -eu

BUILDKIT_VERSION="${BUILDKIT_VERSION:-v0.23.2}"
NET="${BUILD_NETWORK:-buildnet}"

docker network inspect "$NET" >/dev/null 2>&1 ||
	docker network create --driver overlay --attachable "$NET"

docker rm -f buildkitd >/dev/null 2>&1 || true
docker run -d --name buildkitd --restart always --privileged \
	--network "$NET" --network-alias buildkitd \
	-v buildkitd_state:/var/lib/buildkit \
	"moby/buildkit:$BUILDKIT_VERSION" --addr tcp://0.0.0.0:1234

echo "buildkitd is running on network $NET as buildkitd:1234"
