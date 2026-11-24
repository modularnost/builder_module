# builder_module

Builds a repository and runs it, for the
[modularnost](https://github.com/modularnost/panel) panel: it clones a repo,
builds its Dockerfile with BuildKit, pushes the image to a registry and creates
the service (or moves it to the new tag). Permissions: `service:read`,
`service:create`. Needs a panel with `service:create` and the `code` block, and
contract 3. For private pulls, the panel must support
[`panel.module.pull-registry`](https://github.com/modularnost/panel/blob/main/docs/modules.md#asking-the-core-to-act)
and have registry credential encryption configured.

Status: first version, **not yet run on a cluster**. One button builds and
deploys; there is no build on push, no private repositories and no removing an
app yet.

## How it fits

```text
panel ──ui──▶ builder-module ──buildctl──▶ buildkitd ──push──▶ registry:5000
                    │                                              ▲
                    └──/internal/services (service:create)         │
                                  ▼                                │
                       app_<name>  ◀── every node pulls via REGISTRY_PULL
```

A Swarm service cannot be `--privileged`, so BuildKit runs as a plain container
on one node. BuildKit and the registry require mutually authenticated TLS on
`buildnet`. The module has no `docker.sock`; the panel creates the service.

Dockerfile `RUN` steps use a separate worker bridge. They can reach public
IPv4 dependencies. The firewall blocks the daemon, the Swarm overlay and
gateway subnets, private and link-local IPv4 ranges, and IPv6. The worker
uses the public resolvers in `scripts/buildkitd.toml`. BuildKit itself still
uses `buildnet` for registry exports.

## Install

Provision two separate, dedicated CAs, one for BuildKit and one for the
registry. Issue PEM server certificates with DNS SAN `buildkitd` and `registry`
respectively and the `serverAuth` extended key usage. Issue separate client
certificates with `clientAuth` for the module (BuildKit CA), the BuildKit daemon
(registry CA), and the registry pull proxy (registry CA).

On a manager node, as root, place these six files in a dedicated, root-only
directory `/etc/builder/buildkit-tls` for the daemon:

| File | Purpose |
| --- | --- |
| `buildkit-ca.pem` | BuildKit CA certificate |
| `buildkit-server.pem`, `buildkit-server-key.pem` | BuildKit server pair |
| `registry-ca.pem` | Registry CA certificate |
| `registry-client.pem`, `registry-client-key.pem` | BuildKit client pair for registry pushes |

Create the Swarm secrets from the corresponding PEM files. Keep each CA's
private key offline.

```sh
docker secret create builder_buildkit_ca buildkit-ca.pem
docker secret create builder_buildkit_client buildkit-client.pem
docker secret create builder_buildkit_client_key buildkit-client-key.pem
docker secret create builder_registry_ca registry-ca.pem
docker secret create builder_registry_cert registry-server.pem
docker secret create builder_registry_key registry-server-key.pem
BUILDKIT_TLS_DIR=/etc/builder/buildkit-tls sh scripts/buildkitd.sh
REGISTRY_PULL=registry.internal.example docker stack deploy -c stack.yml builder
```

### Authenticated pull endpoint

Use [the Caddy configuration](scripts/registry-pull.Caddyfile) or configure
your existing HTTPS proxy with the same rules. Require Basic Auth for reads,
allow only `GET` and `HEAD` on `/v2/`, and verify the registry's certificate
while authenticating upstream with the proxy's own registry client certificate.
Keep `registry:5000` unpublished on `buildnet`. Docker NAT can give workloads
the same source IP as their node, so use IP allowlisting as an additional
filter.

For Caddy, create a root-only directory `/etc/builder/registry-pull` containing
`proxy.pem` (the HTTPS certificate chain for the pull hostname),
`proxy-key.pem`, `registry-ca.pem`, and the proxy's own
`registry-client.pem` and `registry-client-key.pem`. Generate a random pull
token, hash it with `caddy hash-password`, and put `builder-pull <bcrypt hash>`
in the directory's `users` file. Keep the original token for the panel.
Run Caddy on a Swarm node connected to the attachable `buildnet` overlay.
For example, with DNS pointing at the node's private address:

```sh
docker run -d --name builder-pull-proxy --restart unless-stopped \
  --network buildnet -p 192.0.2.10:443:443 \
  -e REGISTRY_PULL_HOST=registry.internal.example \
  --mount "type=bind,src=$PWD/scripts/registry-pull.Caddyfile,dst=/etc/caddy/Caddyfile,readonly" \
  --mount type=bind,src=/etc/builder/registry-pull,dst=/etc/registry-pull,readonly \
  caddy:2.11.7@sha256:36c24ad91a7ff7bc4c87fe22bfa8df3c4b18da0773d73fc12ee2c52b98c0bb7e
```

Every eligible node's Docker engine must resolve the pull hostname and trust
its HTTPS certificate. For a private CA, install it on each node at
`/etc/docker/certs.d/<host[:port]>/ca.crt`. In the panel, save the username
`builder-pull` and the original token for the exact host and optional port
used in `REGISTRY_PULL`, without a URL scheme. The stack configures the
module's pull registry label from that value.

From a Swarm node, check that an anonymous HTTPS request to `/v2/` returns
`401` and that `docker login <host[:port]>` succeeds with the pull token.

Open **Builder** in the panel (admin only), add an app and press **Build and
deploy**. A domain makes the module add Traefik labels. Leave it empty to run
the service without a route.

## Upgrade

Prepare the certificates and the proxy's mTLS upstream before the upgrade.
Use a Builder image from the same revision as the stack and daemon script.
Set `BUILDER_IMAGE` to its tag or digest when deploying a specific build.
On the node that holds the Builder volume, stop the module and change the
owner of `/data` and its existing app state files to UID and GID `10001`.
For the default stack and volume names:

```sh
docker service scale builder_module=0
docker run --rm -v builder_builder_data:/data alpine:3.22 \
  sh -ec 'chown 10001:10001 /data
    for file in /data/apps.json /data/apps.json.tmp; do
      if [ -f "$file" ]; then chown -h 10001:10001 "$file"; fi
    done'
```

Repeat the installation steps to replace the daemon and deploy the stack.
When moving an existing pull endpoint to Basic Auth, save the credentials
in the panel and refresh existing Swarm services' pull credentials before
enforcing authentication. Log in to the registry on the manager and use
`docker service update --with-registry-auth <service>` for each existing app.
Existing apps keep their image hostname. If the protected hostname changes,
update their service image references before the next build.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `BUILDKIT_ADDR` | `tcp://buildkitd:1234` | TCP uses the BuildKit client secrets |
| `REGISTRY_PUSH` | `registry:5000` | HTTPS push target configured in `scripts/buildkitd.toml` |
| `REGISTRY_PULL` | required in the stack | authenticated HTTPS pull host and optional port |
| `TRAEFIK_ENTRYPOINT` | `websecure` | |
| `TRAEFIK_RESOLVER` | `le` | empty serves plain HTTP |
| `DATA_DIR` | `/data` | the list of apps |
| `PANEL_MODULE_NAME` | `builder` | must match the module's name in the panel |

## Develop

```sh
go test ./...
```
