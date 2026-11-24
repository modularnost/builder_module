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
on one node, on a network of its own (`buildnet`) that only the module and the
registry join. The module has no `docker.sock`; the panel creates the service.

## Install

On a manager node, as root:

```sh
sh scripts/buildkitd.sh          # buildkitd and the buildnet network
REGISTRY_PULL=registry.internal.example docker stack deploy -c stack.yml builder
```

Then open **Builder** in the panel (admin only), add an app and press **Build
and deploy**. A domain makes the module add Traefik labels; leave it empty to
run the service without a route.

Before deploying apps, provision a protected endpoint for the same registry
storage. It must be reachable by Docker on every eligible Swarm node and
restricted to the cluster's trusted nodes, for example through a TLS proxy on
a private network with source allowlisting. A proxy outside Swarm can join
the attachable `buildnet` overlay and forward to `registry:5000`. The provided
stack keeps the unauthenticated registry on `buildnet` and publishes no port.
Point `REGISTRY_PULL` at that endpoint, without a URL scheme.

On upgrade, deploy the stack with the protected endpoint to remove the old
ingress mapping. Existing apps keep the registry hostname in their image
reference. Update those services to use the protected hostname before the
next build; the builder's update action changes only the image tag.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `PANEL_MODULE_NAME` | `builder` | must match the module's name in the panel |
| `BUILDKIT_ADDR` | `tcp://buildkitd:1234` | |
| `REGISTRY_PUSH` | `registry:5000` | where BuildKit pushes, on `buildnet` |
| `REGISTRY_PULL` | required in the stack | protected endpoint used in the image reference |
| `TRAEFIK_ENTRYPOINT` | `websecure` | |
| `TRAEFIK_RESOLVER` | `le` | empty serves plain HTTP |
| `DATA_DIR` | `/data` | the list of apps |

## Develop

```sh
go test ./...
```
