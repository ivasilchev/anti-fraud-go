# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bin/ ./cmd/server ./cmd/generate ./cmd/verify \
 && mkdir -p /out/data /out/manifest

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --link /out/bin/ /usr/local/bin/
# Mount points for the snapshot and manifest volumes, owned by nonroot so that
# fresh named volumes inherit a writable directory.
COPY --from=build --link --chown=65532:65532 /out/data /data
COPY --from=build --link --chown=65532:65532 /out/manifest /manifest
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
