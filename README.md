# builder_module

Builds a repository and runs it, for the
[modularnost](https://github.com/modularnost/panel) panel: it clones a repo,
builds its Dockerfile with BuildKit, pushes the image to a registry and creates
the service (or moves it to the new tag). Permissions: `service:read`,
`service:create`. Needs a panel with `service:create` and the `code` block, and
contract 3.

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

Then open **Builder** in the panel (admin only), add an app and press **Build
and deploy**. A domain makes the module add Traefik labels; leave it empty to
run the service without a route.

Before deploying apps, provision a protected endpoint for the same registry
storage. For a TLS proxy on a private network, restrict ingress to eligible
Swarm nodes and forward only `GET` and `HEAD` to
`https://registry:5000` from `buildnet`. Configure upstream certificate
verification using the registry CA and give the proxy its own registry client
certificate. Keep the proxy and its certificate trusted as registry clients.
Point `REGISTRY_PULL` at the proxy hostname, without a URL scheme.

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
Existing apps keep their image hostname. If the protected hostname changes,
update their service image references before the next build.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `BUILDKIT_ADDR` | `tcp://buildkitd:1234` | TCP uses the BuildKit client secrets |
| `REGISTRY_PUSH` | `registry:5000` | HTTPS push target configured in `scripts/buildkitd.toml` |
| `REGISTRY_PULL` | required in the stack | protected endpoint used in the image reference |
| `TRAEFIK_ENTRYPOINT` | `websecure` | |
| `TRAEFIK_RESOLVER` | `le` | empty serves plain HTTP |
| `DATA_DIR` | `/data` | the list of apps |

## Develop

```sh
go test ./...
```
