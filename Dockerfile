FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /module .

FROM moby/buildkit:v0.35.1@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c AS buildkit

FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
RUN apk add --no-cache git ca-certificates \
    && addgroup -S -g 10001 builder \
    && adduser -S -D -u 10001 -G builder builder \
    && mkdir -m 0700 /data \
    && chown 10001:10001 /data \
    && ln -s /usr/share/modularnost /usr/share/builder
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
COPY --from=build /module /module
COPY LICENSE /usr/share/licenses/builder-module/LICENSE
COPY deployment/manifest.json deployment/buildkit-worker.json stack.yml stack.registry-pull.yml /usr/share/modularnost/deploy/
COPY scripts/buildkitd.sh scripts/buildkitd-network.sh scripts/buildkitd.toml scripts/registry-pull.Caddyfile /usr/share/modularnost/deploy/scripts/
USER 10001:10001
VOLUME /data
EXPOSE 8080
HEALTHCHECK CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/module"]
