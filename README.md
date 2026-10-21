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
                       app_<name>  ◀── every node pulls 127.0.0.1:5000
```

A Swarm service cannot be `--privileged`, so BuildKit runs as a plain container
on one node, on a network of its own (`buildnet`) that only the module and the
registry join. The module has no `docker.sock`; the panel creates the service.

## Install

On a manager node, as root:

```sh
sh scripts/buildkitd.sh          # buildkitd and the buildnet network
docker stack deploy -c stack.yml builder
```

Then open **Builder** in the panel (admin only), add an app and press **Build
and deploy**. A domain makes the module add Traefik labels; leave it empty to
run the service without a route.

The registry publishes port 5000 so every node can pull from `127.0.0.1:5000`.
Close it to the outside in the panel's firewall.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `BUILDKIT_ADDR` | `tcp://buildkitd:1234` | |
| `REGISTRY_PUSH` | `registry:5000` | where BuildKit pushes, on `buildnet` |
| `REGISTRY_PULL` | `127.0.0.1:5000` | the image name the service runs |
| `TRAEFIK_ENTRYPOINT` | `websecure` | |
| `TRAEFIK_RESOLVER` | `le` | empty serves plain HTTP |
| `DATA_DIR` | `/data` | the list of apps |

## Develop

```sh
go test ./...
```
