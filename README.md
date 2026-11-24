# Builder for Modularnost

Builder turns a public HTTPS Git repository with a Dockerfile into a Docker
Swarm service. From the [Modularnost panel](https://github.com/modularnost/panel),
add an app and select **Build and deploy**. Subsequent builds update the same
service. Set an app domain to route traffic through Traefik.

Builder is an early preview. Builds are started manually by panel
administrators. Use repositories and Dockerfiles you trust.

## Installation

You need a Linux Docker Swarm manager and a panel that supports module contract
3, the `code` block, `service:read` and `service:create`, and
[`panel.module.pull-registry`](https://github.com/modularnost/panel/blob/main/docs/modules.md#asking-the-core-to-act).
Enable encrypted registry credentials in the panel.

Follow the [installation guide](docs/install.md) to prepare the Builder image,
connect the panel network, start BuildKit, and configure authenticated pulls.
The same guide covers [upgrades](docs/install.md#upgrade) and
[configuration](docs/install.md#configuration).

## Network access

Reach Builder through the panel. Keep the BuildKit listener, the internal
registry, and the authenticated pull endpoint on private networks. BuildKit
uses mTLS and a separate worker network for Dockerfile `RUN` steps. The panel
creates and updates services on Builder's behalf.

## Development

```sh
go test ./...
```

## License

Builder's source code is available under the [MIT License](LICENSE).
