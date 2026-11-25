#!/bin/sh
# End-to-end verification of the isolated BuildKit worker Swarm service.
# Runs an isolated docker:28-dind Swarm with a fixture mTLS registry, fixture
# certificates, and a panelnet listener, then tests provisioning, mTLS
# readiness, RUN isolation, build+push, retry with cache preservation, and removal.
set -eu
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."

DIND="${DIND_IMAGE:-docker:28-dind}"
ALPINE="alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f"
REGISTRY_IMAGE="registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"
MGR="builder-worker-smoke-$$"

for file in deployment/buildkit-worker.json scripts/buildkitd.toml scripts/buildkitd-network.sh; do
	test -s "$file" || { echo "Missing $file" >&2; exit 1; }
done

docker info >/dev/null

cleanup() {
	status=$?
	trap - EXIT
	if [ "$status" -ne 0 ]; then
		docker exec "$MGR" docker service ps --no-trunc builder_buildkitd >&2 2>/dev/null || true
		docker exec "$MGR" docker service logs --tail 50 builder_buildkitd >&2 2>/dev/null || true
	fi
	docker rm -f -v "$MGR" >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "==> starting isolated DinD Swarm manager ($MGR)"
docker run -d --privileged --name "$MGR" --hostname "$MGR" \
	-e DOCKER_TLS_CERTDIR= \
	"$DIND" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null

attempts=0
until docker exec "$MGR" docker info >/dev/null 2>&1; do
	attempts=$((attempts + 1))
	[ "$attempts" -lt 60 ] || { docker logs "$MGR" >&2; exit 1; }
	sleep 1
done

docker exec "$MGR" docker swarm init >/dev/null
docker exec "$MGR" mkdir -p /work/scripts /work/deployment
docker cp deployment/buildkit-worker.json "$MGR:/work/deployment/buildkit-worker.json"
docker cp scripts/buildkitd.toml "$MGR:/work/scripts/buildkitd.toml"
docker cp scripts/buildkitd-network.sh "$MGR:/work/scripts/buildkitd-network.sh"

docker exec -i "$MGR" sh -s "$ALPINE" "$REGISTRY_IMAGE" <<'EOF'
set -eu
ALPINE=$1
REGISTRY_IMAGE=$2
cd /work

# Parse declarative worker spec with jq inside an alpine container (or python/sed).
docker run --rm -v /work:/work "$ALPINE" sh -c '
  set -eu
  apk add --no-cache jq openssl >/dev/null

  # Generate separate BuildKit and Registry CAs + leaf mTLS certs.
  mkdir -p /work/tls
  openssl genrsa -out /work/tls/buildkit-ca-key.pem 2048 2>/dev/null
  openssl req -x509 -new -nodes -key /work/tls/buildkit-ca-key.pem -days 1 -subj "/CN=buildkit-ca" -out /work/tls/buildkit-ca.pem 2>/dev/null

  openssl genrsa -out /work/tls/buildkit-server-key.pem 2048 2>/dev/null
  openssl req -new -key /work/tls/buildkit-server-key.pem -subj "/CN=buildkitd" -out /tmp/bk-srv.csr 2>/dev/null
  printf "subjectAltName=DNS:buildkitd\nextendedKeyUsage=serverAuth\n" > /tmp/bk-srv.ext
  openssl x509 -req -in /tmp/bk-srv.csr -CA /work/tls/buildkit-ca.pem -CAkey /work/tls/buildkit-ca-key.pem -CAcreateserial -days 1 -extfile /tmp/bk-srv.ext -out /work/tls/buildkit-server.pem 2>/dev/null

  openssl genrsa -out /work/tls/buildkit-client-key.pem 2048 2>/dev/null
  openssl req -new -key /work/tls/buildkit-client-key.pem -subj "/CN=builder-module" -out /tmp/bk-cli.csr 2>/dev/null
  printf "extendedKeyUsage=clientAuth\n" > /tmp/cli.ext
  openssl x509 -req -in /tmp/bk-cli.csr -CA /work/tls/buildkit-ca.pem -CAkey /work/tls/buildkit-ca-key.pem -CAcreateserial -days 1 -extfile /tmp/cli.ext -out /work/tls/buildkit-client.pem 2>/dev/null

  openssl genrsa -out /work/tls/registry-ca-key.pem 2048 2>/dev/null
  openssl req -x509 -new -nodes -key /work/tls/registry-ca-key.pem -days 1 -subj "/CN=registry-ca" -out /work/tls/registry-ca.pem 2>/dev/null

  openssl genrsa -out /work/tls/registry-server-key.pem 2048 2>/dev/null
  openssl req -new -key /work/tls/registry-server-key.pem -subj "/CN=registry" -out /tmp/reg-srv.csr 2>/dev/null
  printf "subjectAltName=DNS:registry\nextendedKeyUsage=serverAuth\n" > /tmp/reg-srv.ext
  openssl x509 -req -in /tmp/reg-srv.csr -CA /work/tls/registry-ca.pem -CAkey /work/tls/registry-ca-key.pem -CAcreateserial -days 1 -extfile /tmp/reg-srv.ext -out /work/tls/registry-server.pem 2>/dev/null

  openssl genrsa -out /work/tls/registry-client-key.pem 2048 2>/dev/null
  openssl req -new -key /work/tls/registry-client-key.pem -subj "/CN=buildkitd" -out /tmp/reg-cli.csr 2>/dev/null
  openssl x509 -req -in /tmp/reg-cli.csr -CA /work/tls/registry-ca.pem -CAkey /work/tls/registry-ca-key.pem -CAcreateserial -days 1 -extfile /tmp/cli.ext -out /work/tls/registry-client.pem 2>/dev/null

  # Unrelated client cert for negative mTLS rejection check.
  openssl genrsa -out /work/tls/rogue-client-key.pem 2048 2>/dev/null
  openssl req -new -key /work/tls/rogue-client-key.pem -subj "/CN=rogue" -out /tmp/rogue.csr 2>/dev/null
  openssl x509 -req -in /tmp/rogue.csr -CA /work/tls/registry-ca.pem -CAkey /work/tls/registry-ca-key.pem -CAcreateserial -days 1 -extfile /tmp/cli.ext -out /work/tls/rogue-client.pem 2>/dev/null

  jq -r .image /work/deployment/buildkit-worker.json > /work/worker.image
  jq -r .storage.volume /work/deployment/buildkit-worker.json > /work/worker.volume
  jq -r .storage.target /work/deployment/buildkit-worker.json > /work/worker.target
  jq -r .network.name /work/deployment/buildkit-worker.json > /work/worker.net
  jq -r .network.alias /work/deployment/buildkit-worker.json > /work/worker.alias
'

WORKER_IMAGE=$(cat /work/worker.image)
WORKER_VOL="builder_$(cat /work/worker.volume)"
WORKER_TARGET=$(cat /work/worker.target)
WORKER_NET=$(cat /work/worker.net)
WORKER_ALIAS=$(cat /work/worker.alias)

echo "==> creating isolated networks, fixture panelnet listener, and fixture mTLS registry"
docker network create --driver overlay --attachable panelnet >/dev/null
docker network create --driver overlay --attachable "$WORKER_NET" >/dev/null
BLOCKED_SUBNETS=$(docker network inspect --format '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' panelnet "$WORKER_NET" docker_gwbridge | tr '\n' ' ')

docker service create --name panel_probe --network panelnet "$ALPINE" \
	sh -c 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nPANEL" | nc -l -p 8080; done' >/dev/null

docker secret create builder_buildkit_ca /work/tls/buildkit-ca.pem >/dev/null
docker secret create builder_buildkit_server /work/tls/buildkit-server.pem >/dev/null
docker secret create builder_buildkit_server_key /work/tls/buildkit-server-key.pem >/dev/null
docker secret create builder_registry_ca /work/tls/registry-ca.pem >/dev/null
docker secret create builder_registry_cert /work/tls/registry-server.pem >/dev/null
docker secret create builder_registry_key /work/tls/registry-server-key.pem >/dev/null
docker secret create builder_buildkit_registry_client /work/tls/registry-client.pem >/dev/null
docker secret create builder_buildkit_registry_client_key /work/tls/registry-client-key.pem >/dev/null

docker service create --name registry --network "$WORKER_NET" \
	-e REGISTRY_HTTP_TLS_CERTIFICATE=/run/secrets/registry.pem \
	-e REGISTRY_HTTP_TLS_KEY=/run/secrets/registry-key.pem \
	-e 'REGISTRY_HTTP_TLS_CLIENTCAS=[/run/secrets/registry-ca.pem]' \
	-e REGISTRY_HTTP_RELATIVEURLS=true \
	--secret source=builder_registry_cert,target=registry.pem,mode=0400 \
	--secret source=builder_registry_key,target=registry-key.pem,mode=0400 \
	--secret source=builder_registry_ca,target=registry-ca.pem,mode=0400 \
	"$REGISTRY_IMAGE" >/dev/null

docker config create builder_buildkitd_toml /work/scripts/buildkitd.toml >/dev/null
docker config create builder_buildkitd_net /work/scripts/buildkitd-network.sh >/dev/null

NODE_ID=$(docker info --format '{{.Swarm.NodeID}}')
BUILDNET_ID=$(docker network inspect --format '{{.Id}}' "$WORKER_NET")
CFG_TOML_ID=$(docker config inspect --format '{{.ID}}' builder_buildkitd_toml)
CFG_NET_ID=$(docker config inspect --format '{{.ID}}' builder_buildkitd_net)
S_BK_CA=$(docker secret inspect --format '{{.ID}}' builder_buildkit_ca)
S_BK_SRV=$(docker secret inspect --format '{{.ID}}' builder_buildkit_server)
S_BK_KEY=$(docker secret inspect --format '{{.ID}}' builder_buildkit_server_key)
S_REG_CA=$(docker secret inspect --format '{{.ID}}' builder_registry_ca)
S_REG_CLI=$(docker secret inspect --format '{{.ID}}' builder_buildkit_registry_client)
S_REG_KEY=$(docker secret inspect --format '{{.ID}}' builder_buildkit_registry_client_key)

provision_worker() {
	docker volume create "$WORKER_VOL" >/dev/null
	cat > /work/worker-service.json <<JSON
{
  "Name": "builder_buildkitd",
  "TaskTemplate": {
    "ContainerSpec": {
      "Image": "$WORKER_IMAGE",
      "Env": ["BUILDKIT_BLOCKED_SUBNETS=$BLOCKED_SUBNETS"],
      "Command": ["/bin/sh"],
      "Args": ["/etc/buildkit/network.sh"],
      "CapabilityAdd": ["SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"],
      "Privileges": {
        "Seccomp": {"Mode": "unconfined"},
        "AppArmor": {"Mode": "disabled"}
      },
      "Healthcheck": {
        "Test": ["CMD-SHELL", "buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1 && nc -z 127.0.0.1 1234"],
        "Interval": 5000000000,
        "Timeout": 5000000000,
        "StartPeriod": 5000000000,
        "Retries": 3
      },
      "Mounts": [
        {"Type": "volume", "Source": "$WORKER_VOL", "Target": "$WORKER_TARGET"}
      ],
      "Configs": [
        {"ConfigID": "$CFG_TOML_ID", "ConfigName": "builder_buildkitd_toml", "File": {"Name": "/etc/buildkit/buildkitd.toml", "UID": "0", "GID": "0", "Mode": 292}},
        {"ConfigID": "$CFG_NET_ID", "ConfigName": "builder_buildkitd_net", "File": {"Name": "/etc/buildkit/network.sh", "UID": "0", "GID": "0", "Mode": 365}}
      ],
      "Secrets": [
        {"SecretID": "$S_BK_CA", "SecretName": "builder_buildkit_ca", "File": {"Name": "/etc/buildkit/tls/buildkit-ca.pem", "UID": "0", "GID": "0", "Mode": 256}},
        {"SecretID": "$S_BK_SRV", "SecretName": "builder_buildkit_server", "File": {"Name": "/etc/buildkit/tls/buildkit-server.pem", "UID": "0", "GID": "0", "Mode": 256}},
        {"SecretID": "$S_BK_KEY", "SecretName": "builder_buildkit_server_key", "File": {"Name": "/etc/buildkit/tls/buildkit-server-key.pem", "UID": "0", "GID": "0", "Mode": 256}},
        {"SecretID": "$S_REG_CA", "SecretName": "builder_registry_ca", "File": {"Name": "/etc/buildkit/tls/registry-ca.pem", "UID": "0", "GID": "0", "Mode": 256}},
        {"SecretID": "$S_REG_CLI", "SecretName": "builder_buildkit_registry_client", "File": {"Name": "/etc/buildkit/tls/registry-client.pem", "UID": "0", "GID": "0", "Mode": 256}},
        {"SecretID": "$S_REG_KEY", "SecretName": "builder_buildkit_registry_client_key", "File": {"Name": "/etc/buildkit/tls/registry-client-key.pem", "UID": "0", "GID": "0", "Mode": 256}}
      ]
    },
    "Networks": [{"Target": "$BUILDNET_ID", "Aliases": ["$WORKER_ALIAS"]}],
    "Placement": {"Constraints": ["node.id==$NODE_ID"]}
  },
  "UpdateConfig": {"Order": "stop-first"}
}
JSON
	wget -qO- --header='Content-Type: application/json' --post-file=/work/worker-service.json http://127.0.0.1:2375/services/create >/dev/null
}

wait_worker_ready() {
	ready=0
	for _ in $(seq 1 45); do
		cid=$(docker ps -q --filter "label=com.docker.swarm.service.name=builder_buildkitd" | head -n1)
		if [ -n "$cid" ]; then
			health=$(docker inspect --format '{{.State.Health.Status}}' "$cid" 2>/dev/null || true)
			if [ "$health" = "healthy" ] && docker run --rm --network "$WORKER_NET" \
				-v /work/tls:/tls:ro --entrypoint /usr/bin/buildctl "$WORKER_IMAGE" \
				--addr "tcp://$WORKER_ALIAS:1234" \
				--tlscacert /tls/buildkit-ca.pem \
				--tlscert /tls/buildkit-client.pem \
				--tlskey /tls/buildkit-client-key.pem \
				debug workers >/dev/null 2>&1; then
				ready=1
				break
			fi
		fi
		sleep 1
	done
	[ "$ready" -eq 1 ] || { echo "BuildKit worker did not pass health and mTLS readiness" >&2; exit 1; }
}

remove_worker_service() {
	docker service rm builder_buildkitd >/dev/null 2>&1 || true
	# Wait for the container holding buildkitd.lock to exit before retrying or deleting storage.
	for _ in $(seq 1 30); do
		if [ -z "$(docker ps -aq --filter 'label=com.docker.swarm.service.name=builder_buildkitd')" ]; then
			return 0
		fi
		sleep 1
	done
	echo "Timed out waiting for builder_buildkitd container removal" >&2
	exit 1
}

echo "==> provisioning isolated BuildKit worker"
provision_worker
wait_worker_ready

echo "==> checking mTLS enforcement"
if docker run --rm --network "$WORKER_NET" \
	-v /work/tls:/tls:ro --entrypoint /usr/bin/buildctl "$WORKER_IMAGE" \
	--addr "tcp://$WORKER_ALIAS:1234" \
	--tlscacert /tls/buildkit-ca.pem \
	--tlscert /tls/rogue-client.pem \
	--tlskey /tls/rogue-client-key.pem \
	debug workers >/dev/null 2>&1; then
	echo "Expected untrusted mTLS client certificate to be rejected" >&2
	exit 1
fi

echo "==> running fixture Dockerfile with RUN isolation checks and mTLS push (v1)"
PANEL_VIP=$(docker service inspect panel_probe --format '{{range .Endpoint.VirtualIPs}}{{.Addr}}{{end}}' | cut -d/ -f1)
mkdir -p /work/src
cat > /work/src/Dockerfile <<DF
FROM $ALPINE
RUN set -eu; \
  [ "\$(ps -o pid= | wc -l)" -le 4 ]; \
  grep -q '^Seccomp:\s*2$' /proc/self/status; \
  ! test -e /etc/buildkit/tls/buildkit-server-key.pem; \
  ! test -e /var/lib/buildkit/buildkitd.lock; \
  ! wget -qO- -T 2 "http://$PANEL_VIP:8080/" 2>/dev/null; \
  ! wget -qO- -T 2 https://registry:5000/v2/ 2>/dev/null; \
  ! nc -z -w 2 "$WORKER_ALIAS" 1234 2>/dev/null; \
  echo "v1" > /built.txt
DF

docker run --rm --network "$WORKER_NET" \
	-v /work/tls:/tls:ro -v /work/src:/src:ro \
	--entrypoint /usr/bin/buildctl "$WORKER_IMAGE" \
	--addr "tcp://$WORKER_ALIAS:1234" \
	--tlscacert /tls/buildkit-ca.pem \
	--tlscert /tls/buildkit-client.pem \
	--tlskey /tls/buildkit-client-key.pem \
	build --progress=plain --frontend dockerfile.v0 --local context=/src --local dockerfile=/src \
	--output type=image,name=registry:5000/fixture/app:v1,push=true

docker run --rm --network "$WORKER_NET" -v /work/tls:/tls:ro "$ALPINE" sh -c '
  apk add --no-cache curl >/dev/null
  curl -fsS -H "Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json" \
    --cacert /tls/registry-ca.pem --cert /tls/registry-client.pem --key /tls/registry-client-key.pem \
    https://registry:5000/v2/fixture/app/manifests/v1 >/dev/null
'

echo "==> verifying retry with preserved cache volume (v2)"
remove_worker_service
provision_worker
wait_worker_ready

sed -i 's/echo "v1"/echo "v2"/' /work/src/Dockerfile
docker run --rm --network "$WORKER_NET" \
	-v /work/tls:/tls:ro -v /work/src:/src:ro \
	--entrypoint /usr/bin/buildctl "$WORKER_IMAGE" \
	--addr "tcp://$WORKER_ALIAS:1234" \
	--tlscacert /tls/buildkit-ca.pem \
	--tlscert /tls/buildkit-client.pem \
	--tlskey /tls/buildkit-client-key.pem \
	build --progress=plain --frontend dockerfile.v0 --local context=/src --local dockerfile=/src \
	--output type=image,name=registry:5000/fixture/app:v2,push=true

docker run --rm --network "$WORKER_NET" -v /work/tls:/tls:ro "$ALPINE" sh -c '
  apk add --no-cache curl >/dev/null
  curl -fsS -H "Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json" \
    --cacert /tls/registry-ca.pem --cert /tls/registry-client.pem --key /tls/registry-client-key.pem \
    https://registry:5000/v2/fixture/app/manifests/v2 >/dev/null
'

echo "==> verifying worker removal"
remove_worker_service
docker config rm builder_buildkitd_toml builder_buildkitd_net >/dev/null
docker secret rm builder_buildkit_ca builder_buildkit_server builder_buildkit_server_key \
	builder_buildkit_registry_client builder_buildkit_registry_client_key >/dev/null
docker volume rm "$WORKER_VOL" >/dev/null
! docker service inspect builder_buildkitd >/dev/null 2>&1
! docker volume inspect "$WORKER_VOL" >/dev/null 2>&1
echo "Isolated BuildKit worker verification passed."
EOF
