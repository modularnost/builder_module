#!/bin/sh
# Starts the BuildKit daemon the builder module talks to. Run it once, as root, on
# a node of the swarm (a manager is fine).
#
# BuildKit runs as a plain container because Swarm services cannot use
# --privileged. Its TCP listener requires a trusted client certificate.
set -eu

BUILDKIT_IMAGE="${BUILDKIT_IMAGE:-moby/buildkit:v0.35.1@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c}"
NET="${BUILD_NETWORK:-buildnet}"
: "${BUILDKIT_TLS_DIR:?Set BUILDKIT_TLS_DIR to the directory with the daemon and registry client certificates}"
TLS_DIR=$(cd "$BUILDKIT_TLS_DIR" && pwd)
CONFIG=$(cd "$(dirname "$0")" && pwd)/buildkitd.toml

for name in buildkit-ca.pem buildkit-server.pem buildkit-server-key.pem \
	registry-ca.pem registry-client.pem registry-client-key.pem; do
	test -s "$TLS_DIR/$name" || { echo "Missing $TLS_DIR/$name" >&2; exit 1; }
done

docker network inspect "$NET" >/dev/null 2>&1 ||
	docker network create --driver overlay --attachable "$NET"

docker image inspect "$BUILDKIT_IMAGE" >/dev/null 2>&1 || docker pull "$BUILDKIT_IMAGE"
docker rm -f buildkitd >/dev/null 2>&1 || true
docker run -d --name buildkitd --restart always --privileged \
	--network "$NET" --network-alias buildkitd \
	-v buildkitd_state:/var/lib/buildkit \
	--mount "type=bind,src=$TLS_DIR,dst=/etc/buildkit/tls,readonly" \
	--mount "type=bind,src=$CONFIG,dst=/etc/buildkit/buildkitd.toml,readonly" \
	"$BUILDKIT_IMAGE"

echo "buildkitd is running on network $NET as buildkitd:1234"
