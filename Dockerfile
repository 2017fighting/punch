# punch-controller — the cluster half (ADR-0044). The router-side `punchd` is a static
# binary shipped as a workflow artifact, not an image: it runs on the router.
FROM --platform=$BUILDPLATFORM golang:1.24 AS build

WORKDIR /src
ENV CGO_ENABLED=0

# Dependencies first, so a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
# The release identity. CI passes github.ref_name (the `vX.Y.Z` tag) and nothing else
# publishes an image, so this is the only value it ever gets in the registry; the default
# is what a local `docker build` gets. Baked in with -X: the process logs it at startup.
ARG VERSION=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/punch-controller ./cmd/punch-controller

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/punch-controller /usr/local/bin/punch-controller

USER nonroot:nonroot
EXPOSE 8080

# Flags are all optional: the defaults are the contract (`-listen :8080`, the bearer
# token from `PUNCH_BEARER_TOKEN`, 15s resync, 60s deadline). The Deployment supplies the
# token from its SOPS Secret; without it the controller refuses to start.
ENTRYPOINT ["/usr/local/bin/punch-controller"]
