# Install Builder

Run the commands on a Linux Swarm manager from the Builder checkout.

## Deployment files for installers

Published images contain the shared module deployment bundle at
`/usr/share/modularnost/deploy`. Its `manifest.json` names the Compose files in
merge order and declares Builder's need for encrypted registry credentials.
An installer can copy the directory from a created container. Use the same
digest-pinned image for `BUILDER_IMAGE` when deploying the stack so the module
and configuration come from the same build.

The directory includes the base stack, the private registry pull overlay,
the Caddy configuration, and the BuildKit startup files under `scripts/`.
The installer supplies the cluster networks, registry route, and secrets
described below.

## Image and panel network

Set `BUILDER_IMAGE` to a tag or digest matching this checkout. If you build
the image locally, push it to a registry the managers can pull from.

```sh
export BUILDER_IMAGE=images.internal.example/builder-module:preview
docker build -t "$BUILDER_IMAGE" .
docker push "$BUILDER_IMAGE"
```

Replace the example hostname with your registry. If it requires credentials,
log in on the manager before pushing or deploying the image.

The Builder stack expects an external network named `panelnet`. Create the
following `stack.panel-network.yml` for the panel installer's default network.

```yaml
networks:
  panelnet:
    external: true
    name: modularnost
```

Use your panel's `PANEL_NETWORK` value if it differs. If the panel is already
on `panelnet`, omit the override file from the deploy command.

## Certificates and stack

Use separate CAs for BuildKit and the internal registry. Issue server
certificates with the `serverAuth` extended key usage and DNS SANs
`buildkitd` and `registry`, respectively. Issue three client certificates
with `clientAuth` for the module (BuildKit CA), the BuildKit daemon (registry
CA), and the pull proxy (registry CA). Keep the CA private keys offline.

In the root-only directory `/etc/builder/buildkit-tls`, place
`buildkit-ca.pem`, `buildkit-server.pem`, `buildkit-server-key.pem`,
`registry-ca.pem`, `registry-client.pem`, and `registry-client-key.pem`.
The registry client pair here belongs to the BuildKit daemon.

From the directory holding the certificate files named below, create the
Swarm secrets, then return to the Builder checkout to start the services.

```sh
docker secret create builder_buildkit_ca buildkit-ca.pem
docker secret create builder_buildkit_client buildkit-client.pem
docker secret create builder_buildkit_client_key buildkit-client-key.pem
docker secret create builder_registry_ca registry-ca.pem
docker secret create builder_registry_cert registry-server.pem
docker secret create builder_registry_key registry-server-key.pem
```

```sh
BUILDKIT_TLS_DIR=/etc/builder/buildkit-tls sh scripts/buildkitd.sh
REGISTRY_PULL=registry.internal.example docker stack deploy \
  --with-registry-auth -c stack.yml -c stack.panel-network.yml builder
```

Set `REGISTRY_PULL` to the pull proxy hostname and optional port, without a
URL scheme. The stack uses the same value for the module's registry label.

## Pull proxy

Use the [Caddy configuration](../scripts/registry-pull.Caddyfile) on a Swarm
node attached to `buildnet`. The proxy requires Basic Auth for `GET` and
`HEAD` on the registry API and connects to `registry:5000` using mTLS.
Keep the upstream registry unpublished.

Place these files in the root-only directory `/etc/builder/registry-pull`.

| Files | Purpose |
| --- | --- |
| `proxy.pem`, `proxy-key.pem` | TLS certificate chain and key for the pull hostname |
| `registry-ca.pem` | CA used to verify the internal registry |
| `registry-client.pem`, `registry-client-key.pem` | The proxy's registry client certificate and key |
| `users` | A line containing `builder-pull <bcrypt hash>` |

Use a random token and generate its bcrypt hash with `caddy hash-password`.
Retain the original token to enter in the panel.

```sh
docker run -d --name builder-pull-proxy --restart unless-stopped \
  --network buildnet -p 192.0.2.10:443:443 \
  -e REGISTRY_PULL_HOST=registry.internal.example \
  --mount "type=bind,src=$PWD/scripts/registry-pull.Caddyfile,dst=/etc/caddy/Caddyfile,readonly" \
  --mount type=bind,src=/etc/builder/registry-pull,dst=/etc/registry-pull,readonly \
  caddy:2.11.7@sha256:36c24ad91a7ff7bc4c87fe22bfa8df3c4b18da0773d73fc12ee2c52b98c0bb7e
```

Replace the example address with the node's private IP and point the pull
hostname at that address. Docker must resolve and trust the endpoint on each
eligible node. For a private CA, install it at
`/etc/docker/certs.d/<host[:port]>/ca.crt` on each node.

In the panel, save the username `builder-pull` and the original token for the
exact `REGISTRY_PULL` value. Verify that an anonymous request to `/v2/`
returns `401` and that `docker login <host[:port]>` accepts the token.
Open **Builder** in the panel to add and deploy an app.

### Swarm pull proxy overlay

`stack.registry-pull.yml` packages the same proxy as a Swarm service for
provisioning by an installer. Apply it with `stack.yml`. The installer sets
`REGISTRY_PULL_HOST` to the certificate hostname and `REGISTRY_PULL_NETWORK` to
an existing private overlay network. Route TLS for `REGISTRY_PULL` to
`builder-registry-pull:443` on that network with TCP passthrough. The service
publishes no host port.

Supply `builder_pull_cert` and `builder_pull_key` for the pull endpoint,
`builder_pull_client` and `builder_pull_client_key` for registry mTLS, and
`builder_pull_users` for the bcrypt users file. The service reuses
`builder_registry_ca` from the base stack.

## Upgrade

Use the stack, daemon script, and Builder image from the same revision. If
upgrading from a release that ran Builder as root, migrate the existing data
volume on the node that holds it. The example uses the default stack and
volume names.

```sh
docker service scale builder_module=0
docker run --rm -v builder_builder_data:/data alpine:3.22 \
  sh -ec 'chown 10001:10001 /data
    for file in /data/apps.json /data/apps.json.tmp; do
      if [ -f "$file" ]; then chown -h 10001:10001 "$file"; fi
    done'
```

Start BuildKit and deploy the stack as above. Before enforcing Basic Auth on
an existing pull endpoint, save the token in the panel, log in to the
registry on the manager, and refresh each app's Swarm credentials with
`docker service update --with-registry-auth <service>`. If the pull hostname
changes, update the image references on existing services.

## Configuration

| Setting | Default or purpose |
| --- | --- |
| `BUILDER_IMAGE` | Builder image used by the stack |
| `REGISTRY_PULL` | Required HTTPS pull host and optional port |
| `REGISTRY_PUSH` | `registry:5000`, the HTTPS target in BuildKit's configuration |
| `PANEL_CORE_URL` | `http://panel:8080` |
| `BUILDKIT_ADDR` | `tcp://buildkitd:1234` with client TLS credentials |
| `TRAEFIK_ENTRYPOINT` | `websecure` |
| `TRAEFIK_RESOLVER` | `le`. Set an empty value to serve app routes over HTTP. |
| `DATA_DIR` | `/data` |
| `PANEL_MODULE_NAME` | `builder`. Keep it aligned with the panel's module name. |
