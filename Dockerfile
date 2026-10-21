FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /module .

FROM moby/buildkit:v0.23.2 AS buildkit

FROM alpine:3.22
RUN apk add --no-cache git ca-certificates
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
COPY --from=build /module /module
VOLUME /data
EXPOSE 8080
HEALTHCHECK CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/module"]
